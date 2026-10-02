package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A Da Vinci-native request is carried as the participant's client sent it
// unless the participant opts in to enrichment (Config.EnrichNativeRequests).
// These rows pin the default: nothing is added, the callback strip
// (E-01) is the only edit, and a coverage the request leaves out is read from
// the system of record only to choose the payer.

// At every conformance level, a CDS Hooks request that carries its coverage
// is carried with only the callback removed: no prefetch key is added, no fill
// finding is recorded, and the system of record is neither searched nor read.
func TestLevelCRDIngress_DefaultFillsNothing(t *testing.T) {
	body := ehrRequest(`"coverage":` + ehrCoverage)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			env, rec, findings := levelIngressRow(t, s, level, body)
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
			}
			sent := sentRequest(t, env)
			wantCarriedLessCallback(t, body, sent)
			if got := names(membersOf(t, sent, "prefetch")); !slices.Equal(got, []string{"coverage"}) {
				t.Fatalf("prefetch keys %v, want only the EHR's", got)
			}
			for _, f := range findings {
				if f.Rule == string(RulePrefetchFill) {
					t.Fatalf("nothing is filled without enrichment, so nothing is left unfilled: %+v", f)
				}
			}
			if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 {
				t.Fatalf("the system of record was searched %v or read %v for a request carrying its coverage", searched, read)
			}
		})
	}
}

// A CDS Hooks request that leaves out its coverage is routed by the coverage
// the system of record holds, and carried without it: the only edit is the
// callback strip.
func TestCRDIngress_DefaultRoutesByCoverageItDoesNotInsert(t *testing.T) {
	for name, row := range map[string]struct {
		sor      func(*testing.T) *prefetchSoR
		body     []byte
		wantKeys []string
	}{
		"no prefetch": {func(t *testing.T) *prefetchSoR {
			s := newPrefetchSoR()
			s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", "00001")))
			return s
		}, ehrRequest("-"), nil},
		"the patient only": {func(t *testing.T) *prefetchSoR {
			s := newPrefetchSoR()
			s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", "00001")))
			return s
		}, ehrRequest(patientOnly), []string{"patient"}},
		// Read only to route by, the system's own id serves; so it does under
		// enrichment (TestCRDIngress_OptInRoutesAPatientNamedByAnotherIDAsTheDefault).
		"the system of record names the patient differently": {namedDifferently, ehrRequest(patientOnly), []string{"patient"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := row.sor(t)
			env, rec := carryRow(t, s, row.body)
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
			}
			sent := sentRequest(t, env)
			wantCarriedLessCallback(t, row.body, sent)
			if row.wantKeys != nil {
				if got := names(membersOf(t, sent, "prefetch")); !slices.Equal(got, row.wantKeys) {
					t.Fatalf("prefetch keys %v, want only the EHR's %v", got, row.wantKeys)
				}
			} else if _, ok := valueOf(t, sent, "prefetch"); ok {
				t.Fatal("a request sent without prefetch must be carried without prefetch")
			}
			if searched, _ := s.calls(); !onlyCoverageSearched(searched) {
				t.Fatalf("searched %v, want only the coverage the request is routed by", searched)
			}
			p, _ := mustPrepare(t, carryGateway(row.sor(t)), row.body)
			if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditCDSCallbackStrip}) {
				t.Fatalf("edits %v, want only the callback strip", got)
			}
		})
	}
}

// The routing read at the default searches every Coverage the system of record
// holds for the patient, with the payor include (unlike the coverage template,
// status=active: what is read only to route by is never carried); a payor the
// search did not include is resolved through the system of record, and the
// request is routed by it and carried without the coverage.
func TestCRDIngress_DefaultRoutingReadResolvesPayorFromTheSystem(t *testing.T) {
	cov := "{ \"resourceType\" : \"Coverage\", \"id\" : \"cov-9\", \"status\" : \"active\",\n  \"beneficiary\" : { \"reference\" : \"Patient/" + prefetchSoRID + "\" },\n  \"payor\" : [ { \"reference\" : \"Organization/pay-9\" } ] }"
	org := `{"resourceType":"Organization","id":"pay-9","identifier":[{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"00001"}]}`
	s := newPrefetchSoR()
	s.answer(t, "Coverage", searchPage(cov)) // the server did not include the payor
	s.reads["Organization/pay-9"] = []byte(org)
	body := ehrRequest(patientOnly)
	env, rec := carryRow(t, s, body)
	if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
		t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
	}
	wantCarriedLessCallback(t, body, sentRequest(t, env))
	searched, read := s.calls()
	if want := []string{routingCoverageQuery}; !slices.Equal(searched, want) {
		t.Fatalf("searched %v, want %v", searched, want)
	}
	if !slices.Contains(read, "Organization/pay-9") {
		t.Fatalf("read %v: want the payor resolved through the system of record", read)
	}
	t.Run("a payor the search included routes without another read", func(t *testing.T) {
		s := newPrefetchSoR() // holds no Organization to read
		s.answer(t, "Coverage", page("", "", sorEntry(cov), `{"fullUrl":"https://sor.example/fhir/Organization/pay-9","resource":`+org+`,"search":{"mode":"include"}}`))
		env, rec := carryRow(t, s, body)
		if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
			t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
		}
		if _, read := s.calls(); slices.Contains(read, "Organization/pay-9") {
			t.Fatalf("the included payor was read again: %v", read)
		}
	})
	t.Run("without the payor in the system nothing routes", func(t *testing.T) {
		s := newPrefetchSoR()
		s.answer(t, "Coverage", searchPage(cov))
		env, rec := carryRow(t, s, body)
		refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "no payer identifier on member coverage")
	})
}

