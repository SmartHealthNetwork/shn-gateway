//go:build shn_toyworkstream

package legtaxonomy

import _ "embed"

// toyFile is the toy test profile, byte-identical to the root's
// internal/legtaxonomy/testprofile/toy.json (the root package's hash fence),
// compiled in only under the shn_toyworkstream build tag.
//
//go:embed testprofile/toy.json
var toyFile []byte

// Toy returns a fresh copy of the toy test profile's taxonomy: one
// request-response leg that shares nothing with PA, so a test can run a second
// workstream through the network. A release build has no Toy.
func Toy() Taxonomy {
	t, err := Parse(toyFile)
	if err != nil {
		panic(err)
	}
	return t
}
