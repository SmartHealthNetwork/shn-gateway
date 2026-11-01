//go:build !shn_coreonly

package engine

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The request-body limit on the PA ingress routes, the payer's delivery route
// and the sealed envelope (bodycap.go); the bounded read itself is
// TestReadCapped.

// overTheCap is one byte over the network's request limit.
var overTheCap = bytes.Repeat([]byte(" "), shnsdk.MaxRequestBytes+1)

// overTheAttachmentCeiling is one byte over the network's ceiling on a
// documentation attachment submission, the limit of the CDex routes.
var overTheAttachmentCeiling = bytes.Repeat([]byte(" "), shnsdk.MaxAttachmentBytes+1)

// ingressLimitBody is one byte over route's request limit: the attachment
// ceiling on a CDex route, the network's request limit on every other.
func ingressLimitBody(route string) []byte {
	if route == RouteCDexSubmit || route == RouteCDexReturnSubmit {
		return overTheAttachmentCeiling
	}
	return overTheCap
}

// Every provider ingress route refuses a body over the network's limit 413
// with the published reason (as it is on CDS Hooks, an OperationOutcome on the
// FHIR operation routes, as each of their refusals is), recorded as the provider gateway's routing
// refusal, at every level, and sends nothing; a body at the limit is read
// whole (here it is only spaces, so it is refused as what it is, never 413).
// A CDex route's limit is the attachment ceiling.
func TestIngress_ABodyOverTheLimitIs413(t *testing.T) {
	t.Parallel()
	for _, route := range workstreamIngressRouteNames() {
		overTheCap := ingressLimitBody(route)
		atTheCap := overTheCap[:len(overTheCap)-1]
		for _, level := range allLevels {
			t.Run(route+"/over/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(route)(rec, ingressPostTo(route, overTheCap))
				// CDS Hooks answers the published body as it is; a FHIR
				// operation route answers it as an OperationOutcome.
				want := `"diagnostics":"` + errRequestTooLarge + `"`
				if route == RouteCRD {
					want = `{"error":"` + errRequestTooLarge + `"}`
				}
				refusedBeforeTheNetwork(t, env, rec, http.StatusRequestEntityTooLarge, want)
				wantRefusal(t, got.only(t), http.StatusRequestEntityTooLarge, RefusedByProviderGateway, RefusalRouting)
			})
		}
		t.Run(route+"/at the limit", func(t *testing.T) {
			env := newInProcessExchange(t)
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(route)(rec, ingressPostTo(route, atTheCap))
			if rec.Code == http.StatusRequestEntityTooLarge || strings.Contains(rec.Body.String(), "read body failed") {
				t.Fatalf("a body at the limit: %d %s; want it read whole", rec.Code, rec.Body.String())
			}
		})
	}
}

// ingressPostTo is a POST of body to an ingress route: a CDS Hooks route at
// one of its services.
func ingressPostTo(route string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if route == RouteCRD {
		r = httptest.NewRequest(http.MethodPost, "/cds-services/shn-order-select", bytes.NewReader(body))
		r.SetPathValue("id", "shn-order-select")
	}
	r.Header.Set("Content-Type", "application/fhir+json")
	return r
}

// A broken request stream is still the 400 it was, never a 413: only the limit
// is answered as one.
func TestIngress_ABrokenStreamIsStill400(t *testing.T) {
	env := newInProcessExchange(t)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", &failingReader{n: 10})
	env.originator.ingressRoute(RoutePAS)(rec, r)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "read body failed") {
		t.Fatalf("answer %d %s, want 400 read body failed", rec.Code, rec.Body.String())
	}
}

// A request whose sealed envelope would be over the limit is refused 413
// before a token is minted or anything is sent, whatever the body's own size:
// the ciphertext is base64 in the envelope. A request that seals under it is
// sent.
func TestOriginateLeg_AnEnvelopeOverTheLimitIsNotSent(t *testing.T) {
	t.Parallel()
	// 6.5 MiB seals to over 8 MiB as base64.
	big := []byte(`{"a":"` + strings.Repeat("a", 13<<19) + `"}`)
	env := newInProcessExchange(t)
	_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-big", "", Content{WorkstreamType: workstreamPA, Payload: testRequest(big)})
	if !errors.Is(err, errEnvelopeTooLarge) {
		t.Fatalf("err %v, want errEnvelopeTooLarge", err)
	}
	if n := env.routeHitCount(); n != 0 || env.substrate.authorizeHits != 0 {
		t.Fatalf("the refused request crossed the network (%d) or was authorized (%d)", n, env.substrate.authorizeHits)
	}
	if !strings.HasPrefix(err.Error(), errRequestTooLarge+": ") || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatalf("refusal %q, want the leg's 8 MiB named", err)
	}
	rec := httptest.NewRecorder()
	if !env.originator.relayOriginationError(rec, err) || rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), err.Error()) {
		t.Fatalf("answer %d %s, want 413 %q", rec.Code, rec.Body.String(), err)
	}
}

