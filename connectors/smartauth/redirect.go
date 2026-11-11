package smartauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// The class of a redirect hop a participant's system answered with: its origin
// against the origin of the request that began the chain (the participant
// system the token was issued for) when the hop was followed, or the
// downgrade that stopped it. A closed list, never the URL.
const (
	// RedirectSameOrigin: the hop went to the chain's first origin (scheme,
	// host and port). It carried the bearer, and any header the deployment
	// configured (ParticipantClientWith), unless an earlier hop of the chain
	// had left that origin.
	RedirectSameOrigin = "same-origin"
	// RedirectCrossOrigin: the hop went to another origin, without the bearer
	// or any header the deployment configured; no later hop of the chain
	// carries them, even back at the first origin.
	RedirectCrossOrigin = "cross-origin"
	// RedirectCleartextRefused: the system asked for a hop from https to
	// http, and it was not sent (ErrCleartextRedirect); the chain ends there.
	RedirectCleartextRefused = "https-to-http-refused"
	// RedirectTokenRefused: the participant's authorization server answered a
	// token request with a redirect, within https or to another origin, and it
	// was not sent (ErrTokenRedirect): a token request follows no redirect,
	// since its body is the client's credential.
	RedirectTokenRefused = "token-redirect-refused"
	// RedirectCrossOriginRefused: the system answered a request that carried
	// a body with a redirect to another origin, and it was not sent
	// (ErrCrossOriginBodyRedirect): such a request follows a redirect only
	// within its own origin.
	RedirectCrossOriginRefused = "cross-origin-refused"
)

// RedirectValues is the closed list a hop's class draws from.
var RedirectValues = []string{RedirectSameOrigin, RedirectCrossOrigin, RedirectCleartextRefused, RedirectTokenRefused, RedirectCrossOriginRefused}

// ErrCleartextRedirect is a redirect from https to http, refused before the hop
// is sent, whatever its origin: a 307 or 308 replays the request body, which
// would travel in cleartext.
var ErrCleartextRedirect = errors.New("smartauth: a redirect from https to http is not followed")

// maxRedirects is net/http's own limit on a chain, kept.
const maxRedirects = 10

// RedirectObservation records the class of each redirect hop asked for by the
// answers to requests carrying its context: each hop followed, and a downgrade
// refused. It is safe for concurrent use.
type RedirectObservation struct {
	mu   sync.Mutex
	hops []string
}

// Hops is the class of each hop, in order (RedirectValues), nil when no answer
// asked for one.
func (o *RedirectObservation) Hops() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.hops)
}

func (o *RedirectObservation) hop(class string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.hops = append(o.hops, class)
}

type redirectObservationKey struct{}

// WithRedirectObservation derives a context whose requests' followed redirect
// hops are recorded on the returned handle, shadowing any inherited one.
func WithRedirectObservation(ctx context.Context) (context.Context, *RedirectObservation) {
	o := &RedirectObservation{}
	return context.WithValue(ctx, redirectObservationKey{}, o), o
}

// CheckRedirect is the one redirect policy for every client that reaches a
// participant's system, whether or not it carries a bearer: net/http's
// ten-hop limit, a hop from https to http refused before it is sent
// (RefuseCleartextRedirect), and each hop it lets through recorded with its
// origin class. NewHTTPClient's clients use it; set it on any other.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	return checkRedirectDropping(req, via, nil)
}

// checkRedirectDropping is CheckRedirect for a client whose requests may carry
// the configured headers named in configured: a hop to another origin carries
// none of them, nor Authorization.
func checkRedirectDropping(req *http.Request, via []*http.Request, configured []string) error {
	if len(via) >= maxRedirects {
		return errors.New("stopped after 10 redirects")
	}
	if err := RefuseCleartextRedirect(req, via); err != nil {
		return err
	}
	if err := RefuseCrossOriginBodyRedirect(req, via); err != nil {
		return err
	}
	cross := dropCrossOriginCredentials(req, via, configured)
	if o := redirectObservationFrom(req.Context()); o != nil {
		class := RedirectSameOrigin
		if cross {
			class = RedirectCrossOrigin
		}
		o.hop(class)
	}
	return nil
}

