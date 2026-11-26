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

// errPatientCarriesNoMember is errMemberNamesNoOne (errors.Is) for a request whose
// patient reference resolves, by structure, to one Patient the request carries that
// holds no member key: a contained Patient with no member identifier, or two
// different ones (a contained resource's id is local to the resource that carries
// it, never a member id), or a Bundle entry with neither an id nor a member
// identifier. Its patient names no one: never read and never derived from, which
// would bind every such request to one identifier. Refused and recorded as
// errMemberNamesNoOne is, with a text that says what the request must carry.
var errPatientCarriesNoMember error = patientCarriesNoMember{}

type patientCarriesNoMember struct{}

func (patientCarriesNoMember) Error() string { return msgPatientCarriesNoMember }
func (patientCarriesNoMember) Unwrap() error { return errMemberNamesNoOne }

const msgPatientCarriesNoMember = "cannot identify the patient: the request names its patient by a reference to a Patient it carries that holds no member identifier; name the patient by Patient/<id>, or give that Patient one member identifier"

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
