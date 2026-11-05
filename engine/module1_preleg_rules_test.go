//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The refusals that stay before an origination's first leg:
// the caller's own request is malformed, the leg cannot be addressed, or a
// record the first leg itself carries cannot be supplied. Each names the rule
// it refuses on: routing for addressing, the fidelity of SHN's own build for a
// record the build cannot make faithfully, the fidelity of SHN's own edit at
// the verify stage for the fill fence. A system of record that fails to answer
// is a backend failure, never a refusal.

// REJECTION, at every level: a `wait` that is not a whole number of seconds is
// the request to this gateway malformed, refused 400 before the first leg on a
// route that follows the decision for it. A route that reads no wait carries
// the request whatever its wait says.
func TestOrigination_MalformedWaitRefusedBeforeAnyLeg(t *testing.T) {
	for _, level := range allLevels {
		t.Run("uc06 single call/"+level.String(), func(t *testing.T) {
			gw, stub := uc06RouteFixture(t, "2.0")
			gw.cfg.ConformanceEnforcement = level
			rec := httptest.NewRecorder()
			gw.handleUC06(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc06?wait=soon", strings.NewReader(testRouteBody(gw, "/scenario/uc06", "")))))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "wait must be a whole number of seconds") || len(stub.legTypes) != 0 {
				t.Fatalf("%d %s legs %v, want 400 and no leg", rec.Code, rec.Body, stub.legTypes)
			}
		})
		t.Run("dispatch/"+level.String(), func(t *testing.T) {
			fix := newMBROXDispatchFixture(t)
			fix.gw.cfg.ConformanceEnforcement = level
			rec := httptest.NewRecorder()
			fix.gw.handleDispatch(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/dispatch?wait=1.5", strings.NewReader(testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`)))))
			if rec.Code != http.StatusBadRequest || len(fix.stub.legTypes) != 0 {
				t.Fatalf("%d %s legs %v, want 400 and no leg", rec.Code, rec.Body, fix.stub.legTypes)
			}
		})
		t.Run("uc02 reads no wait/"+level.String(), func(t *testing.T) {
			g, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededNoAuth})
			declareOriginationProfile(g, "demo")
			aimStubAt(t, stub, "MBR-D-UC02")
			g.cfg.ConformanceEnforcement = level
			rec := httptest.NewRecorder()
			g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc02?wait=soon", strings.NewReader(testRouteBody(g, "/scenario/uc02", ""))))
			if rec.Code != http.StatusOK || !legAttempted(stub.legTypes, "crd-order-select") {
				t.Fatalf("%d %s legs %v, want the coverage check answered", rec.Code, rec.Body, stub.legTypes)
			}
		})
	}
}

// unknownPatients is a system of record that holds no patient for any member.
type unknownPatients struct{ SystemOfRecord }

func (unknownPatients) ResolvePatient(string) (string, Demo, bool) { return "", Demo{}, false }

// REJECTION, at every level: a member the participant's system does not hold
// cannot be addressed: a routing refusal, 400, before any leg.
func TestOrigination_UnknownMemberIsRouting(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			g, stub := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			g.cfg.ConformanceEnforcement = level
			g.cfg.SoR = unknownPatients{g.cfg.SoR}
			const route = "POST /scenario/uc04"
			rec, recs := serveRecorded(t, g, route, testRouteBody(g, "/scenario/uc04", ""))
			if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Body.String(), `{"error":"unknown member. `) || len(stub.legTypes) != 0 {
				t.Fatalf("%s = %d %s legs %v, want 400 unknown member and no leg", route, rec.Code, rec.Body, stub.legTypes)
			}
			wantCarried(t, rec.Body.String())
			xr := onlyRecord(t, recs)
			wantRefusal(t, xr, http.StatusBadRequest, RefusedByProviderGateway, RefusalRouting)
			wantReasonInAnswer(t, xr, rec.Body.String())
		})
	}
}

// coverageWithoutPayer is a system of record whose member Coverage names its
// payer by neither an identifier nor a reference the network routes on.
type coverageWithoutPayer struct{ SystemOfRecord }

func (s coverageWithoutPayer) OpenCoverage(member string) ([]byte, bool) {
	raw, found := s.SystemOfRecord.OpenCoverage(member)
	if !found {
		return raw, found
	}
	return withoutPayer(member), true
}

