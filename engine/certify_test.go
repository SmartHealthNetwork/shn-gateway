package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type certificationValidatorFunc func(context.Context, []byte, string) (shnsdk.Result, error)

func (f certificationValidatorFunc) Validate(c context.Context, b []byte, p string) (shnsdk.Result, error) {
	return f(c, b, p)
}
func certificationGateway(t *testing.T, v shnsdk.Validator, observer func(ObserverEvent)) *Gateway {
	t.Helper()
	g := &Gateway{cfg: Config{Clock: time.Now, Observer: observer, CertificationValidatorsByLine: map[string]shnsdk.Validator{"2.0": v, "2.1": v, "2.2": v}}}
	g.startCertification()
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// lockedBuffer is a log destination the certification worker may write to
// while a test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Certification evidence is a conformance check: at none it is not gathered
// (no validator call, no certify: line, no leg.certified event); at observe it
// is.
func TestCertificationEvidenceOnlyWhereChecksRun(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
		t.Run(level.String(), func(t *testing.T) {
			var calls atomic.Int32
			v := certificationValidatorFunc(func(context.Context, []byte, string) (shnsdk.Result, error) {
				calls.Add(1)
				return shnsdk.Result{Valid: true}, nil
			})
			var logged lockedBuffer
			previous := log.Writer()
			log.SetOutput(&logged)
			defer log.SetOutput(previous)
			var mu sync.Mutex
			certified := 0
			g := &Gateway{cfg: Config{Clock: time.Now, ConformanceEnforcement: level, Observer: func(e ObserverEvent) {
				if e.Kind == "leg.certified" {
					mu.Lock()
					certified++
					mu.Unlock()
				}
			}, CertificationValidatorsByLine: map[string]shnsdk.Validator{"2.0": v, "2.1": v, "2.2": v}}}
			g.startCertification()
			t.Cleanup(func() { _ = g.Close() })
			certificationSubmit(g, "synthetic")
			certificationFlush(t, g)
			mu.Lock()
			defer mu.Unlock()
			evidence := g.CertificationEvidenceForTest()
			lines := strings.Contains(logged.String(), "certify: ")
			switch level {
			case EnforcementNone:
				if calls.Load() != 0 || certified != 0 || len(evidence) != 0 || lines {
					t.Fatalf("at none no evidence is gathered: %d call(s), %d event(s), %d record(s), certify line %v", calls.Load(), certified, len(evidence), lines)
				}
			case EnforcementObserve:
				if calls.Load() == 0 || certified != 1 || len(evidence) != 1 || !lines {
					t.Fatalf("at observe evidence is gathered: %d call(s), %d event(s), %d record(s), certify line %v", calls.Load(), certified, len(evidence), lines)
				}
			}
		})
	}
}

func certificationFlush(t *testing.T, g *Gateway) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.FlushCertificationForTest(ctx); err != nil {
		t.Fatal(err)
	}
}
func certificationSubmit(g *Gateway, id string) {
	g.enqueueCertification(certificationJob{evidence: CertificationEvidence{LegType: "pas-claim", Seam: "provider-ingress", Direction: "request", CorrelationID: id, TargetLine: "2.1"}, payload: []byte(`{"resourceType":"Claim"}`)})
}

// A line whose client answers CertificationLaneUnavailable is recorded
// unavailable with that authored text as written — not the hashed form a
// validator failure gets — so the evidence names the lane to configure.
func TestCertificationLaneUnavailableIsStatedNotHashed(t *testing.T) {
	v := certificationValidatorFunc(func(_ context.Context, _ []byte, profile string) (shnsdk.Result, error) {
		if strings.Contains(profile, "|2.1.") {
			return shnsdk.Result{}, &CertificationLaneUnavailable{Reason: "FHIR_VALIDATE_URL_2_1 is not configured and the default lane has not qualified"}
		}
		return shnsdk.Result{Valid: true}, nil
	})
	g := certificationGateway(t, v, nil)
	certificationSubmit(g, "synthetic")
	certificationFlush(t, g)
	e := g.CertificationEvidenceForTest()
	if len(e) != 1 {
		t.Fatalf("%+v", e)
	}
	for _, verdict := range e[0].Verdicts {
		switch verdict.Line {
		case "2.1":
			if verdict.State != "unavailable" || verdict.Error != "certification validator unavailable: FHIR_VALIDATE_URL_2_1 is not configured and the default lane has not qualified" {
				t.Fatalf("2.1 verdict %+v", verdict)
			}
		default:
			if verdict.State != "valid" {
				t.Fatalf("%s verdict %+v", verdict.Line, verdict)
			}
		}
	}
}

