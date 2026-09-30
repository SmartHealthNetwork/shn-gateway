package checks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// bearerClient adds a bearer the way the gateway's authenticated clients do.
type bearerClient struct {
	tok  string
	err  error
	base http.RoundTripper
}

func (b bearerClient) RoundTrip(req *http.Request) (*http.Response, error) {
	if b.err != nil {
		return nil, b.err
	}
	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", "Bearer "+b.tok)
	return b.base.RoundTrip(r2)
}

// metadataServer answers /metadata per the request's bearer: authed for
// "Bearer good", anon for none; it records every request's route header.
func metadataServer(t *testing.T, authed, anon int) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Route"))
		mu.Unlock()
		status := anon
		if r.Header.Get("Authorization") == "Bearer good" {
			status = authed
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func runAuthed(t *testing.T, srv *httptest.Server, auth http.RoundTripper) Result {
	t.Helper()
	return runAuthedVia(t, srv, auth, srv.Client())
}

// runAuthedVia runs the probe with plain as the runner's own (anonymous) client.
func runAuthedVia(t *testing.T, srv *httptest.Server, auth http.RoundTripper, plain *http.Client) Result {
	t.Helper()
	tgt := Target{ID: "PAYER_DAVINCI_BASE_URL", Kind: KindFHIRMetadata, URL: srv.URL,
		Headers: http.Header{"X-Route": {"plan-7"}}}
	if auth != nil {
		tgt.AuthClient = &http.Client{Transport: auth}
	}
	rn := NewRunner([]Target{tgt}, plain, newFakeClock(time.Unix(0, 0)).Now)
	results, err := rn.Run(context.Background())
	if err != nil || len(results) != 1 {
		t.Fatalf("Run: %v %v", results, err)
	}
	return results[0]
}

// A partner base that answers /metadata only to the gateway's credential
// passes: the check reads the way the gateway calls it. The anonymous
// refusal is kept as a fact, never a failure.
func TestFHIRMetadataAuthenticatedOnlyPasses(t *testing.T) {
	srv, seen := metadataServer(t, http.StatusOK, http.StatusForbidden)
	got := runAuthed(t, srv, bearerClient{tok: "good", base: http.DefaultTransport})
	if !got.OK || got.Failure != nil {
		t.Fatalf("OK=%v failure=%+v detail=%q, want a pass", got.OK, got.Failure, got.Detail)
	}
	if want := "CapabilityStatement (FHIR 4.0.1); the base answers /metadata only when authenticated (anonymous: HTTP 403)"; got.Detail != want {
		t.Errorf("Detail = %q, want %q", got.Detail, want)
	}
	if got.Anonymous == nil || got.Anonymous.OK || got.Anonymous.Detail != "HTTP 403" ||
		got.Anonymous.Failure == nil || *got.Anonymous.Failure != (Failure{Code: FailHTTPStatus, Hint: "HTTP 403"}) {
		t.Errorf("Anonymous = %+v, want the 403 recorded", got.Anonymous)
	}
	if got.Capability == nil || got.Capability.FHIRVersion != "4.0.1" {
		t.Errorf("Capability = %+v, want the authenticated read's", got.Capability)
	}
	// Both reads carry the partner's routing headers; only the first a bearer.
	if want := []string{"Bearer good|plan-7", "|plan-7"}; strings.Join(*seen, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %q, want %q", *seen, want)
	}
}

// An anonymous read that fails for any reason but a 401 or 403 says nothing
// about authentication, and the detail does not claim it does.
func TestFHIRMetadataAnonymousFailureNotARefusal(t *testing.T) {
	srv, _ := metadataServer(t, http.StatusOK, http.StatusBadGateway)
	got := runAuthed(t, srv, bearerClient{tok: "good", base: http.DefaultTransport})
	if want := "CapabilityStatement (FHIR 4.0.1); anonymous read: HTTP 502"; !got.OK || got.Detail != want {
		t.Errorf("OK=%v Detail = %q, want %q", got.OK, got.Detail, want)
	}
	// The anonymous read never reaches the base (it ran out of time, or its
	// connection failed): recorded, not called a refusal.
	down := &http.Client{Transport: bearerClient{err: errors.New("dial tcp: i/o timeout")}}
	got = runAuthedVia(t, srv, bearerClient{tok: "good", base: http.DefaultTransport}, down)
	if !got.OK || strings.Contains(got.Detail, "only when authenticated") ||
		got.Detail != "CapabilityStatement (FHIR 4.0.1); anonymous read: "+got.Anonymous.Detail {
		t.Errorf("OK=%v Detail = %q", got.OK, got.Detail)
	}
	if got.Anonymous.Failure == nil || got.Anonymous.Failure.Code != FailUnreachable {
		t.Errorf("Anonymous = %+v, want unreachable", got.Anonymous)
	}
}

// The authenticated read decides: it failing fails the check, whatever the
// anonymous read answers.
func TestFHIRMetadataAuthenticatedReadDecides(t *testing.T) {
	for _, tc := range []struct {
		name       string
		authed     int
		anon       int
		wantDetail string
		wantAnonOK bool
	}{
		{"both refused", http.StatusBadGateway, http.StatusForbidden, "HTTP 502", false},
		{"only the anonymous read answers", http.StatusUnauthorized, http.StatusOK,
			"HTTP 401; an anonymous read answers a CapabilityStatement", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := metadataServer(t, tc.authed, tc.anon)
			got := runAuthed(t, srv, bearerClient{tok: "good", base: http.DefaultTransport})
			if got.OK || got.Failure == nil || got.Failure.Code != FailHTTPStatus {
				t.Fatalf("OK=%v failure=%+v, want http-status", got.OK, got.Failure)
			}
			if got.Detail != tc.wantDetail {
				t.Errorf("Detail = %q, want %q", got.Detail, tc.wantDetail)
			}
			if got.Anonymous == nil || got.Anonymous.OK != tc.wantAnonOK {
				t.Errorf("Anonymous = %+v, want ok=%v", got.Anonymous, tc.wantAnonOK)
			}
		})
	}
}

