package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// noOrderSoR resolves the provider-data UC-02 member as a Patient (the member exists in the
// provider tenant) but has NO open order in the SoR — the embedded StubHolderData's OpenOrder
// returns (nil,false). It mirrors the real provider-data lane: MBR-PD-UC02 lives in the FHIR-store
// seed (internal/fhirseed), not the engine's in-memory stubPersonas map, so ResolvePatient is
// overridden here exactly as the HomeOxygen fake does for MBR-OX.
type noOrderSoR struct{ *StubHolderData }

func (s *noOrderSoR) ResolvePatient(memberID string) (string, Demo, bool) {
	if memberID != "MBR-PD-UC02" {
		return s.StubHolderData.ResolvePatient(memberID)
	}
	return "pci-uc02", Demo{BirthDate: "1953-09-17", FamilyName: "Bergstrom-HospitalBed"}, true
}

// UC-02 (no-PA) ORIGINATES the seeded E0250 hospital-bed DeviceRequest off provider data
// (the MBR-PD-UC02 persona) — it is no longer descoped (D-PD-2 dropped). Like the other
// orderSource-routed scenarios, the provider-data lane reads the order via OpenOrder(member),
// so a mis-seeded member with NO open order in the SoR must fail closed at OpenOrder — never
// originate a literal order. That is the provider-data honesty boundary: every origination
// traces to the provider's seeded SoR. The sandbox lane keeps the no-PA origination off the
// per-UC tuple, so this guard is provider-data-only.
func TestHandleUC02_ProviderData_NoSeededOrder_FailsClosed(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", SoR: &noOrderSoR{NewStubHolderData()}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc02", strings.NewReader("{}"))
	g.handleUC02(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("UC-02 provider-data, no seeded order: status=%d, want 502 — must fail closed at OpenOrder, never originate a literal order", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no open order") {
		t.Fatalf("UC-02 provider-data no-open-order body=%q, want the no-open-order fail-closed reason", rec.Body.String())
	}
}
