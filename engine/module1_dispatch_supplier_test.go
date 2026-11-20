//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
)

// End to end on POST /scenario/dispatch, at every level: the dispatched
// supplier is read from the provider's own system, and the Claim names it as
// its provider, so the build places it. The device history search includes it
// (DeviceRequest:performer); a page holding one that cannot be carried is
// malformed and the history is left out, while the supplier read by reference
// reaches the build. The fixture's connector returns the supplier's bytes as
// held, as a participant's own connector may; the gateway's own FHIR connector
// (fhirsor) refuses one naming a member twice, or not one JSON object, at its
// read (fhirclient's repeated-member and resource checks), as before. One the request cannot carry as read
// (each form of the splice's own rule that reaches the route) is refused 502,
// the fidelity of the gateway's own build, naming the record by the order's
// performer reference, with the pas-claim leg observed as failed, no
// Retry-After and no PAS claim sent. Before, it
// was 502 "PAS evidence linkage failed" under no named rule, or, for a member
// named twice, the gateway's own 500. The control: a supplier carried as read
// rides, and the payer's answer is returned.
func TestDispatch_SupplierNotCarriedAsReadIsRefused502(t *testing.T) {
	const (
		member       = "MBR-PD-UC03"
		performerRef = "Organization/org-dme-uc03"
	)
	demo := Demo{BirthDate: "1962-03-02", FamilyName: "Delgado-Dispatch"}
	held, err := buildHomeOxygenSupplier("org-dme-uc03")
	if err != nil {
		t.Fatal(err)
	}
	edit := func(t *testing.T, old, with string) []byte {
		out := bytes.Replace(held, []byte(old), []byte(with), 1)
		if bytes.Equal(out, held) {
			t.Fatalf("the supplier record holds no %s", old)
		}
		return out
	}
	for name, row := range map[string]struct {
		supplier func(*testing.T) []byte
		why      string // "" for the control
	}{
		"carried as read":          {func(*testing.T) []byte { return held }, ""},
		"a lone surrogate escape":  {func(t *testing.T) []byte { return edit(t, `"name":"`, `"name":"\ud800`) }, "holds a lone surrogate escape"},
		"not UTF-8":                {func(t *testing.T) []byte { return edit(t, `"name":"`, "\"name\":\"\xff") }, "is not UTF-8"},
		"a member named twice":     {func(t *testing.T) []byte { return edit(t, `"name":`, `"name":"x","name":`) }, "names a member twice, or in another case"},
		"a member in another case": {func(t *testing.T) []byte { return edit(t, `"name":`, `"Name":"x","name":`) }, "names a member twice, or in another case"},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				order, err := buildDispatchDeviceRequest("dr-uc03", "Patient/"+member, performerRef, "E1390", "Stationary oxygen concentrator", "J96.10")
				if err != nil {
					t.Fatal(err)
				}
				fix := newDispatchFixture(t, member, demo, withReason(t, order), performerRef, row.supplier(t))
				fix.gw.cfg.ConformanceEnforcement = level
				useRecordIDSoR(t, fix.gw, member, member, demo, "member's own Coverage", "Patient/"+member)
				var mu sync.Mutex
				var failed []ObserverEvent
				var history []prefetchObtained
				fix.gw.cfg.Observer = func(e ObserverEvent) {
					mu.Lock()
					defer mu.Unlock()
					switch {
					case e.Kind == "leg.failed":
						failed = append(failed, e)
					case e.Kind == PrefetchObtainedEvent && e.Op == "deviceHistory":
						var p prefetchObtained
						if err := json.Unmarshal([]byte(e.Detail), &p); err != nil {
							t.Errorf("deviceHistory event: %v", err)
						}
						history = append(history, p)
					}
				}
				rec, recs := serveRecorded(t, fix.gw, "POST /scenario/dispatch", testRouteBody(fix.gw, "/scenario/dispatch", `"member":"`+member+`"`))
				r := onlyRecord(t, recs)
				mu.Lock()
				defer mu.Unlock()
				// The device history is searched with the order's performer
				// included, as a real search is: a page holding a supplier that
				// cannot be carried is malformed, and the history is left out.
				wantHistory := SearchMalformed
				if row.why == "" {
					wantHistory = SearchOK
				}
				if len(history) != 1 || history[0].Outcome != wantHistory {
					t.Fatalf("deviceHistory obtained %+v, want one search %s", history, wantHistory)
				}
				if row.why == "" {
					if rec.Code != http.StatusOK || !legAttempted(fix.stub.legTypes, "pas-claim") || len(failed) != 0 {
						t.Fatalf("dispatch = %d %s legs %v failed %v, want 200 with the PAS claim sent", rec.Code, rec.Body, fix.stub.legTypes, failed)
					}
					return
				}
				want := "a record read from the system of record cannot be carried as read: the dispatched supplier " + performerRef + " " + row.why
				if len(failed) != 1 || failed[0].LegType != "pas-claim" || failed[0].Detail != want {
					t.Fatalf("leg.failed events %+v, want one for the pas-claim leg with %q", failed, want)
				}
				if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), want) || legAttempted(fix.stub.legTypes, "pas-claim") {
					t.Fatalf("dispatch = %d %s legs %v, want 502 %q and no PAS claim", rec.Code, rec.Body, fix.stub.legTypes, want)
				}
				if ra := rec.Header().Get("Retry-After"); ra != "" {
					t.Fatalf("Retry-After %q, want none: a retry cannot succeed until the record changes", ra)
				}
				wantRefusal(t, r, http.StatusBadGateway, RefusedByProviderGateway, RefusalFidelity)
				if r.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
				}
			})
		}
	}
}

