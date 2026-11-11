//go:build !shn_coreonly

package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

// listingCall is one read of the CDS service listing a payer's system
// received: the URL called and the Authorization it carried.
type listingCall struct{ url, auth string }

// cdsListingPayer is a payer's system that issues an access token at /token
// and answers its CDS service listing at /cds-services, or, with redirect
// set, redirects that read (307, same origin) to /real/cds-services and
// answers there. It records every listing read.
func cdsListingPayer(t *testing.T, redirect bool) (*httptest.Server, func() []listingCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []listingCall
	srv := httptest.NewServer(nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"access-token-1","token_type":"bearer","expires_in":300}`)
			return
		case "/cds-services", "/real/cds-services":
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		calls = append(calls, listingCall{srv.URL + r.URL.Path, r.Header.Get("Authorization")})
		mu.Unlock()
		if redirect && r.URL.Path == "/cds-services" {
			http.Redirect(w, r, "/real/cds-services", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"services":[{"id":"svc","hook":"order-sign"}]}`)
	})
	t.Cleanup(srv.Close)
	return srv, func() []listingCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]listingCall(nil), calls...)
	}
}

// credentialKind is what an Authorization value carries: "jwt", "access
// token" or "none".
func credentialKind(auth string) string {
	switch {
	case auth == "":
		return "none"
	case auth == "Bearer access-token-1":
		return "access token"
	case strings.HasPrefix(auth, "Bearer ") && strings.Count(auth, ".") == 2:
		return "jwt"
	}
	return "other: " + auth
}

// runCDSCheck runs b's checks and returns the CDS listing check's result.
func runCDSCheck(t *testing.T, b built) checks.Result {
	t.Helper()
	results, err := b.checksRunner.Run(context.Background())
	if err != nil {
		t.Fatalf("checks: %v", err)
	}
	for _, r := range results {
		if r.ID == payerCDSCheckID {
			return r
		}
	}
	t.Fatalf("no %s among the checks: %+v", payerCDSCheckID, results)
	return checks.Result{}
}

// TestPayerCDSCheck_CarriesWhatCRDCarries: in each credential mode, the
// connectivity check reads the CDS service listing with the same kind of
// credential as CRD's own listing read at boot (the client JWT, the access
// token, or none), at the same URL (here the base CRD falls back to with no
// CDS base set), and passes. A row fails if the two ever diverge.
func TestPayerCDSCheck_CarriesWhatCRDCarries(t *testing.T) {
	path, _ := writeCDSKey(t)
	for _, c := range []struct {
		name  string
		extra func(payerURL string) map[string]string
		want  string
	}{
		{"client JWT", func(u string) map[string]string {
			return map[string]string{"PAYER_DAVINCI_TOKEN_URL": u + "/token", "PAYER_DAVINCI_CLIENT_ID": "shn-gateway",
				"PAYER_DAVINCI_CLIENT_KEY": path, "PAYER_DAVINCI_CLIENT_ALG": "ES384"}
		}, "jwt"},
		{"access token", func(u string) map[string]string {
			return map[string]string{"PAYER_DAVINCI_TOKEN_URL": u + "/token", "PAYER_DAVINCI_CLIENT_ID": "shn-gateway",
				"PAYER_DAVINCI_CLIENT_SECRET": "s"}
		}, "access token"},
		{"access token kept with a key", func(u string) map[string]string {
			return map[string]string{"PAYER_DAVINCI_TOKEN_URL": u + "/token", "PAYER_DAVINCI_CLIENT_ID": "shn-gateway",
				"PAYER_DAVINCI_CLIENT_KEY": path, "PAYER_DAVINCI_CLIENT_ALG": "ES384", "PAYER_DAVINCI_CDS_AUTH": "access-token"}
		}, "access token"},
		{"no credential", func(string) map[string]string { return map[string]string{} }, "none"},
		{"a key with no token endpoint", func(string) map[string]string {
			return map[string]string{"PAYER_DAVINCI_CLIENT_KEY": path, "PAYER_DAVINCI_CLIENT_ALG": "ES384"}
		}, "jwt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			payer, calls := cdsListingPayer(t, false)
			b, out, err := buildForTest(t, payerBootEnv(t, payer.URL, "https://payer-gw.example", c.extra(payer.URL)))
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			boot := calls()
			res := runCDSCheck(t, b)
			check := calls()[len(boot):]
			if len(boot) != 1 || len(check) != 1 {
				t.Fatalf("listing reads: %v at boot, %v by the check; want one each", boot, check)
			}
			if boot[0].url != payer.URL+"/cds-services" || check[0].url != boot[0].url {
				t.Fatalf("CRD read %s, the check %s; want both %s", boot[0].url, check[0].url, payer.URL+"/cds-services")
			}
			if got, crd := credentialKind(check[0].auth), credentialKind(boot[0].auth); got != crd || got != c.want {
				t.Fatalf("the check carried %s, CRD's listing read %s; want both %s", got, crd, c.want)
			}
			if !res.OK || res.Target != payer.URL {
				t.Fatalf("check %+v, want a pass on %s", res, payer.URL)
			}
		})
	}
}

