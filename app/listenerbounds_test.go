//go:build !shn_coreonly

package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
)

// The listener's connection bounds (listenerbounds.go): isolated rejection
// rows beside the guard, each driving the real server serverFor or
// observerServer builds, over plain HTTP/1.1, TLS HTTP/1.1 and TLS HTTP/2
// where the row means something there. The bounds are shortened; each row's
// timing keeps a wide ratio to them.

// shortBounds are every bound shortened, for one rig. Each rig carries its
// own bounds (cuts.b), so the rows run in parallel.
func shortBounds() bounds {
	return bounds{
		readHeader:  400 * time.Millisecond,
		maxHeader:   64 << 10,
		idle:        1200 * time.Millisecond,
		bodyStall:   400 * time.Millisecond,
		bodyCeiling: 30 * time.Second,
		writeStall:  400 * time.Millisecond,
	}
}

// widenBounds are shortBounds with the progress bounds at 2 s, for the rows
// whose timing needs room under a loaded run: a slow but moving client that
// must complete, a trickle that must reach its ceiling before a stall bound,
// a stall that must fall while other traffic keeps moving, and the HTTP/2
// connection stalls, whose blocked write must be recognised under load.
func widenBounds() bounds {
	bd := shortBounds()
	bd.bodyStall = 2 * time.Second
	bd.writeStall = 2 * time.Second
	return bd
}

// cutSink keeps the cuts recorded, as "bound/listener".
type cutSink struct {
	mu  sync.Mutex
	got []string
}

func (s *cutSink) Record(m telemetry.Measurement) {
	if m.Instrument != telemetry.ListenerCuts {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for range int(m.Value) {
		s.got = append(s.got, m.Attributes[0]+"/"+m.Attributes[1])
	}
}

func (s *cutSink) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// boundsRig is one running listener and what it recorded.
type boundsRig struct {
	srv  *http.Server
	addr string
	tls  bool
	sink *cutSink
	log  *syncBuf
	// handlers counts the handlers running, as Run's countHandlers does,
	// so a test ends only after they return.
	handlers sync.WaitGroup
	// conns are the tracked connections the listener accepted, and closed
	// how many of them net/http has closed.
	mu     sync.Mutex
	conns  []*trackedConn
	closed int
}

// waitWriterBlocked waits until a write on one of the rig's connections has
// been blocked for at least d: a write deadline is in force (a completed
// write clears it) and was set at least d ago.
func (rig *boundsRig) waitWriterBlocked(t *testing.T, d, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		rig.mu.Lock()
		for _, c := range rig.conns {
			if c.writeAt.Load() != 0 && time.Since(time.Unix(0, c.writeSetAt.Load())) >= d {
				rig.mu.Unlock()
				return
			}
		}
		rig.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no write blocked")
}

// waitConnClosed waits until net/http has closed one of the rig's
// connections, observed on the server's side, never from what a client can
// still read of the bytes the server had sent. Callers must not close from
// the client first: net/http closes then too.
func (rig *boundsRig) waitConnClosed(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		rig.mu.Lock()
		n := rig.closed
		rig.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the connection stayed open")
}

// closeWrites is how many half-closes the rig's connections passed through.
func (rig *boundsRig) closeWrites() int {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	n := 0
	for _, c := range rig.conns {
		n += int(c.closeWrites.Load())
	}
	return n
}

type rigMode struct {
	name string
	tls  bool
	h2   bool
}

var (
	h1Modes  = []rigMode{{"plain", false, false}, {"tls", true, false}}
	allModes = []rigMode{{"plain", false, false}, {"tls", true, false}, {"h2", true, true}}
)

// startMain serves h behind the main listener's bounds, as Run does.
func startMain(t *testing.T, bd bounds, h http.Handler, useTLS bool) *boundsRig {
	t.Helper()
	rig := &boundsRig{tls: useTLS, sink: &cutSink{}, log: &syncBuf{}}
	c := &cuts{reg: telemetry.New(rig.sink), out: rig.log, listener: listenerMain, b: &bd}
	var cert *tls.Certificate
	if useTLS {
		cert = boundsTestCert(t)
	}
	rig.srv = serverFor("127.0.0.1:0", countHandlers(boundRequests(h, c), &rig.handlers), cert, c)
	rig.serve(t, c)
	return rig
}

// startObserver serves h behind the observer listener's bounds, as Run does.
func startObserver(t *testing.T, bd bounds, h http.Handler) *boundsRig {
	t.Helper()
	rig := &boundsRig{sink: &cutSink{}, log: &syncBuf{}}
	c := &cuts{reg: telemetry.New(rig.sink), out: rig.log, listener: listenerObserver, b: &bd}
	rig.srv = observerServer("127.0.0.1:0", h, c)
	rig.serve(t, c)
	return rig
}

func (rig *boundsRig) serve(t *testing.T, c *cuts) {
	t.Helper()
	rig.srv.ErrorLog = log.New(io.Discard, "", 0)
	state := rig.srv.ConnState
	rig.srv.ConnState = func(nc net.Conn, st http.ConnState) {
		if tc := trackedOf(nc); tc != nil {
			switch st {
			case http.StateNew:
				rig.mu.Lock()
				rig.conns = append(rig.conns, tc)
				rig.mu.Unlock()
			case http.StateClosed:
				rig.mu.Lock()
				rig.closed++
				rig.mu.Unlock()
			}
		}
		state(nc, st)
	}
	ln, err := listenBounded(rig.srv, c)
	if err != nil {
		t.Fatal(err)
	}
	rig.addr = ln.Addr().String()
	done := make(chan struct{})
	go func() { _ = serveBounded(rig.srv, ln); close(done) }()
	t.Cleanup(func() { _ = rig.srv.Close(); <-done; rig.handlers.Wait() })
}

// dial opens a raw connection: under TLS, negotiating proto.
func (rig *boundsRig) dial(t *testing.T, proto string) net.Conn {
	t.Helper()
	if !rig.tls {
		c, err := net.Dial("tcp", rig.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	c, err := tls.Dial("tcp", rig.addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{proto}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if got := c.ConnectionState().NegotiatedProtocol; got != proto {
		t.Fatalf("negotiated %q, want %q", got, proto)
	}
	return c
}

// client is an HTTP client for the rig: HTTP/2 when h2.
func (rig *boundsRig) client(h2 bool) (*http.Client, string) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: h2, DisableCompression: true}
	if !h2 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	scheme := "http"
	if rig.tls {
		scheme = "https"
	}
	return &http.Client{Transport: tr}, scheme + "://" + rig.addr
}

// slowClient is client with small receive buffers (the socket's, and HTTP/2's
// flow-control windows), so a reader's pace reaches the server's writes. Its
// reader takes a whole buffer (64 KiB) at a time: a read of less leaves
// macOS's receive window shut until a persist probe, seconds later.
func (rig *boundsRig) slowClient(h2 bool) (*http.Client, string) {
	cl, base := rig.client(h2)
	tr := cl.Transport.(*http.Transport)
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(64 << 10)
		}
		return c, err
	}
	tr.HTTP2 = &http.HTTP2Config{MaxReceiveBufferPerConnection: 64 << 10, MaxReceiveBufferPerStream: 64 << 10}
	return cl, base
}

