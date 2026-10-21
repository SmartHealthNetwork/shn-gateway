package engine

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// bearerCases are the Authorization headers the bearer scheme reads: the
// scheme is case-insensitive (RFC 7235 §2.1), so a client that builds its
// header from the token answer's own token_type ("bearer") is accepted; a
// wrong scheme, an empty token and a scheme with no separating space are
// refused.
func bearerCases(typ, tok string) []struct {
	name, header string
	ok           bool
} {
	return []struct {
		name, header string
		ok           bool
	}{
		{"from token_type", typ + " " + tok, true},
		{"lowercase", "bearer " + tok, true},
		{"uppercase", "BEARER " + tok, true},
		{"canonical", "Bearer " + tok, true},
		{"wrong scheme", "Basic " + tok, false},
		{"empty token", "Bearer ", false},
		{"no space", "Bearer" + tok, false},
		{"scheme only", "Bearer", false},
	}
}

func TestIngressBearerSchemeIsCaseInsensitive(t *testing.T) {
	key, pub := newTestClientKey(t)
	s := newTestAuthServer(t, "br-provider", pub, "ES384")
	now := ingressFixedClock()()
	w := postToken(t, http.HandlerFunc(s.handleToken), mintAssertion(t, key, jwt.SigningMethodES384, validClaims("br-provider", s.tokenURL(), now)))
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil || tr.AccessToken == "" || tr.TokenType == "" {
		t.Fatalf("token answer %d: %s (%v)", w.Code, w.Body, err)
	}
	for _, c := range bearerCases(tr.TokenType, tr.AccessToken) {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, testIngressBaseURL+"/cds-services/order-sign-crd", nil)
			r.Header.Set("Authorization", c.header)
			if _, ok, unavailable := s.verifyBearer(r); ok != c.ok || unavailable {
				t.Fatalf("verifyBearer(%q): ok=%v unavailable=%v, want ok=%v", c.name, ok, unavailable, c.ok)
			}
		})
	}
}

// A registered client's own direct bearer reads the scheme the same way.
func TestDirectBearerSchemeIsCaseInsensitive(t *testing.T) {
	key, pub := newTestClientKey(t)
	s := newTestAuthServer(t, "br-provider", pub, "ES384")
	now := ingressFixedClock()()
	aud := testIngressBaseURL + "/cds-services/order-sign-crd"
	for i, c := range bearerCases("bearer", "") {
		t.Run(c.name, func(t *testing.T) {
			claims := directClaims("br-provider", aud, now)
			claims["jti"] = claims["jti"].(string) + "-" + string(rune('a'+i))
			tok := signJWT(t, jwt.SigningMethodES384, key, claims)
			header := c.header + tok
			if c.name == "empty token" || c.name == "scheme only" {
				header = c.header
			}
			r, _ := http.NewRequest(http.MethodPost, aud, nil)
			r.Header.Set("Authorization", header)
			if _, got := s.verifyDirectBearer(r); got != c.ok {
				t.Fatalf("verifyDirectBearer(%q): %v, want %v", c.name, got, c.ok)
			}
		})
	}
}
