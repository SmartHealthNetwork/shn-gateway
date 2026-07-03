package scenariodriver

import (
	"encoding/json"
	"fmt"
)

// CardExtension is the SHN CardCoverage projection in a CDS Hooks card response,
// carrying the payer's coverage determination and questionnaire canonicals.
type CardExtension struct {
	Covered        string   `json:"covered"`
	PANeeded       string   `json:"paNeeded"`
	Questionnaires []string `json:"questionnaires"`
}

// Card is a CDS Hooks card from a CRD decision response, carrying a summary,
// indicator, and the SHN-normalized extension.
type Card struct {
	Summary   string        `json:"summary"`
	Indicator string        `json:"indicator"`
	Extension CardExtension `json:"extension"`
}

// Cards is a CDS Hooks cards response (cards[] array). It provides normalized
// accessors over the first card's coverage determination and all cards' questionnaire canonicals.
type Cards struct {
	Cards []Card `json:"cards"`
}

// ParseCards unmarshals a CDS Hooks cards response and returns an error if the JSON is
// invalid or the response contains zero cards. Zero-value Cards accessors are safe and
// return empty strings / nil, never panic.
func ParseCards(body []byte) (Cards, error) {
	var c Cards
	if err := json.Unmarshal(body, &c); err != nil {
		return Cards{}, fmt.Errorf("unmarshal cards: %w", err)
	}
	if len(c.Cards) == 0 {
		return Cards{}, fmt.Errorf("cards response contains zero cards")
	}
	return c, nil
}

// Covered returns the first card's Extension.Covered ("" if no cards).
func (c Cards) Covered() string {
	if len(c.Cards) == 0 {
		return ""
	}
	return c.Cards[0].Extension.Covered
}

// PANeeded returns the first card's Extension.PANeeded ("" if no cards).
func (c Cards) PANeeded() string {
	if len(c.Cards) == 0 {
		return ""
	}
	return c.Cards[0].Extension.PANeeded
}

// Questionnaires returns all cards' Extension.Questionnaires in order,
// concatenating the questionnaire canonicals across all cards (nil if no cards).
func (c Cards) Questionnaires() []string {
	var out []string
	for _, card := range c.Cards {
		out = append(out, card.Extension.Questionnaires...)
	}
	return out
}
