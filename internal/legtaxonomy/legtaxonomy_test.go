package legtaxonomy

import (
	"encoding/json"
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
	} {
		if _, err := Parse([]byte(c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Parse = %v, want an error containing %q", c.name, err, c.want)
		}
	}
}
