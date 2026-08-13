# Context On Validate

## Context

Every `SecretsClient` method takes `ctx context.Context` first, except `Validate() (ValidationResult, error)`
(`apis/externalsecrets/v1/provider.go:91`). One call site:
`pkg/controllers/secretstore/common.go:190`, inside `validateStore(ctx, ...)`, which already has
`ctx` in scope.

This isn't purely cosmetic. Two providers already do real network I/O inside `Validate()`, both
hardcoding `context.Background()` because there's no caller-supplied context to use instead:

- `providers/v1/vault/validate.go:191`: `checkToken(context.Background(), c.token)`.
- `providers/v1/aws/secretsmanager/secretsmanager.go:551`: `sm.cfg.Credentials.Retrieve(context.Background())`.

`gcp/secretmanager` and `azure/keyvault`'s `Validate()` do no I/O at all (they just check locally
already-known config), so for them this change is a no-op parameter.

## What this practically entails (it's cheap here — here's why)

Adding `ctx` to an interface method can be expensive when the call site is buried under several
layers that themselves have no context to forward, or when a struct captured a `context.Background()`
at construction time and now needs it threaded through unrelated call chains. None of that applies
here:

1. **The interface signature**: `Validate(ctx context.Context) (ValidationResult, error)` — one
   line in `provider.go`.
2. **~40 of 42 providers' method bodies do no I/O.** Their `ctx` parameter is unused (name it `_`
   or use it in a doc comment); zero behavior change, mechanical signature edit only.
3. **The 2 providers doing real I/O already thread a context as far as the SDK call** — `checkToken`
   and `Credentials.Retrieve` both already accept a `context.Context` parameter. The only missing
   link is the outermost hop: swap `context.Background()` for the `ctx` now available as a
   parameter. No new plumbing through intermediate functions; both are called directly from
   `Validate()`'s own body.
4. **The one call site already has `ctx` in scope** as a parameter of `validateStore`. The change
   there is `cl.Validate()` → `cl.Validate(ctx)`.

Contrast with when this kind of change *is* hard: if `Validate()` were called from a goroutine
started without a context, or from a method on a struct that only captured `context.TODO()` at
construction, adding `ctx` would ripple through every signature between the call site and wherever
a real context first becomes available — sometimes forcing an awkward choice (store a `ctx` on a
struct, an anti-pattern, or accept `context.TODO()` as a permanent stopgap). Neither condition
holds here: `validateStore` is already a `ctx`-first function, and `Validate()` is a leaf call
within it.

## Behavior once fixed

A `SecretStore` validation that today can block on `checkToken`/`Credentials.Retrieve` until the
network stack itself times out will instead respect whatever cancellation/deadline the reconciler
already applies to `ctx` — consistent with the rest of the reconcile loop, which is already
`ctx`-aware everywhere else.

## Non-Goals

- Not adding a new timeout or cancellation policy to the secretstore controller itself. If a future
  change wants a bounded validate-timeout, it now has exactly one place to add it
  (`validateStore`'s own `ctx`), not 42.
- Not touching any other method's signature — this is `Validate` only.

## Migration

Mechanical: update the interface, then every `func (.*) Validate() (esv1.ValidationResult, error)`
across all ~42 provider modules to accept and (mostly) ignore `ctx`. `vault` and
`aws/secretsmanager` additionally swap `context.Background()` for the parameter.

## Acceptance Criteria

- `SecretsClient.Validate` takes `ctx context.Context`; all providers compile across their
  individual `go.mod`s (each provider is its own module — this needs a build sweep, not just a
  root-module `go build`).
- `vault`/`aws/secretsmanager` unit tests confirm an already-canceled `ctx` passed to `Validate`
  aborts the underlying SDK call with `context.Canceled`, rather than proceeding uncancelably.
- `make test` and `make check-diff` pass.
