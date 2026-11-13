//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Splicing entries into an encoded PAS request Bundle: the request's own
// bytes, a participant's record carried as written among them, stay exactly as
// they were. The PA workstream's request-graph completion and module 1's
// requester carry both add entries this way.

// errRecordNotCarried is the fidelity of the gateway's own splice: a record
// read from the participant's system that the splice cannot carry as read. It
// is refused (422, at the build stage), as a record the SDK's Carried builders
// cannot carry is (shnsdk.ErrPASRecordNotCarried), never answered as the
// gateway's own failure: the record is the participant's, and the gateway does
// not judge it, only declines to edit it into a request it cannot carry it in.
var errRecordNotCarried = errors.New("a record read from the system of record cannot be carried as read")

// bundleEntryBytes is one entry to add to an encoded Bundle: its fullUrl,
// already encoded as a JSON string, and its resource's bytes.
type bundleEntryBytes struct {
	fullURL  []byte
	resource []byte
}

// spliceBundleEntries adds entries at the end of the encoded Bundle body's
// entry array, which holds had entries: every byte already there, a
// participant's record carried as written among them, stays exactly as it
// was, and each resource is placed as given (surrounding whitespace aside).
// What was checked of a resource was its decoded text, so a resource whose
// bytes would not decode to exactly that text is refused, as the SDK's Carried
// builders refuse such a record: one that is not exactly one JSON object, one
// that is not UTF-8, one holding a lone surrogate escape (decoded as U+FFFD),
// or one naming a member twice in an object, whatever its case under Unicode
// case folding (readers that keep the first, the last, or match names ignoring
// case, as encoding/json does, read different records). The body must hold one
// top-level entry array of exactly had entries. A record refused is
// errRecordNotCarried; a body not so is the gateway's own failure.
func spliceBundleEntries(body []byte, had int, entries []bundleEntryBytes) ([]byte, error) {
	var added []byte
	for _, e := range entries {
		// Only JSON's own white space (RFC 8259) surrounds the record.
		r := bytes.Trim(e.resource, " \t\r\n")
		if why := uncarriableRecordBytes(r); why != "" {
			return nil, fmt.Errorf("%w: it %s", errRecordNotCarried, why)
		}
		added = append(added, ',')
		added = append(added, `{"fullUrl":`...)
		added = append(added, e.fullURL...)
		added = append(added, `,"resource":`...)
		added = append(added, r...)
		added = append(added, '}')
	}
	if len(added) == 0 {
		return body, nil
	}
	start, end, err := topLevelValueSpan(body, "entry")
	if err != nil {
		return nil, err
	}
	var present []json.RawMessage
	if err := json.Unmarshal(body[start:end], &present); err != nil || len(present) != had {
		return nil, fmt.Errorf("the request's entries are not the %d entries it was read with", had)
	}
	if had == 0 {
		added = added[1:]
	}
	closeAt := bytes.LastIndexByte(body[start:end], ']')
	if closeAt < 0 {
		return nil, fmt.Errorf("the request's entries are not an array")
	}
	closeAt += start
	return slices.Concat(body[:closeAt], added, body[closeAt:]), nil
}

// uncarriableRecordBytes is why r, a record's bytes, cannot be carried as
// read (spliceBundleEntries), or "" when it can: it is not UTF-8, holds a lone
// surrogate escape, is not exactly one JSON object, or names a member twice in
// an object, whatever its case under Unicode case folding.
func uncarriableRecordBytes(r []byte) string {
	switch {
	case !utf8.Valid(r):
		return "is not UTF-8"
	case loneSurrogateEscape(r):
		return "holds a lone surrogate escape"
	case len(r) == 0 || r[0] != '{' || !json.Valid(r):
		return "is not exactly one JSON object"
	case oneSpellingPerMember(r) != nil:
		return "names a member twice, or in another case"
	}
	return ""
}

// topLevelValueSpan returns the byte span [start, end) of member key's value
// in the JSON object doc, which must name key once, whatever its case.
func topLevelValueSpan(doc []byte, key string) (int, int, error) {
	found, start, end := false, 0, 0
	dec := json.NewDecoder(bytes.NewReader(doc))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return 0, 0, fmt.Errorf("the request is not a JSON object")
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return 0, 0, err
		}
		name, _ := t.(string)
		at := int(dec.InputOffset())
		for at < len(doc) && (doc[at] == ':' || doc[at] == ' ' || doc[at] == '\t' || doc[at] == '\n' || doc[at] == '\r') {
			at++
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return 0, 0, err
		}
		if strings.EqualFold(name, key) {
			if found || name != key {
				return 0, 0, fmt.Errorf("the request names %s more than once, or in another case", key)
			}
			found, start, end = true, at, at+len(v)
		}
	}
	if !found {
		return 0, 0, fmt.Errorf("the request has no %s", key)
	}
	return start, end, nil
}

// loneSurrogateEscape reports whether raw, a JSON document, holds a \u escape
// naming half of a UTF-16 surrogate pair without its other half.
func loneSurrogateEscape(raw []byte) bool {
	inString, pendingHigh := false, false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			inString = c == '"'
			continue
		}
		switch {
		case c == '"':
			if pendingHigh {
				return true
			}
			inString = false
		case c == '\\' && i+1 < len(raw):
			i++
			if raw[i] != 'u' {
				if pendingHigh {
					return true
				}
				continue
			}
			if i+4 >= len(raw) {
				return true
			}
			v, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
			if err != nil {
				return true
			}
			i += 4
			switch {
			case v >= 0xD800 && v <= 0xDBFF:
				if pendingHigh {
					return true
				}
				pendingHigh = true
			case v >= 0xDC00 && v <= 0xDFFF:
				if !pendingHigh {
					return true
				}
				pendingHigh = false
			default:
				if pendingHigh {
					return true
				}
			}
		default:
			if pendingHigh {
				return true
			}
		}
	}
	return pendingHigh
}

// oneSpellingPerMember refuses a JSON document any of whose objects names a
// member twice, whatever its case: names are compared by foldName, so two
// names strings.EqualFold matches ("subject" and "ſubject") are one.
func oneSpellingPerMember(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		object  bool
		wantKey bool
		names   map[string]bool
	}
	var stack []*frame
	for {
		t, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("is not JSON: %w", err)
		}
		top := (*frame)(nil)
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.wantKey {
			if d, ok := t.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				if n := len(stack); n > 0 && stack[n-1].object {
					stack[n-1].wantKey = true
				}
				continue
			}
			name := foldName(t.(string))
			if top.names[name] {
				return fmt.Errorf("names the member %q twice, or in another case", t.(string))
			}
			top.names[name] = true
			top.wantKey = false
			continue
		}
		switch d, _ := t.(json.Delim); d {
		case '{':
			stack = append(stack, &frame{object: true, wantKey: true, names: map[string]bool{}})
			continue
		case '[':
			stack = append(stack, &frame{})
			continue
		case ']':
			stack = stack[:len(stack)-1]
		}
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
}

// foldName maps name to a canonical form such that foldName(a) == foldName(b)
// exactly when strings.EqualFold(a, b): each rune becomes the smallest rune of
// its simple case-folding orbit, as the SDK's Carried builders compare names.
func foldName(name string) string {
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

// exactMembers reads a JSON object's members by their exact names: unlike a Go
// struct tag, which matches a member name regardless of case, "Id" is not "id".
func exactMembers(raw []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// exactString is m's member named exactly name, as a string; "" when there is
// none or it is not a string.
func exactString(m map[string]json.RawMessage, name string) string {
	var s string
	if v, ok := m[name]; !ok || json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}