// routingCoverageQuery is the coverage search the gateway runs only to route by:
// every Coverage, with its payor.
const routingCoverageQuery = "Coverage?patient=Patient%2F" + prefetchSoRID + "&_include=Coverage%3Apayor"

// sorCoverageStatus is sorCoverage with the given status.
func sorCoverageStatus(id, payerValue, status string) string {
	return strings.Replace(sorCoverage(id, payerValue), `"status" : "active"`, `"status" : "`+status+`"`, 1)
}

// The coverage read only to route by routes on the active Coverages when any
// is active, else on the others when they name one payer, at the CDS Hooks and
// questionnaire-package ingress alike (routingCoverageChoice). A stale
// cancelled coverage naming another payer never makes routing ambiguous, and a
// member whose coverage is no longer in force is still sent to its payer, which
// answers that it does not cover the member.
func TestDefaultRoutingReadRoutesOnActiveCoverageFirst(t *testing.T) {
	const known, unknown = "00001", "99998" // the test router knows only 00001
	for _, row := range []struct {
		name   string
		page   []byte
		status int
		msg    string
		// crd is a CDS Hooks request's status where it differs: no coverage
		// to route by is CDS Hooks' 412 (the service could not obtain the
		// data it needs); $questionnaire-package keeps its 422.
		crd int
	}{
		{"an active and a cancelled coverage naming another payer: the active one",
			searchPage(sorCoverageStatus("cov-old", unknown, "cancelled"), sorCoverage("cov-1", known)), http.StatusOK, "", 0},
		{"only cancelled coverages naming one payer",
			searchPage(sorCoverageStatus("cov-1", known, "cancelled"), sorCoverageStatus("cov-2", known, "cancelled")), http.StatusOK, "", 0},
		{"only cancelled coverages naming two payers",
			searchPage(sorCoverageStatus("cov-1", known, "cancelled"), sorCoverageStatus("cov-2", unknown, "cancelled")), http.StatusUnprocessableEntity, "ambiguous coverage for routing", 0},
		{"two active coverages naming two payers",
			searchPage(sorCoverage("cov-1", known), sorCoverage("cov-2", unknown), sorCoverageStatus("cov-3", known, "cancelled")), http.StatusUnprocessableEntity, "ambiguous coverage for routing", 0},
		{"no coverage", nil, http.StatusUnprocessableEntity, "no coverage in request or system of record", http.StatusPreconditionFailed},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, ingress := range []struct {
				name string
				run  func(*prefetchSoR) (*inProcessExchange, *httptest.ResponseRecorder)
			}{
				{"CDS Hooks", func(s *prefetchSoR) (*inProcessExchange, *httptest.ResponseRecorder) {
					return carryRow(t, s, ehrRequest(patientOnly))
				}},
				{"questionnaire-package", func(s *prefetchSoR) (*inProcessExchange, *httptest.ResponseRecorder) {
					return dtrIngressRow(t, s, ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire))
				}},
			} {
				t.Run(ingress.name, func(t *testing.T) {
					s := newPrefetchSoR()
					if row.page != nil {
						s.answer(t, "Coverage", row.page)
					}
					env, rec := ingress.run(s)
					if row.status == http.StatusOK {
						if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
							t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
						}
					} else {
						status := row.status
						if ingress.name == "CDS Hooks" && row.crd != 0 {
							status = row.crd
						}
						refusedBeforeTheNetwork(t, env, rec, status, row.msg)
					}
					if searched, _ := s.calls(); !slices.Equal(searched, []string{routingCoverageQuery}) {
						t.Fatalf("searched %v, want only %s", searched, routingCoverageQuery)
					}
				})
			}
		})
	}
	// Under enrichment the coverage is a value carried to the payer: it is the
	// coverage template's search (status=active), not the routing choice. The
	// request is still routed as above (TestEnrichedRoutesAsTheDefault).
	t.Run("enriched: the carried coverage is the template's search", func(t *testing.T) {
		s := newPrefetchSoR()
		s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", known)))
		if _, rec := ingressRow(t, s, ehrRequest(patientOnly)); rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		if searched, _ := s.calls(); !slices.Contains(searched, "Coverage?patient=Patient%2F"+prefetchSoRID+"&status=active&_include=Coverage%3Apayor") {
			t.Fatalf("searched %v", searched)
		}
	})
}

