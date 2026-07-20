package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestCanaryTwins_CoverEverySandboxScenarioMember pins that every member the
// 11 sandbox checks can resolve has a twin — a scenario added without a twin
// must fail this census, not 400 at 2am in the canary.
func TestCanaryTwins_CoverEverySandboxScenarioMember(t *testing.T) {
	for _, m := range []string{"MBR-COVERED", "MBR-NOTCOVERED", "MBR-UC04", "MBR-UC05",
		"MBR-UC05-NOCONSENT", "MBR-UC06", "MBR-UC07", "MBR-UC07HCPCS", "MBR-UC08"} {
		if _, ok := CanaryTwins[m]; !ok {
			t.Errorf("no canary twin for %s", m)
		}
	}
}

// TestStubPersonas_CanaryClones: every twin resolves in stubPersonas with the
// original's coverage/clinical facts and a distinct family name (distinct PCI).
func TestStubPersonas_CanaryClones(t *testing.T) {
	for orig, twin := range CanaryTwins {
		o, ok := stubPersonas[orig]
		if !ok {
			t.Fatalf("original %s missing", orig)
		}
		c, ok := stubPersonas[twin]
		if !ok {
			t.Fatalf("twin %s missing", twin)
		}
		if c.inforce != o.inforce || c.hasClinical != o.hasClinical {
			t.Errorf("%s: coverage/clinical facts diverge from %s", twin, orig)
		}
		if c.demo.FamilyName != o.demo.FamilyName+"-Canary" {
			t.Errorf("%s family = %q", twin, c.demo.FamilyName)
		}
	}
}

// Rejection rows (valid request − one mutation → reject; CLAUDE.md guard
// discipline): unknown personaSet and twin-less member must 400, never
// silently fall through to the shared demo personas.
func TestScenario_PersonaSetRejections(t *testing.T) {
	g := newTestProviderGateway(t) // reuse the package's existing constructor helper
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	post := func(path, body string) *http.Response {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := post("/scenario/uc03?personaSet=bogus", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown personaSet: got %d, want 400", resp.StatusCode)
	}
	// homeoxygen's member literal has no canary twin → canary must fail closed.
	if resp := post("/scenario/homeoxygen?personaSet=canary", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("twin-less member under canary: got %d, want 400", resp.StatusCode)
	}
	// control: personaSet absent keeps working (any status but 400-personaSet).
	resp := post("/scenario/uc03", `{}`)
	if resp.StatusCode == http.StatusBadRequest {
		t.Fatalf("control uc03 without personaSet unexpectedly 400")
	}
}

// newTestProviderGateway builds the minimal provider-role Gateway the package's other
// handler tests construct (copied from TestHandler_HomeOxygenRouteRegistered in
// originate_homeoxygen_test.go — no newTestProviderGateway-style helper existed yet).
func newTestProviderGateway(t *testing.T) *Gateway {
	t.Helper()
	_, signPriv := genED25519(t)
	encPub, encPriv := genKeyPair(t)
	stub := NewStubHolderData()
	return New(Config{
		Role:     "provider",
		HolderID: "provider",
		Identity: shnsdk.Identity{
			HolderID: "provider",
			SignPriv: signPriv,
			EncPub:   encPub,
			EncPriv:  encPriv,
		},
		SoR:       stub,
		Store:     stub,
		Validator: shnsdk.NewFakeValidator(),
	})
}
