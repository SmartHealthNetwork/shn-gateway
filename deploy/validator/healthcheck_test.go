package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLane is a validator lane at the wire. By default it answers each request
// with what a real lane of that line answered: GET /fhir/metadata with the
// lane's recorded CapabilityStatement
// (testdata/recordings/lane-<line>-metadata.json, served on every ask), and
// every other request from testdata/recordings/lane-<line>-warm.json, replayed
// strictly: a request the lane was never asked fails the test, and every
// recorded answer must be used.
// Two fault hooks cover what no capture holds. metadata is the status
// /metadata answers with; any status but 200 replaces the recorded answer (a
// lane not yet up). validate is the per-request hook (a hang, a redirect, an
// oversized or non-OperationOutcome answer, a false verdict): returning false
// means the hook already answered (or deliberately never will); returning true
// passes the request on to the recording. A test that sets it, or stops the
// corpus early, calls lane.partial(). Every request past /metadata is
// recorded in posts, and one that arrives while another is still being
// answered (from its arrival until its answer is written) counts in
// overlaps; hold keeps each answer open that much longer after it is written,
// so a client that sends before reading an answer to its end is caught.
type fakeLane struct {
	srv       *httptest.Server
	subset    func()
	metadata  int32 // fault hook: HTTP status /metadata answers with; 200 = the recorded answer
	validate  func(w http.ResponseWriter, r *http.Request) bool
	postsMu   sync.Mutex
	posts     []recordedPost
	validates int32
	active    atomic.Int32
	overlaps  atomic.Int32
	hold      atomic.Int64 // a time.Duration
}

type recordedPost struct {
	path, profile, rawQuery string
	body                    []byte
}

func newFakeLane(t *testing.T, line string) *fakeLane { return newLane(t, line, false) }

// newWarmedFakeLane is the lane as a verification pass meets it, after the
// warm-up. Verification asks 20 of the 42 recorded requests, so the rest go
// unused.
func newWarmedFakeLane(t *testing.T, line string) *fakeLane {
	l := newLane(t, line, true)
	l.partial()
	return l
}

func newLane(t *testing.T, line string, warmed bool) *fakeLane {
	t.Helper()
	lane, subset := replayLane(t, line, warmed)
	capability := recordedMetadata(t, line)
	l := &fakeLane{subset: subset, metadata: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/fhir/metadata", func(w http.ResponseWriter, r *http.Request) {
		if status := atomic.LoadInt32(&l.metadata); status != http.StatusOK {
			w.WriteHeader(int(status))
			return
		}
		serveRecordedMetadata(t, w, r, capability)
	})
	mux.HandleFunc("/fhir/", func(w http.ResponseWriter, r *http.Request) {
		if l.active.Add(1) != 1 {
			l.overlaps.Add(1)
		}
		defer l.active.Add(-1)
		atomic.AddInt32(&l.validates, 1)
		body, _ := io.ReadAll(r.Body)
		l.postsMu.Lock()
		l.posts = append(l.posts, recordedPost{path: r.URL.Path, profile: r.URL.Query().Get("profile"), rawQuery: r.URL.RawQuery, body: body})
		l.postsMu.Unlock()
		if l.validate != nil && !l.validate(w, r) {
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, lane.URL+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := lane.Client().Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		if hold := time.Duration(l.hold.Load()); hold > 0 {
			// The answer's bytes are out; its end is not until the handler returns.
			w.(http.Flusher).Flush()
			time.Sleep(hold)
		}
	})
	l.srv = httptest.NewServer(mux)
	t.Cleanup(l.srv.Close)
	return l
}

// partial marks a test whose validate hook answers rows itself, or whose run
// asks fewer than all the recorded requests: the recorded answers for those
// rows go unused.
func (l *fakeLane) partial() { l.subset() }

func (l *fakeLane) base() string { return l.srv.URL + "/fhir" }

func (l *fakeLane) recorded() []recordedPost {
	l.postsMu.Lock()
	defer l.postsMu.Unlock()
	return append([]recordedPost(nil), l.posts...)
}

func envLine(line string) func(string) string {
	return func(k string) string {
		if k == "SHN_IG_LINE" {
			return line
		}
		return ""
	}
}

// sameJVM / otherJVM are incarnation keys: the same HAPI process across probes, and a
// process restarted underneath a surviving marker.
func sameJVM() string  { return "boot-a:4242" }
func otherJVM() string { return "boot-a:9001" }

func markerPath(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "warm") }
func TestCheckNon200(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // no validation is asked
	atomic.StoreInt32(&lane.metadata, http.StatusServiceUnavailable)
	marker := markerPath(t)
	if got := check(lane.base(), envLine("2.2"), marker, time.Second, sameJVM); got != 1 {
		t.Fatalf("check(503) = %d, want 1", got)
	}
	if n := atomic.LoadInt32(&lane.validates); n != 0 {
		t.Fatalf("validated %d times behind a 503 /metadata, want 0", n)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("observer wrote marker")
	}
}

