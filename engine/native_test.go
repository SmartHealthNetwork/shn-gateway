package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdsServicesJSON returns a /cds-services listing with the given services.
// Each entry is {"id":"<id>","hook":"<hook>"}.
func cdsServicesJSON(services ...struct{ id, hook string }) []byte {
	type svc struct {
		ID   string `json:"id"`
		Hook string `json:"hook"`
	}
	svcs := make([]svc, len(services))
	for i, s := range services {
		svcs[i] = svc{ID: s.id, Hook: s.hook}
	}
	out, _ := json.Marshal(map[string]any{"services": svcs})
	return out
}

// TestDiscoverCRDServiceID covers: override wins, single match, zero matches → error,
// ambiguous → error. Uses br-payer's real cds-services shape (services[].id/hook).
func TestDiscoverCRDServiceID_OverrideWins(t *testing.T) {
	// The server is never called when override is set.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server was called despite override being set")
	}))
	defer srv.Close()

	got, err := DiscoverCRDServiceID(context.Background(), srv.Client(), srv.URL, "my-override-id")
	if err != nil {
		t.Fatalf("DiscoverCRDServiceID with override: %v", err)
	}
	if got != "my-override-id" {
		t.Errorf("got %q, want override", got)
	}
}

// TestDiscoverCRDServiceID_SingleMatch tests discovery against a realistic /cds-services
// listing containing one order-select service and one order-sign service (br-payer shape).
// The function must select the single order-select service and return its id.
func TestDiscoverCRDServiceID_SingleMatch(t *testing.T) {
	listing := cdsServicesJSON(
		struct{ id, hook string }{"order-sign-crd", "order-sign"},     // br-payer's real service (order-sign — not matched)
		struct{ id, hook string }{"order-select-svc", "order-select"}, // hypothetical order-select service (matched)
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cds-services" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(listing)
	}))
	defer srv.Close()

	got, err := DiscoverCRDServiceID(context.Background(), srv.Client(), srv.URL, "")
	if err != nil {
		t.Fatalf("DiscoverCRDServiceID: %v", err)
	}
	if got != "order-select-svc" {
		t.Errorf("got %q, want %q", got, "order-select-svc")
	}
}

// TestDiscoverCRDServiceID_ZeroMatchError proves fail-closed when no order-select
// service exists (e.g. only an order-sign service like br-payer's order-sign-crd).
// This is the expected result for br-payer without the override — callers must set
// PAYER_DAVINCI_CRD_SERVICE_ID=order-sign-crd for br-payer.
func TestDiscoverCRDServiceID_ZeroMatchError(t *testing.T) {
	// Realistic br-payer /cds-services listing: one order-sign service, no order-select.
	listing := cdsServicesJSON(
		struct{ id, hook string }{"order-sign-crd", "order-sign"},
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(listing)
	}))
	defer srv.Close()

	_, err := DiscoverCRDServiceID(context.Background(), srv.Client(), srv.URL, "")
	if err == nil {
		t.Fatal("expected error for zero order-select services, got nil")
	}
	if !strings.Contains(err.Error(), "no") || !strings.Contains(err.Error(), "order-select") {
		t.Errorf("error message should mention missing order-select service, got: %v", err)
	}
}

// TestDiscoverCRDServiceID_AmbiguousError proves fail-closed when multiple
// order-select services exist (operator must set the override to resolve).
func TestDiscoverCRDServiceID_AmbiguousError(t *testing.T) {
	listing := cdsServicesJSON(
		struct{ id, hook string }{"order-select-a", "order-select"},
		struct{ id, hook string }{"order-select-b", "order-select"},
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(listing)
	}))
	defer srv.Close()

	_, err := DiscoverCRDServiceID(context.Background(), srv.Client(), srv.URL, "")
	if err == nil {
		t.Fatal("expected error for ambiguous order-select services, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error message should mention ambiguous, got: %v", err)
	}
}

// stubPartner records the last request path/body and returns a programmed response.
type stubPartner struct {
	srv        *httptest.Server
	lastPath   string
	lastBody   []byte
	status     int
	respByPath map[string][]byte
}

