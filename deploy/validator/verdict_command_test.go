package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// verdictLane is the lane of line as the command meets it: qualify runs on a
// lane that has not validated yet (its prime rows get the lane's first
// answers), verify-verdicts on one the warm-up already settled. Neither
// command asks the four initialization rows, and verification asks 20 of the
// 42 recorded requests, so the rest go unused. Requests must not overlap:
// each answer is held open 1 ms after it is written, and a request that
// arrives before the previous answer ended fails the test.
func verdictLane(t *testing.T, line, command string) *fakeLane {
	t.Helper()
	lane := newLane(t, line, command == "verify-verdicts")
	lane.partial()
	lane.hold.Store(int64(time.Millisecond))
	t.Cleanup(func() {
		if n := lane.overlaps.Load(); n != 0 {
			t.Errorf("%d validation requests arrived while an earlier answer was still being sent", n)
		}
	})
	return lane
}

// Rejection row for the overlap guard: a second request sent after the first
// answer's bytes arrived, but before that answer ended, is counted.
func TestFakeLaneCountsARequestSentBeforeTheLastAnswerEnded(t *testing.T) {
	lane := newWarmedFakeLane(t, "2.2")
	lane.hold.Store(int64(300 * time.Millisecond))
	row := qualificationRows("2.2", "verify")[0]
	body, err := fixtureBody(row)
	if err != nil {
		t.Fatal(err)
	}
	send := func(lane *fakeLane) *http.Response {
		req, err := http.NewRequest(http.MethodPost, lane.base()+"/ClaimResponse/$validate?profile="+url.QueryEscape(row.profile), bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/fhir+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first := send(lane) // headers and body sent; the answer is still open
	second := send(lane)
	for _, resp := range []*http.Response{first, second} {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if lane.overlaps.Load() == 0 {
		t.Fatal("a request sent before the previous answer ended was not counted")
	}
	// Control: read to the end before sending, and nothing is counted.
	calm := newWarmedFakeLane(t, "2.2")
	calm.hold.Store(int64(time.Millisecond))
	for i := 0; i < 2; i++ {
		resp := send(calm)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if n := calm.overlaps.Load(); n != 0 {
		t.Fatalf("sequential requests counted %d overlaps", n)
	}
}

func emptyEnv(string) string { return "" }

func TestVerdictCommandsRunExactSerialCorpora(t *testing.T) {
	for command, wantCount := range map[string]int{"qualify": 38, "verify-verdicts": 20} {
		t.Run(command, func(t *testing.T) {
			lane := verdictLane(t, "2.2", command)
			args := []string{command, "--base", lane.base(), "--line", "2.2", "--pas-version", "2.2.1", "--budget", "5s"}
			if got := verdictCommand(args, emptyEnv); got != 0 {
				t.Fatalf("exit=%d", got)
			}
			posts := lane.recorded()
			if len(posts) != wantCount {
				t.Fatalf("posts=%d want %d", len(posts), wantCount)
			}
			for i, post := range posts {
				wantPath := "/fhir/ClaimResponse/$validate"
				if i >= wantCount-4 && i < wantCount-2 {
					wantPath = "/fhir/Claim/$validate"
				} else if i >= wantCount-8 && i < wantCount-4 {
					wantPath = "/fhir/Bundle/$validate"
				}
				if post.path != wantPath {
					t.Fatalf("post[%d] path=%q", i, post.path)
				}
			}
			if posts[0].profile != pasClaimResponseProfile+"|2.2.1" || posts[6].rawQuery != "" {
				t.Fatalf("request forms not preserved: first=%q meta-query=%q", posts[0].profile, posts[6].rawQuery)
			}
		})
	}
}

func TestVerdictCommandDefaultsFromImageConfiguration(t *testing.T) {
	lane := verdictLane(t, "2.1", "verify-verdicts")
	getenv := func(key string) string {
		switch key {
		case "SHN_VALIDATOR_BASE":
			return lane.base()
		case "SHN_IG_LINE":
			return "2.1"
		case pasVersionEnv:
			return "2.1.0"
		}
		return ""
	}
	if got := verdictCommand([]string{"verify-verdicts", "--budget", "5s"}, getenv); got != 0 || len(lane.recorded()) != 20 {
		t.Fatalf("exit=%d posts=%d", got, len(lane.recorded()))
	}
}

func TestVerdictCommandRejectsConfigurationBeforePost(t *testing.T) {
	lane := verdictLane(t, "2.2", "verify-verdicts")
	s := lane.srv
	cases := map[string][]string{
		"missing version": {"verify-verdicts", "--base", s.URL, "--line", "2.2"},
		"wrong version":   {"verify-verdicts", "--base", s.URL, "--line", "2.2", "--pas-version", "2.1.0"},
		"unknown line":    {"verify-verdicts", "--base", s.URL, "--line", "9.9", "--pas-version", "2.2.1"},
		"credentials":     {"verify-verdicts", "--base", "http://user:pass@localhost/fhir", "--line", "2.2", "--pas-version", "2.2.1"},
		"query":           {"verify-verdicts", "--base", s.URL + "/fhir?x=1", "--line", "2.2", "--pas-version", "2.2.1"},
		"fragment":        {"verify-verdicts", "--base", s.URL + "/fhir#x", "--line", "2.2", "--pas-version", "2.2.1"},
		"non-loopback":    {"verify-verdicts", "--base", "http://example.com/fhir", "--line", "2.2", "--pas-version", "2.2.1"},
		"https":           {"verify-verdicts", "--base", "https://localhost/fhir", "--line", "2.2", "--pas-version", "2.2.1"},
		"zero budget":     {"verify-verdicts", "--base", s.URL, "--line", "2.2", "--pas-version", "2.2.1", "--budget", "0s"},
		"unknown command": {"unknown", "--base", s.URL, "--line", "2.2", "--pas-version", "2.2.1"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			before := len(lane.recorded())
			if got := verdictCommand(args, emptyEnv); got != 1 {
				t.Fatalf("exit=%d", got)
			}
			if len(lane.recorded()) != before {
				t.Fatal("invalid configuration submitted POST")
			}
		})
	}
}

// The second row gets the lane's real answer for it with a status the lane
// never sent: the command stops there.
func TestVerdictCommandStopsAfterFirstBadOutcome(t *testing.T) {
	lane := newWarmedFakeLane(t, "2.2")
	answer := recordedAnswer(t, qualificationRows("2.2", "verify")[1], false)
	var posts atomic.Int32
	lane.validate = func(w http.ResponseWriter, _ *http.Request) bool {
		if posts.Add(1) != 2 {
			return true
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write(answer)
		return false
	}
	args := []string{"verify-verdicts", "--base", lane.base(), "--line", "2.2", "--pas-version", "2.2.1", "--budget", "5s"}
	if got := verdictCommand(args, emptyEnv); got != 1 || posts.Load() != 2 {
		t.Fatalf("exit=%d posts=%d", got, posts.Load())
	}
}

func TestVerdictCommandRejectsTransportFailuresWithoutRetry(t *testing.T) {
	// The first row's real answer on a lane the warm-up settled, under each
	// fault; the malformed bodies are written by hand.
	answer := string(recordedAnswer(t, qualificationRows("2.2", "verify")[0], false))
	for _, mode := range []string{"malformed", "missing-code", "ambiguous-outcome", "status", "redirect", "incomplete", "oversize", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			var posts atomic.Int32
			received := make(chan struct{}, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				select {
				case received <- struct{}{}:
				default:
				}
				switch mode {
				case "malformed":
					fmt.Fprint(w, `{"resourceType":`)
				case "missing-code":
					fmt.Fprint(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"information"}]}`)
				case "ambiguous-outcome":
					fmt.Fprint(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","Severity":"information","code":"processing","diagnostics":"unrelated validation failure"}]}`)
				case "status":
					w.WriteHeader(http.StatusUnprocessableEntity)
					fmt.Fprint(w, answer)
				case "redirect":
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(http.StatusTemporaryRedirect)
				case "incomplete":
					w.Header().Set("Content-Length", fmt.Sprint(len(answer)+500))
					fmt.Fprint(w, answer)
				case "oversize":
					fmt.Fprint(w, answer+strings.Repeat(" ", maxOutcomeBytes))
				case "timeout":
					// Hold the request until the client gives up, so the budget,
					// not the server, ends it. The fallback only bounds a broken run.
					select {
					case <-r.Context().Done():
					case <-time.After(10 * time.Second):
					}
				}
			}))
			budget := "2s"
			if mode == "timeout" {
				// Room for the dial on a loaded runner; the request is still
				// held past the budget, so the budget always expires.
				budget = "500ms"
			}
			args := []string{"verify-verdicts", "--base", s.URL, "--line", "2.2", "--pas-version", "2.2.1", "--budget", budget}
			if got := verdictCommand(args, emptyEnv); got != 1 {
				t.Fatalf("exit=%d", got)
			}
			select {
			case <-received:
			default:
				t.Fatal("the request never reached the server: the failure did not come from the answer")
			}
			if posts.Load() != 1 {
				t.Fatalf("posts=%d want 1 (no retry)", posts.Load())
			}
			s.Close()
		})
	}
}

