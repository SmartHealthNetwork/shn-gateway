package relay

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// ErrFieldNameCase: a body names a member that matches a destination field
// only when case is ignored (a client error). encoding/json would read it
// into that field; a recipient that matches names exactly, as FHIR and CDS
// Hooks require, would not. Returned as a *FieldNameError.
var ErrFieldNameCase = errors.New("relay: member name matches a field only when case is ignored")

// FieldNameError reports the first member whose name matches a destination
// field only when case is ignored. Member is the name as the body spells
// it, Field the field's JSON name, and Path the location of the object
// holding the member ($ is the decoded value itself).
type FieldNameError struct {
	Member string
	Field  string
	Path   string
}

func (e *FieldNameError) Error() string {
	return fmt.Sprintf("relay: member %q at %s does not match field %q exactly", e.Member, e.Path, e.Field)
}

// Is reports whether target is ErrFieldNameCase.
func (e *FieldNameError) Is(target error) bool { return target == ErrFieldNameCase }

var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// checkFieldNames walks node n of d alongside the value v would be decoded
// into, and refuses any object member that reaches a struct field only by
// case folding. It follows the decoder's own rules: pointers and non-nil
// interfaces holding non-nil pointers are followed, struct field names come
// from json tags with the same embedding and precedence rules, map values
// and slice or array elements are checked with their element type, and
// member names that match no field are ignored.
//
// A value whose type decodes itself (json.Unmarshaler or
// encoding.TextUnmarshaler), a json.RawMessage, and a generic value (an
// empty interface, or a map of one) are not inspected below that point: the
// type's own code decides how it reads member names.
func checkFieldNames(d *splice.Doc, n splice.NodeID, v any) error {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() {
		return nil // the decoder reports an invalid target itself
	}
	w := nameWalk{d: d}
	return w.value(n, rv.Type(), rv, "$")
}

type nameWalk struct{ d *splice.Doc }

func decodesItself(t reflect.Type) bool {
	if t.Kind() != reflect.Pointer && t.Name() != "" {
		t = reflect.PointerTo(t)
	}
	return t.Kind() == reflect.Pointer && (t.Implements(jsonUnmarshalerType) || t.Implements(textUnmarshalerType))
}

