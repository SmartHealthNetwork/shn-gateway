package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer gateway frames its system's own 2xx answer with that status, for a
// requester that decodes frames; one that does not gets the answer bare, which
// it reads as 200.
func TestRespondLeg_FramesTheParticipantsStatus(t *testing.T) {
	answer := []byte(`{"resourceType":"ClaimResponse","status":"active"}`)
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		body := answer
		if status == http.StatusNoContent {
			body = nil
		}
		for _, framed := range []bool{true, false} {
			g, requester := newInboundTestGateway(t, framed)
			rec := httptest.NewRecorder()
			g.respondLeg(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select",
				"corr-1", relayedResponse(body), status, "pci-1", requester.ID, "", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("%d framed=%v: to-Hub status %d, want 200", status, framed, rec.Code)
			}
			payload := openResponseLeg(t, requester, rec.Body.Bytes())
			if !framed {
				if shnsdk.IsFramed(payload) || !bytes.Equal(payload, body) {
					t.Fatalf("%d: a requester without frames got %q, want the answer bare", status, payload)
				}
				continue
			}
			hdr, got, err := shnsdk.DecodeHTTPFrame(payload)
			if err != nil {
				t.Fatalf("%d: decode frame: %v", status, err)
			}
			if hdr.Status != status || !bytes.Equal(got, body) {
				t.Fatalf("framed %d %q, want %d and the answer exactly", hdr.Status, got, status)
			}
		}
	}
}

// A success a responder signals on the error path is answered on the success
// seal by the same rule as any other: the status its relayed answer was given
// (AnswerStatus), never the signal itself.
func TestRespondLegError_2xxKeepsTheAnswersStatus(t *testing.T) {
	answer := []byte(`{"resourceType":"ClaimResponse","status":"active"}`)
	for _, row := range []struct {
		name string
		r    LegResult
		want int
	}{
		{"relayed, given 201", LegResult{Status: http.StatusCreated, AnswerStatus: http.StatusCreated, Response: relayedResponse(answer)}, http.StatusCreated},
		{"relayed, given none", LegResult{Status: http.StatusCreated, Response: relayedResponse(answer)}, http.StatusOK},
		{"built by the gateway", LegResult{Status: http.StatusCreated, AnswerStatus: http.StatusCreated, Response: testResponse(answer)}, http.StatusOK},
	} {
		g, requester := newInboundTestGateway(t, true)
		rec := httptest.NewRecorder()
		g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select",
			"corr-1", row.r, "pci-1", requester.ID, "", "")
		hdr, got, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: decode frame: %v", row.name, err)
		}
		if hdr.Status != row.want || !bytes.Equal(got, answer) {
			t.Fatalf("%s: framed %d %q, want %d and the answer exactly", row.name, hdr.Status, got, row.want)
		}
	}
}

// The status a payer gateway frames is its system's own only for an answer
// relayed from that system; an answer it built itself is 200, and a status
// that is not a success is never framed as one.
func TestLegResult_AnswerStatus(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle"}`)
	built, err := relay.Authored(relay.BuilderSDKPatientDTR, body, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name string
		r    LegResult
		want int
	}{
		{"relayed, 201", LegResult{Response: relayedResponse(body), AnswerStatus: http.StatusCreated}, http.StatusCreated},
		{"relayed, 204", LegResult{Response: relayedResponse(nil), AnswerStatus: http.StatusNoContent}, http.StatusNoContent},
		{"relayed, none stated", LegResult{Response: relayedResponse(body)}, http.StatusOK},
		{"relayed, not a success", LegResult{Response: relayedResponse(body), AnswerStatus: http.StatusNotFound}, http.StatusOK},
		{"built by the gateway", LegResult{Response: built, AnswerStatus: http.StatusCreated}, http.StatusOK},
	} {
		if got := row.r.answerStatus(); got != row.want {
			t.Errorf("%s: answerStatus %d, want %d", row.name, got, row.want)
		}
	}
}

// The provider gateway writes a framed answer to its participant's system with
// the status the frame states; a bare answer, or a frame from a payer gateway
// that framed every success as 200, is written 200.
func TestCRDIngress_WritesTheAnswersStatus(t *testing.T) {
	answer := []byte(`{"cards":[]}`)
	for _, row := range []struct {
		name string
		lr   LegResult
		want int
		body []byte
	}{
		{"framed 201", LegResult{Response: testResponse(answer), AnswerStatus: http.StatusCreated}, http.StatusCreated, answer},
		{"framed 202", LegResult{Response: testResponse(answer), AnswerStatus: http.StatusAccepted}, http.StatusAccepted, answer},
		{"framed 200", LegResult{Response: testResponse(answer), AnswerStatus: http.StatusOK}, http.StatusOK, answer},
		{"bare", LegResult{Response: testResponse(answer)}, http.StatusOK, answer},
	} {
		t.Run(row.name, func(t *testing.T) {
			env := newInProcessExchange(t)
			env.payerReturns(row.lr)
			rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			if rec.Code != row.want || !bytes.Equal(rec.Body.Bytes(), row.body) {
				t.Fatalf("EHR got %d %q, want %d %q", rec.Code, rec.Body.Bytes(), row.want, row.body)
			}
		})
	}
}

// A frame stating 204 No Content that carries a body says two things: the
// provider's system could be written the status or the bytes, not both. The
// answer is not read, at every level, and the refusal is integrity.
func TestCRDIngress_NoContentWithABodyIsRefused(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			env.payerReturns(LegResult{Response: testResponse([]byte(`{"cards":[]}`)), AnswerStatus: http.StatusNoContent})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCRD)(rec, crdIngressPost(conformantCRDRequest("MBR-COVERED")))
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "response frame states 204 No Content and carries a body") {
				t.Fatalf("EHR got %d %s", rec.Code, rec.Body.String())
			}
			wantRefusal(t, got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalIntegrity)
		})
	}
	t.Run("an empty 204 is carried", func(t *testing.T) {
		env := newInProcessExchange(t)
		env.originator.cfg.ConformanceEnforcement = EnforcementNone
		env.payerReturns(LegResult{Response: testResponse(nil), AnswerStatus: http.StatusNoContent})
		rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("EHR got %d %q, want 204 and no body", rec.Code, rec.Body.Bytes())
		}
	})
}
