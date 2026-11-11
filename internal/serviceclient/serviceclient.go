// Package serviceclient is the redirect policy of the gateway's clients for
// the network's own services (the Hub, the Authorization Framework, consent,
// the registrar and the audit endpoint), its OTLP export, and its validator
// and certification sidecars: they follow no redirect at all. A network
// service or a local sidecar never redirects a request legitimately, so a
// 3xx answer is that call's failure, the endpoint not answering usably, and
// is never followed, whatever its code or origin. Every refusal is a
// *RedirectError naming the class RedirectRefused, the status and the
// Location's origin, never its path or query; one from https to http is also
// smartauth.ErrCleartextRedirect, as it is for every other client. A 304 is a
// 3xx too and is refused like the rest: none of these calls sends a
// conditional request, so none is answered 304 legitimately. For a client
// whose policy sits beneath its endpoint tag (endpointcert: ServiceClient and
// Transport over a tagged transport), the refusal comes before the tag reads
// the answer, so a redirecting endpoint's certificate is not recorded; a
// certification client, whose tag sits inside its own transport and which
// applies Check after it, records it.
//
// The clients that reach a participant's own system keep smartauth's
// participant policy instead (smartauth.ParticipantClient).
package serviceclient

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
)

// RedirectRefused is the failure class each caller's existing failure record
// carries for a call its endpoint answered with a redirect, which was not
// followed: told apart from an endpoint that cannot be reached.
const RedirectRefused = "redirect-refused"

// ErrRedirect is a network service's or sidecar's redirect, not followed.
// A *RedirectError is it for one answer.
var ErrRedirect = errors.New("serviceclient: a network service or sidecar call follows no redirect")

// RedirectError is one 3xx answer, not followed: its status and the origin
// (scheme, host and port) its Location named, never the path or query.
// Origin is empty when the answer had no Location, or one that could not be
// read. Cleartext is a Location from https to http: the error is then also
// smartauth.ErrCleartextRedirect, as every other client names that hop.
type RedirectError struct {
	StatusCode int
	Origin     string
	Cleartext  bool
}

func (e *RedirectError) Error() string {
	to := e.Origin
	if to == "" {
		to = "no origin it could read"
	}
	if e.Cleartext {
		return fmt.Sprintf("serviceclient: %s: the endpoint answered %d, a redirect from https to http (%s); a hop from https to http is never made", RedirectRefused, e.StatusCode, to)
	}
	return fmt.Sprintf("serviceclient: %s: the endpoint answered %d, a redirect to %s; a network service or sidecar call follows no redirect", RedirectRefused, e.StatusCode, to)
}

// Is reports ErrRedirect, and smartauth.ErrCleartextRedirect for a hop from
// https to http.
func (e *RedirectError) Is(target error) bool {
	return target == ErrRedirect || (e.Cleartext && target == smartauth.ErrCleartextRedirect)
}

// IsRedirectRefused reports whether err is a redirect this policy refused:
// a *RedirectError, wrapped in any way (net/http's *url.Error included). A
// hop from https to http that a participant's client refused
// (smartauth.ErrCleartextRedirect alone) is not: that client keeps its own
// policy and its own failure class.
func IsRedirectRefused(err error) bool {
	return errors.Is(err, ErrRedirect)
}

// RedirectStatus is the status of the refused answer err carries, 0 when err
// is not a *RedirectError.
func RedirectStatus(err error) int {
	var re *RedirectError
	if errors.As(err, &re) {
		return re.StatusCode
	}
	return 0
}

// ServiceClient returns a copy of c whose transport follows no redirect: a
// 3xx answer is caught below net/http's redirect handling and returned as the
// round trip's error, its body closed, so net/http never sees a Location to
// follow and its *url.Error names only the request's own URL. Every other
// setting (timeout, jar, the caller's own CheckRedirect, which then never
// runs) is c's. The caller's client is not changed; nil gets a fresh client.
// Wrapping a client ServiceClient already wrapped changes nothing.
func ServiceClient(c *http.Client) *http.Client {
	var out http.Client
	if c != nil {
		out = *c
	}
	out.Transport = Transport(out.Transport)
	return &out
}

// Transport is rt under the policy ServiceClient sets: nil is
// http.DefaultTransport, read at each request. A transport that records its
// endpoint kind (endpointcert) keeps that wrapper outermost, so its kind reads
// as before, with the policy beneath it. A transport already under the policy
// is returned as it is.
func Transport(rt http.RoundTripper) http.RoundTripper {
	if Refuses(rt) {
		return rt
	}
	if base, _, ok := endpointcert.Unwrap(rt); ok {
		return endpointcert.Rewrap(rt, Transport(base))
	}
	return &transport{base: rt}
}

// Refuses reports whether rt is under the policy: a transport Transport made,
// or one that records its endpoint kind over one.
func Refuses(rt http.RoundTripper) bool {
	for {
		if _, ok := rt.(*transport); ok {
			return true
		}
		base, _, ok := endpointcert.Unwrap(rt)
		if !ok {
			return false
		}
		rt = base
	}
}

// Check applies the policy to one round trip's result, for a transport of
// its own that cannot be wrapped (a certification client's): a 3xx answer
// becomes the error, its body closed; anything else is returned as it is.
func Check(req *http.Request, resp *http.Response, err error) (*http.Response, error) {
	if err != nil || resp == nil || resp.StatusCode/100 != 3 {
		return resp, err
	}
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}
	return nil, refusal(req.URL, resp)
}

// refusal is the error for resp, a 3xx answer to a request for from.
func refusal(from *url.URL, resp *http.Response) error {
	origin, cleartext := "", false
	if raw := resp.Header.Get("Location"); raw != "" {
		if loc, perr := from.Parse(raw); perr == nil && loc.Host != "" && loc.Scheme != "" {
			origin = strings.ToLower(loc.Scheme) + "://" + strings.ToLower(loc.Host)
			cleartext = strings.EqualFold(from.Scheme, "https") && strings.EqualFold(loc.Scheme, "http")
		}
	}
	return &RedirectError{StatusCode: resp.StatusCode, Origin: origin, Cleartext: cleartext}
}

// transport is the policy's RoundTripper over base (nil: http.DefaultTransport).
type transport struct{ base http.RoundTripper }

func (t *transport) next() http.RoundTripper {
	if t.base == nil {
		return http.DefaultTransport
	}
	return t.base
}

// RoundTrip implements http.RoundTripper.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next().RoundTrip(req)
	return Check(req, resp, err)
}

// CloseIdleConnections closes base's idle connections, as http.Client's
// CloseIdleConnections did on base itself.
func (t *transport) CloseIdleConnections() {
	if c, ok := t.next().(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// Base is the transport rt's policy wrapper sends through (nil for
// http.DefaultTransport) when rt is one Transport made; ok is false otherwise.
func Base(rt http.RoundTripper) (base http.RoundTripper, ok bool) {
	t, ok := rt.(*transport)
	if !ok {
		return rt, false
	}
	return t.base, true
}
