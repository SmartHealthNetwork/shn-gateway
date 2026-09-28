package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	"github.com/SmartHealthNetwork/shn-gateway/internal/testrecord"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// warmLane is a validator endpoint that records each request it gets and
// answers with the status its answer function picks for that request number.
// Every request must be the 2.1 warm-up request a real 2.1 lane recorded
// (testdata/recordings/lane-2.1-certify-warm.json: method, path, profile,
// Content-Type and body), or the row fails. Below 500 the lane's recorded
// answer is served, through the strict replay; the 5xx statuses are authored,
// faults a lane sends on no request.
type warmLane struct {
	*httptest.Server
	mu       sync.Mutex
	requests []warmRequest
}

type warmRequest struct {
	method, path, profile, body string
}

func newWarmLane(t *testing.T, status func(n int) int) *warmLane {
	t.Helper()
	rec := testrecord.Load(t, laneRecording(t, "lane-2.1-certify-warm"))
	rec.Subset() // a row whose endpoint never answers, or is never asked, leaves the answer unused; rows count their requests
	want := rec.Exchanges[0].Request
	replay := rec.Server()
	l := &warmLane{}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.requests = append(l.requests, warmRequest{r.Method, r.URL.Path, r.URL.Query().Get("profile"), string(body)})
		n := len(l.requests)
		l.mu.Unlock()
		var sent, recorded any
		if r.Method != want.Method || r.URL.Path != want.Path || !reflect.DeepEqual(r.URL.Query(), url.Values(want.Query)) || r.Header.Get("Content-Type") != want.Headers["Content-Type"] ||
			json.Unmarshal(body, &sent) != nil || json.Unmarshal(want.Body, &recorded) != nil || !reflect.DeepEqual(sent, recorded) {
			t.Errorf("request %d is not the recorded 2.1 warm-up request: %s %s", n, r.Method, r.URL.RequestURI())
			http.Error(w, "unrecorded request", 599)
			return
		}
		if code := status(n); code >= 500 {
			w.WriteHeader(code)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		replay.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(l.Close)
	return l
}

func (l *warmLane) got() []warmRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]warmRequest(nil), l.requests...)
}

func answers(int) int { return http.StatusOK }

// captureWarmLog collects the certification_warm log lines written while it is
// installed, keyed by line.
func captureWarmLog(t *testing.T) func() map[string]map[string]any {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	previous := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(previous) })
	return func() map[string]map[string]any {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]map[string]any{}
		for _, line := range strings.Split(buf.String(), "\n") {
			_, event, ok := strings.Cut(line, "gateway: certification_warm ")
			if !ok {
				continue
			}
			var e map[string]any
			if err := json.Unmarshal([]byte(event), &e); err != nil {
				t.Fatalf("certification_warm line is not JSON: %q", line)
			}
			out[e["line"].(string)] = e
		}
		return out
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Each certification client that dials an address with no qualification of
// its own gets one PAS request bundle for its line, against the versioned
// request-bundle profile, and one log line saying it answered on the first
// request. Each endpoint is a real lane of its line replayed strictly
// (testdata/recordings/lane-<line>-certify-warm.json): any other request fails
// the row.
func TestWarmCertification_SendsOneBundlePerConfiguredEndpoint(t *testing.T) {
	logs := captureWarmLog(t)
	l20, l21, l22 := newRecordedLane(t, nil, "lane-2.0-certify-warm"), newRecordedLane(t, nil, "lane-2.1-certify-warm"), newRecordedLane(t, nil, "lane-2.2-certify-warm")
	validators := map[string]shnsdk.Validator{
		"2.0": engine.NewCertificationOperationValidator(l20.URL + "/fhir"),
		"2.1": engine.NewCertificationOperationValidator(l21.URL + "/fhir"),
		"2.2": engine.NewCertificationOperationValidator(l22.URL + "/fhir"),
	}
	defer engine.CloseCertificationClients(validators)

	warmCertification(t.Context(), validators)

	for line, l := range map[string]*recordedLane{"2.0": l20, "2.1": l21, "2.2": l22} {
		version := map[string]string{"2.0": "2.0.1", "2.1": "2.1.0", "2.2": "2.2.1"}[line]
		body, _, _ := lanequalify.CertificationRow(line)
		recorded := l.rec.Exchanges[0].Request
		var sent, want any
		if recorded.Method != http.MethodPost || recorded.Path != "/fhir/Bundle/$validate" || !slices.Equal(recorded.Query["profile"], []string{"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle|" + version}) ||
			json.Unmarshal(recorded.Body, &sent) != nil || json.Unmarshal(body, &want) != nil || !reflect.DeepEqual(sent, want) {
			t.Fatalf("%s: the recording is not this line's bundle at %s", line, version)
		}
		if got := l.posts.Load() + l.gets.Load(); got != 1 {
			t.Errorf("%s lane got %d requests, want the one POST of its own line's bundle", line, got)
		}
		e := logs()[line]
		if e["state"] != "answered" || e["attempts"] != float64(1) || e["host"] != "127.0.0.1" {
			t.Errorf("%s log %v, want answered after 1 request from host 127.0.0.1", line, e)
		}
	}
}

