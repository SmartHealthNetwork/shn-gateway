package engine

import (
	"context"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// participantLinkageFixture adds one participant-owned synthetic identity to
// the scoped census links. It does not admit an unknown reference or holder.
func participantLinkageFixture(member, pci string, holders ...string) SubjectReferenceResolver {
	base := censusSubjectResolver(holders...)
	return subjectResolverFunc(func(ctx context.Context, ref PatientReference) (string, bool, error) {
		for _, holder := range holders {
			if ref.Holder != holder {
				continue
			}
			if (ref.System == "fhir-relative" && ref.Value == "Patient/"+member) ||
				(ref.System == shnsdk.MemberSystem && ref.Value == member) ||
				(ref.System == "https://"+holder+".example/fhir" && ref.Value == "Patient/"+member) {
				return pci, true, nil
			}
		}
		return base.ResolveSubject(ctx, ref)
	})
}

func TestParticipantLinkageFixtureScopes(t *testing.T) {
	resolver := participantLinkageFixture("MBR-PD-SYNTHETIC", "pci:synthetic", "provider", "payer")
	for _, tc := range []struct {
		ref  PatientReference
		want bool
	}{
		{PatientReference{"payer", "fhir-relative", "Patient/MBR-PD-SYNTHETIC"}, true},
		{PatientReference{"provider", shnsdk.MemberSystem, "MBR-PD-SYNTHETIC"}, true},
		{PatientReference{"payer", "https://payer.example/fhir", "Patient/MBR-PD-SYNTHETIC"}, true},
		{PatientReference{"stranger", "fhir-relative", "Patient/MBR-PD-SYNTHETIC"}, false},
		{PatientReference{"payer", "https://stranger.example/fhir", "Patient/MBR-PD-SYNTHETIC"}, false},
		{PatientReference{"payer", "fhir-relative", "Patient/another"}, false},
	} {
		got, found, err := resolver.ResolveSubject(context.Background(), tc.ref)
		if err != nil || found != tc.want || (found && got != "pci:synthetic") {
			t.Fatalf("resolve %+v = %q, %v, %v", tc.ref, got, found, err)
		}
	}
}
