package diagnostics

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type CaptureBudget struct {
	mu                  sync.Mutex
	maxBytes, usedBytes int64
	slots               chan struct{}
}

var defaultCaptureBudget = NewCaptureBudget(16<<20, 8)

const maxCapturedHeaderBytes int64 = 64 << 10

func NewCaptureBudget(maxBytes int64, maxConcurrent int) *CaptureBudget {
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 8
	}
	return &CaptureBudget{maxBytes: maxBytes, slots: make(chan struct{}, maxConcurrent)}
}
func (b *CaptureBudget) acquireSlot() bool {
	select {
	case b.slots <- struct{}{}:
		return true
	default:
		return false
	}
}
func (b *CaptureBudget) releaseSlot() { <-b.slots }
func (b *CaptureBudget) reserve(want, capLeft int64) (int64, bool) {
	if capLeft <= 0 {
		return 0, want == 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n := want
	if n > capLeft {
		n = capLeft
	}
	if n > b.maxBytes-b.usedBytes {
		n = b.maxBytes - b.usedBytes
	}
	if n < 0 {
		n = 0
	}
	b.usedBytes += n
	return n, n == want
}
func (b *CaptureBudget) release(n int64) {
	b.mu.Lock()
	b.usedBytes -= n
	if b.usedBytes < 0 {
		b.usedBytes = 0
	}
	b.mu.Unlock()
}

type HTTPInfo struct {
	CallID string
	Kind   string
}
type requestIdentity struct{ hash, sender, recipient, correlation string }
type contextKey uint8

const (
	callIDKey contextKey = iota
	identityKey
	ingressKey
	ingressBodyKey
)

func WithCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callIDKey, id)
}
func CallID(ctx context.Context) string { v, _ := ctx.Value(callIDKey).(string); return v }
func WithRequestIdentity(ctx context.Context, requestHash, sender, recipient, correlation string) context.Context {
	return context.WithValue(ctx, identityKey, requestIdentity{requestHash, sender, recipient, correlation})
}

// IngressFingerprint reads the body hash observed so far without consuming bytes.
// Complete remains false until the wrapped handler has consumed the whole body.
func IngressFingerprint(ctx context.Context) RequestFingerprint {
	f, _ := ctx.Value(ingressKey).(func() RequestFingerprint)
	if f == nil {
		return RequestFingerprint{}
	}
	return f()
}

// IngressBody returns a read-only view of the bounded body consumed so far.
// Inspect it synchronously during handling; retaining bytes requires a bounded copy.
func IngressBody(ctx context.Context) []byte {
	f, _ := ctx.Value(ingressBodyKey).(func() []byte)
	if f == nil {
		return nil
	}
	return f()
}

// RequestIdentity adds only independently verified envelope attribution.
func RequestIdentity(ctx context.Context, e Event) Event {
	id, _ := ctx.Value(identityKey).(requestIdentity)
	e.RequestCiphertextHash, e.Sender, e.Recipient, e.CorrelationID = id.hash, id.sender, id.recipient, id.correlation
	e.CallID = CallID(ctx)
	return e
}

type captureState struct {
	mu        sync.Mutex
	budget    *CaptureBudget
	cap       int64
	held      int64
	data      []byte
	complete  bool
	available bool
	hash      hash.Hash
	observed  int64
	failed    bool
	closed    bool
	released  bool
	truncated bool
}

func (s *captureState) add(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if len(p) == 0 {
		return
	}
	s.observed += int64(len(p))
	_, _ = s.hash.Write(p)
	if !s.available || s.truncated {
		return
	}
	s.grow(int64(len(p)))
	n := int64(len(p))
	remaining := int64(cap(s.data) - len(s.data))
	if n > remaining {
		n = remaining
	}
	s.data = append(s.data, p[:n]...)
	if n < int64(len(p)) {
		// Once a byte is missed nothing later is kept, so a partial capture is
		// always a prefix of the body and never joins bytes that were apart.
		s.complete = false
		s.truncated = true
	}
}

type captureSnapshot struct {
	data     []byte
	observed int64
	complete bool
	digest   string
}

// snapshot copies the captured prefix and its accounting under the lock.
func (s *captureState) snapshot() captureSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return captureSnapshot{data: bytes.Clone(s.data), observed: s.observed, complete: s.complete, digest: hex.EncodeToString(s.hash.Sum(nil))}
}

