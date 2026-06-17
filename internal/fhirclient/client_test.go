package fhirclient_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

func TestSearch_ParsesBundleAndBuildsURL(t *testing.T) {
	var gotPath, gotQuery, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAccept = r.URL.Path, r.URL.RawQuery, r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p1"}}]}`))
	}))
	defer srv.Close()

	c := fhirclient.New(srv.URL, nil)
	b, err := c.Search(context.Background(), "Patient", url.Values{"identifier": {"urn:shn:member|MBR-COVERED"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/Patient" {
		t.Errorf("path = %q, want /Patient", gotPath)
	}
	if gotQuery != "identifier=urn%3Ashn%3Amember%7CMBR-COVERED" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotAccept != "application/fhir+json" {
		t.Errorf("Accept = %q, want application/fhir+json", gotAccept)
	}
	if b == nil || len(b.Entry) != 1 {
		t.Fatalf("want 1 entry, got %+v", b)
	}
}

func TestSearch_MalformedJSONIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not json at all")
	}))
	defer srv.Close()
	if _, err := fhirclient.New(srv.URL, nil).Search(context.Background(), "Patient", nil); err == nil {
		t.Fatal("want error for malformed JSON body, got nil")
	}
}

func TestSearch_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := fhirclient.New(srv.URL, nil).Search(context.Background(), "Patient", nil); err == nil {
		t.Fatal("want error on 500, got nil")
	}
}
