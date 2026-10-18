// requester.go — the answering gateway resolves the requester before any
// handler runs. A leg's answer is sealed to its requester's registered
// encryption key, so a requester this gateway's registry does not hold yet (a
// participant that registered within the registrar poll interval) cannot be
// answered at all. Acting on such a leg first — adjudicating, storing, calling
// the participant's own system — and then failing to seal would leave the
// requester a failure it cannot tell from any other, and a retry could repeat
// the act. So the gateway refreshes its registry once on a miss, and a
// requester still unknown after that is refused before anything is done,
// with a retryable answer that says so. The refusal is the network's
// routing-and-addressing primitive (the gateway cannot address an answer), the
// same at every conformance level.
package engine

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// NotActedHeader is the reserved network header a recipient's gateway sets on
// one answer only: the 503 it gives a leg whose requester it does not know
// yet, before acting on it. Its value states why, from the closed set below.
// It is the recipient's own attestation that it took no action; the Hub
// relays it to the requester and does not verify it. No other answer this
// gateway writes on its inbound route carries it (notActedFence).
// internal/hubsvc reads it; the root module's tests pin the two names equal.
const NotActedHeader = "X-SHN-Not-Acted"

// NotActedRequesterUnknown is NotActedHeader's one value: the requester is not
// in this gateway's registry yet.
const NotActedRequesterUnknown = "requester-unknown"

// refusalRequesterUnknown is the inbound refusal's text.
const refusalRequesterUnknown = "requester not yet in this gateway's registry"

// requesterUnknownRetryAfter is the Retry-After, in seconds, the refusal
// carries: the registrar poll interval, after which the registry has read the
// requester's registration.
const requesterUnknownRetryAfter = 3

// registryRefreshBudget bounds how long a leg waits for the registry refresh a
// miss starts. A refresh that has not finished by then leaves the leg refused
// (retryably), never held.
const registryRefreshBudget = 2 * time.Second

// registryRefresher runs Config.RegistryRefresh single-flight: concurrent
// misses share one refresh, and each waits for it at most its budget.
type registryRefresher struct {
	mu       sync.Mutex
	inflight chan struct{}
	// joined counts the callers waiting on the current flight (the
	// single-flight row reads it).
	joined int
}

// joinedCount is how many callers have joined the current flight.
func (s *registryRefresher) joinedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.joined
}

// refresh runs fn unless a refresh is already under way, then waits for that
// refresh to end, for budget, or for ctx, whichever is first. fn runs under
// its own context bounded by budget and detached from ctx, so one caller
// leaving does not cut short the refresh the others are waiting for.
func (s *registryRefresher) refresh(ctx context.Context, fn func(context.Context) error, budget time.Duration) {
	s.mu.Lock()
	done := s.inflight
	if done == nil {
		done = make(chan struct{})
		s.inflight = done
		s.joined = 0
		go func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
			defer cancel()
			if err := fn(rctx); err != nil {
				log.Printf("gateway: registry refresh on an unknown requester failed: %v", err)
			}
			s.mu.Lock()
			s.inflight = nil
			s.mu.Unlock()
			close(done)
		}()
	}
	s.joined++
	s.mu.Unlock()
	t := time.NewTimer(budget)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}
}

// requesterKnown reports whether the registry holds id, refreshing it once
// (single-flight, bounded) on a miss when the deployment can refresh it.
func (g *Gateway) requesterKnown(ctx context.Context, id string) bool {
	if _, ok := g.cfg.Reg.Lookup(id); ok {
		return true
	}
	if g.cfg.RegistryRefresh == nil {
		return false
	}
	budget := g.regRefreshBudget
	if budget <= 0 {
		budget = registryRefreshBudget
	}
	g.regRefresh.refresh(ctx, g.cfg.RegistryRefresh, budget)
	_, ok := g.cfg.Reg.Lookup(id)
	return ok
}

// notActedFence is the inbound route's writer: it removes NotActedHeader from
// every answer except the one the requester guard attests, so no handler,
// connector or relayed answer can claim on this gateway's behalf that it took
// no action.
type notActedFence struct {
	http.ResponseWriter
	attested bool
}

// Unwrap exposes the underlying writer to http.ResponseController and to
// scopeOf.
func (f *notActedFence) Unwrap() http.ResponseWriter { return f.ResponseWriter }

func (f *notActedFence) WriteHeader(code int) {
	if !f.attested || code != http.StatusServiceUnavailable {
		f.Header().Del(NotActedHeader)
	}
	f.ResponseWriter.WriteHeader(code)
}

func (f *notActedFence) Write(b []byte) (int, error) {
	if !f.attested {
		f.Header().Del(NotActedHeader)
	}
	return f.ResponseWriter.Write(b)
}

// refuseRequesterUnknown writes the guard's answer: 503, uncacheable, with
// Retry-After and the attested NotActedHeader.
func refuseRequesterUnknown(w http.ResponseWriter, fence *notActedFence) {
	fence.attested = true
	w.Header().Set("Retry-After", strconv.Itoa(requesterUnknownRetryAfter))
	w.Header().Set(NotActedHeader, NotActedRequesterUnknown)
	writeUnavailable(w, refusalRequesterUnknown)
}
