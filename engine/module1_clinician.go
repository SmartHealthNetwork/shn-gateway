//go:build !shn_coreonly

package engine

import (
	"context"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/scenarioauth"
)

// The clinician a request module 1 originates names, and the answers it
// attests, come from the participant, never from this gateway.
//
// clinicianSources: the ordering or attesting clinician comes from the first
// of these that states one, in this order:
//  1. an npi the request sends, when scenario caller authentication verified
//     its caller (the participant's own provider application, authenticated);
//  2. the order's requester in the participant's system of record;
//  3. the NPI the gateway is configured with (the participant's own statement
//     about its system);
//  4. an npi the request sends without a verified caller (the weakest
//     source: it fills in only when nothing else does).
//
// With none, the request is refused, not built: the fidelity of SHN's own
// build, at every conformance level, before any leg where the flow holds
// every source up front.

// statedClinicianKey is the request context key of the npi a request states
// for its own clinician.
type statedClinicianKey struct{}

// withStatedClinician records npi, the npi the request states for its own
// clinician ("" states none).
func withStatedClinician(r *http.Request, npi string) *http.Request {
	if npi == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), statedClinicianKey{}, npi))
}

// statedClinician is the npi the request states for its own clinician, and
// whether scenario caller authentication verified the caller that states it.
func statedClinician(ctx context.Context) (npi string, verified bool) {
	npi, _ = ctx.Value(statedClinicianKey{}).(string)
	_, verified = scenarioauth.CallerFrom(ctx)
	return npi, verified
}

// attestsBeforeAnyLegKey marks an origination that attests with a clinician
// NPI and holds every source of it before its first leg (the single call,
// UC-03's oxygen attestation): the attesting clinician is resolved there, so a
// request no source names one for is refused before anything is sent.
type attestsBeforeAnyLegKey struct{}

func withAttestationBeforeAnyLeg(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), attestsBeforeAnyLegKey{}, true))
}

func attestsBeforeAnyLeg(ctx context.Context) bool {
	v, _ := ctx.Value(attestsBeforeAnyLegKey{}).(bool)
	return v
}

// answerLinkID is the questionnaire item UC-06's and UC-07's attestation
// answers: the HomeHealthAssessment's free-text functional status on the
// lanes that attest on it, else the Oswestry score.
func (g *Gateway) answerLinkID() string {
	if g.attestsOnHHA() {
		return hhaFunctionalStatusLinkID
	}
	return oswestryLinkID
}

// requireStatedAnswer refuses a request that states no attestation answer:
// the answer is the participant's own statement, and this gateway supplies
// none. It returns false when it refused.
func (g *Gateway) requireStatedAnswer(w http.ResponseWriter, answer, recipient string) bool {
	if answer != "" {
		return true
	}
	g.refuseOwnBuild(w, recipient, "pas-claim-update",
		"answer is required: the request states no answer for questionnaire item "+g.answerLinkID()+", and this gateway does not supply one")
	return false
}

// refuseOwnBuild answers a request this gateway cannot build faithfully for
// the participant: 422, recorded as the leg it would have sent failing and as
// a fidelity refusal, as sealTransformed records its own.
func (g *Gateway) refuseOwnBuild(w http.ResponseWriter, recipient, legType, msg string) {
	g.observe(ObserverEvent{
		Kind: "leg.failed", Direction: "originate", LegType: legType,
		Counterpart: recipient, Detail: msg,
	})
	refuseBuilder(w, msg)
}

// ValidNPI reports whether npi is an NPI: ten digits whose last is the NPI
// check digit, the Luhn check digit over "80840" and the first nine digits.
func ValidNPI(npi string) bool {
	if len(npi) != 10 {
		return false
	}
	digits := "80840" + npi[:9]
	sum := 0
	for i := len(digits) - 1; i >= 0; i-- {
		c := digits[i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if (len(digits)-1-i)%2 == 0 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return npi[9] == byte('0'+(10-sum%10)%10)
}

// msgSentNPIInvalid refuses a request that states an npi that is not one.
const msgSentNPIInvalid = "npi must be a ten-digit NPI with a valid check digit"

// requireValidStatedNPI refuses a request whose stated npi is not an NPI: it
// would be stated on the participant's behalf as one. It returns false when
// it refused.
func requireValidStatedNPI(w http.ResponseWriter, npi string) bool {
	if npi == "" || ValidNPI(npi) {
		return true
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msgSentNPIInvalid})
	return false
}
