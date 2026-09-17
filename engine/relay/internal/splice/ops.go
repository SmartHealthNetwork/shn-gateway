package splice

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"unicode/utf8"
)

// Errors returned by Apply (in addition to the scan sentinels, which a
// refused value or output wraps).
var (
	// ErrOverlap: two operations touch the same bytes, or an operation
	// edits inside a value another operation removes or replaces.
	ErrOverlap = errors.New("splice: overlapping operations")
	// ErrInvalidOp: an operation addresses a node that does not exist, has
	// the wrong kind, or is a Handle not created earlier in the same Apply.
	ErrInvalidOp = errors.New("splice: invalid operation")
	// ErrNoMember: RemoveMember names a member the object does not have.
	ErrNoMember = errors.New("splice: no such member")
	// ErrInvalidValue: value bytes are not exactly one JSON value with no
	// surrounding whitespace. The underlying scan error is also wrapped.
	ErrInvalidValue = errors.New("splice: value is not exactly one JSON value")
	// ErrPostcondition: the output does not re-scan under the document's
	// limits. The underlying scan error is also wrapped.
	ErrPostcondition = errors.New("splice: output does not re-scan")
)

// Op is one edit operation. The set is closed: operations are made only by
// Replace, RemoveMember, InsertMember, EnsureObjectMember and AppendElement.
type Op interface{ isOp() }

type replaceOp struct {
	n     NodeID
	value []byte
}

type removeOp struct {
	obj NodeID
	key string
}

type insertOp struct {
	obj   NodeID
	key   string
	value []byte
}

type ensureOp struct {
	obj NodeID
	key string
	h   Handle
}

type appendOp struct {
	arr   NodeID
	value []byte
}

func (replaceOp) isOp() {}
func (removeOp) isOp()  {}
func (insertOp) isOp()  {}
func (ensureOp) isOp()  {}
func (appendOp) isOp()  {}

// Replace replaces exactly the bytes of value n with value, which must be
// one JSON value. The value bytes are copied.
func Replace(n NodeID, value []byte) Op { return replaceOp{n, clone(value)} }

// RemoveMember removes obj's member named key (decoded name) together with
// exactly one adjacent comma:
//
//   - a first or middle member is removed from its name up to the next
//     member's name, so its own leading whitespace now leads the next member;
//   - a last member is removed from the end of the previous value up to the
//     end of its own value, so the whitespace before the closing brace stays;
//   - removing every member leaves "{}".
//
// Several members of one object may be removed in one Apply; adjacent
// removals are merged by the same rules.
func RemoveMember(obj NodeID, key string) Op { return removeOp{obj, key} }

// InsertMember appends a member before obj's closing brace, preceded by a
// comma when the object has members. When the object's last member is
// preceded by whitespace containing a line break, the new member copies that
// whitespace run and the whitespace around the last member's colon;
// otherwise it is written compact ("key":value). In an empty object the
// member is written compact immediately before the closing brace. obj may be
// NodeID(h) for a Handle h from an earlier EnsureObjectMember in the same
// Apply. The value bytes are copied.
//
// Inserting a name the object already has is refused with ErrDuplicateKey,
// unless the same Apply removes that member: the removal and the insertion
// then compose, and the new member is written last.
func InsertMember(obj NodeID, key string, value []byte) Op {
	return insertOp{obj, key, clone(value)}
}

// AppendElement appends value to array arr with the same whitespace rule as
// InsertMember: after the last element, copying its leading whitespace run
// when that run contains a line break, otherwise compact; in an empty array,
// immediately before the closing bracket. The value bytes are copied.
func AppendElement(arr NodeID, value []byte) Op { return appendOp{arr, clone(value)} }

// Handle names an object that an EnsureObjectMember operation resolves or
// creates. Handles are negative so they never collide with scanned nodes;
// NodeID(h) passes one to InsertMember, RemoveMember or a nested
// EnsureObjectMember later in the same Apply. A handle means nothing
// outside an Apply that also contains the operation that made it.
type Handle NodeID

var handleSeq atomic.Int64

