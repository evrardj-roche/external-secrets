# PushSecret Data Consolidation

## Context

`SecretsClient.PushSecret` is defined as:

```go
PushSecret(ctx context.Context, secret *corev1.Secret, data PushSecretData) error
```

Two problems compound here:

1. `secret` is a full `k8s.io/api/core/v1.Secret`. A field audit of every provider's `PushSecret`
   found `Data` used by all real implementations, `Type`/`Labels`/`Annotations` used only by
   `kubernetes`, `Name`/`Namespace` used only in error strings (`dvls`, `onepasswordsdk`,
   `aws/certificatemanager`), and nothing else ever read.
2. `PushSecretData` (`apis/externalsecrets/v1/pushsecret_interfaces.go`), despite its name, does
   not hold the bytes to push. It holds routing: which local key to read (`GetSecretKey`), where
   remotely to write it (`GetRemoteKey`/`GetProperty`), and an opaque per-provider options blob
   (`GetMetadata`). It is an interface only because its concrete implementation
   (`v1alpha1.PushSecretData`) lives in `apis/externalsecrets/v1alpha1`, and `v1` must not import
   `v1alpha1`.

At least 15 providers (`vault`, `kubernetes`, `azure/keyvault`, `ngrok`, `keepersecurity`,
`github`, `dvls`, `onepasswordsdk`, `oracle`, `scaleway`, plus everyone calling
`runtime/esutils.ExtractSecretData`) each reimplement the same branch:

```go
if data.GetSecretKey() == "" {
    // marshal the whole secret.Data map to JSON, push that as one value
} else {
    // push secret.Data[data.GetSecretKey()] as-is
}
```

There is a precedent for resolving this kind of thing once, centrally: `PushSecretData`'s
`ConversionStrategy` field (e.g. `ReverseUnicode`) is **already** fully resolved before any
provider sees anything. The controller calls `esutils.ReverseKeys(...)` at
`pkg/controllers/pushsecret/pushsecret_controller.go:582`, builds `localSecret` with the
already-converted `Data`, and only then calls `PushSecret` at line 604. No provider implementation
branches on `ConversionStrategy` — in fact `esv1.PushSecretData` doesn't even expose a
`GetConversionStrategy()` method today. This doc extends that same pattern to secret-key
resolution.

## Goals

- Replace `secret *corev1.Secret, data PushSecretData` with one struct, resolved once, so
  providers stop reimplementing key-selection logic and stop depending on `k8s.io/api/core/v1`.
- Keep the "provider makes no resolution decisions" property `ConversionStrategy` already has.
- Preserve the one structural exception (`kubernetes` pushing a full key/value map instead of one
  scalar) without forcing every other provider to handle it.

## Non-Goals

- Not touching `GetSecret`/`GetSecretMap`/`GetAllSecrets` or their ref types.
- Not solving generic audit/traceability. `Source` is included only because three providers
  already read `Name`/`Namespace` today.
- Not touching `apis/externalsecrets/v1beta1` (the webhook-only API version), which has its own,
  separate `PushSecretData`/`PushSecretRemoteRef` interfaces and fakes. Same reasoning would
  apply there, but as a distinct follow-up, not bundled here.
- Not defining the v2 protobuf message.

## 1. Where each piece lives

Nothing here crosses a module boundary that doesn't already exist:

