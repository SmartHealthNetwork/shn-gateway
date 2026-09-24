package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestConformanceStatusCoverage(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		g := &Gateway{cfg: Config{ConformanceEnforcement: level}}
		s := g.ConformanceStatus()
		if s.Level != level.String() || s.RuleSet != ConformanceRuleSet || s.Dropped != 0 {
			t.Fatal(s)
		}
		if level == EnforcementNone {
			for _, v := range s.Availability {
				if v != "disabled" {
					t.Fatal(s)
				}
			}
		} else if s.Availability["structural"] != "available" || s.Availability["profile"] != "unavailable" {
			t.Fatal(s)
		}
		if _, present := s.Availability["terminology"]; present {
			t.Fatalf("independent terminology claim remains: %+v", s)
		}
	}
}

func TestFetchConformanceStatusClosedBoundedObject(t *testing.T) {
	good := `{"conformance":{"level":"strict","ruleSet":"` + ConformanceRuleSet + `","availability":{"structural":"available","builtInDeep":"available","profile":"partial","patient":"secret"},"dropped":3,"findings":["private-patient"],"payload":"private-patient"},"patient":"private-patient"}`
	for _, tc := range []struct {
		name, body string
		status     int
		level      string
	}{
		{"allowlist", good, 200, "strict"},
		{"future rule set", strings.Replace(good, ConformanceRuleSet, "participant-conformance/99", 1), 200, ""},
		{"old", `{"status":"ok"}`, 200, ""},
		{"unknown", strings.Replace(good, `"strict"`, `"future"`, 1), 200, ""},
		{"too large", good + strings.Repeat(" ", 64<<10), 200, ""},
		{"down", good, 503, ""},
		{"redirect", good, 302, ""},
		{"malformed", "{", 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/health" {
					t.Errorf("%s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			got := FetchConformanceStatus(context.Background(), srv.Client(), srv.URL)
			if got.Level != tc.level {
				t.Fatalf("got %+v", got)
			}
			b, _ := json.Marshal(got)
			if strings.Contains(string(b), "private-patient") || strings.Contains(string(b), "secret") || strings.Contains(string(b), "findings") {
				t.Fatal(string(b))
			}
		})
	}
}

func TestConformanceStatusRecentProfileExecution(t *testing.T) {
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Clock: func() time.Time { return now }}}
	ev := validationEvidence{ExecutionAttempted: true, Profile: validationCheckEvidence{State: validationInvalid}}
	g.recordCheckerAvailability("pa.pas", "2.0", ev)
	if got := g.ConformanceStatus().Availability["profile"]; got != "partial" {
		t.Fatal(got)
	}
	ev.Profile.State = validationUnavailable
	g.recordCheckerAvailability("pa.pas", "2.0", ev)
	if got := g.ConformanceStatus().Availability["profile"]; got != "unavailable" {
		t.Fatal(got)
	}
	ev.Profile.State = validationValid
	g.recordCheckerAvailability("pa.pas", "2.0", ev)
	now = now.Add(5 * time.Minute)
	if got := g.ConformanceStatus().Availability["profile"]; got != "unavailable" {
		t.Fatal(got)
	}
	g.cfg.ConformanceEnforcement = EnforcementNone
	g.recordCheckerAvailability("pa.pas", "2.0", ev)
	if got := g.ConformanceStatus().Availability["profile"]; got != "disabled" {
		t.Fatal(got)
	}
}

func TestConformanceStatusConcurrentSnapshots(t *testing.T) {
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Clock: time.Now}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				g.recordCheckerAvailability("pa.crd", "2.1", validationEvidence{ExecutionAttempted: true, Profile: validationCheckEvidence{State: validationValid}})
				g.ConformanceStatus()
			}
		}()
	}
	wg.Wait()
}

func TestConformanceStatusActualCheckerResult(t *testing.T) {
	v := syntheticValidatorFunc(func([]byte) (shnsdk.Result, error) { return shnsdk.Result{Valid: false}, nil })
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Validator: v}}
	g.contentValidation(context.Background(), deepInput(`{"resourceType":"Bundle","entry":[]}`))
	if got := g.ConformanceStatus().Availability["profile"]; got != "partial" {
		t.Fatal(got)
	}
	g.cfg.Validator = failingValidator{}
	g.contentValidation(context.Background(), deepInput(`{"resourceType":"Bundle","entry":[]}`))
	if got := g.ConformanceStatus().Availability["profile"]; got != "unavailable" {
		t.Fatal(got)
	}
}

func TestConformanceStatusDroppedJobsScope(t *testing.T) {
	none := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementNone}}
	(&ObserverInspection{gateway: none}).Drop()
	if none.ConformanceStatus().Dropped != 0 {
		t.Fatal("inspection loss reported as conformance work")
	}
	for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementBasic} {
		g := &Gateway{cfg: Config{ConformanceEnforcement: level}, certification: &certificationWorker{queue: make([]certificationJob, certificationQueueCapacity)}}
		if g.observeContent(observationInput(level)) {
			t.Fatal("overfull queue admitted job")
		}
		if g.ConformanceStatus().Dropped != 1 {
			t.Fatal("dropped job hidden")
		}
	}
}