// readSlowly reads body piece bytes at a time with gap between, and returns
// the bytes read.
func readSlowly(body io.Reader, piece int, gap time.Duration) int {
	total, buf := 0, make([]byte, piece)
	for {
		n, err := io.ReadFull(body, buf)
		total += n
		if err != nil {
			return total
		}
		time.Sleep(gap)
	}
}

// waitCuts waits for want (sorted) to be the cuts recorded, then holds that
// no other arrives for a while.
func (rig *boundsRig) waitCuts(t *testing.T, want ...string) {
	t.Helper()
	slices.Sort(want)
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := rig.sink.list()
		slices.Sort(got)
		if slices.Equal(got, want) || len(got) > len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	got := rig.sink.list()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("cuts = %v, want %v", got, want)
	}
	lines := strings.Count(rig.log.String(), "gateway: listener: cut a connection at its ")
	if lines != len(want) {
		t.Fatalf("%d cut lines, want %d: %q", lines, len(want), rig.log.String())
	}
	for _, w := range want {
		bound, listener, _ := strings.Cut(w, "/")
		line := fmt.Sprintf("gateway: listener: cut a connection at its %s bound (listener=%s)\n", bound, listener)
		if !strings.Contains(rig.log.String(), line) {
			t.Fatalf("log %q lacks %q", rig.log.String(), line)
		}
	}
}

// waitClosed reads c until the server closes it, and returns how long that
// took.
func waitClosed(t *testing.T, c net.Conn, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	_ = c.SetReadDeadline(start.Add(within))
	buf := make([]byte, 4096)
	for {
		if _, err := c.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("connection still open after %s", within)
			}
			return time.Since(start)
		}
	}
}

func boundsTestCert(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "%d", n)
	})
}

// 1. A slow-header client is cut at the header bound, counted and logged.
func TestListenerBounds_SlowHeaderIsCut(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			rig := startMain(t, bd, okHandler(), m.tls)
			c := rig.dial(t, "http/1.1")
			if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: a\r\n"); err != nil {
				t.Fatal(err)
			}
			took := waitClosed(t, c, 10*bd.readHeader)
			if took < bd.readHeader/2 {
				t.Fatalf("cut after %s, before the bound", took)
			}
			rig.waitCuts(t, "header/main")
		})
	}
}

// 2. A TLS handshake that stalls is cut at the header bound.
func TestListenerBounds_StalledHandshakeIsCut(t *testing.T) {
	t.Parallel()
	for _, partial := range []string{"", "\x16\x03\x01"} {
		t.Run(fmt.Sprintf("bytes=%d", len(partial)), func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			rig := startMain(t, bd, okHandler(), true)
			c, err := net.Dial("tcp", rig.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := io.WriteString(c, partial); err != nil {
				t.Fatal(err)
			}
			waitClosed(t, c, 10*bd.readHeader)
			rig.waitCuts(t, "header/main")
		})
	}
}

// 3. A header block over bd.maxHeader (plus net/http's 4096 bytes of slack)
// is answered 431, and that is no cut.
func TestListenerBounds_OversizeHeadersGet431(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			rig := startMain(t, bd, okHandler(), m.tls)
			c := rig.dial(t, "http/1.1")
			big := strings.Repeat("a", bd.maxHeader+4096+1)
			go func() { _, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: a\r\nX-Big: "+big+"\r\n\r\n") }()
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
				t.Fatalf("status %d, want 431", resp.StatusCode)
			}
			rig.waitCuts(t)
		})
	}
}

// 4. The largest legitimate header set passes: the network's own headers,
// each far past its measured size (the Hub's and a holder's assertions and an
// ingress bearer measure under 2 KiB together), 24 KiB in all.
func TestListenerBounds_LegitimateHeadersPass(t *testing.T) {
	t.Parallel()
	for _, m := range allModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			rig := startMain(t, bd, okHandler(), m.tls)
			cl, base := rig.client(m.h2)
			req, _ := http.NewRequest(http.MethodPost, base+"/", strings.NewReader("{}"))
			req.Header.Set("X-Hub-Assertion", strings.Repeat("h", 8<<10))
			req.Header.Set("X-Holder-Assertion", strings.Repeat("o", 8<<10))
			req.Header.Set("Authorization", "Bearer "+strings.Repeat("b", 8<<10))
			resp, err := cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || (m.h2 != (resp.ProtoMajor == 2)) {
				t.Fatalf("status %d proto %s", resp.StatusCode, resp.Proto)
			}
			rig.waitCuts(t)
		})
	}
}

// h2Frame is one HTTP/2 frame.
func h2Frame(typ, flags byte, stream uint32, payload []byte) []byte {
	b := make([]byte, 9, 9+len(payload))
	b[0], b[1], b[2] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	b[3], b[4] = typ, flags
	binary.BigEndian.PutUint32(b[5:], stream)
	return append(b, payload...)
}

