package engine

import (
	"errors"
	"strings"
)

// errMemberNamesNoOne is a read for a member id that names no one: blank, or a
// Patient reference's type with a blank id ("Patient/"). Unknown is not empty:
// such a member is never read from the system of record, where an identifier
// search with an empty value matches any member of the system, and never
// bound to an identifier derived from it, which every such request would
// share. The request is refused on authority (the patient the exchange is for
// cannot be identified), as the caller's input, never as the system of
// record's failure.
var errMemberNamesNoOne = errors.New(msgMemberNamesNoOne)

const msgMemberNamesNoOne = "cannot identify the patient: the request names its patient by a reference that names no one"

// notProvablyThePatient reports whether a binding error leaves the patient
// unidentified (errPatientUnidentifiable, errMemberNamesNoOne): a reference
// that binds so is never the leg's patient, nor any other.
func notProvablyThePatient(err error) bool {
	return errors.Is(err, errPatientUnidentifiable) || errors.Is(err, errMemberNamesNoOne)
}

// memberNamesNoOne reports whether member names no member. Every
// member-keyed read of a system of record (recordingSoR) and every binding
// (resolveBindingFrom) refuses such a member before anything is read or
// derived from it, whatever the connector: a participant's own connector may
// search by identifier as the built-in FHIR one does.
func memberNamesNoOne(member string) bool {
	id, _ := strings.CutPrefix(member, "Patient/")
	return strings.TrimSpace(id) == ""
}
