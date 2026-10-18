package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ownCallCounter counts the requests this gateway makes on its own client
// (the Authorization Framework call a response leg needs, among others).
type ownCallCounter struct {
	rt    http.RoundTripper
	calls atomic.Int32
}

func (c *ownCallCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.rt.RoundTrip(r)
}

// guardPayer is a level payer whose own outbound calls are counted, with the
// requester's registry entry held aside so a row can take it out.
func guardPayer(t *testing.T, level ConformanceEnforcement) (*levelPayer, *ownCallCounter) {
	t.Helper()
	p := newLevelPayer(t, level)
	ct := &ownCallCounter{rt: p.g.cfg.Client.Transport}
	p.g.cfg.Client = &http.Client{Transport: ct}
	return p, ct
}

// wantRequesterUnknown asserts the guard's answer: 503, uncacheable, with
// Retry-After, the attested NotActedHeader and the named refusal.
func wantRequesterUnknown(t *testing.T, got payerAnswer) {
	t.Helper()
	if got.status != http.StatusServiceUnavailable || got.framed {
		t.Fatalf("answer = %d (framed %v) %s, want a bare 503", got.status, got.framed, got.body)
	}
	var e struct{ Error string }
	if err := json.Unmarshal(got.body, &e); err != nil || e.Error != refusalRequesterUnknown {
		t.Errorf("body = %s, want error %q", got.body, refusalRequesterUnknown)
	}
	if v := got.header.Get(NotActedHeader); v != NotActedRequesterUnknown {
		t.Errorf("%s = %q, want %q", NotActedHeader, v, NotActedRequesterUnknown)
	}
	if v := got.header.Get("Retry-After"); v != "3" {
		t.Errorf("Retry-After = %q, want 3", v)
	}
	if v := got.header.Get("Cache-Control"); v != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", v)
	}
}

// An unknown requester is refused before the leg is opened, at every level:
// the payer's system is not called (no adjudication), nothing is recorded,
// and the gateway makes no call of its own. The control row is the same leg
// from a known requester, which does all three, so the assertions can fail.
func TestRequesterGuard_UnknownRequesterRefusedBeforeActing(t *testing.T) {
	request := originatorBuiltConformantBundle(t, "MBR-COVERED")
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, ct := guardPayer(t, level)
			known := p.send(t, "pas-claim", "", request)
			if known.status != http.StatusOK || !known.framed {
				t.Fatalf("control: a known requester's submit = %d %s, want a framed 200", known.status, known.body)
			}
			if _, found := p.pendOf(t, known.corr); !found || p.partner.calls.Load() == 0 || ct.calls.Load() == 0 {
				t.Fatalf("control: pend found=%v, partner calls %d, own calls %d: the control must act", found, p.partner.calls.Load(), ct.calls.Load())
			}

			p.g.cfg.Reg.Delete(p.requester.ID)
			partnerBefore, ownBefore := p.partner.calls.Load(), ct.calls.Load()
			var recs exchangeRecords
			p.g.cfg.ExchangeObserved = recs.observe
			got := p.send(t, "pas-claim", "", request)
			wantRequesterUnknown(t, got)
			// The refusal is this gateway's, on routing and addressing.
			if rec := recs.only(t); rec.Outcome != ExchangeRefused || rec.RefusedBy != RefusedByPayerGateway || rec.Rule != RefusalRouting {
				t.Errorf("exchange record = %s by %s on %s, want refused by the payer gateway on %s", rec.Outcome, rec.RefusedBy, rec.Rule, RefusalRouting)
			}
			if n := p.partner.calls.Load() - partnerBefore; n != 0 {
				t.Errorf("the payer's system was called %d time(s) for an unknown requester", n)
			}
			if n := ct.calls.Load() - ownBefore; n != 0 {
				t.Errorf("the gateway made %d call(s) of its own for an unknown requester", n)
			}
			if _, found := p.pendOf(t, got.corr); found {
				t.Error("a record was stored for an unknown requester's submit")
			}
		})
	}
}

