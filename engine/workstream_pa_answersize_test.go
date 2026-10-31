//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// attestedSize is the payer's gateway's attested refusal of its system's
// answer for size.
const attestedSize = `{"error":"` + errResponseTooLarge + `","rule":"` + RefusalRouting + `"}`

// The payer's system answering over what its gateway reads: the payer's
// gateway refuses it for size at every level, never sealing or relaying it cut
// short. It answers the Hub raw, 502 {"error":"response body too large",
// "rule":"routing"}, attesting with X-SHN-Delivered yes that its system
// answered, and records its own routing refusal. An answer of its own size is carried.
func TestInbound_ASystemAnswerOverTheLimitIsRefusedDelivered(t *testing.T) {
	over := bytes.Repeat([]byte(" "), maxPartnerBody+1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			p.partner.respByPath[eligibilityPath] = over
			ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if ans.status != http.StatusBadGateway || string(bytes.TrimSpace(ans.body)) != attestedSize || ans.header.Get(HubDeliveredHeader) != "yes" {
				t.Fatalf("answer %d delivered=%q %.200s, want the raw 502 attested delivered", ans.status, ans.header.Get(HubDeliveredHeader), ans.body)
			}
			if p.partner.calls.Load() != 1 {
				t.Fatalf("payer system calls %d, want the one that answered", p.partner.calls.Load())
			}
			wantRefusal(t, got.only(t), http.StatusBadGateway, RefusedByPayerGateway, RefusalRouting)
		})
	}
	t.Run("control", func(t *testing.T) {
		p := eligibilityPayer(t, EnforcementObserve, true)
		p.partner.respByPath[eligibilityPath] = eligResponses(t, "Patient/"+dtrFrameMember)
		if ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember)); ans.status != http.StatusOK || ans.header.Get(HubDeliveredHeader) != "" {
			t.Fatalf("answer %d delivered=%q, want it carried, unattested", ans.status, ans.header.Get(HubDeliveredHeader))
		}
	})
}

// A system answer read whole but too large to seal (base64 in the envelope)
// is refused the same way when it is sealed: nothing of it is sent, and no
// token is minted for it.
func TestInbound_ASystemAnswerTooLargeToSealIsRefusedDelivered(t *testing.T) {
	p := eligibilityPayer(t, EnforcementNone, true)
	var got exchangeRecords
	p.g.cfg.ExchangeObserved = got.observe
	base := p.g.authzClient().Transport
	if base == nil {
		base = http.DefaultTransport
	}
	authz := &countingTransport{inner: base}
	p.g.cfg.AuthzClient = &http.Client{Transport: authz}
	answer := eligResponses(t, "Patient/"+dtrFrameMember)
	p.partner.respByPath[eligibilityPath] = append(answer, bytes.Repeat([]byte(" "), 7<<20)...)
	ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
	if ans.status != http.StatusBadGateway || string(bytes.TrimSpace(ans.body)) != attestedSize || ans.header.Get(HubDeliveredHeader) != "yes" {
		t.Fatalf("answer %d delivered=%q %.200s, want the raw 502 attested delivered", ans.status, ans.header.Get(HubDeliveredHeader), ans.body)
	}
	wantRefusal(t, got.only(t), http.StatusBadGateway, RefusedByPayerGateway, RefusalRouting)
	if n := authz.n; n != 0 {
		t.Fatalf("%d authorize calls for an answer already over the limit as sealed, want none", n)
	}
}

