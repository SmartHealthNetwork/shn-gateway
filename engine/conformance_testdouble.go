//go:build shn_testdouble

package engine

// The module-absent opt-outs exist only in a test build (-tags
// shn_testdouble): a released gateway always loads payload conformance, and
// its published API has neither. The engine's own rows set the fields
// directly; these are for the root module's harness.
// TestConformanceModuleOptOutsOnlyInATestBuild holds both halves.

// DisableConformanceModuleForTest runs the gateway with module 3 not loaded:
// a participant that opted out of payload conformance altogether. Network
// rules and module 2's bridged check still refuse.
func DisableConformanceModuleForTest(c *Config) { c.conformanceModuleAbsent = true }

// WithoutConformanceModuleForTest runs the responder with module 3 not
// loaded, as DisableConformanceModuleForTest runs a gateway.
func WithoutConformanceModuleForTest() NativeOption {
	return func(n *nativeResponder) { n.moduleAbsent = true }
}
