package legtaxonomy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestPA_DecodesWithRoutableLegs(t *testing.T) {
	tx := PA()
	routable := 0
	for _, l := range tx.Legs {
		if l.Routable() {
			routable++
		}
	}
	if tx.Workstream != "pa" || routable < 8 {
		t.Fatalf("workstream %q with %d routable legs, want pa with >= 8", tx.Workstream, routable)
	}
}

// TestFQS_DecodesWithTheFederatedQuery: the Federated Query Services file is
// the one routable federated-query leg, staged under its own prefix.
func TestFQS_DecodesWithTheFederatedQuery(t *testing.T) {
	tx := FQS()
	if tx.Workstream != "fqs" || len(tx.Legs) != 1 || tx.Legs[0].TransactionType != "federated-query" || tx.Legs[0].StagedName != "fqs.federated-query" || !tx.Legs[0].Routable() {
		t.Fatalf("FQS taxonomy = %+v, want workstream fqs with the one routable leg federated-query", tx)
	}
}

// TestParse_RejectsMalformedFiles: each check the engine relies on refuses
// the valid file with one mutation.
func TestParse_RejectsMalformedFiles(t *testing.T) {
	valid, err := json.Marshal(PA())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(valid); err != nil {
		t.Fatalf("a re-encoded valid taxonomy is refused: %v", err)
	}
	for _, c := range []struct{ name, body, want string }{
		{"unknown field", strings.Replace(string(valid), `"scope":`, `"scoep":"x","scope":`, 1), "unknown field"},
		{"trailing data", string(valid) + `}`, "trailing data"},
		{"repeated operation", strings.Replace(string(valid), `"operation":"pas-inquire"`, `"operation":"pas-submit"`, 1), `operation "pas-submit" is empty or repeated`},
		{"repeated transaction type", strings.Replace(string(valid), `"transactionType":"pas-claim-update"`, `"transactionType":"pas-claim"`, 1), `transactionType "pas-claim" is empty or repeated`},
		{"empty scope on a routable leg", strings.Replace(string(valid), `"scope":"pas-bundle"`, `"scope":""`, 1), "empty scope"},
		{"empty frame on a routable leg", strings.Replace(string(valid), `"frame":"patient-authorship"`, `"frame":""`, 1), "a side has no frame"},
		{"no legs", `{"workstream":"pa","taxonomyVersion":1,"legs":[]}`, "no legs"},
		{"empty transaction type", strings.Replace(string(valid), `"transactionType":"pas-claim"`, `"transactionType":""`, 1), `transactionType "" is empty or repeated`},
		{"empty operation", strings.Replace(string(valid), `"operation":"pas-submit"`, `"operation":""`, 1), `operation "" is empty or repeated`},
		{"request-response leg without a response", strings.Replace(string(valid), `"response":{"frame":"payer-coverage","roles":["payer"],"operation":"pas-response"}`, `"response":null`, 1), "needs a response side"},
		{"routable leg with no answer envelope", strings.Replace(string(valid), `,"answerEnvelope":"fhir"`, ``, 1), `answerEnvelope "" is neither "fhir" nor "json"`},
		{"answer envelope outside the closed set", strings.Replace(string(valid), `"answerEnvelope":"json"`, `"answerEnvelope":"xml"`, 1), `answerEnvelope "xml" is neither "fhir" nor "json"`},
		{"subject outside the closed set", strings.Replace(string(valid), `"answerEnvelope":"fhir"`, `"answerEnvelope":"fhir","subject":"from-request"`, 1), `subject "from-request": only "from-answer" and "none" are defined`},
	} {
		if _, err := Parse([]byte(c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Parse = %v, want an error containing %q", c.name, err, c.want)
		}
	}
}

// TestParse_RefusesASubjectFlagOffARequestResponseLeg: each flag is valid
// only on a request-response leg with no consent, spelled exactly.
func TestParse_RefusesASubjectFlagOffARequestResponseLeg(t *testing.T) {
	consent := &Consent{Purpose: "treatment", CustodianFrom: "request"}
	for name, mutate := range map[string]func(*Leg){
		"from-answer in another case":       func(l *Leg) { l.Subject = "From-Answer" },
		"from-answer on another physics":    func(l *Leg) { l.Subject = SubjectFromAnswer; l.Physics = "direct-read" },
		"from-answer on an unknown physics": func(l *Leg) { l.Subject = SubjectFromAnswer; l.Physics = "" },
		"from-answer with consent":          func(l *Leg) { l.Subject = SubjectFromAnswer; l.Consent = consent },
		"none in another case":              func(l *Leg) { l.Subject = "None" },
		"none on another physics":           func(l *Leg) { l.Subject = SubjectNone; l.Physics = "direct-read" },
		"none on an unknown physics":        func(l *Leg) { l.Subject = SubjectNone; l.Physics = "" },
		"none with consent":                 func(l *Leg) { l.Subject = SubjectNone; l.Consent = consent },
		"a value outside the closed set":    func(l *Leg) { l.Subject = "nobody" },
	} {
		tx := PA()
		mutate(&tx.Legs[0])
		b, err := json.Marshal(tx)
		if err != nil {
			t.Fatal(err)
		}
		want := `subject "` + tx.Legs[0].Subject + `": only "from-answer" and "none" are defined, and only on a request-response leg with no consent`
		if _, err := Parse(b); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: Parse = %v, want an error containing %q", name, err, want)
		}
	}
}

