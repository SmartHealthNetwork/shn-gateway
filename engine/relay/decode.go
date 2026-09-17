package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

var (
	// ErrDuplicateKey: a body repeats a member name in some object. Such a
	// body can be read two ways, so it is refused before anything decides
	// on it (a client error). Returned as a *DuplicateKeyError.
	ErrDuplicateKey = errors.New("relay: duplicate member name")
	// ErrInvalidJSON: a body is not exactly one well-formed JSON document (a
	// client error). Returned as an *InvalidJSONError.
	ErrInvalidJSON = errors.New("relay: invalid JSON")
	// ErrTooLarge: a body exceeds the size (16 MiB), nesting depth (64) or
	// token count (1,000,000) bound, so it is refused as too large rather
	// than as malformed. Returned as a *LimitError.
	ErrTooLarge = errors.New("relay: body exceeds a size, depth or token limit")
)

// DuplicateKeyError reports the first repeated member name. Key is the
// decoded name and Offset the byte offset of its second occurrence.
type DuplicateKeyError struct {
	Key    string
	Offset int
	err    error
}

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("relay: duplicate member name %q at offset %d", e.Key, e.Offset)
}

// Is reports whether target is ErrDuplicateKey.
func (e *DuplicateKeyError) Is(target error) bool { return target == ErrDuplicateKey }

// Unwrap returns the scanner's error.
func (e *DuplicateKeyError) Unwrap() error { return e.err }

// InvalidJSONError reports why and where a body was refused.
type InvalidJSONError struct {
	Offset int
	Reason string
	err    error
}

func (e *InvalidJSONError) Error() string {
	return fmt.Sprintf("relay: invalid JSON at offset %d: %s", e.Offset, e.Reason)
}

// Is reports whether target is ErrInvalidJSON.
func (e *InvalidJSONError) Is(target error) bool { return target == ErrInvalidJSON }

// Unwrap returns the scanner's error.
func (e *InvalidJSONError) Unwrap() error { return e.err }

// LimitError reports which bound a body exceeded and where.
type LimitError struct {
	Offset int
	Reason string
	err    error
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("relay: body too large at offset %d: %s", e.Offset, e.Reason)
}

// Is reports whether target is ErrTooLarge.
func (e *LimitError) Is(target error) bool { return target == ErrTooLarge }

// Unwrap returns the scanner's error.
func (e *LimitError) Unwrap() error { return e.err }

// scanCount counts strict scans; tests use it to pin how often a body is
// parsed.
var scanCount atomic.Int64

// scan is the single strict parse every reader and editor starts from.
func scan(b []byte) (*splice.Doc, error) {
	scanCount.Add(1)
	d, err := splice.Scan(b, splice.Limits{})
	if err == nil {
		return d, nil
	}
	var dk *splice.DuplicateKeyError
	if errors.As(err, &dk) {
		return nil, &DuplicateKeyError{Key: dk.Key, Offset: dk.Offset, err: err}
	}
	offset, reason := 0, err.Error()
	var se *splice.ScanError
	if errors.As(err, &se) {
		offset, reason = se.Offset, se.Err.Error()
		if se.Detail != "" {
			reason += ": " + se.Detail
		}
	}
	if errors.Is(err, splice.ErrSizeLimit) || errors.Is(err, splice.ErrDepthLimit) || errors.Is(err, splice.ErrTokenLimit) {
		return nil, &LimitError{Offset: offset, Reason: reason, err: err}
	}
	return nil, &InvalidJSONError{Offset: offset, Reason: reason, err: err}
}

// Decode reads body into v. The body is first scanned strictly: a repeated
// member name anywhere (exactly, or equal under case folding) is a
// *DuplicateKeyError, a body over a size, depth or token bound is a
// *LimitError, and anything else that is not one well-formed JSON document
// is an *InvalidJSONError. Member names must then match v's field names
// exactly: a member that would reach a struct field only by case folding
// is a *FieldNameError (members that match no field are ignored, as the
// decoder ignores them). Below a value whose type decodes itself
// (json.Unmarshaler, encoding.TextUnmarshaler, json.RawMessage) or a
// generic value (any), names are not checked; that code reads them as it
// chooses. v is untouched in each refusal. Numbers decode as json.Number,
// so their lexemes are kept. A value that does not fit v is reported with
// the decoder's own error.
func Decode(body Body, v any) error {
	b := body.bytes()
	d, err := scan(b)
	if err != nil {
		return err
	}
	if err := checkFieldNames(d, d.Root(), v); err != nil {
		return err
	}
	return decodeBytes(b, v)
}

func decodeBytes(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("relay: decode: %w", err)
	}
	return nil
}

// NodeID names a value in a Document. Ids are dense (0..Len()-1) in
// document order, the root being 0. Negative ids are Handles.
type NodeID = splice.NodeID

// Kind is a JSON value kind.
type Kind = splice.Kind

