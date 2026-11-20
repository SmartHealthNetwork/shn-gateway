//go:build !shn_coreonly

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	health "github.com/SmartHealthNetwork/shn-sdk/health"
)

// -probe answers 0 for a loopback URL that answers 200, writing nothing; 1,
// with one line, for any other answer, a redirect it does not follow, or a
// port that refuses; and 2, with one line, for a usage that is not one
// loopback http URL. Any other argument is not the probe.
func TestProbeMode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /down", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	mux.HandleFunc("GET /moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/health", http.StatusFound) })
	// /health's slowest answer: a store ping bounded at 2 s, then 200.
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	closed := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		return "http://" + addr + "/health"
	}()
	for _, row := range []struct {
		args []string
		code int
	}{
		{[]string{"-probe", srv.URL + "/health"}, 0},
		{[]string{"-probe", srv.URL + "/slow"}, 0},
		{[]string{"-probe", "http://localhost:" + itoa(port) + "/health"}, 2},
		{[]string{"-probe", "http://127.0.0.1:99999/health"}, 2},
		{[]string{"-probe", "http://127.0.0.1:0/health"}, 2},
		{[]string{"-probe", "http://user:s3cret-pass@127.0.0.1:" + itoa(port) + "/health"}, 2},
		{[]string{"-probe", "http://user:s3cret%zzpass@127.0.0.1:" + itoa(port) + "/health"}, 2},
		{[]string{"-probe", srv.URL + "/down"}, 1},
		{[]string{"-probe", srv.URL + "/moved"}, 1},
		{[]string{"-probe", closed}, 1},
		{[]string{"-probe", "http://example.com/health"}, 2},
		{[]string{"-probe", "http://10.0.0.1:8080/health"}, 2},
		{[]string{"-probe", "https://127.0.0.1:" + itoa(port) + "/health"}, 2},
		{[]string{"-probe", "http://user:pass@127.0.0.1:" + itoa(port) + "/health"}, 2},
		{[]string{"-probe", "127.0.0.1:8080/health"}, 2},
		{[]string{"-probe"}, 2},
		{[]string{"-probe", srv.URL + "/health", "extra"}, 2},
	} {
		var out strings.Builder
		code, ok := probeMode(row.args, &out)
		if !ok {
			t.Errorf("%q: not answered as the probe", row.args)
			continue
		}
		if code != row.code {
			t.Errorf("%q: exit %d, want %d (%s)", row.args, code, row.code, out.String())
		}
		lines := strings.Count(out.String(), "\n")
		if (row.code == 0 && out.Len() != 0) || (row.code != 0 && lines != 1) {
			t.Errorf("%q: wrote %q, want nothing on success and one line otherwise", row.args, out.String())
		}
		if strings.Contains(out.String(), "s3cret") || strings.Contains(out.String(), itoa(port)) || (len(row.args) > 1 && strings.Contains(out.String(), row.args[1])) {
			t.Errorf("%q: repeated the URL: %q", row.args, out.String())
		}
	}
	for _, args := range [][]string{nil, {}, {"-version"}, {"--probe", srv.URL}, {"probe", srv.URL}} {
		var out strings.Builder
		if _, ok := probeMode(args, &out); ok || out.Len() != 0 {
			t.Errorf("%q answered as the probe: %q", args, out.String())
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// The probe's loopback is a loopback address written as one, IPv4 or IPv6,
// and never a name to resolve.
func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": false, "127.0.0.1": true, "127.0.0.2": true, "::1": true, "::ffff:127.0.0.1": true,
		"0.0.0.0": false, "::": false, "10.0.0.1": false, "example.com": false, "localhost.example.com": false, "": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// The built binary answers -probe with an empty environment, before it reads
// any setting: 0 against a port that answers 200, 1 against one that does not.
func TestProbeAnswersWithNothingConfigured(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the gateway binary")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	bin := filepath.Join(t.TempDir(), "gateway")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	for _, row := range []struct {
		path string
		code int
	}{{"/health", 0}, {"/missing", 1}} {
		cmd := exec.Command(bin, "-probe", srv.URL+row.path)
		cmd.Env, cmd.Dir = []string{}, t.TempDir()
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != row.code {
			t.Errorf("%s: exit %d (%s), want %d", row.path, code, out, row.code)
		}
		// The probe answered, not a gateway that failed to boot: nothing on
		// success, and its own one line otherwise.
		if (row.code == 0 && len(out) != 0) || (row.code != 0 && (!strings.HasPrefix(string(out), "gateway: -probe:") || strings.Count(string(out), "\n") != 1)) {
			t.Errorf("%s: wrote %q, want the probe's own answer", row.path, out)
		}
	}
}

// GET /health answers 200 whatever its checks report, so -probe of it says the
// gateway is serving: with a check that reports a dependency down (the body's
// status degraded, as a store that does not answer its ping reports), the
// answer is still 200, and the probe exits 0 and writes nothing.
func TestProbeOfADegradedHealthIsServing(t *testing.T) {
	reg := health.New("gateway", "v")
	reg.Register(func(context.Context) health.Check {
		return health.Check{Name: "store", Status: health.StatusDegraded, LastError: "ping failed"}
	})
	srv := httptest.NewServer(health.Wrap(reg, http.NotFoundHandler()))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Status string }
	err = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || body.Status != "degraded" {
		t.Fatalf("GET /health = %d %+v (%v), want 200 reporting degraded", resp.StatusCode, body, err)
	}
	var stderr bytes.Buffer
	if code := probe(srv.URL+"/health", &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("probe = %d %q, want 0 and nothing written", code, stderr.String())
	}
}
