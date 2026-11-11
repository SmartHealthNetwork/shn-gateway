//go:build !shn_coreonly

package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/nosniff"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
)

// The connection bounds the main and observer listeners carry (DEPLOYMENT.md,
// "Connection bounds"); the Prometheus pull listener keeps its own. They are the listener's own resource bounds, not exchange
// refusals, and they have no settings: defaultBounds holds them, and only a
// test gives a listener other values (cuts.bounds).
//
// The server-level bounds, on the main listener and the observer:
//   - readHeader: a request's headers must arrive within it; net/http also
//     uses it as the TLS handshake's deadline. Over HTTP/2 it does not apply
//     to a stream whose HEADERS never complete: idle ends that connection,
//     and that close is net/http's own and not counted.
//   - maxHeader: the whole header block's ceiling, the load balancer's own;
//     past it (plus net/http's 4096 bytes of slack) the answer is 431.
//   - idle: a kept-alive connection with no request closes after it. It
//     stays above the load balancer's 60 s default idle timeout, so the load
//     balancer closes an idle connection first and never sends on one this
//     side is closing.
//
// Deliberately no ReadTimeout and no WriteTimeout. ReadTimeout caps the whole
// read of a request, headers and body, from its first byte: it would cut a
// slow but moving upload (a full-size documentation attachment envelope over a
// slow link) however steadily it arrives. WriteTimeout counts the handler's
// own time, so it would cut a long answer (a prior authorization's wait plus
// its backend call). On the observer, an event stream, either would end the
// stream. The main listener bounds a request's body and its answer by
// progress instead (boundRequests).
//
// HTTP/2 stays at net/http's defaults, its concurrent stream limit among them
// (TestListenerBounds_H2Settings), but for one field on the main listener:
// WriteByteTimeout at writeStall (boundMainServer). A stream's own write
// deadline only queues a reset, which cannot leave while the connection's
// frame writer is blocked on a client that stopped reading at TCP; the
// connection's write progress bound ends that connection.
type bounds struct {
	readHeader time.Duration
	maxHeader  int
	idle       time.Duration
	// bodyStall is how long a request body may send no byte; bodyCeiling
	// how long it may take in all.
	bodyStall   time.Duration
	bodyCeiling time.Duration
	// writeStall is how long a client may take no piece (writePiece) of the
	// answer.
	writeStall time.Duration
}

// defaultBounds are the main and observer listeners' bounds.
func defaultBounds() bounds {
	return bounds{
		readHeader:  10 * time.Second,
		maxHeader:   64 << 10,
		idle:        120 * time.Second,
		bodyStall:   30 * time.Second,
		bodyCeiling: 10 * time.Minute,
		writeStall:  30 * time.Second,
	}
}

// The listeners and bounds a cut names (telemetry.Listeners,
// telemetry.ListenerBounds).
const (
	listenerMain     = "main"
	listenerObserver = "observer"

	boundHeader            = "header"
	boundBodyStall         = "body-stall"
	boundBodyCeiling       = "body-ceiling"
	boundWriteStall        = "write-stall"
	boundAttachmentArrival = "attachment-arrival"
)

// cuts records a listener's cuts: one count of shn.gateway.listener.cuts and
// one log line each. The line names the bound and the listener only: no
// request data, path, header or remote address.
type cuts struct {
	reg      *telemetry.Registry // nil: the count is not recorded
	out      io.Writer           // nil: no line
	listener string
	// b replaces defaultBounds, in tests only; nil in each listener Run
	// starts.
	b *bounds
}

// bounds are the listener's bounds.
func (c *cuts) bounds() bounds {
	if c == nil || c.b == nil {
		return defaultBounds()
	}
	return *c.b
}

func (c *cuts) cut(bound string) {
	if c == nil {
		return
	}
	c.reg.Add(telemetry.ListenerCuts, 1, bound, c.listener)
	if c.out != nil {
		fmt.Fprintf(c.out, "gateway: listener: cut a connection at its %s bound (listener=%s)\n", bound, c.listener)
	}
}

