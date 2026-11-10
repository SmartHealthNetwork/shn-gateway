package smartauth

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// The challenge a resource server's 401 carried, as the bearer transport
// observed it (credentialChallenge): it is recorded, never used to decide
// whether to resend.
const (
	// ChallengeInvalidToken: a Bearer challenge whose error is invalid_token
	// (RFC 6750 §3.1), the credential the request carried no longer accepted.
	ChallengeInvalidToken = "invalid_token"
	// ChallengeOther: a WWW-Authenticate challenge of any other kind.
	ChallengeOther = "other"
	// ChallengeNone: no WWW-Authenticate challenge.
	ChallengeNone = "none"
)

// ChallengeValues is the closed list a resend's challenge draws from.
var ChallengeValues = []string{ChallengeInvalidToken, ChallengeOther, ChallengeNone}

// CredentialRefreshObservation records each request the bearer transport sent
// a second time, after the resource server answered 401 to its cached bearer:
// the transport drops that bearer, fetches a fresh one and sends the same
// request once more. It keeps, per resend, the challenge the 401 carried, and
// nothing else. The zero value records nothing; use a fresh handle per call
// whose resends are to be counted. It is safe for concurrent use.
type CredentialRefreshObservation struct {
	mu      sync.Mutex
	resends []string
}

// Resends is the challenge of each resend so far, in order (ChallengeValues),
// nil when there was none.
func (o *CredentialRefreshObservation) Resends() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.resends)
}

func (o *CredentialRefreshObservation) resent(challenge string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resends = append(o.resends, challenge)
}

type credentialRefreshObservationKey struct{}

// WithCredentialRefreshObservation derives a context whose requests' resends
// are recorded on the returned handle, shadowing any inherited one while
// keeping the parent's values, deadline and cancellation.
func WithCredentialRefreshObservation(ctx context.Context) (context.Context, *CredentialRefreshObservation) {
	o := &CredentialRefreshObservation{}
	return context.WithValue(ctx, credentialRefreshObservationKey{}, o), o
}

func credentialRefreshObservationFrom(ctx context.Context) *CredentialRefreshObservation {
	o, _ := ctx.Value(credentialRefreshObservationKey{}).(*CredentialRefreshObservation)
	return o
}

// credentialChallenge reads a 401's WWW-Authenticate challenges: a Bearer one
// whose error parameter is invalid_token, any other, or none.
func credentialChallenge(h http.Header) string {
	values := h.Values("WWW-Authenticate")
	if len(values) == 0 {
		return ChallengeNone
	}
	for _, v := range values {
		scheme, params, _ := strings.Cut(strings.TrimSpace(v), " ")
		if !strings.EqualFold(scheme, "Bearer") {
			continue
		}
		for _, p := range strings.Split(params, ",") {
			name, value, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(name), "error") && strings.Trim(strings.TrimSpace(value), `"`) == "invalid_token" {
				return ChallengeInvalidToken
			}
		}
	}
	return ChallengeOther
}
