// Package splice is a byte-preserving JSON scanner and editor.
//
// Scan parses one RFC 8259 JSON document with its own scanner and records,
// for every value, the byte span it occupies and, for every object member,
// the offsets of its name, colon, value and trailing comma. The scan is
// strict: exactly one top-level value followed only by whitespace, valid
// UTF-8 (including inside escapes), no lone UTF-16 surrogate in a \u escape,
// bounded depth, size and token count, and no repeated member name in any
// object anywhere in the document, compared after unescaping and under
// Unicode simple case folding (so "patient" and "Patient" collide, as they
// do for decoders that match names case-insensitively). A document that two
// readers could interpret differently is refused rather than edited.
//
// Apply edits a scanned document with a closed set of operations addressed
// to resolved nodes. The output is the original bytes with only the declared
// spans substituted; everything outside them (number lexemes, whitespace,
// escapes, member order) is carried unchanged.
//
// This package depends only on the Go standard library.
package splice

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Sentinel errors. Every error returned by Scan and Apply matches at least
// one of these with errors.Is.
var (
	// ErrSyntax: the input is not RFC 8259 JSON.
	ErrSyntax = errors.New("splice: invalid JSON")
	// ErrTrailingData: something other than whitespace follows the value.
	ErrTrailingData = errors.New("splice: data after the JSON value")
	// ErrInvalidUTF8: a string holds bytes that are not valid UTF-8.
	ErrInvalidUTF8 = errors.New("splice: invalid UTF-8")
	// ErrLoneSurrogate: a \u escape names half of a UTF-16 surrogate pair.
	ErrLoneSurrogate = errors.New("splice: lone surrogate escape")
	// ErrDepthLimit: containers nest deeper than Limits.MaxDepth.
	ErrDepthLimit = errors.New("splice: nesting depth limit exceeded")
	// ErrTokenLimit: the document holds more than Limits.MaxTokens tokens.
	ErrTokenLimit = errors.New("splice: token limit exceeded")
	// ErrSizeLimit: the document is longer than Limits.MaxBytes.
	ErrSizeLimit = errors.New("splice: size limit exceeded")
	// ErrDuplicateKey: an object repeats a member name. Returned as a
	// *DuplicateKeyError.
	ErrDuplicateKey = errors.New("splice: duplicate member name")
)

// ScanError reports where and why a document was refused. Err is one of the
// sentinel errors above.
type ScanError struct {
	Offset int
	Err    error
	Detail string
}

func (e *ScanError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%v at offset %d", e.Err, e.Offset)
	}
	return fmt.Sprintf("%v at offset %d: %s", e.Err, e.Offset, e.Detail)
}

func (e *ScanError) Unwrap() error { return e.Err }

// DuplicateKeyError reports a member name that occurs twice in one object,
// exactly or under case folding. Key is the decoded name of the second
// occurrence, Offset the byte offset of its opening quote, and Depth the
// number of containers enclosing the object (0 for a top-level object). It
// matches ErrDuplicateKey with errors.Is.
type DuplicateKeyError struct {
	Key    string
	Offset int
	Depth  int
}

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("%v %q at offset %d (depth %d)", ErrDuplicateKey, e.Key, e.Offset, e.Depth)
}

// Is reports whether target is ErrDuplicateKey.
func (e *DuplicateKeyError) Is(target error) bool { return target == ErrDuplicateKey }

// Limits bounds a scan. A zero (or negative) field selects that field's
// default, so Limits{} is the default policy: depth 64, 1,000,000 tokens and
// 16 MiB. Depth counts nested containers, the top-level container being 1.
// Every value and every member name counts as one token.
type Limits struct {
	MaxDepth  int
	MaxTokens int
	MaxBytes  int64
}

const (
	defaultMaxDepth  = 64
	defaultMaxTokens = 1_000_000
	defaultMaxBytes  = 16 << 20
)

// DefaultLimits returns the default policy with every field filled in.
func DefaultLimits() Limits {
	return Limits{MaxDepth: defaultMaxDepth, MaxTokens: defaultMaxTokens, MaxBytes: defaultMaxBytes}
}

