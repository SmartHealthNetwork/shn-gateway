# Offline-proof probe fixtures

Vendored copies of the four initialization resources used by `verify.sh` to prove the validator
resolves the Da Vinci PAS/DTR/PDex/CDex profiles **offline** (the network is
isolated during the proof) and to characterize slicing/discriminator outcomes, including the
exact informational messages permitted by open slicing. These initialization checks are distinct
from the strict PAS verdict qualification: each line also vendors approved, denied and pended PAS
ClaimResponse goldens for that contract. Source-release parity checks require
the PAS/DTR initialization pairs and PAS response copies to remain byte-identical to their
registered source fixtures.
Everything stays here so the proof runs from a bare clone of the published gateway repo.

The same four (per line) begin the image's readiness sequence: `healthcheck.go`
embeds this directory (`//go:embed`, copied into the `hc` build stage by the
Dockerfile). The PID-1 supervisor follows them with fixed PAS priming, two clean
qualification passes and targeted negative controls. It does this sequentially once per
Java incarnation; ordinary probes only observe metadata and the completed marker.

Each PAS pass orders explicit versioned profile, explicit unversioned profile and
no-query `meta.profile` forms, with approved, denied and pended outcomes inside each
form. The pended files are response Bundles; fixture preparation extracts exactly
one ClaimResponse before posting to the ClaimResponse route. The negative control is
derived from the corresponding approved resource by changing the nested
`extension-reviewActionCode` value from CodeableConcept to Boolean. Success requires
the specific profile type rejection, not a parser error or an arbitrary server
failure.

The four initialization fixtures retain a different contract. They prove profile
resolution and fence slicing/discriminator failures, while documented offline
terminology errors are reported separately. The PAS request Bundle legitimately
contains non-Claim entries under its open `Bundle.entry` slice; the verifier counts
only the two exact informational Claim-discriminator explanations for those entries
as `open_slice_information`. Every warning/error/fatal variant or unknown
informational slicing message still fails. These are not strict positive verdict
fixtures, and this finite set is not a claim about every resource accepted by HAPI.

## Per-line subdirectories

`2.1/` and `2.2/` vendor the PAS/DTR-line-bearing initialization pair
(`claim-bundle.json`, `questionnaireresponse-autofill.json`) and the three PAS
ClaimResponse verdict fixtures from that line's matching corpus, used by `verify.sh`'s
per-line probe against the matching `hapi-<line>` sidecar image. The PDex EOB
and CDex Task probes (`eob-approved.json`, `cdex-task-data-request.json`) stay
in this top-level directory and are reused for every line — PDex/CDex are
line-neutral (a single native line each; see the manifest's `shared` block),
so there is no per-line variant to vendor.

## Controlled Linux child

`process-child/` is a test-only executable used by `verify-process.sh` to control metadata,
validation completion, connection loss and child shutdown. The verifier cross-compiles it
with local Go and mounts it into the built image; it is not embedded or installed in the
runtime image. It returns the strict targeted rejection for the derived negative
request, so the lifecycle proof exercises the same verdict contract. Actual cold
HAPI validation remains the separate `verify.sh` proof.