func TestCertificationSpeciesProfiles(t *testing.T) {
	for _, row := range []struct{ raw, species, profile string }{
		{`{"resourceType":"Claim"}`, "Claim", "profile-claim"},
		{`{"resourceType":"ClaimResponse"}`, "ClaimResponse", "profile-claimresponse"},
		{`{"resourceType":"QuestionnaireResponse"}`, "QuestionnaireResponse", "dtr-questionnaireresponse"},
		{`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`, "PASRequestBundle", "profile-pas-request-bundle"},
		{`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`, "PASResponseBundle", "profile-pas-response-bundle"},
	} {
		t.Run(row.species, func(t *testing.T) {
			if got := detectSpecies([]byte(row.raw)); got != row.species {
				t.Fatalf("species %q", got)
			}
			for _, line := range []string{"2.0", "2.1", "2.2"} {
				p, ok := profileFor(row.species, line, "pas-claim")
				d, _ := shnsdk.PASLineDef(line)
				version := d.PackageVersion
				if row.species == "QuestionnaireResponse" {
					d, _ := shnsdk.DTRLineDef(line)
					version = d.PackageVersion
				}
				if !ok || !strings.HasSuffix(p, "/"+row.profile+"|"+version) {
					t.Fatalf("profile %q %v", p, ok)
				}
			}
		})
	}
	for _, raw := range []string{"", `null`, `[]`, `{`, `{"resourceType":"Parameters"}`, `{"resourceType":"Bundle"}`, `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Questionnaire"}}]}`} {
		if got := detectSpecies([]byte(raw)); got != "" {
			t.Errorf("unsupported %q -> %q", raw, got)
		}
	}
	for _, row := range [][3]string{{"Claim", "9.9", "pas-claim"}, {"x", "2.0", "pas-claim"}, {"Claim", "2.0", "other"}} {
		if p, ok := profileFor(row[0], row[1], row[2]); ok || p != "" {
			t.Fatal(row, p, ok)
		}
	}
	p, _ := profileFor("Claim", "2.1", "pas-claim-update")
	if !strings.Contains(p, "/profile-claim-update|") {
		t.Fatal(p)
	}
	p, _ = profileFor("Claim", "2.1", "")
	if strings.Contains(p, "update") {
		t.Fatal(p)
	}
}
func TestCertificationCandidateOrder(t *testing.T) {
	for _, row := range []struct {
		raw  string
		want []string
	}{
		{`{"resourceType":"Claim"}`, []string{"2.2", "2.1", "2.0"}},
		{`{"resourceType":"Claim","meta":{"profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|2.0.1"]}}`, []string{"2.0", "2.2", "2.1"}},
		{`{"resourceType":"Claim","meta":{"profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim"]},"extension":[{"url":"https://example.org/unknown"}]}`, []string{"2.2", "2.1", "2.0"}},
	} {
		if got := candidateOrder("Claim", []byte(row.raw)); !reflect.DeepEqual(got, row.want) {
			t.Fatalf("order %v want %v", got, row.want)
		}
	}
	if got := candidateOrder("unknown", nil); len(got) != 0 {
		t.Fatal(got)
	}
}
func TestCertificationRetainsAllVerdicts(t *testing.T) {
	for _, row := range []struct {
		name          string
		v             shnsdk.Validator
		state, source string
	}{
		{"valid", &shnsdk.FakeValidator{}, "valid", "2.1"},
		{"invalid", &shnsdk.FakeValidator{RejectIfContains: "Claim"}, "invalid", ""},
		{"unavailable", &shnsdk.FakeValidator{Err: errors.New("execution error\nwith spaces")}, "unavailable", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			g := certificationGateway(t, row.v, nil)
			certificationSubmit(g, "synthetic")
			certificationFlush(t, g)
			e := g.CertificationEvidenceForTest()
			if len(e) != 1 || len(e[0].Verdicts) != 3 || e[0].SourceLine != row.source {
				t.Fatalf("%+v", e)
			}
			for _, v := range e[0].Verdicts {
				if v.State != row.state {
					t.Fatal(v)
				}
			}
			e[0].Candidates[0] = "changed"
			e[0].Verdicts[0].Line = "changed"
			if g.CertificationEvidenceForTest()[0].Candidates[0] == "changed" {
				t.Fatal("snapshot aliases ring")
			}
		})
	}
	for _, row := range []struct {
		cert         []string
		target, want string
	}{{[]string{"2.0", "2.2"}, "2.1", "2.2"}, {[]string{"2.0", "2.1"}, "2.2", "2.1"}, {[]string{"2.0", "2.2"}, "", "2.2"}, {nil, "2.0", ""}} {
		if got := certificationSource(row.cert, row.target); got != row.want {
			t.Fatal(got, row)
		}
	}
}
func TestCertificationWorkerBoundsAndBarrier(t *testing.T) {
	held := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	var active, max atomic.Int32
	v := certificationValidatorFunc(func(ctx context.Context, _ []byte, _ string) (shnsdk.Result, error) {
		n := active.Add(1)
		defer active.Add(-1)
		if n > max.Load() {
			max.Store(n)
		}
		once.Do(func() { close(entered) })
		select {
		case <-held:
			return shnsdk.Result{Valid: true}, nil
		case <-ctx.Done():
			return shnsdk.Result{}, ctx.Err()
		}
	})
	var delivered atomic.Int32
	g := certificationGateway(t, v, func(e ObserverEvent) {
		if e.Kind == "leg.certified" {
			delivered.Add(1)
		}
	})
	t.Cleanup(func() { close(held) })
	certificationSubmit(g, "active")
	<-entered
	for i := 0; i < 33; i++ {
		certificationSubmit(g, fmt.Sprint(i))
	}
	e := g.CertificationEvidenceForTest()
	if len(e) != 1 || !strings.Contains(e[0].Verdicts[0].Error, "queue full") {
		t.Fatalf("overflow %+v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.FlushCertificationForTest(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if max.Load() != 1 {
		t.Fatal("concurrent candidate calls", max.Load())
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	certificationFlush(t, g)
	if delivered.Load() != 34 {
		t.Fatalf("overflow completion was not delivered: %d", delivered.Load())
	}
	before := len(g.CertificationEvidenceForTest())
	certificationSubmit(g, "closed")
	if len(g.CertificationEvidenceForTest()) != before+1 {
		t.Fatal("closed observation silently lost")
	}
}
func TestCertificationCooperativeObserver(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := certificationGateway(t, &shnsdk.FakeValidator{}, func(e ObserverEvent) {
		if e.Kind == "leg.certified" {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	t.Cleanup(func() { close(release) })
	certificationSubmit(g, "first")
	<-entered
	if len(g.CertificationEvidenceForTest()) != 1 {
		t.Fatal("completion not stored before callback")
	}
	certificationSubmit(g, "second")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.FlushCertificationForTest(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestCertificationHTTPExecutionClassification(t *testing.T) {
	body := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"java.lang.Error: execution failed"}]}`
	for _, status := range []int{200, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); fmt.Fprint(w, body) }))
			defer srv.Close()
			v := NewCertificationOperationValidator(srv.URL)
			defer v.Client.CloseIdleConnections()
			res, err := v.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "profile")
			if status == 500 {
				var execution *certificationHTTPError
				if !errors.As(err, &execution) || string(execution.raw) != body || strings.Contains(err.Error(), body) {
					t.Fatalf("lost server execution evidence: %+v %v", res, err)
				}
			} else if err != nil || res.Valid || len(res.Issues) != 1 {
				t.Fatal(res, err)
			}
			routing := shnsdk.NewOperationValidator(srv.URL)
			r, e := routing.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "profile")
			if e != nil || r.Valid {
				t.Fatal("routing contract changed", r, e)
			}
		})
	}
}

func TestCertificationExpiryAndRingRetention(t *testing.T) {
	var calls atomic.Int32
	g := certificationGateway(t, certificationValidatorFunc(func(context.Context, []byte, string) (shnsdk.Result, error) {
		calls.Add(1)
		return shnsdk.Result{Valid: true, Issues: []string{"synthetic warning"}}, nil
	}), nil)
	g.enqueueCertification(certificationJob{evidence: CertificationEvidence{LegType: "pas-claim", CorrelationID: "old"}, payload: []byte(`{"resourceType":"Claim"}`), queued: time.Now().Add(-31 * time.Second)})
	certificationFlush(t, g)
	e := g.CertificationEvidenceForTest()
	if calls.Load() != 0 || len(e) != 1 {
		t.Fatal(calls.Load(), e)
	}
	for _, v := range e[0].Verdicts {
		if v.State != "expired" {
			t.Fatal(v)
		}
	}
	for i := 0; i < 260; i++ {
		certificationSubmit(g, fmt.Sprint(i))
		certificationFlush(t, g)
	}
	e = g.CertificationEvidenceForTest()
	if len(e) != 256 || e[0].CorrelationID != "4" || e[255].CorrelationID != "259" {
		t.Fatalf("ring length/order %d %s %s", len(e), e[0].CorrelationID, e[len(e)-1].CorrelationID)
	}
	originalIssue := e[0].Verdicts[0].Issues[0]
	e[0].Verdicts[0].Issues[0] = "mutated"
	if g.CertificationEvidenceForTest()[0].Verdicts[0].Issues[0] != originalIssue {
		t.Fatal("issue snapshot aliases ring")
	}
	_ = g.Close()
	before := calls.Load()
	certificationSubmit(g, "after-close")
	certificationFlush(t, g)
	if calls.Load() != before {
		t.Fatal("validator work after Close")
	}
}
func TestCertificationLimitsAndMalformedResponses(t *testing.T) {
	if certificationQueueCapacity != 32 || certificationRingCapacity != 256 || certificationCandidateTimeout != 2*time.Second || certificationCollectionTimeout != 6*time.Second || certificationQueueMaxAge != 30*time.Second {
		t.Fatal("observation limits changed")
	}
	for _, row := range []struct {
		name, raw string
		status    int
	}{{"echoed-payload", `{"resourceType":"Claim","id":"PRIVATE-SYNTHETIC-SENTINEL"}`, 200}, {"missing-issues", `{"resourceType":"OperationOutcome"}`, 200}, {"unknown-severity", `{"resourceType":"OperationOutcome","issue":[{"severity":"unknown","diagnostics":"PRIVATE-SYNTHETIC-SENTINEL"}]}`, 200}, {"server-echo", `{"resourceType":"Claim","id":"PRIVATE-SYNTHETIC-SENTINEL"}`, 500}} {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(row.status); fmt.Fprint(w, row.raw) }))
			defer srv.Close()
			v := NewCertificationOperationValidator(srv.URL)
			defer v.Client.CloseIdleConnections()
			_, err := v.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "")
			if err == nil || strings.Contains(err.Error(), "PRIVATE-SYNTHETIC-SENTINEL") {
				t.Fatal("raw response leaked or accepted", err)
			}
			var raw *certificationHTTPError
			if !errors.As(err, &raw) || string(raw.raw) != row.raw {
				t.Fatal("bounded raw evidence missing")
			}
		})
	}
}
func TestCertificationCandidateTimeout(t *testing.T) {
	var calls atomic.Int32
	g := certificationGateway(t, certificationValidatorFunc(func(ctx context.Context, _ []byte, _ string) (shnsdk.Result, error) {
		calls.Add(1)
		<-ctx.Done()
		return shnsdk.Result{}, ctx.Err()
	}), nil)
	certificationSubmit(g, "timeout")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := g.FlushCertificationForTest(ctx); err != nil {
		t.Fatal(err)
	}
	records := g.CertificationEvidenceForTest()
	if len(records) != 1 || len(records[0].Verdicts) != 3 {
		t.Fatal(records)
	}
	for _, v := range records[0].Verdicts {
		if v.State != "expired" {
			t.Fatal(v)
		}
	}
	if calls.Load() > 3 {
		t.Fatal("unbounded candidate calls")
	}
}
func TestCertificationCallbackCloseJoinsAfterRelease(t *testing.T) {
	entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := certificationGateway(t, &shnsdk.FakeValidator{}, func(e ObserverEvent) {
		if e.Kind == "leg.certified" {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	certificationSubmit(g, "first")
	<-entered
	go func() { _ = g.Close(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("Close abandoned held observer")
	default:
	}
	unblock()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("Close failed to join released observer")
	}
	certificationFlush(t, g)
}

func TestCertificationNativeRetryCaptureClearsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		fmt.Fprint(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"error"}]}`)
	}))
	n := &nativeResponder{client: srv.Client()}
	capture := &nativeCertificationCapture{}
	ctx := context.WithValue(context.Background(), nativeCertificationKey{}, capture)
	first := []byte(`{"resourceType":"Claim","id":"first"}`)
	_, bad, err := n.post(ctx, srv.URL, "", testRequest(first), "pas-claim", "synthetic")
	if err != nil || bad.Status != 409 || len(capture.response) == 0 {
		t.Fatal(bad, err, capture)
	}
	srv.Close()
	second := []byte(`{"resourceType":"Claim","id":"second"}`)
	_, _, err = n.post(ctx, srv.URL, "", testRequest(second), "pas-claim", "synthetic retry")
	if err == nil || !bytes.Equal(capture.request, second) || capture.response != nil {
		t.Fatal("stale attempt paired with failed retry", err, capture)
	}
}

