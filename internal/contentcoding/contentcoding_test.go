package contentcoding

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func gzipped(t *testing.T, b []byte) []byte {
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

func request(body []byte, codings ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	for _, c := range codings {
		r.Header.Add("Content-Encoding", c)
	}
	return r
}

const message = `{"resourceType":"Bundle","type":"collection"}`

// testLimit is the decoded limit of the rows that do not test the limit.
const testLimit = 8 << 20

// TestDecode_IdentityIsUnchanged: a body sent without a coding, or with
// identity, reaches the handler as sent, with its headers unchanged.
func TestDecode_IdentityIsUnchanged(t *testing.T) {
	for _, codings := range [][]string{nil, {"identity"}, {"IDENTITY"}, {""}} {
		r := request([]byte(message), codings...)
		got := Decode(r, testLimit)
		if got != r {
			t.Fatalf("%q: the request was replaced", codings)
		}
		b, _ := io.ReadAll(got.Body)
		if string(b) != message {
			t.Fatalf("%q: body %q", codings, b)
		}
	}
}

// TestDecode_GzipIsDecoded: a gzip-coded body reaches the handler decoded,
// and the request no longer states a coding or the coded length, so nothing
// below reads the coded size as the body's.
func TestDecode_GzipIsDecoded(t *testing.T) {
	for _, coding := range []string{"gzip", "x-gzip", "GZIP", " gzip "} {
		z := gzipped(t, []byte(message))
		r := request(z, coding)
		r.ContentLength = int64(len(z))
		got := Decode(r, testLimit)
		b, err := io.ReadAll(got.Body)
		if err != nil || string(b) != message {
			t.Fatalf("%q: body %q, %v", coding, b, err)
		}
		if v := got.Header.Values("Content-Encoding"); len(v) != 0 {
			t.Fatalf("%q: Content-Encoding still %q", coding, v)
		}
		if got.ContentLength != -1 || got.Header.Get("Content-Length") != "" {
			t.Fatalf("%q: length %d %q", coding, got.ContentLength, got.Header.Get("Content-Length"))
		}
		if r.Header.Get("Content-Encoding") == "" {
			t.Fatalf("%q: the caller's request was changed", coding)
		}
	}
}

// TestDecode_UnsupportedCodingIsRefused: a body in any coding but gzip, or in
// any stack of codings, gzip's included, fails its reads with ErrUnsupported,
// and none of it is read to guess what it is (a br-labelled body of plain JSON
// is refused too). The copy states no coding or length either.
func TestDecode_UnsupportedCodingIsRefused(t *testing.T) {
	for _, codings := range [][]string{
		{"br"}, {"deflate"}, {"compress"}, {"zstd"}, {"gzip, gzip"}, {"gzip", "gzip"},
		{"gzip, identity"}, {"identity, gzip"}, {"gzip,"}, {"gzip;q=1"},
	} {
		src := &countingReader{r: strings.NewReader(message)}
		r := httptest.NewRequest(http.MethodPost, "/x", src)
		for _, c := range codings {
			r.Header.Add("Content-Encoding", c)
		}
		got := Decode(r, testLimit)
		for range 2 {
			if n, err := got.Body.Read(make([]byte, 64)); n != 0 || !errors.Is(err, ErrUnsupported) {
				t.Fatalf("%q: read %d, %v, want ErrUnsupported", codings, n, err)
			}
		}
		if src.n != 0 {
			t.Fatalf("%q: %d bytes of the body read", codings, src.n)
		}
		if got.Header.Get("Content-Encoding") != "" || got.ContentLength != -1 {
			t.Fatalf("%q: copy states %q, length %d", codings, got.Header.Get("Content-Encoding"), got.ContentLength)
		}
	}
}

// TestDecode_CorruptGzipIsAReadError: a body that is not a whole gzip stream
// is decoded as far as it goes and fails as a read: what the reader answers
// for any body it cannot read (400), never ErrUnsupported.
func TestDecode_CorruptGzipIsAReadError(t *testing.T) {
	whole := gzipped(t, []byte(message))
	trailing := append(append([]byte{}, whole...), "junk"...)
	badSum := append([]byte{}, whole...)
	badSum[len(badSum)-5] ^= 0xff
	for name, body := range map[string][]byte{
		"empty":     {},
		"not gzip":  []byte(message),
		"truncated": whole[:len(whole)-3],
		"trailing":  trailing,
		"checksum":  badSum,
	} {
		got := Decode(request(body, "gzip"), testLimit)
		if _, err := io.ReadAll(got.Body); err == nil || errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s: read error %v", name, err)
		}
	}
}