// enrichedCoverageQuery is the coverage template's search, the coverage value
// carried under enrichment: the active Coverages, with their payors.
const enrichedCoverageQuery = "Coverage?patient=Patient%2F" + prefetchSoRID + "&status=active&_include=Coverage%3Apayor"

// sorPayorCoverage is a Coverage for the system of record's patient with
// status, naming the Organization org as its payor.
func sorPayorCoverage(id, status, org string) string {
	return "{ \"resourceType\" : \"Coverage\", \"id\" : \"" + id + "\", \"status\" : \"" + status + "\",\n  \"beneficiary\" : { \"reference\" : \"Patient/" + prefetchSoRID +
		"\" },\n  \"payor\" : [ { \"reference\" : \"Organization/" + org + "\" } ] }"
}

// includedPayer is the search entry of the payer Organization org, with the
// payer identifier value, as a server includes it (_include=Coverage:payor).
func includedPayer(org, value string) string {
	return `{"fullUrl":"https://sor.example/fhir/Organization/` + org + `","resource":{"resourceType":"Organization","id":"` + org +
		`","identifier":[{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"` + value + `"}]},"search":{"mode":"include"}}`
}

// Under enrichment the payer is still chosen from every Coverage, the active
// ones first, as without it; the coverage carried is only the coverage
// template's search (status=active). A member whose only Coverage is cancelled
// is routed to that coverage's payer as at the default and carries null (CDS
// Hooks) or no coverage parameter ($questionnaire-package): the opt-in never
// leaves a member with less to route by. A member with an active Coverage at one payer
// and a cancelled one at another is routed to the first and carries only the
// active Coverage and its payor. A system that cannot answer the template's
// search leaves the coverage out, and the request is routed by the routing
// read with that read's every answer at the default: fenced, resolved
// through the system of record, or refused.
func TestEnrichedRoutesAsTheDefault(t *testing.T) {
	const known, unknown = "00001", "99998" // the test router knows only 00001
	cancelledOnly := func(t *testing.T) *prefetchSoR {
		s := newPrefetchSoR()
		s.answerQuery(t, enrichedCoverageQuery, "Coverage", page("", ""))
		s.answerQuery(t, routingCoverageQuery, "Coverage", page("", "", sorEntry(sorPayorCoverage("cov-a", "cancelled", "pay-a")), includedPayer("pay-a", known)))
		return s
	}
	activeAndStale := func(t *testing.T) *prefetchSoR {
		s := newPrefetchSoR()
		active, stale := sorPayorCoverage("cov-a", "active", "pay-a"), sorPayorCoverage("cov-b", "cancelled", "pay-b")
		s.answerQuery(t, enrichedCoverageQuery, "Coverage", page("", "", sorEntry(active), includedPayer("pay-a", known)))
		s.answerQuery(t, routingCoverageQuery, "Coverage", page("", "", sorEntry(stale), sorEntry(active), includedPayer("pay-b", unknown), includedPayer("pay-a", known)))
		return s
	}
	// A server that does not apply the template's status filter answers it
	// with every Coverage.
	unfiltering := func(t *testing.T) *prefetchSoR {
		s := newPrefetchSoR()
		active, stale := sorPayorCoverage("cov-a", "active", "pay-a"), sorPayorCoverage("cov-b", "cancelled", "pay-b")
		s.answer(t, "Coverage", page("", "", sorEntry(stale), sorEntry(active), includedPayer("pay-b", unknown), includedPayer("pay-a", known)))
		return s
	}
	// A server that refuses the template's status-filtered search (a strict
	// server's 400, which a connector reports as unsupported) answers the
	// unfiltered routing read.
	refusedThen := func(routing searchAnswer) *prefetchSoR {
		s := newPrefetchSoR()
		s.byQuery[enrichedCoverageQuery] = searchAnswer{err: &SearchError{Outcome: SearchUnsupported, Reason: "status filter not supported"}}
		s.byQuery[routingCoverageQuery] = routing
		return s
	}
	filterRefused := func(t *testing.T) *prefetchSoR {
		return refusedThen(searchAnswer{res: resultOf(t, "Coverage", page("", "", sorEntry(sorPayorCoverage("cov-a", "active", "pay-a")), includedPayer("pay-a", known)))})
	}
	// When the template's search is refused, the request is routed by the
	// routing read alone, and that read answers as it does at the default:
	// another patient's Coverage is fenced; a payor named by reference only
	// and not included is read from the system of record; a system that cannot
	// answer, or holds no Coverage, refuses the request.
	refusedThenOtherPatient := func(t *testing.T) *prefetchSoR {
		other := strings.Replace(sorPayorCoverage("cov-a", "active", "pay-a"), "Patient/"+prefetchSoRID, "Patient/other", 1)
		return refusedThen(searchAnswer{res: resultOf(t, "Coverage", page("", "", sorEntry(other), includedPayer("pay-a", known)))})
	}
	refusedThenPayorByReference := func(t *testing.T) *prefetchSoR {
		s := refusedThen(searchAnswer{res: resultOf(t, "Coverage", searchPage(sorPayorCoverage("cov-a", "active", "pay-a")))}) // the payor not included
		s.reads["Organization/pay-a"] = []byte(`{"resourceType":"Organization","id":"pay-a","identifier":[{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"` + known + `"}]}`)
		return s
	}
	refusedThenUnavailable := func(*testing.T) *prefetchSoR {
		return refusedThen(searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "system of record unavailable"}})
	}
	refusedThenNone := func(t *testing.T) *prefetchSoR {
		return refusedThen(searchAnswer{res: resultOf(t, "Coverage", page("", ""))})
	}
	coverageSearches := func(s *prefetchSoR) []string {
		searched, _ := s.calls()
		return slices.DeleteFunc(searched, func(q string) bool { return !strings.HasPrefix(q, "Coverage?") })
	}

	t.Run("CDS Hooks", func(t *testing.T) {
		body := ehrRequest(patientOnly)
		routed := func(t *testing.T, s *prefetchSoR, enrich bool) []byte {
			t.Helper()
			env, rec := ingressRowWith(t, s, body, enrich)
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
			}
			return sentRequest(t, env)
		}
		t.Run("a cancelled-only member under enrichment is routed and carries null", func(t *testing.T) {
			s := cancelledOnly(t)
			sent := routed(t, s, true)
			if v, ok := valueOf(t, sent, "prefetch", "coverage"); !ok || v != "null" {
				t.Fatalf("prefetch.coverage = %q (present %v), want the template's search: null", v, ok)
			}
			if got := coverageSearches(s); !slices.Equal(got, []string{enrichedCoverageQuery, routingCoverageQuery}) {
				t.Fatalf("coverage searches %v, want the template's search, then the routing read", got)
			}
		})
		t.Run("the same member without enrichment is routed and carries nothing added", func(t *testing.T) {
			s := cancelledOnly(t)
			sent := routed(t, s, false)
			wantCarriedLessCallback(t, body, sent)
			if got := coverageSearches(s); !slices.Equal(got, []string{routingCoverageQuery}) {
				t.Fatalf("coverage searches %v, want only the routing read", got)
			}
		})
		t.Run("an active coverage and a stale one at another payer under enrichment: the active one only", func(t *testing.T) {
			s := activeAndStale(t)
			sent := routed(t, s, true)
			v, ok := valueOf(t, sent, "prefetch", "coverage")
			if !ok || !strings.Contains(v, `"cov-a"`) || !strings.Contains(v, `"pay-a"`) {
				t.Fatalf("prefetch.coverage = %s, want the active Coverage and its payor", v)
			}
			if strings.Contains(v, `"cov-b"`) || strings.Contains(v, "pay-b") {
				t.Fatalf("prefetch.coverage carries the stale coverage or its payer: %s", v)
			}
			// The template's search found a Coverage to route by: no second read.
			if got := coverageSearches(s); !slices.Equal(got, []string{enrichedCoverageQuery}) {
				t.Fatalf("coverage searches %v, want only the template's search", got)
			}
		})
		t.Run("a server that refuses the template's search under enrichment: routed, the key left out", func(t *testing.T) {
			s := filterRefused(t)
			sent := routed(t, s, true)
			if v, ok := valueOf(t, sent, "prefetch", "coverage"); ok {
				t.Fatalf("prefetch.coverage = %s, want the key left out", v)
			}
			if got := coverageSearches(s); !slices.Equal(got, []string{enrichedCoverageQuery, routingCoverageQuery}) {
				t.Fatalf("coverage searches %v, want the template's search, then the routing read", got)
			}
		})
		t.Run("a server that does not filter by status: routed on the active one, carried as answered", func(t *testing.T) {
			s := unfiltering(t)
			sent := routed(t, s, true)
			v, _ := valueOf(t, sent, "prefetch", "coverage")
			if !strings.Contains(v, `"cov-a"`) || !strings.Contains(v, `"cov-b"`) {
				t.Fatalf("prefetch.coverage = %s, want the system's answer to the template's search", v)
			}
		})
		t.Run("the template's search refused, the routing read another patient's: fenced", func(t *testing.T) {
			s := refusedThenOtherPatient(t)
			env, rec := ingressRowWith(t, s, body, true)
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
			if got := coverageSearches(s); !slices.Equal(got, []string{enrichedCoverageQuery, routingCoverageQuery}) {
				t.Fatalf("coverage searches %v, want the template's search, then the routing read", got)
			}
		})
		t.Run("the template's search refused, the routing read naming its payor by reference: routed as the default", func(t *testing.T) {
			for _, enrich := range []bool{true, false} {
				s := refusedThenPayorByReference(t)
				sent := routed(t, s, enrich)
				if v, ok := valueOf(t, sent, "prefetch", "coverage"); ok {
					t.Fatalf("enrich %v: prefetch.coverage = %s, want the key left out", enrich, v)
				}
				if _, read := s.calls(); !slices.Contains(read, "Organization/pay-a") {
					t.Fatalf("enrich %v: read %v, want the payor resolved through the system of record", enrich, read)
				}
			}
		})
		t.Run("the template's search refused, the routing read unavailable: 503", func(t *testing.T) {
			s := refusedThenUnavailable(t)
			env, rec := ingressRowWith(t, s, body, true)
			refusedBeforeTheNetwork(t, env, rec, http.StatusServiceUnavailable, "coverage unavailable from system of record")
			if got := coverageSearches(s); !slices.Equal(got, []string{enrichedCoverageQuery, routingCoverageQuery}) {
				t.Fatalf("coverage searches %v, want the template's search, then the routing read", got)
			}
		})
		t.Run("the template's search refused, the routing read finding none: 412", func(t *testing.T) {
			s := refusedThenNone(t)
			env, rec := ingressRowWith(t, s, body, true)
			refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, "no coverage in request or system of record")
			if got := coverageSearches(s); !slices.Equal(got, []string{enrichedCoverageQuery, routingCoverageQuery}) {
				t.Fatalf("coverage searches %v, want the template's search, then the routing read", got)
			}
		})
	})

	t.Run("questionnaire-package", func(t *testing.T) {
		ownPatient := `{"name":"referenced","resource":{"resourceType":"Patient","id":"example"}}`
		body := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire, ownPatient)
		send := func(t *testing.T, s *prefetchSoR, enrich bool) (*inProcessExchange, *httptest.ResponseRecorder) {
			t.Helper()
			env := newInProcessExchange(t)
			env.originator.cfg.SoR = s.sor()
			env.originator.cfg.EnrichNativeRequests = enrich
			declareFramedDTR(t, env, true)
			env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
			return env, postDTRIngress(env, body)
		}
		post := func(t *testing.T, s *prefetchSoR, enrich bool) []byte {
			t.Helper()
			env, rec := send(t, s, enrich)
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
			}
			_, sent := sentOperation(t, env)
			return sent
		}
		t.Run("a cancelled-only member under enrichment is routed and nothing is appended", func(t *testing.T) {
			s := cancelledOnly(t)
			if sent := post(t, s, true); !bytes.Equal(sent, body) {
				t.Fatalf("the EHR's request changed:\n%s", sent)
			}
			if got := coverageSearches(s); !slices.Equal(got, []string{enrichedCoverageQuery, routingCoverageQuery}) {
				t.Fatalf("coverage searches %v, want the template's search, then the routing read", got)
			}
		})
		t.Run("the same member without enrichment is routed and nothing is appended", func(t *testing.T) {
			s := cancelledOnly(t)
			if sent := post(t, s, false); !bytes.Equal(sent, body) {
				t.Fatalf("the EHR's request changed:\n%s", sent)
			}
			if got := coverageSearches(s); !slices.Equal(got, []string{routingCoverageQuery}) {
				t.Fatalf("coverage searches %v, want only the routing read", got)
			}
		})
		t.Run("an active coverage and a stale one at another payer under enrichment: the active one is appended", func(t *testing.T) {
			s := activeAndStale(t)
			sent := post(t, s, true)
			want := `{"name":"coverage","resource":` + sorPayorCoverage("cov-a", "active", "pay-a") + `}`
			if !bytes.Contains(sent, []byte(want)) {
				t.Fatalf("the active Coverage was not appended:\n%s", sent)
			}
			if bytes.Contains(sent, []byte(`"cov-b"`)) || bytes.Contains(sent, []byte("pay-b")) {
				t.Fatalf("the stale coverage or its payer was carried:\n%s", sent)
			}
			if got := coverageSearches(s); !slices.Equal(got, []string{enrichedCoverageQuery}) {
				t.Fatalf("coverage searches %v, want only the template's search", got)
			}
		})
		t.Run("a server that refuses the template's search under enrichment: routed, nothing appended", func(t *testing.T) {
			s := filterRefused(t)
			if sent := post(t, s, true); !bytes.Equal(sent, body) {
				t.Fatalf("the EHR's request changed:\n%s", sent)
			}
			if got := coverageSearches(s); !slices.Equal(got, []string{enrichedCoverageQuery, routingCoverageQuery}) {
				t.Fatalf("coverage searches %v, want the template's search, then the routing read", got)
			}
		})
		t.Run("a server that does not filter by status: routed on, and appending, the active one", func(t *testing.T) {
			sent := post(t, unfiltering(t), true)
			want := `{"name":"coverage","resource":` + sorPayorCoverage("cov-a", "active", "pay-a") + `}`
			if !bytes.Contains(sent, []byte(want)) || bytes.Contains(sent, []byte(`"cov-b"`)) {
				t.Fatalf("want only the active Coverage appended:\n%s", sent)
			}
		})
		// A $questionnaire-package request keeps its own refusal for no
		// coverage, 422, as at the default.
		for _, row := range []struct {
			name   string
			sor    func(*testing.T) *prefetchSoR
			status int
			msg    string
		}{
			{"the template's search refused, the routing read another patient's: fenced", refusedThenOtherPatient, http.StatusBadGateway, fillFencedOtherPatient},
			{"the template's search refused, the routing read unavailable: 503", refusedThenUnavailable, http.StatusServiceUnavailable, "coverage unavailable from system of record"},
			{"the template's search refused, the routing read finding none: 422", refusedThenNone, http.StatusUnprocessableEntity, "no coverage in request or system of record"},
		} {
			t.Run(row.name, func(t *testing.T) {
				s := row.sor(t)
				env, rec := send(t, s, true)
				refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
				if got := coverageSearches(s); !slices.Equal(got, []string{enrichedCoverageQuery, routingCoverageQuery}) {
					t.Fatalf("coverage searches %v, want the template's search, then the routing read", got)
				}
			})
		}
		t.Run("the template's search refused, the routing read naming its payor by reference: routed as the default", func(t *testing.T) {
			for _, enrich := range []bool{true, false} {
				s := refusedThenPayorByReference(t)
				if sent := post(t, s, enrich); !bytes.Equal(sent, body) {
					t.Fatalf("enrich %v: the EHR's request changed:\n%s", enrich, sent)
				}
				if _, read := s.calls(); !slices.Contains(read, "Organization/pay-a") {
					t.Fatalf("enrich %v: read %v, want the payor resolved through the system of record", enrich, read)
				}
			}
		})
	})
}

