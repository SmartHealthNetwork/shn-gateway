package engine

// originate_wait_test.go — the originator's prior-authorization operation.
//
// HERMETIC TIMING. Every row that waits compresses the schedule to milliseconds
// (fastPASInquire) and drives the deadline through the INJECTED clock. Nothing
// here sleeps on wall-clock seconds: a row that did would be a defect, not a slow
// test, because it would be measuring the machine instead of the rule.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/fhirseed"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// fastPASInquire compresses the inquiry schedule for a hermetic row, the same way
// the update leg's re-issue delay is compressed (nativepas_conflict_test.go).
func fastPASInquire(t *testing.T) {
	t.Helper()
	first, cap := pasInquireFirstDelay, pasInquireBackoffCap
	pasInquireFirstDelay, pasInquireBackoffCap = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { pasInquireFirstDelay, pasInquireBackoffCap = first, cap })
}

// --- the wait's own rules ---

// waitGateway is a bare Gateway with an injected clock: enough for the wait's own
// rules, which read nothing else.
func waitGateway(clock func() time.Time) *Gateway {
	return &Gateway{cfg: Config{Clock: clock, HolderID: "provider"}}
}

func pendedDecision() PASDecision {
	return PASDecision{Decision: PASDecisionPended, Continuation: "m0-abc", ContinuationDurable: false,
		Parsed: shnsdk.PriorAuthResult{Outcome: "pended"}}
}

func approvedDecision() PASDecision {
	return PASDecision{Decision: PASDecisionApproved,
		Parsed: shnsdk.PriorAuthResult{Outcome: "approved", PreAuthRef: "AUTH-1", ValidUntil: "2030-01-01"}}
}

// TestOriginator_WaitCapsAndCancels: the wait is bounded three ways, and each
// bound is asserted on its own.
//
//   - the INQUIRY CAP holds even when the deadline and the schedule would allow
//     more. Prior Authorization asks clients not to inquire repetitively and every
//     inquiry is an audited exchange on somebody else's system.
//   - the DEADLINE stops it early, measured on the gateway's own injected clock.
//   - a CANCELLED request stops it at once, without making the inquiry it was
//     waiting to make.
//   - a wait of ZERO makes no inquiry at all.
func TestOriginator_WaitCapsAndCancels(t *testing.T) {
	fastPASInquire(t)
	frozen := time.Unix(1700000000, 0).UTC()

	t.Run("the inquiry cap holds when nothing else stops it", func(t *testing.T) {
		g := waitGateway(func() time.Time { return frozen }) // never reaches any deadline
		calls := 0
		out, status, msg, err := g.followPended(context.Background(), pendedDecision(), PASWaitMax,
			func(context.Context) (PASDecision, int, string, error) {
				calls++
				return pendedDecision(), 0, "", nil
			})
		if status != 0 || err != nil {
			t.Fatalf("reaching the bound is never an error: %d %q %v", status, msg, err)
		}
		if calls != 6 {
			t.Fatalf("made %d inquiries, want exactly 6 — the cap, not the clock", calls)
		}
		if out.Decision != PASDecisionPended || out.Inquiries != 6 {
			t.Fatalf("out = %+v, want a pend reporting its 6 inquiries", out)
		}
		if out.Continuation != "m0-abc" {
			t.Fatalf("the continuation must survive the wait: %q", out.Continuation)
		}
	})

	t.Run("the deadline stops it early, on the gateway's own clock", func(t *testing.T) {
		now := frozen
		g := waitGateway(func() time.Time { return now })
		calls := 0
		out, _, _, _ := g.followPended(context.Background(), pendedDecision(), 30*time.Second,
			func(context.Context) (PASDecision, int, string, error) {
				calls++
				// The gateway's clock crosses the deadline after the second ask.
				if calls == 2 {
					now = now.Add(31 * time.Second)
				}
				return pendedDecision(), 0, "", nil
			})
		if calls != 2 {
			t.Fatalf("made %d inquiries, want 2 — the deadline, not the cap", calls)
		}
		if out.Decision != PASDecisionPended {
			t.Fatalf("decision = %q", out.Decision)
		}
	})

	t.Run("a cancelled request stops the wait without asking again", func(t *testing.T) {
		g := waitGateway(func() time.Time { return frozen })
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		out, status, _, err := g.followPended(ctx, pendedDecision(), PASWaitMax,
			func(context.Context) (PASDecision, int, string, error) {
				calls++
				cancel()
				return pendedDecision(), 0, "", nil
			})
		if status != 0 || err != nil {
			t.Fatalf("a cancelled wait is not an error: %d %v", status, err)
		}
		if calls != 1 {
			t.Fatalf("made %d inquiries after cancellation, want 1", calls)
		}
		if out.Decision != PASDecisionPended {
			t.Fatalf("decision = %q", out.Decision)
		}
	})

	t.Run("a wait of zero makes no inquiry at all", func(t *testing.T) {
		g := waitGateway(func() time.Time { return frozen })
		calls := 0
		out, _, _, _ := g.followPended(context.Background(), pendedDecision(), 0,
			func(context.Context) (PASDecision, int, string, error) {
				calls++
				return pendedDecision(), 0, "", nil
			})
		if calls != 0 {
			t.Fatalf("made %d inquiries for a wait of zero", calls)
		}
		if out.Decision != PASDecisionPended || out.Inquiries != 0 {
			t.Fatalf("out = %+v", out)
		}
	})

	t.Run("an inquiry that could not be made leaves the pend standing", func(t *testing.T) {
		g := waitGateway(func() time.Time { return frozen })
		out, status, _, err := g.followPended(context.Background(), pendedDecision(), PASWaitMax,
			func(context.Context) (PASDecision, int, string, error) {
				return PASDecision{}, http.StatusBadGateway, "the payer could not be reached", nil
			})
		if status != 0 || err != nil {
			t.Fatalf("a failed follow-up does not erase the payer's own answer: %d %v", status, err)
		}
		if out.Decision != PASDecisionPended || out.Continuation != "m0-abc" {
			t.Fatalf("out = %+v, want the pend and its continuation", out)
		}
	})
}

