package engine

import (
	"os"
	"strings"
	"testing"
)

// sceneMember resolves the distinct provider-data member under provider-data, the
// distinct demo member (§4.3) under demo, and the conformance-roster default member
// for every other value.
func TestSceneMember_ProfileDispatch(t *testing.T) {
	gp := &Gateway{cfg: Config{OriginationProfile: "provider-data"}}
	if got := gp.sceneMember("MBR-UC04", "MBR-PD-UC04", "MBR-D-UC04"); got != "MBR-PD-UC04" {
		t.Fatalf("provider-data sceneMember = %q, want MBR-PD-UC04", got)
	}
	gd := &Gateway{cfg: Config{OriginationProfile: "demo"}}
	if got := gd.sceneMember("MBR-UC04", "MBR-PD-UC04", "MBR-D-UC04"); got != "MBR-D-UC04" {
		t.Fatalf("demo sceneMember = %q, want MBR-D-UC04", got)
	}
	gc := &Gateway{cfg: Config{OriginationProfile: "unknown-lane"}}
	if got := gc.sceneMember("MBR-UC04", "MBR-PD-UC04", "MBR-D-UC04"); got != "MBR-UC04" {
		t.Fatalf("unknown-lane sceneMember = %q, want MBR-UC04 (the default arm)", got)
	}
	// "" (absent OriginationProfile) reaches the same default arm at the ENGINE seam —
	// never provider-data or demo by accident. (gateway/app normalizes "" to "demo" ONE
	// level up, at the config boundary; this asserts the seam itself, not the boundary.)
	ge := &Gateway{cfg: Config{OriginationProfile: ""}}
	if got := ge.sceneMember("MBR-UC04", "MBR-PD-UC04", "MBR-D-UC04"); got != "MBR-UC04" {
		t.Fatalf(`"" sceneMember = %q, want MBR-UC04 (the default arm)`, got)
	}
}

// handleUC04 must thread sceneMember so provider-data reads its own seeded G0151 order
// (OpenOrder is keyed on member) while the default arm stays on MBR-UC04.
func TestHandleUC04_ThreadsSceneMember(t *testing.T) {
	src, err := os.ReadFile("originate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	fn := extractFunc(t, string(src), "handleUC04")
	if !strings.Contains(fn, `g.scenarioMember(w, r, "MBR-UC04", "MBR-PD-UC04", "MBR-D-UC04")`) {
		t.Fatalf("handleUC04 does not thread scenarioMember(MBR-UC04, MBR-PD-UC04, MBR-D-UC04)")
	}
	if strings.Contains(fn, `runCRDThenDTROrder(w, r, "MBR-UC04"`) {
		t.Fatalf("handleUC04 still passes the MBR-UC04 literal to runCRDThenDTROrder — must pass the sceneMember result")
	}
	// The amendment tail's SupplementalReport lookup must also thread the
	// resolved member — not the MBR-UC04 literal — or the canary twin (personaSet=canary,
	// resolved member MBR-CANARY-UC04) gets the original member's operative
	// DiagnosticReport attached, and the payer's member fence rejects the ClaimUpdate
	// bundle as an inconsistent-patient 403.
	if !strings.Contains(fn, `g.cfg.SoR.SupplementalReport(member)`) {
		t.Fatalf("handleUC04 does not pass the resolved member to SoR.SupplementalReport")
	}
	if strings.Contains(fn, `SupplementalReport("MBR-UC04")`) {
		t.Fatalf("handleUC04 still passes the MBR-UC04 literal to SoR.SupplementalReport — must pass the sceneMember result")
	}
}

// handleUC08 must thread sceneMember so provider-data reads its own seeded J3490 order
// (OpenOrder is keyed on member) while the default arm stays on MBR-UC08.
func TestHandleUC08_ThreadsSceneMember(t *testing.T) {
	src, err := os.ReadFile("originate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	fn := extractFunc(t, string(src), "handleUC08")
	if !strings.Contains(fn, `g.scenarioMember(w, r, "MBR-UC08", "MBR-PD-UC08", "MBR-D-UC08")`) {
		t.Fatalf("handleUC08 does not thread scenarioMember(MBR-UC08, MBR-PD-UC08, MBR-D-UC08)")
	}
	if strings.Contains(fn, `runCRDThenDTROrder(w, r, "MBR-UC08"`) {
		t.Fatalf("handleUC08 still passes the MBR-UC08 literal to runCRDThenDTROrder — must pass the sceneMember result")
	}
}

// isDemoProfile is the §4.3 demo-lane predicate: true for exactly "demo", false for
// every other value including the br-payer-targeting provider-data (a distinct axis —
// demo is a hermetic in-process mirror, not a live br-payer HTTP round-trip).
func TestIsDemoProfile(t *testing.T) {
	if !isDemoProfile("demo") {
		t.Fatal(`isDemoProfile("demo") = false, want true`)
	}
	for _, p := range []string{"", "provider-data", "unknown-lane"} {
		if isDemoProfile(p) {
			t.Fatalf("isDemoProfile(%q) = true, want false", p)
		}
	}
}

// handleUC08's opt-in to carry a not-covered CRD verdict through to PAS (D-S2-2) must
// fire for demo the same way it fires for provider-data — both target br-payer's J3490
// NOT-COVERED family — while every other lane keeps the generic FR-G25/AI-1 stop.
func TestHandleUC08_ProceedOnNotCovered_DemoOptIn(t *testing.T) {
	src, err := os.ReadFile("originate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	fn := extractFunc(t, string(src), "handleUC08")
	if !strings.Contains(fn, `targetsBrPayer(g.cfg.OriginationProfile) || isDemoProfile(g.cfg.OriginationProfile)`) {
		t.Fatalf("handleUC08 does not opt demo into proceedOnNotCovered alongside provider-data")
	}
	// REJECTION: no third posture silently rides the opt-in — a literal false stays the
	// ONLY thing runCRDThenDTROrder's proceedOnNotCovered param sees for every profile
	// that is neither provider-data nor demo.
	if targetsBrPayer("unknown-lane") || isDemoProfile("unknown-lane") {
		t.Fatal("an unrecognized profile must not opt into proceedOnNotCovered")
	}
}

// extractFunc returns the source text of the named top-level Gateway method (brace-balanced).
// Shared by the static wiring guards in this package.
func extractFunc(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "func (g *Gateway) "+name+"(")
	if i < 0 {
		t.Fatalf("func %s not found", name)
	}
	depth, start := 0, strings.Index(src[i:], "{")+i
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[i : j+1]
			}
		}
	}
	t.Fatalf("unbalanced braces in %s", name)
	return ""
}
