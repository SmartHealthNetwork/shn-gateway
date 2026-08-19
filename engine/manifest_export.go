// manifest_export.go — read-only exported view of the compatibility manifest
// for the substrate repo's declared-expectations grid (test/xmatrix,
// tools/xmatrixgen — manifest alignment). Same posture
// as transform_run.go's RunTransformChain: a published seam, not a test-only
// escape hatch. Enumeration only — chain selection stays in chainFor/selectChain.
package engine

// ManifestSteps returns a copy of the compatibility manifest rows, in table
// order. Callers cannot mutate the live table through it.
func ManifestSteps() []CompatStep {
	out := make([]CompatStep, len(compatManifest))
	copy(out, compatManifest)
	return out
}
