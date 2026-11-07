//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The rule these rows hold: a request that can still succeed
// is carried, and a refusal sits at the build of the first leg that cannot
// succeed — the leg that consumes what is missing. Each refusal moved there
// says which legs the request already sent: they were carried, and they stand.

// answeredLegs records, on gw, each leg answered 2xx, in order: the legs a
// refusal written after them must name as carried.
type answeredLegs struct {
	mu   sync.Mutex
	legs []string
}

func recordAnsweredLegs(gw *Gateway) *answeredLegs {
	a := &answeredLegs{}
	observe := gw.cfg.Observer
	gw.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == "leg.response" && e.Direction == "originate" && (e.Status == 0 || e.Status/100 == 2) {
			a.mu.Lock()
			a.legs = append(a.legs, e.LegType)
			a.mu.Unlock()
		}
		if observe != nil {
			observe(e)
		}
	}
	return a
}

func (a *answeredLegs) list() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.legs...)
}

// refusedAtThePASBuild drives every module 1 site that sends a pas-claim leg
// (legSiteRows) at every level with setup applied to its gateway, and requires
// a 422 naming want, the pas-claim leg never sent, one leg.failed for it naming
// want, and an answer saying the legs before it — at least the CRD leg — were
// carried.
func refusedAtThePASBuild(t *testing.T, want string, setup func(*testing.T, *Gateway)) {
	t.Helper()
	var rows int
	for _, row := range legSiteRows {
		if row.leg != "pas-claim" {
			continue
		}
		rows++
		for _, level := range allLevels {
			t.Run(row.site+"/"+level.String(), func(t *testing.T) {
				rec := httptest.NewRecorder()
				var answered *answeredLegs
				f := row.run(t, rec, func(gw *Gateway) *legFault {
					setup(t, gw)
					gw.cfg.ConformanceEnforcement = level
					f := failLeg(gw, row.leg)
					answered = recordAnsweredLegs(gw)
					return f
				})
				f.mu.Lock()
				defer f.mu.Unlock()
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
					t.Fatalf("answer %d %s, want 422 %q", rec.Code, rec.Body, want)
				}
				if f.hits != 0 {
					t.Fatalf("the pas-claim leg was sent %d times", f.hits)
				}
				if len(f.failed) != 1 || f.failed[0].Detail != want {
					t.Fatalf("leg.failed for pas-claim = %+v, want one naming the refusal", f.failed)
				}
				legs := answered.list()
				if len(legs) == 0 || !strings.HasPrefix(legs[0], "crd-") {
					t.Fatalf("legs answered before the refusal %v, want the CRD leg first", legs)
				}
				wantCarried(t, rec.Body.String(), legs...)
			})
		}
	}
	if rows < 7 {
		t.Fatalf("%d pas-claim sites, want at least 7", rows)
	}
}

// orderRewrite rewrites the order a system of record holds, wherever it reads
// it (the open order of every lane, a dispatched order included).
type orderRewrite struct {
	SystemOfRecord
	edit func([]byte) []byte
}

func (s orderRewrite) OpenOrder(member string) ([]byte, bool) {
	order, found, err := ReadSystemOfRecord(s.SystemOfRecord).OpenOrderContext(context.Background(), member)
	if err != nil || !found {
		return order, found
	}
	return s.edit(order), true
}

func (s orderRewrite) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	return s.SystemOfRecord.(SearchSystemOfRecord).SearchPatientContext(ctx, resourceType, id, dates...)
}

