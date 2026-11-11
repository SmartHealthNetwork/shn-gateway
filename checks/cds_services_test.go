package checks

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// runCDS runs one cds-services probe of srvURL through plain, with auth as
// its AuthClient's transport when set.
func runCDS(t *testing.T, srvURL string, plain *http.Client, auth http.RoundTripper, credential string) Result {
	t.Helper()
	tgt := Target{ID: "PAYER_DAVINCI_CDS_SERVICES", Kind: KindCDSServices, URL: srvURL,
		Headers: http.Header{"X-Route": {"plan-7"}}, Credential: credential}
	if auth != nil {
		tgt.AuthClient = &http.Client{Transport: auth}
	}
	rn := NewRunner([]Target{tgt}, plain, newFakeClock(time.Unix(0, 0)).Now)
	results, err := rn.Run(context.Background())
	if err != nil || len(results) != 1 {
		t.Fatalf("Run: %v %v", results, err)
	}
	return results[0]
}

// cdsServer answers GET /cds-services with status and body, recording each
// request's path, Authorization and route header.
func cdsServer(t *testing.T, status int, body string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+"|"+r.Header.Get("Authorization")+"|"+r.Header.Get("X-Route")+"|"+r.Header.Get("Accept"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// TestCDSServices_ReadsTheListingAsCRDDoes: the check GETs {base}/cds-services
// (the URL CRD's own listing read builds) with the target's headers, through
// the AuthClient when one is set and the plain client otherwise, and passes on
// a 2xx listing CRD can use (cdslisting.Read), naming the credential, the
// number of services and what the read cannot prove.
func TestCDSServices_ReadsTheListingAsCRDDoes(t *testing.T) {
	const listing = `{"services":[{"id":"a","hook":"order-sign"},{"id":"b","hook":"order-dispatch"}]}`
	for _, c := range []struct {
		name, credential, wantAuth, wantWith string
		auth                                 http.RoundTripper
	}{
		{name: "credential", credential: "the CDS client JWT", wantAuth: "Bearer good", wantWith: "with the CDS client JWT",
			auth: bearerClient{tok: "good", base: http.DefaultTransport}},
		{name: "no credential", wantAuth: "", wantWith: "with no credential"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, seen := cdsServer(t, http.StatusOK, listing)
			res := runCDS(t, srv.URL, srv.Client(), c.auth, c.credential)
			if !res.OK || res.Failure != nil {
				t.Fatalf("result %+v, want a pass", res)
			}
			want := "GET /cds-services|" + c.wantAuth + "|plan-7|application/json"
			if len(*seen) != 1 || (*seen)[0] != want {
				t.Fatalf("the CDS service received %v, want one %q", *seen, want)
			}
			if !strings.Contains(res.Detail, "(2 services) "+c.wantWith) || !strings.Contains(res.Detail, cdsListingProof) {
				t.Fatalf("detail %q, want the count, %q and what the read proves", res.Detail, c.wantWith)
			}
			if res.Target != srv.URL {
				t.Fatalf("target %q, want %q", res.Target, srv.URL)
			}
		})
	}
	// An empty listing is a listing.
	srv, _ := cdsServer(t, http.StatusOK, `{"services":[]}`)
	if res := runCDS(t, srv.URL, srv.Client(), nil, ""); !res.OK {
		t.Fatalf("empty listing: %+v, want a pass", res)
	}
}

// TestCDSServices_FailsWithTheAnswersClass: a refusal of the credential, any
// other failing status, no answer, a credential that could not be obtained,
// and a 2xx that is not a listing each fail with their own code.
func TestCDSServices_FailsWithTheAnswersClass(t *testing.T) {
	for _, c := range []struct {
		name     string
		status   int
		body     string
		auth     http.RoundTripper
		wantCode string
		wantHint string
	}{
		{name: "401", status: http.StatusUnauthorized, wantCode: FailCredentialRejected, wantHint: "HTTP 401"},
		{name: "403", status: http.StatusForbidden, wantCode: FailCredentialRejected, wantHint: "HTTP 403"},
		{name: "404", status: http.StatusNotFound, wantCode: FailHTTPStatus, wantHint: "HTTP 404"},
		{name: "500", status: http.StatusInternalServerError, wantCode: FailHTTPStatus, wantHint: "HTTP 500"},
		{name: "not JSON", status: http.StatusOK, body: `<html>`, wantCode: FailInvalidCDSServices},
		{name: "a JSON array", status: http.StatusOK, body: `[]`, wantCode: FailInvalidCDSServices},
		{name: "no services", status: http.StatusOK, body: `{"cards":[]}`, wantCode: FailInvalidCDSServices, wantHint: "lists no services array"},
		{name: "services null", status: http.StatusOK, body: `{"services":null}`, wantCode: FailInvalidCDSServices, wantHint: "lists no services array"},
		{name: "services not an array", status: http.StatusOK, body: `{"services":{"id":"a"}}`, wantCode: FailInvalidCDSServices},
		{name: "an entry with no id", status: http.StatusOK, body: `{"services":[{"hook":"order-sign"}]}`, wantCode: FailInvalidCDSServices, wantHint: "service 0 has no id or no hook"},
		{name: "an entry with no hook", status: http.StatusOK, body: `{"services":[{"id":"a","hook":"order-sign"},{"id":"b"}]}`, wantCode: FailInvalidCDSServices, wantHint: "service 1 has no id or no hook"},
		{name: "services named in another case", status: http.StatusOK, body: `{"Services":[{"id":"a","hook":"order-sign"}]}`, wantCode: FailInvalidCDSServices, wantHint: "lists no services array"},
		{name: "an id named in another case", status: http.StatusOK, body: `{"services":[{"ID":"a","hook":"order-sign"}]}`, wantCode: FailInvalidCDSServices, wantHint: "service 0 has no id or no hook"},
		{name: "a hook named in another case", status: http.StatusOK, body: `{"services":[{"id":"a","HOOK":"order-sign"}]}`, wantCode: FailInvalidCDSServices, wantHint: "service 0 has no id or no hook"},
		{name: "an id that is not a string", status: http.StatusOK, body: `{"services":[{"id":7,"hook":"order-sign"}]}`, wantCode: FailInvalidCDSServices},
		{name: "an entry that is not an object", status: http.StatusOK, body: `{"services":[1]}`, wantCode: FailInvalidCDSServices},
		{name: "trailing data", status: http.StatusOK, body: `{"services":[]} junk`, wantCode: FailInvalidCDSServices},
		{name: "credential unavailable, with why", status: http.StatusOK, body: `{"services":[]}`,
			auth: bearerClient{err: &CredentialUnavailable{Hint: "no issuer yet"}}, wantCode: FailCredentialRejected, wantHint: "no issuer yet"},
		{name: "token endpoint refused", status: http.StatusOK, body: `{"services":[]}`,
			auth: bearerClient{err: &StatusError{Code: 400}}, wantCode: FailCredentialRejected, wantHint: "HTTP 400"},
		{name: "credential unavailable", status: http.StatusOK, body: `{"services":[]}`,
			auth: bearerClient{err: ErrCredential}, wantCode: FailCredentialRejected},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, seen := cdsServer(t, c.status, c.body)
			res := runCDS(t, srv.URL, srv.Client(), c.auth, "the access token")
			if res.OK || res.Failure == nil || res.Failure.Code != c.wantCode || (c.wantHint != "" && res.Failure.Hint != c.wantHint) {
				t.Fatalf("result %+v, want %s %q", res, c.wantCode, c.wantHint)
			}
			if c.auth != nil && len(*seen) != 0 {
				t.Fatalf("a credential that could not be obtained still reached the service: %v", *seen)
			}
			if c.status == http.StatusUnauthorized && !strings.Contains(res.Detail, "refused its listing read with the access token") {
				t.Fatalf("detail %q, want the refusal named with its credential", res.Detail)
			}
		})
	}
	// No answer at all.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	res := runCDS(t, "http://"+addr, http.DefaultClient, nil, "")
	if res.OK || res.Failure == nil || res.Failure.Code != FailUnreachable {
		t.Fatalf("unreachable: %+v, want %s", res, FailUnreachable)
	}
}
