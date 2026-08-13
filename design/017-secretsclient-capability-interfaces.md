# Splitting SecretsClient Into Per-Method Capabilities, For v1 And v2

## Context

`SecretsClient` is one interface with 8 methods. `Provider.Capabilities()` already declares
`ReadOnly`/`WriteOnly`/`ReadWrite` per provider, but nothing on `SecretsClient` reflects that. An
earlier pass at this proposal split into two bundles, `SecretsReader`
(`GetSecret`/`GetSecretMap`/`GetAllSecrets`) and `SecretsWriter`
(`PushSecret`/`DeleteSecret`/`SecretExists`). That's wrong in a checkable way: several providers
implement `GetSecret` and `GetSecretMap` fully but stub only `GetAllSecrets` —

- `dvls`: `GetAllSecrets is not implemented for DVLS` (`providers/v1/dvls/client.go:153`)
- `beyondtrust`: `GetAllSecrets not implemented` (`providers/v1/beyondtrust/provider.go:340`)
- `passworddepot`: `fmt.Errorf(errNotImplemented, "GetAllSecrets")` (`passworddepot.go:159`)

— and `github`/`ngrok` implement `PushSecret` but stub every read method, not the reverse. Real
support in this codebase is already granular per method. Bundling `GetAllSecrets` into a mandatory
`SecretsReader` wouldn't remove those three stubs, it would relocate them. This doc atomizes fully
instead, and designs the v2 side concretely enough to check the claim that the same split works
for both, rather than asserting it.

## A) Capability discovery: connection-scoped, not store-scoped