// TestDecode_BombStopsWhereTheReaderStops: decoding is driven by the reader,
// so a reader that stops past its bound stops the inflation there: a small
// body that inflates far past the bound is never inflated whole.
func TestDecode_BombStopsWhereTheReaderStops(t *testing.T) {
	const bound = 1 << 20
	z := gzipped(t, make([]byte, 256<<20))
	src := &countingReader{r: bytes.NewReader(z)}
	r := httptest.NewRequest(http.MethodPost, "/x", src)
	r.Header.Set("Content-Encoding", "gzip")
	got := Decode(r, bound)
	b, err := io.ReadAll(io.LimitReader(got.Body, bound+1))
	if err != nil || len(b) != bound+1 {
		t.Fatalf("read %d, %v", len(b), err)
	}
	if src.n >= int64(len(z)) {
		t.Fatalf("the whole coded body (%d bytes) was read for %d decoded", src.n, bound+1)
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// TestAdvertise: a refusal of a coding names the one accepted (RFC 9110
// §15.5.16).
func TestAdvertise(t *testing.T) {
	h := http.Header{}
	Advertise(h)
	if got := h.Values("Accept-Encoding"); len(got) != 1 || got[0] != "gzip" {
		t.Fatalf("Accept-Encoding %q", got)
	}
}

var large = strings.Repeat(`{"a":"b"},`, 1000)

func serve(t *testing.T, h http.HandlerFunc, method string, accept ...string) *http.Response {
	t.Helper()
	r := httptest.NewRequest(method, "/x", nil)
	for _, a := range accept {
		r.Header.Add("Accept-Encoding", a)
	}
	w := httptest.NewRecorder()
	Compress(h).ServeHTTP(w, r)
	return w.Result()
}

func writeBody(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		w.Header().Set("Content-Length", "999999")
		w.Header().Set("X-Kept", "yes")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func decoded(t *testing.T, resp *http.Response) string {
	t.Helper()
	body := resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = zr
	}
	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func statesVary(resp *http.Response) bool {
	for _, v := range resp.Header.Values("Vary") {
		for _, f := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), "Accept-Encoding") {
				return true
			}
		}
	}
	return false
}

// TestCompress_WhenAccepted: an answer is gzip-coded when the caller accepts
// gzip; it decodes to the bytes the handler wrote, with its status and
// headers, and states no length (the handler's is the identity length).
func TestCompress_WhenAccepted(t *testing.T) {
	for _, accept := range [][]string{{"gzip"}, {"GZIP"}, {"x-gzip"}, {"br, gzip;q=0.5"}, {"*"}, {"identity", "gzip"}} {
		resp := serve(t, writeBody(http.StatusCreated, large), http.MethodPost, accept...)
		if resp.Header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("%q: not compressed: %v", accept, resp.Header)
		}
		if resp.StatusCode != http.StatusCreated || resp.Header.Get("X-Kept") != "yes" ||
			resp.Header.Get("Content-Type") != "application/fhir+json" {
			t.Fatalf("%q: %d %v", accept, resp.StatusCode, resp.Header)
		}
		if resp.Header.Get("Content-Length") != "" {
			t.Fatalf("%q: Content-Length %q", accept, resp.Header.Get("Content-Length"))
		}
		if !statesVary(resp) {
			t.Fatalf("%q: no Vary", accept)
		}
		if got := decoded(t, resp); got != large {
			t.Fatalf("%q: decoded %d bytes, want %d", accept, len(got), len(large))
		}
	}
}

// TestCompress_IdentityOtherwise: without gzip in Accept-Encoding, or with
// it refused, the answer is the handler's bytes as written, and still says
// it varies by Accept-Encoding.
func TestCompress_IdentityOtherwise(t *testing.T) {
	for _, accept := range [][]string{nil, {"identity"}, {"br"}, {"gzip;q=0"}, {"*;q=0"}, {"gzip;q=0, *"}, {"*, gzip;q=0"}} {
		resp := serve(t, writeBody(http.StatusOK, large), http.MethodPost, accept...)
		if resp.Header.Get("Content-Encoding") != "" {
			t.Fatalf("%q: compressed", accept)
		}
		if !statesVary(resp) {
			t.Fatalf("%q: no Vary", accept)
		}
		if got := decoded(t, resp); got != large {
			t.Fatalf("%q: body changed", accept)
		}
	}
}

