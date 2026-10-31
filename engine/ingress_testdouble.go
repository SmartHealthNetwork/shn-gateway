//go:build shn_testdouble

package engine

// EnableIngressForTest enables the Da Vinci ingress routes AND the test-only auth bypass on
// cfg. It is the ONLY affordance that sets ingressAuthBypass, and it exists only in a test
// build (-tags=shn_testdouble): an untagged build, as every release is, neither exports nor
// links it. build()/main MUST never call it (ingress_imagepurity_test.go).
//
// A cfg with no IngressBaseURL gets a test base, as a real ingress always has
// one: the address the gateway's own edits name (a documentation return
// path) is built under it.
func EnableIngressForTest(cfg *Config) {
	cfg.IngressEnabled = true
	cfg.ingressAuthBypass = true
	if cfg.IngressBaseURL == "" {
		cfg.IngressBaseURL = testDoubleIngressBaseURL
	}
}

// testDoubleIngressBaseURL is the ingress base EnableIngressForTest gives a
// cfg that names none.
const testDoubleIngressBaseURL = "https://ingress.provider-gw.test"