// Through the ingress, a request read whole whose sealed envelope would be
// over the limit is refused 413 naming the envelope, recorded as the provider
// gateway's routing refusal, never authorized or sent, at every level.
func TestIngress_AnEnvelopeOverTheLimitIs413(t *testing.T) {
	t.Parallel()
	cer := eligibilityRequest(t, dtrFrameMember)
	padded := append(append([]byte(nil), cer...), bytes.Repeat([]byte(" "), 7<<20)...)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(padded))
			refusedBeforeTheNetwork(t, env, rec, http.StatusRequestEntityTooLarge, `"diagnostics":"`+(&envelopeTooLargeError{limit: shnsdk.MaxRequestBytes}).Error()+`"`)
			if env.substrate.authorizeHits != 0 {
				t.Fatalf("the refused request was authorized (%d)", env.substrate.authorizeHits)
			}
			wantRefusal(t, got.only(t), http.StatusRequestEntityTooLarge, RefusedByProviderGateway, RefusalRouting)
		})
	}
}

// The module 1 PA inquiry route reads its body through the same bound: one
// over the limit is refused 413 with the published answer.
func TestPAInquire_ABodyOverTheLimitIs413(t *testing.T) {
	env := newInProcessExchange(t)
	rec := httptest.NewRecorder()
	env.originator.handlePAInquire(rec, httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire", bytes.NewReader(overTheCap)))
	if rec.Code != http.StatusRequestEntityTooLarge || string(bytes.TrimSpace(rec.Body.Bytes())) != `{"error":"`+errRequestTooLarge+`"}` {
		t.Fatalf("answer %d %s, want 413 %s", rec.Code, rec.Body.String(), errRequestTooLarge)
	}
}

// The Hub's own 413 is answered as the network's 413 and recorded as the
// Hub's routing refusal.
func TestOriginateLeg_TheHubs413IsAnswered413(t *testing.T) {
	refused := &hubRefusalError{status: http.StatusRequestEntityTooLarge, reason: errRequestTooLarge, fromHub: true}
	if outcome, by, rule := hubRefusalOutcome(refused); outcome != ExchangeRefused || by != RefusedByHub || rule != RefusalRouting {
		t.Fatalf("recorded %s %s %s, want the Hub's routing refusal", outcome, by, rule)
	}
	env := newInProcessExchange(t)
	rec := httptest.NewRecorder()
	if !env.originator.relayOriginationError(rec, refused) || rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("answer %d %s, want 413", rec.Code, rec.Body.String())
	}
}

// The payer's gateway refuses an envelope over the network's limit 413,
// recorded as its routing refusal, at every level, and calls nothing; an
// envelope exactly at the limit is carried.
func TestInbound_AnEnvelopeOverTheLimitIs413(t *testing.T) {
	t.Parallel()
	answer := eligResponses(t, "Patient/"+dtrFrameMember)
	padTo := func(n int) func([]byte) []byte {
		return func(raw []byte) []byte {
			if len(raw) > n {
				t.Fatalf("envelope %d bytes, over %d before padding", len(raw), n)
			}
			// JSON allows whitespace after the value: the same envelope, n bytes.
			return append(raw, bytes.Repeat([]byte(" "), n-len(raw))...)
		}
	}
	for _, level := range allLevels {
		t.Run("over/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			p.partner.respByPath[eligibilityPath] = answer
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			p.envelope = padTo(shnsdk.MaxRequestBytes + 1)
			ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if ans.status != http.StatusRequestEntityTooLarge || string(bytes.TrimSpace(ans.body)) != `{"error":"`+errRequestTooLarge+`"}` || p.partner.calls.Load() != 0 {
				t.Fatalf("answer %d %s (payer system calls %d), want 413 %s", ans.status, ans.body, p.partner.calls.Load(), errRequestTooLarge)
			}
			wantRefusal(t, got.only(t), http.StatusRequestEntityTooLarge, RefusedByPayerGateway, RefusalRouting)
		})
	}
	t.Run("at the limit", func(t *testing.T) {
		p := eligibilityPayer(t, EnforcementObserve, true)
		p.partner.respByPath[eligibilityPath] = answer
		p.envelope = padTo(shnsdk.MaxRequestBytes)
		if ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember)); ans.status != http.StatusOK {
			t.Fatalf("answer %d %s, want the envelope carried", ans.status, ans.body)
		}
	})
}
