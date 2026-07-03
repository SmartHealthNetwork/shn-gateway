package scenariodriver

import "testing"

const cardsFixture = `{"cards":[
  {"summary":"Prior authorization required","indicator":"warning",
   "extension":{"covered":"covered","paNeeded":"auth-needed",
     "questionnaires":["http://example.org/Questionnaire/home-oxygen|1.0"]}},
  {"summary":"Second card","extension":{"questionnaires":["http://example.org/Questionnaire/q2"]}}
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

func TestParseCards_Errors(t *testing.T) {
	if _, err := ParseCards([]byte(`{"cards":[]}`)); err == nil {
		t.Fatal("want error on zero cards")
	}
	if _, err := ParseCards([]byte(`nope`)); err == nil {
		t.Fatal("want error on non-JSON")
	}
	var zero Cards
	if zero.Covered() != "" || zero.PANeeded() != "" || zero.Questionnaires() != nil {
		t.Fatal("zero-value Cards projections must be empty, not panic")
	}
}
