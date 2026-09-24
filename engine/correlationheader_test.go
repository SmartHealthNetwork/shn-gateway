package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The id is settled and stamped before the handler runs, so the earliest answer a
// handler can give — the 401 for a missing bearer — already carries it. Every later
// answer (the generic 502 included) is written by the same handler on the same
// writer, so it carries the same value.
func TestIngressCorrelationHeader_OnEarliestRefusal(t *testing.T) {
	g := &Gateway{cfg: Config{CorrelationGen: func() string { return "minted-0001" }}}
	for name, h := range map[string]http.HandlerFunc{
		"crd": g.handleCRDIngress, "dtr": g.handleDTRIngress, "pas": g.handlePASIngress, "inquire": g.handlePASInquireIngress,
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
			r.SetPathValue("id", "shn-order-sign")
			w := httptest.NewRecorder()
			g.withIngressCorrelation(h)(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d want 401 body=%s", w.Code, w.Body.String())
			}
			if got := w.Header().Get(CorrelationHeader); got != "minted-0001" {
				t.Errorf("X-Correlation-Id=%q, want the minted id on the refusal", got)
			}
		})
	}
}

// A well-formed caller id becomes the exchange's id and is returned; a malformed
// one is ignored and an id is minted instead.
func TestIngressCorrelationHeader_CallerIdAdoptedOrReplaced(t *testing.T) {
	g := &Gateway{cfg: Config{CorrelationGen: func() string { return "minted-0002" }}}
	for sent, want := range map[string]string{
		"run-2026.09.20_a":      "run-2026.09.20_a",
		strings.Repeat("a", 64): strings.Repeat("a", 64),
		strings.Repeat("a", 65): "minted-0002",
		"two words":             "minted-0002",
		`q"uote`:                "minted-0002",
		"":                      "minted-0002",
	} {
		r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader("{}"))
		if sent != "" {
			r.Header.Set(CorrelationHeader, sent)
		}
		w := httptest.NewRecorder()
		g.withIngressCorrelation(g.handlePASIngress)(w, r)
		if got := w.Header().Get(CorrelationHeader); got != want {
			t.Errorf("sent %q: X-Correlation-Id=%q, want %q", sent, got, want)
		}
	}
}

// A handler reached without the wrapper still answers with the id it used for
// the leg: the header contract does not depend on the mount.
func TestIngressCorrelation_MintsWhenUnsettled(t *testing.T) {
	g := &Gateway{cfg: Config{CorrelationGen: func() string { return "minted-0003" }}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := g.ingressCorrelation(w, r); got != "minted-0003" {
		t.Fatalf("ingressCorrelation=%q", got)
	}
	if got := w.Header().Get(CorrelationHeader); got != "minted-0003" {
		t.Errorf("X-Correlation-Id=%q, want the minted id stamped", got)
	}
	// A Gateway built without a generator (a bare struct in a test) still mints.
	bare := &Gateway{}
	if got := bare.ingressCorrelation(httptest.NewRecorder(), r); len(got) != 32 {
		t.Errorf("bare gateway minted %q, want a 32-hex id", got)
	}
}

// Through the in-process exchange: the header on a relayed payer refusal and on a
// success is the id the leg was originated under, on both the CRD and the
// (framed) refusal path.
func TestIngressCorrelationHeader_OnRelayedAnswers(t *testing.T) {
	env := newInProcessExchange(t)
	env.originator.cfg.CorrelationGen = func() string { return "leg-corr-0009" }
	h := env.originator.withIngressCorrelation(env.originator.handleCRDIngress)

	oo := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}]}`
	env.payerReturns(LegResult{Status: 502, Response: testResponse([]byte(oo))})
	rec := httptest.NewRecorder()
	h(rec, env.crdIngressRequest(t))
	if rec.Code != 502 {
		t.Fatalf("status=%d want 502 body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(CorrelationHeader); got != "leg-corr-0009" {
		t.Errorf("relayed refusal X-Correlation-Id=%q, want the leg's id", got)
	}

	env.payerReturns(LegResult{})
	rec = httptest.NewRecorder()
	h(rec, env.crdIngressRequest(t))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(CorrelationHeader); got != "leg-corr-0009" {
		t.Errorf("success X-Correlation-Id=%q, want the leg's id", got)
	}
}

// pasIngressBundle is a conformant-enough $submit for the in-process harness: the
// member the census holds, a Claim, an order and a Coverage naming payer id payor.
func pasIngressBundle(payor, claimExtra string) string {
	coverage := `{"resourceType":"Coverage","id":"cov1","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"` + payor + `"}}]}`
	return `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Patient","id":"MBR-COVERED"}},{"resource":{"resourceType":"Claim",` + claimExtra + `"patient":{"reference":"Patient/MBR-COVERED"}}},{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}},{"resource":` + coverage + `}]}`
}

// The routing refusal a partner is most likely to quote — 422 for a payer identifier
// the network does not register — carries the id too: it is answered after the id
// is settled and before any leg exists.
func TestIngressCorrelationHeader_OnRoutingRefusal(t *testing.T) {
	env := newInProcessExchange(t)
	env.originator.cfg.CorrelationGen = func() string { return "leg-corr-0422" }
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("99999", "")))
	env.originator.withIngressCorrelation(env.originator.handlePASIngress)(w, r)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "no registered payer for identifier") {
		t.Fatalf("status=%d body=%s, want the 422 routing refusal", w.Code, w.Body.String())
	}
	if got := w.Header().Get(CorrelationHeader); got != "leg-corr-0422" {
		t.Errorf("X-Correlation-Id=%q on the routing refusal, want leg-corr-0422", got)
	}
}

// A $submit that names its own correlation in the Claim (urn:shn:correlation) keeps
// that contract: the Claim's value is the leg's id and the header reports it, over
// both a minted id and a caller header.
func TestIngressCorrelationHeader_ClaimCorrelationWins(t *testing.T) {
	env := newInProcessExchange(t)
	env.originator.cfg.CorrelationGen = func() string { return "leg-corr-minted" }
	claim := `"identifier":[{"system":"urn:shn:correlation","value":"claim-corr-77"}],`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", claim)))
	r.Header.Set(CorrelationHeader, "caller-header-1")
	env.originator.withIngressCorrelation(env.originator.handlePASIngress)(w, r)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden || w.Code == http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s: the request must get past routing for the Claim's correlation to apply", w.Code, w.Body.String())
	}
	if got := w.Header().Get(CorrelationHeader); got != "claim-corr-77" {
		t.Errorf("X-Correlation-Id=%q, want the Claim's own claim-corr-77", got)
	}
}
