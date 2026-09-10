package engine

import (
	"bytes"
	"encoding/json"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildQuestionnairePackageRequest(t *testing.T) {
	// Absent-coverage path (the 8-UC demo path): canonical-only, EXACTLY as
	// before the coverage-carry fix. This locks the demo-path parity — a regression that started
	// emitting a coverage param when none was supplied fails here.
	out, err := buildQuestionnairePackageRequest("http://example.org/Questionnaire/lumbar", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var p struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name           string          `json:"name"`
			ValueCanonical string          `json:"valueCanonical"`
			Resource       json.RawMessage `json:"resource"`
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

// TestBuildQuestionnairePackageRequest_CarriesCoverage is the coverage-carry regression guard
// (FR-G28): when the inbound $questionnaire-package carried a coverage Parameters
// resource, the native-forward rebuild MUST emit a `coverage` parameter carrying that
// resource VERBATIM — a real Da Vinci payer (br-payer) 400s with "The 'coverage'
// parameter is required (min=1)" otherwise. The payer-gw carries the provider's coverage
// through (non-aggregation: it does NOT fabricate one).
func TestBuildQuestionnairePackageRequest_CarriesCoverage(t *testing.T) {
	coverage := json.RawMessage(`{"resourceType":"Coverage","id":"cov-1","status":"active",` +
		`"beneficiary":{"reference":"Patient/p1"}}`)
	out, err := buildQuestionnairePackageRequest("http://example.org/Questionnaire/lumbar", coverage)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var p struct {
		Parameter []struct {
			Name           string          `json:"name"`
			ValueCanonical string          `json:"valueCanonical"`
			Resource       json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var qSeen, covSeen bool
	for _, param := range p.Parameter {
		switch param.Name {
		case "questionnaire":
			qSeen = true
			if param.ValueCanonical != "http://example.org/Questionnaire/lumbar" {
				t.Errorf("questionnaire canonical = %q", param.ValueCanonical)
			}
		case "coverage":
			covSeen = true
			// The Coverage resource is carried VERBATIM (round-trip equal under JSON).
			var got, want any
			if err := json.Unmarshal(param.Resource, &got); err != nil {
				t.Fatalf("coverage resource not valid json: %v", err)
			}
			if err := json.Unmarshal(coverage, &want); err != nil {
				t.Fatalf("want coverage not valid json: %v", err)
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if string(gb) != string(wb) {
				t.Errorf("coverage resource = %s, want %s", gb, wb)
			}
		}
	}
	if !qSeen {
		t.Error("questionnaire parameter missing")
	}
	if !covSeen {
		t.Error("coverage parameter missing — payer would 400")
	}
}

// TestQuestionnairePackageRequestCoverageByLine is the request-side
// coverage-1..1 gate: at DTR line 2.2 (DTRDef.QuestionnairePackageCoverageRequired), an
// empty coverage refuses BEFORE the wire with a legible error naming the line and the
// 1..1 cardinality — replacing what would otherwise be the partner's opaque 400 — and a
// non-empty coverage builds normally. The legacy (unparameterized) name stays byte-identical
// to the 2.0 delegate, fencing the earlier 8-UC demo path.
func TestQuestionnairePackageRequestCoverageByLine(t *testing.T) {
	const canonical = "http://example.org/Questionnaire/lumbar"
	coverage := json.RawMessage(`{"resourceType":"Coverage","id":"cov-1","status":"active",` +
		`"beneficiary":{"reference":"Patient/p1"}}`)

	t.Run("2.2 no coverage errors naming the line and 1..1", func(t *testing.T) {
		_, err := buildQuestionnairePackageRequestAtLine("2.2", canonical, nil)
		if err == nil {
			t.Fatal("want an error refusing before the wire, got nil")
		}
		if !strings.Contains(err.Error(), "2.2") {
			t.Errorf("error must name the line 2.2: %v", err)
		}
		if !strings.Contains(err.Error(), "1..1") {
			t.Errorf("error must name the 1..1 cardinality: %v", err)
		}
	})
	t.Run("2.2 with coverage: parameter present", func(t *testing.T) {
		out, err := buildQuestionnairePackageRequestAtLine("2.2", canonical, coverage)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var p struct {
			Parameter []struct {
				Name string `json:"name"`
			} `json:"parameter"`
		}
		if err := json.Unmarshal(out, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		var covSeen bool
		for _, param := range p.Parameter {
			if param.Name == "coverage" {
				covSeen = true
			}
		}
		if !covSeen {
			t.Error("coverage parameter missing")
		}
	})
	t.Run("2.1 no coverage does not error (min=1 max=* only — not yet gated locally)", func(t *testing.T) {
		if _, err := buildQuestionnairePackageRequestAtLine("2.1", canonical, nil); err != nil {
			t.Fatalf("2.1 with no coverage must NOT refuse locally (the earlier behavior): %v", err)
		}
	})
	t.Run("2.0 legacy call byte-identical to the AtLine delegate", func(t *testing.T) {
		legacy, err := buildQuestionnairePackageRequest(canonical, nil)
		if err != nil {
			t.Fatalf("legacy build: %v", err)
		}
		atLine, err := buildQuestionnairePackageRequestAtLine("2.0", canonical, nil)
		if err != nil {
			t.Fatalf("AtLine(2.0) build: %v", err)
		}
		if string(legacy) != string(atLine) {
			t.Fatalf("legacy = %s, AtLine(2.0) = %s — must be byte-identical", legacy, atLine)
		}
	})
	t.Run("order variant: same coverage-1..1 gate at 2.2", func(t *testing.T) {
		order := json.RawMessage(`{"resourceType":"ServiceRequest","id":"sr-1"}`)
		if _, err := buildQuestionnairePackageOrderRequestAtLine("2.2", order, nil); err == nil {
			t.Fatal("order-driven request at 2.2 with no coverage must refuse before the wire")
		}
		out, err := buildQuestionnairePackageOrderRequestAtLine("2.2", order, coverage)
		if err != nil {
			t.Fatalf("build with coverage: %v", err)
		}
		if !strings.Contains(string(out), `"name":"coverage"`) {
			t.Errorf("coverage parameter missing: %s", out)
		}
		legacy, err := buildQuestionnairePackageOrderRequest(order, nil)
		if err != nil {
			t.Fatalf("legacy order build: %v", err)
		}
		atLine, err := buildQuestionnairePackageOrderRequestAtLine("2.0", order, nil)
		if err != nil {
			t.Fatalf("AtLine(2.0) order build: %v", err)
		}
		if string(legacy) != string(atLine) {
			t.Fatalf("legacy = %s, AtLine(2.0) = %s — must be byte-identical", legacy, atLine)
		}
	})
}

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

// TestNormalizeCRDCoverage_RealRI_brpayer replays a LIVE captured br-payer CRD response
// through the normalizer. The br-payer RI (CRD STU 2.2.1) places the split
// coverage-information at systemActions[].resource.extension[] — the primary walk path.
// Asserts: covered=covered, pa-needed=auth-needed (PARequired true), questionnaire present
// (NeedsDTR true). FR-G25.
func TestNormalizeCRDCoverage_RealRI_brpayer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "br-payer", "crd-response.json"))
	if err != nil {
		t.Fatal(err) // fixture IS committed
	}
	cov, lr := normalizeCRDCoverage(raw)
	if lr.Status != 0 {
		t.Fatalf("rejected real br-payer card: %d %s", lr.Status, lr.Message)
	}
	if cov.Covered != shnsdk.CoveredCovered {
		t.Fatalf("covered=%q, want %q", cov.Covered, shnsdk.CoveredCovered)
	}
	if !cov.PARequired() {
		t.Fatalf("pa-needed=auth-needed must be PARequired; got PANeeded=%q", cov.PANeeded)
	}
	// The br-payer response carries questionnaire=http://example.org/fhir/Questionnaire/PriorAuthRequired.
	if !cov.NeedsDTR() {
		t.Fatalf("questionnaire sub-extension present; NeedsDTR must be true; got Questionnaires=%v", cov.Questionnaires)
	}
}

// TestNormalizeCRDCoverage_STU21_split reads the forward-target STU-2.1 split shape
// (covered + pa-needed + questionnaire sub-extensions) 1:1 onto CardCoverage.
func TestNormalizeCRDCoverage_STU21_split(t *testing.T) {
	// synthetic 2.1 split-shape fixture (inline) with covered+auth-needed+questionnaire.
	body := []byte(`{"cards":[{"suggestions":[{"actions":[{"resource":{"extension":[{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information","extension":[{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"auth-needed"},{"url":"questionnaire","valueCanonical":"http://example/Q|1.0.0"}]}]}}]}]}]}`)
	cov, lr := normalizeCRDCoverage(body)
	if lr.Status != 0 {
		t.Fatal(lr.Message)
	}
	if !cov.PARequired() || !cov.NeedsDTR() {
		t.Fatalf("2.1 split: %+v", cov)
	}
	if cov.Questionnaires[0] != "http://example/Q|1.0.0" {
		t.Fatalf("questionnaire canonical = %q", cov.Questionnaires[0])
	}
}

// TestNormalizeCRDCoverage_STU21_CardExtensionFallback proves the defensive fallback:
// some RIs put coverage-information on cards[].extension[] (a bare card extension) rather
// than the suggestion's update-action resource. The normalizer must find it there too.
func TestNormalizeCRDCoverage_STU21_CardExtensionFallback(t *testing.T) {
	body := []byte(`{"cards":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information","extension":[{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"no-auth"}]}]}]}`)
	cov, lr := normalizeCRDCoverage(body)
	if lr.Status != 0 {
		t.Fatalf("card.extension fallback rejected: %d %s", lr.Status, lr.Message)
	}
	if cov.Covered != shnsdk.CoveredCovered || cov.PARequired() {
		t.Fatalf("fallback → %+v, want covered+no-auth", cov)
	}
}

// TestNormalizeCRDCoverage_Unmappable fails closed when no coverage-information signal is
// resolvable in the response (502, since the CRD leg has no $validate net).
func TestNormalizeCRDCoverage_Unmappable(t *testing.T) {
	_, lr := normalizeCRDCoverage([]byte(`{"cards":[{"summary":"x"}]}`))
	if lr.Status != http.StatusBadGateway {
		t.Fatalf("un-mappable must 502, got %d", lr.Status)
	}
}

// TestNormalizeCRDCoverage_MalformedBody fails closed on a non-JSON partner body.
func TestNormalizeCRDCoverage_MalformedBody(t *testing.T) {
	_, lr := normalizeCRDCoverage([]byte(`{not json`))
	if lr.Status != http.StatusBadGateway {
		t.Fatalf("malformed body must 502, got %d", lr.Status)
	}
}

// Native submit retains the complete payer graph; bare resources belong to polling.
func TestValidateNativePASResponse(t *testing.T) {
	approved, err := assembleTerminalPASBundle([]byte(assemblyRealPending), []byte(assemblyRealTerminal), fixedClock())
	if err != nil {
		t.Fatal(err)
	}
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
