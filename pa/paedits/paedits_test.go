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

// The E-16 row is a header edit of the payer system's answer, as data: it
// sets Location, keeps the ETag, and names the inputs the core's check of it
// reads. The check itself is the core's (engine/relay).
func TestE16RowIsAHeaderEdit(t *testing.T) {
	row, ok := rowOf(relay.EditCDexTaskLocationRekey)
	if !ok {
		t.Fatal("no E-16 row")
	}
	if row.Name != "cdex-task-location-rekey" || row.Kind != relay.EditKindSetHeader || row.Header != "Location" ||
		row.Role != relay.RoleRequester || row.Direction != relay.DirectionResponse ||
		!slices.Equal(row.Legs, []string{"cdex-attachment-request"}) || len(row.DropsAnswerHeaders) != 0 ||
		!slices.Equal(row.HeaderInputs, []string{relay.TaskLocationInputBase, relay.TaskLocationInputTail}) {
		t.Fatalf("E-16 row %+v", row)
	}
	if by, ok := relay.EditReceivedBy(Rows(), relay.EditCDexTaskLocationRekey); !ok || by != 0 {
		t.Fatalf("E-16 is received by %v %v: it is made on the answer to the payer's own system, which no other party receives", by, ok)
	}
}

// rowOf is the row of Rows with the given id.
func rowOf(id relay.EditID) (relay.Edit, bool) {
	for _, e := range Rows() {
		if e.ID == id {
			return e, true
		}
	}
	return relay.Edit{}, false
}
