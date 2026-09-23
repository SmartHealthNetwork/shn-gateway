package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/exchangecontext"
	"github.com/golang-jwt/jwt/v5"
)

// signedFixtureIngress registers the synthetic producer and signs its independently
// supplied addressing and declaration. It uses the normal bearer/context verifiers.
func signedFixtureIngress(t *testing.T, g *Gateway, path, leg, operation, hook, subject, version, correlation string, body []byte) *http.Request {
	t.Helper()
	key, pub := newTestClientKey(t)
	g.cfg.ingressAuthBypass = false
	g.ingressAuth = newTestAuthServer(t, "fixture-source", pub, "ES384")
	registration := g.ingressAuth.clients["fixture-source"]
	registration.ContextOperations = []string{operation}
	g.ingressAuth.clients["fixture-source"] = registration
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	media := "application/fhir+json"
	if hook != "" {
		media = "application/json"
	}
	r.Header.Set("Content-Type", media)
	now := g.ingressAuth.now()
	claims := exchangecontext.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "fixture-source", Subject: "fixture-source", Audience: jwt.ClaimStrings{testIngressBaseURL + path}, ID: "fixture-context", IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, Holder: "provider", Recipient: "payer", Leg: leg, Operation: operation, CRDHook: hook, SubjectPCI: subject, ContractVersion: version, CorrelationID: correlation, ContentType: media}
	putContext(t, r, claims, body, key)
	r.Header.Set("Authorization", "Bearer "+signJWT(t, jwt.SigningMethodES384, key, directClaims("fixture-source", testIngressBaseURL+path, now)))
	return r
}
