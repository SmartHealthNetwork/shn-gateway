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
	t.Parallel()
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
	t.Parallel()
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
// large"; any other is answered as a delivered refusal of the answer, each as
// the problem of a delivered yes answer (writeDeliveredMaybe). A Hub reason
// marked only unknown is not answered as the recipient having answered: it is
// delivered unknown. Every row at every level.
func TestOrigination_AnAnswerRefusedAfterTheRecipientAnsweredIsDelivered(t *testing.T) {
	t.Parallel()
	// hubOn is the Hub's answer to a POST of leg; hub is a provider's PAS
	// claim's, which a payer's gateway answers.
	hubOn := func(leg, body, delivered string) error {
		e := hubRefusal(http.StatusBadGateway, []byte(body), delivered)
		e.leg = leg
		return e
	}
	hub := func(body, delivered string) error { return hubOn("pas-claim", body, delivered) }
	relayed := func(rule string) string {
		if rule == "" {
			return `{"error":"` + hubReasonRecipientRefusedAnswer + `"}`
		}
		return `{"error":"` + hubReasonRecipientRefusedAnswer + `","rule":"` + rule + `"}`
	}
	sizeAnswer := errResponseTooLarge
	refusedAnswer := errAnswerRefusedDelivered
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
		// The payer's attachment request is answered by a provider's gateway:
		// the party is the leg row's recipient's, never the payer's.
		"provider gateway, the payer's attachment request": {hubOn(legCDexRequest, relayed(RefusalAuthority), "yes"), RefusedByProviderGateway, RefusalAuthority, refusedAnswer},
		"hub":          {hub(`{"error":"`+hubReasonAnswerTooLarge+`"}`, "yes"), RefusedByHub, RefusalRouting, sizeAnswer},
		"this gateway": {&answerLostError{cause: errResponseTooLarge, rule: RefusalRouting, tooLarge: true}, RefusedByProviderGateway, RefusalRouting, sizeAnswer},
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
				if name == "this gateway" {
					wantDelivered = ""
				}
				if x.outcome != ExchangeRefused || x.party != row.by || x.rule != row.rule || x.rec.HubDelivered != wantDelivered {
					t.Fatalf("recorded %s by %q on %q delivered %q, want refused by %s on %s, delivered %q", x.outcome, x.party, x.rule, x.rec.HubDelivered, row.by, row.rule, wantDelivered)
				}
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				rec := httptest.NewRecorder()
				if !env.originator.relayOriginationError(rec, row.err) {
					t.Fatal("relayOriginationError did not answer")
				}
				wantDeliveredProblem(t, rec, http.StatusBadGateway, deliveredYes, "application/json", row.answer)
			})
		}
	}
	// The Hub marks these reasons delivered yes; the answer to the caller says
	// the recipient answered only on that mark. Marked unknown, the answer is
	// delivered unknown, never the size refusal.
	t.Run("unmarked", func(t *testing.T) {
		err := hub(relayed(RefusalRouting), "unknown")
		rec := httptest.NewRecorder()
		env := newInProcessExchange(t)
		env.originator.relayOriginationError(rec, err)
		if strings.Contains(rec.Body.String(), `"error":"`+errResponseTooLarge+`"`) {
			t.Fatalf("an unmarked reason answered %d delivered=%q %s", rec.Code, rec.Header().Get(HubDeliveredHeader), rec.Body.String())
		}
		wantDeliveredProblem(t, rec, http.StatusBadGateway, deliveredUnknown, "application/json", "the recipient may have received this request ("+hubReasonRecipientRefusedAnswer+"): check its outcome before resending")
	})
}

