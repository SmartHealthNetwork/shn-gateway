package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// exhausted is a capture budget whose only slot is held, so every exchange it
// observes keeps no body.
func exhausted(t *testing.T) *CaptureBudget {
	t.Helper()
	b := NewCaptureBudget(1<<20, 1)
	_, _, release := captureSession(b, 1<<20)
	t.Cleanup(release)
	return b
}

func digestOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ingressEvent serves one request with body through ObserveHTTP on budget and
// returns its request event; read decides how much of the body the handler
// reads.
func ingressEvent(t *testing.T, budget *CaptureBudget, body io.Reader, read func(io.Reader)) (Event, RequestFingerprint) {
	t.Helper()
	var mu sync.Mutex
	var got Event
	var handlerSaw RequestFingerprint
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read(r.Body)
		// What a gateway stamps on its seal for this request.
		handlerSaw = IngressFingerprint(r.Context())
		_, _ = w.Write([]byte("ok"))
	}), func(e Event) bool {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasSuffix(e.Kind, "-response") && e.Kind != "response" {
			got = e
		}
		return true
	}, nil, time.Now, 4<<10, budget)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/Claim/$submit", body))
	mu.Lock()
	defer mu.Unlock()
	return got, handlerSaw
}

// transportEvent forwards one request with body through ObserveTransport on
// budget and returns its request event.
func transportEvent(t *testing.T, budget *CaptureBudget, body string) (Event, RequestFingerprint) {
	t.Helper()
	var mu sync.Mutex
	var got Event
	var answered RequestFingerprint
	rt := ObserveTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	}), func(e Event) bool {
		mu.Lock()
		defer mu.Unlock()
		switch e.Kind {
		case "http-request":
			got = e
		case "http-response":
			answered = e.RequestFingerprint
		}
		return true
	}, time.Now, 4<<10, budget)
	req, err := http.NewRequest("POST", "http://payer.example/Claim/$submit", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	return got, answered
}

// A request fingerprint is complete when its hash saw every byte of the body,
// whether or not the capture budget kept the body: a door or gateway whose
// budget is exhausted still proves which bytes it forwarded and received, and
// the two ends of a forward still link. The body itself is reported
// not kept.
func TestAnUnkeptBodyStillFingerprintsWhole(t *testing.T) {
	const body = `{"resourceType":"Bundle","id":"unkept"}`
	forward, answered := transportEvent(t, exhausted(t), body)
	ingress, sealed := ingressEvent(t, exhausted(t), strings.NewReader(body), func(r io.Reader) { _, _ = io.Copy(io.Discard, r) })
	for name, e := range map[string]Event{"forward": forward, "ingress": ingress} {
		fp := e.RequestFingerprint
		if e.BodyComplete || len(e.Body) != 0 {
			t.Fatalf("%s: BodyComplete=%v with %d body bytes; an exhausted budget keeps no body", name, e.BodyComplete, len(e.Body))
		}
		if !fp.Complete || fp.ObservedBytes != int64(len(body)) || fp.BodySHA256 != digestOf(body) {
			t.Fatalf("%s: fingerprint %+v; want complete over all %d bytes", name, fp, len(body))
		}
	}
	if !FingerprintLink(forward.RequestFingerprint, ingress.RequestFingerprint) {
		t.Fatalf("a forward and its ingress, both unkept, do not link: %+v %+v", forward.RequestFingerprint, ingress.RequestFingerprint)
	}
	// The fingerprint a gateway stamps on its seal (IngressFingerprint), and
	// the one the transport's answer carries, are the same whole fingerprint.
	if sealed != ingress.RequestFingerprint || !FingerprintLink(sealed, forward.RequestFingerprint) {
		t.Fatalf("the ingress fingerprint a seal carries is %+v; the ingress event's is %+v", sealed, ingress.RequestFingerprint)
	}
	if answered != forward.RequestFingerprint {
		t.Fatalf("the transport's answer carries %+v; its request carried %+v", answered, forward.RequestFingerprint)
	}
	// A body past the capture cap, with the budget free, is kept only in
	// part, but its hash saw every byte.
	long := strings.Repeat("x", 5<<10)
	capped, _ := ingressEvent(t, NewCaptureBudget(1<<20, 8), strings.NewReader(long), func(r io.Reader) { _, _ = io.Copy(io.Discard, r) })
	if capped.BodyComplete || !capped.RequestFingerprint.Complete || capped.RequestFingerprint.BodySHA256 != digestOf(long) || capped.RequestFingerprint.ObservedBytes != int64(len(long)) {
		t.Fatalf("a body past the cap: BodyComplete=%v fingerprint %+v; want the body partial and the fingerprint whole", capped.BodyComplete, capped.RequestFingerprint)
	}
}

type failingReader struct {
	data string
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// A hash that did not see every byte is never complete, kept body or not: a
// handler that stops reading early, a body whose read fails, or a body cut off
// mid-stream cannot link to anything.
func TestAFingerprintThatMissedBytesIsIncomplete(t *testing.T) {
	const body = `{"resourceType":"Bundle","id":"partial"}`
	wholeEvent, _ := ingressEvent(t, exhausted(t), strings.NewReader(body), func(r io.Reader) { _, _ = io.Copy(io.Discard, r) })
	whole := wholeEvent.RequestFingerprint
	for _, budget := range []struct {
		name string
		b    func() *CaptureBudget
	}{{"unkept", func() *CaptureBudget { return exhausted(t) }}, {"kept", func() *CaptureBudget { return NewCaptureBudget(1<<20, 8) }}} {
		for _, tc := range []struct {
			name string
			body io.Reader
			read func(io.Reader)
		}{
			{"read in part", strings.NewReader(body), func(r io.Reader) { _, _ = io.ReadFull(r, make([]byte, 5)) }},
			{"read fails", &failingReader{data: body[:9], err: errors.New("connection reset")}, func(r io.Reader) { _, _ = io.Copy(io.Discard, r) }},
			{"cut off", &failingReader{data: body[:9], err: io.ErrUnexpectedEOF}, func(r io.Reader) { _, _ = io.Copy(io.Discard, r) }},
		} {
			e, sealed := ingressEvent(t, budget.b(), tc.body, tc.read)
			fp := e.RequestFingerprint
			if fp.Complete || sealed.Complete {
				t.Fatalf("%s, %s: fingerprint %+v (a seal's %+v) is complete; its hash did not see every byte", budget.name, tc.name, fp, sealed)
			}
			if FingerprintLink(fp, whole) || FingerprintLink(whole, fp) {
				t.Fatalf("%s, %s: a partial fingerprint links to the whole one", budget.name, tc.name)
			}
		}
	}
}
