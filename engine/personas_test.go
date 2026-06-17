package engine

import "testing"

func TestPersonaRefs_ResolveToPCIs(t *testing.T) {
	d := NewStubHolderData()
	want := map[string]bool{
		"coveredPci": true, "notCoveredPci": true, "uc04Pci": true,
		"uc05Pci": true, "uc06Pci": true, "uc07Pci": true, "uc08Pci": true,
	}
	if len(PersonaRefs) != len(want) {
		t.Fatalf("PersonaRefs len = %d, want %d", len(PersonaRefs), len(want))
	}
	seen := map[string]bool{}
	for _, ref := range PersonaRefs {
		if !want[ref.Key] {
			t.Errorf("unexpected key %q", ref.Key)
		}
		seen[ref.Key] = true
		pci, _, found := d.ResolvePatient(ref.MemberID)
		if !found || pci == "" {
			t.Errorf("ResolvePatient(%q) found=%v pci=%q; want a PCI", ref.MemberID, found, pci)
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("missing key %q", k)
		}
	}
}