// Without enrichment a coverage the system of record cannot supply still
// leaves nothing to route by, so the request is refused before the network,
// as under enrichment.
func TestCRDIngress_DefaultRefusesWhenNoCoverageToRouteBy(t *testing.T) {
	s := newPrefetchSoR() // holds no Coverage
	env, rec := carryRow(t, s, ehrRequest(patientOnly))
	refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, "no coverage in request or system of record")
}

// onlyCoverageSearched reports whether the one search run was the coverage
// search the request is routed by.
func onlyCoverageSearched(searched []string) bool {
	return len(searched) == 1 && strings.HasPrefix(searched[0], "Coverage?")
}

// wantCarriedLessCallback asserts sent is the EHR's request with only
// fhirServer and fhirAuthorization removed: every other member, in order,
// byte for byte.
func wantCarriedLessCallback(t *testing.T, ehr, sent []byte) {
	t.Helper()
	want := slices.DeleteFunc(membersOf(t, ehr), func(m member) bool {
		return m.name == "fhirServer" || m.name == "fhirAuthorization"
	})
	got := membersOf(t, sent)
	if !slices.Equal(got, want) {
		t.Fatalf("carried members %v, want the EHR's less the callback %v", names(got), names(want))
	}
}

// A questionnaire-package request is carried exactly as the EHR sent it: no
// Coverage (E-04) and no Patient (E-05) is appended. A request without a
// coverage parameter is routed by the Coverage the system of record holds,
// which is searched for and not added; the Patient is not read.
func TestDTRIngress_DefaultAppendsNothing(t *testing.T) {
	body := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire, `{"name":"context","valueString":"ctx-1"}`)
	s := newPrefetchSoR()
	s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value)))
	env, rec := dtrIngressRow(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	if _, sent := sentOperation(t, env); !bytes.Equal(sent, body) {
		t.Fatalf("the EHR's request changed:\n%s", sent)
	}
	if searched, read := s.calls(); !onlyCoverageSearched(searched) || len(read) != 0 {
		t.Fatalf("searched %v, read %v: want only the coverage the request is routed by", searched, read)
	}
	p, status, msg := carryGateway(s).prepareDTRPackageRequest(context.Background(), body)
	if status != 0 || p.request.Ownership() != relay.OwnershipRelayed || len(p.request.Edits()) != 0 {
		t.Fatalf("%d %s: ownership %v, edits %v; want the EHR's request relayed exactly", status, msg, p.request.Ownership(), p.request.Edits())
	}
}

