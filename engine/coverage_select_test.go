package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// coveragesSoR answers OpenCoverageContext with a fixed list (or error); every
// other read comes from the census.
type coveragesSoR struct {
	*censusSoR
	ContextSystemOfRecord
	covs [][]byte
	err  error
}

func (s coveragesSoR) OpenCoverageContext(ctx context.Context, _ string) ([][]byte, error) {
	return s.covs, s.err
}

func newCoveragesSoR(err error, covs ...[]byte) coveragesSoR {
	c := newCensusSoR()
	return coveragesSoR{censusSoR: c, ContextSystemOfRecord: ReadSystemOfRecord(c), covs: covs, err: err}
}

func coverageFor(t *testing.T, id string, payer shnsdk.PayerIdentifier) []byte {
	t.Helper()
	b, err := shnsdk.BuildCoverageWithPayer("Patient/MBR-COVERED", id, payer)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var otherPayer = shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "00078"}

func TestMemberCoverage_Selection(t *testing.T) {
	cms1 := coverageFor(t, "cov-a", shnsdk.CMSPayerIdentity)
	cms2 := coverageFor(t, "cov-b", shnsdk.CMSPayerIdentity)
	other := coverageFor(t, "cov-c", otherPayer)
	unparseable := []byte(`{"resourceType":"Coverage","id":"cov-d","status":"active","beneficiary":{"reference":"Patient/MBR-COVERED"}}`)
	cases := map[string]struct {
		sor        coveragesSoR
		want       []byte
		found      bool
		status     int
		msgContain string
	}{
		"none":                    {sor: newCoveragesSoR(nil), found: false},
		"one":                     {sor: newCoveragesSoR(nil, other), want: other, found: true},
		"one unparseable":         {sor: newCoveragesSoR(nil, unparseable), want: unparseable, found: true},
		"several, one payer":      {sor: newCoveragesSoR(nil, cms1, cms2), want: cms1, found: true},
		"several payers":          {sor: newCoveragesSoR(nil, cms1, other), status: http.StatusUnprocessableEntity, msgContain: "ambiguous coverage"},
		"several, one unreadable": {sor: newCoveragesSoR(nil, cms1, unparseable), status: http.StatusUnprocessableEntity, msgContain: "ambiguous coverage"},
		"read failure":            {sor: newCoveragesSoR(&SoRReadError{Kind: SoRUnavailable}), status: http.StatusServiceUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := &Gateway{cfg: Config{SoR: tc.sor}}
			cov, found, status, msg := g.memberCoverage(context.Background(), "MBR-COVERED")
			if status != tc.status || !strings.Contains(msg, tc.msgContain) {
				t.Fatalf("status=%d msg=%q, want %d containing %q", status, msg, tc.status, tc.msgContain)
			}
			if status != 0 {
				if cov != nil || found {
					t.Fatalf("refusal returned a coverage")
				}
				return
			}
			if found != tc.found || string(cov) != string(tc.want) {
				t.Fatalf("found=%v cov=%s", found, cov)
			}
		})
	}
}

// The origination route refuses a member whose system of record holds
// Coverages naming different payers, instead of routing on whichever the
// server listed first.
func TestScenario_AmbiguousCoverageRefused(t *testing.T) {
	sor := newCoveragesSoR(nil, coverageFor(t, "cov-a", shnsdk.CMSPayerIdentity), coverageFor(t, "cov-c", otherPayer))
	for _, observed := range []bool{false, true} {
		var s SystemOfRecord = sor
		if observed {
			s = observingSoR{inner: sor, clock: time.Now, observer: func(ObserverEvent) {}}
		}
		g := &Gateway{cfg: Config{SoR: s, PayerRouter: payerRouterFor(t, "payer")}}
		w := httptest.NewRecorder()
		g.handleScenario(w, httptest.NewRequest("POST", "/scenario", strings.NewReader(`{"branch":"covered"}`)))
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "ambiguous coverage") {
			t.Fatalf("observed=%v: %d %s", observed, w.Code, w.Body.String())
		}
	}
}

// Every engine read of a member's Coverage goes through memberCoverage, so
// none can pick one of several Coverages on its own.
func TestMemberCoverage_OnlyReader(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`\.OpenCoverageContext\(`)
	allowed := map[string]bool{"sor_context.go": true, "observer.go": true, "sor_read.go": true}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || allowed[f] {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if call.Match(b) {
			t.Errorf("%s reads Coverage directly; use memberCoverage", f)
		}
	}
}

func TestObservingSoR_OpenCoverageReportsEveryMatch(t *testing.T) {
	a := coverageFor(t, "cov-a", shnsdk.CMSPayerIdentity)
	b := coverageFor(t, "cov-c", otherPayer)
	var events []ObserverEvent
	o := observingSoR{inner: newCoveragesSoR(nil, a, b), clock: time.Now, observer: func(e ObserverEvent) { events = append(events, e) }}
	got, err := o.OpenCoverageContext(context.Background(), "MBR-COVERED")
	if err != nil || len(got) != 2 || string(got[0]) != string(a) || string(got[1]) != string(b) {
		t.Fatalf("got %d err %v", len(got), err)
	}
	if len(events) != 1 || events[0].Detail != "found 2 records" || string(events[0].Payload) != "["+string(a)+","+string(b)+"]" {
		t.Fatalf("events = %+v", events)
	}
	// The legacy single-record read keeps its answer for one match and
	// reports nothing for several.
	if cov, found := o.OpenCoverage("MBR-COVERED"); found || cov != nil {
		t.Fatalf("legacy read chose one of several: %s", cov)
	}
}

// oldSignatureSoR is a connector written against the earlier
// ContextSystemOfRecord, whose OpenCoverageContext returned one record.
type oldSignatureSoR struct {
	*censusSoR
}

func (oldSignatureSoR) OpenCoverageContext(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

// A connector with the earlier OpenCoverageContext would silently stop
// satisfying ContextSystemOfRecord, and every read error would become
// "not found". The gateway refuses to start with it instead.
func TestNew_RefusesEarlierOpenCoverageContext(t *testing.T) {
	_, err := New(Config{SoR: oldSignatureSoR{newCensusSoR()}, Store: NewMemStore()})
	if err == nil || !strings.Contains(err.Error(), "OpenCoverageContext") || !strings.Contains(err.Error(), "[][]byte") {
		t.Fatalf("error = %v", err)
	}
	if !errors.Is(err, ErrSystemOfRecordSignature) {
		t.Fatalf("error = %v, want ErrSystemOfRecordSignature", err)
	}
}
