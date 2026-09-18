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

// stubCDSServices is the stub partner's CDS service listing.
var stubCDSServices = []CDSService{
	{ID: "shn-order-select", Hook: "order-select"},
	{ID: "svc", Hook: "order-select"},
	{ID: "order-sign", Hook: "order-sign"},
	{ID: "order-sign-crd", Hook: "order-sign"},
	{ID: "order-dispatch-crd", Hook: "order-dispatch"},
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
		if r.Method == http.MethodGet && r.URL.Path == "/cds-services" {
			// The CDS service listing the CRD legs read; the tests name the service.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"services": stubCDSServices})
			return
		}
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
	if string(responseBytes(res)) != string(pkg) {
		t.Errorf("Response = %s, want partner package verbatim", responseBytes(res))
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
	// Driven on the DTR leg: it is a read-only forward like the retired eligibility arm
	// (§3.2 deleted that arm — eligibility never reaches a responder any more), and the
	// non-2xx relay under test is leg-independent post() behaviour.
	res, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci",
		[]byte(`{"canonical":"http://x/q"}`))
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
	if string(responseBytes(res)) != string(pkg) {
		t.Errorf("Response = %s, want verbatim", responseBytes(res))
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

// TestNativeResponder_DTRRejectsMalformedFetch locks the fail-closed posture preserved
// across the coverage-carry switch from jsonUnmarshalStrictCanonical to unmarshaling the published
// QuestionnaireFetchRequest: a malformed body OR a missing/empty canonical → 400 (parity
// with a malformed-request 400, never a 500), and the partner is never called.
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
		_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`))
	}))
	defer srv.Close()
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", nil, nil) // store=nil, clock=nil
	// DTR is the read-only leg here: the eligibility arm this row used to drive was
	// deleted with the split counterparty (§3.2 — eligibility is engine-side, R11).
	res, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr-1", "PCI-1",
		[]byte(`{"canonical":"http://x/q"}`))
	if err != nil || res.Status != 0 {
		t.Fatalf("read-only leg with nil store must succeed: err=%v status=%d", err, res.Status)
	}
}

// TestNativeForwardVersionFilter: the operator-declared foreign-peer token set
// (PAYER_DAVINCI_CONTRACT_VERSIONS) gates forwarding exactly like a registry
// declaration gates substrate routing ("foreign endpoints
// route by the same filter"): no shared line → legible 422 LegResult, ZERO
// bytes forwarded; shared or silent → forward as before.
func TestNativeForwardVersionFilter(t *testing.T) {
	hits := 0
	partner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"services": stubCDSServices})
			return
		}
		hits++
		_, _ = w.Write(crdPartnerCoverageCard)
	}))
	defer partner.Close()

	// Declared 2.2-only: the CRD leg (pa.crd, own 2.0) refuses without forwarding.
	n := NewNativeResponder(partner.Client(), partner.URL, "svc", nil, nil,
		WithDeclaredContractVersions([]string{"pa.crd@2.2", "pa.crd@2.2"})) // duplicate on purpose
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", cdsRequest("order-select"))
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
	if res, err := n2.Handle(context.Background(), "crd-order-select", "corr", "pci", cdsRequest("order-select")); err != nil || res.Status != 0 {
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
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"services": stubCDSServices})
			return
		}
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
		p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"resource":{"resourceType":"Questionnaire","id":"q1","url":"http://x/q"}}]}`)
		var opts []NativeOption
		if strict {
			opts = append(opts, WithStrictExtensions(true))
		}
		return NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil, opts...), p
	}
	off, _ := mk(false)
	on, _ := mk(true)
	// Driven on DTR: the eligibility leg this row used to drive is gone from the native
	// responder (§3.2). The dormancy claim is leg-independent — no Handle arm consults
	// the flag — so any forwarded leg is a faithful witness.
	req := []byte(`{"canonical":"http://x/q"}`)

	resOff, errOff := off.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", req)
	resOn, errOn := on.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", req)
	if errOff != nil || errOn != nil {
		t.Fatalf("Handle errors: strict=false -> %v, strict=true -> %v", errOff, errOn)
	}
	if resOff.Status != resOn.Status || string(responseBytes(resOff)) != string(responseBytes(resOn)) {
		t.Fatalf("WithStrictExtensions must be dormant (byte-identical): off=%+v on=%+v", resOff, resOn)
	}
}