// EnsureObjectMember makes sure obj has an object-valued member named key
// and returns a Handle to that object.
//
//   - If obj already has the member and its value is an object, the handle
//     resolves to that existing node and the operation itself edits nothing.
//     An existing member of another kind is refused with ErrInvalidOp. Any
//     other operation in the same Apply that replaces or removes the
//     resolved node, or a value enclosing it, is ErrOverlap.
//   - Otherwise the member is created as `key: {}`, placed by the
//     InsertMember rules. Members inserted through the handle in the same
//     Apply are composed into the created object, written compact in
//     operation order.
//
// Ensuring the same absent name twice in one Apply creates one member and
// both handles name it.
func EnsureObjectMember(obj NodeID, key string) (Op, Handle) {
	h := Handle(-handleSeq.Add(1))
	return ensureOp{obj, key, h}, h
}

// Edit is one substituted range: the original bytes [Start, End) are
// replaced by New. Start == End is an insertion.
type Edit struct {
	Start, End int
	New        []byte
}

// created is an object made by EnsureObjectMember in this Apply.
type created struct {
	members []newMember
}

// newMember is a member written by this Apply: either value bytes or a
// created object.
type newMember struct {
	key   string
	value []byte
	obj   *created
}

func (c *created) find(key string) int {
	for i := range c.members {
		if c.members[i].key == key {
			return i
		}
	}
	return -1
}

type objEdits struct {
	removed  map[int]bool
	inserts  created
	resolved map[int]bool // existing members an ensure resolved to
}

type target struct {
	real NodeID
	obj  *created // non-nil for a created object
}

type applier struct {
	d       *Doc
	handles map[Handle]target
	objs    map[NodeID]*objEdits
	arrs    map[NodeID][][]byte
	edits   []Edit
	// claims are existing nodes an EnsureObjectMember resolved to. Each
	// takes part in the overlap check as a zero-length range at the node's
	// first byte, so an operation that replaces or removes the node (or an
	// enclosing value) is refused; claims are never emitted.
	claims map[NodeID]bool
}

func (a *applier) obj(n NodeID) *objEdits {
	oe := a.objs[n]
	if oe == nil {
		oe = &objEdits{removed: map[int]bool{}, resolved: map[int]bool{}}
		a.objs[n] = oe
	}
	return oe
}

// resolve maps an operation's target to a scanned node or a created object.
func (a *applier) resolve(n NodeID) (target, error) {
	if n >= 0 {
		if a.d.node(n) == nil {
			return target{}, fmt.Errorf("%w: node %d does not exist", ErrInvalidOp, n)
		}
		return target{real: n}, nil
	}
	t, ok := a.handles[Handle(n)]
	if !ok {
		return target{}, fmt.Errorf("%w: handle %d was not made by an earlier operation in this Apply", ErrInvalidOp, n)
	}
	return t, nil
}

func (a *applier) realKind(n NodeID, want Kind) error {
	if k := a.d.Kind(n); k != want {
		return fmt.Errorf("%w: node %d is %v, not %v", ErrInvalidOp, n, k, want)
	}
	return nil
}

func (a *applier) checkValue(v []byte) error {
	vd, err := Scan(v, a.d.lim)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidValue, err)
	}
	if s, e := vd.Span(vd.Root()); s != 0 || e != len(v) {
		return fmt.Errorf("%w: whitespace around the value", ErrInvalidValue)
	}
	return nil
}

func (a *applier) dupErr(obj NodeID, key string) error {
	off, _ := a.d.Span(obj)
	return &DuplicateKeyError{Key: key, Offset: off, Depth: -1}
}