func withoutPayer(member string) []byte {
	return []byte(`{"resourceType":"Coverage","id":"cov-x","status":"active","beneficiary":{"reference":"Patient/` + member + `"},"payor":[{"display":"a payer the record does not identify"}]}`)
}

// SearchPatientContext answers the Coverage search with the same Coverage the
// coverage read answers (coverageSearchRewritten).
func (s coverageWithoutPayer) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	res, err := s.SystemOfRecord.(SearchSystemOfRecord).SearchPatientContext(ctx, resourceType, id, dates...)
	if resourceType != "Coverage" {
		return res, err
	}
	return coverageSearchRewritten(res, err, func([]byte) []byte { return withoutPayer(id) })
}

// REJECTION, at every level: an origination whose payer cannot be addressed
// — this gateway configures no payer router, or the member's Coverage names
// no payer identifier — is a routing refusal, 422, before any leg.
func TestOrigination_UnaddressablePayerIsRouting(t *testing.T) {
	for _, row := range []struct {
		name  string
		setup func(*Gateway)
		want  string
	}{
		{"no payer router", func(g *Gateway) { g.cfg.PayerRouter = nil }, "no payer router configured"},
		{"no payer identifier", func(g *Gateway) { g.cfg.SoR = coverageWithoutPayer{g.cfg.SoR} }, noPayerIdentifier},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				g, stub := uc04RouteFixture(t, "2.0")
				mountOriginationRoutes(g)
				g.cfg.ConformanceEnforcement = level
				row.setup(g)
				const route = "POST /scenario/uc04"
				rec, recs := serveRecorded(t, g, route, testRouteBody(g, "/scenario/uc04", ""))
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), row.want) || len(stub.legTypes) != 0 {
					t.Fatalf("%s = %d %s legs %v, want 422 %q and no leg", route, rec.Code, rec.Body, stub.legTypes, row.want)
				}
				xr := onlyRecord(t, recs)
				wantRefusal(t, xr, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
				wantReasonInAnswer(t, xr, rec.Body.String())
			})
		}
	}
}

