//go:build !shn_coreonly

package app

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	health "github.com/SmartHealthNetwork/shn-sdk/health"
)

// statsAnswer stands in for /internal/stats' handler: the routing around it
// is what these tests hold.
var statsAnswer = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("stats")) })

// semconvServed serves r through the metric around the main listener as the
// gateway builds it (/internal/checks, then health.Wrap's /health, then
// routes), and returns the one measurement it recorded.
func semconvServed(t *testing.T, r *http.Request, routes func(*http.ServeMux)) telemetry.Measurement {
	t.Helper()
	sink := &captureSink{}
	inner := http.NewServeMux()
	routes(inner)
	outer := mainListener(checks.NewRunner(nil, http.DefaultClient, time.Now), "", statsAnswer, health.Wrap(health.New("gateway", "v-test"), inner))
	now := time.Unix(1_761_350_400, 0)
	clock := func() time.Time { now = now.Add(5 * time.Millisecond); return now }
	h := httpServerMetrics(telemetry.New(sink), clock, outer)
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), r)
	}()
	got := sink.of(telemetry.HTTPServerRequestDuration)
	if len(got) != 1 {
		t.Fatalf("%d measurements, want 1", len(got))
	}
	if got[0].Duration != 5*time.Millisecond {
		t.Fatalf("duration %v, want 5ms", got[0].Duration)
	}
	return got[0]
}