// boundServer gives s the server-level bounds and tracks its connections, so
// a header cut is counted.
func boundServer(s *http.Server, c *cuts) *http.Server {
	b := c.bounds()
	s.ReadHeaderTimeout = b.readHeader
	s.MaxHeaderBytes = b.maxHeader
	s.IdleTimeout = b.idle
	s.ConnContext = func(ctx context.Context, nc net.Conn) context.Context {
		if tc := trackedOf(nc); tc != nil {
			return context.WithValue(ctx, trackedConnKey{}, tc)
		}
		return ctx
	}
	s.ConnState = func(nc net.Conn, st http.ConnState) {
		tc := trackedOf(nc)
		if tc == nil {
			return
		}
		// Over HTTP/2 the hook reports the connection's streams, not one
		// request: StateIdle comes after the preface and whenever the last
		// stream closes, our handler's or one net/http answered itself. The
		// connection's write deadline is then WriteByteTimeout's, on a frame
		// write that may be blocked, and stays. Decided from the connection,
		// so it holds before any request reaches a handler.
		if tl, ok := nc.(*tls.Conn); ok && tl.ConnectionState().NegotiatedProtocol == "h2" {
			tc.h2.Store(true)
			if st == http.StateActive {
				tc.served.Store(true)
			}
			return
		}
		switch st {
		case http.StateActive:
			tc.served.Store(true)
			tc.inRequest.Store(true)
		case http.StateIdle:
			// The request is finished, its final flush included: what
			// arrives from here belongs to the next request, and no write
			// deadline of the last one may reach a write net/http makes
			// before the next handler runs.
			tc.readBound.Store(nil)
			tc.idleBytes.Store(0)
			tc.inRequest.Store(false)
			_ = nc.SetWriteDeadline(time.Time{})
		}
	}
	return s
}

// boundMainServer gives the main listener's server the server-level bounds
// and its one departure from net/http's HTTP/2 defaults: the connection's
// write progress bound, writeStall. A frame write that makes no progress for
// that long ends the connection, which the tracked connection counts as a
// write-stall cut.
func boundMainServer(s *http.Server, c *cuts) *http.Server {
	s.HTTP2 = &http.HTTP2Config{WriteByteTimeout: c.bounds().writeStall}
	return boundServer(s, c)
}

// observerServer builds the observer listener's server: the loopback event
// stream, plain HTTP, with the server-level bounds. Every answer it serves
// carries X-Content-Type-Options: nosniff, as the main listener's does.
func observerServer(addr string, h http.Handler, c *cuts) *http.Server {
	return boundServer(&http.Server{Addr: addr, Handler: nosniff.Handler(h)}, c)
}