// requesterNamed names ref as the order's requester.
func requesterNamed(t *testing.T, ref string) func([]byte) []byte {
	return func(order []byte) []byte {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(order, &m); err != nil {
			t.Fatal(err)
		}
		m["requester"] = json.RawMessage(`{"reference":"` + ref + `"}`)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
}

const unknownRequesterRef = "Practitioner/prac-9"

const msgUnknownRequester = "the order's requester " + unknownRequesterRef + " is not in your system of record, so a prior-authorization request about this order cannot carry the clinician it names"

// REJECTION, at every level and every PAS site: an order whose requester the
// participant's system cannot supply is refused at the PAS claim's build, the
// first leg that carries the requester's records: 422, the fidelity of SHN's
// own build, after the CRD (and questionnaire) legs, which stand.
func TestOrigination_RequesterUnavailableRefusedAtThePASBuild(t *testing.T) {
	refusedAtThePASBuild(t, msgUnknownRequester, func(t *testing.T, gw *Gateway) {
		gw.cfg.SoR = orderRewrite{gw.cfg.SoR, requesterNamed(t, unknownRequesterRef)}
	})
}

// RELAY, at every level: the same order on a request the payer ends
// not-covered at CRD needs no PAS leg, so the requester's records are never
// read and the payer's answer reaches the caller.
func TestOrigination_RequesterUnavailableNotCoveredIsCarried(t *testing.T) {
	_, _, base := originRecords(prefetchMember)
	order := bytes.Replace(base, []byte(`"intent" : "order",`), []byte(`"intent" : "order", "requester" : { "reference" : "`+unknownRequesterRef+`" },`), 1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sor, _ := originSystem(t, prefetchMember)
			sor.order = order
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				return 0, payerAnswerFor(t, order, shnsdk.CoveredNotCovered, "", "")
			})
			g.cfg.ConformanceEnforcement = level
			rec := httptest.NewRecorder()
			g.runCRDThenDTROrder(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(testStatedBody(g, prefetchMember, "", false))), prefetchMember, "", "", "", "", false)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"outcome":"not-covered"`) {
				t.Fatalf("answer %d %s, want 200 not-covered", rec.Code, rec.Body)
			}
			payer.mu.Lock()
			defer payer.mu.Unlock()
			if len(payer.requests) != 1 {
				t.Fatalf("%d legs sent, want the CRD leg alone", len(payer.requests))
			}
		})
	}
}

// payorNaming is a system of record whose member Coverage read, the one
// origination routes on, names its payer by the identifier the route reads
// and by ref, a reference the system's records do not resolve (an
// Organization it does not hold, or a contained one the Coverage lacks).
type payorNaming struct {
	SystemOfRecord
	ref string
}

func (s payorNaming) OpenCoverage(member string) ([]byte, bool) {
	raw, found := s.SystemOfRecord.OpenCoverage(member)
	if !found {
		return raw, found
	}
	return s.named(raw), found
}

// named is coverage naming its payer by s.ref, with the payer's identifier.
func (s payorNaming) named(raw []byte) []byte {
	payer, err := shnsdk.ParseCoveragePayer(raw, s.ResolveByReference)
	if err != nil {
		return raw
	}
	var coverage map[string]any
	if err := json.Unmarshal(raw, &coverage); err != nil {
		return raw
	}
	coverage["payor"] = []any{map[string]any{"reference": s.ref, "identifier": map[string]any{"system": payer.System, "value": payer.Value}}}
	out, err := json.Marshal(coverage)
	if err != nil {
		return raw
	}
	return out
}

func (s payorNaming) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	res, err := s.SystemOfRecord.(SearchSystemOfRecord).SearchPatientContext(ctx, resourceType, id, dates...)
	if resourceType != "Coverage" {
		return res, err
	}
	return coverageSearchRewritten(res, err, s.named)
}

// REJECTION, at every level and every PAS site: a Coverage naming a payer
// Organization the participant's system cannot supply — one it does not hold,
// or a contained one the Coverage lacks — is refused at the PAS claim's build,
// the first leg that carries that record (the claim's insurer), 422, the
// fidelity of SHN's own build, after the CRD and questionnaire legs, which
// stand. The questionnaire request carries the record only when the system
// supplies it.
func TestOrigination_PayerRecordUnavailableRefusedAtThePASBuild(t *testing.T) {
	for _, row := range []struct{ name, ref, want string }{
		{"not in the system", "Organization/missing-payer", "the system of record has no payer organization Organization/missing-payer"},
		{"contained and missing", "#missing-payer", "the member's coverage names contained payer organization #missing-payer and does not contain it"},
	} {
		t.Run(row.name, func(t *testing.T) {
			refusedAtThePASBuild(t, row.want, func(t *testing.T, gw *Gateway) {
				gw.cfg.SoR = payorNaming{gw.cfg.SoR, row.ref}
			})
		})
	}
}

// refReadFails is a system of record whose read of ref fails (unavailable),
// or, when missing, finds nothing: every other read is the inner system's.
type refReadFails struct {
	SystemOfRecord
	ref     string
	missing bool
}

func (s refReadFails) reader() ContextSystemOfRecord { return ReadSystemOfRecord(s.SystemOfRecord) }

func (s refReadFails) ResolvePatientContext(ctx context.Context, k string) (string, Demo, bool, error) {
	return s.reader().ResolvePatientContext(ctx, k)
}
func (s refReadFails) PatientFHIRRefContext(ctx context.Context, k string) (string, bool, error) {
	return s.reader().PatientFHIRRefContext(ctx, k)
}
func (s refReadFails) CoverageInforceContext(ctx context.Context, k string) (bool, string, error) {
	return s.reader().CoverageInforceContext(ctx, k)
}
func (s refReadFails) ClinicalContextContext(ctx context.Context, k string) (shnsdk.ClinicalContext, bool, error) {
	return s.reader().ClinicalContextContext(ctx, k)
}
func (s refReadFails) SupplementalReportContext(ctx context.Context, k string) ([]byte, bool, error) {
	return s.reader().SupplementalReportContext(ctx, k)
}
func (s refReadFails) FacilityRecordsContext(ctx context.Context, k string) (map[string][]byte, bool, error) {
	return s.reader().FacilityRecordsContext(ctx, k)
}
func (s refReadFails) OpenOrderContext(ctx context.Context, k string) ([]byte, bool, error) {
	if s.ref == openOrderRead {
		return nil, false, &SoRReadError{Kind: SoRUnavailable}
	}
	return s.reader().OpenOrderContext(ctx, k)
}

// openOrderRead names, as refReadFails.ref, the read of the member's open
// order rather than a reference.
const openOrderRead = "the open order"

func (s refReadFails) OpenCoverageContext(ctx context.Context, k string) ([][]byte, error) {
	return s.reader().OpenCoverageContext(ctx, k)
}
func (s refReadFails) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	if ref == s.ref {
		if s.missing {
			return nil, false, nil
		}
		return nil, false, &SoRReadError{Kind: SoRUnavailable}
	}
	return s.reader().ResolveByReferenceContext(ctx, ref)
}
func (s refReadFails) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	return s.SystemOfRecord.(SearchSystemOfRecord).SearchPatientContext(ctx, resourceType, id, dates...)
}

// backendFailureBeforeDTR drives every pas-claim site at every level with
// setup applied, and requires the system of record's failure answered as it
// stands (status, a backend failure: no leg.failed for the claim, never a
// refusal) at the questionnaire leg's build, the first that carries the
// record: the CRD leg alone was carried, named so, and neither the
// questionnaire request nor the claim was sent.
func backendFailureBeforeDTR(t *testing.T, status int, setup func(*testing.T, *Gateway)) {
	t.Helper()
	var rows int
	defer func() {
		if rows < 7 {
			t.Fatalf("%d pas-claim sites, want at least 7", rows)
		}
	}()
	for _, row := range legSiteRows {
		if row.leg != "pas-claim" {
			continue
		}
		rows++
		for _, level := range allLevels {
			t.Run(row.site+"/"+level.String(), func(t *testing.T) {
				rec := httptest.NewRecorder()
				var answered *answeredLegs
				f := row.run(t, rec, func(gw *Gateway) *legFault {
					setup(t, gw)
					gw.cfg.ConformanceEnforcement = level
					f := failLeg(gw, row.leg)
					answered = recordAnsweredLegs(gw)
					return f
				})
				f.mu.Lock()
				defer f.mu.Unlock()
				if rec.Code != status {
					t.Fatalf("answer %d %s, want %d", rec.Code, rec.Body, status)
				}
				if f.hits != 0 || len(f.failed) != 0 {
					t.Fatalf("pas-claim sent %d times, %d failures recorded: want neither", f.hits, len(f.failed))
				}
				legs := answered.list()
				if len(legs) != 1 || !strings.HasPrefix(legs[0], "crd-") {
					t.Fatalf("legs answered before the failure %v, want the CRD leg alone", legs)
				}
				wantCarried(t, rec.Body.String(), legs...)
			})
		}
	}
}

// The system of record failing to answer for the payer's Organization record
// is a backend failure, never a refusal and never a record left out (unknown
// is not empty): answered 503 at the questionnaire request's build, the first
// leg that carries the record, after the CRD leg, which stands.
func TestOrigination_PayerRecordReadFailureIsABackendFailureBeforeDTR(t *testing.T) {
	backendFailureBeforeDTR(t, http.StatusServiceUnavailable, func(t *testing.T, gw *Gateway) {
		gw.cfg.SoR = refReadFails{payorNaming{gw.cfg.SoR, "Organization/unreadable-payer"}, "Organization/unreadable-payer", false}
	})
}

// The dispatched order's supplier is a record only the PAS claim carries: the
// order-dispatch leg names the performer by reference. A supplier the system
// does not hold is refused at the claim's build, 422, the fidelity of SHN's
// own build; the system failing to answer for it is a backend failure, 503.
// Either way the order-dispatch and questionnaire legs were carried and stand,
// at every level.
func TestDispatch_SupplierRecordAtThePASBuild(t *testing.T) {
	for _, row := range []struct {
		name   string
		setup  func(*dispatchFixture)
		status int
		want   string
	}{
		{"not in the system", func(fix *dispatchFixture) {
			fix.gw.cfg.SoR = refReadFails{fix.gw.cfg.SoR, "Organization/org-dme-ox", true}
		}, http.StatusUnprocessableEntity,
			"the system of record has no supplier record Organization/org-dme-ox for the order's performer"},
		{"the read fails", func(fix *dispatchFixture) {
			fix.gw.cfg.SoR = refReadFails{fix.gw.cfg.SoR, "Organization/org-dme-ox", false}
		}, http.StatusServiceUnavailable, ""},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				fix := newMBROXDispatchFixture(t)
				mountOriginationRoutes(fix.gw)
				fix.gw.cfg.ConformanceEnforcement = level
				row.setup(fix)
				const route = "POST /scenario/dispatch"
				rec, recs := serveRecorded(t, fix.gw, route, testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`))
				if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.want) {
					t.Fatalf("%s = %d %s, want %d %q", route, rec.Code, rec.Body, row.status, row.want)
				}
				if !legAttempted(fix.stub.legTypes, "crd-order-dispatch") || legAttempted(fix.stub.legTypes, "pas-claim") {
					t.Fatalf("legs %v: want the order-dispatch leg sent and no claim", fix.stub.legTypes)
				}
				wantCarried(t, rec.Body.String(), fix.stub.legTypes...)
				r := onlyRecord(t, recs)
				if row.status == http.StatusUnprocessableEntity {
					wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
					if r.RefusalStage != vocab.RefusalStageBuild {
						t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
					}
					return
				}
				if r.Outcome != ExchangeUpstreamError {
					t.Fatalf("a read failure recorded %s on %q, want an upstream error: the system failed to answer, never a refusal", r.Outcome, r.Rule)
				}
			})
		}
	}
}

