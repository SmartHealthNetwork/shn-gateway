package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if got := check(srv.URL); got != 0 {
		t.Fatalf("check(200) = %d, want 0", got)
	}
}

func TestCheckNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if got := check(srv.URL); got != 1 {
		t.Fatalf("check(503) = %d, want 1", got)
	}
}

func TestCheckUnreachable(t *testing.T) {
	if got := check("http://127.0.0.1:1"); got != 1 {
		t.Fatalf("check(unreachable) = %d, want 1", got)
	}
}
