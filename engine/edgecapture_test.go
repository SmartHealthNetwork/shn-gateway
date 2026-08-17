// edgecapture_test.go — the edgeCaptureStore's own bounds contract, isolated
// from egressAdapt (see egressadapt_test.go for the capture-hook tests: what
// gets recorded, when, and the on/off conformance-neutrality guarantee).
package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestEdgeCaptureStore_BoundsAndEviction(t *testing.T) {
	s := newEdgeCaptureStore(edgeCaptureCap)
	for i := 0; i < edgeCaptureCap+1; i++ {
		s.Record(EdgeCapture{CorrelationID: fmt.Sprintf("c%d", i), Before: []byte(`{}`), After: []byte(`{}`)})
	}
	if _, ok := s.Get("c0"); ok {
		t.Fatalf("entry 0 must be evicted after edgeCaptureCap+1 records (FIFO cap %d)", edgeCaptureCap)
	}
	if _, ok := s.Get(fmt.Sprintf("c%d", edgeCaptureCap)); !ok {
		t.Fatalf("newest entry must be present")
	}
}

func TestEdgeCaptureStore_OversizeNotStored(t *testing.T) {
	s := newEdgeCaptureStore(edgeCaptureCap)
	// Before alone is exactly 2 MiB (edgeCaptureMaxPayloadBytes) — AT the cap,
	// not over it. It's the combined Before+After that must exceed the cap
	// (Record's own `>` check is strict): the 2-byte After (`{}`) is what
	// tips len(Before)+len(After) one byte past 2 MiB.
	big := bytes.Repeat([]byte("x"), 2<<20)
	s.Record(EdgeCapture{CorrelationID: "big", Before: big, After: []byte(`{}`)})
	if _, ok := s.Get("big"); ok {
		t.Fatalf("oversize entry must not be stored (combined payload cap 2 MiB)")
	}
}

// TestEdgeCaptureStore_ExactBoundaryStored: a combined size exactly AT the
// cap is stored (the cap check is strict `>`, not `>=`) — one byte over is
// the boundary TestEdgeCaptureStore_OversizeNotStored above proves is
// dropped.
func TestEdgeCaptureStore_ExactBoundaryStored(t *testing.T) {
	s := newEdgeCaptureStore(edgeCaptureCap)
	before := bytes.Repeat([]byte("x"), (2<<20)-2) // + the 2-byte After below == exactly 2 MiB
	s.Record(EdgeCapture{CorrelationID: "exact", Before: before, After: []byte(`{}`)})
	if _, ok := s.Get("exact"); !ok {
		t.Fatalf("an entry exactly at the combined cap must be stored, not dropped")
	}
}

// TestEdgeCaptureStore_DuplicateCorrelationIDReplacesInPlace proves the
// store's documented single-writer-per-id contract (edgecapture.go's Record
// doc comment): a CorrelationID already present is REPLACED — its content
// updates, but it keeps its ORIGINAL FIFO position rather than moving to the
// back as if newly inserted. Two distinct claims, both pinned here: (1) a
// replace never double-counts toward the cap (the store's length is
// unchanged), and (2) a replaced entry is evicted at its ORIGINAL insertion
// order, not refreshed to "most recently written".
func TestEdgeCaptureStore_DuplicateCorrelationIDReplacesInPlace(t *testing.T) {
	s := newEdgeCaptureStore(3)
	s.Record(EdgeCapture{CorrelationID: "a", Before: []byte(`{"n":1}`)})
	s.Record(EdgeCapture{CorrelationID: "b", Before: []byte(`{"n":2}`)})
	s.Record(EdgeCapture{CorrelationID: "c", Before: []byte(`{"n":3}`)})

	// Re-record "a" with new content: the store is already at capacity, so a
	// bug that appends rather than replaces would either grow past cap or
	// wrongly evict "b" here.
	s.Record(EdgeCapture{CorrelationID: "a", Before: []byte(`{"n":99}`)})
	if got, ok := s.Get("a"); !ok || string(got.Before) != `{"n":99}` {
		t.Fatalf(`replace must update content: got %+v ok=%v, want Before {"n":99}`, got, ok)
	}
	if len(s.order) != 3 {
		t.Fatalf("replacing a duplicate id must not grow the store: order=%v, want len 3", s.order)
	}

	// A genuinely new 4th id must evict whichever entry sits at FIFO
	// position 0. "a" was re-recorded above but MUST NOT have moved to the
	// back — so "a", not "b", is still the oldest position and is the one
	// evicted here.
	s.Record(EdgeCapture{CorrelationID: "d"})
	if _, ok := s.Get("a"); ok {
		t.Fatal(`want "a" evicted — its FIFO position must not be refreshed by a replace`)
	}
	if _, ok := s.Get("b"); !ok {
		t.Fatal(`want "b" still present — replacing "a" must not disturb "b"'s position`)
	}
	if _, ok := s.Get("c"); !ok {
		t.Fatal(`want "c" still present`)
	}
	if _, ok := s.Get("d"); !ok {
		t.Fatal(`want "d" present — it is the entry that triggered the eviction`)
	}
}

// TestEdgeCaptureStore_NilPayloadPreservesNil: a nil Before/After must
// come back nil from Get, never a non-nil EMPTY json.RawMessage. Record's
// deep copy used to be a naive `make(json.RawMessage, len(nil))` + copy,
// which yields a non-nil zero-length slice — and encoding/json's
// RawMessage.MarshalJSON treats nil as the literal `null` but REFUSES to
// marshal a non-nil empty one at all. A nil Before/After is a real captured
// shape (e.g. an envelope leg with no meaningful body), so this is not an
// edge case the store gets to ignore.
func TestEdgeCaptureStore_NilPayloadPreservesNil(t *testing.T) {
	s := newEdgeCaptureStore(edgeCaptureCap)
	s.Record(EdgeCapture{CorrelationID: "nil-payload", Before: nil, After: nil})
	got, ok := s.Get("nil-payload")
	if !ok {
		t.Fatal("want the entry stored")
	}
	if got.Before != nil {
		t.Fatalf("got.Before = %#v (non-nil), want nil", got.Before)
	}
	if got.After != nil {
		t.Fatalf("got.After = %#v (non-nil), want nil", got.After)
	}
	// The marshal failure this store must never reintroduce: a non-nil empty
	// json.RawMessage errors on Marshal, where nil round-trips as `null`.
	if _, err := json.Marshal(got.Before); err != nil {
		t.Fatalf("marshaling got.Before must not error: %v", err)
	}
}