// dropCrossOriginCredentials removes Authorization, and every header named in
// configured (the fixed headers a deployment configures for that system, by
// their names, whatever they hold), from a hop to another origin than the
// chain's first (scheme, host and port), and from every later hop of that
// chain, a hop back to the first origin included: once the chain has left
// the origin, another origin chose where the hop goes (any path, and with a
// 307 or 308 the method and body), so nothing configured for the origin is
// sent for it. It reports whether this hop is cross-origin. Names match
// without regard to case. A chain that never leaves the origin keeps them
// all: they were configured for that origin. net/http's own rule keeps
// Authorization for the same host on another port or scheme, and for a
// subdomain, and keeps every other header wherever the hop goes.
func dropCrossOriginCredentials(req *http.Request, via []*http.Request, configured []string) bool {
	cross := !sameOrigin(req.URL, via[0].URL)
	if !cross && !leftOrigin(via) {
		return false
	}
	req.Header.Del("Authorization")
	for _, name := range configured {
		for k := range req.Header {
			if strings.EqualFold(k, name) {
				delete(req.Header, k)
			}
		}
	}
	return cross
}

// leftOrigin reports whether any request of via, a redirect chain so far,
// went to another origin than its first.
func leftOrigin(via []*http.Request) bool {
	for _, v := range via[1:] {
		if !sameOrigin(v.URL, via[0].URL) {
			return true
		}
	}
	return false
}

// chainLeftOrigin reports whether req, or any request of the redirect chain
// that led to it, went to another origin than the chain's first.
func chainLeftOrigin(req *http.Request) bool {
	first := chainOrigin(req)
	for r := req; r != nil; {
		if !sameOrigin(r.URL, first) {
			return true
		}
		if r.Response == nil {
			break
		}
		r = r.Response.Request
	}
	return false
}

// configuredNames are the names of configured, sorted, each once.
func configuredNames(configured http.Header) []string {
	names := make([]string, 0, len(configured))
	for k := range configured {
		names = append(names, http.CanonicalHeaderKey(k))
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// RefuseCleartextRedirect is ErrCleartextRedirect, recorded on the request's
// RedirectObservation, when req is a hop from https to http; nil otherwise.
// A client that follows no redirect calls it first, so a downgrade is named as
// one wherever it is refused.
func RefuseCleartextRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 || !strings.EqualFold(via[len(via)-1].URL.Scheme, "https") || !strings.EqualFold(req.URL.Scheme, "http") {
		return nil
	}
	if o := redirectObservationFrom(req.Context()); o != nil {
		o.hop(RedirectCleartextRefused)
	}
	return ErrCleartextRedirect
}

// ErrTokenRedirect is a token endpoint's redirect, refused before the hop is
// sent, whatever its origin: OAuth defines no redirect of a token request, and
// a 307 or 308 would replay its body, the client's secret or signed assertion,
// to wherever the endpoint names. An https-to-http one is ErrCleartextRedirect,
// as for any other request.
var ErrTokenRedirect = errors.New("smartauth: a token request follows no redirect")

// TokenRedirectError is ErrTokenRedirect for one answer: its status and the
// origin (scheme, host and port) its Location named, never the path or query.
type TokenRedirectError struct {
	StatusCode int
	Origin     string
}

func (e *TokenRedirectError) Error() string {
	to := e.Origin
	if to == "" {
		to = "no origin it could read"
	}
	return fmt.Sprintf("smartauth: the token endpoint answered %d, a redirect to %s; a token request follows no redirect", e.StatusCode, to)
}

// Is reports ErrTokenRedirect.
func (e *TokenRedirectError) Is(target error) bool { return target == ErrTokenRedirect }

// ErrCrossOriginBodyRedirect is a redirect to another origin than the chain's
// first, of a request that carried a body, refused before the hop is sent:
// such a request goes only to the participant's registered endpoint, and a
// 307 or 308 would replay its body (a patient's data) wherever the redirect
// names, while a 301, 302 or 303 would answer it with a request it never
// made. A redirect within the origin is followed. A GET or HEAD with no body
// follows a redirect to another origin as before, without its credentials.
var ErrCrossOriginBodyRedirect = errors.New("smartauth: a request that carries a body follows no redirect to another origin")

// CrossOriginRedirectError is ErrCrossOriginBodyRedirect for one answer: its
// status and the origin (scheme, host and port) its Location named, never
// the path or query, so the system's operator can point its registered
// endpoint at the final address.
type CrossOriginRedirectError struct {
	StatusCode int
	Origin     string
}

func (e *CrossOriginRedirectError) Error() string {
	return fmt.Sprintf("smartauth: the system answered %d, a redirect to another origin (%s); a request that carries a body follows no redirect to another origin", e.StatusCode, e.Origin)
}

// Is reports ErrCrossOriginBodyRedirect.
func (e *CrossOriginRedirectError) Is(target error) bool { return target == ErrCrossOriginBodyRedirect }

// RefuseCrossOriginBodyRedirect is a CrossOriginRedirectError, recorded on the
// request's RedirectObservation as RedirectCrossOriginRefused, when the
// chain's first request carried a body and req is a hop to another origin
// than that request's; nil otherwise. It decides by the request the chain
// began with, its method and body, never the hop's: a 303 that turned it
// into a GET does not let a later hop leave the origin.
func RefuseCrossOriginBodyRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 || !carriedBody(via[0]) || sameOrigin(req.URL, via[0].URL) {
		return nil
	}
	if o := redirectObservationFrom(req.Context()); o != nil {
		o.hop(RedirectCrossOriginRefused)
	}
	status := 0
	if req.Response != nil {
		status = req.Response.StatusCode
	}
	return &CrossOriginRedirectError{StatusCode: status, Origin: strings.ToLower(req.URL.Scheme) + "://" + req.URL.Host}
}