func TestCertificationMarkersOnlyReorder(t *testing.T) {
	raw := []byte(`{"resourceType":"Claim","item":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-certificationType","valueCode":"synthetic"}]}]}`)
	if got := candidateOrder("Claim", raw); !reflect.DeepEqual(got, []string{"2.1", "2.2", "2.0"}) {
		t.Fatal("known marker did not reorder", got)
	}
	g := certificationGateway(t, &shnsdk.FakeValidator{}, nil)
	g.enqueueCertification(certificationJob{evidence: CertificationEvidence{LegType: "pas-claim"}, payload: raw})
	certificationFlush(t, g)
	if len(g.CertificationEvidenceForTest()[0].Certified) != 3 {
		t.Fatal("marker rejected a candidate")
	}
}

func TestCertificationCannotChangeRoutingReadiness(t *testing.T) {
	g := certificationGateway(t, &shnsdk.FakeValidator{Err: errors.New("evidence unavailable")}, nil)
	g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": &shnsdk.FakeValidator{}}
	before := g.ValidatorReadinessForTest()
	certificationSubmit(g, "first")
	certificationFlush(t, g)
	if !reflect.DeepEqual(before, g.ValidatorReadinessForTest()) || !before["2.0"] || before["2.1"] {
		t.Fatal("evidence changed independent routing readiness")
	}
}