// Of the addresses a gateway is given, the warm-up sends its bundle only to the
// 2.0 validator: a 2.1 or 2.2 address certifies only once it has passed its own
// qualification, which posts the whole readiness corpus to it, so the warm-up
// leaves it alone.
func TestWarmCertification_SkipsAddressesQualifiedByTheCorpus(t *testing.T) {
	logs := captureWarmLog(t)
	l20, l21, l22 := newRecordedLane(t, nil, "lane-2.0-certify-warm"), newWarmLane(t, answers), newWarmLane(t, answers)
	validators := certificationValidators(env(nil), config{FHIRCertifyURL21: l21.URL + "/fhir", FHIRValidateURL22: l22.URL + "/fhir"}, l20.URL+"/fhir", nil, nil)
	defer engine.CloseCertificationClients(validators)

	warmCertification(t.Context(), validators)

	if got := l20.posts.Load(); got != 1 {
		t.Errorf("2.0 validator got %d warm-up requests, want 1", got)
	}
	if got21, got22 := l21.got(), l22.got(); len(got21) != 0 || len(got22) != 0 {
		t.Errorf("gated addresses got warm-up requests: 2.1 %+v, 2.2 %+v", got21, got22)
	}
	if got := logs(); len(got) != 1 || got["2.0"] == nil {
		t.Errorf("logged %v, want one line, for 2.0", got)
	}
}

// An endpoint that does not answer is sent the bundle again, up to three
// requests in all, and the warm-up stops at the first answer.
func TestWarmCertification_RetriesUntilAnsweredAtMostThreeTimes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   func(int) int
		requests int
		state    string
	}{
		{"answers on the second request", func(n int) int { return map[bool]int{true: 500, false: 200}[n == 1] }, 2, "answered"},
		{"never answers", func(int) int { return http.StatusServiceUnavailable }, 3, "unanswered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureWarmLog(t)
			lane := newWarmLane(t, tc.status)
			warmCertification(t.Context(), map[string]shnsdk.Validator{"2.1": engine.NewCertificationOperationValidator(lane.URL + "/fhir")})
			if got := len(lane.got()); got != tc.requests {
				t.Fatalf("%d requests, want %d", got, tc.requests)
			}
			if e := logs()["2.1"]; e["state"] != tc.state || e["attempts"] != float64(tc.requests) {
				t.Fatalf("log %v, want %s after %d", e, tc.state, tc.requests)
			}
		})
	}
}

// trapTransport fails the test if the engine's own certification client is
// used: the warm-up must send through a client of its own.
type trapTransport struct{ t *testing.T }

func (tr trapTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.t.Errorf("the warm-up used the engine's certification client: %s", r.URL)
	return nil, errors.New("trap")
}