// withoutProductCoding replaces the order's product codings with one no PAS
// item can name.
func withoutProductCoding(t *testing.T) func([]byte) []byte {
	return func(order []byte) []byte {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(order, &m); err != nil {
			t.Fatal(err)
		}
		coded := json.RawMessage(`{"coding":[{"system":"http://snomed.info/sct","code":"123456","display":"not a product code"}]}`)
		for _, k := range []string{"code", "codeCodeableConcept"} {
			if _, ok := m[k]; ok {
				m[k] = coded
			}
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
}

// REJECTION, at every level: an order of record with no product coding the
// PAS item can name is carried by the CRD and questionnaire legs as the system
// holds it, and refused at the claim's build, the first leg that names the
// product, 422, the fidelity of SHN's own build: on an order-select route
// (UC-06's start) and an order-dispatch route (/scenario/dispatch). The
// request states its facts for the order's product as the system held it
// before its coding went.
func TestOrigination_OrderWithoutProductCodingRefusedAtThePASBuild(t *testing.T) {
	const want = "the member's open order has no recognized product coding"
	for _, level := range allLevels {
		t.Run("uc06 start/"+level.String(), func(t *testing.T) {
			gw, stub := uc06RouteFixture(t, "2.0")
			gw.cfg.ConformanceEnforcement = level
			body := testRouteBody(gw, "/scenario/uc06/start", "")
			gw.cfg.SoR = orderRewrite{gw.cfg.SoR, withoutProductCoding(t)}
			rec := startUC06(gw, body)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("%d %s, want 422 %q", rec.Code, rec.Body, want)
			}
			if !legAttempted(stub.legTypes, "crd-order-select") || legAttempted(stub.legTypes, "pas-claim") {
				t.Fatalf("legs %v: want the coverage check sent and no claim", stub.legTypes)
			}
			wantCarried(t, rec.Body.String(), stub.legTypes...)
		})
		t.Run("dispatch/"+level.String(), func(t *testing.T) {
			fix := newMBROXDispatchFixture(t)
			mountOriginationRoutes(fix.gw)
			fix.gw.cfg.ConformanceEnforcement = level
			body := testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`)
			fix.gw.cfg.SoR = orderRewrite{fix.gw.cfg.SoR, withoutProductCoding(t)}
			rec, recs := serveRecorded(t, fix.gw, "POST /scenario/dispatch", body)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("%d %s, want 422 %q", rec.Code, rec.Body, want)
			}
			if !legAttempted(fix.stub.legTypes, "crd-order-dispatch") || legAttempted(fix.stub.legTypes, "pas-claim") {
				t.Fatalf("legs %v: want the order-dispatch leg sent and no claim", fix.stub.legTypes)
			}
			wantCarried(t, rec.Body.String(), fix.stub.legTypes...)
			r := onlyRecord(t, recs)
			wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
			if r.RefusalStage != vocab.RefusalStageBuild {
				t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
			}
		})
	}
}

// RELAY: UC-02's coverage check carries the member's draft order as the
// system holds it, and nothing it sends names the product apart from it, so a
// draft order with no product coding is carried and the payer's answer
// reaches the caller.
func TestUC02_DraftOrderWithoutProductCodingIsCarried(t *testing.T) {
	sor, _ := originSystem(t, prefetchMember)
	draft := `{"resourceType":"ServiceRequest","id":"sr-d","status":"draft","intent":"order","code":{"coding":[{"system":"http://snomed.info/sct","code":"123456"}]},"subject":{"reference":"Patient/` + prefetchMember + `"},"requester":{"reference":"Practitioner/dr-1"}}`
	sor.answer(t, "ServiceRequest", searchsetOf(matchOf(draft)))
	g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
		return 0, payerAnswerFor(t, []byte(draft), shnsdk.CoveredCovered, shnsdk.PANeededNoAuth, "")
	})
	rec := httptest.NewRecorder()
	g.originateNoPACRD(rec, httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s, want the payer's answer", rec.Code, rec.Body)
	}
	if sent := payer.sent("crd-order-select"); len(sent) != 1 || !bytes.Contains(decodeSent(t, sent[0]).Context.DraftOrders, []byte(draft)) {
		t.Fatalf("the draft order was not carried as the system holds it")
	}
}

// RELAY, at every level: the member a request names vouches only for the
// claim facts it states, which no leg of UC-01 (eligibility) or UC-02 (the
// coverage check) consumes, so neither checks it: a request naming another
// member, or none, gets the route's own answer.
func TestNoPASRoutes_DoNotCheckTheStatedMember(t *testing.T) {
	for _, level := range allLevels {
		for _, edit := range []struct {
			name string
			fn   func(map[string]json.RawMessage)
		}{
			{"no member", func(m map[string]json.RawMessage) { delete(m, "member") }},
			{"another member", func(m map[string]json.RawMessage) { m["member"] = json.RawMessage(`"MBR-SOMEONE-ELSE"`) }},
		} {
			t.Run(level.String()+"/uc02/"+edit.name, func(t *testing.T) {
				g, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededNoAuth})
				declareOriginationProfile(g, "demo")
				aimStubAt(t, stub, "MBR-D-UC02")
				g.cfg.ConformanceEnforcement = level
				rec := httptest.NewRecorder()
				g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc02", strings.NewReader(editBody(t, testRouteBody(g, "/scenario/uc02", ""), edit.fn))))
				if rec.Code != http.StatusOK || !legAttempted(stub.legTypes, "crd-order-select") {
					t.Fatalf("%d %s legs %v, want the coverage check answered", rec.Code, rec.Body, stub.legTypes)
				}
			})
			t.Run(level.String()+"/uc01/"+edit.name, func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				env.payerReturns(LegResult{Response: testResponse([]byte(`{"resourceType":"CoverageEligibilityResponse"}`))})
				rec := httptest.NewRecorder()
				env.originator.handleScenario(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc01", strings.NewReader(editBody(t, testRouteBody(env.originator, "/scenario/uc01", `"branch":"covered"`), edit.fn))))
				if rec.Code != http.StatusOK {
					t.Fatalf("%d %s, want the eligibility answer", rec.Code, rec.Body)
				}
			})
		}
	}
}

// The exchange record of a route whose payer Organization read the system
// failed to answer names an upstream error, never a refusal, at every level:
// the system did not answer, so what it would have said is unknown.
func TestOrigination_PayerRecordReadFailureIsRecordedAsUpstream(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			g, stub := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			g.cfg.ConformanceEnforcement = level
			g.cfg.SoR = refReadFails{payorNaming{g.cfg.SoR, "Organization/unreadable-payer"}, "Organization/unreadable-payer", false}
			const route = "POST /scenario/uc04"
			rec, recs := serveRecorded(t, g, route, testRouteBody(g, "/scenario/uc04", ""))
			if rec.Code != http.StatusServiceUnavailable || !slices.Equal(stub.legTypes, []string{"crd-order-select"}) {
				t.Fatalf("%s = %d %s legs %v, want 503 after the coverage check alone", route, rec.Code, rec.Body, stub.legTypes)
			}
			wantCarried(t, rec.Body.String(), stub.legTypes...)
			if r := onlyRecord(t, recs); r.Outcome != ExchangeUpstreamError {
				t.Fatalf("recorded %s on %q, want an upstream error", r.Outcome, r.Rule)
			}
		})
	}
}

// The requesting provider a PAS claim names is read at its build. An order
// whose provider the participant's records cannot supply is refused there,
// 422, the fidelity of SHN's own build; a read of that record the system
// failed to answer is a backend failure (503), recorded as an upstream error.
// Either way the coverage check and the questionnaire were carried and stand,
// at every level.
func TestOrigination_RequestingProviderAtThePASBuild(t *testing.T) {
	noProvider := func(t *testing.T) func([]byte) []byte {
		return func(order []byte) []byte {
			return editJSON(t, order, func(m map[string]any) {
				delete(m, "requester")
				m["performer"] = []any{map[string]any{"reference": "Organization/no-such-provider"}}
			})
		}
	}
	for _, row := range []struct {
		name   string
		setup  func(*testing.T, *Gateway)
		status int
		want   string
	}{
		{"not in the system", func(t *testing.T, g *Gateway) { g.cfg.SoR = orderRewrite{g.cfg.SoR, noProvider(t)} },
			http.StatusUnprocessableEntity, "the system of record has no requesting provider Organization/no-such-provider"},
		{"the read fails", func(t *testing.T, g *Gateway) {
			g.cfg.SoR = refReadFails{orderRewrite{g.cfg.SoR, noProvider(t)}, "Organization/no-such-provider", false}
		}, http.StatusServiceUnavailable, ""},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				g, stub := uc04RouteFixture(t, "2.0")
				mountOriginationRoutes(g)
				g.cfg.ConformanceEnforcement = level
				body := testRouteBody(g, "/scenario/uc04", "")
				row.setup(t, g)
				const route = "POST /scenario/uc04"
				rec, recs := serveRecorded(t, g, route, body)
				if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.want) {
					t.Fatalf("%s = %d %s, want %d %q", route, rec.Code, rec.Body, row.status, row.want)
				}
				if !legAttempted(stub.legTypes, "crd-order-select") || legAttempted(stub.legTypes, "pas-claim") {
					t.Fatalf("legs %v: want the coverage check sent and no claim", stub.legTypes)
				}
				wantCarried(t, rec.Body.String(), stub.legTypes...)
				r := onlyRecord(t, recs)
				if row.status == http.StatusUnprocessableEntity {
					wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
					if r.RefusalStage != vocab.RefusalStageBuild {
						t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
					}
					return
				}
				if r.Outcome != ExchangeUpstreamError {
					t.Fatalf("recorded %s on %q, want an upstream error", r.Outcome, r.Rule)
				}
			})
		}
	}
}

// The shared tail checks the member the request names when its caller says
// which member the route resolved (pasFollowInputs.statedMember): UC-04's
// provider-data lane hands it the route's member, so a request naming another
// is refused at the claim's build, 422, before the claim is sent.
func TestSubmitPASClaim_ChecksTheStatedMember(t *testing.T) {
	env := newInProcessExchange(t)
	const member = "MBR-COVERED"
	recs, status, msg, _ := env.originator.originCRDRecords(env.ctx, "crd-order-select", member)
	if status != 0 {
		t.Fatalf("originCRDRecords: %d %s", status, msg)
	}
	realCov, found, status, msg := env.originator.memberCoverage(env.ctx, member)
	if status != 0 || !found {
		t.Fatalf("memberCoverage: %d %v %s", status, found, msg)
	}
	realPayerOrg, status, msg, _ := env.originator.memberPayerOrganization(env.ctx, realCov)
	if status != 0 {
		t.Fatalf("memberPayerOrganization: %d %s", status, msg)
	}
	req := withLegsTracked(withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(testStatedBody(env.originator, "MBR-SOMEONE-ELSE", "", false)))))
	_, status, msg, err := env.originator.submitPASClaim(req.Context(), req, pasFollowInputs{pci: "pci-1", orderJSON: pasTailServiceRequest(), coverage: realCov, insurer: realPayerOrg,
		patientRef: "Patient/" + member, coverageRef: "Coverage/" + member, member: member, memberSystem: recs.memberSystem, payer: shnsdk.CMSPayerIdentity, recipient: env.payerID,
		statedMember: member})
	if err != nil || status != http.StatusUnprocessableEntity || !strings.Contains(msg, "this route originates for "+member+"; the request names MBR-SOMEONE-ELSE") {
		t.Fatalf("submitPASClaim = %d %q %v, want 422 naming the other member", status, msg, err)
	}
	if env.routeHitCount() != 0 {
		t.Fatal("the claim was sent")
	}
}

// REJECTION, at every level: UC-03's oxygen arm checks what its claim
// consumes before it attests item 6.1 (pasClaimSourcesAt), after the
// order-dispatch and questionnaire legs, which stand: a request naming another
// member is refused there, 422, with no claim sent.
func TestUC03Oxygen_ChecksTheClaimsSourcesBeforeTheAttestation(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			fix := uc03DemoOxygenFixture(t)
			fix.gw.cfg.ConformanceEnforcement = level
			body := editBody(t, testRouteBody(fix.gw, "/scenario/uc03", `"branch":""`), func(m map[string]json.RawMessage) { m["member"] = json.RawMessage(`"MBR-SOMEONE-ELSE"`) })
			rec := httptest.NewRecorder()
			fix.gw.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(body)))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "this route originates for MBR-D-UC03; the request names MBR-SOMEONE-ELSE") {
				t.Fatalf("%d %s, want 422 naming the other member", rec.Code, rec.Body)
			}
			if !legAttempted(fix.stub.legTypes, "crd-order-dispatch") || legAttempted(fix.stub.legTypes, "pas-claim") {
				t.Fatalf("legs %v: want the order-dispatch leg sent and no claim", fix.stub.legTypes)
			}
			wantCarried(t, rec.Body.String(), fix.stub.legTypes...)
		})
	}
}

// REJECTION, at every level: a report type stated malformed is refused at the
// in-flow amendment's build (UC-04, UC-05) even on a route bridged to the
// payer's line, which does not carry a report type across lines:
// the statement is the request's, and it is checked, never dropped unread.
func TestInFlowAmendment_MalformedReportTypeRefusedOnABridgedRoute(t *testing.T) {
	bad := func(m map[string]json.RawMessage) {
		m["reportType"] = json.RawMessage(`{"system":"http://loinc.org","code":"PY"}`)
	}
	for _, level := range allLevels {
		for _, site := range amendmentSites[:2] {
			t.Run(level.String()+"/"+site.name, func(t *testing.T) {
				gw, stub := routeFixtureAtLine(t, site.opts, "2.1")
				gw.cfg.ConformanceEnforcement = level
				entry, _ := gw.cfg.Reg.Lookup("payer")
				entry.ContractVersions = []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS22}
				gw.cfg.Reg.Set("payer", entry)
				gw.cfg.TranslateIGLines = true
				gw.cfg.EgressNativeLines = []string{"2.1"}
				gw.cfg.DeclaredContractVersions = []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}
				gw.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": NewLineFakeValidator("2.0"), "2.1": NewLineFakeValidator("2.1"), "2.2": NewLineFakeValidator("2.2")})
				rec, _ := serveAmendment(t, gw, site.path, site.serve, bad)
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "ReportType") {
					t.Fatalf("%d %s, want 422 naming ReportType", rec.Code, rec.Body.String())
				}
				if !legAttempted(stub.legTypes, "pas-claim") || legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("legs %v: want the submission sent and no amendment", stub.legTypes)
				}
			})
		}
	}
}

// refRecord is a system of record that resolves ref to record.
type refRecord struct {
	SystemOfRecord
	ref    string
	record []byte
}

func (s refRecord) ResolveByReference(ref string) ([]byte, bool) {
	if ref == s.ref {
		return s.record, true
	}
	return s.SystemOfRecord.ResolveByReference(ref)
}

func (s refRecord) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	return s.SystemOfRecord.(SearchSystemOfRecord).SearchPatientContext(ctx, resourceType, id, dates...)
}

// REJECTION, at every level: UC-06's single call whose order names a
// clinician (so its coverage check is built) whose records carry no NPI, with
// none stated or configured, is refused at its amendment, the leg that carries
// the attestation, 422, after the submission the payer pended, which stands.
func TestUC06SingleCall_AttestingClinicianRefusedAtTheAmendment(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			gw, stub := uc06RouteFixture(t, "2.0")
			gw.cfg.NPI, gw.cfg.ConformanceEnforcement = "", level
			body := testRouteBody(gw, "/scenario/uc06", "")
			gw.cfg.SoR = refRecord{orderRewrite{gw.cfg.SoR, requesterNamed(t, "Practitioner/prac-nonpi")}, "Practitioner/prac-nonpi",
				[]byte(`{"resourceType":"Practitioner","id":"prac-nonpi","name":[{"family":"NoNPI"}]}`)}
			rec := httptest.NewRecorder()
			gw.handleUC06(rec, withOriginationClaimInput(withLegsTracked(httptest.NewRequest(http.MethodPost, "/scenario/uc06", strings.NewReader(body)))))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), msgNoAttestingClinician) {
				t.Fatalf("%d %s, want 422 %q", rec.Code, rec.Body, msgNoAttestingClinician)
			}
			if !legAttempted(stub.legTypes, "pas-claim") || legAttempted(stub.legTypes, "pas-claim-update") {
				t.Fatalf("legs %v: want the submission sent and no amendment", stub.legTypes)
			}
			wantCarried(t, rec.Body.String(), stub.legTypes...)
		})
	}
}

// REJECTION: every route that follows a decision for the caller's `wait`
// refuses one that is not a whole number of seconds, 400, before its first
// leg, at every level.
func TestOrigination_MalformedWaitRefusedOnEveryRouteThatReadsIt(t *testing.T) {
	routes := map[string]func(*testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string){
		"uc03 bridge": func(t *testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string) {
			gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-BRIDGE-DEMO", birthDate: "1983-03-11", familyName: "Solberg-BridgeDemo", pendedItem: "operative-diagnostic-report"})
			routeIdentityTo(t, &gw.cfg, BridgeDemoPayerID)
			return gw, (*Gateway).handleUC03, `"branch":"bridge-demo"`, "/scenario/uc03"
		},
		"uc03 oxygen": func(t *testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string) {
			return uc03DemoOxygenFixture(t).gw, (*Gateway).handleUC03, `"branch":""`, "/scenario/uc03"
		},
		"uc07hcpcs": func(t *testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string) {
			gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC07HCPCS", birthDate: "1977-01-30", familyName: "Nakamura", pendedItem: "operative-diagnostic-report"})
			return gw, (*Gateway).handleUC07HCPCS, "", "/scenario/uc07hcpcs"
		},
		"uc04": func(t *testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string) {
			gw, _ := uc04RouteFixture(t, "2.0")
			return gw, (*Gateway).handleUC04, "", "/scenario/uc04"
		},
		"uc05": func(t *testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string) {
			return amendmentFixture(t, 1), (*Gateway).handleUC05, "", "/scenario/uc05"
		},
		"uc06": func(t *testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string) {
			gw, _ := uc06RouteFixture(t, "2.0")
			return gw, (*Gateway).handleUC06, "", "/scenario/uc06"
		},
		"uc07": func(t *testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string) {
			gw, _ := uc07RouteFixture(t, "2.0")
			return gw, (*Gateway).handleUC07, "", "/scenario/uc07"
		},
		"uc06 complete": func(t *testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string) {
			gw, _ := uc06RouteFixture(t, "2.0")
			return gw, (*Gateway).handleUC06Complete, `"resumeToken":"no-such-token"`, "/scenario/uc06/complete"
		},
		"uc07 complete": func(t *testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string) {
			gw, _ := uc07RouteFixture(t, "2.0")
			return gw, (*Gateway).handleUC07Complete, `"resumeToken":"no-such-token"`, "/scenario/uc07/complete"
		},
		"dispatch": func(t *testing.T) (*Gateway, func(*Gateway, http.ResponseWriter, *http.Request), string, string) {
			return newMBROXDispatchFixture(t).gw, (*Gateway).handleDispatch, `"member":"MBR-OX"`, "/scenario/dispatch"
		},
	}
	for name, route := range routes {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				gw, serve, extra, path := route(t)
				gw.cfg.ConformanceEnforcement = level
				var sent []string
				observe := gw.cfg.Observer
				gw.cfg.Observer = func(e ObserverEvent) {
					if e.Kind == "leg.originated" {
						sent = append(sent, e.LegType)
					}
					if observe != nil {
						observe(e)
					}
				}
				rec := httptest.NewRecorder()
				serve(gw, rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, path+"?wait=soon", strings.NewReader(testRouteBody(gw, path, extra)))))
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "wait must be a whole number of seconds") || len(sent) != 0 {
					t.Fatalf("%d %s legs %v, want 400 and no leg", rec.Code, rec.Body, sent)
				}
			})
		}
	}
}

// amendmentFixture is amendmentSites[i]'s gateway at line 2.0.
func amendmentFixture(t *testing.T, i int) *Gateway {
	t.Helper()
	gw, _ := routeFixtureAtLine(t, amendmentSites[i].opts, "2.0")
	return gw
}

// A read of the member's open order the system fails to answer, before the
// first leg, is a backend failure: 503, recorded as an upstream error, never
// a refusal, with no leg sent, at every level.
// coverageSearchAnswers is a system of record that answers the Coverage
// search with res and err: every other read is the inner system's. When
// readsBySearch, its coverage read (OpenCoverageContext, the routing read
// every origination makes first) runs that same search and reads its answer
// exactly as the built-in FHIR connector does (fhirsorCoverageRead), so a
// failed search fails the routing read first, as it does there; otherwise its
// coverage read is the inner system's own records, as a connector whose
// coverage read is not that search answers.
type coverageSearchAnswers struct {
	refReadFails
	res           SearchResult
	err           error
	readsBySearch bool
}

func (s coverageSearchAnswers) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	if resourceType == "Coverage" {
		return s.res, s.err
	}
	return s.refReadFails.SearchPatientContext(ctx, resourceType, id, dates...)
}

func (s coverageSearchAnswers) OpenCoverageContext(ctx context.Context, k string) ([][]byte, error) {
	if !s.readsBySearch {
		return s.refReadFails.OpenCoverageContext(ctx, k)
	}
	return fhirsorCoverageRead(s.res, s.err)
}

// fhirsorCoverageRead is the built-in FHIR connector's coverage read
// (connectors/fhirsor, OpenCoverageContext) of a Coverage search answered res
// and err: a search the server cannot answer is unavailable; any other failed
// search (over the bounds, unsupported, malformed) an invalid response, one
// repeating a member name saying so; else every matched Coverage, as sent.
func fhirsorCoverageRead(res SearchResult, err error) ([][]byte, error) {
	if err != nil {
		var se *SearchError
		switch {
		case errors.As(err, &se) && se.Outcome == SearchUnavailable:
			return nil, &SoRReadError{Kind: SoRUnavailable}
		case errors.Is(err, relay.ErrDuplicateKey):
			return nil, SoRRepeatedMember()
		}
		return nil, &SoRReadError{Kind: SoRInvalidResponse}
	}
	var out [][]byte
	for i, page := range res.Pages {
		parsed, err := ParseSearchPage(page, "Coverage")
		if err != nil {
			return nil, &SoRReadError{Kind: SoRInvalidResponse}
		}
		for j, r := range parsed.Resources {
			if parsed.Match[j] {
				out = append(out, res.Pages[i][r.Start:r.End])
			}
		}
	}
	return out, nil
}

// coverageSearchRewritten is res, a Coverage search's answer, with each
// matched Coverage rewritten as rewrite does it, so a double that rewrites its
// coverage read rewrites the search the built-in FHIR connector reads that
// coverage by too: the two never disagree, as they cannot behind it.
func coverageSearchRewritten(res SearchResult, err error, rewrite func([]byte) []byte) (SearchResult, error) {
	if err != nil {
		return res, err
	}
	out := SearchResult{}
	for i, page := range res.Pages {
		parsed, err := ParseSearchPage(page, "Coverage")
		if err != nil {
			return res, err
		}
		b := append([]byte(nil), page...)
		for j := len(parsed.Resources) - 1; j >= 0; j-- {
			if !parsed.Match[j] {
				continue
			}
			r := parsed.Resources[j]
			b = slices.Concat(b[:r.Start], rewrite(b[r.Start:r.End]), b[r.End:])
		}
		again, err := ParseSearchPage(b, "Coverage")
		if err != nil {
			return res, err
		}
		for _, e := range again.Entries {
			e.Page = i
			out.Entries = append(out.Entries, e)
		}
		out.Pages = append(out.Pages, b)
	}
	out.Total = len(out.Entries)
	return out, nil
}

// noSearchSoR is a system of record whose connector cannot search.
type noSearchSoR struct{ SystemOfRecord }

// The coverage a CRD leg carries is first needed at that leg, and every
// origination reads it first to route by. Behind the built-in FHIR connector
// both reads are the one Coverage search, so a failed search fails the
// routing read: a backend failure with its own status, recorded as one (an
// answer repeating a member name as message integrity), never a refusal,
// whatever the failure, an answer past the bounds included (the connector
// reads it as unreadable). Behind a connector whose coverage read is its own
// records, the search the CRD leg's coverage is read by fails at that leg's
// build: unknown answers (unavailable, unreadable, or an error answer that
// does not say the search cannot run) are the same backend failure; a
// connector that cannot search, a definite not-supported answer, or an
// answer past the gateway's bounds, is the system as connected definitely
// unable to supply it, so SHN cannot build the leg faithfully: 422, fidelity
// at the build stage. Behind the built-in FHIR connector the routing read
// still reads a definite answer as unreadable. No leg is sent, at every
// level.
func TestOrigination_CoverageSearchFailureBeforeTheCRDLeg(t *testing.T) {
	over := make([][]byte, SoRSearchMaxPages+1)
	for i := range over {
		over[i] = []byte(`{"resourceType":"Bundle","type":"searchset"}`)
	}
	repeated := &SearchError{Outcome: SearchMalformed, Reason: "repeated member name", cause: relay.ErrDuplicateKey}
	integrity := func(t *testing.T, r ExchangeRecord) {
		t.Helper()
		if r.Outcome != ExchangeRefused || r.Rule != RefusalIntegrity {
			t.Fatalf("recorded %s on %q, want refused on %s", r.Outcome, r.Rule, RefusalIntegrity)
		}
	}
	const unavailable, invalid = "system of record unavailable", "system of record returned an invalid response"
	for _, row := range []struct {
		name          string
		readsBySearch bool
		res           SearchResult
		err           error
		status        int
		want          string
		check         func(*testing.T, ExchangeRecord)
	}{
		// Behind the built-in FHIR connector: the routing read fails first.
		{"fhir connector/unavailable", true, SearchResult{}, &SearchError{Outcome: SearchUnavailable, Reason: "system of record unavailable"},
			http.StatusServiceUnavailable, unavailable, upstreamError},
		{"fhir connector/not a searchset", true, SearchResult{}, &SearchError{Outcome: SearchMalformed, Reason: "not a searchset"},
			http.StatusBadGateway, invalid, upstreamError},
		{"fhir connector/repeating a member name", true, SearchResult{}, repeated,
			http.StatusBadGateway, invalid, integrity},
		// The routing read (OpenCoverageContext) answers only the read errors
		// a ContextSystemOfRecord may return: a definite not-supported is
		// still read as an invalid response there.
		{"fhir connector/a definite not-supported answer", true, SearchResult{}, &SearchError{Outcome: SearchUnsupported, Reason: "search not supported"},
			http.StatusBadGateway, invalid, upstreamError},
		{"fhir connector/an error answer", true, SearchResult{}, &SearchError{Outcome: SearchFailed, Reason: "search answered with an error status"},
			http.StatusBadGateway, invalid, upstreamError},
		{"fhir connector/past the size bound", true, SearchResult{}, &SearchError{Outcome: SearchBound, Reason: "size bound"},
			http.StatusBadGateway, invalid, upstreamError},
		// Behind a connector whose coverage read is its own records: the
		// CRD leg's coverage search fails at that leg's build.
		{"own coverage read/unavailable", false, SearchResult{}, &SearchError{Outcome: SearchUnavailable, Reason: "down"},
			http.StatusServiceUnavailable, "coverage unavailable from system of record", upstreamError},
		{"own coverage read/an answer with no pages", false, SearchResult{}, nil,
			http.StatusBadGateway, "coverage unreadable from system of record", upstreamError},
		{"own coverage read/repeating a member name", false, SearchResult{}, repeated,
			http.StatusBadGateway, "coverage unreadable from system of record", integrity},
		{"own coverage read/an error answer", false, SearchResult{}, &SearchError{Outcome: SearchFailed, Reason: "search answered with an error status"},
			http.StatusBadGateway, coverageSearchAnsweredError, upstreamError},
		{"own coverage read/a definite not-supported answer", false, SearchResult{}, &SearchError{Outcome: SearchUnsupported, Reason: "search not supported"},
			http.StatusUnprocessableEntity, "the system of record cannot search for it", ownBuildRefusal},
		{"own coverage read/a connector that cannot search", false, SearchResult{}, errNoSearch,
			http.StatusUnprocessableEntity, "the system of record cannot search for it", ownBuildRefusal},
		{"own coverage read/a connector with no search", false, SearchResult{}, nil,
			http.StatusUnprocessableEntity, "the system of record cannot search for it", ownBuildRefusal},
		{"own coverage read/past the page bound", false, SearchResult{Pages: over}, nil,
			http.StatusUnprocessableEntity, "coverage unreadable from system of record", ownBuildRefusal},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				g, stub := uc04RouteFixture(t, "2.0")
				mountOriginationRoutes(g)
				g.cfg.ConformanceEnforcement = level
				body := testRouteBody(g, "/scenario/uc04", "")
				g.cfg.SoR = coverageSearchAnswers{refReadFails{SystemOfRecord: g.cfg.SoR}, row.res, row.err, row.readsBySearch}
				if strings.HasSuffix(row.name, "a connector with no search") {
					g.cfg.SoR = noSearchSoR{g.cfg.SoR.(coverageSearchAnswers).refReadFails}
				}
				const route = "POST /scenario/uc04"
				rec, recs := serveRecorded(t, g, route, body)
				if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.want) || len(stub.legTypes) != 0 {
					t.Fatalf("%s = %d %s legs %v, want %d %q and no leg", route, rec.Code, rec.Body, stub.legTypes, row.status, row.want)
				}
				wantCarried(t, rec.Body.String())
				row.check(t, onlyRecord(t, recs))
			})
		}
	}
}

func upstreamError(t *testing.T, r ExchangeRecord) {
	t.Helper()
	if r.Outcome != ExchangeUpstreamError {
		t.Fatalf("recorded %s on %q, want an upstream error: the system failed to answer, never a refusal", r.Outcome, r.Rule)
	}
}

func ownBuildRefusal(t *testing.T, r ExchangeRecord) {
	t.Helper()
	wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
	if r.RefusalStage != vocab.RefusalStageBuild {
		t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
	}
}

func TestOrigination_OpenOrderReadFailureIsABackendFailure(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			g, stub := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			g.cfg.ConformanceEnforcement = level
			body := testRouteBody(g, "/scenario/uc04", "")
			g.cfg.SoR = refReadFails{g.cfg.SoR, openOrderRead, false}
			const route = "POST /scenario/uc04"
			rec, recs := serveRecorded(t, g, route, body)
			if rec.Code != http.StatusServiceUnavailable || len(stub.legTypes) != 0 {
				t.Fatalf("%s = %d %s legs %v, want 503 and no leg", route, rec.Code, rec.Body, stub.legTypes)
			}
			wantCarried(t, rec.Body.String())
			if r := onlyRecord(t, recs); r.Outcome != ExchangeUpstreamError {
				t.Fatalf("recorded %s on %q, want an upstream error", r.Outcome, r.Rule)
			}
		})
	}
}

// The doubles that rewrite a member's coverage read rewrite the Coverage
// search to the same Coverage: behind the built-in FHIR connector the routing
// read and the coverage a CRD leg carries are one search, so a double whose
// two answers disagree is a stand-in the real connector cannot be.
func TestCoverageDoublesAgreeWithTheirSearch(t *testing.T) {
	g, _ := uc04RouteFixture(t, "2.0")
	const member = "MBR-UC04"
	for name, sor := range map[string]SystemOfRecord{
		"payorNaming":          payorNaming{g.cfg.SoR, "Organization/unreadable-payer"},
		"payorByIdentifier":    payorByIdentifier{g.cfg.SoR},
		"coverageWithoutPayer": coverageWithoutPayer{g.cfg.SoR},
	} {
		t.Run(name, func(t *testing.T) {
			read, found := sor.OpenCoverage(member)
			if !found {
				t.Fatal("no coverage read")
			}
			ref, _, err := ReadSystemOfRecord(sor).PatientFHIRRefContext(context.Background(), member)
			if err != nil {
				t.Fatal(err)
			}
			res, err := sor.(SearchSystemOfRecord).SearchPatientContext(context.Background(), "Coverage", strings.TrimPrefix(ref, "Patient/"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := fhirsorCoverageRead(res, nil)
			if err != nil || len(got) != 1 || !bytes.Equal(got[0], read) {
				t.Fatalf("search answers %q (%v), want the coverage read's %s", got, err, read)
			}
		})
	}
}

// patientReadFails is a system of record whose patient read fails
// (unavailable): every other read is the inner system's.
type patientReadFails struct{ refReadFails }

func (patientReadFails) ResolvePatientContext(context.Context, string) (string, Demo, bool, error) {
	return "", Demo{}, false, &SoRReadError{Kind: SoRUnavailable}
}

// clinicalReadFails is a system of record whose clinical context read fails
// (unavailable): every other read is the inner system's.
type clinicalReadFails struct{ refReadFails }

func (clinicalReadFails) ClinicalContextContext(context.Context, string) (shnsdk.ClinicalContext, bool, error) {
	return shnsdk.ClinicalContext{}, false, &SoRReadError{Kind: SoRUnavailable}
}

// supplementalReadFails is a system of record whose supplemental report read
// fails (unavailable): every other read is the inner system's.
type supplementalReadFails struct{ refReadFails }

func (supplementalReadFails) SupplementalReportContext(context.Context, string) ([]byte, bool, error) {
	return nil, false, &SoRReadError{Kind: SoRUnavailable}
}

// refReadsFailAfter is a system of record whose reads by reference fail
// (unavailable) once sent reports the leg it names sent: every other read,
// and those before, is the inner system's.
type refReadsFailAfter struct {
	refReadFails
	sent func() []string
	leg  string
}

func (s refReadsFailAfter) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	if legAttempted(s.sent(), s.leg) {
		return nil, false, &SoRReadError{Kind: SoRUnavailable}
	}
	return s.refReadFails.ResolveByReferenceContext(ctx, ref)
}

// patientRefFailsAfter is a system of record whose patient reference read
// fails (unavailable) once sent reports the leg it names sent: every other
// read, and that one before, is the inner system's.
type patientRefFailsAfter struct {
	refReadFails
	sent func() []string
	leg  string
}

func (s patientRefFailsAfter) PatientFHIRRefContext(ctx context.Context, k string) (string, bool, error) {
	if legAttempted(s.sent(), s.leg) {
		return "", false, &SoRReadError{Kind: SoRUnavailable}
	}
	return s.refReadFails.PatientFHIRRefContext(ctx, k)
}

// Every read of the participant's system an origination makes, before any
// leg or after some, that the system fails to answer is a backend failure:
// its own status (503 here), recorded as an upstream error, never a refusal,
// and saying which legs the request sent were carried, at every level.
func TestOrigination_EveryReadFailureIsABackendFailure(t *testing.T) {
	type drive struct {
		g     *Gateway
		route string
		body  string
		legs  func() []string // the legs the gateway sent, when the fixture keeps them
	}
	none := func() []string { return nil }
	for _, row := range []struct {
		name string
		// after: the legs the read comes after, which were carried.
		after []string
		setup func(t *testing.T) drive
	}{
		{"the patient, eligibility", nil, func(t *testing.T) drive {
			env := newInProcessExchange(t)
			g := env.originator
			mountOriginationRoutes(g)
			body := testRouteBody(g, "/scenario/uc01", `"branch":"covered"`)
			g.cfg.SoR = patientReadFails{refReadFails{SystemOfRecord: g.cfg.SoR}}
			return drive{g, "POST /scenario/uc01", body, none}
		}},
		{"the patient, no-PA coverage check", nil, func(t *testing.T) drive {
			env := newInProcessExchange(t)
			g := env.originator
			declareOriginationProfile(g, "demo")
			mountOriginationRoutes(g)
			body := testRouteBody(g, "/scenario/uc02", "")
			g.cfg.SoR = patientReadFails{refReadFails{SystemOfRecord: g.cfg.SoR}}
			return drive{g, "POST /scenario/uc02", body, none}
		}},
		{"the patient, order select", nil, func(t *testing.T) drive {
			g, stub := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			body := testRouteBody(g, "/scenario/uc04", "")
			g.cfg.SoR = patientReadFails{refReadFails{SystemOfRecord: g.cfg.SoR}}
			return drive{g, "POST /scenario/uc04", body, func() []string { return stub.legTypes }}
		}},
		{"the patient, dispatch", nil, func(t *testing.T) drive {
			fix := newMBROXDispatchFixture(t)
			mountOriginationRoutes(fix.gw)
			body := testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`)
			fix.gw.cfg.SoR = patientReadFails{refReadFails{SystemOfRecord: fix.gw.cfg.SoR}}
			return drive{fix.gw, "POST /scenario/dispatch", body, func() []string { return fix.stub.legTypes }}
		}},
		{"the open order, dispatch", nil, func(t *testing.T) drive {
			fix := newMBROXDispatchFixture(t)
			mountOriginationRoutes(fix.gw)
			body := testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`)
			fix.gw.cfg.SoR = refReadFails{fix.gw.cfg.SoR, openOrderRead, false}
			return drive{fix.gw, "POST /scenario/dispatch", body, func() []string { return fix.stub.legTypes }}
		}},
		{"the payer the coverage names by reference", nil, func(t *testing.T) drive {
			g, stub := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			body := testRouteBody(g, "/scenario/uc04", "")
			shaped := shapePayor(t, g.cfg.SoR, "MBR-UC04", "reference")
			g.cfg.SoR = refReadFails{shaped, shaped.ref, false}
			return drive{g, "POST /scenario/uc04", body, func() []string { return stub.legTypes }}
		}},
		{"UC-03's clinical context, after the order-dispatch and questionnaire legs", []string{"crd-order-dispatch", "dtr-questionnaire-fetch"}, func(t *testing.T) drive {
			fix := uc03DemoOxygenFixture(t)
			body := testRouteBody(fix.gw, "/scenario/uc03", `"branch":""`)
			fix.gw.cfg.SoR = clinicalReadFails{refReadFails{SystemOfRecord: fix.gw.cfg.SoR}}
			return drive{fix.gw, "POST /scenario/uc03", body, func() []string { return fix.stub.legTypes }}
		}},
		{"the clinical context the questionnaire is filled from, after the order-select and questionnaire legs", []string{"crd-order-select", "dtr-questionnaire-fetch"}, func(t *testing.T) drive {
			g, stub := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			body := testRouteBody(g, "/scenario/uc04", "")
			g.cfg.SoR = clinicalReadFails{refReadFails{SystemOfRecord: g.cfg.SoR}}
			g.paState().populator = newManagedPopulator(g.cfg.SoR)
			return drive{g, "POST /scenario/uc04", body, func() []string { return stub.legTypes }}
		}},
		{"the patient reference the pend is recorded with, after the claim the payer pended", []string{"crd-order-dispatch", "dtr-questionnaire-fetch", "pas-claim"}, func(t *testing.T) drive {
			fix := newMBROXDispatchFixture(t)
			mountOriginationRoutes(fix.gw)
			body := testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`)
			fix.stub.pendFor = "Patient/MBR-OX"
			fix.gw.cfg.SoR = patientRefFailsAfter{refReadFails{SystemOfRecord: fix.gw.cfg.SoR}, func() []string { return fix.stub.legTypes }, "pas-claim"}
			return drive{fix.gw, "POST /scenario/dispatch", body, func() []string { return fix.stub.legTypes }}
		}},
		{"the supplemental report the amendment attaches, after the claim the payer pended", []string{"crd-order-select", "dtr-questionnaire-fetch", "pas-claim"}, func(t *testing.T) drive {
			g, stub := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			body := testRouteBody(g, "/scenario/uc04", "")
			g.cfg.SoR = supplementalReadFails{refReadFails{SystemOfRecord: g.cfg.SoR}}
			return drive{g, "POST /scenario/uc04", body, func() []string { return stub.legTypes }}
		}},
		{"the evidence the claim links, after the order-select and questionnaire legs", []string{"crd-order-select", "dtr-questionnaire-fetch"}, func(t *testing.T) drive {
			const member = "MBR-PD-UC04"
			uc04 := DemoOrderCodes().UC04
			order := buildAuthoredQROrderWithID(t, "sr-uc04-evidence", "Patient/"+member, uc04.System, uc04.Code, uc04.Display, uc04.Dx)
			order = editJSON(t, order, func(m map[string]any) {
				m["reasonReference"] = []any{map[string]any{"reference": "Condition/unreadable"}}
			})
			g, stub, _ := newAuthoredQRSingleShotFixture(t, member, Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}, order)
			mountOriginationRoutes(g)
			g.cfg.SoR = refReadFails{g.cfg.SoR, "Condition/unreadable", false}
			return drive{g, "POST /scenario/uc04", testOrderBody(order, `"member":"`+member+`"`), func() []string { return stub.legTypes }}
		}},
		{"the order's supporting assessment, after the order-select and questionnaire legs", []string{"crd-order-select", "dtr-questionnaire-fetch"}, func(t *testing.T) drive {
			const member = "MBR-PD-UC04"
			uc04 := DemoOrderCodes().UC04
			order := buildAuthoredQROrderWithID(t, "sr-uc04-read", "Patient/"+member, uc04.System, uc04.Code, uc04.Display, uc04.Dx)
			order = editJSON(t, order, func(m map[string]any) {
				m["supportingInfo"] = []any{map[string]any{"reference": "ClinicalImpression/unreadable"}}
			})
			g, stub, _ := newAuthoredQRSingleShotFixture(t, member, Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}, order)
			mountOriginationRoutes(g)
			g.cfg.SoR = refReadFails{g.cfg.SoR, "ClinicalImpression/unreadable", false}
			return drive{g, "POST /scenario/uc04", testOrderBody(order, `"member":"`+member+`"`), func() []string { return stub.legTypes }}
		}},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				d := row.setup(t)
				d.g.cfg.ConformanceEnforcement = level
				rec, recs := serveRecorded(t, d.g, d.route, d.body)
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s = %d %s, want 503", d.route, rec.Code, rec.Body)
				}
				legs := d.legs()
				for _, leg := range row.after {
					if !legAttempted(legs, leg) {
						t.Fatalf("legs %v: want %s sent before the read", legs, leg)
					}
				}
				if len(row.after) == 0 && len(legs) != 0 || legAttempted(legs, "pas-claim") && !slices.Contains(row.after, "pas-claim") || legAttempted(legs, "pas-claim-update") {
					t.Fatalf("legs %v: want only %v", legs, row.after)
				}
				wantCarried(t, rec.Body.String(), legs...)
				if r := onlyRecord(t, recs); r.Outcome != ExchangeUpstreamError {
					t.Fatalf("recorded %s on %q, want an upstream error: the system failed to answer, never a refusal", r.Outcome, r.Rule)
				}
			})
		}
	}
}

