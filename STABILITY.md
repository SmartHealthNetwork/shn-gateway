# Stability and versioning

## Versioning policy

`shn-gateway` follows semantic versioning. The module is currently **pre-1.0
(0.x)**:

- **MINOR** versions (0.x.0 → 0.(x+1).0) may carry breaking changes. Each
  breaking change is called out in the release changelog.
- **PATCH** versions (0.x.y → 0.x.(y+1)) contain backwards-compatible fixes
  only.

A published version tag is **never re-tagged** with different content. The Go
module proxy caches a tag's tree permanently; always bump to a new version
rather than moving an existing tag.

This gateway requires `shn-sdk` — see `go.mod` for the pinned version.

## Native PAS response contract

PAS operation responses must be complete Bundles containing exactly one
ClaimResponse and a closed set of referenced resources. **Every decision a payer
gives is relayed as the payer gave it**: the gateway does not poll its own payer
for a later decision, does not replace a payer's answer with one it assembled,
and stamps no contract line of its own on a message it did not produce. A payer
that pends answers `pended`, and the requester obtains the determination by
asking for it (`Claim/$inquire`, leg type `pas-claim-inquire`).

**Breaking in this release** (the assembly path is gone):

- `engine.WithPendReQuery` is REMOVED. It configured the pend re-query the poll
  performed; there is no poll.
- `engine.LegResult.ResponseAssembled` is REMOVED. A partner `LegResponder` that
  set it no longer compiles. It marked an answer this gateway had assembled, and
  a relayed answer is never one — delete the assignment; nothing replaces it.
- The `leg.assembled` observer event is REMOVED. Nothing assembles a terminal PAS
  response, so nothing emits it. Consumers should drop the case; no event takes
  its place (`leg.response` already records the answer that was relayed).
- `engine.PASWaitDefault` is now `0` (was 30s). An originator route waits only when
  its caller asks it to. A caller that relied on the default to turn a pend into a
  determination now receives the pend and its continuation, and asks.
- `engine.PASWaitMax` is now 30s (was 120s). These still compile, so the change is
  silent at build time: a caller asking for more than 30s is capped rather than
  refused. Nothing is lost by it — the inquiry schedule always finished near 26s, so
  every wait above that was already a no-op that reported itself as a longer one.
  The bound and the schedule are now derived from each other and held together by a
  test, so they cannot drift apart again.

## Observational source certification

PAS ingress and native POST forwarding collect source-profile evidence after dispatch.
Each supported payload is checked independently at all three PAS/DTR lines; the
certified set and nearest source line are observations only. They do not change
routing, authority, payload bytes, acceptance, response stamps or lane readiness.
The native record describes the final POST attempt, before any polling or terminal
assembly. The provider ingress record describes the bytes dispatched and relayed.

The app creates separate clients at each resolved validator endpoint. Embedders may
supply independent clients through `Config.CertificationValidatorsByLine`; they must
not share routing validators or qualification wrappers. Missing clients are recorded
as unavailable. HTTP server execution failures are unavailable rather than conclusive
invalidity. Logs beginning `certify: ` and `leg.certified` observer details contain
JSON metadata, hashes and verdicts, without payload snapshots. External validator
issues and errors are represented only by bounded counts, byte lengths and
SHA-256 digests; diagnostic text is never retained or broadcast in evidence.

Each gateway owns one worker, a 32-payload queue and a 256-record evidence ring.
Candidates have a two-second limit, collection has a six-second limit, and queued
work expires after thirty seconds. Overflow records retain metadata only; bounded
observer notifications report drops explicitly. Callbacks must return promptly and
support concurrent invocation. Completion is stored before callback delivery.

Call `Gateway.Close()` after stopping service and before releasing dependencies.
It cancels observation HTTP work, closes idle connections and joins the worker.
It waits for cooperative callbacks; it cannot forcibly cancel a blocked callback.
The app runner and managed app handlers own this cleanup. Tests may use
`FlushCertificationForTest` as a context-bounded completion barrier, and
`DisableCertificationForTest` for evidence-off comparison runs.

## Observer source completion

The existing opt-in, loopback-only `OBSERVER_ADDR` listener also serves
`POST /barrier`. Success is HTTP 200 with `Content-Type: application/json`,
`Cache-Control: no-store`, and
`{"protocol":1,"incarnation":"<opaque>","events":N}`. The server waits for at
most five seconds, bounded further by request cancellation. A failed or canceled
wait returns a diagnostic non-2xx response without a successful event count;
deadline failures return 504 and other completion failures return 503. Wrong
methods return 405.

