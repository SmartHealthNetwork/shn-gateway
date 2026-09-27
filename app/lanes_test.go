package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	"github.com/SmartHealthNetwork/shn-gateway/internal/testrecord"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestLanesDiscoveredAtDefaultAddresses(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()
	t.Run("malformed overrides refuse before qualification", func(t *testing.T) {
		for _, base := range []string{":invalid", "ftp://validator/fhir", "http://"} {
			_, manager, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.dtr@2.1"}, canonical, config{FHIRValidateURL21: base}, engine.DefaultLaneURL, func(context.Context, string, string) error { return errors.New("unexpected qualification") })
			manager.Close()
			if err == nil {
				t.Fatalf("malformed override %q admitted", base)
			}
		}
	})
	t.Run("fake mode keeps distinct deterministic lines", func(t *testing.T) {
		lanes, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "1" }, shnsdk.NativeContractVersions(), canonical, config{}, func(string) string { t.Fatal("fake mode resolved DNS"); return "" }, func(context.Context, string, string) error { t.Fatal("fake mode qualified"); return nil })
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		if len(m.defaults) != 0 {
			t.Fatal("fake mode created default workers")
		}
		for _, line := range []string{"2.0", "2.1", "2.2"} {
			assertLineFake(t, lanes[line], line)
		}
	})
	for _, declared := range [][]string{shnsdk.SupportedContractVersions(), {"pa.pdex@2.1"}, {"pa.crd@2.2"}} {
		for _, fail := range []bool{false, true} {
			t.Run(strings.Join(declared, ",")+map[bool]string{true: " unavailable", false: " reachable"}[fail], func(t *testing.T) {
				entered, release := make(chan struct{}, 2), make(chan struct{})
				qualifier := func(ctx context.Context, base, line string) error {
					if base != engine.DefaultLaneURL(line) {
						t.Errorf("address=%s", base)
					}
					entered <- struct{}{}
					if strings.Contains(strings.Join(declared, ","), "pa.crd@2.2") {
						if fail {
							return errors.New("wrong-line verdict")
						}
						return nil
					}
					select {
					case <-release:
						if fail {
							return errors.New("negative control passed")
						}
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				lanes, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, declared, canonical, config{}, engine.DefaultLaneURL, qualifier)
				if declared[0] == "pa.crd@2.2" && fail {
					if err == nil || !strings.Contains(err.Error(), "2.2") || !strings.Contains(err.Error(), "wrong-line verdict") {
						t.Fatalf("boot error=%v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer m.Close()
				if lanes["2.0"] != canonical {
					t.Fatal("configured canonical missing")
				}
				if declared[0] != "pa.crd@2.2" {
					<-entered
					if m.defaults["2.1"].Ready() {
						t.Fatal("warming lane ready")
					}
					if lanes["2.1"] != canonical || !m.fallbacks["2.1"] {
						t.Fatal("PDex fallback lost before qualification")
					}
					close(release)
					m.workers.Wait()
					if m.defaults["2.1"].Ready() == fail {
						t.Fatal("wrong readiness")
					}
					if lanes["2.1"] != canonical {
						t.Fatal("qualification overwrote PDex fallback")
					}
				}
			})
		}
	}
	t.Run("explicit override is never qualified", func(t *testing.T) {
		var calls atomic.Int32
		_, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.crd@2.2"}, canonical, config{FHIRValidateURL21: "http://explicit21/fhir", FHIRValidateURL22: "http://explicit22/fhir"}, engine.DefaultLaneURL, func(context.Context, string, string) error {
			calls.Add(1)
			return errors.New("unexpected qualification")
		})
		if err != nil {
			t.Fatal(err)
		}
		m.Close()
		if calls.Load() != 0 {
			t.Fatal("explicit endpoint probed")
		}
	})
	t.Run("close cancels and joins", func(t *testing.T) {
		entered, exited := make(chan struct{}, 2), make(chan struct{}, 2)
		_, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.pas@2.0"}, canonical, config{}, engine.DefaultLaneURL, func(ctx context.Context, base, line string) error {
			entered <- struct{}{}
			<-ctx.Done()
			exited <- struct{}{}
			return ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		<-entered
		m.Close()
		if len(exited) != 2 {
			t.Fatal("Close returned with a running worker")
		}
		m.Close()
	})
}

// laneRecording is a recording of what a real validator lane answered: one
// beside the validator code (../internal/lanequalify/testdata/recordings: the
// readiness corpus, the metadata) or this package's own
// (testdata/recordings: a lane qualified as another line, the certification
// warm-up), each with a README.md stating its capture and scrub.
func laneRecording(t *testing.T, name string) string {
	t.Helper()
	var found []string
	for _, dir := range []string{filepath.Join("..", "internal", "lanequalify", "testdata", "recordings"), filepath.Join("testdata", "recordings")} {
		if _, err := os.Stat(filepath.Join(dir, name+".json")); err == nil {
			found = append(found, filepath.Join(dir, name+".json"))
		}
	}
	switch len(found) {
	case 0:
		t.Fatalf("no recording %s.json beside the validator code or in this package", name)
	case 2:
		t.Fatalf("recording %s.json is in both %s and %s; name one", name, found[0], found[1])
	}
	return found[0]
}

// recordedLane replays, strictly, what a real validator lane answered (the
// named recordings, joined: a request the lane was never asked fails the test,
// and every recorded answer must be used unless the row calls Subset). It
// counts what it is asked; edit, when set, may change the recorded answer to a
// request before it is sent: a fault no capture holds, or a deliberate
// mutation of the recorded answer.
type recordedLane struct {
	*httptest.Server
	rec         *testrecord.Recording
	gets, posts atomic.Int32
}

func newRecordedLane(t *testing.T, edit func(r *http.Request, body []byte, status int, answer []byte) (int, []byte), names ...string) *recordedLane {
	t.Helper()
	recs := make([]*testrecord.Recording, 0, len(names))
	for _, name := range names {
		recs = append(recs, testrecord.Load(t, laneRecording(t, name)))
	}
	rec := recs[0]
	if len(recs) > 1 {
		rec = testrecord.Join(t, recs...)
	}
	replay := rec.Server()
	l := &recordedLane{rec: rec}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			l.gets.Add(1)
		} else {
			l.posts.Add(1)
		}
		if edit == nil {
			replay.Config.Handler.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		answer := httptest.NewRecorder()
		replay.Config.Handler.ServeHTTP(answer, r)
		status, out := edit(r, body, answer.Code, answer.Body.Bytes())
		w.Header().Set("Content-Type", answer.Header().Get("Content-Type"))
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}))
	t.Cleanup(l.Server.Close)
	return l
}

