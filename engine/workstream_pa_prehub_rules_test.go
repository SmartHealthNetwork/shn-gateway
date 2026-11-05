//go:build !shn_coreonly

package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Rows for the rule a gateway records for the refusals it makes before the
// request reaches the Hub that named none (rule other) before v1.7.0.

// A member the participant's own system of record does not hold is refused
// only when the participant opted in (Config.RequireKnownMembers): the check
// is its own, recorded as conformance with no reason, on every ingress route
// that binds a member. Not opted in, the member is bound from the request and
// never refused for it.
func TestPreHubRules_UnknownMemberIsTheParticipantsOwnCheck(t *testing.T) {
	pasBase := pasIngressBundle("00001", "")
	routes := []struct {
		name, route, path, service string
		body                       func(t *testing.T) []byte
		status                     int
		msg                        string
		prefetchSoR                bool
	}{
		{name: "crd", route: RouteCRD, path: "/cds-services/shn-order-select", service: "shn-order-select",
			body: func(*testing.T) []byte {
				return []byte(strings.ReplaceAll(string(conformantCRDRequest("MBR-COVERED")), "MBR-COVERED", "MBR-NOBODY"))
			},
			status: http.StatusBadRequest, msg: "unknown member"},
		{name: "pas", route: RoutePAS, path: "/Claim/$submit",
			body:   func(*testing.T) []byte { return []byte(strings.ReplaceAll(pasBase, "MBR-COVERED", "MBR-NOBODY")) },
			status: http.StatusBadRequest, msg: "unknown member"},
		{name: "pas-inquire", route: RoutePASInquire, path: "/Claim/$inquire",
			body: func(t *testing.T) []byte {
				return []byte(strings.ReplaceAll(levelInquiry(t, "MBR-COVERED", ""), "MBR-COVERED", "MBR-NOBODY"))
			},
			status: http.StatusBadRequest, msg: "unknown member"},
		{name: "dtr", route: RouteDTR, path: "/Questionnaire/$questionnaire-package",
			body:   func(*testing.T) []byte { return ehrParams(ehrCoverageParam("MBR-UNKNOWN", "00001"), dtrQuestionnaire) },
			status: http.StatusForbidden, msg: "request patient does not resolve", prefetchSoR: true},
		{name: "eligibility", route: RouteEligibility, path: "/CoverageEligibilityRequest/$submit",
			body: func(t *testing.T) []byte {
				return []byte(strings.ReplaceAll(string(eligibilityRequest(t, "MBR-COVERED")), "MBR-COVERED", "MBR-NOBODY"))
			},
			status: http.StatusBadRequest, msg: "unknown member"},
	}
	for _, rt := range routes {
		for _, level := range allLevels {
			for _, optedIn := range []bool{true, false} {
				name := rt.name + "/" + level.String() + "/not opted in"
				if optedIn {
					name = rt.name + "/" + level.String() + "/opted in"
				}
				t.Run(name, func(t *testing.T) {
					env := newInProcessExchange(t)
					if rt.prefetchSoR {
						env.originator.cfg.SoR = newPrefetchSoR().sor()
						declareFramedDTR(t, env, true)
					}
					env.originator.cfg.ConformanceEnforcement = level
					env.originator.cfg.RequireKnownMembers = optedIn
					var got exchangeRecords
					env.originator.cfg.ExchangeObserved = got.observe
					req := httptest.NewRequest(http.MethodPost, rt.path, strings.NewReader(string(rt.body(t))))
					if rt.service != "" {
						req.SetPathValue("id", rt.service)
					}
					rec := httptest.NewRecorder()
					env.originator.ingressRoute(rt.route)(rec, req)
					x := got.only(t)
					if optedIn {
						refusedBeforeTheNetwork(t, env, rec, rt.status, rt.msg)
						wantRefusal(t, x, rt.status, RefusedByProviderGateway, RefusalConformance)
						if x.RefusalReason != "" {
							t.Fatalf("an opt-in check recorded the reason %q: only network rules carry one", x.RefusalReason)
						}
						return
					}
					// Bound from the request and sent: whatever the payer's
					// answer, the member was not refused for being unknown.
					if strings.Contains(rec.Body.String(), rt.msg) || env.routeHitCount() == 0 {
						t.Fatalf("not opted in, the member was refused: %d %s (rule %q, sent %d)", rec.Code, rec.Body.String(), x.Rule, env.routeHitCount())
					}
				})
			}
		}
	}
}

