package engine

import (
	"context"
	"encoding/json"
	"fmt"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Snapshots are capability metadata, never empty-findings verdicts.
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
		} else if s.Availability["structural"] != "available" || s.Availability["profile"] != "unavailable" || s.Availability["terminology"] != "unavailable" || s.Availability["patientConsistency"] != "unavailable" {
			t.Fatal(s)
		}
	}
}
func TestConformanceStatusQualifiedScopeAndAllowlist(t *testing.T) {
	lane := NewDiscoveredLane("2.1", "http://private-host", nil)
	g := &Gateway{cfg: Config{HolderID: "private-holder", ConformanceEnforcement: EnforcementStrict, DefaultValidatorsByLine: map[string]*DiscoveredLane{"2.1": lane}}}
	if got := g.ConformanceStatus().Availability["profile"]; got != "unavailable" {
		t.Fatal(got)
	}
	if err := lane.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	s := g.ConformanceStatus()
	if s.Availability["profile"] != "unavailable" || s.Availability["terminology"] != "unavailable" {
		t.Fatal(s)
	}
	b, _ := json.Marshal(s)
	var keys map[string]json.RawMessage
	json.Unmarshal(b, &keys)
	if len(keys) != 4 {
		t.Fatalf("unexpected status fields: %s", b)
	}
	for _, k := range []string{"level", "ruleSet", "availability", "dropped"} {
		if _, ok := keys[k]; !ok {
			t.Fatal(k)
		}
	}
}

func TestFetchConformanceStatusClosedBoundedObject(t *testing.T) {
	good := `{"conformance":{"level":"strict","ruleSet":"participant-conformance/5","availability":{"structural":"available","builtInDeep":"available","patientConsistency":"unavailable","profile":"partial","terminology":"unavailable","patient":"secret"},"dropped":3,"findings":["private-patient"],"payload":"private-patient"},"patient":"private-patient"}`
	for _, tc := range []struct {
		name, body string
		status     int
		level      string
	}{
		{"allowlist", good, 200, "strict"}, {"previous rule set 4", strings.Replace(good, "participant-conformance/5", "participant-conformance/4", 1), 200, "strict"}, {"previous rule set 3", strings.Replace(good, "participant-conformance/5", "participant-conformance/3", 1), 200, "strict"}, {"previous rule set 2", strings.Replace(good, "participant-conformance/5", "participant-conformance/2", 1), 200, "strict"}, {"previous rule set 1", strings.Replace(good, "participant-conformance/5", "participant-conformance/1", 1), 200, "strict"}, {"future rule set", strings.Replace(good, "participant-conformance/5", "participant-conformance/6", 1), 200, ""}, {"old", `{"status":"ok"}`, 200, ""}, {"unknown", strings.Replace(good, `"strict"`, `"future"`, 1), 200, ""}, {"too large", good + strings.Repeat(" ", 64<<10), 200, ""}, {"down", good, 503, ""}, {"redirect", good, 302, ""}, {"malformed", "{", 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.ContentLength != 0 || r.Body != nil && r.Body != http.NoBody {
					t.Error("operational status read must not send a payload")
				}
				if r.URL.Path != "/health" {
					t.Error(r.URL.Path)
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

func TestConformanceStatusRecentExecution(t *testing.T) {
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Clock: func() time.Time { return now }}}
	ev := shnsdk.ValidationEvidence{ExecutionAttempted: true, Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationInvalid}, Terminology: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationUnavailable}}
	g.recordCheckerAvailability("pa.pas", "2.0", ev)
	if got := g.ConformanceStatus().Availability; got["profile"] != "partial" || got["terminology"] != "unavailable" {
		t.Fatal(got)
	}
	ev.Profile.State = shnsdk.ValidationUnavailable
	g.recordCheckerAvailability("pa.pas", "2.0", ev)
	if got := g.ConformanceStatus().Availability["profile"]; got != "unavailable" {
		t.Fatal(got)
	}
	ev.Profile.State = shnsdk.ValidationValid
	g.recordCheckerAvailability("pa.pas", "2.0", ev)
	now = now.Add(5 * time.Minute)
	if got := g.ConformanceStatus().Availability["profile"]; got != "unavailable" {
		t.Fatal("expired", got)
	}
	g.recordCheckerAvailability("patient-private", "unknown", ev)
	if got := g.ConformanceStatus().Availability["profile"]; got != "unavailable" {
		t.Fatal("unknown scope", got)
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
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				g.recordCheckerAvailability("pa.crd", "2.1", shnsdk.ValidationEvidence{ExecutionAttempted: true, Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid}})
				g.ConformanceStatus()
			}
		}()
	}
	wg.Wait()
}

