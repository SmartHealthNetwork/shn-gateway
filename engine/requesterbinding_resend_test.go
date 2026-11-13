//go:build !shn_coreonly

package engine

// requesterbinding_resend_test.go — the one resend of a requester-side
// binding record (PARTICIPANT_PROTOCOL §6.1c "Sending it"): a record the Hub
// answers 503 with X-SHN-Delivered: no, its statement that it never took it,
// is sent again once, the same request byte for byte, after its Retry-After
// bounded to 1-3 s, when the wait and a send fit the report's budget. Every
// other answer is final, a 503 without that header among them, and so is a
// second refusal.

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// notTaken is the Hub's 503 stating it never took the record, as its
// readiness refusal answers: X-SHN-Delivered: no, with retryAfter when set.
func notTaken(retryAfter ...string) bindingAnswer {
	h := http.Header{HubDeliveredHeader: {"no"}}
	if len(retryAfter) > 0 {
		h["Retry-After"] = retryAfter
	}
	return bindingAnswer{status: http.StatusServiceUnavailable, header: h}
}

// waits records the waits a report asked for before a resend, standing in for
// them so a row does not sleep.
type waits struct {
	mu sync.Mutex
	d  []time.Duration
}

func (w *waits) wait(ctx context.Context, d time.Duration) bool {
	w.mu.Lock()
	w.d = append(w.d, d)
	w.mu.Unlock()
	return ctx.Err() == nil
}

func (w *waits) got() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.d...)
}

// wantSamePosts checks every post the Hub got is the same request: the same
// record body, byte for byte, under the same holder assertion, which binds it.
func wantSamePosts(t *testing.T, p *readPayer, posts []postedBinding) {
	t.Helper()
	for i, post := range posts {
		wantBodyBoundAssertion(t, post, p.sign)
		if i > 0 && (!bytes.Equal(post.body, posts[0].body) || post.assertion != posts[0].assertion) {
			t.Fatalf("post %d is not the first's request: body %s assertion %q, want %s %q", i+1, post.body, post.assertion, posts[0].body, posts[0].assertion)
		}
	}
}

