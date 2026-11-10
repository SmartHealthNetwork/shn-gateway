package smartauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// NewHTTPClient returns an *http.Client whose Transport adds a SMART Backend
// Services bearer (from a cached TokenSource) to every request. Fail-closed: a
// token-fetch failure surfaces as the request's error. Validates cfg up front so
// a misconfigured gateway fails at construction, not on first read.
func NewHTTPClient(cfg Config) (*http.Client, error) {
	if cfg.TokenURL == "" {
		return nil, fmt.Errorf("smartauth: TokenURL required")
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("smartauth: ClientID required")
	}
	if err := cfg.validateMode(); err != nil {
		return nil, err
	}
	ts := &TokenSource{Config: cfg}
	base := cfg.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &bearerTransport{ts: ts, base: base},
		CheckRedirect: CheckRedirect,
	}, nil
}

type bearerTransport struct {
	ts   *TokenSource
	base http.RoundTripper
}

// RoundTrip sends req with the source's bearer. A 401 to that bearer is the
// resource server refusing the credential before acting on the request, as
// RFC 6750 has it: the transport drops the bearer, fetches a fresh one and
// sends the same request once more, as a direct SMART client does. The resend
// is req exactly, its body read again (req.GetBody) and only Authorization
// changed; its answer, a second 401 included, is the answer. A request whose
// body cannot be read again is not resent: its 401 is the answer, and the next
// request goes with a fresh bearer. A fresh bearer that cannot be obtained, or
// a request that ends first, leaves the 401 as the answer, as it arrived. Each
// resend is recorded on the context's CredentialRefreshObservation, with the
// challenge the 401 carried.
func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	// The bearer goes only to the origin the chain began at, the participant
	// system it was issued for, and only while the chain has not left it: a
	// redirect hop to another origin is sent without it, and so is every later
	// hop of that chain, back at the first origin too, since another origin
	// chose it. That origin's 401, or the first origin's to the hop back, is
	// its answer, never this token's.
	if chainLeftOrigin(req) {
		r2 := req.Clone(ctx)
		r2.Header.Del("Authorization")
		return t.base.RoundTrip(r2)
	}
	tok, gen, err := t.token(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := t.send(req, tok, nil)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	// A request that has ended cannot drop the bearer here; the fresh one
	// below then cannot be obtained either, and its 401 is the answer.
	_ = t.ts.invalidate(ctx, gen)
	body, replayable := replayBody(req)
	if !replayable {
		return resp, nil
	}
	// The participant's 401 stays the answer unless the request is sent again:
	// a fresh bearer that cannot be obtained is not this request's failure, and
	// is not marked as one. The next request fetches again.
	fresh, _, err := t.ts.token(ctx)
	if err != nil {
		if body != nil {
			body.Close()
		}
		return resp, nil
	}
	challenge := credentialChallenge(resp.Header)
	// The 401's own body is the participant's, never read: it is replaced by
	// the resend's answer.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if o := credentialRefreshObservationFrom(ctx); o != nil {
		o.resent(challenge)
	}
	return t.send(req, fresh, body)
}

// token is the source's bearer and its generation, a failure to obtain one
// marked on the context's TokenAcquisitionObservation.
func (t *bearerTransport) token(ctx context.Context) (string, uint64, error) {
	tok, gen, err := t.ts.token(ctx)
	if err != nil {
		// Record the actual boundary before net/http can replace the returned error
		// with its non-unwrapping outer-client timeout error.
		if observation, ok := ctx.Value(tokenAcquisitionObservationKey{}).(*TokenAcquisitionObservation); ok {
			observation.failed.Store(true)
		}
		return "", 0, &tokenAcquisitionError{cause: err}
	}
	return tok, gen, nil
}

// send sends a clone of req carrying tok, with body in place of req's own
// when it is set (a resend).
func (t *bearerTransport) send(req *http.Request, tok string, body io.ReadCloser) (*http.Response, error) {
	// Clone before mutating headers (RoundTripper contract: must not modify the input).
	r2 := req.Clone(req.Context())
	if body != nil {
		r2.Body = body
	}
	r2.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r2)
}

// replayBody is req's body read again for a resend: nil for a request with no
// body. replayable is false when the body cannot be read again.
func replayBody(req *http.Request) (io.ReadCloser, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	b, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	return b, true
}
