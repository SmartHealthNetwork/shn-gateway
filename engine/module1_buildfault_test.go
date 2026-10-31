//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// historyWithoutOrder is the dispatch member's system of record whose device
// history holds another order than the open one being dispatched.
type historyWithoutOrder struct{ *homeOxygenSoR }

func (s historyWithoutOrder) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	if resourceType != "DeviceRequest" {
		return s.homeOxygenSoR.SearchPatientContext(ctx, resourceType, id, dates...)
	}
	page := []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"DeviceRequest","id":"another-order","status":"active","intent":"order","subject":{"reference":"Patient/MBR-OX"}},"search":{"mode":"match"}}]}`)
	parsed, err := ParseSearchPage(page, resourceType)
	if err != nil {
		return SearchResult{}, err
	}
	return SearchResult{Pages: [][]byte{page}, Entries: parsed.Entries, Total: len(parsed.Entries)}, nil
}

// A dispatch whose participant's device history does not hold the order
// being dispatched cannot be built faithfully from the participant's records:
// a fidelity refusal at the build stage, before any leg, at every conformance
// level, with its answer unchanged.
func TestOriginationRecord_DispatchHistoryWithoutTheOrderIsBuildStage(t *testing.T) {
	const want = `{"error":"build order-dispatch request failed: the system of record's device history does not hold the dispatched order DeviceRequest/dr-ox"}`
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			fix := newMBROXDispatchFixture(t)
			mountOriginationRoutes(fix.gw)
			fix.gw.cfg.ConformanceEnforcement = level
			body := testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`)
			fix.gw.cfg.SoR = historyWithoutOrder{fix.sor}
			const route = "POST /scenario/dispatch"
			rec, recs := serveRecorded(t, fix.gw, route, body)
			if rec.Code != http.StatusUnprocessableEntity || strings.TrimSpace(rec.Body.String()) != want {
				t.Fatalf("%s = %d %s, want 422 %s", route, rec.Code, rec.Body.String(), want)
			}
			if len(fix.stub.legTypes) != 0 {
				t.Fatalf("legs %v sent before the refusal", fix.stub.legTypes)
			}
			r := onlyRecord(t, recs)
			wantOrigination(t, r, route, http.StatusUnprocessableEntity)
			wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
			if r.RefusalStage != vocab.RefusalStageBuild {
				t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
			}
		})
	}
}

// historyByStatus is the dispatch member's system of record holding the
// dispatched order at status. Its DeviceRequest search applies the status
// filter it is handed, as a FHIR server does, and keeps the filter in filter.
type historyByStatus struct {
	*homeOxygenSoR
	filter *[]SearchDateRange
}

func (s historyByStatus) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	if resourceType != "DeviceRequest" {
		return s.homeOxygenSoR.SearchPatientContext(ctx, resourceType, id, dates...)
	}
	*s.filter = dates
	var order struct{ Status string }
	if err := json.Unmarshal(s.orderJSON, &order); err != nil {
		return SearchResult{}, err
	}
	for _, d := range dates {
		if d.Param == "status" && !slices.Contains(d.AnyOf, order.Status) {
			page := []byte(`{"resourceType":"Bundle","type":"searchset"}`)
			return SearchResult{Pages: [][]byte{page}}, nil
		}
	}
	return s.homeOxygenSoR.SearchPatientContext(ctx, resourceType, id, dates...)
}

