package diagnostics

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// kindLog is one event the ingest was sent: its kind, at the fake time.
type kindLog struct {
	at   time.Time
	kind string
}

// kindAnswers answers each event in a batch with status(kind), logging it.
func kindAnswers(status func(f *fakeIngest, kind string) string, log *[]kindLog) func(f *fakeIngest, events []Event) (int, string) {
	return func(f *fakeIngest, events []Event) (int, string) {
		var a batchAnswer
		for _, e := range events {
			*log = append(*log, kindLog{at: f.now, kind: e.Kind})
			a.Results = append(a.Results, batchResult{Sequence: e.Sequence, Status: status(f, e.Kind)})
		}
		raw, _ := json.Marshal(a)
		return 200, string(raw)
	}
}

func sentOf(log []kindLog, kind string) []time.Time {
	var at []time.Time
	for _, l := range log {
		if l.kind == kind {
			at = append(at, l.at)
		}
	}
	return at
}

// A kind the ingest says it does not admit is withheld: its later events are
// discarded as suppressed without being sent, and the other kinds flow. The
// counts still agree.
func TestKindNotAdmittedIsWithheld(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 4096, MaxBytes: 64 << 20})
	f := newFakeIngest(t, q)
	f.rtt, f.rate, f.total, f.kinds = 50*time.Millisecond, 20, 20*60, []string{"http-request", KindAccess}
	var log []kindLog
	f.batch = kindAnswers(func(_ *fakeIngest, kind string) string {
		if kind == "http-request" {
			return batchKindNotAdmitted
		}
		return batchAccepted
	}, &log)
	f.run(context.Background(), batchURL)
	sent := sentOf(log, "http-request")
	if len(sent) == 0 || len(sent) > 3 {
		t.Fatalf("http-request sent %d times, want the first few before the answer and then none", len(sent))
	}
	// A withheld event is taken from the emitter, not refused.
	if !q.TryEmit(Event{Kind: "http-request"}) {
		t.Fatal("a withheld event reads as refused")
	}
	f.now = f.now.Add(time.Second)
	h := q.Health(f.now)
	h.LastSequence--
	h.Discarded--
	h.Suppressed--
	if h.Acknowledged != 600 || h.Discarded != 600 || h.Suppressed != 600-uint64(len(sent)) || h.Dropped != 0 || h.Pending != 0 || !h.CountsAgree() {
		t.Fatalf("health %+v (http-request sent %d)", h, len(sent))
	}
}

// A withheld kind is probed again after kindReprobe: when the ingest has come
// to admit it, it flows again; while it still does not, it is withheld again
// for another interval.
func TestWithheldKindIsProbedAgain(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 4096, MaxBytes: 64 << 20})
	f := newFakeIngest(t, q)
	// 25 minutes at 2 events a second: probes fall due at about 10 and 20
	// minutes. The ingest admits the kind from minute 15.
	f.rtt, f.rate, f.total, f.kinds = 50*time.Millisecond, 2, 2*60*25, []string{"http-request"}
	var log []kindLog
	start := f.now
	f.batch = kindAnswers(func(f *fakeIngest, _ string) string {
		if f.now.Sub(start) < 15*time.Minute {
			return batchKindNotAdmitted
		}
		return batchAccepted
	}, &log)
	f.run(context.Background(), batchURL)
	sent := sentOf(log, "http-request")
	var early, probe, late int
	for _, at := range sent {
		switch d := at.Sub(start); {
		case d < time.Minute:
			early++
		case d >= kindReprobe && d < kindReprobe+time.Minute:
			probe++
		case d >= 20*time.Minute:
			late++
		default:
			t.Fatalf("http-request sent at %v while withheld", d)
		}
	}
	if early == 0 || probe == 0 || late < 2*60*4 {
		t.Fatalf("sent: %d at first, %d at the first probe, %d after the ingest admitted it", early, probe, late)
	}
	if h := q.Health(f.now); h.Pending != 0 || h.Dropped != 0 || !h.CountsAgree() || h.Suppressed == 0 {
		t.Fatalf("health %+v", h)
	}
}

