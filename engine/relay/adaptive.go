package relay

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// sdcAdaptiveExtension is the SDC extension that names an adaptive
// Questionnaire's $next-question server, as a valueUrl or, at DTR 2.0.1, as
// the boolean true.
const sdcAdaptiveExtension = "http://hl7.org/fhir/uv/sdc/StructureDefinition/sdc-questionnaire-questionnaireAdaptive"

// The two member names the adaptive address is written under.
const (
	adaptiveURLMember  = "valueUrl"
	adaptiveBoolMember = "valueBoolean"
)

// adaptiveEdits are the registered edits of a questionnaireAdaptive address:
// the re-key (E-12) and its restore (E-13). Each may make only two changes
// (adaptiveChange), and no other edit may turn the address's boolean form
// into its URL form or back, or change its value (adaptiveFormsHeld).
var adaptiveEdits = map[EditID]bool{EditDTRAdaptiveAddressRekey: true, EditDTRAdaptiveAddressRestore: true}

// adaptiveExtension reports whether obj is a questionnaireAdaptive extension
// of a Questionnaire, decided by structure: an object whose url is
// sdcAdaptiveExtension, an element of the extension array of an object whose
// resourceType is Questionnaire, at any depth.
func (m *model) adaptiveExtension(obj splice.NodeID) bool {
	d := m.doc
	if obj < 0 || d.Kind(obj) != splice.KindObject || stringMember(d, obj, "url") != sdcAdaptiveExtension {
		return false
	}
	arr := m.parent[obj]
	if arr < 0 || d.Kind(arr) != splice.KindArray {
		return false
	}
	q := m.parent[arr]
	if q < 0 || d.Kind(q) != splice.KindObject || stringMember(d, q, "resourceType") != "Questionnaire" {
		return false
	}
	ext, ok := d.Member(q, "extension")
	return ok && ext == arr
}

// adaptiveAddressValue reports whether n is the valueUrl of a
// questionnaireAdaptive extension (adaptiveExtension).
func (m *model) adaptiveAddressValue(n splice.NodeID) bool {
	ext := m.parent[n]
	if ext < 0 || !m.adaptiveExtension(ext) {
		return false
	}
	v, ok := m.doc.Member(ext, adaptiveURLMember)
	return ok && v == n
}

// adaptivePair reports whether replacing the questionnaireAdaptive extension
// obj with repl changes exactly the address's form in the direction edit id
// may change it, and nothing else: the re-key turns "valueBoolean": true into
// a valueUrl string, the restore turns a valueUrl string back into
// "valueBoolean": true. Only the member's name token and its value token
// differ; every other byte of the element (its whitespace, its other members
// and their order) is kept.
func (m *model) adaptivePair(id EditID, obj splice.NodeID, repl []byte) bool {
	if !m.adaptiveExtension(obj) {
		return false
	}
	from, to := adaptiveBoolMember, adaptiveURLMember
	if id == EditDTRAdaptiveAddressRestore {
		from, to = adaptiveURLMember, adaptiveBoolMember
	} else if id != EditDTRAdaptiveAddressRekey {
		return false
	}
	var mem *splice.MemberRef
	for _, r := range m.doc.Members(obj) {
		if r.Name == from {
			r := r
			mem = &r
		}
	}
	if mem == nil {
		return false
	}
	os, oe := m.doc.Span(obj)
	vs, ve := m.doc.Span(mem.Value)
	old := m.src[vs:ve]
	switch from {
	case adaptiveBoolMember:
		if !bytes.Equal(old, []byte("true")) {
			return false
		}
	default:
		if m.doc.Kind(mem.Value) != splice.KindString {
			return false
		}
	}
	prefix := append(append(append([]byte{}, m.src[os:mem.KeyStart]...), `"`+to+`"`...), m.src[mem.KeyEnd:vs]...)
	suffix := m.src[ve:oe]
	if len(repl) < len(prefix)+len(suffix) || !bytes.HasPrefix(repl, prefix) || !bytes.HasSuffix(repl, suffix) {
		return false
	}
	val := repl[len(prefix) : len(repl)-len(suffix)]
	if to == adaptiveBoolMember {
		return bytes.Equal(val, []byte("true"))
	}
	return isStringToken(val)
}