// grow reserves room for n more body bytes as they arrive, never more than the
// body cap, so concurrent exchanges share the budget by the bytes they actually
// carry. Capacity grows geometrically and the reservation always equals the
// current allocation; the array a growth step replaces is not counted, so
// transient heap use can reach about twice the reserved bytes. A released
// state reserves nothing.
func (s *captureState) grow(n int64) {
	have := int64(cap(s.data))
	need := int64(len(s.data)) + n
	if need > s.cap {
		need = s.cap
	}
	if s.released || need <= have {
		return
	}
	want := 2 * have
	if want < need {
		want = need
	}
	if want > s.cap {
		want = s.cap
	}
	got, _ := s.budget.reserve(want-have, s.cap-have)
	if got == 0 {
		return
	}
	s.held += got
	data := make([]byte, len(s.data), have+got)
	copy(data, s.data)
	s.data = data
}
func (s *captureState) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.available && !s.released {
		s.budget.release(s.held)
		s.held = 0
	}
	s.released = true
}
func captureHeader(s *captureState, h http.Header) (http.Header, bool) {
	if !s.available || h == nil {
		return nil, h == nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return nil, false
	}
	out := make(http.Header)
	var used int64
	for k, vs := range h {
		cost := int64(mapEntryCost + sliceEntryCost + len(k))
		for _, v := range vs {
			cost += sliceEntryCost + int64(len(v))
		}
		if used+cost > maxCapturedHeaderBytes {
			return out, false
		}
		n, all := s.budget.reserve(cost, maxCapturedHeaderBytes-used)
		if !all {
			s.held += n
			return out, false
		}
		s.held += n
		used += n
		copied := make([]string, len(vs))
		for i, v := range vs {
			copied[i] = strings.Clone(v)
		}
		out[strings.Clone(k)] = copied
	}
	return out, true
}

type observedReadCloser struct {
	io.ReadCloser
	s             *captureState
	contentLength int64
	onClose       func()
}

func (r *observedReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.s.add(p[:n])
	}
	r.s.mu.Lock()
	// An error can accompany the final bytes of a known-length body. The
	// observed bytes are complete when they match the declared wire length;
	// the error must still be returned to the handler unchanged.
	if err != nil && err != io.EOF && !(r.contentLength > 0 && r.s.observed == r.contentLength) {
		r.s.failed = true
		r.s.complete = false
	}
	if err == io.EOF && !r.s.failed {
		r.s.complete = true
	}
	if r.contentLength > 0 && r.s.observed == r.contentLength {
		r.s.failed = false
		r.s.complete = true
	}
	r.s.mu.Unlock()
	return n, err
}
func (r *observedReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.s.mu.Lock()
	if err != nil && !r.s.complete {
		r.s.failed = true
		r.s.complete = false
	}
	r.s.closed = true
	r.s.mu.Unlock()
	if r.onClose != nil {
		r.onClose()
		r.onClose = nil
	}
	return err
}

type observedWriter struct {
	base            http.ResponseWriter
	status          int
	headers         http.Header
	headersComplete bool
	s               *captureState
	writeFailed     bool
}

func (w *observedWriter) Header() http.Header { return w.base.Header() }

// The snapshot below is the header committed by the handler. Headers generated
// below ResponseWriter (for example, content sniffing) are visible only to the
// transport-side observer and are never inferred here.
func (w *observedWriter) WriteHeader(code int) {
	if (code == http.StatusSwitchingProtocols || code >= 200) && w.status == 0 {
		w.status = code
		w.headers, w.headersComplete = captureHeader(w.s, w.base.Header())
	}
	w.base.WriteHeader(code)
}
func (w *observedWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
		w.headers, w.headersComplete = captureHeader(w.s, w.base.Header())
	}
	n, err := w.base.Write(p)
	if n > 0 {
		w.s.add(p[:n])
	}
	if err != nil || n < len(p) {
		w.writeFailed = true
	}
	return n, err
}
func (w *observedWriter) Unwrap() http.ResponseWriter { return w.base }

type flusherCap struct{ core *observedWriter }

func (c flusherCap) Flush() {
	_ = c.FlushError()
}
func (c flusherCap) FlushError() error {
	if c.core.status == 0 {
		c.core.status = 200
		c.core.headers, c.core.headersComplete = captureHeader(c.core.s, c.core.base.Header())
	}
	if f, ok := c.core.base.(interface{ FlushError() error }); ok {
		return f.FlushError()
	}
	c.core.base.(http.Flusher).Flush()
	return nil
}

type hijackerCap struct{ core *observedWriter }

func (c hijackerCap) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return c.core.base.(http.Hijacker).Hijack()
}