// value checks node n against type t. v, when valid, is the existing value
// of type t the decoder would write into.
func (w nameWalk) value(n splice.NodeID, t reflect.Type, v reflect.Value, path string) error {
	kind := w.d.Kind(n)
	if kind != splice.KindObject && kind != splice.KindArray {
		return nil
	}
	for {
		if decodesItself(t) {
			return nil
		}
		switch t.Kind() {
		case reflect.Pointer:
			if v.IsValid() && !v.IsNil() {
				v = v.Elem()
			} else {
				v = reflect.Value{}
			}
			t = t.Elem()
			continue
		case reflect.Interface:
			if !v.IsValid() || v.IsNil() {
				return nil
			}
			e := v.Elem()
			if e.Kind() != reflect.Pointer || e.IsNil() {
				return nil
			}
			t, v = e.Type(), e
			continue
		}
		break
	}
	switch {
	case kind == splice.KindObject && t.Kind() == reflect.Struct:
		fields := structNames(t)
		for _, m := range w.d.Members(n) {
			f, ok := fields.exact[m.Name]
			if !ok {
				if folded, hit := fields.folded[foldKey(m.Name)]; hit {
					return &FieldNameError{Member: m.Name, Field: folded.name, Path: path}
				}
				continue
			}
			var fv reflect.Value
			if v.IsValid() {
				if x, err := v.FieldByIndexErr(f.index); err == nil {
					fv = x
				}
			}
			if err := w.value(m.Value, f.typ, fv, path+"."+m.Name); err != nil {
				return err
			}
		}
	case kind == splice.KindObject && t.Kind() == reflect.Map:
		for _, m := range w.d.Members(n) {
			if err := w.value(m.Value, t.Elem(), reflect.Value{}, path+"["+strconv.Quote(m.Name)+"]"); err != nil {
				return err
			}
		}
	case kind == splice.KindArray && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array):
		all := v
		if v.IsValid() && v.Kind() == reflect.Slice {
			all = v.Slice(0, v.Cap()) // the decoder reuses the spare capacity
		}
		for i, el := range w.d.Elems(n) {
			if t.Kind() == reflect.Array && i >= t.Len() {
				break // the decoder skips elements past a fixed array
			}
			var ev reflect.Value
			if all.IsValid() && i < all.Len() {
				ev = all.Index(i)
			}
			if err := w.value(el, t.Elem(), ev, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	}
	return nil
}

type structField struct {
	name   string
	tagged bool
	index  []int
	typ    reflect.Type
}

type structFields struct {
	exact  map[string]*structField
	folded map[string]*structField
}

var structNameCache sync.Map // reflect.Type -> *structFields

func structNames(t reflect.Type) *structFields {
	if f, ok := structNameCache.Load(t); ok {
		return f.(*structFields)
	}
	f, _ := structNameCache.LoadOrStore(t, buildStructNames(t))
	return f.(*structFields)
}

// buildStructNames lists the member names encoding/json decodes into t,
// with the same embedding and precedence rules.
func buildStructNames(t reflect.Type) *structFields {
	type pending struct {
		typ   reflect.Type
		index []int
	}
	var fields []structField
	next := []pending{{typ: t}}
	var count, nextCount map[reflect.Type]int
	visited := map[reflect.Type]bool{}
	for len(next) > 0 {
		current := next
		next = nil
		count, nextCount = nextCount, map[reflect.Type]int{}
		for _, p := range current {
			if visited[p.typ] {
				continue
			}
			visited[p.typ] = true
			for i := 0; i < p.typ.NumField(); i++ {
				sf := p.typ.Field(i)
				if sf.Anonymous {
					et := sf.Type
					if et.Kind() == reflect.Pointer {
						et = et.Elem()
					}
					if !sf.IsExported() && et.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, _, _ := strings.Cut(tag, ",")
				if !validTagName(name) {
					name = ""
				}
				index := append(slices.Clip(p.index), i)
				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					f := structField{name: name, tagged: name != "", index: index, typ: sf.Type}
					if f.name == "" {
						f.name = sf.Name
					}
					fields = append(fields, f)
					if count[p.typ] > 1 {
						// Several embeddings of the same type at this depth:
						// the duplicate makes the name ambiguous below.
						fields = append(fields, f)
					}
					continue
				}
				nextCount[ft]++
				if nextCount[ft] == 1 {
					next = append(next, pending{typ: ft, index: index})
				}
			}
		}
	}
	slices.SortStableFunc(fields, func(a, b structField) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		if c := len(a.index) - len(b.index); c != 0 {
			return c
		}
		if a.tagged != b.tagged {
			if a.tagged {
				return -1
			}
			return 1
		}
		return slices.Compare(a.index, b.index)
	})
	var kept []structField
	for i := 0; i < len(fields); {
		j := i + 1
		for j < len(fields) && fields[j].name == fields[i].name {
			j++
		}
		group := fields[i:j]
		// The shallowest field wins, a tagged one before an untagged one at
		// the same depth; two equal candidates hide the name.
		if len(group) == 1 || len(group[0].index) != len(group[1].index) || group[0].tagged != group[1].tagged {
			kept = append(kept, group[0])
		}
		i = j
	}
	slices.SortFunc(kept, func(a, b structField) int { return slices.Compare(a.index, b.index) })
	out := &structFields{exact: map[string]*structField{}, folded: map[string]*structField{}}
	for i := range kept {
		f := &kept[i]
		out.exact[f.name] = f
		if _, ok := out.folded[foldKey(f.name)]; !ok {
			out.folded[foldKey(f.name)] = f
		}
	}
	return out
}

// validTagName reports whether a json tag name is used as given; otherwise
// the decoder falls back to the Go field name.
func validTagName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}

// foldKey maps a name to the smallest rune of each rune's simple
// case-folding orbit, so foldKey(a) == foldKey(b) exactly when the decoder
// would match a to b ignoring case.
func foldKey(name string) string {
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
