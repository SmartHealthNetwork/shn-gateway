# Validator sidecar readiness

This directory builds the IG-loaded HAPI sidecar shipped with the Smart Gateway.
The image starts `/healthcheck supervise` as PID 1. That supervisor owns one Java
child and one finite, sequential readiness worker for the lifetime of the Java
incarnation.

Readiness has two parts. The worker first submits the four initialization fixtures
that exercise PAS request Bundle, DTR QuestionnaireResponse, PDex
ExplanationOfBenefit and CDex Task profile resolution. It then submits approved,
denied and extracted pended PAS ClaimResponses in versioned, unversioned and
resource `meta.profile` request forms. One initialization pass, two strict clean
passes and three targeted type-error controls are followed by a complete PAS response
Bundle and three support controls: unknown HCPCS L9999, unassigned place of service
98, and Claim.encounter with a string where a Reference is required. All 38 rows
must complete before the marker is published. Each support control requires the
exact two expected errors (severity, code, message identity, diagnostic and path);
unrelated errors, unknown definitions and missing errors fail. The initialization pass has the only narrow allowance for the reproduced
PAS `extension-reviewAction` slicing diagnostics; later slicing diagnostics fail the
incarnation.

An ordinary `/healthcheck` invocation is passive. It checks metadata and the
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
strict passes and negative controls (34 rows). `verify-verdicts` runs one strict pass and the
negative controls (16 rows), without priming, retrying or writing the runtime marker.

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
