package smartauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

type acquisitionRoundTrip func(*http.Request) (*http.Response, error)

func (f acquisitionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTokenAcquisitionErrorMarker(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("CANARY-error")} {
		t.Run(cause.Error(), func(t *testing.T) {
			tokenClient := &http.Client{Transport: acquisitionRoundTrip(func(*http.Request) (*http.Response, error) { return nil, cause })}
			tr := &bearerTransport{ts: &TokenSource{Config: Config{TokenURL: "https://token.test", ClientID: "client", ClientSecret: "secret", HTTPClient: tokenClient}}, base: acquisitionRoundTrip(func(*http.Request) (*http.Response, error) {
				t.Fatal("resource request after token failure")
				return nil, nil
			})}
			req, _ := http.NewRequest(http.MethodGet, "https://resource.test", nil)
			_, err := tr.RoundTrip(req)
			want := fmt.Errorf("smartauth: acquire token: %w", fmt.Errorf("smartauth: token endpoint: %w", &url.Error{Op: "Post", URL: "https://token.test", Err: cause}))
			if err.Error() != want.Error() {
				t.Fatalf("text=%q want=%q", err, want)
			}
			if !errors.Is(err, cause) || !IsTokenAcquisitionError(err) || !IsTokenAcquisitionError(&url.Error{Op: "Get", URL: req.URL.String(), Err: err}) {
				t.Fatalf("lost marker/cause: %v", err)
			}
			if errors.Unwrap(err).Error() != errors.Unwrap(want).Error() {
				t.Fatalf("changed prior immediate chain: %v", errors.Unwrap(err))
			}
		})
	}
	for _, err := range []error{nil, errors.New("smartauth: acquire token: token endpoint status 401"), context.Canceled} {
		if IsTokenAcquisitionError(err) {
			t.Fatalf("false marker: %v", err)
		}
	}
}

func TestTokenAcquisitionResourceFailureIsUnmarked(t *testing.T) {
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"token","expires_in":3600}`)
	}))
	defer tok.Close()
	cause := errors.New("smartauth: acquire token: resource error")
	tr := &bearerTransport{ts: &TokenSource{Config: Config{TokenURL: tok.URL, ClientID: "client", ClientSecret: "secret"}}, base: acquisitionRoundTrip(func(*http.Request) (*http.Response, error) { return nil, cause })}
	req, _ := http.NewRequest(http.MethodGet, "https://resource.test", nil)
	_, err := tr.RoundTrip(req)
	if err != cause || IsTokenAcquisitionError(err) {
		t.Fatalf("changed resource error: %v", err)
	}
}

func TestTokenEndpointStatusEvidence(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, "private-response-sentinel")
		}))
		ts := &TokenSource{Config: Config{TokenURL: srv.URL, ClientID: "private-client-sentinel", ClientSecret: "private-secret-sentinel"}}
		token, err := ts.Token(context.Background())
		srv.Close()
		var endpoint *TokenEndpointError
		if token != "" || !errors.As(err, &endpoint) || endpoint.StatusCode != status {
			t.Fatalf("missing typed refusal: token present=%v error=%v", token != "", err)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(status)) || strings.Contains(err.Error(), "sentinel") {
			t.Fatalf("status evidence missing or unsafe: %v", err)
		}
	}
}
