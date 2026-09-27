//go:build !linux

package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// downEndpoint is the base URL of a closed server: without a way to hold
// its port here, the port may be given to another listener.
func downEndpoint(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	s.Close()
	return s.URL
}
