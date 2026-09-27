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
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	"github.com/SmartHealthNetwork/shn-gateway/internal/testrecord"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type certificationValidatorFunc func(context.Context, []byte, string) (shnsdk.Result, error)

func (f certificationValidatorFunc) Validate(c context.Context, b []byte, p string) (shnsdk.Result, error) {
	return f(c, b, p)
}
func certificationGateway(t *testing.T, v shnsdk.Validator, observer func(ObserverEvent)) *Gateway {
	t.Helper()
	return certificationGatewayByLine(t, map[string]shnsdk.Validator{"2.0": v, "2.1": v, "2.2": v}, observer)
}

func certificationGatewayByLine(t *testing.T, validators map[string]shnsdk.Validator, observer func(ObserverEvent)) *Gateway {
	t.Helper()
	g := &Gateway{cfg: Config{Clock: time.Now, Observer: observer, CertificationValidatorsByLine: validators}}
	g.startCertification()
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// certifyRecording loads testdata/recordings/<name>.json: what a real
// validator lane answered the certification client (README.md beside it).
func certifyRecording(t *testing.T, name string) *testrecord.Recording {
	t.Helper()
	return testrecord.Load(t, filepath.Join("testdata", "recordings", name+".json"))
}

// countingLane serves rec strictly and counts the requests it is asked.
func countingLane(t *testing.T, rec *testrecord.Recording, hits *int32) *httptest.Server {
	t.Helper()
	replay := rec.Server()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		replay.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordedAnswer is rec's one answer to a request for profile ("" for none)
// with body: its status, its bytes and the diagnostics of its error and fatal
// issues, in order, as a client reads them.
func recordedAnswer(t *testing.T, rec *testrecord.Recording, profile string, body []byte) (int, []byte, []string) {
	t.Helper()
	var found *testrecord.Exchange
	for i, ex := range rec.Exchanges {
		profiles := ex.Request.Query["profile"]
		if (profile == "" && len(profiles) == 0 || len(profiles) == 1 && profiles[0] == profile) && jsonEqualForTest(ex.Request.Body, body) {
			if found != nil {
				t.Fatalf("two recorded answers for profile %q", profile)
			}
			found = &rec.Exchanges[i]
		}
	}
	if found == nil {
		t.Fatalf("no recorded answer for profile %q and body %s", profile, body)
	}
	var outcome struct {
		Issue []struct{ Severity, Diagnostics string } `json:"issue"`
	}
	if err := json.Unmarshal(found.Response.Body, &outcome); err != nil {
		t.Fatal(err)
	}
	var errs []string
	for _, issue := range outcome.Issue {
		if issue.Severity == "error" || issue.Severity == "fatal" {
			errs = append(errs, issue.Diagnostics)
		}
	}
	return found.Response.Status, found.Response.Body, errs
}

func jsonEqualForTest(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// literalClaim is the payload most rows certify; every recorded lane finds it
// invalid (testdata/recordings/lane-<line>-certify-literal.json).
var literalClaim = []byte(`{"resourceType":"Claim"}`)

// sentinelClaim is a Claim a lane refuses to parse, quoting the foreign value
// back in its diagnostics (testdata/recordings/lane-<line>-certify-collect.json).
var sentinelClaim = []byte(`{"resourceType":"Claim","status":"PRIVATE-SYNTHETIC-SENTINEL"}`)

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
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural} {
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
			case EnforcementObserve, EnforcementStructural:
				if calls.Load() == 0 || certified != 1 || len(evidence) != 1 || !lines {
					t.Fatalf("at observe and structural evidence is gathered: %d call(s), %d event(s), %d record(s), certify line %v", calls.Load(), certified, len(evidence), lines)
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
	certificationSubmitPayload(g, id, literalClaim)
}
func certificationSubmitPayload(g *Gateway, id string, payload []byte) {
	g.enqueueCertification(certificationJob{evidence: CertificationEvidence{LegType: "pas-claim", Seam: "provider-ingress", Direction: "request", CorrelationID: id, TargetLine: "2.1"}, payload: payload})
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

// Status alone decides between a verdict and an execution failure: the same
// real lane body (testdata/recordings/lane-<line>-certify-literal.json, which
// finds the literal Claim invalid, the unknown profile among its errors) is a
// verdict at its recorded 200 and unavailable evidence at a 500. HAPI sends no
// 5xx on demand, so the 500 is an authored fault serving the recorded body. The
// routing client reads a 5xx OperationOutcome as a verdict today; whether it
// should read it as unavailable is an open question this row flips with.
func TestCertificationHTTPExecutionClassification(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			rec := certifyRecording(t, "lane-"+line+"-certify-literal")
			status, body, want := recordedAnswer(t, rec, "profile", literalClaim)
			if status != http.StatusOK || !slices.Contains(want, "Invalid profile. Failed to retrieve explicitly requested profile with url=profile") {
				t.Fatalf("the recorded answer is no longer a 200 refusing the unknown profile: %d %q", status, want)
			}
			_, _, wantRouting := recordedAnswer(t, rec, "", literalClaim)
			lane := rec.Server()
			fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/fhir+json;charset=UTF-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write(body)
			}))
			defer fault.Close()

			v := NewCertificationOperationValidator(lane.URL + "/fhir")
			defer v.Client.CloseIdleConnections()
			res, err := v.Validate(context.Background(), literalClaim, "profile")
			if err != nil || res.Valid || !reflect.DeepEqual(res.Issues, want) {
				t.Fatalf("at 200: %+v %v, want the lane's verdict with its %d errors", res, err, len(want))
			}
			f := NewCertificationOperationValidator(fault.URL + "/fhir")
			defer f.Client.CloseIdleConnections()
			fres, ferr := f.Validate(context.Background(), literalClaim, "profile")
			var execution *certificationHTTPError
			if !errors.As(ferr, &execution) || execution.status != http.StatusInternalServerError || !bytes.Equal(execution.raw, body) || strings.Contains(ferr.Error(), string(body)) || fres.Valid || len(fres.Issues) != 0 {
				t.Fatalf("at 500: %+v %v, want execution evidence holding the same body, and no verdict", fres, ferr)
			}

			routing := shnsdk.NewOperationValidator(lane.URL + "/fhir")
			r, e := routing.Validate(context.Background(), literalClaim, "")
			if e != nil || r.Valid || !reflect.DeepEqual(r.Issues, wantRouting) {
				t.Fatal("routing reading of the lane's 200 answer changed", r, e)
			}
			routingFault := shnsdk.NewOperationValidator(fault.URL + "/fhir")
			r, e = routingFault.Validate(context.Background(), literalClaim, "profile")
			if e != nil || r.Valid || !reflect.DeepEqual(r.Issues, want) {
				t.Fatal("routing reading of a 5xx OperationOutcome changed", r, e)
			}
		})
	}
}

