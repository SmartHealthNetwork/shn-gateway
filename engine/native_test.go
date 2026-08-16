package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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

// TestNativeResponder_PartnerNon2xxIsRelayedVerbatim supersedes the pre-relay
// TestNativeResponder_PartnerNon2xxIs502: post()/Handle() no longer collapse every
// upstream non-2xx to a generic 502 — an upstream that PRODUCED a response is a
// relayable answer, so its REAL status flows through verbatim (relay-recipient-response,
// 2026-07-15).
func TestNativeResponder_PartnerNon2xxIsRelayedVerbatim(t *testing.T) {
	p := newStubPartner(t)
	p.status = 500
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)
	res, err := n.Handle(context.Background(), "coverage-eligibility", "corr", "pci",
		[]byte(`{"resourceType":"CoverageEligibilityRequest"}`))
	if err != nil {
		t.Fatalf("Handle returned error (want a relayable Status, not error): %v", err)
	}
	if res.Status != http.StatusInternalServerError {
		t.Errorf("Status = %d, want 500 (the partner's real status, relayed verbatim)", res.Status)
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

// TestNativeResponder_DTRForwardsCoverageWhenCarried is the coverage-carry end-to-end leg guard
// (FR-G28): a dtr-questionnaire-fetch leg request carrying a Coverage resource must yield
// a forwarded $questionnaire-package body that INCLUDES a `coverage` parameter — a real
// Da Vinci payer (br-payer) 400s "The 'coverage' parameter is required (min=1)" otherwise.
// The leg request is the published shnsdk.QuestionnaireFetchRequest (canonical + optional
// coverage), so this also proves native.go reads the optional coverage off the wire.
func TestNativeResponder_DTRForwardsCoverageWhenCarried(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/Questionnaire/$questionnaire-package"] =
		[]byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","url":"http://x/q"}}]}`)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)

	coverage := json.RawMessage(`{"resourceType":"Coverage","id":"cov-1","status":"active","beneficiary":{"reference":"Patient/p1"}}`)
	reqFHIR, err := json.Marshal(shnsdk.QuestionnaireFetchRequest{Canonical: "http://x/q", Coverage: coverage})
	if err != nil {
		t.Fatalf("marshal fetch request: %v", err)
	}
	if _, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", reqFHIR); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var got struct {
		Parameter []struct {
			Name     string          `json:"name"`
			Resource json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if err := json.Unmarshal(p.lastBody, &got); err != nil {
		t.Fatalf("forwarded body not Parameters: %v (%s)", err, p.lastBody)
	}
	var covParam json.RawMessage
	for _, pr := range got.Parameter {
		if pr.Name == "coverage" {
			covParam = pr.Resource
		}
	}
	if covParam == nil {
		t.Fatalf("forwarded $questionnaire-package missing coverage parameter (payer would 400): %s", p.lastBody)
	}
	if !bytes.Contains(covParam, []byte(`"resourceType":"Coverage"`)) ||
		!bytes.Contains(covParam, []byte(`"id":"cov-1"`)) {
		t.Errorf("coverage parameter resource not the carried Coverage: %s", covParam)
	}
}

// TestNativeResponder_DTRForwardsOrderWhenCarried proves the order-driven DTR path (the external-payer
// lane): a dtr-questionnaire-fetch leg request carrying an `order` (the CRD-updated ServiceRequest
// with its coverage-assertion-id) yields a forwarded $questionnaire-package with an `order`
// parameter (NOT `questionnaire`) plus the carried `coverage` — that payer 501s "ServiceRequest
// without a Coverage Assertion Id extension is not supported" / 500s without both.
func TestNativeResponder_DTRForwardsOrderWhenCarried(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/Questionnaire/$questionnaire-package"] =
		[]byte(`{"resourceType":"Parameters","parameter":[{"name":"packagebundle","resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","url":"http://x/q"}}]}}]}`)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)

	order := `{"resourceType":"ServiceRequest","id":"sr-81162","status":"draft","intent":"order","extension":[{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information","extension":[{"url":"coverage-assertion-id","valueString":"assert-1"}]}]}`
	coverage := `{"resourceType":"Coverage","id":"cov-1","status":"active","beneficiary":{"reference":"Patient/p1"}}`
	reqFHIR := []byte(`{"coverage":` + coverage + `,"order":` + order + `}`)
	if _, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", reqFHIR); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var got struct {
		Parameter []struct {
			Name     string          `json:"name"`
			Resource json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if err := json.Unmarshal(p.lastBody, &got); err != nil {
		t.Fatalf("forwarded body not Parameters: %v (%s)", err, p.lastBody)
	}
	names := map[string]json.RawMessage{}
	for _, pr := range got.Parameter {
		names[pr.Name] = pr.Resource
	}
	if _, ok := names["questionnaire"]; ok {
		t.Errorf("order-driven DTR must NOT send a questionnaire parameter: %s", p.lastBody)
	}
	if _, ok := names["order"]; !ok {
		t.Fatalf("forwarded $questionnaire-package missing the order parameter: %s", p.lastBody)
	}
	if !bytes.Contains(names["order"], []byte(`"coverage-assertion-id"`)) {
		t.Errorf("order parameter dropped the coverage-assertion-id extension: %s", names["order"])
	}
	if _, ok := names["coverage"]; !ok {
		t.Errorf("order-driven DTR must still carry the coverage parameter: %s", p.lastBody)
	}
}

// TestNativeResponder_CRDMergesSystemActions proves the external-payer-lane CRD passthrough: with
// WithCRDCoverageBundle on, the partner's CRD systemActions (the coverage-annotated order the
// provider needs to drive DTR) are relayed alongside the normalized SHN cards; with it OFF the
// response is cards-only (br-payer byte-unchanged).
func TestNativeResponder_CRDMergesSystemActions(t *testing.T) {
	partnerCRD := []byte(`{"cards":[],"systemActions":[{"type":"update","resource":{"resourceType":"ServiceRequest","id":"sr-81162","extension":[{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information","extension":[{"url":"coverage-assertion-id","valueString":"assert-1"},{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"auth-needed"}]}]}}]}`)
	run := func(t *testing.T, bundle bool) []byte {
		p := newStubPartner(t)
		p.respByPath["/cds-services/order-sign"] = partnerCRD
		opts := []NativeOption{}
		if bundle {
			opts = append(opts, WithCRDCoverageBundle(true))
		}
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "order-sign", nil, nil, opts...)
		req := []byte(`{"hook":"order-sign","context":{"draftOrders":{"resourceType":"Bundle","entry":[]}},"prefetch":{"coverage":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/p1"}}}}`)
		res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", req)
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		return res.ResponseFHIR
	}
	on := run(t, true)
	if !bytes.Contains(on, []byte(`"systemActions"`)) || !bytes.Contains(on, []byte(`"coverage-assertion-id"`)) {
		t.Fatalf("external-payer-lane CRD response must relay systemActions with the annotated order: %s", on)
	}
	if !bytes.Contains(on, []byte(`"cards"`)) {
		t.Fatalf("CRD response must still carry the SHN cards: %s", on)
	}
	off := run(t, false)
	if bytes.Contains(off, []byte(`"systemActions"`)) {
		t.Fatalf("flag OFF (br-payer) must be cards-only, no systemActions: %s", off)
	}
}

// TestNativeResponder_DTRRejectsMalformedFetch locks the fail-closed posture preserved
// across the coverage-carry switch from jsonUnmarshalStrictCanonical to unmarshaling the published
// QuestionnaireFetchRequest: a malformed body OR a missing/empty canonical → 400 (parity
// with the sandbox's 400, never a 500), and the partner is never called.
func TestNativeResponder_DTRRejectsMalformedFetch(t *testing.T) {
	for name, body := range map[string]string{
		"not-json":          `{not json`,
		"missing-canonical": `{"coverage":{"resourceType":"Coverage"}}`,
		"empty-canonical":   `{"canonical":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := newStubPartner(t)
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)
			res, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", []byte(body))
			if err != nil {
				t.Fatalf("Handle returned error (want Status 400, not error): %v", err)
			}
			if res.Status != http.StatusBadRequest {
				t.Errorf("Status = %d, want 400", res.Status)
			}
			if p.lastBody != nil {
				t.Errorf("partner was called on a malformed fetch: %s", p.lastBody)
			}
		})
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

// TestNativeResponder_CRDNativeForwardsVerbatim proves crd-order-select forwards
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
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", conformant)
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
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci",
		[]byte(`{"hook":"order-select","hookInstance":"hi-1","context":{"patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest"}}]}}}`))
	if err != nil {
		t.Fatalf("Handle returned error (want Status 502, not error): %v", err)
	}
	if res.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want 502 (un-mappable partner CRD card)", res.Status)
	}
}

// TestNativeResponder_RewritesCRDHook proves the native-forward rewrites the request
// hook to the configured CRD service's hook before forwarding — SHN originates
// hook:order-select but br-payer's order-sign-crd demands hook:order-sign (400 otherwise).
func TestNativeResponder_RewritesCRDHook(t *testing.T) {
	var gotHook string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Hook string `json:"hook"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotHook = body.Hook
		// minimal valid cards response so normalizeCRDResponse succeeds
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cards":[],"systemActions":[]}`))
	}))
	defer srv.Close()

	n := NewNativeResponder(srv.Client(), srv.URL, "order-sign-crd", nil, nil, WithCRDHook("order-sign"))
	reqJSON := []byte(`{"hook":"order-select","hookInstance":"hi","context":{"draftOrders":{"resourceType":"Bundle","entry":[]}},"prefetch":{}}`)
	if _, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", reqJSON); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gotHook != "order-sign" {
		t.Fatalf("forwarded hook = %q, want order-sign (rewritten)", gotHook)
	}
}

