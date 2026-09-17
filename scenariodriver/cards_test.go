package scenariodriver

import "testing"

const cardsFixture = `{"cards":[
  {"summary":"Prior authorization required","indicator":"warning",
   "extension":{"covered":"covered","paNeeded":"auth-needed",
     "questionnaires":["http://example.org/Questionnaire/home-oxygen|1.0"]}},
  {"summary":"Second card","extension":{"covered":"conditional","questionnaires":["http://example.org/Questionnaire/q2"]}}
]}`

func TestParseCards(t *testing.T) {
	c, err := ParseCards([]byte(cardsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if c.Covered() != "covered" || c.PANeeded() != "auth-needed" {
		t.Fatalf("first-card projection = %q/%q", c.Covered(), c.PANeeded())
	}
	if q := c.Questionnaires(); len(q) != 2 || q[0] != "http://example.org/Questionnaire/home-oxygen|1.0" {
		t.Fatalf("questionnaires = %v", q)
	}
}

// realPayerAnswer is the reference payer's shape: no card, the order in an
// update system action carrying its coverage information.
const realPayerAnswer = `{"cards":[],"systemActions":[{"type":"update","description":"Add coverage information","resource":{"resourceType":"DeviceRequest","id":"dr1","extension":[
  {"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information","extension":[
    {"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"auth-needed"},
    {"url":"questionnaire","valueCanonical":"http://example.org/fhir/Questionnaire/PriorAuthRequired"}]}]}}]}`

func TestParseCards_SystemActions(t *testing.T) {
	c, err := ParseCards([]byte(realPayerAnswer))
	if err != nil {
		t.Fatal(err)
	}
	if c.Covered() != "covered" || c.PANeeded() != "auth-needed" || len(c.Cards) != 0 {
		t.Fatalf("projection = %q/%q cards=%d", c.Covered(), c.PANeeded(), len(c.Cards))
	}
	if q := c.Questionnaires(); len(q) != 1 || q[0] != "http://example.org/fhir/Questionnaire/PriorAuthRequired" {
		t.Fatalf("questionnaires = %v", q)
	}
}

func TestParseCards_Errors(t *testing.T) {
	if _, err := ParseCards([]byte(`{"cards":[]}`)); err == nil {
		t.Fatal("want error on an answer with no coverage information")
	}
	if _, err := ParseCards([]byte(`{"cards":[],"cards":[]}`)); err == nil {
		t.Fatal("want error on a repeated member")
	}
	if _, err := ParseCards([]byte(`nope`)); err == nil {
		t.Fatal("want error on non-JSON")
	}
	var zero Cards
	if zero.Covered() != "" || zero.PANeeded() != "" || zero.Questionnaires() != nil {
		t.Fatal("zero-value Cards projections must be empty, not panic")
	}
}
