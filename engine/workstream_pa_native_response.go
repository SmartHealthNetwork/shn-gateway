package engine

import (
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"
	"github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer"
)

// The readers of a native PAS answer moved to package pasanswer, the Prior
// Authorization workstream's own. The names below forward to them, unchanged
// in behaviour, until v1.0.0, the inquiry answer's readers
// (ConsistentPASInquiryAnswerSubjects, PASInquiryAnswerReadsAt) included.

// NativePASFacts describes the existing native submit response contract. It
// keeps its own type, field for field the same as pasanswer.NativePASFacts
// (the forwarders below convert one to the other, which would not compile if
// the fields differed).
//
// Deprecated: use pasanswer.NativePASFacts (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
type NativePASFacts struct {
	Decision                string
	ClaimResponseID         string
	ClaimResponseIdentity   string
	ClaimResponseIdentifier string
	ReferencesComplete      bool
}

// errNativePASDecision is the refusal of an answer whose payer decision the
// rule does not read (pasgraph.ErrDecision).
var errNativePASDecision = pasgraph.ErrDecision

// inspectNativePASAnswerAt is InspectNativePASResponseAt with the graph's own
// refusal kept (its cause and text), or errNativePASDecision.
func inspectNativePASAnswerAt(body []byte, line string) (pasgraph.Facts, error) {
	return pasgraph.InspectAnswerAt(body, line)
}

// InspectNativePASResponse checks complete Bundle closure and the explicit
// payer decision (FR-G28), read at PAS 2.0.1.
//
// Deprecated: use pasanswer.InspectNativePASResponse (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
func InspectNativePASResponse(body []byte) (NativePASFacts, error) {
	f, err := pasanswer.InspectNativePASResponse(body)
	return NativePASFacts(f), err
}

// InspectNativePASResponseAt reads a PAS $submit or amendment answer
// exchanged at IG line as the gateway reads one there.
//
// Deprecated: use pasanswer.InspectNativePASResponseAt (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
func InspectNativePASResponseAt(body []byte, line string) (NativePASFacts, error) {
	f, err := pasanswer.InspectNativePASResponseAt(body, line)
	return NativePASFacts(f), err
}

// The causes a refusal of a native PAS response Bundle is named by.
const (
	// PASRefusalUndecodable is pasanswer.PASRefusalUndecodable.
	//
	// Deprecated: use pasanswer.PASRefusalUndecodable (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalUndecodable = pasanswer.PASRefusalUndecodable
	// PASRefusalNotCollection is pasanswer.PASRefusalNotCollection.
	//
	// Deprecated: use pasanswer.PASRefusalNotCollection (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalNotCollection = pasanswer.PASRefusalNotCollection
	// PASRefusalNoEntries is pasanswer.PASRefusalNoEntries.
	//
	// Deprecated: use pasanswer.PASRefusalNoEntries (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalNoEntries = pasanswer.PASRefusalNoEntries
	// PASRefusalEntry is pasanswer.PASRefusalEntry.
	//
	// Deprecated: use pasanswer.PASRefusalEntry (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalEntry = pasanswer.PASRefusalEntry
	// PASRefusalFullURL is pasanswer.PASRefusalFullURL.
	//
	// Deprecated: use pasanswer.PASRefusalFullURL (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalFullURL = pasanswer.PASRefusalFullURL
	// PASRefusalIdentity is pasanswer.PASRefusalIdentity.
	//
	// Deprecated: use pasanswer.PASRefusalIdentity (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalIdentity = pasanswer.PASRefusalIdentity
	// PASRefusalRepeatedIdentity is pasanswer.PASRefusalRepeatedIdentity.
	//
	// Deprecated: use pasanswer.PASRefusalRepeatedIdentity (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalRepeatedIdentity = pasanswer.PASRefusalRepeatedIdentity
	// PASRefusalNoClaimResponse is pasanswer.PASRefusalNoClaimResponse.
	//
	// Deprecated: use pasanswer.PASRefusalNoClaimResponse (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalNoClaimResponse = pasanswer.PASRefusalNoClaimResponse
	// PASRefusalClaimResponses is pasanswer.PASRefusalClaimResponses.
	//
	// Deprecated: use pasanswer.PASRefusalClaimResponses (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalClaimResponses = pasanswer.PASRefusalClaimResponses
	// PASRefusalRequest is pasanswer.PASRefusalRequest.
	//
	// Deprecated: use pasanswer.PASRefusalRequest (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalRequest = pasanswer.PASRefusalRequest
	// PASRefusalContained is pasanswer.PASRefusalContained.
	//
	// Deprecated: use pasanswer.PASRefusalContained (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalContained = pasanswer.PASRefusalContained
	// PASRefusalNested is pasanswer.PASRefusalNested.
	//
	// Deprecated: use pasanswer.PASRefusalNested (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalNested = pasanswer.PASRefusalNested
	// PASRefusalEmptyReference is pasanswer.PASRefusalEmptyReference.
	//
	// Deprecated: use pasanswer.PASRefusalEmptyReference (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalEmptyReference = pasanswer.PASRefusalEmptyReference
	// PASRefusalReference is pasanswer.PASRefusalReference.
	//
	// Deprecated: use pasanswer.PASRefusalReference (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalReference = pasanswer.PASRefusalReference
	// PASRefusalBound is pasanswer.PASRefusalBound.
	//
	// Deprecated: use pasanswer.PASRefusalBound (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalBound = pasanswer.PASRefusalBound
	// PASRefusalPolicy is pasanswer.PASRefusalPolicy.
	//
	// Deprecated: use pasanswer.PASRefusalPolicy (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalPolicy = pasanswer.PASRefusalPolicy
	// PASRefusalNoDecision is pasanswer.PASRefusalNoDecision.
	//
	// Deprecated: use pasanswer.PASRefusalNoDecision (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalNoDecision = pasanswer.PASRefusalNoDecision
	// PASRefusalDecision is pasanswer.PASRefusalDecision.
	//
	// Deprecated: use pasanswer.PASRefusalDecision (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
	PASRefusalDecision = pasanswer.PASRefusalDecision
)