type pusherCap struct{ core *observedWriter }

func (c pusherCap) Push(s string, o *http.PushOptions) error {
	return c.core.base.(http.Pusher).Push(s, o)
}

type readerFromCap struct{ core *observedWriter }

func (c readerFromCap) ReadFrom(r io.Reader) (int64, error) {
	c.core.s.mu.Lock()
	beforeObserved := c.core.s.observed
	beforeLen := len(c.core.s.data)
	c.core.s.mu.Unlock()
	cr := &captureReader{r: r, s: c.core.s}
	n, err := c.core.base.(io.ReaderFrom).ReadFrom(cr)
	c.core.s.mu.Lock()
	suffix := len(c.core.s.data) - beforeLen
	keep := n
	if keep > int64(suffix) {
		keep = int64(suffix)
	}
	if keep < 0 {
		keep = 0
	}
	c.core.s.data = c.core.s.data[:beforeLen+int(keep)]
	c.core.s.observed = beforeObserved + n
	c.core.s.mu.Unlock()
	// A delegated ReaderFrom may commit details below ResponseWriter. As with
	// generated headers, this observer records only the handler-visible commit:
	// a positive reported write implies 200; an empty call commits nothing.
	if n > 0 && c.core.status == 0 {
		c.core.status = 200
		c.core.headers, c.core.headersComplete = captureHeader(c.core.s, c.core.base.Header())
	}
	if err != nil {
		c.core.writeFailed = true
	}
	return n, err
}

type captureReader struct {
	r io.Reader
	s *captureState
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.s.add(p[:n])
	}
	return n, err
}

func wrapWriter(core *observedWriter) http.ResponseWriter {
	f := false
	h := false
	p := false
	rf := false
	_, f = core.base.(http.Flusher)
	_, h = core.base.(http.Hijacker)
	_, p = core.base.(http.Pusher)
	_, rf = core.base.(io.ReaderFrom)
	switch { // Dynamic shapes preserve the underlying optional interface set.
	case f && h && p && rf:
		return struct {
			*observedWriter
			flusherCap
			hijackerCap
			pusherCap
			readerFromCap
		}{core, flusherCap{core}, hijackerCap{core}, pusherCap{core}, readerFromCap{core}}
	case f && h && p:
		return struct {
			*observedWriter
			flusherCap
			hijackerCap
			pusherCap
		}{core, flusherCap{core}, hijackerCap{core}, pusherCap{core}}
	case f && h && rf:
		return struct {
			*observedWriter
			flusherCap
			hijackerCap
			readerFromCap
		}{core, flusherCap{core}, hijackerCap{core}, readerFromCap{core}}
	case f && p && rf:
		return struct {
			*observedWriter
			flusherCap
			pusherCap
			readerFromCap
		}{core, flusherCap{core}, pusherCap{core}, readerFromCap{core}}
	case h && p && rf:
		return struct {
			*observedWriter
			hijackerCap
			pusherCap
			readerFromCap
		}{core, hijackerCap{core}, pusherCap{core}, readerFromCap{core}}
	case f && h:
		return struct {
			*observedWriter
			flusherCap
			hijackerCap
		}{core, flusherCap{core}, hijackerCap{core}}
	case f && p:
		return struct {
			*observedWriter
			flusherCap
			pusherCap
		}{core, flusherCap{core}, pusherCap{core}}
	case f && rf:
		return struct {
			*observedWriter
			flusherCap
			readerFromCap
		}{core, flusherCap{core}, readerFromCap{core}}
	case h && p:
		return struct {
			*observedWriter
			hijackerCap
			pusherCap
		}{core, hijackerCap{core}, pusherCap{core}}
	case h && rf:
		return struct {
			*observedWriter
			hijackerCap
			readerFromCap
		}{core, hijackerCap{core}, readerFromCap{core}}
	case p && rf:
		return struct {
			*observedWriter
			pusherCap
			readerFromCap
		}{core, pusherCap{core}, readerFromCap{core}}
	case f:
		return struct {
			*observedWriter
			flusherCap
		}{core, flusherCap{core}}
	case h:
		return struct {
			*observedWriter
			hijackerCap
		}{core, hijackerCap{core}}
	case p:
		return struct {
			*observedWriter
			pusherCap
		}{core, pusherCap{core}}
	case rf:
		return struct {
			*observedWriter
			readerFromCap
		}{core, readerFromCap{core}}
	default:
		return core
	}
}

