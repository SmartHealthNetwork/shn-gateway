//go:build !shn_coreonly

package engine

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// End to end on UC-04, at every level: the supplemental report the amendment
// would carry is read from the provider's own system. One that names no
// subject.reference, or names another subject, is a report the build cannot
// carry as read: 502, the fidelity of the gateway's own build, with no
// Retry-After and no claim update sent; the submission's legs were sent and
// stand, and the claim update it did not send is observed as failed. (One a signature covers where E-06 would re-point it takes the same
// answer, pinned in TestSupplementalReport_Refusals: this route's system holds
// the member's Patient under the member id, where E-06 changes nothing and a
// signed report is carried as held.)
func TestHandleUC04_SupplementalReportTheBuildCannotCarryIsRefused502(t *testing.T) {
	const route = "POST /scenario/uc04"
	for name, row := range map[string]struct{ report, want string }{
		"no subject.reference": {`{"resourceType":"DiagnosticReport","id":"dr-7","status":"final","code":{"text":"report"}}`, msgSupplementalNoSubject},
		"another subject":      {supplementalReportAsHeld("Patient/someone-else"), msgSupplementalOtherSubject},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				gw, stub := newPendResumeFixture(t, pendFixtureOpts{
					member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen",
					pendedItem: "operative-diagnostic-report",
				})
				mountOriginationRoutes(gw)
				gw.cfg.ConformanceEnforcement = level
				gw.cfg.SoR = &renamedReportSoR{censusSoR: gw.cfg.SoR.(*censusSoR), member: "MBR-UC04", sorID: "MBR-UC04", report: []byte(row.report)}
				var mu sync.Mutex
				var failed []ObserverEvent
				gw.cfg.Observer = func(e ObserverEvent) {
					if e.Kind == "leg.failed" {
						mu.Lock()
						failed = append(failed, e)
						mu.Unlock()
					}
				}
				rec, recs := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc04", ""))
				body := rec.Body.String()
				if rec.Code != http.StatusBadGateway || !strings.Contains(body, row.want) || rec.Header().Get("Retry-After") != "" ||
					strings.Contains(body, "someone-else") || strings.Contains(body, "MBR-UC04") || !strings.Contains(body, "pas-claim") {
					t.Fatalf("status %d (Retry-After %q): %s; want 502 %q naming no patient, stating the legs carried", rec.Code, rec.Header().Get("Retry-After"), body, row.want)
				}
				if !legAttempted(stub.legTypes, "pas-claim") || legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("legs %v: want the submission sent and no claim update", stub.legTypes)
				}
				// The claim update it did not send is observed as failed, with the reason.
				mu.Lock()
				observed := len(failed) == 1 && failed[0].LegType == "pas-claim-update" && failed[0].Detail == row.want
				mu.Unlock()
				if !observed {
					t.Fatalf("leg.failed events %+v, want one for the pas-claim-update leg with the reason", failed)
				}
				r := onlyRecord(t, recs)
				wantRefusal(t, r, http.StatusBadGateway, RefusedByProviderGateway, RefusalFidelity)
				if r.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
				}
			})
		}
	}
}

// End to end on UC-04, at every level: a supplemental report the SDK's carried
// update build cannot place as written (an id that is not a FHIR id, or a
// record whose resourceType names another type) is a record read from the provider's
// own system: 502, the SDK's reason, the fidelity of the gateway's own build,
// with no Retry-After and no claim update sent. Before v1.16.0 it was 422.
// (The same refusal of a facility's report on UC-05 stays 422:
// TestHandleUC05_FacilityProvenanceWithNoExactIDIsRefusedByTheSDKBuild.)
func TestHandleUC04_SupplementalReportTheSDKBuildCannotCarryIsRefused502(t *testing.T) {
	const route = "POST /scenario/uc04"
	held := `{"resourceType":"DiagnosticReport","id":"dr-7","status":"final","code":{"text":"report"},"subject":{"reference":"Patient/MBR-UC04"}}`
	for name, row := range map[string]struct{ report, want string }{
		"an id that is not a FHIR id": {strings.Replace(held, `"dr-7"`, `"dr 7!"`, 1), "the supplemental report has no id"},
		"not a DiagnosticReport":      {strings.Replace(held, `"DiagnosticReport"`, `"Observation"`, 1), "the supplemental report is not a DiagnosticReport"},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				gw, stub := newPendResumeFixture(t, pendFixtureOpts{
					member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen",
					pendedItem: "operative-diagnostic-report",
				})
				mountOriginationRoutes(gw)
				gw.cfg.ConformanceEnforcement = level
				gw.cfg.SoR = &renamedReportSoR{censusSoR: gw.cfg.SoR.(*censusSoR), member: "MBR-UC04", sorID: "MBR-UC04", report: []byte(row.report)}
				rec, recs := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc04", ""))
				body := rec.Body.String()
				if rec.Code != http.StatusBadGateway || !strings.Contains(body, shnsdk.ErrPASRecordNotCarried.Error()) || !strings.Contains(body, row.want) || rec.Header().Get("Retry-After") != "" {
					t.Fatalf("status %d (Retry-After %q): %s; want 502 with the SDK's reason %q", rec.Code, rec.Header().Get("Retry-After"), body, row.want)
				}
				if !legAttempted(stub.legTypes, "pas-claim") || legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("legs %v: want the submission sent and no claim update", stub.legTypes)
				}
				r := onlyRecord(t, recs)
				wantRefusal(t, r, http.StatusBadGateway, RefusedByProviderGateway, RefusalFidelity)
				if r.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
				}
			})
		}
	}
}