func TestConformanceStatusActualCheckerEvidence(t *testing.T) {
	v := syntheticEvidenceValidatorFunc(func([]byte) (shnsdk.Result, error) { return shnsdk.Result{Valid: false}, nil })
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Validator: v}}
	g.contentValidation(context.Background(), deepInput(`{"resourceType":"Bundle","entry":[]}`))
	if s := g.ConformanceStatus(); s.Availability["profile"] != "partial" {
		t.Fatalf("completed invalid check was not recorded: %+v", s)
	}
	g.cfg.Validator = observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
		ev := unavailableValidatorEvidence()
		ev.ExecutionAttempted = true
		return ev, fmt.Errorf("controlled attempted outage")
	})
	g.contentValidation(context.Background(), deepInput(`{"resourceType":"Bundle","entry":[]}`))
	if s := g.ConformanceStatus(); s.Availability["profile"] != "unavailable" {
		t.Fatalf("execution outage did not supersede success: %+v", s)
	}
}

func TestConformanceStatusDoesNotInferExecutionFromLocalInput(t *testing.T) {
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Validator: failingValidator{}}}
	g.recordCheckerAvailability("pa.pas", "2.0", shnsdk.ValidationEvidence{ExecutionAttempted: true, Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid}})
	for _, in := range []CheckInput{deepInput(`not-json`), deepInput(`{"resourceType":"Bundle","entry":[]}`)} {
		in.DeclaredVersion = ""
		g.contentValidation(context.Background(), in)
		if got := g.ConformanceStatus().Availability["profile"]; got != "partial" {
			t.Fatal("local input gap claimed checker outage", got)
		}
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
			t.Fatal("overfull observation queue admitted job")
		}
		if g.ConformanceStatus().Dropped != 1 {
			t.Fatal("dropped observation job hidden")
		}
	}
}

// Local serialization/budget failure is not checker execution or an outage.
func TestConformanceStatusLocalTargetFailureAfterExecution(t *testing.T) {
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve}}
	calls := 0
	g.cfg.Validator = observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
		calls++
		g.observationMemory.mu.Lock()
		g.observationMemory.bytes = observationBodyBudget
		g.observationMemory.mu.Unlock()
		return *syntheticEvidence(), nil
	})
	in := deepInput(`{"resourceType":"Parameters","parameter":[{"resource":{"resourceType":"Bundle"}},{"resource":{"resourceType":"Bundle","id":"larger-second-target"}}]}`)
	in.Direction = "response"
	in.Exchange.legType = "pas-claim-inquire"
	in.observation = &g.observationMemory
	result := g.contentValidation(context.Background(), in)
	if calls != 1 || result.Profile.State != shnsdk.ValidationUnavailable {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
	if got := g.ConformanceStatus().Availability["profile"]; got != "partial" {
		t.Fatalf("local budget failure erased actual execution: %s", got)
	}
}

func TestConformanceStatusConcurrentExecutionOrdering(t *testing.T) {
	for _, tc := range []struct {
		name        string
		old, recent shnsdk.ValidationEvidence
		want        string
	}{
		{"delayed success after outage", *validContentEvidence(), unavailableValidatorEvidence(), "unavailable"},
		{"delayed outage after evaluated execution", unavailableValidatorEvidence(), *validContentEvidence(), "partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.old.ExecutionAttempted = true
			tc.recent.ExecutionAttempted = true
			old := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Clock: func() time.Time {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
					return old
				}
				return old.Add(time.Second)
			}}}
			done := make(chan struct{})
			go func() { defer close(done); g.recordCheckerAvailability("pa.pas", "2.0", tc.old) }()
			<-entered
			g.recordCheckerAvailability("pa.pas", "2.0", tc.recent)
			close(release)
			<-done
			if got := g.ConformanceStatus().Availability["profile"]; got != tc.want {
				t.Fatalf("older execution erased newer result: got %s want %s", got, tc.want)
			}
		})
	}
}