// listenBounded opens addr's listener for s, its connections tracked for c.
func listenBounded(s *http.Server, c *cuts) (net.Listener, error) {
	addr := s.Addr
	if addr == "" {
		addr = ":http"
		if s.TLSConfig != nil {
			addr = ":https"
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &boundListener{Listener: ln, cuts: c}, nil
}

// serveBounded serves s on ln: over TLS exactly as before (the keypair in
// s.TLSConfig, HTTP/2 negotiated), else plain.
func serveBounded(s *http.Server, ln net.Listener) error {
	if s.TLSConfig != nil {
		return s.ServeTLS(ln, "", "")
	}
	return s.Serve(ln)
}

// boundListener tracks every connection it accepts.
type boundListener struct {
	net.Listener
	cuts *cuts
}

func (l *boundListener) Accept() (net.Conn, error) {
	nc, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &trackedConn{Conn: nc, cuts: l.cuts}, nil
}

type trackedConnKey struct{}

// trackedOf is nc's tracked connection, under TLS the one beneath it.
func trackedOf(nc net.Conn) *trackedConn {
	if tc, ok := nc.(*tls.Conn); ok {
		nc = tc.NetConn()
	}
	t, _ := nc.(*trackedConn)
	return t
}

// trackedConn classifies its raw read and write timeouts, counting each
// connection's cut once. Every read deadline set on the connection passes
// through it (under TLS, tls.Conn sets the one beneath), so it knows which
// deadline a timeout is.
//
// A read timeout is a body bound's cut when the deadline in force is exactly
// the one that bound set (readBound) and it has passed: net/http's own
// deadlines (the header and idle bounds, the past deadline that ends its
// background read, the clear that starts it) never match one. Otherwise it is
// a header cut when the connection is fresh (its TLS handshake included) or
// has read part of a next request whose handler has not started; with no byte
// of a next request it is the idle close, not counted. Bytes of a pipelined
// next request read while the last one was in progress are not seen as the
// next request's, so a stall in such a request is taken for the idle close.
//
// A write timeout on a fresh connection is the TLS handshake's (a header
// cut). On a served one it is a write-stall only when the deadline in force
// is ours: over HTTP/1.1 the one boundWriter recorded (ourWrite), over HTTP/2
// WriteByteTimeout's, told by its span (writeStall from when it was set).
// Any other, such as crypto/tls's own deadline on its close_notify, is not a
// cut of ours.
type trackedConn struct {
	net.Conn
	cuts *cuts

	active    atomic.Int32 // HTTP/1.1 handlers running (boundRequests)
	served    atomic.Bool  // a request has started on it
	inRequest atomic.Bool  // between a request's headers and its end
	idleBytes atomic.Int64 // bytes read since the last request ended
	// readAt is the read deadline in force, in Unix nanoseconds; 0 for none.
	readAt atomic.Int64
	// readBound is the request body's bound over HTTP/1.1, and the read
	// deadline it set; nil when none is.
	readBound atomic.Pointer[readBound]
	h2        atomic.Bool // it negotiated HTTP/2 (set by the ConnState hook)
	// writeAt is the write deadline in force, in Unix nanoseconds; 0 for
	// none. writeSetAt is when it was set; ourWrite is the last write
	// deadline boundWriter set, over HTTP/1.1.
	writeAt    atomic.Int64
	writeSetAt atomic.Int64
	ourWrite   atomic.Int64
	done       atomic.Bool // a cut is counted
	// closeWrites counts the half-closes passed through (CloseWrite).
	closeWrites atomic.Int32
}

// readBound is a body bound and the read deadline it set. A discard bound
// (net/http discarding a body the handler left unread) is on progress: each
// read that brings bytes moves it forward by bodyStall.
type readBound struct {
	name    string
	at      time.Time
	discard bool
	// ceiling is a discard bound's overall ceiling: progress moves it
	// forward up to there and no further.
	ceiling time.Time
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func (c *trackedConn) SetReadDeadline(t time.Time) error {
	c.readAt.Store(unixNano(t))
	return c.Conn.SetReadDeadline(t)
}

func (c *trackedConn) SetDeadline(t time.Time) error {
	c.readAt.Store(unixNano(t))
	c.recordWrite(t)
	return c.Conn.SetDeadline(t)
}

func (c *trackedConn) SetWriteDeadline(t time.Time) error {
	c.recordWrite(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *trackedConn) recordWrite(t time.Time) {
	c.writeSetAt.Store(time.Now().UnixNano())
	c.writeAt.Store(unixNano(t))
}

// setOurWrite sets a write deadline of ours on the connection, through rc.
func (c *trackedConn) setOurWrite(rc *http.ResponseController, t time.Time) {
	c.ourWrite.Store(unixNano(t))
	_ = rc.SetWriteDeadline(t)
}

// oursTimedOut is whether the write deadline in force is ours (see
// trackedConn).
func (c *trackedConn) oursTimedOut() bool {
	at := c.writeAt.Load()
	if at == 0 {
		return false
	}
	if at == c.ourWrite.Load() {
		return true
	}
	if !c.h2.Load() {
		return false
	}
	// One-sided and wide: under load the deadline is set a little after
	// its base time was read, never before, so the span only shrinks, by up
	// to half of writeStall here. It excludes crypto/tls's own 5 s close
	// deadline when writeStall is at least 10 s (30 s), or under 5 s by more
	// than any scheduling delay (the tests' 2 s and 400 ms)
	// (TestListenerBounds_AttributionWindow).
	ws := c.cuts.bounds().writeStall
	diff := time.Duration(at-c.writeSetAt.Load()) - ws
	return diff > -ws/2 && diff <= 5*time.Millisecond
}

func (c *trackedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.active.Load() == 0 && !c.inRequest.Load() {
		c.idleBytes.Add(int64(n))
	}
	if n > 0 {
		c.discardProgress()
	}
	if err != nil && isTimeout(err) {
		c.readTimedOut()
	}
	return n, err
}

func (c *trackedConn) readTimedOut() {
	if b := c.readBound.Load(); b != nil && c.readAt.Load() == b.at.UnixNano() && !time.Now().Before(b.at) {
		c.cutOnce(b.name)
		return
	}
	if c.active.Load() > 0 || c.inRequest.Load() {
		return
	}
	if !c.served.Load() || c.idleBytes.Load() > 0 {
		c.cutOnce(boundHeader)
	}
}

// discardProgress moves a discard bound in force forward by bodyStall from
// now, and with it the write deadline of a write it holds up (net/http
// discards inside the answer's first write), by writeStall from now.
func (c *trackedConn) discardProgress() {
	b := c.readBound.Load()
	if b == nil || !b.discard || c.readAt.Load() != b.at.UnixNano() {
		return
	}
	bd := c.cuts.bounds()
	now := time.Now()
	at, name := drainDeadline(now, bd.bodyStall, b.ceiling)
	next := &readBound{name: name, at: at, discard: true, ceiling: b.ceiling}
	if !c.readBound.CompareAndSwap(b, next) {
		return
	}
	_ = c.SetReadDeadline(next.at)
	if c.writeAt.Load() != 0 {
		w := now.Add(bd.writeStall)
		c.ourWrite.Store(w.UnixNano())
		_ = c.SetWriteDeadline(w)
	}
}

// drainDeadline is a discard bound's deadline at now: bodyStall from now,
// but never past its ceiling (and then it is the ceiling's bound).
func drainDeadline(now time.Time, stall time.Duration, ceiling time.Time) (time.Time, string) {
	if at := now.Add(stall); at.Before(ceiling) {
		return at, boundBodyStall
	}
	return ceiling, boundBodyCeiling
}

func (c *trackedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil && isTimeout(err) {
		switch {
		case !c.served.Load():
			c.cutOnce(boundHeader)
		case c.oursTimedOut():
			c.cutOnce(boundWriteStall)
		}
	}
	return n, err
}

// CloseWrite passes a half-close through to the connection beneath, so
// net/http's closeWriteAndWait still sends it.
func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		c.closeWrites.Add(1)
		return cw.CloseWrite()
	}
	return nil
}

// setReadBound records bound's deadline at, then sets it.
func (c *trackedConn) setReadBound(rc *http.ResponseController, at time.Time, bound string) {
	if bound == "" {
		c.readBound.Store(nil)
	} else {
		c.readBound.Store(&readBound{name: bound, at: at})
	}
	_ = rc.SetReadDeadline(at)
}

// setDrainBound bounds net/http's discard of an unread body from now: by
// progress (drainDeadline) up to ceiling.
func (c *trackedConn) setDrainBound(rc *http.ResponseController, now time.Time, stall time.Duration, ceiling time.Time) time.Time {
	at, name := drainDeadline(now, stall, ceiling)
	c.readBound.Store(&readBound{name: name, at: at, discard: true, ceiling: ceiling})
	_ = rc.SetReadDeadline(at)
	return at
}

// cutOnce counts the connection's cut, if none is counted yet.
func (c *trackedConn) cutOnce(bound string) {
	if c.done.CompareAndSwap(false, true) {
		c.cuts.cut(bound)
	}
}

// isTimeout is a deadline's error.
func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// boundRequests bounds each request on the main listener by progress: its
// body must keep arriving (bodyStall) and finish within bodyCeiling, and its
// client must keep taking the answer (writeStall). The deadlines go through
// http.ResponseController, so they hold over HTTP/1.1 and HTTP/2 alike.
//
// A handler that sets its own read deadline (a documentation attachment
// submission's arrival bound) governs the body until it clears it; a timeout
// then is an attachment-arrival cut.
func boundRequests(h http.Handler, c *cuts) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc, _ := r.Context().Value(trackedConnKey{}).(*trackedConn)
		bw := &boundWriter{ResponseWriter: w, rc: http.NewResponseController(w), b: c.bounds(), h2: r.ProtoMajor >= 2, tc: tc, cuts: c}
		if tc != nil {
			tc.served.Store(true)
			if !bw.h2 {
				tc.active.Add(1)
			}
		}
		// No write deadline of an earlier request on this connection may
		// reach this one's writes.
		_ = bw.rc.SetWriteDeadline(time.Time{})
		var body *boundBody
		if r.Body != nil && r.Body != http.NoBody {
			body = &boundBody{ReadCloser: r.Body, w: bw, start: time.Now()}
			bw.body = body
			// The handler gets a shallow copy carrying the bounded body:
			// net/http's own request keeps its own body, whose type decides
			// what it does with a body the handler left unread (close after
			// an unanswered 100-continue or a large remainder, else discard).
			r2 := *r
			r2.Body = body
			r = &r2
		}
		defer func() {
			// net/http's own final flush of the answer gets a fresh
			// bound, not one left from before a long pause; when a body is
			// left to discard first, that discard's bound on top.
			// Over HTTP/2 a stream whose context has ended gets no
			// deadline. A stream can still end after this check (the
			// deadline and its last frame reach Go's HTTP/2 server
			// separately), and then a fresh timer resets the closed stream
			// once that deadline passes; clients ignore that reset
			// (MONITORING.md).
			if !bw.h2 || r.Context().Err() == nil {
				flush := bw.b.writeStall
				if body != nil && body.handlerReturned() {
					flush += bw.b.bodyStall
				}
				if tc := bw.h1(); tc != nil {
					tc.setOurWrite(bw.rc, time.Now().Add(flush))
				} else {
					_ = bw.rc.SetWriteDeadline(time.Now().Add(flush))
				}
			}
			if tc != nil && !bw.h2 {
				tc.active.Add(-1)
			}
		}()
		h.ServeHTTP(bw, r)
	})
}

