package paedits

import (
	"slices"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// The rows are a registry (relay.ValidateEdits) holding every closed id, in
// id order, and each call returns rows of its own: a caller that changes one
// changes no other caller's.
func TestRowsAreTheClosedRegistry(t *testing.T) {
	rows := Rows()
	if err := relay.ValidateEdits(rows); err != nil {
		t.Fatal(err)
	}
	ids := make([]relay.EditID, len(rows))
	for i, e := range rows {
		ids[i] = e.ID
	}
	if !slices.Equal(ids, relay.EditIDs()) {
		t.Fatalf("rows %v, want the closed set %v", ids, relay.EditIDs())
	}
	rows[0].Name = "changed"
	rows[0].Legs[0] = "changed"
	if again := Rows(); again[0].Name == "changed" || again[0].Legs[0] == "changed" {
		t.Fatal("a caller's change reached another caller's rows")
	}
}
