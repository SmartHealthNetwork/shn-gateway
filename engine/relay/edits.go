package relay

import (
	"fmt"
	"slices"
)

// EditKind is the one kind of change a registered edit makes.
type EditKind string

const (
	EditKindRemoveMember EditKind = "remove-member"
	EditKindInsertMember EditKind = "insert-member"
	EditKindReplaceValue EditKind = "replace-value"
	EditKindArrayAppend  EditKind = "array-append"
	// EditKindRegisteredTransform rewrites the whole body of the gateway's own
	// message through the registered compatibility steps, verified by running
	// them again (Transformed), never by spans.
	EditKindRegisteredTransform EditKind = "registered-transform"
)

// Edit declares one registered edit: the only changes a gateway may make to
// a participant's message it relays, and the one transform it makes to a
// message it built itself to carry it to the peer's IG line (E-09). Anything
// not declared is not an edit and is never made. A workstream declares the
// edits on its own legs; the gateway gathers every compiled-in workstream's
// edits into its registry (ValidateEdits).
type Edit struct {
	ID   EditID
	Name string
	// Legs, Role and Direction name the transmit the edit applies at.
	Legs      []string
	Role      Role
	Direction Direction
	// Paths are the exact JSON locations the edit may change.
	Paths []string
	Kind  EditKind
	// Precondition says when the edit is made; Authority says where the
	// value it writes comes from.
	Precondition string
	Authority    string
	// Disclosure is the statement participants are given about the edit.
	Disclosure string
	// DropsAnswerHeaders are the headers of the participant's answer the edit
	// makes untrue, left out as part of it and stated in its Disclosure. An
	// edit of an answer names ETagHeader: the bytes the caller receives are not
	// the ones that validator named.
	DropsAnswerHeaders []string
}

// ETagHeader is the header an edit of an answer always drops.
const ETagHeader = "ETag"

// ValidateEdits reports why rows cannot be a registry of edits, or nil: an
// edit whose id is not in the closed set (EditIDs), an id two edits declare,
// or an edit of an answer that leaves its ETag standing. It reads rows and
// changes nothing.
func ValidateEdits(rows []Edit) error {
	closed := map[EditID]bool{}
	for _, id := range editIDs {
		closed[id] = true
	}
	seen := map[EditID]bool{}
	for _, e := range rows {
		if !closed[e.ID] {
			return fmt.Errorf("edit %s is not in the relay package's closed set", e.ID)
		}
		if seen[e.ID] {
			return fmt.Errorf("edit %s is declared twice", e.ID)
		}
		seen[e.ID] = true
		if e.Direction == DirectionResponse && !slices.Contains(e.DropsAnswerHeaders, ETagHeader) {
			return fmt.Errorf("edit %s changes an answer and does not drop its %s", e.ID, ETagHeader)
		}
	}
	return nil
}

// EditDropsAnswerHeaders returns the answer headers the edit of rows with the
// given id drops.
func EditDropsAnswerHeaders(rows []Edit, id EditID) []string {
	e, _ := editIn(rows, id)
	return e.DropsAnswerHeaders
}

// editIn returns the edit of rows with the given id.
func editIn(rows []Edit, id EditID) (Edit, bool) {
	for _, e := range rows {
		if e.ID == id {
			return e, true
		}
	}
	return Edit{}, false
}

// EditName returns the name rows register for the edit id names (E-01 is
// "cds-callback-strip"), and whether rows hold it. Staff views show an edit
// by its id and this name only.
func EditName(rows []Edit, id EditID) (string, bool) {
	e, ok := editIn(rows, id)
	return e.Name, ok
}

// EditReceivedBy names the party of a leg whose received bytes the edit id
// names changes, by the edit rows register, and whether rows hold it. An
// edit a gateway makes to what it sends into the network reaches the leg's
// other party: a requester's request edit (E-01, E-02, E-04 … E-07) is
// received by the recipient. An edit a gateway makes to what it sends its
// own participant's system (the payer identity mapping, E-03, on the
// recipient's forward) is received by no other party: the role returned is 0.
func EditReceivedBy(rows []Edit, id EditID) (Role, bool) {
	e, ok := editIn(rows, id)
	if !ok {
		return 0, false
	}
	switch {
	case e.Role == RoleRequester && e.Direction == DirectionRequest:
		return RoleRecipient, true
	case e.Role == RoleRecipient && e.Direction == DirectionResponse:
		return RoleRequester, true
	}
	return 0, true
}