// TestOriginator_WaitReturnsTheLaterDecision: the wait bound is exhausted, and
// continuing later returns the LATER decision — including a later denial. This is
// the whole point of the continuation: the answer that arrives after the caller
// stopped waiting is not lost.
func TestOriginator_WaitReturnsTheLaterDecision(t *testing.T) {
	fastPASInquire(t)
	frozen := time.Unix(1700000000, 0).UTC()
	g := waitGateway(func() time.Time { return frozen })

	// Round one: the payer has not decided, and the wait runs to its bound.
	stillPending := func(context.Context) (PASDecision, int, string, error) { return pendedDecision(), 0, "", nil }
	first, _, _, _ := g.followPended(context.Background(), pendedDecision(), PASWaitMax, stillPending)
	if first.Decision != PASDecisionPended || first.Inquiries != 6 {
		t.Fatalf("round one = %+v, want a pend at the bound", first)
	}

	// Round two, later: the same continuation, and the payer has decided.
	for _, tc := range []struct {
		name string
		then PASDecision
	}{
		{"approved", approvedDecision()},
		{"denied", PASDecision{Decision: PASDecisionDenied, Parsed: shnsdk.PriorAuthResult{
			Outcome: "denied", Denial: &shnsdk.Denial{Rationale: "Conservative therapy is not documented."}}}},
	} {
		t.Run("continuing later returns a later "+tc.name, func(t *testing.T) {
			later := tc.then
			out, status, _, err := g.followPended(context.Background(), pendedDecision(), PASWaitMax,
				func(context.Context) (PASDecision, int, string, error) { return later, 0, "", nil })
			if status != 0 || err != nil {
				t.Fatalf("%d %v", status, err)
			}
			if out.Decision != tc.then.Decision {
				t.Fatalf("decision = %q, want %q", out.Decision, tc.then.Decision)
			}
			if out.Inquiries != 1 {
				t.Fatalf("a decision on the first ask must not keep asking (%d inquiries)", out.Inquiries)
			}
			if out.Continuation != "m0-abc" {
				t.Fatal("the continuation must ride through to the decision")
			}
			resp := paInquireRespOf(out)
			if tc.name == "denied" && (!resp.Denied || resp.Rationale == "") {
				t.Fatalf("a denial must carry the payer's own rationale: %+v", resp)
			}
			if tc.name == "approved" && resp.AuthNumber != "AUTH-1" {
				t.Fatalf("an approval must carry the payer's own authorization number: %+v", resp)
			}
		})
	}
}

