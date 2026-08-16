# Vendored engine goldens

Every file under this directory is a **byte-for-byte COPY** of the repo-root
`testdata/golden/<same relative path>` fixture. Nineteen files across the three
contract lines (the top-level 2.0 set, `2.1/`, `2.2/`, and the `conformant/`
request bundles under each).

They are read by this package's tests through two helpers:

- `pasGolden` (`transform_pas_test.go`) — the cross-version transform suites in
  `transform_pas_test.go`, `transform_dtr_test.go`, `transform_run_test.go` and
  `egressadapt_test.go`.
- `readConformantGolden` (`pas_native_test.go`) — the conformant-request
  byte-match assertions.

## Why copies

The `gateway` module is **published standalone** (as `shn-gateway`) and does
not carry the repo root's `testdata/` alongside it. A reach above the module
root resolves in the monorepo and nowhere else, so it makes these tests either
red or silently skipped in the published module — which means the module's own
`go test ./...` stops telling the truth about the code it ships. Both failure
modes have happened; `gateway/sweep_test.go`'s `TestNoRepoRootReach` now fails
at authoring time on any such reach, in either the literal (`"../../…"`) or the
split (`filepath.Join("..", "..", …)`) spelling.

## Drift guard

These copies are **not** independently maintained. The root module's
`test/conformance/gateway_vendored_golden_drift_test.go` asserts byte-equality
between every file here and its repo-root original on each `make check` run —
discovery-based, so a newly vendored fixture is pinned automatically. Only the
root module can see both trees in one build.

If that test fails, re-copy the named file(s) from the repo-root
`testdata/golden/` originals — **do not hand-edit the copies**.
