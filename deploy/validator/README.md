# Validator sidecar readiness

This directory builds the IG-loaded HAPI sidecar shipped with the Smart Gateway.
Each line's image bakes the `validator-sidecar` package set from
`tools/contracts/manifest.json` — the line's Da Vinci IGs (line 2.2 also
`hl7.fhir.uv.extensions.r4`) and the committed SHN packages, among them the
validation-support package carrying the closures of the cross-version encounter
extension and the `artifact-versionAlgorithm` extension copied unchanged from their pinned packages — every archive verified against its manifest digest at build time.
The image starts `/healthcheck supervise` as PID 1. That supervisor owns one Java
child and one finite, sequential readiness worker for the lifetime of the Java
incarnation.

PID 1 reserves the public `:8080` listener before launching Java. Java binds
`127.0.0.1:18080`; only the owned worker uses that private endpoint while cold.
Its private HTTP client authors `Host: localhost:8080`, preserving the historical
worker authority while dialing the fixed loopback backend; any other destination
is refused. HAPI globally caches CapabilityStatements, so ordinary metadata may
reflect an earlier caller's authority. The worker must never seed private backend
URLs into that cache. The verifier checks the earliest ordinary localhost response
and later ordinary responses for private URLs throughout the complete JSON, then
uses separate no-cache Host probes to verify fresh URL derivation. No-cache is a
verifier-only probe; public requests retain their original bytes.
Public connections are closed without reaching Java until the worker returns
success and its complete current-incarnation marker is verified. Each Smart
Gateway then runs its own unchanged qualification corpus. This prevents several
gateways from entering a sidecar's cold initialization concurrently.

The supervisor preserves the literal Java launch prefix and inherited environment,
then appends the owned Spring application options `--server.address=127.0.0.1`
and `--server.port=18080`. Do not supply either option yourself, even with a matching
value. Matching lower-priority `SERVER_PORT`/`SERVER_ADDRESS`, canonical JVM `-D`
options and structured `SPRING_APPLICATION_JSON` values are accepted; conflicting
or ambiguous owned values fail startup. The supported JVM spelling is an unquoted
atomic `-Dserver.port=18080` or `-Dserver.address=127.0.0.1`. Loader redirection,
JDK argument-file indirection and JVM-option-embedded Spring JSON are unsupported.
Use `SPRING_APPLICATION_JSON` for structured JSON instead. Unrelated environment
and JVM option bytes are preserved; `JAVA_OPTS` is not expanded by this literal launch.

After admission, TCP streams preserve the original HTTP bytes, Host, profile query,
response status, headers and body without replay or URL rewriting. Up to 256 pending
or established connection pairs are allowed; excess connections close before a
backend dial. Each copy direction uses a 32KiB buffer. Orderly FIN propagates a write
half-close and allows the response to drain; it does not imply immediate cancellation
of Java validation. Transport errors and process shutdown close both directions.
Shutdown revokes public admission before signaling Java, cancels and joins forwarding
and the worker, and retains the 20-second child-stop bound. The worker's 600-second
startup deadline does not terminate an already qualified service.

Readiness has two parts. The worker first submits the four initialization fixtures
that exercise PAS request Bundle, DTR QuestionnaireResponse, PDex
ExplanationOfBenefit and CDex Task profile resolution. It then submits approved,
denied and extracted pended PAS ClaimResponses in versioned, unversioned and
resource `meta.profile` request forms. One initialization pass, two strict clean
passes and three targeted type-error controls are followed by a complete PAS response
Bundle and three support controls: unknown HCPCS L9999, unassigned place of service
98, and Claim.encounter with a string where a Reference is required. Two encounter
rows extend the corpus: a core Claim whose R5 backport `Claim.encounter` extension
references a contained Encounter validates clean (the extension and its target
profile come from the support package's derived closure, copied unchanged from the
pinned cross-version and extensions packages), and the same Claim
with a Patient in the Encounter's place is refused for the target type. Two final
rows retain a valid in-band ClaimResponse profile while explicitly requesting an
unavailable version and a nonexistent canonical. Each requires the exact requested-
profile ERROR and message identity; a clean or warning-only result cannot qualify.
All 42 rows must complete before the marker is published. Each support control requires exactly
the expected errors its pinned outcome lists (severity, code, message identity,
diagnostic and path); unrelated errors, unknown definitions and missing errors fail. The initialization pass has the only narrow allowance for the reproduced
PAS `extension-reviewAction` slicing diagnostics; later slicing diagnostics fail the
incarnation.