// On the requester's gateway, an answer refused after the recipient answered
// is answered to its caller with X-SHN-Delivered yes, and recorded as refused
// by whoever refused it on its rule, delivered: the payer's gateway on the
// rule the Hub relays (unknown, "other", when the Hub relays none or one
// outside the closed list), the Hub on its own read, or this gateway on its
// read of the Hub's answer. The size refusal is answered 502 "response body too
// large"; any other is answered as a delivered refusal of the answer. A Hub
// reason without its delivered mark is not answered as delivered. Every row
// at every level.
func TestOrigination_AnAnswerRefusedAfterTheRecipientAnsweredIsDelivered(t *testing.T) {
	hub := func(body, delivered string) error {
		return hubRefusal(http.StatusBadGateway, []byte(body), delivered)
	}
	relayed := func(rule string) string {
		if rule == "" {
			return `{"error":"` + hubReasonRecipientRefusedAnswer + `"}`
		}
		return `{"error":"` + hubReasonRecipientRefusedAnswer + `","rule":"` + rule + `"}`
	}
	sizeAnswer := `{"error":"` + errResponseTooLarge + `"}`
	refusedAnswer := `{"error":"` + errAnswerRefusedDelivered + `"}`
	rows := map[string]struct {
		err      error
		by, rule string
		answer   string
	}{
		"payer gateway, routing":          {hub(relayed(RefusalRouting), "yes"), RefusedByPayerGateway, RefusalRouting, sizeAnswer},
		"payer gateway, authority":        {hub(relayed(RefusalAuthority), "yes"), RefusedByPayerGateway, RefusalAuthority, refusedAnswer},
		"payer gateway, no rule":          {hub(relayed(""), "yes"), RefusedByPayerGateway, ExchangeOther, refusedAnswer},
		"payer gateway, rule outside set": {hub(relayed("size"), "yes"), RefusedByPayerGateway, ExchangeOther, refusedAnswer},
		"payer gateway, fidelity":         {hub(relayed(RefusalFidelity), "yes"), RefusedByPayerGateway, ExchangeOther, refusedAnswer},
		"hub":                             {hub(`{"error":"`+hubReasonAnswerTooLarge+`"}`, "yes"), RefusedByHub, RefusalRouting, sizeAnswer},
		"this gateway":                    {&answerLostError{cause: errResponseTooLarge, rule: RefusalRouting, tooLarge: true}, RefusedByProviderGateway, RefusalRouting, sizeAnswer},
	}
	for _, level := range allLevels {
		for name, row := range rows {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				x := &exchangeRecorder{self: RefusedByProviderGateway}
				x.legOutcome(row.err)
				// The record's HubDelivered is the Hub's own word, never
				// inferred: yes on its refusals, none on this gateway's own
				// read of an answer the Hub delivered.
				wantDelivered := "yes"
				if row.by == RefusedByProviderGateway {
					wantDelivered = ""
				}
				if x.outcome != ExchangeRefused || x.party != row.by || x.rule != row.rule || x.rec.HubDelivered != wantDelivered {
					t.Fatalf("recorded %s by %q on %q delivered %q, want refused by %s on %s, delivered %q", x.outcome, x.party, x.rule, x.rec.HubDelivered, row.by, row.rule, wantDelivered)
				}
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				rec := httptest.NewRecorder()
				if !env.originator.relayOriginationError(rec, row.err) || rec.Code != http.StatusBadGateway ||
					string(bytes.TrimSpace(rec.Body.Bytes())) != row.answer || rec.Header().Get(HubDeliveredHeader) != "yes" {
					t.Fatalf("answer %d delivered=%q %s, want 502 %s delivered yes", rec.Code, rec.Header().Get(HubDeliveredHeader), rec.Body.String(), row.answer)
				}
			})
		}
	}
	// The Hub marks these reasons delivered yes; the answer to the caller says
	// delivered only on that mark.
	t.Run("unmarked", func(t *testing.T) {
		err := hub(relayed(RefusalRouting), "unknown")
		rec := httptest.NewRecorder()
		env := newInProcessExchange(t)
		env.originator.relayOriginationError(rec, err)
		if rec.Header().Get(HubDeliveredHeader) != "" || strings.Contains(rec.Body.String(), `"error":"`+errResponseTooLarge+`"`) {
			t.Fatalf("an unmarked reason answered %d delivered=%q %s", rec.Code, rec.Header().Get(HubDeliveredHeader), rec.Body.String())
		}
	})
}

// The requester answers a relayed routing refusal of an answer as its size
// (relayOriginationError): that holds only while the size refusal is the one
// refusal of an answer the recipient attests on routing. Every attested rule
// is one the requester can record (relayedRule), so a fidelity entry fails
// here until the attestation states its stage.
func TestAnswerRefusalRules_RoutingIsTheSizeRefusalAlone(t *testing.T) {
	for reason, rule := range answerRefusalRules {
		if rule == RefusalRouting && reason != errResponseTooLarge {
			t.Errorf("%q is attested on routing, which the requester answers as the answer's size", reason)
		}
		if relayedRule(rule) != rule {
			t.Errorf("%q is attested on %q, which the requester cannot record (relayedRule)", reason, rule)
		}
	}
	if answerRefusalRules[errResponseTooLarge] != RefusalRouting {
		t.Fatalf("the size refusal is attested on %q, want routing", answerRefusalRules[errResponseTooLarge])
	}
}

// The requester's gateway reads the Hub's answer up to the network's limit:
// one over it is refused for size, never decoded cut short; one at it is read
// whole.
func TestPostEnvelope_AHubAnswerOverTheLimitIsRefusedForSize(t *testing.T) {
	for _, n := range []int{shnsdk.MaxResponseBytes, shnsdk.MaxResponseBytes + 1} {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte(" "), n))
		}))
		env := newInProcessExchange(t)
		_, err := env.originator.postEnvelope(context.Background(), hub.Client(), hub.URL, []byte("{}"), "")
		hub.Close()
		var lost *answerLostError
		tooLarge := errors.As(err, &lost) && lost.tooLarge
		if tooLarge != (n > shnsdk.MaxResponseBytes) {
			t.Fatalf("%d bytes: err %v, refused for size %v", n, err, tooLarge)
		}
	}
}