// TestPayerCDSCheck_ReadsTheCDSBase: with PAYER_DAVINCI_CDS_BASE_URL set, the
// check reads the listing there, as CRD does, and not on the FHIR base.
func TestPayerCDSCheck_ReadsTheCDSBase(t *testing.T) {
	fhir, fhirCalls := cdsListingPayer(t, false)
	cds, cdsCalls := cdsListingPayer(t, false)
	b, out, err := buildForTest(t, payerBootEnv(t, fhir.URL, "https://payer-gw.example", map[string]string{"PAYER_DAVINCI_CDS_BASE_URL": cds.URL}))
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	res := runCDSCheck(t, b)
	if got := cdsCalls(); len(got) != 2 || got[1].url != cds.URL+"/cds-services" {
		t.Fatalf("the CDS base received %v, want CRD's read and the check's", got)
	}
	if got := fhirCalls(); len(got) != 0 {
		t.Fatalf("the FHIR base received listing reads %v", got)
	}
	if !res.OK || res.Target != cds.URL {
		t.Fatalf("check %+v, want a pass on %s", res, cds.URL)
	}
}

// TestPayerCDSCheck_FollowsASameOriginRedirectAsCRDDoes: a CDS service that
// answers its listing read with a same-origin 307 is followed there by CRD's
// own read and by the check alike, each hop with a JWT of its own whose aud
// is the URL that hop called.
func TestPayerCDSCheck_FollowsASameOriginRedirectAsCRDDoes(t *testing.T) {
	path, pk := writeCDSKey(t)
	payer, calls := cdsListingPayer(t, true)
	b, out, err := buildForTest(t, payerBootEnv(t, payer.URL, "https://payer-gw.example", map[string]string{
		"PAYER_DAVINCI_TOKEN_URL": payer.URL + "/token", "PAYER_DAVINCI_CLIENT_ID": "shn-gateway",
		"PAYER_DAVINCI_CLIENT_KEY": path, "PAYER_DAVINCI_CLIENT_ALG": "ES384",
	}))
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if strings.Contains(out, "listing is not readable yet") {
		t.Fatalf("CRD's listing read did not follow the redirect:\n%s", out)
	}
	boot := calls()
	res := runCDSCheck(t, b)
	check := calls()[len(boot):]
	if !res.OK {
		t.Fatalf("check %+v, want a pass through the redirect", res)
	}
	for who, hops := range map[string][]listingCall{"CRD": boot, "the check": check} {
		if len(hops) != 2 || hops[0].url != payer.URL+"/cds-services" || hops[1].url != payer.URL+"/real/cds-services" {
			t.Fatalf("%s read %v, want the listing then the redirect's target", who, hops)
		}
		for _, h := range hops {
			claims := jwt.MapClaims{}
			if _, err := jwt.ParseWithClaims(strings.TrimPrefix(h.auth, "Bearer "), claims, func(*jwt.Token) (any, error) { return &pk.PublicKey, nil }, jwt.WithValidMethods([]string{"ES384"})); err != nil {
				t.Fatalf("%s: %s carried no verifiable JWT: %v", who, h.url, err)
			}
			if claims["aud"] != h.url {
				t.Fatalf("%s: %s carried aud %v", who, h.url, claims["aud"])
			}
		}
	}
}

