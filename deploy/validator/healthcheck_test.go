package main

import (
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

// fakeLane is a validator stand-in that serves /metadata and $validate the way
// HAPI does at the wire: a 200 CapabilityStatement, and an OperationOutcome for
// any $validate (HTTP 200 whatever the issues). validate is the per-request hook
// (nil = warm: answer immediately); every $validate is recorded in posts.
type fakeLane struct {
	srv       *httptest.Server
	metadata  int32 // HTTP status /metadata answers with
	validate  func(w http.ResponseWriter, r *http.Request) bool
	postsMu   sync.Mutex
	posts     []recordedPost
	validates int32
}

type recordedPost struct {
	path, profile, rawQuery string
	body                    []byte
}

func testOutcome(body []byte) string {
	if strings.Contains(string(body), `"valueBoolean":true`) {
		return targetedNegativeOutcome
	}
	return cleanOutcome
}

func newFakeLane(t *testing.T) *fakeLane {
	t.Helper()
	l := &fakeLane{metadata: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/fhir/metadata", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&l.metadata)))
		_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement"}`))
	})
	mux.HandleFunc("/fhir/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&l.validates, 1)
		body, _ := io.ReadAll(r.Body)
		l.postsMu.Lock()
		l.posts = append(l.posts, recordedPost{path: r.URL.Path, profile: r.URL.Query().Get("profile"), rawQuery: r.URL.RawQuery, body: body})
		l.postsMu.Unlock()
		if l.validate != nil && !l.validate(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(testOutcome(body)))
	})
	l.srv = httptest.NewServer(mux)
	t.Cleanup(l.srv.Close)
	return l
}

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
	lane := newFakeLane(t)
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
	lane := newFakeLane(t)
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
	lane := newFakeLane(t)
	got := check(lane.base(), envLine("2.2"), markerPath(t), time.Second, sameJVM)
	if got != 1 || len(lane.recorded()) != 0 {
		t.Fatalf("observer submitted or credited cold work: exit=%d posts=%d", got, len(lane.recorded()))
	}
}
