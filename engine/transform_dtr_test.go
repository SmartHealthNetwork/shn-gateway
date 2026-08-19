package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
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

// dtrQRWithAnswers builds a 2.2-shaped QuestionnaireResponse whose single item
// carries the supplied answer JSON verbatim. The resource-level extensions are
// the shape dtrStep2122Down/Up require (a qr-coverage entry to relocate, a
// qr-context entry, an intendedUse coding on the 2.2 system) — without them the
// steps refuse for reasons that have nothing to do with the carry under test.
func dtrQRWithAnswers(answersJSON string) []byte {
	return []byte(`{
		"resourceType":"QuestionnaireResponse","id":"qr-weighted","status":"completed",
		"questionnaire":"http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri|1.0.0",
		"subject":{"reference":"Patient/MBR-COVERED"},
		"authored":"2026-06-04T00:00:00Z",
		"extension":[
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage","valueReference":{"reference":"Coverage/MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/sr-MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes","code":"withpa","display":"Information needed for a prior authorization"}]}}
		],
		"item":[{"linkId":"conservative-therapy-weeks","answer":[` + answersJSON + `]}]
	}`)
}

// dtrAnswersOf decodes a payload and returns item[0].answer as raw maps.
func dtrAnswersOf(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	items, _ := doc["item"].([]any)
	if len(items) == 0 {
		t.Fatalf("payload has no item: %s", payload)
	}
	answers, _ := items[0].(map[string]any)["answer"].([]any)
	out := make([]map[string]any, 0, len(answers))
	for _, a := range answers {
		am, ok := a.(map[string]any)
		if !ok {
			t.Fatalf("answer is not an object: %v", a)
		}
		out = append(out, am)
	}
	return out
}

// dtrExtURLsAt returns the urls of container["extension"], or nil when the
// container or its extension array is absent.
func dtrExtURLsAt(container map[string]any) []string {
	if container == nil {
		return nil
	}
	exts, _ := container["extension"].([]any)
	var out []string
	for _, e := range exts {
		em, _ := e.(map[string]any)
		u, _ := em["url"].(string)
		out = append(out, u)
	}
	return out
}

func dtrSubMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("expected %q to be an object, got %v (in %v)", key, m[key], m)
	}
	return v
}

const dtrTestItemWeight = `{"url":"http://hl7.org/fhir/StructureDefinition/itemWeight","valueDecimal":0.5}`

// TestDTRStep2122_ItemWeightCarryRoundTrip_PrimitiveValue pins the carry for a
// PRIMITIVE-valued answer, where FHIR puts the value's extensions on the
// sibling "_value[x]" object. The itemWeight SD contexts the element to
// QuestionnaireResponse.item.answer.value (verified live at the 2.2 lane: at
// answer.extension the validator reports a context ERROR naming the allowed
// contexts; at answer._valueInteger.extension it reports none), so this is the
// only shape a conformant 2.2 peer can send for an integer answer.
//
// The wrapper replaces the element IN PLACE — restore reverses it in the same
// array, so the wrapper's position is the record of which encoding it came
// from, and LossEntry.Path never has to encode it.
func TestDTRStep2122_ItemWeightCarryRoundTrip_PrimitiveValue(t *testing.T) {
	in := dtrQRWithAnswers(`{"valueInteger":6,"_valueInteger":{"extension":[` + dtrTestItemWeight + `]}}`)

	down, downReport, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: unexpected error: %v", err)
	}
	if len(downReport.Carried) != 1 {
		t.Fatalf("want exactly 1 Carried entry, got %+v", downReport.Carried)
	}
	if downReport.Carried[0].Path != dtrItemWeightLocus {
		t.Fatalf("Carried[0].Path = %q, want %q", downReport.Carried[0].Path, dtrItemWeightLocus)
	}

	// The wrapper sits where the element was — inside _valueInteger, NOT on the
	// answer. A wrapper on the answer would mean the carry relocated content.
	downAnswers := dtrAnswersOf(t, down)
	if got := dtrExtURLsAt(dtrSubMap(t, downAnswers[0], "_valueInteger")); len(got) != 1 ||
		got[0] != shnsdk.CarriedContentExtURL {
		t.Fatalf("_valueInteger.extension after carry = %v, want exactly the shn-carried-content wrapper", got)
	}
	if got := dtrExtURLsAt(downAnswers[0]); len(got) != 0 {
		t.Fatalf("answer.extension after carry = %v, want empty — the carry must not relocate the wrapper", got)
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
				UnderInteger struct {
					Extension []json.RawMessage `json:"extension"`
				} `json:"_valueInteger"`
			} `json:"answer"`
		} `json:"item"`
	}
	if err := json.Unmarshal(up, &upDoc); err != nil {
		t.Fatalf("decode up output: %v", err)
	}
	gotExts := upDoc.Item[0].Answer[0].UnderInteger.Extension
	if len(gotExts) != 1 {
		t.Fatalf("want exactly 1 extension under _valueInteger after restore, got %d: %s", len(gotExts), up)
	}
	if !bytes.Equal(gotExts[0], []byte(dtrTestItemWeight)) {
		t.Fatalf("carry round-trip is not byte-faithful:\n got  %s\n want %s", gotExts[0], dtrTestItemWeight)
	}
}

