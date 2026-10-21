package engine

import (
	"fmt"
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
			want := w.newGatewayState(g)
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
