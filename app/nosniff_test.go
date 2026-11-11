//go:build !shn_coreonly

package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	health "github.com/SmartHealthNetwork/shn-sdk/health"
)

// wantNosniff fails unless h, an answer's headers, carries exactly one
// X-Content-Type-Options value, nosniff. The names are spelled out here, not
// read from the package that sets them, so a change to either is caught.
func wantNosniff(t *testing.T, h http.Header, what string) {
	t.Helper()
	if got := h.Values("X-Content-Type-Options"); len(got) != 1 || got[0] != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options %q, want exactly [nosniff]", what, got)
	}
}

// routeWildcard is a path wildcard of a ServeMux pattern, {x} or {x...}.
var routeWildcard = regexp.MustCompile(`\{[^}]*\}`)

// patternRequest is a request that names the route pattern p: its method (GET
// when p names none), its path with each wildcard filled in by a literal
// segment, and, for a method that takes one, a body of {}. It fails unless the
// request matches p on a ServeMux holding p alone.
func patternRequest(t *testing.T, ctx context.Context, p string) *http.Request {
	t.Helper()
	method, path, ok := strings.Cut(p, " ")
	if !ok {
		method, path = http.MethodGet, p
	}
	var body io.Reader
	if method != http.MethodGet {
		body = strings.NewReader(`{}`)
	}
	r := httptest.NewRequestWithContext(ctx, method, "http://gw.test"+routeWildcard.ReplaceAllString(path, "probe"), body)
	// Control: the request is one the route serves, not one the mux answers
	// 404 or 405 around it.
	alone := http.NewServeMux()
	alone.Handle(p, http.NotFoundHandler())
	if _, got := alone.Handler(r); got != p {
		t.Fatalf("control: %s %s matches pattern %q, want %q", r.Method, r.URL.Path, got, p)
	}
	return r
}

// loopbackOnly is an http.DefaultTransport that carries a request to a
// loopback host (the build's own local stand-ins) and refuses, and records,
// any other, so no request a route makes through the default transport dials
// the network. A transport a build makes for itself is not seen here; every
// request below carries no credential and is refused before one is used.
type loopbackOnly struct {
	base http.RoundTripper
	mu   sync.Mutex
	away []string
}

func (l *loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if ip := net.ParseIP(r.URL.Hostname()); ip != nil && ip.IsLoopback() {
		return l.base.RoundTrip(r)
	}
	l.mu.Lock()
	l.away = append(l.away, r.URL.Host)
	l.mu.Unlock()
	return nil, fmt.Errorf("nosniff test: no network: %s", r.URL.Host)
}

// Every answer the main listener writes carries X-Content-Type-Options:
// nosniff, whatever its status: every route each role mounts, the
// listener's own health, checks and stats routes, and a path no route
// serves. The handler is the one Run serves (serverFor around build's
// handler), and each request carries no credential, so the answers are the
// routes' own refusals or their unauthenticated answers, all back before a
// short deadline. Control: the routes walked cover every route a gateway
// can mount (telemetry.HTTPRoutePatterns), so a route added later is walked
// with no new row.
func TestMainListener_EveryAnswerCarriesNosniff(t *testing.T) {
	guard := &loopbackOnly{base: http.DefaultTransport}
	http.DefaultTransport = guard
	t.Cleanup(func() { http.DefaultTransport = guard.base })

	full := map[string]string{
		"PROVIDER_DTR_POPULATE_URL":         "https://populate.test/fhir/Questionnaire/$populate",
		"PROVIDER_DAVINCI_INGRESS":          "1",
		"PROVIDER_DAVINCI_INGRESS_BASE_URL": "https://gw.test",
		"INGRESS_CLIENTS_FILE":              writeClientsFile(t, moduleClientsJSON(t, map[string]any{"module": engine.ModuleOnBehalf})),
	}
	walked := map[string]bool{}
	for _, role := range []string{"provider", "payer", "facility", "phg"} {
		env := map[string]string{}
		switch role {
		case "provider":
			env = full
		case "payer":
			// A local stand-in for the payer's own system: the build may probe it.
			payerSystem := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(payerSystem.Close)
			env = map[string]string{"PAYER_DAVINCI_BASE_URL": payerSystem.URL}
		}
		b, _, err := buildRoleForPopulate(t, role, env)
		if err != nil {
			t.Fatalf("%s: build: %v", role, err)
		}
		h := serverFor("127.0.0.1:0", b.handler, nil, nil).Handler
		patterns := append([]string{"GET /health", "/internal/checks", "/internal/stats"}, b.gateway.RoutePatternsForTest()...)
		for _, p := range patterns {
			t.Run(role+"/"+p, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, patternRequest(t, ctx, p))
				if ctx.Err() != nil {
					t.Fatalf("answered %d only at the deadline", rec.Code)
				}
				wantNosniff(t, rec.Result().Header, fmt.Sprintf("%s answered %d", p, rec.Code))
			})
			walked[p] = true
		}
		t.Run(role+"/no route", func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gw.test/no-such-route", nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET /no-such-route: %d, want the mux's 404", rec.Code)
			}
			wantNosniff(t, rec.Result().Header, "the mux's 404")
		})
	}
	for _, p := range telemetry.HTTPRoutePatterns {
		if !walked[p] {
			t.Errorf("control: %q, a route a gateway can mount, was walked for no role", p)
		}
	}
	if len(guard.away) > 0 {
		t.Errorf("a route dialled beyond loopback: %q", guard.away)
	}
}

