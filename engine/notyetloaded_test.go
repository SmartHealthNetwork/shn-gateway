package engine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
)

// The not-yet-loaded retry's rule and its parts, in the core build: the
// refusal decided by its fields, the retry's own client, its wait and its
// results. The exchange rows, which drive a PA leg, are
// workstream_pa_notyetloaded_test.go's.

var notYetLoadedBody = `{"error":"` + hubReasonNotYetLoaded + `"}`

// notAnHTTPTransport is a transport an embedding program might supply: not an
// *http.Transport, so no retry client can be made over it.
type notAnHTTPTransport struct{}

func (notAnHTTPTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, io.EOF }

// The retry's client is its base with keep-alives off and nothing else
// changed: the same TLS configuration and client certificate (a server that
// requires the certificate answers it), the same timeout and redirect policy,
// and an endpointcert wrapper kept around it.
func TestFreshConnClient_KeepsTLSAndClientCertificate(t *testing.T) {
	t.Parallel()
	clientCert, clientPool := selfSignedClientCert(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientPool}
	var conns atomic.Int32
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	base := srv.Client().Transport.(*http.Transport).Clone()
	base.TLSClientConfig.Certificates = []tls.Certificate{clientCert}
	t.Cleanup(base.CloseIdleConnections)
	redirects := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := &http.Client{Transport: base, Timeout: 7 * time.Second, CheckRedirect: redirects}

	get := func(c *http.Client) int {
		t.Helper()
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(c); code != http.StatusNoContent || conns.Load() != 1 {
		t.Fatalf("base: %d on %d connections", code, conns.Load())
	}
	fresh, ok := freshConnClient(c)
	if !ok {
		t.Fatal("no fresh client for an *http.Transport base")
	}
	if code := get(fresh); code != http.StatusNoContent {
		t.Fatalf("the fresh client got %d: it did not present the base's client certificate or trust its roots", code)
	}
	if conns.Load() != 2 {
		t.Fatalf("%d connections, want the fresh client's own second one", conns.Load())
	}
	ft := fresh.Transport.(*http.Transport)
	if !ft.DisableKeepAlives || ft.TLSClientConfig.RootCAs == nil || !ft.TLSClientConfig.RootCAs.Equal(base.TLSClientConfig.RootCAs) ||
		len(ft.TLSClientConfig.Certificates) != 1 || ft.TLSClientConfig.MinVersion != base.TLSClientConfig.MinVersion {
		t.Fatalf("the clone changed the base's TLS: %+v", ft.TLSClientConfig)
	}
	if fresh.Timeout != c.Timeout || fresh.CheckRedirect == nil {
		t.Fatalf("the fresh client lost the base's timeout or redirect policy")
	}
	again, _ := freshConnClient(c)
	if again.Transport != fresh.Transport {
		t.Fatal("a second retry cloned the base again")
	}

	// An endpointcert-wrapped client is rewrapped: same recorder and tag, the
	// clone beneath.
	rec := endpointcert.New()
	wrapped := rec.Client(endpointcert.Of(endpointcert.Hub), c)
	fw, ok := freshConnClient(wrapped)
	if !ok {
		t.Fatal("no fresh client for a wrapped *http.Transport base")
	}
	inner, tag, ok := endpointcert.Unwrap(fw.Transport)
	if !ok || tag != endpointcert.Of(endpointcert.Hub) || inner != fresh.Transport {
		t.Fatalf("the wrapped fresh client is %T over %T (tag %v): want the Hub's tag over the clone", fw.Transport, inner, tag)
	}
	if code := get(fw); code != http.StatusNoContent {
		t.Fatalf("the wrapped fresh client got %d", code)
	}

	// As the gateway builds its Hub client: the redirect policy beneath the
	// tags (serviceclient), both kept, the clone at the bottom.
	svc := serviceclient.ServiceClient(wrapped)
	fs, ok := freshConnClient(svc)
	if !ok {
		t.Fatal("no fresh client under the redirect policy")
	}
	if !serviceclient.Refuses(fs.Transport) {
		t.Fatal("the fresh client lost the redirect policy")
	}
	mid, tag, ok := endpointcert.Unwrap(fs.Transport)
	if !ok || tag != endpointcert.Of(endpointcert.Hub) {
		t.Fatalf("the fresh client lost its tag: %T", fs.Transport)
	}
	if bottom, ok := serviceclient.Base(mid); !ok || bottom != fresh.Transport {
		t.Fatalf("beneath the policy is %T, want the clone", bottom)
	}
	if code := get(fs); code != http.StatusNoContent {
		t.Fatalf("the fresh client under the policy got %d", code)
	}

	// A base that is not an *http.Transport gets no retry.
	if _, ok := freshConnClient(&http.Client{Transport: notAnHTTPTransport{}}); ok {
		t.Fatal("a fresh client was made over a transport that cannot promise a new connection")
	}
}

func selfSignedClientCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "retry-client"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// The rule itself: the named refusal, decided by its fields.
func TestNotYetLoadedAnswer(t *testing.T) {
	t.Parallel()
	// delivered builds the header as net/http reads one off the wire.
	delivered := func(vs ...string) http.Header {
		h := http.Header{}
		for _, v := range vs {
			h.Add(HubDeliveredHeader, v)
		}
		return h
	}
	no := delivered("no")
	withRetryAfter := delivered("no")
	withRetryAfter.Set("Retry-After", "3")
	for name, row := range map[string]struct {
		status int
		h      http.Header
		body   string
		want   bool
	}{
		"the refusal":           {503, no, notYetLoadedBody, true},
		"with Retry-After too":  {503, withRetryAfter, notYetLoadedBody, true},
		"another status":        {500, no, notYetLoadedBody, false},
		"no delivered header":   {503, delivered(), notYetLoadedBody, false},
		"delivered yes":         {503, delivered("yes"), notYetLoadedBody, false},
		"delivered No":          {503, delivered("No"), notYetLoadedBody, false},
		"two delivered values":  {503, delivered("no", "no"), notYetLoadedBody, false},
		"error null":            {503, no, `{"error":null}`, false},
		"error another reason":  {503, no, `{"error":"no healthy upstream"}`, false},
		"error a longer reason": {503, no, `{"error":"registry not yet loaded."}`, false},
		"error in another case": {503, no, `{"error":"Registry not yet loaded"}`, false},
		"not JSON":              {503, no, "registry not yet loaded", false},
		"the reason elsewhere":  {503, no, `{"message":"registry not yet loaded"}`, false},
		"error not a string":    {503, no, `{"error":["registry not yet loaded"]}`, false},
		"trailing data":         {503, no, notYetLoadedBody + `x`, false},
		"empty body":            {503, no, "", false},
	} {
		if got := notYetLoadedAnswer(row.status, row.h, []byte(row.body)); got != row.want {
			t.Errorf("%s: %v, want %v", name, got, row.want)
		}
	}
}

// retryWait itself: the wait elapses, or the context ends it first.
func TestRetryWait(t *testing.T) {
	t.Parallel()
	g := &Gateway{}
	if !g.retryWait(context.Background(), time.Millisecond) {
		t.Fatal("a wait with a live context did not elapse")
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if g.retryWait(ended, time.Hour) {
		t.Fatal("a wait whose context had ended reported it elapsed")
	}
	short, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	start := time.Now()
	if g.retryWait(short, time.Hour) || time.Since(start) > 10*time.Second {
		t.Fatalf("a wait outlived its context's deadline (%v)", time.Since(start))
	}
}

// Each retry's result, by what it met.
func TestRetryResults(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		err  error
		want string
	}{
		"answered":                         {nil, RetryAnswered},
		"refused not yet loaded again":     {&hubRefusalError{status: 503, fromHub: true, notYetLoaded: true}, RetryRefused},
		"another Hub refusal":              {&hubRefusalError{status: 503, fromHub: true, reason: "replay guard unavailable"}, RetryAnswered},
		"a Hub 502 delivered unknown":      {&hubRefusalError{status: 502, fromHub: true, delivered: "unknown"}, RetryAnswered},
		"an answer from in front of it":    {&hubRefusalError{status: 503, fromHub: false, delivered: "unknown"}, RetryFailed},
		"an answer it could not accept":    {&answerLostError{cause: "x"}, RetryAnswered},
		"a connection failed after write":  {&hubSentError{err: io.EOF}, RetryFailed},
		"a connection failed before write": {&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, RetryFailed},
		"a redirect":                       {&hubRedirectError{status: 302}, RetryFailed},
		"the leg's deadline":               {context.DeadlineExceeded, RetryFailed},
	} {
		if got := hubRetryResult(row.err); got != row.want {
			t.Errorf("hub, %s: %s, want %s", name, got, row.want)
		}
	}
	unreachable := fmt.Errorf("%w (%w)", errAuthzUnreachable, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
	for name, row := range map[string]struct {
		err  error
		want string
	}{
		"answered":                     {nil, RetryAnswered},
		"refused not yet loaded again": {&authzNotYetLoaded{err: fmt.Errorf("%w (503)", errAuthzUnreachable)}, RetryRefused},
		"no answer":                    {unreachable, RetryFailed},
		"a proxy's 503":                {fmt.Errorf("%w (the authorize call was answered 503)", errAuthzUnreachable), RetryFailed},
		"the Framework's 429":          {&authzLimited{}, RetryAnswered},
		"a denial":                     {errAuthorizationDenied, RetryAnswered},
		"a stated rule":                {&authzRefusal{rule: "audit"}, RetryAnswered},
		"no token":                     {errAuthzNoToken, RetryAnswered},
	} {
		if got := authzRetryResult(row.err); got != row.want {
			t.Errorf("authz, %s: %s, want %s", name, got, row.want)
		}
	}
}
