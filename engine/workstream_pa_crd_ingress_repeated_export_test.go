//go:build !shn_coreonly

package engine

import (
	"net/http/httptest"
	"testing"
)

// CRDIngressWithSoRForTest sends a CRD order-select request for the member
// RepeatedMemberE2EMember names through a provider gateway's ingress route,
// with sor as that gateway's system of record and a payer that answers, and
// returns the answer to the participant's own caller and the exchange record.
// For the end-to-end rows in package engine_test
// (workstream_pa_crd_ingress_repeated_e2e_test.go).
func CRDIngressWithSoRForTest(t *testing.T, sor SystemOfRecord) (int, string, ExchangeRecord) {
	t.Helper()
	env := newInProcessExchange(t)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	env.originator.cfg.SoR = sor
	env.payerReturns(LegResult{})
	w := httptest.NewRecorder()
	env.originator.ingressRoute(RouteCRD)(w, env.crdIngressRequest(t))
	return w.Code, w.Body.String(), got.only(t)
}
