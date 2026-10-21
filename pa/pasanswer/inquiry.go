package pasanswer

import "github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"

// InquiryAnswerReadsAt is the rule the gateway reads a PAS inquiry answer by
// at IG line ("2.0", "2.1", "2.2"): the answer is one of the shapes the
// published operation definitions declare, its graph is not closed at any
// line, and at 2.2 every ClaimResponse it carries names its Claim or gives a
// Data Absent Reason on its request. ok reports whether the answer meets it,
// and detail is the refusal when it does not, "" when it does. A reader of
// retained evidence judges an inquiry answer by it as the gateway does. It
// reads; it decides nothing for the relay.
func InquiryAnswerReadsAt(body []byte, line string) (ok bool, detail string) {
	return pasgraph.ValidateInquiryAnswerAt(body, line)
}

// ConsistentInquiryAnswerSubjects reports whether every patient a PAS inquiry
// answer names, in every response Bundle and at every depth, is the same one:
// the subject rule the inquiry leg applies to a payer's answer. An answer that
// names none is consistent, and one that cannot be read is not. It reads; it
// decides nothing for the relay.
func ConsistentInquiryAnswerSubjects(answer []byte) bool {
	return pasgraph.ConsistentInquiryAnswerSubjects(answer)
}