// TestNativeResponder_WrapsCRDCoverageBundle proves WithCRDCoverageBundle rewrites the
// CRD request's bare prefetch.coverage into a searchset Bundle on egress — that payer's
// order-sign `coverage` prefetch is a SEARCH template (Coverage?beneficiary=…) demanding a
// searchset Bundle (bare Coverage → 412 "Missing Coverage"), while the SHN spine carries a
// BARE Coverage (provider routing + the payer-side bind both read bare, crd_native.go). The
// wrap runs AFTER the bind, gated peer-scoped so br-payer conformance is untouched.
func TestNativeResponder_WrapsCRDCoverageBundle(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/cds-services/order-sign"] = []byte(`{"cards":[],"systemActions":[]}`)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "order-sign", nil, nil, WithCRDCoverageBundle(true))
	reqJSON := []byte(`{"hook":"order-sign","context":{"userId":"Practitioner/p1","draftOrders":{"resourceType":"Bundle","entry":[]}},` +
		`"prefetch":{"patient":{"resourceType":"Patient","id":"MBR"},"coverage":{"resourceType":"Coverage","id":"cov","beneficiary":{"reference":"Patient/MBR"}}}}`)
	if _, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", reqJSON); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var fwd struct {
		Prefetch struct {
			Coverage struct {
				ResourceType string `json:"resourceType"`
				Type         string `json:"type"`
				Entry        []struct {
					Resource struct {
						ResourceType string `json:"resourceType"`
						ID           string `json:"id"`
					} `json:"resource"`
				} `json:"entry"`
			} `json:"coverage"`
			Patient struct {
				ResourceType string `json:"resourceType"`
			} `json:"patient"`
		} `json:"prefetch"`
	}
	if err := json.Unmarshal(p.lastBody, &fwd); err != nil {
		t.Fatalf("parse forwarded body: %v (%s)", err, p.lastBody)
	}
	cov := fwd.Prefetch.Coverage
	if cov.ResourceType != "Bundle" || cov.Type != "searchset" {
		t.Fatalf("forwarded prefetch.coverage = %s/%s, want Bundle/searchset: %s", cov.ResourceType, cov.Type, p.lastBody)
	}
	if len(cov.Entry) != 1 || cov.Entry[0].Resource.ResourceType != "Coverage" || cov.Entry[0].Resource.ID != "cov" {
		t.Fatalf("searchset must wrap the bare Coverage verbatim: %s", p.lastBody)
	}
	if fwd.Prefetch.Patient.ResourceType != "Patient" {
		t.Fatalf("prefetch.patient dropped on wrap: %s", p.lastBody)
	}
}

