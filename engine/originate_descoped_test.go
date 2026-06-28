package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// UC-02 (no-PA) is descoped for the provider-data lane (D-PD-2). Unlike the orderSource-routed
// descoped scenarios (which fail closed at OpenOrder when the member has no seeded order),
// handleUC02 builds its order INLINE — so under provider-data it MUST explicitly fail closed
// rather than originate a hardcoded literal order (the provider-data honesty principle: every
// origination traces to the provider's seeded SoR, never a literal). composite/sandbox keep the
// no-PA origination (covered by the composite all-eight gate), so the guard is provider-data-only.
func TestHandleUC02_ProviderDataDescopedFailsClosed(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", SoR: NewStubHolderData()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc02", strings.NewReader("{}"))
	g.handleUC02(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("UC-02 provider-data: status=%d, want 501 — descoped (D-PD-2), must NOT originate a literal order", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "D-PD-2") {
		t.Fatalf("UC-02 provider-data fail-closed body=%q, want an explicit D-PD-2 descoped message", rec.Body.String())
	}
}