// TestCompress_Exclusions: an answer that is small, has no body, is a stream,
// or that the handler coded itself is written as the handler wrote it.
func TestCompress_Exclusions(t *testing.T) {
	small := writeBody(http.StatusOK, message)
	noContent := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	notModified := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotModified) }
	stream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, large)
	}
	precoded := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(w, large)
	}
	for name, c := range map[string]struct {
		h      http.HandlerFunc
		method string
		want   string
	}{
		"small":        {small, http.MethodPost, message},
		"no content":   {noContent, http.MethodPost, ""},
		"not modified": {notModified, http.MethodGet, ""},
		"head":         {writeBody(http.StatusOK, large), http.MethodHead, ""},
		"stream":       {stream, http.MethodGet, large},
		"precoded":     {precoded, http.MethodGet, large},
	} {
		resp := serve(t, c.h, c.method, "gzip")
		if ce := resp.Header.Get("Content-Encoding"); ce == "gzip" {
			t.Fatalf("%s: compressed", name)
		}
		b, _ := io.ReadAll(resp.Body)
		if c.method != http.MethodHead && string(b) != c.want {
			t.Fatalf("%s: body %q", name, b)
		}
	}
}

// TestCompress_SmallAnswerKeepsItsLength: an answer written whole below the
// threshold keeps the length its handler stated.
func TestCompress_SmallAnswerKeepsItsLength(t *testing.T) {
	h := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "45")
		_, _ = io.WriteString(w, message)
	}
	resp := serve(t, h, http.MethodGet, "gzip")
	if resp.Header.Get("Content-Length") != "45" {
		t.Fatalf("Content-Length %q", resp.Header.Get("Content-Length"))
	}
}

// TestCompress_ManyWrites: an answer written in many small pieces is
// compressed whole once it passes the threshold.
func TestCompress_ManyWrites(t *testing.T) {
	h := func(w http.ResponseWriter, _ *http.Request) {
		for range 1000 {
			_, _ = io.WriteString(w, `{"a":"b"},`)
		}
	}
	resp := serve(t, h, http.MethodGet, "gzip")
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("not compressed")
	}
	if resp.StatusCode != http.StatusOK || decoded(t, resp) != large {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// TestCompress_UntypedAnswerIsTypedFromItsBytes: an answer whose handler set
// no Content-Type is typed from its identity bytes, as net/http would type it
// uncompressed, never from the compressed ones.
func TestCompress_UntypedAnswerIsTypedFromItsBytes(t *testing.T) {
	h := func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, large) }
	resp := serve(t, h, http.MethodGet, "gzip")
	if got, want := resp.Header.Get("Content-Type"), http.DetectContentType([]byte(large)); got != want {
		t.Fatalf("Content-Type %q, want %q", got, want)
	}
}

// TestCompress_Flush: a handler that flushes reaches its caller as it
// flushes, compressed or not.
func TestCompress_Flush(t *testing.T) {
	for _, first := range []string{message, large} {
		var flushed bool
		h := func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, first)
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			flushed = true
			_, _ = io.WriteString(w, large)
		}
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		Compress(http.HandlerFunc(h)).ServeHTTP(w, r)
		if !flushed || !w.Flushed {
			t.Fatalf("%d: not flushed", len(first))
		}
		if got := decoded(t, w.Result()); got != first+large {
			t.Fatalf("%d: body changed", len(first))
		}
	}
}

// TestUnsupported: a request whose body Decode refuses says so, whatever
// wraps its body later; one Decode reads, or leaves as sent, does not.
func TestUnsupported(t *testing.T) {
	refused := Decode(request([]byte(message), "br"), testLimit)
	refused.Body = io.NopCloser(refused.Body)
	if !Unsupported(refused.WithContext(refused.Context())) {
		t.Fatal("a br body wrapped later is not reported")
	}
	for _, codings := range [][]string{nil, {"gzip"}, {"identity"}} {
		if Unsupported(Decode(request(gzipped(t, []byte(message)), codings...), testLimit)) {
			t.Fatalf("%q: reported", codings)
		}
	}
}

// endless repeats unit without end, counting the bytes read.
type endless struct {
	unit []byte
	off  int
	n    int64
}

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = e.unit[e.off]
		e.off = (e.off + 1) % len(e.unit)
	}
	e.n += int64(len(p))
	return len(p), nil
}

// TestDecode_CodedBodyIsBounded: a coded body that decodes to nothing, an
// endless stream of empty gzip members or of empty deflate blocks inside one
// member, fails as a body over its limit once more coded bytes are read than
// an honest gzip of the limit takes, never read without end.
func TestDecode_CodedBodyIsBounded(t *testing.T) {
	const limit = 64 << 10
	// A gzip header, then non-final empty stored blocks (BFINAL 0, BTYPE 00,
	// LEN 0, NLEN 0xffff), each decoding to nothing.
	header := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff}
	block := []byte{0, 0, 0, 0xff, 0xff}
	for name, src := range map[string]io.Reader{
		"empty members": &endless{unit: gzipped(t, nil)},
		"empty blocks":  io.MultiReader(bytes.NewReader(header), &endless{unit: block}),
	} {
		r := httptest.NewRequest(http.MethodPost, "/x", src)
		r.Header.Set("Content-Encoding", "gzip")
		done := make(chan error, 1)
		go func() {
			_, err := io.ReadAll(Decode(r, limit).Body)
			done <- err
		}()
		select {
		case err := <-done:
			var tooLarge *http.MaxBytesError
			if !errors.As(err, &tooLarge) || tooLarge.Limit != limit {
				t.Fatalf("%s: %v, want a body over %d", name, err, limit)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: still reading the coded body", name)
		}
	}
}

