package lanequalify

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

	"github.com/SmartHealthNetwork/shn-gateway/internal/testrecord"
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
