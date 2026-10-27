//go:build shn_testdouble

package engine

// EnableIngressForTest enables the Da Vinci ingress routes AND the test-only auth bypass on
// cfg. It is the ONLY affordance that sets ingressAuthBypass, and it exists only in a test
// build (-tags=shn_testdouble): an untagged build, as every release is, neither exports nor
// links it. build()/main MUST never call it (ingress_imagepurity_test.go).
func EnableIngressForTest(cfg *Config) {
	cfg.IngressEnabled = true
	cfg.ingressAuthBypass = true
}
