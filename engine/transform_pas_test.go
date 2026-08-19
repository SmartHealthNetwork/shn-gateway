package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// ---- fixture helpers -------------------------------------------------------

// pasGolden reads a real per-line golden fixture — never a hand-rolled stub,
// so every cross-version transform assertion below is made against the same
// corpus a live HAPI validates.
//
// The fixtures are vendored copies under this package's own testdata/golden/
// (see that directory's README.md), NOT a reach into the surrounding monorepo:
// the gateway module is published standalone, so a reach above the module root
// would make these tests unrunnable — and therefore untrue — in the published
// module. The copies are pinned byte-for-byte against their originals by the
// root module's test/conformance/gateway_vendored_golden_drift_test.go.
func pasGolden(t *testing.T, relPath string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden", relPath))
	if err != nil {
		t.Fatalf("read vendored golden %s: %v", relPath, err)
	}
	return raw
}

func decodeAny(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode JSON: %v (raw=%s)", err, raw)
	}
	return v
}

func assertJSONEqual(t *testing.T, got, want []byte, msg string) {
	t.Helper()
	if !reflect.DeepEqual(decodeAny(t, got), decodeAny(t, want)) {
		t.Fatalf("%s: JSON mismatch\ngot:  %s\nwant: %s", msg, got, want)
	}
}

var corr = ExchangeIdentity{CorrelationID: "golden-corr"}

// ---- pa.pas 2.0<->2.1 -------------------------------------------------------

// TestPasStep2021Up_ResponseSynthesizesRequest: the 2.0 approved golden
// (bare ClaimResponse, no request field) transformed Up to 2.1 must match
// the 2.1 approved golden EXACTLY (which differs from 2.0's only by the
// added request field) — and the LossReport must name the synthesis.
func TestPasStep2021Up_ResponseSynthesizesRequest(t *testing.T) {
	in := pasGolden(t, "claimresponse-approved.json")
	want := pasGolden(t, "2.1/claimresponse-approved.json")

	out, report, err := pasStep2021Up(in, corr)
	if err != nil {
		t.Fatalf("pasStep2021Up: unexpected error: %v", err)
	}
	assertJSONEqual(t, out, want, "2.0->2.1 approved response")

	if len(report.Synthesized) != 1 {
		t.Fatalf("want exactly 1 Synthesized entry, got %d: %+v", len(report.Synthesized), report.Synthesized)
	}
	if report.Synthesized[0].Path != "ClaimResponse.request" {
		t.Fatalf("Synthesized[0].Path = %q, want %q", report.Synthesized[0].Path, "ClaimResponse.request")
	}
	if len(report.Carried) != 0 {
		t.Fatalf("full-class step must carry nothing, got %+v", report.Carried)
	}
	if report.Module != "pa.pas 2.0->2.1" || report.Source != "2.0" || report.Target != "2.1" {
		t.Fatalf("unexpected report trace: %+v", report)
	}
}

