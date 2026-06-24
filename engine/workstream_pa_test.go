package engine

import (
	"context"
	"testing"
)

func TestPACatalog_Ops(t *testing.T) {
	want := map[string]string{
		"coverage-eligibility":    "eligibility-inquiry",
		"crd-order-select":        "crd-order-select",
		"dtr-questionnaire-fetch": "dtr-questionnaire-fetch",
		"federated-query":         "federated-query-submit",
		"patient-dtr":             "patient-dtr-request",
		"pas-claim":               "pas-submit",
		"pas-claim-update":        "pas-update-submit",
	}
	if len(paCatalog) != len(want) {
		t.Fatalf("paCatalog has %d entries, want %d", len(paCatalog), len(want))
	}
	for legType, op := range want {
		spec, ok := paCatalog[legType]
		if !ok {
			t.Errorf("paCatalog missing %q", legType)
			continue
		}
		if spec.Op != op {
			t.Errorf("paCatalog[%q].Op = %q, want %q", legType, spec.Op, op)
		}
	}
}

// TestPACatalog_AllReqFramesProviderTPO documents the PA invariant that every
// originated leg is framed provider-tpo (handleInbound pins this frame). This is what
// lets handleInbound read spec.ReqFrame from the catalog in place of its former inline
// "provider-tpo" literal (the inbound FulfillLeg dispatch now does exactly that).
func TestPACatalog_AllReqFramesProviderTPO(t *testing.T) {
	for legType, spec := range paCatalog {
		if spec.ReqFrame != "provider-tpo" {
			t.Errorf("paCatalog[%q].ReqFrame = %q, want provider-tpo", legType, spec.ReqFrame)
		}
	}
}

func TestOriginateLeg_UnknownLegTypeFailsClosed(t *testing.T) {
	g := &Gateway{} // never reaches roundTrip: the catalog miss returns first
	// workstreamPA passes the selection-seam guard, so this exercises the legType-miss
	// path specifically (an unknown legType WITHIN the served workstream).
	_, err := g.OriginateLeg(context.Background(), nil, "payer", "no-such-leg", "pci", "corr", "", Content{WorkstreamType: workstreamPA, Bytes: []byte("{}")})
	if err == nil {
		t.Fatal("OriginateLeg with unknown legType: want error, got nil")
	}
}

// TestOriginateLeg_WrongWorkstreamFailsClosed proves the WorkstreamType selection-seam
// guard fires BEFORE the catalog lookup: a VALID legType ("crd-order-select") carried
// by a FOREIGN WorkstreamType must fail closed without ever reaching paCatalog or roundTrip
// (a future catalogFor() would have no catalog for that workstream). The error returns
// cleanly on a zero-value Gateway — if the guard did NOT fire first, the valid legType
// would reach roundTrip and panic on the nil registry, failing the test differently.
func TestOriginateLeg_WrongWorkstreamFailsClosed(t *testing.T) {
	g := &Gateway{}
	_, err := g.OriginateLeg(context.Background(), nil, "payer", "crd-order-select", "pci", "corr", "", Content{WorkstreamType: "x12-278", Bytes: []byte("{}")})
	if err == nil {
		t.Fatal("OriginateLeg with foreign WorkstreamType: want error, got nil")
	}
}

// TestExchangeIR_Composes is a COMPOSE/shape check: it confirms the IR vocabulary
// nests (Exchange holds LegRecords projected from in-flight Legs) and compiles. The
// len()==2 + field readbacks are tautological by design — this slice does not yet
// thread a live Exchange, so there is no behavior to assert. The LOAD-BEARING
// assertions are the physics cross-checks below: each leg's Physics, pulled from
// paCatalog, must carry the right Effect (CRD readonly, PAS mutating). Do not read the
// rest as behavioral coverage.
func TestExchangeIR_Composes(t *testing.T) {
	legs := []Leg{
		{Type: "crd-order-select", Physics: paCatalog["crd-order-select"].Physics,
			Content: Content{WorkstreamType: workstreamPA, Bytes: []byte("{}")}, Subjects: []string{"pci-1"}},
		{Type: "pas-claim", Physics: paCatalog["pas-claim"].Physics,
			Content: Content{WorkstreamType: workstreamPA, Bytes: []byte("{}")}, Subjects: []string{"pci-1"}},
	}
	ex := Exchange{ID: "exch-1", Workstream: workstreamPA}
	for _, l := range legs {
		ex.Legs = append(ex.Legs, l.Project("corr-"+l.Type, "ok"))
	}
	if len(ex.Legs) != 2 {
		t.Fatalf("want 2 legs, got %d", len(ex.Legs))
	}
	if legs[0].Physics.Effect != EffectReadOnly {
		t.Errorf("crd-order-select Effect = %q, want readonly", legs[0].Physics.Effect)
	}
	if legs[1].Physics.Effect != EffectMutating {
		t.Errorf("pas-claim Effect = %q, want mutating", legs[1].Physics.Effect)
	}
	if ex.Legs[0].Physics.Effect != EffectReadOnly || ex.Legs[1].Physics.Effect != EffectMutating {
		t.Errorf("projected LegRecord physics not carried: %+v", ex.Legs)
	}
}
