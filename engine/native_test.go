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
// relayable answer, so its REAL status flows through verbatim.
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
// parameter (NOT `questionnaire`) plus the carried `coverage` — the external payer 501s "ServiceRequest
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
// CRD request's bare prefetch.coverage into a searchset Bundle on egress — the external payer's
// order-sign `coverage` prefetch is a SEARCH template (Coverage?beneficiary=…) demanding a
// searchset Bundle (bare Coverage → 412 "Missing Coverage"), while the SHN spine carries a
// BARE Coverage (provider routing + the payer-side bind both read bare, crd_native.go). The
// wrap runs AFTER the bind, gated behind the option so br-payer conformance is untouched.
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

	// OFF: bare stays bare (no wrap without the option).
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
