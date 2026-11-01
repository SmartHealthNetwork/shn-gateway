package contentcoding

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A strong ETag names exact bytes, so coding the answer would make it untrue.
// The answer stays as it is, which is always legal.
func TestCompress_AStrongETagStaysUncoded(t *testing.T) {
	body := strings.Repeat("answer ", 400)
	for etag, coded := range map[string]bool{
		`"v1"`:    false,
		"v1":      false,
		`W/"v1"`:  true,
		` W/"v1"`: true,
		"":        true,
	} {
		h := Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if etag != "" {
				w.Header().Set("ETag", etag)
			}
			w.Header().Set("Content-Type", "application/fhir+json")
			_, _ = w.Write([]byte(body))
		}))
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if got := rec.Header().Get("Content-Encoding") == "gzip"; got != coded {
			t.Errorf("ETag %q: coded %v, want %v", etag, got, coded)
		}
		if !coded && rec.Body.String() != body {
			t.Errorf("ETag %q: the body changed", etag)
		}
	}
}