// carriedBody reports whether r, the first request of a redirect chain, is one
// that carries a body: any method but GET and HEAD (an empty POST included),
// or any request that carried one.
func carriedBody(r *http.Request) bool {
	if r.Method != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
		return true
	}
	return r.GetBody != nil || r.ContentLength > 0 || (r.Body != nil && r.Body != http.NoBody)
}

func redirectObservationFrom(ctx context.Context) *RedirectObservation {
	o, _ := ctx.Value(redirectObservationKey{}).(*RedirectObservation)
	return o
}

// chainOrigin is the URL of the request that began req's redirect chain: req
// itself when it is not a redirect hop.
func chainOrigin(req *http.Request) *url.URL {
	first := req
	for first.Response != nil && first.Response.Request != nil {
		first = first.Response.Request
	}
	return first.URL
}

// sameOrigin reports whether a and b share scheme, host and port, the port
// defaulted by scheme.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// ParticipantClient is a copy of c that never sends a redirect hop from https
// to http (RefuseCleartextRedirect), and sends a hop to another origin, and
// every later hop of that chain, without Authorization: the rules for every
// client that
// reaches a participant's own system, whether or not it carries a bearer. A
// client with no redirect policy of its own gets CheckRedirect, which also
// records each hop followed by its origin class; a client with its own policy
// keeps it after the downgrade is refused, so a stricter one (refusing every
// redirect) stays as strict. The caller's client is not changed; its
// transport is shared. A nil c stays nil.
func ParticipantClient(c *http.Client) *http.Client {
	return ParticipantClientWith(c, nil)
}

// ParticipantClientWith is ParticipantClient for a client whose requests carry
// fixed headers a deployment configures for the participant's system
// (PAYER_DAVINCI_BACKEND_HEADERS): a hop to another origin than the chain's
// first carries none of the headers configured names, as it carries no
// Authorization, decided by the configured set's own names and never by a list
// of names known to hold a credential, and neither does any later hop of that
// chain, back at the first origin too; a chain that stays within the origin
// keeps them.
// Wrapping a client ParticipantClient or ParticipantClientWith already wrapped
// keeps the outer and the inner rules both. nil or empty configured is
// ParticipantClient.
func ParticipantClientWith(c *http.Client, configured http.Header) *http.Client {
	if c == nil {
		return nil
	}
	names := configuredNames(configured)
	cp := *c
	own := c.CheckRedirect
	if own == nil {
		if len(names) == 0 {
			cp.CheckRedirect = CheckRedirect
		} else {
			cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				return checkRedirectDropping(req, via, names)
			}
		}
		return &cp
	}
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := RefuseCleartextRedirect(req, via); err != nil {
			return err
		}
		if err := RefuseCrossOriginBodyRedirect(req, via); err != nil {
			return err
		}
		dropCrossOriginCredentials(req, via, names)
		return own(req, via)
	}
	return &cp
}