// TestDTRStep2122_ItemWeightCarryRoundTrip_CodingValue is the COMPLEX-value
// half: the itemWeight SD names "Coding" as a context in its own right, so an
// extension on a valueCoding answer sits on the value OBJECT, not on a sibling.
// Verified live: itemWeight at answer.valueCoding.extension validates clean at
// the 2.2 lane, and the carry wrapper validates clean at that locus at 2.1.
//
// Without this case a walker that only handles "_"+key passes every other test
// in this file while silently dropping every Coding-valued weight.
func TestDTRStep2122_ItemWeightCarryRoundTrip_CodingValue(t *testing.T) {
	in := dtrQRWithAnswers(`{"valueCoding":{"system":"http://terminology.hl7.org/CodeSystem/v2-0136","code":"N","display":"No","extension":[` + dtrTestItemWeight + `]}}`)

	down, downReport, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: unexpected error: %v", err)
	}
	if len(downReport.Carried) != 1 || downReport.Carried[0].Path != dtrItemWeightLocus {
		t.Fatalf("Carried = %+v, want exactly one entry at %q", downReport.Carried, dtrItemWeightLocus)
	}
	downAnswers := dtrAnswersOf(t, down)
	if got := dtrExtURLsAt(dtrSubMap(t, downAnswers[0], "valueCoding")); len(got) != 1 ||
		got[0] != shnsdk.CarriedContentExtURL {
		t.Fatalf("valueCoding.extension after carry = %v, want exactly the shn-carried-content wrapper", got)
	}

	up, _, err := dtrStep2122Up(down, corr)
	if err != nil {
		t.Fatalf("up: unexpected error: %v", err)
	}
	upAnswers := dtrAnswersOf(t, up)
	coding := dtrSubMap(t, upAnswers[0], "valueCoding")
	if got := dtrExtURLsAt(coding); len(got) != 1 || got[0] != "http://hl7.org/fhir/StructureDefinition/itemWeight" {
		t.Fatalf("valueCoding.extension after restore = %v, want the itemWeight extension back", got)
	}
	// The Coding's own fields must be untouched — the carry replaces one array
	// entry, it does not rewrite the value.
	if coding["code"] != "N" || coding["system"] != "http://terminology.hl7.org/CodeSystem/v2-0136" {
		t.Fatalf("valueCoding was modified by the carry round trip: %v", coding)
	}
}

// TestDTRStep2122_ItemWeightMultipleAnswers: a multi-answer item must restore
// each weight onto ITS OWN answer. In-place carry gets this for free — position
// is the record — but a path-driven restore, or one that took answer[0], would
// pass every single-answer test in this file and silently collapse them here.
// The two weights are deliberately DIFFERENT values so a swap is visible.
func TestDTRStep2122_ItemWeightMultipleAnswers(t *testing.T) {
	first := `{"url":"http://hl7.org/fhir/StructureDefinition/itemWeight","valueDecimal":0.25}`
	second := `{"url":"http://hl7.org/fhir/StructureDefinition/itemWeight","valueDecimal":0.75}`
	in := dtrQRWithAnswers(
		`{"valueInteger":6,"_valueInteger":{"extension":[` + first + `]}},` +
			`{"valueInteger":9,"_valueInteger":{"extension":[` + second + `]}}`)

	down, downReport, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: unexpected error: %v", err)
	}
	if len(downReport.Carried) != 2 {
		t.Fatalf("want 2 Carried entries (one per answer), got %+v", downReport.Carried)
	}

	up, _, err := dtrStep2122Up(down, corr)
	if err != nil {
		t.Fatalf("up: unexpected error: %v", err)
	}
	answers := dtrAnswersOf(t, up)
	if len(answers) != 2 {
		t.Fatalf("want 2 answers after restore, got %d", len(answers))
	}
	for i, want := range []struct {
		value  float64
		weight float64
	}{{6, 0.25}, {9, 0.75}} {
		gotValue, _ := answers[i]["valueInteger"].(float64)
		if gotValue != want.value {
			t.Errorf("answer[%d].valueInteger = %v, want %v — answers were reordered", i, gotValue, want.value)
		}
		exts, _ := dtrSubMap(t, answers[i], "_valueInteger")["extension"].([]any)
		if len(exts) != 1 {
			t.Fatalf("answer[%d]._valueInteger.extension = %v, want exactly one entry", i, exts)
		}
		em, _ := exts[0].(map[string]any)
		if got, _ := em["valueDecimal"].(float64); got != want.weight {
			t.Errorf("answer[%d] restored weight = %v, want %v — the weights landed on the wrong answers", i, got, want.weight)
		}
	}
}

