package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAugmentCRDHook_AddsHookInstance(t *testing.T) {
	in := []byte(`{"hook":"order-select","context":{"patientId":"Patient/p1","draftOrders":[{"resourceType":"ServiceRequest"}]},"prefetch":{"coverage":{"resourceType":"Coverage"}}}`)
	out, err := augmentCRDHook(in, "hi-123")
	if err != nil {
		t.Fatalf("augmentCRDHook: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}
	if got := strings.Trim(string(m["hookInstance"]), `"`); got != "hi-123" {
		t.Errorf("hookInstance = %q, want hi-123", got)
	}
	// Original fields preserved.
	if _, ok := m["hook"]; !ok {
		t.Error("hook field dropped")
	}
	if _, ok := m["context"]; !ok {
		t.Error("context field dropped")
	}
}

func TestAugmentCRDHook_RejectsMalformed(t *testing.T) {
	if _, err := augmentCRDHook([]byte(`not json`), "hi"); err == nil {
		t.Error("expected error on malformed hook JSON")
	}
}

func TestBuildQuestionnairePackageRequest(t *testing.T) {
	out, err := buildQuestionnairePackageRequest("http://example.org/Questionnaire/lumbar")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var p struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name           string `json:"name"`
			ValueCanonical string `json:"valueCanonical"`
		} `json:"parameter"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ResourceType != "Parameters" {
		t.Errorf("resourceType = %q, want Parameters", p.ResourceType)
	}
	if len(p.Parameter) != 1 || p.Parameter[0].Name != "questionnaire" ||
		p.Parameter[0].ValueCanonical != "http://example.org/Questionnaire/lumbar" {
		t.Errorf("parameter = %+v, want one questionnaire=canonical", p.Parameter)
	}
}

// TestExtractQuestionnaireFromPackage_ReturnsVerbatimAndDropsDeps IS the §8.3
// anti-circularity proof, satisfied IN-PACKAGE against the unexported extractor: the
// fixture is a STANDALONE hand-authored $questionnaire-package (Library + Questionnaire
// + ValueSet) loaded from a reviewable golden file — NOT wrap(sandboxQ) — so extraction
// is proven on input the connector did not construct. Asserts (a) the Questionnaire is
// extracted verbatim and (b) the Library/ValueSet deps are NOT in the extracted output
// — the extractor's job is to return the bare Questionnaire that the consumer feeds to
// ParseQuestionnaireURL (F5) + FillQuestionnaire; the full package (with its deps) is
// what travels the wire to the consumer (originate.go). The extractor is now called
// consumer-side (originate.go), not by native.go. Because it runs in-package on the
// unexported func, NO exported shim is needed.
func TestExtractQuestionnaireFromPackage_ReturnsVerbatimAndDropsDeps(t *testing.T) {
	pkg, err := os.ReadFile(filepath.Join("testdata", "dtr-package-with-deps.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	q, err := extractQuestionnaireFromPackage(pkg)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// (a) the extracted resource IS the Questionnaire entry, verbatim.
	if !json.Valid(q) {
		t.Fatalf("extracted not valid json: %s", q)
	}
	var probe struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if err := json.Unmarshal(q, &probe); err != nil {
		t.Fatalf("unmarshal extracted: %v", err)
	}
	if probe.ResourceType != "Questionnaire" || probe.ID != "real-partner-q" {
		t.Errorf("extracted = %s, want the Questionnaire entry real-partner-q", q)
	}
	// (b) the dropped deps are NOT in the output (lossy narrowing, VISIBLE — §6.2).
	if strings.Contains(string(q), "Library") || strings.Contains(string(q), "ValueSet") ||
		strings.Contains(string(q), "cql-lib-1") || strings.Contains(string(q), "vs-1") {
		t.Errorf("extracted output leaked dropped package deps (see follow-up §6.2): %s", q)
	}
}

func TestExtractQuestionnaireFromPackage_NoQuestionnaire(t *testing.T) {
	pkg := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Library"}}]}`)
	if _, err := extractQuestionnaireFromPackage(pkg); err == nil {
		t.Error("expected error when the package has no Questionnaire")
	}
}

func TestBuildQuestionnairePackage_WrapsAndRoundTrips(t *testing.T) {
	q := []byte(`{"resourceType":"Questionnaire","id":"q1","url":"http://x/q"}`)
	pkg, err := buildQuestionnairePackage(q)
	if err != nil {
		t.Fatalf("buildQuestionnairePackage: %v", err)
	}
	// (a) it is a collection Bundle.
	var probe struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
	}
	if err := json.Unmarshal(pkg, &probe); err != nil {
		t.Fatalf("unmarshal package: %v", err)
	}
	if probe.ResourceType != "Bundle" || probe.Type != "collection" {
		t.Errorf("package = %s, want a collection Bundle", pkg)
	}
	// (b) extract∘wrap == the original Questionnaire (verbatim round-trip).
	got, err := extractQuestionnaireFromPackage(pkg)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	var gp struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if err := json.Unmarshal(got, &gp); err != nil {
		t.Fatalf("unmarshal extracted: %v", err)
	}
	if gp.ResourceType != "Questionnaire" || gp.ID != "q1" {
		t.Errorf("extracted = %s, want the q1 Questionnaire", got)
	}
	// (c) the canonical byte shape (json.Marshal of the map → sorted keys). The
	// loopback's default wrap (a later task) MUST match this exactly for DTR byte-parity.
	want := `{"entry":[{"fullUrl":"http://x/q","resource":{"resourceType":"Questionnaire","id":"q1","url":"http://x/q"}}],"resourceType":"Bundle","type":"collection"}`
	if string(pkg) != want {
		t.Errorf("package bytes = %s, want %s", pkg, want)
	}
}

func TestBuildQuestionnairePackage_RejectsInvalidJSON(t *testing.T) {
	if _, err := buildQuestionnairePackage([]byte("{not json")); err == nil {
		t.Error("expected error wrapping invalid Questionnaire json")
	}
}
