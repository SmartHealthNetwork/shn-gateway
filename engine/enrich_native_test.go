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
		// Under enrichment this request is refused (patientNamedDifferently),
		// because an inserted coverage would name the patient by another id.
		// Read only to route by, the system's own id serves.
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

// Without enrichment a coverage the system of record cannot supply still
// leaves nothing to route by, so the request is refused before the network,
// as under enrichment.
func TestCRDIngress_DefaultRefusesWhenNoCoverageToRouteBy(t *testing.T) {
	s := newPrefetchSoR() // holds no Coverage
	env, rec := carryRow(t, s, ehrRequest(patientOnly))
	refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "no coverage in request or system of record")
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

// The routing-only coverage read under the system's own id is fenced to that
// id alone: in a system that names the patient differently, Patient/<member
// id> is another patient, so a Coverage naming it is refused before the
// network (502), at the CDS Hooks and questionnaire-package ingress alike.
func TestDefaultRoutingReadFencedToTheSystemsOwnID(t *testing.T) {
	t.Run("CDS Hooks", func(t *testing.T) {
		s := namedDifferently(t)
		s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", "00001"))) // names Patient/example
		env, rec := carryRow(t, s, ehrRequest(patientOnly))
		refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
	})
	t.Run("questionnaire-package", func(t *testing.T) {
		body := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)
		s := newPrefetchSoR()
		s.sorID = "sor-9"
		s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value))) // names Patient/example
		env, rec := dtrIngressRow(t, s, body)
		refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
	})
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