// A requester missing from the cached registry but present upstream is
// answered with no refusal: the guard's one refresh reads it.
func TestRequesterGuard_RefreshResolvesALaggingRequester(t *testing.T) {
	request := originatorBuiltConformantBundle(t, "MBR-COVERED")
	p, _ := guardPayer(t, EnforcementObserve)
	entry, _ := p.g.cfg.Reg.Lookup(p.requester.ID)
	p.g.cfg.Reg.Delete(p.requester.ID)
	var refreshes atomic.Int32
	p.g.cfg.RegistryRefresh = func(context.Context) error {
		refreshes.Add(1)
		p.g.cfg.Reg.Set(entry.ID, entry)
		return nil
	}
	got := p.send(t, "pas-claim", "", request)
	if got.status != http.StatusOK || !got.framed {
		t.Fatalf("lagging requester = %d %s, want a framed 200 after the refresh", got.status, got.body)
	}
	if n := refreshes.Load(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
	if _, found := p.pendOf(t, got.corr); !found {
		t.Error("the refreshed requester's submit was not acted on")
	}
	// Known now: no further refresh.
	if again := p.send(t, "pas-claim", "", request); again.status != http.StatusOK || refreshes.Load() != 1 {
		t.Errorf("second leg = %d with %d refreshes, want 200 with 1", again.status, refreshes.Load())
	}
}

// A refresh that fails, or that does not end within its budget, leaves the
// requester refused with the named, retryable answer, never a held leg.
func TestRequesterGuard_RefreshFailureOrTimeoutRefuses(t *testing.T) {
	request := originatorBuiltConformantBundle(t, "MBR-COVERED")
	release := make(chan struct{})
	defer close(release)
	for name, refresh := range map[string]func(context.Context) error{
		"fails": func(context.Context) error { return errors.New("registrar unreachable") },
		"times out with its context": func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		"ignores its context": func(context.Context) error {
			<-release
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := guardPayer(t, EnforcementObserve)
			p.g.cfg.Reg.Delete(p.requester.ID)
			p.g.regRefreshBudget = 50 * time.Millisecond
			p.g.cfg.RegistryRefresh = refresh
			start := time.Now()
			got := p.send(t, "pas-claim", "", request)
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("the leg was held %s by its refresh", d)
			}
			wantRequesterUnknown(t, got)
			if n := p.partner.calls.Load(); n != 0 {
				t.Errorf("the payer's system was called %d time(s)", n)
			}
		})
	}
}

// Concurrent misses share one refresh.
func TestRegistryRefresher_ConcurrentMissesRefreshOnce(t *testing.T) {
	var s registryRefresher
	var calls atomic.Int32
	release := make(chan struct{})
	fn := func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	}
	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { s.refresh(context.Background(), fn, 5*time.Second) })
	}
	// Every caller joins the one flight before it ends.
	deadline := time.Now().Add(5 * time.Second)
	for s.joinedCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d callers joined", s.joinedCount(), n)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	if c := calls.Load(); c != 1 {
		t.Fatalf("refreshes = %d for %d concurrent misses, want 1", c, n)
	}
	// A later miss starts a new one.
	s.refresh(context.Background(), func(context.Context) error { calls.Add(1); return nil }, time.Second)
	if c := calls.Load(); c != 2 {
		t.Errorf("refreshes after a later miss = %d, want 2", c)
	}
}

// Only the guard's 503 carries NotActedHeader: the fence strips it from any
// other answer, whoever set it.
func TestNotActedFence(t *testing.T) {
	for _, row := range []struct {
		name     string
		attested bool
		status   int // 0: Write with no WriteHeader
		want     bool
	}{
		{"attested 503", true, http.StatusServiceUnavailable, true},
		{"unattested 503", false, http.StatusServiceUnavailable, false},
		{"unattested 200", false, http.StatusOK, false},
		{"unattested implicit 200", false, 0, false},
		{"attested but not a 503", true, http.StatusForbidden, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			f := &notActedFence{ResponseWriter: rec, attested: row.attested}
			f.Header().Set(NotActedHeader, NotActedRequesterUnknown)
			if row.status != 0 {
				f.WriteHeader(row.status)
			}
			_, _ = f.Write([]byte(`{}`))
			if got := rec.Result().Header.Get(NotActedHeader) != ""; got != row.want {
				t.Errorf("header carried = %v, want %v", got, row.want)
			}
		})
	}
}

