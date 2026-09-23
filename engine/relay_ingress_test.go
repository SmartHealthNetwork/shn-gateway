// gateway/engine/relay_ingress_test.go
package engine

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The Da Vinci ingress must surface a recipient 502 + OperationOutcome verbatim,
// not "hub routing failed". Drives the CRD ingress through the in-process exchange
// (flag on) with the payer returning 502.
func TestCRDIngress_RecipientNon2xx_SurfacesVerbatim(t *testing.T) {
	env := newTransportExchange(t)
	oo := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}]}`
	env.payerReturns(LegResult{Status: 502, Response: testResponse([]byte(oo))})
	rec := httptest.NewRecorder()
	pci, _, _ := env.originator.cfg.SoR.ResolvePatient("MBR-COVERED")
	req := signedFixtureIngress(t, env.originator, "/cds-services/shn-order-select", "crd-order-select", "crd-order-select", "order-select", pci, "pa.crd@2.0", "error-relay", conformantCRDRequest("MBR-COVERED"))
	env.originator.Handler().ServeHTTP(rec, req)
	if rec.Code != 502 {
		t.Fatalf("ingress status = %d, want 502 (the payer's real status)", rec.Code)
	}
	if b := rec.Body.String(); b == "" || strings.Contains(b, "hub routing failed") {
		t.Fatalf("ingress body must be the payer's OperationOutcome, got %q", b)
	}
	if rec.Body.String() != oo || env.routeHitCount() != 1 {
		t.Fatalf("ingress body not the relayed OperationOutcome: %s", rec.Body.String())
	}
}
