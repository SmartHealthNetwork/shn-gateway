package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func verdictServer(t *testing.T) (*httptest.Server, *[]recordedPost) {
	t.Helper()
	var mu sync.Mutex
	posts := []recordedPost{}
	var active atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if active.Add(1) != 1 {
			t.Error("overlapping validation requests")
		}
		defer active.Add(-1)
		time.Sleep(time.Millisecond)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, recordedPost{path: r.URL.Path, profile: r.URL.Query().Get("profile"), rawQuery: r.URL.RawQuery, body: body})
		mu.Unlock()
		if outcome := explicitProfileTestOutcome(r.URL.Query().Get("profile")); outcome != "" {
			fmt.Fprint(w, outcome)
			return
		}
		if outcome := supportTestOutcome(body, r.URL.Query().Get("profile")); outcome != "" {
			fmt.Fprint(w, outcome)
			return
		}
		var resource map[string]any
		_ = json.Unmarshal(body, &resource)
		if strings.Contains(string(body), `"valueBoolean":true`) {
			fmt.Fprint(w, targetedNegativeOutcome)
			return
		}
		fmt.Fprint(w, cleanOutcome)
	}))
	t.Cleanup(s.Close)
	return s, &posts
}

func emptyEnv(string) string { return "" }

func TestVerdictCommandsRunExactSerialCorpora(t *testing.T) {
	for command, wantCount := range map[string]int{"qualify": 38, "verify-verdicts": 20} {
		t.Run(command, func(t *testing.T) {
			s, posts := verdictServer(t)
			args := []string{command, "--base", s.URL + "/fhir", "--line", "2.2", "--pas-version", "2.2.1", "--budget", "5s"}
			if got := verdictCommand(args, emptyEnv); got != 0 {
				t.Fatalf("exit=%d", got)
			}
			if len(*posts) != wantCount {
				t.Fatalf("posts=%d want %d", len(*posts), wantCount)
			}
			for i, post := range *posts {
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
			if (*posts)[0].profile != pasClaimResponseProfile+"|2.2.1" || (*posts)[6].rawQuery != "" {
				t.Fatalf("request forms not preserved: first=%q meta-query=%q", (*posts)[0].profile, (*posts)[6].rawQuery)
			}
		})
	}
}

func TestVerdictCommandDefaultsFromImageConfiguration(t *testing.T) {
	s, posts := verdictServer(t)
	getenv := func(key string) string {
		switch key {
		case "SHN_VALIDATOR_BASE":
			return s.URL + "/fhir"
		case "SHN_IG_LINE":
			return "2.1"
		case pasVersionEnv:
			return "2.1.0"
		}
		return ""
	}
	if got := verdictCommand([]string{"verify-verdicts", "--budget", "5s"}, getenv); got != 0 || len(*posts) != 20 {
		t.Fatalf("exit=%d posts=%d", got, len(*posts))
	}
}

func TestVerdictCommandRejectsConfigurationBeforePost(t *testing.T) {
	s, posts := verdictServer(t)
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
			before := len(*posts)
			if got := verdictCommand(args, emptyEnv); got != 1 {
				t.Fatalf("exit=%d", got)
			}
			if len(*posts) != before {
				t.Fatal("invalid configuration submitted POST")
			}
		})
	}
}

func TestVerdictCommandStopsAfterFirstBadOutcome(t *testing.T) {
	var posts int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if posts == 2 {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
		fmt.Fprint(w, cleanOutcome)
	}))
	defer s.Close()
	args := []string{"verify-verdicts", "--base", s.URL, "--line", "2.2", "--pas-version", "2.2.1", "--budget", "5s"}
	if got := verdictCommand(args, emptyEnv); got != 1 || posts != 2 {
		t.Fatalf("exit=%d posts=%d", got, posts)
	}
}

func TestVerdictCommandRejectsTransportFailuresWithoutRetry(t *testing.T) {
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
					fmt.Fprint(w, cleanOutcome)
				case "redirect":
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(http.StatusTemporaryRedirect)
				case "incomplete":
					w.Header().Set("Content-Length", "500")
					fmt.Fprint(w, cleanOutcome)
				case "oversize":
					fmt.Fprint(w, cleanOutcome+strings.Repeat(" ", maxOutcomeBytes))
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
				server, posts := verdictServer(t)
				version, _ := pasVersion(line)
				want := 20
				if command == "qualify" {
					want = 38
				}
				if code := verdictCommand([]string{command, "--base", server.URL + "/fhir", "--line", line, "--pas-version", version, "--budget", "5s"}, emptyEnv); code != 0 {
					t.Fatalf("exit %d", code)
				}
				if len(*posts) != want {
					t.Fatalf("posts %d want %d", len(*posts), want)
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
					post := (*posts)[want-2+i]
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
						posts := 0
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							posts++
							body, _ := io.ReadAll(r.Body)
							profile := r.URL.Query().Get("profile")
							profiles := []string{pasClaimResponseProfile + "|9.9.9", "https://example.org/fhir/StructureDefinition/unavailable-profile"}
							if profile == profiles[target] {
								fmt.Fprintf(w, `{"resourceType":"OperationOutcome","issue":[{"severity":%q,"code":"processing"}]}`, severity)
								return
							}
							if oo := explicitProfileTestOutcome(profile); oo != "" {
								fmt.Fprint(w, oo)
								return
							}
							if oo := supportTestOutcome(body, profile); oo != "" {
								fmt.Fprint(w, oo)
								return
							}
							if strings.Contains(string(body), `"valueBoolean":true`) {
								fmt.Fprint(w, targetedNegativeOutcome)
								return
							}
							fmt.Fprint(w, cleanOutcome)
						}))
						defer server.Close()
						version, _ := pasVersion(line)
						stop := 19 + target
						if command == "qualify" {
							stop = 37 + target
						}
						if code := verdictCommand([]string{command, "--base", server.URL + "/fhir", "--line", line, "--pas-version", version, "--budget", "5s"}, emptyEnv); code != 1 || posts != stop {
							t.Fatalf("exit=%d posts=%d want=%d", code, posts, stop)
						}
					})
				}
			}
		}
	}
}
