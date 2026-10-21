package engine

import "github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"

// pasInquiryAnswerSubjects is pasgraph.InquiryAnswerSubjects, under the name
// the inquiry leg's rows read the answer's subjects by.
func pasInquiryAnswerSubjects(answer []byte) (map[string]bool, bool) {
	return pasgraph.InquiryAnswerSubjects(answer)
}