func readH2Frame(r io.Reader) (typ, flags byte, payload []byte, err error) {
	var h [9]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	payload = make([]byte, int(h[0])<<16|int(h[1])<<8|int(h[2]))
	_, err = io.ReadFull(r, payload)
	return h[3], h[4], payload, err
}

const h2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// h2GetBlock is an HPACK block for GET https://127.0.0.1/.
var h2GetBlock = append([]byte{0x82, 0x87, 0x84, 0x41, 9}, "127.0.0.1"...)

// 5. HTTP/2: the server's SETTINGS carry net/http's default stream limit
// (nothing in the gateway sets one), and a stream whose HEADERS never
// complete is not cut by the header bound but by the idle bound, which
// net/http's HTTP/2 server enforces itself and which is not counted.
func TestListenerBounds_H2Settings(t *testing.T) {
	t.Parallel()
	bd := shortBounds()
	rig := startMain(t, bd, okHandler(), true)
	c := rig.dial(t, "h2")
	if _, err := io.WriteString(c, h2Preface); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(h2Frame(0x4, 0, 0, nil)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	streams := uint32(0)
	for streams == 0 {
		typ, flags, p, err := readH2Frame(c)
		if err != nil {
			t.Fatal(err)
		}
		if typ != 0x4 || flags&0x1 != 0 {
			continue
		}
		for i := 0; i+6 <= len(p); i += 6 {
			if binary.BigEndian.Uint16(p[i:]) == 0x3 {
				streams = binary.BigEndian.Uint32(p[i+2:])
			}
		}
		if streams == 0 {
			t.Fatal("SETTINGS without MAX_CONCURRENT_STREAMS")
		}
	}
	if streams != 250 {
		t.Fatalf("MAX_CONCURRENT_STREAMS = %d, want net/http's default 250", streams)
	}

	// HEADERS without END_HEADERS, then nothing.
	if _, err := c.Write(h2Frame(0x1, 0x1, 1, h2GetBlock[:2])); err != nil {
		t.Fatal(err)
	}
	took := waitClosed(t, c, 10*bd.idle)
	if took < bd.idle*3/4 {
		t.Fatalf("closed after %s: before the idle bound %s", took, bd.idle)
	}
	rig.waitCuts(t)
}

// postStalled sends a request whose body stops after part of it.
func postStalled(t *testing.T, c net.Conn, path string, total int, sent string) {
	t.Helper()
	if _, err := fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: a\r\nContent-Length: %d\r\n\r\n%s", path, total, sent); err != nil {
		t.Fatal(err)
	}
}

// 6. A body that stalls past bd.bodyStall is cut, counted body-stall.
func TestListenerBounds_BodyStallIsCut(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			rig := startMain(t, bd, okHandler(), m.tls)
			c := rig.dial(t, "http/1.1")
			postStalled(t, c, "/", 1000, "0123456789")
			took := waitClosed(t, c, 20*bd.bodyStall)
			if took < bd.bodyStall/2 {
				t.Fatalf("cut after %s, before the bound", took)
			}
			rig.waitCuts(t, "body-stall/main")
		})
	}
	t.Run("h2", func(t *testing.T) {
		t.Parallel()
		bd := shortBounds()
		rig := startMain(t, bd, okHandler(), true)
		cl, base := rig.client(true)
		pr, pw := io.Pipe()
		defer pw.Close()
		go func() { _, _ = pw.Write([]byte("0123456789")) }()
		req, _ := http.NewRequest(http.MethodPost, base+"/", pr)
		resp, err := cl.Do(req)
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		rig.waitCuts(t, "body-stall/main")
	})
}

// pacedBody yields total bytes in chunks with a gap before each chunk.
type pacedBody struct {
	left, chunk int
	inChunk     int
	gap         time.Duration
}

func (p *pacedBody) Read(b []byte) (int, error) {
	if p.left == 0 {
		return 0, io.EOF
	}
	if p.inChunk == 0 {
		time.Sleep(p.gap)
		p.inChunk = p.chunk
	}
	n := min(len(b), p.left, p.inChunk)
	p.left -= n
	p.inChunk -= n
	return n, nil
}

// 7. A slow but moving body completes: it takes well over bodyStall in all
// (32 gaps of an eighth of it, four bounds), every gap well under it. The
// row moves 8 MiB: the bound is on progress, not size, and less data keeps
// the row short.
func TestListenerBounds_SlowMovingBodyCompletes(t *testing.T) {
	t.Parallel()
	for _, m := range allModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := widenBounds()
			rig := startMain(t, bd, okHandler(), m.tls)
			cl, base := rig.client(m.h2)
			const total = 8 << 20
			body := &pacedBody{left: total, chunk: total / 32, gap: bd.bodyStall / 8}
			req, _ := http.NewRequest(http.MethodPost, base+"/", body)
			req.ContentLength = total
			start := time.Now()
			resp, err := cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			took := time.Since(start)
			if resp.StatusCode != http.StatusOK || string(got) != fmt.Sprint(total) {
				t.Fatalf("status %d body %q", resp.StatusCode, got)
			}
			if took < 3*bd.bodyStall {
				t.Fatalf("took %s: not well over bd.bodyStall", took)
			}
			rig.waitCuts(t)
		})
	}
}

// 8. A body that trickles past bd.bodyCeiling is cut, counted body-ceiling.
func TestListenerBounds_TrickleHitsCeiling(t *testing.T) {
	t.Parallel()
	bd := widenBounds()
	bd.bodyCeiling = 2 * bd.bodyStall
	rig := startMain(t, bd, okHandler(), false)
	c := rig.dial(t, "http/1.1")
	postStalled(t, c, "/", 1<<20, "")
	stop := make(chan struct{})
	defer close(stop)
	gap := bd.bodyStall / 8
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(gap):
				if _, err := c.Write([]byte("x")); err != nil {
					return
				}
			}
		}
	}()
	took := waitClosed(t, c, 10*bd.bodyCeiling)
	if took < bd.bodyCeiling*3/4 {
		t.Fatalf("cut after %s, before the ceiling %s", took, bd.bodyCeiling)
	}
	rig.waitCuts(t, "body-ceiling/main")
}