// TestPayerCDSCheck_JudgesAListingAsCRDDoes: for each answer, the check
// passes exactly when CRD's own listing read (engine.DiscoverCDSServices) can
// use it: a listing CRD refuses is never green on the check.
func TestPayerCDSCheck_JudgesAListingAsCRDDoes(t *testing.T) {
	for _, body := range []string{
		`{"services":[{"id":"a","hook":"order-sign"}]}`,
		`{"services":[]}`,
		`{"services":[{"title":"x"}]}`,
		`{"services":[{"id":"a"}]}`,
		`{"services":[{"hook":"order-sign"}]}`,
		`{"services":[1]}`,
		`{"services":[{"id":1,"hook":"order-sign"}]}`,
		`{"Services":[{"id":"a","hook":"order-sign"}]}`,
		`{"services":[{"ID":"a","hook":"order-sign"}]}`,
		`{"services":[{"id":"a","HOOK":"order-sign"}]}`,
		`{"services":[{"id":"a","hook":"order-sign","ID":"b"}]}`,
		`{"services":[]} junk`,
		`{"services":null}`,
		`{"cards":[]}`,
		`[]`,
		`<html>`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		}))
		_, crdErr := engine.DiscoverCDSServices(context.Background(), srv.Client(), srv.URL, nil)
		cfg := config{Role: "payer", PayerDavinciBaseURL: srv.URL}
		tgt, _ := payerCDSCheckTarget(cfg, shnsdk.NewRegistry(), "")
		results, err := checks.NewRunner([]checks.Target{tgt}, srv.Client(), nil).Run(context.Background())
		srv.Close()
		if err != nil || len(results) != 1 {
			t.Fatalf("%s: checks %v %v", body, results, err)
		}
		if results[0].OK != (crdErr == nil) {
			t.Errorf("%s: the check passes=%v, CRD's listing read err=%v", body, results[0].OK, crdErr)
		}
	}
}

// TestPayerCDSCheck_AuthClientIsMarkedCDS: whatever credential the check
// carries, its client marks the read as a call to the payer's CDS service,
// as CRD's own reads are marked, so the payer's tagged transport records it
// under cds.
func TestPayerCDSCheck_AuthClientIsMarkedCDS(t *testing.T) {
	path, _ := writeCDSKey(t)
	for name, cfg := range map[string]config{
		"client JWT": {Role: "payer", PayerDavinciBaseURL: "https://payer.example", PayerDavinciClientKey: path, PayerDavinciClientAlg: "ES384",
			PayerCDSAuth: payerCDSAuth{mode: cdsAuthClientJWT, iss: "https://payer-gw.example"}},
		"access token": {Role: "payer", PayerDavinciBaseURL: "https://payer.example", PayerDavinciTokenURL: "https://payer.example/token",
			PayerDavinciClientID: "c", PayerDavinciClientSecret: "s"},
	} {
		tgt, ok := payerCDSCheckTarget(cfg, shnsdk.NewRegistry(), "")
		if !ok || tgt.AuthClient == nil {
			t.Fatalf("%s: no authenticated check", name)
		}
		if _, marked := tgt.AuthClient.Transport.(cdsTagged); !marked {
			t.Errorf("%s: the check's client %T does not mark its reads cds", name, tgt.AuthClient.Transport)
		}
	}
}

// TestPayerCDSCheck_NoIssuerSaysSo: a gateway whose CDS client JWT has no
// issuer yet sends nothing, and its check fails credential-rejected saying
// why.
func TestPayerCDSCheck_NoIssuerSaysSo(t *testing.T) {
	path, _ := writeCDSKey(t)
	payer, calls := cdsListingPayer(t, false)
	b, out, err := buildForTest(t, payerBootEnv(t, payer.URL, "", map[string]string{
		"PAYER_DAVINCI_CLIENT_KEY": path, "PAYER_DAVINCI_CLIENT_ALG": "ES384",
	}))
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	res := runCDSCheck(t, b)
	if res.OK || res.Failure == nil || res.Failure.Code != checks.FailCredentialRejected || res.Failure.Hint != "the CDS client JWT has no issuer yet" {
		t.Fatalf("check %+v, want credential-rejected naming the missing issuer", res)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("the CDS service received %v with no issuer known", got)
	}
}