// TestDTRStep2122_ItemWeightMixedValueTypes: one QR carrying a primitive-valued
// and a Coding-valued weighted answer must round-trip BOTH to their correct
// keys. A walker that resolves the container once per item, or that returns on
// the first match, passes the two single-shape tests above and fails here.
func TestDTRStep2122_ItemWeightMixedValueTypes(t *testing.T) {
	in := dtrQRWithAnswers(
		`{"valueInteger":6,"_valueInteger":{"extension":[` + dtrTestItemWeight + `]}},` +
			`{"valueCoding":{"code":"N","extension":[` + dtrTestItemWeight + `]}}`)

	down, downReport, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: unexpected error: %v", err)
	}
	if len(downReport.Carried) != 2 {
		t.Fatalf("want 2 Carried entries (primitive + Coding), got %+v", downReport.Carried)
	}

	up, _, err := dtrStep2122Up(down, corr)
	if err != nil {
		t.Fatalf("up: unexpected error: %v", err)
	}
	answers := dtrAnswersOf(t, up)
	if got := dtrExtURLsAt(dtrSubMap(t, answers[0], "_valueInteger")); len(got) != 1 ||
		got[0] != "http://hl7.org/fhir/StructureDefinition/itemWeight" {
		t.Errorf("primitive answer restored to _valueInteger.extension = %v, want the itemWeight extension", got)
	}
	if got := dtrExtURLsAt(dtrSubMap(t, answers[1], "valueCoding")); len(got) != 1 ||
		got[0] != "http://hl7.org/fhir/StructureDefinition/itemWeight" {
		t.Errorf("Coding answer restored to valueCoding.extension = %v, want the itemWeight extension", got)
	}
}

// TestDTRStep2122_ItemWeightAtAnswerExtensionIsNotCarried is THE rejection test
// for the locus claim. The DTR 2.2.0 profile differential declares a slice
// at QuestionnaireResponse.item.answer.extension:itemWeight, but the extension's
// own SD contexts it to answer.value — a profile cannot widen an extension's
// context, and the validator enforces the context, so that slice is
// unsatisfiable on the wire (an upstream IG defect). Verified live: an itemWeight
// at answer.extension is a context ERROR at the 2.2 line.
//
// The engine must therefore NOT treat that locus as the carry site. It is left
// exactly as it arrived — untouched, not carried, not dropped.
func TestDTRStep2122_ItemWeightAtAnswerExtensionIsNotCarried(t *testing.T) {
	in := dtrQRWithAnswers(`{"valueInteger":6,"extension":[` + dtrTestItemWeight + `]}`)

	down, downReport, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: unexpected error: %v", err)
	}
	if len(downReport.Carried) != 0 {
		t.Fatalf("itemWeight at answer.extension was carried (%+v) — that locus is context-illegal "+
			"and must not be the carry site", downReport.Carried)
	}
	if bytes.Contains(down, []byte(shnsdk.CarriedContentExtURL)) {
		t.Fatalf("downcast emitted a carry wrapper for an element at the illegal locus:\n%s", down)
	}
	// Not dropped either: it passes through. Dropping it would be a NEW silent
	// loss, the opposite of what this slice fixes.
	answers := dtrAnswersOf(t, down)
	if got := dtrExtURLsAt(answers[0]); len(got) != 1 ||
		got[0] != "http://hl7.org/fhir/StructureDefinition/itemWeight" {
		t.Fatalf("answer.extension after downcast = %v, want the itemWeight extension passed through untouched", got)
	}
}