// readRecordNotCarried reports the SDK's carry refusal as a record read from
// the system of record (502) and keeps the SDK's sentinel and message; any
// other build error, and the SDK's refusal unwrapped, keep their status.
func TestReadRecordNotCarried(t *testing.T) {
	sdkErr := fmt.Errorf("shnsdk: conformant update: %w: the supplemental report is not a DiagnosticReport", shnsdk.ErrPASRecordNotCarried)
	got := readRecordNotCarried(sdkErr)
	if !errors.Is(got, errRecordNotCarried) || !errors.Is(got, shnsdk.ErrPASRecordNotCarried) || got.Error() != sdkErr.Error() {
		t.Fatalf("readRecordNotCarried = %v: want errRecordNotCarried and the SDK sentinel, with the SDK's message", got)
	}
	if s := buildFidelityStatus(got); s != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", s)
	}
	if s := buildFidelityStatus(sdkErr); s != http.StatusUnprocessableEntity {
		t.Fatalf("the SDK's refusal unwrapped: status %d, want 422", s)
	}
	for _, other := range []error{shnsdk.ErrPASClaimFactAbsent, shnsdk.ErrPASPriorClaimNotCarried, errors.New("boom")} {
		if got := readRecordNotCarried(other); got != other {
			t.Fatalf("readRecordNotCarried(%v) = %v, want it unchanged", other, got)
		}
	}
}

// End to end on UC-04, at every level: the supplemental report the amendment
// needs, read from the provider's own system, is refused 502 when the system
// holds none, or holds one with no id: unknown is never empty, and the build
// carries what the system holds or nothing (the fidelity of the gateway's own
// build, with the claim update it does not send observed). Before v1.16.0
// each was the gateway's own 500. A report that cannot be read as a resource
// is 502 as before, recorded as the failed read of the system it is (message
// integrity when it repeats a member name), never as the build's fidelity.
func TestHandleUC04_SupplementalReportAbsentOrUnreadableIsRefused502(t *testing.T) {
	const route = "POST /scenario/uc04"
	held := `{"resourceType":"DiagnosticReport","id":"dr-7","status":"final","code":{"text":"report"},"subject":{"reference":"Patient/MBR-UC04"}}`
	for name, row := range map[string]struct {
		report []byte // nil: the system holds none
		want   string
		record func(*testing.T, ExchangeRecord)
	}{
		"none held": {nil, "no supplemental report", wantBuildFidelity},
		"no id":     {[]byte(strings.Replace(held, `"id":"dr-7",`, "", 1)), "supplemental report missing id", wantBuildFidelity},
		"not a resource": {[]byte(held + " {}"), msgSupplementalUnreadable, func(t *testing.T, r ExchangeRecord) {
			if r.Outcome != ExchangeUpstreamError || r.Rule != "" || r.RefusalStage != "" {
				t.Fatalf("recorded %s %q stage %q, want upstream-error: a read of the system the gateway cannot use", r.Outcome, r.Rule, r.RefusalStage)
			}
		}},
		"a member named twice": {[]byte(strings.Replace(held, `"status":"final"`, `"status":"final","Status":"x"`, 1)), msgSupplementalUnreadable, func(t *testing.T, r ExchangeRecord) {
			wantRefusal(t, r, http.StatusBadGateway, RefusedByProviderGateway, RefusalIntegrity)
		}},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				gw, stub := newPendResumeFixture(t, pendFixtureOpts{
					member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen",
					pendedItem: "operative-diagnostic-report",
				})
				mountOriginationRoutes(gw)
				gw.cfg.ConformanceEnforcement = level
				gw.cfg.SoR = &renamedReportSoR{censusSoR: gw.cfg.SoR.(*censusSoR), member: "MBR-UC04", sorID: "MBR-UC04", report: row.report}
				var mu sync.Mutex
				var failed []ObserverEvent
				gw.cfg.Observer = func(e ObserverEvent) {
					if e.Kind == "leg.failed" {
						mu.Lock()
						failed = append(failed, e)
						mu.Unlock()
					}
				}
				rec, recs := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc04", ""))
				body := rec.Body.String()
				if rec.Code != http.StatusBadGateway || !strings.Contains(body, row.want) || rec.Header().Get("Retry-After") != "" {
					t.Fatalf("status %d (Retry-After %q): %s; want 502 %q", rec.Code, rec.Header().Get("Retry-After"), body, row.want)
				}
				if !legAttempted(stub.legTypes, "pas-claim") || legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("legs %v: want the submission sent and no claim update", stub.legTypes)
				}
				r := onlyRecord(t, recs)
				row.record(t, r)
				if r.RefusalStage == vocab.RefusalStageBuild {
					mu.Lock()
					observed := len(failed) == 1 && failed[0].LegType == "pas-claim-update" && failed[0].Detail == row.want
					mu.Unlock()
					if !observed {
						t.Fatalf("leg.failed events %+v, want one for the pas-claim-update leg with the reason", failed)
					}
				}
			})
		}
	}
}

// wantBuildFidelity fails unless r is a 502 refusal on the fidelity of the
// gateway's own build.
func wantBuildFidelity(t *testing.T, r ExchangeRecord) {
	t.Helper()
	wantRefusal(t, r, http.StatusBadGateway, RefusedByProviderGateway, RefusalFidelity)
	if r.RefusalStage != vocab.RefusalStageBuild {
		t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
	}
}
