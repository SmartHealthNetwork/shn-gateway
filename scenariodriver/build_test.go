package scenariodriver

import (
	"bytes"
	"encoding/json"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestGoldens_Embedded(t *testing.T) {
	for name, g := range map[string][]byte{"approve": PASApproveGolden(), "pend": PASPendGolden()} {
		var b map[string]any
		if err := json.Unmarshal(g, &b); err != nil {
			t.Fatalf("%s golden is not JSON: %v", name, err)
		}
		if b["resourceType"] != "Bundle" {
			t.Fatalf("%s golden is not a Bundle", name)
		}
	}
	// Accessors must return copies — a caller mutating the slice must not poison the embed.
	g := PASApproveGolden()
	g[0] = 'X'
	if PASApproveGolden()[0] == 'X' {
		t.Fatal("PASApproveGolden returned the embedded slice, not a copy")
	}
}

func TestBuildCRDRequest_Shape(t *testing.T) {
	body, err := BuildCRDRequest("MBR-COVERED", SystemHCPCS, "L8000", "Breast prosthesis, mastectomy bra")
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req["hook"] != "order-sign" {
		t.Fatalf("hook = %v, want order-sign", req["hook"])
	}
	// The ServiceRequest carrier + subject binding (the payer-side bind extracts it).
	for _, want := range []string{`"Patient/MBR-COVERED"`, `"L8000"`, `"ServiceRequest"`, SystemHCPCS,
		`"urn:oid:2.16.840.1.113883.6.300"`, `"00001"`, `"Breast prosthesis, mastectomy bra"`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("CRD request missing %s: %s", want, body)
		}
	}
	// display omitted when empty
	noDisp, err := BuildCRDRequest("MBR-COVERED", SystemHCPCS, "E0250", "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(noDisp, []byte(`"display"`)) {
		t.Fatalf("empty display must be omitted: %s", noDisp)
	}
}

func TestBuildPASBundle_RebindsAndRoutes(t *testing.T) {
	out, err := BuildPASBundle(PASApproveGolden(), "MBR-TEST")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"MBR-TEST"`)) || !bytes.Contains(out, []byte(`Patient/MBR-TEST`)) {
		t.Fatalf("bundle not rebound onto MBR-TEST: %.400s", out)
	}
	// Routable payor identifier added, original payor reference preserved (additive).
	if !bytes.Contains(out, []byte(`"urn:oid:2.16.840.1.113883.6.300"`)) {
		t.Fatalf("no routable payor identifier: %.400s", out)
	}
	if !bytes.Contains(out, []byte(`Organization/InsurerExample`)) {
		t.Fatalf("original payor reference dropped (must stay additive): %.400s", out)
	}
}

// coveragePayorIdentifier extracts the inline Coverage.payor[0].identifier a bundle
// carries, failing the test if there is no Coverage entry or no identifier.
func coveragePayorIdentifier(t *testing.T, bundleJSON []byte) shnsdk.PayerIdentifier {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	entries, _ := b["entry"].([]any)
	for _, e := range entries {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res == nil || res["resourceType"] != "Coverage" {
			continue
		}
		payors, _ := res["payor"].([]any)
		if len(payors) == 0 {
			t.Fatal("Coverage has no payor entries")
		}
		p0, _ := payors[0].(map[string]any)
		ident, _ := p0["identifier"].(map[string]any)
		if ident == nil {
			t.Fatal("Coverage.payor[0] has no inline identifier")
		}
		sys, _ := ident["system"].(string)
		val, _ := ident["value"].(string)
		return shnsdk.PayerIdentifier{System: sys, Value: val}
	}
	t.Fatal("bundle has no Coverage entry")
	return shnsdk.PayerIdentifier{}
}

// TestAddRoutablePayorFor_StampsGivenPayer: a non-CMS payer stamps THAT payer inline on
// Coverage.payor[0].identifier — the misroute rejection test for AddRoutablePayorFor
// (the parameterized form the bridging-demo submit row now uses instead of the
// always-CMS AddRoutablePayor).
func TestAddRoutablePayorFor_StampsGivenPayer(t *testing.T) {
	bridgePayor := shnsdk.PayerIdentifier{System: "urn:shn:demo-payer", Value: "SHN-BRIDGE-DEMO"}
	out, err := AddRoutablePayorFor(PASApproveGolden(), bridgePayor)
	if err != nil {
		t.Fatalf("AddRoutablePayorFor: %v", err)
	}
	if got := coveragePayorIdentifier(t, out); got != bridgePayor {
		t.Fatalf("Coverage.payor[0].identifier = %+v, want %+v", got, bridgePayor)
	}
}

// TestAddRoutablePayor_StillStampsCMS: AddRoutablePayor keeps its historical
// always-CMS behavior — it must be byte-identical to AddRoutablePayorFor(b,
// shnsdk.CMSPayerIdentity), i.e. a pure delegation.
func TestAddRoutablePayor_StillStampsCMS(t *testing.T) {
	got, err := AddRoutablePayor(PASApproveGolden())
	if err != nil {
		t.Fatalf("AddRoutablePayor: %v", err)
	}
	want, err := AddRoutablePayorFor(PASApproveGolden(), shnsdk.CMSPayerIdentity)
	if err != nil {
		t.Fatalf("AddRoutablePayorFor: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("AddRoutablePayor must delegate to AddRoutablePayorFor(CMSPayerIdentity):\ngot:  %s\nwant: %s", got, want)
	}
	if gotID := coveragePayorIdentifier(t, got); gotID != shnsdk.CMSPayerIdentity {
		t.Fatalf("AddRoutablePayor stamped %+v, want CMSPayerIdentity %+v", gotID, shnsdk.CMSPayerIdentity)
	}
}

func TestRebindPASPatient_Errors(t *testing.T) {
	if _, err := RebindPASPatient([]byte(`not json`), "X"); err == nil {
		t.Fatal("want error on non-JSON")
	}
	if _, err := RebindPASPatient([]byte(`{"resourceType":"Bundle","entry":[]}`), "X"); err == nil {
		t.Fatal("want error when the bundle has no Patient to rebind")
	}
}

func TestInjectShnCorrelation(t *testing.T) {
	out, err := InjectShnCorrelation(PASPendGolden(), "corr-123")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"urn:shn:correlation"`)) || !bytes.Contains(out, []byte(`"corr-123"`)) {
		t.Fatalf("correlation identifier not injected: %.400s", out)
	}
}

func TestBuildQuestionnairePackageRequest(t *testing.T) {
	out, err := BuildQuestionnairePackageRequest("http://example.org/Questionnaire/q1|1.0", "MBR-COVERED")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"Parameters"`, `"valueCanonical"`, `http://example.org/Questionnaire/q1|1.0`,
		`"Coverage"`, `Patient/MBR-COVERED`, `dtr-qpackage-input-parameters`,
		// The urn:shn:coverage business identifier, carrying the BARE member
		// id — the same convention shnsdk.BuildCoverageWithPayer stamps (a member
		// number, not a reference), including its "type" v2-0203 MB coding
		// (close-out: completes the copy).
		// The prefetch Coverage is what makes a payer answering at DTR 2.2 able to return
		// a QuestionnaireResponse shell at all: the engine derives that shell's
		// coverageRef from Coverage.id ("coverage-1" here), failing closed without it —
		// i.e. dropping the Coverage makes the whole conformant lane silently 2.0-only.
		`"id":"coverage-1"`, `"urn:shn:coverage"`, `"value":"MBR-COVERED"`,
		`"http://terminology.hl7.org/CodeSystem/v2-0203"`, `"MB"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("package request missing %s: %s", want, out)
		}
	}
}
