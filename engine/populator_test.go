package engine

import (
	"bytes"
	"context"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// managedPopulator.Populate must produce EXACTLY the QR the inline path produced:
// extract the Questionnaire from the package, read ClinicalContext, FillQuestionnaire.
func TestManagedPopulator_ByteParityWithInlineFill(t *testing.T) {
	sor := NewStubHolderData() // the demo SoR personas
	member := "MBR-COVERED"
	pkg := wrapSandboxPackage(t) // §6.2 one-entry package around the sandbox Questionnaire

	cc, ok := sor.ClinicalContext(member)
	if !ok {
		t.Fatal("no clinical context for member")
	}
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	want, err := shnsdk.FillQuestionnaire(shnsdk.SandboxLumbarQuestionnaire(), cc, shnsdk.QRContext{
		PatientRef: "Patient/" + member, CoverageRef: "Coverage/" + member,
		OrderRef: "ServiceRequest/sr-" + member, Authored: clock(),
	})
	if err != nil {
		t.Fatalf("inline fill: %v", err)
	}

	mp := newManagedPopulator(sor)
	got, fill, err := mp.Populate(context.Background(), pkg, PopulateContext{
		Member: member, PatientRef: "Patient/" + member, CoverageRef: "Coverage/" + member,
		OrderRef: "ServiceRequest/sr-" + member, Authored: clock(),
	})
	if err != nil {
		t.Fatalf("managed populate: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("QR mismatch:\n got=%s\nwant=%s", got, want)
	}
	if len(fill) == 0 {
		t.Fatal("expected non-empty fill summary from managed backend")
	}
}

// wrapSandboxPackage builds the §6.2 one-entry $questionnaire-package around the
// sandbox Questionnaire (the shape originate.go receives on the DTR-fetch leg).
func wrapSandboxPackage(t *testing.T) []byte {
	t.Helper()
	q := shnsdk.SandboxLumbarQuestionnaire()
	pkg, err := buildQuestionnairePackage(q) // engine helper in davincimap.go
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	return pkg
}