// TestDTRStep2122_NoItemWeightCarriesNothing is the non-vacuity control: a QR
// with no itemWeight anywhere must produce no Carried entries and no wrapper.
// Without it, a walker that wrapped every extension it found at the value locus
// would satisfy all the positive tests above.
func TestDTRStep2122_NoItemWeightCarriesNothing(t *testing.T) {
	in := dtrQRWithAnswers(`{"valueInteger":6,"_valueInteger":{"extension":[{"url":"http://example.org/fhir/StructureDefinition/unrelated","valueString":"x"}]}},` +
		`{"valueCoding":{"code":"N","extension":[{"url":"http://example.org/fhir/StructureDefinition/other","valueString":"y"}]}}`)

	down, downReport, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: unexpected error: %v", err)
	}
	if len(downReport.Carried) != 0 {
		t.Fatalf("want no Carried entries for a QR with no itemWeight, got %+v", downReport.Carried)
	}
	if bytes.Contains(down, []byte(shnsdk.CarriedContentExtURL)) {
		t.Fatalf("downcast wrapped something with no itemWeight present:\n%s", down)
	}
	// The unrelated extensions survive untouched at both loci.
	answers := dtrAnswersOf(t, down)
	if got := dtrExtURLsAt(dtrSubMap(t, answers[0], "_valueInteger")); len(got) != 1 ||
		got[0] != "http://example.org/fhir/StructureDefinition/unrelated" {
		t.Errorf("_valueInteger.extension = %v, want the unrelated extension untouched", got)
	}
	if got := dtrExtURLsAt(dtrSubMap(t, answers[1], "valueCoding")); len(got) != 1 ||
		got[0] != "http://example.org/fhir/StructureDefinition/other" {
		t.Errorf("valueCoding.extension = %v, want the unrelated extension untouched", got)
	}
}