// Apply resolves ops against this document's scan and returns the edited
// bytes and the substituted ranges in original offsets, sorted by Start.
// The document itself is not changed. Operations must address disjoint
// bytes (ErrOverlap otherwise); the only composition is between
// EnsureObjectMember and the operations that use its Handle, and between
// removals and insertions in the same object (see RemoveMember and
// InsertMember). The output must re-scan under the document's limits
// (ErrPostcondition otherwise).
//
// For a collision found by Apply, the *DuplicateKeyError has Offset set to
// the object's first byte and Depth -1.
func (d *Doc) Apply(ops ...Op) (out []byte, spans []Edit, err error) {
	a := &applier{
		d:       d,
		handles: map[Handle]target{},
		objs:    map[NodeID]*objEdits{},
		arrs:    map[NodeID][][]byte{},
		claims:  map[NodeID]bool{},
	}
	for _, op := range ops {
		if err := a.add(op); err != nil {
			return nil, nil, err
		}
	}
	if err := a.build(); err != nil {
		return nil, nil, err
	}
	byOffset := func(x, y Edit) int {
		if x.Start != y.Start {
			return x.Start - y.Start
		}
		return x.End - y.End
	}
	slices.SortFunc(a.edits, byOffset)
	ranges := append([]Edit{}, a.edits...)
	for n := range a.claims {
		ranges = append(ranges, Edit{Start: d.nodes[n].start, End: d.nodes[n].start})
	}
	slices.SortFunc(ranges, byOffset)
	for i := 1; i < len(ranges); i++ {
		p, q := ranges[i-1], ranges[i]
		if q.Start < p.End || q.Start == p.Start {
			return nil, nil, fmt.Errorf("%w: [%d,%d) and [%d,%d)", ErrOverlap, p.Start, p.End, q.Start, q.End)
		}
	}
	size := len(d.src)
	for _, e := range a.edits {
		size += len(e.New) - (e.End - e.Start)
	}
	out = make([]byte, 0, size)
	pos := 0
	for _, e := range a.edits {
		out = append(out, d.src[pos:e.Start]...)
		out = append(out, e.New...)
		pos = e.End
	}
	out = append(out, d.src[pos:]...)
	if _, err := Scan(out, d.lim); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrPostcondition, err)
	}
	if a.edits == nil {
		a.edits = []Edit{}
	}
	return out, a.edits, nil
}

func (a *applier) add(op Op) error {
	d := a.d
	switch o := op.(type) {
	case replaceOp:
		if d.node(o.n) == nil {
			return fmt.Errorf("%w: Replace target %d is not a scanned node", ErrInvalidOp, o.n)
		}
		if err := a.checkValue(o.value); err != nil {
			return err
		}
		s, e := d.Span(o.n)
		a.edits = append(a.edits, Edit{Start: s, End: e, New: o.value})

	case removeOp:
		t, err := a.resolve(o.obj)
		if err != nil {
			return err
		}
		if t.obj != nil {
			return fmt.Errorf("%w: RemoveMember on an object created in this Apply", ErrInvalidOp)
		}
		if err := a.realKind(t.real, KindObject); err != nil {
			return err
		}
		idx := d.nodes[t.real].memberIndex(o.key)
		if idx < 0 {
			return fmt.Errorf("%w: %q", ErrNoMember, o.key)
		}
		oe := a.obj(t.real)
		if oe.removed[idx] {
			return fmt.Errorf("%w: member %q removed twice", ErrOverlap, o.key)
		}
		oe.removed[idx] = true

	case insertOp:
		if !utf8.ValidString(o.key) {
			return fmt.Errorf("%w: member name: %w", ErrInvalidOp, ErrInvalidUTF8)
		}
		if err := a.checkValue(o.value); err != nil {
			return err
		}
		t, err := a.resolve(o.obj)
		if err != nil {
			return err
		}
		m := newMember{key: o.key, value: o.value}
		if t.obj != nil {
			if t.obj.find(o.key) >= 0 {
				return a.dupErr(o.obj, o.key)
			}
			t.obj.members = append(t.obj.members, m)
			return nil
		}
		if err := a.realKind(t.real, KindObject); err != nil {
			return err
		}
		oe := a.obj(t.real)
		if oe.inserts.find(o.key) >= 0 {
			return a.dupErr(t.real, o.key)
		}
		oe.inserts.members = append(oe.inserts.members, m)

	case ensureOp:
		if !utf8.ValidString(o.key) {
			return fmt.Errorf("%w: member name: %w", ErrInvalidOp, ErrInvalidUTF8)
		}
		t, err := a.resolve(o.obj)
		if err != nil {
			return err
		}
		parent := t.obj
		if parent == nil {
			if err := a.realKind(t.real, KindObject); err != nil {
				return err
			}
			if idx := d.nodes[t.real].memberIndex(o.key); idx >= 0 {
				v := d.nodes[t.real].members[idx].value
				if err := a.realKind(v, KindObject); err != nil {
					return fmt.Errorf("ensure %q: %w", o.key, err)
				}
				a.obj(t.real).resolved[idx] = true
				a.claims[v] = true
				a.handles[o.h] = target{real: v}
				return nil
			}
			parent = &a.obj(t.real).inserts
		}
		if i := parent.find(o.key); i >= 0 {
			if parent.members[i].obj == nil {
				return a.dupErr(o.obj, o.key)
			}
			a.handles[o.h] = target{obj: parent.members[i].obj}
			return nil
		}
		c := &created{}
		parent.members = append(parent.members, newMember{key: o.key, obj: c})
		a.handles[o.h] = target{obj: c}

	case appendOp:
		t, err := a.resolve(o.arr)
		if err != nil {
			return err
		}
		if t.obj != nil {
			return fmt.Errorf("%w: AppendElement on an object", ErrInvalidOp)
		}
		if err := a.realKind(t.real, KindArray); err != nil {
			return err
		}
		if err := a.checkValue(o.value); err != nil {
			return err
		}
		a.arrs[t.real] = append(a.arrs[t.real], o.value)

	default:
		return fmt.Errorf("%w: unknown operation %T", ErrInvalidOp, op)
	}
	return nil
}

