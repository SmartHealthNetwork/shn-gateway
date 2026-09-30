package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	checks "github.com/SmartHealthNetwork/shn-gateway/checks"
)

// authedPartner is a partner FHIR base that answers /metadata only to the
// bearer its token endpoint mints, as some partner gateways do. The token
// endpoint mints only for the client id and secret it is given, so a check
// built from another partner's credentials fails.
func authedPartner(t *testing.T, tokenStatus int, clientID, secret string) (base, token string, mints *int32) {
	t.Helper()
	var n int32
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		if tokenStatus != http.StatusOK {
			w.WriteHeader(tokenStatus)
			return
		}
		if r.PostFormValue("client_id") != clientID || r.PostFormValue("client_secret") != secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"minted-` + clientID + `","expires_in":3600}`))
	}))
	t.Cleanup(tok.Close)
	fhir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fhir/metadata" {
			// the davinci-configuration companion probe; not published here
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer minted-"+clientID {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`))
	}))
	t.Cleanup(fhir.Close)
	return fhir.URL + "/fhir", tok.URL, &n
}

func runTarget(t *testing.T, cfg config, id string) checks.Result {
	t.Helper()
	res, err := checks.NewRunner(checkTargets(cfg), http.DefaultClient, nil).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no %s result in %+v", id, res)
	return checks.Result{}
}

// The partner-base /metadata checks authenticate with the same SMART config
// the gateway's own calls to that base use, and pass where only an
// authenticated read answers; the anonymous 403 is recorded, not failed.
func TestCheckTargets_PartnerMetadataAuthenticates(t *testing.T) {
	payerBase, payerTok, _ := authedPartner(t, http.StatusOK, "c1", "s1")
	sorBase, sorTok, _ := authedPartner(t, http.StatusOK, "c2", "s2")
	cfg := config{
		PayerDavinciBaseURL: payerBase, PayerDavinciTokenURL: payerTok,
		PayerDavinciClientID: "c1", PayerDavinciClientSecret: "s1",
		FHIRDataURL: sorBase, FHIRTokenURL: sorTok,
		FHIRClientID: "c2", FHIRClientSecret: "s2",
	}
	for _, id := range []string{"PAYER_DAVINCI_BASE_URL", "FHIR_DATA_URL"} {
		got := runTarget(t, cfg, id)
		if !got.OK {
			t.Errorf("%s: %q %+v, want a pass", id, got.Detail, got.Failure)
		}
		if got.Anonymous == nil || got.Anonymous.OK || got.Anonymous.Detail != "HTTP 403" {
			t.Errorf("%s: anonymous = %+v, want the 403 recorded", id, got.Anonymous)
		}
	}
	for _, tgt := range checkTargets(cfg) {
		authed := tgt.ID == "PAYER_DAVINCI_BASE_URL" || tgt.ID == "FHIR_DATA_URL"
		if (tgt.AuthClient != nil) != authed {
			t.Errorf("%s: AuthClient set = %v, want %v", tgt.ID, tgt.AuthClient != nil, authed)
		}
	}
}

// A token the partner refuses fails the check as a credential failure with
// its status, and a key that does not load fails it without calling the token
// endpoint.
func TestCheckTargets_PartnerMetadataCredentialFailures(t *testing.T) {
	base, tok, _ := authedPartner(t, http.StatusUnauthorized, "c1", "s1")
	cfg := config{PayerDavinciBaseURL: base, PayerDavinciTokenURL: tok, PayerDavinciClientID: "c1", PayerDavinciClientSecret: "s1"}
	got := runTarget(t, cfg, "PAYER_DAVINCI_BASE_URL")
	if got.OK || got.Failure == nil || got.Failure.Code != checks.FailCredentialRejected || got.Failure.Hint != "HTTP 401" {
		t.Errorf("token refused: %q %+v, want credential-rejected HTTP 401", got.Detail, got.Failure)
	}

	base, tok, mints := authedPartner(t, http.StatusOK, "c1", "s1")
	cfg = config{PayerDavinciBaseURL: base, PayerDavinciTokenURL: tok, PayerDavinciClientID: "c1",
		PayerDavinciClientKey: "/nonexistent/key.pem", PayerDavinciClientAlg: "ES384"}
	got = runTarget(t, cfg, "PAYER_DAVINCI_BASE_URL")
	if got.OK || got.Failure == nil || got.Failure.Code != checks.FailCredentialRejected || got.Detail != "credential check failed" {
		t.Errorf("key unloadable: %q %+v, want credential-rejected", got.Detail, got.Failure)
	}
	if *mints != 0 {
		t.Errorf("the token endpoint was called %d times with no key", *mints)
	}

	// A token obtained and a base that is down: the base is unreachable, not
	// the credential rejected.
	_, tok, mints = authedPartner(t, http.StatusOK, "c1", "s1")
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL + "/fhir"
	gone.Close()
	cfg = config{PayerDavinciBaseURL: goneURL, PayerDavinciTokenURL: tok, PayerDavinciClientID: "c1", PayerDavinciClientSecret: "s1"}
	got = runTarget(t, cfg, "PAYER_DAVINCI_BASE_URL")
	if got.OK || got.Failure == nil || got.Failure.Code != checks.FailUnreachable || strings.HasPrefix(got.Detail, "credential check failed") {
		t.Errorf("base down: %q %+v, want unreachable", got.Detail, got.Failure)
	}
	if *mints == 0 {
		t.Error("the token was never minted, so the row proves nothing")
	}
}