| What | Package (unchanged location) | Change |
|---|---|---|
| `v1alpha1.PushSecretData` (CRD schema: `Match`, `Metadata`, `ConversionStrategy`, kubebuilder markers) | `apis/externalsecrets/v1alpha1/pushsecret_types.go` | **Untouched.** It's YAML/JSON schema for the `PushSecret` CRD; this proposal doesn't change the CRD. |
| `PushSecretData` (4-method interface) | `apis/externalsecrets/v1/pushsecret_interfaces.go` | **Removed.** |
| `PushSecretRemoteRef` (2-method interface) | same file | **Removed**, replaced by a struct of the same name. |
| `PushSecretData` (new struct — see naming below) | `apis/externalsecrets/v1/pushsecret_interfaces.go` (or renamed to `pushsecret_types.go` to stop calling a file "interfaces" that no longer has any) | **New.** |
| Resolution logic (build the new `v1.PushSecretData` from `v1alpha1.PushSecretData` + a `*corev1.Secret`) | `runtime/esutils` — this is where `ExtractSecretData` already lives today (`runtime/esutils/utils.go:610`) | **Moved here** from being duplicated inline in ~15 provider packages. `runtime/esutils` already sits in the dependency path between `apis` and both `pkg/controllers` (root module) and `providers/v1/*` (each provider's `go.mod` already depends on `runtime`), so nothing new needs wiring. |
| Call site | `pkg/controllers/pushsecret/pushsecret_controller.go:601-604` | Calls the new `runtime/esutils` resolver instead of building `localSecret *corev1.Secret`, then calls `secretClient.PushSecret(ctx, resolved)`. |

So: `v1alpha1` keeps the CRD type exactly as it is today. `v1` loses an interface and gains a
struct, in the same file it already lived in. `runtime/esutils` gains the one resolver function
that today's ~15 providers each hand-roll. No new module, no new dependency edge.

## 2. Testability impact

Checked before proposing this, not asserted: interfaces used purely as data carriers (as these
are — no method does anything but return a field) generate a specific kind of test boilerplate,
and this codebase already has three flavors of it for these two interfaces:

1. `runtime/testing/fake.PushSecretData` — a plain struct with 4 exported fields and 4
   one-line getter methods that exist solely to satisfy `esv1.PushSecretData`. Used via literals
   like `&testingfake.PushSecretData{RemoteKey: "secret"}` across 15+ provider test files
   (`vault`, `github`, `akeyless`, `doppler`, `kubernetes`, `onboardbase`, `bitwarden`,
   `secretserver`, `keepersecurity`, `oracle`, `infisical`, `onepassword`, ...).
2. Per-provider local stubs, e.g. `providers/v1/dvls/client_test.go:141`
   `pushSecretDataStub`/`pushSecretRemoteRefStub` — the same pattern, reinvented locally, in at
   least 9 provider test files (`dvls`, `ngrok`, `onboardbase`, `onepassword`, `onepasswordsdk`,
   `secretserver`, `volcengine`, `aws/certificatemanager`, `crd`).
3. `apis/externalsecrets/v1/fakes/pushremoteref.go` (and its `v1beta1` twin) — a full
   `counterfeiter`-generated behavioral spy (call-count tracking, per-call argument capture,
   stubbed return sequencing). I checked every consumer of this type
   (`grep -rn "fakes.PushRemoteRef{"` and `CallCount()`/`ArgsForCall(` anywhere combined with
   these types): **zero usages outside the `fakes` package itself.** It's generated, compiles, and
   is never imported. No test today asserts "the provider called `GetRemoteKey` N times" or
   inspects per-call arguments through this type.

Replacing the interfaces with plain structs:

- Removes (1) and (2) outright — every one of those literals (`&testingfake.PushSecretData{RemoteKey: "secret"}`,
  `pushSecretDataStub{...}`) becomes a literal of the real `esv1.PushSecretData`/
  `esv1.PushSecretRemoteRef` struct instead, same call-site shape, one less type to import.
- Deletes the dead generated mock (3) entirely — nothing currently depends on the behavioral
  spying it offers.
- **Real, honest downside:** if a future test needs to assert call counts or per-call arguments on
  these specific methods, that capability is gone (a struct has no "was this field read" signal).
  No current test needs this — providers that verify their own logic today do it by mocking the
  cloud SDK client (e.g. a fake Vault/AWS client), not by spying on the ref type — so this is a
  capability that would have to be re-added only if a concrete need for it shows up later.

Net: fewer fake types to maintain (one file deleted, ~9+ local stubs deleted, ~15+ test files
simplified), same literal-construction ergonomics, one theoretical (currently unused) capability
given up.

## 3. Exact API change

Before:

```go
// apis/externalsecrets/v1/pushsecret_interfaces.go
type PushSecretData interface {
	GetMetadata() *apiextensionsv1.JSON
	GetSecretKey() string
	GetRemoteKey() string
	GetProperty() string
}
type PushSecretRemoteRef interface {
	GetRemoteKey() string
	GetProperty() string
}

// apis/externalsecrets/v1/provider.go
PushSecret(ctx context.Context, secret *corev1.Secret, data PushSecretData) error
DeleteSecret(ctx context.Context, remoteRef PushSecretRemoteRef) error
SecretExists(ctx context.Context, remoteRef PushSecretRemoteRef) (bool, error)
```

After:

```go
// apis/externalsecrets/v1/pushsecret_interfaces.go (interfaces gone, structs in their place)
type PushSecretRemoteRef struct {
	RemoteKey string
	Property  string
}

type PushSecretSource struct { // optional, nil-able
	Name      string
	Namespace string
}

type PushSecretData struct {
	PushSecretRemoteRef

	// SecretKey is the source key this was resolved from ("" meant "the whole secret").
	// Informational only (used today by dvls/onepasswordsdk for error messages) — providers
	// pushing a single value should use Value, not re-derive anything from SecretKey.
	SecretKey string

	// Value is the single resolved value to write. Replaces runtime/esutils.ExtractSecretData
	// and the equivalent inline branch duplicated across ~15 providers.
	Value []byte

	// Data is the full source Secret's data (already conversion-strategy-applied), for the one
	// provider (kubernetes) that mirrors a whole secret rather than one entry.
	Data map[string][]byte

	SecretType  string // corev1.SecretType as a string; kubernetes only.
	Labels      map[string]string
	Annotations map[string]string
	Source      *PushSecretSource

	Metadata *apiextensionsv1.JSON // unchanged from today's GetMetadata().
}

// apis/externalsecrets/v1/provider.go
PushSecret(ctx context.Context, data PushSecretData) error
DeleteSecret(ctx context.Context, remoteRef PushSecretRemoteRef) error
SecretExists(ctx context.Context, remoteRef PushSecretRemoteRef) (bool, error)
```

`v1alpha1.PushSecretData` (the CRD schema type, with `Match`, `Metadata`, `ConversionStrategy`,
and its own `Get*()` convenience methods) is not changed by this proposal at all. Those `Get*()`
methods on the `v1alpha1` type stop being required for interface satisfaction, but nothing stops
`pkg/controllers/pushsecret` from keeping and using them for readability if it wants — that's an
independent, non-breaking choice.

### Naming: why the new struct is still called `PushSecretData`

Once the old interface is deleted, the name `PushSecretData` is free. The v1alpha1 doc comment
already says what this concept should be: *"PushSecretData defines data to be pushed to the
provider and associated metadata."* The old `v1` interface never matched that description (it held
no data); the new struct does, so it gets the name that was always intended for it. This mirrors
the existing convention in this codebase where `v1alpha1.X` (CRD schema) and `esv1.X` (runtime
type) share a name across packages — e.g. today, `v1alpha1.PushSecretRemoteRef` already implements
`esv1.PushSecretRemoteRef`, same name, different package, different job.

