//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
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
// is refused at the build stage, never answered as the gateway's own failure:
// the record is the participant's, and the gateway does not judge it, only
// declines to edit it into a request it cannot carry it in. What cannot be
// carried is what the gateway read from its own participant's system, so the
// answer is 502 (buildFidelityStatus; gateway/STABILITY.md, "What the gateway
// cannot carry or edit"). One refusal errors.Is reports as it answers 422
// instead: a record the request names that the system does not hold
// (evidenceNotHeldError).
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
//
// raw must be valid JSON (json.Valid, which uncarriableRecordBytes checks
// first). It is read in one pass over its bytes, never a token at a time:
// each open object's member names are folded into one buffer the walk
// shares, and compared there, so a record near the relay's token bound costs
// one pass and no allocation per token. A name holding an escape or a byte
// past ASCII is decoded and folded by foldName; any other name is folded to
// its ASCII upper case, which is what foldName gives it (each ASCII letter's
// smallest case-folding partner is its upper case).
func oneSpellingPerMember(raw []byte) error {
	var w memberWalk
	return w.walk(raw)
}

// memberLinearMax is how many names an object holds before its names are
// also kept in a set: below it a linear comparison is cheaper than hashing.
const memberLinearMax = 16

// memberWalk is oneSpellingPerMember's state: the open containers, and the
// folded names of every open object, in the order they were read.
type memberWalk struct {
	folded []byte   // the open objects' folded names, back to back
	names  [][2]int // each name's span in folded
	// next chains a name to the previous name of its object with the same
	// hash (-1: none), for an object whose names are in a set.
	next  []int32
	stack []memberFrame
	// sets are the name sets of objects past memberLinearMax names, one per
	// nesting depth, emptied when their object closes and used again by the
	// next such object at that depth: a record of many wide objects does not
	// allocate a set for each.
	sets []map[uint64]int32
	seed maphash.Seed
	// hash is the name hash; nil is maphash under seed. A test sets a weak
	// one, so names collide and the chains are walked.
	hash func([]byte) uint64
}

// memberFrame is one open container. For an object, its names are
// names[first:] and its folded bytes start at folded[at]; past
// memberLinearMax names, each name's hash maps to the last of its names with
// that hash, in set.
type memberFrame struct {
	object, wantKey bool
	first, at       int
	set             map[uint64]int32
}

func (w *memberWalk) walk(raw []byte) error {
	if w.hash == nil {
		w.seed = maphash.MakeSeed()
		w.hash = func(b []byte) uint64 { return maphash.Bytes(w.seed, b) }
	}
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '{':
			w.stack = append(w.stack, memberFrame{object: true, wantKey: true, first: len(w.names), at: len(w.folded)})
		case '[':
			w.stack = append(w.stack, memberFrame{})
		case '}', ']':
			if len(w.stack) == 0 {
				return fmt.Errorf("is not JSON: unbalanced %q", raw[i])
			}
			top := w.stack[len(w.stack)-1]
			w.stack = w.stack[:len(w.stack)-1]
			if top.object {
				w.names, w.next, w.folded = w.names[:top.first], w.next[:top.first], w.folded[:top.at]
				if top.set != nil {
					clear(top.set)
				}
			}
		case ',':
			if n := len(w.stack); n > 0 && w.stack[n-1].object {
				w.stack[n-1].wantKey = true
			}
		case '"':
			end := jsonStringEnd(raw, i)
			if end < 0 {
				return fmt.Errorf("is not JSON: an unterminated string")
			}
			if n := len(w.stack); n > 0 && w.stack[n-1].object && w.stack[n-1].wantKey {
				w.stack[n-1].wantKey = false
				if err := w.admitName(n-1, raw[i:end+1]); err != nil {
					return err
				}
			}
			i = end
		}
	}
	return nil
}

// jsonStringEnd is the index of the quote that closes the JSON string
// opening at raw[open], or -1 when there is none.
func jsonStringEnd(raw []byte, open int) int {
	for i := open + 1; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// admitName records quoted, a member name as written (quotes included), as one
// of the names of the object open at stack[depth], or refuses it when that
// object already names it under case folding.
func (w *memberWalk) admitName(depth int, quoted []byte) error {
	f := &w.stack[depth]
	start := len(w.folded)
	if body := quoted[1 : len(quoted)-1]; plainASCIIName(body) {
		for _, c := range body {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			w.folded = append(w.folded, c)
		}
	} else {
		var name string
		if err := json.Unmarshal(quoted, &name); err != nil {
			return fmt.Errorf("is not JSON: %w", err)
		}
		w.folded = append(w.folded, foldName(name)...)
	}
	name := w.folded[start:]
	same := func(k int) bool { n := w.names[k]; return bytes.Equal(w.folded[n[0]:n[1]], name) }
	dup := false
	var h uint64
	if f.set != nil {
		h = w.hash(name)
		if k, ok := f.set[h]; ok {
			for ; k >= 0; k = w.next[k] {
				if same(int(k)) {
					dup = true
					break
				}
			}
		}
	} else {
		for k := f.first; k < len(w.names); k++ {
			if same(k) {
				dup = true
				break
			}
		}
	}
	if dup {
		var shown string
		_ = json.Unmarshal(quoted, &shown)
		return fmt.Errorf("names the member %q twice, or in another case", shown)
	}
	at := int32(len(w.names))
	w.names = append(w.names, [2]int{start, len(w.folded)})
	w.next = append(w.next, -1)
	switch {
	case f.set != nil:
		w.link(f.set, h, at)
	case int(at)+1-f.first > memberLinearMax:
		f.set = w.setAt(depth)
		for k := f.first; k <= int(at); k++ {
			n := w.names[k]
			w.link(f.set, w.hash(w.folded[n[0]:n[1]]), int32(k))
		}
	}
	return nil
}

// link adds name k, of hash h, to set.
func (w *memberWalk) link(set map[uint64]int32, h uint64, k int32) {
	if prev, ok := set[h]; ok {
		w.next[k] = prev
	} else {
		w.next[k] = -1
	}
	set[h] = k
}

// setAt is the empty name set kept for objects at nesting depth.
func (w *memberWalk) setAt(depth int) map[uint64]int32 {
	for len(w.sets) <= depth {
		w.sets = append(w.sets, nil)
	}
	if w.sets[depth] == nil {
		w.sets[depth] = make(map[uint64]int32, 4*memberLinearMax)
	}
	return w.sets[depth]
}

// plainASCIIName reports whether body, a member name's bytes between its
// quotes, is printable ASCII with no escape: its text is its bytes.
func plainASCIIName(body []byte) bool {
	for _, c := range body {
		if c < 0x20 || c > 0x7e || c == '\\' {
			return false
		}
	}
	return true
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
