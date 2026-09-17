// nativepas_conflict_test.go — the pas-claim-update leg's one re-issue after a payer-side
// version conflict (nativepas_conflict.go): the acceptance half (a conflict is re-issued
// exactly once and whatever the re-issue answers is relayed) and the rejection half (any
// other 409, a conflict-shaped body on another status, a second conflict, a cancelled leg
// — none of them re-issue more than the contract says). The re-issue that ends APPROVED
// and finalizes the shadow ledger is proven end to end, through the reference-payer
// mirror's own conflict window, by the platform's native PAS suite.
package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// hapiVersionConflict is the reference payer's answer when an amendment's $submit lands
// while its pend-resolution timer is writing the same ClaimResponse (observed live
// 2026-09-11): HAPI's ResourceVersionConflictException relayed as an OperationOutcome.
const hapiVersionConflict = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"HAPI-0550: HAPI-0989: Trying to update ClaimResponse/5039/_history/2 but this is not the current version"}]}`

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

// conflictPartner is a stub payer whose /Claim/$submit answers its queued responses in
// order and counts the posts it saw; a post past the queue is a test failure.
type conflictPartner struct {
	mu      sync.Mutex
	posts   int
	answers []conflictAnswer
}

type conflictAnswer struct {
	status int
	body   string
}

func newConflictPartner(t *testing.T, answers ...conflictAnswer) (*httptest.Server, *conflictPartner) {
	t.Helper()
	p := &conflictPartner{answers: answers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/Claim/$submit") {
			t.Errorf("unexpected partner call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		p.mu.Lock()
		i := p.posts
		p.posts++
		p.mu.Unlock()
		if i >= len(p.answers) {
			t.Errorf("partner post #%d past the queued %d answer(s): the responder re-issued more than once", i+1, len(p.answers))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(p.answers[i].status)
		_, _ = w.Write([]byte(p.answers[i].body))
	}))
	t.Cleanup(srv.Close)
	return srv, p
}

func (p *conflictPartner) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.posts
}

// updateLegUnderTest wires a responder at the stub partner with a pend recorded for the
// vendored conformant update golden, so handlePASClaimUpdateNative reaches the $submit post.
func updateLegUnderTest(t *testing.T, srv *httptest.Server) (*nativeResponder, []byte, string) {
	t.Helper()
	golden := readConformantGolden(t, "pas-update-request.json")
	f, status, msg := parseConformantPASUpdateFacts(golden)
	if status != 0 {
		t.Fatalf("golden update bundle rejected: %d %s", status, msg)
	}
	store := NewMemStore()
	const pci = "pci:conflict-leg"
	if err := store.RecordPendedClaim(pci, f.relatedClaim); err != nil {
		t.Fatal(err)
	}
	n := NewNativeResponder(srv.Client(), srv.URL, "", store, nil)
	return n, golden, pci
}

func fastConflictRetry(t *testing.T) {
	t.Helper()
	prev := payerVersionConflictRetryDelay
	payerVersionConflictRetryDelay = time.Millisecond
	t.Cleanup(func() { payerVersionConflictRetryDelay = prev })
}

func TestPASUpdate_PayerVersionConflict_ReissuedOnceAndTheReissueRelayed(t *testing.T) {
	fastConflictRetry(t)
	second := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"amendment still insufficient (partner)"}]}`
	srv, p := newConflictPartner(t, conflictAnswer{http.StatusConflict, hapiVersionConflict}, conflictAnswer{http.StatusUnprocessableEntity, second})
	n, golden, pci := updateLegUnderTest(t, srv)
	lr, err := n.handlePASClaimUpdateNative(context.Background(), "corr-conflict-1", pci, peerBody(golden), golden)
	if err != nil {
		t.Fatalf("a relayed re-issue answer is not an error return: %v", err)
	}
	if p.count() != 2 {
		t.Fatalf("posts = %d, want 2 (the conflict, then exactly one re-issue)", p.count())
	}
	if lr.Status != http.StatusUnprocessableEntity || string(responseBytes(lr)) != second {
		t.Fatalf("the re-issue's answer must be the one relayed: status=%d body=%s", lr.Status, responseBytes(lr))
	}
	if lr.Rollback == nil {
		t.Fatal("a relayed non-2xx after Begin must arm Rollback")
	}
}