// TestPASWait_BoundsWhatACallerMayAskFor: the wait is clamped, and the literals
// are pinned here rather than compared against the constants the code reads.
func TestPASWait_BoundsWhatACallerMayAskFor(t *testing.T) {
	if PASWaitDefault != 0 {
		t.Fatalf("the originator routes' default wait is %v, want none — waiting is the caller's patience, stated by the caller", PASWaitDefault)
	}
	if PASWaitMax != 30*time.Second {
		t.Fatalf("the maximum wait is %v, want 30s", PASWaitMax)
	}
	if pasInquireMax != 6 {
		t.Fatalf("the inquiry cap is %d, want 6", pasInquireMax)
	}
	for _, tc := range []struct{ in, want time.Duration }{
		{-time.Second, 0},
		{0, 0},
		{10 * time.Second, 10 * time.Second},
		{PASWaitMax, PASWaitMax},
		{10 * time.Minute, PASWaitMax},
	} {
		if got := clampPASWait(tc.in); got != tc.want {
			t.Errorf("clampPASWait(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestPASWait_TheBoundIsReachable holds the maximum wait and the inquiry schedule
// to each other.
//
// The bound and the schedule used to be two independent numbers: the schedule
// handed out 2, 4, 5, 5, 5, 5 seconds, so its sixth and last inquiry fell due at
// 26 s — while the maximum a caller could ask for was 120 s. Every second beyond
// 26 was a connection held open with no inquiry left to make, and no row could
// see it, because a hermetic row compresses the schedule to milliseconds and then
// passes PASWaitMax: at that scale everything fits.
//
// So this row asserts the relationship rather than the numbers, at the SHIPPED
// schedule. It goes red if the maximum is raised past what the schedule delivers,
// if the schedule is slowed until its last inquiry falls outside the maximum, or
// if the cap and the schedule stop agreeing about how many inquiries there are.
func TestPASWait_TheBoundIsReachable(t *testing.T) {
	// The shipped schedule, not a compressed one: this row is about what an
	// operator's wait actually buys.
	if pasInquireFirstDelay != 2*time.Second || pasInquireBackoffCap != 5*time.Second {
		t.Fatalf("the shipped schedule is first=%v cap=%v; this row reads the shipped one", pasInquireFirstDelay, pasInquireBackoffCap)
	}
	reach := pasInquireScheduleReach()
	if reach != 26*time.Second {
		t.Fatalf("the schedule's last inquiry falls due at %v, want 26s (2+4+5+5+5+5)", reach)
	}
	if reach >= PASWaitMax {
		t.Fatalf("the schedule's last inquiry falls due at %v, at or beyond the %v maximum — a caller asking for the maximum can never make the last inquiry the cap allows",
			reach, PASWaitMax)
	}
	// And the maximum is not further past the schedule than one more delay would
	// carry it: beyond that the extra seconds buy nothing but a held connection.
	if PASWaitMax > reach+pasInquireBackoffCap {
		t.Fatalf("the maximum wait is %v and the schedule stops asking at %v — the %v beyond it makes no inquiry, it only holds the caller's connection",
			PASWaitMax, reach, PASWaitMax-reach)
	}

	// Drive it: at the maximum, on the gateway's own clock advancing by the
	// schedule's own delays, the wait makes every inquiry the cap allows.
	now := time.Unix(1700000000, 0).UTC()
	g := waitGateway(func() time.Time { return now })
	first, capped := pasInquireFirstDelay, pasInquireBackoffCap
	delay := first
	pasInquireFirstDelay, pasInquireBackoffCap = time.Millisecond, time.Millisecond
	t.Cleanup(func() { pasInquireFirstDelay, pasInquireBackoffCap = first, capped })
	calls := 0
	out, _, _, _ := g.followPended(context.Background(), pendedDecision(), PASWaitMax,
		func(context.Context) (PASDecision, int, string, error) {
			calls++
			// The clock moves as the SHIPPED schedule would move it, while the
			// sleeps stay at a millisecond: the rule under test is the deadline
			// against the schedule, not how long this test takes.
			now = now.Add(delay)
			delay = min(2*delay, capped)
			return pendedDecision(), 0, "", nil
		})
	if calls != pasInquireMax || out.Inquiries != pasInquireMax {
		t.Fatalf("a wait of %v made %d inquiries (reported %d), want all %d — the schedule must fit inside the bound a caller may ask for",
			PASWaitMax, calls, out.Inquiries, pasInquireMax)
	}
}

// TestPASWaitOf_ReadsTheCallersBound: an absent bound is the route's default, an
// explicit zero is honoured, and a bound that is not a number is REFUSED rather
// than silently read as the default — a caller that mistyped its bound must not
// be given a different one without being told.
func TestPASWaitOf_ReadsTheCallersBound(t *testing.T) {
	waitFor := func(query string) (time.Duration, bool) {
		return pasWaitOf(httptest.NewRequest(http.MethodPost, "/scenario/uc03"+query, nil))
	}
	if d, ok := waitFor(""); !ok || d != PASWaitDefault {
		t.Fatalf("absent = %v %v, want the route default", d, ok)
	}
	if d, ok := waitFor("?wait=0"); !ok || d != 0 {
		t.Fatalf("wait=0 = %v %v, want an honoured zero", d, ok)
	}
	if d, ok := waitFor("?wait=5"); !ok || d != 5*time.Second {
		t.Fatalf("wait=5 = %v %v", d, ok)
	}
	if d, ok := waitFor("?wait=9999"); !ok || d != PASWaitMax {
		t.Fatalf("wait=9999 = %v %v, want the maximum", d, ok)
	}
	for _, bad := range []string{"?wait=soon", "?wait=-1", "?wait=1.5"} {
		if _, ok := waitFor(bad); ok {
			t.Errorf("%q was accepted; a bound that is not a whole number of seconds must be refused", bad)
		}
	}
}

// --- the order-changed check ---

// TestContinuation_OrderChanged: at inquiry time the originator re-reads the order
// from the participant's own system. The item map pins what was SUBMITTED, so an
// order that has since become a different request is DETECTED and reported —
// never silently followed, and never used to report a decision about a request
// the participant no longer has.
func TestContinuation_OrderChanged(t *testing.T) {
	order := func(code, occurrence string) []byte {
		body := `{"resourceType":"ServiceRequest","id":"sr-1","code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"` + code + `"}]}`
		if occurrence != "" {
			body += `,"occurrenceDateTime":"` + occurrence + `"`
		}
		return []byte(body + `}`)
	}
	line := func(seq int, code, date string) ContinuationItem {
		return ContinuationItem{Sequence: seq, ProductCode: "http://www.ama-assn.org/go/cpt|" + code, ServiceDate: date}
	}
	cont := Continuation{Items: []ContinuationItem{line(1, "72148", "2027-04-01")}}

	if changed, detail := pasOrderChanged(order("72148", "2027-04-01T09:00:00Z"), cont); changed {
		t.Fatalf("the unchanged order must not be reported as changed: %s", detail)
	}

	t.Run("a different service is reported, naming both codes", func(t *testing.T) {
		changed, detail := pasOrderChanged(order("70551", "2027-04-01T09:00:00Z"), cont)
		if !changed {
			t.Fatal("an order that now asks for a different service must be reported")
		}
		// An operator reading "order changed" alone cannot tell whether the order
		// or the record is the surprising one.
		if !strings.Contains(detail, "72148") || !strings.Contains(detail, "70551") {
			t.Fatalf("the report must name what was submitted and what the order now asks for: %q", detail)
		}
	})

	// A RESCHEDULED ORDER IS A DIFFERENT REQUEST. Same device, same patient, a
	// different date of service — a payer adjudicates that on its own terms, so
	// inquiring about it as the original asks about something the payer never
	// received. A check that compared only the product code waved this through.
	t.Run("a rescheduled order is reported", func(t *testing.T) {
		changed, detail := pasOrderChanged(order("72148", "2028-12-31T09:00:00Z"), cont)
		if !changed {
			t.Fatal("an order rescheduled to another date must be reported")
		}
		if !strings.Contains(detail, "2027-04-01") || !strings.Contains(detail, "2028-12-31") {
			t.Fatalf("the report must name both dates: %q", detail)
		}
	})

	// EVERY LINE IS COMPARED. A submission's lines all derive from this one
	// order, so an order that now accounts for only one of them is a change. A
	// check that stopped at the first matching line waved this through.
	t.Run("a line the order no longer accounts for is reported", func(t *testing.T) {
		two := Continuation{Items: []ContinuationItem{
			line(1, "72148", "2027-04-01"),
			line(2, "70551", "2027-04-01"),
		}}
		changed, detail := pasOrderChanged(order("72148", "2027-04-01T09:00:00Z"), two)
		if !changed {
			t.Fatal("a submitted line the order no longer accounts for must be reported")
		}
		if !strings.Contains(detail, "line 2") {
			t.Fatalf("the report must name the line that no longer matches: %q", detail)
		}
	})

	// An absence on both sides is agreement, not a change: an order that never
	// stated a date, submitted as a line with none, still matches itself.
	t.Run("an order that states no date matches a line that recorded none", func(t *testing.T) {
		dateless := Continuation{Items: []ContinuationItem{line(1, "72148", "")}}
		if changed, detail := pasOrderChanged(order("72148", ""), dateless); changed {
			t.Fatalf("two absences are agreement: %s", detail)
		}
		// …and one side gaining a date is a change, reported in words rather
		// than as an empty string.
		changed, detail := pasOrderChanged(order("72148", "2028-12-31T09:00:00Z"), dateless)
		if !changed || !strings.Contains(detail, "no stated date") {
			t.Fatalf("changed=%v detail=%q", changed, detail)
		}
	})

	// An order this gateway cannot read, and a continuation with nothing to
	// compare against, are both changes: the check cannot say the request is
	// still the same one, and "cannot say" is not "unchanged".
	t.Run("what cannot be compared is not unchanged", func(t *testing.T) {
		for _, unreadable := range [][]byte{[]byte(`not json`), []byte(`{"resourceType":"ServiceRequest","id":"sr-1"}`)} {
			if changed, _ := pasOrderChanged(unreadable, cont); !changed {
				t.Fatalf("an order this gateway cannot read must not pass the changed check: %s", unreadable)
			}
		}
		if changed, _ := pasOrderChanged(order("72148", "2027-04-01T09:00:00Z"), Continuation{}); !changed {
			t.Fatal("a continuation recording no submitted line has nothing to say the order still matches")
		}
	})

	// And the whole sentence is the one the route answers 409 with.
	if ErrOrderChanged.Error() != "order changed since submission" {
		t.Fatalf("ErrOrderChanged = %q", ErrOrderChanged.Error())
	}
}

// --- the submit operation ---

// TestOriginator_SubmitFollowReturnsPayerDecision: an approval, a denial and a
// pend all come back as DECISIONS. None of them is an error return, and a pend
// comes back with a continuation that can be continued.
//
// The pend row runs with a wait of zero, which is the wait bound reached
// immediately: the payer's answer is reported and the continuation is handed
// back. The wait's own bounds are proven on their own terms above.
func TestOriginator_SubmitFollowReturnsPayerDecision(t *testing.T) {
	for _, tc := range []struct {
		name         string
		answer       string
		wantDecision string
	}{
		{"approved", "approved", PASDecisionApproved},
		{"denied", "denied", PASDecisionDenied},
		{"pended at the wait bound", "pended", PASDecisionPended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, _ := pasFollowSystem(t, tc.answer)
			req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
			out, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
			if status != 0 || err != nil {
				t.Fatalf("a payer determination is never an error return: %d %q %v", status, msg, err)
			}
			if out.Decision != tc.wantDecision {
				t.Fatalf("decision = %q, want %q", out.Decision, tc.wantDecision)
			}
			if len(out.PayerResponse) == 0 {
				t.Fatal("the payer's own bytes must come back with its decision")
			}
			switch tc.wantDecision {
			case PASDecisionApproved:
				if out.Parsed.PreAuthRef == "" {
					t.Fatal("an approval carries the payer's authorization number")
				}
				if out.Continuation != "" {
					t.Fatal("a decided authorization needs no continuation from the submit")
				}
			case PASDecisionDenied:
				if out.Parsed.Denial == nil || out.Parsed.Denial.Rationale == "" {
					t.Fatalf("a denial carries the payer's own rationale: %+v", out.Parsed)
				}
			case PASDecisionPended:
				if out.Continuation == "" {
					t.Fatal("a pend must come back with the capability that continues it")
				}
				if out.ContinuationDurable {
					t.Fatal("this deployment keeps continuations in memory only and must say so")
				}
				// The continuation is real: it resolves, and it holds what was
				// submitted rather than a promise of it.
				store, _ := gw.continuations()
				cont, look, err := store.ReadContinuation("provider", out.Continuation)
				if err != nil || look != ContinuationFound {
					t.Fatalf("the continuation must resolve: %v %v", look, err)
				}
				if cont.PayerHolder != "payer" || cont.MemberID != pasFollowMember || cont.OrderRef != pasFollowOrderRef {
					t.Fatalf("continuation = %+v", cont)
				}
				if len(cont.Items) == 0 || cont.Items[0].TraceNumber == "" {
					t.Fatalf("the item map must pin what was submitted, with its trace numbers: %+v", cont.Items)
				}
				if cont.ClaimType == "" || cont.ClaimPriority == "" {
					t.Fatal("the inquiry Claim must be able to carry the request's own type and priority")
				}
				if cont.LastOutcome != ContinuationOutcomePended {
					t.Fatalf("LastOutcome = %q", cont.LastOutcome)
				}
			}
		})
	}
}

// TestOriginator_PendThenInquireApproved: the pend is continued through the
// INQUIRY leg — the gateway builds an inquiry from the continuation plus the
// participant's own records, sends it, and reports the payer's later decision.
//
// The wait is STATED here, because the route's default is not to wait at all: a
// caller that wants the gateway to follow a decision inside its own request says
// so, and this row is that caller. It used to ride on PASWaitDefault, which is
// how a thirty-second default nobody asked for went unnoticed.
func TestOriginator_PendThenInquireApproved(t *testing.T) {
	fastPASInquire(t)
	gw, stub := pasFollowSystem(t, "pended")
	stub.inquireAnswer = "approved"
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)

	out, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(PASWaitMax))
	if status != 0 || err != nil {
		t.Fatalf("%d %q %v", status, msg, err)
	}
	if out.Decision != PASDecisionApproved {
		t.Fatalf("decision = %q, want the later approval the inquiry found", out.Decision)
	}
	if out.Inquiries != 1 {
		t.Fatalf("inquiries = %d, want 1 — the payer answered on the first ask", out.Inquiries)
	}
	if !stub.sawInquiry {
		t.Fatal("the decision must have come from the inquiry leg, not from anything else")
	}
	if out.Continuation == "" {
		t.Fatal("the continuation rides through to the decision")
	}
	// The continuation records what the payer has said SINCE, so asking again
	// resolves to the decision rather than starting over.
	store, _ := gw.continuations()
	cont, _, _ := store.ReadContinuation("provider", out.Continuation)
	if cont.LastOutcome != ContinuationOutcomeApproved {
		t.Fatalf("LastOutcome = %q, want the decision the inquiry found", cont.LastOutcome)
	}
}

// TestOriginator_InquiryRunsAtTheLineTheSubmissionRanAt: the continuation pins
// the line, and the inquiry is selected against that pin rather than
// re-negotiated.
//
// The rejection row is what proves it: move the continuation's line to one this
// gateway cannot build, and the inquiry is REFUSED. A gateway that re-negotiated
// would quietly run at its own line and pass.
func TestOriginator_InquiryRunsAtTheLineTheSubmissionRanAt(t *testing.T) {
	gw, stub := pasFollowSystem(t, "pended")
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
	out, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
	if status != 0 || err != nil {
		t.Fatalf("%d %q %v", status, msg, err)
	}
	store, _ := gw.continuations()
	cont, _, _ := store.ReadContinuation("provider", out.Continuation)
	if cont.Line != "2.0" {
		t.Fatalf("the submission ran at %q; this row assumes 2.0", cont.Line)
	}

	// The control: at its own pinned line the inquiry goes out.
	if _, status, msg, _ := gw.inquireContinuation(req.Context(), req, cont); status != 0 {
		t.Fatalf("the pinned line must be usable: %d %q", status, msg)
	}
	if !stub.sawInquiry {
		t.Fatal("the control row did not reach the payer")
	}

	// The rejection: a pin this gateway cannot build refuses, and sends nothing.
	stub.sawInquiry = false
	moved := cont
	moved.Line = "2.2"
	_, status, msg, _ = gw.inquireContinuation(req.Context(), req, moved)
	if status == 0 {
		t.Fatal("an inquiry pinned to a line this gateway cannot build must be refused, not re-negotiated onto another line")
	}
	if stub.sawInquiry {
		t.Fatal("the refused inquiry was sent anyway")
	}
	if !strings.Contains(msg, "2.2") {
		t.Fatalf("the refusal must name the line it could not reach: %q", msg)
	}
}

// TestPAInquireRoute_Refusals: the route's answers for a continuation it cannot
// serve. The id IS the capability, so an id this holder did not mint says nothing
// more than that it names nothing here.
func TestPAInquireRoute_Refusals(t *testing.T) {
	gw, _ := pasFollowSystem(t, "pended")
	store, _ := gw.continuations()
	mine, err := store.PutContinuation(Continuation{Holder: "provider", PayerHolder: "payer", MemberID: pasFollowMember})
	if err != nil {
		t.Fatal(err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		gw.handlePAInquire(rec, httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire", strings.NewReader(body)))
		return rec
	}

	t.Run("no continuation", func(t *testing.T) {
		if rec := post(`{}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})

	t.Run("an id this holder never minted answers 404", func(t *testing.T) {
		rec := post(`{"continuation":"` + NewContinuationID(ContinuationDurableMark) + `"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		// 404, never 403: a distinguishable refusal would confirm the id exists.
		if strings.Contains(rec.Body.String(), "forbidden") || strings.Contains(rec.Body.String(), "not yours") {
			t.Fatalf("the refusal must not say whose it is: %s", rec.Body)
		}
	})

	t.Run("an id this gateway minted and lost answers 410", func(t *testing.T) {
		// The restart: the gateway's in-memory continuations are discarded. This
		// reaches the store the ROUTE reads — the one the fixture's Store ships —
		// rather than type-asserting for a concrete type the fixture does not have.
		//
		// There is deliberately NO skip branch. A refusal row whose green exit can
		// mean "did not run" proves nothing, and that is exactly what an earlier
		// draft of this row did: the assertion asserted, the type assertion failed,
		// and the route's mapping of a lost continuation onto 410 went unproven
		// while the row reported success.
		mustResetContinuations(t, store)

		rec := post(`{"continuation":"` + mine.ID + `"}`)
		if rec.Code != http.StatusGone {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "continuation lost (gateway restarted; submit again or inquire from your own system)") {
			t.Fatalf("the refusal must say what happened and both ways forward: %s", rec.Body)
		}
	})
}

// mustResetContinuations discards a store's continuations the way a restart
// does, and FAILS when it cannot — a row that quietly did not restart anything
// would assert against a store that still held the record.
func mustResetContinuations(t *testing.T, store ContinuationStore) {
	t.Helper()
	r, ok := store.(interface{ ResetContinuations() })
	if !ok {
		// MemStore discards its continuations through Reset, which clears the
		// rest of its holder state with them.
		m, isMem := store.(interface{ Reset() })
		if !isMem {
			t.Fatalf("%T can neither reset nor be restarted, so this row cannot drive the case it is about", store)
		}
		m.Reset()
		return
	}
	r.ResetContinuations()
}

// TestPAInquireRoute_ContinuesToTheDecision: the route's SUCCESS path — the one
// an operator actually uses.
//
// It pins what the answer says, not merely that it is a 200: the determination,
// the payer's own authorization number or rationale, the continuation it belongs
// to, how many inquiries were made, and the payer's own bytes. Without this row
// the route could return an empty answer, or count its inquiries wrong, and every
// other row in this package would still be green.
func TestPAInquireRoute_ContinuesToTheDecision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
	}{
		{"a later approval", "approved"},
		{"a later denial", "denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, stub := pasFollowSystem(t, "pended")
			req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
			pend, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
			if status != 0 || err != nil {
				t.Fatalf("submit: %d %q %v", status, msg, err)
			}
			if pend.Decision != PASDecisionPended || pend.Continuation == "" {
				t.Fatalf("this row starts from a pend with a continuation: %+v", pend)
			}

			// The payer has since decided. waitSeconds 0: ask once, report what
			// comes back — no wait is entered, so nothing here is timing-bound.
			stub.inquireAnswer = tc.answer
			rec := httptest.NewRecorder()
			gw.handlePAInquire(rec, httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire",
				strings.NewReader(`{"continuation":"`+pend.Continuation+`","waitSeconds":0}`)))
			if rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if !stub.sawInquiry {
				t.Fatal("the decision must have come from the inquiry leg")
			}
			var got paInquireResp
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body)
			}
			if got.Decision != tc.answer {
				t.Fatalf("decision = %q, want %q", got.Decision, tc.answer)
			}
			if got.Continuation != pend.Continuation {
				t.Fatalf("continuation = %q, want the one asked about (%q)", got.Continuation, pend.Continuation)
			}
			if got.ContinuationDurable {
				t.Fatal("this deployment keeps continuations in memory only and must say so")
			}
			if got.Inquiries != 1 {
				t.Fatalf("inquiries = %d, want 1 — one ask, counted", got.Inquiries)
			}
			if len(got.PayerResponse) == 0 {
				t.Fatal("the payer's own bytes must come back with its decision")
			}
			switch tc.answer {
			case "approved":
				if got.Denied || got.AuthNumber != "AUTH-FOLLOW-1" {
					t.Fatalf("an approval carries the payer's own authorization number: %+v", got)
				}
			case "denied":
				if !got.Denied || got.Rationale == "" {
					t.Fatalf("a denial carries the payer's own rationale: %+v", got)
				}
				if got.AuthNumber != "" {
					t.Fatalf("a denial authorizes nothing, yet carries %q", got.AuthNumber)
				}
			}

			// And the continuation now records the payer's latest word, so asking
			// again resolves to the decision rather than starting over.
			store, _ := gw.continuations()
			cont, look, err := store.ReadContinuation("provider", pend.Continuation)
			if err != nil || look != ContinuationFound {
				t.Fatalf("the continuation must survive its own decision: %v %v", look, err)
			}
			if cont.LastOutcome != tc.answer {
				t.Fatalf("LastOutcome = %q, want %q", cont.LastOutcome, tc.answer)
			}
		})
	}
}