// dispatchedSupplierNotCarried refuses by the splice's own rule only, names
// the record by the order's exact performer reference, or as the dispatched
// supplier where the order names none exactly, and passes no supplier and one
// carried as read, surrounding white space aside.
func TestDispatchedSupplierNotCarried(t *testing.T) {
	order := []byte(`{"resourceType":"DeviceRequest","id":"dr-1","performer":{"reference":"Organization/s-1"}}`)
	clean := []byte(`{"resourceType":"Organization","id":"s-1","name":"Supplier"}`)
	for name, row := range map[string]struct {
		order, supplier []byte
		want            string // "" for none
	}{
		"no supplier":                     {order, nil, ""},
		"carried as read":                 {order, clean, ""},
		"carried as read, spaced":         {order, append(append([]byte(" \n"), clean...), '\t'), ""},
		"not one JSON object":             {order, []byte(`[` + string(clean) + `]`), "the dispatched supplier Organization/s-1 is not exactly one JSON object"},
		"a lone surrogate":                {order, bytes.Replace(clean, []byte(`"Supplier"`), []byte(`"\udc00"`), 1), "the dispatched supplier Organization/s-1 holds a lone surrogate escape"},
		"no performer reference":          {[]byte(`{"resourceType":"DeviceRequest","id":"dr-1"}`), []byte("{\"name\":\"\xff\"}"), "the dispatched supplier is not UTF-8"},
		"a performer spelled another way": {[]byte(`{"resourceType":"DeviceRequest","Performer":{"reference":"Organization/s-1"}}`), []byte(`{"a":1,"A":2}`), "the dispatched supplier names a member twice, or in another case"},
	} {
		err := dispatchedSupplierNotCarried(row.order, row.supplier)
		if row.want == "" {
			if err != nil {
				t.Errorf("%s: %v, want none", name, err)
			}
			continue
		}
		if !errors.Is(err, errRecordNotCarried) || err.Error() != errRecordNotCarried.Error()+": "+row.want {
			t.Errorf("%s: %v, want %q", name, err, row.want)
		}
		if got := buildFidelityStatus(err); got != http.StatusBadGateway {
			t.Errorf("%s: status %d, want 502", name, got)
		}
	}
}