// TestNativeResponder_CoverageBundleScopedAndIdempotent proves the wrap is scoped and safe:
// with the option OFF a bare Coverage forwards verbatim (br-payer's shape, untouched); with it
// ON an already-searchset coverage is left as-is (idempotent — never a Bundle-in-a-Bundle).
func TestNativeResponder_CoverageBundleScopedAndIdempotent(t *testing.T) {
	mkReq := func(covJSON string) []byte {
		return []byte(`{"hook":"order-sign","context":{"draftOrders":{"resourceType":"Bundle","entry":[]}},"prefetch":{` + covJSON + `}}`)
	}
	bareCov := `"coverage":{"resourceType":"Coverage","id":"cov","beneficiary":{"reference":"Patient/MBR"}}`

	// OFF: bare stays bare (no wrap without the peer-scoped option).
	pOff := newStubPartner(t)
	pOff.respByPath["/cds-services/order-sign"] = []byte(`{"cards":[],"systemActions":[]}`)
	nOff := NewNativeResponder(pOff.srv.Client(), pOff.srv.URL, "order-sign", nil, nil)
	if _, err := nOff.Handle(context.Background(), "crd-order-select", "c", "p", mkReq(bareCov)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(pOff.lastBody, []byte(`"searchset"`)) {
		t.Fatalf("option OFF must forward bare coverage verbatim: %s", pOff.lastBody)
	}

	// ON + already a searchset: not re-wrapped (exactly one searchset wrapper survives).
	pOn := newStubPartner(t)
	pOn.respByPath["/cds-services/order-sign"] = []byte(`{"cards":[],"systemActions":[]}`)
	nOn := NewNativeResponder(pOn.srv.Client(), pOn.srv.URL, "order-sign", nil, nil, WithCRDCoverageBundle(true))
	alreadyBundle := `"coverage":{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Coverage","id":"cov"}}]}`
	if _, err := nOn.Handle(context.Background(), "crd-order-select", "c", "p", mkReq(alreadyBundle)); err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(pOn.lastBody, []byte(`"searchset"`)); got != 1 {
		t.Fatalf("already-searchset coverage must not be re-wrapped (searchset count=%d): %s", got, pOn.lastBody)
	}
}

// TestNativeResponder_CRDDispatchForwardsVerbatim proves crd-order-dispatch forwards
// the verbatim CDS Hooks request to the partner's dispatch CDS service, preserving
// the order-dispatch hook + the dispatchedOrders + performer fields.
func TestNativeResponder_CRDDispatchForwardsVerbatim(t *testing.T) {
	p := newStubPartner(t)
	partnerCard := []byte(`{"cards":[{"suggestions":[{"actions":[{"resource":{"extension":[` +
		`{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information",` +
		`"extension":[{"url":"covered","valueCode":"conditional"},{"url":"pa-needed","valueCode":"auth-needed"}]}]}}]}]}]}`)
	p.respByPath["/cds-services/order-dispatch-crd"] = partnerCard
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil,
		WithCRDDispatchService("order-dispatch-crd", "order-dispatch"))
	req := []byte(`{"hook":"order-dispatch","context":{"patientId":"MBR-OX","dispatchedOrders":["DeviceRequest/dr1"],"performer":"Organization/dme1"},"prefetch":{}}`)
	if _, err := n.Handle(context.Background(), "crd-order-dispatch", "corr", "pci", req); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(p.lastBody, []byte(`"dispatchedOrders"`)) || !bytes.Contains(p.lastBody, []byte(`"performer"`)) {
		t.Fatalf("dispatch context dropped on forward: %s", p.lastBody)
	}
	if !bytes.Contains(p.lastBody, []byte(`"hook":"order-dispatch"`)) {
		t.Fatalf("hook not preserved/rewritten: %s", p.lastBody)
	}
}

