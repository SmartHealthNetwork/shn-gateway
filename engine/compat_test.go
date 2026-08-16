package engine

import (
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestChainForAdjacentAndComposed pins chainFor's pure path-walk: one step for
// an adjacent pair, N-1 ordered steps for a composed span, and nil for holes
// (unknown contract or unknown line — the manifest has nothing to walk).
func TestChainForAdjacentAndComposed(t *testing.T) {
	one := chainFor("pa.pas", "2.0", "2.1")
	if len(one) != 1 {
		t.Fatalf("pa.pas 2.0->2.1: want 1 step, got %d: %+v", len(one), one)
	}
	if one[0].Contract != "pa.pas" || one[0].From != "2.0" || one[0].To != "2.1" {
		t.Fatalf("pa.pas 2.0->2.1: unexpected step: %+v", one[0])
	}

	composed := chainFor("pa.pas", "2.0", "2.2")
	if len(composed) != 2 {
		t.Fatalf("pa.pas 2.0->2.2: want 2 steps, got %d: %+v", len(composed), composed)
	}
	if composed[0].From != "2.0" || composed[0].To != "2.1" {
		t.Fatalf("pa.pas 2.0->2.2 step 0: want 2.0->2.1, got %s->%s", composed[0].From, composed[0].To)
	}
	if composed[1].From != "2.1" || composed[1].To != "2.2" {
		t.Fatalf("pa.pas 2.0->2.2 step 1: want 2.1->2.2, got %s->%s", composed[1].From, composed[1].To)
	}

	// Reverse (high->low) walks Down-direction steps in reverse order.
	down := chainFor("pa.pas", "2.2", "2.0")
	if len(down) != 2 {
		t.Fatalf("pa.pas 2.2->2.0: want 2 steps, got %d: %+v", len(down), down)
	}
	if down[0].From != "2.1" || down[0].To != "2.2" {
		t.Fatalf("pa.pas 2.2->2.0 step 0: want the 2.1->2.2 row first, got %s->%s", down[0].From, down[0].To)
	}
	if down[1].From != "2.0" || down[1].To != "2.1" {
		t.Fatalf("pa.pas 2.2->2.0 step 1: want the 2.0->2.1 row last, got %s->%s", down[1].From, down[1].To)
	}

	if got := chainFor("no.such.contract", "2.0", "2.1"); got != nil {
		t.Fatalf("unknown contract: want nil, got %+v", got)
	}
	if got := chainFor("pa.pas", "2.0", "9.9"); got != nil {
		t.Fatalf("unknown line: want nil, got %+v", got)
	}
	if got := chainFor("pa.pas", "9.9", "2.0"); got != nil {
		t.Fatalf("unknown source line: want nil, got %+v", got)
	}
}

// TestCompatManifestCoversAdjacentNativeSteps is the coverage pin: every
// adjacent pair of NativeContractVersions() lines, per contract, has a
// manifest row. Single-line contracts (pa.pdex@2.1) have ZERO adjacent pairs
// and are tolerated rowless — pinned explicitly, not just skipped. Rows whose
// Up/Down are both nil must be Class == "full" (identity) — unconditionally,
// every row, since the temporary CompatStep.wired exemption was removed
// (every PAS/DTR row carries its real Up/Down; CRD's identity
// rows are the genuine content, not a stub).
func TestCompatManifestCoversAdjacentNativeSteps(t *testing.T) {
	contracts := nativeContracts()
	if len(contracts) == 0 {
		t.Fatal("nativeContracts() returned nothing — NativeContractVersions() empty?")
	}

	sawSingleLineContract := false
	for _, c := range contracts {
		lines := nativeLinesForContract(c)
		if len(lines) < 2 {
			// Rowless-tolerated: pin it explicitly rather than silently skipping.
			sawSingleLineContract = true
			for _, s := range compatManifest {
				if s.Contract == c {
					t.Errorf("single-line contract %q (lines=%v) is tolerated ROWLESS, but compatManifest has a row: %+v", c, lines, s)
				}
			}
			continue
		}
		for i := 0; i+1 < len(lines); i++ {
			from, to := lines[i], lines[i+1]
			if _, ok := manifestStep(c, from, to); !ok {
				t.Errorf("compatManifest missing row for adjacent native pair %s %s->%s", c, from, to)
			}
		}
	}
	if !sawSingleLineContract {
		t.Fatal("expected at least one single-line native contract (pa.pdex@2.1) to exercise the rowless-tolerance pin — did NativeContractVersions() change shape?")
	}

	// pa.pdex specifically, by name, per the brief's explicit callout.
	pdexLines := nativeLinesForContract("pa.pdex")
	if len(pdexLines) != 1 || pdexLines[0] != "2.1" {
		t.Fatalf("pa.pdex: want exactly one native line (2.1), got %v", pdexLines)
	}

	for _, s := range compatManifest {
		if s.Up == nil || s.Down == nil {
			if s.Class != StepFull {
				t.Errorf("wired row %s %s->%s has a nil Up/Down but Class=%q (want %q — nil funcs are identity, permitted only for full)",
					s.Contract, s.From, s.To, s.Class, StepFull)
			}
		}
	}
}

// TestNativeContractVersionsSanity guards the premise the two tests above
// build on: this package's understanding of shnsdk.NativeContractVersions()
// tokens (contract@line grammar) still parses the way nativeContracts /
// nativeLinesForContract assume.
func TestNativeContractVersionsSanity(t *testing.T) {
	toks := shnsdk.NativeContractVersions()
	if len(toks) == 0 {
		t.Fatal("shnsdk.NativeContractVersions() returned nothing")
	}
	for _, tok := range toks {
		if shnsdk.LineOf(tok) == "" {
			t.Fatalf("token %q has no line component (contract@line grammar violated)", tok)
		}
	}
}
