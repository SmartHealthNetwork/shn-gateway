package exchangecontext

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func contextFixture() (*ecdsa.PrivateKey, Claims, Binding, []byte, time.Time) {
	curve := elliptic.P384()
	x, y := curve.ScalarBaseMult([]byte{1})
	key := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: big.NewInt(1)}
	now := time.Unix(1800000000, 0)
	audience := "https://gateway.example/Claim/$submit"
	c := Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "source", Subject: "source", Audience: jwt.ClaimStrings{audience}, ID: "request-1", IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, Holder: "provider", Recipient: "payer", Leg: "pas-claim", Operation: "pas-submit", SubjectPCI: "pci:synthetic", CorrelationID: "exchange-1", ContentType: "application/fhir+json"}
	return key, c, Binding{"source", "provider", "payer", "pas-claim", "pas-submit", "application/fhir+json", audience}, []byte("{bad"), now
}
func TestContextAssertionBindsExactBytes(t *testing.T) {
	key, c, b, body, now := contextFixture()
	token, err := Sign(c, body, "ES384", key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(token, body, b, "ES384", &key.PublicKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubjectPCI != c.SubjectPCI || len(got.BodySHA256) != 64 {
		t.Fatalf("claims lost: %+v", got)
	}
	if _, err := Verify(token, []byte("{changed"), b, "ES384", &key.PublicKey, now); err == nil {
		t.Fatal("different bytes accepted")
	}
	c.BodySHA256 = "wrong"
	if _, err := Sign(c, body, "ES384", key); err == nil {
		t.Fatal("wrong supplied digest accepted")
	}
}
func TestContextAssertionRejections(t *testing.T) {
	rows := []struct {
		name   string
		mutate func(*Claims)
	}{
		{"issuer", func(c *Claims) { c.Issuer = "other" }}, {"subject", func(c *Claims) { c.Subject = "other" }},
		{"holder", func(c *Claims) { c.Holder = "other" }}, {"recipient", func(c *Claims) { c.Recipient = "other" }},
		{"leg", func(c *Claims) { c.Leg = "other" }}, {"operation", func(c *Claims) { c.Operation = "other" }},
		{"media", func(c *Claims) { c.ContentType = "application/json" }}, {"audience", func(c *Claims) { c.Audience = jwt.ClaimStrings{"https://evil"} }},
		{"extra audience", func(c *Claims) { c.Audience = append(c.Audience, "https://evil") }},
		{"no iat", func(c *Claims) { c.IssuedAt = nil }}, {"no nbf", func(c *Claims) { c.NotBefore = nil }}, {"no exp", func(c *Claims) { c.ExpiresAt = nil }}, {"no jti", func(c *Claims) { c.ID = "" }},
		{"no pci", func(c *Claims) { c.SubjectPCI = "" }}, {"no correlation", func(c *Claims) { c.CorrelationID = "" }},
		{"expired", func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(c.IssuedAt.Time) }},
		{"future iat", func(c *Claims) { c.IssuedAt = jwt.NewNumericDate(c.IssuedAt.Add(time.Second)) }},
		{"future nbf", func(c *Claims) { c.NotBefore = jwt.NewNumericDate(c.NotBefore.Add(time.Second)) }},
		{"long lifetime", func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(c.IssuedAt.Add(5*time.Minute + time.Second)) }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			key, c, b, body, now := contextFixture()
			row.mutate(&c)
			token, err := Sign(c, body, "ES384", key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Verify(token, body, b, "ES384", &key.PublicKey, now); err == nil {
				t.Fatal("invalid assertion accepted")
			}
		})
	}
	key, c, b, body, now := contextFixture()
	token, _ := Sign(c, body, "ES384", key)
	for _, raw := range []string{"bad", token + "x", strings.Repeat("x", 16*1024+1)} {
		if _, err := Verify(raw, body, b, "ES384", &key.PublicKey, now); err == nil {
			t.Fatal("malformed assertion accepted")
		}
	}
	if _, err := Verify(token, body, b, "RS384", &key.PublicKey, now); err == nil {
		t.Fatal("algorithm confusion accepted")
	}
	// The hook is signed even though it is not inferred from opaque application bytes.
	c.CRDHook = "order-select"
	token, _ = Sign(c, body, "ES384", key)
	parts := strings.Split(token, ".")
	payload, _ := jwt.NewParser().DecodeSegment(parts[1])
	parts[1] = jwt.New(jwt.SigningMethodES384).EncodeSegment([]byte(strings.Replace(string(payload), "order-select", "order-sign", 1)))
	if _, err := Verify(strings.Join(parts, "."), body, b, "ES384", &key.PublicKey, now); err == nil {
		t.Fatal("tampered hook accepted")
	}
}
func TestContextAssertionRS384(t *testing.T) {
	_, c, b, body, now := contextFixture()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	token, err := Sign(c, body, "RS384", key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(token, body, b, "RS384", &key.PublicKey, now); err != nil {
		t.Fatal(err)
	}
}

func TestContextAssertionRejectsRequestSuppliedKeys(t *testing.T) {
	key, c, b, body, now := contextFixture()
	signed, err := Sign(c, body, "ES384", key)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := jwt.NewParser().ParseUnverified(signed, &c); err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"jku", "x5u", "jwk", "x5c", "crit"} {
		t.Run(header, func(t *testing.T) {
			token := jwt.NewWithClaims(jwt.SigningMethodES384, c)
			token.Header[header] = "https://untrusted.invalid/key"
			raw, err := token.SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Verify(raw, body, b, "ES384", &key.PublicKey, now); err == nil {
				t.Fatal("request key authority accepted")
			}
		})
	}
	c.BodySHA256 = strings.ToUpper(c.BodySHA256)
	raw, err := jwt.NewWithClaims(jwt.SigningMethodES384, c).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(raw, body, b, "ES384", &key.PublicKey, now); err == nil {
		t.Fatal("non-canonical digest accepted")
	}
}
