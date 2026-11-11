//go:build !shn_coreonly

package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The federated query's answer is written to the participant's system with
// the media type the facility's system stated, exactly, whatever the type,
// and application/fhir+json when its frame states none, at every
// conformance level: the media type is no check a level turns on.
func TestFederatedQueryIngress_WritesTheFacilitysStatedAnswerType(t *testing.T) {
	t.Parallel()
	task := fqTask(t, "MBR-UC05", "facility")
	answer := fqAnswer(t, task, fqRecords("Patient/MBR-UC05"))
	for _, row := range statedAnswerTypes {
		for _, level := range allLevels {
			t.Run(statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
				env := newFQExchange(t, level)
				env.payerReturns(framedAnswer(answer, row.stated, http.StatusOK))
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(task))
				wantWritten(t, rec, http.StatusOK, row.written, answer)
				if x := got.only(t); x.Outcome != ExchangeAnswered {
					t.Fatalf("recorded %s, want answered", x.Outcome)
				}
			})
		}
	}
}