// A real lane's clean answer certifies through the real certification client:
// each lane's answer to the versioned approved ClaimResponse
// (../internal/lanequalify/testdata/recordings/lane-<line>-warm.json, corpus row
// 5), which carries warnings and no error. The client sends exactly the recorded
// request: the fixture as the body, the versioned ClaimResponse profile the
// collector derives for the line, and application/fhir+json. The lane is the
// one a gateway certifies against once it is admitted, after its prime pass:
// any recorded answer the lane gave while still warming (on 2.2, slicing errors
// the first time it met this request) is asked for once first, so the
// collector and then the client meet the lane's settled answer.
func TestCertificationRealLaneValidAnswerCertifies(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			rec, payload, profile, answers := recordedClaimResponseRow(t, line)
			lane := rec.Server()
			v := NewCertificationOperationValidator(lane.URL + "/fhir")
			defer v.Client.CloseIdleConnections()

			// The prime pass: every answer recorded before the settled one.
			for range answers[:len(answers)-1] {
				if _, err := v.Validate(context.Background(), payload, profile); err != nil {
					t.Fatal(err)
				}
			}

			// The collector derives the profile from the species and line.
			g := certificationGatewayByLine(t, map[string]shnsdk.Validator{line: v}, nil)
			certificationSubmitPayload(g, "valid", payload)
			certificationFlush(t, g)
			e := g.CertificationEvidenceForTest()
			if len(e) != 1 {
				t.Fatalf("records=%d", len(e))
			}
			var got LaneVerdict
			for _, verdict := range e[0].Verdicts {
				if verdict.Line == line {
					got = verdict
				}
			}
			if got.State != "valid" || !got.Valid || got.Profile != profile || !slices.Equal(e[0].Certified, []string{line}) {
				t.Fatalf("collector: verdict %+v certified %v, want %s valid", got, e[0].Certified, line)
			}

			// The client itself, asked again: the lane's settled clean answer.
			res, err := v.Validate(context.Background(), payload, profile)
			if err != nil || !res.Valid || len(res.Issues) != 0 || len(res.Details) == 0 {
				t.Fatalf("certification client: %+v %v, want valid with the lane's warnings as details", res, err)
			}
		})
	}
}

