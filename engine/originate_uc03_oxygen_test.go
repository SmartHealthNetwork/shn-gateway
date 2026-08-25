// originate_uc03_oxygen_test.go — TestUC03_AutoFillMutations is the executable mutation
// evidence for homeOxygenAutoFillEvidence (the hermetic FR-17 source=auto attribution
// proof, register §9 row 4 / §11) that test/conformance's TestUC03_AutoApproved names but
// never carried: each row independently breaks one condition the cross-check depends on
// and asserts the specific item it protects disappears, never the whole result going
// empty by coincidence.
package engine

import (
	"encoding/json"
	"strconv"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// clinicalOnlySoR is a minimal SystemOfRecord fake for homeOxygenAutoFillEvidence's
// tests: it answers ClinicalContext from a canned value and panics on every other
// method — homeOxygenAutoFillEvidence reads ONLY ClinicalContext, so a call to any
// other method would mean the test is exercising more than it claims to.
type clinicalOnlySoR struct {
	cc    shnsdk.ClinicalContext
	found bool
}

func (clinicalOnlySoR) ResolvePatient(string) (string, Demo, bool) {
	panic("unused by homeOxygenAutoFillEvidence")
}
func (clinicalOnlySoR) PatientFHIRRef(string) (string, bool) {
	panic("unused by homeOxygenAutoFillEvidence")
}
func (clinicalOnlySoR) CoverageInforce(string) (bool, string) {
	panic("unused by homeOxygenAutoFillEvidence")
}
func (s clinicalOnlySoR) ClinicalContext(string) (shnsdk.ClinicalContext, bool) {
	return s.cc, s.found
}
func (clinicalOnlySoR) SupplementalReport(string) ([]byte, bool) {
	panic("unused by homeOxygenAutoFillEvidence")
}
func (clinicalOnlySoR) FacilityRecords(string) (map[string][]byte, bool) {
	panic("unused by homeOxygenAutoFillEvidence")
}
func (clinicalOnlySoR) OpenOrder(string) ([]byte, bool) {
	panic("unused by homeOxygenAutoFillEvidence")
}
func (clinicalOnlySoR) OpenCoverage(string) ([]byte, bool) {
	panic("unused by homeOxygenAutoFillEvidence")
}
func (clinicalOnlySoR) ResolveByReference(string) ([]byte, bool) {
	panic("unused by homeOxygenAutoFillEvidence")
}

// oxygenQRJSON builds a minimal QuestionnaireResponse carrying a numeric answer for
// linkId "2.2" (O2Sat) and/or "2.3" (PaO2) — whichever is non-empty. Mirrors the shape
// questionnaireResponseNumericAnswers reads (valueDecimal).
func oxygenQRJSON(t *testing.T, o2sat, pao2 string) []byte {
	t.Helper()
	var items []map[string]any
	addItem := func(linkID, value string) {
		if value == "" {
			return
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("oxygenQRJSON: value %q is not numeric: %v", value, err)
		}
		items = append(items, map[string]any{
			"linkId": linkID,
			"answer": []map[string]any{{"valueDecimal": v}},
		})
	}
	addItem("2.2", o2sat)
	addItem("2.3", pao2)
	qr := map[string]any{"resourceType": "QuestionnaireResponse", "status": "completed", "item": items}
	raw, err := json.Marshal(qr)
	if err != nil {
		t.Fatalf("oxygenQRJSON: marshal: %v", err)
	}
	return raw
}

// TestUC03_AutoFillMutations is the mutation evidence test/conformance's
// TestUC03_AutoApproved cites: homeOxygenAutoFillEvidence attributes an item
// Origin="auto" ONLY when the QR's answer for that linkId EXACTLY matches the member's
// OWN seeded ClinicalContext value — never on the QR's say-so alone, and never for any
// linkId outside {2.2, 2.3}.
func TestUC03_AutoFillMutations(t *testing.T) {
	member := "MBR-TEST-OXYGEN"

	// Positive control: both cross-checks match ⇒ both items attributed.
	t.Run("both-match-both-attributed", func(t *testing.T) {
		g := &Gateway{cfg: Config{SoR: clinicalOnlySoR{found: true, cc: shnsdk.ClinicalContext{
			OxygenSaturationPct: "89", OxygenSaturationRef: "Observation/o2sat-1",
			ArterialPaO2mmHg: "56", ArterialPaO2Ref: "Observation/pao2-1",
		}}}}
		got := g.homeOxygenAutoFillEvidence(member, oxygenQRJSON(t, "89", "56"))
		if len(got) != 2 {
			t.Fatalf("want 2 auto-filled items, got %d: %+v", len(got), got)
		}
	})

	// Mutation 1 (R3 evidence item 1): the QR's 2.2 answer diverges from the seed ⇒ 2.2
	// is UNATTRIBUTED, 2.3 is untouched. A cross-check keyed on presence alone (not
	// value equality) would still attribute 2.2 here.
	t.Run("2.2-diverges-from-seed-drops-2.2-only", func(t *testing.T) {
		g := &Gateway{cfg: Config{SoR: clinicalOnlySoR{found: true, cc: shnsdk.ClinicalContext{
			OxygenSaturationPct: "89", OxygenSaturationRef: "Observation/o2sat-1",
			ArterialPaO2mmHg: "56", ArterialPaO2Ref: "Observation/pao2-1",
		}}}}
		got := g.homeOxygenAutoFillEvidence(member, oxygenQRJSON(t, "77", "56"))
		if len(got) != 1 || got[0].LinkID != "2.3" {
			t.Fatalf("want exactly [2.3], got %+v", got)
		}
	})

	// Mutation 2 (R3 evidence item 2), symmetric: the QR's 2.3 answer diverges ⇒ 2.3 is
	// UNATTRIBUTED, 2.2 is untouched.
	t.Run("2.3-diverges-from-seed-drops-2.3-only", func(t *testing.T) {
		g := &Gateway{cfg: Config{SoR: clinicalOnlySoR{found: true, cc: shnsdk.ClinicalContext{
			OxygenSaturationPct: "89", OxygenSaturationRef: "Observation/o2sat-1",
			ArterialPaO2mmHg: "56", ArterialPaO2Ref: "Observation/pao2-1",
		}}}}
		got := g.homeOxygenAutoFillEvidence(member, oxygenQRJSON(t, "89", "99"))
		if len(got) != 1 || got[0].LinkID != "2.2" {
			t.Fatalf("want exactly [2.2], got %+v", got)
		}
	})

	// A seeded fact with no backing Observation ref (SourceRef=="") never attributes,
	// even when the QR's answer would otherwise match — an attribution with no source is
	// not evidence.
	t.Run("empty-source-ref-never-attributed", func(t *testing.T) {
		g := &Gateway{cfg: Config{SoR: clinicalOnlySoR{found: true, cc: shnsdk.ClinicalContext{
			OxygenSaturationPct: "89", OxygenSaturationRef: "",
			ArterialPaO2mmHg: "56", ArterialPaO2Ref: "Observation/pao2-1",
		}}}}
		got := g.homeOxygenAutoFillEvidence(member, oxygenQRJSON(t, "89", "56"))
		if len(got) != 1 || got[0].LinkID != "2.3" {
			t.Fatalf("want exactly [2.3] (2.2 has no SourceRef), got %+v", got)
		}
	})

	// No ClinicalContext at all for the member ⇒ nil, not merely empty — the cross-check
	// has nothing to check against.
	t.Run("no-clinical-context-returns-nil", func(t *testing.T) {
		g := &Gateway{cfg: Config{SoR: clinicalOnlySoR{found: false}}}
		got := g.homeOxygenAutoFillEvidence(member, oxygenQRJSON(t, "89", "56"))
		if got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})

	// The QR carries no numeric answers at all (the pre-attestation shell shape) ⇒ nil.
	t.Run("qr-carries-no-answers-returns-nil", func(t *testing.T) {
		g := &Gateway{cfg: Config{SoR: clinicalOnlySoR{found: true, cc: shnsdk.ClinicalContext{
			OxygenSaturationPct: "89", OxygenSaturationRef: "Observation/o2sat-1",
			ArterialPaO2mmHg: "56", ArterialPaO2Ref: "Observation/pao2-1",
		}}}}
		got := g.homeOxygenAutoFillEvidence(member, oxygenQRJSON(t, "", ""))
		if got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
}