`GET /health` remains immediate and adds `protocol:1` and the same nonempty
`incarnation` to its existing `events` count. It never waits or submits validation.
`GET /events` includes `X-SHN-Observer-Incarnation` before sending frames; SSE
IDs and data bytes retain their representation. Each Hub construction gets a fresh
random identity, so counts from different source instances are not interchangeable.

`Gateway.WaitObserverCompletion(ctx)` snapshots HTTP operations already entered
through `Gateway.Handler()`, awaits their return (including deferred evidence
enqueue), then snapshots accepted certification work and awaits its observer
callbacks. Operations admitted after the first snapshot do not hold that operation
cutoff; certification accepted before the second snapshot is included. Direct
`OriginateLeg` calls outside Handler and requests not yet entered are outside this
protocol. Simultaneous external traffic is not a causal attribution guarantee.
Completion proves diagnostic delivery through the source callback, not receipt by
an SSE client; clients must separately catch up to `events` in the same incarnation.

This accounting is diagnostic only: clinical responses, asynchronous certification,
routing, authority, payload bytes and readiness remain unchanged. The ordinary
app health wrapper bypasses operation tracking. `FlushCertificationForTest` retains
its accepted-queue-only semantics. `observer.Hub.HandlerWithBarrier(wait)` enables
the capability with a context-cooperative completion waiter; nil and the existing
`Handler()` retain count-only health and do not expose `/barrier`. Both forms serve
the SSE incarnation header. No observer listener is created by default.

## Supported seams

Partners may depend on the following packages across minor versions (breaking
changes will be noted in the changelog):

| Package | Description |
|---|---|
| `engine` | Leg-processing core — the `Config`, `Engine`, and `Handler` types; `SystemOfRecord` and `Store` connector interfaces (`shnsdk.Adjudicator` is the SDK-level decision interface — see below) |
| `app` | Config-only gateway runner — `app.Run` and `app.Handler`/`app.HandlerWithClock` for embedding |
| `connectors/fhirsor` | FHIR-backed `SystemOfRecord` connector |
| `connectors/pgstore` | Postgres-backed `Store` connector |
| `connectors/scaffold` | Runnable `SystemOfRecord` skeleton for custom / legacy backends |
| `connectors/smartauth` | SMART Backend Services HTTP client for FHIR SoR authentication |

