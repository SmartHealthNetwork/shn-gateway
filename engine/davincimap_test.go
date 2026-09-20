package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractQuestionnaireFromPackage_ReturnsVerbatimAndDropsDeps IS the
// anti-circularity proof, satisfied IN-PACKAGE against the unexported extractor: the
// fixture is a STANDALONE hand-authored $questionnaire-package (Library + Questionnaire
// + ValueSet) loaded from a reviewable golden file — NOT a one-entry wrap — so extraction
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
	// (b) the dropped deps are NOT in the output (lossy narrowing, VISIBLE).
	if strings.Contains(string(q), "Library") || strings.Contains(string(q), "ValueSet") ||
		strings.Contains(string(q), "cql-lib-1") || strings.Contains(string(q), "vs-1") {
		t.Errorf("extracted output leaked dropped package deps: %s", q)
	}
}

func TestExtractQuestionnaireFromPackage_NoQuestionnaire(t *testing.T) {
	pkg := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Library"}}]}`)
	if _, err := extractQuestionnaireFromPackage(pkg); err == nil {
		t.Error("expected error when the package has no Questionnaire")
	}
}

// TestExtractQuestionnaireFromPackage_ParametersWrapper is the regression test for
// br-payer's $questionnaire-package response shape: a Parameters wrapper with
// parameter[name=="packagebundle"].resource == the collection Bundle. The extractor
// must unwrap to the inner Bundle before walking entries, so that the Questionnaire
// is found exactly as in the bare-Bundle case. Spike capture: br-payer a8bece4
// returns the DTR dtr-qpackage-output-parameters Parameters shape.
func TestExtractQuestionnaireFromPackage_ParametersWrapper(t *testing.T) {
	// Minimal fixture: Parameters{packagebundle → Bundle{Questionnaire}}.
	wrapped := []byte(`{"resourceType":"Parameters","parameter":[` +
		`{"name":"packagebundle","resource":{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"Library","id":"lib1"}},` +
		`{"resource":{"resourceType":"Questionnaire","id":"wrapped-q","url":"http://example.org/Q/wrapped-q","status":"active"}}` +
		`]}},` +
		`{"name":"outcome","resource":{"resourceType":"OperationOutcome"}}` +
		`]}`)

	q, err := extractQuestionnaireFromPackage(wrapped)
	if err != nil {
		t.Fatalf("extractQuestionnaireFromPackage on Parameters wrapper: %v", err)
	}
	var probe struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if err := json.Unmarshal(q, &probe); err != nil {
		t.Fatalf("unmarshal extracted: %v", err)
	}
	if probe.ResourceType != "Questionnaire" || probe.ID != "wrapped-q" {
		t.Errorf("extracted = %s, want Questionnaire wrapped-q", q)
	}
}

// TestExtractQuestionnaireFromPackage_ParametersWrapper_NoPackagebundle confirms that
// a Parameters response with no packagebundle parameter passes through unchanged and
// the downstream walk fails with its normal "no Questionnaire" error (not a panic or
// silent mismatch).
func TestExtractQuestionnaireFromPackage_ParametersWrapper_NoPackagebundle(t *testing.T) {
	noBundle := []byte(`{"resourceType":"Parameters","parameter":[{"name":"outcome","resource":{"resourceType":"OperationOutcome"}}]}`)
	_, err := extractQuestionnaireFromPackage(noBundle)
	if err == nil {
		t.Error("expected error when Parameters has no packagebundle param")
	}
}

// A PAS answer is a complete payer graph. A bare ClaimResponse is not one, and
// neither is a Bundle that names records it does not carry.
func TestValidateNativePASResponse(t *testing.T) {
	approved := pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal))
	for _, body := range [][]byte{approved, []byte(assemblyRealPending)} {
		out, lr := validateNativePASResponse(body)
		if lr.Status != 0 || !bytes.Equal(out, body) {
			t.Fatalf("native retention: %d %s", lr.Status, lr.Message)
		}
	}
	for _, body := range [][]byte{[]byte(assemblyRealTerminal), []byte("null"), []byte("{"), []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)} {
		if _, lr := validateNativePASResponse(body); lr.Status != http.StatusBadGateway {
			t.Fatal("accepted invalid native response")
		}
	}
	// The historical capture is genuinely incomplete, and cannot certify success.
	raw, err := os.ReadFile(filepath.Join("testdata", "br-payer", "pas-submit-response.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, lr := validateNativePASResponse(raw); lr.Status != http.StatusBadGateway {
		t.Fatal("accepted historical missing references")
	}
}

// TestExtractQuestionnaireFromPackage_EveryPublishedBundleName: the package
// Bundle parameter is read under each name a published DTR line gives it
// (return at 2.0.1's operation, PackageBundle at the 2.0.1/2.1.0 output
// profile, packagebundle at 2.2.0), and under no other.
func TestExtractQuestionnaireFromPackage_EveryPublishedBundleName(t *testing.T) {
	wrapped := func(name string) []byte {
		return []byte(`{"resourceType":"Parameters","parameter":[{"name":"operationOutcome","resource":{"resourceType":"OperationOutcome"}},` +
			`{"name":"` + name + `","resource":{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"resource":{"resourceType":"Questionnaire","id":"q-` + name + `","url":"http://example.org/Q/q"}}]}}]}`)
	}
	for _, name := range []string{"return", "PackageBundle", "packagebundle"} {
		q, err := extractQuestionnaireFromPackage(wrapped(name))
		if err != nil || !strings.Contains(string(q), `"q-`+name+`"`) {
			t.Errorf("%s: %s %v", name, q, err)
		}
	}
	for _, name := range []string{"Packagebundle", "bundle", "outcome"} {
		if _, err := extractQuestionnaireFromPackage(wrapped(name)); err == nil {
			t.Errorf("%s read as the package Bundle", name)
		}
	}
}
