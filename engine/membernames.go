package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode"
	"unicode/utf8"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
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
// exactly or under case folding. The strict reader decides for a body it can
// read. A body it refuses for its own limits (not well-formed UTF-8 or a lone
// surrogate escape, or past its depth or token bound) is walked token by token
// as encoding/json reads it, so a repeat is found wherever that reader would
// read the body.
func repeatsAName(body []byte) bool {
	err := scanMessage(body)
	if err == nil || errors.Is(err, relay.ErrDuplicateKey) {
		return err != nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	type object struct {
		names map[string]bool
		// name is set when the next token is a member name or the object's end.
		name bool
	}
	var open []*object // nil for an array
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if n := len(open); n > 0 && open[n-1] != nil && open[n-1].name {
			in := open[n-1]
			if tok != json.Delim('}') {
				s, _ := tok.(string)
				k := foldedName(s)
				if in.names[k] {
					return true
				}
				in.names[k], in.name = true, false
				continue
			}
			open = open[:n-1]
		} else {
			switch tok {
			case json.Delim('{'):
				open = append(open, &object{names: map[string]bool{}, name: true})
				continue
			case json.Delim('['):
				open = append(open, nil)
				continue
			case json.Delim(']'):
				open = open[:len(open)-1]
			}
		}
		// A value has ended: the object holding it reads a name next.
		if n := len(open); n > 0 && open[n-1] != nil {
			open[n-1].name = true
		}
	}
}

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
// folds names: each rune becomes the smallest rune of its simple
// case-folding orbit.
func foldedName(name string) string {
	out := make([]byte, 0, len(name))
	for _, r := range name {
		for {
			f := unicode.SimpleFold(r)
			if f <= r {
				r = f
				break
			}
			r = f
		}
		out = utf8.AppendRune(out, r)
	}
	return string(out)
}