func TestCheckUnreachable(t *testing.T) {
	marker := markerPath(t)
	if got := check("http://127.0.0.1:1/fhir", envLine("2.2"), marker, time.Second, sameJVM); got != 1 {
		t.Fatalf("check(unreachable) = %d, want 1", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("observer wrote marker")
	}
}

func TestCheckMetadataHangIsNotReady(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // no validation is asked
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/fhir/metadata", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	mux.Handle("/fhir/", lane.srv.Config.Handler)
	hung := httptest.NewServer(mux)
	t.Cleanup(hung.Close)
	t.Cleanup(func() { close(release) }) // registered after hung.Close so it runs first (LIFO)
	marker := markerPath(t)
	done := make(chan int, 1)
	go func() { done <- check(hung.URL+"/fhir", envLine("2.2"), marker, 300*time.Millisecond, sameJVM) }()
	select {
	case got := <-done:
		if got != 1 {
			t.Fatalf("check(hanging /metadata) = %d, want 1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("probe still running 2s into a hanging /metadata; must give up inside its budget")
	}
	if n := atomic.LoadInt32(&lane.validates); n != 0 {
		t.Fatalf("validated %d times behind a hanging /metadata, want 0", n)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("observer wrote marker")
	}
}

func TestIncarnationFrom(t *testing.T) {
	stat := "1 (java (x) y) S 0 1 1 0 -1 4194560 1234 0 0 0 55 7 0 0 20 0 41 0 987654 3000000000 100000 18446744073709551615 1 1 0 0 0 0 0 4096 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	if got := incarnationFrom(stat, "b0ot-id\n"); got != "b0ot-id:987654" {
		t.Fatalf("incarnationFrom = %q, want b0ot-id:987654", got)
	}
	if got := incarnationFrom("garbage", "x"); got != "" {
		t.Fatalf("incarnationFrom(garbage) = %q, want empty", got)
	}
	if got := incarnationFrom("1 (java) S 0 1", "x"); got != "" {
		t.Fatalf("incarnationFrom(short) = %q, want empty", got)
	}
	// Two incarnations differ by start time or by boot: neither matches the other.
	a := incarnationFrom(stat, "boot-a")
	b := incarnationFrom(strings.Replace(stat, " 987654 ", " 987700 ", 1), "boot-a")
	c := incarnationFrom(stat, "boot-b")
	if a == b || a == c {
		t.Fatalf("incarnations collide: %q %q %q", a, b, c)
	}
}

func TestWarmupsPerLine(t *testing.T) {
	cases := map[string]map[string]string{
		"2.0": {"Bundle": "testdata/claim-bundle.json", "QuestionnaireResponse": "testdata/questionnaireresponse-autofill.json"},
		"2.1": {"Bundle": "testdata/2.1/claim-bundle.json", "QuestionnaireResponse": "testdata/2.1/questionnaireresponse-autofill.json"},
		"2.2": {"Bundle": "testdata/2.2/claim-bundle.json", "QuestionnaireResponse": "testdata/2.2/questionnaireresponse-autofill.json"},
		"":    {"Bundle": "testdata/claim-bundle.json", "QuestionnaireResponse": "testdata/questionnaireresponse-autofill.json"},
	}
	for line, want := range cases {
		rows := warmups(line)
		if len(rows) != 4 {
			t.Fatalf("line %q: %d warm-up rows, want 4 (PAS bundle, DTR QR, PDex EOB, CDex Task)", line, len(rows))
		}
		byType := map[string]warmup{}
		for _, row := range rows {
			byType[row.resourceType] = row
			raw, err := fixtures.ReadFile(row.file)
			if err != nil {
				t.Fatalf("line %q: fixture %s not embedded: %v", line, row.file, err)
			}
			var probe struct {
				ResourceType string `json:"resourceType"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil {
				t.Fatalf("line %q: fixture %s is not JSON: %v", line, row.file, err)
			}
			if probe.ResourceType != row.resourceType {
				t.Fatalf("line %q: fixture %s is a %s, row says %s", line, row.file, probe.ResourceType, row.resourceType)
			}
			if row.profile == "" {
				t.Fatalf("line %q: row %s has no profile", line, row.resourceType)
			}
		}
		for rt, file := range want {
			if byType[rt].file != file {
				t.Fatalf("line %q: %s warm-up uses %s, want %s", line, rt, byType[rt].file, file)
			}
		}
		if byType["ExplanationOfBenefit"].file != "testdata/eob-approved.json" || byType["Task"].file != "testdata/cdex-task-data-request.json" {
			t.Fatalf("line %q: PDex/CDex rows must be the line-neutral top-level fixtures: %+v", line, rows)
		}
	}
}

func TestLineFromEnv(t *testing.T) {
	if got := lineFromEnv(envLine("2.1")); got != "2.1" {
		t.Fatalf("lineFromEnv(2.1) = %q", got)
	}
	if got := lineFromEnv(envLine("2.0")); got != "2.0" {
		t.Fatalf("lineFromEnv(2.0) = %q", got)
	}
	if got := lineFromEnv(envLine("")); got != "" {
		t.Fatalf("lineFromEnv(unset) = %q, want empty", got)
	}
	if got := lineFromEnv(envLine("9.9")); got != "" {
		t.Fatalf("lineFromEnv(unknown) = %q, want empty", got)
	}
}

func TestObserverNeverSubmitsWarmup(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // an observer asks only the recorded metadata
	got := check(lane.base(), envLine("2.2"), markerPath(t), time.Second, sameJVM)
	if got != 1 || len(lane.recorded()) != 0 {
		t.Fatalf("observer submitted or credited cold work: exit=%d posts=%d", got, len(lane.recorded()))
	}
}
