// Package observer serves the opt-in participant inspection stream over local
// SSE. Retained serialized buffers share their originating gateway's budget.
package observer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

const (
	bufSize          = 1000
	subDepth         = 1024
	maxSubscribers   = 32
	maxRetainedBytes = 32 << 20
)

type sequenced struct {
	Seq uint64 `json:"seq"`
	engine.ObserverEvent
}
type retainedEvent struct {
	seq         uint64
	data        []byte
	refs        int
	reservation *engine.ObserverReservation
	owner       *engine.ObserverInspection
}

// Hub shares each immutable serialized event between ring, replay, subscriber
// queue and in-flight writer owners. Network writes never hold the Hub lock.
type Hub struct {
	incarnation  string
	mu           sync.Mutex
	seq, dropped uint64
	owner        *engine.ObserverInspection
	retained     int
	closed       bool
	buf          []*retainedEvent
	subs         map[*subscription]struct{}
}

func NewHub() *Hub { return newHub(rand.Text()) }
func newHub(incarnation string) *Hub {
	return &Hub{incarnation: incarnation, subs: make(map[*subscription]struct{})}
}

// serializationBound includes JSON escaping, malformed-body quoting and its
// bounded metadata marshaling allocations. Conservative admission can shed a body smaller
// than the eight-MiB engine limit; it never allocates first and accounts later.
func serializationBound(e engine.ObserverEvent) int {
	n := 1024
	add := func(size, factor int) {
		if size > (maxRetainedBytes-n)/factor {
			n = maxRetainedBytes + 1
		} else {
			n += size * factor
		}
	}
	add(len(e.Payload), 7)
	for _, s := range []string{e.Kind, e.LegType, e.Direction, e.CorrelationID, e.Counterpart, e.AuthorityFrame, e.Op, e.Detail} {
		add(len(s), 24)
	}
	if e.Route != nil {
		add(len(e.Route.Token)+len(e.Route.BuildLine)+len(e.Route.BridgeIssue), 24)
		add(len(e.Route.Chain), 128)
		for _, v := range e.Route.Chain {
			add(len(v.Module)+len(v.From)+len(v.To)+len(v.Class), 24)
		}
		add(len(e.Route.Own)+len(e.Route.Peer), 8)
		for _, v := range e.Route.Own {
			add(len(v), 24)
		}
		for _, v := range e.Route.Peer {
			add(len(v), 24)
		}
	}
	return n
}

// Payload bytes never enter encoding/json's pooled marshaling buffer. A single
// charged destination receives compact JSON or an escaped malformed-body string.
func marshalEvent(seq uint64, e engine.ObserverEvent) ([]byte, error) {
	payload := e.Payload
	e.Payload = nil
	valid := json.Valid(payload)
	if len(payload) > 0 && !valid {
		if e.Detail != "" {
			e.Detail += "; "
		}
		e.Detail += "payload was not valid JSON (delivered as string)"
	}
	if len(payload) > 0 {
		e.Payload = json.RawMessage("null")
	}
	metadata, err := json.Marshal(sequenced{Seq: seq, ObserverEvent: e})
	if err != nil || len(payload) == 0 {
		return metadata, err
	}
	marker := bytes.Index(metadata, []byte(`"payload":null`)) + len(`"payload":`)
	capacity := len(metadata) + 6*len(payload) + 2
	out := make([]byte, 0, capacity)
	out = append(out, metadata[:marker]...)
	if valid {
		compact := bytes.NewBuffer(make([]byte, 0, len(payload)))
		if err := json.Compact(compact, payload); err != nil {
			return nil, err
		}
		buf := bytes.NewBuffer(out)
		json.HTMLEscape(buf, compact.Bytes())
		out = buf.Bytes()
	} else {
		out = append(out, '"')
		for len(payload) > 0 {
			r, n := utf8.DecodeRune(payload)
			payload = payload[n:]
			switch r {
			case '"', '\\':
				out = append(out, '\\', byte(r))
			case '\b':
				out = append(out, `\b`...)
			case '\f':
				out = append(out, `\f`...)
			case '\n':
				out = append(out, `\n`...)
			case '\r':
				out = append(out, `\r`...)
			case '\t':
				out = append(out, `\t`...)
			default:
				if r < 0x20 || r == '<' || r == '>' || r == '&' || r == 0x2028 || r == 0x2029 || (r == utf8.RuneError && n == 1) {
					const hex = "0123456789abcdef"
					out = append(out, '\\', 'u', hex[(r>>12)&15], hex[(r>>8)&15], hex[(r>>4)&15], hex[r&15])
				} else {
					out = utf8.AppendRune(out, r)
				}
			}
		}
		out = append(out, '"')
	}
	return append(out, metadata[marker+len("null"):]...), nil
}
func (h *Hub) lossLocked(owner *engine.ObserverInspection) { h.dropped++; owner.Drop() }

