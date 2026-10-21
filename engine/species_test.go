package engine

import (
	"reflect"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestCertificationSeam_ReachesTheWorkstreamThatCertifies: the core's
// certification lookups (workstreams.go) answer through the adapter of the
// compiled-in workstream that certifies the species or declares the contract,
// and answer nothing where none does.
func TestCertificationSeam_ReachesTheWorkstreamThatCertifies(t *testing.T) {
	for _, species := range []string{"Claim", "ClaimResponse", "QuestionnaireResponse", "PASRequestBundle", "PASResponseBundle"} {
		if a := certificationAdapterFor(species); a != (paCertification{}) {
			t.Errorf("species %s is certified by %#v, want the PA workstream's adapter", species, a)
		}
	}
	for _, species := range []string{"", "Parameters", "toy"} {
		if a := certificationAdapterFor(species); a != nil {
			t.Errorf("species %q is certified by %#v, want none", species, a)
		}
		if order := candidateOrder(species, []byte(`{"resourceType":"Claim"}`)); order != nil {
			t.Errorf("species %q orders %v, want none", species, order)
		}
	}

	claim := []byte(`{"resourceType":"Claim","meta":{"profile":["` + certificationPAS + `profile-claim|` + pasPackage(t, "2.0") + `"]}}`)
	wantOrder, wantClaimed := paCertification{}.lineClaims(claim)
	for _, contract := range []string{"pa.pas", "pa.dtr", "pa.crd"} {
		if order, claimed := answerLineClaims(contract, claim); !reflect.DeepEqual(order, wantOrder) || !reflect.DeepEqual(claimed, wantClaimed) {
			t.Errorf("%s: %v %v, want the PA adapter's %v %v", contract, order, claimed, wantOrder, wantClaimed)
		}
	}
	if !reflect.DeepEqual(wantOrder, []string{"2.0", "2.2", "2.1"}) || !wantClaimed["2.0"] || len(wantClaimed) != 1 {
		t.Fatalf("the PA adapter orders a 2.0 Claim %v claiming %v", wantOrder, wantClaimed)
	}
	for _, contract := range []string{"", "fqs", "toy.echo", "pa.unknown"} {
		if order, claimed := answerLineClaims(contract, claim); order != nil || len(claimed) != 0 {
			t.Errorf("contract %q: %v %v, want no workstream's order", contract, order, claimed)
		}
	}
}

// TestCertificationSeam_TargetLineIsACertifiedLine: certification keeps an
// evidence record's target line only where its species is certified at that
// line on any leg (enqueueCertification). For every species the PA workstream
// certifies, those are exactly the PAS lines.
func TestCertificationSeam_TargetLineIsACertifiedLine(t *testing.T) {
	for _, species := range []string{"Claim", "ClaimResponse", "QuestionnaireResponse", "PASRequestBundle", "PASResponseBundle"} {
		for _, line := range []string{"2.0", "2.1", "2.2", "", "1.0", "2.3", "9.9"} {
			_, pas := shnsdk.PASLineDef(line)
			if _, ok := profileFor(species, line, ""); ok != pas {
				t.Errorf("species %s at line %q: certified %v, want %v", species, line, ok, pas)
			}
		}
	}
}

func pasPackage(t *testing.T, line string) string {
	t.Helper()
	d, ok := shnsdk.PASLineDef(line)
	if !ok {
		t.Fatalf("no PAS line %s", line)
	}
	return d.PackageVersion
}
