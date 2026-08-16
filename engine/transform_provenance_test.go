package engine

import (
	"encoding/json"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestLossReportSDKSchemaParity proves the layering resolution: this
// package's own LossReport/LossEntry (engine-internal, no sdk twin — see
// transform.go's file doc comment) marshal to EXACTLY the same JSON as the
// canonical shnsdk.LossReport/LossEntry schema the shn-loss-report extension
// carries (sdk/provenance.go). One canonical encoding, byte-parity-tested
// here because this package (unlike sdk) already imports shnsdk.
func TestLossReportSDKSchemaParity(t *testing.T) {
	cases := []struct {
		name string
		eng  LossReport
		sdk  shnsdk.LossReport
	}{
		{
			name: "full report with carried and synthesized entries",
			eng: LossReport{
				Module: "pa.pas 2.1->2.2",
				Source: "2.1",
				Target: "2.2",
				Carried: []LossEntry{
					{Path: "Claim.item[0].extension", Detail: "authorizationNumber carried; source line 2.2"},
				},
				Synthesized: []LossEntry{
					{Path: "Claim.identifier", Detail: "synthesized from correlation id"},
				},
			},
			sdk: shnsdk.LossReport{
				Module: "pa.pas 2.1->2.2",
				Source: "2.1",
				Target: "2.2",
				Carried: []shnsdk.LossEntry{
					{Path: "Claim.item[0].extension", Detail: "authorizationNumber carried; source line 2.2"},
				},
				Synthesized: []shnsdk.LossEntry{
					{Path: "Claim.identifier", Detail: "synthesized from correlation id"},
				},
			},
		},
		{
			name: "zero-value report (identity step, no loss)",
			eng:  LossReport{Module: "pa.dtr 2.1->2.2", Source: "2.1", Target: "2.2"},
			sdk:  shnsdk.LossReport{Module: "pa.dtr 2.1->2.2", Source: "2.1", Target: "2.2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engJSON, err := json.Marshal(tc.eng)
			if err != nil {
				t.Fatalf("marshal engine.LossReport: %v", err)
			}
			sdkJSON, err := json.Marshal(tc.sdk)
			if err != nil {
				t.Fatalf("marshal shnsdk.LossReport: %v", err)
			}
			if string(engJSON) != string(sdkJSON) {
				t.Fatalf("schema drift:\n engine: %s\n sdk:    %s", engJSON, sdkJSON)
			}
		})
	}
}

// TestTransformedObserverEvent pins the observer emit seam:
// transformedObserverEvent builds Kind=leg.transformed with Detail = the
// chain's module summary, comma-joined in step order. Construction only —
// the call site (g.egressAdapt) fills in the remaining
// ObserverEvent fields (LegType/CorrelationID/Counterpart) before emitting
// through Gateway.observe.
func TestTransformedObserverEvent(t *testing.T) {
	reports := []LossReport{
		{Module: "pa.pas 2.0->2.1", Source: "2.0", Target: "2.1"},
		{Module: "pa.pas 2.1->2.2", Source: "2.1", Target: "2.2"},
	}
	e := transformedObserverEvent(reports)
	if e.Kind != "leg.transformed" {
		t.Fatalf("Kind = %q, want leg.transformed", e.Kind)
	}
	if e.Kind != legTransformedKind {
		t.Fatalf("Kind = %q != legTransformedKind constant %q", e.Kind, legTransformedKind)
	}
	want := "pa.pas 2.0->2.1, pa.pas 2.1->2.2"
	if e.Detail != want {
		t.Fatalf("Detail = %q, want %q", e.Detail, want)
	}
}

// TestTransformedObserverEventEmptyChain: an empty/nil report slice (e.g. a
// zero-step chain — should not occur in practice, but the helper must not
// panic) produces an empty Detail, not a malformed join.
func TestTransformedObserverEventEmptyChain(t *testing.T) {
	e := transformedObserverEvent(nil)
	if e.Kind != legTransformedKind {
		t.Fatalf("Kind = %q, want %q", e.Kind, legTransformedKind)
	}
	if e.Detail != "" {
		t.Fatalf("Detail = %q, want empty", e.Detail)
	}
}
