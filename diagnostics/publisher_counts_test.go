package diagnostics

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Each way the publisher lets an event go is counted under its reason, the
// heartbeat's counts agree, and an event the ingest refuses for good is sent
// once, never retried.
func TestPublisherCountsWhyEachEventLeft(t *testing.T) {
	respond := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
	}
	type env struct {
		q      *Queue
		now    *time.Time
		cancel context.CancelFunc
	}
	for name, c := range map[string]struct {
		event     Event
		setup     func(env)                                // after the event is queued
		send      func(env, Event) (*http.Response, error) // an event's answer
		heartbeat func(env)                                // on each heartbeat
		wait      func(env, time.Duration) error           // a retry's wait
		want      Health
		sends     int // when set, how many times the event is sent
	}{
		"acknowledged": {
			send:  func(env, Event) (*http.Response, error) { return respond(204, ""), nil },
			want:  Health{Acknowledged: 1, LastAcknowledged: 1},
			sends: 1,
		},
		"declined as out of scope": {
			send:  func(env, Event) (*http.Response, error) { return respond(422, `{"code":"scope_ignored"}`), nil },
			want:  Health{Discarded: 1},
			sends: 1,
		},
		"refused as invalid": {
			send:  func(env, Event) (*http.Response, error) { return respond(400, `{"code":"invalid_event"}`), nil },
			want:  Health{Dropped: 1, DroppedBy: DropCounts{Invalid: 1}},
			sends: 1,
		},
		"refused as conflicting with a held event": {
			send:  func(env, Event) (*http.Response, error) { return respond(409, `{"code":"conflict"}`), nil },
			want:  Health{Dropped: 1, DroppedBy: DropCounts{Invalid: 1}},
			sends: 1,
		},
		// Only those exact answers are final: any other refusal is retried
		// until the event's ownership window runs out.
		"a 400 with another code is retried": {
			send: func(env, Event) (*http.Response, error) { return respond(400, `{"code":"invalid_health"}`), nil },
			want: Health{Dropped: 1, DroppedBy: DropCounts{Expired: 1}},
		},
		"a 409 invalid_event is retried": {
			send: func(env, Event) (*http.Response, error) { return respond(409, `{"code":"invalid_event"}`), nil },
			want: Health{Dropped: 1, DroppedBy: DropCounts{Expired: 1}},
		},
		"a 400 conflict is retried": {
			send: func(env, Event) (*http.Response, error) { return respond(400, `{"code":"conflict"}`), nil },
			want: Health{Dropped: 1, DroppedBy: DropCounts{Expired: 1}},
		},
		"held for its binding until it expires": {
			send: func(env, Event) (*http.Response, error) { return respond(409, `{"code":"binding_pending"}`), nil },
			want: Health{Dropped: 1, DroppedBy: DropCounts{Expired: 1}},
		},
		"a 409 with another code is retried": {
			send: func(env, Event) (*http.Response, error) { return respond(409, `{"code":"a_later_code"}`), nil },
			want: Health{Dropped: 1, DroppedBy: DropCounts{Expired: 1}},
		},
		"a 400 with no code is retried": {
			send: func(env, Event) (*http.Response, error) { return respond(400, `bad request`), nil },
			want: Health{Dropped: 1, DroppedBy: DropCounts{Expired: 1}},
		},
		// The code is whole, but the answer runs past what the publisher
		// reads, so it is not read as the ingest's verdict.
		"an invalid_event code in a cut-off answer is retried": {
			send: func(env, Event) (*http.Response, error) {
				return respond(400, `{"code":"invalid_event"}`+strings.Repeat(" ", 5000)), nil
			},
			want: Health{Dropped: 1, DroppedBy: DropCounts{Expired: 1}},
		},
		"a test event not accepted": {
			event: Event{Kind: "test"},
			want:  Health{Dropped: 1, DroppedBy: DropCounts{Test: 1}},
			sends: 1,
		},
		"unanswered until it expires": {
			want: Health{Dropped: 1, DroppedBy: DropCounts{Expired: 1}},
		},
		// The two rows below expire at different points of a send; the
		// publisher's first expiry check after them is what counts it.
		"expired while it was sent": {
			send: func(e env, _ Event) (*http.Response, error) {
				*e.now = e.now.Add(time.Hour)
				return respond(503, ""), nil
			},
			want:  Health{Dropped: 1, DroppedBy: DropCounts{Expired: 1}},
			sends: 1,
		},
		"expired during a heartbeat": {
			heartbeat: func(e env) {
				if e.q.Health(*e.now).Pending > 0 {
					*e.now = e.now.Add(time.Hour)
				}
			},
			want:  Health{Dropped: 1, DroppedBy: DropCounts{Expired: 1}},
			sends: 1,
		},
		"too large to retain": {
			event: Event{Kind: "x", Body: []byte(strings.Repeat("b", 3*maxPublisherIdentityBytes))},
			setup: func(e env) {
				e.q.mu.Lock()
				e.q.limits.MaxBytes = 0
				e.q.mu.Unlock()
			},
			want:  Health{Dropped: 1, DroppedBy: DropCounts{Oversized: 1}},
			sends: -1,
		},
		"unencodable": {
			event: Event{Kind: "x", Time: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
			want:  Health{Dropped: 1, DroppedBy: DropCounts{Unencodable: 1}},
			sends: -1,
		},
		"stopped before it was sent": {
			setup: func(e env) { e.cancel() },
			want:  Health{Dropped: 1, DroppedBy: DropCounts{Stopped: 1}},
			sends: -1,
		},
		"stopped while waiting to retry": {
			wait: func(e env, _ time.Duration) error {
				e.cancel()
				return context.Canceled
			},
			want:  Health{Dropped: 1, DroppedBy: DropCounts{Stopped: 1}},
			sends: 1,
		},
		"a test event stopped while it was sent": {
			event: Event{Kind: "test"},
			send: func(e env, _ Event) (*http.Response, error) {
				e.cancel()
				return nil, context.Canceled
			},
			want:  Health{Dropped: 1, DroppedBy: DropCounts{Stopped: 1}},
			sends: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			q := NewQueue(Limits{MaxEvents: 8, MaxBytes: 1 << 20, MaxBodyBytes: 1 << 20})
			ev := c.event
			if ev.Kind == "" {
				ev.Kind = "x"
			}
			if !q.TryEmit(ev) {
				t.Fatal("the event was not queued")
			}
			now := time.Unix(0, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e := env{q: q, now: &now, cancel: cancel}
			if c.setup != nil {
				c.setup(e)
			}
			sends := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var got Event
				_ = json.NewDecoder(r.Body).Decode(&got)
				if got.Kind == "" {
					if c.heartbeat != nil {
						c.heartbeat(e)
					}
					// Once the event has left the queue, stop.
					if q.Health(now).Pending == 0 {
						cancel()
					}
					return respond(204, ""), nil
				}
				sends++
				if c.send != nil {
					return c.send(e, got)
				}
				return respond(503, ""), nil
			})}
			_ = RunPublisher(ctx, q, PublisherConfig{Source: "payer", Incarnation: "boot", URL: "https://sink", Client: client, Heartbeat: time.Millisecond,
				Jitter: func(d time.Duration) time.Duration { return d },
				Clock:  func() time.Time { return now },
				Wait: func(ctx context.Context, d time.Duration) error {
					if c.wait != nil {
						return c.wait(e, d)
					}
					now = now.Add(d)
					return ctx.Err()
				}})
			h := q.Health(now)
			if h.LastSequence != 1 || h.Pending != 0 || h.Acknowledged != c.want.Acknowledged || h.LastAcknowledged != c.want.LastAcknowledged ||
				h.Discarded != c.want.Discarded || h.Dropped != c.want.Dropped || h.DroppedBy != c.want.DroppedBy || !h.CountsAgree() {
				t.Fatalf("health = %+v\nwant counts %+v", h, c.want)
			}
			switch {
			case c.sends > 0 && sends != c.sends:
				t.Fatalf("sent %d times, want %d", sends, c.sends)
			case c.sends < 0 && sends != 0:
				t.Fatalf("sent %d times, want none", sends)
			case c.sends == 0 && sends < 2:
				t.Fatalf("sent %d times: a retried event is sent more than once", sends)
			}
		})
	}
}