func TestDeclaredDefaultPassesTheExactFiniteCorpus(t *testing.T) {
	lane := newRecordedLane(t, nil, "lane-2.1-metadata", "lane-2.1-warm")
	_, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.dtr@2.1"}, shnsdk.NewFakeValidator(), config{FHIRValidateURL22: "http://configured.invalid/fhir"}, func(string) string { return lane.URL + "/fhir" }, qualifyDefaultLane)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if !m.defaults["2.1"].Ready() || int(lane.posts.Load()) != lanequalify.RowCount("2.1") || lane.gets.Load() != 1 {
		t.Fatalf("ready=%v metadata=%d rows=%d", m.defaults["2.1"].Ready(), lane.gets.Load(), lane.posts.Load())
	}
}

// Metadata only opens the qualification: a lane is admitted only when it
// answers the whole corpus as a lane of the declared line does. The base is
// the recorded 2.1 lane, which qualifies; each row changes one of its answers.
func TestDefaultLaneMetadataIsOnlyAStartingGate(t *testing.T) {
	t.Run("the recorded lane qualifies", func(t *testing.T) {
		lane := newRecordedLane(t, nil, "lane-2.1-metadata", "lane-2.1-warm")
		if err := qualifyDefaultLane(context.Background(), lane.URL+"/fhir", "2.1"); err != nil {
			t.Fatalf("the base the rows below edit must qualify: %v", err)
		}
	})
	metadata := recordedBody(t, "lane-2.1-metadata", 0)
	r5 := replaceOnce(t, metadata, `"fhirVersion":"4.0.1"`, `"fhirVersion":"5.0.0"`)
	var negativeEdits atomic.Int32
	for _, row := range []struct {
		name string
		edit func(r *http.Request, body []byte, status int, answer []byte) (int, []byte)
		want string
	}{
		// Authored: no lane answers its metadata with HTML (something in front of it might).
		{"HTML metadata", func(r *http.Request, _ []byte, _ int, answer []byte) (int, []byte) {
			if r.Method == http.MethodGet {
				return http.StatusOK, []byte(`<html>healthy</html>`)
			}
			return http.StatusOK, answer
		}, "not an R4 CapabilityStatement"},
		// The lane's own metadata answer, its FHIR version changed.
		{"wrong FHIR service", func(r *http.Request, _ []byte, status int, answer []byte) (int, []byte) {
			if r.Method == http.MethodGet {
				return status, r5
			}
			return status, answer
		}, "not an R4 CapabilityStatement"},
		// Authored: the lane's real metadata, then $validate answers that are not
		// an OperationOutcome (no lane sends them).
		{"metadata only", func(r *http.Request, _ []byte, status int, answer []byte) (int, []byte) {
			if r.Method == http.MethodPost {
				return http.StatusOK, []byte(`<html>not found</html>`)
			}
			return status, answer
		}, "row 1/42"},
		{"invalid outcome", func(r *http.Request, _ []byte, status int, answer []byte) (int, []byte) {
			if r.Method == http.MethodPost {
				return http.StatusOK, []byte(`{`)
			}
			return status, answer
		}, "row 1/42"},
		// The lane's own answers to the negative controls, their targeted
		// rejection removed: a lane that accepts the type mutation.
		{"negative control accidentally passes", func(r *http.Request, body []byte, status int, answer []byte) (int, []byte) {
			if r.URL.Path != "/fhir/ClaimResponse/$validate" || !bytes.Contains(body, []byte(`"valueBoolean":true`)) {
				return status, answer
			}
			edited, removed := withoutIssueCoded(answer, "Extension_EXT_Type")
			if removed != 1 {
				t.Errorf("mutation target: the recorded negative-control answer holds %d Extension_EXT_Type issues, want 1", removed)
			}
			negativeEdits.Add(1)
			return status, edited
		}, "row 32/42 negative-versioned"},
	} {
		t.Run(row.name, func(t *testing.T) {
			lane := newRecordedLane(t, row.edit, "lane-2.1-metadata", "lane-2.1-warm")
			lane.rec.Subset() // each row stops the corpus early
			err := qualifyDefaultLane(context.Background(), lane.URL+"/fhir", "2.1")
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("incomplete qualification: %v, want a failure naming %q", err, row.want)
			}
		})
	}
	if negativeEdits.Load() != 1 {
		t.Fatalf("the negative-control mutation applied %d times, want once", negativeEdits.Load())
	}
	// A lane qualified as a line it does not serve: the lane's own refusal of
	// the first decision row at the asked line's PAS version
	// (testdata/recordings/lane-<lane>-asked-<line>.json, with the lane's
	// recorded metadata).
	for _, pair := range [][2]string{{"2.0", "2.1"}, {"2.0", "2.2"}, {"2.1", "2.0"}, {"2.1", "2.2"}, {"2.2", "2.0"}, {"2.2", "2.1"}} {
		t.Run("lane "+pair[0]+" qualified as "+pair[1], func(t *testing.T) {
			lane := newRecordedLane(t, nil, "lane-"+pair[0]+"-metadata", "lane-"+pair[0]+"-asked-"+pair[1])
			err := qualifyDefaultLane(context.Background(), lane.URL+"/fhir", pair[1])
			if !errors.Is(err, lanequalify.ErrCorpus) || !strings.Contains(err.Error(), "row 5/42 prime-versioned-approved: unexpected verdict") || !strings.Contains(err.Error(), "error/processing: ") {
				t.Fatalf("a lane of %s qualified as %s: %v, want the corpus refused at row 5/42", pair[0], pair[1], err)
			}
			if lane.gets.Load() != 1 || lane.posts.Load() != 5 {
				t.Fatalf("metadata=%d rows=%d, want 1 and 5", lane.gets.Load(), lane.posts.Load())
			}
		})
	}
	// Authored: an answer that never comes, cut off by shutdown.
	t.Run("shutdown interrupts an active request", func(t *testing.T) {
		entered, exited := make(chan struct{}), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(exited) }))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- qualifyDefaultLane(ctx, server.URL, "2.2") }()
		<-entered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		<-exited
	})
}

