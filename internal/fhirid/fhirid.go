// Package fhirid is the gateway's one rule for a FHIR id it writes into a
// request's path or search: the id of a resource it reads, a Patient id its
// participant's system of record answered with, or one a reference names.
package fhirid

import "regexp"

// shape is FHIR's id type: 1 to 64 of [A-Za-z0-9-.].
var shape = regexp.MustCompile(`^[A-Za-z0-9\-.]{1,64}$`)

// Valid reports whether id is a FHIR id the gateway may write into a path or a
// search: FHIR's id type, and neither "." nor "..". Those two are a FHIR id's
// characters, but a dot segment in a path: a server or proxy that cleans the
// path reads one as a step in it, and could answer for another resource, or a
// history, in place of the one named.
func Valid(id string) bool { return shape.MatchString(id) && id != "." && id != ".." }