// TestNativeForwardVersionFilter: the operator-declared foreign-peer token set
// (PAYER_DAVINCI_CONTRACT_VERSIONS) gates forwarding exactly like a registry
// declaration gates substrate routing ("foreign endpoints
// route by the same filter"): no shared line → legible 422 LegResult, ZERO
// bytes forwarded; shared or silent → forward as before.
func TestNativeForwardVersionFilter(t *testing.T) {
	hits := 0
	partnerCard := []byte(`{"cards":[{"suggestions":[{"actions":[{"resource":{"extension":[` +
		`{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information",` +
		`"extension":[{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"no-auth"}]}]}}]}]}]}`)
	partner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(partnerCard)
	}))
	defer partner.Close()

	// Declared 2.2-only: the CRD leg (pa.crd, own 2.0) refuses without forwarding.
	n := NewNativeResponder(partner.Client(), partner.URL, "svc", nil, nil,
		WithDeclaredContractVersions([]string{"pa.crd@2.2", "pa.crd@2.2"})) // duplicate on purpose
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", []byte(`{}`))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("Status = %d, want 422", res.Status)
	}
	for _, must := range []string{"pa.crd", "pa.crd@2.0", "pa.crd@2.2"} {
		if !strings.Contains(res.Message, must) {
			t.Fatalf("message %q missing %q", res.Message, must)
		}
	}
	if hits != 0 {
		t.Fatal("refused leg must not touch the partner endpoint")
	}

	// Silent (no declaration): forwards.
	n2 := NewNativeResponder(partner.Client(), partner.URL, "svc", nil, nil)
	if res, err := n2.Handle(context.Background(), "crd-order-select", "corr", "pci", []byte(`{}`)); err != nil || res.Status == http.StatusUnprocessableEntity {
		t.Fatalf("silent peer must forward: %+v / %v", res, err)
	}
	if hits != 1 {
		t.Fatalf("silent peer must forward exactly once: hits=%d", hits)
	}
}