// Only kind_not_admitted withholds a kind. A decline of one event
// (scope_ignored), an invalid or conflicting event, or one the ingest could
// not take now says nothing about its kind, which keeps flowing.
func TestOnlyKindNotAdmittedWithholdsAKind(t *testing.T) {
	for _, status := range []string{batchScopeIgnored, batchInvalid, batchConflict, batchUnavailable, batchBindingPending} {
		q := NewQueue(Limits{MaxEvents: 4096, MaxBytes: 64 << 20})
		f := newFakeIngest(t, q)
		f.rtt, f.rate, f.total, f.kinds = 50*time.Millisecond, 10, 10*30, []string{"leg.verified"}
		var log []kindLog
		f.batch = kindAnswers(func(_ *fakeIngest, _ string) string { return status }, &log)
		f.run(context.Background(), batchURL)
		if h := q.Health(f.now); h.Suppressed != 0 {
			t.Fatalf("%s withheld the kind: %+v", status, h)
		}
		if n := len(sentOf(log, "leg.verified")); n < 300 {
			t.Fatalf("%s: the kind was sent %d times for 300 events", status, n)
		}
	}
}

// A sink that never says it takes batches never answers kind_not_admitted,
// so every kind reaches it: a participant's own diagnostics stream keeps all
// of them.
func TestASinkWithoutBatchesGetsEveryKind(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 4096, MaxBytes: 64 << 20})
	f := newFakeIngest(t, q)
	f.noBatches = true
	for i := 0; i < 99; i++ {
		q.TryEmit(Event{Kind: []string{"http-request", "http-response", KindAccess}[i%3]})
	}
	q.TryEmit(Event{Kind: "http-request"})
	f.run(context.Background(), batchURL)
	if len(f.singles) != 100 || len(f.batches) != 0 {
		t.Fatalf("singles %d, batches %d", len(f.singles), len(f.batches))
	}
	if h := q.Health(f.now); h.Acknowledged != 100 || h.Suppressed != 0 || h.Discarded != 0 {
		t.Fatalf("health %+v", h)
	}
}

// Suppressed is part of Discarded: more suppressed than discarded, or
// suppressed without the counts, does not agree.
func TestSuppressedIsPartOfDiscarded(t *testing.T) {
	for name, c := range map[string]struct {
		h    Health
		want bool
	}{
		"within the discards":       {Health{LastSequence: 3, LastAcknowledged: 1, Acknowledged: 1, Discarded: 2, Suppressed: 2}, true},
		"more than the discards":    {Health{LastSequence: 3, LastAcknowledged: 1, Acknowledged: 1, Discarded: 2, Suppressed: 3}, false},
		"without any other count":   {Health{LastSequence: 0, Suppressed: 1}, false},
		"none, with no other count": {Health{LastSequence: 2, LastAcknowledged: 2}, true},
	} {
		if got := c.h.CountsAgree(); got != c.want {
			t.Errorf("%s: CountsAgree = %v, want %v", name, got, c.want)
		}
	}
}

// One event per request (a sink without batches, or today's ingest) never
// withholds a kind: whatever the ingest answers an event, the kind's later
// events are still sent.
func TestNoSingleEventAnswerWithholdsAKind(t *testing.T) {
	for name, answerWith := range map[string]func() (int, string){
		"scope_ignored": func() (int, string) { return 422, `{"code":"scope_ignored"}` },
		"invalid_event": func() (int, string) { return 400, `{"code":"invalid_event"}` },
		"conflict":      func() (int, string) { return 409, `{"code":"conflict"}` },
		"unavailable":   func() (int, string) { return 503, `{"code":"unavailable"}` },
		// Even a kind_not_admitted code: only a batch result says that.
		"kind_not_admitted": func() (int, string) { return 422, `{"code":"kind_not_admitted"}` },
	} {
		// The events arrive while the ingest is answering earlier ones.
		q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
		f := newFakeIngest(t, q)
		f.noBatches = true
		f.rtt, f.rate, f.total, f.kinds = 10*time.Millisecond, 100, 20, []string{"http-request"}
		f.single = func(*fakeIngest, Event) (int, string) { return answerWith() }
		f.run(context.Background(), batchURL)
		sent := map[uint64]bool{}
		for _, s := range f.singles {
			sent[s] = true
		}
		h := q.Health(f.now)
		if f.emitted != 20 || len(sent) != 20 || h.Suppressed != 0 || !h.CountsAgree() {
			t.Fatalf("%s: sent %d of %d events; health %+v", name, len(sent), f.emitted, h)
		}
	}
}
