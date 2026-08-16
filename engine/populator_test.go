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
	pkg := wrapSandboxPackage(t) // one-entry package around the sandbox Questionnaire

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

// TestManagedPopulatorBuildsAtLine proves managedPopulator.Populate threads
// PopulateContext.Line into shnsdk.FillQuestionnaireAtLine (closes an
// earlier KNOWN GAP): a 2.2 request produces a QR carrying the 2.2 wire markers
// (per_line_uc_test.go's assertWireBuiltAtLines discipline — the intendedUse
// code system + the qr-coverage extension, the only markers that distinguish
// 2.2 from 2.0/2.1 on this build's own DTRLineDef), an empty Line is the
// documented "" → 2.0 delegate (a regression fence: byte-identical to an
// explicit "2.0" request, so the additive seam for kit/custom Populator
// implementations never silently changes today's default output), and an
// unknown line fails closed — mirroring every other AtLine builder
// (FillQuestionnaireAtLine/FillQuestionnaireFromAnswersAtLine), never a silent
// 2.0 fallback.
func TestManagedPopulatorBuildsAtLine(t *testing.T) {
	sor := NewStubHolderData()
	member := "MBR-COVERED"
	pkg := wrapSandboxPackage(t)
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	mp := newManagedPopulator(sor)

	populateAt := func(t *testing.T, line string) ([]byte, error) {
		t.Helper()
		got, _, err := mp.Populate(context.Background(), pkg, PopulateContext{
			Member: member, PatientRef: "Patient/" + member, CoverageRef: "Coverage/" + member,
			OrderRef: "ServiceRequest/sr-" + member, Authored: clock(), Line: line,
		})
		return got, err
	}

	t.Run("2.2 carries the 2.2 wire markers", func(t *testing.T) {
		got, err := populateAt(t, "2.2")
		if err != nil {
			t.Fatalf("Populate at line 2.2: %v", err)
		}
		def, ok := shnsdk.DTRLineDef("2.2")
		if !ok {
			t.Fatal("no DTRLineDef for 2.2")
		}
		if !bytes.Contains(got, []byte(def.IntendedUseCodeSystem)) {
			t.Errorf("QR at line 2.2 missing intendedUse code system %q:\n%s", def.IntendedUseCodeSystem, got)
		}
		if !bytes.Contains(got, []byte("/StructureDefinition/qr-coverage")) {
			t.Errorf("QR at line 2.2 missing qr-coverage extension:\n%s", got)
		}
	})

	t.Run(`empty Line delegates to 2.0 (regression fence)`, func(t *testing.T) {
		want, err := populateAt(t, "2.0")
		if err != nil {
			t.Fatalf("Populate at explicit line 2.0: %v", err)
		}
		got, err := populateAt(t, "")
		if err != nil {
			t.Fatalf("Populate with Line unset: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Line=\"\" QR != explicit Line=\"2.0\" QR:\n got=%s\nwant=%s", got, want)
		}
	})

	t.Run("unknown line fails closed", func(t *testing.T) {
		_, err := populateAt(t, "9.9")
		if err == nil {
			t.Fatal("Populate at unknown line 9.9: want error, got nil (fail-closed expected)")
		}
	})
}

// wrapSandboxPackage builds the one-entry $questionnaire-package around the
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