func captureSession(b *CaptureBudget, cap int64) (*captureState, *captureState, func()) {
	if b == nil {
		b = defaultCaptureBudget
	}
	available := b.acquireSlot()
	first := &captureState{budget: b, cap: cap, hash: sha256.New(), available: available}
	second := &captureState{budget: b, cap: cap, hash: sha256.New(), available: available}
	release := func() {
		first.release()
		second.release()
		if available {
			b.releaseSlot()
		}
	}
	return first, second, release
}
func requestURI(r *http.Request) string {
	u := r.URL.RequestURI()
	if u == "" {
		return "/"
	}
	return u
}
func safeEmit(emit func(Event) bool, e Event) (accepted bool) {
	if emit == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			accepted = false
		}
	}()
	return emit(e)
}
func safeNow(now func() time.Time) (v time.Time) { defer func() { _ = recover() }(); return now() }
func safeInfo(info func(*http.Request) HTTPInfo, r *http.Request) (v HTTPInfo) {
	if info == nil {
		return
	}
	defer func() { _ = recover() }()
	return info(r)
}
func ObserveHTTP(next http.Handler, emit func(Event) bool, info func(*http.Request) HTTPInfo, now func() time.Time, bodyCap int64, budget *CaptureBudget) http.Handler {
	if now == nil {
		now = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := safeNow(now)
		method, uri := r.Method, requestURI(r)
		hi := HTTPInfo{Kind: "http-request"}
		if info != nil {
			hi = safeInfo(info, r)
			if hi.Kind == "" {
				hi.Kind = "http-request"
			}
		}
		rs, ws, release := captureSession(budget, bodyCap)
		defer release()
		requestHeaders, requestHeadersComplete := captureHeader(rs, r.Header)
		noRequestBody := r.Body == nil || r.Body == http.NoBody
		if r.Body == nil {
			r.Body = http.NoBody
		}
		orc := &observedReadCloser{ReadCloser: r.Body, s: rs, contentLength: r.ContentLength}
		r.Body = orc
		r = r.WithContext(context.WithValue(r.Context(), ingressKey, func() RequestFingerprint {
			rs.mu.Lock()
			defer rs.mu.Unlock()
			return RequestFingerprint{Algorithm: "sha256-body-v1", Method: method, RequestURI: uri, BodySHA256: hex.EncodeToString(rs.hash.Sum(nil)), ObservedBytes: rs.observed, Complete: (noRequestBody || rs.complete) && !rs.failed && rs.available}
		}))
		r = r.WithContext(context.WithValue(r.Context(), ingressBodyKey, func() []byte {
			rs.mu.Lock()
			defer rs.mu.Unlock()
			return rs.data
		}))
		ow := &observedWriter{base: w, s: ws}
		returned := false
		// Finalize observed prefixes on unwind without recovering or replacing the
		// handler panic. An aborted response is incomplete and an uncommitted
		// response has no inferred status or committed header snapshot.
		defer func() {
			ws.mu.Lock()
			ws.complete = returned && !ow.writeFailed
			ws.mu.Unlock()
			if returned && ow.status == 0 {
				ow.status = 200
				ow.headers, ow.headersComplete = captureHeader(ws, w.Header())
			}
			if noRequestBody {
				rs.mu.Lock()
				rs.complete = true
				rs.mu.Unlock()
			}
			if hi.CallID == "" {
				hi.CallID = CallID(r.Context())
			}
			// The request body may still be read elsewhere (a proxy's transport
			// can outlive the handler), so events are built from locked copies.
			in, out := rs.snapshot(), ws.snapshot()
			id, _ := r.Context().Value(identityKey).(requestIdentity)
			fp := RequestFingerprint{Algorithm: "sha256-body-v1", Method: method, RequestURI: uri, BodySHA256: in.digest, ObservedBytes: in.observed, Complete: in.complete && rs.available}
			req := Event{Time: start, Kind: hi.Kind, CallID: hi.CallID, CorrelationID: id.correlation, RequestCiphertextHash: id.hash, Sender: id.sender, Recipient: id.recipient, Method: method, URL: uri, Headers: requestHeaders, HeadersComplete: requestHeadersComplete, Body: in.data, BodyComplete: rs.available && in.complete && int64(len(in.data)) == in.observed, RequestFingerprint: fp, Status: ow.status, DurationNanos: safeNow(now).Sub(start).Nanoseconds()}
			safeEmit(emit, req)
			resp := Event{Time: safeNow(now), Kind: hi.Kind + "-response", CallID: hi.CallID, Method: method, URL: uri, Headers: ow.headers, HeadersComplete: ow.headersComplete, Body: out.data, BodyComplete: ws.available && out.complete && int64(len(out.data)) == out.observed, RequestFingerprint: fp, Status: ow.status}
			safeEmit(emit, resp)
		}()
		next.ServeHTTP(wrapWriter(ow), r)
		returned = true
	})
}