// The answer over the limit this gateway reads is its own refusal, whichever
// role sent the leg: a payer's gateway sending the payer's attachment request
// records it as the payer gateway's.
func TestOrigination_AnAnswerTooLargeIsTheSendersOwnRefusal(t *testing.T) {
	for _, self := range []string{RefusedByProviderGateway, RefusedByPayerGateway} {
		x := &exchangeRecorder{self: self}
		x.legOutcome(&answerLostError{cause: errResponseTooLarge, rule: RefusalRouting, tooLarge: true})
		if x.outcome != ExchangeRefused || x.party != self || x.rule != RefusalRouting {
			t.Errorf("a %s: recorded %s by %q on %q, want refused by itself on routing", self, x.outcome, x.party, x.rule)
		}
	}
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
	t.Parallel()
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

// REJECTION (network rule, authentication), on PA's coverage eligibility leg: a
// payer's gateway refuses to send an answer whose requester's registered encryption key is of small order, in
// every encoding, or absent, at every level, before the answer's token is minted: 502,
// the status of every refusal of an answer, with the rule and no
// cryptographic detail. Once its system has answered, the refusal is attested
// delivered, as every refusal of a system's answer is; an answer built from
// the payer's records, before its system was asked, is refused unattested.
// The record names the rule; the log names the cause. The same answer to a
// requester with a usable key is sealed and sent.
func TestPAAnswer_ARequesterKeyNotUsableIsRefusedOnAuthentication(t *testing.T) {
	logged := captureLog(t)
	keys := lowOrderKeys(t)
	for _, level := range allLevels {
		for _, systemAnswers := range []bool{true, false} {
			attested := map[bool]string{true: "attested", false: "unattested"}[systemAnswers]
			for _, name := range sortedKeyNames(keys) {
				t.Run(level.String()+"/"+attested+"/"+name, func(t *testing.T) {
					p := eligibilityPayer(t, level, systemAnswers)
					p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + dtrFrameMember)
					var got exchangeRecords
					p.g.cfg.ExchangeObserved = got.observe
					tokens := countTokens(p.g)
					registerEncKey(t, p.g.cfg.Reg, p.requester.ID, keys[name])
					a := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
					if a.status != http.StatusBadGateway || a.framed || string(bytes.TrimSpace(a.body)) != recipientKeyNotUsableBody {
						t.Fatalf("answer %d (framed %v) %s, want 502 %s", a.status, a.framed, a.body, recipientKeyNotUsableBody)
					}
					if bytes.Contains(a.body, []byte("X25519")) || bytes.Contains(a.body, []byte("order")) {
						t.Fatalf("the answer carries the cryptographic detail: %s", a.body)
					}
					wantDelivered := map[bool]string{true: "yes", false: ""}[systemAnswers]
					if d := a.header.Get(HubDeliveredHeader); d != wantDelivered {
						t.Fatalf("%s %q, want %q", HubDeliveredHeader, d, wantDelivered)
					}
					if systemAnswers != (p.partner.lastPath == eligibilityPath) {
						t.Fatalf("the payer's system was asked %q; want it asked: %v", p.partner.lastPath, systemAnswers)
					}
					if n := tokens.Load(); n != 0 {
						t.Fatalf("the refused answer asked for %d tokens", n)
					}
					r := got.only(t)
					wantRefusal(t, r, http.StatusBadGateway, RefusedByPayerGateway, RefusalAuthentication)
					if strings.Contains(r.RefusalReason, "X25519") {
						t.Fatalf("an inbound record carries a reason: %q", r.RefusalReason)
					}
					if !strings.Contains(logged.String(), "coverage-eligibility answer not sealed to requester: "+string(reasonRecipientKeyNotUsable)) {
						t.Fatalf("the log does not name the cause:\n%s", logged.String())
					}
				})
			}
			t.Run(level.String()+"/"+attested+"/control: a usable key", func(t *testing.T) {
				p := eligibilityPayer(t, level, systemAnswers)
				p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + dtrFrameMember)
				tokens := countTokens(p.g)
				a := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
				if a.status != http.StatusOK || !a.framed || tokens.Load() == 0 {
					t.Fatalf("answer %d (framed %v) %s after %d tokens, want the answer sealed and sent", a.status, a.framed, a.body, tokens.Load())
				}
			})
		}
	}
}