// TestPasStep2021Up_ResponseHonorsExistingRequest: a foreign 2.0 payload that
// (unusually) already carries a ClaimResponse.request must not have it
// clobbered — synthesis only fires when the field is genuinely absent.
func TestPasStep2021Up_ResponseHonorsExistingRequest(t *testing.T) {
	in := []byte(`{"resourceType":"ClaimResponse","identifier":[{"system":"urn:shn:correlation","value":"golden-corr"}],"request":{"identifier":{"system":"urn:shn:correlation","value":"pre-existing"}}}`)
	out, report, err := pasStep2021Up(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(report.Synthesized) != 0 {
		t.Fatalf("must not synthesize over an existing value, got %+v", report.Synthesized)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req := got["request"].(map[string]any)["identifier"].(map[string]any)["value"]
	if req != "pre-existing" {
		t.Fatalf("existing request field was clobbered: got %v", req)
	}
}

// TestPasStep2021Up_RequestDirectionGated pins the gated refusal: a
// request-direction (Claim-bearing) payload Up 2.0->2.1 has no honest source
// for the 2.1-mandatory item/related elements and must refuse with the typed
// SemanticChangeError naming them — never fabricate, never silently pass a
// non-conformant payload through.
func TestPasStep2021Up_RequestDirectionGated(t *testing.T) {
	in := pasGolden(t, "conformant/pas-submit-request.json") // a request Bundle containing a Claim entry
	out, report, err := pasStep2021Up(in, corr)
	if err == nil {
		t.Fatalf("want a semantic-change refusal, got success (out=%q report=%+v)", out, report)
	}
	var scErr *SemanticChangeError
	if !errors.As(err, &scErr) {
		t.Fatalf("want *SemanticChangeError (errors.As), got %T: %v", err, err)
	}
	if scErr.Contract != "pa.pas" || scErr.From != "2.0" || scErr.To != "2.1" || scErr.Direction != "up" {
		t.Fatalf("unexpected error fields: %+v", scErr)
	}
	wantMissing := []string{
		"Claim.item.extension:certificationType",
		"Claim.item.extension:requestType",
		"Claim.item.location[x]",
		"Claim.related.relationship",
	}
	if !reflect.DeepEqual(scErr.MissingElements, wantMissing) {
		t.Fatalf("MissingElements = %v, want %v", scErr.MissingElements, wantMissing)
	}
	if out != nil {
		t.Fatalf("a refused step must return nil output, got %q", out)
	}
}

// TestPasStep2021Down_PassThrough: the 2.1 approved golden downcast to 2.0
// must pass through BYTE-IDENTICAL (verified live: 2.0's profile tolerates
// every element 2.1 requires) — no Carried/Synthesized entries.
func TestPasStep2021Down_PassThrough(t *testing.T) {
	in := pasGolden(t, "2.1/claimresponse-approved.json")
	out, report, err := pasStep2021Down(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("down-cast must pass through byte-identical:\ngot:  %s\nwant: %s", out, in)
	}
	if len(report.Carried) != 0 || len(report.Synthesized) != 0 {
		t.Fatalf("drop-nothing step must carry/synthesize nothing, got %+v", report)
	}
}

// TestPasStep2021Down_PassesRequestDirectionThroughToo: the 2.1 submit
// request golden (with certificationType/requestType/location extensions
// present, min=1 at 2.1) validates unchanged at 2.0 too, per the same
// superset-tolerance finding — proving the request direction isn't silently
// stripped either.
func TestPasStep2021Down_PassesRequestDirectionThroughToo(t *testing.T) {
	in := pasGolden(t, "2.1/conformant/pas-submit-request.json")
	out, _, err := pasStep2021Down(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("down-cast request bundle must pass through byte-identical")
	}
}

// ---- pa.pas 2.1<->2.2 -------------------------------------------------------

// TestPasStep2122Up_PendedSynthesizesIdentifierAndRemapsOutcome is Step 2's
// explicit worked example: the 2.1 pended golden transformed Up to 2.2 must
// match the 2.2 pended golden exactly (Bundle.identifier added, outcome
// queued->complete), with the synthesis named in the LossReport.
func TestPasStep2122Up_PendedSynthesizesIdentifierAndRemapsOutcome(t *testing.T) {
	in := pasGolden(t, "2.1/claimresponse-pended.json")
	want := pasGolden(t, "2.2/claimresponse-pended.json")

	out, report, err := pasStep2122Up(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONEqual(t, out, want, "2.1->2.2 pended response")

	if len(report.Synthesized) != 1 || report.Synthesized[0].Path != "Bundle.identifier" {
		t.Fatalf("want exactly 1 Synthesized entry for Bundle.identifier, got %+v", report.Synthesized)
	}
	if len(report.Carried) != 0 {
		t.Fatalf("full-class step must carry nothing, got %+v", report.Carried)
	}
}

// TestPasStep2122Up_RequestDirectionPassesThrough: PAS 2.1.0 and 2.2.1 are
// byte-identical on every CLAIM-scoped element this adjacency could touch
// (verified live: the Claim/Patient/Coverage/ServiceRequest entries of the
// 2.1 and 2.2 conformant submit-request goldens are byte-identical — only
// the embedded QuestionnaireResponse differs, and that is DTR-owned content,
// out of pa.pas's scope; see pasStep2122Up's doc comment). So the request
// direction is a genuine no-op pass-through: this step must not touch the
// bundle at all, not even reorder it.
func TestPasStep2122Up_RequestDirectionPassesThrough(t *testing.T) {
	in := pasGolden(t, "2.1/conformant/pas-submit-request.json")
	out, report, err := pasStep2122Up(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONEqual(t, out, in, "2.1->2.2 request bundle (pass-through)")
	if len(report.Synthesized) != 0 || len(report.Carried) != 0 {
		t.Fatalf("request-direction up-cast must lose/synthesize nothing here, got %+v", report)
	}
}

// TestPasStep2122Up_DoesNotTouchEmbeddedDTRContent guards the scope boundary
// directly: the embedded QuestionnaireResponse (DTR-owned; DTRDef's
// SingleCoverageConstraint/IntendedUseCodeSystem/AutoOriginSourceCode deltas
// are pa.dtr's compat row — verified live 2026-08-12 that the 2.1 and
// 2.2 goldens' QR entries genuinely differ) must survive this PAS step
// byte-identical, proving pa.pas never reaches into DTR-scoped content.
func TestPasStep2122Up_DoesNotTouchEmbeddedDTRContent(t *testing.T) {
	in := pasGolden(t, "2.1/conformant/pas-submit-request.json")
	out, _, err := pasStep2122Up(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var inDoc, outDoc map[string]any
	if err := json.Unmarshal(in, &inDoc); err != nil {
		t.Fatalf("decode in: %v", err)
	}
	if err := json.Unmarshal(out, &outDoc); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	inQR := findEntryByResourceType(t, inDoc, "QuestionnaireResponse")
	outQR := findEntryByResourceType(t, outDoc, "QuestionnaireResponse")
	if !reflect.DeepEqual(inQR, outQR) {
		t.Fatalf("pa.pas step must not touch the embedded DTR QuestionnaireResponse:\nin:  %+v\nout: %+v", inQR, outQR)
	}
}

// TestPasStep2122Down_DoesNotTouchEmbeddedDTRContent is
// TestPasStep2122Up_DoesNotTouchEmbeddedDTRContent's mirror for the Down
// direction: the same scope boundary must hold both ways, not just Up.
func TestPasStep2122Down_DoesNotTouchEmbeddedDTRContent(t *testing.T) {
	in := pasGolden(t, "2.2/conformant/pas-submit-request.json")
	out, _, err := pasStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var inDoc, outDoc map[string]any
	if err := json.Unmarshal(in, &inDoc); err != nil {
		t.Fatalf("decode in: %v", err)
	}
	if err := json.Unmarshal(out, &outDoc); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	inQR := findEntryByResourceType(t, inDoc, "QuestionnaireResponse")
	outQR := findEntryByResourceType(t, outDoc, "QuestionnaireResponse")
	if !reflect.DeepEqual(inQR, outQR) {
		t.Fatalf("pa.pas step must not touch the embedded DTR QuestionnaireResponse:\nin:  %+v\nout: %+v", inQR, outQR)
	}
}

func findEntryByResourceType(t *testing.T, bundle map[string]any, rt string) map[string]any {
	t.Helper()
	entries, _ := bundle["entry"].([]any)
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		rm, ok := em["resource"].(map[string]any)
		if !ok {
			continue
		}
		if rm["resourceType"] == rt {
			return rm
		}
	}
	t.Fatalf("no %s entry found in bundle", rt)
	return nil
}

// TestPasStep2122Down_PendedOutcomeRemap: the 2.2 pended golden downcast to
// 2.1 must have outcome rewritten complete->queued (def-driven from
// PASLineDef("2.1").PendedResponseOutcome) while Bundle.identifier is left
// in place (tolerated, not stripped — 2.1's profile has no constraint on it).
func TestPasStep2122Down_PendedOutcomeRemap(t *testing.T) {
	in := pasGolden(t, "2.2/claimresponse-pended.json")
	out, report, err := pasStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	entries := got["entry"].([]any)
	cr := entries[0].(map[string]any)["resource"].(map[string]any)
	if cr["outcome"] != "queued" {
		t.Fatalf("outcome = %v, want %q", cr["outcome"], "queued")
	}
	if _, present := got["identifier"]; !present {
		t.Fatalf("Bundle.identifier must be tolerated (left in place), not stripped")
	}
	if len(report.Carried) != 0 {
		t.Fatalf("no 2.2-only extensions in this fixture, want no Carried entries, got %+v", report.Carried)
	}
}

// TestPasStep2122Down_ApprovedOutcomeUntouched: a non-pended (no Task)
// "complete" outcome must NOT be remapped to "queued" — only the Task-
// discriminated pended shape triggers the rebinding.
func TestPasStep2122Down_ApprovedOutcomeUntouched(t *testing.T) {
	in := pasGolden(t, "2.2/claimresponse-approved.json")
	out, _, err := pasStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["outcome"] != "complete" {
		t.Fatalf("a non-pended outcome must be left alone, got %v", got["outcome"])
	}
}

// TestPasStep2122_CarryRoundTrip pins the loss-policy's carry mechanism end
// to end: a 2.2-only top-level Claim.extension downcast to 2.1 is carried
// into shn-carried-content (removed from the live extension array, LossReport
// names it), and a subsequent Up back to 2.2 restores it BYTE-IDENTICAL to
// the original (sdk/carry.go's Restore(Carry(x))==x contract).
//
// The worked example used to be authorizationNumber. That element was deleted
// from pas22OnlyClaimExtensions: PAS 2.2.1 declares the top-level
// slice but the extension's own SD context[] forbids the host, so it cannot
// appear on a conformant wire, and at its only legal (item) locus it is
// line-invariant across 2.1.0/2.2.1 — nothing to carry. The MECHANISM this
// test pins is unaffected, so the test is repointed at a surviving carried
// element rather than deleted; its shape is the live-verified complex one.
func TestPasStep2122_CarryRoundTrip(t *testing.T) {
	tidExt := `{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers","extension":[{"url":"applicationSenderCode","valueString":"SND-0001"},{"url":"applicationReceiverCode","valueString":"RCV-0001"}]}`
	in := []byte(`{"resourceType":"Claim","id":"c1","extension":[` + tidExt + `]}`)

	down, downReport, err := pasStep2122Down(in, corr)
	if err != nil {
		t.Fatalf("down: unexpected error: %v", err)
	}
	if len(downReport.Carried) != 1 {
		t.Fatalf("want exactly 1 Carried entry, got %+v", downReport.Carried)
	}
	if downReport.Carried[0].Path != "Claim.extension:transmissionIdentifiers" {
		t.Fatalf("Carried[0].Path = %q, want %q", downReport.Carried[0].Path, "Claim.extension:transmissionIdentifiers")
	}

	var downDoc map[string]any
	if err := json.Unmarshal(down, &downDoc); err != nil {
		t.Fatalf("decode down output: %v", err)
	}
	exts := downDoc["extension"].([]any)
	if len(exts) != 1 {
		t.Fatalf("want exactly 1 extension after carry (the wrapper), got %d: %+v", len(exts), exts)
	}
	wrapperURL := exts[0].(map[string]any)["url"]
	if wrapperURL != "http://smarthealth.network/fhir/StructureDefinition/shn-carried-content" {
		t.Fatalf("extension[0] is not the shn-carried-content wrapper: %v", wrapperURL)
	}

	up, upReport, err := pasStep2122Up(down, corr)
	if err != nil {
		t.Fatalf("up: unexpected error: %v", err)
	}
	if len(upReport.Carried) != 0 && len(upReport.Synthesized) != 0 {
		// Restoring is neither a loss nor a synthesis by this step.
		t.Fatalf("restore must not itself be reported as Carried/Synthesized, got %+v", upReport)
	}
	// Decode "extension" as []json.RawMessage — NOT map[string]any — so what is
	// compared below is the ACTUAL bytes encoding/json found in the up-cast
	// output's own JSON text, not a value the TEST decoded and re-marshaled a
	// second time.
	//
	// What the restore actually guarantees is VALUE identity, including numeric
	// literals — NOT byte identity. The step decodes the resource into
	// map[string]any before handing the element to sdk.CarryElement, so
	// encoding/json re-serializes object keys in alphabetical order on the way
	// out. sdk/carry.go itself is byte-faithful (json.RawMessage end to end);
	// the normalization is the ENGINE's, one layer up.
	//
	// This test previously asserted bytes.Equal and passed — but only because
	// its worked example was {"url":…,"valueString":…}, which is already in
	// alphabetical order. Repointing it at a complex extension
	// ({"url":…,"extension":[…]}) surfaced that the byte-faithful claim was
	// true by coincidence, never by mechanism. That is the same shape as the
	// S1 defect where a "byte-faithful" claim passed green through a comparison
	// that normalized both sides; test/xmatrix/effects_test.go has documented
	// the real bar as value-identical-not-byte-identical all along.
	//
	// Strengthening the engine to carry raw bytes is deliberately out of scope
	// here. If that ever lands, this assertion should go back to bytes.Equal.
	var upDoc struct {
		Extension []json.RawMessage `json:"extension"`
	}
	if err := json.Unmarshal(up, &upDoc); err != nil {
		t.Fatalf("decode up output: %v", err)
	}
	if len(upDoc.Extension) != 1 {
		t.Fatalf("want exactly 1 extension after restore, got %d: %s", len(upDoc.Extension), up)
	}
	// UseNumber on BOTH sides: a float64 compare would normalize the literals
	// away and make this weaker than it reads.
	if !reflect.DeepEqual(decodeExactAny(t, upDoc.Extension[0]), decodeExactAny(t, []byte(tidExt))) {
		t.Fatalf("carry round-trip did not restore value-identically:\n got  %s\n want %s", upDoc.Extension[0], tidExt)
	}
	// The bytes are NOT expected to match, and this pins WHY: only key order
	// differs. If this ever starts failing because the bytes now match, the
	// engine became byte-faithful — restore the bytes.Equal assertion above.
	if bytes.Equal(upDoc.Extension[0], []byte(tidExt)) {
		t.Logf("carry is now byte-faithful for this shape — consider restoring the stronger bytes.Equal assertion")
	}
}

// decodeExactAny decodes JSON preserving numeric literals as json.Number, so a
// value comparison cannot silently normalize 0.50 to 0.5.
func decodeExactAny(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode JSON: %v (raw=%s)", err, raw)
	}
	return v
}

// ---- chain composition + determinism ---------------------------------------

// TestPASChain_2020To22ThroughBothSteps proves the two adjacent steps compose
// through chainFor/applyChain (via the exported test seam) exactly as a real
// 2.0->2.2 leg would: a 2.0 pended response walks Up through BOTH adjacent
// steps and lands on the 2.2 pended golden's shape.
func TestPASChain_2020To22ThroughBothSteps(t *testing.T) {
	in := pasGolden(t, "claimresponse-pended.json")
	want := pasGolden(t, "2.2/claimresponse-pended.json")

	out, reports, err := TransformPASForTest("2.0", "2.2", in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONEqual(t, out, want, "2.0->2.2 chained pended response")
	if len(reports) != 2 {
		t.Fatalf("want 2 step reports (2.0->2.1, 2.1->2.2), got %d: %+v", len(reports), reports)
	}
}

// TestPasStep2021Up_Determinism / TestPasStep2122Down_Determinism pin the
// framework-wide determinism invariant at the step level: same
// input run twice produces byte-identical output and structurally-equal
// reports.
func TestPasStep2021Up_Determinism(t *testing.T) {
	in := pasGolden(t, "claimresponse-approved.json")
	out1, r1, err1 := pasStep2021Up(in, corr)
	out2, r2, err2 := pasStep2021Up(in, corr)
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

func TestPasStep2122Down_Determinism(t *testing.T) {
	in := pasGolden(t, "2.2/claimresponse-pended.json")
	out1, r1, err1 := pasStep2122Down(in, corr)
	out2, r2, err2 := pasStep2122Down(in, corr)
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
