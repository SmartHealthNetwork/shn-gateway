package engine

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Every clinical Store method is promoted from a nil interface and will panic
// if native relay tries to read or write clinical state.
type nativeClinicalStoreForbidden struct{ Store }

func nativeRelayServer(t *testing.T, status int, response, request []byte, mediaOverride ...string) *httptest.Server {
	t.Helper()
	media := "application/json"
	if len(mediaOverride) != 0 {
		media = mediaOverride[0]
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		got, err := io.ReadAll(r.Body)
		if err != nil || r.Method != http.MethodPost || r.URL.Path != "/Claim/$submit" || !bytes.Equal(got, request) {
			t.Errorf("native request method=%s path=%s body=%q err=%v", r.Method, r.URL.Path, got, err)
		}
		w.Header().Set("Content-Type", media)
		w.WriteHeader(status)
		w.Write(response)
	}))
	t.Cleanup(func() {
		srv.Close()
		if calls.Load() != 1 {
			t.Errorf("backend calls=%d want1", calls.Load())
		}
	})
	return srv
}
func assertNativeRelayWithoutClinicalEffects(t *testing.T, res LegResult, body []byte, status int, media string) {
	t.Helper()
	if res.Status != 0 || res.ApplicationStatus != status || res.Response.ContentType() != media || !bytes.Equal(responseBytes(res), body) {
		t.Fatalf("native reply status=%d application=%d media=%s body=%q", res.Status, res.ApplicationStatus, res.Response.ContentType(), responseBytes(res))
	}
	if res.Commit != nil || res.Rollback != nil || len(res.SideEffectFHIR) != 0 {
		t.Fatalf("implicit clinical effects: commit=%v rollback=%v sideeffects=%d", res.Commit != nil, res.Rollback != nil, len(res.SideEffectFHIR))
	}
}