// Emit reserves before serialization. Loss is visible in health and makes the
// source completion barrier fail, including when a slow subscriber drops data.
func (h *Hub) Emit(e engine.ObserverEvent) {
	owner := e.Inspection()
	bound := serializationBound(e)
	h.mu.Lock()
	if owner != nil {
		h.owner = owner
	}
	if h.closed || bound > maxRetainedBytes-h.retained {
		h.lossLocked(owner)
		h.mu.Unlock()
		return
	}
	h.retained += bound
	h.mu.Unlock()
	reservation, ok := owner.Reserve(bound)
	if !ok {
		h.mu.Lock()
		h.retained -= bound
		h.lossLocked(owner)
		h.mu.Unlock()
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	b, err := marshalEvent(h.seq+1, e)
	if err != nil || h.closed || cap(b) > bound {
		h.retained -= bound
		reservation.Release()
		h.lossLocked(owner)
		return
	}
	h.seq++
	h.retained -= bound - cap(b)
	reservation.Shrink(cap(b))
	item := &retainedEvent{seq: h.seq, data: b, refs: 1, reservation: reservation, owner: owner}
	h.buf = append(h.buf, item)
	if len(h.buf) > bufSize {
		h.releaseLocked(h.buf[0])
		copy(h.buf, h.buf[1:])
		h.buf[len(h.buf)-1] = nil
		h.buf = h.buf[:len(h.buf)-1]
	}
	for s := range h.subs {
		select {
		case s.ch <- item:
			item.refs++
		default:
			h.lossLocked(owner)
		}
	}
}
func (h *Hub) releaseLocked(b *retainedEvent) {
	b.refs--
	if b.refs == 0 {
		h.retained -= cap(b.data)
		b.data = nil
		b.reservation.Release()
	}
}

// Close releases replay/ring/queued ownership immediately. A blocked writer
// retains its one charged buffer until it returns; at most 32 writers exist.
func (h *Hub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for s := range h.subs {
		s.closeLocked()
	}
	for _, b := range h.buf {
		h.releaseLocked(b)
	}
	h.buf = nil
}

type subscription struct {
	hub    *Hub
	ch     chan *retainedEvent
	replay []*retainedEvent
	done   chan struct{}
	closed bool
}

func (h *Hub) subscribe(after uint64) (*subscription, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.subs) >= maxSubscribers {
		h.lossLocked(nil)
		return nil, false
	}
	s := &subscription{hub: h, ch: make(chan *retainedEvent, subDepth), done: make(chan struct{})}
	for _, b := range h.buf {
		if b.seq > after {
			b.refs++
			s.replay = append(s.replay, b)
		}
	}
	h.subs[s] = struct{}{}
	return s, true
}
func (s *subscription) close() { s.hub.mu.Lock(); defer s.hub.mu.Unlock(); s.closeLocked() }
func (s *subscription) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	close(s.done)
	delete(s.hub.subs, s)
	for _, b := range s.replay {
		s.hub.releaseLocked(b)
	}
	s.replay = nil
	for {
		select {
		case b := <-s.ch:
			s.hub.releaseLocked(b)
		default:
			return
		}
	}
}
func (s *subscription) next(ctx context.Context) *retainedEvent {
	h := s.hub
	h.mu.Lock()
	if s.closed {
		h.mu.Unlock()
		return nil
	}
	if len(s.replay) > 0 {
		b := s.replay[0]
		s.replay[0] = nil
		s.replay = s.replay[1:]
		h.mu.Unlock()
		return b
	}
	h.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil
	case <-s.done:
		return nil
	case b := <-s.ch:
		h.mu.Lock()
		closed := s.closed
		if closed {
			h.releaseLocked(b)
		}
		h.mu.Unlock()
		if closed {
			return nil
		}
		return b
	}
}
func (s *subscription) release(b *retainedEvent) {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.releaseLocked(b)
}
func (h *Hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	after, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	s, ok := h.subscribe(after)
	if !ok {
		http.Error(w, "observer subscription unavailable", 503)
		return
	}
	defer s.close()
	w.Header().Set("X-SHN-Observer-Incarnation", h.incarnation)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	fl.Flush()
	for {
		b := s.next(r.Context())
		if b == nil {
			return
		}
		err := func() error {
			defer s.release(b)
			if _, err := fmt.Fprintf(w, "id: %d\ndata: ", b.seq); err != nil {
				return err
			}
			if _, err := w.Write(b.data); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\n\n"); err != nil {
				return err
			}
			fl.Flush()
			return nil
		}()
		if err != nil {
			h.mu.Lock()
			h.lossLocked(b.owner)
			h.mu.Unlock()
			return
		}
	}
}