// attachmentHandler reads its body under its own read deadline, the way the
// engine's documentation attachment path does (limitAttachmentRead). The
// engine's own bound cannot be shortened from this package without a seam in
// the published module, so this row drives a handler that does what it does.
func attachmentHandler(arrival time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(arrival))
		n, err := io.Copy(io.Discard, r.Body)
		_ = rc.SetReadDeadline(time.Time{})
		if err != nil {
			return
		}
		fmt.Fprintf(w, "%d", n)
	})
}

// 9. A handler's own read deadline governs its body: a gap past bd.bodyStall
// but inside the handler's deadline completes; a stall past it is counted
// attachment-arrival.
func TestListenerBounds_HandlerDeadlineGoverns(t *testing.T) {
	t.Parallel()
	for _, m := range allModes {
		t.Run(m.name+"/gap-completes", func(t *testing.T) {
			t.Parallel()
			bd := widenBounds()
			rig := startMain(t, bd, attachmentHandler(4*bd.bodyStall), m.tls)
			cl, base := rig.client(m.h2)
			pr, pw := io.Pipe()
			gap := 2 * bd.bodyStall
			go func() {
				_, _ = pw.Write([]byte("01234"))
				time.Sleep(gap)
				_, _ = pw.Write([]byte("56789"))
				_ = pw.Close()
			}()
			req, _ := http.NewRequest(http.MethodPost, base+"/", pr)
			req.ContentLength = 10
			resp, err := cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(got) != "10" {
				t.Fatalf("status %d body %q", resp.StatusCode, got)
			}
			rig.waitCuts(t)
		})
	}
	for _, m := range h1Modes {
		t.Run(m.name+"/stall-cut", func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			arrival := 3 * bd.bodyStall
			rig := startMain(t, bd, attachmentHandler(arrival), m.tls)
			c := rig.dial(t, "http/1.1")
			postStalled(t, c, "/", 1000, "01234")
			took := waitClosed(t, c, 10*arrival)
			if took < arrival*3/4 {
				t.Fatalf("cut after %s, before the handler's %s", took, arrival)
			}
			rig.waitCuts(t, "attachment-arrival/main")
		})
	}
}

// 10. A long answer completes: the handler waits past every bound, then
// writes a large answer to a slow but steady reader; and a small write, a
// pause past bd.writeStall, then return (net/http's final flush) completes too.
func TestListenerBounds_LongAnswerCompletes(t *testing.T) {
	t.Parallel()
	pause := func(bd bounds) time.Duration { return 3 * max(bd.writeStall, bd.bodyStall, bd.readHeader) / 2 }
	for _, m := range allModes {
		t.Run(m.name+"/large", func(t *testing.T) {
			t.Parallel()
			bd := widenBounds()
			const size = 2 << 20
			rig := startMain(t, bd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(pause(bd))
				chunk := bytes.Repeat([]byte("a"), 32<<10)
				for n := 0; n < size; n += len(chunk) {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
			}), m.tls)
			cl, base := rig.slowClient(m.h2)
			resp, err := cl.Get(base + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if total := readSlowly(resp.Body, 64<<10, bd.writeStall/8); total != size {
				t.Fatalf("read %d of %d", total, size)
			}
			rig.waitCuts(t)
		})
		t.Run(m.name+"/one-large-write", func(t *testing.T) {
			t.Parallel()
			// The engine writes a whole answer in one Write: the bound is
			// still on progress, so a reader slower than size/writeStall
			// but steady completes; over HTTP/2 under the main listener's
			// own configuration, its write progress bound included.
			bd := widenBounds()
			const size = 2 << 20
			rig := startMain(t, bd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(bytes.Repeat([]byte("a"), size))
			}), m.tls)
			if rig.srv.HTTP2 == nil || rig.srv.HTTP2.WriteByteTimeout != bd.writeStall {
				t.Fatalf("main listener HTTP/2 config %+v", rig.srv.HTTP2)
			}
			cl, base := rig.slowClient(m.h2)
			start := time.Now()
			resp, err := cl.Get(base + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if total := readSlowly(resp.Body, 64<<10, bd.writeStall/8); total != size {
				t.Fatalf("read %d of %d", total, size)
			}
			if took := time.Since(start); took < 3*bd.writeStall {
				t.Fatalf("took %s: not three writeStall bounds (%s) of progress", took, bd.writeStall)
			}
			rig.waitCuts(t)
		})
		t.Run(m.name+"/final-flush", func(t *testing.T) {
			t.Parallel()
			bd := widenBounds()
			rig := startMain(t, bd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("partial-"))
				time.Sleep(pause(bd))
				_, _ = w.Write([]byte("done"))
			}), m.tls)
			cl, base := rig.client(m.h2)
			resp, err := cl.Get(base + "/")
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(got) != "partial-done" {
				t.Fatalf("body %q err %v", got, err)
			}
			rig.waitCuts(t)
		})
	}
}

// bigWriter writes until a write fails.
func bigWriter(failed chan<- error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := bytes.Repeat([]byte("a"), 64<<10)
		for range 4096 {
			if _, err := w.Write(chunk); err != nil {
				failed <- err
				return
			}
		}
		failed <- nil
	})
}