// refReadsFailWhen is a system of record whose reads by reference fail
// (unavailable) once *on: every other read, and those before, is the inner
// system's.
type refReadsFailWhen struct {
	refReadFails
	on *bool
}

func (s refReadsFailWhen) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	if *s.on {
		return nil, false, &SoRReadError{Kind: SoRUnavailable}
	}
	return s.refReadFails.ResolveByReferenceContext(ctx, ref)
}

// Continuing a pended authorization later (POST /scenario/pa/inquire) reads
// the order, the patient, the coverage and the payer's records again before
// its inquiry leg: a read the system fails to answer is a backend failure,
// its own status (503 here), recorded as one, and says no leg was sent, at
// every level.
func TestPAInquire_ReadFailureIsABackendFailure(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			fix := newMBROXDispatchFixture(t)
			mountOriginationRoutes(fix.gw)
			fix.gw.cfg.ConformanceEnforcement = level
			fix.stub.pendFor = "Patient/MBR-OX"
			on := false
			fix.gw.cfg.SoR = refReadsFailWhen{refReadFails{SystemOfRecord: fix.gw.cfg.SoR}, &on}
			rec, _ := serveRecorded(t, fix.gw, "POST /scenario/dispatch?wait=0", testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`))
			var pend struct {
				Continuation string `json:"continuation"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &pend) != nil || pend.Continuation == "" {
				t.Fatalf("dispatch %d %s, want the pend and its continuation", rec.Code, rec.Body)
			}
			on = true
			before := len(fix.stub.legTypes)
			rec, recs := serveRecorded(t, fix.gw, "POST /scenario/pa/inquire", `{"continuation":"`+pend.Continuation+`","waitSeconds":0}`)
			if rec.Code != http.StatusServiceUnavailable || len(fix.stub.legTypes) != before {
				t.Fatalf("inquire = %d %s legs %v, want 503 and no inquiry", rec.Code, rec.Body, fix.stub.legTypes[before:])
			}
			wantCarried(t, rec.Body.String())
			if r := onlyRecord(t, recs); r.Outcome != ExchangeUpstreamError {
				t.Fatalf("recorded %s on %q, want an upstream error", r.Outcome, r.Rule)
			}
		})
	}
}