// The queue counts every sequenced event once, however it leaves: refused or
// shed from a full queue, acknowledged, discarded, or dropped for a reason.
// Its heartbeat's counts always agree.
func TestQueueCountsEveryEventOnce(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 5, MaxBytes: 1 << 20})
	var seqs []uint64
	for i := 0; i < 7; i++ { // two beyond the queue's five: refused as queue full
		if q.TryEmit(Event{Kind: "x"}) {
			seqs = append(seqs, q.nextSequence)
		}
	}
	q.Acknowledge(seqs[2])
	q.Acknowledge(seqs[0])
	q.discard(seqs[1])
	q.dropFor(seqs[3], invalid)
	q.Acknowledge(seqs[0]) // already gone: not counted again
	h := q.Health(time.Unix(0, 0))
	want := Health{Time: time.Unix(0, 0), LastSequence: 7, LastAcknowledged: 3, Pending: 1, Dropped: 3, State: "degraded",
		Acknowledged: 2, Discarded: 1, DroppedBy: DropCounts{QueueFull: 2, Invalid: 1}}
	if h != want {
		t.Fatalf("health = %+v\nwant     %+v", h, want)
	}
	if !h.CountsAgree() {
		t.Fatal("the queue's counts do not agree")
	}
	q.Drop(seqs[4])
	if h := q.Health(time.Unix(0, 0)); h.DroppedBy.Expired != 1 || h.Pending != 0 || !h.CountsAgree() {
		t.Fatalf("Drop is an expiry: %+v", h)
	}

	// A full queue sheds undelivered events to admit an access event: each
	// shed event is a queue-full drop, and the counts still agree.
	q = NewQueue(Limits{MaxEvents: 3, MaxBytes: 1 << 20})
	for i := 0; i < 3; i++ {
		q.TryEmit(Event{Kind: "x"})
	}
	if !q.TryEmit(Event{Kind: KindAccess}) {
		t.Fatal("the access event was not admitted")
	}
	if h := q.Health(time.Unix(0, 0)); h.DroppedBy != (DropCounts{QueueFull: 1}) || h.Dropped != 1 || h.Pending != 3 || !h.CountsAgree() {
		t.Fatalf("after a shed: %+v", h)
	}
}

// Every drop reason is counted in its own DropCounts field, and CountsAgree
// sums every one of them: a reason that reached Dropped but no field would
// make every later heartbeat refused.
func TestEveryDropReasonIsCountedAndSummed(t *testing.T) {
	seen := map[DropCounts]bool{}
	for r := queueFull; r < dropReasons; r++ {
		var d DropCounts
		d.add(r)
		if d == (DropCounts{}) || seen[d] {
			t.Fatalf("reason %d is counted in no field of its own: %+v", r, d)
		}
		seen[d] = true
		if h := (Health{LastSequence: 1, Dropped: 1, DroppedBy: d}); !h.CountsAgree() {
			t.Fatalf("reason %d is not summed: %+v", r, d)
		}
	}
}