// leadingSpace returns the whitespace run that ends at off.
func (d *Doc) leadingSpace(off int) []byte {
	i := off
	for i > 0 && isSpace(d.src[i-1]) {
		i--
	}
	return d.src[i:off]
}

func hasBreak(b []byte) bool {
	for _, c := range b {
		if c == '\n' || c == '\r' {
			return true
		}
	}
	return false
}

// build turns the collected member and element operations into edits.
func (a *applier) build() error {
	d := a.d
	objIDs := make([]NodeID, 0, len(a.objs))
	for id := range a.objs {
		objIDs = append(objIDs, id)
	}
	slices.Sort(objIDs)
	for _, id := range objIDs {
		oe := a.objs[id]
		o := &d.nodes[id]
		for idx := range oe.resolved {
			if oe.removed[idx] {
				return fmt.Errorf("%w: member %q is both ensured and removed", ErrOverlap, o.members[idx].name)
			}
		}
		for _, m := range oe.inserts.members {
			if idx := o.memberIndex(m.key); idx >= 0 && !oe.removed[idx] {
				return a.dupErr(id, m.key)
			}
		}
		a.buildObject(o, oe)
	}

	arrIDs := make([]NodeID, 0, len(a.arrs))
	for id := range a.arrs {
		arrIDs = append(arrIDs, id)
	}
	slices.Sort(arrIDs)
	for _, id := range arrIDs {
		vals := a.arrs[id]
		arr := &d.nodes[id]
		var b []byte
		if len(arr.elems) == 0 {
			for i, v := range vals {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, v...)
			}
			a.edits = append(a.edits, Edit{Start: arr.end - 1, End: arr.end - 1, New: b})
			continue
		}
		last := &d.nodes[arr.elems[len(arr.elems)-1]]
		lead := d.leadingSpace(last.start)
		multi := hasBreak(lead)
		for _, v := range vals {
			b = append(b, ',')
			if multi {
				b = append(b, lead...)
			}
			b = append(b, v...)
		}
		a.edits = append(a.edits, Edit{Start: last.end, End: last.end, New: b})
	}
	return nil
}