// failEvidenceLinkage answers a read of the participant's system that failed
// to answer with that read's own status, and anything else with the gateway's
// own 502, each saying which legs were carried, once.
func TestFailEvidenceLinkage(t *testing.T) {
	for _, row := range []struct {
		name   string
		err    error
		status int
	}{
		{"the system did not answer", &SoRReadError{Kind: SoRUnavailable}, http.StatusServiceUnavailable},
		{"the system's answer was unusable", &SoRReadError{Kind: SoRInvalidResponse}, http.StatusBadGateway},
		{"the gateway's own failure", errors.New("evidence graph too large"), http.StatusBadGateway},
		{"evidence E-06 would re-point under a signature (D113)", &relay.SignedContentError{Edit: relay.EditEvidenceSubjectRekey, Carrier: "Signature in Condition"}, http.StatusUnprocessableEntity},
		{"the gateway could not prepare a record it read", errEvidencePrepare, http.StatusInternalServerError},
		{"a record read the splice cannot carry as read", fmt.Errorf("%w: it is not UTF-8", errRecordNotCarried), http.StatusUnprocessableEntity},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), legsCarriedKey{}, &legsCarried{legs: []string{"crd-order-select", "dtr-questionnaire-fetch"}})
			x := &exchangeRecorder{}
			ctx = context.WithValue(ctx, exchangeRecorderKey{}, x)
			rec := httptest.NewRecorder()
			failEvidenceLinkage(ctx, rec, row.err)
			switch row.status {
			case http.StatusUnprocessableEntity:
				// The fidelity of SHN's own edit, at the build.
				if x.outcome != ExchangeRefused || x.rule != RefusalFidelity || x.refusalStage != vocab.RefusalStageBuild {
					t.Fatalf("recorded %s %s/%s, want refused fidelity/build", x.outcome, x.rule, x.refusalStage)
				}
			case http.StatusInternalServerError:
				// This gateway's own failure, never a refusal.
				if x.outcome != ExchangeOther {
					t.Fatalf("recorded %s %s, want the gateway's own failure", x.outcome, x.rule)
				}
			}
			want := "PAS evidence linkage failed"
			switch row.status {
			case http.StatusUnprocessableEntity:
				want = "signed content cannot be edited (E-06, Signature in Condition)"
				if errors.Is(row.err, errRecordNotCarried) {
					want = "a record read from the system of record cannot be carried as read: it is not UTF-8"
				}
			case http.StatusInternalServerError:
				want = "prepare evidence record failed"
			}
			if rec.Code != row.status || !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("%d %s, want %d", rec.Code, rec.Body, row.status)
			}
			wantCarried(t, rec.Body.String(), "crd-order-select", "dtr-questionnaire-fetch")
		})
	}
}

