// edgecapture.go — the bounded pre-seal edge-capture store: a local
// inspection surface over each transformed egress leg's own before/after
// payload pair, captured at egressAdapt's own choke point (never on the
// wire, never in the audit record, never part of any conformance surface).
// Enabled only by the loudly-named demonstration env, SHN_DEMO_EDGE_CAPTURE
// (Config.DemoEdgeCapture) — production carries it off.
package engine

import (
	"encoding/json"
	"sync"
	"time"
)

// edgeCaptureCap is the FIFO bound on how many legs the store holds at once.
// A bound, not a tuning knob: this is a bounded inspection surface, and
// growing it without limit would turn it into an unbounded log.
const edgeCaptureCap = 32

// edgeCaptureMaxPayloadBytes is the combined Before+After size above which
// an entry is dropped rather than stored.
const edgeCaptureMaxPayloadBytes = 2 << 20 // 2 MiB

// EdgeCapture is one transformed egress leg's pre-seal before/after payload
// pair, plus the identity and routing context needed to make sense of it:
// which leg (CorrelationID/LegType), which compatibility module walked which
// lines (Contract/From/To/Chain), what it reported losing or synthesizing
// (LossReports), and when it ran.
type EdgeCapture struct {
	CorrelationID string
	LegType       string
	Contract      string
	From          string
	To            string
	Chain         []ChainStep
	LossReports   []LossReport
	Before        json.RawMessage
	After         json.RawMessage
	CapturedAt    time.Time
}

// edgeCaptureStore is a bounded in-memory inspection surface for a
// participant's OWN pre-seal egress payloads — never part of the wire, the
// audit record, or any conformance surface. It exists only so the
// participant that produced a transformed leg can look at what it just
// built, enabled only by the loudly-named demonstration env
// (SHN_DEMO_EDGE_CAPTURE / Config.DemoEdgeCapture); production carries it
// disabled, at zero cost.
//
// FIFO-bounded (edgeCaptureCap entries) with a per-entry combined-payload
// cap (edgeCaptureMaxPayloadBytes, Before+After together): an oversize pair
// is simply not stored, never truncated — a truncated payload would be
// misleading, not merely incomplete.
//
// Record deep-copies Before/After on entry. A caller's byte slice may alias
// another slice it also holds onto (egressAdapt's envelope carve-out hands
// back the exact slice it was given as both its input and its output), and
// this store must never let a caller's later mutation of its own buffer
// silently corrupt an already-captured entry.
//
// Single-writer-per-id assumption: Record replaces an existing entry in
// place (at its original FIFO position) when given a CorrelationID already
// present, rather than appending a second entry under it. Production mints a
// fresh correlation id per egress leg, so this never collapses distinct legs
// in practice; a caller that records more than once under the SAME id will
// only ever see the last write.
type edgeCaptureStore struct {
	mu      sync.Mutex
	cap     int
	order   []string
	entries map[string]EdgeCapture
}

// newEdgeCaptureStore constructs an empty store bounded to capacity entries.
func newEdgeCaptureStore(capacity int) *edgeCaptureStore {
	return &edgeCaptureStore{cap: capacity, entries: make(map[string]EdgeCapture, capacity)}
}

// Record stores e. An entry whose combined Before+After size exceeds
// edgeCaptureMaxPayloadBytes is dropped rather than stored. Otherwise: a
// CorrelationID already present replaces its existing entry in place (see
// the store's single-writer-per-id doc comment); a genuinely new
// CorrelationID is appended, evicting the oldest entry first if the store is
// already at capacity.
func (s *edgeCaptureStore) Record(e EdgeCapture) {
	if len(e.Before)+len(e.After) > edgeCaptureMaxPayloadBytes {
		return
	}
	// Deep-copy Before/After so a caller's later mutation of an aliased
	// buffer can never reach back into the stored entry. cloneRawMessage
	// preserves nil-ness (nil in ⇒ nil out): a naive make+copy would turn a
	// nil payload into a non-nil EMPTY json.RawMessage, and encoding/json
	// refuses to marshal an empty (len 0) RawMessage — the capture-fetch
	// endpoint would 200 with a broken/empty body, since its handler has
	// already written the status header by the time Encode fails.
	e.Before = cloneRawMessage(e.Before)
	e.After = cloneRawMessage(e.After)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[e.CorrelationID]; !exists {
		if len(s.order) >= s.cap {
			oldest := s.order[0]
			s.order = s.order[1:]
			delete(s.entries, oldest)
		}
		s.order = append(s.order, e.CorrelationID)
	}
	s.entries[e.CorrelationID] = e
}

// Get returns the captured entry for correlationID, if any.
func (s *edgeCaptureStore) Get(correlationID string) (EdgeCapture, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[correlationID]
	return e, ok
}

// cloneRawMessage deep-copies raw, preserving nil-ness: a nil input yields a
// nil output, never a non-nil empty slice. json.RawMessage's own
// MarshalJSON special-cases nil as the literal `null`; a non-nil EMPTY
// RawMessage instead fails to marshal at all ("unexpected end of JSON
// input"), so silently promoting nil to empty here would be a wire-breaking
// bug, not a harmless normalization.
func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}
