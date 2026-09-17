package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// noOrderSoR resolves the provider-data UC-02 member as a Patient (the member exists in the
// provider tenant) but has NO open order in the SoR — the embedded censusSoR's OpenOrder
// returns (nil,false). It mirrors the real provider-data lane: MBR-PD-UC02 lives in the FHIR-store
// seed (internal/fhirseed), not the engine's in-memory censusPersonas map, so ResolvePatient is
// overridden here exactly as the HomeOxygen fake does for MBR-OX.
type noOrderSoR struct{ *censusSoR }

func (s *noOrderSoR) ResolvePatient(memberID string) (string, Demo, bool) {
	if memberID != "MBR-PD-UC02" {
		return s.censusSoR.ResolvePatient(memberID)
	}
	return "pci-uc02", Demo{BirthDate: "1953-09-17", FamilyName: "Bergstrom-HospitalBed"}, true
}

func (s *noOrderSoR) PatientFHIRRef(memberID string) (string, bool) {
	if memberID != "MBR-PD-UC02" {
		return s.censusSoR.PatientFHIRRef(memberID)
	}
	return "Patient/MBR-PD-UC02", true
}

// SearchPatientContext answers every search with no match: the member's
// system of record holds no order.
func (s *noOrderSoR) SearchPatientContext(context.Context, string, string, ...SearchDateRange) (SearchResult, error) {
	return SearchResult{Pages: [][]byte{[]byte(`{"resourceType":"Bundle","type":"searchset","entry":[]}`)}}, nil
}

// UC-02 (no-PA) ORIGINATES the seeded E0250 hospital-bed DeviceRequest off provider data
// (the MBR-PD-UC02 persona) — it is no longer descoped (D-PD-2 dropped). The coverage check is
// about an order still being chosen, so the provider-data lane searches the SoR for the member's
// DRAFT order; a mis-seeded member with no draft order must fail closed — never originate a
// literal order. That is the provider-data honesty boundary: every origination
// traces to the provider's seeded SoR. Every other lane keeps the no-PA origination off the
// per-UC tuple, so this guard is provider-data-only.
func TestHandleUC02_ProviderData_NoSeededOrder_FailsClosed(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", SoR: &noOrderSoR{newCensusSoR()}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc02", strings.NewReader("{}"))
	g.handleUC02(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("UC-02 provider-data, no seeded order: status=%d (%s), want 502 — must fail closed, never originate a literal order", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no draft order for member in system of record") {
		t.Fatalf("UC-02 provider-data no-draft-order body=%q, want the no-draft-order fail-closed reason", rec.Body.String())
	}
}
