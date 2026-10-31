package engine

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// endlessReader yields b forever, counting what it gave: a stream that would
// inflate without end (a decoded body).
type endlessReader struct {
	b    byte
	read int64
}

func (e *endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = e.b
	}
	e.read += int64(len(p))
	return len(p), nil
}

// failingReader gives n bytes, then fails as a broken stream does.
type failingReader struct{ n int }

var errBrokenStream = errors.New("broken stream")

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errBrokenStream
	}
	k := min(len(p), f.n)
	f.n -= k
	return k, nil
}

// readCapped reads a body up to its limit whole, refuses one byte more as an
// *http.MaxBytesError without returning any of it, never reads past limit+1,
// and returns any other read failure as it is.
func TestReadCapped(t *testing.T) {
	const limit = 64
	for _, n := range []int{0, 1, limit - 1, limit} {
		got, err := readCapped(bytes.NewReader(bytes.Repeat([]byte("a"), n)), limit)
		if err != nil || len(got) != n {
			t.Fatalf("%d bytes: read %d, err %v; want them all", n, len(got), err)
		}
	}
	got, err := readCapped(bytes.NewReader(bytes.Repeat([]byte("a"), limit+1)), limit)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) || tooLarge.Limit != limit || got != nil || !isBodyTooLarge(err) {
		t.Fatalf("limit+1 bytes: read %d, err %v; want a MaxBytesError at %d and nothing", len(got), err, limit)
	}
	endless := &endlessReader{b: 'a'}
	if _, err := readCapped(endless, limit); !isBodyTooLarge(err) || endless.read > 2*limit+512 {
		t.Fatalf("endless stream: err %v after %d bytes; want it refused at the limit", err, endless.read)
	}
	if _, err := readCapped(io.LimitReader(endless, 0), limit); err != nil {
		t.Fatalf("empty stream: %v", err)
	}
	got, err = readCapped(&failingReader{n: 3}, limit)
	if !errors.Is(err, errBrokenStream) || isBodyTooLarge(err) || len(got) != 3 {
		t.Fatalf("broken stream: read %d, err %v; want the stream's own error", len(got), err)
	}
	// A MaxBytesReader beneath the read (a server's own bound) is the same
	// refusal.
	rec := httptest.NewRecorder()
	_, err = readCapped(http.MaxBytesReader(rec, io.NopCloser(strings.NewReader(strings.Repeat("a", limit))), limit/2), limit)
	if !isBodyTooLarge(err) {
		t.Fatalf("MaxBytesReader beneath: %v", err)
	}
}