// recordedBody is the answer body of exchange i in the named recording.
func recordedBody(t *testing.T, name string, i int) []byte {
	t.Helper()
	path := laneRecording(t, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := testrecord.Parse(path, raw)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return append([]byte(nil), rec.Exchanges[i].Response.Body...)
}

// replaceOnce is a deliberate mutation of a recorded answer's bytes; old must
// occur in it, so a mutation that stopped applying fails instead of passing
// the unchanged answer.
func replaceOnce(t *testing.T, raw []byte, old, replacement string) []byte {
	t.Helper()
	if !bytes.Contains(raw, []byte(old)) {
		t.Fatalf("mutation target %q is not in the answer", old)
	}
	return bytes.Replace(raw, []byte(old), []byte(replacement), 1)
}

// withoutIssueCoded is a recorded OperationOutcome without its issues whose
// message id is code, and how many it removed.
func withoutIssueCoded(answer []byte, code string) ([]byte, int) {
	var outcome map[string]any
	if json.Unmarshal(answer, &outcome) != nil {
		return answer, 0
	}
	issues, _ := outcome["issue"].([]any)
	kept := make([]any, 0, len(issues))
	for _, issue := range issues {
		details, _ := issue.(map[string]any)["details"].(map[string]any)
		codings, _ := details["coding"].([]any)
		if len(codings) == 1 && codings[0].(map[string]any)["code"] == code {
			continue
		}
		kept = append(kept, issue)
	}
	outcome["issue"] = kept
	out, err := json.Marshal(outcome)
	if err != nil {
		return answer, 0
	}
	return out, len(issues) - len(kept)
}

func TestQualificationEventsDescribeTerminalState(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var out bytes.Buffer
			old := log.Writer()
			log.SetOutput(&out)
			defer log.SetOutput(old)
			_, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.pas@2.1"}, shnsdk.NewFakeValidator(), config{FHIRValidateURL22: "http://explicit.test/fhir"}, engine.DefaultLaneURL, func(context.Context, string, string) error {
				if fail {
					return errors.New("private outcome payload must not be logged")
				}
				return nil
			})
			if m != nil {
				m.Close()
			}
			if (err != nil) != fail {
				t.Fatalf("error=%v", err)
			}
			records := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(records) != 2 {
				t.Fatalf("qualification events=%q", out.String())
			}
			for i, record := range records {
				_, raw, ok := strings.Cut(record, "gateway: validator_qualification ")
				var event struct {
					Version  int     `json:"version"`
					Line     string  `json:"line"`
					Base     string  `json:"base"`
					Host     string  `json:"host"`
					State    string  `json:"state"`
					Reason   string  `json:"reason"`
					At       string  `json:"at"`
					Duration float64 `json:"duration_ms"`
				}
				if !ok || json.Unmarshal([]byte(raw), &event) != nil {
					t.Fatalf("invalid event %q", record)
				}
				want := "started"
				if i == 1 {
					want = "ready"
					if fail {
						want = "failed"
					}
				}
				if event.Version != 1 || event.Line != "2.1" || event.Base != engine.DefaultLaneURL("2.1") || event.Host != "shn-validator-2-1" || event.State != want || event.Duration < 0 {
					t.Fatalf("event=%+v", event)
				}
				// A failure names why in the qualifier's own words; the failing
				// qualifier's text never reaches the log (checked below).
				wantReason := ""
				if want == "failed" {
					wantReason = "qualification did not pass"
				}
				if event.Reason != wantReason {
					t.Fatalf("reason=%q, want %q", event.Reason, wantReason)
				}
				if _, err := time.Parse(time.RFC3339Nano, event.At); err != nil {
					t.Fatal(err)
				}
			}
			if strings.Contains(out.String(), "private outcome") {
				t.Fatal("failure payload leaked")
			}
		})
	}
}

