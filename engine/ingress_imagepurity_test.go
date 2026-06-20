package engine

import (
	"os"
	"strings"
	"testing"
)

// Image purity: the production wiring (app.go, cmd/gateway/main.go) must NEVER reference
// the test-only ingress bypass — neither the unexported field nor the EnableIngressForTest
// helper. A grep-style structural guard; the scaffold pattern. EnableIngressForTest remains
// the ONLY setter of ingressAuthBypass — new token/well-known routes must not introduce
// another path to set the bypass.
func TestIngressBypass_AbsentFromProductionWiring(t *testing.T) {
	for _, path := range []string{"../app/app.go", "../cmd/gateway/main.go"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, forbidden := range []string{"ingressAuthBypass", "EnableIngressForTest"} {
			if strings.Contains(string(src), forbidden) {
				t.Errorf("%s references %q — the ingress auth bypass must be build-time-absent", path, forbidden)
			}
		}
	}
}

// TestIngressAuthOK_NoRuntimeDisable asserts there is no env var or header that flips
// ingressAuthOK to true: the ONLY non-bypass path to true is a validly-issued bearer.
// Structural: ingressAuthOK reads g.cfg.ingressAuthBypass and g.ingressAuth only —
// it must not consult os.Getenv or a header-named flag.
func TestIngressAuthOK_NoRuntimeDisable(t *testing.T) {
	src, err := os.ReadFile("gateway.go")
	if err != nil {
		t.Fatalf("read gateway.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (g *Gateway) ingressAuthOK(")
	if start < 0 {
		t.Fatal("ingressAuthOK not found")
	}
	end := strings.Index(body[start:], "\n}\n")
	fn := body[start : start+end]
	for _, forbidden := range []string{"os.Getenv", "Getenv", "Header.Get", "FormValue", "ingressAuthDisable"} {
		if strings.Contains(fn, forbidden) {
			t.Errorf("ingressAuthOK references %q — no runtime auth-disable path may exist", forbidden)
		}
	}
}