func TestConformanceStatusAdapterLocalInputPreservesExecution(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`)
	}))
	defer srv.Close()
	v := shnsdk.NewOperationValidator(srv.URL)
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Validator: v}}
	g.contentValidation(context.Background(), deepInput(`{"resourceType":"Bundle","entry":[]}`))
	if got := g.ConformanceStatus().Availability["profile"]; got != "partial" {
		t.Fatal(got)
	}
	for _, body := range []string{`{}`, `{"resourceType":123}`} {
		result := g.contentValidation(context.Background(), deepInput(body))
		if result.Profile.State != shnsdk.ValidationUnavailable || requests.Load() != 1 {
			t.Fatalf("result=%+v requests=%d", result, requests.Load())
		}
		if got := g.ConformanceStatus().Availability["profile"]; got != "partial" {
			t.Errorf("adapter-local input erased actual execution: %s", got)
		}
	}
}

type checkerTransportFunc func(*http.Request) (*http.Response, error)

func (f checkerTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConformanceStatusAttemptProvenanceThroughWrapper(t *testing.T) {
	var requests atomic.Int32
	var outage atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`)
	}))
	defer srv.Close()
	v := shnsdk.NewOperationValidator(srv.URL)
	transport := srv.Client().Transport
	v.Client = &http.Client{Transport: checkerTransportFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if outage.Load() {
			return nil, fmt.Errorf("controlled transport outage")
		}
		return transport.RoundTrip(r)
	})}
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve}}
	wrapped := observingValidator{inner: v, g: g}
	g.cfg.Validator = wrapped
	valid := `{"resourceType":"Bundle","entry":[]}`
	g.contentValidation(context.Background(), deepInput(valid))
	for _, body := range []string{`{}`, `{"resourceType":123}`} {
		ev, err := wrapped.ValidateEvidence(context.Background(), []byte(body), "")
		if err == nil || ev.ExecutionAttempted {
			t.Fatalf("local result=%+v err=%v", ev, err)
		}
		g.contentValidation(context.Background(), deepInput(body))
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ev, err := wrapped.ValidateEvidence(canceled, []byte(valid), "")
	if err == nil || ev.ExecutionAttempted || requests.Load() != 1 {
		t.Fatalf("canceled result=%+v err=%v requests=%d", ev, err, requests.Load())
	}
	if g.ConformanceStatus().Availability["profile"] != "partial" {
		t.Fatal("local input/cancellation erased prior evidence")
	}
	outage.Store(true)
	g.contentValidation(context.Background(), deepInput(valid))
	if requests.Load() != 2 || g.ConformanceStatus().Availability["profile"] != "unavailable" {
		t.Fatal("attempted outage was lost through wrapper/normalization")
	}
	// A real local checker also supplies execution provenance, without HTTP.
	g.cfg.Validator = observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
		return shnsdk.ValidationEvidence{ExecutionAttempted: true, Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationInvalid}}, nil
	})
	g.contentValidation(context.Background(), deepInput(valid))
	if g.ConformanceStatus().Availability["profile"] != "partial" || requests.Load() != 2 {
		t.Fatal("local checker execution was not recognized")
	}
	// An older/custom implementation without provenance cannot establish outage.
	g.cfg.Validator = failingValidator{}
	g.contentValidation(context.Background(), deepInput(valid))
	if g.ConformanceStatus().Availability["profile"] != "partial" {
		t.Fatal("unproven implementation erased evidence")
	}
}

func TestConformanceStatusConservativeMetadataBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	cfg := Config{ConformanceEnforcement: EnforcementObserve, Clock: func() time.Time { return now }}
	g := &Gateway{cfg: cfg}
	g.recordCheckerAvailability("pa.pas", "2.0", *validContentEvidence())
	if g.ConformanceStatus().Availability["profile"] != "unavailable" {
		t.Fatal("unproven local synthesis established coverage")
	}
	ev := *syntheticEvidence()
	ev.Profile.State = shnsdk.ValidationNotApplicable
	ev.Terminology.State = shnsdk.ValidationNotApplicable
	g.recordCheckerAvailability("pa.pas", "2.0", ev)
	if g.ConformanceStatus().Availability["profile"] != "unavailable" {
		t.Fatal("inapplicability established coverage")
	}
	g.recordCheckerAvailability("pa.pas", "2.0", *syntheticEvidence())
	fresh := &Gateway{cfg: cfg}
	if fresh.ConformanceStatus().Availability["profile"] != "unavailable" {
		t.Fatal("new instance inherited evidence")
	}
	now = now.Add(-time.Second)
	if g.ConformanceStatus().Availability["profile"] != "unavailable" {
		t.Fatal("future timestamp established coverage")
	}
}
