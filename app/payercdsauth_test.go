//go:build !shn_coreonly

package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

// writeCDSKey writes a fresh ES384 key as PEM under t's temp dir, returning
// its path and the key.
func writeCDSKey(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	pk, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "payer-client.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, pk
}

// TestCheckPayerCDSAuth: the mode defaults to the client JWT when the payer
// client has a signing key and to the access token when it has none (a key
// stored as an empty value is none); an explicit mode is honoured where it can
// be; ISS and JKU take only what the JWT can carry; and none of it is read on
// a gateway that is not a forwarding payer.
func TestCheckPayerCDSAuth(t *testing.T) {
	payer := config{Role: "payer", PayerDavinciBaseURL: "https://payer.example"}
	keyed := payer
	keyed.PayerDavinciClientKey, keyed.PayerDavinciClientAlg = "/keys/k.pem", "ES384"
	emptyKey := payer
	emptyKey.PayerDavinciClientKey, emptyKey.PayerDavinciClientAlg = "", "ES384"
	blankKey := payer
	blankKey.PayerDavinciClientKey, blankKey.PayerDavinciClientAlg = "  ", "ES384"
	keyNoAlg := payer
	keyNoAlg.PayerDavinciClientKey = "/keys/k.pem"
	badAlg := keyed
	badAlg.PayerDavinciClientAlg = "HS256"
	rs256 := keyed
	rs256.PayerDavinciClientAlg = "RS256"
	for _, c := range []struct {
		name            string
		role            string
		cfg             config
		mode, iss, jku  string
		want            string // the mode; "" is refused
		wantErrContains string
	}{
		{name: "key, unset: client JWT", role: "payer", cfg: keyed, want: cdsAuthClientJWT},
		{name: "key with an alg it cannot sign with", role: "payer", cfg: badAlg, wantErrContains: "PAYER_DAVINCI_CLIENT_ALG must be ES384|RS384"},
		{name: "key with RS256, which the payer client does not take", role: "payer", cfg: rs256, wantErrContains: "PAYER_DAVINCI_CLIENT_ALG must be ES384|RS384"},
		{name: "key with a bad alg, access-token", role: "payer", cfg: badAlg, mode: "access-token", want: cdsAuthAccessToken},
		{name: "no key, unset: access token", role: "payer", cfg: payer, want: cdsAuthAccessToken},
		{name: "key stored empty: no key", role: "payer", cfg: emptyKey, want: cdsAuthAccessToken},
		{name: "key stored blank: no key", role: "payer", cfg: blankKey, want: cdsAuthAccessToken},
		{name: "key with no alg: no key", role: "payer", cfg: keyNoAlg, want: cdsAuthAccessToken},
		{name: "key, access-token", role: "payer", cfg: keyed, mode: "access-token", want: cdsAuthAccessToken},
		{name: "key, client-jwt", role: "payer", cfg: keyed, mode: "client-jwt", want: cdsAuthClientJWT},
		{name: "no key, client-jwt", role: "payer", cfg: payer, mode: "client-jwt", wantErrContains: "needs the payer client's signing key"},
		{name: "key stored empty, client-jwt", role: "payer", cfg: emptyKey, mode: "client-jwt", wantErrContains: "needs the payer client's signing key"},
		{name: "unknown mode", role: "payer", cfg: keyed, mode: "jwt", wantErrContains: "must be client-jwt or access-token"},
		{name: "iss", role: "payer", cfg: keyed, iss: "https://issuer.example", want: cdsAuthClientJWT},
		{name: "iss urn", role: "payer", cfg: keyed, iss: "urn:example:payer-gw", want: cdsAuthClientJWT},
		{name: "iss not absolute", role: "payer", cfg: keyed, iss: "payer-gw", wantErrContains: "absolute URI"},
		{name: "jku off", role: "payer", cfg: keyed, jku: "off", want: cdsAuthClientJWT},
		{name: "jku https", role: "payer", cfg: keyed, jku: "https://keys.example/jwks.json", want: cdsAuthClientJWT},
		{name: "jku http", role: "payer", cfg: keyed, jku: "http://keys.example/jwks.json", wantErrContains: "off or an https URL"},
		{name: "iss on access token", role: "payer", cfg: payer, iss: "https://issuer.example", wantErrContains: "apply to the client JWT"},
		{name: "jku on explicit access token", role: "payer", cfg: keyed, mode: "access-token", jku: "off", wantErrContains: "apply to the client JWT"},
		{name: "provider", role: "provider", cfg: config{Role: "provider"}, mode: "client-jwt", wantErrContains: "need ROLE=payer"},
		{name: "payer not forwarding", role: "payer", cfg: config{Role: "payer"}, mode: "client-jwt", wantErrContains: "need ROLE=payer"},
		{name: "provider, nothing set", role: "provider", cfg: config{Role: "provider"}, want: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := checkPayerCDSAuth(c.role, c.cfg, c.mode, c.iss, c.jku)
			if c.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErrContains) {
					t.Fatalf("err %v, want one saying %q", err, c.wantErrContains)
				}
				return
			}
			if err != nil || a.mode != c.want {
				t.Fatalf("mode %q, err %v; want %q", a.mode, err, c.want)
			}
		})
	}
}