// The device history is the gateway's search of the participant's system,
// narrowed to the statuses of the published Da Vinci prefetch template
// (prefetchStatus, the table the advertised deviceHistory template reads). An
// order the participant's system holds at another status is not in that
// history: the request cannot be built from the participant's records under
// the published rule, so it is refused exactly as an order the history does
// not hold at all (TestOriginationRecord_DispatchHistoryWithoutTheOrderIsBuildStage):
// the same answer, a fidelity refusal at the build stage, at every level. An
// order at a status the filter names is held, and is not refused.
func TestOriginationRecord_DispatchOrderOutsideTheHistoryStatusFilterIsBuildStage(t *testing.T) {
	const want = `{"error":"build order-dispatch request failed: the system of record's device history does not hold the dispatched order DeviceRequest/dr-ox"}`
	wantFilter := []SearchDateRange{{Param: "status", AnyOf: prefetchStatus["DeviceRequest"]}}
	for _, row := range []struct {
		status string
		held   bool
	}{
		{"draft", false},
		{"revoked", false},
		{"entered-in-error", false},
		{"active", true},
	} {
		for _, level := range allLevels {
			t.Run(row.status+"/"+level.String(), func(t *testing.T) {
				fix := newMBROXDispatchFixture(t)
				mountOriginationRoutes(fix.gw)
				fix.gw.cfg.ConformanceEnforcement = level
				var order map[string]any
				if err := json.Unmarshal(fix.sor.orderJSON, &order); err != nil {
					t.Fatal(err)
				}
				order["status"] = row.status
				orderJSON, err := json.Marshal(order)
				if err != nil {
					t.Fatal(err)
				}
				fix.sor.orderJSON = orderJSON
				body := testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`)
				var filter []SearchDateRange
				fix.gw.cfg.SoR = historyByStatus{fix.sor, &filter}
				const route = "POST /scenario/dispatch"
				rec, recs := serveRecorded(t, fix.gw, route, body)
				if !reflect.DeepEqual(filter, wantFilter) {
					t.Fatalf("device history searched with %+v, want prefetchStatus's %+v", filter, wantFilter)
				}
				if row.held {
					if strings.TrimSpace(rec.Body.String()) == want {
						t.Fatalf("%s = %d %s: an order at %s is in the history", route, rec.Code, rec.Body.String(), row.status)
					}
					return
				}
				if rec.Code != http.StatusUnprocessableEntity || strings.TrimSpace(rec.Body.String()) != want {
					t.Fatalf("%s = %d %s, want 422 %s", route, rec.Code, rec.Body.String(), want)
				}
				if len(fix.stub.legTypes) != 0 {
					t.Fatalf("legs %v sent before the refusal", fix.stub.legTypes)
				}
				r := onlyRecord(t, recs)
				wantOrigination(t, r, route, http.StatusUnprocessableEntity)
				wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
				if r.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
				}
			})
		}
	}
}

// Each builder classes its failures by what they are about: the
// participant's records (an order record it cannot read, a device history
// without the dispatched order) or this gateway's own assembly (the Coverage
// searchset it assembled, the questionnaire-package request it seals). The
// text is the failure's own.
func TestBuildFault_BuildersClassTheirFailures(t *testing.T) {
	g := &Gateway{}
	fault := func(t *testing.T, err error, own bool, text string) {
		t.Helper()
		var f *buildFault
		if !errors.As(err, &f) || f.own != own || err.Error() != text {
			t.Fatalf("err %v (%T), want a build fault own=%v reading %q", err, err, own, text)
		}
	}
	t.Run("an order record with no id", func(t *testing.T) {
		_, err := g.originatedOrderingRequest(context.Background(), hookOrderSelect, "corr-1", crdOriginRecords{}, []byte(`{"resourceType":"ServiceRequest"}`))
		fault(t, err, false, "order: resource has no type or id")
	})
	t.Run("a dispatched order record with no id", func(t *testing.T) {
		_, err := g.originatedDispatchRequest("corr-1", crdOriginRecords{}, []byte(`{"resourceType":"DeviceRequest"}`), "Organization/o")
		fault(t, err, false, "order: resource has no type or id")
	})
	t.Run("a dispatch history without the order", func(t *testing.T) {
		recs := crdOriginRecords{history: map[string][]byte{"deviceHistory": []byte(`{"resourceType":"Bundle","type":"searchset","entry":[]}`)}}
		_, err := g.originatedDispatchRequest("corr-1", recs, []byte(`{"resourceType":"DeviceRequest","id":"dr-1"}`), "Organization/o")
		fault(t, err, false, "the system of record's device history does not hold the dispatched order DeviceRequest/dr-1")
	})
	t.Run("the Coverage searchset it assembled, unreadable", func(t *testing.T) {
		_, _, err := originatedPackageRequest("2.0", crdOriginRecords{coverage: []byte(`{`)}, []byte(`{"resourceType":"ServiceRequest","id":"sr-1"}`), "http://q", "")
		if err == nil || !strings.HasPrefix(err.Error(), "read the coverage search result: ") {
			t.Fatalf("err %v, want the coverage search result refusal", err)
		}
		fault(t, err, true, err.Error())
	})
	in := shnsdk.QuestionnairePackageInputs{Orders: [][]byte{[]byte(`{"resourceType":"ServiceRequest","id":"sr-1"}`)}}
	t.Run("a copied input the gateway did not pass", func(t *testing.T) {
		pkg := shnsdk.QuestionnairePackageParameters{Body: []byte(`{}`), Copied: []shnsdk.QuestionnairePackageCopiedSpan{{Parameter: "referenced", Index: 0}}}
		_, err := sealPackageRequest(pkg, in)
		fault(t, err, true, "questionnaire-package request copies an unknown input referenced[0]")
	})
	t.Run("a copied span the body does not hold", func(t *testing.T) {
		pkg := shnsdk.QuestionnairePackageParameters{Body: []byte(`{}`), Copied: []shnsdk.QuestionnairePackageCopiedSpan{{Parameter: "order", Index: 0, Start: 0, End: 44, At: 0}}}
		_, err := sealPackageRequest(pkg, in)
		if err == nil {
			t.Fatal("sealed a body that does not hold its copied span")
		}
		fault(t, err, true, err.Error())
	})
}

// refuseBuildFailure answers every build failure with the unchanged 422 and
// records it by what it is about: the participant's records (and an ordering
// clinician named nowhere) at the build stage, this gateway's own assembly at
// the verify stage, and the SDK builder's own, untyped errors not at all
// (the exchange's 4xx fallback records them as rule other).
func TestRefuseBuildFailure_RecordsByWhatFailed(t *testing.T) {
	for _, row := range []struct {
		name  string
		err   error
		stage string // "" : no fidelity refusal
	}{
		{"no ordering clinician", fmt.Errorf("order %w", errNoOrderingClinician), vocab.RefusalStageBuild},
		{"a participant record", recordFault(errors.New("order: no id")), vocab.RefusalStageBuild},
		{"this gateway's assembly", assemblyFault(errors.New("copies an unknown input order[3]")), vocab.RefusalStageVerify},
		{"an untyped builder error", errors.New("patient is not a Patient resource"), ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			g, _, _ := crdTestSystem(t, uc03Coverage())
			var events []ObserverEvent
			g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			x := &exchangeRecorder{}
			rec := httptest.NewRecorder()
			msg := "build order-select request failed: " + row.err.Error()
			g.refuseBuildFailure(context.WithValue(context.Background(), exchangeRecorderKey{}, x), &exchangeWriter{ResponseWriter: rec, x: x}, "payer", "crd-order-select", msg, row.err)
			if rec.Code != http.StatusUnprocessableEntity || strings.TrimSpace(rec.Body.String()) != `{"error":"`+msg+`"}` {
				t.Fatalf("answer %d %s, want the unchanged 422", rec.Code, rec.Body.String())
			}
			if row.stage == "" {
				if x.outcome == ExchangeRefused {
					t.Fatalf("recorded refused %s/%s for an untyped builder error", x.rule, x.refusalStage)
				}
				return
			}
			if x.outcome != ExchangeRefused || x.rule != RefusalFidelity || x.refusalStage != row.stage {
				t.Fatalf("recorded %q/%q stage %q, want fidelity at %s", x.outcome, x.rule, x.refusalStage, row.stage)
			}
			if len(events) != 1 || events[0].Kind != "leg.failed" || events[0].LegType != "crd-order-select" {
				t.Fatalf("events %+v, want one leg.failed", events)
			}
		})
	}
}