// The opt-in is the participant's alone: a request posted at the default
// records no prefetch obtained for anything but the routing coverage.
func TestCRDIngress_DefaultRecordsOnlyTheRoutingRead(t *testing.T) {
	s := newPrefetchSoR()
	s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", "00001")))
	obs := &observed{}
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = s.sor()
	env.originator.cfg.Observer = obs.observe
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(ehrRequest(patientOnly)))
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	events := obs.prefetch(t)
	keys := make([]string, 0, len(events))
	for k := range events {
		keys = append(keys, k)
	}
	if !slices.Equal(keys, []string{"coverage"}) {
		detail, _ := json.Marshal(events)
		t.Fatalf("prefetch events for %v (%s), want only the routing coverage", keys, detail)
	}
}

// routingCoverageQueryFor is routingCoverageQuery under the system's own id
// for the patient.
func routingCoverageQueryFor(sorID string) string {
	return strings.Replace(routingCoverageQuery, "Patient%2F"+prefetchSoRID, "Patient%2F"+sorID, 1)
}

// optInLabel names a row's enrichment setting.
func optInLabel(enrich bool) string {
	if enrich {
		return "opt-in"
	}
	return "default"
}

// The coverage read only to route by, under the system's own id, is fenced to
// that id alone, with or without the opt-in: in a system that names the
// patient differently, Patient/<member id> is another patient, so a Coverage
// naming it is refused before the network (502), at the CDS Hooks and
// questionnaire-package ingress alike, at every level.
func TestRoutingReadFencedToTheSystemsOwnID(t *testing.T) {
	for _, enrich := range []bool{false, true} {
		for _, level := range allLevels {
			name := optInLabel(enrich) + "/" + level.String()
			t.Run("CDS Hooks/"+name, func(t *testing.T) {
				s := namedDifferently(t)
				s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", "00001"))) // names Patient/example
				env, rec, _ := levelIngressRowWith(t, s, level, ehrRequest(patientOnly), enrich)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
				if searched, read := s.calls(); !slices.Equal(searched, []string{routingCoverageQueryFor("pat-elsewhere")}) || len(read) != 0 {
					t.Fatalf("searched %v, read %v: want only the routing read under the system's id", searched, read)
				}
			})
			t.Run("questionnaire-package/"+name, func(t *testing.T) {
				body := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)
				s := newPrefetchSoR()
				s.sorID = "sor-9"
				s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value))) // names Patient/example
				env, rec := dtrIngressRowWith(t, s, body, level, enrich)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
				if searched, read := s.calls(); !slices.Equal(searched, []string{routingCoverageQueryFor("sor-9")}) || len(read) != 0 {
					t.Fatalf("searched %v, read %v: want only the routing read under the system's id", searched, read)
				}
			})
		}
	}
}