// TestPAInquireRoute_FollowsAStillPendedDecision: the route's OTHER branch — the
// payer has still not decided on the first ask, so the route keeps following the
// decision for the rest of the caller's bound. Without this row that branch is
// unreachable by any test, and an inquiry count that never advanced past one
// would look correct.
func TestPAInquireRoute_FollowsAStillPendedDecision(t *testing.T) {
	fastPASInquire(t)
	gw, stub := pasFollowSystem(t, "pended")
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
	pend, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
	if status != 0 || err != nil {
		t.Fatalf("submit: %d %q %v", status, msg, err)
	}

	// The payer keeps answering "still pended", so the route exhausts its bound
	// and reports the pend — never an error, and with the continuation intact.
	stub.inquireAnswer = "pended"
	rec := httptest.NewRecorder()
	gw.handlePAInquire(rec, httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire",
		strings.NewReader(`{"continuation":"`+pend.Continuation+`","waitSeconds":120}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reaching the bound is never an error: %d %s", rec.Code, rec.Body)
	}
	var got paInquireResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Decision != PASDecisionPended || got.Continuation != pend.Continuation {
		t.Fatalf("got = %+v, want the pend and its continuation", got)
	}
	// The first ask plus the wait's own, capped: the route counts every inquiry
	// it made, not just the one it made itself.
	if got.Inquiries != pasInquireMax {
		t.Fatalf("inquiries = %d, want %d — the first ask and the wait's, capped", got.Inquiries, pasInquireMax)
	}
}

// TestPAInquireRoute_OrderChanged409: continuing an authorization whose order has
// since changed is REPORTED, and the inquiry is not sent.
func TestPAInquireRoute_OrderChanged409(t *testing.T) {
	gw, stub := pasFollowSystem(t, "pended")
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
	out, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
	if status != 0 || err != nil {
		t.Fatalf("%d %q %v", status, msg, err)
	}

	// The participant's system now holds a different order under the same
	// reference.
	gw.cfg.SoR.(*pasFollowSoR).orderCode = "70551"

	rec := httptest.NewRecorder()
	gw.handlePAInquire(rec, httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire",
		strings.NewReader(`{"continuation":"`+out.Continuation+`","waitSeconds":0}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "order changed since submission") {
		t.Fatalf("body = %s", rec.Body)
	}
	if stub.sawInquiry {
		t.Fatal("an inquiry about lines the payer never received must not be sent")
	}
}

// TestOriginator_SeededPendingPersonaIsInquirable: the persona this repository
// seeds SO THAT a follow-up runs on a request the reference payer genuinely
// pends must itself be one an inquiry can be built for.
//
// It is driven through inquireContinuation over the persona's OWN seeded records
// — the published provider-data seed, not a fixture written beside this test —
// so the row goes red the moment the persona stops satisfying the inquiry it
// exists to exercise. That happened: the persona first named a bare Practitioner
// as its ordering provider, which a prior-authorization inquiry cannot carry, and
// nothing noticed because the only path that would have built its inquiry does
// not run while the payer edge still polls.
func TestOriginator_SeededPendingPersonaIsInquirable(t *testing.T) {
	gw, stub := pendingPersonaSystem(t)
	cont := pendingPersonaContinuation()
	req := httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire", nil)

	out, status, msg, err := gw.inquireContinuation(req.Context(), req, cont)
	if status != 0 || err != nil {
		t.Fatalf("the seeded pending persona must be inquirable: %d %q %v", status, msg, err)
	}
	if !stub.sawInquiry {
		t.Fatal("the inquiry never reached the payer")
	}
	if out.Decision != PASDecisionApproved {
		t.Fatalf("decision = %q", out.Decision)
	}

	// The inquiry names the persona's own supplier, by reference, carrying the
	// NPI a payer matches on — not a party invented here.
	records, status, msg := gw.inquiryRecords(req.Context(), cont)
	if status != 0 {
		t.Fatalf("records: %d %q", status, msg)
	}
	if got := shnsdk.PASResourceTypeOf(records.Provider); !slices.Contains(shnsdk.PASProviderTypes, got) {
		t.Fatalf("the inquiry's provider is a %s, which a prior-authorization inquiry cannot carry", got)
	}
	if npi := shnsdk.PASProviderNPI(records.Provider); npi == "" {
		t.Fatalf("the seeded provider carries no NPI, so a payer has nothing to match on: %s", records.Provider)
	}
}

// TestOriginator_ProviderAnInquiryCannotCarryIsRefusedByName: the rejection row
// for the guard above. An order naming only a bare Practitioner is refused with
// the reference AND the type, before the inquiry is built or sent.
func TestOriginator_ProviderAnInquiryCannotCarryIsRefusedByName(t *testing.T) {
	gw, stub := pendingPersonaSystem(t)
	sor := gw.cfg.SoR.(*pendingPersonaSoR)
	// The order now names ONLY the ordering clinician — a real requester, and one
	// a prior-authorization inquiry has nowhere to put.
	sor.dropPerformer = true

	_, status, msg := gw.inquiryRecords(context.Background(), pendingPersonaContinuation())
	if status == 0 {
		t.Fatal("an order naming no provider an inquiry can carry must be refused")
	}
	if !strings.Contains(msg, "Practitioner/prac-mbrpdpend") || !strings.Contains(msg, "Practitioner") {
		t.Fatalf("the refusal must name the reference and the type it found: %q", msg)
	}
	if !strings.Contains(msg, "Organization or a PractitionerRole") {
		t.Fatalf("the refusal must say what an inquiry CAN carry: %q", msg)
	}
	if stub.sawInquiry {
		t.Fatal("nothing may be sent for an inquiry that cannot be built")
	}
}

// --- the seeded pending persona, as its own fixture ---

const (
	pendingPersonaMember   = "MBR-PD-PEND"
	pendingPersonaOrderRef = "DeviceRequest/dr-mbrpdpend-e0424"
)

// pendingPersonaSoR serves the pending persona's own SEEDED resources, read out
// of the published provider-data seed. Reading them rather than restating them is
// the point: a row written against a hand-copied persona would keep passing after
// the seeded one changed.
type pendingPersonaSoR struct {
	*censusSoR
	byRef map[string][]byte
	// dropPerformer removes the order's supplier, leaving only the ordering
	// clinician — the shape the rejection row is about.
	dropPerformer bool
}

func newPendingPersonaSoR(t *testing.T) *pendingPersonaSoR {
	t.Helper()
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(fhirseed.ProviderDataSeedBundle(), &bundle); err != nil {
		t.Fatalf("read the published provider-data seed: %v", err)
	}
	byRef := map[string][]byte{}
	for _, e := range bundle.Entry {
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		if json.Unmarshal(e.Resource, &head) != nil || head.ID == "" {
			continue
		}
		byRef[head.ResourceType+"/"+head.ID] = append([]byte(nil), e.Resource...)
	}
	for _, need := range []string{
		"Patient/" + pendingPersonaMember, "Coverage/cov-mbrpdpend",
		"Organization/org-cms-payer", "Organization/org-dme-mbrpdpend",
		"Practitioner/prac-mbrpdpend", pendingPersonaOrderRef,
	} {
		if len(byRef[need]) == 0 {
			t.Fatalf("the published seed no longer carries %s — this row is about the SEEDED persona", need)
		}
	}
	return &pendingPersonaSoR{censusSoR: newCensusSoR(), byRef: byRef}
}

func (s *pendingPersonaSoR) order(t *testing.T) []byte {
	t.Helper()
	res := s.byRef[pendingPersonaOrderRef]
	if !s.dropPerformer {
		return res
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(res, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "performer")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (s *pendingPersonaSoR) ResolveByReference(ref string) ([]byte, bool) {
	if ref == pendingPersonaOrderRef && s.dropPerformer {
		var m map[string]json.RawMessage
		if json.Unmarshal(s.byRef[ref], &m) == nil {
			delete(m, "performer")
			if out, err := json.Marshal(m); err == nil {
				return out, true
			}
		}
	}
	if res, ok := s.byRef[ref]; ok {
		return res, true
	}
	return s.censusSoR.ResolveByReference(ref)
}

func (s *pendingPersonaSoR) OpenCoverage(member string) ([]byte, bool) {
	if member != pendingPersonaMember {
		return s.censusSoR.OpenCoverage(member)
	}
	return s.byRef["Coverage/cov-mbrpdpend"], true
}

func (s *pendingPersonaSoR) OpenOrder(member string) ([]byte, bool) {
	if member != pendingPersonaMember {
		return s.censusSoR.OpenOrder(member)
	}
	return s.byRef[pendingPersonaOrderRef], true
}

func (s *pendingPersonaSoR) PatientFHIRRef(member string) (string, bool) {
	if member == pendingPersonaMember {
		return "Patient/" + pendingPersonaMember, true
	}
	return s.censusSoR.PatientFHIRRef(member)
}

func (s *pendingPersonaSoR) ResolvePatient(member string) (string, Demo, bool) {
	if member == pendingPersonaMember {
		demo := Demo{BirthDate: "1951-10-06", FamilyName: "Thorvaldsen-StationaryOxygen"}
		return string(shnsdk.ResolvePCI(member, demo.BirthDate, demo.FamilyName)), demo, true
	}
	return s.censusSoR.ResolvePatient(member)
}

// pendingPersonaContinuation is what a submission of the seeded pending persona's
// own order records: its line, its member, its order, and the one item that
// order asks for.
func pendingPersonaContinuation() Continuation {
	return Continuation{
		ID:              "m0-pending",
		Holder:          "provider",
		PayerHolder:     "payer",
		Line:            "2.0",
		CorrID:          "corr-pending",
		MemberID:        pendingPersonaMember,
		SoRPatientID:    "Patient/" + pendingPersonaMember,
		OrderRef:        pendingPersonaOrderRef,
		ClaimIdentifier: "urn:shn:correlation|corr-pending",
		ClaimType:       "http://terminology.hl7.org/CodeSystem/claim-type|professional",
		ClaimPriority:   "|normal",
		// What the payer's own pend answered with — the identifier a later
		// answer about this authorization carries, and what the inquiry's answer
		// is matched back by.
		PayerClaimResponseIDs: []string{"urn:shn:correlation|corr-pending"},
		ItemTraceNumbers:      []string{"urn:shn:pas:item-trace|corr-pending.1"},
		Items: []ContinuationItem{{
			Sequence:    1,
			ProductCode: "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets|E0424",
			TraceNumber: "urn:shn:pas:item-trace|corr-pending.1",
		}},
		LastOutcome: ContinuationOutcomePended,
	}
}

func pendingPersonaSystem(t *testing.T) (*Gateway, *pasFollowStub) {
	t.Helper()
	sor := newPendingPersonaSoR(t)
	gw, stub := pasFollowSystemWithSoR(t, "pended", sor, sor)
	stub.inquireAnswer = "approved"
	stub.submitCorr = "corr-pending"
	return gw, stub
}

func pendingPersonaOrder(t *testing.T) []byte {
	t.Helper()
	return newPendingPersonaSoR(t).order(t)
}

// --- the fixture ---

const (
	pasFollowMember   = "MBR-COVERED"
	pasFollowOrderRef = "ServiceRequest/sr-follow"
	pasFollowNPI      = "1234567893"
)

func pasFollowSubmit(wait time.Duration) pasFollowInputs {
	return pasFollowInputs{
		pci:          string(shnsdk.ResolvePCI(pasFollowMember, "1975-04-02", "Johansson")),
		patientRef:   "Patient/" + pasFollowMember,
		coverageRef:  "Coverage/" + pasFollowMember,
		coverage:     testMemberCoverage(pasFollowMember),
		member:       pasFollowMember,
		memberSystem: shnsdk.MemberSystem,
		recipient:    "payer",
		orderRef:     pasFollowOrderRef,
		orderJSON:    pasFollowOrder("72148"),
		payer:        shnsdk.CMSPayerIdentity,
		wait:         wait,
	}
}

// pasFollowOrder is the participant's own order: a ServiceRequest naming the
// ordering provider by reference AND by NPI, which is what a payer matches an
// inquiry on.
func pasFollowOrder(code string) []byte {
	return []byte(`{"resourceType":"ServiceRequest","id":"sr-follow","status":"active","intent":"order",` +
		`"subject":{"reference":"Patient/` + pasFollowMember + `"},` +
		`"requester":{"reference":"Organization/prov-follow","identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"` + pasFollowNPI + `"}},` +
		`"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"` + code + `","display":"MRI lumbar spine"}]},` +
		`"occurrenceDateTime":"2027-04-01T00:00:00Z"}`)
}

// pasFollowSoR is the participant's own system of record for these rows: the four
// records an inquiry carries, plus the order it re-reads. It is deliberately a
// REAL set of resolvable references rather than a synthesized bundle — the whole
// point of the inquiry path is that every fact comes from the participant's own
// system.
type pasFollowSoR struct {
	*censusSoR
	orderCode string
	// orderOverride replaces the order this system holds, for the rows about an
	// order that names no provider a request can carry.
	orderOverride []byte
}

func newPASFollowSoR() *pasFollowSoR {
	return &pasFollowSoR{censusSoR: newCensusSoR(), orderCode: "72148"}
}

func (s *pasFollowSoR) OpenOrder(string) ([]byte, bool) { return s.order(), true }

func (s *pasFollowSoR) order() []byte {
	if s.orderOverride != nil {
		return s.orderOverride
	}
	return pasFollowOrder(s.orderCode)
}

func (s *pasFollowSoR) OpenCoverage(member string) ([]byte, bool) {
	if member != pasFollowMember {
		return nil, false
	}
	return []byte(`{"resourceType":"Coverage","id":"cov-follow","status":"active",` +
		`"beneficiary":{"reference":"Patient/` + pasFollowMember + `"},` +
		`"subscriberId":"` + pasFollowMember + `",` +
		`"payor":[{"reference":"Organization/payer-follow"}]}`), true
}

func (s *pasFollowSoR) ResolveByReference(ref string) ([]byte, bool) {
	switch ref {
	case pasFollowOrderRef:
		return s.order(), true
	case "Organization/prov-follow":
		return []byte(`{"resourceType":"Organization","id":"prov-follow","active":true,"name":"Bay Area Orthopedics",` +
			`"identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"` + pasFollowNPI + `"}]}`), true
	case "Practitioner/prac-follow":
		// A real ordering clinician the participant's system holds — and not a
		// party a prior-authorization request can carry, which is what the
		// not-carryable rejection rows are about.
		return []byte(`{"resourceType":"Practitioner","id":"prac-follow",` +
			`"identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1644556676"}]}`), true
	case "Organization/payer-follow":
		return []byte(`{"resourceType":"Organization","id":"payer-follow","active":true,"name":"Reference Payer",` +
			`"identifier":[{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"` + shnsdk.CMSPayerIdentity.Value + `"}]}`), true
	case "Patient/" + pasFollowMember:
		return []byte(`{"resourceType":"Patient","id":"` + pasFollowMember + `",` +
			`"identifier":[{"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v2-0203","code":"MB"}]},` +
			`"system":"` + shnsdk.MemberSystem + `","value":"` + pasFollowMember + `"}],` +
			`"name":[{"family":"Johansson"}],"birthDate":"1975-04-02"}`), true
	}
	return s.censusSoR.ResolveByReference(ref)
}

// pasFollowStub answers the two legs these rows drive: the PAS submit, with the
// determination the row asked for, and the inquiry, with the later one.
type pasFollowStub struct {
	*stubSubstrate
	submitAnswer  string
	inquireAnswer string
	sawInquiry    bool
	line          string
	// submitCorr is the correlation the submission ran under. The payer's own
	// identifiers for the authorization derive from it, and an inquiry's answer
	// carries those, not the inquiry's own.
	submitCorr string
}

// inquiryResponseBundle wraps a payer answer in the response Bundle the 2.0 and
// 2.1 inquiry operations return. A bare ClaimResponse is not one of the shapes
// the published operation definitions declare, and the reader refuses it.
func inquiryResponseBundle(answer []byte, at time.Time) []byte {
	var head struct {
		ResourceType string `json:"resourceType"`
	}
	if json.Unmarshal(answer, &head) == nil && head.ResourceType == "Bundle" {
		return answer
	}
	b, _ := json.Marshal(map[string]any{
		"resourceType": "Bundle",
		"type":         "collection",
		"timestamp":    at.UTC().Format(time.RFC3339),
		"entry":        []any{map[string]any{"resource": json.RawMessage(answer)}},
	})
	return b
}

func (s *pasFollowStub) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/route") {
		return s.stubSubstrate.RoundTrip(req)
	}
	body, _ := io.ReadAll(req.Body)
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		return errResp("stub: decode envelope: " + err.Error()), nil
	}
	corrID := env.Metadata.CorrelationID
	leg := env.Metadata.TransactionType
	var payload []byte
	var op string
	switch leg {
	case "pas-claim":
		s.submitCorr = corrID
		payload, err = s.pasAnswer(s.submitAnswer, corrID)
		op = "pas-response"
	case "pas-claim-inquire":
		s.sawInquiry = true
		// A payer answers an inquiry about the AUTHORIZATION, echoing the
		// identifiers it gave that authorization — not the inquiry's own
		// correlation. That is exactly what the requester matches the answer back
		// by, so the stub must do it too or the row would prove nothing about the
		// matching.
		inner, aerr := s.pasAnswer(s.inquireAnswer, s.submitCorr)
		if aerr != nil {
			return errResp("stub: inquiry answer: " + aerr.Error()), nil
		}
		// The 2.0/2.1 answer shape: one response Bundle.
		payload, err = inquiryResponseBundle(inner, s.clock()), nil
		op = "pas-inquire-response"
	default:
		return errResp("stub: unexpected leg " + leg), nil
	}
	if err != nil {
		return errResp("stub: build answer: " + err.Error()), nil
	}
	meta := shnsdk.Metadata{
		Sender: "payer", Recipient: "provider", TransactionType: leg,
		AuthorityFrame: "payer-coverage", Timestamp: s.clock().UTC().Format(time.RFC3339), CorrelationID: corrID,
	}
	out, err := sealForProvider(meta, payload, s.providerEncPub, s.authzPriv, corrID, op, "payer-coverage", "payer", s.pci, s.clock())
	if err != nil {
		return errResp("stub: sealForProvider: " + err.Error()), nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(out))),
		Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
}