// TestNativeForward_NeverCallsEgressAdapt is the EXPLICIT structural pin
// backing TestNativeForwardStaysArm1's "no transformation" claim:
// nativeResponder has no Observer seam and Handle never references
// the transform machinery at all — grepped here, not merely inferred from the
// byte-identical-forward assertion, which would hold even if a call existed
// but happened to be a no-op for THIS fixture's inputs. If native.go ever
// grows an egressAdapt call (the transform-at-the-forward-edge deferral going
// live — the strictExtensions flag goes live together with it), this guard
// fails LOUDLY and both this test and TestNativeForwardStaysArm1 need a
// deliberate update, not a silent pass.
func TestNativeForward_NeverCallsEgressAdapt(t *testing.T) {
	src, err := os.ReadFile("native.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	for _, forbidden := range []string{"egressAdapt", "legTransformedKind", "transformedObserverEvent"} {
		if strings.Contains(string(src), forbidden) {
			t.Fatalf("native.go now references %q — the native-forward arm-1-only pin appears to "+
				"have been widened; this is only safe as the deliberate transform-at-the-forward-edge deferral "+
				"going live (update this guard + TestNativeForwardStaysArm1 together), never an accidental slip", forbidden)
		}
	}
}

// TestNativeForwardStaysArm1 pins the native-forward row of the caller×arm
// matrix: native.go's
// Handle stays ARM-1-ONLY (selectContractToken, intersection-only) — a peer
// that declares a line THIS build could reach via native-reach or a
// transform chain (2.2 is native+laned in general) must still refuse rather
// than forward re-labeled/transformed bytes: the forwarded body is
// PROVIDER-BUILT, not this gateway's own build product, so re-labeling or
// chaining it is out of scope (transform-at-the-forward-edge, deferred).
// The accepted (shared-line) case forwards VERBATIM, byte-identical, with no
// transformation. "No leg.transformed was observed" is not separately
// assertable here (nativeResponder has no Observer seam to hook — see
// TestNativeForward_NeverCallsEgressAdapt for the explicit structural proof
// that backs this claim instead of leaving it implicit).
func TestNativeForwardStaysArm1(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/cds-services/svc"] = []byte(`{"cards":[]}`)

	// No shared declared line (own defaults to pa.crd@2.0; peer declares
	// pa.crd@2.2 only) — 2.2 IS this build's native set, an arm-2/3-worthy
	// peer. Handle must refuse, never native-reach or chain.
	nRefuse := NewNativeResponder(p.srv.Client(), p.srv.URL, "svc", nil, nil,
		WithDeclaredContractVersions([]string{"pa.crd@2.2"}))
	req := []byte(`{"hook":"order-select"}`)
	res, err := nRefuse.Handle(context.Background(), "crd-order-select", "corr-refuse", "pci", req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("Status = %d, want 422 (native reach/chain must never rescue the forward edge)", res.Status)
	}
	if p.lastBody != nil {
		t.Fatalf("refused leg forwarded %d bytes to the partner — refuse-before-forward violated", len(p.lastBody))
	}

	// Shared declared line: forwards VERBATIM, byte-identical, no
	// leg.transformed-style processing exists on this path at all.
	nAccept := NewNativeResponder(p.srv.Client(), p.srv.URL, "svc", nil, nil,
		WithDeclaredContractVersions([]string{"pa.crd@2.0"}))
	if _, err := nAccept.Handle(context.Background(), "crd-order-select", "corr-accept", "pci", req); err != nil {
		t.Fatalf("Handle (shared line): %v", err)
	}
	if !bytes.Equal(p.lastBody, req) {
		t.Fatalf("forwarded body = %s, want byte-identical to the request %s", p.lastBody, req)
	}
}

