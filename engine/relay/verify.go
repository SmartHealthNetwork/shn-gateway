package relay

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/SmartHealthNetwork/shn-sdk/signedcontent"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

var (
	// ErrVerifyMismatch: an edit's output did not match what its declared
	// operations allow. This is a fault in the gateway; nothing is sent.
	ErrVerifyMismatch = errors.New("relay: edited output failed independent verification")
	// ErrSignedContent: an edit would change content covered by an
	// in-payload signature. Returned as a *SignedContentError.
	ErrSignedContent = errors.New("relay: signed content cannot be edited")
)

// SignedContentError names the edit that was refused and the signature
// that covers the content it would have changed: "Bundle.signature",
// "Provenance.signature", "Signature in <resource type>" for a Signature
// element inside a resource, or "Signature" for one outside any resource.
type SignedContentError struct {
	Edit    EditID
	Carrier string
}

func (e *SignedContentError) Error() string {
	return fmt.Sprintf("signed content cannot be edited (%s, %s)", e.Edit, e.Carrier)
}

// Is reports whether target is ErrSignedContent.
func (e *SignedContentError) Is(target error) bool { return target == ErrSignedContent }

func mismatch(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrVerifyMismatch, fmt.Sprintf(format, args...))
}

// parents maps every value to its enclosing container (-1 for the root).
func parents(d *splice.Doc) []splice.NodeID {
	p := make([]splice.NodeID, d.Len())
	if len(p) > 0 {
		p[0] = -1
	}
	for i := 0; i < d.Len(); i++ {
		n := splice.NodeID(i)
		switch d.Kind(n) {
		case splice.KindObject:
			for _, m := range d.Members(n) {
				p[m.Value] = n
			}
		case splice.KindArray:
			for _, el := range d.Elems(n) {
				p[el] = n
			}
		}
	}
	return p
}

// newMember is a member the operations write: value bytes, or an object
// created in the same change set.
type newMember struct {
	key   string
	value []byte
	obj   *createdObject
}

type createdObject struct {
	members []newMember
	home    splice.NodeID // the existing object the creation is written into
}

func (c *createdObject) find(key string) int {
	for i := range c.members {
		if c.members[i].key == key {
			return i
		}
	}
	return -1
}

type opTarget struct {
	real splice.NodeID
	obj  *createdObject
}

func (t opTarget) home() splice.NodeID {
	if t.obj != nil {
		return t.obj.home
	}
	return t.real
}

// effect is what one operation changes: a byte range of the original, or
// (inside) an insertion somewhere within the container [start, end).
type effect struct {
	start, end int
	inside     bool
	// replaced is the value a replace changes, for the signed-content
	// exemption (signedExempt); -1 for every other operation.
	replaced splice.NodeID
	// inserted says the operation inserted a member into the document's own
	// object insertedIn (not into an object the change creates), named
	// insertedKey, with insertedValue, for the signed-content exemption.
	inserted      bool
	insertedIn    splice.NodeID
	insertedKey   string
	insertedValue []byte
	// removed says the operation removed the member removedKey of the
	// document's own object removedIn, for the signed-content exemption.
	removed    bool
	removedIn  splice.NodeID
	removedKey string
}

// model is the expected result of a change set, derived from the
// operations' declared meaning and the original document alone.
type model struct {
	doc      *splice.Doc
	src      []byte
	parent   []splice.NodeID
	replaced map[splice.NodeID][]byte
	removed  map[splice.NodeID]map[string]bool
	inserts  map[splice.NodeID]*createdObject // members appended to existing objects
	appended map[splice.NodeID][][]byte
	handles  map[splice.Handle]opTarget
	touched  map[splice.NodeID]bool
	// removedSpans are the removed members (name through value) and
	// replacedSpans the replaced values, in original offsets.
	removedSpans  []span
	replacedSpans []span
	effects       []*effect // one per operation; nil when it changes nothing
	// restored is E-20's restored channel.endpoints by their original
	// string (restoredEndpoints), built once, on first need.
	restored map[string]string
	// restoreIn is E-20's inputs, the search the subscriber's gateway
	// framed (Change.Restore), or nil.
	restoreIn *SubscriptionRestoreInputs
	// rootLists is a root Bundle's array members by folded name
	// (rootBundleLists), built once, on first need.
	rootLists map[string][]splice.NodeID
	// statusLists, eventParts and entryLinks memoize E-17's structural
	// reads of a notification's SubscriptionStatus and of its entries' link
	// arrays (statusParams, eventFocusParts, entryLink) by the array they
	// read, so each is decided once whatever many edits share it.
	statusLists map[splice.NodeID]bool
	eventParts  map[splice.NodeID]bool
	entryLinks  map[splice.NodeID]bool
}

