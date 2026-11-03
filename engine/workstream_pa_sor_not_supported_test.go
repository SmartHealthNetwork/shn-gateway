//go:build !shn_coreonly

package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A coverage search the system of record answers definitely unsupported
// means the system, as connected, cannot supply the coverage: the request
// with none to route by is refused 422, as for a connector that cannot
// search. A search it answers with any other error status is a backend
// failure, 502: what it holds is unknown, never empty. Both hold at every
// level, with the enrichment opt-in and without it, at the CDS Hooks and the
// $questionnaire-package ingress.
func TestIngress_CoverageSearchNotSupportedOrFailed(t *testing.T) {
	rows := []struct {
		name   string
		answer searchAnswer
		status int
		msg    string
		record func(*testing.T, ExchangeRecord)
	}{
		{"a definite not-supported answer", searchAnswer{err: &SearchError{Outcome: SearchUnsupported, Reason: "search not supported"}},
			http.StatusUnprocessableEntity, "the system of record cannot search for it", func(t *testing.T, r ExchangeRecord) {
				t.Helper()
				if r.Outcome != ExchangeRefused || r.RefusedBy != RefusedByProviderGateway || r.Status != http.StatusUnprocessableEntity {
					t.Fatalf("recorded %s by %q status %d, want this gateway's refusal, 422", r.Outcome, r.RefusedBy, r.Status)
				}
			}},
		{"an error answer", searchAnswer{err: &SearchError{Outcome: SearchFailed, Reason: "search answered with an error status"}},
			http.StatusBadGateway, coverageSearchAnsweredError, func(t *testing.T, r ExchangeRecord) {
				t.Helper()
				// Recorded as the ingress records its system being
				// unavailable (503): never a refusal.
				if r.Outcome != ExchangeOther || r.Rule != "" || r.RefusedBy != "" {
					t.Fatalf("recorded %s on %q by %q, want %s, as for an unavailable system: the system failed to answer, never a refusal", r.Outcome, r.Rule, r.RefusedBy, ExchangeOther)
				}
			}},
	}
	crd := ehrRequest(patientOnly)
	ownPatient := `{"name":"referenced","resource":{"resourceType":"Patient","id":"example"}}`
	dtr := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire, `{"name":"context","valueString":"ctx-1"}`, ownPatient)
	for _, row := range rows {
		for _, enrich := range []bool{false, true} {
			for _, level := range allLevels {
				for _, ingress := range []struct {
					name  string
					route string
					body  []byte
				}{{"cds hooks", RouteCRD, crd}, {"questionnaire package", RouteDTR, dtr}} {
					t.Run(row.name+"/"+ingress.name+"/"+optInLabel(enrich)+"/"+level.String(), func(t *testing.T) {
						s := newPrefetchSoR()
						s.searches["Coverage"] = row.answer
						env := newInProcessExchange(t)
						var got exchangeRecords
						env.originator.cfg.ExchangeObserved = got.observe
						env.originator.cfg.SoR = s.sor()
						env.originator.cfg.ConformanceEnforcement = level
						optInEnrichment(env.originator, enrich)
						req := crdIngressPost(ingress.body)
						if ingress.route == RouteDTR {
							declareFramedDTR(t, env, true)
							req = httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(ingress.body))
							req.Header.Set("Content-Type", "application/fhir+json")
						}
						rec := httptest.NewRecorder()
						env.originator.ingressRoute(ingress.route)(rec, req)
						refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
						if searched, _ := s.calls(); len(searched) == 0 {
							t.Fatal("the coverage was not searched")
						}
						row.record(t, got.only(t))
					})
				}
			}
		}
	}
}