// isStringToken reports whether b is exactly one JSON string token, with no
// whitespace around it.
func isStringToken(b []byte) bool {
	var s string
	return len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' && json.Valid(b) && json.Unmarshal(b, &s) == nil
}

// adaptiveChange reports why the change eff of an adaptive edit (id in
// adaptiveEdits) is not one it may make, or nil. It may only replace the
// valueUrl of a questionnaireAdaptive extension with a JSON string, or
// replace the extension itself by the one name change adaptivePair allows.
func (m *model) adaptiveChange(id EditID, eff effect) error {
	if eff.replaced < 0 {
		return fmt.Errorf("%w: %s may only replace a questionnaireAdaptive address", ErrInvalidEdit, id)
	}
	n, v := eff.replaced, m.replaced[eff.replaced]
	if m.adaptiveAddressValue(n) {
		if isStringToken(v) {
			return nil
		}
		return fmt.Errorf("%w: %s may write only a string as a questionnaireAdaptive valueUrl", ErrInvalidEdit, id)
	}
	if m.adaptivePair(id, n, v) {
		return nil
	}
	return fmt.Errorf("%w: %s changes something other than a questionnaireAdaptive address", ErrInvalidEdit, id)
}

// adaptiveFormsHeld reports why out, the result of changes to the original,
// changes a questionnaireAdaptive address in a way no edit may, or nil. Only
// the re-key and the restore may change an address at all, in form or in
// value, and only as registered: over the whole document, the objects whose
// url is the questionnaireAdaptive extension must be the same in number, in
// the same order, each carrying the same form of the address (a valueUrl, a
// valueBoolean, both or neither) and the same value bytes, except a
// Questionnaire's own adaptive extension that E-12 or E-13 itself changed (a
// string replace of its valueUrl, or the one name change adaptivePair allows).
// So no other edit, whatever operations it uses, can add, remove, re-point or
// change the form of an address, and the restore's guarantee (the payer gets
// back the bytes it wrote) holds against the whole edit set.
func (m *model) adaptiveFormsHeld(out []byte, changes []Change) error {
	// An extension the re-key or the restore changed may differ: wholly, for
	// the one form change (no other operation can touch a value a replace
	// covers: the splice refuses overlapping operations); and only in its
	// valueUrl's bytes, for a string replace of that valueUrl.
	pairChanged, urlChanged := map[splice.NodeID]bool{}, map[splice.NodeID]bool{}
	i := 0
	for _, c := range changes {
		for range c.Ops {
			eff := m.effects[i]
			i++
			if eff == nil || !adaptiveEdits[c.Edit] || eff.replaced < 0 {
				continue
			}
			switch n := eff.replaced; {
			case m.adaptivePair(c.Edit, n, m.replaced[n]):
				pairChanged[n] = true
			case m.adaptiveAddressValue(n):
				urlChanged[m.parent[n]] = true
			}
		}
	}
	before := adaptiveFormsOf(m.doc, m.src)
	od, err := splice.Scan(out, splice.Limits{})
	if err != nil {
		return fmt.Errorf("%w: the output does not scan: %v", ErrVerifyMismatch, err)
	}
	after := adaptiveFormsOf(od, out)
	refused := fmt.Errorf("%w: a questionnaireAdaptive address is added, removed or changed; only %s and %s may change it, and only as registered",
		ErrInvalidEdit, EditDTRAdaptiveAddressRekey, EditDTRAdaptiveAddressRestore)
	if len(before) != len(after) {
		return refused
	}
	// The extensions are matched by position: the count and order are held,
	// so the k-th address before is the k-th after.
	for k := range before {
		b, a := before[k], after[k]
		switch {
		case pairChanged[b.node]:
		case urlChanged[b.node]:
			if b.form != a.form || b.boolValue != a.boolValue {
				return refused
			}
		case b.form != a.form || b.urlValue != a.urlValue || b.boolValue != a.boolValue:
			return refused
		}
	}
	return nil
}

