package engine

import (
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
)

// The engine's exchange record vocabulary is package recording's, not a copy
// of it: each deprecated list is the same slice, so a reader of the recording
// package and the gateway that records an exchange can never disagree.
func TestRecordingVocabularyIsTheEngines(t *testing.T) {
	for name, pair := range map[string][2][]string{
		"ExchangeDirections":  {ExchangeDirections, vocab.ExchangeDirections},
		"ExchangeOutcomes":    {ExchangeOutcomes, vocab.ExchangeOutcomes},
		"RefusalParties":      {RefusalParties, vocab.RefusalParties},
		"RefusalRules":        {RefusalRules, vocab.RefusalRules},
		"BackendErrorClasses": {BackendErrorClasses, vocab.BackendErrorClasses},
		"AnswerErrorClasses":  {AnswerErrorClasses, vocab.AnswerErrorClasses},
	} {
		if len(pair[0]) == 0 || &pair[0][0] != &pair[1][0] || len(pair[0]) != len(pair[1]) {
			t.Errorf("engine.%s is not recording.%s", name, name)
		}
	}
	if ExchangeOther != vocab.ExchangeOther || RefusalIntegrity != vocab.RefusalIntegrity || BackendCancelled != vocab.BackendCancelled {
		t.Error("the engine's constants are not recording's")
	}
}