// The checks and stats routes' admitted answers carry nosniff too, not only
// their refusals: with CHECKS_TOKEN set and the right bearer, from a
// non-loopback caller, checks answers 503 until its first run completes and
// stats 200. Control: the statuses are those admitted answers.
func TestMainListener_AdmittedChecksAndStatsCarryNosniff(t *testing.T) {
	const token = "checks-token-for-test"
	inner := health.Wrap(health.New("gateway", "v-test"), http.NotFoundHandler())
	h := serverFor("", mainListener(checks.NewRunner(nil, http.DefaultClient, time.Now), token, statsAnswer, inner), nil, nil).Handler
	for _, row := range []struct {
		path string
		want int
	}{
		{"/internal/checks", http.StatusServiceUnavailable},
		{"/internal/stats", http.StatusOK},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://gw.test"+row.path, nil)
		r.RemoteAddr = "203.0.113.5:4100"
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != row.want {
			t.Fatalf("control: %s answered %d, want the admitted %d", row.path, rec.Code, row.want)
		}
		wantNosniff(t, rec.Result().Header, row.path)
	}
}

// A gateway Run serves carries nosniff over the wire, on the main listener
// and on the observer listener OBSERVER_ADDR opts into: the observer's demo
// endpoints answer with it (a capture fetch with edge capture off, 404, and
// a transform with a body it cannot read, 400), as does the main listener's
// health. Run is then stopped and joined.
func TestRun_MainAndObserverListenersCarryNosniff(t *testing.T) {
	env, _, _, release := lifecycleEnv(t, false)
	release()
	// A second port for the observer: freePort returns one port per test
	// name while it is free, so it is chosen under a subtest's name.
	var obsPort string
	for i := 0; obsPort == "" || obsPort == env["PORT"]; i++ {
		if i == 5 {
			t.Fatalf("no observer port apart from the main listener's %s", env["PORT"])
		}
		t.Run(fmt.Sprintf("observer port %d", i), func(t *testing.T) { obsPort = freePort(t) })
	}
	env["OBSERVER_ADDR"] = net.JoinHostPort("127.0.0.1", obsPort)
	mainAddr := net.JoinHostPort(env["HOST"], env["PORT"])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var runErr error
	go func() { runErr = Run(ctx, func(k string) string { return env[k] }, io.Discard); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for _, addr := range []string{mainAddr, env["OBSERVER_ADDR"]} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			c, err := net.Dial("tcp", addr)
			if err == nil {
				_ = c.Close()
				break
			}
			select {
			case <-done:
				t.Fatalf("Run returned before serving: %v", runErr)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("Run's listener at %s did not accept: %v", addr, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// The test's own transport: lifecycleEnv's default transport carries
	// only its fixture hosts.
	client := &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	for _, row := range []struct {
		name, method, url, body string
		want                    int
	}{
		{"main listener health", http.MethodGet, "http://" + mainAddr + "/health", "", http.StatusOK},
		{"observer capture fetch", http.MethodGet, "http://" + env["OBSERVER_ADDR"] + "/demo/capture/x", "", http.StatusNotFound},
		{"observer transform", http.MethodPost, "http://" + env["OBSERVER_ADDR"] + "/demo/transform", "not json", http.StatusBadRequest},
	} {
		req, err := http.NewRequest(row.method, row.url, strings.NewReader(row.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != row.want {
			t.Errorf("control: %s answered %d, want %d", row.name, resp.StatusCode, row.want)
		}
		wantNosniff(t, resp.Header, row.name)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after the stop")
	}
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
}

// A program that serves app.Handler gets nosniff on every answer, the main
// listener's own health, checks and stats included, as on the listener Run
// serves.
func TestAppHandler_EveryAnswerCarriesNosniff(t *testing.T) {
	env, _, _, release := lifecycleEnv(t, false)
	release()
	h, cleanup, err := HandlerForTest(context.Background(), func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	for _, row := range []struct {
		path string
		want int
	}{
		{"/health", http.StatusOK},
		{"/internal/checks", http.StatusForbidden},
		{"/internal/stats", http.StatusForbidden},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gw.test"+row.path, nil))
		if rec.Code != row.want {
			t.Errorf("control: %s answered %d, want %d", row.path, rec.Code, row.want)
		}
		wantNosniff(t, rec.Result().Header, fmt.Sprintf("app.Handler's %s answered %d", row.path, rec.Code))
	}
}