func TestPASUpdate_PayerVersionConflict_SecondConflictRelayedNotReissuedAgain(t *testing.T) {
	fastConflictRetry(t)
	secondConflict := strings.Replace(hapiVersionConflict, "_history/2", "_history/3", 1)
	srv, p := newConflictPartner(t, conflictAnswer{http.StatusConflict, hapiVersionConflict}, conflictAnswer{http.StatusConflict, secondConflict})
	n, golden, pci := updateLegUnderTest(t, srv)
	lr, err := n.handlePASClaimUpdateNative(context.Background(), "corr-conflict-3", pci, peerBody(golden), golden)
	if err != nil {
		t.Fatal(err)
	}
	if p.count() != 2 {
		t.Fatalf("posts = %d, want exactly 2: one re-issue, never a loop", p.count())
	}
	if lr.Status != http.StatusConflict || string(responseBytes(lr)) != secondConflict || lr.Rollback == nil {
		t.Fatalf("the second conflict is relayed as-is with Rollback armed: status=%d rollback=%v body=%s", lr.Status, lr.Rollback != nil, responseBytes(lr))
	}
}

func TestPASUpdate_Other409_RelayedWithoutReissue(t *testing.T) {
	fastConflictRetry(t)
	other := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"Duplicate submission"}]}`
	srv, p := newConflictPartner(t, conflictAnswer{http.StatusConflict, other})
	n, golden, pci := updateLegUnderTest(t, srv)
	lr, err := n.handlePASClaimUpdateNative(context.Background(), "corr-conflict-4", pci, peerBody(golden), golden)
	if err != nil {
		t.Fatal(err)
	}
	if p.count() != 1 {
		t.Fatalf("posts = %d, want 1: a 409 that is not a store version conflict is relayed on the first answer", p.count())
	}
	if lr.Status != http.StatusConflict || string(responseBytes(lr)) != other || lr.Rollback == nil {
		t.Fatalf("status=%d rollback=%v body=%s", lr.Status, lr.Rollback != nil, responseBytes(lr))
	}
}

func TestPASUpdate_ConflictShapeOnOtherStatus_RelayedWithoutReissue(t *testing.T) {
	fastConflictRetry(t)
	srv, p := newConflictPartner(t, conflictAnswer{http.StatusUnprocessableEntity, hapiVersionConflict})
	n, golden, pci := updateLegUnderTest(t, srv)
	lr, err := n.handlePASClaimUpdateNative(context.Background(), "corr-conflict-5", pci, peerBody(golden), golden)
	if err != nil {
		t.Fatal(err)
	}
	if p.count() != 1 {
		t.Fatalf("posts = %d, want 1: the re-issue is keyed on HTTP 409 AND the conflict body, never the body alone", p.count())
	}
	if lr.Status != http.StatusUnprocessableEntity || lr.Rollback == nil {
		t.Fatalf("status=%d rollback=%v", lr.Status, lr.Rollback != nil)
	}
}

func TestPASUpdate_PayerVersionConflict_CancelledLegNeverReissues(t *testing.T) {
	prev := payerVersionConflictRetryDelay
	payerVersionConflictRetryDelay = 2 * time.Second
	t.Cleanup(func() { payerVersionConflictRetryDelay = prev })
	srv, p := newConflictPartner(t, conflictAnswer{http.StatusConflict, hapiVersionConflict})
	n, golden, pci := updateLegUnderTest(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	lr, err := n.handlePASClaimUpdateNative(ctx, "corr-conflict-6", pci, peerBody(golden), golden)
	if err == nil {
		t.Fatal("a leg cancelled while waiting to re-issue is an error return, not a relay")
	}
	if p.count() != 1 {
		t.Fatalf("posts = %d, want 1: no re-issue after cancellation", p.count())
	}
	if lr.Rollback == nil {
		t.Fatal("the cancelled leg must still release the claim")
	}
	if time.Since(start) >= 2*time.Second {
		t.Fatal("the wait must end at cancellation, not at the full delay")
	}
}
