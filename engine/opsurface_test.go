package engine

import "testing"

// TestPALegOperations_MatchesCatalog pins the exported operation-binding
// accessor fresh against the live catalog: every catalog row is projected
// exactly (frames + ops), no row is dropped, none invented. The cross-domain
// lockstep conformance check reads the other trust domains' copies through
// accessors like this one, so a stale projection here would let real drift
// hide behind a green cross-check.
func TestPALegOperations_MatchesCatalog(t *testing.T) {
	ops := PALegOperations()
	if len(ops) != len(paCatalog) {
		t.Fatalf("PALegOperations returned %d entries, catalog has %d", len(ops), len(paCatalog))
	}
	for legType, spec := range paCatalog {
		b, ok := ops[legType]
		if !ok {
			t.Errorf("PALegOperations missing catalog leg %q", legType)
			continue
		}
		if b.RequestFrame != spec.ReqFrame || b.RequestOp != spec.Op ||
			b.ResponseFrame != spec.RespFrame || b.ResponseOp != spec.RespOp {
			t.Errorf("PALegOperations[%q] = %+v, want {%s %s %s %s}",
				legType, b, spec.ReqFrame, spec.Op, spec.RespFrame, spec.RespOp)
		}
	}
}

// TestPALegOperations_ReturnsFreshCopy rejects the aliasing failure mode: a
// caller mutating the returned map must never reach the catalog projection a
// later caller sees.
func TestPALegOperations_ReturnsFreshCopy(t *testing.T) {
	first := PALegOperations()
	first["pas-claim"] = PALegOperation{RequestOp: "tampered"}
	delete(first, "crd-order-select")
	fresh := PALegOperations()
	if fresh["pas-claim"].RequestOp != paCatalog["pas-claim"].Op {
		t.Errorf("mutation through a returned map reached a later call: %+v", fresh["pas-claim"])
	}
	if _, ok := fresh["crd-order-select"]; !ok {
		t.Error("deletion through a returned map reached a later call")
	}
}
