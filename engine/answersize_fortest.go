//go:build shn_testdouble

package engine

// HubAnswerRefusalReasonsForTest is the Hub's reasons for an answer refused
// after the recipient answered, as this gateway reads them: its own refusal of
// an answer over the limit, and its relay of a recipient gateway's attested
// refusal. The root module's tests pin the Hub's names to these.
func HubAnswerRefusalReasonsForTest() (tooLarge, recipientRefused string) {
	return hubReasonAnswerTooLarge, hubReasonRecipientRefusedAnswer
}