// TestParse_ReadsASubjectFromTheAnswer: a leg flagged subject from-answer
// parses, and only that leg reports it.
func TestParse_ReadsASubjectFromTheAnswer(t *testing.T) {
	valid, err := json.Marshal(PA())
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(valid), `"answerEnvelope":"fhir"`, `"answerEnvelope":"fhir","subject":"from-answer"`, 1)
	tx, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("a leg flagged from-answer is refused: %v", err)
	}
	flaggedIn := func(tx Taxonomy) []string {
		var out []string
		for _, l := range tx.Legs {
			if l.FromAnswer() {
				out = append(out, l.TransactionType)
			}
		}
		return out
	}
	// The shipped taxonomy flags the payer's read of its Task alone; the
	// body above flags one leg more.
	if got := flaggedIn(PA()); len(got) != 1 || got[0] != "cdex-task-status" {
		t.Fatalf("the shipped taxonomy flags %v, want [cdex-task-status]", got)
	}
	if got := flaggedIn(tx); len(got) != 2 {
		t.Fatalf("%v report a subject from the answer, want the shipped leg and the 1 flagged", got)
	}
}

// TestParse_ReadsASubjectOfNone: a leg flagged subject none parses, and only
// that leg reports it; it does not read as a leg whose patient comes from the
// answer. The shipped taxonomy flags the payer's subscription to its CDex
// Tasks' changes and the provider's notification of them alone (D132).
func TestParse_ReadsASubjectOfNone(t *testing.T) {
	var shipped []string
	for _, l := range PA().Legs {
		if l.NamesNoPatient() {
			shipped = append(shipped, l.TransactionType)
		}
	}
	if !slices.Equal(shipped, []string{"cdex-task-subscribe", "cdex-task-signal"}) {
		t.Fatalf("the shipped taxonomy flags %v subject none, want [cdex-task-subscribe cdex-task-signal]", shipped)
	}
	valid, err := json.Marshal(PA())
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(valid), `"answerEnvelope":"fhir"`, `"answerEnvelope":"fhir","subject":"none"`, 1)
	tx, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("a leg flagged none is refused: %v", err)
	}
	var flagged []string
	for _, l := range tx.Legs {
		if l.NamesNoPatient() {
			if l.FromAnswer() {
				t.Fatalf("%s reads as both none and from-answer", l.TransactionType)
			}
			flagged = append(flagged, l.TransactionType)
		}
	}
	if len(flagged) != len(shipped)+1 {
		t.Fatalf("%v report a subject of none, want the shipped legs and the 1 flagged", flagged)
	}
}