015's sketch is `StoreCapabilitiesReader.Capabilities(ctx, esv1.GenericStore) (CapabilitySet, error)`
— parameterized by store. For v2 that's asking the wrong entity: whether a given gRPC endpoint can
write is a property of the server binary/version behind that endpoint, not of any particular
`SecretStore` that happens to reference it. (Correcting an earlier draft of this doc: this isn't
grounded in a `ClusterProviderClass` CRD — that object comes from an unrelated, separate PR
(#6245), and neither 015 nor this doc depends on it. The point stands on its own: ask the
connection, not a store.)

```protobuf
service CapabilityService {
  rpc GetCapabilities(google.protobuf.Empty) returns (CapabilitySet);
}

message CapabilitySet {
  bool get_secret      = 1;
  bool get_secret_map  = 2;
  bool get_all_secrets = 3;
  bool push_secret     = 4;
  bool delete_secret   = 5;
  bool secret_exists   = 6;
}
```

**Open question for 015, not resolved here:** if `Capabilities(ctx, store)` takes a store because
one v2 endpoint can multiplex several logically distinct backends selected by a field inside the
store spec, `store` is needed to pick *which* backend, not to describe the whole spec. Worth
confirming explicitly in 015; this doc proceeds on the simpler premise (capability is a property of
the connection alone) since it's the common case and keeps discovery a one-time, cacheable fact.

## B/C) Full atomization — every data-plane method gets its own interface

```go
// apis/externalsecrets/v1/provider.go

type SecretGetter interface {
	GetSecret(ctx context.Context, ref ExternalSecretDataRemoteRef) ([]byte, error)
}
type SecretMapGetter interface {
	GetSecretMap(ctx context.Context, ref ExternalSecretDataRemoteRef) (map[string][]byte, error)
}
type SecretsFinder interface { // finds multiple secrets by ExternalSecretFind criteria
	GetAllSecrets(ctx context.Context, ref ExternalSecretFind) (map[string][]byte, error)
}
type SecretPusher interface {
	PushSecret(ctx context.Context, data PushSecretData) error // PushSecretData: design/016
}
type SecretDeleter interface {
	DeleteSecret(ctx context.Context, remoteRef PushSecretRemoteRef) error
}
type SecretExistenceChecker interface {
	SecretExists(ctx context.Context, remoteRef PushSecretRemoteRef) (bool, error)
}
type Validator interface {
	Validate(ctx context.Context) (ValidationResult, error) // ctx addition: design/019
}

// SecretsClient is the only interface every provider is required to implement.
type SecretsClient interface {
	Validator
}

// Convenience bundles for the common case (most providers implement all of one side). Dispatch
// never checks against these — see below — only against the single-method interface for the
// exact operation being performed.
type SecretsReader interface {
	SecretGetter
	SecretMapGetter
	SecretsFinder
}
type SecretsWriter interface {
	SecretPusher
	SecretDeleter
	SecretExistenceChecker
}
```

Naming: `-er` suffix on the method verb, matching `io.Reader`/`io.Writer`. `SecretsFinder` over
"SecretAllGetter" because the ref type is already `ExternalSecretFind` — it's a filtered search,
not an enumeration of everything. Open to better names; the split itself is the point.

## v1 dispatch: one type assertion per call site, at every call site

6 call sites, two files:

```
pkg/controllers/externalsecret/externalsecret_controller_secret.go:132  client.GetSecret(ctx, ...)
pkg/controllers/externalsecret/externalsecret_controller_secret.go:227  client.GetSecretMap(ctx, ...)
pkg/controllers/externalsecret/externalsecret_controller_secret.go:278  client.GetAllSecrets(ctx, ...)
pkg/controllers/pushsecret/pushsecret_controller.go:478                 client.DeleteSecret(ctx, ...)
pkg/controllers/pushsecret/pushsecret_controller.go:593                 client.SecretExists(ctx, ...)
pkg/controllers/pushsecret/pushsecret_controller.go:604                 client.PushSecret(ctx, ...)
```

Each becomes:

```go
getter, ok := client.(esv1.SecretGetter)
if !ok {
	return nil, esv1.NotSupportedError{Operation: "GetSecret"} // design/018
}
secretData, err := getter.GetSecret(ctx, secretRef.RemoteRef)
```

`dvls`, `beyondtrust`, `passworddepot` now type-assert to `esv1.SecretsFinder` and fail there,
while their `SecretGetter`/`SecretMapGetter` calls succeed — matching their real, partial support,
with zero stub code in any of the three packages.

## Is Close worth keeping, and where does ESO actually call it?

Checked before answering: `Close` has exactly one caller shape in this codebase.
`pkg/controllers/secretstore/client_manager.go:212` `Manager.Close(ctx)` loops over every client
the manager constructed and calls `val.client.Close(ctx)`, and `Manager.Close` itself is deferred
once per reconcile, at the end of it, in three controllers (`externalsecret_controller_secret.go:51`,
`pushsecret_controller.go:189`, `secretstore/common.go:181`). `Manager` itself is built fresh per
reconcile (`NewManager(...)` at the top of each of those functions) — so in v1, a provider's client
is constructed and torn down **within a single reconcile**, and `Close` is that reconcile's "I'm
done with you" signal. That's genuinely load-bearing for the ~4-5 providers that do real work
there — `vault` revokes a token it minted for this reconcile, `passbolt` logs out, `openbao`
releases idle HTTP connections, `infisical` cancels a background SDK client — and a no-op for the
other ~30-plus, which is why it doesn't belong in the mandatory interface: keep it as its own
optional `Closer` interface for v1, called by `Manager.Close` via type assertion, same pattern as
everywhere else in this doc.

**This does not carry over to v2 as a domain capability, and shouldn't be forced to.** v1's
"construct fresh, tear down at end of reconcile" model doesn't match how a pooled gRPC connection
should be used — a v2 connection is meant to be dialed once and reused across many reconciles and
many stores (that's the entire point of caching it, below). If ESO called an equivalent "Close" RPC
at the end of every reconcile the way it calls `Close` on a v1 client, it would tear down (or
signal teardown of) a connection every other store sharing that endpoint is still using. So: no
`CloseService` RPC, no per-reconcile close signal to a v2 provider at all. Whatever a v2 provider
needs for its own lifecycle (rotate a lease, expire a session) is that provider's own internal
concern, driven by its own timers/logic — not something ESO tells it to do after a call. The only
thing ever "closed" on the v2 side is the local `*grpc.ClientConn`, when ESO's own connection cache
(below) evicts it — a client-side resource, invisible to the provider, no protocol needed.

## D) Caching: only the connection is expensive; the per-store view isn't

An earlier draft of this doc proposed two cache layers: the connection (keyed by whatever
"runtime reference" object carries the connection target), and a second, per-`SecretStore` cache
for the constructed `SecretsClient` value, modeled on `runtime/cache.Must`'s existing v1 use for
"OIDC, vault leases, token exchange" (AGENTS.md). The second layer doesn't hold up under scrutiny
for v2 specifically, and should be dropped:

In v1, `Provider.NewClient(ctx, store, kube, namespace)` does real, store-specific work —
`providers/v1/vault/provider.go:91` and `providers/v1/akeyless/akeyless.go:103` both resolve
credentials out of `kube` for *this* store and build an SDK client around them. That's genuinely
expensive per store, which is exactly why v1 providers reach for `cache.Must[esv1.SecretsClient]`
keyed by store identity.

In v2, the equivalent of "construct a client for this store" is just: look up the shared connection
(the one expensive thing, already cached), and wrap it in a small struct exposing whichever
interfaces the connection's `CapabilitySet` allows (part F). That wrap is pointer assignment, not
I/O — there is nothing store-specific and expensive happening at this step to justify caching its
result. If a store needs per-call parameters (region, sub-service selection, a resolved auth
token), those travel as fields on each RPC request (or as gRPC call metadata attached per-call),
not as state baked into a cached client object — so even a store-parameterized wrapper stays cheap
to construct fresh every time. Caching something that costs nothing to rebuild only adds an
invalidation problem for no benefit. (If a genuinely expensive per-store resource shows up later —
e.g. a resolved bearer token that needs periodic refresh — that's its own small, specifically-keyed
cache for that resource, designed when it's actually needed, not a reason to cache the whole
`SecretsClient` wrapper preemptively.)

So there is one cache, for one thing:

```go
// The only expensive, shared, cacheable resource: the connection and its discovered capabilities.
// Keyed by the identity+generation of whatever object carries the connection target (015's
// "runtime reference", name not yet settled).
var connCache = cache.Must[*connection](256, func(c *connection) { c.conn.Close() })

type connection struct {
	conn *grpc.ClientConn
	caps esv1.CapabilitySet
}

func dial(ctx context.Context, refKey cache.Key, refGeneration int64, addr string) (*connection, error) {
	if c, ok := connCache.Get(refKey, refGeneration); ok {
		return c, nil
	}
	conn, err := grpc.NewClient(addr /* + TLS opts */)
	if err != nil {
		return nil, err
	}
	caps, err := pb.NewCapabilityServiceClient(conn).GetCapabilities(ctx, &emptypb.Empty{})
	if err != nil {
		conn.Close()
		return nil, err
	}
	c := &connection{conn: conn, caps: fromProto(caps)}
	connCache.Add(refKey, refGeneration, c)
	return c, nil
}

// NewClient wraps the (cached) connection fresh every call — cheap, no cache needed here.
func NewClient(ctx context.Context, refKey cache.Key, refGeneration int64, addr string) (esv1.SecretsClient, error) {
	conn, err := dial(ctx, refKey, refGeneration, addr)
	if err != nil {
		return nil, err
	}
	return buildClient(conn), nil // part F
}
```

**Generation, not ResourceVersion.** If the object `refKey` points at has a status subresource this
same controller writes to (health, last-observed capabilities, etc.), keying on `ResourceVersion`
would invalidate the connection cache on the controller's own status writes — self-inflicted
cache thrashing. `Generation` only changes on a spec edit, the only time the connection genuinely
needs rebuilding. This is a deviation from AGENTS.md's currently-documented v1 convention
(`ResourceVersion`), worth adopting more broadly, not just here — flagged as a candidate follow-up,
out of scope to change v1's existing caches in this document.

## F) Why not equally granular at the transport/wrapper-type level — and matching v1's split

Two proto services, `SecretReaderService`/`SecretWriterService` (matching the
`SecretsReader`/`SecretsWriter` bundles, not the full 6-way split), and `CapabilitySet` — not
service registration — is the sole source of truth for what's actually callable. The Go code
splits the same way, on both sides:

```go
// One small adapter per proto service, each implementing exactly the single-method interfaces
// that service covers, each still checking its own capability bit per method (this is what
// handles a dvls/beyondtrust-style *partial* reader without needing a third, fourth, fifth type).

type readerAdapter struct {
	conn *connection
	stub pb.SecretReaderServiceClient
}

func (a *readerAdapter) GetSecret(ctx context.Context, ref esv1.ExternalSecretDataRemoteRef) ([]byte, error) {
	if !a.conn.caps.GetSecret {
		return nil, esv1.NotSupportedError{Operation: "GetSecret"}
	}
	resp, err := a.stub.GetSecret(ctx, &pb.GetSecretRequest{Key: ref.Key, Property: ref.Property})
	if err != nil {
		return nil, err
	}
	return resp.Value, nil
}
// GetSecretMap, GetAllSecrets: same shape.

type writerAdapter struct {
	conn *connection
	stub pb.SecretWriterServiceClient
}
// PushSecret, DeleteSecret, SecretExists: same shape as readerAdapter's methods.

// buildClient composes the two adapters by VALUE into exactly the combination this connection
// actually has, mirroring how a v1 provider package either has these methods or doesn't. Value
// embedding (not a nil-able pointer) is what keeps every promoted method backed by a real,
// connected stub whenever it's reachable at all.
func buildClient(conn *connection) esv1.SecretsClient {
	hasRead := conn.caps.GetSecret || conn.caps.GetSecretMap || conn.caps.GetAllSecrets
	hasWrite := conn.caps.PushSecret || conn.caps.DeleteSecret || conn.caps.SecretExists
	reader := readerAdapter{conn, pb.NewSecretReaderServiceClient(conn.conn)}
	writer := writerAdapter{conn, pb.NewSecretWriterServiceClient(conn.conn)}
	switch {
	case hasRead && hasWrite:
		return &struct {
			readerAdapter
			writerAdapter
			validator
		}{reader, writer, validator{conn}}
	case hasWrite:
		return &struct {
			writerAdapter
			validator
		}{writer, validator{conn}}
	default:
		return &struct {
			readerAdapter
			validator
		}{reader, validator{conn}}
	}
}
```

Why this stops at 3 composite shapes and doesn't try to enumerate all 64 combinations of 6
independent bits: value-embedding lets a type "inherit" an interface for free, but only pays for
itself when the number of *shapes you must name* stays small. There are exactly 3 shapes that
matter at this level — no read capability, no write capability, both — a direct mirror of how v1
providers already split (read-only, write-only like `github`/`ngrok`, read-write). Going further,
to name a distinct type for every one of the 64 possible combinations of the 6 individual method
bits, would buy nothing: those combinations mostly won't occur, and the within-bucket cases that
do occur (dvls-style "reads two of three") are already handled correctly by the per-method guard
inside `readerAdapter`/`writerAdapter`, without needing a type for them. So the split is genuinely
the same on both sides — `SecretGetter`, `SecretMapGetter`, `SecretsFinder`, `SecretPusher`,
`SecretDeleter`, `SecretExistenceChecker` all exist as real, independent interfaces in both v1 and
v2 — but the *number of named concrete types* that implement various subsets of them differs (42
on v1, because that's how many independently compiled provider packages exist; up to 3 on v2,
because one shared adapter package serves every out-of-tree provider), for a reason inherent to
the two models, not a design shortcut on either side.

One real, honest gap remains: `readerAdapter` embedded into the `hasRead && hasWrite` case
structurally satisfies `SecretsFinder` even for a connection whose `caps.GetAllSecrets` is false
(dvls-equivalent case in v2) — the type assertion at the v1-identical call site succeeds, and the
"not supported" answer arrives one line later, from the method body's own guard, not from a failed
assertion. That is a real behavioral difference from a v1 provider package that simply never
defines `GetAllSecrets` (whose assertion fails outright). Both paths return the same
`esv1.NotSupportedError` (design/018) to the controller, so the observable outcome for a user is
identical — worth stating plainly, since avoiding exactly this kind of gloss is why this document
exists.

## Testing

1. **v1**: compile-time `var _ esv1.SecretGetter = &Client{}`-style lines are useful documentation
   per provider but not mechanically enforced by this design alone.
2. **Repo-wide conformance test** (root module): for each registered v1 provider, type-assert its
   client against all 6 single-method interfaces and compare the result against its documented
   capability metadata — catches `SecretsFinder` claimed-but-missing drift like the dvls case
   automatically instead of by manual code reading.
3. **Controller unit tests**, one per call site (6 total): a fake client implementing a chosen
   subset of the 6 interfaces produces `esv1.NotSupportedError` exactly for the methods it lacks.
4. **v2 connection-cache test**: two `dial` calls with the same `refKey`/`refGeneration` connect
   exactly once (mock the dialer, assert call count); a changed `refGeneration` dials again; a
   changed `ResourceVersion` alone (simulating a status-only write) does not.
5. **v2 `buildClient` test**: for each of the 3 `CapabilitySet` shapes (read-only, write-only,
   read-write), assert the returned value's type-assertion results against
   `SecretsReader`/`SecretsWriter` match the intended shape, and that a partial-read
   `CapabilitySet` (e.g. `GetSecret`+`GetSecretMap` true, `GetAllSecrets` false) produces a value
   that satisfies `esv1.SecretsFinder` structurally but returns `NotSupportedError` when called —
   documenting the honest gap from part F as a test, not just prose.
6. **Existing per-provider and e2e suites** pass unmodified.

## Overlap with design/015 and design/016

- 015 defines `CapabilitySet`/`StoreCapabilitiesReader`; part A proposes narrowing its signature to
  connection-scoped, flagged as an open question for 015 rather than assumed settled.
- 015 lists "give v1 and v2 a common controller-facing capability contract" as an undefined goal;
  the 6 single-method interfaces plus `Validator`, dispatched identically at all 6 v1 call sites
  and composed into 3 shapes in the v2 adapter, are the concrete proposal for that contract.
- 016 defines `PushSecretData`; `SecretPusher.PushSecret` takes that exact type, and a
  `PushSecretRequest` proto message would mirror its fields directly.

## Non-Goals

- Not defining the final `.proto` beyond the two services already agreed above.
- Not touching `Provider.Capabilities()` beyond the connection-vs-store question in part A.
- Not designing any v2 provider-side lifecycle/lease-renewal signal — explicitly deferred, not
  solved, in the Close discussion above.
- Not designing a per-store resolved-credential cache for v2 — noted in part D as a possible future
  need, not proposed here absent a concrete case for it.

## Drawbacks

- 6 interfaces instead of 1 (or 2) is more names to learn, even at one method each.
- The gap called out at the end of part F is real: a v2 connection's "not supported" for a
  within-bucket partial capability costs one always-succeeding type assertion followed by a local
  guard, not a failed assertion. Acceptable, and tested (item 5), not just claimed.

## Alternatives

- **Two bundles only, no further split.** Rejected: doesn't remove the `dvls`/`beyondtrust`/
  `passworddepot`-style partial-reader stubs, which already exist today.
- **64 named v2 wrapper types for full combinatorial parity with v1's per-method granularity.**
  Rejected per part F: not worth building for combinations that mostly won't occur.
- **A per-reconcile "Close" RPC for v2, mirroring v1 exactly.** Rejected: would undermine the
  entire reason to cache and reuse a v2 connection across reconciles and stores.
- **A per-store client cache for v2, mirroring v1's `cache.Must[esv1.SecretsClient]`.** Rejected
  per part D: v2 client construction does no store-specific I/O, so there's nothing expensive to
  cache at that layer.

## Acceptance Criteria

- The 6 single-method interfaces plus `Validator` exist in `apis/externalsecrets/v1`;
  `SecretsClient` is just `Validator`. `Closer` exists separately, used only by the v1 manager.
- All 6 controller call sites type-assert to the specific interface for that operation.
- The ~28 Push/Delete/Exists (or reader-side) stubs and ~30 no-op `Close` methods are deleted, not
  migrated.
- `runtime/provider/grpc` implements the single connection cache in part D and the 3-shape
  `buildClient` in part F, with no per-store cache.
- Tests in items 1-6 above exist and pass; `make test`/`make check-diff` pass across all modules.
