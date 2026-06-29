package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCompleteClinician_ProviderData_RejectsOrderWithoutID proves the provider-data UC-06 amendment
// fails CLOSED when the parked order has no resolvable id — the amendment must bind to the REAL seeded
// order ref (resourceRef), never a composite literal. Hermetic: resourceRef is checked BEFORE any leg
// or validator call, so no network/validator is needed.
func TestCompleteClinician_ProviderData_RejectsOrderWithoutID(t *testing.T) {
	g := &Gateway{cfg: Config{
		OriginationProfile: "provider-data",
		Clock:              func() time.Time { return time.Unix(0, 0).UTC() },
	}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/scenario/uc06/complete", nil)
	st := pendState{scenario: "uc06", srJSON: []byte(`{"resourceType":"ServiceRequest","status":"active"}`)} // no id

	if ok := g.completeClinician(w, r, st, "", ""); ok {
		t.Fatalf("completeClinician returned ok=true for an order without an id; want fail-closed")
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 (order missing id)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "order missing id") {
		t.Fatalf("body=%q, want to contain 'order missing id'", w.Body.String())
	}
}
