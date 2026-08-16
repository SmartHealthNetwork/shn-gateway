package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// ---- pa.dtr 2.0<->2.1 (identity) -------------------------------------------
//
// No dtrStep2021Up/Down functions exist (compat.go's row: nil Up/Down,
// Class full) — these tests exercise the identity path through
// TransformDTRForTest/applyChain directly on real goldens, proving the
// pass-through is byte-identical BOTH WAYS for both document shapes DTR
// carries (bare QuestionnaireResponse, and a $questionnaire-package
// response Bundle).

// TestDTRQRChain2020To21Identity: the bare QR autofill goldens at 2.0 and
// 2.1 are already byte-identical (per_line_uc_test.go:389-407's record) —
// transforming the 2.0 golden Up to 2.1 (and the 2.1 golden Down to 2.0)
// must reproduce that exactly, with reports naming a true identity module
// (no Carried/Synthesized).
func TestDTRQRChain2020To21Identity(t *testing.T) {
	in20 := pasGolden(t, "questionnaireresponse-autofill.json")
	want21 := pasGolden(t, "2.1/questionnaireresponse-autofill.json")
	if !bytes.Equal(in20, want21) {
		t.Fatalf("precondition: 2.0/2.1 autofill goldens are expected byte-identical")
	}

	up, reports, err := TransformDTRForTest("2.0", "2.1", in20, corr)
	if err != nil {
		t.Fatalf("2.0->2.1: unexpected error: %v", err)
	}
	assertJSONEqual(t, up, want21, "2.0->2.1 QR identity")
	if len(reports) != 1 || len(reports[0].Carried) != 0 || len(reports[0].Synthesized) != 0 {
		t.Fatalf("want exactly 1 lossless identity report, got %+v", reports)
	}

	down, reports2, err := TransformDTRForTest("2.1", "2.0", want21, corr)
	if err != nil {
		t.Fatalf("2.1->2.0: unexpected error: %v", err)
	}
	assertJSONEqual(t, down, in20, "2.1->2.0 QR identity")
	if len(reports2) != 1 || len(reports2[0].Carried) != 0 || len(reports2[0].Synthesized) != 0 {
		t.Fatalf("want exactly 1 lossless identity report, got %+v", reports2)
	}
}

// TestDTRPackageChain2020To21Identity proves the package-SHAPE identity
// holds both ways too: the 2.0 golden package (Questionnaire only, no QR
// entry — "unconstrained") upcast to 2.1 ("qr-optional", min=0) is
// unchanged, and the 2.1 golden package (Questionnaire + QR entry) downcast
// to 2.0 ("unconstrained", tolerates anything) is unchanged.
func TestDTRPackageChain2020To21Identity(t *testing.T) {
	pkg20 := pasGolden(t, "questionnaire-package-pa-lumbar-mri.json")
	pkg21 := pasGolden(t, "2.1/questionnaire-package-pa-lumbar-mri.json")

	up, _, err := TransformDTRForTest("2.0", "2.1", pkg20, corr)
	if err != nil {
		t.Fatalf("2.0->2.1 package: unexpected error: %v", err)
	}
	assertJSONEqual(t, up, pkg20, "2.0->2.1 package identity (QR-less 2.0 package unchanged)")

	// Up, QR-PRESENT sub-case (the manifest-class claim demands BOTH
	// directions with "QR entry present or absent"): pkg21 is itself a
	// valid 2.0 payload too — 2.0's "unconstrained" profile permits a
	// QuestionnaireResponse entry, it just doesn't require one — so feeding
	// it through the Up direction must be byte-unchanged with empty loss,
	// closing the loop the QR-less-only Up case above left open.
	upQR, upQRReports, err := TransformDTRForTest("2.0", "2.1", pkg21, corr)
	if err != nil {
		t.Fatalf("2.0->2.1 package (QR-present source): unexpected error: %v", err)
	}
	assertJSONEqual(t, upQR, pkg21, "2.0->2.1 package identity (QR-bearing 2.0-valid package unchanged)")
	if len(upQRReports) != 1 || len(upQRReports[0].Carried) != 0 || len(upQRReports[0].Synthesized) != 0 {
		t.Fatalf("want exactly 1 lossless identity report, got %+v", upQRReports)
	}

	down, _, err := TransformDTRForTest("2.1", "2.0", pkg21, corr)
	if err != nil {
		t.Fatalf("2.1->2.0 package: unexpected error: %v", err)
	}
	assertJSONEqual(t, down, pkg21, "2.1->2.0 package identity (QR-bearing 2.1 package unchanged)")
}

// ---- pa.dtr 2.1<->2.2 -------------------------------------------------------