// NativePASRefusalCauses are the causes above, in a fixed order: a copy of
// pasanswer.NativePASRefusalCauses, so changing one list leaves the other.
//
// Deprecated: use pasanswer.NativePASRefusalCauses (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
var NativePASRefusalCauses = slices.Clone(pasanswer.NativePASRefusalCauses)

// NativePASRefusalCause names why InspectNativePASResponse refuses body.
//
// Deprecated: use pasanswer.NativePASRefusalCause (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
func NativePASRefusalCause(body []byte) string { return pasanswer.NativePASRefusalCause(body) }

// NativePASRefusalCauseAt is NativePASRefusalCause for an answer read at IG
// line.
//
// Deprecated: use pasanswer.NativePASRefusalCauseAt (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
func NativePASRefusalCauseAt(body []byte, line string) string {
	return pasanswer.NativePASRefusalCauseAt(body, line)
}

// NativePASGraphRefusalCause names why CheckNativePASGraphWithoutClaimResponse
// refuses body.
//
// Deprecated: use pasanswer.NativePASGraphRefusalCause (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
func NativePASGraphRefusalCause(body []byte) string {
	return pasanswer.NativePASGraphRefusalCause(body)
}

// NativePASUnresolvedReference names the first reference the response graph
// rule finds a native PAS response Bundle does not resolve.
//
// Deprecated: use pasanswer.NativePASUnresolvedReference (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
func NativePASUnresolvedReference(body []byte) (holder, element, reference string, ok bool) {
	return pasanswer.NativePASUnresolvedReference(body)
}

// NativePASUnresolvedReferenceAt is NativePASUnresolvedReference for an
// answer read at IG line.
//
// Deprecated: use pasanswer.NativePASUnresolvedReferenceAt (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
func NativePASUnresolvedReferenceAt(body []byte, line string) (holder, element, reference string, ok bool) {
	return pasanswer.NativePASUnresolvedReferenceAt(body, line)
}

// CheckNativePASGraphWithoutClaimResponse applies the response graph rule to a
// PAS response Bundle that carries no ClaimResponse.
//
// Deprecated: use pasanswer.CheckNativePASGraphWithoutClaimResponse (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
func CheckNativePASGraphWithoutClaimResponse(body []byte) error {
	return pasanswer.CheckNativePASGraphWithoutClaimResponse(body)
}

// ConsistentPASInquiryAnswerSubjects reports whether every patient a PAS
// inquiry answer names, in every response Bundle and at every depth, is the
// same one: the subject rule the inquiry leg applies to a payer's answer
// (RulePatientAnswer). An answer that names none is consistent, and one that
// cannot be read is not. It reads; it decides nothing for the relay.
//
// Deprecated: use pasanswer.ConsistentInquiryAnswerSubjects (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
func ConsistentPASInquiryAnswerSubjects(answer []byte) bool {
	return pasanswer.ConsistentInquiryAnswerSubjects(answer)
}

// PASInquiryAnswerReadsAt is the rule the gateway reads a PAS inquiry answer
// by at IG line ("2.0", "2.1", "2.2"): the answer is one of the shapes the
// published operation definitions declare, its graph is not closed at any
// line, and at 2.2 every ClaimResponse it carries names its Claim or gives a
// Data Absent Reason on its request. ok reports whether the answer meets it,
// and detail is the refusal when it does not, "" when it does. It reads; it
// decides nothing for the relay.
//
// Deprecated: use pasanswer.InquiryAnswerReadsAt (package github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer).
func PASInquiryAnswerReadsAt(body []byte, line string) (ok bool, detail string) {
	return pasanswer.InquiryAnswerReadsAt(body, line)
}