func buildModel(doc *splice.Doc, src []byte, ops []splice.Op) (*model, error) {
	m := &model{
		doc:      doc,
		src:      src,
		parent:   parents(doc),
		replaced: map[splice.NodeID][]byte{},
		removed:  map[splice.NodeID]map[string]bool{},
		inserts:  map[splice.NodeID]*createdObject{},
		appended: map[splice.NodeID][][]byte{},
		handles:  map[splice.Handle]opTarget{},
		touched:  map[splice.NodeID]bool{},
	}
	for i, op := range ops {
		eff, err := m.add(splice.Describe(op))
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", i, err)
		}
		m.effects = append(m.effects, eff)
	}
	return m, nil
}

func (m *model) touch(n splice.NodeID) {
	for n >= 0 && !m.touched[n] {
		m.touched[n] = true
		n = m.parent[n]
	}
}

// inside is the effect of adding content somewhere within container n.
func (m *model) inside(n splice.NodeID) *effect {
	cs, ce := m.doc.Span(n)
	return &effect{start: cs, end: ce, inside: true, replaced: -1}
}

func (m *model) resolve(n splice.NodeID) (opTarget, error) {
	if n >= 0 {
		if m.doc.Kind(n) == 0 {
			return opTarget{}, fmt.Errorf("node %d does not exist", n)
		}
		return opTarget{real: n}, nil
	}
	t, ok := m.handles[splice.Handle(n)]
	if !ok {
		return opTarget{}, fmt.Errorf("unknown handle %d", n)
	}
	return t, nil
}

func (m *model) realObject(n splice.NodeID) error {
	if k := m.doc.Kind(n); k != splice.KindObject {
		return fmt.Errorf("node %d is %v, not an object", n, k)
	}
	return nil
}

func (m *model) existing(obj splice.NodeID) *createdObject {
	c := m.inserts[obj]
	if c == nil {
		c = &createdObject{home: obj}
		m.inserts[obj] = c
	}
	return c
}

func (m *model) add(op splice.OpInfo) (*effect, error) {
	switch op.Kind {
	case splice.OpReplace:
		n := op.Target
		if n < 0 || m.doc.Kind(n) == 0 {
			return nil, fmt.Errorf("replace target %d is not a value", n)
		}
		if _, dup := m.replaced[n]; dup {
			return nil, fmt.Errorf("value %d replaced twice", n)
		}
		m.replaced[n] = op.Value
		m.touch(m.parent[n])
		s, e := m.doc.Span(n)
		m.replacedSpans = append(m.replacedSpans, span{s, e})
		if bytes.Equal(op.Value, m.src[s:e]) {
			return nil, nil
		}
		return &effect{start: s, end: e, replaced: n}, nil

	case splice.OpRemoveMember:
		t, err := m.resolve(op.Target)
		if err != nil {
			return nil, err
		}
		if t.obj != nil {
			return nil, errors.New("remove from a created object")
		}
		if err := m.realObject(t.real); err != nil {
			return nil, err
		}
		for _, mem := range m.doc.Members(t.real) {
			if mem.Name != op.Key {
				continue
			}
			if m.removed[t.real] == nil {
				m.removed[t.real] = map[string]bool{}
			}
			m.removed[t.real][op.Key] = true
			m.touch(t.real)
			_, ve := m.doc.Span(mem.Value)
			m.removedSpans = append(m.removedSpans, span{mem.KeyStart, ve})
			return &effect{start: mem.KeyStart, end: ve, replaced: -1, removed: true, removedIn: t.real, removedKey: op.Key}, nil
		}
		return nil, fmt.Errorf("no member %q to remove", op.Key)

	case splice.OpInsertMember:
		t, err := m.resolve(op.Target)
		if err != nil {
			return nil, err
		}
		dst := t.obj
		if dst == nil {
			if err := m.realObject(t.real); err != nil {
				return nil, err
			}
			dst = m.existing(t.real)
		}
		if dst.find(op.Key) >= 0 {
			return nil, fmt.Errorf("member %q inserted twice", op.Key)
		}
		dst.members = append(dst.members, newMember{key: op.Key, value: op.Value})
		m.touch(t.home())
		eff := m.inside(t.home())
		if t.obj == nil {
			eff.inserted, eff.insertedIn, eff.insertedKey, eff.insertedValue = true, t.real, op.Key, op.Value
		}
		return eff, nil

	case splice.OpEnsureObjectMember:
		t, err := m.resolve(op.Target)
		if err != nil {
			return nil, err
		}
		parent := t.obj
		if parent == nil {
			if err := m.realObject(t.real); err != nil {
				return nil, err
			}
			if v, ok := m.doc.Member(t.real, op.Key); ok {
				if err := m.realObject(v); err != nil {
					return nil, err
				}
				m.handles[op.Handle] = opTarget{real: v}
				m.touch(v)
				return nil, nil
			}
			parent = m.existing(t.real)
		}
		if i := parent.find(op.Key); i >= 0 {
			if parent.members[i].obj == nil {
				return nil, fmt.Errorf("member %q is not a created object", op.Key)
			}
			m.handles[op.Handle] = opTarget{obj: parent.members[i].obj}
			return nil, nil
		}
		c := &createdObject{home: t.home()}
		parent.members = append(parent.members, newMember{key: op.Key, obj: c})
		m.handles[op.Handle] = opTarget{obj: c}
		m.touch(t.home())
		return m.inside(t.home()), nil

	case splice.OpAppendElement:
		n := op.Target
		if n < 0 || m.doc.Kind(n) != splice.KindArray {
			return nil, fmt.Errorf("append target %d is not an array", n)
		}
		m.appended[n] = append(m.appended[n], op.Value)
		m.touch(n)
		return m.inside(n), nil
	}
	return nil, errors.New("unknown operation")
}

