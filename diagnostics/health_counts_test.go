package diagnostics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The counts, when a publisher sends them, account for every sequenced event
// exactly once; a Health without them is judged as before.
func TestHealthCountsAgree(t *testing.T) {
	base := Health{Source: "payer", Incarnation: "boot", Time: time.Unix(0, 0), State: "degraded", LastSequence: 724, LastAcknowledged: 720}
	with := func(f func(*Health)) Health { h := base; f(&h); return h }
	for name, c := range map[string]struct {
		h    Health
		want bool
	}{
		"no counts (a publisher that does not keep them)": {base, true},
		"every event accounted for": {with(func(h *Health) {
			h.Acknowledged, h.Discarded, h.Dropped, h.Pending = 700, 4, 18, 2
			h.DroppedBy = DropCounts{Expired: 16, QueueFull: 2}
		}), true},
		"only discards and acknowledgements": {with(func(h *Health) { h.Acknowledged, h.Discarded = 720, 4 }), true},
		"an event unaccounted for":           {with(func(h *Health) { h.Acknowledged, h.Discarded = 719, 4 }), false},
		"an event counted twice":             {with(func(h *Health) { h.Acknowledged, h.Discarded = 720, 5 }), false},
		// Each count alone makes the counts present.
		"only discards, disagreeing": {Health{LastSequence: 10, Discarded: 3}, false},
		"only discards, agreeing":    {Health{LastSequence: 3, Discarded: 3}, true},
		"only reasons, disagreeing":  {Health{LastSequence: 10, Dropped: 1, DroppedBy: DropCounts{Expired: 1}}, false},
		"only reasons, agreeing":     {Health{LastSequence: 1, Dropped: 1, DroppedBy: DropCounts{Expired: 1}}, true},
		"drops without their reasons": {with(func(h *Health) {
			h.Acknowledged, h.Discarded, h.Dropped = 702, 4, 18
		}), false},
		"reasons that do not add up to the drops": {with(func(h *Health) {
			h.Acknowledged, h.Discarded, h.Dropped = 702, 4, 18
			h.DroppedBy = DropCounts{Expired: 17}
		}), false},
		"more acknowledged than the highest sequence acknowledged": {Health{LastSequence: 10, LastAcknowledged: 5, Acknowledged: 10}, false},
		"acknowledged with none acknowledged":                      {Health{LastSequence: 10, Acknowledged: 7, Discarded: 3}, false},
		"none acknowledged with one acknowledged":                  {Health{LastSequence: 10, LastAcknowledged: 4, Discarded: 10}, false},
		"counts that overflow": {with(func(h *Health) {
			h.Acknowledged, h.LastAcknowledged, h.Discarded = ^uint64(0), ^uint64(0), 725
		}), false},
		"reasons that overflow on their own": {Health{LastSequence: 1, LastAcknowledged: 1, Acknowledged: 1,
			DroppedBy: DropCounts{Expired: ^uint64(0), QueueFull: 1}}, false},
		"reasons that overflow": {with(func(h *Health) {
			h.Acknowledged, h.Dropped = 1, 723
			h.DroppedBy = DropCounts{Expired: ^uint64(0), QueueFull: 724}
		}), false},
	} {
		if got := c.h.CountsAgree(); got != c.want {
			t.Errorf("%s: CountsAgree = %v, want %v", name, got, c.want)
		}
	}
}

// Health stays comparable: the published type can be compared with == and
// used as a map key, as before the counts.
func TestHealthIsComparable(t *testing.T) {
	a := Health{Source: "p", DroppedBy: DropCounts{Expired: 1}}
	if a != a || map[Health]bool{a: true}[a] != true {
		t.Fatal("Health is not comparable")
	}
}

// A publisher that keeps no counts sends exactly the fields it sent before
// they existed, so an ingest that predates them still accepts it.
func TestHealthWithoutCountsSendsTheOldShape(t *testing.T) {
	raw, err := json.Marshal(Health{Source: "payer", Incarnation: "boot", Time: time.Unix(0, 0).UTC(), LastSequence: 3, LastAcknowledged: 3, State: "healthy"})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	for _, k := range []string{"acknowledged", "discarded", "droppedBy"} {
		if _, ok := keys[k]; ok {
			t.Fatalf("a Health without counts sends %q: %s", k, raw)
		}
	}
	if len(got) != 8 || !strings.Contains(string(raw), `"lastAcknowledged":3`) {
		t.Fatalf("fields = %v", got)
	}
}
