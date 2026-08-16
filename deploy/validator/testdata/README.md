# Offline-proof probe fixtures

Vendored copies of four resources used by `verify.sh` to prove the validator
resolves the Da Vinci PAS/DTR/PDex/CDex profiles **offline** (the network is
isolated during the proof). The check keys only on profile *resolution* (the absence of
HAPI's "Failed to retrieve profile" issue), which is insensitive to resource
content — so harmless drift from the upstream copies cannot affect the gate.
Kept here (rather than referencing a path outside this directory) so the proof
runs from a bare clone of the published gateway repo.

## Per-line subdirectories

`2.1/` and `2.2/` vendor the PAS/DTR-line-bearing pair (`claim-bundle.json`,
`questionnaireresponse-autofill.json`) from that line's own golden corpus
(`testdata/golden/{2.1,2.2}/` at the repo root), used by `verify.sh`'s
per-line probe against the matching `hapi-<line>` sidecar image. The PDex EOB
and CDex Task probes (`eob-approved.json`, `cdex-task-data-request.json`) stay
in this top-level directory and are reused for every line — PDex/CDex are
line-neutral (a single native line each; see the manifest's `shared` block),
so there is no per-line variant to vendor.