// The opt-in never leaves a member with less to route by than the default. A system
// of record that names the patient by an id other than context.patientId
// cannot fill the coverage: the value would name the patient by an id the
// request does not use. So, under the opt-in as without it, a CDS Hooks
// request that carries its patient and no coverage is routed by the coverage
// read only to route by, under the system's own id (fenced to that id alone:
// TestRoutingReadFencedToTheSystemsOwnID), and carried with only the callback
// removed: no coverage is filled, and no finding is recorded, at every level.
func TestCRDIngress_OptInRoutesAPatientNamedByAnotherIDAsTheDefault(t *testing.T) {
	body := ehrRequest(patientOnly)
	for _, enrich := range []bool{false, true} {
		for _, level := range allLevels {
			t.Run(optInLabel(enrich)+"/"+level.String(), func(t *testing.T) {
				s := namedDifferently(t)
				env, rec, findings := levelIngressRowWith(t, s, level, body, enrich)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
				}
				sent := sentRequest(t, env)
				wantCarriedLessCallback(t, body, sent)
				if got := names(membersOf(t, sent, "prefetch")); !slices.Equal(got, []string{"patient"}) {
					t.Fatalf("prefetch keys %v, want only the EHR's patient: the coverage is not filled", got)
				}
				if searched, read := s.calls(); !slices.Equal(searched, []string{routingCoverageQueryFor("pat-elsewhere")}) || len(read) != 0 {
					t.Fatalf("searched %v, read %v: want only the routing read under the system's id", searched, read)
				}
				if len(findings) != 0 {
					t.Fatalf("nothing was left unfilled that the request needs, got findings %+v", findings)
				}
				g := carryGateway(namedDifferently(t))
				g.cfg.EnrichNativeRequests = enrich
				p, _ := mustPrepare(t, g, body)
				if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditCDSCallbackStrip}) {
					t.Fatalf("edits %v, want only the callback strip", got)
				}
				if !p.coverageFromSoR || p.values["coverage"] == nil {
					t.Fatal("the request must be routed by the coverage read from the system of record")
				}
			})
		}
	}
}

