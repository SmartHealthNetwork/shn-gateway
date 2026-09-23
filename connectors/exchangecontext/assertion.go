// Package exchangecontext signs gateway-local ingress metadata without interpreting
// the application body. Keys and algorithms come from participant registration.
package exchangecontext

import (
	"crypto"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const Header = "SHN-Exchange-Context"
const MaxTokenBytes = 16 << 10

// ErrAuthentication identifies an assertion whose authenticity could not be verified.
// Callers must never fall back to unsigned extraction for a presented assertion.
var ErrAuthentication = errors.New("exchange context authentication failed")
var ErrBinding = errors.New("exchange context binding invalid")

type Completion struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}
type Claims struct {
	jwt.RegisteredClaims
	Holder          string       `json:"holder"`
	Recipient       string       `json:"recipient"`
	Leg             string       `json:"leg"`
	Operation       string       `json:"operation"`
	CRDHook         string       `json:"crd_hook,omitempty"`
	SubjectPCI      string       `json:"subject_pci"`
	CorrelationID   string       `json:"correlation_id"`
	Custodian       string       `json:"custodian,omitempty"`
	ConsentRef      string       `json:"consent_ref,omitempty"`
	ContractVersion string       `json:"contract_version,omitempty"`
	ContentType     string       `json:"content_type"`
	BodySHA256      string       `json:"body_sha256"`
	Completed       []Completion `json:"completed,omitempty"`
}
type Binding struct{ ClientID, Holder, Recipient, Leg, Operation, ContentType, Audience string }

// Sign computes the lowercase SHA-256 digest of the exact application bytes.
func Sign(c Claims, body []byte, alg string, key crypto.Signer) (string, error) {
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	if c.BodySHA256 != "" && c.BodySHA256 != digest {
		return "", ErrBinding
	}
	c.BodySHA256 = digest
	if alg != "ES384" && alg != "RS384" {
		return "", ErrAuthentication
	}
	return jwt.NewWithClaims(jwt.GetSigningMethod(alg), c).SignedString(key)
}

// Verify checks a registration-pinned key, exact audience and byte binding. It
// never fetches a key URL or parses the application body. Replay and registration
// grants are enforced by the gateway after this verification succeeds.
func Verify(token string, body []byte, b Binding, alg string, key crypto.PublicKey, now time.Time) (Claims, error) {
	if len(token) == 0 || len(token) > MaxTokenBytes || (alg != "ES384" && alg != "RS384") || key == nil {
		return Claims{}, ErrAuthentication
	}
	c := Claims{}
	_, err := jwt.NewParser(jwt.WithValidMethods([]string{alg}), jwt.WithoutClaimsValidation()).ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		// The only key authority is the registration; supplied key material is refused.
		for _, h := range []string{"jku", "x5u", "jwk", "x5c", "crit"} {
			if _, ok := t.Header[h]; ok {
				return nil, ErrAuthentication
			}
		}
		return key, nil
	})
	if err != nil {
		return Claims{}, ErrAuthentication
	}
	if c.Issuer != b.ClientID || c.Subject != b.ClientID || b.ClientID == "" || len(c.Audience) != 1 || c.Audience[0] != b.Audience || b.Audience == "" || c.Holder != b.Holder || c.Recipient != b.Recipient || c.Leg != b.Leg || c.Operation != b.Operation || c.ContentType != b.ContentType || c.SubjectPCI == "" || c.CorrelationID == "" || c.ID == "" || c.Holder == "" || c.Recipient == "" || c.Leg == "" || c.Operation == "" || c.ContentType == "" {
		return Claims{}, ErrBinding
	}
	if c.IssuedAt == nil || c.NotBefore == nil || c.ExpiresAt == nil || c.IssuedAt.After(now) || c.NotBefore.After(now) || !c.ExpiresAt.After(now) || c.NotBefore.Before(c.IssuedAt.Time) || !c.ExpiresAt.After(c.NotBefore.Time) || c.ExpiresAt.Sub(c.IssuedAt.Time) > 5*time.Minute {
		return Claims{}, ErrBinding
	}
	if c.BodySHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) {
		return Claims{}, ErrBinding
	}
	return c, nil
}
