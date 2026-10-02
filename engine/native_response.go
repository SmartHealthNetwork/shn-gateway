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

// NativePASUnresolvedReference names the first reference the response graph
// rule (FR-G28) finds a native PAS response Bundle does not resolve: the
// resource type of the entry holding it ("Bundle" for Bundle or entry
// metadata), the element within that resource, and the reference exactly as
// the payer wrote it. ok is false when the graph resolves, and when the Bundle
// is refused for a reason that names no reference. It states why
// InspectNativePASResponse refused a Bundle; it decides nothing more, and it
// names no other value from the Bundle: neither the holder's id nor the address
// the reference resolved to.
func NativePASUnresolvedReference(body []byte) (holder, element, reference string, ok bool) {
	g, err := readPASGraphOf(body, false)
	if err != nil {
		return "", "", "", false
	}
	r := pasGraphRefusalOf(g.validate())
	if r == nil {
		return "", "", "", false
	}
	return r.holder, r.Path, r.Reference, true
}

// CheckNativePASGraphWithoutClaimResponse applies the response graph rule to a
// PAS response Bundle that carries no ClaimResponse, as an inquiry response
// Bundle may: a collection whose every entry carries a resource under an
// absolute fullUrl, and whose every reference resolves in it. It returns nil
// when that holds, and refuses a Bundle that carries a ClaimResponse (read
// such a Bundle with InspectNativePASResponse). NativePASUnresolvedReference
// names the reference a refusal is about. It reads; it decides nothing for the
// relay.
func CheckNativePASGraphWithoutClaimResponse(body []byte) error {
	g, err := readPASGraphOf(body, false)
	if err != nil {
		return err
	}
	if g.response != nil {
		return pasGraphStructural("the Bundle carries a ClaimResponse")
	}
	return g.validate()
}

// ConsistentPASInquiryAnswerSubjects reports whether every patient a PAS
// inquiry answer names, in every response Bundle and at every depth, is the
// same one: the subject rule the inquiry leg applies to a payer's answer
// (RulePatientAnswer). An answer that names none is consistent, and one that
// cannot be read is not. It reads; it decides nothing for the relay.
func ConsistentPASInquiryAnswerSubjects(answer []byte) bool {
	return consistentPASInquiryAnswerSubjects(answer)
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