`PushSecretRequest` was considered and rejected as a name: it reads as if it covered
`DeleteSecret`/`SecretExists` too, but it doesn't — those keep the smaller `PushSecretRemoteRef`,
unchanged in scope from today.

## 4. Where did `Match` go?

`v1alpha1.PushSecretData.Match` is `PushSecretMatch{SecretKey string; RemoteRef PushSecretRemoteRef}`.
Nothing here is dropped — it's redistributed onto the new struct:

| Old (`v1alpha1.PushSecretData.Match`) | New (`v1.PushSecretData`) |
|---|---|
| `Match.RemoteRef.RemoteKey` | `PushSecretRemoteRef.RemoteKey` (embedded) |
| `Match.RemoteRef.Property` | `PushSecretRemoteRef.Property` (embedded) |
| `Match.SecretKey` | `SecretKey` (kept, informational — see below) — **and** resolved into `Value` |
| `ConversionStrategy` (sibling of `Match`, not part of it) | fully applied before construction; still absent from the `v1` type, exactly like today |

`SecretKey` is kept as a plain field rather than dropped, because it isn't purely redundant with
`Value`: `dvls` (`client.go:410,414`) puts the literal key name in its error messages ("key %q not
found in secret %q"), and that's a legitimate use that has nothing to do with resolving the value
to push. What changes is that **no provider branches on whether `SecretKey` is empty anymore** —
that decision (single value vs. whole-map JSON) is made once by the `runtime/esutils` resolver
before `PushSecret` is called, exactly as already happens for `ConversionStrategy`.

## Behavior / Migration

- Providers using `ExtractSecretData` or the equivalent inline branch: delete that code, use
  `data.Value` directly.
- `kubernetes`: uses `data.Data`, `data.SecretType`, `data.Labels`, `data.Annotations` for its
  structural cases, `data.Value` for the scalar case — this simplifies `mergePushSecretData`
  rather than just renaming its inputs.
- `dvls`, `onepasswordsdk`, `aws/certificatemanager`: swap `secret.Name`/`secret.Namespace` for
  `data.Source.Name`/`data.Source.Namespace` (nil-checked).
- A missing `SecretKey` (key not found in the source Secret) is now detected by the
  `runtime/esutils` resolver before any provider is called, producing one consistent
  error/condition instead of each provider's own wrapped error text.

## Drawbacks

- Still a breaking signature change across all ~42 provider modules.
- The controller now copies fields into the new struct on every call instead of the interface
  satisfying itself for free — a small, bounded allocation per push-per-key, not a practical
  concern.
- `Value` and `Data` overlap in the common case (`Value` is derivable from `Data`+`SecretKey`).
  Accepted deliberately, same trade-off already made for `ConversionStrategy`: resolve once,
  centrally, rather than ask every provider to re-derive it.

## Alternatives

- **Keep `*corev1.Secret`.** Rejected: keeps the `k8s.io/api` dependency and the duplicated
  key-resolution branch.
- **Two separate types (a "content" struct plus the existing `PushSecretData` interface),
  unmerged.** Considered first; rejected once `ConversionStrategy`'s existing precedent made clear
  that resolving `SecretKey` the same way, into one struct, is more consistent, not less.
- **A `Value() ([]byte, error)` method instead of a pre-resolved field.** Rejected: the failure
  case (key not found) is knowable by the controller before dialing the provider at all, so
  failing fast there is strictly better than making every provider call a method that can fail for
  a reason unrelated to the provider itself.

## Acceptance Criteria

- All in-tree providers compile against the new `PushSecret`/`DeleteSecret`/`SecretExists`
  signatures; `make test` and `make check-diff` pass.
- `runtime/esutils.ExtractSecretData`, `runtime/testing/fake.PushSecretData`, and
  `apis/externalsecrets/v1/fakes/pushremoteref.go` are deleted; no remaining references.
- `kubernetes` provider tests confirm `Data`/`SecretType`/`Labels`/`Annotations` still produce the
  same merge behavior as today.
- A regression test confirms a missing `SecretKey` fails during reconcile with one consistent
  error/condition, before any provider client is invoked.
