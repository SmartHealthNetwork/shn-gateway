package scaffold

import (
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestScaffold_ResolvesWiredPersona(t *testing.T) {
	s := New()
	pci, demo, found := s.ResolvePatient("MBR-COVERED")
	if !found {
		t.Fatal("MBR-COVERED not resolved by scaffold")
	}
	wantPCI := shnsdk.ResolvePCI("MBR-COVERED", "1975-04-02", "Johansson")
	if pci != wantPCI {
		t.Errorf("PCI = %q; want %q (Demo must match stub so the leg correlates)", pci, wantPCI)
	}
	if demo.FamilyName != "Johansson" || demo.BirthDate != "1975-04-02" {
		t.Errorf("Demo = %+v; want {1975-04-02 Johansson}", demo)
	}
}

func TestScaffold_DistinctClinicalMarker(t *testing.T) {
	s := New()
	cc, ok := s.ClinicalContext("MBR-COVERED")
	if !ok {
		t.Fatal("no ClinicalContext for MBR-COVERED")
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