// TestDTRStep2122_LegacyWrapperAtAnswerExtensionStaysWrapped pins the STRICT
// restore decision. Carry and restore walk the same locus set,
// so a wrapper at answer.extension — what an engine built before this change
// produced, reachable only in a split-version round trip across a rolling
// upgrade — is left wrapped.
//
// The two tolerant alternatives are both worse: unwrapping IN PLACE would
// restore itemWeight to answer.extension and emit a 2.2 payload that fails
// validation, and unwrap-and-relocate would silently move content relative to
// what was carried. Leaving it wrapped loses nothing — shn-carried-content's
// context is Element, so it validates where it sits, and the content stays
// present and legible.
//
// The 2.1 input is produced by running Down, not hand-built: dtrStep2122Up
// requires a genuinely 2.1-shaped resource (its coverage reference under
// qr-context) and refuses a 2.2-shaped one for reasons unrelated to the carry.
// Routing through Down is also the truer model of the scenario — a wrapper at
// answer.extension is exactly what an older engine's Down emitted — and it
// pins the second half of strict: Down does not touch the stale wrapper
// either, since it scans only the value containers.
func TestDTRStep2122_LegacyWrapperAtAnswerExtensionStaysWrapped(t *testing.T) {
	wrapper, err := shnsdk.CarryElement(dtrItemWeightLocus, json.RawMessage(dtrTestItemWeight), "2.2")
	if err != nil {
		t.Fatalf("build wrapper: %v", err)
	}
	down, _, err := dtrStep2122Down(dtrQRWithAnswers(`{"valueInteger":6,"extension":[`+string(wrapper)+`]}`), corr)
	if err != nil {
		t.Fatalf("down: unexpected error: %v", err)
	}
	if got := dtrExtURLsAt(dtrAnswersOf(t, down)[0]); len(got) != 1 || got[0] != shnsdk.CarriedContentExtURL {
		t.Fatalf("answer.extension after downcast = %v, want the stale wrapper left exactly as it arrived", got)
	}

	up, _, err := dtrStep2122Up(down, corr)
	if err != nil {
		t.Fatalf("up: unexpected error: %v", err)
	}
	answers := dtrAnswersOf(t, up)
	if got := dtrExtURLsAt(answers[0]); len(got) != 1 || got[0] != shnsdk.CarriedContentExtURL {
		t.Fatalf("answer.extension after restore = %v, want the wrapper left in place", got)
	}
	if _, present := answers[0]["_valueInteger"]; present {
		t.Fatal("restore relocated a legacy wrapper's content into _valueInteger — strict restore must not relocate")
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

// dtrNestedQRGroupAxis builds a QR whose answer-bearing item sits under a group
// item — QuestionnaireResponse.item.item, the shape a captured Da Vinci RI
// questionnaire package uses (a group item wrapping its children is the ordinary
// DTR authoring idiom).
func dtrNestedQRGroupAxis(answersJSON string) []byte {
	return []byte(`{
		"resourceType":"QuestionnaireResponse","id":"qr-nested-group","status":"completed",
		"questionnaire":"http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri|1.0.0",
		"subject":{"reference":"Patient/MBR-COVERED"},
		"authored":"2026-06-04T00:00:00Z",
		"extension":[
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage","valueReference":{"reference":"Coverage/MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/sr-MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes","code":"withpa","display":"Information needed for a prior authorization"}]}}
		],
		"item":[{"linkId":"grp-1","item":[{"linkId":"conservative-therapy-weeks","answer":[` + answersJSON + `]}]}]
	}`)
}

// dtrNestedQRAnswerAxis builds a QR whose answer-bearing item sits under ANOTHER
// answer — QuestionnaireResponse.item.answer.item, the second recursion axis.
// Both axes contentReference back to QuestionnaireResponse.item; a walker that
// handles only the group axis leaves this one silently unvisited.
func dtrNestedQRAnswerAxis(answersJSON string) []byte {
	return []byte(`{
		"resourceType":"QuestionnaireResponse","id":"qr-nested-answer","status":"completed",
		"questionnaire":"http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri|1.0.0",
		"subject":{"reference":"Patient/MBR-COVERED"},
		"authored":"2026-06-04T00:00:00Z",
		"extension":[
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage","valueReference":{"reference":"Coverage/MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/sr-MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes","code":"withpa","display":"Information needed for a prior authorization"}]}}
		],
		"item":[{"linkId":"parent-q","answer":[{"valueBoolean":true,"item":[{"linkId":"conservative-therapy-weeks","answer":[` + answersJSON + `]}]}]}]
	}`)
}

// dtrNestedAnswersOf returns EVERY answer in the document, at every depth, on
// both axes — the test-side counterpart of the walker under test, written
// independently so a bug in the walker cannot hide behind a shared helper.
func dtrNestedAnswersOf(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var out []map[string]any
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		items, _ := node["item"].([]any)
		for _, it := range items {
			im, ok := it.(map[string]any)
			if !ok {
				continue
			}
			answers, _ := im["answer"].([]any)
			for _, a := range answers {
				am, ok := a.(map[string]any)
				if !ok {
					continue
				}
				out = append(out, am)
				walk(am)
			}
			walk(im)
		}
	}
	walk(doc)
	return out
}

// TestDTRStep2122_ItemWeightCarryRoundTrip_NestedGroupAxis is R1: a weight on an
// answer below a group item must carry and restore to ITS OWN answer. Before the
// walkers recursed, this element was neither carried nor reported — it crossed
// into the 2.1 payload bare, where a profile-filtering peer drops it with no
// conformance violation.
func TestDTRStep2122_ItemWeightCarryRoundTrip_NestedGroupAxis(t *testing.T) {
	in := dtrNestedQRGroupAxis(`{"valueInteger":6,"_valueInteger":{"extension":[` + dtrTestItemWeight + `]}}`)
	dtrAssertNestedCarryRoundTrip(t, in)
}

// TestDTRStep2122_ItemWeightCarryRoundTrip_NestedAnswerAxis is R2: the same, on
// the item.answer.item axis.
func TestDTRStep2122_ItemWeightCarryRoundTrip_NestedAnswerAxis(t *testing.T) {
	in := dtrNestedQRAnswerAxis(`{"valueInteger":6,"_valueInteger":{"extension":[` + dtrTestItemWeight + `]}}`)
	dtrAssertNestedCarryRoundTrip(t, in)
}

// TestDTRStep2122_ItemWeightCarryRoundTrip_Depth3 is R5.
func TestDTRStep2122_ItemWeightCarryRoundTrip_Depth3(t *testing.T) {
	in := []byte(`{
		"resourceType":"QuestionnaireResponse","id":"qr-deep","status":"completed",
		"questionnaire":"http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri|1.0.0",
		"subject":{"reference":"Patient/MBR-COVERED"},
		"authored":"2026-06-04T00:00:00Z",
		"extension":[
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage","valueReference":{"reference":"Coverage/MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/sr-MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes","code":"withpa","display":"Information needed for a prior authorization"}]}}
		],
		"item":[{"linkId":"g0","item":[{"linkId":"g1","item":[{"linkId":"conservative-therapy-weeks",
			"answer":[{"valueInteger":6,"_valueInteger":{"extension":[` + dtrTestItemWeight + `]}}]}]}]}]
	}`)
	dtrAssertNestedCarryRoundTrip(t, in)
}

// dtrAssertNestedCarryRoundTrip: exactly one carry, the wrapper replaces the
// element IN PLACE inside _valueInteger (never relocated onto the answer), and
// the restore returns the original bytes to the same container.
func dtrAssertNestedCarryRoundTrip(t *testing.T, in []byte) {
	t.Helper()

	down, downReport, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: unexpected error: %v", err)
	}
	if len(downReport.Carried) != 1 {
		t.Fatalf("want exactly 1 Carried entry, got %+v", downReport.Carried)
	}
	if downReport.Carried[0].Path != dtrItemWeightLocus {
		t.Fatalf("Carried[0].Path = %q, want %q", downReport.Carried[0].Path, dtrItemWeightLocus)
	}

	downAnswers := dtrNestedAnswersOf(t, down)
	var weighted map[string]any
	for _, a := range downAnswers {
		if _, ok := a["_valueInteger"]; ok {
			weighted = a
		}
	}
	if weighted == nil {
		t.Fatalf("no answer with _valueInteger found after carry: %s", down)
	}
	if got := dtrExtURLsAt(dtrSubMap(t, weighted, "_valueInteger")); len(got) != 1 ||
		got[0] != shnsdk.CarriedContentExtURL {
		t.Fatalf("_valueInteger.extension after carry = %v, want exactly the shn-carried-content wrapper", got)
	}
	if got := dtrExtURLsAt(weighted); len(got) != 0 {
		t.Fatalf("answer.extension after carry = %v, want empty — the carry must not relocate the wrapper", got)
	}

	up, upReport, err := dtrStep2122Up(down, corr)
	if err != nil {
		t.Fatalf("up: unexpected error: %v", err)
	}
	if len(upReport.Carried) != 0 || len(upReport.Synthesized) != 0 {
		t.Fatalf("restore must not itself be reported as Carried/Synthesized, got %+v", upReport)
	}

	upAnswers := dtrNestedAnswersOf(t, up)
	restored := 0
	for _, a := range upAnswers {
		sub, ok := a["_valueInteger"].(map[string]any)
		if !ok {
			continue
		}
		exts, _ := sub["extension"].([]any)
		for _, e := range exts {
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatalf("marshal restored extension: %v", err)
			}
			var probe struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil {
				t.Fatalf("decode restored extension: %v", err)
			}
			if probe.URL != "http://hl7.org/fhir/StructureDefinition/itemWeight" {
				t.Fatalf("unexpected extension after restore: %s", raw)
			}
			restored++
		}
	}
	if restored != 1 {
		t.Fatalf("want exactly 1 restored itemWeight, got %d: %s", restored, up)
	}
}