// pasAnswer builds the payer's own answer bytes. The pended answer carries the
// request's item trace number, which is what a later inquiry names the
// authorization by.
func (s *pasFollowStub) pasAnswer(kind, corrID string) ([]byte, error) {
	patientRef := "Patient/" + pasFollowMember
	switch kind {
	case "approved":
		return shnsdk.BuildClaimResponse("AUTH-FOLLOW-1", "2030-01-01", patientRef, corrID, s.clock())
	case "denied":
		return shnsdk.BuildDeniedResponseAtLine(s.line, patientRef, corrID, "Conservative therapy is not documented.", s.clock())
	case "pended":
		return testPendedResponse(patientRef, corrID, "operative-diagnostic-report", s.clock())
	}
	return nil, errors.New("unknown answer kind " + kind)
}

// pasFollowSystem builds a provider gateway whose payer answers with the given
// determination, over the participant's own system of record.
func pasFollowSystem(t *testing.T, submitAnswer string) (*Gateway, *pasFollowStub) {
	t.Helper()
	sor := newPASFollowSoR()
	return pasFollowSystemWithSoR(t, submitAnswer, sor, sor)
}

// pasFollowSystemWithSoR is pasFollowSystem over a caller-supplied system of
// record, so a row can drive the gateway against a PARTICULAR participant's
// records — the seeded pending persona's, for instance — rather than the
// fixture's own.
func pasFollowSystemWithSoR(t *testing.T, submitAnswer string, sor SystemOfRecord, store Store) (*Gateway, *pasFollowStub) {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	var provSignPriv ed25519.PrivateKey
	_, provSignPriv = genED25519(t)
	payerEncPub, _ := genKeyPair(t)
	payerSignPub, _ := genED25519(t)

	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	pci, _, _ := sor.ResolvePatient(pasFollowMember)

	stub := &pasFollowStub{
		stubSubstrate: &stubSubstrate{authzPriv: authzPriv, providerEncPub: provEncPub, clock: clock, pci: pci},
		submitAnswer:  submitAnswer,
		inquireAnswer: "pended",
		line:          "2.0",
	}

	reg := shnsdk.NewRegistry()
	reg.Set("provider", shnsdk.RegistryEntry{ID: "provider", Role: "provider", EncPub: provEncPub, SignPub: authzPub})
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", EncPub: payerEncPub, SignPub: payerSignPub})

	const fakeBase = "http://stub.test"
	gw := mustNew(t, Config{
		Role:        "provider",
		HolderID:    "provider",
		PayerRouter: payerRouterFor(t, "payer"),
		Identity: shnsdk.Identity{
			HolderID: "provider", SignPriv: provSignPriv, EncPub: provEncPub, EncPriv: provEncPriv,
		},
		AuthzURL:        fakeBase,
		AuthzPub:        authzPub,
		HubTransportPub: authzPub,
		HubURL:          fakeBase,
		Reg:             reg,
		Validator:       shnsdk.NewFakeValidator(),
		// One laned line. A non-empty map makes the deployment AUTHORITATIVE about
		// which lines it can validate, which is what lets the line-pin row below
		// have a real rejection: a continuation pinned to an unlaned line is
		// refused rather than quietly re-negotiated onto this one.
		ValidatorsByLine: map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()},
		SoR:              sor,
		Store:            store,
		Clock:            clock,
		NPI:              pasFollowNPI,
		Client:           &http.Client{Transport: stub},
	})
	return gw, stub
}

