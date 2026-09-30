package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The payer's own system is told the leg's id in X-Correlation-Id, and is
// sent exactly the bytes it was sent before: the header is metadata about the
// request, never an edit of it, and the answer is relayed as it answered.
func TestBackendCorrelation_LegIDReachesThePayerSystem(t *testing.T) {
	p := newLevelPayer(t, EnforcementObserve)
	body := conformantCRD("MBR-COVERED", "72148")
	ans := p.send(t, "crd-order-select", "", body)
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	if got := p.partner.lastHeader.Get(CorrelationHeader); got != ans.corr {
		t.Fatalf("X-Correlation-Id to the payer's system = %q, want the leg's id %q", got, ans.corr)
	}
	if !bytes.Equal(p.partner.lastBody, body) {
		t.Fatalf("the request was changed on its way to the payer's system:\n got %s\nwant %s", p.partner.lastBody, body)
	}
	if want := p.partner.respByPath[crdSelectPath]; !bytes.Equal(ans.body, want) {
		t.Fatalf("the answer was not relayed exactly:\n got %s\nwant %s", ans.body, want)
	}
}

// A leg id that is not one bounded token — a Claim may name its own — is not
// sent, and the request still goes: a header is never why a call fails.
func TestBackendCorrelation_UnboundedIDIsNotSent(t *testing.T) {
	for id, want := range map[string]string{
		"leg-0001":                            "leg-0001",
		"two words":                           "",
		"line\r\nInjected: 1":                 "",
		string(bytes.Repeat([]byte("a"), 65)): "",
		"":                                    "",
	} {
		req := httptest.NewRequest(http.MethodPost, "http://payer.test/Claim/$submit", nil)
		setBackendCorrelation(withResponderCorrelation(context.Background(), id), req)
		if got := req.Header.Get(CorrelationHeader); got != want {
			t.Errorf("leg id %q: header %q, want %q", id, got, want)
		}
	}
}

// Off, the payer's system is sent no X-Correlation-Id at all, configured or
// not; the default sends the leg's id.
func TestBackendCorrelation_OffSendsNone(t *testing.T) {
	p := newLevelPayer(t, EnforcementObserve)
	n := p.g.cfg.Responder.(*nativeResponder)
	WithBackendHeaders(http.Header{"X-Correlation-Id": {"configured"}, "X-Route": {"r1"}})(n)
	WithoutBackendCorrelation()(n)
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	if got := p.partner.lastHeader.Values(CorrelationHeader); len(got) != 0 {
		t.Fatalf("off: X-Correlation-Id %v reached the payer's system", got)
	}
	if p.partner.lastHeader.Get("X-Route") != "r1" {
		t.Fatal("off dropped the payer's own routing header")
	}
}

// Every operation forwarded to the payer's system carries the leg's id, and
// none does when it is off.
func TestBackendCorrelation_EveryForwardedLeg(t *testing.T) {
	type row struct {
		leg, operation string
		body           func(t *testing.T, p *levelPayer) []byte
	}
	rows := []row{
		{leg: "crd-order-select", body: func(*testing.T, *levelPayer) []byte { return conformantCRD("MBR-COVERED", "72148") }},
		{leg: "crd-order-dispatch", body: func(*testing.T, *levelPayer) []byte { return []byte(crdDispatchRequest) }},
		{leg: "dtr-questionnaire-fetch", operation: shnsdk.FrameOperationQuestionnairePackage, body: func(*testing.T, *levelPayer) []byte { return dtrFetchReq }},
		{leg: "pas-claim", body: func(t *testing.T, _ *levelPayer) []byte { return originatorBuiltConformantBundle(t, "MBR-COVERED") }},
		{leg: "pas-claim-update", body: func(t *testing.T, p *levelPayer) []byte {
			request, related := updateBundle(t)
			p.seedPend(t, related)
			return request
		}},
		{leg: "pas-claim-inquire", body: func(*testing.T, *levelPayer) []byte { return inquiryBundle("MBR-COVERED", "", "TRN-1", "72148") }},
		{leg: "coverage-eligibility", body: func(t *testing.T, _ *levelPayer) []byte { return eligibilityRequest(t, dtrFrameMember) }},
	}
	for _, off := range []bool{false, true} {
		for _, r := range rows {
			t.Run(fmt.Sprintf("%s/off=%v", r.leg, off), func(t *testing.T) {
				p := eligibilityPayer(t, EnforcementObserve, true)
				if off {
					WithoutBackendCorrelation()(p.g.cfg.Responder.(*nativeResponder))
				}
				body := r.body(t, p)
				p.partner.lastHeader = nil
				ans := p.send(t, r.leg, r.operation, body)
				if p.partner.lastHeader == nil {
					t.Fatalf("nothing was forwarded: answer %d %s", ans.status, ans.body)
				}
				got := p.partner.lastHeader.Values(CorrelationHeader)
				switch {
				case off && len(got) != 0:
					t.Fatalf("off: X-Correlation-Id %v reached the payer's system", got)
				case !off && (len(got) != 1 || got[0] != ans.corr):
					t.Fatalf("X-Correlation-Id %v, want the leg's id %q", got, ans.corr)
				}
			})
		}
	}
}
