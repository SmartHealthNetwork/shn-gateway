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
passes and three targeted type-error controls must complete before the marker is
published. The initialization pass has the only narrow allowance for the reproduced
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
strict passes and negative controls. `verify-verdicts` runs one strict pass and the
negative controls, without priming, retrying or writing the runtime marker.

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