// Through the real inbound route: an answer other than the guard's never
// carries NotActedHeader, even when something before the handler set it. A
// served leg and a refusal before the guard (a bad Hub assertion) are both
// stripped; the guard's own answer keeps it.
func TestRequesterGuard_OnlyTheGuardAttests(t *testing.T) {
	request := originatorBuiltConformantBundle(t, "MBR-COVERED")
	p, _ := guardPayer(t, EnforcementObserve)
	presetHeader := func(rw http.ResponseWriter) { rw.Header().Set(NotActedHeader, NotActedRequesterUnknown) }
	p.preWrite = presetHeader

	served := p.send(t, "pas-claim", "", request)
	if served.status != http.StatusOK || served.header.Get(NotActedHeader) != "" {
		t.Errorf("served leg = %d carrying %s=%q, want 200 without it", served.status, NotActedHeader, served.header.Get(NotActedHeader))
	}
	wrongHub := p.hubPriv
	_, p.hubPriv = genED25519(t)
	refused := p.send(t, "pas-claim", "", request)
	p.hubPriv = wrongHub
	if refused.status != http.StatusForbidden || refused.header.Get(NotActedHeader) != "" {
		t.Errorf("refused assertion = %d carrying %s=%q, want 403 without it", refused.status, NotActedHeader, refused.header.Get(NotActedHeader))
	}
	p.g.cfg.Reg.Delete(p.requester.ID)
	wantRequesterUnknown(t, p.send(t, "pas-claim", "", request))
}

// Every refusal ahead of the guard is unchanged for an unknown requester:
// the guard answers only a leg whose Hub assertion, envelope and authority
// have all verified, so an unauthenticated or unauthorized leg is refused as
// before and learns nothing about this gateway's registry.
func TestRequesterGuard_EarlierRefusalsUnchanged(t *testing.T) {
	request := originatorBuiltConformantBundle(t, "MBR-COVERED")
	p, _ := guardPayer(t, EnforcementObserve)
	p.g.cfg.Reg.Delete(p.requester.ID)

	goodHub := p.hubPriv
	_, p.hubPriv = genED25519(t)
	if got := p.send(t, "pas-claim", "", request); got.status != http.StatusForbidden || strings.TrimSpace(string(got.body)) != `{"error":"missing or invalid hub assertion"}` {
		t.Errorf("bad hub assertion = %d %s, want the unchanged 403", got.status, got.body)
	}
	p.hubPriv = goodHub

	goodAuthz := p.authzPriv
	_, p.authzPriv = genED25519(t)
	if got := p.send(t, "pas-claim", "", request); got.status != http.StatusForbidden || strings.TrimSpace(string(got.body)) != `{"error":"authz verification failed"}` {
		t.Errorf("bad authz token = %d %s, want the unchanged 403", got.status, got.body)
	}
	p.authzPriv = goodAuthz
}

// guardValidations counts $validate calls (an outbound call in production).
type guardValidations struct {
	shnsdk.Validator
	calls *atomic.Int32
}

func (c guardValidations) Validate(ctx context.Context, b []byte, profile string) (shnsdk.Result, error) {
	c.calls.Add(1)
	return c.Validator.Validate(ctx, b, profile)
}

// Every catalog leg reaches the guard before any handler: for each, an unknown
// requester is refused with the named answer, the payer's system is not
// called, nothing is validated and the gateway makes no call of its own,
// whatever the leg's body.
func TestRequesterGuard_EveryLegRefusedBeforeItsHandler(t *testing.T) {
	for leg := range paCatalog {
		t.Run(leg, func(t *testing.T) {
			p, ct := guardPayer(t, EnforcementStrict)
			var validations atomic.Int32
			p.g.cfg.Validator = guardValidations{p.g.cfg.Validator, &validations}
			for line, v := range p.g.cfg.ValidatorsByLine {
				p.g.cfg.ValidatorsByLine[line] = guardValidations{v, &validations}
			}
			p.g.cfg.Reg.Delete(p.requester.ID)
			wantRequesterUnknown(t, p.send(t, leg, "", []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)))
			if n := p.partner.calls.Load(); n != 0 {
				t.Errorf("the payer's system was called %d time(s)", n)
			}
			if n := validations.Load(); n != 0 {
				t.Errorf("%d validation(s) ran", n)
			}
			if n := ct.calls.Load(); n != 0 {
				t.Errorf("the gateway made %d call(s) of its own", n)
			}
		})
	}
}