func TestVerdictCommandDeadlineBoundsHangingRequest(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(release); s.Close() }()
	started := time.Now()
	args := []string{"verify-verdicts", "--base", s.URL, "--line", "2.2", "--pas-version", "2.2.1", "--budget", "20ms"}
	if got := verdictCommand(args, emptyEnv); got != 1 || time.Since(started) > time.Second {
		t.Fatalf("exit=%d elapsed=%s", got, time.Since(started))
	}
}

func TestVerdictCommandsIncludeExplicitProfileControls(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, command := range []string{"qualify", "verify-verdicts"} {
			t.Run(line+"/"+command, func(t *testing.T) {
				lane := verdictLane(t, line, command)
				version, _ := pasVersion(line)
				want := 20
				if command == "qualify" {
					want = 38
				}
				if code := verdictCommand([]string{command, "--base", lane.base(), "--line", line, "--pas-version", version, "--budget", "5s"}, emptyEnv); code != 0 {
					t.Fatalf("exit %d", code)
				}
				posts := lane.recorded()
				if len(posts) != want {
					t.Fatalf("posts %d want %d", len(posts), want)
				}
				dir := "testdata/"
				if line != "2.0" {
					dir += line + "/"
				}
				raw, err := fixtures.ReadFile(dir + "claimresponse-approved.json")
				if err != nil {
					t.Fatal(err)
				}
				for i, profile := range []string{pasClaimResponseProfile + "|9.9.9", "https://example.org/fhir/StructureDefinition/unavailable-profile"} {
					post := posts[want-2+i]
					if post.path != "/fhir/ClaimResponse/$validate" || post.profile != profile || !bytes.Equal(post.body, raw) || !bytes.Contains(post.body, []byte(`"`+pasClaimResponseProfile+`"`)) {
						t.Fatalf("control %d changed: %+v", i, post)
					}
				}
			})
		}
	}
}
func TestVerdictCommandsRejectFalseExplicitProfileVerdicts(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, command := range []string{"qualify", "verify-verdicts"} {
			for target := 0; target < 2; target++ {
				for _, severity := range []string{"information", "warning"} {
					t.Run(fmt.Sprintf("%s/%s/%d/%s", line, command, target, severity), func(t *testing.T) {
						lane := newLane(t, line, command == "verify-verdicts")
						lane.partial() // the hook answers the targeted control and the command stops there
						profiles := []string{pasClaimResponseProfile + "|9.9.9", "https://example.org/fhir/StructureDefinition/unavailable-profile"}
						lane.validate = func(w http.ResponseWriter, r *http.Request) bool {
							if r.URL.Query().Get("profile") != profiles[target] {
								return true
							}
							// A false verdict for an unavailable profile.
							fmt.Fprintf(w, `{"resourceType":"OperationOutcome","issue":[{"severity":%q,"code":"processing"}]}`, severity)
							return false
						}
						version, _ := pasVersion(line)
						stop := 19 + target
						if command == "qualify" {
							stop = 37 + target
						}
						if code := verdictCommand([]string{command, "--base", lane.base(), "--line", line, "--pas-version", version, "--budget", "5s"}, emptyEnv); code != 1 || len(lane.recorded()) != stop {
							t.Fatalf("exit=%d posts=%d want=%d", code, len(lane.recorded()), stop)
						}
					})
				}
			}
		}
	}
}