// TestNativeResponder_PerOperationBases: a partner that serves CRD,
// DTR and PAS from three different bases is addressed natively — CRD posts
// to the CDS base, DTR to the DTR base, PAS to the PAS base — and a partner
// that sets neither per-operation base keeps every FHIR operation on the
// shared base (the byte-identical fallback every existing deployment relies
// on). Four httptest servers stand in for the four bases.
func TestNativeResponder_PerOperationBases(t *testing.T) {
	newBase := func(t *testing.T, body string) (*httptest.Server, *string) {
		t.Helper()
		var path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/cds-services" {
				_ = json.NewEncoder(w).Encode(map[string]any{"services": stubCDSServices})
				return
			}
			path = r.URL.Path
			w.Header().Set("Content-Type", "application/fhir+json")
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv, &path
	}
	const pkg = `{"resourceType":"Bundle","type":"collection","entry":[]}`
	conformant := originatorBuiltConformantBundle(t, "MBR-COVERED")
	pasAnswer := string(fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"REF-1","preAuthPeriod":{"end":"2030-01-01"}}`), true))

	t.Run("three bases: each operation lands on its own", func(t *testing.T) {
		shared, sharedPath := newBase(t, pkg)
		cds, cdsPath := newBase(t, `{"cards":[],"systemActions":[]}`)
		dtr, dtrPath := newBase(t, pkg)
		pas, pasPath := newBase(t, pasAnswer)
		n := NewNativeResponder(shared.Client(), shared.URL, "order-sign-crd", newCensusSoR(), fixedClock,
			WithCDSBaseURL(cds.URL), WithDTRBaseURL(dtr.URL), WithPASBaseURL(pas.URL))

		_, _ = n.Handle(context.Background(), "crd-order-select", "c", "p",
			[]byte(`{"hook":"order-sign","context":{"patientId":"x"}}`))
		if *cdsPath != "/cds-services/order-sign-crd" {
			t.Errorf("CRD path on the CDS base = %q, want /cds-services/order-sign-crd", *cdsPath)
		}
		if _, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "c", "p", dtrFetchReq); err != nil {
			t.Fatalf("DTR Handle: %v", err)
		}
		if *dtrPath != "/Questionnaire/$questionnaire-package" {
			t.Errorf("DTR path on the DTR base = %q, want /Questionnaire/$questionnaire-package", *dtrPath)
		}
		res, err := n.Handle(context.Background(), "pas-claim", "c", "PCI-1", conformant)
		if err != nil || res.Status != 0 {
			t.Fatalf("PAS Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if *pasPath != "/Claim/$submit" {
			t.Errorf("PAS path on the PAS base = %q, want /Claim/$submit", *pasPath)
		}
		if *sharedPath != "" {
			t.Errorf("the shared base received %q; with all three bases set it must receive nothing", *sharedPath)
		}
	})

	t.Run("neither per-operation base set: DTR and PAS stay on the shared base", func(t *testing.T) {
		shared, sharedPath := newBase(t, pkg)
		n := NewNativeResponder(shared.Client(), shared.URL, "order-sign-crd", newCensusSoR(), fixedClock,
			WithDTRBaseURL(""), WithPASBaseURL(""))
		if _, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "c", "p", dtrFetchReq); err != nil {
			t.Fatalf("DTR Handle: %v", err)
		}
		if *sharedPath != "/Questionnaire/$questionnaire-package" {
			t.Errorf("DTR path on the shared base = %q, want /Questionnaire/$questionnaire-package", *sharedPath)
		}
	})
}

// TestEndpointEvidenceFenceJudgedPerContractBase (the rejection row
// for the per-operation bases): the same-origin trust rule fences each
// published endpoint against the base ITS contract forwards to, never
// against the shared base alone. Otherwise a split-base partner's own DTR
// endpoint (same origin as the DTR base, a different origin from the shared
// base) would be dropped as "cross-origin" and the partner fenced out of its
// own published rule — while an entry on the shared base's origin, which the
// DTR forward never uses, would be honored for DTR.
func TestEndpointEvidenceFenceJudgedPerContractBase(t *testing.T) {
	pkg := []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	shared := newStubPartner(t) // the shared base: PAS lives here
	dtr := newStubPartner(t)    // the DTR base: a different origin
	dtr.respByPath["/Questionnaire/$questionnaire-package-v22"] = pkg
	dtr.respByPath["/Questionnaire/$questionnaire-package"] = pkg
	shared.respByPath["/Questionnaire/$questionnaire-package-v22"] = pkg

	var notes []string
	n := NewNativeResponder(shared.srv.Client(), shared.srv.URL, "shn-order-select", nil, nil,
		WithDTRBaseURL(dtr.srv.URL),
		WithEndpointEvidenceObserver(func(note string) { notes = append(notes, note) }))

	t.Run("a DTR entry on the DTR base's origin is kept, and the published endpoint beats the configured base+path", func(t *testing.T) {
		notes = nil
		n.SetEndpointEvidence(map[string]string{"pa.dtr@2.2": dtr.srv.URL + "/Questionnaire/$questionnaire-package-v22"})
		if len(notes) != 0 {
			t.Fatalf("the partner's own DTR endpoint was dropped: %v", notes)
		}
		ctx := withAnswerLine(context.Background(), "pa.dtr@2.2")
		if _, err := n.Handle(ctx, "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if dtr.lastPath != "/Questionnaire/$questionnaire-package-v22" {
			t.Fatalf("DTR base lastPath = %q, want the published #2.2 endpoint", dtr.lastPath)
		}
	})

	t.Run("a DTR entry on the shared base's origin is dropped: the DTR forward never goes there", func(t *testing.T) {
		notes = nil
		dtr.lastPath, shared.lastPath = "", ""
		n.SetEndpointEvidence(map[string]string{"pa.dtr@2.2": shared.srv.URL + "/Questionnaire/$questionnaire-package-v22"})
		if len(notes) != 1 {
			t.Fatalf("want exactly one drop note, got %d: %v", len(notes), notes)
		}
		ctx := withAnswerLine(context.Background(), "pa.dtr@2.2")
		if _, err := n.Handle(ctx, "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if shared.lastPath != "" {
			t.Fatalf("the shared base received a DTR forward at %q — the dropped entry was selected", shared.lastPath)
		}
		if dtr.lastPath != "/Questionnaire/$questionnaire-package" {
			t.Fatalf("DTR base lastPath = %q, want the configured DTR base+path fallback", dtr.lastPath)
		}
	})

	t.Run("a PAS entry on the shared base's origin is still kept (PAS base unset ⇒ shared)", func(t *testing.T) {
		notes = nil
		n.SetEndpointEvidence(map[string]string{"pa.pas@2.2": shared.srv.URL + "/Claim/$submit-v22"})
		if len(notes) != 0 {
			t.Fatalf("a PAS entry on the shared base was dropped: %v", notes)
		}
	})
}
