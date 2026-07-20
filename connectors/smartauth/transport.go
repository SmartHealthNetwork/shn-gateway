package smartauth

import (
	"fmt"
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
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &bearerTransport{ts: ts, base: http.DefaultTransport},
	}, nil
}

type bearerTransport struct {
	ts   *TokenSource
	base http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.ts.Token(req.Context())
	if err != nil {
		return nil, fmt.Errorf("smartauth: acquire token: %w", err)
	}
	// Clone before mutating headers (RoundTripper contract: must not modify the input).
	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r2)
}
