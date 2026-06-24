package engine

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// UDAP B2B direct-bearer: the ingress accepts a config-registered client's self-signed
// private_key_jwt presented DIRECTLY as the Authorization bearer (the form br-provider's
// BFF sends), verified per-call against the registered key. Design:
// docs/superpowers/specs/2026-06-22-udap-b2b-ingress-auth-design.md.

func rsaTestClientKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("marshal rsa pub: %v", err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func signJWT(t *testing.T, method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return s
}

// directBearerReq builds an ingress request carrying the JWT as a direct bearer.
func directBearerReq(jwtStr string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, testIngressBaseURL+"/cds-services/order-sign-crd", nil)
	r.Header.Set("Authorization", "Bearer "+jwtStr)
	return r
}

// directClaims is a valid direct-bearer claim set, FAITHFUL to br-provider's real CDS
// client JWT (verified against CdsClientJwtService): iss = client_id, aud = the specific
// ingress ENDPOINT url under the config base, fresh jti, exp within the 5-min cap, and
// crucially NO sub (br-provider omits it).
func directClaims(clientID, aud string, now time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": clientID, "aud": aud,
		"jti": "dbjti-" + now.Format("150405.000000"),
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

func TestDirectBearer_Accepted_ES384(t *testing.T) {
	key, pub := newTestClientKey(t)
	s := newTestAuthServer(t, "br-provider", pub, "ES384")
	now := ingressFixedClock()()
	aud := testIngressBaseURL + "/cds-services/order-sign-crd"
	tok := signJWT(t, jwt.SigningMethodES384, key, directClaims("br-provider", aud, now))
	if !s.verifyDirectBearer(directBearerReq(tok)) {
		t.Fatal("valid ES384 direct bearer rejected")
	}
}

func TestDirectBearer_Accepted_RS384(t *testing.T) {
	key, pub := rsaTestClientKey(t)
	s := newTestAuthServer(t, "br-provider", pub, "RS384") // br-provider is RSA
	now := ingressFixedClock()()
	aud := testIngressBaseURL + "/Questionnaire/$questionnaire-package"
	tok := signJWT(t, jwt.SigningMethodRS384, key, directClaims("br-provider", aud, now))
	if !s.verifyDirectBearer(directBearerReq(tok)) {
		t.Fatal("valid RS384 direct bearer rejected")
	}
}

// The two bearer forms are disjoint: an issued ephemeral bearer must NOT pass
// the direct path (it has no iss), and a direct bearer must NOT pass verifyBearer.
func TestDirectBearer_Disjoint(t *testing.T) {
	key, pub := newTestClientKey(t)
	s := newTestAuthServer(t, "br-provider", pub, "ES384")
	now := ingressFixedClock()()

	issued, err := s.issueBearer("br-provider", ingressScope)
	if err != nil {
		t.Fatal(err)
	}
	issuedReq, _ := http.NewRequest(http.MethodPost, testIngressBaseURL+"/cds-services/x", nil)
	issuedReq.Header.Set("Authorization", "Bearer "+issued)
	if s.verifyDirectBearer(issuedReq) {
		t.Error("issued ephemeral bearer wrongly accepted by the direct path (it has no iss)")
	}

	aud := testIngressBaseURL + "/cds-services/order-sign-crd"
	direct := signJWT(t, jwt.SigningMethodES384, key, directClaims("br-provider", aud, now))
	if s.verifyBearer(directBearerReq(direct)) {
		t.Error("direct bearer wrongly accepted by verifyBearer (foreign key)")
	}
}

// The rejection table — every guard ships its rejection test.
func TestDirectBearer_Rejections(t *testing.T) {
	key, pub := newTestClientKey(t)
	wrongKey, _ := newTestClientKey(t)
	s := newTestAuthServer(t, "br-provider", pub, "ES384")
	now := ingressFixedClock()()
	goodAud := testIngressBaseURL + "/cds-services/order-sign-crd"

	es := func(c jwt.MapClaims) string { return signJWT(t, jwt.SigningMethodES384, key, c) }

	rows := []struct {
		name string
		tok  string
	}{
		{"unknown client", es(directClaims("stranger", goodAud, now))},
		{"wrong key", signJWT(t, jwt.SigningMethodES384, wrongKey, directClaims("br-provider", goodAud, now))},
		{"alg confusion (RS384 token vs ES384 reg)", func() string {
			rk, _ := rsaTestClientKey(t)
			return signJWT(t, jwt.SigningMethodRS384, rk, directClaims("br-provider", goodAud, now))
		}()},
		// THE load-bearing aud case: suffix-of-authority bypass MUST be rejected.
		{"aud suffix-of-authority", es(directClaims("br-provider", testIngressBaseURL+".evil.com/x", now))},
		{"aud foreign", es(directClaims("br-provider", "https://evil/cds-services/x", now))},
		{"aud missing", es(jwt.MapClaims{
			"iss": "br-provider", "sub": "br-provider", "jti": "j1",
			"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		})},
		{"iss != sub", es(jwt.MapClaims{
			"iss": "br-provider", "sub": "someone-else", "aud": goodAud, "jti": "j2",
			"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		})},
		{"expired", es(jwt.MapClaims{
			"iss": "br-provider", "sub": "br-provider", "aud": goodAud, "jti": "j3",
			"iat": now.Add(-10 * time.Minute).Unix(), "exp": now.Add(-time.Minute).Unix(),
		})},
		{"exp too long", es(jwt.MapClaims{
			"iss": "br-provider", "sub": "br-provider", "aud": goodAud, "jti": "j4",
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		})},
		{"missing jti", es(jwt.MapClaims{
			"iss": "br-provider", "sub": "br-provider", "aud": goodAud,
			"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		})},
	}
	for _, row := range rows {
		if s.verifyDirectBearer(directBearerReq(row.tok)) {
			t.Errorf("%s: direct bearer wrongly ACCEPTED", row.name)
		}
	}
}

// jti presence-only: the same jti reused within exp is ACCEPTED (matches
// the replayable-within-life issued bearer; single-use would wedge br-provider's
// discovery-GET-then-POST with one JWT).
func TestDirectBearer_JTIReusableWithinExp(t *testing.T) {
	key, pub := newTestClientKey(t)
	s := newTestAuthServer(t, "br-provider", pub, "ES384")
	now := ingressFixedClock()()
	aud := testIngressBaseURL + "/cds-services/order-sign-crd"
	tok := signJWT(t, jwt.SigningMethodES384, key, directClaims("br-provider", aud, now))
	if !s.verifyDirectBearer(directBearerReq(tok)) {
		t.Fatal("first presentation rejected")
	}
	if !s.verifyDirectBearer(directBearerReq(tok)) {
		t.Error("second presentation of the same jti rejected — should be reusable within exp (baseline)")
	}
}
