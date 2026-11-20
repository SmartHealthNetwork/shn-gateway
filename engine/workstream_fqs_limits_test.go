//go:build !shn_coreonly

package engine

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
)

// fqDeepRecord is facDR1 with a note nested n levels deep, which the
// facility's system of record answers within its own read.
func fqDeepRecord(n int) string {
	return strings.Replace(facDR1, `"status": "final",`, `"status": "final", "note": [{"text":"x","x":`+strings.Repeat(`{"a":`, n)+`1`+strings.Repeat(`}`, n)+`}],`, 1)
}

// fqTokenPages are two search pages of the member's DiagnosticReports, each
// record holding n JSON tokens: each page within its own read, the records
// together n*2.
func fqTokenPages(n int) [][]byte {
	record := func(id string) string {
		r := strings.Replace(facDR1, `"id": "dr-1"`, `"id": "`+id+`"`, 1)
		return strings.Replace(r, `"status": "final",`, `"status": "final", "note": [{"text":"x","x":[`+strings.TrimSuffix(strings.Repeat(`0,`, n), ",")+`]}],`, 1)
	}
	return [][]byte{facPage("http://sor.internal/fhir/facility?page=2", record("dr-a")), facPage("", record("dr-b"))}
}

// fqBigPages are k search pages of one DiagnosticReport each, its note n bytes.
func fqBigPages(k, n int) [][]byte {
	var pages [][]byte
	for i := 0; i < k; i++ {
		next := ""
		if i < k-1 {
			next = fmt.Sprintf("http://sor.internal/fhir/facility?page=%d", i+2)
		}
		r := strings.Replace(facDR1, `"id": "dr-1"`, fmt.Sprintf(`"id": "dr-%d"`, i), 1)
		r = strings.Replace(r, `"status": "final",`, `"status": "final", "note": [{"text":"`+strings.Repeat("x", n)+`"}],`, 1)
		pages = append(pages, facPage(next, r))
	}
	return pages
}

// fqManyRecords are n of the member's DiagnosticReports, each its own id.
func fqManyRecords(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.Replace(facDR1, `"id": "dr-1"`, fmt.Sprintf(`"id": "dr-%d"`, i), 1)
	}
	return out
}

// A facility's gateway whose fulfillment of a federated query, built from the
// facility's own records, goes past the gateway's own carrying limits refuses
// it 502, at every level: recorded as the fidelity of its own build (refusal
// rule fidelity, stage build), with no Retry-After, its reason naming no
// record, since the answer goes to another participant; nothing of the
// fulfillment is sent, and no record is dropped to make it fit. The records
// reach the limit either nested in the fulfillment (past its depth) or
// together in the records Bundle (past its tokens), each record within its own
// read, or a request Task within its own limits and the records together past
// the fulfillment's tokens. The reason is chosen by the path: the fulfillment,
// which extends the request with the records, names neither as the cause; the
// records Bundle, which holds nothing of the request, names the records. Records
// past one search's per-answer bound (4 MiB, 200 entries) are refused the
// same way, naming the requested type and the bound. The controls, just under
// each, are fulfilled.
func TestFederatedQueryInbound_AFulfillmentPastTheCarryingLimitsIsRefused502(t *testing.T) {
	fulfillment, records, bound := refusalCDexFulfillmentPastLimits, refusalCDexRecordsPastLimits, refusalCDexRecordsPastBound("DiagnosticReport")
	one := [][]byte{facPage("", facDR1)}
	rows := []struct {
		name    string
		task    int // tokens the request Task adds, within its own limits
		pages   [][]byte
		refused string
	}{
		{"a record nested into the fulfillment past its depth", 0, [][]byte{facPage("", fqDeepRecord(57))}, fulfillment},
		{"control: a record nested just within it", 0, [][]byte{facPage("", fqDeepRecord(55))}, ""},
		{"a request Task within its limits and a record together past the fulfillment's tokens", 999_800, one, fulfillment},
		{"control: the request Task and the record together within them", 999_700, one, ""},
		{"records together past the records Bundle's tokens", 0, fqTokenPages(520_000), records},
		{"control: records together within them", 0, fqTokenPages(400_000), ""},
		{"records past one search's 4 MiB", 0, fqBigPages(2, 2_100_000), bound},
		{"control: records just within it", 0, fqBigPages(1, SoRSearchMaxBytes-4096), ""},
		{"records past one search's 200 entries", 0, [][]byte{facPage("", fqManyRecords(SoRSearchMaxEntries+1)...)}, bound},
	}
	for _, level := range bindLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.answering(t, "federated-query")
			p.g.cfg.ConsentURL = "http://consent.test"
			p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
			for _, row := range rows {
				sor := &facilitySoR{censusSoR: newCensusSoR(), patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": row.pages}}
				sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
				p.g.cfg.SoR = sor
				pci, _, ok := sor.ResolvePatient("MBR-UC05")
				if !ok {
					t.Fatal("the facility resolves no patient for MBR-UC05")
				}
				task := cdexTaskFor(t, "MBR-UC05")
				if row.task > 0 {
					task = replaceOnce(t, task, `"resourceType":"Task"`, `"resourceType":"Task","note":[{"text":"x","n":[`+strings.TrimSuffix(strings.Repeat("0,", row.task), ",")+`]}]`)
				}
				got.reset()
				ans := sendConsented(t, p, "federated-query", task, pci, "Consent/c-1")
				rec := got.only(t)
				if row.refused == "" {
					if ans.status != http.StatusOK || rec.Outcome != ExchangeAnswered || !bytes.Contains(ans.body, []byte(`"output"`)) {
						t.Fatalf("%s: answered %d (%s), want the Task fulfilled", row.name, ans.status, rec.Outcome)
					}
					continue
				}
				if ans.status != http.StatusBadGateway || !bytes.Contains(ans.body, []byte(row.refused)) {
					t.Fatalf("%s: answered %d %.300s, want 502 %q", row.name, ans.status, ans.body, row.refused)
				}
				// Nothing of the fulfillment is sent: no output, no record.
				for _, part := range []string{`"output"`, `"DiagnosticReport"`, `dr-1`, `dr-a`, `dr-b`, `fac-77`} {
					if bytes.Contains(ans.body, []byte(part)) {
						t.Fatalf("%s: the refusal carries %s of the fulfillment: %.300s", row.name, part, ans.body)
					}
				}
				if v := ans.frame["Retry-After"]; v != "" {
					t.Fatalf("%s: Retry-After %q, want none (no retry succeeds until the records change)", row.name, v)
				}
				if rec.Outcome != ExchangeRefused || rec.Status != http.StatusBadGateway || rec.Rule != RefusalFidelity || rec.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("%s: record %s %s/%s status %d, want refused 502 %s/%s", row.name, rec.Outcome, rec.Rule, rec.RefusalStage, rec.Status, RefusalFidelity, vocab.RefusalStageBuild)
				}
			}
		})
	}
}