// recordedClaimResponseRow loads the lane of line's recorded readiness corpus
// (../internal/lanequalify/testdata/recordings/lane-<line>-warm.json) with
// Subset, and returns its row 5 request (the versioned approved ClaimResponse)
// as a certification payload and profile, with every answer recorded for it in
// order. The request is the one the certification client sends for that
// payload and profile (the profile is the one the collector derives for a
// ClaimResponse at line), and the lane's last answer to it is warnings without
// an error.
func recordedClaimResponseRow(t *testing.T, line string) (*testrecord.Recording, []byte, string, []testrecord.Exchange) {
	t.Helper()
	rec := testrecord.Load(t, filepath.Join("..", "internal", "lanequalify", "testdata", "recordings", "lane-"+line+"-warm.json"))
	rec.Subset() // one request of the 42-row corpus is asked
	profile, ok := profileFor("ClaimResponse", line, "pas-claim")
	if !ok {
		t.Fatal("no ClaimResponse certification profile")
	}
	row5 := rec.Exchanges[4] // rows 1-4 are distinct initialization requests
	if row5.Request.Method != http.MethodPost || row5.Request.Path != "/fhir/ClaimResponse/$validate" || !slices.Equal(row5.Request.Query["profile"], []string{profile}) || row5.Request.Headers["Content-Type"] != "application/fhir+json" {
		t.Fatalf("recorded row 5 is %s %s %v %v, not the versioned ClaimResponse at %s", row5.Request.Method, row5.Request.Path, row5.Request.Query, row5.Request.Headers, profile)
	}
	var answers []testrecord.Exchange
	for _, ex := range rec.Exchanges {
		if ex.Request.Path == row5.Request.Path && slices.Equal(ex.Request.Query["profile"], []string{profile}) && jsonEqualForTest(ex.Request.Body, row5.Request.Body) {
			answers = append(answers, ex)
		}
	}
	settled := answers[len(answers)-1].Response.Body
	if answers[len(answers)-1].Response.Status != http.StatusOK || !bytes.Contains(settled, []byte(`"severity":"warning"`)) || bytes.Contains(settled, []byte(`"severity":"error"`)) || bytes.Contains(settled, []byte(`"severity":"fatal"`)) {
		t.Fatal("the recorded settled answer is no longer a 200 with warnings and no error")
	}
	return rec, []byte(row5.Request.Body), profile, answers
}