// span is a half-open byte range.
type span struct{ start, end int }

// overlaps reports whether r shares a byte with [s, e), or, for an empty
// [s, s), whether s lies strictly inside r.
func (r span) overlaps(s, e int) bool {
	if s == e {
		return r.start < s && s < r.end
	}
	return s < r.end && r.start < e
}

// outAnchors are the ranges of the output that hold declared content, found
// by compare: inserted members (name through value), appended elements and
// replaced values.
type outAnchors struct {
	added    []span
	replaced []span
}

// check verifies a splice result against the model, independently of the
// splice, with three checks that each stand on their own:
//
//  1. checkRebuild: the output is the original with only the reported spans
//     substituted;
//  2. checkSpans: every reported span is either exactly a replaced value, or
//     holds at least one declared removal or addition and otherwise only
//     layout bytes (whitespace and commas), so no span can touch a byte
//     that belongs to a value or name no operation addresses;
//  3. compare: a fresh scan of the output holds exactly the declared values
//     at the declared places, every surviving member keeps its name token
//     byte for byte, and every value no operation addresses is unchanged.
//
// What is not pinned: the layout bytes inside a span that holds a declared
// removal or addition, i.e. the whitespace and the one comma between a
// removed or added member (or element) and its neighbours. Those bytes are
// the edit's own seam; every other byte of the output is pinned.
func (m *model) check(out []byte, spans []splice.Edit) error {
	if err := m.checkRebuild(out, spans); err != nil {
		return err
	}
	od, err := splice.Scan(out, splice.Limits{})
	if err != nil {
		return mismatch("output does not scan: %v", err)
	}
	var anchors outAnchors
	if err := m.compare(od, out, m.doc.Root(), od.Root(), "$", &anchors); err != nil {
		return err
	}
	return m.checkSpans(out, spans, &anchors)
}

func (m *model) checkRebuild(out []byte, spans []splice.Edit) error {
	src := m.src
	rebuilt := make([]byte, 0, len(out))
	pos := 0
	for _, e := range spans {
		if e.Start < pos || e.End < e.Start || e.End > len(src) {
			return mismatch("span [%d,%d) out of order or range", e.Start, e.End)
		}
		rebuilt = append(rebuilt, src[pos:e.Start]...)
		rebuilt = append(rebuilt, e.New...)
		pos = e.End
	}
	rebuilt = append(rebuilt, src[pos:]...)
	if !bytes.Equal(rebuilt, out) {
		return mismatch("output differs from the original outside the reported spans")
	}
	return nil
}

