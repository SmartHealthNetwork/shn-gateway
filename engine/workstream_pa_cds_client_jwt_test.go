//go:build !shn_coreonly

package engine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

// accessTokenTransport stands in for the payer client's SMART bearer
// (smartauth.NewHTTPClient): every request it sends carries the access token.
type accessTokenTransport struct{}

const testAccessToken = "Bearer access-token-1"

func (accessTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header.Set("Authorization", testAccessToken)
	return http.DefaultTransport.RoundTrip(r2)
}

// authSeen is one request the payer's system received: its path, its
// Authorization and the fixed header a backend routes on.
type authSeen struct{ method, path, auth, tenant string }

// cdsAuthPayer is a payer's system serving CRD (the listing and its hooks),
// DTR and PAS at one base, recording how each request authenticated.
func cdsAuthPayer(t *testing.T) (*httptest.Server, func() []authSeen) {
	t.Helper()
	const pkg = `{"resourceType":"Bundle","type":"collection","entry":[]}`
	pasAnswer := fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"REF-1","preAuthPeriod":{"end":"2030-01-01"}}`), true)
	crdAnswer := realCRDAnswer(t)
	var mu sync.Mutex
	var seen []authSeen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, authSeen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Tenant")})
		mu.Unlock()
		switch {
		case r.URL.Path == "/cds-services":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"services": referencePayerServices})
		case strings.HasPrefix(r.URL.Path, "/cds-services/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(crdAnswer)
		case r.URL.Path == "/Questionnaire/$questionnaire-package":
			w.Header().Set("Content-Type", "application/fhir+json")
			_, _ = io.WriteString(w, pkg)
		default:
			w.Header().Set("Content-Type", "application/fhir+json")
			_, _ = w.Write(pasAnswer)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []authSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]authSeen(nil), seen...)
	}
}

// TestCDSClientJWT_OnlyCRDCarriesTheJWT: on a payer's gateway whose CDS calls
// carry a client JWT (WithCDSClient), the CDS service listing and each CRD
// hook carry a JWT that verifies against the client's JWK Set, its aud the
// exact URL called on the base CRD falls back to when no CDS base is set,
// while DTR and PAS carry the payer client's access token as before; the
// payer's fixed backend headers go on all of them. Without the CDS client
// (access-token mode), CRD carries the access token as DTR and PAS do.
func TestCDSClientJWT_OnlyCRDCarriesTheJWT(t *testing.T) {
	pk, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := smartauth.NewCDSClientKey("ES384", pk, "")
	if err != nil {
		t.Fatal(err)
	}
	conformant := originatorBuiltConformantBundle(t, "MBR-COVERED")
	headers := http.Header{"X-Tenant": {"plan-7"}}
	for _, mode := range []string{"client-jwt", "access-token"} {
		t.Run(mode, func(t *testing.T) {
			srv, seen := cdsAuthPayer(t)
			opts := []NativeOption{WithCRDService("order-sign-crd"), WithPASStore(newCensusSoR()), WithBackendHeaders(headers)}
			if mode == "client-jwt" {
				cds, err := smartauth.NewClientJWTHTTPClient(key, smartauth.ClientJWTConfig{
					Issuer: func() (string, bool) { return "https://payer-gw.example", true },
				})
				if err != nil {
					t.Fatal(err)
				}
				opts = append(opts, WithCDSClient(cds))
			}
			n := NewNativeResponder(&http.Client{Transport: accessTokenTransport{}}, srv.URL, fixedClock, opts...)
			if res, err := n.Handle(context.Background(), "crd-order-select", "c", "p", cdsRequest("order-sign")); err != nil || res.Status != 0 {
				t.Fatalf("CRD: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			if _, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "c", "p", dtrFetchReq); err != nil {
				t.Fatalf("DTR: %v", err)
			}
			if res, err := n.Handle(context.Background(), "pas-claim", "c", "PCI-1", conformant); err != nil || res.Status != 0 {
				t.Fatalf("PAS: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			got := seen()
			paths := map[string]bool{}
			for _, s := range got {
				paths[s.path] = true
				if s.tenant != "plan-7" {
					t.Errorf("%s %s: X-Tenant %q, want the backend header", s.method, s.path, s.tenant)
				}
				crd := strings.HasPrefix(s.path, "/cds-services")
				if !crd || mode == "access-token" {
					if s.auth != testAccessToken {
						t.Errorf("%s %s: Authorization %q, want the access token", s.method, s.path, s.auth)
					}
					continue
				}
				raw, ok := strings.CutPrefix(s.auth, "Bearer ")
				if !ok || raw == "access-token-1" {
					t.Errorf("%s %s: Authorization %q, want a client JWT", s.method, s.path, s.auth)
					continue
				}
				claims := jwt.MapClaims{}
				if _, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return &pk.PublicKey, nil }, jwt.WithValidMethods([]string{"ES384"})); err != nil {
					t.Errorf("%s %s: JWT does not verify: %v", s.method, s.path, err)
				}
				if want := srv.URL + s.path; claims["aud"] != want {
					t.Errorf("%s %s: aud %v, want the exact URL called %s", s.method, s.path, claims["aud"], want)
				}
			}
			for _, p := range []string{"/cds-services", "/cds-services/order-sign-crd", "/Questionnaire/$questionnaire-package", "/Claim/$submit"} {
				if !paths[p] {
					t.Errorf("the payer's system never received %s (got %v)", p, got)
				}
			}
		})
	}
}

// cdsJWKSPayerConfig is a payer gateway's config serving set as its CDS client
// key's JWK Set.
func cdsJWKSPayerConfig(t *testing.T, role string, set []byte) Config {
	t.Helper()
	hubKey, _ := genED25519(t)
	cfg := mountConfig(t, role)
	cfg.Client = http.DefaultClient
	cfg.HubURL = "http://hub.test"
	cfg.Reg = shnsdk.NewRegistry()
	cfg.Validator = shnsdk.NewFakeValidator()
	cfg.HubTransportPub = hubKey
	cfg.CDSClientJWKS = set
	return cfg
}

// TestCDSClientJWKS_ServedWithNoCredential: a payer's gateway serves its CDS
// client key's public JWK Set to a request with no credential, as RFC 7517's
// media type, exactly the set it was given: one key, its kid, public members
// only. A payer in access-token mode serves none.
func TestCDSClientJWKS_ServedWithNoCredential(t *testing.T) {
	pk, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	key, err := smartauth.NewCDSClientKey("ES384", pk, "")
	if err != nil {
		t.Fatal(err)
	}
	g := mustNew(t, cdsJWKSPayerConfig(t, "payer", key.JWKSet()))
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, smartauth.CDSClientJWKSPath, nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/jwk-set+json" {
		t.Fatalf("GET %s: %d %q, want 200 application/jwk-set+json", smartauth.CDSClientJWKSPath, rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Body.String() != string(key.JWKSet()) {
		t.Fatalf("served %s, want the key's set %s", rec.Body.String(), key.JWKSet())
	}
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil || len(set.Keys) != 1 || set.Keys[0]["kid"] != key.KID() {
		t.Fatalf("served set %s: want one key named %s (%v)", rec.Body.String(), key.KID(), err)
	}
	if err := smartauth.CheckPublicJWKSet(rec.Body.Bytes()); err != nil {
		t.Fatalf("served set: %v", err)
	}

	off := mustNew(t, cdsJWKSPayerConfig(t, "payer", nil))
	rec = httptest.NewRecorder()
	off.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, smartauth.CDSClientJWKSPath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("access-token mode: GET %s = %d, want 404", smartauth.CDSClientJWKSPath, rec.Code)
	}
}

// TestCDSClientJWKS_NewRefusesAPrivateMemberOrAnotherRole: a set carrying a
// private member is never served: New refuses it, for every private member.
// Only a payer serves one.
func TestCDSClientJWKS_NewRefusesAPrivateMemberOrAnotherRole(t *testing.T) {
	pk, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	key, _ := smartauth.NewCDSClientKey("ES384", pk, "")
	for _, m := range []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"} {
		var set map[string][]map[string]any
		_ = json.Unmarshal(key.JWKSet(), &set)
		set["keys"][0][m] = "AQAB"
		planted, _ := json.Marshal(set)
		if _, err := New(cdsJWKSPayerConfig(t, "payer", planted)); err == nil || !strings.Contains(err.Error(), "private member") {
			t.Errorf("a set with %q planted: New = %v, want refused", m, err)
		}
	}
	if _, err := New(cdsJWKSPayerConfig(t, "provider", key.JWKSet())); err == nil || !strings.Contains(err.Error(), `role is "provider"`) {
		t.Errorf("a provider gateway with a CDS client JWK Set: New = %v, want refused for its role", err)
	}
}

// TestCDSClientJWT_NoIssuerSendsNothing: while the gateway knows no issuer for
// its CDS client JWT, no CDS call is sent, with the access token or without
// one: the CRD request is answered 502, and the failed call is this gateway's
// own credential (auth), never a network failure (other).
func TestCDSClientJWT_NoIssuerSendsNothing(t *testing.T) {
	pk, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	key, _ := smartauth.NewCDSClientKey("ES384", pk, "")
	cds, err := smartauth.NewClientJWTHTTPClient(key, smartauth.ClientJWTConfig{Issuer: func() (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	srv, seen := cdsAuthPayer(t)
	// status is the requester's answer to a CRD request: the leg's own
	// refusal, or the gateway's answer to a forward that failed.
	status := func(n *nativeResponder) int {
		res, err := n.Handle(context.Background(), "crd-order-select", "c", "p", cdsRequest("order-sign"))
		if err != nil {
			code, _ := (&Gateway{}).responderFailure(context.Background(), "crd-order-select", err)
			return code
		}
		return res.Status
	}
	// The listing cannot be read without an issuer, and neither can the hook
	// be called once a listing is held.
	listing := NewNativeResponder(&http.Client{Transport: accessTokenTransport{}}, srv.URL, fixedClock, WithCRDService("order-sign-crd"), WithCDSClient(cds))
	if got := status(listing); got != http.StatusBadGateway {
		t.Fatalf("CRD with no issuer, no listing held: %d, want 502", got)
	}
	hook := NewNativeResponder(&http.Client{Transport: accessTokenTransport{}}, srv.URL, fixedClock, WithCRDService("order-sign-crd"), WithCDSClient(cds))
	hook.PrimeCDSServices(referencePayerServices)
	if got := status(hook); got != http.StatusBadGateway {
		t.Fatalf("CRD with no issuer, the listing held: %d, want 502", got)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("the payer's system received %v with no issuer known", got)
	}
	_, callErr := cds.Get(srv.URL + "/cds-services")
	if class := backendClass(0, callErr); class != BackendAuth {
		t.Fatalf("a CDS call with no issuer is classed %q, want %q", class, BackendAuth)
	}
}
