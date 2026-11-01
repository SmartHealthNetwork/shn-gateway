//go:build !shn_coreonly

package engine

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer's system that answers gzip over a real socket: the transport asks for
// it and decodes it, so the bytes carried are not the ones a strong ETag named.
// That ETag is left out and recorded. A weak one, and a strong one on bytes the
// system sent as they are, are carried.
func TestNativePost_AStrongETagOnADecodedAnswerIsLeftOut(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle"}`)
	for name, c := range map[string]struct {
		etag     string
		gzipped  bool
		carried  bool
		recorded bool
	}{
		"strong, decoded":   {`"gzip-variant-1"`, true, false, true},
		"unquoted, decoded": {"gzip-variant-1", true, false, true},
		"weak, decoded":     {`W/"variant-1"`, true, true, false},
		"strong, as sent":   {`"identity-1"`, false, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			var accepted string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				accepted = r.Header.Get("Accept-Encoding")
				w.Header().Set("Content-Type", "application/fhir+json")
				w.Header().Set("ETag", c.etag)
				w.Header().Set("Retry-After", "30")
				if !c.gzipped {
					_, _ = w.Write(body)
					return
				}
				w.Header().Set("Content-Encoding", "gzip")
				z := gzip.NewWriter(w)
				_, _ = z.Write(body)
				_ = z.Close()
			}))
			defer srv.Close()
			x := &exchangeRecorder{}
			ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
			n := &nativeResponder{client: srv.Client()}
			up, _, err := n.post(ctx, srv.URL, "/Claim/$submit", relayedResponse([]byte(`{}`)), "pas-claim", "PAS")
			if err != nil {
				t.Fatal(err)
			}
			if accepted != "gzip" {
				t.Fatalf("the transport sent Accept-Encoding %q: this row proves nothing", accepted)
			}
			if string(up.raw) != string(body) {
				t.Fatalf("read %q", up.raw)
			}
			if _, got := up.headers[shnsdk.FrameHeaderETag]; got != c.carried {
				t.Fatalf("ETag carried %v, want %v: %v", got, c.carried, up.headers)
			}
			if up.headers[shnsdk.FrameHeaderRetryAfter] != "30" {
				t.Fatalf("Retry-After %v", up.headers)
			}
			want := []string(nil)
			if c.recorded {
				want = []string{"ETag:" + AnswerHeaderDecodedFromGzip}
			}
			if !slices.Equal(x.rec.AnswerHeadersNotCarried, want) {
				t.Fatalf("recorded %v, want %v", x.rec.AnswerHeadersNotCarried, want)
			}
		})
	}
}