// boundWriter is the main listener's answer writer: each write is bounded by
// writeStall, and a handler's own read deadline (ResponseController's
// SetReadDeadline) reaches the body through it.
type boundWriter struct {
	http.ResponseWriter
	rc   *http.ResponseController // the writer beneath
	b    bounds
	h2   bool
	tc   *trackedConn
	cuts *cuts
	body *boundBody

	// handlerDeadline is the read deadline the handler set, in Unix
	// nanoseconds; 0 when it has set none, or cleared it.
	handlerDeadline atomic.Int64
	cut             atomic.Bool // this request's cut is counted
}

// handlerSet is the handler's own read deadline, and whether it has one.
func (w *boundWriter) handlerSet() (time.Time, bool) {
	ns := w.handlerDeadline.Load()
	if ns == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

// h1 is the request's tracked HTTP/1.1 connection, nil over HTTP/2 or when
// the connection is not tracked.
func (w *boundWriter) h1() *trackedConn {
	if w.h2 {
		return nil
	}
	return w.tc
}

// setRead sets the body's read deadline, over HTTP/1.1 recording which bound
// it is so the connection can count its timeout.
func (w *boundWriter) setRead(at time.Time, bound string) {
	if tc := w.h1(); tc != nil {
		tc.setReadBound(w.rc, at, bound)
		return
	}
	_ = w.rc.SetReadDeadline(at)
}

// Unwrap lets http.ResponseController reach the writer beneath for what
// boundWriter does not implement itself.
func (w *boundWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// SetReadDeadline is a handler's own read deadline: a non-zero t governs the
// body until a zero t ends it.
func (w *boundWriter) SetReadDeadline(t time.Time) error {
	w.handlerDeadline.Store(unixNano(t))
	bound := ""
	if !t.IsZero() && w.body != nil && !w.body.eof.Load() {
		bound = boundAttachmentArrival
	}
	if tc := w.h1(); tc != nil {
		tc.setReadBound(w.rc, t, bound)
		return nil
	}
	return w.rc.SetReadDeadline(t)
}

func (w *boundWriter) WriteHeader(code int) {
	arm := w.armWrite()
	w.ResponseWriter.WriteHeader(code)
	w.disarmWrite(nil, arm)
}

// writePiece is the most one write deadline covers: the bound is on
// progress, so a large write (the engine writes a whole answer in one) is
// made a piece at a time, each with a fresh deadline. An answer is cut when
// its client takes no piece of it within writeStall.
const writePiece = 8 << 10

func (w *boundWriter) Write(p []byte) (int, error) {
	total := 0
	for {
		piece := p[:min(len(p), writePiece)]
		arm := w.armWrite()
		n, err := w.ResponseWriter.Write(piece)
		w.disarmWrite(err, arm)
		total += n
		p = p[n:]
		if err != nil || len(p) == 0 {
			return total, err
		}
		if n < len(piece) {
			return total, io.ErrShortWrite
		}
	}
}

func (w *boundWriter) Flush() { _ = w.FlushError() }

func (w *boundWriter) FlushError() error {
	arm := w.armWrite()
	err := w.rc.Flush()
	w.disarmWrite(err, arm)
	return err
}

// armed is what armWrite set: the write deadline, and over HTTP/1.1 the read
// deadline it gave a body net/http may discard during the write (zero when
// it set none).
type armed struct {
	write, read time.Time
}

// armWrite puts writeStall on the write about to be made. The deadline is
// cleared again after the write: over HTTP/2 a stream's write deadline is a
// timer that would reset the stream while the handler is merely thinking.
//
// Over HTTP/1.1, a write of the answer's first 2 KiB or more makes net/http
// discard a body the handler has not read, inside the write: while the body
// is unfinished and the handler has set no read deadline of its own, the
// write also bounds that discard by bodyStall.
func (w *boundWriter) armWrite() armed {
	now := time.Now()
	a := armed{write: now.Add(w.b.writeStall)}
	tc := w.h1()
	if tc != nil {
		tc.setOurWrite(w.rc, a.write)
	} else {
		_ = w.rc.SetWriteDeadline(a.write)
	}
	if tc != nil && w.body != nil && !w.body.eof.Load() {
		if _, handler := w.handlerSet(); !handler {
			if tc.done.Load() {
				// The body is cut already: its client is not held again.
				_ = w.rc.SetReadDeadline(now)
				return a
			}
			a.read = tc.setDrainBound(w.rc, now, w.b.bodyStall, w.body.drainCeiling(now))
		}
	}
	return a
}

// disarmWrite clears the write deadline and puts back the body's ceiling.
// When the read deadline in force is no longer the one armWrite set, net/http
// has discarded the body itself and taken over the connection's reads (its
// background read): the body is finished, and nothing of ours bounds a read
// again.
//
// Over HTTP/2 a write that the stream's own deadline ended is counted here:
// the stream's reset reaches its writes, as os.ErrDeadlineExceeded, only once
// the reset frame is written, so only while the connection still takes
// bytes. A connection that stops taking them is cut by its write progress
// bound first, counted once by the connection (done), and its streams then
// end with another error. Other streams writing on the connection do not
// hide a stalled one.
//
// Known limit, for the author of a handler that sets its own read deadline
// (as the documentation attachment path does): while that deadline is in
// force, armWrite arms no read bound, so this hand-over is not seen. If such
// a handler writes 2 KiB or more (or flushes) before reading its body to the
// end, net/http's discard inside that write can finish the body unseen; if
// the handler then clears its deadline and returns, handlerReturned puts a
// drain deadline on net/http's background read. That deadline matters only
// if the final flush also stalls past it, and then the stall is counted
// body-stall rather than write-stall. No handler does this today; one that
// would should read its body to the end, or keep its deadline, before
// writing that much.
func (w *boundWriter) disarmWrite(err error, a armed) {
	if err != nil && w.h2 && errors.Is(err, os.ErrDeadlineExceeded) {
		if tc := w.tc; tc == nil || !tc.done.Load() {
			w.cutOnce(boundWriteStall)
		}
	}
	_ = w.rc.SetWriteDeadline(time.Time{})
	if tc := w.h1(); tc != nil && !a.read.IsZero() {
		if b := tc.readBound.Load(); b != nil && tc.readAt.Load() == b.at.UnixNano() && b.discard {
			tc.setReadBound(w.rc, w.body.start.Add(w.b.bodyCeiling), boundBodyCeiling)
		} else {
			w.body.eof.Store(true)
			tc.readBound.Store(nil)
		}
	}
}

// cutOnce counts this request's cut once (over HTTP/2: a stream's).
func (w *boundWriter) cutOnce(bound string) {
	if w.cut.CompareAndSwap(false, true) {
		w.cuts.cut(bound)
	}
}

// boundBody is a request body bounded by progress: before each read its
// deadline is the sooner of bodyStall from now and bodyCeiling from the
// request's start; between reads only the ceiling holds (over HTTP/2 a
// stream's read deadline is a timer that would fire while the handler is not
// reading). At its end the deadline is cleared: no read after the body's end
// is this bound's (over HTTP/1.1 net/http's background read clears it
// itself; over HTTP/2 this stops the stream's timer). Over HTTP/1.1 the
// connection counts a cut (trackedConn); over HTTP/2 the body does.
type boundBody struct {
	io.ReadCloser
	w     *boundWriter
	start time.Time
	eof   atomic.Bool
}

func (b *boundBody) Read(p []byte) (int, error) {
	hdl, handler := b.w.handlerSet()
	own := !handler && !b.eof.Load()
	dl, bound := hdl, boundAttachmentArrival
	if own {
		dl, bound = b.deadline(time.Now())
		b.w.setRead(dl, bound)
	}
	n, err := b.ReadCloser.Read(p)
	switch {
	case err == io.EOF:
		b.eof.Store(true)
		b.w.setRead(time.Time{}, "")
	case err != nil && b.w.h2 && !dl.IsZero() && !time.Now().Before(dl):
		// A cut only once the deadline this request's bound set has
		// passed; any other error is the client's or net/http's own.
		b.w.cutOnce(bound)
	case err == nil && own:
		b.w.setRead(b.start.Add(b.w.b.bodyCeiling), boundBodyCeiling)
	}
	return n, err
}

// deadline is the body's deadline before a read at now, and the bound it is.
func (b *boundBody) deadline(now time.Time) (time.Time, string) {
	stall, ceiling := now.Add(b.w.b.bodyStall), b.start.Add(b.w.b.bodyCeiling)
	if ceiling.Before(stall) {
		return ceiling, boundBodyCeiling
	}
	return stall, boundBodyStall
}

// handlerReturned bounds what net/http still reads of an unfinished body
// after the handler (it discards a small remainder before reusing the
// connection), and reports whether the body is unfinished. A request already
// cut ends at once; otherwise the drain bound holds from now (drainDeadline:
// bodyStall from now, up to the drain's ceiling, which is never sooner than
// bodyStall from now; named body-ceiling when the ceiling is the deadline),
// unless the handler's own deadline still governs. Without this, a handler that cleared
// its own deadline after a timeout would leave that read unbounded.
func (b *boundBody) handlerReturned() bool {
	if b.eof.Load() {
		return false
	}
	b.armDrain(true)
	return true
}

// armDrain bounds a discard of the unfinished body by net/http: a request
// already cut ends at once; otherwise, unless the handler's own deadline
// governs, the drain bound holds from now (progress up to drainCeiling). Over
// HTTP/2 there is no discard; afterHandler gives the stream a stall bound on
// what is left.
func (b *boundBody) armDrain(afterHandler bool) {
	_, handler := b.w.handlerSet()
	cut := b.w.cut.Load()
	tc := b.w.h1()
	if tc != nil {
		cut = tc.done.Load()
	}
	switch {
	case cut:
		_ = b.w.rc.SetReadDeadline(time.Now())
	case handler:
	case tc != nil:
		now := time.Now()
		tc.setDrainBound(b.w.rc, now, b.w.b.bodyStall, b.drainCeiling(now))
	case afterHandler:
		_ = b.w.rc.SetReadDeadline(time.Now().Add(b.w.b.bodyStall))
	}
}

// Close of an unfinished body makes net/http discard up to 256 KiB of the
// rest over HTTP/1.1, inside the handler: that discard is bounded like the
// one after the handler. The body is finished after it: no bound of ours
// reaches a read again (net/http's background read follows).
func (b *boundBody) Close() error {
	if !b.eof.Load() {
		b.armDrain(false)
	}
	err := b.ReadCloser.Close()
	if !b.eof.Swap(true) {
		if tc := b.w.h1(); tc != nil {
			tc.readBound.Store(nil)
		}
	}
	return err
}

// drainCeiling is the overall ceiling of a discard of the body by net/http
// that starts at now: the body's own ceiling, or for a handler that ran past
// it, bodyStall from now. Each discard bound carries its ceiling from when it
// is armed, and progress moves the bound up to it (discardProgress); a write
// that starts no discard leaves no ceiling behind.
func (b *boundBody) drainCeiling(now time.Time) time.Time {
	c := b.start.Add(b.w.b.bodyCeiling)
	if s := now.Add(b.w.b.bodyStall); s.After(c) {
		return s
	}
	return c
}
