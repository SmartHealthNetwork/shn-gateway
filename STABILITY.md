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

## Supported seams

Partners may depend on the following packages across minor versions (breaking
changes will be noted in the changelog):

| Package | Description |
|---|---|
| `engine` | Leg-processing core — the `Config`, `Engine`, and `Handler` types; `SystemOfRecord`, `Store`, and `Adjudicator` connector interfaces |
| `app` | Config-only gateway runner — `app.Run` and `app.Handler`/`app.HandlerWithClock` for embedding |
| `connectors/fhirsor` | FHIR-backed `SystemOfRecord` connector |
| `connectors/pgstore` | Postgres-backed `Store` connector |
| `connectors/scaffold` | Runnable `SystemOfRecord` skeleton for custom / legacy backends |
| `connectors/smartauth` | SMART Backend Services HTTP client for FHIR SoR authentication |

`engine.Config.Adjudicator` is the **supported partner injection point** for payer
decisioning. It is stable across minor versions. Do not depend on
`engine.LegResponder` — it is an internal 0.x seam (see below).

## Evolving surfaces

These surfaces are new and intentionally **not yet pinned to a stability tier**
(neither "supported" nor "internal-only" in the senses above) — they are
expected to change shape as their consumer matures:

- **Observer stream** (`OBSERVER_ADDR`, `engine.Config.Observer`, `ObserverEvent` JSON,
  `observer.Hub`): new in this release and **evolving** — field additions and event-kind
  additions may happen in minor releases. Consumers (the SHN Kit) pin exact gateway
  versions. Will graduate to a stability tier when the Kit's inspector stabilizes.

- **`scenariodriver`** (`Config`, `Driver`, transport methods, builders, `Cards`/`ParseCards`):
  the UC-01…08 scenario-driving package. New in this release and **evolving** — signatures and
  return shapes may change in minor releases as the SHN Kit's daemon and the live conformance
  gate exercise it further. Consumers pin exact gateway versions.

- **`fhirseed`** (`Client` and its methods, `CRPrepopLibraries`, `SandboxProviderPersonasBundle`):
  the partner/Kit FHIR seed loader and baked persona fixture. New in this release and
  **evolving** — the seed sequence and fixture contents may change in minor releases as the Kit
  stabilizes its seeding needs. Consumers pin exact gateway versions.

## Internal seams (not for partner use)

`engine.LegResponder` is the gateway's internal payer-content seam (FHIR-in /
FHIR-out). It is **unstable**: it may change in any 0.x minor version without
notice. Partners who need to customize payer decisions should inject a custom
`engine.Config.Adjudicator`; the engine derives the internal `LegResponder` from
it automatically. `LegResponder` will be promoted to `shnsdk` once its shape has
stabilized across both the native-forward and managed connector families; until
then it is gateway-internal only.

`engine.NewNativeResponder` is the native-forward Da Vinci forwarder; it handles
the read-only legs and — when `PAYER_DAVINCI_PAS_NATIVE=true` — the PAS legs too
(submit/update forwarded to the partner's `/Claim/$submit`). It is an internal
`LegResponder` 0.x seam. The composite Responder routes read-only legs to native
and the PAS pair to native or the sandbox fallback depending on the switch. This is
not a partner contract. They will graduate to `connectors/davinci` when
`LegResponder` is promoted to `shnsdk`.

As of the P4 native-Da-Vinci-interop slice, the native-forward CRD leg
**normalizes a real partner RI's `coverage-information`** (`normalizeCRDCoverage`,
the split `covered`/`paNeeded`/`questionnaires[]`/`satisfiedPaId` shape → the
`shnsdk.CardCoverage` canonical, fail-closed) and **discovers the order-select CDS
service id** from the partner's `/cds-services` (`DiscoverCRDServiceID`, override →
unique-service → fail-closed); the PAS leg normalizes the partner's `$submit`
response Bundle via a content discriminator (`normalizePASResponse`). These map a
real RI's **response** vocabulary; the **request** direction (a conformant CDS Hooks
order-select with the payer's prefetch resolved-and-inlined) is a provider-side
follow-on slice, not a payer-edge concern. The widened `shnsdk.CardCoverage` card
contract is a **breaking** `shn-sdk` change shipped in **v0.10.0** (see the SDK
changelog); this gateway requires `shn-sdk v0.10.0`.

`engine.Populator` is the gateway-internal provider-side DTR population seam
(FHIR-in / QuestionnaireResponse-out). It is a **0.x internal seam** and is NOT
a published `shn-sdk` contract this slice. Two backends exist: **managed** (wraps
the existing `FillQuestionnaire` — the sandbox/demo green-keeper, not a general
legacy fallback) and **native** (forward to a provider's SDC
`Questionnaire/$populate` endpoint, selected by `PROVIDER_DTR_NATIVE` +
`PROVIDER_DTR_POPULATE_URL`). A third backend (operated CQL engine) is a
config-only drop-in when that slice lands. `Populator` follows `LegResponder` to
`connectors/` and eventually `shnsdk` once the shape is proven stable across
backends.

No test-only exported shim was added for the DTR package extractor — the §8.3
anti-circularity proof (`TestExtractQuestionnaireFromPackage_ReturnsVerbatimAndDropsDeps`)
runs in-package (`gateway/engine`) against the unexported `extractQuestionnaireFromPackage`
function, so the extractor is never exposed beyond its single production call site in
`originate.go` (the consumer, in-package). The `dtr-questionnaire-fetch` leg's
`ResponseFHIR` is the full `$questionnaire-package` collection Bundle: the sandbox path
wraps via `buildQuestionnairePackage`; the native path forwards the partner's package
verbatim. The consumer (`originate.go`) extracts the bare Questionnaire for F5/auto-fill;
the dependent Libraries/ValueSets survive the wire intact inside the Bundle.

The exported `engine.ParseCoverageEligibilityResponsePatient` /
`engine.ParsePASResponsePatients` FHIR-subject readers (used by the outbound
patient fence) are part of the same internal seam — exported only so the
substrate's adversarial tests can drive them, **not** a partner contract. They
promote to `shnsdk` with `LegResponder`.

## Unsupported internals

`internal/` packages and `cmd/` binaries are implementation details and may
change without notice between any versions. Do not import `internal/`
directly.

## Cross-version conformance contract

The **`shn-sdk` wire vectors** and the **SHN Participant Protocol
specification** (published with `shn-sdk`) are the conformance contract across
gateway versions. A gateway that passes the wire-vector suite is conformant
with the substrate protocol regardless of the gateway version it runs.