// TestDecode_AnHonestBodyAtTheLimitIsReadWhole: the coded bound leaves room
// for gzip's worst case, a body of incompressible bytes stored uncompressed.
func TestDecode_AnHonestBodyAtTheLimitIsReadWhole(t *testing.T) {
	const limit = 1 << 20
	body := make([]byte, limit)
	_, _ = rand.NewChaCha8([32]byte{1}).Read(body)
	for _, level := range []int{gzip.NoCompression, gzip.HuffmanOnly, gzip.DefaultCompression} {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, level)
		_, _ = zw.Write(body)
		_ = zw.Close()
		got, err := io.ReadAll(Decode(request(buf.Bytes(), "gzip"), limit).Body)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("level %d (%d coded bytes): read %d, %v", level, buf.Len(), len(got), err)
		}
	}
}

// heads is a ResponseWriter recording each status it is given, with no
// Flush of its own.
type heads struct {
	h        http.Header
	statuses []int
	body     bytes.Buffer
}

func (w *heads) Header() http.Header         { return w.h }
func (w *heads) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *heads) WriteHeader(status int)      { w.statuses = append(w.statuses, status) }

// TestCompress_ARepeatedStatusKeepsTheFirst: a handler that sets its status
// twice before writing gets the first, as net/http keeps it, and an answer
// coded or not as its head states.
func TestCompress_ARepeatedStatusKeepsTheFirst(t *testing.T) {
	for _, accept := range []string{"gzip", "identity"} {
		h := func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, large)
		}
		resp := serve(t, h, http.MethodGet, accept)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", accept, resp.StatusCode)
		}
		if got := decoded(t, resp); got != large {
			t.Fatalf("%s: body %d bytes", accept, len(got))
		}
	}
}

// TestCompress_APanicWritesNoAnswer: a handler that panics before it writes
// leaves no status written, as without the wrapper.
func TestCompress_APanicWritesNoAnswer(t *testing.T) {
	w := &heads{h: http.Header{}}
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	func() {
		defer func() { _ = recover() }()
		Compress(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("handler") })).ServeHTTP(w, r)
	}()
	if len(w.statuses) != 0 || w.body.Len() != 0 {
		t.Fatalf("wrote %v and %d bytes", w.statuses, w.body.Len())
	}
}

// TestCompress_VaryIsStatedOnce: an answer that already varies by
// Accept-Encoding is not given it again; one that varies by something else
// is given it too.
func TestCompress_VaryIsStatedOnce(t *testing.T) {
	for _, vary := range []string{"Accept-Encoding", "accept-encoding", "Origin, Accept-Encoding", "*"} {
		resp := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Vary", vary)
			_, _ = io.WriteString(w, large)
		}, http.MethodGet, "gzip")
		if got := resp.Header.Values("Vary"); len(got) != 1 {
			t.Fatalf("%q: Vary %q", vary, got)
		}
	}
	resp := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Vary", "Origin")
		_, _ = io.WriteString(w, large)
	}, http.MethodGet, "gzip")
	if got := resp.Header.Values("Vary"); len(got) != 2 || !statesVary(resp) {
		t.Fatalf("Vary %q", got)
	}
}

// TestCompress_AFailedFlushIsReported: a flush the writer beneath cannot
// make is an error, not a silent success.
func TestCompress_AFailedFlushIsReported(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	var err error
	Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, message)
		err = http.NewResponseController(w).Flush()
	})).ServeHTTP(&heads{h: http.Header{}}, r)
	if err == nil {
		t.Fatal("the flush was reported done")
	}
}

// TestCompress_AFlushedHeadWritesItsStatusOnce: a HEAD answer flushed before
// its handler sets a status is written with one status.
func TestCompress_AFlushedHeadWritesItsStatusOnce(t *testing.T) {
	w := &heads{h: http.Header{}}
	r := httptest.NewRequest(http.MethodHead, "/x", nil)
	Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = http.NewResponseController(w).Flush()
	})).ServeHTTP(w, r)
	if len(w.statuses) != 1 || w.statuses[0] != http.StatusOK {
		t.Fatalf("statuses %v, want one 200", w.statuses)
	}
}
