package diagnostics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fullBodyErrorReader struct {
	body     []byte
	readErr  error
	closeErr error
}

func (r *fullBodyErrorReader) Read(p []byte) (int, error) {
	if len(r.body) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.body)
	r.body = r.body[n:]
	return n, r.readErr
}

func (r *fullBodyErrorReader) Close() error { return r.closeErr }

func TestObserveHTTPExactLengthRemainsCompleteAfterBodyError(t *testing.T) {
	for _, tc := range []struct {
		name         string
		declared     int64
		readErr      error
		closeErr     error
		wantComplete bool
	}{
		{"read error with final bytes", 4, errors.New("synthetic read error"), nil, true},
		{"close error after final bytes", 4, nil, errors.New("synthetic close error"), true},
		{"read error before final bytes", 5, errors.New("synthetic read error"), nil, false},
		{"close error before final bytes", 5, nil, errors.New("synthetic close error"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []Event
			req := httptest.NewRequest("POST", "/", nil)
			req.Body = &fullBodyErrorReader{body: []byte("full"), readErr: tc.readErr, closeErr: tc.closeErr}
			req.ContentLength = tc.declared
			h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := make([]byte, 4)
				n, err := r.Body.Read(body)
				if n != 4 || err != tc.readErr {
					t.Fatalf("read = %d, %v", n, err)
				}
				if err := r.Body.Close(); err != tc.closeErr {
					t.Fatalf("close = %v", err)
				}
			}), func(e Event) bool { events = append(events, e); return true }, nil, time.Now, 1024, NewCaptureBudget(8192, 1))
			h.ServeHTTP(httptest.NewRecorder(), req)
			if len(events) != 2 || string(events[0].Body) != "full" || events[0].BodyComplete != tc.wantComplete || events[0].RequestFingerprint.Complete != tc.wantComplete {
				t.Fatalf("known-length completeness = %+v", events)
			}
		})
	}
}

// A response-copy abort must retain the bytes already observed without turning
// the aborted exchange into a successful response or consuming more request data.
func TestObserveHTTPPreservesPanicAndPartialEvidence(t *testing.T) {
	applicationPanic := &struct{ message string }{"synthetic application failure"}
	for _, tc := range []struct {
		name       string
		panicValue any
		readBytes  int
		writeBody  bool
	}{
		{"abort after partial request", http.ErrAbortHandler, 3, true},
		{"application panic after complete request", applicationPanic, 7, true},
		{"application panic before response", applicationPanic, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := NewCaptureBudget(8192, 1)
			var events []Event
			h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.ReadFull(r.Body, make([]byte, tc.readBytes)); err != nil {
					t.Fatal(err)
				}
				if tc.writeBody {
					w.Header().Set("X-Observed", "yes")
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte("out"))
				}
				panic(tc.panicValue)
			}), func(e Event) bool { events = append(events, e); return true }, nil, time.Now, 1024, budget)
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/a%2Fb?b=2&a=1", strings.NewReader("request")))
			}()
			if recovered != tc.panicValue {
				t.Fatalf("panic = %v, want identical %v", recovered, tc.panicValue)
			}
			if len(events) != 2 {
				t.Fatalf("observations = %d, want request and response", len(events))
			}
			req, resp := events[0], events[1]
			wantBody := "request"[:tc.readBytes]
			complete := tc.readBytes == 7
			if req.Kind != "http-request" || string(req.Body) != wantBody || req.BodyComplete != complete || !req.HeadersComplete {
				t.Fatalf("request evidence = %+v", req)
			}
			if fp := req.RequestFingerprint; fp.BodySHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(wantBody))) || fp.ObservedBytes != int64(tc.readBytes) || fp.Complete != complete || fp.RequestURI != "/a%2Fb?b=2&a=1" || fp.Method != "POST" {
				t.Fatalf("fingerprint = %+v", fp)
			}
			if resp.Kind != "http-request-response" || resp.BodyComplete || resp.RequestFingerprint != req.RequestFingerprint {
				t.Fatalf("response evidence = %+v", resp)
			}
			if tc.writeBody {
				if resp.Status != http.StatusAccepted || string(resp.Body) != "out" || !resp.HeadersComplete || resp.Headers.Get("X-Observed") != "yes" {
					t.Fatalf("committed response = %+v", resp)
				}
			} else if resp.Status != 0 || len(resp.Body) != 0 || resp.HeadersComplete {
				t.Fatalf("uncommitted response = %+v", resp)
			}
			if budget.usedBytes != 0 || len(budget.slots) != 0 {
				t.Fatal("panic retained capture capacity")
			}
		})
	}
}

// truncatedUpstreamBody yields the partial response in transport-sized reads,
// then blocks until the test releases it and ends with io.ErrUnexpectedEOF, the
// error a transport reports when an upstream closes before its declared
// Content-Length. No upstream server, connection or shared transport is
// involved. The body records how it ended, so an abort that arrives by any
// other route (a cancelled request, a torn-down connection) is reported
// instead of passing as the intended one.
type truncatedUpstreamBody struct {
	partial []byte
	release <-chan struct{}
	ctx     context.Context
	mu      sync.Mutex
	ended   error
}