func TestCertificationRejectionBounds(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		g := &Gateway{cfg: Config{Clock: time.Now}}
		DisableCertificationForTest(&g.cfg)
		g.startCertification()
		certificationSubmit(g, "disabled")
		certificationFlush(t, g)
		if g.certification != nil || len(g.CertificationEvidenceForTest()) != 0 {
			t.Fatal("disabled collection started")
		}
		_ = g.Close()
	})
	t.Run("payload-limit", func(t *testing.T) {
		g := certificationGateway(t, &shnsdk.FakeValidator{}, nil)
		raw := append([]byte(`{"resourceType":"Claim"}`), bytes.Repeat([]byte(" "), shnsdk.MaxRequestBytes)...)
		g.enqueueCertification(certificationJob{evidence: CertificationEvidence{LegType: "pas-claim"}, payload: raw})
		certificationFlush(t, g)
		e := g.CertificationEvidenceForTest()
		if len(e) != 1 {
			t.Fatal("missing oversized metadata")
		}
		for _, v := range e[0].Verdicts {
			if v.State != "unavailable" || v.Error != "payload exceeds observation limit" {
				t.Fatal(v)
			}
		}
	})
	t.Run("response-limit", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), shnsdk.MaxResponseBytes+1))
		}))
		defer srv.Close()
		v := NewCertificationOperationValidator(srv.URL)
		defer v.Client.CloseIdleConnections()
		_, err := v.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "")
		if err == nil || !strings.Contains(err.Error(), "response exceeds limit") {
			t.Fatal(err)
		}
	})
	t.Run("notification-retention", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		g := certificationGateway(t, &shnsdk.FakeValidator{}, func(e ObserverEvent) { once.Do(func() { close(entered) }); <-release })
		var released sync.Once
		unblock := func() { released.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		certificationSubmit(g, "held")
		<-entered
		for i := 0; i < 400; i++ {
			certificationSubmit(g, fmt.Sprint(i))
		}
		e := g.CertificationEvidenceForTest()
		if len(e) != 256 || !strings.Contains(e[len(e)-1].Verdicts[0].Error, "observer notifications dropped=") {
			t.Fatal("metadata delivery overflow is not explicit")
		}
		g.certification.mu.Lock()
		if len(g.certification.queue) != 32 || len(g.certification.notices) != 256 {
			t.Error("unbounded retention")
		}
		g.certification.mu.Unlock()
		unblock()
		certificationFlush(t, g)
	})
}

