package engine

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestScenarioResetRoute pins where the unauthenticated demo reset is reachable from.
//
// POST /scenario/reset clears this holder's exchange records on the SHARED store — a
// holder-wide DELETE that reaches every replica — and the demo session state of the
// replica that served it. It carries no credential of its own, so it may only be mounted
// where the whole /scenario/* demo surface is: the provider gateway, which is internal
// (never given a public host).
//
// Payer, facility and PHG expose no reset route and return 404.
func TestScenarioResetRoute(t *testing.T) {
	cases := []struct {
		name        string
		role        string
		payerNative bool
		wantStatus  int
	}{
		{"provider (internal, the whole /scenario/* surface lives here)", "provider", false, http.StatusOK},
		{"native-forward payer (the PUBLIC fhir.<apex> front door): removed", "payer", true, http.StatusNotFound},
		{"payer running a partner's own responder (also public): removed", "payer", false, http.StatusNotFound},
		{"facility", "facility", false, http.StatusNotFound},
		{"phg", "phg", false, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &Gateway{cfg: Config{Role: tc.role, PayerDavinciNative: tc.payerNative}}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/scenario/reset", nil)
			g.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("POST /scenario/reset for %s: got %d, want %d", tc.name, rec.Code, tc.wantStatus)
			}
		})
	}
}

// A refused payer reset must leave both shared exchange and local session state intact.
func TestScenarioReset_PayerRefusalPreservesState(t *testing.T) {
	fake := &fakeExchangeStore{mem: NewInMemoryExchangeStore(time.Hour, time.Now)}
	g := &Gateway{cfg: Config{Role: "payer", PayerDavinciNative: true}, exchanges: fake}
	g.pending = map[string]pendState{"tok": {scenario: "uc06"}}
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scenario/reset", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("payer reset: got %d, want 404", rec.Code)
	}
	if fake.resets != 0 || len(g.pending) != 1 {
		t.Fatal("refused payer reset changed state")
	}
}

// A reset whose STORE half failed is a failed reset. Reporting 200 while the holder's
// exchange records are still there is how a demo run starts on stale state and the
// operator is told nothing: the store error propagates out of Reset, the route answers
// 503 with a cause-free body, and the outage is counted under the store's own name.
//
// The in-memory pending map is cleared FIRST and stays cleared even when the store half
// fails. Clearing it cannot fail, it is what the demo session actually needs, and leaving
// it stale would add a second failure to the one being reported — the caller is told the
// reset did not complete either way, and a retry re-runs both halves.
func TestGatewayReset_StoreFailureIs503AndCounted(t *testing.T) {
	fake := &fakeExchangeStore{mem: NewInMemoryExchangeStore(time.Hour, time.Now), failReset: errors.New("store down")}
	var stores []string
	g := &Gateway{
		cfg:       Config{Role: "provider", StoreErrorMetric: func(s string) { stores = append(stores, s) }},
		exchanges: fake,
	}
	g.pending = map[string]pendState{"tok": {scenario: "uc06"}}
	if err := g.Reset(); err == nil {
		t.Fatal("Reset must return the store's error, not log it and report success")
	}
	if len(g.pending) != 0 {
		t.Fatal("the pending map must still be cleared when the store half fails")
	}
	if len(stores) != 1 || stores[0] != storeErrExchange {
		t.Fatalf("StoreErrorMetric calls = %v, want exactly [%s]", stores, storeErrExchange)
	}
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scenario/reset", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /scenario/reset over a failing store: got %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"exchange store reset failed"`) || strings.Contains(got, "store down") {
		t.Fatalf("503 body = %s; want the fixed cause-free message, never the store's own error text", got)
	}
	// The route's own call counts a second outage: one per refusing call, never per half.
	if len(stores) != 2 {
		t.Fatalf("StoreErrorMetric calls after the route = %d, want 2 (one per Reset)", len(stores))
	}
	// A healthy store answers 200 {"ok":true} — the shape the console has always read.
	fake.failReset = nil
	rec = httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scenario/reset", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("healthy reset = %d %s; want 200 {\"ok\":true}", rec.Code, rec.Body.String())
	}
}