// REJECTION, at every level: a dispatched order of record with no id, or no
// performer, is a record the order-dispatch leg carries that SHN's build
// cannot make from it: the fidelity of SHN's own build, 422, before any leg.
func TestDispatch_OrderMissingIDOrPerformerIsBuildStage(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"no id":        func(m map[string]any) { delete(m, "id") },
		"no performer": func(m map[string]any) { delete(m, "performer") },
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				fix := newMBROXDispatchFixture(t)
				mountOriginationRoutes(fix.gw)
				fix.gw.cfg.ConformanceEnforcement = level
				fix.sor.orderJSON = editJSON(t, fix.sor.orderJSON, edit)
				const route = "POST /scenario/dispatch"
				rec, recs := serveRecorded(t, fix.gw, route, testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`))
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "open order missing id or performer") || len(fix.stub.legTypes) != 0 {
					t.Fatalf("%s = %d %s legs %v, want 422 and no leg", route, rec.Code, rec.Body, fix.stub.legTypes)
				}
				wantCarried(t, rec.Body.String())
				r := onlyRecord(t, recs)
				wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
				if r.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
				}
			})
		}
	}
}

// originRun drives runCRDThenDTROrder on g for prefetchMember behind an
// exchange record, with the tracker a route gives its call.
func originRun(t *testing.T, g *Gateway) (*httptest.ResponseRecorder, *exchangeRecorder) {
	t.Helper()
	x := &exchangeRecorder{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(testStatedBody(g, prefetchMember, "", false)))
	req = withLegsTracked(req.WithContext(context.WithValue(req.Context(), exchangeRecorderKey{}, x)))
	g.runCRDThenDTROrder(&exchangeWriter{ResponseWriter: rec, x: x}, req, prefetchMember, "", "", "", "", false)
	return rec, x
}

// REJECTION, at every level: an order the system of record names the patient
// in by its own id, which SHN's build cannot re-key to the member (the record
// repeats a member name, so it reads two ways), is refused before the CRD leg
// that carries it: the fidelity of SHN's own build, 422.
func TestOrigination_OrderNamingFailureIsBuildStage(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sor, _ := originSystem(t, "p-1")
			sor.order = bytes.Replace(sor.order, []byte(`"intent"`), []byte(`"status" : "active", "intent"`), 1)
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				t.Fatal("a leg was sent")
				return 0, nil
			})
			g.cfg.ConformanceEnforcement = level
			rec, x := originRun(t, g)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "name the patient in the open order") {
				t.Fatalf("answer %d %s, want 422 naming the order", rec.Code, rec.Body)
			}
			payer.mu.Lock()
			defer payer.mu.Unlock()
			if len(payer.requests) != 0 {
				t.Fatalf("%d legs sent", len(payer.requests))
			}
			if x.outcome != ExchangeRefused || x.rule != RefusalFidelity || x.refusalStage != vocab.RefusalStageBuild {
				t.Fatalf("recorded %q/%q stage %q, want fidelity at build", x.outcome, x.rule, x.refusalStage)
			}
		})
	}
}

// REJECTION, at every level: the CRD request's records answered about
// another patient (the Coverage search holds another patient's Coverage) are
// refused by the fill fence before the leg that would carry them: the
// fidelity of SHN's own edit, at the verify stage, 422.
func TestOrigination_PatientFenceIsVerifyStage(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sor, _ := originSystem(t, prefetchMember)
			_, other, _ := originRecords("someone-else")
			sor.answer(t, "Coverage", []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"https://sor.example/fhir/Coverage/cov-1","resource":`+string(other)+`,"search":{"mode":"match"}}]}`))
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				t.Fatal("a leg was sent")
				return 0, nil
			})
			g.cfg.ConformanceEnforcement = level
			rec, x := originRun(t, g)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), fillFencedOtherPatient) {
				t.Fatalf("answer %d %s, want 422 %q", rec.Code, rec.Body, fillFencedOtherPatient)
			}
			payer.mu.Lock()
			defer payer.mu.Unlock()
			if len(payer.requests) != 0 {
				t.Fatalf("%d legs sent", len(payer.requests))
			}
			if x.outcome != ExchangeRefused || x.rule != RefusalFidelity || x.refusalStage != vocab.RefusalStageVerify {
				t.Fatalf("recorded %q/%q stage %q, want fidelity at verify", x.outcome, x.rule, x.refusalStage)
			}
		})
	}
}

// REJECTION, at every level: a payer that declares no framed DTR operations
// cannot be sent the questionnaire request: a routing refusal, 422, at that
// leg, after the CRD leg, which was carried and stands.
func TestOrigination_UnframedDTRPayerIsRouting(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sor, _ := originSystem(t, prefetchMember)
			g, payer := originGateway(t, "provider-data", sor, nil, func(_ string, _ []byte) (int, []byte) {
				return 0, payerAnswerFor(t, sor.order, shnsdk.CoveredCovered, shnsdk.PANeededAuthNeeded, "http://example.org/Questionnaire/q")
			})
			entry, _ := g.cfg.Reg.Lookup("payer")
			entry.RequestFrames = nil
			g.cfg.Reg.Set("payer", entry)
			g.cfg.ConformanceEnforcement = level
			rec, x := originRun(t, g)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), shnsdk.ErrFramedDTRUnsupported.Error()) {
				t.Fatalf("answer %d %s, want 422 naming the unframed payer", rec.Code, rec.Body)
			}
			payer.mu.Lock()
			defer payer.mu.Unlock()
			if len(payer.requests) != 1 {
				t.Fatalf("%d legs sent, want the CRD leg alone", len(payer.requests))
			}
			wantCarried(t, rec.Body.String(), "crd-order-select")
			if x.outcome != ExchangeRefused || x.rule != RefusalRouting {
				t.Fatalf("recorded %q/%q, want routing", x.outcome, x.rule)
			}
		})
	}
}

// REJECTION, at every level: UC-07's /complete refuses before the patient-DTR
// exchange what that exchange cannot be sent with: an answer that is no score (400, the
// request malformed), no patient health gateway to carry it (routing, 422),
// and a resume token naming no pending scenario (404).
func TestUC07Complete_ThePatientLegsOwnChecks(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) { testUC07CompletePatientLegChecks(t, level) })
	}
}