The unchanged HAPI image and package pins are distinct from the shared explicit-
profile behavior correction in `backport/`. Its pinned build-only compiler replaces
one validation class while preserving the other archive members. Each runtime
image carries `/app/backport-provenance.json` with source, compiler/platform,
class, nested-JAR and platform-specific WAR identities. Runtime uses that WAR
without an overlay or another Java process. Source and Apache license ship with
the build context; compiler inputs and build caches do not enter runtime images.

An ordinary `/healthcheck` invocation is passive. It reports a valid terminal failed
marker before probing an unavailable public service. Success requires public metadata
and a fresh read of the complete current marker after that request; an earlier ready
snapshot cannot conceal a failure during the probe. It checks metadata and the
schema-, line- and incarnation-bound marker within four seconds. It never submits a
validation request, repairs the marker or retries failed work. A terminal or
uncertain readiness failure requires a new Java incarnation.

Two explicit commands reuse the same PAS fixtures and verdict assertions:

```sh
/healthcheck qualify --base http://localhost:8080/fhir --line 2.2 --pas-version 2.2.1 --budget 600s
/healthcheck verify-verdicts --base http://localhost:8080/fhir --line 2.2 --pas-version 2.2.1 --budget 30s
```

Both accept only unauthenticated HTTP loopback endpoints and require the PAS package
version to match the selected line. `qualify` runs the initialization pass, two
strict passes and negative controls (38 rows). `verify-verdicts` runs one strict pass and the
negative controls (20 rows), without priming, retrying or writing the runtime marker.
Both include the two encounter rows and reject a missing explicitly requested
profile version and canonical while preserving the valid in-band profile.

`verify.sh` builds fresh per-line source images on an internal Docker network. Once
each image first reports healthy, it runs `verify-verdicts` immediately and at
five-second intervals through at least 100 seconds. It separately characterizes the
four initialization profiles: unresolved profiles and slicing or discriminator
failure diagnostics at any severity fail. The PAS request Bundle's two exact
informational open-slice explanations are counted separately; changes to their
severity, issue code, message identity, profile, diagnostic shape or Bundle entry
fail. Documented offline terminology errors and other fixture errors are also
separate because these fixtures are profile-resolution probes, not strict positive
verdicts. The never-touched US Core Patient latency, image configuration and Linux
process lifecycle checks remain separate assertions.

The corpus is deliberately finite. It establishes the listed PAS outcomes and
request forms plus the four named profile-resolution fixtures for the selected
line. It does not certify arbitrary FHIR resources. A local source-image pass also
does not prove that a published image or a deployed digest contains the same bytes.

The SHN Kit runs the same HAPI WAR as a supervised desktop process. It checks
the same finite verdict corpus once per child process before reporting that
validator ready, including after a restart.

## Complete response fixture provenance

`testdata/pas-response-complete.json` preserves the synthetic nine-resource approval
response from the patched reference payer at upstream
`a8bece458cb31f151845db9ea5a892e398deef56` (captured September 10, 2026).
Only JSON indentation changes. The 2.2 fixture is identical. The 2.1 fixture adds
`Patient.identifier[0].type.coding = v2-0203#MB` to satisfy that version's required
memberIdentifier slice. That is a synthetic validator fixture adaptation, not a
claim that the payer supports PAS 2.1; its qualified wire contract remains 2.0.1.
Negative request bodies are derived from that same fixture with exactly one code
replacement or one added wrongly typed extension. The committed `*-errors.json`
files pin the independently inspected HAPI rejection pairs for each PAS version.
The three cold support-loaded lanes returned zero error/fatal issues for the
positive fixture and the two intended errors for each mutation. Licensed X12
terminology warnings remain visible; these controls do not certify that dataset.

The complete-response fixture preserves synthetic payer resource data; its generated narrative separates the member identifier system and value with a colon for readable publication. The PAS 2.1 fixture also includes that line's required member-identifier type.