// TestDTRStep2122Up_AutofillGolden is Step 2's worked example: the 2.1
// autofill golden transformed Up to 2.2 must match the 2.2 autofill golden
// exactly — coverage relocated qr-context->qr-coverage, intendedUse system
// temp->coverage-information-codes, origin auto->auto-client.
func TestDTRStep2122Up_AutofillGolden(t *testing.T) {
	in := pasGolden(t, "2.1/questionnaireresponse-autofill.json")
	want := pasGolden(t, "2.2/questionnaireresponse-autofill.json")

	out, report, err := dtrStep2122Up(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONEqual(t, out, want, "2.1->2.2 autofill QR")
	if len(report.Carried) != 0 || len(report.Synthesized) != 0 {
		t.Fatalf("full-class content move must not report Carried/Synthesized, got %+v", report)
	}
	if report.Module != "pa.dtr 2.1->2.2" || report.Source != "2.1" || report.Target != "2.2" {
		t.Fatalf("unexpected report trace: %+v", report)
	}
}

// TestDTRStep2122Down_AutofillGolden is the mirror: the 2.2 golden downcast
// to 2.1 must match the 2.1 golden exactly.
func TestDTRStep2122Down_AutofillGolden(t *testing.T) {
	in := pasGolden(t, "2.2/questionnaireresponse-autofill.json")
	want := pasGolden(t, "2.1/questionnaireresponse-autofill.json")

	out, report, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONEqual(t, out, want, "2.2->2.1 autofill QR")
	if len(report.Carried) != 0 {
		t.Fatalf("no itemWeight in this fixture, want no Carried entries, got %+v", report.Carried)
	}
}

// TestDTRStep2122Up_AttestedGolden proves the origin-code remap targets ONLY
// the "auto" family: the attested golden's clinician-sourced
// functional-status-oswestry item (source="manual") must survive Up
// UNCHANGED while the auto items move auto->auto-client — a real fixture,
// not a hand-rolled one, pinning the "never clobber unrelated origin codes"
// guard.
func TestDTRStep2122Up_AttestedGolden(t *testing.T) {
	in := pasGolden(t, "2.1/questionnaireresponse-attested.json")
	want := pasGolden(t, "2.2/questionnaireresponse-attested.json")

	out, _, err := dtrStep2122Up(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONEqual(t, out, want, "2.1->2.2 attested QR (manual-origin item untouched)")
}

// TestDTRStep2122Down_AttestedGolden is the Down mirror.
func TestDTRStep2122Down_AttestedGolden(t *testing.T) {
	in := pasGolden(t, "2.2/questionnaireresponse-attested.json")
	want := pasGolden(t, "2.1/questionnaireresponse-attested.json")

	out, _, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONEqual(t, out, want, "2.2->2.1 attested QR (manual-origin item untouched)")
}

// TestDTRStep2122Up_PackageGolden proves the Up direction transforms a full
// $questionnaire-package response Bundle (Questionnaire + QR entries) end
// to end, matching the 2.2 package golden exactly.
func TestDTRStep2122Up_PackageGolden(t *testing.T) {
	in := pasGolden(t, "2.1/questionnaire-package-pa-lumbar-mri.json")
	want := pasGolden(t, "2.2/questionnaire-package-pa-lumbar-mri.json")

	out, _, err := dtrStep2122Up(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONEqual(t, out, want, "2.1->2.2 package bundle")
}

// TestDTRStep2122Down_PackageGolden is the Down mirror.
func TestDTRStep2122Down_PackageGolden(t *testing.T) {
	in := pasGolden(t, "2.2/questionnaire-package-pa-lumbar-mri.json")
	want := pasGolden(t, "2.1/questionnaire-package-pa-lumbar-mri.json")

	out, _, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONEqual(t, out, want, "2.2->2.1 package bundle")
}

// TestDTRStep2122Up_QRLessPackageGated is the package-shape rejection test:
// a 2.1 package Bundle with NO QuestionnaireResponse entry (2.1's
// "qr-optional" tolerates this — it's a real, conformant 2.1 shape) upcast
// to 2.2 ("qr-required") must refuse — the module never fabricates a QR —
// with the typed SemanticChangeError naming the missing element.
func TestDTRStep2122Up_QRLessPackageGated(t *testing.T) {
	// Build a QR-less package Bundle by stripping the 2.1 golden's second
	// entry (the QuestionnaireResponse) — leaves a real Questionnaire-only
	// package, the exact shape 2.1's own "unconstrained-successor" profile
	// permits.
	var pkg map[string]any
	if err := json.Unmarshal(pasGolden(t, "2.1/questionnaire-package-pa-lumbar-mri.json"), &pkg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	entries := pkg["entry"].([]any)
	pkg["entry"] = entries[:1] // Questionnaire only
	in, err := json.Marshal(pkg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out, report, err := dtrStep2122Up(in, corr)
	if err == nil {
		t.Fatalf("want a semantic-change refusal, got success (out=%q report=%+v)", out, report)
	}
	var scErr *SemanticChangeError
	if !errors.As(err, &scErr) {
		t.Fatalf("want *SemanticChangeError (errors.As), got %T: %v", err, err)
	}
	if scErr.Contract != "pa.dtr" || scErr.From != "2.1" || scErr.To != "2.2" || scErr.Direction != "up" {
		t.Fatalf("unexpected error fields: %+v", scErr)
	}
	if len(scErr.MissingElements) != 1 || scErr.MissingElements[0] != "Bundle.entry:questionnaireResponse" {
		t.Fatalf("MissingElements = %v, want [Bundle.entry:questionnaireResponse]", scErr.MissingElements)
	}
	if out != nil {
		t.Fatalf("a refused step must return nil output, got %q", out)
	}
}

// TestDTRStep2122Up_MultiCoverageGated is the spec's canonical
// semantic-change refusal, Step 2's mandated rejection test: a 2.1 QR whose
// qr-context slice carries TWO Coverage-referencing entries (a genuinely
// multi-coverage source) has no single honest source for 2.2's
// singular-by-convention qr-coverage relocation and must refuse.
func TestDTRStep2122Up_MultiCoverageGated(t *testing.T) {
	in := []byte(`{
		"resourceType":"QuestionnaireResponse","id":"qr-multi","status":"completed",
		"questionnaire":"http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri|1.0.0",
		"subject":{"reference":"Patient/MBR-COVERED"},
		"authored":"2026-06-04T00:00:00Z",
		"extension":[
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"Coverage/MBR-PRIMARY"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"Coverage/MBR-SECONDARY"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/sr-MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp","code":"withpa","display":"Information needed for a prior authorization"}]}}
		],
		"item":[]
	}`)

	out, report, err := dtrStep2122Up(in, corr)
	if err == nil {
		t.Fatalf("want a semantic-change refusal, got success (out=%q report=%+v)", out, report)
	}
	var scErr *SemanticChangeError
	if !errors.As(err, &scErr) {
		t.Fatalf("want *SemanticChangeError (errors.As), got %T: %v", err, err)
	}
	if scErr.Contract != "pa.dtr" || scErr.From != "2.1" || scErr.To != "2.2" || scErr.Direction != "up" {
		t.Fatalf("unexpected error fields: %+v", scErr)
	}
	if len(scErr.MissingElements) != 1 {
		t.Fatalf("MissingElements = %v, want exactly 1 entry naming the ambiguity", scErr.MissingElements)
	}
	if want := "QuestionnaireResponse.extension:qr-coverage"; !bytes.Contains([]byte(scErr.MissingElements[0]), []byte(want)) {
		t.Fatalf("MissingElements[0] = %q, want it to name %q", scErr.MissingElements[0], want)
	}
	if out != nil {
		t.Fatalf("a refused step must return nil output, got %q", out)
	}
}

// TestDTRStep2122Up_NoCoverageGated: the defensive symmetric case — a
// foreign 2.1 QR with NO Coverage-referencing qr-context entry at all has no
// honest source to mint the now-required qr-coverage extension from.
func TestDTRStep2122Up_NoCoverageGated(t *testing.T) {
	in := []byte(`{
		"resourceType":"QuestionnaireResponse","id":"qr-none","status":"completed",
		"questionnaire":"http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri|1.0.0",
		"subject":{"reference":"Patient/MBR-COVERED"},
		"authored":"2026-06-04T00:00:00Z",
		"extension":[
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/sr-MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp","code":"withpa","display":"Information needed for a prior authorization"}]}}
		],
		"item":[]
	}`)

	_, _, err := dtrStep2122Up(in, corr)
	if err == nil {
		t.Fatalf("want a semantic-change refusal for a coverage-less source")
	}
	var scErr *SemanticChangeError
	if !errors.As(err, &scErr) {
		t.Fatalf("want *SemanticChangeError (errors.As), got %T: %v", err, err)
	}
}

// TestDTRStep2122_ItemWeightCarryRoundTrip pins the loss-policy's carry
// mechanism for the one genuine 2.2-only element with no 2.1 slot
// (item.answer.extension:itemWeight — dtrItemWeightExt's doc comment): a
// hand-built 2.2 QR carrying one (SHN's own builder never does — no honest
// weight source — so a real golden cannot exercise this; the fixture is
// realistic-but-synthetic, same posture as transform_pas_test.go's
// TestPasStep2122_CarryRoundTrip) downcast to 2.1 carries it into
// shn-carried-content, and a subsequent Up back to 2.2 restores it
// BYTE-IDENTICAL (sdk/carry.go's Restore(Carry(x))==x contract).
func TestDTRStep2122_ItemWeightCarryRoundTrip(t *testing.T) {
	itemWeightExt := `{"url":"http://hl7.org/fhir/StructureDefinition/itemWeight","valueDecimal":0.5}`
	in := []byte(`{
		"resourceType":"QuestionnaireResponse","id":"qr-weighted","status":"completed",
		"questionnaire":"http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri|1.0.0",
		"subject":{"reference":"Patient/MBR-COVERED"},
		"authored":"2026-06-04T00:00:00Z",
		"extension":[
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage","valueReference":{"reference":"Coverage/MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/sr-MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes","code":"withpa","display":"Information needed for a prior authorization"}]}}
		],
		"item":[{"linkId":"conservative-therapy-weeks","answer":[{"valueInteger":6,"extension":[` + itemWeightExt + `]}]}]
	}`)

	down, downReport, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: unexpected error: %v", err)
	}
	if len(downReport.Carried) != 1 {
		t.Fatalf("want exactly 1 Carried entry, got %+v", downReport.Carried)
	}
	if downReport.Carried[0].Path != "QuestionnaireResponse.item.answer.extension:itemWeight" {
		t.Fatalf("Carried[0].Path = %q, want %q", downReport.Carried[0].Path, "QuestionnaireResponse.item.answer.extension:itemWeight")
	}

	var downDoc map[string]any
	if err := json.Unmarshal(down, &downDoc); err != nil {
		t.Fatalf("decode down output: %v", err)
	}
	items := downDoc["item"].([]any)
	answer := items[0].(map[string]any)["answer"].([]any)[0].(map[string]any)
	exts := answer["extension"].([]any)
	if len(exts) != 1 {
		t.Fatalf("want exactly 1 extension after carry (the wrapper), got %d: %+v", len(exts), exts)
	}
	if exts[0].(map[string]any)["url"] != "http://smarthealth.network/fhir/StructureDefinition/shn-carried-content" {
		t.Fatalf("extension[0] is not the shn-carried-content wrapper: %v", exts[0])
	}

	up, upReport, err := dtrStep2122Up(down, corr)
	if err != nil {
		t.Fatalf("up: unexpected error: %v", err)
	}
	if len(upReport.Carried) != 0 || len(upReport.Synthesized) != 0 {
		t.Fatalf("restore must not itself be reported as Carried/Synthesized, got %+v", upReport)
	}
	var upDoc struct {
		Item []struct {
			Answer []struct {
				Extension []json.RawMessage `json:"extension"`
			} `json:"answer"`
		} `json:"item"`
	}
	if err := json.Unmarshal(up, &upDoc); err != nil {
		t.Fatalf("decode up output: %v", err)
	}
	gotExts := upDoc.Item[0].Answer[0].Extension
	if len(gotExts) != 1 {
		t.Fatalf("want exactly 1 extension after restore, got %d: %s", len(gotExts), up)
	}
	if !bytes.Equal(gotExts[0], []byte(itemWeightExt)) {
		t.Fatalf("carry round-trip is not byte-faithful:\n got  %s\n want %s", gotExts[0], itemWeightExt)
	}
}

// ---- chain composition + determinism ---------------------------------------

// TestDTRChain2020To22ThroughBothSteps proves the two steps compose through
// chainFor/applyChain exactly as a real 2.0->2.2 leg would.
func TestDTRChain2020To22ThroughBothSteps(t *testing.T) {
	in := pasGolden(t, "questionnaireresponse-autofill.json")
	want := pasGolden(t, "2.2/questionnaireresponse-autofill.json")

	out, reports, err := TransformDTRForTest("2.0", "2.2", in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONEqual(t, out, want, "2.0->2.2 chained QR")
	if len(reports) != 2 {
		t.Fatalf("want 2 step reports (2.0->2.1, 2.1->2.2), got %d: %+v", len(reports), reports)
	}
}

func TestDTRStep2122Up_Determinism(t *testing.T) {
	in := pasGolden(t, "2.1/questionnaireresponse-autofill.json")
	out1, r1, err1 := dtrStep2122Up(in, corr)
	out2, r2, err2 := dtrStep2122Up(in, corr)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected errors: %v / %v", err1, err2)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatalf("non-deterministic output: %q vs %q", out1, out2)
	}
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("non-deterministic report: %+v vs %+v", r1, r2)
	}
}

func TestDTRStep2122Down_Determinism(t *testing.T) {
	in := pasGolden(t, "2.2/questionnaireresponse-autofill.json")
	out1, r1, err1 := dtrStep2122Down(in, corr)
	out2, r2, err2 := dtrStep2122Down(in, corr)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected errors: %v / %v", err1, err2)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatalf("non-deterministic output: %q vs %q", out1, out2)
	}
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("non-deterministic report: %+v vs %+v", r1, r2)
	}
}