// TestNativeResponder_SplitBaseURLs proves CRD (CDS Hooks) posts to the CDS base
// while DTR/PAS post to the FHIR base — the br-payer topology (CDS at root, FHIR
// under /fhir). Two httptest servers stand in for the two bases.
func TestNativeResponder_SplitBaseURLs(t *testing.T) {
	var cdsPath, fhirPath string
	cds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdsPath = r.URL.Path
		w.Write([]byte(`{"cards":[],"systemActions":[]}`))
	}))
	defer cds.Close()
	fhir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fhirPath = r.URL.Path
		w.Write([]byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`))
	}))
	defer fhir.Close()

	n := NewNativeResponder(fhir.Client(), fhir.URL, "order-sign-crd", nil, nil, WithCDSBaseURL(cds.URL))
	// CRD → CDS base
	_, _ = n.Handle(context.Background(), "crd-order-select", "c", "p",
		[]byte(`{"hook":"order-sign","context":{"patientId":"x"}}`))
	if cdsPath != "/cds-services/order-sign-crd" {
		t.Errorf("CRD path on CDS server = %q, want /cds-services/order-sign-crd", cdsPath)
	}
	// DTR → FHIR base
	_, _ = n.Handle(context.Background(), "dtr-questionnaire-fetch", "c", "p",
		[]byte(`{"canonical":"http://x/Questionnaire/Q"}`))
	if fhirPath != "/Questionnaire/$questionnaire-package" {
		t.Errorf("DTR path on FHIR server = %q, want /Questionnaire/$questionnaire-package", fhirPath)
	}
}

// dtrFetchReq is a working dtr-questionnaire-fetch leg body that clears
// EVERY line's build gate, including 2.2's coverage-1..1 requirement
// (DTRDef.QuestionnairePackageCoverageRequired — see
// TestNativeResponder_DTRForwardsCoverageWhenCarried's precedent) — the
// endpoint-evidence tests below route legs at specific lines via withAnswerLine, so the
// fixture must not 400 regardless of which line gets picked.
var dtrFetchReq = []byte(`{"canonical":"http://x/q","coverage":{"resourceType":"Coverage","id":"cov-1","status":"active","beneficiary":{"reference":"Patient/p1"}}}`)

// TestNativeForwardSelectsLineEndpoint: the
// per-line endpoint resolution before n.post. Evidence present AND
// token-matched to the routed line -> the #<line> endpoint is used; evidence
// absent, or present for a DIFFERENT token, both fall back to the configured
// base+path UNCHANGED (the fence — never a partial/wrong-token match).
func TestNativeForwardSelectsLineEndpoint(t *testing.T) {
	p := newStubPartner(t)
	pkg := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Library"}}]}`)
	p.respByPath["/Questionnaire/$questionnaire-package"] = pkg
	p.respByPath["/Questionnaire/$questionnaire-package-v22"] = pkg
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)

	t.Run("evidence token-matched to the routed line: the #<line> endpoint is used", func(t *testing.T) {
		n.SetEndpointEvidence(map[string]string{"pa.dtr@2.2": p.srv.URL + "/Questionnaire/$questionnaire-package-v22"})
		ctx := withAnswerLine(context.Background(), "pa.dtr@2.2")
		res, err := n.Handle(ctx, "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if res.Status != 0 {
			t.Fatalf("Status = %d, want 0", res.Status)
		}
		if p.lastPath != "/Questionnaire/$questionnaire-package-v22" {
			t.Fatalf("lastPath = %q, want the #2.2 evidence endpoint", p.lastPath)
		}
	})

	t.Run("evidence absent: byte-identical fallback to the configured base+path", func(t *testing.T) {
		n.SetEndpointEvidence(nil)
		ctx := withAnswerLine(context.Background(), "pa.dtr@2.1")
		res, err := n.Handle(ctx, "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if res.Status != 0 {
			t.Fatalf("Status = %d, want 0", res.Status)
		}
		if p.lastPath != "/Questionnaire/$questionnaire-package" {
			t.Fatalf("lastPath = %q, want the configured base+path (no evidence at all)", p.lastPath)
		}
	})

	t.Run("token-mismatch rejection: evidence for a DIFFERENT line is never selected", func(t *testing.T) {
		n.SetEndpointEvidence(map[string]string{"pa.dtr@2.1": p.srv.URL + "/Questionnaire/$questionnaire-package-v22"})
		ctx := withAnswerLine(context.Background(), "pa.dtr@2.2") // routed at 2.2; evidence is keyed 2.1
		res, err := n.Handle(ctx, "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if res.Status != 0 {
			t.Fatalf("Status = %d, want 0", res.Status)
		}
		if p.lastPath != "/Questionnaire/$questionnaire-package" {
			t.Fatalf("lastPath = %q, want the configured base+path — a mismatched token must never be selected", p.lastPath)
		}
	})
}