// 11. A reader that stops reading is cut, counted write-stall, over
// HTTP/1.1 and HTTP/2.
func TestListenerBounds_StoppedReaderIsCut(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			failed := make(chan error, 1)
			rig := startMain(t, bd, bigWriter(failed), m.tls)
			c := rig.dial(t, "http/1.1")
			if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-failed:
				if err == nil {
					t.Fatal("every write succeeded")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the handler was not cut")
			}
			rig.waitCuts(t, "write-stall/main")
		})
	}
	t.Run("h2", func(t *testing.T) {
		t.Parallel()
		bd := shortBounds()
		failed := make(chan error, 1)
		rig := startMain(t, bd, bigWriter(failed), true)
		c := rig.dial(t, "h2")
		// No WINDOW_UPDATE ever: the stream's window fills and stays full.
		if _, err := c.Write(append([]byte(h2Preface), append(h2Frame(0x4, 0, 0, nil), h2Frame(0x1, 0x5, 1, h2GetBlock)...)...)); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-failed:
			if err == nil {
				t.Fatal("every write succeeded")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the handler was not cut")
		}
		rig.waitCuts(t, "write-stall/main")
	})
}

// 12. An idle kept-alive connection is closed after bd.idle, uncounted.
func TestListenerBounds_IdleKeepAliveClosedUncounted(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			rig := startMain(t, bd, okHandler(), m.tls)
			c := rig.dial(t, "http/1.1")
			br := bufio.NewReader(c)
			for range 2 {
				if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			start := time.Now()
			_ = c.SetReadDeadline(start.Add(10 * bd.idle))
			if _, err := br.ReadByte(); err == nil {
				t.Fatal("read a byte on an idle connection")
			}
			if took := time.Since(start); took < bd.idle*3/4 || took > 5*bd.idle {
				t.Fatalf("closed after %s, idle bound %s", took, bd.idle)
			}
			rig.waitCuts(t)
		})
	}
}

// 13. The observer carries the server-level bounds, and its event stream
// outlives every one of them; a slow header on it is cut and counted.
func TestListenerBounds_ObserverStreamOutlivesBounds(t *testing.T) {
	t.Parallel()
	bd := shortBounds()
	events := 0
	span := 3 * bd.idle
	rig := startObserver(t, bd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		end := time.Now().Add(span)
		for n := 0; time.Now().Before(end); n++ {
			fmt.Fprintf(w, "data: %d\n\n", n)
			http.NewResponseController(w).Flush()
			time.Sleep(span / 30)
		}
	}))
	if rig.srv.ReadHeaderTimeout != bd.readHeader || rig.srv.MaxHeaderBytes != bd.maxHeader || rig.srv.IdleTimeout != bd.idle ||
		rig.srv.ReadTimeout != 0 || rig.srv.WriteTimeout != 0 {
		t.Fatalf("observer server bounds: %+v", rig.srv)
	}
	cl, base := rig.client(false)
	start := time.Now()
	resp, err := cl.Get(base + "/events")
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			events++
		}
	}
	resp.Body.Close()
	if took := time.Since(start); took < span || events < 20 {
		t.Fatalf("stream ran %s with %d events", took, events)
	}
	rig.waitCuts(t)

	c := rig.dial(t, "http/1.1")
	if _, err := io.WriteString(c, "GET /events HTTP/1.1\r\n"); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, c, 10*bd.readHeader)
	rig.waitCuts(t, "header/observer")
}

// The main server carries the same server-level bounds, and no ReadTimeout or
// WriteTimeout.
// Its HTTP/2 departs from net/http's defaults in one field only, the
// connection's write progress bound; the observer's not at all.
func TestListenerBounds_MainServerFields(t *testing.T) {
	t.Parallel()
	s := serverFor("127.0.0.1:0", http.NewServeMux(), boundsTestCert(t), nil)
	if s.ReadHeaderTimeout != 10*time.Second || s.MaxHeaderBytes != 64<<10 || s.IdleTimeout != 120*time.Second ||
		s.ReadTimeout != 0 || s.WriteTimeout != 0 || !onlyWriteByteTimeout(s.HTTP2, 30*time.Second) {
		t.Fatalf("main server bounds: header %s max %d idle %s read %s write %s http2 %+v",
			s.ReadHeaderTimeout, s.MaxHeaderBytes, s.IdleTimeout, s.ReadTimeout, s.WriteTimeout, s.HTTP2)
	}
	o := observerServer("127.0.0.1:0", http.NewServeMux(), nil)
	if o.ReadHeaderTimeout != 10*time.Second || o.MaxHeaderBytes != 64<<10 || o.IdleTimeout != 120*time.Second ||
		o.ReadTimeout != 0 || o.WriteTimeout != 0 || o.HTTP2 != nil {
		t.Fatalf("observer server bounds: %+v", o)
	}
	if bd := defaultBounds(); bd.bodyStall != 30*time.Second || bd.bodyCeiling != 10*time.Minute || bd.writeStall != 30*time.Second || writePiece != 8<<10 {
		t.Fatalf("request bounds: stall %s ceiling %s write %s piece %d", bd.bodyStall, bd.bodyCeiling, bd.writeStall, writePiece)
	}
}

// 14. A cut's log line carries no request data: not the path, not a header's
// value, not the remote address.
func TestListenerBounds_LogCarriesNoRequestData(t *testing.T) {
	t.Parallel()
	const path, value = "/zq-distinct-path-7731", "zq-distinct-value-4417"
	bd := shortBounds()
	rig := startMain(t, bd, okHandler(), false)

	header := rig.dial(t, "http/1.1")
	if _, err := io.WriteString(header, "GET "+path+" HTTP/1.1\r\nHost: a\r\nX-Probe: "+value+"\r\n"); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, header, 10*bd.readHeader)

	body := rig.dial(t, "http/1.1")
	if _, err := fmt.Fprintf(body, "POST %s HTTP/1.1\r\nHost: a\r\nX-Probe: %s\r\nContent-Length: 100\r\n\r\n%s", path, value, value); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, body, 20*bd.bodyStall)
	rig.waitCuts(t, "header/main", "body-stall/main")

	logged := rig.log.String()
	for _, secret := range []string{path, value, "X-Probe", header.LocalAddr().String(), body.LocalAddr().String(), "127.0.0.1"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log line carries %q: %q", secret, logged)
		}
	}
}

