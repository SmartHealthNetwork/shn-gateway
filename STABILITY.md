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

This gateway currently requires `shn-sdk v0.30.0` (see `go.mod`).

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
  additions may happen in minor releases. The SHN Kit's `shnkitd` daemon (`kit/relay`) is now
  this stream's first real consumer: a local desktop inspection tool that SSE-subscribes to a
  provider-role gateway child's `/events` and re-emits frames onto its own run-timeline bus,
  stamped with the active run's identity. That consumer stays payer-role-aware (a payer-role
  gateway's stream is validation-only — `kit/relay`'s package doc), and pins an exact gateway
  version like any other consumer. The surface stays **evolving**, not yet a pinned stability
  tier — it will graduate once the Kit's inspector stabilizes.

  **v0.26.0** adds the `sor.read` event kind (the gateway's `SystemOfRecord` reads, one event
  per call) — an event-kind addition, covered by the evolving-contract clause above.

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

- **`fhirseed`** (`Client` and its methods, `CRPrepopLibraries`, `SandboxProviderPersonasBundle`,
  `SandboxLumbarLibrary`, `PutGlobalArtifact`, `ProviderDataSeedBundle`, `ConformantSeedBundle`):
  the partner/Kit FHIR seed loader, baked persona fixture, and the two downloadable seed-bundle
  getters (embedded baked artifacts). New in this release and **evolving** — the seed sequence, fixture contents, and
  bundle bytes may change in minor releases as the Kit stabilizes its seeding needs. Consumers pin
  exact gateway versions.

## Internal seams (not for partner use)

Everything under `engine.*` beyond the supported seams listed above — including
`engine.LegResponder`, `engine.NewNativeResponder`, `engine.Populator`, and their
helper functions and types — is gateway-internal and **unstable**: it may change
in any 0.x minor version without notice, and none of it is a published `shn-sdk`
contract. Partners should not import or depend on these directly.

To customize partner behavior, use the stable public seams instead: inject a
custom `engine.Config.Adjudicator` to control payer decisions, and build against
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
