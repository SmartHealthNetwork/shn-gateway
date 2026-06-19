package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// adjTestClock is the fixed clock used by sandbox-adjudicator tests.
var adjTestClock = time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

// adjTestCPT is the CPT code in the UC-03/approved fixture.
const adjTestCPT = "72148"

// newSandboxResponderForTest builds a sandboxResponder backed by a fresh
// StubHolderData (satisfies both SystemOfRecord and Store) and a fixed clock.
func newSandboxResponderForTest(t *testing.T) LegResponder {
	t.Helper()
	data := NewStubHolderData()
	clock := func() time.Time { return adjTestClock }
	adj := NewSandboxAdjudicator(data, clock)
	return NewSandboxResponder(adj, data, data, clock)
}

// loadTestClaimApproved builds a PAS submit Bundle for the UC-03 approved path
// (CPT 72148, 6 weeks conservative therapy → PASApproved). The QR is filled
// with SandboxUC03Context so SandboxAdjudicate returns PASApproved.
func loadTestClaimApproved(t *testing.T) []byte {
	t.Helper()
	const (
		patientRef  = "Patient/MBR-COVERED"
		coverageRef = "Coverage/MBR-COVERED"
		corrID      = "test-corr-approved"
	)
	srJSON, err := shnsdk.BuildServiceRequest(adjTestCPT, "MRI lumbar spine w/o contrast", "M51.16", patientRef)
	if err != nil {
		t.Fatalf("loadTestClaimApproved BuildServiceRequest: %v", err)
	}
	q := shnsdk.SandboxLumbarQuestionnaire()
	qrJSON, err := shnsdk.FillQuestionnaire(q, shnsdk.SandboxUC03Context(), shnsdk.QRContext{
		PatientRef:  patientRef,
		CoverageRef: coverageRef,
		OrderRef:    "ServiceRequest/sr-mbr-covered",
		Authored:    adjTestClock,
	})
	if err != nil {
		t.Fatalf("loadTestClaimApproved FillQuestionnaire: %v", err)
	}
	bundle, err := shnsdk.BuildClaimBundle(qrJSON, srJSON, patientRef, coverageRef, corrID, adjTestClock)
	if err != nil {
		t.Fatalf("loadTestClaimApproved BuildClaimBundle: %v", err)
	}
	return bundle
}

// loadTestClaimCPTlessSR builds a PAS submit Bundle where the ServiceRequest
// entry is PRESENT (so ParseClaimBundle succeeds with ≥3 entries) but its
// code.coding is stripped (so ParseServiceRequestCPT errors → 400).
func loadTestClaimCPTlessSR(t *testing.T) []byte {
	t.Helper()
	base := loadTestClaimApproved(t)

	// Walk the bundle entries and strip code.coding from the ServiceRequest.
	var raw struct {
		ResourceType string            `json:"resourceType"`
		Entry        []json.RawMessage `json:"entry"`
		Type         json.RawMessage   `json:"type,omitempty"`
		Timestamp    json.RawMessage   `json:"timestamp,omitempty"`
	}
	if err := json.Unmarshal(base, &raw); err != nil {
		t.Fatalf("loadTestClaimCPTlessSR unmarshal bundle: %v", err)
	}

	for i, entryRaw := range raw.Entry {
		var entry struct {
			Resource json.RawMessage `json:"resource"`
		}
		if err := json.Unmarshal(entryRaw, &entry); err != nil {
			t.Fatalf("unmarshal entry[%d]: %v", i, err)
		}
		var rt struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(entry.Resource, &rt); err != nil {
			t.Fatalf("unmarshal entry[%d] resourceType: %v", i, err)
		}
		if rt.ResourceType != "ServiceRequest" {
			continue
		}
		// Unmarshal as a generic map and delete "code" (removes code.coding entirely).
		var srMap map[string]json.RawMessage
		if err := json.Unmarshal(entry.Resource, &srMap); err != nil {
			t.Fatalf("unmarshal SR as map: %v", err)
		}
		delete(srMap, "code")
		patchedSR, err := json.Marshal(srMap)
		if err != nil {
			t.Fatalf("re-marshal patched SR: %v", err)
		}
		var entryMap map[string]json.RawMessage
		if err := json.Unmarshal(entryRaw, &entryMap); err != nil {
			t.Fatalf("unmarshal entry[%d] as map: %v", i, err)
		}
		entryMap["resource"] = json.RawMessage(patchedSR)
		patched, err := json.Marshal(entryMap)
		if err != nil {
			t.Fatalf("re-marshal entry[%d]: %v", i, err)
		}
		raw.Entry[i] = json.RawMessage(patched)
		break
	}

	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("loadTestClaimCPTlessSR re-marshal bundle: %v", err)
	}
	return out
}

// TestSandboxPAS_EOBCPTSourcedFromClaim proves that the approved-path EOB's CPT
// is sourced from the Claim's ServiceRequest, not a hardcoded constant (design §1.4
// — lineage gap #1 closed in the sandbox). The UC fixture's CPT is "72148", so
// the EOB is byte-identical to the prior constant; this test proves it TRACKS the Claim.
func TestSandboxPAS_EOBCPTSourcedFromClaim(t *testing.T) {
	r := newSandboxResponderForTest(t)
	claim := loadTestClaimApproved(t)
	res, err := r.Handle(context.Background(), "pas-claim", "corr-1", "pci:test-1", claim)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(res.SideEffectFHIR) != 1 {
		t.Fatalf("want 1 EOB side-effect, got %d", len(res.SideEffectFHIR))
	}
	if !bytes.Contains(res.SideEffectFHIR[0], []byte(adjTestCPT)) {
		t.Fatalf("EOB did not carry the Claim's CPT %s:\n%s", adjTestCPT, res.SideEffectFHIR[0])
	}
}

// TestSandboxPAS_CPTlessServiceRequestIs400 proves that a Claim whose ServiceRequest
// is present but has NO parseable CPT is a malformed CLIENT request → 400
// (design §1.4), not a 500. The SR entry is present (ParseClaimBundle succeeds
// with ≥3 entries) but its code.coding is stripped.
func TestSandboxPAS_CPTlessServiceRequestIs400(t *testing.T) {
	r := newSandboxResponderForTest(t)
	claim := loadTestClaimCPTlessSR(t)
	res, err := r.Handle(context.Background(), "pas-claim", "corr-1", "pci:test-1", claim)
	if err != nil {
		t.Fatalf("unexpected error return (must be Status 400, not 500): %v", err)
	}
	if res.Status != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", res.Status, res.Message)
	}
}