// A network with no Compose default validator services (a hosted tenant):
// FHIR_DEFAULT_VALIDATOR_LANES=none creates no default lane, so nothing probes
// their names, and a declared line then needs its own FHIR_VALIDATE_URL_<line>.
func TestNoDefaultLanesNeverProbes(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()
	none := config{DefaultValidatorLanes: defaultValidatorLanesNone}
	neverResolve := func(string) string { t.Fatal("a default lane name was resolved"); return "" }
	neverQualify := func(context.Context, string, string) error { t.Fatal("a default lane was probed"); return nil }

	lanes, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, nil, canonical, none, neverResolve, neverQualify)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	if len(m.defaults) != 0 || lanes["2.1"] != nil || lanes["2.2"] != nil {
		t.Fatalf("defaults=%d lanes=%v", len(m.defaults), lanes)
	}

	_, m, err = discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.crd@2.2"}, canonical, none, neverResolve, neverQualify)
	m.Close()
	if err == nil || !strings.Contains(err.Error(), "FHIR_VALIDATE_URL_2_2") {
		t.Fatalf("a declared line with no lane must refuse boot naming its setting, got %v", err)
	}

	with := none
	with.FHIRValidateURL22 = "http://validator-2-2.hosted.internal:8080/fhir"
	lanes, m, err = discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.crd@2.2"}, canonical, with, neverResolve, neverQualify)
	if err != nil || lanes["2.2"] == nil {
		t.Fatalf("a configured line boots without probing: lanes=%v err=%v", lanes, err)
	}
	m.Close()
}

