package subscription

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
)

// maxDepth bounds how deeply a body may nest, so a hostile body costs no more
// than this to refuse.
const maxDepth = 128

// jnode is one JSON value in a body, with its byte span, so an edit can
// replace exactly that value and leave every other byte as it was.
type jnode struct {
	kind       byte // '{' object, '[' array, '"' string, '0' number, 't' true, 'f' false, 'n' null
	start, end int
	members    []jmember
	elems      []*jnode
	str        string // a string's decoded value
}

// jmember is one object member: its decoded name, where its name token
// starts, and its value.
type jmember struct {
	name      string
	nameStart int
	val       *jnode
}

// errJSON is a body that is not one well-formed JSON value in UTF-8, or that
// names one member twice in an object (exactly, or equal under case folding,
// as the relay's strict decoder refuses), so it cannot be read unambiguously.
var errJSON = errors.New("subscription: the body is not one unambiguous JSON value")

// scanJSON reads body into its value tree.
func scanJSON(body []byte) (*jnode, error) {
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, errJSON
	}
	p := &jparser{b: body}
	n, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, errJSON
	}
	return n, nil
}

// scanDecodedJSON is scanJSON for raw, one JSON value a json.Decoder has
// already read whole, so its syntax is known to be valid: only its UTF-8,
// its repeated names and its depth are left to check.
//
// w counts UTF-8's pass, the parser's, its strings' decoding and the values
// it reads.
func scanDecodedJSON(raw []byte, w *walk) (*jnode, error) {
	w.add(len(raw))
	if !utf8.Valid(raw) {
		return nil, errJSON
	}
	p := &jparser{b: raw, w: w}
	n, err := p.value(0)
	w.add(p.i)
	if err != nil || p.i != len(p.b) {
		return nil, errJSON
	}
	return n, nil
}

type jparser struct {
	b []byte
	i int
	w *walk
}

func (p *jparser) ws() {
	for p.i < len(p.b) && (p.b[p.i] == ' ' || p.b[p.i] == '\t' || p.b[p.i] == '\n' || p.b[p.i] == '\r') {
		p.i++
	}
}

