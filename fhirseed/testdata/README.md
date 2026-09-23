# Validator warm-up response

`hapi-dom6-warning.json` is the byte-preserved synthetic HAPI v8.10.0-1
OperationOutcome used by the SDK adapter qualification. SHA256:
`f1d5187bafe37f28125f7b3248cf84bfadfb6d14608de9da57cbe5f12d98385e`.
It records HTTP 200 from the US Core `seed-warm` Patient request, whose SHA256 is
`b6bc12b2062e17eb195413dba9c61ac20b2d80dbbd2dc1c97d98764a8375cc69`.
The SDK's `validation_evidence_mapping.md` documents independent runtime-WAR and
US Core 6.1.0 definition hashes and the warning-only OperationOutcome qualification.

This copy keeps the gateway test independent of the SDK source checkout layout.
The test verifies the response hash and posts the actual warm-up body. It proves
hermetic interpretation and readiness behavior, not live HAPI readiness, valid
source resources or terminology coverage. Source preparation still rejects invalid
and unavailable profile results before PUT.
