package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/contentcoding"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// largeAnswer writes an answer well past contentcoding's threshold.
func largeAnswer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/fhir+json")
	_, _ = io.WriteString(w, strings.Repeat(`{"resourceType":"Basic"},`, 400))
}

// TestCodedRoute_CompressesOnlyARouteStatingNoCredential: a route that
// states it carries no credential is gzip-coded for a caller that accepts
// gzip; one that carries a credential, or states nothing, is written as its
// handler wrote it and does not vary by Accept-Encoding.
func TestCodedRoute_CompressesOnlyARouteStatingNoCredential(t *testing.T) {
	for credential, want := range map[routeCredential]bool{noCredential: true, carriesCredential: false, credentialUnstated: false} {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		codedRoute(shnsdk.MaxRequestBytes, credential, largeAnswer).ServeHTTP(w, r)
		if got := w.Header().Get("Content-Encoding") == "gzip"; got != want {
			t.Errorf("credential %d: compressed %v, want %v", credential, got, want)
		}
		if got := w.Header().Get("Vary") != ""; got != want {
			t.Errorf("credential %d: Vary %q", credential, w.Header().Get("Vary"))
		}
	}
}

// TestCodedRoute_DecodesTheRequestBody: every route, a credential route's
// included, reads a gzip-coded request body decoded, and its handler sees
// the body as the caller wrote it, with the route's path values.
func TestCodedRoute_DecodesTheRequestBody(t *testing.T) {
	const sent = `{"hook":"order-select"}`
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	_, _ = io.WriteString(zw, sent)
	_ = zw.Close()
	for _, credential := range []routeCredential{noCredential, carriesCredential} {
		var got, id string
		mux := http.NewServeMux()
		mux.Handle("POST /cds-services/{id}", codedRoute(shnsdk.MaxRequestBytes, credential, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got, id = string(b), r.PathValue("id")
		}))
		r := httptest.NewRequest(http.MethodPost, "/cds-services/probe", bytes.NewReader(z.Bytes()))
		r.Header.Set("Content-Encoding", "gzip")
		mux.ServeHTTP(httptest.NewRecorder(), r)
		if got != sent || id != "probe" {
			t.Errorf("credential %d: handler read %q at %q", credential, got, id)
		}
	}
}

func gzipT(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// wantUnsupportedCoding: the 415 a body in a coding the gateway does not
// decode is answered, naming gzip.
func wantUnsupportedCoding(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnsupportedMediaType || rec.Header().Get("Accept-Encoding") != "gzip" ||
		!strings.Contains(rec.Body.String(), errUnsupportedCoding) {
		t.Fatalf("answer %d (Accept-Encoding %q) %s, want 415 naming gzip", rec.Code, rec.Header().Get("Accept-Encoding"), rec.Body.String())
	}
}

// The Hub's 415 for a body in a coding it does not decode is recorded as the
// Hub's refusal on message integrity.
func TestHubRefusalOutcome_UnsupportedCodingIsIntegrity(t *testing.T) {
	refused := &hubRefusalError{status: http.StatusUnsupportedMediaType, reason: errUnsupportedCoding, fromHub: true}
	if outcome, by, rule := hubRefusalOutcome(refused); outcome != ExchangeRefused || by != RefusedByHub || rule != RefusalIntegrity {
		t.Fatalf("recorded %s %s %s, want the Hub's integrity refusal", outcome, by, rule)
	}
}

// acceptsGzip is a caller that accepts gzip on every request.
type acceptsGzip struct{ h http.Handler }

func (a *acceptsGzip) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Header.Set("Accept-Encoding", "gzip")
	a.h.ServeHTTP(w, r)
}

// endlessEmptyGzip is a gzip body without end whose members each decode to
// nothing.
type endlessEmptyGzip struct{ off int }

var emptyGzipMember = func() []byte {
	var b bytes.Buffer
	_ = gzip.NewWriter(&b).Close()
	return b.Bytes()
}()

func (e *endlessEmptyGzip) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = emptyGzipMember[e.off]
		e.off = (e.off + 1) % len(emptyGzipMember)
	}
	return len(p), nil
}

// TestCodedRoute_ABodyThatDecodesToNothingIs413: a gzip body that never
// decodes to anything is the 413 a body over the network's limit is, once
// more of it is read than any body under the limit takes, never read
// without end.
func TestCodedRoute_ABodyThatDecodesToNothingIs413(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/x", &endlessEmptyGzip{})
	r.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		codedRoute(shnsdk.MaxRequestBytes, noCredential, func(w http.ResponseWriter, r *http.Request) {
			readIngressBody(w, r, shnsdk.MaxRequestBytes)
		}).ServeHTTP(rec, r)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("still reading the coded body")
	}
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), errRequestTooLarge) {
		t.Fatalf("answer %d %s, want 413", rec.Code, rec.Body.String())
	}
}

// TestPostEnvelope_AGzipAnswerIsTheUncodedAnswer: the Hub gzip-codes its
// answer for a gateway whose transport asks for it, as Go's does, and the
// gateway reads the envelope decoded: the ciphertext its records hash is the
// one the Hub answered, as with an answer the Hub did not code.
func TestPostEnvelope_AGzipAnswerIsTheUncodedAnswer(t *testing.T) {
	want := shnsdk.Envelope{
		Metadata:   shnsdk.Metadata{Sender: "payer", Recipient: "provider", TransactionType: "coverage-eligibility", CorrelationID: "corr-coded"},
		Ciphertext: bytes.Repeat([]byte("answer "), 600), // past contentcoding's threshold
	}
	raw, err := shnsdk.EncodeEnvelope(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, coding := range []bool{false, true} {
		var coded atomic.Int32
		var hub http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
		})
		if coding {
			compress := contentcoding.Compress(hub)
			hub = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				compress.ServeHTTP(w, r)
				if w.Header().Get("Content-Encoding") == "gzip" {
					coded.Add(1)
				}
			})
		}
		srv := httptest.NewServer(hub)
		got, err := (&Gateway{}).postEnvelope(context.Background(), &http.Client{}, srv.URL+"/route", []byte("{}"), "assertion")
		srv.Close()
		if err != nil {
			t.Fatalf("coded %v: %v", coding, err)
		}
		if coding && coded.Load() != 1 {
			t.Fatalf("the Hub coded %d answers, want 1: the gateway did not ask for gzip", coded.Load())
		}
		if !bytes.Equal(got.Ciphertext, want.Ciphertext) || got.Metadata != want.Metadata {
			t.Fatalf("coded %v: read %+v, want the envelope the Hub answered", coding, got.Metadata)
		}
	}
}
