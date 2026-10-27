//go:build !shn_coreonly

package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestNew_EachWorkstreamGetsItsGatewayState: New builds every compiled-in
// workstream's own state on the gateway, and the PA workstream's holds the
// enrichment hook exactly when the participant has opted in.
func TestNew_EachWorkstreamGetsItsGatewayState(t *testing.T) {
	env := newInProcessExchange(t)
	for _, on := range []bool{false, true} {
		cfg := env.originator.cfg
		cfg.EnrichNativeRequests = on
		g, err := New(cfg)
		if err != nil {
			t.Fatalf("opt-in %v: New: %v", on, err)
		}
		for _, w := range workstreams {
			want, err := w.newGatewayState(g)
			if err != nil {
				t.Fatalf("opt-in %v: workstream %q: %v", on, w.id(), err)
			}
			got, ok := g.states[w.id()]
			if (want == nil) != !ok {
				t.Fatalf("opt-in %v: workstream %q: state held %v, built %T", on, w.id(), ok, want)
			}
			if ok && fmt.Sprintf("%T", got) != fmt.Sprintf("%T", want) {
				t.Errorf("opt-in %v: workstream %q holds %T, builds %T", on, w.id(), got, want)
			}
		}
		if st := g.paState(); st == nil || (st.enrich != nil) != on {
			t.Fatalf("opt-in %v: PA state %+v", on, st)
		}
	}
}

// TestGateway_WithoutThePAStateEnrichesNothing is the rejection row for the
// one above: a gateway that holds no PA state, opted in or not, answers no
// enrichment hook, so no enrichment code runs and nothing panics.
func TestGateway_WithoutThePAStateEnrichesNothing(t *testing.T) {
	env := newInProcessExchange(t)
	g := env.originator
	optInEnrichment(g, true)
	if g.enrichment() == nil {
		t.Fatal("control: an opted-in gateway holds no hook; the row below would be vacuous")
	}
	delete(g.states, workstreamPA)
	if h := g.enrichment(); h != nil {
		t.Fatalf("a gateway without the PA state answers the hook %T", h)
	}
	if h := (&Gateway{cfg: Config{EnrichNativeRequests: true}}).enrichment(); h != nil {
		t.Fatalf("a gateway assembled without New answers the hook %T", h)
	}
}

// TestNew_AnOptInWithNoEnrichmentCompiledInIsRefused: when no enrichment is
// compiled in (on-behalf building's init sets none), New refuses a
// participant's opt-in rather than carry its requests as if it had not opted
// in, and a gateway that has not opted in still boots. It clears the
// package's one builder, so it saves and restores it and does not run in
// parallel: a parallel test's New would see the cleared builder.
func TestNew_AnOptInWithNoEnrichmentCompiledInIsRefused(t *testing.T) {
	saved := nativeEnrichmentFor
	if saved == nil {
		t.Fatal("control: this build has no enrichment compiled in; the rows below would prove nothing")
	}
	t.Cleanup(func() { nativeEnrichmentFor = saved })
	env := newInProcessExchange(t)
	cfg := env.originator.cfg
	cfg.EnrichNativeRequests = true
	mustNew(t, cfg) // control: an opted-in gateway with enrichment compiled in boots
	nativeEnrichmentFor = nil
	if g, err := New(cfg); err == nil || !strings.Contains(err.Error(), "EnrichNativeRequests") {
		t.Fatalf("an opt-in with no enrichment compiled in: New = %v, %v; want a refusal naming the opt-in", g, err)
	}
	cfg.EnrichNativeRequests = false
	g := mustNew(t, cfg) // a gateway that has not opted in boots with no enrichment compiled in
	if h := g.enrichment(); h != nil {
		t.Fatalf("a gateway that has not opted in holds the hook %T", h)
	}
}
