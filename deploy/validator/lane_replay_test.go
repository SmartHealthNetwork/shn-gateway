package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"shnvalidatorhealthcheck/internal/testrecord"
)

// replayLane serves what a real validator lane of line answered
// (testdata/recordings/lane-<line>-warm.json) and returns the way to excuse
// recorded answers a test leaves unused. warmed replays the lane as its
// verification pass met it, after the warm-up: every answer the lane gave a
// request before its last one (a lane still warming) is asked for once first.
func replayLane(t *testing.T, line string, warmed bool) (*httptest.Server, func()) {
	t.Helper()
	rec := testrecord.Load(t, filepath.Join("testdata", "recordings", "lane-"+line+"-warm.json"))
	srv := rec.Server()
	if warmed {
		for i, ex := range rec.Exchanges {
			if !askedAgainLater(rec.Exchanges, i) {
				continue
			}
			u := srv.URL + ex.Request.Path
			if len(ex.Request.Query) > 0 {
				u += "?" + url.Values(ex.Request.Query).Encode()
			}
			req, err := http.NewRequest(ex.Request.Method, u, bytes.NewReader(ex.Request.Body))
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range ex.Request.Headers {
				req.Header.Set(k, v)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
	}
	return srv, rec.Subset
}

// askedAgainLater reports whether a later exchange records the same request.
func askedAgainLater(exs []testrecord.Exchange, i int) bool {
	for _, later := range exs[i+1:] {
		a, b := exs[i].Request, later.Request
		if a.Method == b.Method && a.Path == b.Path && url.Values(a.Query).Encode() == url.Values(b.Query).Encode() && bytes.Equal(a.Body, b.Body) {
			return true
		}
	}
	return false
}

func recordingPath(name string) string {
	return filepath.Join("testdata", "recordings", name+".json")
}

// replayRecording serves testdata/recordings/<name>.json strictly, as
// replayLane does, and returns the way to excuse recorded answers a test
// leaves unused.
func replayRecording(t *testing.T, name string) (*httptest.Server, func()) {
	t.Helper()
	rec := testrecord.Load(t, recordingPath(name))
	return rec.Server(), rec.Subset
}

var laneRecordings sync.Map // recording name -> []recordedExchange

// laneRecording is testdata/recordings/<name>.json as the twin tests read it:
// each recorded request and the lane's answer, checked but not served.
func laneRecording(t *testing.T, name string) []recordedExchange {
	t.Helper()
	if got, ok := laneRecordings.Load(name); ok {
		return got.([]recordedExchange)
	}
	rec := parseRecording(t, name)
	out := make([]recordedExchange, 0, len(rec.Exchanges))
	for _, ex := range rec.Exchanges {
		out = append(out, recordedExchange{
			method:      ex.Request.Method,
			path:        ex.Request.Path,
			query:       url.Values(ex.Request.Query),
			body:        rawOrText(ex.Request.Body, ex.Request.BodyText),
			status:      ex.Response.Status,
			contentType: ex.Response.Headers["Content-Type"],
			answer:      rawOrText(ex.Response.Body, ex.Response.BodyText),
		})
	}
	laneRecordings.Store(name, out)
	return out
}

func parseRecording(t *testing.T, name string) *testrecord.Recording {
	t.Helper()
	path := recordingPath(name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := testrecord.Parse(path, raw)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rec
}

func rawOrText(raw json.RawMessage, text string) []byte {
	if len(raw) > 0 {
		return append([]byte(nil), raw...)
	}
	return []byte(text)
}

// recordedMetadata is the lane of line's real answer to GET /fhir/metadata
// (testdata/recordings/lane-<line>-metadata.json).
func recordedMetadata(t *testing.T, line string) recordedExchange {
	t.Helper()
	exchanges := laneRecording(t, "lane-"+line+"-metadata")
	if len(exchanges) != 1 || exchanges[0].method != http.MethodGet || exchanges[0].path != "/fhir/metadata" || exchanges[0].status != http.StatusOK {
		t.Fatalf("lane-%s-metadata.json does not record one GET /fhir/metadata answered 200", line)
	}
	return exchanges[0]
}

// measuredMetadataBytes is the size of the lane of line's real metadata answer
// as captured, before its scrub replaced the resource listing.
func measuredMetadataBytes(t *testing.T, line string) int {
	t.Helper()
	rec := parseRecording(t, "lane-"+line+"-metadata")
	if len(rec.Scrubbed) != 1 || rec.Scrubbed[0].Pointer != "/exchanges/0/response/body/rest" {
		t.Fatalf("lane-%s-metadata.json: want the one scrub of the resource listing, got %+v", line, rec.Scrubbed)
	}
	return rec.Scrubbed[0].OriginalBytes
}

// serveRecordedMetadata answers the one request the metadata recording holds,
// GET /fhir/metadata, with the lane's recorded answer; anything else fails the
// test and gets 599, as testrecord's own replay answers an unrecorded request.
func serveRecordedMetadata(t *testing.T, w http.ResponseWriter, r *http.Request, capability recordedExchange) {
	if r.Method != capability.method || r.URL.Path != capability.path || r.URL.RawQuery != "" {
		t.Errorf("no recorded metadata answer for %s %s", r.Method, r.URL.RequestURI())
		http.Error(w, "no recorded exchange", 599)
		return
	}
	serveRecorded(w, capability)
}

// serveRecorded writes a recorded answer as the lane sent it.
func serveRecorded(w http.ResponseWriter, ex recordedExchange) {
	if ex.contentType != "" {
		w.Header().Set("Content-Type", ex.contentType)
	}
	w.WriteHeader(ex.status)
	_, _ = w.Write(ex.answer)
}

// replayT stands in for the testing.T a replay reports through, so a
// rejection row can see what the replay refused (as internal/testrecord's own
// rejection rows do).
type replayT struct {
	mu       sync.Mutex
	errors   []string
	cleanups []func()
}

func (r *replayT) Helper() {}
func (r *replayT) Errorf(format string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, a...))
}
func (r *replayT) Fatalf(format string, a ...any) { r.Errorf(format, a...) }
func (r *replayT) Cleanup(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, fn)
}
func (r *replayT) finish() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}
func (r *replayT) reported(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.errors {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// Rejection row: the replay is strict. The recorded explicit-profile control
// is answered; the same request naming a PAS version the lane was never asked
// about gets no answer and fails the test, never the closest recorded answer.
func TestLaneReplayRefusesARequestTheLaneWasNeverAsked(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			rt := &replayT{}
			rec := testrecord.Load(rt, recordingPath("lane-"+line+"-warm"))
			rec.Subset() // this row asks one recorded request and one unrecorded one
			srv := rec.Server()
			row := explicitProfileRows(line)[0]
			if err := validate(context.Background(), httpClient(), srv.URL+"/fhir", row); err != nil {
				t.Fatalf("recorded request: %v", err)
			}
			row.profile = pasClaimResponseProfile + "|0.0.1"
			err := validate(context.Background(), httpClient(), srv.URL+"/fhir", row)
			rt.finish()
			if err == nil || err.Error() != "wrong response status" {
				t.Fatalf("an unrecorded request = %v, want the replay's refusal as \"wrong response status\"", err)
			}
			if !rt.reported("no recorded exchange") || len(rt.errors) != 1 {
				t.Fatalf("the replay did not fail the test exactly once for the unrecorded request: %q", rt.errors)
			}
		})
	}
}