`engine.Config.Adjudicator` (`shnsdk.Adjudicator`) is **declared for source
compatibility but no longer consumed by the engine** since the v0.39.0 payer
retirement (its breaking entry below); setting it alone does nothing. The
supported custom-adjudication paths are (1) the **standalone `shnsdk.Responder`**,
which drives the same `shnsdk.Adjudicator` interface outside the gateway (see the
SDK's PREVIEW guide §3c; `crd-order-dispatch` is not currently served), and
(2) **native-forward** to your own Da Vinci endpoint (`PAYER_DAVINCI_*`).
In-gateway injection — a `LegResponder` on `Config.Responder` — remains an
internal 0.x seam: do not depend on it (see below).

**Additive native population diagnostics (FR-G23).**
`engine.NewNativePopulator(client, url)` retains its exact two-argument function
type and behavior. `engine.NewNativePopulatorWithFailureObserver(client, url,
observer)` adds an optional immutable callback; the old constructor delegates with
nil. `engine.PopulateFailure` has only `Stage`, `Reason`, and `Status` with JSON
names `stage`, `reason`, and `status`. The app adds `version: 1` when formatting
its fixed-field output record (see the README troubleshooting table).

Callbacks run synchronously, once per upstream failure, and may run concurrently
for separate calls. They must return promptly and be safe for concurrent use.
There is no mutable setter, background delivery, payload retention, or callback
error return. A nil callback disables diagnostic delivery and formatting. Subject
and canonical refusals keep their distinct behavior and produce no upstream record.

`connectors/smartauth.IsTokenAcquisitionError(err)` recognizes only failures
marked at the bearer transport's token acquisition boundary, through wrapped
errors. Existing error text and the immediate unwrap target are preserved;
resource-request errors with similar text remain unmarked. This does not change
token cache, refresh, credentials, timeout, or retry behavior.

`connectors/smartauth.WithTokenAcquisitionObservation(ctx)` returns a derived
context and a `*TokenAcquisitionObservation` with a read-only `Failed()` method.
Use one fresh handle per `http.Client.Do`; its zero value is unmarked and it must
not be copied after use. The handle records only an actual token acquisition
failure returned by the bearer transport, never an acquisition in progress or a
timeout alone. It retains no error or payload and can be read concurrently.

Native population creates fresh evidence for every request, even with a nil
callback, and shadows any inherited caller observation. The derived context
preserves values, cancellation and deadlines. This request-local evidence keeps
the acquisition boundary available when an outer client timeout replaces the
returned error chain; resource timeouts remain transport failures. No client-wide
or global failure state is introduced.

**Breaking in this release** (payer wiring):

- `engine.New` returns `(*Gateway, error)`. It errors — rather than starting — for the two
  conditions a deployment can hit with otherwise-valid config: a `role=payer` gateway with
  no content occupant, and an unusable ingress client registration.
- A `role=payer` gateway REQUIRES `engine.Config.Responder` (from the published binary:
  `PAYER_DAVINCI_BASE_URL`). The engine no longer synthesizes an in-process payer from
  `Config.Adjudicator`; a payer with no occupant fails closed at boot rather than answering
  Da Vinci legs out of the gateway itself.
- Every role REQUIRES a `SystemOfRecord` (from the published binary: `FHIR_DATA_URL`). The
  in-process persona stub (`engine.StubHolderData`) is gone. Its Store half survives as
  `engine.NewMemStore` — the in-memory `Store` default, carrying no persona content.

**Breaking in this release** (wire behaviour — new refusal class):

- **The gateway now enforces the FR-16 / FR-27 attestation requirements at the inbound
  gate, before dispatch, and answers `403`.** This runs on all three PAS entrances — the
  payer inbound `pas-claim` and `pas-claim-update` legs, and the provider-facing Da Vinci
  ingress. No earlier gateway inspected attestations on the wire, so **every refusal in this
  class is new**: traffic a v0.38.x gateway forwarded to the occupant can now be stopped at
  the door, and the occupant never sees it.

  A `QuestionnaireResponse` item that declares itself manually entered (the DTR
  information-origin extension with `source="manual"`) and names a `Practitioner` author
  must carry a complete clinician attestation: `npi`, `text`, and `date`, each present and
  non-empty. One naming a `Patient` author must carry a complete
  `questionnaireresponse-signature`: a signature `type` code, `when`, a `who` carrying a
  non-empty `reference` OR an `identifier` with a non-empty `value` (`system` optional —
  both of FHIR R4's legal `Signature.who` forms are accepted), and `data`. Whitespace-only
  counts as empty. A system-sourced item — one with no manual-source
  marker at all — is untouched and requires no attestation. The refusal names the failing
  requirement, the item's `linkId`, and the specific field that is absent or empty.

  `Config.Adjudicator` is unaffected in shape; what changes is that a nonconformant item is
  refused before any adjudication runs, rather than being handed to it.

## Evolving surfaces

These surfaces are new and intentionally **not yet pinned to a stability tier**
(neither "supported" nor "internal-only" in the senses above) — they are
expected to change shape as their consumer matures:

- **Observer stream** (`OBSERVER_ADDR`, `engine.Config.Observer`, `ObserverEvent` JSON,
  `observer.Hub`): new in this release and **evolving** — field additions and event-kind
  additions may happen in minor releases. The SHN Kit's `shnkitd` daemon (`kit/relay`) is now
  this stream's first real consumer: a local desktop inspection tool that SSE-subscribes to a
  provider-role gateway child's `/events` and re-emits frames onto its own run-timeline bus,
  stamped with the active run's identity. That consumer stays payer-role-aware (a payer-role
  gateway's stream is validation-only — `kit/relay`'s package doc), and pins an exact gateway
  version like any other consumer. The surface stays **evolving**, not yet a pinned stability
  tier — it will graduate once the Kit's inspector stabilizes.

  **v0.26.0** adds the `sor.read` event kind (the gateway's `SystemOfRecord` reads, one event
  per call) — an event-kind addition, covered by the evolving-contract clause above.

  **v0.34.0** adds `POST /demo/transform` on the same observer listener (`engine.RunTransformChain`
  exported for it) — a loopback-only JSON shim over the real compat-chain machinery, not itself
  part of the SSE stream (a run through it never appears on `/events`). `shnkitd`'s
  `POST /api/bridging/exhibit` is now this endpoint's first real consumer, proxying it
  over embedded reference content so the Kit's engine exhibit provably runs "the same modules
  your live legs route through" — same binary, same manifest. Same evolving posture as the rest
  of this surface: consumers pin exact gateway versions.

  **v0.35.0** adds `GET /demo/capture/{correlationId}` on the same observer listener: a
  loopback-only read-back of this gateway's own bounded, in-memory record of one transformed
  egress leg's pre-seal before/after payload pair — never a wire exchange, never audited, and
  never checked by any conformance surface (see `docs/CONFIGURATION.md`, "Demo-only pre-seal
  edge capture"). It is populated only when the new env `SHN_DEMO_EDGE_CAPTURE`
  (`engine.Config.DemoEdgeCapture`) is set, which as of this release also requires
  `OBSERVER_ADDR` to be set — otherwise the flag is gated off at config load rather than
  capturing into a store nothing could ever read. `POST /demo/transform`'s existing 200 and
  422 response bodies also gain an additive `chain` field on this same release — the
  compatibility-chain hops the run walked (or attempted), in the same shape already published
  on observer events; every existing field on both responses is unchanged. New exported engine
  surface backing this release, each its own release-notes bullet:

  - `engine.ChainSteps(contract, from, to string) []ChainStep` — a read-only accessor reporting
    the compatibility chain `RunTransformChain` would walk, without running any step function.
  - `engine.EdgeCapture` — the pre-seal before/after payload-pair type the capture store holds
    (`CorrelationID`, `LegType`, `Contract`, `From`, `To`, `Chain`, `LossReports`, `Before`,
    `After`, `CapturedAt`).
  - `(*Gateway) EdgeCaptureFor(id string) (EdgeCapture, bool)` — the production read seam the
    capture-fetch endpoint reads through.
  - `(*Gateway) RecordEdgeCaptureForTest(e EdgeCapture)` — a test seam over the same store for
    cross-package tests that need to seed a known capture entry without driving a full leg
    through the engine.
  - `Config.DemoEdgeCapture bool` — the config field `SHN_DEMO_EDGE_CAPTURE` parses into.

  Same evolving posture as the rest of this surface: consumers pin exact gateway versions.

- **`scenariodriver`** (`Config`, `Driver`, transport methods, builders, `Cards`/`ParseCards`):
  the UC-01…08 scenario-driving package. New in this release and **evolving** — signatures and
  return shapes may change in minor releases as the SHN Kit's daemon and the live conformance
  gate exercise it further. Consumers pin exact gateway versions.

- **`GET /health`** (served by the `app` runner in front of the engine handler): the shared
  SHN health payload — `service` (the gateway's holder id), optional `version`
  (from `SHN_VERSION`), `uptimeSeconds`, a worst-check-wins `status`, and a `checks`
  array (`registrar-poller` when a registrar feed is configured; `store` when the
  durable Postgres store is configured). The payload shape is the published
  `shn-sdk/health` contract (v0.29.0) and is non-sensitive by construction —
  statuses, timestamps, counts, and coarse error classes only. **Evolving**: check
  names and the set of registered checks may change in minor releases; the JSON
  field shape follows the `shn-sdk/health` package's compatibility.

- **`fhirseed`** (`Client` and its methods, `CRPrepopLibraries`, `DemoProviderPersonasBundle`,
  `DemoLumbarLibrary`, `PutGlobalArtifact`, `ProviderDataSeedBundle`, `ConformantSeedBundle`):
  the partner/Kit FHIR seed loader, baked persona fixture, and the two downloadable seed-bundle
  getters (embedded baked artifacts). **Evolving** — the seed sequence, fixture contents, and
  bundle bytes may change in minor releases as the Kit stabilizes its seeding needs. Consumers pin
  exact gateway versions.

  **BREAKING in v0.39.0** (evolving tier — announced, not guarded): `SandboxProviderPersonasBundle`
  is renamed `DemoProviderPersonasBundle` and `SandboxLumbarLibrary` is renamed
  `DemoLumbarLibrary`. The bytes each returns are unchanged; only the names are: the old prefix
  named the retired preview-era demo world and describes nothing in the platform today. A
  consumer on the old names updates the two call sites and re-pins.

- **`LegMetric`** (`engine.Config.LegMetric func(outcome string)`, consts
  `engine.LegOutcomeRouted/Answered/Denied/Unreachable/Failed`): new in this release and
  **evolving** — a nil-safe hook that receives one outcome string per origination-leg event at
  the roundTrip choke point. Nil (the published-binary default) means no emission; the hook
  carries no payloads and is conformance-neutral (`TestLegMetric_ConformanceNeutral` — responses
  are byte-identical hook-on vs hook-off). `gateway/app` wires it to CloudWatch EMF behind the
  `METRICS_SERVICE` opt-in (see `docs/CONFIGURATION.md`). `Unreachable` means the Hub leg did not
  complete; it also covers a Hub refusal after an unverifiable recipient response envelope and
  does not prove the responder itself was unreachable. Requires `shn-sdk` ≥ v0.31.0.

- **`GET`/`POST /internal/checks` results** (evolving surface, since v0.32.0; structured
  `failure` since v0.33.0). Each result is `{id, target, ok, detail, checkedAt,
  latencyMs}` plus, on failing results only, `failure {code, hint}` with `code` drawn
  from a closed set (`unreachable`, `http-status`, `invalid-capability-statement`,
  `credential-rejected`, `not-checked`, `internal`). Additive-only intent: existing keys
  and `detail` strings are stable fallbacks; new keys may appear in 0.x minors — decode
  tolerantly, never with unknown-field rejection. See `docs/CONFIGURATION.md`
  ("Operational checks") for semantics and redaction guarantees.

## Internal seams (not for partner use)

Everything under `engine.*` beyond the supported seams listed above — including
`engine.LegResponder`, `engine.NewNativeResponder`, `engine.Populator`, and their
helper functions and types — is gateway-internal and **unstable**: it may change
in any 0.x minor version without notice, and none of it is a published `shn-sdk`
contract. Partners should not import or depend on these directly.

To customize partner behavior, use the stable public paths instead: implement
`shnsdk.Adjudicator` and run it behind the standalone `shnsdk.Responder` (or
native-forward to your own Da Vinci endpoint) to control payer decisions, and build against
the published `shnsdk` types for wire data — never the internal `engine.*`
equivalents. Internal seams are promoted to `shnsdk` once their shape has proven
stable; until then, treat them as an implementation detail that may disappear or
change shape without notice.

## Unsupported internals

`internal/` packages and `cmd/` binaries are implementation details and may
change without notice between any versions. Do not import `internal/`
directly.

## Cross-version conformance contract

The **`shn-sdk` wire vectors** and the **SHN Participant Protocol
specification** (published with `shn-sdk`) are the conformance contract across
gateway versions. A gateway that passes the wire-vector suite is conformant
with the SHN exchange protocol regardless of the gateway version it runs.

## Optional diagnostic events (evolving)

`engine.Config.Diagnostic func(diagnostics.Event) bool` is a separate opt-in sink
for raw HTTP and participant-stage observations. `DiagnosticTraceKey` verifies
optional private ingress call attribution. `engine.WithNativeDiagnostic` adds the
same sink to native forwarding. Nil disables each hook. Sinks run synchronously,
may be called concurrently, must return promptly without blocking, and must
reserve bounded memory before copying read-only event bytes. Sink panics are
contained. The app supplies the bounded `diagnostics.Queue` publisher and owns its
shutdown in both `Run` and the closable `Handler` constructors.

`diagnostics.IngressFingerprint(ctx)` snapshots the handler-consumed ingress hash;
it never reads the body. `diagnostics.IngressBody(ctx)` exposes a synchronous read-only view of the bounded
body for existing observer adapters. `diagnostics.RequestIdentity(ctx, event)` projects the
verified envelope identity carried by `WithRequestIdentity`. A partial fingerprint
cannot establish a byte link. HTTP transport events inherit that identity through
token acquisition and native forwarding. No SDK metadata or wire field changes.

`connectors/smartauth.Config.Transport` optionally selects the authorized
operation transport beneath bearer injection; nil retains `http.DefaultTransport`.
`Config.HTTPClient` continues to select only the token-acquisition client. Keys,
token caching, caller-request cloning and failure behavior are unchanged.

The existing observer event names and completion barrier remain available.
Ingress observation now tees reads performed by the original handler, preserving
authentication ordering and read failures. The legacy ingress event fires when the original
handler finishes reading, before response commitment; unread rejected bodies are
reported on return as incomplete. Ingress events add `payloadIncomplete: true`
when request or response bytes were unread, truncated, failed, or unavailable due
to capture limits. An absent payload with this flag does not assert an empty body.
Complete events omit the flag and retain their existing JSON shape. Durable HTTP
events finalize on return. The existing observer
callback's panic behavior is retained, independently of the isolated diagnostic
sink. The SSE payload representation is unchanged; durable diagnostics carry raw
bytes directly. This source addition is not a published release or a version pin.


Relayed non-2xx responses preserve the participant's declared `Content-Type` with
its exact body bytes. The existing `application/fhir+json` fallback applies only
when that media type is absent; locally authored and empty-body refusal rules are
unchanged. This fixes a prior hardcoded media type on nonempty foreign errors.
