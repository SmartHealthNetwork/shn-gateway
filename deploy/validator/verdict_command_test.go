package main

import (
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
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, recordedPost{path: r.URL.Path, profile: r.URL.Query().Get("profile"), rawQuery: r.URL.RawQuery, body: body})
		mu.Unlock()
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
	for command, wantCount := range map[string]int{"qualify": 34, "verify-verdicts": 16} {
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
				if i >= wantCount-4 {
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
	if got := verdictCommand([]string{"verify-verdicts", "--budget", "5s"}, getenv); got != 0 || len(*posts) != 16 {
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
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
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
					select {
					case <-r.Context().Done():
					case <-time.After(100 * time.Millisecond):
					}
				}
			}))
			budget := "2s"
			if mode == "timeout" {
				budget = "20ms"
			}
			args := []string{"verify-verdicts", "--base", s.URL, "--line", "2.2", "--pas-version", "2.2.1", "--budget", budget}
			if got := verdictCommand(args, emptyEnv); got != 1 {
				t.Fatalf("exit=%d", got)
			}
			if posts.Load() != 1 {
				t.Fatalf("posts=%d want 1", posts.Load())
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
