package observer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Handler serves GET /events (SSE) and immediate GET /health without advertising
// source completion support.
func (h *Hub) Handler() http.Handler { return h.HandlerWithBarrier(nil) }

// HandlerWithBarrier adds diagnostic source completion when wait is non-nil.
// The waiter must honor context cancellation and include observer delivery before
// returning. The app exposes this handler only on its opt-in loopback listener.
func (h *Hub) HandlerWithBarrier(wait func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", h.handleEvents)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if wait != nil {
			h.writeCompletion(w)
			return
		}
		h.mu.Lock()
		n := h.seq
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"events":%d}`, n)
	})
	if wait != nil {
		mux.HandleFunc("POST /barrier", func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			err := wait(ctx)
			if err == nil {
				err = ctx.Err()
			}
			if err != nil {
				status := http.StatusServiceUnavailable
				if errors.Is(err, context.DeadlineExceeded) {
					status = http.StatusGatewayTimeout
				}
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "observer completion unavailable: "+err.Error(), status)
				return
			}
			h.writeCompletion(w)
		})
	}
	return mux
}
func (h *Hub) writeCompletion(w http.ResponseWriter) {
	h.mu.Lock()
	n := h.seq
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Protocol    int    `json:"protocol"`
		Incarnation string `json:"incarnation"`
		Events      uint64 `json:"events"`
	}{1, h.incarnation, n})
}
