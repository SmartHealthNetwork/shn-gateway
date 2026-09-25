package engine

// pas_ledger_write_test.go — a record this payer gateway could not write never
// withholds its payer's answer. The submit and update legs are pinned in
// TestPASInboundCommitOrdering ("store failure"); the inquiry leg is pinned here,
// with the failure classes the operator is told about.

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestPASInquire_FailedRecordStillRelaysTheAnswer(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	answer := inquiryFixture(t, "pas-inquiry-response-2.0.json")
	rollbacks := 0
	g.cfg.Responder = pasResultResponder{result: LegResult{
		Response: testResponse(answer), ResponseSubjectForeign: true,
		Commit:   func() error { return fmt.Errorf("store unavailable: private-store-sentinel") },
		Rollback: func() { rollbacks++ },
	}}
	var failed []ObserverEvent
	g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == LocalWriteFailedEvent {
			failed = append(failed, e)
		}
	}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-inquire-write-failed", requester.ID
	tok := shnsdk.Token{Subject: pci, CorrelationID: env.Metadata.CorrelationID}
	rec := httptest.NewRecorder()
	g.handlePASInquireInbound(rec, newSignedInboundRequest(t, g, requester.ID), env, tok, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), "pa.pas@2.0")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the answer leg; body %s", rec.Code, rec.Body.String())
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
	if err != nil || hdr.Status != http.StatusOK || !bytes.Equal(body, answer) {
		t.Fatalf("the payer's answer must reach the requester unchanged: %v %d", err, hdr.Status)
	}
	if rollbacks != 1 {
		t.Fatalf("rollbacks = %d, want 1: an unrecorded answer releases what the responder acquired", rollbacks)
	}
	if len(failed) != 1 || failed[0].CorrelationID != env.Metadata.CorrelationID || strings.Contains(failed[0].Detail, "sentinel") {
		t.Fatalf("the operator must be told, without the store's text: %+v", failed)
	}
}

func TestLocalWriteFailure_NamesTheClassNeverTheText(t *testing.T) {
	for _, row := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", ErrPendKeyTooLong), "longer than the ledger keeps"},
		{fmt.Errorf("wrapped: %w", ErrPendRequesterMismatch), "another requester"},
		{ErrPendRequesterRequired, "no requester"},
		{fmt.Errorf("wrapped: %w", ErrPendEOBInvalid), "decision EOB"},
		{errors.New("pgx: duplicate key value (subject_pci)=(pci:private)"), "the store did not accept the write"},
	} {
		got := localWriteFailure(row.err)
		if !strings.Contains(got, row.want) || strings.Contains(got, "pci:private") {
			t.Errorf("localWriteFailure(%v) = %q, want it to name %q and nothing of the error's text", row.err, got, row.want)
		}
	}
}