// The resend's rows, at every level, the read's answer the provider's Task in
// each: a 503 stating the record was never taken, then a 204, is two posts of
// the same request and accepted, resent; two such 503s are two posts and the
// second is final, refused by the Hub with 503, resent; a 503 without the
// header (a load balancer's own), or stating the record was delivered or may
// have been, is one post and final; X-SHN-Delivered: no on any other status,
// or twice, is one post and final; and a budget too short for the wait and a
// send is one post and final, with no wait. The wait is the Retry-After bounded
// to 1-3 s: absent, unreadable, zero or repeated is 1 s, 2 is 2 s, and 9 or
// any number above 3 is 3 s.
func TestRequesterBindingResend(t *testing.T) {
	refused503 := RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: http.StatusServiceUnavailable}
	for _, level := range allLevels {
		for _, row := range []struct {
			name    string
			answers []bindingAnswer
			budget  time.Duration
			want    RequesterBindingRecord
			posts   int
			waits   []time.Duration
		}{
			{name: "not taken, then 204", answers: []bindingAnswer{notTaken(), {status: http.StatusNoContent}},
				want: RequesterBindingRecord{Outcome: RequesterBindingAccepted, Resent: true}, posts: 2, waits: []time.Duration{time.Second}},
			{name: "not taken twice", answers: []bindingAnswer{notTaken("1"), notTaken("1")},
				want: RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: 503, Resent: true}, posts: 2, waits: []time.Duration{time.Second}},
			{name: "not taken, then 409", answers: []bindingAnswer{notTaken(), {status: http.StatusConflict}},
				want: RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: 409, Resent: true}, posts: 2, waits: []time.Duration{time.Second}},
			{name: "503 without the header", answers: []bindingAnswer{{status: http.StatusServiceUnavailable, header: http.Header{"Retry-After": {"1"}}}, {status: http.StatusNoContent}},
				want: refused503, posts: 1},
			{name: "503 delivered unknown", answers: []bindingAnswer{{status: http.StatusServiceUnavailable, header: http.Header{HubDeliveredHeader: {"unknown"}}}, {status: http.StatusNoContent}},
				want: refused503, posts: 1},
			{name: "503 delivered yes", answers: []bindingAnswer{{status: http.StatusServiceUnavailable, header: http.Header{HubDeliveredHeader: {"yes"}}}, {status: http.StatusNoContent}},
				want: refused503, posts: 1},
			{name: "503 stating no twice", answers: []bindingAnswer{{status: http.StatusServiceUnavailable, header: http.Header{HubDeliveredHeader: {"no", "no"}}}, {status: http.StatusNoContent}},
				want: refused503, posts: 1},
			{name: "502 not delivered", answers: []bindingAnswer{{status: http.StatusBadGateway, header: http.Header{HubDeliveredHeader: {"no"}}}, {status: http.StatusNoContent}},
				want: RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: 502}, posts: 1},
			{name: "429 not delivered", answers: []bindingAnswer{{status: http.StatusTooManyRequests, header: http.Header{HubDeliveredHeader: {"no"}}}, {status: http.StatusNoContent}},
				want: RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: 429}, posts: 1},
			{name: "not taken, the budget too short for the wait and a send", answers: []bindingAnswer{notTaken(), {status: http.StatusNoContent}},
				budget: 700 * time.Millisecond, want: refused503, posts: 1},
			{name: "not taken, the budget too short for Retry-After 3", answers: []bindingAnswer{notTaken("3"), {status: http.StatusNoContent}},
				budget: 2500 * time.Millisecond, want: refused503, posts: 1},
			{name: "not taken, Retry-After 9 waits 3 s", answers: []bindingAnswer{notTaken("9"), {status: http.StatusNoContent}},
				want: RequesterBindingRecord{Outcome: RequesterBindingAccepted, Resent: true}, posts: 2, waits: []time.Duration{3 * time.Second}},
			{name: "not taken, Retry-After 2 waits 2 s", answers: []bindingAnswer{notTaken("2"), {status: http.StatusNoContent}},
				want: RequesterBindingRecord{Outcome: RequesterBindingAccepted, Resent: true}, posts: 2, waits: []time.Duration{2 * time.Second}},
			{name: "not taken, Retry-After unreadable waits 1 s", answers: []bindingAnswer{notTaken("soon"), {status: http.StatusNoContent}},
				want: RequesterBindingRecord{Outcome: RequesterBindingAccepted, Resent: true}, posts: 2, waits: []time.Duration{time.Second}},
		} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := newReadPayer(t, level)
				var w waits
				p.g.resendWaitForTest = w.wait
				if row.budget > 0 {
					p.g.bindingBudgetForTest = row.budget
				}
				task := readTaskAnswer(t, nil)
				p.hub.answerBody, p.hub.answerSubject = task, "pci:00000000000000000000000000000d15"
				p.hub.bindingAnswers = row.answers
				if rec := p.get(t, "/cdex/provider/"+readProviderID+"/Task/t-1"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), task) {
					t.Fatalf("answer %d %s, want the provider's Task whatever became of the record", rec.Code, rec.Body)
				}
				_, _, posts := p.hub.snapshot()
				if len(posts) != row.posts {
					t.Fatalf("the Hub got %d records, want %d", len(posts), row.posts)
				}
				wantSamePosts(t, p, posts)
				if got := w.got(); !equalDurations(got, row.waits) {
					t.Fatalf("waited %v before a resend, want %v", got, row.waits)
				}
				want := row.want
				want.Binding, want.Involvement = shnsdk.RequesterBindingBound, shnsdk.InvolvementRequesterDerived
				if x := p.recs.only(t); x.RequesterBinding == nil || *x.RequesterBinding != want {
					t.Fatalf("requester binding %+v, want %+v", x.RequesterBinding, want)
				}
			})
		}
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The wait itself, with no stand-in: a record the Hub never took is sent
// again no sooner than the 1 s its absent Retry-After reads as, and accepted.
func TestRequesterBindingResendWaits(t *testing.T) {
	p := newReadPayer(t, EnforcementNone)
	task := readTaskAnswer(t, nil)
	p.hub.answerBody, p.hub.answerSubject = task, "pci:00000000000000000000000000000d15"
	p.hub.bindingAnswers = []bindingAnswer{notTaken(), {status: http.StatusNoContent}}
	start := time.Now()
	if rec := p.get(t, "/cdex/provider/"+readProviderID+"/Task/t-1"); rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body)
	}
	if took := time.Since(start); took < time.Second {
		t.Fatalf("the resend came after %v, want at least the 1 s wait", took)
	}
	_, _, posts := p.hub.snapshot()
	if len(posts) != 2 {
		t.Fatalf("the Hub got %d records, want 2", len(posts))
	}
	wantSamePosts(t, p, posts)
	want := RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: shnsdk.InvolvementRequesterDerived, Outcome: RequesterBindingAccepted, Resent: true}
	if x := p.recs.only(t); x.RequesterBinding == nil || *x.RequesterBinding != want {
		t.Fatalf("requester binding %+v, want %+v", x.RequesterBinding, want)
	}
}

// A stop during the wait before a resend cuts it within the stop's documented
// step, at every level: the Hub stated it never took the record, so it is
// other, not unknown, and it is not sent again.
func TestRequesterBindingResendStopMidWait(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newReadPayer(t, level)
			task := readTaskAnswer(t, nil)
			p.hub.answerBody, p.hub.answerSubject = task, "pci:00000000000000000000000000000d15"
			p.hub.bindingAnswers = []bindingAnswer{notTaken("3"), {status: http.StatusNoContent}}
			if rec := p.getNoWait(t, "/cdex/provider/"+readProviderID+"/Task/t-1"); rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, _, posts := p.hub.snapshot(); len(posts) > 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			// The first answer read, the report is waiting.
			time.Sleep(50 * time.Millisecond)
			start := time.Now()
			p.g.stopAfterAnswers()
			// The wait ends with the stop's cancel, after its first step, so
			// the report returns at once: never the forced second step.
			if bound := afterAnswerStopWait + 300*time.Millisecond; time.Since(start) > bound {
				t.Fatalf("the stop took %v, over its first step: the wait did not end with the stop", time.Since(start))
			}
			if _, _, posts := p.hub.snapshot(); len(posts) != 1 {
				t.Fatalf("the Hub got %d records, want the first alone", len(posts))
			}
			want := RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: shnsdk.InvolvementRequesterDerived, Outcome: ExchangeOther}
			if x := p.recs.only(t); x.RequesterBinding == nil || *x.RequesterBinding != want {
				t.Fatalf("requester binding %+v, want %+v", x.RequesterBinding, want)
			}
		})
	}
}

