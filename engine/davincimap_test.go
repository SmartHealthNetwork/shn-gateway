package engine

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
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

// TestNormalizePASResponse_OtherBundle_FailClosed verifies that a Bundle with no Task
// and no complete ClaimResponse (e.g. a real-RI queued/pended shape) fails closed with
// a 502 — deferred normalization (DEF-G1).
func TestNormalizePASResponse_OtherBundle_FailClosed(t *testing.T) {
	input := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"ClaimResponse","outcome":"queued"}}]}`)
	_, lr := normalizePASResponse(input)
	if lr.Status != http.StatusBadGateway {
		t.Fatalf("other Bundle must 502 fail-closed, got %d", lr.Status)
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
