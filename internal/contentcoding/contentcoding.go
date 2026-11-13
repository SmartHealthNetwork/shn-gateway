// Package contentcoding is HTTP content coding (RFC 9110 §8.4) on the routes
// a gateway or the Hub serves: a request body sent gzip-coded is decoded
// before its handler reads it, and an answer is gzip-coded when the caller
// accepts gzip. The gateway module's copy (gateway/internal/contentcoding) is
// the source; the Hub's (internal/contentcoding) is a byte-identical copy,
// fenced by the Hub copy's tests.
//
// Content coding is transport, not payload. What a handler reads, carries,
// verifies, hashes and records is the decoded (identity) body, and what it
// writes is coded only on the way out, so a coded exchange is the exchange
// it would have been uncoded (relay fidelity, as the relay-fidelity design
// treats the gateway's own outbound calls, which Go's client already codes).
//
// gzip is the only coding accepted. A body sent in any other coding, or in
// any stack of codings, fails its first read with ErrUnsupported, and none
// of it is read to guess what it is: the bytes cannot be read as their sender
// declared them, a refusal on message integrity. It fails where the handler
// reads the body, so a route refuses its caller on authentication, and on
// anything else it decides first, before the coding. A body that is not a
// whole gzip stream fails as a read, as any body its reader cannot read does.
// Decoding is driven by the reader, so the bound a reader stops at bounds the
// decoded body: a small body that inflates past it is never inflated whole.
// The coded body is bounded too, at the most an honest gzip of that many
// decoded bytes can take: a stream of members or blocks that decode to
// nothing would otherwise be read without end, never reaching the reader's
// bound. Past it the body fails as over its limit (*http.MaxBytesError).
//
// Compression and secrets (BREACH): a caller that can put chosen bytes in an
// answer beside a secret and watch the answer's compressed length, over many
// answers carrying the same secret, can recover the secret. So an answer that
// carries a reusable credential or session (a bearer token or client secret
// the route issues, a cookie, an operator or console session) is never
// compressed; a route is compressed only when its declaration states it
// carries none. An envelope's authorization tokens do not make it such an
// answer: each is minted for one leg, bound to that leg (its frame,
// operation, correlation, holder and payload hash) and refused by the network
// if replayed, so every answer carries a different one, and there is no
// secret repeated across answers for a length to reveal.
package contentcoding

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ErrUnsupported is the read error of a request body sent in a coding other
// than gzip, or in a stack of codings.
var ErrUnsupported = errors.New("unsupported content coding")

// Accepted is the one content coding a request body may be sent in.
const Accepted = "gzip"

// Advertise states, on the answer refusing a request's coding, the coding a
// request may be sent in (RFC 9110 §15.5.16).
func Advertise(h http.Header) { h.Set("Accept-Encoding", Accepted) }

// Decode returns r when its body is sent in no coding, else a copy of r whose
// body is read decoded: gzip as it is read, any other coding or stack of
// codings failing with ErrUnsupported. The copy states no coding and no
// length (the stated one is the coded body's), so nothing below reads the
// coded size as the body's. limit is the most decoded bytes the route reads;
// the coded body is read no further than codedBound(limit).
func Decode(r *http.Request, limit int64) *http.Request {
	var codings []string
	for _, v := range r.Header.Values("Content-Encoding") {
		for _, c := range strings.Split(v, ",") {
			codings = append(codings, strings.ToLower(strings.TrimSpace(c)))
		}
	}
	// One empty value is a header stating no coding; an empty member of a
	// list is a malformed list.
	if len(codings) == 1 && codings[0] == "" {
		codings = nil
	}
	if len(codings) == 0 || (len(codings) == 1 && codings[0] == "identity") {
		return r
	}
	d := r.Clone(r.Context())
	d.Header.Del("Content-Encoding")
	d.Header.Del("Content-Length")
	d.ContentLength = -1
	d.Body = &gzipBody{coded: r.Body, bounded: &boundedCoded{
		r: io.LimitReader(r.Body, codedBound(limit)+1), max: codedBound(limit), limit: limit,
	}}
	if len(codings) > 1 || (codings[0] != "gzip" && codings[0] != "x-gzip") {
		d = d.WithContext(context.WithValue(d.Context(), unsupportedKey{}, true))
		d.Body = unsupportedBody{coded: r.Body}
	}
	return d
}

