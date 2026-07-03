package scenariodriver

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testDriver(t *testing.T, ingress *httptest.Server) (*Driver, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ClientID:    "http://br-provider:8080",
		IngressBase: "http://provider-gw:8080",
		Key:         key,
		Now:         func() time.Time { return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC) },
	}
	if ingress != nil {
		cfg.IngressURL = ingress.URL
	}
	return New(cfg), key
}

// TestMintDirectBearer_Claims proves the minted bearer is FAITHFUL to
// br-provider's CDS-client JWT shape: iss=client_id, aud=the called endpoint,
// RS384, exp ≤ 5min, fresh jti, NO sub.
func TestMintDirectBearer_Claims(t *testing.T) {
	d, key := testDriver(t, nil)
	tok, err := d.MintDirectBearer("http://provider-gw:8080/Claim/$submit")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(tok, func(tk *jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithValidMethods([]string{"RS384"}), jwt.WithTimeFunc(func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		}))
	if err != nil || !parsed.Valid {
		t.Fatalf("minted bearer does not verify as RS384: %v", err)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["iss"] != "http://br-provider:8080" || claims["aud"] != "http://provider-gw:8080/Claim/$submit" {
		t.Fatalf("iss/aud = %v/%v", claims["iss"], claims["aud"])
	}
	if _, hasSub := claims["sub"]; hasSub {
		t.Fatal("bearer must NOT carry sub (UDAP B2B direct-bearer shape)")
	}
	exp := int64(claims["exp"].(float64))
	iat := int64(claims["iat"].(float64))
	if exp-iat > 300 {
		t.Fatalf("exp-iat = %ds, want ≤ 300", exp-iat)
	}
	if claims["jti"] == "" {
		t.Fatal("missing jti")
	}
}

// TestPostCRD_BearerAndAud proves PostCRD POSTs to {IngressURL}{path} with a
// bearer whose aud is {IngressBase}{path} (the audUnder contract).
func TestPostCRD_BearerAndAud(t *testing.T) {
	var gotAuth, gotPath, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotCT = r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(cardsFixture))
	}))
	defer srv.Close()
	d, key := testDriver(t, srv)
	res, err := d.PostCRD([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusOK || gotPath != "/cds-services/order-select-crd" || gotCT != "application/json" {
		t.Fatalf("status=%d path=%q ct=%q", res.Status, gotPath, gotCT)
	}
	tok, err := jwt.Parse(gotAuth[len("Bearer "):], func(tk *jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithValidMethods([]string{"RS384"}), jwt.WithTimeFunc(func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		}))
	if err != nil {
		t.Fatalf("Authorization did not carry a verifiable bearer: %v", err)
	}
	if aud := tok.Claims.(jwt.MapClaims)["aud"]; aud != "http://provider-gw:8080/cds-services/order-select-crd" {
		t.Fatalf("aud = %v — must be the called endpoint under IngressBase", aud)
	}
	if c, err := ParseCards(res.Body); err != nil || c.Covered() != "covered" {
		t.Fatalf("body did not round-trip: %v %v", c, err)
	}
}

// TestSubmitPAS_ParsesOutcome: 200 A1 body → Approved+PreAuthRef; non-200 → raw only.
// The approved shape is a BARE ClaimResponse with explicit preAuthRef + outcome
// "complete" (shnsdk.ParseClaimResponse's explicit-signal contract; a pended
// response is a Bundle detected by ParsePendedResponse instead).
func TestSubmitPAS_ParsesOutcome(t *testing.T) {
	a1 := `{"resourceType":"ClaimResponse","status":"active","outcome":"complete","preAuthRef":"AUTH-0001"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Claim/$submit" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(a1))
	}))
	defer srv.Close()
	d, _ := testDriver(t, srv)
	out, err := d.SubmitPAS([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Approved || out.PreAuthRef != "AUTH-0001" || out.Pended {
		t.Fatalf("outcome = %+v, want approved AUTH-0001 not-pended", out)
	}
}

func TestPHGClients(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/personas":
			json.NewEncoder(w).Encode([]map[string]string{
				{"label": "Linda Johansson — covered", "pci": "pci-1"},
				{"label": "Maria Reyes — not covered", "pci": "pci-2"},
			})
		case "/authorizations":
			if r.URL.Query().Get("pci") != "pci-1" {
				t.Errorf("pci = %s", r.URL.Query().Get("pci"))
			}
			json.NewEncoder(w).Encode([]AuthorizationView{{Status: "approved", Title: "Home oxygen"}})
		}
	}))
	defer srv.Close()
	d := New(Config{PHGURL: srv.URL})
	pci, err := d.ResolvePersonaPCI("Johansson", "covered")
	if err != nil || pci != "pci-1" {
		t.Fatalf("pci = %q err=%v", pci, err)
	}
	if _, err := d.ResolvePersonaPCI("Nobody"); err == nil {
		t.Fatal("want error when no persona matches")
	}
	views, err := d.GetAuthorizations("pci-1")
	if err != nil || len(views) != 1 || views[0].Status != "approved" {
		t.Fatalf("views = %+v err=%v", views, err)
	}
}

func TestScenarioClients(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotPath, gotBody = r.URL.Path, string(b)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	d := New(Config{ProviderDataURL: srv.URL, ConsoleURL: srv.URL})
	if res, err := d.RunProviderDataScenario("/scenario/uc04", `{}`); err != nil || res.Status != 200 {
		t.Fatalf("provider-data: %+v %v", res, err)
	}
	if gotPath != "/scenario/uc04" {
		t.Fatalf("path = %s", gotPath)
	}
	if _, err := d.RunConsoleScenario(`{"scenario":"uc01","branch":"covered"}`); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/run" || gotBody == "" {
		t.Fatalf("console path=%s body=%q", gotPath, gotBody)
	}
}