// sealCDexFulfillment returns a fulfillment past the gateway's own carrying
// limits, the SDK's (ErrCDexLimit) or the relay's on the sealed answer
// (relay.ErrTooLarge), as a *cdexBuildPastLimits, for each limit: depth,
// tokens and size, which a request reaches past the records' own reads only
// through some of them; any other error stays the facility's own build fault.
func TestSealCDexFulfillment_PastTheCarryingLimitsIsTyped(t *testing.T) {
	task := cdexTaskFor(t, "MBR-UC05")
	records := func(note string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"urn:shn:fedquery:0","resource":{"resourceType":"Observation","id":"o","status":"final","code":{"text":"x"},"note":[` + note + `]}}]}`)
	}
	for name, recs := range map[string][]byte{
		"depth":  records(strings.Repeat(`{"a":`, 66) + `1` + strings.Repeat(`}`, 66)),
		"tokens": records(`{"text":"x","x":[` + strings.TrimSuffix(strings.Repeat(`0,`, 1_000_100), ",") + `]}`),
		"size":   records(`{"text":"` + strings.Repeat("x", 17<<20) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := sealCDexFulfillment(task, recs)
			var past *cdexBuildPastLimits
			if !errors.As(err, &past) {
				t.Fatalf("err %v (%T), want a *cdexBuildPastLimits", err, err)
			}
		})
	}
	if _, _, err := sealCDexFulfillment(task, records(`{"text":"x"}`)); err != nil {
		t.Fatalf("control: err %v, want the fulfillment built", err)
	}
	var past *cdexBuildPastLimits
	if !errors.As(pastLimitsOr(fmt.Errorf("seal: %w", relay.ErrTooLarge)), &past) {
		t.Fatal("the relay's carrying limits are not typed as past the limits")
	}
	other := errors.New("other")
	if got := pastLimitsOr(other); got != other {
		t.Fatalf("pastLimitsOr(other) = %v, want other unchanged", got)
	}
}

// Each path's reason is the text the docs state, and says only what is true of
// its path: the fulfillment's names the request and the records as either
// cause, the records Bundle's and the bound's the records alone.
func TestFederatedQuery_PastTheLimitsReasonsAreAsDocumented(t *testing.T) {
	for got, want := range map[string]string{
		refusalCDexFulfillmentPastLimits:                "federated query refused: the fulfillment is past the gateway's own carrying limits; it cannot be answered until the request or the facility's records change",
		refusalCDexRecordsPastLimits:                    "federated query refused: the facility's records are past the gateway's own carrying limits; it cannot be answered until the facility's records change",
		refusalCDexRecordsPastBound("DiagnosticReport"): "federated query refused: the facility's DiagnosticReport records are past the gateway's per-answer bound (10 pages, 200 entries or 4 MiB); it cannot be answered until the facility's records change",
	} {
		if got != want {
			t.Errorf("reason %q, want %q", got, want)
		}
	}
}