func (b *truncatedUpstreamBody) Read(p []byte) (int, error) {
	if len(b.partial) > 0 {
		n := copy(p[:min(len(p), 4096)], b.partial)
		b.partial = b.partial[n:]
		return n, nil
	}
	var err error
	select {
	case <-b.release:
		err = io.ErrUnexpectedEOF
	case <-b.ctx.Done():
		err = fmt.Errorf("request context ended before the release: %w", context.Cause(b.ctx))
	}
	b.mu.Lock()
	b.ended = err
	b.mu.Unlock()
	return 0, err
}

func (b *truncatedUpstreamBody) endedWith() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ended
}

// testLogWriter sends a logger's output to the test log, shown only on
// failure or with -v.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) { w.t.Logf("%s", p); return len(p), nil }

func (b *truncatedUpstreamBody) Close() error { return nil }

func TestObserveHTTPTruncatedUpstreamPreservesWireAbort(t *testing.T) {
	const requestBody = "synthetic request"
	partial := bytes.Repeat([]byte("partial response\n"), 512)
	type wireResult struct {
		status    int
		headers   http.Header
		body      []byte
		readError error
		panicked  any // what the proxy's handler panicked with; the wire alone cannot tell
	}
	var baseline wireResult
	for _, capture := range []bool{false, true} {
		// The capture=true row is judged against the capture=false baseline,
		// so a baseline failure stops the test rather than comparing against
		// an empty result.
		if !t.Run(fmt.Sprintf("capture=%v", capture), func(t *testing.T) {
			finishUpstream := make(chan struct{})
			finish := sync.OnceFunc(func() { close(finishUpstream) })
			target, err := url.Parse("http://upstream.invalid")
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(target)
			var upstream *truncatedUpstreamBody
			proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != requestBody {
					t.Errorf("upstream request = %q, %v", body, err)
				}
				_ = r.Body.Close()
				upstream = &truncatedUpstreamBody{partial: partial, release: finishUpstream, ctx: r.Context()}
				header := http.Header{}
				header.Set("Content-Length", fmt.Sprint(len(partial)+1))
				header.Set("Content-Type", "application/octet-stream")
				header.Set("Date", "Thu, 01 Jan 1970 00:00:00 GMT")
				header.Set("X-Upstream", "short")
				return &http.Response{
					Status:        "202 Accepted",
					StatusCode:    http.StatusAccepted,
					Proto:         "HTTP/1.1",
					ProtoMajor:    1,
					ProtoMinor:    1,
					Header:        header,
					ContentLength: int64(len(partial) + 1),
					Body:          upstream,
					Request:       r,
				}, nil
			})
			proxy.FlushInterval = -1
			// The proxy logs the upstream error that cut the copy short; keep it
			// in the test log so a failure names its cause.
			proxy.ErrorLog = log.New(testLogWriter{t}, "", 0)
			var events []Event
			var handler http.Handler = proxy
			if capture {
				handler = ObserveHTTP(handler, func(e Event) bool { events = append(events, e); return true }, nil, time.Now, 16384, NewCaptureBudget(65536, 1))
			}
			done := make(chan struct{})
			var panicked any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// A handler that panics and one that returns short look the same
				// on the wire; record the panic, then let the server see it.
				defer func() {
					panicked = recover()
					close(done)
					if panicked != nil {
						panic(panicked)
					}
				}()
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			defer finish()
			client := server.Client()
			client.Timeout = 5 * time.Second
			resp, err := client.Post(server.URL+"/a%2Fb?b=2&a=1", "application/octet-stream", strings.NewReader(requestBody))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body := make([]byte, len(partial))
			if _, err := io.ReadFull(resp.Body, body); err != nil {
				t.Fatal(err)
			}
			finish()
			rest, readError := io.ReadAll(resp.Body)
			<-done
			got := wireResult{resp.StatusCode, resp.Header, append(body, rest...), readError, panicked}
			if ended := upstream.endedWith(); ended != io.ErrUnexpectedEOF {
				t.Fatalf("upstream body ended with %v, want the released io.ErrUnexpectedEOF", ended)
			}
			if got.status != http.StatusAccepted || !bytes.Equal(got.body, partial) || got.readError != io.ErrUnexpectedEOF || got.panicked != http.ErrAbortHandler {
				t.Fatalf("wire response = %+v", got)
			}
			if !capture {
				baseline = got
				return
			}
			if !reflect.DeepEqual(got, baseline) {
				t.Fatal("capture changed response status, headers, bytes, or abort error")
			}
			if len(events) != 2 {
				t.Fatalf("observations = %d, want request and response after proxy abort", len(events))
			}
			req, response := events[0], events[1]
			if req.Kind != "http-request" || string(req.Body) != requestBody || !req.BodyComplete || !req.HeadersComplete || !req.RequestFingerprint.Complete || req.RequestFingerprint.ObservedBytes != int64(len(requestBody)) || req.RequestFingerprint.BodySHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(requestBody))) {
				t.Fatalf("request evidence = %+v", req)
			}
			if response.Kind != "http-request-response" || !bytes.Equal(response.Body, partial) || response.BodyComplete || !response.HeadersComplete || !reflect.DeepEqual(response.Headers, got.headers) || response.Status != got.status || response.RequestFingerprint != req.RequestFingerprint {
				t.Fatalf("response status=%d bytes=%d complete=%v headers=%v", response.Status, len(response.Body), response.BodyComplete, response.Headers)
			}
		}) {
			t.FailNow()
		}
	}
}
