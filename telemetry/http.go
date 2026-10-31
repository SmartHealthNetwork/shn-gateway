package telemetry

import (
	"net/http"
	"strconv"
	"strings"
)

// The HTTP server request duration (MON-R29): OpenTelemetry's semantic
// convention http.server.request.duration, for every request the main
// listener serves. It is OpenTelemetry only: no EMF line. Its attribute
// keys and values are the convention's own (http.*, url.scheme,
// network.protocol.version, error.type), each from a closed list here.

// SemconvOther is the semantic conventions' value for anything a closed list
// does not name.
const SemconvOther = "_OTHER"

// The semantic-convention attribute keys.
const (
	KeyHTTPMethod      = "http.request.method"
	KeyHTTPStatusCode  = "http.response.status_code"
	KeyHTTPRoute       = "http.route"
	KeyURLScheme       = "url.scheme"
	KeyProtocolVersion = "network.protocol.version"
	KeyErrorType       = "error.type"
)

var (
	// HTTPMethods are the methods the convention knows by default; any
	// other is _OTHER.
	HTTPMethods = []string{"CONNECT", "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "TRACE"}
	// HTTPStatusCodes are every status code net/http can write, 100 to 999
	// (it panics on any other), as strings: each is exported as the integer
	// it is, so the attribute has one type.
	HTTPStatusCodes = func() (out []string) {
		for code := 100; code <= 999; code++ {
			out = append(out, strconv.Itoa(code))
		}
		return out
	}()
	// HTTPErrorTypes are the error types a server request records: a 5xx
	// status code, and _OTHER for a request that failed with no such status
	// (its handler panicked, or it answered 600 or above). A request that
	// did not fail records none.
	HTTPErrorTypes = func() (out []string) {
		for code := 500; code <= 599; code++ {
			out = append(out, strconv.Itoa(code))
		}
		return append(out, SemconvOther)
	}()
	// URLSchemes are the main listener's schemes: https when it serves TLS.
	URLSchemes = []string{"http", "https"}
	// ProtocolVersions are the HTTP versions net/http names; it also serves any
	// other HTTP/1 minor version, recorded as _OTHER.
	ProtocolVersions = []string{"1.0", "1.1", "2"}
)

// HTTPRoutePatterns are the patterns of every route a gateway's main
// listener can serve, whatever its role and configuration: the engine's
// (engine.Gateway.RoutePatternsForTest, pinned to this list by
// TestHTTPRoutePatternsAreTheMountedRoutes), /health, /internal/checks and
// /internal/stats.
// A pattern without a method serves any method; a GET pattern also serves
// HEAD.
var HTTPRoutePatterns = []string{
	"GET /health",
	"/internal/checks",
	"/internal/stats",
	"POST /substrate/inbound",
	"GET /cds-services",
	"POST /cds-services/{id}",
	"POST /Questionnaire/$questionnaire-package",
	"POST /Claim/$submit",
	"POST /Claim/$inquire",
	"POST /CoverageEligibilityRequest/$submit",
	"POST /$submit-attachment",
	"POST /cdex/payer/{payer}",
	"POST /cdex/payer/{payer}/$submit-attachment",
	"GET /cdex/payers",
	"GET /metadata",
	"POST /oauth/token",
	"GET /.well-known/smart-configuration",
	"GET /.well-known/davinci-configuration",
	"POST /Task",
	"POST /scenario/uc01",
	"POST /scenario/uc02",
	"POST /scenario/uc02-payerb",
	"POST /scenario/uc02-unknownpayer",
	"POST /scenario/uc03",
	"POST /scenario/uc04",
	"POST /scenario/uc05",
	"POST /scenario/uc06",
	"POST /scenario/uc07",
	"POST /scenario/uc07hcpcs",
	"POST /scenario/uc08",
	"POST /scenario/homeoxygen",
	"POST /scenario/dispatch",
	"POST /scenario/uc06/start",
	"POST /scenario/uc06/complete",
	"POST /scenario/uc06/cancel",
	"POST /scenario/uc07/start",
	"POST /scenario/uc07/complete",
	"POST /scenario/uc07/cancel",
	"GET /scenario/uc07/pending",
	"POST /scenario/pa/inquire",
	"POST /scenario/reset",
}

// HTTPRoute is the convention's http.route of the pattern a request was
// served as: the path template of one of HTTPRoutePatterns, without the
// method, and "" for any other pattern, which matched no route of the
// gateway's. The health wrapper's catch-all "/" is one: it answers a path
// that is not clean (//metadata, /internal/checks/.) with a redirect before
// the gateway's own routes see the request.
func HTTPRoute(pattern string) string {
	if !httpRoutePatternSet[pattern] {
		return ""
	}
	if method, path, ok := strings.Cut(pattern, " "); ok && method != "" && !strings.Contains(method, "/") {
		return path
	}
	return pattern
}

// patternMethods are the methods a pattern serves.
func patternMethods(pattern string) []string {
	method, _, ok := strings.Cut(pattern, " ")
	switch {
	case !ok || strings.Contains(method, "/"):
		return append(append([]string(nil), HTTPMethods...), SemconvOther)
	case method == http.MethodGet:
		return []string{http.MethodGet, http.MethodHead}
	}
	return []string{method}
}

var httpRoutePatternSet = func() map[string]bool {
	set := map[string]bool{}
	for _, p := range HTTPRoutePatterns {
		set[p] = true
	}
	return set
}()

var httpRouteValues = func() (out []string) {
	for _, p := range HTTPRoutePatterns {
		out = append(out, HTTPRoute(p))
	}
	return out
}()

// httpServerBound is the most series http.server.request.duration can record
// (MON-R18). It is a cap, since not every status pair meets every route: a
// request no route matched answers only a few codes. Its attributes are not
// independent: a route serves only its pattern's methods (no route, any
// method), and error.type is the status code's. So it is, for each scheme and
// version (the three versions net/http names, and _OTHER for any other HTTP/1
// minor version it serves), the method-route pairs a request can be served
// as, times the status and error pairs a request can record
// (TestHTTPStatusErrorPairs_MON_R18 serves every one of them):
//   - a 1xx none: it is never the answer recorded;
//   - each status from 200 to 599 two: its own error type (none below 500,
//     its code from 500), and _OTHER for a handler that panicked after
//     writing it;
//   - each status from 600 one: _OTHER, panicked or not;
//   - and no status with _OTHER, for a handler that panicked before writing.
//
// TestHTTPServerBound_MON_R18 pins it.
func httpServerBound() int {
	routes := len(HTTPMethods) + 1 // no route matched: any method
	for _, p := range HTTPRoutePatterns {
		routes += len(patternMethods(p))
	}
	return len(URLSchemes) * (len(ProtocolVersions) + 1) * routes * HTTPStatusErrorPairs()
}

// HTTPStatusErrorPairs is the number of status and error.type pairs a server
// request can record (httpServerBound).
func HTTPStatusErrorPairs() int {
	pairs := 1 // a handler that panicked before writing a status
	for _, c := range HTTPStatusCodes {
		switch code, _ := strconv.Atoi(c); {
		case code < 200:
		case code < 600:
			pairs += 2
		default:
			pairs++
		}
	}
	return pairs
}

// HTTPServerRequestDuration is the semantic convention's server request
// duration (MON-R29), OpenTelemetry only. Its series bound, 682,168, is a
// cap the SDK's cardinality limit is set to; a gateway records far fewer.
var HTTPServerRequestDuration = &Instrument{Name: "http.server.request.duration", Kind: Histogram, Unit: "s",
	Description: "Duration of HTTP server requests.",
	Attributes: []Attribute{
		{Key: KeyHTTPMethod, Values: HTTPMethods, OtherValue: SemconvOther},
		{Key: KeyHTTPStatusCode, Values: HTTPStatusCodes, OtherValue: SemconvOther, Int: true, Optional: true},
		{Key: KeyHTTPRoute, Values: httpRouteValues, OtherValue: SemconvOther, Optional: true},
		{Key: KeyURLScheme, Values: URLSchemes, OtherValue: SemconvOther},
		{Key: KeyProtocolVersion, Values: ProtocolVersions, OtherValue: SemconvOther},
		{Key: KeyErrorType, Values: HTTPErrorTypes, OtherValue: SemconvOther, Optional: true},
	},
	Buckets: DurationBuckets, Bound: httpServerBound()}
