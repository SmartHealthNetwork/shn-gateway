package scenariodriver

import (
	"bytes"
	"encoding/json"
	"testing"
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
		`"Coverage"`, `Patient/MBR-COVERED`, `dtr-qpackage-input-parameters`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("package request missing %s: %s", want, out)
		}
	}
}
