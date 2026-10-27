//go:build !shn_coreonly

package app

import (
	"net/http"
	"strconv"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
)

// checksRoute and statsRoute are the patterns /internal/checks and
// /internal/stats are served as.
const (
	checksRoute = "/internal/checks"
	statsRoute  = "/internal/stats"
)

// mainListener is the main listener's routing: /internal/checks, by its
// decoded path and for any method, answered by runner behind token; then
// /internal/stats the same way, answered by stats behind the same gate; then
// everything else through inner (/health, then the gateway's own routes),
// unchanged. The request served as /internal/checks or /internal/stats
// carries that pattern (r.Pattern), as one a ServeMux serves does, so the
// HTTP server metric names its route; it routes nothing
// (TestMainListenerRoutesAsBefore).
func mainListener(runner *checks.Runner, token string, stats, inner http.Handler) http.Handler {
	checksH := checks.Handler(runner, token)
	// /internal/stats shares /internal/checks' access rule, by the same gate.
	statsH := checks.Guard(stats, token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/checks" {
			r.Pattern = checksRoute
			checksH.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/internal/stats" {
			r.Pattern = statsRoute
			statsH.ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// httpServerMetrics wraps the main listener's handler so every request it
// serves records http.server.request.duration with the semantic
// convention's attributes (MON-R29): the method; the status written, absent
// when a handler panicked before writing one; the route, the matched
// pattern's template as the muxes set it on the request (r.Pattern), never
// the path, absent when no route of the gateway's matched
// (telemetry.HTTPRoute); the scheme; the HTTP version; and error.type, the
// status for a 5xx and _OTHER for a handler that panicked, absent otherwise. A panic is recorded and then continues to the server.
func httpServerMetrics(reg *telemetry.Registry, clock func() time.Time, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := clock()
		sw := &statusWriter{ResponseWriter: w}
		panicked := true
		defer func() {
			status := sw.status
			if status == 0 && !panicked {
				status = http.StatusOK // a handler that wrote nothing answered 200
			}
			statusCode, errorType := "", ""
			if status != 0 {
				statusCode = strconv.Itoa(status)
			}
			switch {
			case panicked:
				errorType = telemetry.SemconvOther
			case status >= 500:
				errorType = statusCode
			}
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			reg.RecordDuration(telemetry.HTTPServerRequestDuration, clock().Sub(start),
				r.Method, statusCode, telemetry.HTTPRoute(r.Pattern), scheme, protocolVersion(r), errorType)
		}()
		next.ServeHTTP(sw, r)
		panicked = false
	})
}

// protocolVersion is the request's HTTP version as the convention spells it.
func protocolVersion(r *http.Request) string {
	if r.ProtoMajor == 2 {
		return "2"
	}
	return strconv.Itoa(r.ProtoMajor) + "." + strconv.Itoa(r.ProtoMinor)
}

// statusWriter notes the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 && code <= 999 { // a 1xx is not the answer; net/http refuses above 999
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