// The wait's bounds, by the Retry-After alone.
func TestRequesterBindingResendWait(t *testing.T) {
	for _, row := range []struct {
		values []string
		want   time.Duration
	}{
		{nil, time.Second},
		{[]string{"0"}, time.Second},
		{[]string{"1"}, time.Second},
		{[]string{"2"}, 2 * time.Second},
		{[]string{"3"}, 3 * time.Second},
		{[]string{"4"}, 3 * time.Second},
		{[]string{"9"}, 3 * time.Second},
		{[]string{"99999999999999999999999"}, 3 * time.Second},
		{[]string{"-1"}, time.Second},
		{[]string{"1.5"}, time.Second},
		{[]string{" 2"}, time.Second},
		{[]string{"Wed, 11 Nov 2026 12:00:00 GMT"}, time.Second},
		{[]string{"2", "2"}, time.Second},
	} {
		h := http.Header{}
		if row.values != nil {
			h["Retry-After"] = row.values
		}
		if got := requesterBindingResendWait(h); got != row.want {
			t.Errorf("Retry-After %q: %v, want %v", row.values, got, row.want)
		}
	}
}

// A record the Hub stated it never took is settled as other, not unknown,
// when a stop writes the exchange record before its resend: nothing of it was
// the Hub's. Once it is written again, it is unknown.
func TestRequesterBindingNotTakenSettlesOther(t *testing.T) {
	bound := RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: shnsdk.InvolvementRequesterHeld}
	x := &exchangeRecorder{}
	x.requesterBindingPending(bound)
	x.requesterBindingMaySend()
	x.requesterBindingNotTaken()
	want := bound
	want.Outcome = ExchangeOther
	if got := x.settledRequesterBinding(); got == nil || *got != want {
		t.Fatalf("waiting to resend: %+v, want %+v", got, want)
	}
	resent := bound
	resent.Resent = true
	x.requesterBindingPending(resent)
	x.requesterBindingMaySend()
	want = resent
	want.Outcome = RequesterBindingUnknown
	if got := x.settledRequesterBinding(); got == nil || *got != want {
		t.Fatalf("resent, no answer: %+v, want %+v", got, want)
	}
}

// A stop forced past its second step while the report waits to resend (a
// wait that does not end with the cancel stands in for one that has not yet
// seen it) records the report as other, not unknown: the Hub stated it never
// took the record, so none of it is the Hub's until it is sent again.
func TestRequesterBindingResendForcedMidWaitIsOther(t *testing.T) {
	p := newReadPayer(t, allLevels[0])
	task := readTaskAnswer(t, nil)
	p.hub.answerBody, p.hub.answerSubject = task, "pci:00000000000000000000000000000d15"
	p.hub.bindingAnswers = []bindingAnswer{notTaken("1"), {status: http.StatusNoContent}}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	p.g.resendWaitForTest = func(context.Context, time.Duration) bool { <-release; return false }
	if rec := p.getNoWait(t, "/cdex/provider/"+readProviderID+"/Task/t-1"); rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, posts := p.hub.snapshot(); len(posts) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	p.g.stopAfterAnswers()
	want := RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: shnsdk.InvolvementRequesterDerived, Outcome: ExchangeOther}
	if x := p.recs.only(t); x.RequesterBinding == nil || *x.RequesterBinding != want {
		t.Fatalf("requester binding %+v, want %+v", x.RequesterBinding, want)
	}
}

// The resend is asked only when the wait and another send, timed as the
// first took, end before the report's deadline: the first send's time counts.
func TestResendRequesterBindingCountsTheFirstSendsTime(t *testing.T) {
	h := http.Header{}
	h.Set(HubDeliveredHeader, "no")
	h.Set("Retry-After", "1")
	for _, c := range []struct {
		left, took time.Duration
		want       bool
	}{
		{left: 3 * time.Second, took: 500 * time.Millisecond, want: true},
		{left: 1400 * time.Millisecond, took: 500 * time.Millisecond, want: false},
		{left: 1500 * time.Millisecond, took: 0, want: true},
	} {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(c.left))
		_, ok := resendRequesterBinding(ctx, http.StatusServiceUnavailable, h, nil, c.took)
		cancel()
		if ok != c.want {
			t.Errorf("left %v, took %v: resend %v, want %v", c.left, c.took, ok, c.want)
		}
	}
}
