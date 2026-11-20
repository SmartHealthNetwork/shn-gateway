//go:build !shn_coreonly

package engine

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
)

// End to end on UC-05, at every level: a facility's records Bundle that holds
// a report (an entry whose exact resourceType is DiagnosticReport) stating no
// id exactly is held, not absent. The requester cannot carry it into the
// claim update, its own message, under the report's own id: 422, the fidelity
// of the gateway's own build, naming the facility's report and no patient,
// with no claim update sent. Before v1.16.0 it was 502 "federated response
// parse failed: records Bundle holds no report". A Bundle that holds no
// report at all keeps that answer. (The route fixture's validator is the
// fake: at structural or strict a real $validate of the facility's records
// may refuse an "ID" first, as the participant's opt-in conformance.)
func TestHandleUC05_FacilityReportTheBuildCannotCarryIsAFidelityRefusal(t *testing.T) {
	const route = "POST /scenario/uc05"
	for name, row := range map[string]struct {
		change func(*testing.T, []any) []any
		status int
		want   string
	}{
		"its id spelled another way": {func(t *testing.T, entries []any) []any {
			r := facilityEntry(t, entries, "DiagnosticReport")
			r["ID"] = r["id"]
			delete(r, "id")
			return entries
		}, http.StatusUnprocessableEntity, "cannot be the claim update's evidence: it states no id exactly"},
		"an id that is not a string": {func(t *testing.T, entries []any) []any {
			facilityEntry(t, entries, "DiagnosticReport")["id"] = 7
			return entries
		}, http.StatusUnprocessableEntity, "cannot be the claim update's evidence: it states no id exactly"},
		// Where any report states an id exactly, the gateway takes it, and the
		// claim update is sent.
		"a second report that states its id": {func(t *testing.T, entries []any) []any {
			held := facilityEntry(t, entries, "DiagnosticReport")
			spelled := map[string]any{}
			for k, v := range held {
				spelled[k] = v
			}
			spelled["ID"] = "dr-spelled"
			delete(spelled, "id")
			return append(entries, map[string]any{"fullUrl": "urn:shn:fedquery:dr-spelled", "resource": spelled})
		}, http.StatusOK, ""},
		"no report at all": {func(t *testing.T, entries []any) []any {
			var kept []any
			for _, e := range entries {
				if res, _ := e.(map[string]any)["resource"].(map[string]any); res["resourceType"] != "DiagnosticReport" {
					kept = append(kept, e)
				}
			}
			if len(kept) == len(entries) {
				t.Fatal("the records Bundle holds no DiagnosticReport to remove")
			}
			return kept
		}, http.StatusBadGateway, "federated response parse failed: records Bundle holds no report"},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				gw, stub := uc05Fixture(t, level, func(n int, p []byte) []byte {
					if n != 1 {
						return p
					}
					return withFacilityRecords(t, p, func(entries []any) []any { return row.change(t, entries) })
				})
				var mu sync.Mutex
				var failed []ObserverEvent
				gw.cfg.Observer = func(e ObserverEvent) {
					if e.Kind == "leg.failed" {
						mu.Lock()
						failed = append(failed, e)
						mu.Unlock()
					}
				}
				rec, recs := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc05", ""))
				body := rec.Body.String()
				if row.status == http.StatusOK {
					if rec.Code != http.StatusOK || !legAttempted(stub.legTypes, "pas-claim-update") {
						t.Fatalf("status %d: %s legs %v; want 200 with the claim update sent", rec.Code, body, stub.legTypes)
					}
					return
				}
				if rec.Code != row.status || !strings.Contains(body, row.want) || strings.Contains(body, "MBR-UC05") {
					t.Fatalf("status %d: %s; want %d %q naming no patient", rec.Code, body, row.status, row.want)
				}
				if legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("a claim update was sent (legs: %v)", stub.legTypes)
				}
				if row.status != http.StatusUnprocessableEntity {
					return
				}
				if strings.Contains(body, "holds no report") || !strings.Contains(body, "(fidelity of SHN's own build)") || rec.Header().Get("Retry-After") != "" {
					t.Fatalf("%s (Retry-After %q): a held report is not an absent one, and the refusal is the build's fidelity", body, rec.Header().Get("Retry-After"))
				}
				mu.Lock()
				observed := len(failed) == 1 && failed[0].LegType == "pas-claim-update" && strings.Contains(failed[0].Detail, row.want)
				mu.Unlock()
				if !observed {
					t.Fatalf("leg.failed events %+v, want one for the pas-claim-update leg with the reason", failed)
				}
				r := onlyRecord(t, recs)
				wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
				if r.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
				}
			})
		}
	}
}