// otherProduct rewrites the order's product coding to a product no
// origination route asks about.
func otherProduct(t *testing.T) func([]byte) []byte {
	return func(order []byte) []byte {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(order, &m); err != nil {
			t.Fatal(err)
		}
		coded := json.RawMessage(`{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0100","display":"Cane"}]}`)
		for _, k := range []string{"code", "codeCodeableConcept"} {
			if _, ok := m[k]; ok {
				m[k] = coded
			}
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
}

// REJECTION, at every level: a member whose open order is about another
// product than the one the route originates for is refused before the first
// leg, which would carry that order: the participant's records hold no order
// for what the route asks about, so SHN cannot build the request from them,
// 422, the fidelity of its own build, saying no leg was sent.
func TestOrigination_OrderForAnotherProductIsBuildStage(t *testing.T) {
	for _, row := range []struct {
		name  string
		setup func(t *testing.T) (*Gateway, string, string, func() []string)
	}{
		{"order select", func(t *testing.T) (*Gateway, string, string, func() []string) {
			g, stub := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			body := testRouteBody(g, "/scenario/uc04", "")
			g.cfg.SoR = orderRewrite{g.cfg.SoR, otherProduct(t)}
			return g, "POST /scenario/uc04", body, func() []string { return stub.legTypes }
		}},
		{"no-PA coverage check", func(t *testing.T) (*Gateway, string, string, func() []string) {
			env := newInProcessExchange(t)
			g := env.originator
			declareOriginationProfile(g, "demo")
			mountOriginationRoutes(g)
			body := testRouteBody(g, "/scenario/uc02", "")
			g.cfg.SoR = orderRewrite{g.cfg.SoR, otherProduct(t)}
			return g, "POST /scenario/uc02", body, func() []string { return nil }
		}},
		{"order dispatch", func(t *testing.T) (*Gateway, string, string, func() []string) {
			fix := uc03DemoOxygenFixture(t)
			body := testRouteBody(fix.gw, "/scenario/uc03", `"branch":""`)
			fix.gw.cfg.SoR = orderRewrite{fix.gw.cfg.SoR, otherProduct(t)}
			return fix.gw, "POST /scenario/uc03", body, func() []string { return fix.stub.legTypes }
		}},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				g, route, body, legs := row.setup(t)
				g.cfg.ConformanceEnforcement = level
				rec, recs := serveRecorded(t, g, route, body)
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "this origination is about") || len(legs()) != 0 {
					t.Fatalf("%s = %d %s legs %v, want 422 naming both products and no leg", route, rec.Code, rec.Body, legs())
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
