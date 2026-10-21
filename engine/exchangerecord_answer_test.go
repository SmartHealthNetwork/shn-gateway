package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// wantGatewayRefusalAnswer asserts rec records the answer as the gateway's
// own refusal, and no request the gateway sent.
func wantGatewayRefusalAnswer(t *testing.T, rec ExchangeRecord) {
	t.Helper()
	if rec.AnswerOwnership != "authored" || rec.AnswerBuilder != string(relay.BuilderGatewayRefusal) || len(rec.AnswerEdits) != 0 {
		t.Fatalf("answer %q by %q edits %v, want the gateway's own refusal", rec.AnswerOwnership, rec.AnswerBuilder, rec.AnswerEdits)
	}
	if rec.Ownership != "" || rec.Builder != "" || len(rec.RequestEdits) != 0 || len(rec.Edits) != 0 {
		t.Fatalf("request %q by %q edits %v / %v, want none sent", rec.Ownership, rec.Builder, rec.RequestEdits, rec.Edits)
	}
}

// The answer an inbound leg's gateway refuses with, sealed for the network,
// is recorded as its own, and the forward it never sent as nothing, at every
// level.
func TestExchangeRecord_InboundRefusalIsTheGatewaysAnswer(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			ans := p.send(t, "crd-order-select", "", []byte(`{`))
			if ans.status != http.StatusBadRequest || !ans.framed {
				t.Fatalf("answer %d framed=%v %s, want a framed 400", ans.status, ans.framed, ans.body)
			}
			rec := got.only(t)
			if rec.Direction != DirectionInbound || rec.Outcome != ExchangeRefused {
				t.Fatalf("record %s %s", rec.Direction, rec.Outcome)
			}
			wantGatewayRefusalAnswer(t, rec)
		})
	}
}

// A refusal at the ingress before any leg is opened is an answer the gateway
// wrote its participant's system: recorded as its own, at every level.
func TestExchangeRecord_IngressRefusalBeforeALegIsTheGatewaysAnswer(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			got, _ := recording(env)
			if rec := ingressCRD(env, []byte(`{`)); rec.Code < http.StatusBadRequest {
				t.Fatalf("answer %d %s, want a refusal", rec.Code, rec.Body)
			}
			if env.routeHitCount() != 0 {
				t.Fatal("a leg was opened; the row proves nothing")
			}
			rec := got.only(t)
			if rec.Direction != DirectionIngress || rec.Outcome != ExchangeRefused {
				t.Fatalf("record %s %s", rec.Direction, rec.Outcome)
			}
			wantGatewayRefusalAnswer(t, rec)
		})
	}
}

// An answer is recorded only once the transmit check admitted it: an edited
// answer the ownership table refuses leaves no trace of its edit, and what
// was written in its place, the gateway's own fault answer, is recorded.
func TestExchangeRecord_ARefusedAnswerIsNotRecorded(t *testing.T) {
	edited := newBoundaryPayloads(t, relay.BuilderGatewayRefusal, `{}`).edited
	k := answerKey("crd-order-select", relay.OutcomeAnswered)
	t.Run("the payload writer", func(t *testing.T) {
		x := &exchangeRecorder{}
		rec := httptest.NewRecorder()
		(&Gateway{}).writePayload(&exchangeWriter{ResponseWriter: rec, x: x}, http.StatusOK, "application/json", edited, k)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("answer %d %s, want the ownership fault", rec.Code, rec.Body)
		}
		if x.rec.AnswerOwnership != "authored" || x.rec.AnswerBuilder != string(relay.BuilderGatewayRefusal) || len(x.rec.AnswerEdits) != 0 || len(x.rec.Edits) != 0 {
			t.Fatalf("answer %q by %q edits %v / %v, want the fault answer and no edit", x.rec.AnswerOwnership, x.rec.AnswerBuilder, x.rec.AnswerEdits, x.rec.Edits)
		}
	})
	t.Run("the answer sealed for the network", func(t *testing.T) {
		x := &exchangeRecorder{}
		r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil)
		r = r.WithContext(context.WithValue(r.Context(), exchangeRecorderKey{}, x))
		if _, status, _ := (&Gateway{}).buildResponseLeg(r, "", "", "crd-order-select", "corr-1", edited, k, nil, "", "", ""); status != http.StatusInternalServerError {
			t.Fatalf("status %d, want the ownership fault", status)
		}
		if x.rec.AnswerOwnership != "" || len(x.rec.AnswerEdits) != 0 || len(x.rec.Edits) != 0 {
			t.Fatalf("answer %q edits %v / %v recorded for an answer never sent", x.rec.AnswerOwnership, x.rec.AnswerEdits, x.rec.Edits)
		}
	})
}

// An answer the gateway builds itself names its builder: a payer gateway
// answering coverage eligibility from the payer's records, at every level.
func TestExchangeRecord_TheGatewaysOwnAnswerNamesItsBuilder(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			if ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember)); ans.status != http.StatusOK {
				t.Fatalf("answer %d %s", ans.status, ans.body)
			}
			rec := got.only(t)
			if rec.AnswerOwnership != "authored" || rec.AnswerBuilder != string(relay.BuilderSDKEligibility) || len(rec.AnswerEdits) != 0 {
				t.Fatalf("answer %q by %q edits %v, want authored by %s", rec.AnswerOwnership, rec.AnswerBuilder, rec.AnswerEdits, relay.BuilderSDKEligibility)
			}
			if rec.Ownership != "" || len(rec.RequestEdits) != 0 {
				t.Fatalf("forward %q edits %v, want none: the payer's own system was not called", rec.Ownership, rec.RequestEdits)
			}
		})
	}
}
