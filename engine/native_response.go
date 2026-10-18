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
// own check of an answer at a line (validateNativePASResponseAt) reads by it
// too, so a reader of retained evidence that judges an answer at its line
// agrees with the gateway. An inquiry answer has its own rule
// (PASInquiryAnswerReadsAt). It reads; it decides nothing for the relay.
func InspectNativePASResponseAt(body []byte, line string) (NativePASFacts, error) {
	f, err := inspectNativePASAnswerAt(body, line)
	switch {
	case err == nil:
		return f, nil
	case errors.Is(err, errNativePASDecision):
		return NativePASFacts{}, errNativePASDecision
	}
	return NativePASFacts{}, errNativePASBundle
}

// inspectNativePASAnswerAt is InspectNativePASResponseAt with the graph's own
// refusal kept (its cause and text), or errNativePASDecision.
func inspectNativePASAnswerAt(body []byte, line string) (NativePASFacts, error) {
	// A repeated member name, as the gateway's check finds one (repeatedMember,
	// which also counts names equal but for case), is no one reading of the
	// answer; the gateway names it in its own text.
	if _, ok := repeatedMember(body); ok {
		return NativePASFacts{}, pasGraphError(PASRefusalUndecodable)
	}
	g, err := readPASGraph(body)
	if err != nil {
		return NativePASFacts{}, err
	}
	if err = g.validateAnswerAt(line); err != nil {
		return NativePASFacts{}, err
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
	_, err := inspectNativePASAnswerAt(body, line)
	if err == nil {
		return ""
	}
	if !errors.Is(err, errNativePASDecision) {
		return pasRefusalCause(err)
	}
	g, err := readPASGraph(body)
	if err != nil {
		return pasRefusalCause(err)
	}
	outcome, _ := g.response.resource["outcome"].(string)
	items, _ := g.response.resource["item"].([]any)
	if outcome == "" && len(items) == 0 {
		return PASRefusalNoDecision
	}
	return PASRefusalDecision
}

// NativePASGraphRefusalCause names why CheckNativePASGraphWithoutClaimResponse
// refuses body, as one of NativePASRefusalCauses, and "" when it holds.
func NativePASGraphRefusalCause(body []byte) string {
	if err := CheckNativePASGraphWithoutClaimResponse(body); err != nil {
		return pasRefusalCause(err)
	}
	return ""
}

// pasRefusalCause is the cause a response graph refusal names.
func pasRefusalCause(err error) string {
	var c *pasGraphCauseError
	if errors.As(err, &c) {
		return c.cause
	}
	if pasGraphRefusalOf(err) != nil {
		return PASRefusalReference
	}
	return PASRefusalUndecodable
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
	if _, dup := repeatedMember(body); dup {
		return "", "", "", false
	}
	var r *pasGraphRefusal
	switch line {
	case "2.1", "2.2":
		g, err := readPASGraph(body)
		if err != nil {
			return "", "", "", false
		}
		r = pasGraphRefusalOf(g.validateAnswerAt(line))
	default:
		g, err := readPASGraphOf(body, false)
		if err != nil {
			return "", "", "", false
		}
		r = pasGraphRefusalOf(g.validate())
	}
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
		return pasGraphStructural(PASRefusalClaimResponses, "the Bundle carries a ClaimResponse")
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

// PASInquiryAnswerReadsAt is the rule the gateway reads a PAS inquiry answer
// by at IG line ("2.0", "2.1", "2.2"): the answer is one of the shapes the
// published operation definitions declare, its graph is not closed at any
// line, and at 2.2 every ClaimResponse it carries names its Claim or gives a
// Data Absent Reason on its request. ok reports whether the answer meets it,
// and detail is the refusal when it does not, "" when it does. It is exported
// so a reader of retained evidence judges an inquiry answer as the gateway
// does. It reads; it decides nothing for the relay.
func PASInquiryAnswerReadsAt(body []byte, line string) (ok bool, detail string) {
	lr := validatePASInquiryAnswerAt(body, line)
	if lr.Status == 0 {
		return true, ""
	}
	return false, lr.Message
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