// TestEndpointEvidenceSameOriginEnforced (the endpoint-evidence trust
// rejection test): a cross-origin evidence entry is DROPPED AT SET TIME — it never
// reaches endpointEvidence, is noted via endpointEvidenceObserver, and is
// NEVER selected on a subsequent Handle — a probe-published foreign origin
// must never become the target of a PHI-bearing submission.
func TestEndpointEvidenceSameOriginEnforced(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)

	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("cross-origin evidence endpoint was called: %s %s (must never be selected)", r.Method, r.URL.Path)
	}))
	defer evil.Close()

	var notes []string
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil,
		WithEndpointEvidenceObserver(func(note string) { notes = append(notes, note) }))

	n.SetEndpointEvidence(map[string]string{"pa.dtr@2.2": evil.URL + "/Questionnaire/$questionnaire-package"})
	if len(notes) != 1 {
		t.Fatalf("want exactly one drop note, got %d: %v", len(notes), notes)
	}

	ctx := withAnswerLine(context.Background(), "pa.dtr@2.2")
	res, err := n.Handle(ctx, "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("Status = %d, want 0", res.Status)
	}
	if p.lastPath != "/Questionnaire/$questionnaire-package" {
		t.Fatalf("lastPath = %q, want the configured base — the cross-origin entry must never be selected", p.lastPath)
	}
}

// TestOriginOfDefaultPortNormalization: exact string equality on
// "scheme://host[:port]" wrongly treats
// "https://x" and "https://x:443" as cross-origin — a common config shape
// (operator base without a port, partner-published URL with an explicit
// default port, or vice versa), not an attack. Both directions must
// normalize to the SAME origin; a genuine cross-origin (different host, or a
// different NON-default port) must still compare unequal.
func TestOriginOfDefaultPortNormalization(t *testing.T) {
	cases := []struct {
		name      string
		a, b      string
		wantEqual bool
	}{
		{"https, base bare / evidence :443", "https://payer.example", "https://payer.example:443", true},
		{"https, base :443 / evidence bare", "https://payer.example:443", "https://payer.example", true},
		{"http, base bare / evidence :80", "http://payer.example", "http://payer.example:80", true},
		{"http, base :80 / evidence bare", "http://payer.example:80", "http://payer.example", true},
		{"genuine cross-origin: different host", "https://payer.example", "https://evil.example", false},
		{"genuine cross-origin: different non-default port", "https://payer.example:8443", "https://payer.example", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oa, oka := originOf(tc.a)
			ob, okb := originOf(tc.b)
			if !oka || !okb {
				t.Fatalf("originOf(%q)=%v,%v originOf(%q)=%v,%v — both must parse", tc.a, oa, oka, tc.b, ob, okb)
			}
			if (oa == ob) != tc.wantEqual {
				t.Fatalf("originOf(%q)=%q originOf(%q)=%q — want equal=%v", tc.a, oa, tc.b, ob, tc.wantEqual)
			}
		})
	}
}