func (l Limits) withDefaults() Limits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = defaultMaxDepth
	}
	if l.MaxTokens <= 0 {
		l.MaxTokens = defaultMaxTokens
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = defaultMaxBytes
	}
	return l
}

// Kind is a JSON value kind. The zero Kind is returned for an id that does
// not name a node.
type Kind uint8

const (
	KindObject Kind = iota + 1
	KindArray
	KindString
	KindNumber
	KindBool
	KindNull
)

func (k Kind) String() string {
	switch k {
	case KindObject:
		return "object"
	case KindArray:
		return "array"
	case KindString:
		return "string"
	case KindNumber:
		return "number"
	case KindBool:
		return "bool"
	case KindNull:
		return "null"
	}
	return "invalid"
}

// NodeID names a value in a scanned document. Scanned ids are dense,
// 0..Len()-1, in document order: each value's id is smaller than the ids of
// every value it contains and of every value that starts after it, so the
// root is 0. Negative ids are Handles (see EnsureObjectMember).
type NodeID int

type member struct {
	name     string // decoded
	keyStart int    // opening quote of the name
	keyEnd   int    // one past the closing quote
	colon    int
	value    NodeID
	comma    int // the comma after the value, or -1
}

type node struct {
	kind    Kind
	start   int // first byte of the value
	end     int // one past its last byte
	members []member
	elems   []NodeID
}

// Doc is a scanned document. It retains the scanned bytes, which the caller
// must not modify afterwards.
type Doc struct {
	src   []byte
	lim   Limits
	nodes []node
}

// Scan parses src as exactly one JSON document under lim (see Limits for
// zero values). The refusal reasons are the sentinel errors of this package;
// a repeated member name is reported as a *DuplicateKeyError, everything
// else as a *ScanError.
func Scan(src []byte, lim Limits) (*Doc, error) {
	lim = lim.withDefaults()
	if int64(len(src)) > lim.MaxBytes {
		return nil, &ScanError{Offset: int(lim.MaxBytes), Err: ErrSizeLimit,
			Detail: fmt.Sprintf("%d bytes, limit %d", len(src), lim.MaxBytes)}
	}
	s := scanner{src: src, lim: lim, d: &Doc{src: src, lim: lim}}
	if err := s.run(); err != nil {
		return nil, err
	}
	return s.d, nil
}

// frame is one open container during the scan.
type frame struct {
	id      NodeID
	object  bool
	keys    map[string]struct{} // folded names, built once an object grows past indexAt
	pending member              // the member whose value is being scanned
}

// indexAt is the member count from which duplicate detection uses a map
// instead of a linear scan.
const indexAt = 16

type scanner struct {
	src    []byte
	lim    Limits
	d      *Doc
	tokens int
	stack  []frame
}