// adaptiveForm is one object whose url is the questionnaireAdaptive extension:
// the form of the address it carries, and that address's value bytes.
type adaptiveForm struct {
	node                      splice.NodeID
	form, urlValue, boolValue string
}

// adaptiveFormsOf lists, in document order, every object of d (scanned from
// src) whose url is the questionnaireAdaptive extension, wherever it is, with
// the form of its address (which of valueUrl and valueBoolean it carries) and
// the raw bytes of each, byte for byte.
func adaptiveFormsOf(d *splice.Doc, src []byte) []adaptiveForm {
	var out []adaptiveForm
	for n := splice.NodeID(0); int(n) < d.Len(); n++ {
		if d.Kind(n) != splice.KindObject || stringMember(d, n, "url") != sdcAdaptiveExtension {
			continue
		}
		f := adaptiveForm{node: n}
		if v, ok := d.Member(n, adaptiveURLMember); ok {
			s, e := d.Span(v)
			f.form, f.urlValue = f.form+"url;", string(src[s:e])
		}
		if v, ok := d.Member(n, adaptiveBoolMember); ok {
			s, e := d.Span(v)
			f.form, f.boolValue = f.form+"bool;", string(src[s:e])
		}
		out = append(out, f)
	}
	return out
}

// AdaptiveForm returns the operation that changes the form of the
// questionnaireAdaptive extension ext in d's body, keeping every other byte of
// it: with url set, its "valueBoolean": true becomes "valueUrl" with url (the
// re-key's explicit address); with url empty, its valueUrl becomes
// "valueBoolean": true (the restore). Only the member's name token and its
// value token are written; ApplyChanges then checks the change is the one
// adaptivePair allows for the edit making it.
func (d *Document) AdaptiveForm(ext NodeID, url string) (Op, error) {
	if d.d.Kind(ext) != splice.KindObject {
		return Op{}, fmt.Errorf("%w: node %d is not an extension", ErrInvalidEdit, ext)
	}
	from, to, value := adaptiveBoolMember, adaptiveURLMember, []byte(nil)
	if url == "" {
		from, to, value = adaptiveURLMember, adaptiveBoolMember, []byte("true")
	} else {
		v, err := JSONString(url)
		if err != nil {
			return Op{}, fmt.Errorf("%w: %v", ErrInvalidEdit, err)
		}
		value = v
	}
	var mem *splice.MemberRef
	for _, r := range d.d.Members(ext) {
		if r.Name == from {
			r := r
			mem = &r
		}
	}
	if mem == nil {
		return Op{}, fmt.Errorf("%w: the extension has no %s", ErrInvalidEdit, from)
	}
	src := d.body.bytes()
	os, oe := d.d.Span(ext)
	vs, ve := d.d.Span(mem.Value)
	repl := make([]byte, 0, oe-os+len(value)+8)
	repl = append(repl, src[os:mem.KeyStart]...)
	repl = append(repl, `"`+to+`"`...)
	repl = append(repl, src[mem.KeyEnd:vs]...)
	repl = append(repl, value...)
	repl = append(repl, src[ve:oe]...)
	return d.Replace(ext, repl), nil
}

// JSONString is s as a JSON string token, written without HTML escaping
// (json.Marshal writes &, < and > as \u0026, \u003c and \u003e), so an
// address an edit writes back is the bytes it was read as. The encoder still
// escapes U+2028 and U+2029 and replaces invalid UTF-8; neither can occur in
// the ASCII URL an adaptive address is checked to be.
func JSONString(s string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
