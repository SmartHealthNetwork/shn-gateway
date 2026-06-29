package engine

import (
	"os"
	"strings"
	"testing"
)

// sceneMember resolves the distinct provider-data member under provider-data, and the
// default (composite/sandbox) member otherwise (byte-identical to today).
func TestSceneMember_ProfileDispatch(t *testing.T) {
	gp := &Gateway{cfg: Config{OriginationProfile: "provider-data"}}
	if got := gp.sceneMember("MBR-UC04", "MBR-PD-UC04"); got != "MBR-PD-UC04" {
		t.Fatalf("provider-data sceneMember = %q, want MBR-PD-UC04", got)
	}
	gc := &Gateway{cfg: Config{OriginationProfile: "composite"}}
	if got := gc.sceneMember("MBR-UC04", "MBR-PD-UC04"); got != "MBR-UC04" {
		t.Fatalf("composite sceneMember = %q, want MBR-UC04 (must stay byte-identical)", got)
	}
}

// handleUC04 must thread sceneMember so provider-data reads its own seeded G0151 order
// (OpenOrder is keyed on member) while composite stays on MBR-UC04.
func TestHandleUC04_ThreadsSceneMember(t *testing.T) {
	src, err := os.ReadFile("originate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	fn := extractFunc(t, string(src), "handleUC04")
	if !strings.Contains(fn, `g.sceneMember("MBR-UC04", "MBR-PD-UC04")`) {
		t.Fatalf("handleUC04 does not thread sceneMember(MBR-UC04, MBR-PD-UC04)")
	}
	if strings.Contains(fn, `runCRDThenDTROrder(w, r, "MBR-UC04"`) {
		t.Fatalf("handleUC04 still passes the MBR-UC04 literal to runCRDThenDTROrder — must pass the sceneMember result")
	}
}

// handleUC08 must thread sceneMember so provider-data reads its own seeded J3490 order
// (OpenOrder is keyed on member) while composite stays on MBR-UC08.
func TestHandleUC08_ThreadsSceneMember(t *testing.T) {
	src, err := os.ReadFile("originate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	fn := extractFunc(t, string(src), "handleUC08")
	if !strings.Contains(fn, `g.sceneMember("MBR-UC08", "MBR-PD-UC08")`) {
		t.Fatalf("handleUC08 does not thread sceneMember(MBR-UC08, MBR-PD-UC08)")
	}
	if strings.Contains(fn, `runCRDThenDTROrder(w, r, "MBR-UC08"`) {
		t.Fatalf("handleUC08 still passes the MBR-UC08 literal to runCRDThenDTROrder — must pass the sceneMember result")
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
