package fhirsor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

// A system of record that redirects a read from https to http is never
// followed there, whether or not the gateway's client carries a bearer (here
// it carries none): nothing reaches the http origin, and the read fails as an
// unavailable system's does, with an error that says it was a refused
// downgrade, so the exchange record names it. Each kind of read is a row: a
// member lookup (a search on the client as given), a read by reference, and a
// patient search (on the client that refuses every redirect).
func TestSoR_AnHTTPSToHTTPRedirectIsNeverFollowed(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[]}`))
	}))
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(tlsSrv.Close)
	s := NewFromURL(tlsSrv.URL+"/fhir", tlsSrv.Client())
	for name, read := range map[string]func(context.Context) error{
		"a member lookup": func(ctx context.Context) error {
			_, _, _, err := s.ResolvePatientContext(ctx, "MBR-1")
			return err
		},
		"a read by reference": func(ctx context.Context) error {
			_, _, err := s.ResolveByReferenceContext(ctx, "Coverage/c1")
			return err
		},
		"a patient search": func(ctx context.Context) error {
			_, err := s.SearchPatientContext(ctx, "Observation", "p1")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			plainHits.Store(0)
			ctx, obs := smartauth.WithRedirectObservation(context.Background())
			err := read(ctx)
			if !errors.Is(err, smartauth.ErrCleartextRedirect) {
				t.Fatalf("err %v, want a refused downgrade", err)
			}
			// The read fails as it does for any redirect it does not follow:
			// unavailable, or, for a search, its refused redirect.
			var readErr *engine.SoRReadError
			var searchErr *engine.SearchError
			switch {
			case errors.As(err, &readErr):
				if readErr.Kind != engine.SoRUnavailable {
					t.Fatalf("err %#v, want an unavailable read", err)
				}
			case errors.As(err, &searchErr):
				if searchErr.Outcome != engine.SearchMalformed || searchErr.Reason != "redirect refused" {
					t.Fatalf("err %#v, want the search's refused redirect", err)
				}
			default:
				t.Fatalf("err %#v, want a read or search error", err)
			}
			if plainHits.Load() != 0 {
				t.Fatalf("the http origin received %d requests, want none", plainHits.Load())
			}
			if hops := obs.Hops(); len(hops) != 1 || hops[0] != smartauth.RedirectCleartextRefused {
				t.Fatalf("hops %q, want the refused downgrade", hops)
			}
		})
	}
}

// The caller's client is not changed: the policy is the SoR's own copy's.
func TestNewFromURL_LeavesTheCallersClientUnchanged(t *testing.T) {
	c := &http.Client{}
	_ = NewFromURL("https://sor.example/fhir", c)
	if c.CheckRedirect != nil {
		t.Fatal("the caller's client gained a redirect policy")
	}
}

// A token endpoint that redirects the token request from https to http is a
// token failure, as before: the read is not recorded as the system's own
// redirect, for a read and for a search.
func TestSoR_ATokenEndpointDowngradeIsATokenFailure(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(plain.Close)
	tokenSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/token", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(tokenSrv.Close)
	hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tokenSrv.URL + "/token", ClientID: "gw", ClientSecret: "OPAQUE-VALUE-3686", Scope: "system/*.read", Clock: time.Now, HTTPClient: tokenSrv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	s := NewFromURL(plain.URL+"/fhir", hc)
	_, _, _, err = s.ResolvePatientContext(context.Background(), "MBR-1")
	var readErr *engine.SoRReadError
	if !errors.As(err, &readErr) || readErr.Kind != engine.SoRUnavailable || errors.Is(err, smartauth.ErrCleartextRedirect) {
		t.Fatalf("read err %#v, want an unavailable read that does not name the system's own redirect", err)
	}
	_, err = s.SearchPatientContext(context.Background(), "Observation", "p1")
	var searchErr *engine.SearchError
	if !errors.As(err, &searchErr) || searchErr.Outcome != engine.SearchUnavailable || errors.Is(err, smartauth.ErrCleartextRedirect) {
		t.Fatalf("search err %#v, want an unavailable search that does not name the system's own redirect", err)
	}
}
