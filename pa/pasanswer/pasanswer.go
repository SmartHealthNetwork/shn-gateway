// Package pasanswer holds the Prior Authorization workstream's readers of a
// native PAS answer: whether a PAS $submit or amendment answer reads at its
// IG line as the gateway reads it and what decision it states, whether an
// inquiry response Bundle's graph closes, and, when an answer does not read,
// the cause and the reference that refused it. They read
// retained evidence; they decide nothing for the relay.
//
// The readers moved here from package engine, which keeps its names
// (engine.InspectNativePASResponse and the rest) as deprecated forwarders to
// these until v1.0.0. These readers and the gateway's own checks of an answer
// read it by one definition (the response graph rule in the gateway's
// internal pasgraph package), so a reader of retained evidence agrees with the
// gateway that relayed the answer.
package pasanswer

import (
	"errors"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
	"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"
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

// InspectNativePASResponse checks complete Bundle closure and the explicit payer
// decision with the same guards used by native ingress (FR-G28): the answer
// read at PAS 2.0.1, or with no line known (InspectNativePASResponseAt).
func InspectNativePASResponse(body []byte) (NativePASFacts, error) {
	return InspectNativePASResponseAt(body, "")
}

// InspectNativePASResponseAt reads a PAS $submit or amendment answer
// exchanged at IG line ("2.0", "2.1", "2.2"; any other, or none, reads as
// 2.0.1) exactly as the gateway reads one there: the response graph rule at
// that line (FR-G28 as each line's specification closes an answer), then the
// payer's explicit decision. It is the one per-line definition: the gateway's
// own check of an answer at a line reads by it too, so a reader of retained
// evidence that judges an answer at its line agrees with the gateway. An
// inquiry answer has its own rule (InquiryAnswerReadsAt). It reads;
// it decides nothing for the relay.
func InspectNativePASResponseAt(body []byte, line string) (NativePASFacts, error) {
	f, err := pasgraph.InspectAnswerAt(body, line)
	switch {
	case err == nil:
		return NativePASFacts{
			Decision:                f.Decision,
			ClaimResponseID:         f.ClaimResponseID,
			ClaimResponseIdentity:   f.ClaimResponseIdentity,
			ClaimResponseIdentifier: f.ClaimResponseIdentifier,
			ReferencesComplete:      true,
		}, nil
	case errors.Is(err, pasgraph.ErrDecision):
		return NativePASFacts{}, pasgraph.ErrDecision
	}
	return NativePASFacts{}, errNativePASBundle
}

// The causes a refusal of a native PAS response Bundle is named by
// (NativePASRefusalCause, NativePASGraphRefusalCause): a closed set, each
// naming why the Bundle was not read, never a value it carries.
const (
	// PASRefusalUndecodable: not one JSON object read strictly (a repeated
	// member name, a value of no JSON type, text after it).
	PASRefusalUndecodable = "undecodable"
	// PASRefusalNotCollection: not a Bundle of type collection.
	PASRefusalNotCollection = "not-collection"
	// PASRefusalNoEntries: a Bundle with no entry.
	PASRefusalNoEntries = "no-entries"
	// PASRefusalEntry: an entry that is not an object, has no fullUrl, or
	// carries no resource.
	PASRefusalEntry = "entry"
	// PASRefusalFullURL: a fullUrl that is not an absolute, versionless
	// identity, or does not agree with its resource's identity.
	PASRefusalFullURL = "fullurl"
	// PASRefusalIdentity: a resource without a usable resourceType and id.
	PASRefusalIdentity = "identity"
	// PASRefusalRepeatedIdentity: two entries under one fullUrl.
	PASRefusalRepeatedIdentity = "repeated-identity"
	// PASRefusalNoClaimResponse: no ClaimResponse where the Bundle must carry
	// one.
	PASRefusalNoClaimResponse = "no-claimresponse"
	// PASRefusalClaimResponses: more than one ClaimResponse, or one where the
	// Bundle may carry none.
	PASRefusalClaimResponses = "claimresponses"
	// PASRefusalRequest: a ClaimResponse.request that names the request Claim
	// neither by a reference to a Claim nor by an identifier (PAS 2.1.0 and
	// 2.2.1).
	PASRefusalRequest = "request"
	// PASRefusalContained: contained resources that are not an array of
	// resources each with a unique, usable identity, or that contain more.
	PASRefusalContained = "contained"
	// PASRefusalNested: a Bundle or Parameters carried inside a resource.
	PASRefusalNested = "nested"
	// PASRefusalEmptyReference: a reference that is empty or not a string.
	PASRefusalEmptyReference = "empty-reference"
	// PASRefusalReference: a reference the Bundle does not resolve
	// (NativePASUnresolvedReference names it).
	PASRefusalReference = "reference"
	// PASRefusalBound: more bytes, entries, resources, references or depth
	// than the rule reads.
	PASRefusalBound = "bound"
	// PASRefusalPolicy: a Bundle another reference policy was made for.
	PASRefusalPolicy = "policy"
	// PASRefusalNoDecision: a ClaimResponse that states no outcome and no
	// item, so there is no decision to read.
	PASRefusalNoDecision = "no-decision"
	// PASRefusalDecision: a decision stated in a way this rule does not read.
	PASRefusalDecision = "decision"
)

// NativePASRefusalCauses are the causes above, in a fixed order.
var NativePASRefusalCauses = []string{
	PASRefusalUndecodable, PASRefusalNotCollection, PASRefusalNoEntries, PASRefusalEntry, PASRefusalFullURL,
	PASRefusalIdentity, PASRefusalRepeatedIdentity, PASRefusalNoClaimResponse, PASRefusalClaimResponses,
	PASRefusalRequest, PASRefusalContained, PASRefusalNested, PASRefusalEmptyReference, PASRefusalReference,
	PASRefusalBound, PASRefusalPolicy, PASRefusalNoDecision, PASRefusalDecision,
}

// NativePASRefusalCause names why InspectNativePASResponse refuses body, as
// one of NativePASRefusalCauses, and "" when it reads it. It names the cause
// alone, never a value from the Bundle, so a reader of retained evidence can
// say why an answer was not read. It reads; it decides nothing for the relay.
func NativePASRefusalCause(body []byte) string { return NativePASRefusalCauseAt(body, "") }

// NativePASRefusalCauseAt is NativePASRefusalCause for an answer read at IG
// line (InspectNativePASResponseAt).
func NativePASRefusalCauseAt(body []byte, line string) string {
	_, err := pasgraph.InspectAnswerAt(body, line)
	if err == nil {
		return ""
	}
	if !errors.Is(err, pasgraph.ErrDecision) {
		return pasgraph.RefusalCause(err)
	}
	g, err := pasgraph.Read(body)
	if err != nil {
		return pasgraph.RefusalCause(err)
	}
	outcome, _ := g.Response.Resource["outcome"].(string)
	items, _ := g.Response.Resource["item"].([]any)
	if outcome == "" && len(items) == 0 {
		return PASRefusalNoDecision
	}
	return PASRefusalDecision
}

// NativePASGraphRefusalCause names why CheckNativePASGraphWithoutClaimResponse
// refuses body, as one of NativePASRefusalCauses, and "" when it holds.
func NativePASGraphRefusalCause(body []byte) string {
	if err := CheckNativePASGraphWithoutClaimResponse(body); err != nil {
		return pasgraph.RefusalCause(err)
	}
	return ""
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
	return NativePASUnresolvedReferenceAt(body, "")
}

// NativePASUnresolvedReferenceAt is NativePASUnresolvedReference for an answer
// read at IG line (InspectNativePASResponseAt): the first reference that
// line's rule refuses. The lines differ for a Bundle with no ClaimResponse:
// at 2.1 and 2.2 it names no reference (ok is false), while at 2.0, or no
// line, its references are read all the same, though the answer is refused
// for carrying no ClaimResponse (PASRefusalNoClaimResponse) first. An answer
// that repeats a member name is not read at all, so it names none.
func NativePASUnresolvedReferenceAt(body []byte, line string) (holder, element, reference string, ok bool) {
	if _, dup := jsonmember.Repeated(body); dup {
		return "", "", "", false
	}
	var r *pasgraph.Refusal
	switch line {
	case "2.1", "2.2":
		g, err := pasgraph.Read(body)
		if err != nil {
			return "", "", "", false
		}
		r = pasgraph.RefusalOf(g.ValidateAnswerAt(line))
	default:
		g, err := pasgraph.ReadOf(body, false)
		if err != nil {
			return "", "", "", false
		}
		r = pasgraph.RefusalOf(g.Validate())
	}
	if r == nil {
		return "", "", "", false
	}
	return r.Holder(), r.Path, r.Reference, true
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
	g, err := pasgraph.ReadOf(body, false)
	if err != nil {
		return err
	}
	if g.Response != nil {
		return pasgraph.Structural(PASRefusalClaimResponses, "the Bundle carries a ClaimResponse")
	}
	return g.Validate()
}
