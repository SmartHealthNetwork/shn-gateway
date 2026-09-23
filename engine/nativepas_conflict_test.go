// Native PAS updates relay the first payer reply once. Conflict classification
// remains covered independently for any future explicit caller-owned retry.
package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hapiVersionConflict is the reference payer's answer when an amendment's $submit lands
// while its pend-resolution timer is writing the same ClaimResponse (observed live
// 2026-09-11): HAPI's ResourceVersionConflictException relayed as an OperationOutcome.
const hapiVersionConflict = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"HAPI-0550: HAPI-0989: Trying to update ClaimResponse/5039/_history/2 but this is not the current version"}]}`

type conflictAnswer struct {
	status int
	body   string
}

func TestIsPayerVersionConflict(t *testing.T) {
	rows := []struct {
		name string
		body string
		want bool
	}{
		{"HAPI ResourceVersionConflictException, as observed live", hapiVersionConflict, true},
		{"the FHIR conflict issue code", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"conflict","diagnostics":"version mismatch"}]}`, true},
		{"HAPI diagnostics without the code prefix", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"Trying to update ClaimResponse/1/_history/3 but this is not the current version"}]}`, true},
		{"an unrelated processing failure", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"Failure to submit prior auth"}]}`, false},
		{"the derived-ledger refusal text is not a store conflict", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"ClaimUpdate references no pending claim available for this patient"}]}`, false},
		{"not an OperationOutcome", `{"resourceType":"Bundle","issue":[{"code":"conflict"}]}`, false},
		{"not JSON", `HAPI-0989 not the current version`, false},
		{"empty", ``, false},
		{"no issues", `{"resourceType":"OperationOutcome"}`, false},
	}
	for _, row := range rows {
		if got := isPayerVersionConflict([]byte(row.body)); got != row.want {
			t.Errorf("%s: isPayerVersionConflict = %v, want %v", row.name, got, row.want)
		}
	}
}

// Parser classification above remains independent of native dispatch. An explicit
// caller-owned retry action is not implemented here (PCV-07/08).
func TestPASUpdate_ConflictRepliesAreFirstReplyOnly(t *testing.T) {
	second := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"amendment still insufficient (partner)"}]}`
	secondConflict := strings.Replace(hapiVersionConflict, "_history/2", "_history/3", 1)
	other := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"Duplicate submission"}]}`
	for _, row := range []struct {
		name   string
		first  conflictAnswer
		queued *conflictAnswer
	}{
		{"version conflict then queued 422", conflictAnswer{http.StatusConflict, hapiVersionConflict}, &conflictAnswer{http.StatusUnprocessableEntity, second}},
		{"version conflict then queued second conflict", conflictAnswer{http.StatusConflict, hapiVersionConflict}, &conflictAnswer{http.StatusConflict, secondConflict}},
		{"other 409", conflictAnswer{http.StatusConflict, other}, nil},
		{"conflict body on 422", conflictAnswer{http.StatusUnprocessableEntity, hapiVersionConflict}, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			answers := []conflictAnswer{row.first}
			if row.queued != nil {
				answers = append(answers, *row.queued)
			}
			srv, payer := newRecordingPayer(t, answers...)
			golden := readConformantGolden(t, "pas-update-request.json")
			n := NewNativeResponder(srv.Client(), srv.URL, "", nativeClinicalStoreForbidden{}, fixedClock)
			lr, err := n.handlePASClaimUpdateNative(context.Background(), "corr-conflict", "pci:conflict-leg", peerBody(golden), golden)
			if err != nil {
				t.Fatal(err)
			}
			if lr.ApplicationStatus != row.first.status || lr.Status != row.first.status || lr.Response.ContentType() != "application/fhir+json" || string(responseBytes(lr)) != row.first.body || !lr.ResponseRelayed() {
				t.Fatalf("first peer reply changed: status=%d application=%d body=%q", lr.Status, lr.ApplicationStatus, responseBytes(lr))
			}
			if lr.Commit != nil || lr.Rollback != nil || len(lr.SideEffectFHIR) != 0 {
				t.Fatal("native callback or clinical side effect")
			}
			calls := payer.seen()
			if len(calls) != 1 || calls[0].method != http.MethodPost || calls[0].path != "/Claim/$submit" || !bytes.Equal(calls[0].body, golden) {
				t.Fatalf("native dispatch was not exactly one original POST: %+v", calls)
			}
			payer.refuseAnyPoll(t)
		})
	}
}

func TestPASUpdate_CancelDuringBackendRead(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Claim/$submit" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		close(entered)
		<-release
	}))
	t.Cleanup(srv.Close)
	golden := readConformantGolden(t, "pas-update-request.json")
	n := NewNativeResponder(srv.Client(), srv.URL, "", nativeClinicalStoreForbidden{}, fixedClock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { <-entered; cancel() }()
	lr, err := n.handlePASClaimUpdateNative(ctx, "corr-cancel", "pci:conflict-leg", peerBody(golden), golden)
	select {
	case <-entered:
	default:
		t.Fatal("cancellation row never reached active backend IO")
	}
	if err == nil {
		t.Fatal("cancellation during backend IO must return an error")
	}
	if lr.ApplicationStatus != 0 || len(responseBytes(lr)) != 0 || lr.Commit != nil || lr.Rollback != nil || len(lr.SideEffectFHIR) != 0 {
		t.Fatalf("cancelled backend read produced a reply or clinical effect: %+v", lr)
	}
}