func testUC07CompletePatientLegChecks(t *testing.T, level ConformanceEnforcement) {
	start := func(t *testing.T, gw *Gateway) string {
		t.Helper()
		rec := httptest.NewRecorder()
		gw.handleUC07Start(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc07/start", strings.NewReader(testRouteBody(gw, "/scenario/uc07/start", "")))))
		var started startResp
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
			t.Fatalf("start %d %s", rec.Code, rec.Body)
		}
		return started.ResumeToken
	}
	t.Run("no such pending scenario", func(t *testing.T) {
		gw, stub := uc07RouteFixture(t, "2.0")
		gw.cfg.ConformanceEnforcement = level
		before := len(stub.legTypes)
		rec := httptest.NewRecorder()
		gw.handleUC07Complete(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc07/complete",
			strings.NewReader(testRouteBody(gw, "/scenario/uc07/complete", `"resumeToken":"no-such-token"`)))))
		if rec.Code != http.StatusNotFound || len(stub.legTypes) != before {
			t.Fatalf("%d %s legs %v, want 404 and no leg", rec.Code, rec.Body, stub.legTypes[before:])
		}
	})
	t.Run("an answer that is no score", func(t *testing.T) {
		gw, stub := uc07RouteFixture(t, "2.0")
		gw.cfg.ConformanceEnforcement = level
		if gw.attestsOnHHA() {
			t.Fatal("the fixture's lane attests a free-text item; this row needs the Oswestry score")
		}
		token := start(t, gw)
		before := len(stub.legTypes)
		rec := httptest.NewRecorder()
		gw.handleUC07Complete(rec, withOriginationClaimInput(withLegsTracked(httptest.NewRequest(http.MethodPost, "/scenario/uc07/complete",
			strings.NewReader(testRouteBody(gw, "/scenario/uc07/complete", `"resumeToken":"`+token+`","answer":"not-a-score"`))))))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid patient answer") || len(stub.legTypes) != before {
			t.Fatalf("%d %s legs %v, want 400 and no leg", rec.Code, rec.Body, stub.legTypes[before:])
		}
		wantCarried(t, rec.Body.String())
	})
	// On the single call the answer is checked at the patient questionnaire
	// exchange that consumes it, after the submission the payer pended: those
	// legs were carried and stand, and the refusal says so.
	t.Run("an answer that is no score, single call", func(t *testing.T) {
		gw, stub := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad", pendedItem: "patient-reported-functional-status",
			extraRoles: map[string]string{"phg": "phg"}})
		gw.cfg.ConformanceEnforcement = level
		rec := httptest.NewRecorder()
		gw.handleUC07(rec, withOriginationClaimInput(withLegsTracked(httptest.NewRequest(http.MethodPost, "/scenario/uc07",
			strings.NewReader(testRouteBody(gw, "/scenario/uc07", `"answer":"not-a-score"`))))))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid patient answer") {
			t.Fatalf("%d %s, want 400", rec.Code, rec.Body)
		}
		if !legAttempted(stub.legTypes, "pas-claim") || legAttempted(stub.legTypes, "patient-dtr") || legAttempted(stub.legTypes, "pas-claim-update") {
			t.Fatalf("legs %v: want the submission sent and no patient-DTR exchange or amendment", stub.legTypes)
		}
		wantCarried(t, rec.Body.String(), stub.legTypes...)
	})
	t.Run("no PHG", func(t *testing.T) {
		gw, stub := routeFixtureAtLine(t, pendFixtureOpts{member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad", pendedItem: "patient-reported-functional-status"}, "2.0")
		gw.cfg.ConformanceEnforcement = level
		mountOriginationRoutes(gw)
		token := start(t, gw)
		before := len(stub.legTypes)
		const route = "POST /scenario/uc07/complete"
		rec, recs := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc07/complete", `"resumeToken":"`+token+`"`))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no PHG registered") || len(stub.legTypes) != before {
			t.Fatalf("%d %s legs %v, want 422 and no leg", rec.Code, rec.Body, stub.legTypes[before:])
		}
		wantCarried(t, rec.Body.String())
		xr := onlyRecord(t, recs)
		wantRefusal(t, xr, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
		wantReasonInAnswer(t, xr, rec.Body.String())
	})
}

// editJSON applies edit to the JSON object raw.
func editJSON(t *testing.T, raw []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