func (s *scanner) fail(off int, err error, detail string) error {
	return &ScanError{Offset: off, Err: err, Detail: detail}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (s *scanner) skipSpace(pos int) int {
	for pos < len(s.src) && isSpace(s.src[pos]) {
		pos++
	}
	return pos
}

func (s *scanner) token(off int) error {
	s.tokens++
	if s.tokens > s.lim.MaxTokens {
		return s.fail(off, ErrTokenLimit, fmt.Sprintf("limit %d", s.lim.MaxTokens))
	}
	return nil
}

func (s *scanner) describe(pos int) string {
	if pos >= len(s.src) {
		return "unexpected end of input"
	}
	return fmt.Sprintf("unexpected byte %q", s.src[pos])
}

// run scans the whole document without recursion, so input nesting cannot
// grow the goroutine stack.
func (s *scanner) run() error {
	src := s.src
	pos := s.skipSpace(0)
	for {
		// A value starts at pos.
		if pos >= len(src) {
			return s.fail(pos, ErrSyntax, s.describe(pos))
		}
		if err := s.token(pos); err != nil {
			return err
		}
		id := NodeID(len(s.d.nodes))
		n := node{start: pos}
		container := false
		switch c := src[pos]; {
		case c == '{' || c == '[':
			container = true
			if c == '{' {
				n.kind = KindObject
			} else {
				n.kind = KindArray
			}
			pos++
		case c == '"':
			end, err := s.scanString(pos)
			if err != nil {
				return err
			}
			n.kind, pos = KindString, end
		case c == '-' || (c >= '0' && c <= '9'):
			end, err := s.scanNumber(pos)
			if err != nil {
				return err
			}
			n.kind, pos = KindNumber, end
		case c == 't':
			end, err := s.scanLiteral(pos, "true")
			if err != nil {
				return err
			}
			n.kind, pos = KindBool, end
		case c == 'f':
			end, err := s.scanLiteral(pos, "false")
			if err != nil {
				return err
			}
			n.kind, pos = KindBool, end
		case c == 'n':
			end, err := s.scanLiteral(pos, "null")
			if err != nil {
				return err
			}
			n.kind, pos = KindNull, end
		default:
			return s.fail(pos, ErrSyntax, s.describe(pos))
		}
		n.end = pos
		s.d.nodes = append(s.d.nodes, n)
		s.attach(id)

		if container {
			if len(s.stack) >= s.lim.MaxDepth {
				return s.fail(n.start, ErrDepthLimit, fmt.Sprintf("limit %d", s.lim.MaxDepth))
			}
			s.stack = append(s.stack, frame{id: id, object: n.kind == KindObject})
			pos = s.skipSpace(pos)
			closer := byte(']')
			if n.kind == KindObject {
				closer = '}'
			}
			if pos < len(src) && src[pos] == closer {
				s.d.nodes[id].end = pos + 1
				s.stack = s.stack[:len(s.stack)-1]
				pos++
			} else {
				if n.kind == KindObject {
					var err error
					if pos, err = s.scanName(pos); err != nil {
						return err
					}
				}
				continue // the first value of the container
			}
		}

		// A value just completed: close containers and find the next value.
		for {
			if len(s.stack) == 0 {
				pos = s.skipSpace(pos)
				if pos != len(src) {
					return s.fail(pos, ErrTrailingData, "")
				}
				return nil
			}
			pos = s.skipSpace(pos)
			if pos >= len(src) {
				return s.fail(pos, ErrSyntax, s.describe(pos))
			}
			top := &s.stack[len(s.stack)-1]
			c := src[pos]
			if c == ',' {
				if top.object {
					ms := s.d.nodes[top.id].members
					ms[len(ms)-1].comma = pos
				}
				pos = s.skipSpace(pos + 1)
				if top.object {
					var err error
					if pos, err = s.scanName(pos); err != nil {
						return err
					}
				}
				break // the next value of the container
			}
			if (top.object && c == '}') || (!top.object && c == ']') {
				s.d.nodes[top.id].end = pos + 1
				s.stack = s.stack[:len(s.stack)-1]
				pos++
				continue
			}
			return s.fail(pos, ErrSyntax, s.describe(pos))
		}
	}
}

// attach links a new value to its enclosing container.
func (s *scanner) attach(id NodeID) {
	if len(s.stack) == 0 {
		return
	}
	top := &s.stack[len(s.stack)-1]
	p := &s.d.nodes[top.id]
	if top.object {
		m := top.pending
		m.value = id
		p.members = append(p.members, m)
		return
	}
	p.elems = append(p.elems, id)
}

// scanName scans `"name" :` at pos (after optional whitespace has been
// skipped), checks the name against the object's other names, and leaves pos
// at the value.
func (s *scanner) scanName(pos int) (int, error) {
	src := s.src
	if pos >= len(src) || src[pos] != '"' {
		return pos, s.fail(pos, ErrSyntax, "expected a member name: "+s.describe(pos))
	}
	if err := s.token(pos); err != nil {
		return pos, err
	}
	end, err := s.scanString(pos)
	if err != nil {
		return pos, err
	}
	name := unquote(src[pos:end])
	top := &s.stack[len(s.stack)-1]
	obj := &s.d.nodes[top.id]
	dup := false
	if top.keys != nil {
		_, dup = top.keys[foldName(name)]
	} else {
		for i := range obj.members {
			if strings.EqualFold(obj.members[i].name, name) {
				dup = true
				break
			}
		}
	}
	if dup {
		return pos, &DuplicateKeyError{Key: name, Offset: pos, Depth: len(s.stack) - 1}
	}
	if top.keys != nil {
		top.keys[foldName(name)] = struct{}{}
	} else if len(obj.members)+1 >= indexAt {
		top.keys = make(map[string]struct{}, 2*indexAt)
		for i := range obj.members {
			top.keys[foldName(obj.members[i].name)] = struct{}{}
		}
		top.keys[foldName(name)] = struct{}{}
	}
	colon := s.skipSpace(end)
	if colon >= len(src) || src[colon] != ':' {
		return colon, s.fail(colon, ErrSyntax, "expected ':': "+s.describe(colon))
	}
	top.pending = member{name: name, keyStart: pos, keyEnd: end, colon: colon, comma: -1}
	return s.skipSpace(colon + 1), nil
}

// foldName maps name to a canonical form such that foldName(a) ==
// foldName(b) exactly when strings.EqualFold(a, b): each rune becomes the
// smallest rune of its simple case-folding orbit.
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

func hexVal(c byte) (rune, bool) {
	if c >= '0' && c <= '9' {
		return rune(c - '0'), true
	}
	if lower := c | 0x20; lower >= 'a' && lower <= 'f' { // ASCII letters only
		return rune(lower-'a') + 10, true
	}
	return 0, false
}

// hex4 decodes the four hex digits at src[i:i+4].
func hex4(src []byte, i int) (rune, bool) {
	if i+4 > len(src) {
		return 0, false
	}
	var r rune
	for j := i; j < i+4; j++ {
		v, ok := hexVal(src[j])
		if !ok {
			return 0, false
		}
		r = r<<4 | v
	}
	return r, true
}

// scanString validates the string token starting at the quote at pos and
// returns the offset one past its closing quote.
func (s *scanner) scanString(pos int) (int, error) {
	src := s.src
	i := pos + 1
	for {
		if i >= len(src) {
			return i, s.fail(i, ErrSyntax, "unterminated string")
		}
		c := src[i]
		switch {
		case c == '"':
			return i + 1, nil
		case c == '\\':
			if i+1 >= len(src) {
				return i, s.fail(i, ErrSyntax, "unterminated escape")
			}
			switch src[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				r, ok := hex4(src, i+2)
				if !ok {
					return i, s.fail(i, ErrSyntax, "malformed \\u escape")
				}
				switch {
				case utf16.IsSurrogate(r) && r < 0xDC00:
					if i+7 < len(src) && src[i+6] == '\\' && src[i+7] == 'u' {
						lo, ok := hex4(src, i+8)
						if !ok {
							return i + 6, s.fail(i+6, ErrSyntax, "malformed \\u escape")
						}
						if lo >= 0xDC00 && lo <= 0xDFFF {
							i += 12
							continue
						}
					}
					return i, s.fail(i, ErrLoneSurrogate, "high surrogate without a low surrogate")
				case utf16.IsSurrogate(r):
					return i, s.fail(i, ErrLoneSurrogate, "low surrogate without a high surrogate")
				}
				i += 6
			default:
				return i, s.fail(i, ErrSyntax, fmt.Sprintf("invalid escape %q", src[i+1]))
			}
		case c < 0x20:
			return i, s.fail(i, ErrSyntax, "control character in string")
		case c < utf8.RuneSelf:
			i++
		default:
			r, size := utf8.DecodeRune(src[i:])
			if r == utf8.RuneError && size == 1 {
				return i, s.fail(i, ErrInvalidUTF8, "")
			}
			i += size
		}
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// scanNumber validates the number token at pos (RFC 8259 grammar).
func (s *scanner) scanNumber(pos int) (int, error) {
	src := s.src
	i := pos
	if src[i] == '-' {
		i++
	}
	switch {
	case i < len(src) && src[i] == '0':
		i++
	case i < len(src) && src[i] >= '1' && src[i] <= '9':
		for i < len(src) && isDigit(src[i]) {
			i++
		}
	default:
		return i, s.fail(i, ErrSyntax, "malformed number: "+s.describe(i))
	}
	if i < len(src) && src[i] == '.' {
		i++
		if i >= len(src) || !isDigit(src[i]) {
			return i, s.fail(i, ErrSyntax, "malformed number fraction: "+s.describe(i))
		}
		for i < len(src) && isDigit(src[i]) {
			i++
		}
	}
	if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
		i++
		if i < len(src) && (src[i] == '+' || src[i] == '-') {
			i++
		}
		if i >= len(src) || !isDigit(src[i]) {
			return i, s.fail(i, ErrSyntax, "malformed number exponent: "+s.describe(i))
		}
		for i < len(src) && isDigit(src[i]) {
			i++
		}
	}
	return i, nil
}

func (s *scanner) scanLiteral(pos int, lit string) (int, error) {
	for j := 0; j < len(lit); j++ {
		if pos+j >= len(s.src) || s.src[pos+j] != lit[j] {
			return pos + j, s.fail(pos+j, ErrSyntax, "malformed literal: "+s.describe(pos+j))
		}
	}
	return pos + len(lit), nil
}

// unquote decodes a string token that scanString has already validated.
func unquote(tok []byte) string {
	body := tok[1 : len(tok)-1]
	esc := false
	for _, c := range body {
		if c == '\\' {
			esc = true
			break
		}
	}
	if !esc {
		return string(body)
	}
	out := make([]byte, 0, len(body))
	for i := 0; i < len(body); {
		c := body[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		switch body[i+1] {
		case '"', '\\', '/':
			out = append(out, body[i+1])
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, _ := hex4(body, i+2)
			if utf16.IsSurrogate(r) {
				lo, _ := hex4(body, i+8)
				r = utf16.DecodeRune(r, lo)
				i += 6
			}
			out = utf8.AppendRune(out, r)
			i += 6
			continue
		}
		i += 2
	}
	return string(out)
}

func (d *Doc) node(n NodeID) *node {
	if n < 0 || int(n) >= len(d.nodes) {
		return nil
	}
	return &d.nodes[n]
}

// Root returns the top-level value.
func (d *Doc) Root() NodeID { return 0 }

// Member returns the value of obj's member whose decoded name equals key.
func (d *Doc) Member(obj NodeID, key string) (NodeID, bool) {
	o := d.node(obj)
	if o == nil || o.kind != KindObject {
		return 0, false
	}
	if i := o.memberIndex(key); i >= 0 {
		return o.members[i].value, true
	}
	return 0, false
}

func (o *node) memberIndex(key string) int {
	for i := range o.members {
		if o.members[i].name == key {
			return i
		}
	}
	return -1
}

// Len returns the number of scanned values.
func (d *Doc) Len() int { return len(d.nodes) }

// MemberRef describes one object member: its decoded name, its value, and
// the half-open byte range of the name token (quotes included).
type MemberRef struct {
	Name             string
	Value            NodeID
	KeyStart, KeyEnd int
}

// Members returns obj's members in document order, or nil if obj is not an
// object. The slice is the caller's.
func (d *Doc) Members(obj NodeID) []MemberRef {
	o := d.node(obj)
	if o == nil || o.kind != KindObject {
		return nil
	}
	out := make([]MemberRef, len(o.members))
	for i, m := range o.members {
		out[i] = MemberRef{Name: m.name, Value: m.value, KeyStart: m.keyStart, KeyEnd: m.keyEnd}
	}
	return out
}

// Elems returns the elements of an array in order, or nil if arr is not an
// array. The returned slice must not be modified.
func (d *Doc) Elems(arr NodeID) []NodeID {
	a := d.node(arr)
	if a == nil || a.kind != KindArray {
		return nil
	}
	return a.elems
}

// Kind returns n's kind, or the zero Kind if n names no node.
func (d *Doc) Kind(n NodeID) Kind {
	if x := d.node(n); x != nil {
		return x.kind
	}
	return 0
}

// Span returns the half-open byte range [start, end) of n in the scanned
// bytes, or (-1, -1) if n names no node.
func (d *Doc) Span(n NodeID) (start, end int) {
	if x := d.node(n); x != nil {
		return x.start, x.end
	}
	return -1, -1
}

// StringValue returns the decoded value of a string node.
func (d *Doc) StringValue(n NodeID) (string, error) {
	x := d.node(n)
	if x == nil || x.kind != KindString {
		return "", fmt.Errorf("%w: node %d is not a string", ErrInvalidOp, n)
	}
	return unquote(d.src[x.start:x.end]), nil
}
