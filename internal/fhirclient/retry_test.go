package fhirclient_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// retryServer answers each GET by first, for the first request, and 200
// "ok" after; it counts the requests that reached it.
func retryServer(t *testing.T, first func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			first(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// hangUp drops the connection without an answer: closed, or reset when
// linger is 0.
func hangUp(linger bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		if linger {
			_ = conn.(*net.TCPConn).SetLinger(0)
		}
		_ = conn.Close()
	}
}

// cutOff starts an answer and closes the connection inside its headers.
func cutOff(w http.ResponseWriter, _ *http.Request) {
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(err)
	}
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n")
	_ = buf.Flush()
	_ = conn.Close()
}

func status(code int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func get(t *testing.T, ctx context.Context, hc *http.Client, url string) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fhirclient.Do(hc, req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

// A GET with no answer, or a proxy's 502, 503 or 504, is tried once more: a
// system of record rolling to a new task answers the next try.
func TestDoTriesAGETOnceMoreAfterNoAnswerOrAProxyStatus(t *testing.T) {
	for _, c := range []struct {
		name  string
		first func(http.ResponseWriter, *http.Request)
	}{
		{"closed", hangUp(false)},
		{"reset", hangUp(true)},
		{"cut off", cutOff},
		{"502", status(http.StatusBadGateway)},
		{"503", status(http.StatusServiceUnavailable)},
		{"504", status(http.StatusGatewayTimeout)},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, n := retryServer(t, c.first)
			began := time.Now()
			code, body, err := get(t, context.Background(), srv.Client(), srv.URL)
			if err != nil || code != 200 || body != "ok" || n.Load() != 2 {
				t.Fatalf("%d %q %v after %d requests", code, body, err, n.Load())
			}
			if waited := time.Since(began); waited < fhirclient.RetryPause {
				t.Fatalf("tried again after %s", waited)
			}
		})
	}
	for name, errno := range map[string]syscall.Errno{"refused": syscall.ECONNREFUSED, "refused on Windows": 10061, "reset on Windows": 10054} {
		t.Run(name, func(t *testing.T) { triedAfterDialError(t, errno) })
	}
}

// triedAfterDialError fails the first dial with errno and lets the second
// through.
func triedAfterDialError(t *testing.T, errno syscall.Errno) {
	t.Helper()
	srv, n := retryServer(t, status(http.StatusTeapot))
	n.Store(1) // every request the server sees answers 200
	var dials atomic.Int32
	transport := srv.Client().Transport.(*http.Transport).Clone()
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", errno)}
		}
		return dial(ctx, network, addr)
	}
	code, body, err := get(t, context.Background(), &http.Client{Transport: transport}, srv.URL)
	if err != nil || code != 200 || body != "ok" || dials.Load() != 2 || n.Load() != 2 {
		t.Fatalf("%d %q %v after %d dials, %d requests", code, body, err, dials.Load(), n.Load()-1)
	}
}

// Every other answer is the answer, after one request: a 4xx is never tried
// again, so a denial never becomes a success.
func TestDoAnswersAnyOtherStatusAtOnce(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 429, 500, 501} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv, n := retryServer(t, status(code))
			got, _, err := get(t, context.Background(), srv.Client(), srv.URL)
			if err != nil || got != code || n.Load() != 1 {
				t.Fatalf("%d %v after %d requests, want %d after 1", got, err, n.Load(), code)
			}
		})
	}
}

// An outage that outlasts the pause is still an outage: the second answer
// is the answer, after exactly two requests.
func TestDoAnswersTheSecondTryOfAnOutage(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	code, _, err := get(t, context.Background(), srv.Client(), srv.URL)
	if err != nil || code != http.StatusServiceUnavailable || n.Load() != 2 {
		t.Fatalf("%d %v after %d requests", code, err, n.Load())
	}
	// A server that is not there at all: refused twice.
	var dials atomic.Int32
	refused := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}}
	if _, _, err := get(t, context.Background(), &http.Client{Transport: refused}, srv.URL); !errors.Is(err, syscall.ECONNREFUSED) || dials.Load() != 2 {
		t.Fatalf("a server that is not there: %v after %d dials", err, dials.Load())
	}
}

// The request's deadline bounds the try: with no more than the pause left,
// there is no second try; a caller who leaves during the pause gets no
// second try either.
func TestDoTriesAgainOnlyWithinTheDeadline(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		srv, n := retryServer(t, status(http.StatusServiceUnavailable))
		ctx, cancel := context.WithTimeout(context.Background(), fhirclient.RetryPause)
		defer cancel()
		code, _, err := get(t, ctx, srv.Client(), srv.URL)
		if err != nil || code != http.StatusServiceUnavailable || n.Load() != 1 {
			t.Fatalf("%d %v after %d requests", code, err, n.Load())
		}
	})
	t.Run("caller left", func(t *testing.T) {
		srv, n := retryServer(t, status(http.StatusServiceUnavailable))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The caller leaves once the first answer is in.
		hc := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			resp, err := srv.Client().Transport.RoundTrip(r)
			cancel()
			return resp, err
		})}
		began := time.Now()
		_, _, err := get(t, ctx, hc, srv.URL)
		if !errors.Is(err, context.Canceled) || n.Load() != 1 || time.Since(began) >= fhirclient.RetryPause {
			t.Fatalf("%v after %d requests, %s", err, n.Load(), time.Since(began))
		}
	})
	t.Run("slow failure", func(t *testing.T) {
		// A 504 after the proxy waited past RetryWithin is the answer: a
		// second wait would hold the caller's answer past its requester's.
		srv, n := retryServer(t, func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(fhirclient.RetryWithin + 100*time.Millisecond)
			w.WriteHeader(http.StatusGatewayTimeout)
		})
		code, _, err := get(t, context.Background(), srv.Client(), srv.URL)
		if err != nil || code != http.StatusGatewayTimeout || n.Load() != 1 {
			t.Fatalf("%d %v after %d requests", code, err, n.Load())
		}
	})
	t.Run("time out", func(t *testing.T) {
		// No answer within the client's own timeout is not tried again.
		var n atomic.Int32
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n.Add(1)
			<-release
		}))
		defer srv.Close()
		defer close(release)
		hc := srv.Client()
		hc.Timeout = 50 * time.Millisecond
		if _, _, err := get(t, context.Background(), hc, srv.URL); err == nil || n.Load() != 1 {
			t.Fatalf("%v after %d requests", err, n.Load())
		}
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
