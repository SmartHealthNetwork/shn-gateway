# Vendored demo-endpoint goldens

The files under this directory (`2.1/questionnaireresponse-autofill.json`,
`2.2/questionnaireresponse-autofill.json`) are **byte-for-byte COPIES** of the
repo-root `testdata/golden/2.1/questionnaireresponse-autofill.json` and
`testdata/golden/2.2/questionnaireresponse-autofill.json` fixtures.

They exist here, inside the `gateway` module, because `gateway/app`'s
`demo_endpoint_test.go` needs to read them and the `gateway` module is
published standalone (it does not carry the repo root's `testdata/` alongside
it) — a `../../testdata/golden/...` reach would `Fatalf` in the published
snapshot.

## Drift guard

These copies are **not** independently maintained. Root-module
`test/conformance/gateway_app_golden_drift_test.go` asserts byte-equality
between each file here and its repo-root original on every `make check` run,
so a regeneration of the repo-root goldens can't silently strand a stale copy
here. If that test fails, re-copy the named file(s) from `testdata/golden/` —
do not hand-edit the copies.
