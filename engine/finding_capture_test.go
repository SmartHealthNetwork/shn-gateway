package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Every finding is also captured as its own metadata-only event, keyed by the
// leg's correlation id, so the stats can count a call's findings once they no
// longer reach its access line. It speaks for this gateway's holder: a payer's
// as the leg's recipient, any other gateway's as its sender.
func TestFindingIsCapturedAsItsOwnEvent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		payer bool
	}{{"provider", false}, {"payer", true}} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := observeGateway(t, EnforcementObserve, &shnsdk.FakeValidator{RejectIfContains: "Coverage"})
			g.cfg.HolderID = "holder-1"
			if tc.payer {
				g.cfg.Responder = stubResponder{}
			}
			var mu sync.Mutex
			var captured []diagnostics.Event
			g.cfg.Diagnostic = func(e diagnostics.Event) bool { mu.Lock(); captured = append(captured, e); mu.Unlock(); return true }

			g.validateFHIR(ingressCtx("corr-cap"), []byte(coverageJSON), "ingress", "")
			g.drainObserveChecks()

			mu.Lock()
			defer mu.Unlock()
			var found []diagnostics.Event
			for _, e := range captured {
				if e.Kind == diagnostics.KindConformanceFinding {
					found = append(found, e)
				}
			}
			if len(found) != 1 {
				t.Fatalf("want one captured finding, got %d of %d events", len(found), len(captured))
			}
			e := found[0]
			if e.CorrelationID != "corr-cap" || e.LegType != "crd-order-select" || len(e.Body) != 0 {
				t.Fatalf("captured finding = %+v, want the leg's correlation id and type and no body", e)
			}
			if tc.payer && (e.Recipient != "holder-1" || e.Sender != "") || !tc.payer && (e.Sender != "holder-1" || e.Recipient != "") {
				t.Fatalf("a %s finding speaks for its holder: sender %q recipient %q", tc.name, e.Sender, e.Recipient)
			}
			var f ConformanceFinding
			if err := json.Unmarshal([]byte(e.Detail), &f); err != nil || f.CorrelationID != "corr-cap" || f.Decision != Record.String() {
				t.Fatalf("Detail is the finding as logged, got %q (%v)", e.Detail, err)
			}
		})
	}
}

type stubResponder struct{}

func (stubResponder) Handle(context.Context, string, string, string, []byte) (LegResult, error) {
	return LegResult{}, nil
}

// captureSink collects captured events.
type captureSink struct {
	mu     sync.Mutex
	events []diagnostics.Event
}

func (c *captureSink) emit(e diagnostics.Event) bool {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
	return true
}

func (c *captureSink) kind(k string) []diagnostics.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []diagnostics.Event
	for _, e := range c.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func resultDetail(t *testing.T, e diagnostics.Event) (count int, truncated, incomplete bool) {
	t.Helper()
	var d struct {
		Count      int  `json:"count"`
		Truncated  bool `json:"truncated"`
		Incomplete bool `json:"incomplete"`
	}
	if err := json.Unmarshal([]byte(e.Detail), &d); err != nil {
		t.Fatalf("result detail %q: %v", e.Detail, err)
	}
	return d.Count, d.Truncated, d.Incomplete
}

// At observe a payer leg's findings and its one result event carry the leg's
// full binding (ciphertext hash, sender, recipient), like its access line, and
// the result counts every finding the leg's checks recorded.
func TestObserveLegResultCountsItsFindingsAndIsBound(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	sink := &captureSink{}
	p.g.cfg.Diagnostic = sink.emit
	// A slow validator, so the leg ends while its checks are still queued: the
	// result must wait for them rather than count what had arrived by the end.
	// It finds fault with the order, so the queued check itself records a finding.
	p.g.cfg.Validator = validatorFunc(func(b []byte) (shnsdk.Result, error) {
		time.Sleep(150 * time.Millisecond)
		if bytes.Contains(b, []byte(`"ServiceRequest"`)) {
			return shnsdk.Result{Valid: false, Issues: []string{"error: ServiceRequest.status: minimum required = 1, but only found 0"}}, nil
		}
		return shnsdk.Result{Valid: true}, nil
	})
	p.partner.respByPath[crdSelectPath] = []byte(`{"cards":[{"summary":"` + strings.Repeat("s", 200) + `","indicator":"info"}]}`)
	p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	rec := got.only(t)
	p.g.drainObserveChecks()

	findings, results := sink.kind(diagnostics.KindConformanceFinding), sink.kind(diagnostics.KindConformanceResult)
	if len(findings) == 0 || len(results) != 1 {
		t.Fatalf("want findings and exactly one result, got %d findings and %d results", len(findings), len(results))
	}
	if n, truncated, incomplete := resultDetail(t, results[0]); n != len(findings) || truncated || incomplete {
		t.Fatalf("result counts %d (truncated %v, incomplete %v), want %d complete", n, truncated, incomplete, len(findings))
	}
	for _, e := range append(findings, results...) {
		if e.RequestCiphertextHash != rec.RequestCiphertextHash || e.Sender != rec.Sender || e.Recipient != rec.Recipient || e.Recipient != p.g.cfg.HolderID || e.RequestCiphertextHash == "" || e.Sender == "" {
			t.Fatalf("%s bound to (%q, %q, %q), want the leg's (%q, %q, %q)", e.Kind, e.RequestCiphertextHash, e.Sender, e.Recipient, rec.RequestCiphertextHash, rec.Sender, rec.Recipient)
		}
	}
}

