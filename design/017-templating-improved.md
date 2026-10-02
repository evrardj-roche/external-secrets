```yaml
---
title: Resolved Templating and Target Writers
version: v1alpha1
authors: Jean-Philippe Evrard
creation-date: 2026-10-01
status: draft
---
```

# Resolved Templating and Target Writers

## Table of Contents

<!-- toc -->
// autogen please
<!-- /toc -->

## Summary

External Secrets templating currently mixes five responsibilities:

1. loading template text from inline fields, ConfigMaps, Secrets, and literals;
2. evaluating Go templates;
3. interpreting the rendered value or document;
4. mutating a Kubernetes object;
5. deciding how and where to persist the result.

The resulting execution is spread across the ExternalSecret controller, PushSecret
controller, esoctl, `pkg/controllers/templating`, and `runtime/template/v2`. It is hard
to determine which fields may be changed, which validation applies, and which exact
object reaches an external write boundary.

This proposal replaces that flow with three explicit stages:

```text
Build a target-specific renderer
    - validate and normalize configuration
    - load every referenced template source
    - construct typed render operations
                |
                v
Render a complete desired result
    - perform no Kubernetes, provider, or file reads
    - evaluate and place each operation
    - return a typed immutable-by-contract result
                |
                v
Write through a destination-specific writer
    - select the exact object or payload to persist
    - perform last-mile validation
    - write and return a typed result
```

The principal concrete flows are:

```text
ExternalSecret -> SecretRenderer   -> DesiredSecret   -> Kubernetes Secret writer
ExternalSecret -> ConfigMapRenderer-> DesiredConfigMap-> Kubernetes ConfigMap writer
ExternalSecret -> ManifestRenderer -> DesiredManifest -> Kubernetes manifest writer
PushSecret     -> PushSecretRenderer-> PushPayload     -> ProviderWriter
esoctl         -> SecretRenderer   -> DesiredSecret   -> YAMLSecretWriter
```

The types deliberately have matching shapes, but this design does not introduce one
universal renderer or writer interface. Interfaces are used only where implementations
are genuinely interchangeable, such as Secret create and update writers.

The security fixes that reject privileged Secret candidates are prerequisites to this
refactor. This design centralizes those checks at the final write boundaries; it does
not make the architectural migration a prerequisite for the security fix.

## Motivation

### Current execution is fragmented

The normal ExternalSecret path currently resembles:

```text
reconcile
  -> fetch existing Secret
  -> fetch provider data
  -> choose create/update helper
  -> mutate candidate
       -> initialize and clean managed fields
       -> load referenced templates through Parser
       -> execute templates through runtime/template/v2
       -> mutate data, labels, annotations, and type
       -> apply ownership and management metadata
       -> validate candidate
  -> Kubernetes create/update
```

Generic targets have different construction and rendering code. ConfigMap/Secret-backed
generic templates use a temporary Secret and, in some cases, evaluate template output a
second time. PushSecret and esoctl share the old Parser but have different input and
persistence behavior.

The main maintainability problems are:

- `Parser` combines source loading with mutation of `TargetSecret`;
- the low-level template engine accepts a `client.Object` and interprets destinations;
- source kind, scope, target kind, and destination are repeatedly interpreted through
  switches and conditionals;
- one common options structure would contain fields irrelevant to most targets;
- generic path capabilities can accidentally leak into typed Secret behavior;
- create/update mechanics and final candidate validation are difficult to audit as one
  write boundary;
- PushSecret uses a Kubernetes Secret as a transport object even though its result is a
  provider payload, not desired Kubernetes state.

### Readability requirement

The replacement must not merely move the existing conditionals into a larger
`Prepare` function or a magic `NewDesiredObject` factory.

The intended rule is:

> Interpret flexible API input once at a construction boundary. After construction,
> behavior is selected by concrete types and small target-specific values.

A few localized, exhaustive switches are preferable to registries or double-dispatch
machinery. In particular:

- one controller target switch selects the renderer constructor;
- one API translation function expands optional TemplateFrom source fields;
- one target-specific destination parser validates a closed destination set;
- one lifecycle switch selects create, update, delete, retain, or skip behavior.

Those switches must not reappear in source loaders, render loops, validators, and
writers.

### Placement must remain linear