func TestCertificationExternalDiagnosticsStayPrivate(t *testing.T) {
	const sentinel = "PRIVATE-SYNTHETIC-SENTINEL"
	for _, row := range []struct {
		name, diagnostics, state string
		custom                   bool
	}{
		{"normal", `"` + sentinel + `"`, "invalid", false},
		{"object", `{"foreign":"` + sentinel + `"}`, "unavailable", false},
		{"array", `["` + sentinel + `"]`, "unavailable", false},
		{"custom-error-and-issues", "", "unavailable", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			raw := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","diagnostics":` + row.diagnostics + `}]}`
			var validator shnsdk.Validator
			if row.custom {
				validator = certificationValidatorFunc(func(context.Context, []byte, string) (shnsdk.Result, error) {
					return shnsdk.Result{Issues: []string{strings.Repeat(sentinel, 10000), sentinel}}, errors.New(strings.Repeat(sentinel, 10000))
				})
			} else {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, raw) }))
				defer srv.Close()
				validator = NewCertificationOperationValidator(srv.URL)
				// Keep bounded raw diagnostic proof separate from metadata, including malformed OO.
				result, err := validator.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "")
				if row.state == "invalid" {
					if err != nil || result.Valid || !reflect.DeepEqual(result.Issues, []string{sentinel}) {
						t.Fatal("ordinary SDK classification changed")
					}
				} else {
					var diagnostic *certificationHTTPError
					if !errors.As(err, &diagnostic) || string(diagnostic.raw) != raw {
						t.Error("separate bounded raw diagnostic lost")
					}
				}
			}
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			var observed []string
			g := certificationGateway(t, validator, func(e ObserverEvent) {
				if e.Kind == "leg.certified" {
					observed = append(observed, e.Detail)
				}
			})
			defer g.Close()
			certificationSubmit(g, "privacy")
			certificationFlush(t, g)
			records := g.CertificationEvidenceForTest()
			if len(records) != 1 || len(records[0].Verdicts) != 3 || len(observed) != 1 {
				t.Fatal("missing evidence")
			}
			ring, _ := json.Marshal(records[0])
			for name, metadata := range map[string]string{"ring": string(ring), "observer": observed[0], "log": output.String()} {
				if strings.Contains(metadata, sentinel) || strings.Contains(metadata, raw) || len(metadata) > 4096 {
					t.Errorf("%s retained foreign bytes or unbounded metadata", name)
				}
			}
			if observed[0] != string(ring) || !strings.Contains(output.String(), "certify: "+string(ring)) {
				t.Error("outputs differ")
			}
			for _, verdict := range records[0].Verdicts {
				if verdict.State != row.state || verdict.Valid {
					t.Error("classification changed")
				}
				if len(verdict.Issues) > 1 || len(verdict.Error) > 256 {
					t.Error("unbounded external metadata")
				}
				if row.state == "invalid" && (len(verdict.Issues) != 1 || !strings.Contains(verdict.Issues[0], "count=1 bytes=")) {
					t.Error("missing issue metadata")
				}
				if row.state == "unavailable" && !strings.Contains(verdict.Error, "sha256=") {
					t.Error("missing error digest")
				}
			}
		})
	}
}

func TestCertificationFlushRemainsQueueOnly(t *testing.T) {
	g := certificationGateway(t, &shnsdk.FakeValidator{}, nil)
	done := g.operations.begin()
	defer done()
	certificationSubmit(g, "queue-only")
	certificationFlush(t, g)
	g.operations.mu.Lock()
	defer g.operations.mu.Unlock()
	if len(g.operations.pending) != 1 {
		t.Fatal("queue flush consumed a live operation")
	}
}
