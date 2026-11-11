package nosniff

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every answer carries the header, whatever its status and however the
// handler writes it, and the handler's own Content-Type is left as it set it.
func TestHandler_EveryAnswerCarriesNosniff(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		ct   string
	}{
		{"an answer with a body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<p>"))
		}, "text/html; charset=utf-8"},
		{"a status alone", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }, ""},
		{"http.Error", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusUnauthorized) }, "text/plain; charset=utf-8"},
		{"an answer with no Content-Type", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }, "text/plain; charset=utf-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Handler(tc.h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if got := rec.Result().Header.Values(Header); len(got) != 1 || got[0] != Value {
				t.Fatalf("%s = %q, want exactly %q", Header, got, Value)
			}
			if got := rec.Result().Header.Get("Content-Type"); got != tc.ct {
				t.Fatalf("Content-Type %q, want %q as the handler wrote it", got, tc.ct)
			}
		})
	}
}