// Under the opt-in, a CDS Hooks request that also leaves out its patient asks
// for a value the system cannot supply under context.patientId
// (RulePrefetchFill): strict refuses before anything is read, as it always
// has; below strict the patient is left out and the request is routed by the
// routing read under the system's own id, as without the opt-in, with nothing
// added.
func TestLevelCRDIngress_OptInPatientNamedByAnotherIDLeftOut(t *testing.T) {
	for name, body := range map[string][]byte{"no prefetch": ehrRequest("-"), "an empty prefetch": ehrRequest("")} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				s := namedDifferently(t)
				env, rec, findings := levelFillRow(t, s, level, body)
				wantLevelOutcome(t, level, env, rec, findings, RulePrefetchFill, http.StatusUnprocessableEntity, patientNamedDifferently)
				searched, read := s.calls()
				if refusesAt(level, RulePrefetchFill) {
					if len(searched) != 0 || len(read) != 0 {
						t.Fatalf("a refused request read the system of record: searched %v, read %v", searched, read)
					}
					return
				}
				wantCarriedLessCallback(t, body, sentRequest(t, env))
				if !slices.Equal(searched, []string{routingCoverageQueryFor("pat-elsewhere")}) || len(read) != 0 {
					t.Fatalf("searched %v, read %v: want only the routing read under the system's id", searched, read)
				}
			})
		}
	}
}

