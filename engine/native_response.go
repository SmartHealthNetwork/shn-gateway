package engine

import (
	"errors"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// NativePASFacts describes the existing native submit response contract. It is
// not a profile-certification verdict, and no response bytes are rewritten.
//
// The ClaimResponse is named three ways, each exactly as the payer stated it and
// none minted here: ClaimResponseID is its resource id, "" when the payer placed
// it under a urn:uuid fullUrl without one; ClaimResponseIdentity is the entry's
// fullUrl, the identity the response graph resolves it by, never empty;
// ClaimResponseIdentifier is the payer's own first ClaimResponse.identifier as
// "system|value", "" when it states none. A consumer that needs the payer's
// name for the answer reads the identifier, then the identity.
type NativePASFacts struct {
	Decision                string
	ClaimResponseID         string
	ClaimResponseIdentity   string
	ClaimResponseIdentifier string
	ReferencesComplete      bool
}

var errNativePASBundle = errors.New("invalid native PAS response Bundle")
var errNativePASDecision = errors.New("invalid native PAS response decision")

// InspectNativePASResponse checks complete Bundle closure and the explicit payer
// decision with the same guards used by native ingress (FR-G28).
func InspectNativePASResponse(body []byte) (NativePASFacts, error) {
	g, err := readPASGraph(body)
	if err != nil {
		return NativePASFacts{}, errNativePASBundle
	}
	if err = g.validate(); err != nil {
		return NativePASFacts{}, errNativePASBundle
	}
	pended, _, err := shnsdk.ParsePendedResponseDetail(body)
	if err != nil {
		return NativePASFacts{}, errNativePASDecision
	}
	decision := "pending"
	if !pended {
		result, err := shnsdk.ParseClaimResponse(body)
		if err != nil {
			return NativePASFacts{}, errNativePASDecision
		}
		decision = result.Outcome
		if result.Partial {
			decision = "partial"
		}
	}
	id, _ := g.response.resource["id"].(string)
	return NativePASFacts{
		Decision:                decision,
		ClaimResponseID:         id,
		ClaimResponseIdentity:   g.response.fullURL,
		ClaimResponseIdentifier: firstIdentifierKey(g.response.resource),
		ReferencesComplete:      true,
	}, nil
}

// firstIdentifierKey is the resource's first identifier as "system|value" (or
// the bare value when it has no system), "" when it states none usable.
func firstIdentifierKey(r map[string]any) string {
	list, _ := r["identifier"].([]any)
	for _, v := range list {
		ident, _ := v.(map[string]any)
		system, _ := ident["system"].(string)
		value, _ := ident["value"].(string)
		if value == "" {
			continue
		}
		if system == "" {
			return value
		}
		return system + "|" + value
	}
	return ""
}