// A credential the client cannot obtain is a credential failure, reported by
// status or as a fixed string, never as the base being unreachable.
func TestFHIRMetadataAuthenticatedCredentialFailure(t *testing.T) {
	for _, tc := range []struct {
		name, detail, hint string
		err                error
	}{
		{"token endpoint status", "credential check failed (HTTP 401)", "HTTP 401", &StatusError{Code: 401}},
		{"no credential", "credential check failed", "", ErrCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, seen := metadataServer(t, http.StatusOK, http.StatusForbidden)
			got := runAuthed(t, srv, bearerClient{err: tc.err})
			if got.OK || got.Failure == nil || got.Failure.Code != FailCredentialRejected || got.Failure.Hint != tc.hint {
				t.Fatalf("OK=%v failure=%+v, want credential-rejected %q", got.OK, got.Failure, tc.hint)
			}
			if got.Detail != tc.detail {
				t.Errorf("Detail = %q, want %q", got.Detail, tc.detail)
			}
			if got.Anonymous == nil || got.Anonymous.Detail != "HTTP 403" {
				t.Errorf("Anonymous = %+v, want the 403 recorded", got.Anonymous)
			}
			// Only the anonymous read reached the base.
			if len(*seen) != 1 || (*seen)[0] != "|plan-7" {
				t.Errorf("requests = %q", *seen)
			}
		})
	}
	// Any other client error is the base being unreachable, as before.
	srv, _ := metadataServer(t, http.StatusOK, http.StatusOK)
	got := runAuthed(t, srv, bearerClient{err: errors.New("dial tcp: refused")})
	if got.Failure == nil || got.Failure.Code != FailUnreachable {
		t.Errorf("failure = %+v, want unreachable", got.Failure)
	}
}

// A partner without credentials is probed anonymously, as before: one read,
// and no anonymous fact on the wire.
func TestFHIRMetadataWithoutCredentialsUnchanged(t *testing.T) {
	srv, seen := metadataServer(t, http.StatusOK, http.StatusForbidden)
	got := runAuthed(t, srv, nil)
	if got.OK || got.Detail != "HTTP 403" || got.Anonymous != nil {
		t.Fatalf("OK=%v detail=%q anonymous=%+v, want the anonymous 403 as the result", got.OK, got.Detail, got.Anonymous)
	}
	if len(*seen) != 1 {
		t.Errorf("requests = %q, want one", *seen)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "anonymous") {
		t.Errorf("wire shape carries anonymous: %s", b)
	}
}