// MON-R29: every request the main listener serves records the convention's
// attributes: the route is the matched pattern's template, never the path,
// and absent when no route of the gateway's matched, a path the health
// wrapper redirects to a clean form only its catch-all matches among them
// (//health redirects as the health route); error.type is a 5xx's
// status, _OTHER for a panic (which still reaches the server), and absent
// otherwise.
func TestHTTPServerMetric_MONR29(t *testing.T) {
	routes := func(mux *http.ServeMux) {
		mux.HandleFunc("POST /cds-services/{id}", func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("id") == "fail" {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		})
		mux.HandleFunc("GET /metadata", func(http.ResponseWriter, *http.Request) { panic("boom") })
	}
	h2 := httptest.NewRequest(http.MethodPost, "https://gw.test/cds-services/order-select", nil)
	h2.ProtoMajor, h2.ProtoMinor, h2.TLS = 2, 0, &tls.ConnectionState{}
	// net/http serves any HTTP/1 minor version.
	h15 := httptest.NewRequest(http.MethodPost, "/cds-services/order-select", nil)
	h15.Proto, h15.ProtoMinor = "HTTP/1.5", 5
	for _, row := range []struct {
		name string
		r    *http.Request
		want []string
	}{
		{"a matched route", httptest.NewRequest(http.MethodPost, "/cds-services/order-select", nil),
			[]string{"POST", "200", "/cds-services/{id}", "http", "1.1", ""}},
		{"a 5xx", httptest.NewRequest(http.MethodPost, "/cds-services/fail", nil),
			[]string{"POST", "503", "/cds-services/{id}", "http", "1.1", "503"}},
		{"TLS and HTTP/2", h2, []string{"POST", "200", "/cds-services/{id}", "https", "2", ""}},
		{"another HTTP/1 version", h15, []string{"POST", "200", "/cds-services/{id}", "http", telemetry.SemconvOther, ""}},
		{"no route", httptest.NewRequest(http.MethodGet, "/Patient/123", nil), []string{"GET", "404", "", "http", "1.1", ""}},
		{"a method the route does not serve", httptest.NewRequest(http.MethodDelete, "/cds-services/x", nil),
			[]string{"DELETE", "405", "", "http", "1.1", ""}},
		{"an unknown method", httptest.NewRequest("BREW", "/Patient/123", nil), []string{telemetry.SemconvOther, "404", "", "http", "1.1", ""}},
		{"the checks route", httptest.NewRequest(http.MethodGet, "/internal/checks", nil), []string{"GET", "403", "/internal/checks", "http", "1.1", ""}},
		{"the stats route", httptest.NewRequest(http.MethodGet, "/internal/stats", nil), []string{"GET", "403", "/internal/stats", "http", "1.1", ""}},
		{"the health route", httptest.NewRequest(http.MethodGet, "/health", nil), []string{"GET", "200", "/health", "http", "1.1", ""}},
		{"the health route redirected", httptest.NewRequest(http.MethodGet, "//health", nil), []string{"GET", "307", "/health", "http", "1.1", ""}},
		{"a path the health wrapper redirects", httptest.NewRequest(http.MethodGet, "//metadata", nil), []string{"GET", "307", "", "http", "1.1", ""}},
		{"a dot path the health wrapper redirects", httptest.NewRequest(http.MethodGet, "/internal/checks/.", nil), []string{"GET", "307", "", "http", "1.1", ""}},
		{"a panic before any status", httptest.NewRequest(http.MethodGet, "/metadata", nil),
			[]string{"GET", "", "/metadata", "http", "1.1", telemetry.SemconvOther}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := semconvServed(t, row.r, routes); !slices.Equal(got.Attributes, row.want) {
				t.Fatalf("attributes %q, want %q", got.Attributes, row.want)
			}
		})
	}
	// The panic continues to the server.
	h := httpServerMetrics(telemetry.New(&captureSink{}), time.Now, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	defer func() {
		if recover() == nil {
			t.Fatal("the metric swallowed the handler's panic")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

// MON-R18: the status and error.type pairs a request records are exactly
// the ones the HTTP metric's series bound counts: every status code a
// handler can write, answered, and again after a panic, and a panic before
// any status.
func TestHTTPStatusErrorPairs_MON_R18(t *testing.T) {
	sink := &captureSink{}
	reg := telemetry.New(sink)
	serve := func(h http.HandlerFunc) {
		func() {
			defer func() { _ = recover() }()
			httpServerMetrics(reg, time.Now, h).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	for code := 100; code <= 999; code++ {
		serve(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		serve(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code); panic("after the status") })
	}
	serve(func(http.ResponseWriter, *http.Request) { panic("before any status") })
	serve(func(http.ResponseWriter, *http.Request) {})
	pairs := map[[2]string]bool{}
	for _, m := range sink.of(telemetry.HTTPServerRequestDuration) {
		pairs[[2]string{m.Attributes[1], m.Attributes[5]}] = true
	}
	if len(pairs) != telemetry.HTTPStatusErrorPairs() {
		t.Fatalf("%d status and error pairs recorded, the bound counts %d", len(pairs), telemetry.HTTPStatusErrorPairs())
	}
}

// MON-R29: the HTTP metric's routes are exactly the routes a gateway can
// mount, whatever its role and configuration, and the main listener's own:
// a route a gateway mounts that the list does not name would record as
// _OTHER, and a name no gateway mounts inflates the bound.
func TestHTTPRoutePatternsAreTheMountedRoutes(t *testing.T) {
	mounted := map[string]bool{"GET /health": true, "/internal/checks": true, "/internal/stats": true}
	full := map[string]string{
		"PROVIDER_DTR_POPULATE_URL":         "https://populate.test/fhir/Questionnaire/$populate",
		"PROVIDER_DAVINCI_INGRESS":          "1",
		"PROVIDER_DAVINCI_INGRESS_BASE_URL": "https://gw.test",
		"INGRESS_CLIENTS_FILE":              writeClientsFile(t, moduleClientsJSON(t, map[string]any{"module": engine.ModuleOnBehalf})),
	}
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
		for _, p := range b.gateway.RoutePatternsForTest() {
			if !slices.Contains(telemetry.HTTPRoutePatterns, p) {
				t.Errorf("%s mounts %q, which the HTTP metric's routes do not name", role, p)
			}
			mounted[p] = true
		}
	}
	var unmounted []string
	for _, p := range telemetry.HTTPRoutePatterns {
		if !mounted[p] {
			unmounted = append(unmounted, p)
		}
	}
	sort.Strings(unmounted)
	if len(unmounted) > 0 || len(mounted) != len(telemetry.HTTPRoutePatterns) {
		t.Errorf("routes no gateway mounted: %q; %d mounted, %d listed", unmounted, len(mounted), len(telemetry.HTTPRoutePatterns))
	}
}

// mainListenerBefore is the main listener's routing as it was before the HTTP
// server metric, verbatim: the shape TestMainListenerRoutesAsBefore holds
// mainListener to.
func mainListenerBefore(checksH, statsH, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/checks" {
			checksH.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/internal/stats" {
			statsH.ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// MON-R29: naming the checks and stats routes for the HTTP server metric
// changes no answer of the main listener. Every request below, with CHECKS_TOKEN set
// and unset, from loopback and from elsewhere, with no bearer, a wrong one
// and the right one, by every spelling of the path and by methods the route
// does and does not take, answers mainListener exactly as before: status,
// body and redirect. The escaped spellings reach the checks and stats
// handlers, as before.
func TestMainListenerRoutesAsBefore(t *testing.T) {
	const token = "checks-token-for-test"
	paths := []string{"/internal/checks", "/internal%2Fchecks", "//internal/checks", "/internal/checks/.", "/internal/./checks", "/internal/checks/",
		"/internal/stats", "/internal%2Fstats", "//internal/stats", "/internal/stats/.", "/internal/stats/", "/health", "/substrate/inbound"}
	for _, configured := range []string{"", token} {
		runner := checks.NewRunner(nil, http.DefaultClient, time.Now)
		inner := health.Wrap(health.New("gateway", "v-test"), http.NotFoundHandler())
		before := mainListenerBefore(checks.Handler(runner, configured), checks.Guard(statsAnswer, configured), inner)
		now := mainListener(runner, configured, statsAnswer, inner)
		for _, remote := range []string{"203.0.113.5:4100", "127.0.0.1:4100"} {
			for _, bearer := range []string{"", "Bearer wrong", "Bearer " + token} {
				for _, path := range paths {
					for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
						req := func() *http.Request {
							r := httptest.NewRequest(method, "http://gw.test"+path, nil)
							r.RemoteAddr = remote
							if bearer != "" {
								r.Header.Set("Authorization", bearer)
							}
							return r
						}
						a, b := httptest.NewRecorder(), httptest.NewRecorder()
						before.ServeHTTP(a, req())
						now.ServeHTTP(b, req())
						if a.Code != b.Code || a.Body.String() != b.Body.String() || a.Header().Get("Location") != b.Header().Get("Location") {
							t.Errorf("token set=%v %s %s from %s with %q: before %d %q %q, now %d %q %q", configured != "", method, path, remote, bearer,
								a.Code, a.Body.String(), a.Header().Get("Location"), b.Code, b.Body.String(), b.Header().Get("Location"))
						}
					}
				}
			}
		}
	}
	// The table compares like with like; these rows say what the rule is. A
	// request the rule admits reaches the handler: the checks answer 503
	// until their first run completes, and the stats stand-in 200.
	for _, row := range []struct {
		configured, remote, bearer, method, path string
		want                                     int
	}{
		{token, "203.0.113.5:4100", "", http.MethodGet, "/internal/checks", http.StatusUnauthorized},
		{token, "203.0.113.5:4100", "", http.MethodGet, "/internal/stats", http.StatusUnauthorized},
		{token, "203.0.113.5:4100", "Bearer " + token, http.MethodGet, "/internal/stats", http.StatusOK},
		{"", "127.0.0.1:4100", "", http.MethodGet, "/internal/stats", http.StatusOK},
		{"", "203.0.113.5:4100", "", http.MethodGet, "/internal/stats", http.StatusForbidden},
		{token, "203.0.113.5:4100", "Bearer wrong", http.MethodGet, "/internal/checks", http.StatusUnauthorized},
		{token, "203.0.113.5:4100", "Bearer " + token, http.MethodGet, "/internal/checks", http.StatusServiceUnavailable},
		{token, "127.0.0.1:4100", "", http.MethodGet, "/internal/checks", http.StatusUnauthorized},
		{"", "127.0.0.1:4100", "", http.MethodGet, "/internal/checks", http.StatusServiceUnavailable},
		{"", "203.0.113.5:4100", "", http.MethodGet, "/internal/checks", http.StatusForbidden},
		{"", "203.0.113.5:4100", "", http.MethodGet, "/internal%2Fchecks", http.StatusForbidden},
		{token, "203.0.113.5:4100", "Bearer " + token, http.MethodDelete, "/internal/checks", http.StatusMethodNotAllowed},
		{token, "203.0.113.5:4100", "", http.MethodGet, "//internal/checks", http.StatusTemporaryRedirect},
	} {
		inner := health.Wrap(health.New("gateway", "v-test"), http.NotFoundHandler())
		h := mainListener(checks.NewRunner(nil, http.DefaultClient, time.Now), row.configured, statsAnswer, inner)
		r := httptest.NewRequest(row.method, "http://gw.test"+row.path, nil)
		r.RemoteAddr = row.remote
		if row.bearer != "" {
			r.Header.Set("Authorization", row.bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != row.want {
			t.Errorf("token set=%v %s %s from %s with %q: %d, want %d", row.configured != "", row.method, row.path, row.remote, row.bearer, w.Code, row.want)
		}
	}
}