// unsupportedKey marks a request Decode gave an unsupportedBody: what wraps
// the body later (a counter, a recorder) keeps the request's context.
type unsupportedKey struct{}

// Unsupported reports whether r's body is one Decode refuses, sent in a
// coding other than gzip or in a stack of codings, without reading it.
func Unsupported(r *http.Request) bool {
	v, _ := r.Context().Value(unsupportedKey{}).(bool)
	return v
}

// unsupportedBody is a body sent in a coding this package does not decode:
// every read fails, and none of it is read.
type unsupportedBody struct{ coded io.ReadCloser }

func (unsupportedBody) Read([]byte) (int, error) { return 0, ErrUnsupported }

func (b unsupportedBody) Close() error { return b.coded.Close() }

// codedBound is the most coded bytes read for a body read to limit decoded
// bytes. A gzip of limit bytes from zlib's deflate at its default settings
// (windowBits 15, memLevel 8), or any encoder writing stored blocks of the
// usual size, is at most limit plus deflate's framing plus gzip's: stored
// blocks cost 5 bytes per block of up to 65,535 bytes (about limit/13,000),
// zlib's deflateBound allows limit/4096 + limit/16384 + 13 at those
// settings, and each gzip member adds a 10-byte header and an 8-byte
// trailer. limit/1024 is over three times deflateBound's share, and 1 KiB
// covers the fixed framing of a small body and of a few members. Do not
// tighten it below deflateBound: an honest body at the limit would then be
// refused. A sender that writes many small stored blocks, or flushes after
// every few bytes, can exceed the margin, and so can a header carrying a
// name or extra fields longer than it; each is refused as over the limit,
// and a network request has no reason to be coded that way.
func codedBound(limit int64) int64 { return limit + limit/1024 + 1024 }

// boundedCoded is a coded body read no further than max bytes; past them it
// fails as a body over limit, the decoded limit max was derived from.
type boundedCoded struct {
	r          io.Reader
	n          int64
	max, limit int64
}

func (b *boundedCoded) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.max {
		return 0, &http.MaxBytesError{Limit: b.limit}
	}
	return n, err
}

// gzipBody decodes coded as it is read. The gzip header is read with the
// first byte asked for, so a body that is not gzip fails as a read.
type gzipBody struct {
	coded   io.ReadCloser
	bounded *boundedCoded
	zr      *gzip.Reader
	err     error
}

func (b *gzipBody) Read(p []byte) (int, error) {
	if b.zr == nil && b.err == nil {
		b.zr, b.err = gzip.NewReader(b.bounded)
		// An empty body is no gzip stream, not an empty one.
		if errors.Is(b.err, io.EOF) {
			b.err = io.ErrUnexpectedEOF
		}
	}
	if b.err != nil {
		return 0, b.err
	}
	return b.zr.Read(p)
}

func (b *gzipBody) Close() error { return b.coded.Close() }

// minCoded is the smallest answer compressed: below it, gzip's own framing
// costs about what it saves.
const minCoded = 1024

// Compress wraps h: its answer is gzip-coded when the request accepts gzip
// (RFC 9110 §12.5.3) and the answer is at least minCoded bytes with a body to
// code. An answer with no body (1xx, 204 or 304), an event stream, or one h
// coded itself is written as h wrote it. A HEAD is decided as the GET it
// stands for (RFC 9110 §9.3.2, §8.6): its handler writes the GET's body, which
// is coded or not by the same rules and handed to net/http, which sends the
// head the same bytes give a GET (Content-Encoding, Content-Length when the
// GET would state one) and none of the body. Every answer states that it
// varies by Accept-Encoding. A route that carries a reusable credential or
// session must not be wrapped (the package doc).
func Compress(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aw := &answerWriter{ResponseWriter: w, accepts: acceptsGzip(r.Header)}
		h.ServeHTTP(aw, r)
		// Not deferred: a handler that panics has written no answer, and
		// finishing one would write a 200 for it.
		aw.finish()
	})
}

