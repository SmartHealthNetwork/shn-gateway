package scenariodriver

import (
	"encoding/json"
	"fmt"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
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

// Cards is a CDS Hooks CRD answer: its cards, and the coverage information it
// carries wherever the payer put it — in an update or create system action, in
// a card suggestion's action, or in the card extension object earlier versions
// of this package's builders wrote. The accessors read the coverage
// information.
type Cards struct {
	Cards []Card `json:"cards"`

	coverage []shnsdk.CoverageInformation
}

// ParseCards reads a CDS Hooks CRD answer without changing it. It returns an
// error if the body is not one JSON object with unique member names, or if the
// answer carries no coverage information. Zero-value Cards accessors are safe
// and return empty strings / nil, never panic.
func ParseCards(body []byte) (Cards, error) {
	obs, err := shnsdk.ParseCRDResponse(body)
	if err != nil {
		return Cards{}, fmt.Errorf("parse CRD answer: %w", err)
	}
	var c Cards
	if err := json.Unmarshal(body, &c); err != nil {
		return Cards{}, fmt.Errorf("unmarshal cards: %w", err)
	}
	for _, o := range obs.Orders {
		c.coverage = append(c.coverage, o.Coverage...)
	}
	if len(c.coverage) == 0 {
		return Cards{}, fmt.Errorf("CRD answer carries no coverage information")
	}
	return c, nil
}

// Covered returns the first coverage information's covered value ("" if none).
func (c Cards) Covered() string {
	if len(c.coverage) == 0 {
		return ""
	}
	return c.coverage[0].Covered
}

// PANeeded returns the first coverage information's pa-needed value ("" if none).
func (c Cards) PANeeded() string {
	if len(c.coverage) == 0 {
		return ""
	}
	return c.coverage[0].PANeeded
}

// Questionnaires returns every coverage information's questionnaire canonicals,
// in answer order (nil if none).
func (c Cards) Questionnaires() []string {
	var out []string
	for _, ci := range c.coverage {
		out = append(out, ci.Questionnaires...)
	}
	return out
}
