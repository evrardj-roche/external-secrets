# A Real Sentinel For "Not Supported"

## Context

AGENTS.md already instructs provider authors: *"Read-only providers still implement Push/Delete
but return a sentinel error! Do NOT return nil!"* No such sentinel exists in
`apis/externalsecrets/v1`. `provider.go` defines `NoSecretErr`/`NotModifiedErr` as proper typed
sentinels (checkable via `errors.Is`), but "this provider doesn't support this operation" has none.
Every provider that needed to say it invented its own string:

- `"not implemented"` — repeated verbatim, each with its own local `errNotImplemented` constant,
  in at least 15 providers (`beyondtrust`, `chef`, `crd`, `dvls`, `gitlab`, `ibm`, `passbolt`,
  `previder`, `webhook`, `volcengine`, and others).
- `"pushing secrets is currently not supported"` (`fortanix`, `pulumi`, worded slightly
  differently in each).
- `"not implemented - this provider supports write-only operations"` (`github`).
- `fmt.Errorf(errNotImplemented, "PushSecret")` (`passworddepot`, parameterized differently again).

None of these are distinguishable by calling code. There is no way today to write
`errors.Is(err, someSentinel)` and learn "this failed because the provider doesn't support the
operation" as opposed to any other failure.

## Goals

- One typed, exported error in `apis/externalsecrets/v1` that means exactly "this provider does
  not support this operation," checkable via `errors.Is`.
- Make it carry which operation, for error messages and metrics, without needing a new type per
  operation.

## Non-Goals

- Not retrofitting every existing ad hoc string into the new type as a mechanical rename. design/017
  already deletes the majority of these call sites outright (a provider that doesn't implement
  `SecretPusher` no longer has a `PushSecret` method to return anything from). This sentinel is for
  the cases design/017 leaves behind: providers that do implement an interface but must reject a
  specific unsupported request shape at call time (see below).

## Proposal

```go
// apis/externalsecrets/v1/provider.go

// ErrNotSupported is a sentinel for errors.Is checks against NotSupportedError.
var ErrNotSupported = NotSupportedError{}

// NotSupportedError indicates a provider does not support the requested operation or a specific
// variant of it (e.g. GetAllSecrets by tag, when it only supports GetAllSecrets by name).
type NotSupportedError struct {
	Operation string
	Reason    string // optional, e.g. "find.tags is not supported"
}

func (e NotSupportedError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s: not supported: %s", e.Operation, e.Reason)
	}
	return fmt.Sprintf("%s: not supported", e.Operation)
}

func (e NotSupportedError) Is(target error) bool {
	_, ok := target.(NotSupportedError)
	return ok
}
```

## Where this is actually used, after design/017

design/017 removes the whole-method "not implemented" case: a provider that never implements
`SecretPusher`/`SecretsFinder`/etc. simply doesn't have that method, and the controller's own type
assertion produces `esv1.NotSupportedError{Operation: "PushSecret"}` directly at the 6 call sites
listed there — no provider code involved.

What's left, and what this sentinel is actually for, is **partial support within an implemented
method** — cases where a provider does implement, say, `SecretsFinder`, but must reject one
specific request shape:

- `vault`: `errUnsupportedMetadataKvVersion` when `MetadataPolicy: Fetch` is requested against a
  KV v1 store (design/020 covers this family in more depth).
- `gitlab`: `errPathNotImplemented` — `find.path` isn't supported, only `find.tags`/`find.name`.
- `keepersecurity`, `onepassword`: `find.tags`/`find.path`/`remoteRef.version` unsupported variants.
- `openbao`: `"tag based search is not implemented"`.

These become:

```go
return nil, esv1.NotSupportedError{Operation: "GetAllSecrets", Reason: "find.path is not supported by GitLab"}
```

## Behavior

- `errors.Is(err, esv1.ErrNotSupported)` works uniformly, whether the error came from a design/017
  type-assertion failure or a provider's own partial-support rejection.
- Error messages keep their provider-specific detail via `Reason`, so nothing is lost for users
  reading logs/events compared to today's hand-written strings.

## Drawbacks

- `NotSupportedError.Operation` is a free-form string, not an enum — a typo in a provider's
  `Operation` value doesn't fail to compile. Acceptable: it's informational (for messages/metrics),
  not something dispatch branches on.

## Alternatives

- **A distinct error type per operation** (`PushNotSupportedError`, `FindNotSupportedError`, ...).
  Rejected: more types for no behavioral gain over one struct with an `Operation` field.
- **Keep ad hoc strings, document the convention better.** Rejected: doesn't make the failure
  reason checkable by calling code, which is the actual gap.

## Acceptance Criteria

- `esv1.NotSupportedError`/`ErrNotSupported` exist and are used by design/017's 6 call sites.
- The partial-support cases listed above (vault, gitlab, keepersecurity, onepassword, openbao, and
  any others found during implementation) are migrated to wrap `NotSupportedError` instead of a
  bare string.
- A test confirms `errors.Is(err, esv1.ErrNotSupported)` for both a design/017 type-assertion
  failure and a provider's own partial-support rejection.