func newStubPartner(t *testing.T) *stubPartner {
	t.Helper()
	s := &stubPartner{status: 200, respByPath: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lastPath = r.URL.Path
		s.lastBody, _ = io.ReadAll(r.Body)
		if s.status/100 != 2 {
			w.WriteHeader(s.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.respByPath[r.URL.Path])
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func TestNativeResponder_EligibilityForwardsVerbatim(t *testing.T) {
	p := newStubPartner(t)
	cer := []byte(`{"resourceType":"CoverageEligibilityResponse","patient":{"reference":"Patient/p1"}}`)
	p.respByPath["/CoverageEligibilityRequest"] = cer
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)

	res, err := n.Handle(context.Background(), "coverage-eligibility", "corr", "pci",
		[]byte(`{"resourceType":"CoverageEligibilityRequest","patient":{"reference":"Patient/p1"}}`))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("Status = %d, want 0", res.Status)
	}
	if string(res.ResponseFHIR) != string(cer) {
		t.Errorf("ResponseFHIR = %s, want partner bytes verbatim", res.ResponseFHIR)
	}
	if p.lastPath != "/CoverageEligibilityRequest" {
		t.Errorf("forwarded to %q", p.lastPath)
	}
}

func TestNativeResponder_CRDAddsHookInstanceAndNormalizes(t *testing.T) {
	p := newStubPartner(t)
	// The partner returns a split-shape coverage-information (covered=covered, pa-needed=no-auth);
	// the CRD leg NORMALIZES it (FR-G25) and re-renders canonical SHN cards rather than
	// forwarding the partner's RI-specific wire form verbatim.
	partnerCard := []byte(`{"cards":[{"suggestions":[{"actions":[{"resource":{"extension":[` +
		`{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information",` +
		`"extension":[{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"no-auth"}]}]}}]}]}]}`)
	// The conventional CDS service path (Task: keep in sync with native.go's const).
	p.respByPath["/cds-services/shn-order-select"] = partnerCard
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)

	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci",
		[]byte(`{"hook":"order-select","context":{"patientId":"p1","draftOrders":[{}]}}`))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("Status = %d, want 0", res.Status)
	}
	// The response is the canonical SHN cards (normalized), NOT the partner bytes verbatim.
	cov, perr := shnsdk.ParseCards(res.ResponseFHIR)
	if perr != nil {
		t.Fatalf("response is not canonical SHN cards: %v (%s)", perr, res.ResponseFHIR)
	}
	if cov.Covered != shnsdk.CoveredCovered || cov.PARequired() {
		t.Errorf("normalized coverage = %+v, want covered+no-auth", cov)
	}
	// The forwarded hook carries the CDS-Hooks-required hookInstance.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(p.lastBody, &m); err != nil {
		t.Fatalf("forwarded body not JSON: %v", err)
	}
	if _, ok := m["hookInstance"]; !ok {
		t.Errorf("forwarded hook missing hookInstance: %s", p.lastBody)
	}
}

// TestNativeResponder_CRDUnmappablePartnerIs502 proves the CRD leg fails closed when the
// partner returns a card with no resolvable coverage-information (FR-G25; no $validate net).
func TestNativeResponder_CRDUnmappablePartnerIs502(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/cds-services/shn-order-select"] = []byte(`{"cards":[{"summary":"x"}]}`)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci",
		[]byte(`{"hook":"order-select","context":{"patientId":"p1","draftOrders":[{}]}}`))
	if err != nil {
		t.Fatalf("Handle returned error (want Status 502, not error): %v", err)
	}
	if res.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want 502 (un-mappable partner CRD card)", res.Status)
	}
}

func TestNativeResponder_DTRForwardsPackageVerbatim(t *testing.T) {
	p := newStubPartner(t)
	// A deps-RICH package — the native path must forward it byte-for-byte (deps preserved).
	pkg := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"Questionnaire","id":"q1","url":"http://x/q"}},` +
		`{"resource":{"resourceType":"Library","id":"cql-lib-1"}},` +
		`{"resource":{"resourceType":"ValueSet","id":"vs-1"}}]}`)
	p.respByPath["/Questionnaire/$questionnaire-package"] = pkg
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)

	res, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci",
		[]byte(`{"canonical":"http://x/q"}`))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if string(res.ResponseFHIR) != string(pkg) {
		t.Errorf("ResponseFHIR = %s, want partner package verbatim", res.ResponseFHIR)
	}
	if !strings.Contains(string(p.lastBody), `"resourceType":"Parameters"`) {
		t.Errorf("forwarded body = %s, want Parameters", p.lastBody)
	}
}

func TestNativeResponder_PartnerNon2xxIs502(t *testing.T) {
	p := newStubPartner(t)
	p.status = 500
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)
	res, err := n.Handle(context.Background(), "coverage-eligibility", "corr", "pci",
		[]byte(`{"resourceType":"CoverageEligibilityRequest"}`))
	if err != nil {
		t.Fatalf("Handle returned error (want Status 502, not error): %v", err)
	}
	if res.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want 502", res.Status)
	}
}

func TestNativeResponder_DTRForwardsQuestionnaireLessPackageVerbatim(t *testing.T) {
	p := newStubPartner(t)
	pkg := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Library"}}]}`)
	p.respByPath["/Questionnaire/$questionnaire-package"] = pkg
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)
	res, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", []byte(`{"canonical":"http://x/q"}`))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != 0 {
		t.Errorf("Status = %d, want 0 (verbatim forward, no producer-side 502)", res.Status)
	}
	if string(res.ResponseFHIR) != string(pkg) {
		t.Errorf("ResponseFHIR = %s, want verbatim", res.ResponseFHIR)
	}
}

