package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, nil, nil)

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

func TestNativeResponder_CRDAddsHookInstance(t *testing.T) {
	p := newStubPartner(t)
	cards := []byte(`{"cards":[{"summary":"x"}]}`)
	// The conventional CDS service path (Task: keep in sync with native.go's const).
	p.respByPath["/cds-services/shn-order-select"] = cards
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, nil, nil)

	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci",
		[]byte(`{"hook":"order-select","context":{"patientId":"p1","draftOrders":[{}]}}`))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if string(res.ResponseFHIR) != string(cards) {
		t.Errorf("cards = %s, want partner verbatim", res.ResponseFHIR)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(p.lastBody, &m); err != nil {
		t.Fatalf("forwarded body not JSON: %v", err)
	}
	if _, ok := m["hookInstance"]; !ok {
		t.Errorf("forwarded hook missing hookInstance: %s", p.lastBody)
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
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, nil, nil)

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
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, nil, nil)
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
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, nil, nil)
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
	n := NewNativeResponder(srv.Client(), srv.URL, nil, nil) // store=nil, clock=nil
	res, err := n.Handle(context.Background(), "coverage-eligibility", "corr-1", "PCI-1",
		[]byte(`{"resourceType":"CoverageEligibilityRequest"}`))
	if err != nil || res.Status != 0 {
		t.Fatalf("read-only leg with nil store must succeed: err=%v status=%d", err, res.Status)
	}
}