// Without the opt-in, the same request (no patient and no coverage, for a
// member the system of record names by another id) is routed at every level
// with no finding: nothing is filled, and the system of record is read only
// to route by, under its own id.
func TestLevelCRDIngress_PatientNamedByAnotherIDRoutedWithoutTheOptIn(t *testing.T) {
	for name, body := range map[string][]byte{"no prefetch": ehrRequest("-"), "an empty prefetch": ehrRequest("")} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				s := namedDifferently(t)
				env, rec, findings := levelIngressRowWith(t, s, level, body, false)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				if len(findings) != 0 {
					t.Fatalf("findings %+v, want none", findings)
				}
				wantCarriedLessCallback(t, body, sentRequest(t, env))
				searched, read := s.calls()
				if !slices.Equal(searched, []string{routingCoverageQueryFor("pat-elsewhere")}) || len(read) != 0 {
					t.Fatalf("searched %v, read %v: want only the routing read under the system's id", searched, read)
				}
			})
		}
	}
}

// $questionnaire-package: the opt-in never leaves a member with less to route by
// than the default. A system of record that names the patient by another id can
// supply no Coverage to append (it would name the patient by an id the
// request does not use) and no Patient (E-05 needs the member id). So, under
// the opt-in as without it, a request carrying no coverage parameter is
// routed by the routing read under the system's own id (fenced to that id
// alone: TestRoutingReadFencedToTheSystemsOwnID) and carried exactly as the
// EHR sent it, at every level.
func TestDTRIngress_OptInRoutesAPatientNamedByAnotherIDAsTheDefault(t *testing.T) {
	body := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)
	sor := func(t *testing.T) *prefetchSoR {
		s := newPrefetchSoR()
		s.sorID = "sor-9"
		s.answer(t, "Coverage", searchPage(strings.ReplaceAll(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value), "Patient/"+prefetchSoRID, "Patient/sor-9")))
		return s
	}
	for _, enrich := range []bool{false, true} {
		for _, level := range allLevels {
			t.Run(optInLabel(enrich)+"/"+level.String(), func(t *testing.T) {
				s := sor(t)
				env, rec := dtrIngressRowWith(t, s, body, level, enrich)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
				}
				if _, sent := sentOperation(t, env); !bytes.Equal(sent, body) {
					t.Fatalf("the EHR's request changed:\n%s", sent)
				}
				if searched, read := s.calls(); !slices.Equal(searched, []string{routingCoverageQueryFor("sor-9")}) || len(read) != 0 {
					t.Fatalf("searched %v, read %v: want only the routing read under the system's id", searched, read)
				}
				g := carryGateway(sor(t))
				g.cfg.EnrichNativeRequests = enrich
				p, status, msg := g.prepareDTRPackageRequest(context.Background(), body)
				if status != 0 || p.request.Ownership() != relay.OwnershipRelayed || len(p.request.Edits()) != 0 {
					t.Fatalf("%d %s: ownership %v, edits %v; want the EHR's request relayed exactly", status, msg, p.request.Ownership(), p.request.Edits())
				}
			})
		}
	}
}

// A request that carries its coverage has nothing filled by default, so a
// system of record that cannot answer for the patient changes nothing: at
// every level the request is carried with only the callback removed, and no
// fill finding is recorded.
func TestLevelCRDIngress_DefaultSystemUnavailableRequestCarriesCoverage(t *testing.T) {
	body := ehrRequest(`"coverage":` + ehrCoverage)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			s.refErr = &SoRReadError{Kind: SoRUnavailable}
			env, rec, findings := levelIngressRow(t, s, level, body)
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
			}
			wantCarriedLessCallback(t, body, sentRequest(t, env))
			for _, f := range findings {
				if f.Rule == string(RulePrefetchFill) {
					t.Fatalf("nothing is filled by default, so nothing is left unfilled: %+v", f)
				}
			}
		})
	}
}