// TestLoadConfig_PayerCDSAuthSettings: the three settings are read under their
// names, and a key stored as an empty row reads as no key.
func TestLoadConfig_PayerCDSAuthSettings(t *testing.T) {
	path, _ := writeCDSKey(t)
	cfg, err := loadConfig(func(k string) string {
		return payerEnv(map[string]string{
			"PAYER_DAVINCI_CLIENT_KEY": path, "PAYER_DAVINCI_CLIENT_ALG": "ES384",
			"PAYER_DAVINCI_CDS_AUTH": "client-jwt", "PAYER_DAVINCI_CDS_JWT_ISS": "https://issuer.example", "PAYER_DAVINCI_CDS_JWT_JKU": "off",
		})[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.PayerCDSAuth; got.mode != cdsAuthClientJWT || got.iss != "https://issuer.example" || got.jku != cdsJKUOff {
		t.Fatalf("PayerCDSAuth = %+v", got)
	}
	cfg, err = loadConfig(func(k string) string {
		return payerEnv(map[string]string{
			"PAYER_DAVINCI_TOKEN_URL": "https://auth.example/token", "PAYER_DAVINCI_CLIENT_ID": "c",
			"PAYER_DAVINCI_CLIENT_SECRET": "s", "PAYER_DAVINCI_CLIENT_KEY": "",
		})[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PayerCDSAuth.mode != cdsAuthAccessToken {
		t.Fatalf("a key stored as an empty row: mode %q, want the access token", cfg.PayerCDSAuth.mode)
	}
}

// cdsCall is one request the payer's CDS service stub received.
type cdsCall struct{ method, url, auth string }

// recordingPayer is a payer's system that records how each request
// authenticated and answers the CDS listing with one service.
func recordingPayer(t *testing.T) (*httptest.Server, func() []cdsCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []cdsCall
	srv := httptest.NewServer(nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, cdsCall{r.Method, srv.URL + r.URL.Path, r.Header.Get("Authorization")})
		mu.Unlock()
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"access-token-1","token_type":"bearer","expires_in":300}`)
		case "/cds-services":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"services":[{"id":"svc","hook":"order-sign"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Cleanup(srv.Close)
	return srv, func() []cdsCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]cdsCall(nil), calls...)
	}
}

// payerBootEnv is a forwarding payer's build() environment: hermetic
// discovery and keys, its registration bundle, and a registrar whose feed
// lists this holder at registeredBase ("" is no registrar); extra laid over it.
func payerBootEnv(t *testing.T, payerURL, registeredBase string, extra map[string]string) map[string]string {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	t.Cleanup(keys.Close)
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{`+acceptingWire+`"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	t.Cleanup(disc.Close)
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-test-payer-cds-auth")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": disc.URL, "SHN_FAKE_VALIDATOR": "1",
		"PAYER_DAVINCI_BASE_URL": payerURL, "PAYER_DAVINCI_CRD_SERVICE_ID": "svc",
	}
	if registeredBase != "" {
		feed, _ := json.Marshal([]shnsdk.Holder{{
			ID: id.HolderID, Role: "payer", BaseURL: registeredBase,
			EncPub: base64.StdEncoding.EncodeToString(id.EncPub[:]), SignPub: base64.StdEncoding.EncodeToString(id.SignPub),
		}})
		registrar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(feed)
		}))
		t.Cleanup(registrar.Close)
		env["REGISTRAR_URL"] = registrar.URL
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// TestBuild_PayerCDSClientJWT: a forwarding payer whose client has a signing
// key, with no CDS base, no ingress and no issuer set, takes its issuer from
// its own registration on the network feed and still reads its payer's CDS
// service listing at boot with a client JWT: it verifies against the key, its
// aud is the exact listing URL on the base CRD falls back to, its iss the
// registered base and its jku the JWK Set under it. The gateway serves that
// JWK Set with no credential. Its boot output says how CDS calls authenticate
// and never holds the JWT or the key.
func TestBuild_PayerCDSClientJWT(t *testing.T) {
	payer, calls := recordingPayer(t)
	path, pk := writeCDSKey(t)
	const registered = "https://payer-gw.example"
	env := payerBootEnv(t, payer.URL, registered, map[string]string{
		"PAYER_DAVINCI_TOKEN_URL": payer.URL + "/token", "PAYER_DAVINCI_CLIENT_ID": "shn-gateway",
		"PAYER_DAVINCI_CLIENT_KEY": path, "PAYER_DAVINCI_CLIENT_ALG": "ES384",
	})
	b, out, err := buildForTest(t, env)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	var listing []cdsCall
	for _, c := range calls() {
		if c.url == payer.URL+"/cds-services" {
			listing = append(listing, c)
		}
	}
	if len(listing) != 1 {
		t.Fatalf("the payer's CDS service listing was read %d times at boot, want once (calls %v)\n%s", len(listing), calls(), out)
	}
	raw, ok := strings.CutPrefix(listing[0].auth, "Bearer ")
	if !ok || raw == "access-token-1" {
		t.Fatalf("listing Authorization %q, want a client JWT", listing[0].auth)
	}
	claims := jwt.MapClaims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return &pk.PublicKey, nil }, jwt.WithValidMethods([]string{"ES384"}))
	if err != nil {
		t.Fatalf("the listing's JWT does not verify against the payer client key: %v", err)
	}
	wantJKU := registered + smartauth.CDSClientJWKSPath
	if claims["aud"] != payer.URL+"/cds-services" || claims["iss"] != registered || tok.Header["jku"] != wantJKU || tok.Header["typ"] != "JWT" {
		t.Fatalf("listing JWT header %v claims %v: want aud %s, iss %s, jku %s, typ JWT", tok.Header, claims, payer.URL+"/cds-services", registered, wantJKU)
	}
	if strings.Contains(out, "listing is not readable yet") {
		t.Fatalf("boot could not read the listing:\n%s", out)
	}
	for _, line := range []string{
		"gateway: CDS calls to the payer carry a CDS Hooks client JWT (the payer client has a signing key (PAYER_DAVINCI_CLIENT_KEY)): alg ES384, kid " + tok.Header["kid"].(string),
		"gateway: CDS client JWT iss " + registered + " (this gateway's registered base on the network feed)",
		"gateway: CDS client JWT jku " + wantJKU + ", this gateway's own JWK Set",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("boot output lacks %q:\n%s", line, out)
		}
	}
	keyPEM, _ := os.ReadFile(path)
	der, _ := x509.MarshalECPrivateKey(pk)
	for what, secret := range map[string]string{
		"the JWT": raw, "its signature": raw[strings.LastIndex(raw, ".")+1:],
		"the key PEM": strings.Split(string(keyPEM), "\n")[1], "the key": base64.StdEncoding.EncodeToString(der)[:40],
	} {
		if strings.Contains(out, secret) {
			t.Errorf("boot output holds %s", what)
		}
	}

	rec := httptest.NewRecorder()
	b.gateway.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, smartauth.CDSClientJWKSPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", smartauth.CDSClientJWKSPath, rec.Code)
	}
	var set struct {
		Keys []struct{ Kty, Kid, Crv, X, Y, D string } `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil || len(set.Keys) != 1 {
		t.Fatalf("served %s (%v), want one key", rec.Body.String(), err)
	}
	k := set.Keys[0]
	x, _ := base64.RawURLEncoding.DecodeString(k.X)
	y, _ := base64.RawURLEncoding.DecodeString(k.Y)
	if k.Kid != tok.Header["kid"] || k.D != "" || !bytes.Equal(x, pk.X.FillBytes(make([]byte, 48))) || !bytes.Equal(y, pk.Y.FillBytes(make([]byte, 48))) {
		t.Fatalf("served key %+v is not the payer client key's public half under the JWT's kid", k)
	}
}

// TestBuild_PayerCDSAccessToken: a payer whose client has no signing key (a
// client secret, its key stored empty), and one with a key that keeps CDS on
// the access token, read the listing with the access token, say so at boot,
// and serve no JWK Set.
func TestBuild_PayerCDSAccessToken(t *testing.T) {
	path, _ := writeCDSKey(t)
	for name, c := range map[string]struct {
		extra    map[string]string
		token    bool
		wantAuth string
		wantLine string
	}{
		"no key": {extra: map[string]string{"PAYER_DAVINCI_CLIENT_SECRET": "s", "PAYER_DAVINCI_CLIENT_KEY": ""}, token: true,
			wantAuth: "Bearer access-token-1", wantLine: "gateway: CDS calls to the payer carry the payer client's access token: "},
		"access-token set": {extra: map[string]string{"PAYER_DAVINCI_CLIENT_KEY": path, "PAYER_DAVINCI_CLIENT_ALG": "ES384",
			"PAYER_DAVINCI_CDS_AUTH": "access-token"}, token: true,
			wantAuth: "Bearer access-token-1", wantLine: "gateway: CDS calls to the payer carry the payer client's access token: "},
		"no credentials at all": {extra: map[string]string{},
			wantAuth: "", wantLine: "gateway: CDS calls to the payer carry no credential, as its other operations do (PAYER_DAVINCI_TOKEN_URL unset): "},
	} {
		t.Run(name, func(t *testing.T) {
			payer, calls := recordingPayer(t)
			extra := c.extra
			if c.token {
				extra["PAYER_DAVINCI_TOKEN_URL"] = payer.URL + "/token"
				extra["PAYER_DAVINCI_CLIENT_ID"] = "shn-gateway"
			}
			b, out, err := buildForTest(t, payerBootEnv(t, payer.URL, "https://payer-gw.example", extra))
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			var listing []cdsCall
			for _, call := range calls() {
				if call.url == payer.URL+"/cds-services" {
					listing = append(listing, call)
				}
			}
			if len(listing) != 1 || listing[0].auth != c.wantAuth {
				t.Errorf("listing read %v at boot, want once with Authorization %q", listing, c.wantAuth)
			}
			if !strings.Contains(out, c.wantLine) {
				t.Errorf("boot output lacks %q:\n%s", c.wantLine, out)
			}
			rec := httptest.NewRecorder()
			b.gateway.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, smartauth.CDSClientJWKSPath, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404 with no client JWT", smartauth.CDSClientJWKSPath, rec.Code)
			}
		})
	}
}

// TestPayerCDSClient_IssuerAndJKU: the issuer is the setting, else the payer
// ingress's base, else this gateway's registered base on the feed, read for
// each request; with none known, nothing is sent. jku is the JWK Set under the
// same base when that base is https, and is left out, saying why, when it is
// not; off leaves it out and a URL names it.
func TestPayerCDSClient_IssuerAndJKU(t *testing.T) {
	path, pk := writeCDSKey(t)
	const holder = "h-self"
	base := config{Role: "payer", PayerDavinciBaseURL: "https://payer.example", PayerDavinciClientKey: path, PayerDavinciClientAlg: "ES384"}
	for _, c := range []struct {
		name              string
		ingress, iss, jku string
		registered        string
		wantISS, wantJKU  string
		wantLine          string
		wantErr           error
	}{
		{name: "setting wins", ingress: "https://ingress.example", registered: "https://reg.example", iss: "https://issuer.example",
			wantISS: "https://issuer.example", wantJKU: "https://ingress.example/cds-client/jwks.json", wantLine: "iss https://issuer.example (PAYER_DAVINCI_CDS_JWT_ISS)"},
		{name: "ingress base", ingress: "https://ingress.example", registered: "https://reg.example",
			wantISS: "https://ingress.example", wantJKU: "https://ingress.example/cds-client/jwks.json", wantLine: "iss https://ingress.example (PAYER_DAVINCI_INGRESS_BASE_URL)"},
		{name: "registered base", registered: "https://reg.example",
			wantISS: "https://reg.example", wantJKU: "https://reg.example/cds-client/jwks.json", wantLine: "iss https://reg.example (this gateway's registered base on the network feed)"},
		{name: "http base: jku left out", registered: "http://payer-gw:8080",
			wantISS: "http://payer-gw:8080", wantJKU: "", wantLine: "jku left out: this gateway's base http://payer-gw:8080 is not https"},
		{name: "jku off", registered: "https://reg.example", jku: "off",
			wantISS: "https://reg.example", wantJKU: "", wantLine: "jku left out (PAYER_DAVINCI_CDS_JWT_JKU=off)"},
		{name: "jku set", registered: "https://reg.example", jku: "https://keys.example/jwks.json",
			wantISS: "https://reg.example", wantJKU: "https://keys.example/jwks.json", wantLine: "jku https://keys.example/jwks.json (PAYER_DAVINCI_CDS_JWT_JKU)"},
		{name: "no issuer known", wantErr: smartauth.ErrCDSClientIssuerUnknown, wantLine: "WARNING CDS client JWT has no issuer yet"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var seen []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = append(seen, r.Header.Get("Authorization"))
			}))
			defer srv.Close()
			cfg := base
			cfg.PayerDavinciIngressBaseURL = c.ingress
			reg := shnsdk.NewRegistry()
			if c.registered != "" {
				reg.Set(holder, shnsdk.RegistryEntry{ID: holder, Role: "payer", BaseURL: c.registered})
			}
			var out bytes.Buffer
			hc, key, err := payerCDSClient(cfg, payerCDSAuth{mode: cdsAuthClientJWT, why: "test", iss: c.iss, jku: c.jku}, http.DefaultTransport, reg, holder, &out)
			if err != nil || hc == nil || key == nil {
				t.Fatalf("payerCDSClient: %v", err)
			}
			if !strings.Contains(out.String(), c.wantLine) {
				t.Errorf("boot output lacks %q:\n%s", c.wantLine, out.String())
			}
			resp, err := hc.Get(srv.URL + "/cds-services")
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) || len(seen) != 0 {
					t.Fatalf("err %v after %d requests, want %v and nothing sent", err, len(seen), c.wantErr)
				}
				// The registration arrives on the feed: the next request has its issuer.
				reg.Set(holder, shnsdk.RegistryEntry{ID: holder, Role: "payer", BaseURL: "https://late.example"})
				if resp, err = hc.Get(srv.URL + "/cds-services"); err != nil {
					t.Fatalf("after the feed registers this gateway: %v", err)
				}
				resp.Body.Close()
				c.wantISS, c.wantJKU = "https://late.example", "https://late.example/cds-client/jwks.json"
			} else {
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
			}
			raw := strings.TrimPrefix(seen[len(seen)-1], "Bearer ")
			claims := jwt.MapClaims{}
			tok, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return &pk.PublicKey, nil }, jwt.WithValidMethods([]string{"ES384"}))
			if err != nil {
				t.Fatalf("JWT: %v", err)
			}
			jku, _ := tok.Header["jku"].(string)
			if claims["iss"] != c.wantISS || jku != c.wantJKU {
				t.Fatalf("iss %v jku %q, want %s and %q", claims["iss"], jku, c.wantISS, c.wantJKU)
			}
		})
	}
}

