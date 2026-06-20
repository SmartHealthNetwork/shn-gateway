package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIngressHandlers_FailClosedWithoutBypass is the rejection row for the ingress auth guard.
// The PRODUCTION posture is IngressEnabled=true with ingressAuthBypass=false (real inbound UDAP
// auth is a planned future enhancement): the routes are mounted but every handler MUST fail
// closed (401) until the test-only bypass is set. A zero-value Gateway has
// ingressAuthBypass=false, so each handler must 401 before touching any other state.
func TestIngressHandlers_FailClosedWithoutBypass(t *testing.T) {
	g := &Gateway{} // zero Config ⇒ ingressAuthBypass == false (prod posture)
	handlers := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"discovery", g.handleCDSDiscovery},
		{"crd", g.handleCRDIngress},
		{"dtr", g.handleDTRIngress},
		{"pas", g.handlePASIngress},
	}
	for _, tc := range handlers {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		w := httptest.NewRecorder()
		tc.h(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s handler without bypass: status = %d, want 401 (fail-closed)", tc.name, w.Code)
		}
	}
}