// TestEndpointEvidenceSameOriginDefaultPortNormalized: the
// SetEndpointEvidence-level proof, not just the originOf
// unit — a default-port-only mismatch is HONORED, a genuine cross-origin
// entry is still DROPPED, and a malformed/unparseable evidence URL is
// dropped too (its own assertion, reviewer minor).
func TestEndpointEvidenceSameOriginDefaultPortNormalized(t *testing.T) {
	n := NewNativeResponder(http.DefaultClient, "https://payer.example", "svc", nil, nil)

	t.Run("base without a port, evidence with the scheme's default :443 -> HONORED", func(t *testing.T) {
		n.SetEndpointEvidence(map[string]string{"pa.pas@2.2": "https://payer.example:443/Claim/$submit"})
		got := n.EndpointEvidenceForTest()
		if got["pa.pas@2.2"] != "https://payer.example:443/Claim/$submit" {
			t.Fatalf("evidence = %v, want the default-port entry honored", got)
		}
	})

	t.Run("genuine cross-origin (different host) is still DROPPED", func(t *testing.T) {
		n.SetEndpointEvidence(map[string]string{"pa.pas@2.2": "https://evil.example/Claim/$submit"})
		got := n.EndpointEvidenceForTest()
		if _, ok := got["pa.pas@2.2"]; ok {
			t.Fatalf("evidence = %v, want the cross-origin entry dropped", got)
		}
	})

	t.Run("malformed/unparseable evidence URL is dropped", func(t *testing.T) {
		var notes []string
		n2 := NewNativeResponder(http.DefaultClient, "https://payer.example", "svc", nil, nil,
			WithEndpointEvidenceObserver(func(note string) { notes = append(notes, note) }))
		n2.SetEndpointEvidence(map[string]string{"pa.pas@2.2": "://not-a-url"})
		got := n2.EndpointEvidenceForTest()
		if _, ok := got["pa.pas@2.2"]; ok {
			t.Fatalf("evidence = %v, want the malformed entry dropped", got)
		}
		if len(notes) != 1 {
			t.Fatalf("want exactly one drop note for the malformed entry, got %d: %v", len(notes), notes)
		}
	})
}

// TestEndpointEvidenceRaceClean: SetEndpointEvidence (writer, wholesale
// replace under Lock) racing against Handle's resolvedURL reads (RLock) must
// be -race clean.
func TestEndpointEvidenceRaceClean(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			n.SetEndpointEvidence(map[string]string{"pa.dtr@2.2": p.srv.URL + "/Questionnaire/$questionnaire-package"})
		}
	}()

	ctx := withAnswerLine(context.Background(), "pa.dtr@2.2")
	for i := 0; i < 200; i++ {
		if _, err := n.Handle(ctx, "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestNativeStrictExtensionsFieldIsDormant (FR-G52): WithStrictExtensions
// produces ZERO behavior delta on the native-forward path today — no
// Handle-filter consult exists. Byte-identical fence: two otherwise-identical
// responders, one with the flag on, answer identically.
func TestNativeStrictExtensionsFieldIsDormant(t *testing.T) {
	mk := func(strict bool) (*nativeResponder, *stubPartner) {
		p := newStubPartner(t)
		p.respByPath["/CoverageEligibilityRequest"] = []byte(`{"resourceType":"CoverageEligibilityResponse","patient":{"reference":"Patient/p1"}}`)
		var opts []NativeOption
		if strict {
			opts = append(opts, WithStrictExtensions(true))
		}
		return NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil, opts...), p
	}
	off, _ := mk(false)
	on, _ := mk(true)
	req := []byte(`{"resourceType":"CoverageEligibilityRequest","patient":{"reference":"Patient/p1"}}`)

	resOff, errOff := off.Handle(context.Background(), "coverage-eligibility", "corr", "pci", req)
	resOn, errOn := on.Handle(context.Background(), "coverage-eligibility", "corr", "pci", req)
	if errOff != nil || errOn != nil {
		t.Fatalf("Handle errors: strict=false -> %v, strict=true -> %v", errOff, errOn)
	}
	if resOff.Status != resOn.Status || string(resOff.ResponseFHIR) != string(resOn.ResponseFHIR) {
		t.Fatalf("WithStrictExtensions must be dormant (byte-identical): off=%+v on=%+v", resOff, resOn)
	}
}