// What the collector records against real lanes today: each lane's own answer,
// at its own line, to the payloads recorded in
// testdata/recordings/lane-<line>-certify-collect.json. The literal Claim
// lacks required elements and the sentinel Claim does not parse: invalid is
// what they are. The synthetic PAS request bundles are a known gap. At its own
// line each bundle's errors all come from terminology the lanes do not load (the
// X12 code system; on 2.1 and 2.2 the Claim entry then matches neither Claim
// profile, and the lane also reports the update profile's own mismatch).
// Certification records them as invalid today, where unavailable is what they
// are; this row flips when that is fixed.
func TestCertificationRecordsWhatRealLanesAnswer(t *testing.T) {
	validators := map[string]shnsdk.Validator{}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		rec := certifyRecording(t, "lane-"+line+"-certify-collect")
		v := NewCertificationOperationValidator(rec.Server().URL + "/fhir")
		t.Cleanup(v.Client.CloseIdleConnections)
		validators[line] = v
	}
	g := certificationGatewayByLine(t, validators, nil)
	payloads := [][]byte{literalClaim, sentinelClaim}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		body, _, ok := lanequalify.CertificationRow(line)
		if !ok {
			t.Fatalf("no certification row for %s", line)
		}
		payloads = append(payloads, body)
	}
	for i, payload := range payloads {
		certificationSubmitPayload(g, fmt.Sprint(i), payload)
	}
	certificationFlush(t, g)
	records := g.CertificationEvidenceForTest()
	if len(records) != len(payloads) {
		t.Fatalf("records=%d want %d", len(records), len(payloads))
	}
	for _, e := range records {
		if len(e.Verdicts) != 3 || len(e.Certified) != 0 || e.SourceLine != "" {
			t.Fatalf("%s: %+v", e.CorrelationID, e)
		}
		for _, v := range e.Verdicts {
			if v.State != "invalid" || v.Valid || v.Error != "" || len(v.Issues) != 1 || !strings.HasPrefix(v.Issues[0], "validator issues count=") {
				t.Fatalf("%s: verdict %+v, want the lane's invalid verdict", e.CorrelationID, v)
			}
		}
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

// Answer shapes no recorded lane sends (every recorded $validate answer is an
// OperationOutcome with an issue list of known severities, and none is a 5xx):
// authored rejection rows for the certification transport's guard.
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
	defer srv.Close()
	n := &nativeResponder{client: srv.Client()}
	capture := &nativeCertificationCapture{}
	ctx := context.WithValue(context.Background(), nativeCertificationKey{}, capture)
	first := []byte(`{"resourceType":"Claim","id":"first"}`)
	_, bad, err := n.post(ctx, srv.URL, "", testRequest(first), "pas-claim", "synthetic")
	if err != nil || bad.Status != 409 || len(capture.response) == 0 {
		t.Fatal(bad, err, capture)
	}
	second := []byte(`{"resourceType":"Claim","id":"second"}`)
	_, _, err = n.post(ctx, refusedURL, "", testRequest(second), "pas-claim", "synthetic retry")
	var failure *upstreamFailure
	if !errors.As(err, &failure) || failure.sent || !strings.HasPrefix(err.Error(), "upstream payer synthetic retry unreachable: ") {
		t.Fatalf("retry error = %v, want an unsent dial failure", err)
	}
	if !bytes.Equal(capture.request, second) || capture.response != nil {
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
		// Authored: an answer past the size bound, which no lane was seen to send.
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

// Foreign bytes in a validator's answer never reach the evidence, the
// observer or the log. The "normal" row is real: each lane's answer to a Claim
// it cannot parse quotes the sentinel back in its diagnostics
// (testdata/recordings/lane-<line>-certify-collect.json). The other rows are
// authored shapes no lane sends (diagnostics that are not a string, and a
// client returning both issues and an error), rejection rows for the guard.
func TestCertificationExternalDiagnosticsStayPrivate(t *testing.T) {
	const sentinel = "PRIVATE-SYNTHETIC-SENTINEL"
	for _, row := range []struct {
		name, diagnostics, state string
		custom, recorded         bool
	}{
		{"normal", "", "invalid", false, true},
		{"object", `{"foreign":"` + sentinel + `"}`, "unavailable", false, false},
		{"array", `["` + sentinel + `"]`, "unavailable", false, false},
		{"custom-error-and-issues", "", "unavailable", true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			raw := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","diagnostics":` + row.diagnostics + `}]}`
			payload := literalClaim
			validators := map[string]shnsdk.Validator{}
			switch {
			case row.recorded:
				payload = sentinelClaim
				for _, line := range []string{"2.0", "2.1", "2.2"} {
					rec := certifyRecording(t, "lane-"+line+"-certify-collect")
					rec.Subset() // this row asks only the sentinel Claim
					v := NewCertificationOperationValidator(rec.Server().URL + "/fhir")
					t.Cleanup(v.Client.CloseIdleConnections)
					validators[line] = v
					profile, _ := profileFor("Claim", line, "pas-claim")
					status, answer, want := recordedAnswer(t, rec, profile, sentinelClaim)
					if status != http.StatusBadRequest || len(want) != 1 || !strings.Contains(want[0], sentinel) {
						t.Fatalf("%s: the recorded refusal no longer quotes the sentinel: %d %q", line, status, want)
					}
					if line == "2.2" {
						raw = string(answer)
						result, err := v.Validate(context.Background(), sentinelClaim, profile)
						if err != nil || result.Valid || !reflect.DeepEqual(result.Issues, want) {
							t.Fatal("ordinary SDK classification changed", result, err)
						}
					}
				}
			case row.custom:
				v := certificationValidatorFunc(func(context.Context, []byte, string) (shnsdk.Result, error) {
					return shnsdk.Result{Issues: []string{strings.Repeat(sentinel, 10000), sentinel}}, errors.New(strings.Repeat(sentinel, 10000))
				})
				validators = map[string]shnsdk.Validator{"2.0": v, "2.1": v, "2.2": v}
			default:
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, raw) }))
				defer srv.Close()
				v := NewCertificationOperationValidator(srv.URL)
				// Keep bounded raw diagnostic proof separate from metadata, including malformed OO.
				_, err := v.Validate(context.Background(), literalClaim, "")
				var diagnostic *certificationHTTPError
				if !errors.As(err, &diagnostic) || string(diagnostic.raw) != raw {
					t.Error("separate bounded raw diagnostic lost")
				}
				validators = map[string]shnsdk.Validator{"2.0": v, "2.1": v, "2.2": v}
			}
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			var observed []string
			g := certificationGatewayByLine(t, validators, func(e ObserverEvent) {
				if e.Kind == "leg.certified" {
					observed = append(observed, e.Detail)
				}
			})
			defer g.Close()
			certificationSubmitPayload(g, "privacy", payload)
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