// TestPASDecision_AppliesOneVocabularyToEverySurface: every originator surface
// reports a determination through one projection, so a denial does not read one
// way on one route and another way on the next.
func TestPASDecision_AppliesOneVocabularyToEverySurface(t *testing.T) {
	denial := PASDecision{Decision: PASDecisionDenied, Parsed: shnsdk.PriorAuthResult{
		Outcome: "denied", Denial: &shnsdk.Denial{Rationale: "not documented"}}}
	got := denial.applyTo(uc03Resp{PARequired: true})
	if !got.Denied || got.Rationale != "not documented" || got.Decision != "denied" || got.AuthNumber != "" {
		t.Fatalf("denial = %+v", got)
	}
	pend := PASDecision{Decision: PASDecisionPended, Continuation: "m0-x", ContinuationDurable: false,
		Parsed: shnsdk.PriorAuthResult{Outcome: "pended", NeededItems: []shnsdk.NeededItem{{Code: "operative-report"}}}}
	got = pend.applyTo(uc03Resp{PARequired: true})
	if !got.Pended || got.Continuation != "m0-x" || len(got.PendedItems) != 1 {
		t.Fatalf("pend = %+v", got)
	}
	// THE DISCLOSURE MUST REACH THE WIRE AS ITSELF. A non-durable continuation
	// says so explicitly; it does not say so by being absent, which is what a
	// plain omitempty boolean would have done — and it is exactly the shapes
	// where it is false that most need to state it.
	if got.ContinuationDurable == nil || *got.ContinuationDurable {
		t.Fatalf("a non-durable continuation must state that it is non-durable: %+v", got.ContinuationDurable)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"continuationDurable":false`) {
		t.Fatalf("the disclosure must be on the wire, not omitted: %s", b)
	}

	got = approvedDecision().applyTo(uc03Resp{PARequired: true})
	if got.Decision != "approved" || got.AuthNumber != "AUTH-1" || got.Denied || got.Pended || got.Continuation != "" {
		t.Fatalf("approval = %+v", got)
	}
	// And a decision with no continuation says NOTHING about durability: there
	// is nothing to be durable about, and a bare false would read as a warning.
	if got.ContinuationDurable != nil {
		t.Fatal("a decision with no continuation must not state a durability")
	}
	// The wire shape a consumer reads.
	b, err = json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"decision":"approved"`) {
		t.Fatalf("the decision must reach the consumer: %s", b)
	}
	if strings.Contains(string(b), "continuationDurable") {
		t.Fatalf("a decision with no continuation must not carry the disclosure: %s", b)
	}
}