// onlyWriteByteTimeout is whether h sets WriteByteTimeout to d and no other
// field.
func onlyWriteByteTimeout(h *http.HTTP2Config, d time.Duration) bool {
	if h == nil || h.WriteByteTimeout != d {
		return false
	}
	c := *h
	c.WriteByteTimeout = 0
	return reflect.ValueOf(c).IsZero()
}

// gatewayGoFiles is every non-test Go file of the gateway module's packages
// (outboundDirs), the module root relative. Each walk's root is one of those
// directories, so the test is selected for a change under them only.
func gatewayGoFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range outboundDirs {
		err := filepath.WalkDir(filepath.Join(gatewayModule, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(gatewayModule, path)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) < 50 {
		t.Fatalf("walked %d files: not the gateway module", len(out))
	}
	return out
}

// 15. Fences: HTTP/2 stays at net/http's defaults (no file sets a stream limit
// or an HTTP/2 configuration), and the only read and write deadlines are the
// listener's own and the documentation attachment's arrival bound.
func TestListenerBounds_Fences(t *testing.T) {
	t.Parallel()
	files := gatewayGoFiles(t)
	var deadlines []string
	for p, src := range files {
		if strings.Contains(src, "MaxConcurrentStreams") {
			t.Errorf("%s names MaxConcurrentStreams: HTTP/2 keeps net/http's default stream limit", p)
		}
		// The main listener's one departure from net/http's HTTP/2
		// defaults is its write progress bound, set in listenerbounds.go
		// and nowhere else.
		if n := strings.Count(src, "HTTP2Config"); p == "app/listenerbounds.go" {
			if n != 1 || !strings.Contains(src, "s.HTTP2 = &http.HTTP2Config{WriteByteTimeout: c.bounds().writeStall}") {
				t.Errorf("%s: the HTTP/2 configuration is not WriteByteTimeout alone", p)
			}
		} else if n > 0 || strings.Contains(src, ".HTTP2 =") {
			t.Errorf("%s configures HTTP/2: only the main listener's write progress bound departs from net/http's defaults", p)
		}
		if strings.Contains(src, "SetReadDeadline(") || strings.Contains(src, "SetWriteDeadline(") {
			deadlines = append(deadlines, p)
		}
	}
	slices.Sort(deadlines)
	want := []string{"app/listenerbounds.go", "engine/workstream_pa_cdex_size.go"}
	if !slices.Equal(deadlines, want) {
		t.Errorf("files setting read or write deadlines = %v, want %v", deadlines, want)
	}
	if n := strings.Count(files["engine/workstream_pa_cdex_size.go"], "SetReadDeadline("); n != 2 ||
		strings.Contains(files["engine/workstream_pa_cdex_size.go"], "SetWriteDeadline(") {
		t.Errorf("the attachment path's deadlines changed: %d SetReadDeadline calls", n)
	}
}

// refuseUnread answers 401 without reading the request's body.
func refuseUnread() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
}

// A body the handler leaves unread is discarded by net/http, whose background
// read it then ends with a deadline in the past: no cut, and the connection
// goes on to its next request.
func TestListenerBounds_UnreadBodyIsNoCut(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			rig := startMain(t, bd, refuseUnread(), m.tls)
			c := rig.dial(t, "http/1.1")
			br := bufio.NewReader(c)
			if _, err := io.WriteString(c, "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 10\r\n\r\n0123456789"); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || resp.Close {
				t.Fatalf("status %d close %t", resp.StatusCode, resp.Close)
			}
			if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			if resp, err = http.ReadResponse(br, nil); err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("second request: status %d", resp.StatusCode)
			}
			rig.waitCuts(t)
		})
	}
}

// net/http keeps deciding what to do with a body the handler left unread
// (the handler sees a copy of the request carrying the bounded body; net/http's
// own keeps its body's type). A request awaiting 100-continue is answered with
// no 100 Continue and marked Connection: close; its client then closes, as a
// client given that answer does. A remainder past 256 KiB is not read first
// (a read of it would stall and be cut) and the connection is half-closed,
// which the tracked connection passes through (CloseWrite). Neither is a cut.
func TestListenerBounds_UnreadBodyKeepsNetHTTPRules(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		for _, tc := range []struct {
			name, req    string
			clientCloses bool
		}{
			{"100-continue", "POST / HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: 1000\r\n\r\n", true},
			{"large-remainder", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 1048576\r\n\r\n" + strings.Repeat("x", 1024), false},
		} {
			t.Run(m.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				bd := shortBounds()
				rig := startMain(t, bd, refuseUnread(), m.tls)
				c := rig.dial(t, "http/1.1")
				if _, err := io.WriteString(c, tc.req); err != nil {
					t.Fatal(err)
				}
				_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
				br := bufio.NewReader(c)
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("no answer: %v", err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized || !resp.Close {
					t.Fatalf("status %d close %t: want 401 and Connection: close", resp.StatusCode, resp.Close)
				}
				if tc.clientCloses {
					_ = c.Close()
					time.Sleep(2 * bd.bodyStall)
					rig.waitCuts(t)
					return
				}
				if _, err := br.ReadByte(); err == nil {
					t.Fatal("the connection stayed open")
				} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
					t.Fatal("the connection was not closed")
				}
				// Over TLS the half-close is TLS's own close_notify alert,
				// which reaches no CloseWrite beneath.
				if n, want := rig.closeWrites(), map[bool]int{false: 1, true: 0}[m.tls]; n != want {
					t.Fatalf("%d half-closes passed through, want %d", n, want)
				}
				rig.waitCuts(t)
			})
		}
	}
}

// Over HTTP/1.1, an answer's first 2 KiB or more makes net/http discard a body
// the handler has not read, inside the handler's write. That discard is
// bounded too: a client that declared a body and sends none is cut near
// bodyStall, counted once as body-stall.
func TestListenerBounds_DiscardInsideWriteIsBounded(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			rig := startMain(t, bd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(bytes.Repeat([]byte("m"), 4<<10))
			}), m.tls)
			c := rig.dial(t, "http/1.1")
			if _, err := io.WriteString(c, "GET /metadata HTTP/1.1\r\nHost: a\r\nContent-Length: 1000\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			took := waitClosed(t, c, 20*bd.bodyStall)
			if took < bd.bodyStall/2 {
				t.Fatalf("closed after %s, before the bound", took)
			}
			rig.waitCuts(t, "body-stall/main")
		})
	}
}