// A panicking exchange observer does not cost the leg its result: the result
// is still captured once the leg's queued checks have run.
func TestLegResultSurvivesAPanickingExchangeObserver(t *testing.T) {
	p, _ := newRecordingLevelPayer(t, EnforcementObserve)
	sink := &captureSink{}
	p.g.cfg.Diagnostic = sink.emit
	p.g.cfg.ExchangeObserved = func(ExchangeRecord) { panic("synthetic observer failure") }
	p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	p.g.drainObserveChecks()
	if r := sink.kind(diagnostics.KindConformanceResult); len(r) != 1 {
		t.Fatalf("captured %d results after the observer panicked, want 1", len(r))
	}
}

// At strict and structural the access line counts the findings, so no result
// event is captured.
func TestNoLegResultBelowObserve(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementStructural} {
		p, _ := newRecordingLevelPayer(t, level)
		sink := &captureSink{}
		p.g.cfg.Diagnostic = sink.emit
		p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		if r := sink.kind(diagnostics.KindConformanceResult); len(r) != 0 {
			t.Fatalf("%s captured %d result events", level, len(r))
		}
	}
}

// The result waits for the leg's queued checks: a leg that has ended with a
// check still running has no result until that check finishes.
func TestLegResultWaitsForQueuedChecks(t *testing.T) {
	g, _ := observeGateway(t, EnforcementObserve, nil)
	sink := &captureSink{}
	g.cfg.Diagnostic = sink.emit
	tl := &legTally{}
	tl.hold()
	tl.end(g, ExchangeRecord{CorrelationID: "leg-w"})
	if r := sink.kind(diagnostics.KindConformanceResult); len(r) != 0 {
		t.Fatal("a result was captured while a check was still queued")
	}
	tl.release(g)
	tl.release(g) // a stray second release captures nothing more
	if r := sink.kind(diagnostics.KindConformanceResult); len(r) != 1 || r[0].CorrelationID != "leg-w" {
		t.Fatalf("want one result once the check finished, got %v", r)
	}
}

// Past the per-leg cap a finding is counted but not captured on its own, and
// the result says truncated; a dropped check makes the result incomplete.
func TestLegResultTruncatedAndIncomplete(t *testing.T) {
	g, _ := observeGateway(t, EnforcementObserve, nil)
	sink := &captureSink{}
	g.cfg.Diagnostic = sink.emit
	tl := &legTally{}
	for i := 0; i < findingEventsPerLeg+3; i++ {
		g.emitFinding(ConformanceFinding{Kind: "content", CorrelationID: "leg-c", tally: tl})
	}
	tl.dropped()
	tl.end(g, ExchangeRecord{CorrelationID: "leg-c"})
	if f := sink.kind(diagnostics.KindConformanceFinding); len(f) != findingEventsPerLeg {
		t.Fatalf("captured %d finding events, want the cap %d", len(f), findingEventsPerLeg)
	}
	r := sink.kind(diagnostics.KindConformanceResult)
	if len(r) != 1 {
		t.Fatalf("want one result, got %d", len(r))
	}
	if n, truncated, incomplete := resultDetail(t, r[0]); n != findingEventsPerLeg+3 || !truncated || !incomplete {
		t.Fatalf("result = %d truncated %v incomplete %v, want %d true true", n, truncated, incomplete, findingEventsPerLeg+3)
	}
}

// A leg refused before it was verified has no leg id: its result would join
// nothing, so none is captured.
func TestNoLegResultForAnUnverifiedLeg(t *testing.T) {
	g, _ := observeGateway(t, EnforcementObserve, nil)
	sink := &captureSink{}
	g.cfg.Diagnostic = sink.emit
	(&legTally{}).end(g, ExchangeRecord{})
	if r := sink.kind(diagnostics.KindConformanceResult); len(r) != 0 {
		t.Fatalf("captured %d results for a leg with no id", len(r))
	}
}

// At observe a refusal is still judged before the answer, so the record keeps
// Refused while it defers the count.
func TestObserveRecordKeepsRefused(t *testing.T) {
	g, _ := observeGateway(t, EnforcementObserve, nil)
	var got []ExchangeRecord
	g.cfg.ExchangeObserved = func(r ExchangeRecord) { got = append(got, r) }
	h := g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
		g.emitFindingIn(r.Context(), ConformanceFinding{Kind: string(KindFHIRBridged), Decision: Refuse.String()})
		w.WriteHeader(http.StatusInternalServerError)
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil))
	if len(got) != 1 || !got[0].Findings.Deferred || !got[0].Findings.Refused || got[0].Findings.Count != 0 {
		t.Fatalf("records = %+v, want one deferred record that keeps refused", got)
	}
}