func isLayout(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' }

// layoutOutside reports whether every byte of b[s:e] outside the given
// ranges (each within [s, e)) is a layout byte.
func layoutOutside(b []byte, s, e int, covered []span) bool {
	for i := s; i < e; i++ {
		in := false
		for _, r := range covered {
			if r.start <= i && i < r.end {
				in = true
				break
			}
		}
		if !in && !isLayout(b[i]) {
			return false
		}
	}
	return true
}

// checkSpans checks each reported span (check's second check) in time near
// linear in the spans and the declared content: the model's replaced and
// removed spans and the output's anchors are each sorted once (spanIndex),
// and each span finds the ones it meets by binary search, so a change of
// many edits, which a peer's message can set the number of, costs no more
// than the edits it makes. When a list holds an empty span, or spans that
// overlap each other, it falls back to comparing each span with every one
// (checkSpanEach); both reach the same verdict on the same input.
func (m *model) checkSpans(out []byte, spans []splice.Edit, a *outAnchors) error {
	ix, indexed := newSpanIndexes(m, a)
	delta := 0
	for _, e := range spans {
		os := e.Start + delta
		oe := os + len(e.New)
		delta += len(e.New) - (e.End - e.Start)
		var err error
		if indexed {
			err = m.checkSpanIndexed(e, out, os, oe, ix)
		} else {
			err = m.checkSpanEach(e, out, os, oe, a)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// spanIndex is a span list sorted by start, each span with its place in the
// list it came from. It is built only when every span is non-empty and none
// overlaps another: then starts and ends both rise, and the spans any range
// overlaps are one run of it.
type spanIndex struct {
	spans []span
	order []int
}

// newSpanIndex sorts list, and reports false when a span in it is empty or
// overlaps another.
func newSpanIndex(list []span) (spanIndex, bool) {
	order := make([]int, len(list))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(i, j int) int { return cmp.Compare(list[i].start, list[j].start) })
	ix := spanIndex{spans: make([]span, len(list)), order: order}
	for k, i := range order {
		r := list[i]
		if r.start >= r.end || (k > 0 && r.start < ix.spans[k-1].end) {
			return spanIndex{}, false
		}
		ix.spans[k] = r
	}
	return ix, true
}

// overlapping is the run [lo, hi) of the spans r with r.overlaps(s, e): those
// ending after s and starting before e (for an empty [s, s), strictly before
// s, which is the same bound).
func (ix spanIndex) overlapping(s, e int) (lo, hi int) {
	lo = sort.Search(len(ix.spans), func(k int) bool { return ix.spans[k].end > s })
	hi = sort.Search(len(ix.spans), func(k int) bool { return ix.spans[k].start >= e })
	return lo, max(lo, hi)
}

// has reports whether the list holds exactly [s, e).
func (ix spanIndex) has(s, e int) bool {
	k := sort.Search(len(ix.spans), func(k int) bool { return ix.spans[k].start >= s })
	return k < len(ix.spans) && ix.spans[k] == span{s, e}
}

// spanIndexes are checkSpanEach's four lists, indexed.
type spanIndexes struct {
	replaced, removed, outReplaced, outAdded spanIndex
}

func newSpanIndexes(m *model, a *outAnchors) (spanIndexes, bool) {
	var ix spanIndexes
	for _, l := range []struct {
		to   *spanIndex
		from []span
	}{{&ix.replaced, m.replacedSpans}, {&ix.removed, m.removedSpans}, {&ix.outReplaced, a.replaced}, {&ix.outAdded, a.added}} {
		var ok bool
		if *l.to, ok = newSpanIndex(l.from); !ok {
			return spanIndexes{}, false
		}
	}
	return ix, true
}

// layoutOutsideSorted is layoutOutside for covered sorted by start, none
// overlapping another: one pass over b[s:e].
func layoutOutsideSorted(b []byte, s, e int, covered []span) bool {
	i := s
	for _, r := range covered {
		for ; i < r.start; i++ {
			if !isLayout(b[i]) {
				return false
			}
		}
		i = max(i, r.end)
	}
	for ; i < e; i++ {
		if !isLayout(b[i]) {
			return false
		}
	}
	return true
}

// checkSpanIndexed is checkSpanEach over indexed lists: the same checks, in
// the same order, with the same refusals. Of the replaced values a span
// meets, the one the model lists first decides, as checkSpanEach's loop
// finds it.
func (m *model) checkSpanIndexed(e splice.Edit, out []byte, os, oe int, ix spanIndexes) error {
	lo, hi := ix.replaced.overlapping(e.Start, e.End)
	if lo < hi {
		first := ix.replaced.order[lo]
		for k := lo + 1; k < hi; k++ {
			first = min(first, ix.replaced.order[k])
		}
		r := m.replacedSpans[first]
		if r.start != e.Start || r.end != e.End {
			return mismatch("span [%d,%d) is not exactly the replaced value [%d,%d)", e.Start, e.End, r.start, r.end)
		}
		if ix.outReplaced.has(os, oe) {
			return nil
		}
		return mismatch("span [%d,%d) does not write exactly the replaced value", e.Start, e.End)
	}
	lo, hi = ix.removed.overlapping(e.Start, e.End)
	for _, r := range ix.removed.spans[lo:hi] {
		if e.Start > r.start || r.end > e.End {
			return mismatch("span [%d,%d) cuts through a removed member", e.Start, e.End)
		}
	}
	anchored := lo < hi
	if !layoutOutsideSorted(m.src, e.Start, e.End, ix.removed.spans[lo:hi]) {
		return mismatch("span [%d,%d) removes bytes no operation removes", e.Start, e.End)
	}
	if lo, hi := ix.outReplaced.overlapping(os, oe); lo < hi {
		return mismatch("span [%d,%d) writes into a replaced value", e.Start, e.End)
	}
	lo, hi = ix.outAdded.overlapping(os, oe)
	for _, r := range ix.outAdded.spans[lo:hi] {
		if os > r.start || r.end > oe {
			return mismatch("span [%d,%d) cuts through an added value", e.Start, e.End)
		}
	}
	anchored = anchored || lo < hi
	if !layoutOutsideSorted(out, os, oe, ix.outAdded.spans[lo:hi]) {
		return mismatch("span [%d,%d) writes bytes no operation adds", e.Start, e.End)
	}
	if !anchored {
		return mismatch("span [%d,%d) holds no declared removal or addition", e.Start, e.End)
	}
	return nil
}

// checkSpanEach checks one reported span against every replaced, removed and
// anchored span in turn: checkSpans's fallback for lists it cannot index,
// and the reference its indexed check is held to.
func (m *model) checkSpanEach(e splice.Edit, out []byte, os, oe int, a *outAnchors) error {
	for _, r := range m.replacedSpans {
		if !r.overlaps(e.Start, e.End) && !(r.start == e.Start && r.end == e.End) {
			continue
		}
		if r.start != e.Start || r.end != e.End {
			return mismatch("span [%d,%d) is not exactly the replaced value [%d,%d)", e.Start, e.End, r.start, r.end)
		}
		for _, o := range a.replaced {
			if o.start == os && o.end == oe {
				return nil
			}
		}
		return mismatch("span [%d,%d) does not write exactly the replaced value", e.Start, e.End)
	}
	anchored := false
	var in []span
	for _, r := range m.removedSpans {
		switch {
		case e.Start <= r.start && r.end <= e.End:
			in = append(in, r)
			anchored = true
		case r.overlaps(e.Start, e.End):
			return mismatch("span [%d,%d) cuts through a removed member", e.Start, e.End)
		}
	}
	if !layoutOutside(m.src, e.Start, e.End, in) {
		return mismatch("span [%d,%d) removes bytes no operation removes", e.Start, e.End)
	}
	in = in[:0]
	for _, r := range a.replaced {
		if r.overlaps(os, oe) {
			return mismatch("span [%d,%d) writes into a replaced value", e.Start, e.End)
		}
	}
	for _, r := range a.added {
		switch {
		case os <= r.start && r.end <= oe:
			in = append(in, r)
			anchored = true
		case r.overlaps(os, oe):
			return mismatch("span [%d,%d) cuts through an added value", e.Start, e.End)
		}
	}
	if !layoutOutside(out, os, oe, in) {
		return mismatch("span [%d,%d) writes bytes no operation adds", e.Start, e.End)
	}
	if !anchored {
		return mismatch("span [%d,%d) holds no declared removal or addition", e.Start, e.End)
	}
	return nil
}

func valueBytes(d *splice.Doc, b []byte, n splice.NodeID) []byte {
	s, e := d.Span(n)
	return b[s:e]
}

func (m *model) compare(od *splice.Doc, out []byte, o, n splice.NodeID, path string, a *outAnchors) error {
	got := valueBytes(od, out, n)
	if v, ok := m.replaced[o]; ok {
		if !bytes.Equal(got, v) {
			return mismatch("%s does not hold the declared value", path)
		}
		s, e := od.Span(n)
		a.replaced = append(a.replaced, span{s, e})
		return nil
	}
	if !m.touched[o] {
		if !bytes.Equal(got, valueBytes(m.doc, m.src, o)) {
			return mismatch("%s changed but no operation addresses it", path)
		}
		return nil
	}
	if od.Kind(n) != m.doc.Kind(o) {
		return mismatch("%s changed kind", path)
	}
	switch m.doc.Kind(o) {
	case splice.KindObject:
		type want struct {
			orig splice.MemberRef
			nm   *newMember
		}
		var wants []want
		for _, mem := range m.doc.Members(o) {
			if !m.removed[o][mem.Name] {
				wants = append(wants, want{orig: mem})
			}
		}
		if c := m.inserts[o]; c != nil {
			for i := range c.members {
				wants = append(wants, want{nm: &c.members[i]})
			}
		}
		have := od.Members(n)
		if len(have) != len(wants) {
			return mismatch("%s has %d members, want %d", path, len(have), len(wants))
		}
		for i, w := range wants {
			if w.nm == nil {
				if !bytes.Equal(out[have[i].KeyStart:have[i].KeyEnd], m.src[w.orig.KeyStart:w.orig.KeyEnd]) {
					return mismatch("%s member %d name token is not %q byte for byte", path, i, w.orig.Name)
				}
				if err := m.compare(od, out, w.orig.Value, have[i].Value, path+"."+w.orig.Name, a); err != nil {
					return err
				}
				continue
			}
			if have[i].Name != w.nm.key {
				return mismatch("%s member %d is %q, want %q", path, i, have[i].Name, w.nm.key)
			}
			if err := compareNew(od, out, *w.nm, have[i].Value, path+"."+w.nm.key); err != nil {
				return err
			}
			_, ve := od.Span(have[i].Value)
			a.added = append(a.added, span{have[i].KeyStart, ve})
		}
		return nil
	case splice.KindArray:
		orig, app := m.doc.Elems(o), m.appended[o]
		have := od.Elems(n)
		if len(have) != len(orig)+len(app) {
			return mismatch("%s has %d elements, want %d", path, len(have), len(orig)+len(app))
		}
		for i, el := range orig {
			if err := m.compare(od, out, el, have[i], fmt.Sprintf("%s[%d]", path, i), a); err != nil {
				return err
			}
		}
		for i, v := range app {
			el := have[len(orig)+i]
			if !bytes.Equal(valueBytes(od, out, el), v) {
				return mismatch("%s[%d] does not hold the appended value", path, len(orig)+i)
			}
			s, e := od.Span(el)
			a.added = append(a.added, span{s, e})
		}
		return nil
	}
	return mismatch("%s is a scalar marked as edited", path)
}

func compareNew(od *splice.Doc, out []byte, nm newMember, n splice.NodeID, path string) error {
	if nm.obj == nil {
		if !bytes.Equal(valueBytes(od, out, n), nm.value) {
			return mismatch("%s does not hold the inserted value", path)
		}
		return nil
	}
	if od.Kind(n) != splice.KindObject {
		return mismatch("%s is not the created object", path)
	}
	have := od.Members(n)
	if len(have) != len(nm.obj.members) {
		return mismatch("%s has %d members, want %d", path, len(have), len(nm.obj.members))
	}
	for i, c := range nm.obj.members {
		if have[i].Name != c.key {
			return mismatch("%s member %d is %q, want %q", path, i, have[i].Name, c.key)
		}
		if err := compareNew(od, out, c, have[i].Value, path+"."+c.key); err != nil {
			return err
		}
	}
	return nil
}

// Signature coverage.

type coveredRegion struct {
	start, end int
	carrier    string
}

// coverageMap is what a document's signatures cover: its regions in the
// order coverage found them, each span with each carrier once, indexed so an
// effect finds the regions it touches without reading the others. A region
// is a value's span, so any two either nest or lie apart: sorted by start,
// the outer first, each region's nearest enclosing one (up) makes the
// regions containing a point one chain, no longer than the document's
// nesting, and the regions starting inside a range one run.
type coverageMap struct {
	regions []coveredRegion
	sorted  []int // region indices by start, the outer first
	up      []int // for each sorted position, the nearest enclosing region's position, or -1
	// nested says every two regions nest or lie apart; when they do not,
	// hits reads every region in turn.
	nested bool
}

func newCoverageMap(regions []coveredRegion) coverageMap {
	c := coverageMap{regions: regions, sorted: make([]int, len(regions)), up: make([]int, len(regions)), nested: true}
	for i := range c.sorted {
		c.sorted[i] = i
	}
	slices.SortFunc(c.sorted, func(i, j int) int {
		a, b := regions[i], regions[j]
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(b.end, a.end), cmp.Compare(i, j))
	})
	var stack []int
	for k, i := range c.sorted {
		r := regions[i]
		for len(stack) > 0 && regions[c.sorted[stack[len(stack)-1]]].end <= r.start {
			stack = stack[:len(stack)-1]
		}
		c.up[k] = -1
		if len(stack) > 0 {
			top := stack[len(stack)-1]
			if regions[c.sorted[top]].end < r.end {
				c.nested = false
			}
			c.up[k] = top
		}
		stack = append(stack, k)
	}
	return c
}

// hits reports the carriers of every covered region an effect touches, in
// coverage order (by the first region of each it touches), each once. A byte
// range touches a region when they overlap: the regions containing its
// start, which begin before it, and those beginning inside it. An insertion
// inside a container touches a region that contains the whole container:
// one of those containing its start.
func (c coverageMap) hits(e effect) []string {
	// An empty insertion range (no container has one) is contained by a
	// region ending where it stands, which the chain has left behind: it,
	// and regions that do not nest, are read in turn.
	if !c.nested || (e.inside && e.start >= e.end) {
		return c.hitsEach(e)
	}
	start := func(k int) int { return c.regions[c.sorted[k]].start }
	var touched []int
	if e.inside {
		for k := sort.Search(len(c.sorted), func(k int) bool { return start(k) > e.start }) - 1; k >= 0; k = c.up[k] {
			if r := c.regions[c.sorted[k]]; r.start <= e.start && e.end <= r.end {
				touched = append(touched, c.sorted[k])
			}
		}
	} else {
		for k := sort.Search(len(c.sorted), func(k int) bool { return start(k) >= e.start }) - 1; k >= 0; k = c.up[k] {
			if c.regions[c.sorted[k]].end > e.start {
				touched = append(touched, c.sorted[k])
			}
		}
		lo := sort.Search(len(c.sorted), func(k int) bool { return start(k) >= e.start })
		hi := sort.Search(len(c.sorted), func(k int) bool { return start(k) >= e.end })
		touched = append(touched, c.sorted[lo:max(lo, hi)]...)
	}
	slices.Sort(touched)
	var out []string
	seen := map[string]bool{}
	for _, i := range touched {
		if carrier := c.regions[i].carrier; !seen[carrier] {
			seen[carrier] = true
			out = append(out, carrier)
		}
	}
	return out
}

// hitsEach is hits read from every region in turn, for regions that do not
// nest and for an empty insertion range.
func (c coverageMap) hitsEach(e effect) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range c.regions {
		touched := e.start < r.end && r.start < e.end
		if e.inside {
			touched = r.start <= e.start && e.end <= r.end
		}
		if touched && !seen[r.carrier] {
			seen[r.carrier] = true
			out = append(out, r.carrier)
		}
	}
	return out
}

// SignedCarrier names the first signature, in coverage order, that covers
// the value n as a whole, or returns "" when none does. Apply refuses any
// change at or inside n by a registered edit not exempt for that change,
// naming the first signature in coverage order among those the change
// touches: this one, unless a signature over a value inside n that the
// change also touches comes first (a replace of n touches every one). ""
// does not free n: a change to a signed value inside it is still refused.
// For n the root, "" says a builder may copy the document and add to it,
// outside every signed value it holds, without changing signed content.
func (d *Document) SignedCarrier(n NodeID) string {
	start, end := d.d.Span(n)
	for _, r := range coverage(d.body.bytes()).regions {
		if r.start <= start && end <= r.end {
			return r.carrier
		}
	}
	return ""
}

// signedExempt are the registered edits made even where an in-payload
// signature covers what they change, each with the places it may change
// there: for most, a replace of the value one place names, with a string
// (stringAt). Any other change an exempt edit makes to signed content is
// refused, as every other edit's is.
var signedExempt = map[EditID]func(m *model, eff effect) bool{
	// D127: the return address of a pended PAS answer's Task is re-keyed
	// to the network even when signed, and the forward discloses it.
	EditPASPendReturnRekey: stringAt((*model).payerURLValue),
	// D127: so is the return address of a payer's attachment-request Task.
	EditCDexTaskReturnRekey: stringAt((*model).payerURLValue),
	// D127 decision 13, applied to the notified decision (the lead,
	// 2026-11-12): so is the return address of a PAS Task in a payer's
	// content notification.
	EditPASNotifiedReturnRekey: stringAt((*model).payerURLValue),
	// D128: the questionnaireAdaptive address is re-keyed even where the
	// payer's signature covers it, and restored even where the provider's
	// does; each place may also take the one form change adaptivePair allows.
	EditDTRAdaptiveAddressRekey:   stringAt((*model).adaptiveAddressValue),
	EditDTRAdaptiveAddressRestore: stringAt((*model).adaptiveAddressValue),
	// D132 (N2): a topic Subscription's channel.endpoint and channel.header
	// are re-keyed to the notifier gateway's notification address and
	// credential even where a signature covers them, and the forward
	// discloses it.
	EditSubscriptionEndpointRekey: (*model).subscriptionChannelChange,
	// D132 (N2): the subscriber's gateway seals a topic Subscription's
	// channel (E-18) and restores it on the answers (E-20) even where a
	// signature covers it, and each is disclosed.
	EditSubscriptionHeaderSeal:      (*model).subscriptionChannelSeal,
	EditSubscriptionEndpointRestore: (*model).subscriptionChannelRestore,
	// The CDex design §15.7, applied by the lead as E-12 is (D128): a CDex
	// Task notification's addresses into the provider's server are re-keyed
	// to the payer gateway's base for that provider even where a signature
	// covers them, and the delivery discloses it.
	EditCDexTaskNotificationAddressRekey: stringAt((*model).notificationAddressValue),
}

// bodyEditChecks are the registered body edits the core holds to a check of
// their own on every change, whether a signature covers it or not: a closed
// map, as signedExempt is. E-20 (subscription-endpoint-restore) changes only
// its places, a link's query only as its restore of the url parameter allows
// (subscriptionLinkURLRestored).
var bodyEditChecks = map[EditID]func(m *model, eff effect) bool{
	EditSubscriptionEndpointRestore: (*model).subscriptionChannelRestore,
}

// stringAt is the exemption of an edit whose one change to signed content is
// a replace, with a JSON string, of the string value place names.
func stringAt(place func(m *model, n splice.NodeID) bool) func(m *model, eff effect) bool {
	return func(m *model, eff effect) bool {
		if eff.replaced < 0 {
			return false
		}
		v := m.replaced[eff.replaced]
		if len(v) < 2 || v[0] != '"' || !json.Valid(v) || m.doc.Kind(eff.replaced) != splice.KindString {
			return false
		}
		return place(m, eff.replaced)
	}
}

// subscriptionChannelChange is E-19's exemption (D132, N2): a change of the
// root Subscription's channel that is a replace, with a JSON string, of its
// endpoint string; a replace, with a JSON array of strings, of its header
// array; the insert of an endpoint member that is a JSON string, into a
// channel that has none (the subscriber's gateway seals the channel, so the
// body may state none); or the insert of a header member that is a JSON
// array of strings, into a channel that has none. Nothing else, decided by structure: the
// channel is the "channel" member of the document's root object, whose
// resourceType is Subscription.
func (m *model) subscriptionChannelChange(eff effect) bool {
	d := m.doc
	channel, ok := d.Member(d.Root(), "channel")
	if !ok || d.Kind(channel) != splice.KindObject || stringMember(d, d.Root(), "resourceType") != "Subscription" {
		return false
	}
	if eff.inserted {
		if eff.insertedIn != channel {
			return false
		}
		_, has := d.Member(channel, eff.insertedKey)
		switch eff.insertedKey {
		case "header":
			return !has && stringArray(eff.insertedValue)
		case "endpoint":
			v := bytes.TrimSpace(eff.insertedValue)
			var str string
			return !has && len(v) >= 2 && v[0] == '"' && json.Unmarshal(v, &str) == nil
		}
		return false
	}
	if eff.replaced < 0 {
		return false
	}
	v := m.replaced[eff.replaced]
	if ep, ok := d.Member(channel, "endpoint"); ok && ep == eff.replaced {
		return d.Kind(ep) == splice.KindString && len(v) >= 2 && v[0] == '"' && json.Valid(v)
	}
	if h, ok := d.Member(channel, "header"); ok && h == eff.replaced {
		return d.Kind(h) == splice.KindArray && stringArray(v)
	}
	return false
}

// stringArray reports whether v is a JSON array whose every element is a
// string.
func stringArray(v []byte) bool {
	var ss []string
	t := bytes.TrimSpace(v)
	return len(t) > 0 && t[0] == '[' && json.Unmarshal(t, &ss) == nil
}

// pasTempCodes is the PAS code system that names a Task's payer-url input.
const pasTempCodes = "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes"

// signedExemption reports whether the change eff of edit id may be made to
// signed content: id is exempt, and eff is a change its exemption allows
// (signedExempt), or an adaptive edit's form change (adaptivePair).
func (m *model) signedExemption(id EditID, eff effect) bool {
	place, ok := signedExempt[id]
	if !ok {
		return false
	}
	if adaptiveEdits[id] && eff.replaced >= 0 && m.adaptivePair(id, eff.replaced, m.replaced[eff.replaced]) {
		return true
	}
	return place(m, eff)
}

// payerURLValue reports whether n is the valueUrl of a Task input whose type
// is PASTempCodes#payer-url, decided by structure: n is the valueUrl member
// of an element of the input array of an object whose resourceType is Task.
func (m *model) payerURLValue(n splice.NodeID) bool {
	d := m.doc
	in := m.parent[n]
	if in < 0 || d.Kind(in) != splice.KindObject {
		return false
	}
	if v, ok := d.Member(in, "valueUrl"); !ok || v != n {
		return false
	}
	arr := m.parent[in]
	if arr < 0 || d.Kind(arr) != splice.KindArray {
		return false
	}
	task := m.parent[arr]
	if task < 0 || d.Kind(task) != splice.KindObject || stringMember(d, task, "resourceType") != "Task" {
		return false
	}
	if v, ok := d.Member(task, "input"); !ok || v != arr {
		return false
	}
	typ, ok := d.Member(in, "type")
	if !ok || d.Kind(typ) != splice.KindObject {
		return false
	}
	codings, ok := d.Member(typ, "coding")
	if !ok || d.Kind(codings) != splice.KindArray {
		return false
	}
	for _, c := range d.Elems(codings) {
		if d.Kind(c) == splice.KindObject && stringMember(d, c, "system") == pasTempCodes && stringMember(d, c, "code") == "payer-url" {
			return true
		}
	}
	return false
}

func stringMember(d *splice.Doc, obj splice.NodeID, key string) string {
	v, ok := d.Member(obj, key)
	if !ok || d.Kind(v) != splice.KindString {
		return ""
	}
	s, _ := d.StringValue(v)
	return s
}

// coverage is what the signatures in src, the body the document was scanned
// from, cover: signedcontent's reading (the network's one rule for what an
// in-payload signature covers, which the SDK's CDex fulfillment reads too),
// indexed for hits. signedcontent scans src under the relay's own limits
// (both read with the same splice and its defaults), so a body the relay
// scanned is one it reads; an error there is a fault in the gateway, never the
// peer's, and panics.
func coverage(src []byte) coverageMap {
	regions, err := signedcontent.Coverage(src)
	if err != nil {
		panic(fmt.Sprintf("relay: signedcontent refused a body of %d bytes the relay scanned: %v", len(src), err))
	}
	c := make([]coveredRegion, len(regions))
	for i, r := range regions {
		c[i] = coveredRegion{start: r.Start, end: r.End, carrier: r.Carrier}
	}
	return newCoverageMap(c)
}

// OwnSignatureMember reports whether member name of a resource of type typ
// (its resourceType) is that resource's own signature, Bundle.signature or
// Provenance.signature: the member at which FHIR R4 types a Signature in a
// Bundle or a Provenance, by its name in any case. It is
// signedcontent.OwnSignatureMember, the rule coverage reads a resource's own
// signature by: a gateway that decides elsewhere whether a Bundle or a
// Provenance is signed reads it through this rule too, so the two agree on
// which member is the signature.
func OwnSignatureMember(typ, name string) bool {
	return signedcontent.OwnSignatureMember(typ, name)
}