// JSON value kinds.
const (
	KindObject = splice.KindObject
	KindArray  = splice.KindArray
	KindString = splice.KindString
	KindNumber = splice.KindNumber
	KindBool   = splice.KindBool
	KindNull   = splice.KindNull
)

// MemberRef describes one object member: its decoded name, its value, and
// the byte range of the name token.
type MemberRef = splice.MemberRef

// Document is a read-only structural view of a Body, for locating the
// values an edit addresses. It exposes offsets and decoded values, never
// the body's bytes.
type Document struct {
	d    *splice.Doc
	body Body // pointer-backed, so a copied Document never prints bytes
}

// Doc scans body strictly (see Decode for the refusals) and returns its
// structure. Node ids from a Document address the same values in Apply on
// the same Body.
func Doc(body Body) (*Document, error) {
	b := body.bytes()
	d, err := scan(b)
	if err != nil {
		return nil, err
	}
	return &Document{d: d, body: body}, nil
}

// Root returns the top-level value.
func (d *Document) Root() NodeID { return d.d.Root() }

// Len returns the number of values in the document.
func (d *Document) Len() int { return d.d.Len() }

// Kind returns n's kind, or the zero Kind if n names no value.
func (d *Document) Kind(n NodeID) Kind { return d.d.Kind(n) }

// Member returns the value of obj's member with the given decoded name.
func (d *Document) Member(obj NodeID, key string) (NodeID, bool) { return d.d.Member(obj, key) }

// Members returns obj's members in document order (nil if obj is not an
// object).
func (d *Document) Members(obj NodeID) []MemberRef { return d.d.Members(obj) }

// Elems returns arr's elements in order (nil if arr is not an array).
func (d *Document) Elems(arr NodeID) []NodeID { return append([]NodeID(nil), d.d.Elems(arr)...) }

// Span returns the byte range [start, end) of n in the body, or (-1, -1).
// Authored payloads use it to declare Embed spans.
func (d *Document) Span(n NodeID) (start, end int) { return d.d.Span(n) }

// StringValue returns the decoded value of a string.
func (d *Document) StringValue(n NodeID) (string, error) { return d.d.StringValue(n) }

// DecodeValue decodes the value n into v, with numbers as json.Number and
// member names checked against v's fields as Decode checks them (paths in
// a *FieldNameError start at n).
func (d *Document) DecodeValue(n NodeID, v any) error {
	s, e := d.d.Span(n)
	if s < 0 {
		return fmt.Errorf("relay: node %d does not exist", n)
	}
	if err := checkFieldNames(d.d, n, v); err != nil {
		return err
	}
	return decodeBytes(d.body.bytes()[s:e], v)
}

// Format prints a summary for every verb.
func (d *Document) Format(f fmt.State, _ rune) {
	if d == nil || d.d == nil {
		fmt.Fprint(f, "relay.Document{}")
		return
	}
	fmt.Fprintf(f, "relay.Document{%d values}", d.d.Len())
}

// Op is one edit operation, made by a Document's methods and bound to that
// Document's Body: Apply refuses an Op made from any other Body. The zero Op
// is refused too.
type Op struct {
	body *bodyContent
	op   splice.Op
}

// Format prints the operation's kind, target and value length; never the
// value bytes or the member name.
func (o Op) Format(f fmt.State, _ rune) {
	if o.op == nil {
		fmt.Fprint(f, "relay.Op{}")
		return
	}
	info := splice.Describe(o.op)
	fmt.Fprintf(f, "relay.Op{%s node %d, %d value bytes}", info.Kind, info.Target, len(info.Value))
}

// Handle names an object that EnsureObjectMember resolves or creates;
// NodeID(h) passes it to a later operation in the same change set.
type Handle = splice.Handle

func (d *Document) op(o splice.Op) Op { return Op{body: d.body.c, op: o} }

// Replace replaces exactly the value n with value (one JSON value).
func (d *Document) Replace(n NodeID, value []byte) Op { return d.op(splice.Replace(n, value)) }

// RemoveMember removes obj's member key with exactly one adjacent comma.
func (d *Document) RemoveMember(obj NodeID, key string) Op {
	return d.op(splice.RemoveMember(obj, key))
}

// InsertMember appends a member before obj's closing brace, following the
// object's own layout.
func (d *Document) InsertMember(obj NodeID, key string, value []byte) Op {
	return d.op(splice.InsertMember(obj, key, value))
}

// EnsureObjectMember makes sure obj has an object-valued member key,
// creating `key: {}` when it is absent, and returns a Handle to it.
func (d *Document) EnsureObjectMember(obj NodeID, key string) (Op, Handle) {
	o, h := splice.EnsureObjectMember(obj, key)
	return d.op(o), h
}

// AppendElement appends value to array arr, following the array's own
// layout.
func (d *Document) AppendElement(arr NodeID, value []byte) Op {
	return d.op(splice.AppendElement(arr, value))
}
