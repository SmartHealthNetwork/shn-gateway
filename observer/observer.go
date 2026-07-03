// Package observer turns the engine's ObserverEvent callback into a
// loopback-served SSE stream — the SHN Kit flow inspector's data source (Kit
// spec §6.1). A ring buffer (last bufSize events) gives late/reconnecting
// subscribers replay via Last-Event-ID; live delivery is per-subscriber
// buffered and LOSSY under backpressure (a slow consumer misses events and
// re-syncs from the buffer on reconnect — the stream is diagnostic, never
// load-bearing for exchange correctness).
package observer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

const (
	bufSize  = 1000
	subDepth = 1024
)

type sequenced struct {
	Seq uint64 `json:"seq"`
	engine.ObserverEvent
}

// Hub fan-outs observer events to SSE subscribers. Zero value is not usable;
// call NewHub.
type Hub struct {
	mu   sync.Mutex
	seq  uint64
	buf  [][]byte // marshaled sequenced events, oldest first, len<=bufSize
	subs map[chan []byte]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: make(map[chan []byte]struct{})}
}

// Emit assigns the next seq, buffers, and fans out. Assign to
// engine.Config.Observer. NEVER drops an event for its payload: a payload
// snapshot is the raw bytes seen at the edge, and a malformed ingress body —
// the classic 400 case — is exactly the failure the inspector must show, but
// invalid JSON in a json.RawMessage makes json.Marshal fail. Non-JSON
// payloads are re-encoded as a JSON string and flagged in Detail.
func (h *Hub) Emit(e engine.ObserverEvent) {
	if len(e.Payload) > 0 && !json.Valid(e.Payload) {
		quoted, _ := json.Marshal(string(e.Payload)) // marshaling a string never errors
		e.Payload = quoted
		if e.Detail != "" {
			e.Detail += "; "
		}
		e.Detail += "payload was not valid JSON (delivered as string)"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	b, err := json.Marshal(sequenced{Seq: h.seq, ObserverEvent: e})
	if err != nil {
		return // unreachable after payload sanitization; kept so Emit stays total
	}
	h.buf = append(h.buf, b)
	if len(h.buf) > bufSize {
		h.buf = h.buf[len(h.buf)-bufSize:]
	}
	for ch := range h.subs {
		select {
		case ch <- b:
		default: // lossy under backpressure by design (see package doc)
		}
	}
}

// subscribe registers a live channel and returns the replay set (buffered
// events with seq > after) plus an unsubscribe func.
func (h *Hub) subscribe(after uint64) ([][]byte, chan []byte, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan []byte, subDepth)
	h.subs[ch] = struct{}{}
	var replay [][]byte
	for _, b := range h.buf {
		var s struct {
			Seq uint64 `json:"seq"`
		}
		if json.Unmarshal(b, &s) == nil && s.Seq > after {
			replay = append(replay, b)
		}
	}
	return replay, ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, ch)
	}
}

// Handler serves GET /events (SSE) and GET /health.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", h.handleEvents)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		n := h.seq
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"events":%d}`, n)
	})
	return mux
}

func (h *Hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var after uint64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.ParseUint(v, 10, 64)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	replay, ch, unsub := h.subscribe(after)
	defer unsub()
	write := func(b []byte) bool {
		var s struct {
			Seq uint64 `json:"seq"`
		}
		_ = json.Unmarshal(b, &s)
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", s.Seq, b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	for _, b := range replay {
		if !write(b) {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			if !write(b) {
				return
			}
		}
	}
}
