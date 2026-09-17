package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testAdmission(t *testing.T, backend string, limit int, dial func(context.Context, string, string) (net.Conn, error)) *admission {
	t.Helper()
	a, err := newAdmission(context.Background(), "127.0.0.1:0", backend, limit, dial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.revoke(); a.wait() })
	return a
}
func admissionConn(t *testing.T, a *admission) *net.TCPConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", a.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	return c.(*net.TCPConn)
}
func TestAdmissionColdCannotForwardAndRevocationIsTerminal(t *testing.T) {
	var requests atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, "backend") }))
	defer b.Close()
	a := testAdmission(t, strings.TrimPrefix(b.URL, "http://"), 8, nil)
	for i := 0; i < 6; i++ {
		c := admissionConn(t, a)
		io.WriteString(c, "GET /metadata HTTP/1.1\r\nHost: external:8080\r\n\r\n")
		_, err := c.Read(make([]byte, 1))
		c.Close()
		if err == nil {
			t.Fatal("cold connection reached backend")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("cold requests forwarded")
	}
	if !a.admit() {
		t.Fatal("admission failed")
	}
	c := admissionConn(t, a)
	io.WriteString(c, "GET /metadata HTTP/1.1\r\nHost: external:8080\r\n\r\n")
	r, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	c.Close()
	if string(body) != "backend" {
		t.Fatal(string(body))
	}
	a.revoke()
	a.wait()
	if a.admit() {
		t.Fatal("revoked admission reopened")
	}
	if requests.Load() != 1 {
		t.Fatal("unexpected replay")
	}
}
func TestAdmissionPreservesHTTPBytesAndConnectionReuse(t *testing.T) {
	var connections atomic.Int32
	requests := make(chan string, 2)
	b := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- r.Method + " " + r.RequestURI + " " + r.Host + " " + r.Header.Get("X-Original") + " " + string(body)
		w.Header().Set("Location", "http://"+r.Host+"/fhir/result")
		w.Header().Set("Content-Encoding", "custom")
		w.WriteHeader(422)
		w.Write([]byte{0, 1, 255})
	}))
	b.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}
	b.Start()
	defer b.Close()
	a := testAdmission(t, strings.TrimPrefix(b.URL, "http://"), 8, nil)
	a.admit()
	c := admissionConn(t, a)
	defer c.Close()
	reader := bufio.NewReader(c)
	for i := 0; i < 2; i++ {
		const raw = "POST /fhir/Claim/$validate?profile=http://example.org/p|2.1.0&x=%2f%2B HTTP/1.1\r\nHost: original.example:18089\r\nX-Original: literal\r\nContent-Length: 5\r\n\r\na\x00b\r\n"
		io.WriteString(c, raw)
		r, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 422 || r.Header.Get("Content-Encoding") != "custom" || r.Header.Get("Location") != "http://original.example:18089/fhir/result" || string(body) != string([]byte{0, 1, 255}) {
			t.Fatalf("response changed %+v %q", r, body)
		}
		got := <-requests
		if got != "POST /fhir/Claim/$validate?profile=http://example.org/p|2.1.0&x=%2f%2B original.example:18089 literal a\x00b\r\n" {
			t.Fatal(got)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("connection reuse/replay: %d", connections.Load())
	}
}
func TestAdmissionHalfCloseDrainsResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		body, e := io.ReadAll(c)
		if e == nil && string(body) != "request" {
			e = io.ErrUnexpectedEOF
		}
		if e == nil {
			_, e = io.WriteString(c, "complete response")
		}
		done <- e
	}()
	a := testAdmission(t, ln.Addr().String(), 2, nil)
	a.admit()
	c := admissionConn(t, a)
	defer c.Close()
	io.WriteString(c, "request")
	c.CloseWrite()
	body, err := io.ReadAll(c)
	if err != nil || string(body) != "complete response" {
		t.Fatalf("half-close %q %v", body, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestAdmissionBoundsPendingDialsAndJoinsCancellation(t *testing.T) {
	entered := make(chan struct{}, 2)
	var calls atomic.Int32
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	a := testAdmission(t, "127.0.0.1:1", 1, dial)
	a.admit()
	c := admissionConn(t, a)
	defer c.Close()
	<-entered
	extra := admissionConn(t, a)
	_, err := extra.Read(make([]byte, 1))
	extra.Close()
	if err == nil {
		t.Fatal("saturated connection retained")
	}
	if calls.Load() != 1 {
		t.Fatal("saturation spawned dial")
	}
	a.revoke()
	a.wait()
	if a.count() != 0 {
		t.Fatal("pending dial slot leaked")
	}
}
func TestAdmissionBindCollision(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, err := newAdmission(context.Background(), ln.Addr().String(), "127.0.0.1:1", 1, nil)
	if err == nil {
		a.revoke()
		a.wait()
		t.Fatal("accepted bind collision")
	}
}

func awaitPairCount(t *testing.T, a *admission, want int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if a.count() == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("pair count=%d want=%d", a.count(), want)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAdmissionRSTClosesPeerAndReleasesSlot(t *testing.T) {
	for _, reset := range []string{"client", "backend"} {
		t.Run(reset, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			a := testAdmission(t, ln.Addr().String(), 1, nil)
			a.admit()
			c := admissionConn(t, a)
			defer c.Close()
			backend, err := ln.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			backend.SetDeadline(time.Now().Add(3 * time.Second))
			if reset == "client" {
				c.SetLinger(0)
				c.Close()
				_, err = backend.Read(make([]byte, 1))
			} else {
				backend.(*net.TCPConn).SetLinger(0)
				backend.Close()
				_, err = c.Read(make([]byte, 1))
			}
			if err == nil {
				t.Fatal("reset did not close opposite socket")
			}
			awaitPairCount(t, a, 0)
			// The released slot permits a new connection and is never double-counted.
			next := admissionConn(t, a)
			defer next.Close()
			peer, err := ln.Accept()
			if err != nil {
				t.Fatal(err)
			}
			peer.Close()
			a.revoke()
			a.wait()
			if a.count() != 0 {
				t.Fatal("slot leaked")
			}
		})
	}
}

func TestAdmissionFINOnlyClientJoinsOnOwnerCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a := testAdmission(t, ln.Addr().String(), 1, nil)
	a.admit()
	c := admissionConn(t, a)
	defer c.Close()
	backend, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	backend.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(c, "partial request")
	c.CloseWrite()
	body, err := io.ReadAll(backend)
	if err != nil || string(body) != "partial request" {
		t.Fatalf("FIN %q %v", body, err)
	}
	if a.count() != 1 {
		t.Fatal("FIN incorrectly tore down response direction")
	}
	a.revoke()
	a.wait()
	if a.count() != 0 {
		t.Fatal("FIN-only pair prevented owner join")
	}
}

func TestAdmissionStreamsBeforeRequestCompletes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a := testAdmission(t, ln.Addr().String(), 1, nil)
	a.admit()
	c := admissionConn(t, a)
	defer c.Close()
	backend, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	backend.SetDeadline(time.Now().Add(3 * time.Second))
	prefix := "POST /fhir/Claim/$validate HTTP/1.1\r\nHost: original\r\nContent-Length: 1048576\r\n\r\nfirst chunk"
	io.WriteString(c, prefix)
	got := make([]byte, len(prefix))
	if _, err := io.ReadFull(backend, got); err != nil || string(got) != prefix {
		t.Fatalf("request buffered until complete: %q %v", got, err)
	}
	a.revoke()
	a.wait()
}

func TestAdmissionRevocationClosesLateDialWithoutForwarding(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	backend, peer := net.Pipe()
	defer peer.Close()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	a := testAdmission(t, "127.0.0.1:1", 1, func(context.Context, string, string) (net.Conn, error) {
		close(entered)
		<-release
		return backend, nil
	})
	a.admit()
	c := admissionConn(t, a)
	defer c.Close()
	<-entered
	a.revoke()
	if a.count() != 1 {
		t.Fatal("pending dial abandoned before join")
	}
	unblock()
	a.wait()
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("late backend not closed: %v", err)
	}
	if a.count() != 0 {
		t.Fatal("late dial leaked slot")
	}
}
