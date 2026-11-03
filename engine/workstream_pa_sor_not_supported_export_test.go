//go:build !shn_coreonly

package engine

import (
	"net/http/httptest"
	"testing"
)

// CRDIngressWithoutCoverageForTest sends a CDS Hooks order-select request for
// the patient "example" that carries no coverage through a provider gateway's
// ingress route at level, with sor as that gateway's system of record, and
// returns the answer to the participant's own caller. For the end-to-end rows
// in package engine_test (workstream_pa_sor_not_supported_e2e_test.go).
func CRDIngressWithoutCoverageForTest(t *testing.T, sor SystemOfRecord, level ConformanceEnforcement) (int, string) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = sor
	env.originator.cfg.ConformanceEnforcement = level
	w := httptest.NewRecorder()
	env.originator.ingressRoute(RouteCRD)(w, crdIngressPost(ehrRequest(patientOnly)))
	if n := env.routeHitCount(); n != 0 {
		t.Fatalf("the request crossed the network (%d)", n)
	}
	return w.Code, w.Body.String()
}

// AllLevelsForTest is every conformance level.
var AllLevelsForTest = allLevels
