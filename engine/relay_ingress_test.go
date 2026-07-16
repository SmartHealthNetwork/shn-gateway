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
	env := newInProcessExchange(t)
	oo := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}]}`
	env.payerReturns(LegResult{Status: 502, ResponseFHIR: []byte(oo)})
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, env.crdIngressRequest(t)) // the real ingress handler (ingress.go:96)
	if rec.Code != 502 {
		t.Fatalf("ingress status = %d, want 502 (the external payer's real status)", rec.Code)
	}
	if b := rec.Body.String(); b == "" || strings.Contains(b, "hub routing failed") {
		t.Fatalf("ingress body must be the external payer's OperationOutcome, got %q", b)
	}
	if !strings.Contains(rec.Body.String(), "OperationOutcome") {
		t.Fatalf("ingress body not the relayed OperationOutcome: %s", rec.Body.String())
	}
}
