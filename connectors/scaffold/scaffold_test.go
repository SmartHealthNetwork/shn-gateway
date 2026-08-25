package scaffold

import (
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestScaffold_ResolvesWiredPersona(t *testing.T) {
	s := New()
	pci, demo, found := s.ResolvePatient("MBR-D-UC03")
	if !found {
		t.Fatal("MBR-D-UC03 not resolved by scaffold")
	}
	wantPCI := shnsdk.ResolvePCI("MBR-D-UC03", "1979-09-02", "Whitfield")
	if pci != wantPCI {
		t.Errorf("PCI = %q; want %q (Demo must match stub so the leg correlates)", pci, wantPCI)
	}
	if demo.FamilyName != "Whitfield" || demo.BirthDate != "1979-09-02" {
		t.Errorf("Demo = %+v; want {1979-09-02 Whitfield}", demo)
	}
}

func TestScaffold_DistinctClinicalMarker(t *testing.T) {
	s := New()
	cc, ok := s.ClinicalContext("MBR-D-UC03")
	if !ok {
		t.Fatal("no ClinicalContext for MBR-D-UC03")
	}
	if cc.ConservativeTherapyWeeks != 9 {
		t.Errorf("ConservativeTherapyWeeks = %d; want 9 (the override marker)", cc.ConservativeTherapyWeeks)
	}
}

func TestScaffold_UnknownMemberNotFound(t *testing.T) {
	s := New()
	if _, _, found := s.ResolvePatient("NOPE"); found {
		t.Error("unknown member must not resolve")
	}
}
