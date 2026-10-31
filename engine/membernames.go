package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
)

// errNamedInAnotherCase is a request that names its type, its patient, the
// patient's reference or, for a "#id" reference, its contained resources only
// in another case, found past the strict reader's limits
// (relay.ErrFieldNameCase is that reader's finding of the same).
// Member names are case-sensitive, so the request names no patient.
var errNamedInAnotherCase = errors.New("a member is named only in another case")

// readsTwoWays reports whether err is the strict reader finding a body that
// can be read two ways: a repeated member name, or a member named only in
// another case.
func readsTwoWays(err error) bool {
	return errors.Is(err, relay.ErrDuplicateKey) || errors.Is(err, relay.ErrFieldNameCase)
}

// repeatsAName reports whether body repeats a member name in some object,
// exactly or under case folding, wherever encoding/json would read it
// (jsonmember.RepeatsAName).
func repeatsAName(body []byte) bool { return jsonmember.RepeatsAName(body) }

// resourceRepeatsAName reports whether body repeats a member name in a way a reader may
// read two ways: exactly, in any object, or under case folding, in a FHIR resource (an
// object with a resourceType member, in any case, and every object within it). A vendor's
// own members outside any resource, such as in a CDS Hooks request's extension, are no
// FHIR names: spelling one two ways there is not read as a repeat. A body encoding/json
// cannot read to its end is reported as repeating, so it is never read.
func resourceRepeatsAName(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	type object struct {
		names    map[string]bool // folded names
		exact    map[string]bool
		name     bool // the next token is a member name or the object's end
		repeats  bool // a name repeated under case folding
		resource bool
		parent   int // the object holding this one, -1 for none
	}
	var objects []*object
	var open []int // indices into objects; -1 for an array
	resourceType := foldedName("resourceType")
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if len(open) > 0 {
				return true
			}
			break
		}
		if err != nil {
			return true
		}
		if n := len(open); n > 0 && open[n-1] >= 0 && objects[open[n-1]].name {
			in := objects[open[n-1]]
			if tok != json.Delim('}') {
				s, _ := tok.(string)
				if in.exact[s] {
					return true
				}
				k := foldedName(s)
				in.repeats = in.repeats || in.names[k]
				in.resource = in.resource || k == resourceType
				in.names[k], in.exact[s], in.name = true, true, false
				continue
			}
			open = open[:n-1]
		} else {
			switch tok {
			case json.Delim('{'):
				parent := -1
				for i := len(open) - 1; i >= 0; i-- {
					if open[i] >= 0 {
						parent = open[i]
						break
					}
				}
				objects = append(objects, &object{names: map[string]bool{}, exact: map[string]bool{}, name: true, parent: parent})
				open = append(open, len(objects)-1)
				continue
			case json.Delim('['):
				open = append(open, -1)
				continue
			case json.Delim(']'):
				open = open[:len(open)-1]
			}
		}
		if n := len(open); n > 0 && open[n-1] >= 0 {
			objects[open[n-1]].name = true
		}
	}
	for _, o := range objects {
		if !o.repeats {
			continue
		}
		for at := o; ; at = objects[at.parent] {
			if at.resource {
				return true
			}
			if at.parent < 0 {
				break
			}
		}
	}
	return false
}

// foldedName maps name to a canonical form such that foldedName(a) ==
// foldedName(b) exactly when strings.EqualFold(a, b), as the strict reader
// folds names (jsonmember.FoldedName).
func foldedName(name string) string { return jsonmember.FoldedName(name) }