// Unset keeps the default lanes; the only other value boots is "none".
func TestDefaultValidatorLanesValue(t *testing.T) {
	for value, ok := range map[string]bool{"": true, "none": true, "off": false, "NONE": false, "compose": false} {
		e := baseEnv(map[string]string{"FHIR_DEFAULT_VALIDATOR_LANES": value})
		_, err := loadConfig(func(k string) string { return e[k] })
		if (err == nil) != ok {
			t.Errorf("FHIR_DEFAULT_VALIDATOR_LANES=%q: err=%v", value, err)
		}
		if err != nil && !strings.Contains(err.Error(), "FHIR_DEFAULT_VALIDATOR_LANES") {
			t.Errorf("the refusal must name the setting: %v", err)
		}
	}
}

// With no default lanes, a line with no certification address is unavailable
// in the evidence, naming what to configure, and nothing is dialed for it.
func TestNoDefaultLanesCertificationNamesTheMissingLane(t *testing.T) {
	cfg := config{DefaultValidatorLanes: defaultValidatorLanesNone, FHIRCertifyURL22: "http://validator-2-2.hosted.internal:8080/fhir"}
	vs := certificationValidators(func(string) string { return "" }, cfg, "http://validator.hosted.internal:8080/fhir", nil, func(context.Context, string, string) error {
		t.Fatal("a certification lane was probed")
		return nil
	})
	_, err := vs["2.1"].Validate(context.Background(), []byte(`{}`), "profile")
	var missing *engine.CertificationLaneUnavailable
	if !errors.As(err, &missing) || !strings.Contains(err.Error(), "FHIR_CERTIFY_URL_2_1 and FHIR_VALIDATE_URL_2_1 are not configured") {
		t.Fatalf("2.1 evidence: %v", err)
	}
	if vs["2.2"] == nil || vs["2.0"] == nil {
		t.Fatal("configured lines keep their certification clients")
	}
	// Default lanes kept: an unconfigured line with no default gets no client,
	// as before.
	if vs := certificationValidators(func(string) string { return "" }, config{}, "http://validator:8080/fhir", nil, nil); vs["2.1"] != nil {
		t.Fatal("unset changed the default-lane behavior")
	}
}

// The real qualifier fails naming why: a refused connection, or a metadata
// status, not only that its budget ran out. The refused connection is real (a
// closed port, what a lane still booting does); the 503 is authored, a status
// something in front of a lane may answer.
func TestDefaultLaneQualifierNamesWhy(t *testing.T) {
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer unavailable.Close()
	closed := downEndpoint(t)
	for base, want := range map[string]string{closed + "/fhir": "connection refused", unavailable.URL + "/fhir": "metadata answered HTTP 503"} {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		err := qualifyDefaultLane(ctx, base, "2.1")
		cancel()
		if got := lanequalify.FailureReason(err); got != want {
			t.Errorf("%s: reason %q, want %q (%v)", base, got, want, err)
		}
	}
}

// none never widens a line: a single-line contract's line (pa.pdex@2.1, in the
// build's default declaration) rides the canonical validator as a fallback
// alias exactly as it does with default lanes, so PA traffic at 2.1 stays
// unlaned (the engine refuses it; TestDTRContextCannotUseSingleContractFallback)
// rather than validating against the canonical IG.
func TestNoDefaultLanesKeepsSingleLineAliasesFallbacks(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()
	declared := shnsdk.SupportedContractVersions()
	failing := func(context.Context, string, string) error { return errors.New("no default lane here") }
	_, unset, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, declared, canonical, config{}, engine.DefaultLaneURL, failing)
	if err != nil {
		t.Fatal(err)
	}
	unset.Close()
	_, none, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, declared, canonical, config{DefaultValidatorLanes: defaultValidatorLanesNone}, engine.DefaultLaneURL, failing)
	if err != nil {
		t.Fatal(err)
	}
	none.Close()
	if !unset.fallbacks["2.1"] || !none.fallbacks["2.1"] {
		t.Fatalf("2.1 must be a fallback alias either way: unset=%v none=%v", unset.fallbacks, none.fallbacks)
	}
	for line := range unset.fallbacks {
		if !none.fallbacks[line] {
			t.Fatalf("none dropped the fallback mark on %s", line)
		}
	}
	// A configured 2.1 lane is a real lane, never an alias.
	laned := config{DefaultValidatorLanes: defaultValidatorLanesNone, FHIRValidateURL21: "http://validator-2-1.example:8080/fhir"}
	_, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, declared, canonical, laned, engine.DefaultLaneURL, failing)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	if m.fallbacks["2.1"] {
		t.Fatal("a configured 2.1 lane was marked a fallback")
	}
}
