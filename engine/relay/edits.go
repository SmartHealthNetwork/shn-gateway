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
	// EditKindSetHeader sets one named header on the answer the gateway writes
	// to its own participant's system, and changes no byte of the body: the
	// row names the header (Edit.Header) and the inputs its value is made from
	// (Edit.HeaderInputs), the core checks the value by the edit's id
	// (headerVerifiers), and the edit is made only by HeaderEdited.
	EditKindSetHeader EditKind = "set-header"
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
	// Header is the one header an EditKindSetHeader edit sets, as it is
	// written: one of the closed list of headers a header edit may set
	// (EditableHeaders). Empty for every other kind.
	Header string
	// HeaderInputs name the facts an EditKindSetHeader edit's value is made
	// from (HeaderInputs keys), the ones the core's check of that edit reads
	// (headerVerifiers): HeaderEdited takes exactly these. Empty for every
	// other kind. A row is data: the check of the value is the core's, by the
	// edit's id, never the row's.
	HeaderInputs []string
}

// HeaderInputs are the facts a header edit's value is made from, by the
// names its row declares (Edit.HeaderInputs).
type HeaderInputs map[string]string

// editableHeaders are the headers of an answer a header edit may set: a
// closed list, so no row can set a header that describes the body or the
// exchange (Content-Type, Content-Length, ETag, X-SHN-Edited). A new one is
// a reviewed change here.
var editableHeaders = []string{"Location"}

// EditableHeaders returns the headers a header edit may set, in order.
func EditableHeaders() []string { return slices.Clone(editableHeaders) }

// hasVerifier reports whether the core holds a check of header edit id.
func hasVerifier(id EditID) bool {
	v, ok := headerVerifiers[id]
	return ok && v.verify != nil
}

// ETagHeader is the header an edit of an answer always drops.
const ETagHeader = "ETag"

// ValidateEdits reports why rows cannot be a registry of edits, or nil: an
// edit whose id is not in the closed set (EditIDs), an id two edits declare,
// an edit of an answer's body that leaves its ETag standing, or a header edit
// (EditKindSetHeader) whose id the core does not check, or that is not one
// listed header of an answer with the core's inputs and no path, or that
// drops a header: it changes no byte of the body, so the
// ETag still names the bytes the caller receives, and is kept. A row of any
// other kind names no header and no verifier. It reads rows and changes
// nothing.
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
		if (e.Kind == EditKindSetHeader) != slices.Contains(headerEdits, e.ID) {
			return fmt.Errorf("edit %s: a header edit's row, and only one, is of kind %s", e.ID, EditKindSetHeader)
		}
		if e.Kind == EditKindSetHeader {
			if err := validateHeaderEdit(e); err != nil {
				return err
			}
			continue
		}
		if e.Header != "" || len(e.HeaderInputs) != 0 {
			return fmt.Errorf("edit %s is not a header edit and names a header or header inputs", e.ID)
		}
		if e.Direction == DirectionResponse && !slices.Contains(e.DropsAnswerHeaders, ETagHeader) {
			return fmt.Errorf("edit %s changes an answer and does not drop its %s", e.ID, ETagHeader)
		}
	}
	return nil
}

// validateHeaderEdit reports why e cannot be an EditKindSetHeader row: its id
// is a header edit the core checks (headerVerifiers), and it sets one header a
// header edit may set (editableHeaders), spelled as listed, on an answer,
// names the inputs the core's check reads, at no JSON path, and drops no
// header.
func validateHeaderEdit(e Edit) error {
	switch {
	case e.Kind != EditKindSetHeader || !slices.Contains(headerEdits, e.ID):
		return fmt.Errorf("edit %s is not a header edit", e.ID)
	case e.Direction != DirectionResponse:
		return fmt.Errorf("header edit %s is not made on an answer", e.ID)
	case !slices.Contains(editableHeaders, e.Header):
		return fmt.Errorf("header edit %s sets %q, not a header a header edit may set %v", e.ID, e.Header, editableHeaders)
	case !hasVerifier(e.ID):
		return fmt.Errorf("header edit %s has no check in the core (headerVerifiers)", e.ID)
	case !slices.Equal(e.HeaderInputs, headerVerifiers[e.ID].names()):
		return fmt.Errorf("header edit %s names the inputs %v, not %v, the ones the core's check of it reads", e.ID, e.HeaderInputs, headerVerifiers[e.ID].names())
	case len(e.Paths) != 0:
		return fmt.Errorf("header edit %s names a JSON path: it changes no byte of the body", e.ID)
	case len(e.DropsAnswerHeaders) != 0:
		return fmt.Errorf("header edit %s drops %v: it changes no byte of the body, so the answer's headers still describe it", e.ID, e.DropsAnswerHeaders)
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
// own participant's system (the adaptive address restore, E-13, on the
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