// acceptsGzip reports whether h's Accept-Encoding accepts gzip: named, or
// left to "*", with a weight above zero.
func acceptsGzip(h http.Header) bool {
	named, star := -1.0, -1.0
	for _, v := range h.Values("Accept-Encoding") {
		for _, member := range strings.Split(v, ",") {
			coding, params, _ := strings.Cut(member, ";")
			q := 1.0
			for _, p := range strings.Split(params, ";") {
				k, val, ok := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
					if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
						q = f
					} else {
						q = 0
					}
				}
			}
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "gzip", "x-gzip":
				named = max(named, q)
			case "*":
				star = max(star, q)
			}
		}
	}
	if named >= 0 {
		return named > 0
	}
	return star > 0
}

// answerWriter holds an answer's head and first bytes until it can tell
// whether to code it: at minCoded bytes, a flush, or the handler's return.
type answerWriter struct {
	http.ResponseWriter
	accepts bool
	status  int
	held    []byte
	decided bool
	zw      *gzip.Writer
}

func (a *answerWriter) Unwrap() http.ResponseWriter { return a.ResponseWriter }

func (a *answerWriter) WriteHeader(status int) {
	if a.decided {
		a.ResponseWriter.WriteHeader(status) // net/http reports the superfluous call
		return
	}
	if a.status != 0 {
		// The head is still held: the first status stands, as net/http keeps
		// it, and the head is written once, coded or not.
		return
	}
	if status < 200 {
		a.ResponseWriter.WriteHeader(status)
		return
	}
	a.status = status
	if status == http.StatusNoContent || status == http.StatusNotModified {
		a.decide(false)
	}
}

func (a *answerWriter) Write(p []byte) (int, error) {
	if a.status == 0 {
		a.WriteHeader(http.StatusOK)
	}
	if a.decided {
		if a.zw != nil {
			return a.zw.Write(p)
		}
		return a.ResponseWriter.Write(p)
	}
	a.held = append(a.held, p...)
	if len(a.held) >= minCoded {
		if err := a.decide(true); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush sends what is held: coded when it has reached minCoded, else as
// written, and the rest of the answer then follows the same way.
func (a *answerWriter) Flush() { _ = a.FlushError() }

func (a *answerWriter) FlushError() error {
	var err error
	if !a.decided && a.status == 0 {
		a.WriteHeader(http.StatusOK) // decides at once an answer with no body
	}
	if !a.decided {
		err = a.decide(false)
	}
	if a.zw != nil {
		err = errors.Join(err, a.zw.Flush())
	}
	return errors.Join(err, http.NewResponseController(a.ResponseWriter).Flush())
}

// StrongETag reports an ETag that names exact bytes: coding the answer would
// make it untrue, so the answer stays as it is. Uncoded is always legal.
func StrongETag(v string) bool {
	return v != "" && !strings.HasPrefix(strings.TrimSpace(v), "W/")
}

// decide writes the held head and bytes, coded when code is set and the
// answer can be coded.
func (a *answerWriter) decide(code bool) error {
	a.decided = true
	h := a.Header()
	if !varies(h) {
		h.Add("Vary", "Accept-Encoding")
	}
	if code && a.accepts && h.Get("Content-Encoding") == "" && !StrongETag(h.Get("ETag")) &&
		!strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Get("Content-Type"))), "text/event-stream") {
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", http.DetectContentType(a.held))
		}
		h.Set("Content-Encoding", "gzip")
		h.Del("Content-Length")
		a.ResponseWriter.WriteHeader(a.status)
		a.zw = gzip.NewWriter(a.ResponseWriter)
		_, err := a.zw.Write(a.held)
		a.held = nil
		return err
	}
	a.ResponseWriter.WriteHeader(a.status)
	if len(a.held) == 0 {
		return nil
	}
	_, err := a.ResponseWriter.Write(a.held)
	a.held = nil
	return err
}

// varies reports whether h already states that the answer varies by
// Accept-Encoding (or by everything).
func varies(h http.Header) bool {
	for _, v := range h.Values("Vary") {
		for _, f := range strings.Split(v, ",") {
			f = strings.TrimSpace(f)
			if f == "*" || strings.EqualFold(f, "Accept-Encoding") {
				return true
			}
		}
	}
	return false
}

// finish writes what the handler left held and ends a coded answer.
func (a *answerWriter) finish() {
	if !a.decided {
		if a.status == 0 {
			a.status = http.StatusOK
		}
		_ = a.decide(false)
	}
	if a.zw != nil {
		_ = a.zw.Close()
	}
}