func (a *applier) buildObject(o *node, oe *objEdits) {
	d := a.d
	ins := oe.inserts.members
	n := len(o.members)
	if n == 0 {
		if len(ins) > 0 {
			var b []byte
			for i, m := range ins {
				if i > 0 {
					b = append(b, ',')
				}
				b = appendMember(b, m, nil, nil)
			}
			a.edits = append(a.edits, Edit{Start: o.end - 1, End: o.end - 1, New: b})
		}
		return
	}
	valueEnd := func(i int) int { return d.nodes[o.members[i].value].end }
	lastM := o.members[n-1]
	lead := d.leadingSpace(lastM.keyStart)
	multi := hasBreak(lead)
	var sep1, sep2 []byte
	if multi {
		sep1 = d.src[lastM.keyEnd:lastM.colon]
		sep2 = d.src[lastM.colon+1 : d.nodes[lastM.value].start]
	}

	if len(oe.removed) == n {
		var b []byte
		if len(ins) > 0 {
			for i, m := range ins {
				if i > 0 {
					b = append(b, ',')
				}
				if multi {
					b = append(b, lead...)
				}
				b = appendMember(b, m, sep1, sep2)
			}
			if multi {
				b = append(b, d.src[valueEnd(n-1):o.end-1]...)
			}
		}
		a.edits = append(a.edits, Edit{Start: o.start + 1, End: o.end - 1, New: b})
		return
	}

	tail := -1
	for i := 0; i < n; {
		if !oe.removed[i] {
			i++
			continue
		}
		j := i
		for j+1 < n && oe.removed[j+1] {
			j++
		}
		if j == n-1 {
			tail = i
			break
		}
		a.edits = append(a.edits, Edit{Start: o.members[i].keyStart, End: o.members[j+1].keyStart, New: []byte{}})
		i = j + 1
	}
	var b []byte
	for _, m := range ins {
		b = append(b, ',')
		if multi {
			b = append(b, lead...)
		}
		b = appendMember(b, m, sep1, sep2)
	}
	switch {
	case tail >= 0:
		if b == nil {
			b = []byte{}
		}
		a.edits = append(a.edits, Edit{Start: valueEnd(tail - 1), End: valueEnd(n - 1), New: b})
	case len(ins) > 0:
		a.edits = append(a.edits, Edit{Start: valueEnd(n - 1), End: valueEnd(n - 1), New: b})
	}
}

// appendMember writes `"key"<sep1>:<sep2>value`; created objects are
// written compact.
func appendMember(b []byte, m newMember, sep1, sep2 []byte) []byte {
	b = appendQuoted(b, m.key)
	b = append(b, sep1...)
	b = append(b, ':')
	b = append(b, sep2...)
	if m.obj == nil {
		return append(b, m.value...)
	}
	b = append(b, '{')
	for i, c := range m.obj.members {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendMember(b, c, nil, nil)
	}
	return append(b, '}')
}

const hexDigits = "0123456789abcdef"

// appendQuoted writes s (valid UTF-8) as a JSON string token, escaping only
// what JSON requires.
func appendQuoted(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			if c < 0x20 {
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			} else {
				b = append(b, c)
			}
		}
	}
	return append(b, '"')
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte{}, b...)
}

// OpKind names the kind of an operation.
type OpKind uint8

const (
	OpReplace OpKind = iota + 1
	OpRemoveMember
	OpInsertMember
	OpEnsureObjectMember
	OpAppendElement
)

func (k OpKind) String() string {
	switch k {
	case OpReplace:
		return "replace"
	case OpRemoveMember:
		return "remove-member"
	case OpInsertMember:
		return "insert-member"
	case OpEnsureObjectMember:
		return "ensure-object-member"
	case OpAppendElement:
		return "append-element"
	}
	return "invalid"
}

// OpInfo is a read-only description of an operation: its kind, the node (or
// Handle, as a negative NodeID) it addresses, the member name for member
// operations, the value bytes for Replace, InsertMember and AppendElement,
// and the Handle an EnsureObjectMember makes. It lets a caller recompute the
// expected result of an Apply independently of this package's output.
type OpInfo struct {
	Kind   OpKind
	Target NodeID
	Key    string
	Value  []byte
	Handle Handle
}

// Describe returns op's description; the value bytes are a copy. An
// operation not made by this package's constructors (including nil) yields
// the zero OpInfo.
func Describe(op Op) OpInfo {
	switch o := op.(type) {
	case replaceOp:
		return OpInfo{Kind: OpReplace, Target: o.n, Value: clone(o.value)}
	case removeOp:
		return OpInfo{Kind: OpRemoveMember, Target: o.obj, Key: o.key}
	case insertOp:
		return OpInfo{Kind: OpInsertMember, Target: o.obj, Key: o.key, Value: clone(o.value)}
	case ensureOp:
		return OpInfo{Kind: OpEnsureObjectMember, Target: o.obj, Key: o.key, Handle: o.h}
	case appendOp:
		return OpInfo{Kind: OpAppendElement, Target: o.arr, Value: clone(o.value)}
	}
	return OpInfo{}
}