func TestNativeResponder_NilStoreOKForReadOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resourceType":"CoverageEligibilityResponse"}`))
	}))
	defer srv.Close()
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", nil, nil) // store=nil, clock=nil
	res, err := n.Handle(context.Background(), "coverage-eligibility", "corr-1", "PCI-1",
		[]byte(`{"resourceType":"CoverageEligibilityRequest"}`))
	if err != nil || res.Status != 0 {
		t.Fatalf("read-only leg with nil store must succeed: err=%v status=%d", err, res.Status)
	}
}

// TestNativeResponder_CRDNativeForwardsVerbatim proves crd-order-select-native forwards
// the conformant CDS Hooks request VERBATIM (no augmentCRDHook minimized re-shaping),
// then normalizes the partner response identically to the minimized CRD leg (FR-G25,
// rung-1 faithful pass-through).
func TestNativeResponder_CRDNativeForwardsVerbatim(t *testing.T) {
	p := newStubPartner(t)
	// The partner returns a split-shape coverage-information (same fixture as the minimized leg test).
	partnerCard := []byte(`{"cards":[{"suggestions":[{"actions":[{"resource":{"extension":[` +
		`{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information",` +
		`"extension":[{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"no-auth"}]}]}}]}]}]}`)
	p.respByPath["/cds-services/shn-order-select"] = partnerCard
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)

	// A conformant CDS Hooks request: hookInstance already present, draftOrders is a Bundle.
	conformant := []byte(`{"hook":"order-select","hookInstance":"hi-1","context":{"userId":"Practitioner/p1","patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"urn:uuid:sr1","resource":{"resourceType":"ServiceRequest","id":"sr1","subject":{"reference":"Patient/MBR-COVERED"}}}]},"selections":["ServiceRequest/sr1"]},"prefetch":{"coverage":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}}`)
	res, err := n.Handle(context.Background(), "crd-order-select-native", "corr", "pci", conformant)
	if err != nil {
		t.Fatalf("native conformant CRD: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("native conformant CRD: status=%d msg=%q", res.Status, res.Message)
	}
	// Response is normalized to canonical SHN cards (FR-G25), same as the minimized leg.
	if _, perr := shnsdk.ParseCards(res.ResponseFHIR); perr != nil {
		t.Fatalf("response not normalized to cards: %v", perr)
	}
	// Verbatim: the partner received the conformant Bundle draftOrders (NOT minimized shaping).
	// p.lastBody is the raw bytes the stub partner received.
	if !bytes.Contains(p.lastBody, []byte(`"resourceType":"Bundle"`)) {
		t.Fatalf("partner did not receive the conformant Bundle draftOrders verbatim: %s", p.lastBody)
	}
	// Verbatim also means hookInstance was NOT regenerated — the original "hi-1" survives.
	if !bytes.Contains(p.lastBody, []byte(`"hookInstance":"hi-1"`)) {
		t.Fatalf("partner did not receive the original hookInstance verbatim: %s", p.lastBody)
	}
	// Complement: the partner must NOT have received the MINIMIZED scalar draftOrders shape
	// (an array of bare resources, `"draftOrders":[{`) — only the conformant Bundle.
	if bytes.Contains(p.lastBody, []byte(`"draftOrders":[{`)) {
		t.Fatalf("partner received minimized scalar draftOrders — reshaping leaked: %s", p.lastBody)
	}
}

// TestNativeResponder_CRDNativeUnmappablePartnerIs502 is the per-leg fail-closed rejection row for
// the conformant leg: an unmappable partner CRD response (no resolvable coverage-information) → 502,
// never silent empty cards. The minimized leg has the same guard; this pins it for crd-order-select-
// native independently so a future de-sharing of normalizeCRDResponse cannot silently regress it.
func TestNativeResponder_CRDNativeUnmappablePartnerIs502(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/cds-services/shn-order-select"] = []byte(`{"cards":[{"summary":"x"}]}`)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)
	res, err := n.Handle(context.Background(), "crd-order-select-native", "corr", "pci",
		[]byte(`{"hook":"order-select","hookInstance":"hi-1","context":{"patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest"}}]}}}`))
	if err != nil {
		t.Fatalf("Handle returned error (want Status 502, not error): %v", err)
	}
	if res.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want 502 (un-mappable partner CRD card)", res.Status)
	}
}