// The warm-up never uses the client the engine certifies with, so it cannot
// take the one connection a real certification needs or produce evidence, and
// it leaves a default lane (qualified by its own corpus) and a fake alone.
func TestWarmCertification_UsesItsOwnClientAndSkipsDefaultLanes(t *testing.T) {
	logs := captureWarmLog(t)
	configured, defaulted := newWarmLane(t, answers), newWarmLane(t, answers)
	engineClient := engine.NewCertificationOperationValidator(configured.URL + "/fhir")
	engineClient.Client = &http.Client{Transport: trapTransport{t}}
	gated := engine.NewGatedCertificationValidator(engine.NewDiscoveredLane("2.2", defaulted.URL+"/fhir", nil), nil, "not configured")
	validators := map[string]shnsdk.Validator{"2.0": engine.NewLineFakeValidator("2.0"), "2.1": engineClient, "2.2": gated}
	defer engine.CloseCertificationClients(validators)

	warmCertification(t.Context(), validators)

	if got := len(configured.got()); got != 1 {
		t.Fatalf("configured endpoint got %d requests, want 1 through the warm-up's own client", got)
	}
	if got := defaulted.got(); len(got) != 0 {
		t.Fatalf("default lane got %+v, want nothing", got)
	}
	if got := logs(); len(got) != 1 || got["2.1"] == nil {
		t.Fatalf("logged %v, want one line, for 2.1", got)
	}
}

// Shutdown interrupts a request in flight: the warm-up returns well inside the
// certification client's 2.5 s timeout and says it stopped.
func TestWarmCertification_ShutdownInterruptsARequestInFlight(t *testing.T) {
	logs := captureWarmLog(t)
	received := make(chan struct{})
	var once sync.Once
	lane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: only then does the server notice the client
		// going away and end the request's context.
		_, _ = io.Copy(io.Discard, r.Body)
		once.Do(func() { close(received) })
		<-r.Context().Done()
	}))
	defer lane.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		warmCertification(ctx, map[string]shnsdk.Validator{"2.1": engine.NewCertificationOperationValidator(lane.URL + "/fhir")})
	}()
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("the warm-up never sent its request")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the warm-up did not stop when its context ended")
	}
	if e := logs()["2.1"]; e["state"] != "stopped" || e["attempts"] != float64(1) {
		t.Fatalf("log %v, want stopped after 1 request", e)
	}
}

// Shutdown stops the warm-up: nothing is sent once its context is done, and
// the line says it stopped.
func TestWarmCertification_StopsWhenItsContextEnds(t *testing.T) {
	logs := captureWarmLog(t)
	lane := newWarmLane(t, answers)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	warmCertification(ctx, map[string]shnsdk.Validator{"2.1": engine.NewCertificationOperationValidator(lane.URL + "/fhir")})
	if got := lane.got(); len(got) != 0 {
		t.Fatalf("sent %+v after shutdown", got)
	}
	if e := logs()["2.1"]; e["state"] != "stopped" || e["attempts"] != float64(0) {
		t.Fatalf("log %v, want stopped after 0 requests", e)
	}
}

// The warm-up never delays readiness: Run's workers start beside the listener,
// so startWorkers returns while an endpoint has not answered, and its cleanup
// cancels and joins the warm-up.
func TestStartWorkers_CertificationWarmDoesNotDelayStart(t *testing.T) {
	_ = captureWarmLog(t)
	received, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	lane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(received) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer lane.Close()
	defer close(release)
	b := built{
		checksRunner:      checks.NewRunner(nil, http.DefaultClient, time.Now),
		certification:     map[string]shnsdk.Validator{"2.1": engine.NewCertificationOperationValidator(lane.URL + "/fhir")},
		warmCertification: true,
	}
	returned := make(chan func())
	go func() { returned <- b.startWorkers(t.Context()) }()
	var stop func()
	select {
	case stop = <-returned:
	case <-time.After(time.Second): // well inside one 2.5 s certification request
		t.Fatal("startWorkers waited on the certification warm-up")
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("the certification warm-up never ran")
	}
	stop()
}

// Where certification evidence is not collected (CONFORMANCE_ENFORCEMENT=none),
// Run's workers send the certification clients nothing: once they are joined,
// no warm-up line was ever written (a started warm-up always writes one).
func TestStartWorkers_NoCertificationWarmWhereCertificationDoesNotRun(t *testing.T) {
	logs := captureWarmLog(t)
	lane := newWarmLane(t, answers)
	b := built{
		checksRunner:  checks.NewRunner(nil, http.DefaultClient, time.Now),
		certification: map[string]shnsdk.Validator{"2.1": engine.NewCertificationOperationValidator(lane.URL + "/fhir")},
	}
	b.startWorkers(t.Context())()
	if got := logs(); len(got) != 0 || len(lane.got()) != 0 {
		t.Fatalf("warm-up ran without certification: log %v, %d requests", got, len(lane.got()))
	}
}