// value reads one value. The body is valid JSON already, so only the spans,
// the structure and the repeated names are read here.
func (p *jparser) value(depth int) (*jnode, error) {
	if depth > maxDepth {
		return nil, errJSON
	}
	p.ws()
	if p.i >= len(p.b) {
		return nil, errJSON
	}
	n := &jnode{start: p.i}
	p.w.add(1)
	switch c := p.b[p.i]; {
	case c == '{':
		n.kind = '{'
		p.i++
		// The names read so far, folded: the first held alone, a map made
		// only when a second arrives, so an object of one member costs no
		// map.
		var firstName string
		var seen map[string]bool
		for {
			p.ws()
			if p.b[p.i] == '}' {
				p.i++
				break
			}
			if p.b[p.i] == ',' {
				p.i++
				p.ws()
			}
			nameStart := p.i
			name, err := p.str()
			if err != nil {
				return nil, err
			}
			folded := jsonmember.FoldedName(name)
			switch {
			case len(n.members) == 0:
				firstName = folded
			case seen == nil:
				if folded == firstName {
					return nil, errJSON
				}
				seen = map[string]bool{firstName: true, folded: true}
			case seen[folded]:
				return nil, errJSON
			default:
				seen[folded] = true
			}
			p.ws()
			p.i++ // ':'
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.members = append(n.members, jmember{name: name, nameStart: nameStart, val: v})
		}
	case c == '[':
		n.kind = '['
		p.i++
		for {
			p.ws()
			if p.b[p.i] == ']' {
				p.i++
				break
			}
			if p.b[p.i] == ',' {
				p.i++
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.elems = append(n.elems, v)
		}
	case c == '"':
		n.kind = '"'
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		n.str = s
	case c == 't', c == 'f', c == 'n':
		n.kind = c
		for p.i < len(p.b) && p.b[p.i] >= 'a' && p.b[p.i] <= 'z' {
			p.i++
		}
	default:
		n.kind = '0'
		for p.i < len(p.b) && bytes.IndexByte([]byte("+-.0123456789eE"), p.b[p.i]) >= 0 {
			p.i++
		}
	}
	n.end = p.i
	return n, nil
}

// str reads a string token and decodes it.
func (p *jparser) str() (string, error) {
	start := p.i
	p.i++ // '"'
	for p.i < len(p.b) && p.b[p.i] != '"' {
		if p.b[p.i] == '\\' {
			p.i++
		}
		p.i++
	}
	p.i++
	p.w.add(p.i - start)
	var s string
	if err := json.Unmarshal(p.b[start:p.i], &s); err != nil {
		return "", errJSON
	}
	return s, nil
}

// member is obj's member named name (exactly), or nil.
func (n *jnode) member(name string) *jnode {
	if n == nil || n.kind != '{' {
		return nil
	}
	for _, m := range n.members {
		if m.name == name {
			return m.val
		}
	}
	return nil
}

// memberFolded is obj's member whose name equals name under the relay's case
// folding (jsonmember.FoldedName), or nil: the spelling a lenient reader
// would also take for name. A body naming one member twice under folding is
// refused by scanJSON, so there is at most one.
func (n *jnode) memberFolded(name string) *jnode {
	if n == nil || n.kind != '{' {
		return nil
	}
	want := jsonmember.FoldedName(name)
	for _, m := range n.members {
		if jsonmember.FoldedName(m.name) == want {
			return m.val
		}
	}
	return nil
}

// memberIndex is the index of obj's member named name, or -1.
func (n *jnode) memberIndex(name string) int {
	if n == nil || n.kind != '{' {
		return -1
	}
	for i, m := range n.members {
		if m.name == name {
			return i
		}
	}
	return -1
}

// stringOf is a string value's decoded value, and whether n is a string.
func (n *jnode) stringOf() (string, bool) {
	if n == nil || n.kind != '"' {
		return "", false
	}
	return n.str, true
}

// splice replaces the bytes [start, end) with repl.
type splice struct {
	start, end int
	repl       []byte
}

// applySplices applies non-overlapping splices to src and returns a new
// slice; src is unchanged.
func applySplices(src []byte, sp []splice) ([]byte, error) {
	sp = slices.Clone(sp)
	slices.SortFunc(sp, func(a, b splice) int { return a.start - b.start })
	out := make([]byte, 0, len(src))
	at := 0
	for _, s := range sp {
		if s.start < at || s.end < s.start || s.end > len(src) {
			return nil, fmt.Errorf("subscription: overlapping edits")
		}
		out = append(out, src[at:s.start]...)
		out = append(out, s.repl...)
		at = s.end
	}
	return append(out, src[at:]...), nil
}

// removeMember is the splice that removes obj's member i with exactly one
// adjacent comma, leaving every other byte of the body as it was.
func removeMember(obj *jnode, i int) splice {
	m := obj.members
	switch {
	case i+1 < len(m):
		return splice{start: m[i].nameStart, end: m[i+1].nameStart}
	case i > 0:
		return splice{start: m[i-1].val.end, end: m[i].val.end}
	default:
		return splice{start: m[i].nameStart, end: m[i].val.end}
	}
}

// insertMember is the splice that adds a member to obj after its last one
// (or as its only one).
func insertMember(obj *jnode, name string, value []byte) splice {
	member := append(jsonString(name), ':')
	member = append(member, value...)
	if len(obj.members) == 0 {
		return splice{start: obj.start + 1, end: obj.start + 1, repl: member}
	}
	last := obj.members[len(obj.members)-1].val.end
	return splice{start: last, end: last, repl: append([]byte(","), member...)}
}

// insertMembers is the splice that adds members, each written "<name>:<value>",
// to obj after its last one (or as its only ones), in order.
func insertMembers(obj *jnode, members []string) splice {
	joined := []byte(strings.Join(members, ","))
	if len(obj.members) == 0 {
		return splice{start: obj.start + 1, end: obj.start + 1, repl: joined}
	}
	last := obj.members[len(obj.members)-1].val.end
	return splice{start: last, end: last, repl: append([]byte(","), joined...)}
}

// jsonString is s as a JSON string, with no HTML escaping, so what a
// participant wrote reads back as it was.
func jsonString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a Go string always encodes
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// jsonStrings is ss as a JSON array of strings.
func jsonStrings(ss []string) []byte {
	out := []byte{'['}
	for i, s := range ss {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, jsonString(s)...)
	}
	return append(out, ']')
}