// TestPayerCDSHTTPClient_DiagnosticCaptureNeverHoldsTheJWT: with the
// diagnostic capture on, the client the payer's gateway calls its CDS service
// with records each call as the gateway made it, above the JWT layer: the
// capture holds the call, and never the JWT the service received.
func TestPayerCDSHTTPClient_DiagnosticCaptureNeverHoldsTheJWT(t *testing.T) {
	path, _ := writeCDSKey(t)
	diagKey := filepath.Join(t.TempDir(), "diag.key")
	if err := os.WriteFile(diagKey, []byte("synthetic-observation-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	denv := map[string]string{"DIAGNOSTIC_SINK_URL": "http://collector.test/events", "DIAGNOSTIC_SOURCE": "test-payer", "DIAGNOSTIC_KEY_FILE": diagKey}
	d := newDiagnosticSource(func(k string) string { return denv[k] }, "payer", io.Discard, time.Now)
	if d == nil {
		t.Fatal("diagnostic capture not configured")
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"services":[]}`)
	}))
	defer srv.Close()
	cfg := config{Role: "payer", PayerDavinciBaseURL: srv.URL, PayerDavinciClientKey: path, PayerDavinciClientAlg: "ES384",
		PayerCDSAuth: payerCDSAuth{mode: cdsAuthClientJWT, why: "test", iss: "https://payer-gw.example"}, diagnostic: d}
	hc, _, err := payerCDSHTTPClient(cfg, shnsdk.NewRegistry(), "", io.Discard)
	if err != nil || hc == nil {
		t.Fatalf("payerCDSHTTPClient: %v", err)
	}
	resp, err := hc.Get(srv.URL + "/cds-services")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	jwtRaw, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || strings.Count(jwtRaw, ".") != 2 {
		t.Fatalf("the CDS service received Authorization %q, want a client JWT", auth)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	captured := 0
	for {
		e, err := d.queue.Next(ctx)
		if err != nil {
			break
		}
		if e.Kind == "http-request" && e.URL == "/cds-services" {
			captured++
		}
		raw, _ := json.Marshal(e)
		if bytes.Contains(raw, []byte(jwtRaw)) || bytes.Contains(raw, []byte(jwtRaw[strings.LastIndex(jwtRaw, ".")+1:])) || e.Headers.Get("Authorization") != "" {
			t.Fatalf("the diagnostic capture holds the CDS client JWT: %s", raw)
		}
	}
	if captured == 0 {
		t.Fatal("the diagnostic capture recorded no CDS call: the check above saw nothing")
	}
}

// TestHTTPSJWKSURL: jku names the JWK Set under an https base, however its
// scheme is cased, and nothing under any other.
func TestHTTPSJWKSURL(t *testing.T) {
	for base, want := range map[string]string{
		"https://gw.example": "https://gw.example/cds-client/jwks.json",
		"HTTPS://gw.example": "HTTPS://gw.example/cds-client/jwks.json",
		"http://gw:8080":     "",
		"":                   "",
	} {
		if got := httpsJWKSURL(base); got != want {
			t.Errorf("httpsJWKSURL(%q) = %q, want %q", base, got, want)
		}
	}
}