// Over HTTP/2, a client that stops reading at TCP, with windows wide open so
// flow control never stops the writes, is cut by the main listener's write
// progress bound near writeStall: counted write-stall once for the
// connection, not again for each of its streams. With reset-all, the client
// then resets every stream while the frame writer is blocked: the last
// stream's close (net/http's idle hook over HTTP/2) must leave that write's
// deadline in force. The hook knows the connection is HTTP/2 from the
// connection itself (its negotiated protocol), not from a request having
// reached a handler, so the same holds for streams net/http answers itself.
func TestListenerBounds_H2TCPStallIsCut(t *testing.T) {
	t.Parallel()
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "streams-open", true: "reset-all"}[reset], func(t *testing.T) {
			t.Parallel()
			bd := widenBounds()
			failed := make(chan error, 3)
			rig := startMain(t, bd, bigWriter(failed), true)
			raw, err := net.Dial("tcp", rig.addr)
			if err != nil {
				t.Fatal(err)
			}
			_ = raw.(*net.TCPConn).SetReadBuffer(4 << 10)
			c := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
			t.Cleanup(func() { _ = c.Close() })
			if err := c.Handshake(); err != nil {
				t.Fatal(err)
			}
			settings := []byte{0, 0x4, 0x40, 0, 0, 0} // INITIAL_WINDOW_SIZE 2^30
			window := []byte{0x40, 0, 0, 0}           // connection window +2^30
			out := append([]byte(h2Preface), h2Frame(0x4, 0, 0, settings)...)
			out = append(out, h2Frame(0x8, 0, 0, window)...)
			streams := []uint32{1, 3, 5}
			for _, id := range streams {
				out = append(out, h2Frame(0x1, 0x5, id, h2GetBlock)...)
			}
			if _, err := c.Write(out); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if reset {
				// Reset only once the frame writer is blocked, which on a
				// loaded run can take longer than any fixed wait.
				rig.waitWriterBlocked(t, bd.writeStall/4, 30*time.Second)
				if got := rig.sink.list(); len(got) != 0 {
					t.Fatalf("cut %v before the resets: the row would not reach the idle hook", got)
				}
				var rst []byte
				for _, id := range streams {
					rst = append(rst, h2Frame(0x3, 0, id, []byte{0, 0, 0, 0x8})...) // CANCEL
				}
				if _, err := c.Write(rst); err != nil {
					t.Fatal(err)
				}
			}
			for range streams {
				select {
				case err := <-failed:
					if err == nil {
						t.Fatal("every write succeeded")
					}
				case <-time.After(30 * time.Second):
					t.Fatal("the streams were not ended")
				}
			}
			rig.waitCuts(t, "write-stall/main")
			if took := time.Since(start); took < bd.writeStall/2 {
				t.Fatalf("cut after %s, before the bound", took)
			}
			// The server closes the connection. It is read from the
			// server's side: a client that has stopped reading behind a small
			// receive buffer drains what the server already sent only as
			// fast as TCP's zero-window probes let it, which on Linux can
			// take longer than any bound here.
			rig.waitConnClosed(t, 10*time.Second)
		})
	}
}

// Over HTTP/2, one stream whose client stops granting it window is cut on
// its own while another stream on the same connection keeps the frame
// writer busy: stream B is counted write-stall once, stream A completes, and
// the connection is not cut. A's client reads the socket slowly, so the
// frame writer is mostly blocked on TCP, though never for long, while B
// stalls.
func TestListenerBounds_H2StreamStallAmidTraffic(t *testing.T) {
	t.Parallel()
	bd := widenBounds()
	// A is read slowly for several write bounds, so B's stall falls while A
	// keeps the connection's writer busy.
	const sizeA = 16 << 20
	failed := make(chan error, 1)
	stalled := bigWriter(failed)
	rig := startMain(t, bd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			stalled.ServeHTTP(w, r)
			return
		}
		chunk := bytes.Repeat([]byte("a"), 32<<10)
		for n := 0; n < sizeA; n += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}), true)
	raw, err := net.Dial("tcp", rig.addr)
	if err != nil {
		t.Fatal(err)
	}
	// A receive buffer below one loopback segment (64 KiB on Linux) makes
	// the server's kernel hold data back for its small-window avoidance even
	// while A is read, which no real client does; 256 KiB keeps A's pace the
	// reader's own.
	_ = raw.(*net.TCPConn).SetReadBuffer(256 << 10)
	c := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	big := []byte{0x40, 0, 0, 0} // +2^30
	out := append([]byte(h2Preface), h2Frame(0x4, 0, 0, nil)...)
	out = append(out, h2Frame(0x8, 0, 0, big)...) // the connection's window
	out = append(out, h2Frame(0x1, 0x5, 1, h2GetBlock)...)
	out = append(out, h2Frame(0x8, 0, 1, big)...) // A's window; B keeps the initial 64 KiB
	pathB := append([]byte{0x82, 0x87, 0x85, 0x41, 9}, "127.0.0.1"...)
	out = append(out, h2Frame(0x1, 0x5, 3, pathB)...)
	if _, err := c.Write(out); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(90 * time.Second))
	gotA, ended := 0, false
	for !ended {
		typ, flags, p, err := readH2Frame(c)
		if err != nil {
			t.Fatalf("after %d bytes of A: %v", gotA, err)
		}
		switch {
		case typ == 0x7: // GOAWAY
			t.Fatal("the connection was ended")
		case typ == 0x0 && len(p) > 0: // DATA: a slow reader of the socket
			time.Sleep(8 * time.Millisecond)
		}
		if typ == 0x0 {
			// Stream ids are in the frame header readH2Frame drops; only
			// A's DATA can carry END_STREAM before A ends.
			gotA += len(p)
			ended = flags&0x1 != 0
		}
	}
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("B's writes all succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B was not cut")
	}
	if gotA < sizeA {
		t.Fatalf("A got %d of %d bytes", gotA, sizeA)
	}
	rig.waitCuts(t, "write-stall/main")
}