// TestDTRStep2122_OriginCodeRemapNested is R3: the information-origin source
// code on a NESTED item's answer remaps in both directions. Before recursion a
// 2.2-line code rode into a 2.1 payload that does not define it.
func TestDTRStep2122_OriginCodeRemapNested(t *testing.T) {
	const origin = `{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/information-origin","extension":[{"url":"source","valueCode":"auto-client"}]}`
	in := dtrNestedQRGroupAxis(`{"valueInteger":6,"extension":[` + origin + `]}`)

	down, _, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := dtrNestedOriginCode(t, down); got != "auto" {
		t.Fatalf("nested origin code after downcast = %q, want %q", got, "auto")
	}

	up, _, err := dtrStep2122Up(down, corr)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := dtrNestedOriginCode(t, up); got != "auto-client" {
		t.Fatalf("nested origin code after upcast = %q, want %q", got, "auto-client")
	}
}

// dtrNestedOriginCode returns the single information-origin source code found
// anywhere in the document, and fatals unless there is exactly one.
func dtrNestedOriginCode(t *testing.T, payload []byte) string {
	t.Helper()
	var found []string
	for _, a := range dtrNestedAnswersOf(t, payload) {
		exts, _ := a["extension"].([]any)
		for _, e := range exts {
			em, _ := e.(map[string]any)
			if em["url"] != "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/information-origin" {
				continue
			}
			subs, _ := em["extension"].([]any)
			for _, s := range subs {
				sm, _ := s.(map[string]any)
				if sm["url"] == "source" {
					code, _ := sm["valueCode"].(string)
					found = append(found, code)
				}
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 information-origin source code, got %v in %s", found, payload)
	}
	return found[0]
}

// TestDTRStep2122_ItemWeightMixedFlatAndNested is R4: a QR carrying weights both
// at the top level and below a group must carry BOTH, and each must restore to
// its own answer rather than to whichever the walker reached first.
func TestDTRStep2122_ItemWeightMixedFlatAndNested(t *testing.T) {
	in := []byte(`{
		"resourceType":"QuestionnaireResponse","id":"qr-mixed","status":"completed",
		"questionnaire":"http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri|1.0.0",
		"subject":{"reference":"Patient/MBR-COVERED"},
		"authored":"2026-06-04T00:00:00Z",
		"extension":[
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage","valueReference":{"reference":"Coverage/MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/sr-MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes","code":"withpa","display":"Information needed for a prior authorization"}]}}
		],
		"item":[
			{"linkId":"flat-q","answer":[{"valueInteger":3,"_valueInteger":{"extension":[{"url":"http://hl7.org/fhir/StructureDefinition/itemWeight","valueDecimal":0.25}]}}]},
			{"linkId":"grp-1","item":[{"linkId":"nested-q","answer":[{"valueInteger":6,"_valueInteger":{"extension":[{"url":"http://hl7.org/fhir/StructureDefinition/itemWeight","valueDecimal":0.75}]}}]}]}
		]
	}`)

	down, downReport, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(downReport.Carried) != 2 {
		t.Fatalf("want 2 Carried entries (flat + nested), got %d: %+v", len(downReport.Carried), downReport.Carried)
	}

	up, _, err := dtrStep2122Up(down, corr)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	// Each weight must come back on the answer whose valueInteger it started on.
	want := map[float64]float64{3: 0.25, 6: 0.75}
	got := dtrWeightsByAnswerValue(t, up)
	if len(got) != len(want) {
		t.Fatalf("restored weights = %v, want %v", got, want)
	}
	for v, w := range want {
		if got[v] != w {
			t.Fatalf("answer valueInteger %v carries weight %v, want %v (full map %v)", v, got[v], w, got)
		}
	}
}

// dtrWeightsByAnswerValue maps each answer's valueInteger to the itemWeight
// found under its _valueInteger. This is what catches a walker that recurses but
// FLATTENS — collecting answers into a list and restoring positionally passes
// every single-weight test and fails only here.
func dtrWeightsByAnswerValue(t *testing.T, payload []byte) map[float64]float64 {
	t.Helper()
	out := map[float64]float64{}
	for _, a := range dtrNestedAnswersOf(t, payload) {
		vi, ok := a["valueInteger"].(float64)
		if !ok {
			continue
		}
		sub, ok := a["_valueInteger"].(map[string]any)
		if !ok {
			continue
		}
		exts, _ := sub["extension"].([]any)
		for _, e := range exts {
			em, _ := e.(map[string]any)
			if em["url"] != "http://hl7.org/fhir/StructureDefinition/itemWeight" {
				continue
			}
			w, _ := em["valueDecimal"].(float64)
			out[vi] = w
		}
	}
	return out
}

// TestDTRStep2122_ItemWeightTwoNestedSiblings is R6: two weighted answers under
// ONE parent group. Same discrimination as R4, with both weights nested, so a
// positional restore cannot be masked by the flat one landing correctly.
func TestDTRStep2122_ItemWeightTwoNestedSiblings(t *testing.T) {
	in := []byte(`{
		"resourceType":"QuestionnaireResponse","id":"qr-siblings","status":"completed",
		"questionnaire":"http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri|1.0.0",
		"subject":{"reference":"Patient/MBR-COVERED"},
		"authored":"2026-06-04T00:00:00Z",
		"extension":[
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage","valueReference":{"reference":"Coverage/MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/sr-MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes","code":"withpa","display":"Information needed for a prior authorization"}]}}
		],
		"item":[{"linkId":"grp-1","item":[
			{"linkId":"a","answer":[{"valueInteger":1,"_valueInteger":{"extension":[{"url":"http://hl7.org/fhir/StructureDefinition/itemWeight","valueDecimal":0.1}]}}]},
			{"linkId":"b","answer":[{"valueInteger":2,"_valueInteger":{"extension":[{"url":"http://hl7.org/fhir/StructureDefinition/itemWeight","valueDecimal":0.2}]}}]}
		]}]
	}`)

	down, downReport, err := dtrStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(downReport.Carried) != 2 {
		t.Fatalf("want 2 Carried entries, got %d", len(downReport.Carried))
	}
	up, _, err := dtrStep2122Up(down, corr)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	got := dtrWeightsByAnswerValue(t, up)
	if got[1] != 0.1 || got[2] != 0.2 {
		t.Fatalf("weights crossed over on restore: %v, want map[1:0.1 2:0.2]", got)
	}
}

// TestDTRStep2122_AnswerlessNestedItemIsNoOp is R8: an item with nested children
// but no answer of its own — the shape a real captured questionnaire package
// uses — must walk cleanly and carry nothing.
func TestDTRStep2122_AnswerlessNestedItemIsNoOp(t *testing.T) {
	in := dtrQRWithAnswers(`{"valueInteger":6}`)
	var doc map[string]any
	if err := json.Unmarshal(in, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	doc["item"] = []any{map[string]any{
		"linkId": "grp-1",
		"item":   []any{map[string]any{"linkId": "display-only"}},
	}}
	nested, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	down, downReport, err := dtrStep2122Down(nested, corr)
	if err != nil {
		t.Fatalf("down on an answerless nested item: %v", err)
	}
	if len(downReport.Carried) != 0 {
		t.Fatalf("answerless nested item carried something: %+v", downReport.Carried)
	}
	if _, _, err := dtrStep2122Up(down, corr); err != nil {
		t.Fatalf("up on an answerless nested item: %v", err)
	}
}

// TestDTRStep2122_CapturedRIPackageTraverses walks a REAL captured Da Vinci RI
// questionnaire package — a Parameters document holding a Questionnaire and a
// QuestionnaireResponse nested two levels deep through a group item. It was in
// this repo, read by no test, for its whole life: captured evidence with no
// guard behind it.
//
// TRAVERSAL ONLY, and deliberately so. Every nested item in that capture carries
// a linkId and nothing else — zero answers at every node — so the carry could be
// completely broken and this test would still pass. Because the capture is
// answerless at every node and dtrWalkAnswers' recursive call is unconditional,
// deleting the recursion entirely would produce byte-identical output here — so
// this test does not show that the walkers DESCEND the nested shape, only that
// they do not ERROR on it. It proves nothing else. The carry claim belongs to
// the registry's nested entries, which inject an answer-bearing item; this must
// never stand in for them.
func TestDTRStep2122_CapturedRIPackageTraverses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "br-payer", "questionnaire-package.json"))
	if err != nil {
		t.Fatalf("read captured RI package: %v", err)
	}

	// The capture is a Parameters wrapper (br-payer's $questionnaire-package
	// response shape, profile dtr-qpackage-output-parameters) — dtrCollectResources
	// only unwraps a bare resource or a top-level Bundle, not Parameters, so
	// feeding raw straight to the step transforms would leave them looking at a
	// single opaque "Parameters" resource and never reach the QuestionnaireResponse
	// at all. Unwrap it the same way the real pipeline does before any step
	// transform ever sees it (davincimap.go's unwrapQuestionnairePackage).
	bundle := unwrapQuestionnairePackage(raw)

	// Precondition: locate the QuestionnaireResponse the transform actually
	// operates on (the same view dtrCollectResources gives dtrStep2122Down/Up
	// internally) and measure ITS depth alone. Measuring the whole document
	// would also see the sibling Questionnaire resource, which nests
	// independently — a future edit that flattened only the QuestionnaireResponse
	// would then leave this precondition silently green with nothing red.
	var top map[string]any
	if err := json.Unmarshal(bundle, &top); err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	qrs := dtrCollectResources(top)["QuestionnaireResponse"]
	if len(qrs) != 1 {
		t.Fatalf("captured RI package: expected exactly 1 QuestionnaireResponse, found %d", len(qrs))
	}
	if depth := dtrMaxItemDepth(qrs[0]); depth < 2 {
		t.Fatalf("captured RI package's QuestionnaireResponse nests only %d level(s) — this guard no longer guards anything", depth)
	}

	down, report, err := dtrStep2122Down(bundle, corr)
	if err != nil {
		t.Fatalf("downcast of the captured RI package: %v", err)
	}
	if len(report.Carried) != 0 {
		t.Fatalf("answerless nested items carried something: %+v", report.Carried)
	}
	if _, _, err := dtrStep2122Up(down, corr); err != nil {
		t.Fatalf("upcast of the captured RI package: %v", err)
	}
}

// dtrMaxItemDepth returns the deepest QR/Questionnaire item nesting in doc,
// counting both FHIR axes. 1 means flat.
func dtrMaxItemDepth(node any) int {
	switch v := node.(type) {
	case map[string]any:
		best := 0
		for k, child := range v {
			d := dtrMaxItemDepth(child)
			if k == "item" {
				d++
			}
			if d > best {
				best = d
			}
		}
		return best
	case []any:
		best := 0
		for _, child := range v {
			if d := dtrMaxItemDepth(child); d > best {
				best = d
			}
		}
		return best
	default:
		return 0
	}
}