[Issue #7037](https://github.com/external-secrets/external-secrets/issues/7037)
documents a regression where every rendered key passed through `setField`, which
converted the complete target object to unstructured and back. As the object grew, `n`
rendered keys caused O(n²) CPU and allocations. A 400-key Secret took approximately
126 ms and allocated 82 MB in the reported benchmark, versus approximately 1.9 ms and
0.8 MB with direct placement.

The replacement architecture must make that regression structurally difficult:

- Secret operations write directly to `Secret.Data`, `Labels`, or `Annotations`;
- ConfigMap operations write directly to typed ConfigMap maps;
- manifest operations mutate the existing `Unstructured.Object` through a parsed path;
- Document operations parse once and merge the resulting entries without converting or
  copying the complete candidate per entry;
- no placement operation converts the complete candidate to another representation for
  each rendered key.

For `n` rendered entries and fixed path depth `d`, placement must be O(n) for typed
targets and O(n × d) for manifests. Benchmarks and allocation-growth tests enforce this
invariant independently of the immediate fix proposed in PR #7046.

### Security invariant

The authoritative security boundary is the object actually passed to Kubernetes.
Admission and build-time checks remain defense in depth, but cannot account for retained
state or later writer transformations.

Immediately before every Kubernetes Secret create or update, ESO must validate:

- expected GVK, namespace, and name;
- bootstrap-token Secrets are forbidden;
- service-account-token Secrets carrying the service-account-name annotation are
  forbidden;
- typed Secret templates cannot use arbitrary manifest paths.

Explicit `manifest: {apiVersion: v1, kind: Secret}` and default Secret targets must use
the same `SecretRenderer` and Kubernetes Secret writers.

## Terminology

- **source object**: a ConfigMap or Secret containing selected template text;
- **template source**: a value that knows how to load one selected piece of text;
- **provider data**: values already fetched from providers or generators;
- **render operation**: a resolved Value or Document template with origin, destination,
  and decoding behavior;
- **renderer**: a target-specific object containing every input required by `Render`;
- **desired result**: a complete rendered object ready for a writer;
- **writer**: a destination-specific boundary that validates and persists a desired
  result or provider payload;
- **origin**: a safe structural spec path, never source text, rendered output, generated
  keys, or provider values;
- **canonical manifest destination**: a parsed path plus explicit placement and value
  typing semantics.

## Goals

1. Make the controller flow visibly Build, Render, Write.
2. Keep `Render()` pure with respect to Kubernetes, providers, and files.
3. Represent Values and KeysAndValues with separate concrete operation types.
4. Represent Secret, ConfigMap, manifest, and PushSecret results with target-specific
   types rather than a universal field bag.
5. Interpret source kind only while building the renderer.
6. Interpret destination strings only while building target-specific operations.
7. Validate the exact candidate immediately before every Kubernetes write.
8. Reuse Secret rendering between ExternalSecret and esoctl without coupling either to
   reconciliation.
9. Reuse source loading and evaluation in PushSecret while returning a provider-oriented
   `PushPayload`.
10. Keep lifecycle, deletion policy, status, retry, and provider acquisition visible in
    their controllers.
11. Preserve successful output, source ordering, merge precedence, and decoding except
    for the explicit compatibility changes in this document.
12. Keep each migration commit independently testable and reviewable.

## Constraints

- No CRD shape change.
- API modules must not import runtime or controller packages.
- Existing updates use one candidate deep copy; no copy per template operation.
- Selected source text is resolved before rendering.
- No arbitrary user- or provider-supplied mutation callbacks.
- No source text, provider data, generated key, or rendered output in public errors.
- No ordinary debug logging of `PushPayload` or rendered secret material.
- Existing provider interfaces remain compatible during the migration.

Two deliberate behavior changes are accepted:

1. esoctl YAML output applies the same final Secret validation as Kubernetes Secret
   persistence;
2. equivalent manifest destinations are canonicalized to identical placement, typing,
   and decoding behavior.

All other observable changes require a separate compatibility decision.

## Non-goals

- Combining all configured templates into one Go template program.
- Fetching referenced source objects from admission.
- Moving provider-data acquisition into the templating package.
- Moving deletion policy, generator lifecycle, status, or requeue behavior into a
  renderer.
- Creating one generic options structure for every target and consumer.
- Introducing a universal generic `Renderer[T]` or `Writer[T, R]` abstraction before
  implementations demonstrate meaningful shared behavior.
- Logging provider payloads from controllers.
- Removing generic-target double evaluation in this migration.

## Architecture

### ExternalSecret lifecycle and target routing

Target description first selects a named target-specific reconciliation path:

```go
descriptor, err := describeTarget(es)
if err != nil {
    return err
}

switch descriptor.Kind {
case TargetSecret:
    return reconcileSecretTarget(ctx, descriptor, es, existing, providerData)
case TargetConfigMap:
    return reconcileConfigMapTarget(ctx, descriptor, es, existing, providerData)
case TargetManifest:
    return reconcileManifestTarget(ctx, descriptor, es, existing, providerData)
}
```

There is no universal `NewDesiredObject` constructor. The routing switch is visible at
the controller boundary, and each branch uses concrete renderers and writers.

Within a target-specific path, the controller performs lifecycle decisions that do not
require rendering and constructs the corresponding writer inside that decision:

```go
action, err := decideSecretAction(es, existing, providerData)
if err != nil {
    return err
}

var writer target.KubernetesSecretWriter

switch action {
case target.ActionDelete:
    return deleteSecret(ctx, es, existing)
case target.ActionRetain:
    return finishRetained(es)
case target.ActionSkip:
    return finishSkipped(es)
case target.ActionWait:
    return finishMissing(es)
case target.ActionCreate:
    writer = target.NewSecretCreateWriter(client, createOptions)
case target.ActionUpdate:
    writer = target.NewSecretUpdateWriter(client, updateOptions)
}

renderer, err := target.BuildSecretRenderer(ctx, renderInput)
if err != nil {
    return err
}

desired, err := renderer.Render()
if err != nil {
    return err
}

result, err := writer.Write(ctx, desired)
if err != nil {
    return handleSecretWriteError(err)
}
return handleSecretWriteResult(result)
```

ConfigMap and manifest target paths use the same visible lifecycle shape with their own
concrete renderer and writer types. Deletion and retention do not mutate the target
through templating. If a future backend needs delete preconditions, they belong to
deletion persistence logic.

`describeTarget` classifies:

- no manifest as Secret;
- explicit core/v1 Secret as Secret;
- explicit core/v1 ConfigMap as ConfigMap;
- every other allowed GVK as Manifest.

Rendered fields never select another target kind.

### Build is the I/O boundary

A builder may perform source I/O. Its returned renderer may not.

```text
BuildSecretRenderer
  - translates API fields
  - validates static target capabilities
  - constructs TemplateSource values
  - calls Source.Load for every selected source
  - parses destinations
  - constructs typed render operations containing actual text
  - stores provider data and one base-object snapshot
  -> *SecretRenderer

SecretRenderer.Render
  - performs no ConfigMap, Secret, provider, or file reads
```

Using `Build` rather than `New` signals that construction can fail after external reads.
Provider data and the existing target are passed into the builder; the builder does not
fetch them.

### Template sources

A source represents only where one piece of template text comes from:

```go
type TemplateSource interface {
    Load(context.Context, SourceReader) (ResolvedText, error)
}

type ResolvedText struct {
    Origin TemplateOrigin
    Text   string
}
```

The minimal concrete source set is:

```go
type StaticSource struct {
    Origin TemplateOrigin
    Text   string
}

type ConfigMapKeySource struct {
    Origin    TemplateOrigin
    Namespace string
    Name      string
    Key       string
}

type SecretKeySource struct {
    Origin    TemplateOrigin
    Namespace string
    Name      string
    Key       string
}
```

Inline values and literals both use `StaticSource`. Scope, destination, destination key,
and decoding do not belong to source types.

`SourceReader` allows different loading environments:

```go
type SourceReader interface {
    ConfigMapValue(context.Context, types.NamespacedName, string) (string, error)
    SecretValue(context.Context, types.NamespacedName, string) ([]byte, error)
}
```

Implementations include:

- `KubernetesSourceReader` for controllers;
- `FileSourceReader` or preloaded-object reader for esoctl;
- fakes for source contract tests.

The API translation boundary preserves the current non-exclusive TemplateFrom behavior:

```go
if from.ConfigMap != nil {
    // append one ConfigMapKeySource per item
}
if from.Secret != nil {
    // append one SecretKeySource per item
}
if from.Literal != nil {
    // append one StaticSource
}
```

One TemplateFrom entry may therefore produce ConfigMap, then Secret, then literal
sources. These presence checks are necessary for compatibility and must remain localized
to source construction.

After `Load`, render operations retain only selected text and safe metadata. Complete
source objects are discarded.

### Safe origin

```go
type TemplateOrigin struct {
    SpecPath string
}
```

Examples:

```text
spec.target.template.data
spec.target.template.metadata.labels
spec.target.template.templateFrom[0].configMap.items[1]
spec.template.templateFrom[2].secret.items[0]
```

Inline map keys and rendered keys are not included automatically because they may be
sensitive.

### Pure evaluator

The low-level v2 engine has one responsibility:

```go
type Evaluator interface {
    Evaluate(text string, data map[string][]byte) ([]byte, error)
}
```

It parses and executes Go template text. It does not accept scope, destination,
decoding, or a Kubernetes object.

The existing engine API remains behind adapters until every caller migrates.

## Typed render operations

Values and KeysAndValues are separate concrete types. Scope is not interpreted by a
switch in the render loop.

### Secret operations

```go
type SecretTemplate interface {
    RenderInto(*corev1.Secret, Evaluator, map[string][]byte) error
}

type SecretValueTemplate struct {
    Origin      TemplateOrigin
    Text        string
    Key         string
    Destination SecretDestination
    Decoding    esv1.ExternalSecretDecodingStrategy
}

type SecretDocumentTemplate struct {
    Origin      TemplateOrigin
    Text        string
    Destination SecretDestination
    Decoding    esv1.ExternalSecretDecodingStrategy
}
```

Secret destinations use a closed normalized value rather than polymorphic one-method
types:

```go
type SecretDestination uint8

const (
    SecretData SecretDestination = iota
    SecretLabels
    SecretAnnotations
)
```

The builder parses empty/Data/Labels/Annotations case-insensitively. It rejects dotted
paths. Rendering uses one localized exhaustive placement switch shared by value and
document operations. That switch writes directly to the candidate's typed maps; it must
not call an unstructured converter.

A Values operation evaluates and decodes one value, then places it at its known key. A
Document operation evaluates and parses YAML once, decodes each generated value, and
places each entry through the same direct destination function.

### ConfigMap operations

ConfigMap uses matching concrete types and a closed destination value:

```go
type ConfigMapTemplate interface {
    RenderInto(*corev1.ConfigMap, Evaluator, map[string][]byte) error
}

type ConfigMapValueTemplate struct { /* resolved fields */ }
type ConfigMapDocumentTemplate struct { /* resolved fields */ }
```

Typed ConfigMap behavior does not inherit arbitrary manifest paths. Its operations write
directly to typed maps and never round-trip the complete ConfigMap through unstructured
conversion.

### Manifest operations and `ManifestDestination`

Manifest destinations require more than a string:

```go
type ManifestDestination struct {
    Path      ParsedPath
    Placement ManifestPlacement
    ValueType ManifestValueType
}

type ManifestPlacement uint8

const (
    PlaceValueAtPath ManifestPlacement = iota
    MergeDocumentAtPath
)

type ManifestValueType uint8

const (
    PreserveYAMLType ManifestValueType = iota
    RequireString
    RequireBytes
)
```

`ManifestDestination` is created and validated during `BuildManifestRenderer`:

```text
API target + scope + known key + decoding
                    |
                    v
        buildManifestDestination
                    |
                    v
ParsedPath + Placement + ValueType
                    |
                    v
stored in ManifestValueTemplate / ManifestDocumentTemplate
```

It is used during pure `Render()` only for placement. Rendering does not parse target
strings, decide aliases, or inspect scope enums. Placement walks and mutates the existing
`candidate.Object` maps and slices in place. It must not convert, serialize, or deep-copy
the complete candidate per operation or generated key.

```go
type ManifestTemplate interface {
    RenderInto(
        *unstructured.Unstructured,
        Evaluator,
        map[string][]byte,
    ) error
}

type ManifestValueTemplate struct {
    Origin      TemplateOrigin
    Text        string
    Destination ManifestDestination
    Decoding    esv1.ExternalSecretDecodingStrategy
}

type ManifestDocumentTemplate struct {
    Origin      TemplateOrigin
    Text        string
    Destination ManifestDestination
    Decoding    esv1.ExternalSecretDecodingStrategy
}
```

### Canonical manifest destinations

The builder canonicalizes equivalent final paths using scope and the known Values key.
It does not merely replace `Labels` with `metadata.labels`.

Examples:

```text
Values + Labels + key "team"
Values + metadata.labels.team
  -> Path: metadata.labels.team
  -> Placement: PlaceValueAtPath
  -> ValueType: RequireString

KeysAndValues + Labels
KeysAndValues + metadata.labels
  -> Path: metadata.labels
  -> Placement: MergeDocumentAtPath
  -> ValueType: RequireString

Values + Annotations + key "owner"
Values + metadata.annotations.owner
  -> Path: metadata.annotations.owner
  -> Placement: PlaceValueAtPath
  -> ValueType: RequireString

Values + spec.enabled
  -> Path: spec.enabled
  -> Placement: PlaceValueAtPath
  -> ValueType: PreserveYAMLType
```

The canonical model guarantees that equivalent final destinations have identical:

- decoding behavior;
- scalar typing;
- document merge behavior;
- error classification.

Metadata labels and annotations always require strings, regardless of spelling.
Arbitrary custom-resource paths preserve YAML scalar and document types unless their
schema-specific rule says otherwise.

This intentionally changes edge behavior where generic paths currently preserve a
boolean/number under metadata, ignore a Values source key, or skip per-entry decoding.
Such configurations may change from API rejection to successful canonical output, fail
earlier, or produce corrected decoded values. Characterization tests and release notes
must identify those changes.

## Target-specific renderers

### SecretRenderer

```go
type SecretRenderer struct {
    base         *corev1.Secret
    providerData map[string][]byte
    templates    []SecretTemplate
    evaluator    Evaluator
    expected     SecretIdentity
    options      SecretRenderOptions
}

type DesiredSecret struct {
    candidate *corev1.Secret
    expected  SecretIdentity
}
```

`DesiredSecret` represents a rendered present object. It contains no create/update/delete
intent. The controller has already selected lifecycle behavior.

`Render()`:

1. creates a new candidate or makes one deep copy of the existing snapshot;
2. applies current merge and managed-key behavior;
3. applies type, immutable, metadata, finalizers, ownership, and management metadata;
4. evaluates each `SecretTemplate` in order;
5. publishes `DesiredSecret` only after complete success.

The candidate is private by default. Writers receive an accessor or internal view and
validate the exact object they persist.

### ConfigMapRenderer

`ConfigMapRenderer` and `DesiredConfigMap` have the same visible shape but use typed
ConfigMap operations and validation. They do not satisfy a universal renderer interface
merely for uniformity.

### ManifestRenderer

`ManifestRenderer` owns an unstructured candidate, canonical manifest operations, and
expected identity. Only this renderer applies arbitrary parsed paths.

Final desired validation rejects changes to expected GVK, namespace, or name even if a
path operation reached identity fields.

### Build and render failure behavior

- Source lookup or missing selected item fails during Build; no renderer is returned.
- Evaluation or placement fails during Render; no desired result is returned.
- Render mutates one isolated candidate, so partial in-memory work is discarded without
  copying per operation.

## PushSecret rendering and the v2 provider boundary

PushSecret has two input resources:

1. the PushSecret manifest, which defines mappings, policies, metadata, and stores;
2. the selected or generated Secret, which supplies data and metadata to render and
   push.

`BuildPushSecretRenderer` resolves referenced template sources and captures deep copies
of both inputs. `Render()` templates a copy of the source Secret and returns both
resources as one immutable-by-contract payload:

```go
type PushPayload struct {
    pushSecret *esv1alpha1.PushSecret
    secret     *corev1.Secret
}

func (p *PushPayload) PushSecret() *esv1alpha1.PushSecret {
    return p.pushSecret.DeepCopy()
}

func (p *PushPayload) Secret() *corev1.Secret {
    return p.secret.DeepCopy()
}
```

This does not invent another data or metadata schema. The existing API objects already
contain all provider-generic information.

`ProviderWriter` combines them with resolved stores and provider clients:

```go
func (w *ProviderWriter) Write(
    ctx context.Context,
    payload *PushPayload,
) (PushResult, error)
```

It owns:

- store-specific `dataTo` filtering;
- expansion and bundle construction;
- conversion and selected-key handling;
- update-policy checks;
- calls to the existing `SecretsClient.PushSecret` interface;
- partial synchronization results.

This payload is the intended provider-v2 boundary. A future provider API can consume
`PushPayload` or a smaller resolved entry rather than receiving a Kubernetes Secret as
an unexplained transport type. During migration, `payload.Secret()` supplies the
existing `*corev1.Secret` argument.

A diagnostic writer may serialize a PushPayload only through explicit CLI/test output.
It shows provider-generic input, not necessarily the final provider SDK request.

## Writers and final validation

### Kubernetes Secret writers

Create and update writers are genuinely interchangeable and share a target-specific
interface:

```go
type KubernetesSecretWriter interface {
    Write(
        context.Context,
        *DesiredSecret,
    ) (KubernetesWriteResult, error)
}

type SecretCreateWriter struct { /* client and create options */ }
type SecretUpdateWriter struct { /* client, existing snapshot, update options */ }
```

The controller constructs the writer inside its lifecycle switch. The common
Build/Render/Write path then receives the selected implementation.

```go
type KubernetesWriteResult uint8

const (
    WriteCreated KubernetesWriteResult = iota
    WriteUpdated
    WriteUnchanged
    WriteMetadataOnly
)
```

The result lets the controller own Events, status, and requeue behavior without
injecting those responsibilities into writers.

The create writer validates immediately before `Create`.

The update writer:

1. compares existing and desired state;
2. returns `WriteUnchanged` when appropriate;
3. selects a full update or exact immutable metadata-only candidate;
4. validates that exact candidate;
5. calls `Update`;
6. returns the corresponding typed result.

Conflict and safe API-error classification remain available to controller error
handling.

ConfigMap and manifest writers follow the same reviewable shape, but no universal
generic writer interface is required.

### Shared Secret validation

There is one mandatory function for outputs intended to behave like Kubernetes Secret
persistence:

```go
func ValidateDesiredSecret(
    desired *DesiredSecret,
    candidate *corev1.Secret,
) error
```

It validates expected identity and privileged Secret policy. It is not supplied as an
optional functional argument.

Both Kubernetes Secret writers and `YAMLSecretWriter` call this function. Contract tests
run the same forbidden and allowed candidates through:

- create;
- update;
- immutable metadata-only update;
- YAML output.

Explicit manifest Secret targets use the same DesiredSecret and writers, so they cannot
drift into separate validation.

### YAML writer

The esoctl writer deliberately resembles Kubernetes writers without being forced into
the Kubernetes result interface:

```go
type YAMLSecretWriter struct {
    Output io.Writer
}

func (w *YAMLSecretWriter) Write(
    context.Context,
    *DesiredSecret,
) error
```

It validates with `ValidateDesiredSecret`, marshals, and writes. This is an intentional
behavior change: esoctl no longer emits a Secret that the controller would reject.

### No separate Validate stage

There is no controller-visible `desired.Validate()` stage. Preflight validation may be
used in tests, but authoritative validation is inline in each writer immediately before
the external write.

## Ordering and compatibility

Builders preserve application order:

1. existing fields are retained only when current creation policy requires it;
2. provider data is seeded according to current merge semantics;
3. TemplateFrom entries are processed in list order;
4. within one entry, ConfigMap, Secret, then literal sources are expanded;
5. referenced items remain in item-list order;
6. inline data follows TemplateFrom and wins collisions;
7. inline labels and annotations follow TemplateFrom;
8. ownership and ESO management metadata are complete before final write validation.

Inline Go maps have no meaningful iteration order. Their containing field controls
precedence; keys are not added to public origin information.

### Generic double evaluation

Current ConfigMap/Secret-backed generic rendering can evaluate text twice. For example,
provider data:

```text
first = {{ .second }}
second = final-value
```

and source text:

```gotemplate
{{ .first }}
```

produce `{{ .second }}` in the first pass and `final-value` in the second. A single pass
would leave `{{ .second }}` literal.

The migration preserves this with a dedicated manifest-only operation:

```go
type ManifestTwoPassTemplate struct {
    Origin      TemplateOrigin
    Text        string
    Destination ManifestDestination
    Decoding    esv1.ExternalSecretDecodingStrategy
}
```

Only source/scope combinations that currently use temporary Secret staging receive this
operation. The main manifest render loop does not branch on source kind. Removing the
second pass requires a separate compatibility proposal.

### Explicit compatibility changes

#### Canonical manifest destinations

Equivalent final paths now use identical placement rules. This can change malformed or
unusual generic-target configurations from late API rejection to success, move an error
earlier, coerce metadata values to required strings, or apply decoding that was
previously skipped.

#### esoctl final validation

esoctl rejects privileged Secret candidates that it previously printed. This improves
parity with controller behavior but may break scripts using esoctl for administrative
rendering. Release notes must call out the change.

## Error and provenance handling

Public errors identify fixed stages and safe origins:

```text
build/source resolution at spec.target.template.templateFrom[1].secret.items[0]
render/evaluation at spec.target.template.data
render/placement at spec.target.template.templateFrom[2].literal
write/final validation
write/Kubernetes API
```

They must not include:

- source text or selected source values;
- provider data;
- rendered bytes;
- generated keys;
- arbitrary paths unless separately classified safe;
- raw API response bodies that may echo submitted data.

Concrete errors retain private causes for classification and tests.

## Package boundaries

The final package layout may evolve incrementally, but responsibilities are:

- `runtime/template/v2`: pure Go template evaluation and template functions;
- `pkg/controllers/templating`: shared source loaders, origins, and resolved operation
  mechanics used by ExternalSecret, PushSecret, and esoctl;
- `pkg/controllers/externalsecret/target`: ExternalSecret target renderers, desired
  results, Kubernetes writers, final target validation, and write results;
- `pkg/controllers/pushsecret`: PushSecret builder adapter, PushPayload, ProviderWriter,
  and provider synchronization result handling;
- `cmd/esoctl`: file source reader and YAML writers;
- API modules: CR types and static admission validation only.

Stable pure mechanics may move toward runtime after callers converge. Migration commits
should not begin with package moves.

## Performance and memory

- Updates make one candidate deep copy; creates allocate one candidate.
- No copy or whole-object conversion is made per render operation or generated key.
- Typed target placement writes directly to the candidate maps.
- Manifest placement mutates `Unstructured.Object` directly through pre-parsed paths.
- Document output is parsed once and merged without one object conversion per entry.
- Build retains selected template text, not complete source objects.
- Render evaluates and places one operation at a time and does not retain all rendered
  output.
- PushPayload owns one deep copy of each input resource and returns copies from accessors.
- A Build-local source cache may deduplicate repeated reads, but only after measurement.

Benchmarks cover:

- 50, 100, 200, and 400 Values entries for Secret, ConfigMap, and manifest targets;
- KeysAndValues documents producing the same entry counts;
- repeated and distinct referenced source objects;
- near-limit Secret sizes;
- canonical manifest paths and nested arrays;
- generic double evaluation;
- candidate copies, allocations, source reads, and evaluation count.

Doubling entry count must produce approximately linear allocation growth. Regression
tests compare 100 and 400 entries and reject the approximately 16× growth characteristic
of issue #7037. Thresholds must allow normal test variance without permitting quadratic
behavior.

## Incremental implementation plan

The migration uses vertical slices. Characterization precedes routing changes, and every
commit compiles and passes relevant Makefile tests. A commit approaching a couple of
hundred changed lines must be split. Non-trivial commits receive git notes.

### Commit 1: characterize existing Secret rendering

Tests only:

- TemplateFrom and item order;
- inline precedence;
- Values and KeysAndValues;
- decoding;
- merge and no-template behavior;
- retained existing fields;
- candidate-copy count.

Split controller-level behavior from low-level engine tables if needed.

### Commit 2: characterize generic, PushSecret, and esoctl behavior

Prefer separate commits per consumer:

- generic nested paths and temporary-Secret double evaluation;
- current well-known versus dotted metadata destination behavior;
- PushSecret data, type, labels, annotations, mappings, and partial results;
- esoctl output and source-file loading.

These tests identify the two explicitly accepted behavior changes.

### Commit 3: introduce pure text evaluation

- add `Evaluator.Evaluate(text, data)` or equivalent function;
- make the old engine call it through an adapter;
- preserve existing public engine behavior;
- add parse/execution error redaction tests.

No controller routing changes.

### Commit 4: introduce source loading contracts

- add `TemplateSource`, `ResolvedText`, and `SourceReader`;
- add Static, ConfigMap-key, and Secret-key sources;
- add Kubernetes and fake readers;
- test safe origins, missing values, and ordering;
- keep Parser in production.

### Commit 5: introduce typed Secret operations

Prefer two commits:

1. Secret destination parsing and `SecretValueTemplate`;
2. `SecretDocumentTemplate`, decoding, and generated-entry tests.

Operations use pure evaluation and direct typed Secret placement. Add a many-key
allocation-growth regression derived from issue #7037. No controller switch yet.

### Commit 6: build and render DesiredSecret in shadow tests

- add `BuildSecretRenderer` around already-fetched provider/existing input;
- resolve all sources during Build;
- add `SecretRenderer.Render` and `DesiredSecret`;
- preserve one-copy construction, merge, metadata, ownership, and management behavior;
- compare output with the existing path without routing production writes.

Split static sources, referenced sources, and metadata behavior if required.

### Commit 7: add Kubernetes Secret writers

Prefer independent commits:

1. `SecretCreateWriter`, final validation, and `WriteCreated`;
2. `SecretUpdateWriter`, equality, full updates, and conflict behavior;
3. immutable metadata-only updates and exact-candidate validation;
4. shared writer contract tests.

No universal writer abstraction is introduced.

### Commit 8: switch the ExternalSecret Secret lane

- retain lifecycle and provider acquisition in the controller;
- select create/update writer in the action switch;
- route default and explicit manifest Secrets through SecretRenderer;
- preserve status, Events, conflict requeue, and ownership errors;
- remove only dead Secret-lane Parser calls.

Split explicit manifest Secret routing if needed.

### Commit 9: add YAMLSecretWriter and migrate esoctl

- add the file/preloaded SourceReader;
- build the same SecretRenderer shape;
- write DesiredSecret through YAMLSecretWriter;
- apply shared final Secret validation;
- document and test rejection of privileged output.

### Commit 10: introduce PushSecretRenderer and PushPayload

Prefer two commits:

1. build and render a source Secret using shared source/evaluation mechanics;
2. return immutable PushPayload containing deep-copied PushSecret and rendered Secret.

Shadow-test output against current `applyTemplate` before routing changes.

### Commit 11: introduce ProviderWriter and migrate PushSecret

- move data/dataTo expansion and provider-write orchestration behind ProviderWriter;
- consume PushPayload;
- preserve store filtering, update policy, locks, partial state, and cleanup behavior;
- retain the current provider `PushSecret(*corev1.Secret, PushSecretData)` adapter;
- switch PushSecret and remove its Parser path.

Split dataTo expansion from routing if needed.

### Commit 12: add typed ConfigMap renderer and writers

- add closed ConfigMap destinations and direct typed Value/Document placement;
- add a many-key linear-allocation regression;
- add Build, Render, DesiredConfigMap, create writer, and update writer;
- route explicit core/v1 ConfigMap targets through the typed lane;
- preserve feature gating, lifecycle, metadata, and ownership.

Use separate commits for rendering and persistence if necessary.

### Commit 13: add canonical ManifestDestination

- parse paths during Build;
- normalize scope, known Values key, placement, and value typing;
- make equivalent metadata destinations identical;
- add characterization-to-canonical change tests;
- document intentional behavior changes;
- do not switch production routing yet.

### Commit 14: add ManifestRenderer operations

Prefer small commits:

1. manifest Value operation with in-place unstructured path placement;
2. manifest Document operation with one parse and direct merge;
3. canonical metadata typing and decoding;
4. identity protection and DesiredManifest;
5. many-key allocation-growth tests for top-level and nested paths.

### Commit 15: preserve generic double evaluation

- add `ManifestTwoPassTemplate`;
- preserve source-specific compatibility at Build time;
- remove temporary typed Secret staging;
- prove the practical nested-template example remains unchanged.

### Commit 16: add manifest writers and switch routing

- add create/update writers and write results;
- validate exact GVK/name/namespace before API calls;
- switch custom manifest reconciliation;
- preserve status, Events, informer registration, and conflict behavior.

### Commit 17: remove compatibility APIs

One dead API family per commit:

- old Parser Merge methods;
- `ExecFunc` and object-mutating engine adapters;
- temporary Secret generic staging;
- dead path application from typed Secret and ConfigMap lanes.

Keep manifest path parsing and two-pass compatibility.

### Commit 18: benchmark and optimize measured regressions

Only measured changes are accepted, such as:

- request-local source-object caching;
- reduced byte/string conversion;
- reduced unstructured conversion;
- batched document placement.

Optimization must not move validation away from the exact write boundary.

### Commit validation

After each commit, run the following.

```sh
make test
make check-diff
```

### Final validation

If all commits have passed their own test and check-diff, the
final validation should only run `make check-diff` (another
time after last commit basically) to guarantee everything is
fine.

## Acceptance criteria

### Architecture

- Build functions resolve all template source I/O.
- Render methods perform no Kubernetes, provider, or file reads.
- ExternalSecret target routing selects named target-specific builders.
- Values and Documents are separate concrete operation types.
- Secret and ConfigMap destinations are closed normalized values.
- Only ManifestRenderer uses parsed arbitrary paths.
- `ManifestDestination` is created during Build and consumed during Render.
- DesiredSecret contains no persistence intent.
- The controller selects SecretCreateWriter or SecretUpdateWriter in its lifecycle
  switch.
- PushSecret returns PushPayload containing the PushSecret and rendered source Secret.
- No universal generic renderer/writer framework is required.

### Security and validation

- Default and explicit manifest Secrets use the same SecretRenderer and Kubernetes
  writers.
- The exact create, full-update, or metadata-only candidate is validated immediately
  before the API call.
- GVK, namespace, and name cannot change during rendering.
- Forbidden bootstrap and service-account token candidates are never persisted.
- Typed Secret and ConfigMap targets cannot use dotted paths.
- YAMLSecretWriter uses the same final Secret validation.
- PushSecret is not subjected to Kubernetes token-creation policy merely because its
  compatibility transport is a Secret.

### Compatibility

- Existing successful Secret order, precedence, decoding, merge, and metadata behavior
  remain unchanged.
- Multi-source TemplateFrom entries retain ConfigMap, Secret, literal order.
- Generic double evaluation remains compatible.
- PushSecret provider mappings, partial results, and source metadata remain compatible.
- Canonical manifest and esoctl validation changes are documented and tested.

### Maintainability

- Adding a source requires a new TemplateSource implementation and one API-boundary
  construction case, not edits to every renderer.
- Adding a typed destination changes only that target's parser and placement function.
- Adding a target requires a renderer, desired result, writers, validation, and one
  controller routing case.
- Writer results keep Events/status/requeue behavior in controllers.
- Public errors expose safe stage and origin only.

### Performance

- One candidate copy per update and no redundant create copy.
- No per-operation or per-generated-key deep copy or whole-object conversion.
- Typed placement writes directly to Secret and ConfigMap maps.
- Manifest placement mutates `Unstructured.Object` in place through parsed paths.
- Placement remains linear in rendered entry count, with allocation-growth regressions
  covering issue #7037 for Values and KeysAndValues.
- Selected source text, not full source objects, survives Build.
- Benchmarks record source reads, evaluations, candidate copies, allocations, and bytes.

## Alternatives

### One universal PreparedRender

A universal structure needs target class, client, scheme, existing `client.Object`,
provider data, source text, write intent, ownership, type, immutable, and manifest path
fields. Most combinations are invalid and require repeated switches. Target-specific
renderers make those combinations unrepresentable or local.

### One magic renderer factory

A single `NewRenderer` hides target classification and makes new target behavior harder
to trace. One visible controller switch calling named builders is more maintainable.

### One large Preparer

Fetching provider data, existing targets, template sources, files, and generator state in
one object couples controllers and esoctl. Builders load only template sources and
accept already-resolved consumer data.

### Polymorphic destination types

One implementation per Data/Labels/Annotations destination removes a small switch but
adds many trivial types and indirection. Closed typed targets use normalized values and
one localized placement switch. Manifest paths use a richer explicit value because they
have meaningful semantics.

### Universal generic Renderer and Writer interfaces

Generics can pair `Renderer[T]` and `Writer[T, R]`, but runtime target routing still
requires concrete branches and each result has different behavior. Introduce an
interface only where implementations are actually interchangeable, such as Secret
create/update writers.

### Desired result with write intent

Create/update/delete intent duplicates lifecycle decisions and permits invalid states
such as a delete intent with a rendered candidate. Desired results represent present
rendered state; controllers select lifecycle and writers.

### Controller-only write helpers

Keeping all mechanics in `r.createSecret`/`r.updateSecret` makes persistence visible but
continues accumulating unrelated Reconciler helpers. Small target writer types preserve
the visible lifecycle while isolating validation and API mechanics.

### Apply Kubernetes privileged policy to PushSecret

PushSecret does not create the source Kubernetes Secret, and external providers do not
interpret Kubernetes service-account token semantics. Applying this policy would block
legitimate exports without closing a Kubernetes privilege boundary.

### Remove generic double evaluation

A single pass is simpler but changes output when first-pass values contain template
expressions. The behavior remains isolated in `ManifestTwoPassTemplate` until separately
changed.

### Preserve syntax-specific manifest paths

Keeping `Labels` and `metadata.labels` on separate mechanics preserves edge behavior but
retains inconsistent typing and decoding for the same final field. This design accepts
the compatibility impact of canonical destinations because the explicit model is easier
to test, secure, and extend.
