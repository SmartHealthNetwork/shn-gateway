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
  `DemoLumbarLibrary`. The bytes each returns are unchanged; only the names are. The sandbox payer
  no longer exists anywhere in this platform, and no surface it named survives with it. A consumer
  on the old names updates the two call sites and re-pins.

- **`LegMetric`** (`engine.Config.LegMetric func(outcome string)`, consts
  `engine.LegOutcomeRouted/Answered/Denied/Unreachable/Failed`): new in this release and
  **evolving** — a nil-safe hook that receives one outcome string per origination-leg event at
  the roundTrip choke point. Nil (the published-binary default) means no emission; the hook
  carries no payloads and is conformance-neutral (`TestLegMetric_ConformanceNeutral` — responses
  are byte-identical hook-on vs hook-off). `gateway/app` wires it to CloudWatch EMF behind the
  `METRICS_SERVICE` opt-in (see `docs/CONFIGURATION.md`). Requires `shn-sdk` ≥ v0.31.0.

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