// Over HTTP/1.1, once net/http has discarded an unread body itself (inside
// the answer's first write) and started its background read, no body bound
// of ours reaches that read: a pause past the body's ceiling and more writes
// complete, and nothing is cut.
func TestListenerBounds_DiscardHandsReadToNetHTTP(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := shortBounds()
			bd.bodyCeiling = 2 * bd.bodyStall
			rig := startMain(t, bd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(bytes.Repeat([]byte("a"), 4<<10))
				time.Sleep(3 * bd.bodyStall)
				_, _ = w.Write(bytes.Repeat([]byte("b"), 4<<10))
				// Long enough for a deadline of ours on net/http's
				// background read to fire before the handler returns.
				time.Sleep(bd.bodyStall / 4)
			}), m.tls)
			c := rig.dial(t, "http/1.1")
			if _, err := io.WriteString(c, "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || len(got) != 8<<10 {
				t.Fatalf("read %d bytes: %v", len(got), err)
			}
			rig.waitCuts(t)
		})
	}
}

// Over HTTP/1.1, net/http's discard of an unread body is bounded by progress,
// not by one fixed deadline: a remainder that keeps moving, for well over
// bodyStall in all, is discarded and the answer completes, uncut.
func TestListenerBounds_MovingDiscardCompletes(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := widenBounds()
			const total, chunk = 160 << 10, 10 << 10
			rig := startMain(t, bd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(bytes.Repeat([]byte("a"), 4<<10))
			}), m.tls)
			c := rig.dial(t, "http/1.1")
			if _, err := fmt.Fprintf(c, "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: %d\r\n\r\n", total); err != nil {
				t.Fatal(err)
			}
			gap := bd.bodyStall / 4
			go func() {
				for sent := 0; sent < total; sent += chunk {
					time.Sleep(gap)
					if _, err := c.Write(bytes.Repeat([]byte("x"), chunk)); err != nil {
						return
					}
				}
			}()
			start := time.Now()
			_ = c.SetReadDeadline(start.Add(30 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || len(got) != 4<<10 {
				t.Fatalf("read %d bytes: %v", len(got), err)
			}
			if took := time.Since(start); took < 3*bd.bodyStall {
				t.Fatalf("took %s: not three bodyStall bounds of progress", took)
			}
			rig.waitCuts(t)
		})
	}
}

// net/http's drain of a body the handler left unread is held to the body's
// ceiling as well as its progress: a remainder that trickles, never 30 s
// without a byte, is cut at the ceiling and counted body-ceiling.
func TestListenerBounds_DrainTrickleHitsCeiling(t *testing.T) {
	t.Parallel()
	for _, m := range h1Modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			bd := widenBounds()
			bd.bodyCeiling = 2 * bd.bodyStall
			rig := startMain(t, bd, refuseUnread(), m.tls)
			c := rig.dial(t, "http/1.1")
			if _, err := io.WriteString(c, "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 200000\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			stop := make(chan struct{})
			defer close(stop)
			gap := bd.bodyStall / 4
			go func() {
				for {
					select {
					case <-stop:
						return
					case <-time.After(gap):
						if _, err := c.Write([]byte("x")); err != nil {
							return
						}
					}
				}
			}()
			took := waitClosed(t, c, 10*bd.bodyCeiling)
			if took < bd.bodyCeiling*3/4 {
				t.Fatalf("cut after %s, before the ceiling %s", took, bd.bodyCeiling)
			}
			rig.waitCuts(t, "body-ceiling/main")
		})
	}
}

// A handler that reads part of a body and closes it makes net/http discard
// the rest inside the handler (over HTTP/1.1): that drain is bounded like the
// one after the handler, so a client that stalls is cut near bodyStall and
// counted body-stall.
func TestListenerBounds_HandlerCloseDrainIsBounded(t *testing.T) {
	t.Parallel()
	bd := shortBounds()
	rig := startMain(t, bd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 5)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			return
		}
		_ = r.Body.Close()
		_, _ = w.Write([]byte("ok"))
	}), false)
	c := rig.dial(t, "http/1.1")
	if _, err := io.WriteString(c, "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n5\r\nwo"); err != nil {
		t.Fatal(err)
	}
	took := waitClosed(t, c, 10*bd.bodyStall)
	if took < bd.bodyStall/2 {
		t.Fatalf("cut after %s, before the bound", took)
	}
	rig.waitCuts(t, "body-stall/main")
}

// The write deadline the main listener's HTTP/2 counts as its own
// (WriteByteTimeout's, set writeStall ahead, read a little later under load)
// is told apart from crypto/tls's 5 s close deadline and anything longer
// than writeStall, at production's and the tests' bounds.
func TestListenerBounds_AttributionWindow(t *testing.T) {
	t.Parallel()
	for _, ws := range []time.Duration{30 * time.Second, 2 * time.Second, 400 * time.Millisecond} {
		bd := defaultBounds()
		bd.writeStall = ws
		for _, row := range []struct {
			span time.Duration
			want bool
		}{
			{ws, true},
			{ws - time.Millisecond, true},
			{ws - ws*49/100, true},
			{ws - ws*6/10, false},
			{ws + 10*time.Millisecond, false},
			{5 * time.Second, ws == 5*time.Second},
			{0, false},
		} {
			c := &trackedConn{cuts: &cuts{b: &bd}}
			c.h2.Store(true)
			set := time.Now()
			c.writeSetAt.Store(set.UnixNano())
			c.writeAt.Store(set.Add(row.span).UnixNano())
			if got := c.oursTimedOut(); got != row.want {
				t.Errorf("writeStall %s, span %s: ours %v, want %v", ws, row.span, got, row.want)
			}
		}
	}
}
