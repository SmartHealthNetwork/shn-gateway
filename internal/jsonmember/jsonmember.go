// Package jsonmember names a member name a JSON body repeats, as a refusal
// states it so that the sender can find and fix it. It reads a body through
// the relay's strict decoder (relay.Decode), so it finds exactly the repeats
// that decoder refuses: a member name repeated exactly, or equal to another
// but for case.
package jsonmember

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// Repeated names the first member name body repeats and the object
// that repeats it, as a refusal states it so that the sender can find and fix
// it: `the JSON member "status" in entry[2].resource`. ok is false when body
// repeats no member name.
func Repeated(body []byte) (string, bool) {
	var whole json.RawMessage
	var dk *relay.DuplicateKeyError
	if !errors.As(relay.Decode(relay.NewBody(body, relay.OriginPeerFrame), &whole), &dk) {
		return "", false
	}
	member := fmt.Sprintf("the JSON member %q", capName(dk.Key))
	// A location this cannot read is not guessed: the member alone is named.
	p, ok := memberObjectPath(body, dk.Offset)
	switch {
	case !ok:
		return member, true
	case p == "":
		return member + " in the top-level object", true
	}
	return member + " in " + p, true
}

// maxNameLen bounds each member name a refusal echoes: the names are the
// sender's, and a refusal names, it does not copy.
const maxNameLen = 64

// capName is a member name as a refusal echoes it: at most maxNameLen runes.
func capName(name string) string {
	if r := []rune(name); len(r) > maxNameLen {
		return string(r[:maxNameLen]) + "…"
	}
	return name
}

// plainName is a member name an element path shows bare.
var plainName = regexp.MustCompile(`^[A-Za-z_-][A-Za-z0-9_-]*$`)

// pathSegment is one member of an element path: `.name` for a plain name,
// `["…"]` for any other, so a name holding a dot or a bracket reads as one
// member, not as nesting.
func pathSegment(path, name string) string {
	plain := plainName.MatchString(name)
	switch {
	case plain && path == "":
		return capName(name)
	case plain:
		return path + "." + capName(name)
	}
	return fmt.Sprintf("%s[%q]", path, capName(name))
}

// memberObjectPath is the path, from the top-level value, of the object
// holding the member name that starts at offset: "entry[2].resource", or ""
// for the top-level object. ok is false when no member name starts there.
func memberObjectPath(body []byte, offset int) (string, bool) {
	type frame struct {
		obj, wantKey bool
		path, key    string
		idx          int
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	// A number beyond float64 is still JSON: read it as text.
	dec.UseNumber()
	var stack []frame
	for {
		before := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			stack = stack[:len(stack)-1]
			continue
		}
		var top *frame
		if len(stack) > 0 {
			top = &stack[len(stack)-1]
		}
		if top != nil && top.obj && top.wantKey {
			key, _ := tok.(string)
			if int64(offset) >= before && int64(offset) < dec.InputOffset() {
				return top.path, true
			}
			top.key, top.wantKey = key, false
			continue
		}
		// The path of the value this token starts.
		path := ""
		if top != nil {
			if top.obj {
				path, top.wantKey = pathSegment(top.path, top.key), true
			} else {
				path = fmt.Sprintf("%s[%d]", top.path, top.idx)
				top.idx++
			}
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, frame{obj: true, wantKey: true, path: path})
		case json.Delim('['):
			stack = append(stack, frame{path: path})
		}
	}
}

// MaxDepth is the deepest nesting of objects and arrays RepeatsAName walks:
// encoding/json's own bound, past which that decoder reads no body. The walk
// stops where a body goes deeper: a repeat it met before that point is
// reported; anything at or after it is not, and the body, which no decoder
// reads, is refused by each caller as any body it cannot read.
const MaxDepth = 10000

// RepeatsAName reports whether body repeats a member name in some object,
// exactly or under case folding. The relay's strict reader decides for a body
// it can read. A body it refuses for its own limits (not well-formed UTF-8 or
// a lone surrogate escape, or past its depth or token bound) is walked token by
// token as encoding/json reads it, so a repeat is found wherever that reader
// would read the body, and nowhere past MaxDepth, where it reads nothing (the
// walk holds one level per open object or array, so the bound is its memory's
// too).
func RepeatsAName(body []byte) bool {
	var whole json.RawMessage
	err := relay.Decode(relay.NewBody(body, relay.OriginPeerFrame), &whole)
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
				k := FoldedName(s)
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
				if len(open) == MaxDepth {
					return false
				}
				open = append(open, &object{names: map[string]bool{}, name: true})
				continue
			case json.Delim('['):
				if len(open) == MaxDepth {
					return false
				}
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

// FoldedName maps name to a canonical form such that FoldedName(a) ==
// FoldedName(b) exactly when strings.EqualFold(a, b), as the relay's strict
// reader folds names: each rune becomes the smallest rune of its simple
// case-folding orbit.
func FoldedName(name string) string {
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
