# Splitting Metadata Fetch Out Of GetSecret

## Context

`ExternalSecretDataRemoteRef.MetadataPolicy` (`apis/externalsecrets/v1/externalsecret_types.go:294-321`)
can be `None` (default) or `Fetch`. When `Fetch`, `GetSecret`'s `[]byte` return stops meaning "the
secret value" and starts meaning "the provider's tags/labels for this secret, JSON-marshaled" —
same method, same signature, same return type, completely different content, selected by a field
buried inside the `ref` argument.

Walking `vault`'s implementation end to end (`providers/v1/vault/client_get.go:51-78`):

```go
func (c *client) GetSecret(ctx context.Context, ref esv1.ExternalSecretDataRemoteRef) ([]byte, error) {
	var data map[string]any
	var err error
	if ref.MetadataPolicy == esv1.ExternalSecretMetadataPolicyFetch {
		if c.store.Version == esv1.VaultKVStoreV1 {
			return nil, errors.New(errUnsupportedMetadataKvVersion)
		}
		metadata, err := c.readSecretMetadata(ctx, ref.Key) // custom_metadata from Vault, not the secret
		...
		data = /* metadata, not secret data */
	} else {
		data, err = c.readSecret(ctx, ref.Key, ref.Version) // the actual secret
	}
	return getSecretValue(data, ref.Property)
}
```

`GetSecretMap` (`client_get.go:83`) calls `GetSecret` and `json.Unmarshal`s the result, so the
metadata-vs-data ambiguity flows transparently through both entry points for vault.

Only 4 providers implement this at all: `vault`, `kubernetes`, `ibm`, `ovh` (confirmed by grepping
every occurrence of `MetadataPolicy` outside `apis/`). The other ~36 (`aws/secretsmanager`,
`gcp/secretmanager`, every other provider) never reference the field. That's not "unsupported and
documented" — it's silent: setting `metadataPolicy: Fetch` against, say, AWS Secrets Manager today
produces **no error**. The provider ignores the field entirely and returns the plain secret value,
exactly as if the user hadn't set it. A user who asks for provider metadata and gets back the
secret value instead, with no error and no condition, has a real, silent, wrong-data bug — not just
an architectural rough edge.

## Why this matters more once wire format is typed

The problem isn't that `[]byte` can't cross a gRPC wire — any bytes can. The problem is that the
*meaning* of the bytes is decided by a side-channel field the callee must remember to check, most
implementations don't, and there is no signal anywhere (compile time, runtime, or CRD validation)
when a provider silently doesn't honor it. A typed v2 response can only make this better if the
request/response shape stops representing two unrelated things as one field.

## Goals

- Make it impossible for a provider to silently ignore a metadata-fetch request: either it's
  answered correctly, or the caller gets `esv1.NotSupportedError` (design/018).
- Give this its own method rather than overloading `GetSecret`'s return.

## Non-Goals

- Not changing what "metadata" means per provider (Vault's `custom_metadata`, IBM's/OVH's/
  Kubernetes' equivalents stay whatever they are).
- Not deciding the exact `map[string]string` vs `map[string]any` shape here in detail — vault's
  existing `readSecretMetadata` already returns `map[string]string`; this doc assumes that shape
  is representative but leaves final typing to implementation.

## Proposal

```go
// apis/externalsecrets/v1/provider.go
type SecretMetadataGetter interface {
	GetSecretMetadata(ctx context.Context, ref ExternalSecretDataRemoteRef) (map[string]string, error)
}
```

A fifth single-method interface alongside design/017's six — same pattern, same reasoning: only
`vault`, `kubernetes`, `ibm`, `ovh` implement it; everyone else simply doesn't have the method.

The controller, at the `MetadataPolicy: Fetch` branch (wherever `ExternalSecretDataRemoteRef` is
consumed in `pkg/controllers/externalsecret`), decides which method to call based on the CRD field
— the same YAML stays backward compatible — instead of forwarding the field down into `GetSecret`
for the provider to notice or not:

```go
if secretRef.RemoteRef.MetadataPolicy == esv1.ExternalSecretMetadataPolicyFetch {
	getter, ok := client.(esv1.SecretMetadataGetter)
	if !ok {
		return esv1.NotSupportedError{Operation: "GetSecretMetadata"} // was: silent wrong data
	}
	metadata, err := getter.GetSecretMetadata(ctx, secretRef.RemoteRef)
	...
}
```

## Behavior change (flagged explicitly, not a pure refactor)

Today: `metadataPolicy: Fetch` against an unsupporting provider silently returns the secret value.
After this change: it returns `esv1.NotSupportedError`, surfaced as a reconcile error/condition.
This is a correctness fix riding along with the interface cleanup — call it out to reviewers as a
user-visible behavior change, not wave it through as "just a refactor."

`vault`'s existing explicit rejection for KV v1 (`errUnsupportedMetadataKvVersion`) becomes a
`NotSupportedError{Operation: "GetSecretMetadata", Reason: "..."}` (design/018) — it already had
the right idea, just the wrong error type.

## Migration

- `vault`, `kubernetes`, `ibm`, `ovh`: move their `MetadataPolicy == Fetch` branch out of
  `GetSecret` into a new `GetSecretMetadata` method; `GetSecret` loses that branch entirely.
- Everyone else: no change (they never had the branch).
- `pkg/controllers/externalsecret`: dispatch on `MetadataPolicy` at the point it currently forwards
  `ref` to `GetSecret`, per the snippet above.

## Drawbacks

- One more single-method interface (5th, alongside design/017's 6) to document.
- A provider could in principle support fetching metadata for `GetSecret` but not for
  `GetSecretMap`/`GetAllSecrets` — this proposal only covers the `GetSecret` path, matching every
  current implementation (none of the 4 apply `MetadataPolicy` to `GetAllSecrets`, and only
  `kubernetes` applies it to `GetSecretMap`, itself calling through `GetSecret`). If that changes,
  extending `SecretMetadataGetter` with a second method is straightforward and doesn't require
  redesigning this one.

## Alternatives

- **Keep as-is, document loudly that support is inconsistent.** Rejected: doesn't fix the silent
  wrong-data case, which is the actual bug.
- **A discriminated union return from a single method** (`GetSecretResult{Data []byte; Metadata
  map[string]string}`) instead of a second method. Considered; rejected because real providers only
  ever support one or the other per call today (nobody returns both at once), so two methods match
  actual usage more directly and avoid a return type where one field is always unset.

## Acceptance Criteria

- `esv1.SecretMetadataGetter` exists; `vault`/`kubernetes`/`ibm`/`ovh` implement it;
  `GetSecret` no longer branches on `MetadataPolicy` in any of the four.
- A regression test confirms a provider without `SecretMetadataGetter` now returns
  `esv1.NotSupportedError` for `metadataPolicy: Fetch`, where it previously silently returned the
  secret value.
- Existing metadata-fetch tests for the 4 supporting providers move to target the new method and
  still pass.
