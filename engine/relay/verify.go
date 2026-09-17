package relay

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

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
	return &effect{start: cs, end: ce, inside: true}
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
		return &effect{start: s, end: e}, nil

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
			return &effect{start: mem.KeyStart, end: ve}, nil
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
		return m.inside(t.home()), nil

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

func (m *model) checkSpans(out []byte, spans []splice.Edit, a *outAnchors) error {
	delta := 0
	for _, e := range spans {
		os := e.Start + delta
		oe := os + len(e.New)
		delta += len(e.New) - (e.End - e.Start)
		if err := m.checkSpan(e, out, os, oe, a); err != nil {
			return err
		}
	}
	return nil
}

func (m *model) checkSpan(e splice.Edit, out []byte, os, oe int, a *outAnchors) error {
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

type coverageMap []coveredRegion

// hit reports the first covered region an effect touches. A byte range
// touches a region when they overlap; an insertion inside a container
// touches a region that contains the whole container.
func (c coverageMap) hit(e effect) (string, bool) {
	for _, r := range c {
		if e.inside {
			if r.start <= e.start && e.end <= r.end {
				return r.carrier, true
			}
			continue
		}
		if e.start < r.end && r.start < e.end {
			return r.carrier, true
		}
	}
	return "", false
}

type resource struct {
	node      splice.NodeID
	typ, id   string
	fullURL   string
	contained bool
}

func stringMember(d *splice.Doc, obj splice.NodeID, key string) string {
	v, ok := d.Member(obj, key)
	if !ok || d.Kind(v) != splice.KindString {
		return ""
	}
	s, _ := d.StringValue(v)
	return s
}

func hasMember(d *splice.Doc, obj splice.NodeID, key string) bool {
	_, ok := d.Member(obj, key)
	return ok
}

// coverage finds what the document's in-payload signatures cover:
//
//   - a Bundle with a signature covers the whole Bundle;
//   - a Provenance with a signature covers itself and every resource in the
//     document its target references resolve to (by fullUrl, by
//     type and id, or by #id for contained resources); a target that
//     resolves to nothing in the document covers nothing here;
//   - any Signature element (an object with type, when or its extension
//     form _when, and who) covers its
//     nearest enclosing resource, or the whole document when it has none.
func coverage(d *splice.Doc) coverageMap {
	parent := parents(d)
	byNode := map[splice.NodeID]*resource{}
	var all []*resource
	for i := 0; i < d.Len(); i++ {
		n := splice.NodeID(i)
		if d.Kind(n) != splice.KindObject {
			continue
		}
		typ := stringMember(d, n, "resourceType")
		if typ == "" {
			continue
		}
		r := &resource{node: n, typ: typ, id: stringMember(d, n, "id")}
		if p := parent[n]; p >= 0 {
			if v, ok := d.Member(p, "resource"); ok && v == n {
				r.fullURL = stringMember(d, p, "fullUrl")
			}
			if d.Kind(p) == splice.KindArray {
				if pp := parent[p]; pp >= 0 {
					if v, ok := d.Member(pp, "contained"); ok && v == p {
						r.contained = true
					}
				}
			}
		}
		byNode[n] = r
		all = append(all, r)
	}
	var c coverageMap
	add := func(n splice.NodeID, carrier string) {
		s, e := d.Span(n)
		c = append(c, coveredRegion{start: s, end: e, carrier: carrier})
	}
	for i := 0; i < d.Len(); i++ {
		n := splice.NodeID(i)
		if d.Kind(n) != splice.KindObject {
			continue
		}
		if r := byNode[n]; r != nil && hasMember(d, n, "signature") {
			switch r.typ {
			case "Bundle":
				add(n, "Bundle.signature")
			case "Provenance":
				add(n, "Provenance.signature")
				targets, _ := d.Member(n, "target")
				for _, t := range d.Elems(targets) {
					ref := stringMember(d, t, "reference")
					if ref == "" {
						continue
					}
					for _, cand := range all {
						if refersTo(ref, cand) {
							add(cand.node, "Provenance.signature")
						}
					}
				}
			}
		}
		if hasMember(d, n, "type") && (hasMember(d, n, "when") || hasMember(d, n, "_when")) && hasMember(d, n, "who") {
			enc := parent[n]
			for enc >= 0 && byNode[enc] == nil {
				enc = parent[enc]
			}
			if enc >= 0 {
				add(enc, "Signature in "+byNode[enc].typ)
			} else {
				add(d.Root(), "Signature")
			}
		}
	}
	return c
}

func stripHistory(ref string) string {
	if i := strings.Index(ref, "/_history/"); i >= 0 {
		return ref[:i]
	}
	return ref
}

// refersTo reports whether a reference may name r. It errs toward a match:
// a wider match only refuses more edits.
func refersTo(ref string, r *resource) bool {
	if strings.HasPrefix(ref, "#") {
		return r.contained && len(ref) > 1 && r.id == ref[1:]
	}
	ref = stripHistory(ref)
	if r.fullURL != "" && stripHistory(r.fullURL) == ref {
		return true
	}
	segs := strings.Split(ref, "/")
	if len(segs) < 2 {
		return false
	}
	typ, id := segs[len(segs)-2], segs[len(segs)-1]
	if r.typ != typ || id == "" {
		return false
	}
	// By type and id: the resource's own id, or, for a resource that
	// carries none, the tail of its fullUrl.
	return r.id == id || strings.HasSuffix(stripHistory(r.fullURL), "/"+typ+"/"+id)
}
