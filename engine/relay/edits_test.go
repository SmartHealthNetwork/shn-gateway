package relay

import (
	"strings"
	"testing"
)

// editRows is one edit per closed id, the rows a registry may hold.
func editRows() []Edit {
	var rows []Edit
	for _, id := range EditIDs() {
		rows = append(rows, Edit{ID: id, Name: "name-" + string(id), Role: RoleRequester, Direction: DirectionRequest})
	}
	return rows
}

// The closed set, once each, is a registry; an id outside the closed set, or
// one two edits declare, is refused.
func TestValidateEditsRows(t *testing.T) {
	if err := ValidateEdits(editRows()); err != nil {
		t.Fatalf("the closed set: %v", err)
	}
	if err := ValidateEdits(nil); err != nil {
		t.Fatalf("no edits: %v", err)
	}
	for name, c := range map[string]struct {
		rows []Edit
		want string
	}{
		"an id outside the closed set": {append(editRows(), Edit{ID: "E-99"}), "E-99 is not in the relay package's closed set"},
		"an id two edits declare":      {append(editRows(), Edit{ID: EditCDSCallbackStrip}), "E-01 is declared twice"},
	} {
		if err := ValidateEdits(c.rows); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, c.want)
		}
	}
}

// The lookups read the rows they are given: a held id has its name and its
// receiving party; an id the rows do not hold has neither.
func TestEditLookupsReadTheRows(t *testing.T) {
	rows := []Edit{
		{ID: EditCDSCallbackStrip, Name: "strip", Role: RoleRequester, Direction: DirectionRequest},
		{ID: EditPayorEdgeRestamp, Name: "restamp", Role: RoleRecipient, Direction: DirectionRequest},
		{ID: EditDTRAnswerLineNames, Name: "names", Role: RoleRecipient, Direction: DirectionResponse},
	}
	for _, c := range []struct {
		id   EditID
		name string
		by   Role
		ok   bool
	}{
		{EditCDSCallbackStrip, "strip", RoleRecipient, true},
		{EditPayorEdgeRestamp, "restamp", 0, true},
		{EditDTRAnswerLineNames, "names", RoleRequester, true},
		{EditCDSPrefetchObtain, "", 0, false},
	} {
		if name, ok := EditName(rows, c.id); name != c.name || ok != c.ok {
			t.Errorf("EditName(%s) = %q %v, want %q %v", c.id, name, ok, c.name, c.ok)
		}
		if by, ok := EditReceivedBy(rows, c.id); by != c.by || ok != c.ok {
			t.Errorf("EditReceivedBy(%s) = %v %v, want %v %v", c.id, by, ok, c.by, c.ok)
		}
	}
	if _, ok := EditName(nil, EditCDSCallbackStrip); ok {
		t.Error("no rows named an edit")
	}
}