// The federated query's ingress records the same check the same way.
func TestPreHubRules_FederatedQueryUnknownMemberIsTheParticipantsOwnCheck(t *testing.T) {
	unknown := fqTask(t, "MBR-NOBODY", "facility")
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newFQExchange(t, level)
			env.originator.cfg.RequireKnownMembers = true
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(unknown))
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "unknown member")
			x := got.only(t)
			wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, RefusalConformance)
			if x.RefusalReason != "" {
				t.Fatalf("an opt-in check recorded the reason %q", x.RefusalReason)
			}
		})
	}
}

// The DTR package route refuses a payer that has not declared framed DTR
// operations as routing, with its reason, as the $next-question route and
// module 1 do; its answer is unchanged.
func TestPreHubRules_DTRPackageUnframedPayerIsRouting(t *testing.T) {
	const coverage = `{"name":"coverage","resource":{"resourceType":"Coverage","id":"cov-1","status":"active","beneficiary":{"reference":"Patient/` + prefetchMember + `"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}}`
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.SoR = newPrefetchSoR().sor()
			env.originator.cfg.ConformanceEnforcement = level
			declareFramedDTR(t, env, false)
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", strings.NewReader(string(ehrParams(coverage, dtrQuestionnaire))))
			env.originator.ingressRoute(RouteDTR)(rec, req)
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, shnsdk.ErrFramedDTRUnsupported.Error())
			x := got.only(t)
			wantRefusal(t, x, http.StatusBadGateway, RefusedByProviderGateway, RefusalRouting)
			if x.RefusalReason != string(reasonFramedDTRUnsupported) {
				t.Fatalf("reason %q, want %q", x.RefusalReason, reasonFramedDTRUnsupported)
			}
			wantReasonInAnswer(t, x, rec.Body.String())
		})
	}
}

// An origination body over the network's limit is a size refusal, recorded as
// routing with its reason, on each originating route that reads its body
// against the limit (module1_originate.go, module1_originate_resume.go and the
// claim input's read, module1_claimfacts.go).
func TestPreHubRules_OriginationBodyOverTheLimitIsRouting(t *testing.T) {
	big := `{"pad":"` + strings.Repeat("a", shnsdk.MaxRequestBytes+1) + `"}`
	for _, pattern := range []string{
		"POST /scenario/uc01", "POST /scenario/uc03", "POST /scenario/uc04", "POST /scenario/uc05",
		"POST /scenario/uc07", "POST /scenario/uc07hcpcs", "POST /scenario/uc08",
		"POST /scenario/uc06/start", "POST /scenario/uc07/start", "POST /scenario/pa/inquire",
	} {
		t.Run(pattern, func(t *testing.T) {
			g, stub, _ := crdTestSystem(t, uc03Coverage())
			declareOriginationProfile(g, "demo")
			rec, recs := serveRecorded(t, g, pattern, big)
			if rec.Code != http.StatusRequestEntityTooLarge || len(stub.legTypes) != 0 {
				t.Fatalf("%s = %d %s legs %v, want 413 and no leg", pattern, rec.Code, rec.Body.String(), stub.legTypes)
			}
			r := onlyRecord(t, recs)
			wantOrigination(t, r, pattern, http.StatusRequestEntityTooLarge)
			wantRefusal(t, r, http.StatusRequestEntityTooLarge, RefusedByProviderGateway, RefusalRouting)
			if r.RefusalReason != string(reasonRequestTooLarge) {
				t.Fatalf("reason %q, want %q", r.RefusalReason, reasonRequestTooLarge)
			}
			wantReasonInAnswer(t, r, rec.Body.String())
		})
	}
}
