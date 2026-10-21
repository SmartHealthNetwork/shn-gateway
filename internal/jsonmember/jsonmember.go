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
