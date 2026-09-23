package engine

import (
	"context"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestCensusSubjectResolverScopes(t *testing.T) {
	resolver := censusSubjectResolver("provider", "payer")
	want, _, _ := newCensusSoR().ResolvePatient("MBR-COVERED")
	for _, row := range []struct {
		name  string
		ref   PatientReference
		found bool
	}{
		{"provider relative", PatientReference{"provider", "fhir-relative", "Patient/MBR-COVERED"}, true},
		{"payer relative", PatientReference{"payer", "fhir-relative", "Patient/MBR-COVERED"}, true},
		{"known member namespace", PatientReference{"payer", shnsdk.MemberSystem, "MBR-COVERED"}, true},
		{"known server", PatientReference{"provider", "https://provider.example/fhir", "Patient/MBR-COVERED"}, true},
		{"foreign holder", PatientReference{"foreign", "fhir-relative", "Patient/MBR-COVERED"}, false},
		{"foreign namespace", PatientReference{"provider", "urn:unknown", "MBR-COVERED"}, false},
		{"foreign server", PatientReference{"provider", "https://foreign.example/fhir", "Patient/MBR-COVERED"}, false},
		{"unknown patient", PatientReference{"payer", "fhir-relative", "Patient/unknown"}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, found, err := resolver.ResolveSubject(context.Background(), row.ref)
			if err != nil || found != row.found || (found && got != want) || (!found && got != "") {
				t.Fatalf("got %q %v %v", got, found, err)
			}
		})
	}
	other, found, err := resolver.ResolveSubject(context.Background(), PatientReference{"payer", "fhir-relative", "Patient/MBR-NOTCOVERED"})
	if err != nil || !found || other == want {
		t.Fatal("foreign patient collapsed onto covered patient")
	}
}