type observedTransport struct {
	base   http.RoundTripper
	emit   func(Event) bool
	now    func() time.Time
	cap    int64
	budget *CaptureBudget
}

func ObserveTransport(base http.RoundTripper, emit func(Event) bool, now func() time.Time, bodyCap int64, budget *CaptureBudget) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if now == nil {
		now = time.Now
	}
	return &observedTransport{base, emit, now, bodyCap, budget}
}
func (t *observedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := safeNow(t.now)
	requestSnapshot := new(http.Request)
	*requestSnapshot = *r
	rs, responseState, release := captureSession(t.budget, t.cap)
	requestHeaders, requestHeadersComplete := captureHeader(rs, r.Header)
	var lifecycleMu sync.Mutex
	remaining := 2
	done := func() {
		lifecycleMu.Lock()
		remaining--
		last := remaining == 0
		lifecycleMu.Unlock()
		if last {
			release()
		}
	}
	var requestOnce sync.Once
	emitRequest := func() {
		requestOnce.Do(func() {
			safeEmit(t.emit, RequestIdentity(requestSnapshot.Context(), t.event(requestSnapshot, requestHeaders, requestHeadersComplete, start, rs)))
			done()
		})
	}
	if r.Body != nil && r.Body != http.NoBody {
		requestSnapshot.Body = &observedReadCloser{ReadCloser: r.Body, s: rs, contentLength: r.ContentLength, onClose: emitRequest}
	} else {
		rs.mu.Lock()
		rs.complete = true
		rs.closed = true
		rs.mu.Unlock()
		emitRequest()
	}
	resp, err := t.base.RoundTrip(requestSnapshot)
	if err != nil {
		done()
		return nil, err
	}
	status := resp.StatusCode
	responseHeaders, responseHeadersComplete := captureHeader(responseState, resp.Header)
	orig := resp.Body
	var responseOnce sync.Once
	emitResponse := func() {
		responseOnce.Do(func() {
			fp := t.fingerprint(requestSnapshot, rs)
			responseState.mu.Lock()
			e := Event{Time: safeNow(t.now), Kind: "http-response", CallID: CallID(requestSnapshot.Context()), Method: requestSnapshot.Method, URL: requestURI(requestSnapshot), Headers: responseHeaders, HeadersComplete: responseHeadersComplete, Body: responseState.data, BodyComplete: responseState.available && responseState.complete && int64(len(responseState.data)) == responseState.observed, Status: status, RequestFingerprint: fp}
			responseState.mu.Unlock()
			safeEmit(t.emit, RequestIdentity(requestSnapshot.Context(), e))
			done()
		})
	}
	if orig == nil || orig == http.NoBody {
		responseState.mu.Lock()
		responseState.complete = true
		responseState.closed = true
		responseState.mu.Unlock()
		emitResponse()
	} else {
		resp.Body = &observedReadCloser{ReadCloser: orig, s: responseState, contentLength: resp.ContentLength, onClose: emitResponse}
	}
	return resp, nil
}
func (t *observedTransport) fingerprint(r *http.Request, s *captureState) RequestFingerprint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RequestFingerprint{Algorithm: "sha256-body-v1", Method: r.Method, RequestURI: requestURI(r), BodySHA256: hex.EncodeToString(s.hash.Sum(nil)), ObservedBytes: s.observed, Complete: s.complete && s.available && !s.failed}
}
func (t *observedTransport) event(r *http.Request, headers http.Header, headersComplete bool, start time.Time, s *captureState) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp := RequestFingerprint{Algorithm: "sha256-body-v1", Method: r.Method, RequestURI: requestURI(r), BodySHA256: hex.EncodeToString(s.hash.Sum(nil)), ObservedBytes: s.observed, Complete: s.complete && s.available && !s.failed}
	return Event{Time: start, Kind: "http-request", CallID: CallID(r.Context()), Method: r.Method, URL: requestURI(r), Headers: headers, HeadersComplete: headersComplete, Body: s.data, BodyComplete: s.available && s.complete && !s.failed && int64(len(s.data)) == s.observed, RequestFingerprint: fp, DurationNanos: safeNow(t.now).Sub(start).Nanoseconds()}
}