// A token the probe cannot obtain for any other reason (its endpoint down, or
// an answer that is not a token) fails the check as a fixed credential
// failure: nothing of the token endpoint (its URL, its query, its body) reaches
// the result.
func TestCheckTargets_PartnerMetadataTokenErrorsRedacted(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL + "/token?tenant=SECRETQ"
	down.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>SECRETBODY</html>"))
	}))
	t.Cleanup(garbage.Close)
	for name, tokenURL := range map[string]string{
		"token endpoint down":      downURL,
		"token answer not a token": garbage.URL + "/token?tenant=SECRETQ",
	} {
		t.Run(name, func(t *testing.T) {
			base, _, _ := authedPartner(t, http.StatusOK, "c1", "s1")
			cfg := config{PayerDavinciBaseURL: base, PayerDavinciTokenURL: tokenURL, PayerDavinciClientID: "c1", PayerDavinciClientSecret: "s1"}
			got := runTarget(t, cfg, "PAYER_DAVINCI_BASE_URL")
			if got.OK || got.Failure == nil || got.Failure.Code != checks.FailCredentialRejected ||
				got.Detail != "credential check failed" {
				t.Fatalf("%q %+v, want credential-rejected", got.Detail, got.Failure)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			tokHost := strings.TrimPrefix(strings.SplitN(tokenURL, "/token", 2)[0], "http://")
			for _, leak := range []string{"SECRETQ", "SECRETBODY", tokHost, "smartauth"} {
				if strings.Contains(string(b), leak) {
					t.Errorf("result carries %q: %s", leak, b)
				}
			}
		})
	}
}

// Each base authenticates only with its own partner's credential: a base whose
// partner has none is read anonymously, even when the other partner has one.
func TestCheckTargets_PartnerMetadataOneCredentialOnly(t *testing.T) {
	payerBase, payerTok, _ := authedPartner(t, http.StatusOK, "c1", "s1")
	sorBase, sorTok, _ := authedPartner(t, http.StatusOK, "c2", "s2")
	for _, tc := range []struct {
		name            string
		cfg             config
		authed, anonyID string
	}{
		{"payer credential only", config{PayerDavinciBaseURL: payerBase, PayerDavinciTokenURL: payerTok,
			PayerDavinciClientID: "c1", PayerDavinciClientSecret: "s1", FHIRDataURL: sorBase}, "PAYER_DAVINCI_BASE_URL", "FHIR_DATA_URL"},
		{"system of record credential only", config{PayerDavinciBaseURL: payerBase, FHIRDataURL: sorBase,
			FHIRTokenURL: sorTok, FHIRClientID: "c2", FHIRClientSecret: "s2"}, "FHIR_DATA_URL", "PAYER_DAVINCI_BASE_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runTarget(t, tc.cfg, tc.authed); !got.OK || got.Anonymous == nil {
				t.Errorf("%s: %q anonymous=%+v, want an authenticated pass", tc.authed, got.Detail, got.Anonymous)
			}
			if got := runTarget(t, tc.cfg, tc.anonyID); got.OK || got.Detail != "HTTP 403" || got.Anonymous != nil {
				t.Errorf("%s: %q %+v anonymous=%+v, want the anonymous read's 403", tc.anonyID, got.Detail, got.Failure, got.Anonymous)
			}
			for _, tgt := range checkTargets(tc.cfg) {
				if tgt.ID == tc.anonyID && tgt.AuthClient != nil {
					t.Errorf("%s has an AuthClient without its own credential", tgt.ID)
				}
			}
		})
	}
}

// A partner without credentials is probed anonymously, as before.
func TestCheckTargets_PartnerMetadataWithoutCredentials(t *testing.T) {
	base, _, _ := authedPartner(t, http.StatusOK, "c1", "s1")
	got := runTarget(t, config{PayerDavinciBaseURL: base}, "PAYER_DAVINCI_BASE_URL")
	if got.OK || got.Detail != "HTTP 403" || got.Anonymous != nil {
		t.Errorf("%q anonymous=%+v, want the anonymous read's 403 as the result", got.Detail, got.Anonymous)
	}
}
