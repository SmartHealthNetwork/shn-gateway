package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestBuildQuestionnairePackageRequest(t *testing.T) {
	// Absent-coverage path (the sandbox / 8-UC-demo path): canonical-only, EXACTLY as
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
// to the 2.0 delegate, fencing the earlier sandbox/8-UC-demo path.
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

// TestBuildQuestionnairePackageAtLine_RegressionFence: the legacy
// buildQuestionnairePackage is byte-identical to AtLine("2.0", q, nil) — the
// third twin (mirrors shnsdk.BuildQuestionnairePackageAtLine / internal/dtr.
// WrapQuestionnairePackageAtLine).
func TestBuildQuestionnairePackageAtLine_RegressionFence(t *testing.T) {
	q := []byte(`{"resourceType":"Questionnaire","id":"q1","url":"http://x/q"}`)
	legacy, err := buildQuestionnairePackage(q)
	if err != nil {
		t.Fatalf("buildQuestionnairePackage: %v", err)
	}
	atLine, err := buildQuestionnairePackageAtLine("2.0", q, nil)
	if err != nil {
		t.Fatalf("buildQuestionnairePackageAtLine(2.0): %v", err)
	}
	if !bytes.Equal(legacy, atLine) {
		t.Fatalf("buildQuestionnairePackage != buildQuestionnairePackageAtLine(\"2.0\", nil):\n legacy: %s\n atLine: %s", legacy, atLine)
	}
}

// TestBuildQuestionnairePackageAtLine_UnknownLineErrors: fail-closed rejection.
func TestBuildQuestionnairePackageAtLine_UnknownLineErrors(t *testing.T) {
	q := []byte(`{"resourceType":"Questionnaire","id":"q1","url":"http://x/q"}`)
	if _, err := buildQuestionnairePackageAtLine("9.9", q, nil); err == nil {
		t.Fatal("buildQuestionnairePackageAtLine(\"9.9\") = nil error, want an error")
	}
}

// TestBuildQuestionnairePackageAtLine_QRRequiredAt22 mirrors shnsdk's
// TestBuildQuestionnairePackageAtLine_QRRequiredAt22 / internal/dtr's
// TestWrapQuestionnairePackageAtLine_QRRequiredAt22 (the DTR line delta table:
// DTR-QPackageBundle's Bundle.entry:questionnaireResponse min=1 at 2.2 only).
func TestBuildQuestionnairePackageAtLine_QRRequiredAt22(t *testing.T) {
	q := []byte(`{"resourceType":"Questionnaire","id":"q1","url":"http://x/q"}`)

	for _, line := range []string{"2.0", "2.1"} {
		pkg, err := buildQuestionnairePackageAtLine(line, q, nil)
		if err != nil {
			t.Fatalf("line %s: nil QR must be accepted: %v", line, err)
		}
		var probe struct {
			Entry []json.RawMessage `json:"entry"`
		}
		if err := json.Unmarshal(pkg, &probe); err != nil {
			t.Fatalf("line %s: unmarshal package: %v", line, err)
		}
		if len(probe.Entry) != 1 {
			t.Errorf("line %s: entry count = %d, want 1 (no QR supplied)", line, len(probe.Entry))
		}
	}

	if _, err := buildQuestionnairePackageAtLine("2.2", q, nil); err == nil {
		t.Fatal("buildQuestionnairePackageAtLine(\"2.2\", q, nil) = nil error, want an error (QR required)")
	}

	qr := []byte(`{"resourceType":"QuestionnaireResponse","id":"qr-1","status":"completed"}`)
	pkg, err := buildQuestionnairePackageAtLine("2.2", q, qr)
	if err != nil {
		t.Fatalf("buildQuestionnairePackageAtLine(2.2, q, qr): %v", err)
	}
	var probe struct {
		Entry []struct {
			FullUrl string `json:"fullUrl"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(pkg, &probe); err != nil {
		t.Fatalf("unmarshal 2.2 package: %v", err)
	}
	if len(probe.Entry) != 2 {
		t.Fatalf("2.2 entry count = %d, want 2", len(probe.Entry))
	}
	if probe.Entry[1].FullUrl != "https://shn.example/fhir/QuestionnaireResponse/qr-1" {
		t.Errorf("QR entry fullUrl = %q, want the derived https://shn.example/fhir/QuestionnaireResponse/qr-1", probe.Entry[1].FullUrl)
	}
}

// TestBuildQuestionnairePackageAtLine_QRMissingIDErrors: a supplied QR with no id
// cannot be given a resolvable fullUrl — never fabricated, so this errors. Mirrors
// shnsdk's TestBuildQuestionnairePackageAtLine_QRMissingIDErrors / internal/dtr's
// TestWrapQuestionnairePackageAtLine_QRMissingIDErrors.
func TestBuildQuestionnairePackageAtLine_QRMissingIDErrors(t *testing.T) {
	q := []byte(`{"resourceType":"Questionnaire","id":"q1","url":"http://x/q"}`)
	qr := []byte(`{"resourceType":"QuestionnaireResponse","status":"completed"}`)
	if _, err := buildQuestionnairePackageAtLine("2.2", q, qr); err == nil {
		t.Fatal("buildQuestionnairePackageAtLine(2.2, q, qr-without-id) = nil error, want an error")
	}
}

func TestDTRPackageCoverageSubjectDerivesFromCoverageID(t *testing.T) {
	// A conformant external client's plain US Core Coverage: id + beneficiary,
	// NO urn:shn:coverage identifier — must now SUCCEED (spec §2: the
	// external-client-works property; the private-system coupling is removed).
	cov := []byte(`{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"}}`)
	patientRef, coverageRef, err := dtrPackageCoverageSubject(cov)
	if err != nil {
		t.Fatalf("id-carrying Coverage without private identifier must succeed: %v", err)
	}
	if patientRef != "Patient/p1" || coverageRef != "Coverage/c1" {
		t.Fatalf("got (%q,%q), want (Patient/p1, Coverage/c1)", patientRef, coverageRef)
	}
}

func TestDTRPackageCoverageSubjectFailsClosedWithoutID(t *testing.T) {
	// Valid 2.2 coverage param minus id → error (never guess a reference).
	cov := []byte(`{"resourceType":"Coverage","status":"active","beneficiary":{"reference":"Patient/p1"},"identifier":[{"system":"urn:shn:coverage","value":"MBR-X"}]}`)
	if _, _, err := dtrPackageCoverageSubject(cov); err == nil {
		t.Fatal("id-less Coverage must fail closed — the identifier is no longer a fallback")
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

// TestNormalizePASResponse_BareClaimResponse verifies that a bare ClaimResponse
// (already in SHN canonical shape) passes through unchanged.
func TestNormalizePASResponse_BareClaimResponse(t *testing.T) {
	input := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"PA-abc123","status":"active","use":"preauthorization"}`)
	out, lr := normalizePASResponse(input)
	if lr.Status != 0 {
		t.Fatalf("bare ClaimResponse must pass through; got 502: %s", lr.Message)
	}
	if string(out) != string(input) {
		t.Errorf("bare ClaimResponse output differs from input:\n got: %s\nwant: %s", out, input)
	}
}

// TestNormalizePASResponse_SHNPendedBundle verifies that an SHN pended Bundle
// (ClaimResponse + Task) passes through unchanged — the Task-pass-through branch.
func TestNormalizePASResponse_SHNPendedBundle(t *testing.T) {
	input := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"ClaimResponse","outcome":"queued"}},` +
		`{"resource":{"resourceType":"Task","status":"requested"}}]}`)
	out, lr := normalizePASResponse(input)
	if lr.Status != 0 {
		t.Fatalf("SHN pended Bundle must pass through; got 502: %s", lr.Message)
	}
	if string(out) != string(input) {
		t.Errorf("SHN pended Bundle output differs from input:\n got: %s\nwant: %s", out, input)
	}
}

// TestNormalizePASResponse_BundleCompleteUnwrap verifies that a Bundle containing a
// ClaimResponse with outcome=="complete" (the real Da Vinci approve/deny shape) is
// unwrapped to just the bare ClaimResponse.
func TestNormalizePASResponse_BundleCompleteUnwrap(t *testing.T) {
	crJSON := `{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"PA-xyz"}`
	input := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":` + crJSON + `},` +
		`{"resource":{"resourceType":"Organization","id":"org1"}}]}`)
	out, lr := normalizePASResponse(input)
	if lr.Status != 0 {
		t.Fatalf("Bundle(complete ClaimResponse) must unwrap; got 502: %s", lr.Message)
	}
	// Output must be the bare ClaimResponse, not the Bundle.
	var probe struct {
		ResourceType string `json:"resourceType"`
		Outcome      string `json:"outcome"`
		PreAuthRef   string `json:"preAuthRef"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("unwrapped output not valid JSON: %v", err)
	}
	if probe.ResourceType != "ClaimResponse" || probe.Outcome != "complete" || probe.PreAuthRef != "PA-xyz" {
		t.Errorf("unwrapped = %+v, want ClaimResponse/complete/PA-xyz", probe)
	}
}

// TestNormalizePASResponse_QueuedBundle_PassThrough verifies that a Bundle with a queued
// ClaimResponse (real-RI pended shape, no SHN Task) passes through unchanged — DEF-G1 lifted.
// br-payer's amended re-POST response is exactly this shape (A4 queued, no Task).
func TestNormalizePASResponse_QueuedBundle_PassThrough(t *testing.T) {
	input := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"ClaimResponse","outcome":"queued"}}]}`)
	out, lr := normalizePASResponse(input)
	if lr.Status != 0 {
		t.Fatalf("queued Bundle must pass through (DEF-G1 lifted), got status=%d msg=%s", lr.Status, lr.Message)
	}
	if !bytes.Equal(out, input) {
		t.Fatalf("queued Bundle pass-through must be verbatim")
	}
}

// TestNormalizePASResponse_UnknownBundle_FailClosed verifies that a Bundle with no Task,
// no complete ClaimResponse, and no queued ClaimResponse fails closed with 502.
func TestNormalizePASResponse_UnknownBundle_FailClosed(t *testing.T) {
	input := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"Claim","outcome":"active"}}]}`)
	_, lr := normalizePASResponse(input)
	if lr.Status != http.StatusBadGateway {
		t.Fatalf("unknown Bundle must 502 fail-closed, got %d", lr.Status)
	}
}

// TestNormalizePASResponse_Unparseable_FailClosed verifies that unparseable input
// fails closed with 502.
func TestNormalizePASResponse_Unparseable_FailClosed(t *testing.T) {
	_, lr := normalizePASResponse([]byte(`{not json`))
	if lr.Status != http.StatusBadGateway {
		t.Fatalf("unparseable must 502 fail-closed, got %d", lr.Status)
	}
}

// TestNormalizePASResponse_BrPayerPended pins the relay's A4 path against the REAL captured br-payer
// home-oxygen $submit response. Converts the R-2(b) "discover live" risk into a
// hermetic guard. Verified live: br-payer's A4 Bundle{ClaimResponse(queued,A4)+Org+Task}
// carries a Task, so normalizePASResponse's Task branch passes it through verbatim (Status 0, no
// 502 — DEF-G1 does not bite), and ParsePendedResponse reads it as pended.
func TestNormalizePASResponse_BrPayerPended(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "br-payer", "pas-submit-response-pended.json"))
	if err != nil {
		t.Fatalf("read pended golden: %v", err)
	}
	norm, lr := normalizePASResponse(body)
	if lr.Status != 0 {
		t.Fatalf("br-payer A4 must pass through (it has a Task), got %d: %s", lr.Status, lr.Message)
	}
	pended, _, perr := shnsdk.ParsePendedResponse(norm)
	if perr != nil || !pended {
		t.Fatalf("br-payer A4 must read as pended via ParsePendedResponse; pended=%v err=%v", pended, perr)
	}
}

// TestNormalizePASResponse_RealRI_brpayer is the LIVE real-RI proof: it loads the
// committed br-payer $submit approve response (a Bundle wrapping a ClaimResponse with
// outcome:complete + reviewAction A1 + preAuthRef in the "number" sub-extension), runs
// it through normalizePASResponse, and asserts the unwrapped bare ClaimResponse is
// readable by shnsdk.ParseClaimResponse as approved with preAuthRef=="AUTH-0001" (FR-G28).
func TestNormalizePASResponse_RealRI_brpayer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "br-payer", "pas-submit-response.json"))
	if err != nil {
		t.Fatalf("read br-payer fixture: %v", err)
	}
	// The br-payer fixture is a Bundle with a ClaimResponse(complete) + an Organization
	// entry — no Task present. Discriminator must unwrap to the bare ClaimResponse.
	out, lr := normalizePASResponse(raw)
	if lr.Status != 0 {
		t.Fatalf("normalizePASResponse rejected real br-payer approve Bundle: %d %s", lr.Status, lr.Message)
	}
	// The output must be a bare ClaimResponse (not a Bundle).
	var top struct {
		ResourceType string `json:"resourceType"`
	}
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unwrapped output not valid JSON: %v", err)
	}
	if top.ResourceType != "ClaimResponse" {
		t.Fatalf("unwrapped resourceType = %q, want ClaimResponse", top.ResourceType)
	}
	// ParseClaimResponse must read it as approved with preAuthRef AUTH-0001.
	// The auth number lives in item[0].adjudication[0].extension[reviewAction].extension[number]
	// (real Da Vinci RI convention) — not in a top-level preAuthRef field.
	parsed, err := shnsdk.ParseClaimResponse(out)
	if err != nil {
		t.Fatalf("ParseClaimResponse on unwrapped br-payer ClaimResponse: %v", err)
	}
	if parsed.Outcome != "approved" {
		t.Errorf("outcome = %q, want approved", parsed.Outcome)
	}
	if parsed.PreAuthRef != "AUTH-0001" {
		t.Errorf("preAuthRef = %q, want AUTH-0001", parsed.PreAuthRef)
	}
}
