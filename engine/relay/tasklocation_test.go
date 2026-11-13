package relay

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The core's check of the created Task's Location (E-16), run through
// HeaderEdited with a core row: the value is exactly the base input, "/", then
// the tail input, a CDex Task location spelled as it reads back, under a base
// that is an absolute http or https URL with a host and no user information,
// query, fragment, trailing slash, whitespace or control character. Each
// departure is refused as a verification failure, and nothing is made.
func TestTaskLocationCheckRows(t *testing.T) {
	// dotDot is the URL dot segment, spelled so that it is not read as a file
	// path (tools/gateroute): these rows put it in a URL.
	dotDot := strings.Repeat(".", 2)
	src := Exact(NewBody([]byte(`{"resourceType":"Task","id":"t-1"}`), OriginPeerFrame), "application/fhir+json")
	const base = "https://payer.example/fhir/cdex/provider/provider-x"
	made := func(value string, in HeaderInputs) error {
		p, err := HeaderEdited([]Edit{headerRow()}, src, EditCDexTaskLocationRekey, "Location", value, in)
		if err == nil {
			if name, v, ok := p.EditedHeader(); !ok || name != "Location" || v != value {
				t.Errorf("%q: header %q %q %v", value, name, v, ok)
			}
		}
		return err
	}
	for _, tail := range []string{"Task/t-1", "Task/t-1/_history/2", "Task/A.b-9"} {
		if err := made(base+"/"+tail, HeaderInputs{TaskLocationInputBase: base, TaskLocationInputTail: tail}); err != nil {
			t.Errorf("%s: %v", tail, err)
		}
	}
	// A base the provider id was escaped into is taken as built.
	if err := made("https://payer.example/cdex/provider/a%2Fb/Task/t-1",
		HeaderInputs{TaskLocationInputBase: "https://payer.example/cdex/provider/a%2Fb", TaskLocationInputTail: "Task/t-1"}); err != nil {
		t.Errorf("an escaped base: %v", err)
	}
	in := func(b, tail string) HeaderInputs {
		return HeaderInputs{TaskLocationInputBase: b, TaskLocationInputTail: tail}
	}
	ok := in(base, "Task/t-1")
	for name, row := range map[string]struct {
		value string
		in    HeaderInputs
	}{
		// The value is not the base, then the tail.
		"another base in the value":    {"https://payer.example/fhir/cdex/provider/provider-y/Task/t-1", ok},
		"another tail in the value":    {base + "/Task/t-2", ok},
		"the base with no slash after": {base + "Task/t-1", ok},
		"two slashes after the base":   {base + "//Task/t-1", ok},
		"a re-encoded tail":            {base + "/Task/t%2D1", ok},
		"the base in capitals":         {strings.ToUpper(base) + "/Task/t-1", ok},
		"a relative value":             {"Task/t-1", ok},
		"a value that is not a URL":    {"not a URL", ok},
		"a query on the value":         {base + "/Task/t-1?x=1", ok},
		"a fragment on the value":      {base + "/Task/t-1#f", ok},
		"a trailing slash":             {base + "/Task/t-1/", ok},
		// The tail is not a Task location as it reads back.
		"a non-Task tail":       {base + "/Patient/t-1", in(base, "Patient/t-1")},
		"no id":                 {base + "/Task", in(base, "Task")},
		"a dot-segment id":      {base + "/Task/" + dotDot, in(base, "Task/"+dotDot)},
		"an id too long":        {base + "/Task/" + strings.Repeat("a", 65), in(base, "Task/"+strings.Repeat("a", 65))},
		"an escaped id":         {base + "/Task/t%2D1", in(base, "Task/t%2D1")},
		"history with no vid":   {base + "/Task/t-1/_history", in(base, "Task/t-1/_history")},
		"history and more":      {base + "/Task/t-1/_history/2/x", in(base, "Task/t-1/_history/2/x")},
		"a tail with a query":   {base + "/Task/t-1?x", in(base, "Task/t-1?x")},
		"a tail with a space":   {base + "/Task/t 1", in(base, "Task/t 1")},
		"an empty tail":         {base + "/", in(base, "")},
		"a tail with a newline": {base + "/Task/t-1", in(base, "Task/t-1\n")},
		// The base is not one a Location can begin with.
		"a relative base":                {"cdex/provider/p/Task/t-1", in("cdex/provider/p", "Task/t-1")},
		"a path base":                    {"/cdex/provider/p/Task/t-1", in("/cdex/provider/p", "Task/t-1")},
		"an empty base":                  {"/Task/t-1", in("", "Task/t-1")},
		"another scheme":                 {"ftp://payer.example/p/Task/t-1", in("ftp://payer.example/p", "Task/t-1")},
		"no host":                        {"https:///p/Task/t-1", in("https:///p", "Task/t-1")},
		"an opaque base":                 {"https:payer.example/p/Task/t-1", in("https:payer.example/p", "Task/t-1")},
		"user information":               {"https://user@payer.example/p/Task/t-1", in("https://user@payer.example/p", "Task/t-1")},
		"a query on the base":            {"https://payer.example/p?x/Task/t-1", in("https://payer.example/p?x", "Task/t-1")},
		"a fragment on the base":         {"https://payer.example/p#x/Task/t-1", in("https://payer.example/p#x", "Task/t-1")},
		"a base ending in a slash":       {"https://payer.example/p//Task/t-1", in("https://payer.example/p/", "Task/t-1")},
		"a space in the base":            {"https://payer.example/p q/Task/t-1", in("https://payer.example/p q", "Task/t-1")},
		"a tab in the base":              {"https://payer.example/p\tq/Task/t-1", in("https://payer.example/p\tq", "Task/t-1")},
		"a non-breaking space":           {"https://payer.example/p q/Task/t-1", in("https://payer.example/p q", "Task/t-1")},
		"a control character":            {"https://payer.example/p\x01q/Task/t-1", in("https://payer.example/p\x01q", "Task/t-1")},
		"a DEL in the base":              {"https://payer.example/p\x7fq/Task/t-1", in("https://payer.example/p\x7fq", "Task/t-1")},
		"a backslash in the base":        {"https://payer.example/p\\" + dotDot + "\\q/Task/t-1", in("https://payer.example/p\\"+dotDot+"\\q", "Task/t-1")},
		"a quote in the base":            {"https://payer.example/p\"q/Task/t-1", in("https://payer.example/p\"q", "Task/t-1")},
		"a pipe in the base":             {"https://payer.example/p|q/Task/t-1", in("https://payer.example/p|q", "Task/t-1")},
		"braces in the base":             {"https://payer.example/p{q}/Task/t-1", in("https://payer.example/p{q}", "Task/t-1")},
		"a backtick in the base":         {"https://payer.example/p`q/Task/t-1", in("https://payer.example/p`q", "Task/t-1")},
		"a base that is not a URL":       {"https://payer example/p/Task/t-1", in("https://payer example/p", "Task/t-1")},
		"a base with a bad escape":       {"https://payer.example/p%zz/Task/t-1", in("https://payer.example/p%zz", "Task/t-1")},
		"a dot segment in the base":      {"https://payer.example/p/./q/Task/t-1", in("https://payer.example/p/./q", "Task/t-1")},
		"a dot-dot segment in the base":  {"https://payer.example/p/" + dotDot + "/q/Task/t-1", in("https://payer.example/p/"+dotDot+"/q", "Task/t-1")},
		"a base ending in a dot-dot":     {"https://payer.example/p/" + dotDot + "/Task/t-1", in("https://payer.example/p/"+dotDot, "Task/t-1")},
		"an escaped dot-dot in the base": {"https://payer.example/p/%2E%2e/Task/t-1", in("https://payer.example/p/%2E%2e", "Task/t-1")},
		"a non-ASCII letter in the path": {"https://payer.example/p\u00e9/Task/t-1", in("https://payer.example/p\u00e9", "Task/t-1")},
		"a non-ASCII host":               {"https://pay\u00e9r.example/p/Task/t-1", in("https://pay\u00e9r.example/p", "Task/t-1")},
	} {
		err := made(row.value, row.in)
		if err == nil {
			t.Errorf("%s: %q made", name, row.value)
			continue
		}
		// A value a response cannot carry is refused before the check; every
		// other is the check's.
		if fieldValue(row.value) && !errors.Is(err, ErrVerifyMismatch) {
			t.Errorf("%s: %v, want the core's check to refuse it", name, err)
		}
	}
}

// funcTyped reports the path of each part of t that can carry code: a func or
// an interface (any, or one with a method such as Verify, which a module's
// type could satisfy), itself or as a field, element, key or pointee, at any
// depth.
func funcTyped(t reflect.Type, path string, seen map[reflect.Type]bool) []string {
	if seen[t] {
		return nil
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Func, reflect.Interface:
		return []string{path}
	case reflect.Struct:
		var out []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			out = append(out, funcTyped(f.Type, path+"."+f.Name, seen)...)
		}
		return out
	case reflect.Slice, reflect.Array, reflect.Pointer, reflect.Chan:
		return funcTyped(t.Elem(), path+"[]", seen)
	case reflect.Map:
		return append(funcTyped(t.Key(), path+"{key}", seen), funcTyped(t.Elem(), path+"{}", seen)...)
	}
	return nil
}

// No relay check comes from a module package (D113: the core keeps the edit
// catalog and the independent verifier). Structurally:
//   - a row (Edit) carries no func- or interface-typed part, so a
//     workstream's row is data and cannot bring a check of its own;
//   - every header edit the core lists has its check in the core's map, keyed
//     by the edit's id, with the inputs it reads;
//   - a header row whose id the core's map does not hold is refused.
func TestFence_NoRelayVerifierComesFromAModule(t *testing.T) {
	if parts := funcTyped(reflect.TypeOf(Edit{}), "Edit", map[reflect.Type]bool{}); len(parts) != 0 {
		t.Errorf("relay.Edit carries func- or interface-typed parts %v: a row could bring its own check; the core's checks are keyed by edit id", parts)
	}
	if len(headerEdits) == 0 {
		t.Fatal("no header edit: the rows below prove nothing")
	}
	for _, id := range headerEdits {
		v, ok := headerVerifiers[id]
		if !ok || v.verify == nil || len(v.names()) == 0 {
			t.Errorf("header edit %s has no check in the core's map", id)
		}
	}
	for id := range headerVerifiers {
		if !slices.Contains(headerEdits, id) {
			t.Errorf("the core's map checks %s, which is not a header edit", id)
		}
	}
	// The rejection row: with E-16's check taken out of the core's map, its
	// row is refused by ValidateEdits and by HeaderEdited.
	held := headerVerifiers
	t.Cleanup(func() { headerVerifiers = held })
	headerVerifiers = map[EditID]headerVerifier{}
	if err := ValidateEdits([]Edit{headerRow()}); err == nil || !strings.Contains(err.Error(), "has no check in the core") {
		t.Errorf("a header row the core does not check: %v, want refused", err)
	}
	src := Exact(NewBody([]byte(`{}`), OriginPeerFrame), "application/json")
	if p, err := HeaderEdited([]Edit{headerRow()}, src, EditCDexTaskLocationRekey, "Location", "https://payer.example/Task/1", toyInputs); !errors.Is(err, ErrInvalidEdit) || p.Ownership() != 0 {
		t.Errorf("HeaderEdited with no core check: %v %v, want refused", p, err)
	}
	headerVerifiers = held
	if err := ValidateEdits([]Edit{headerRow()}); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// The structural check finds a func or an interface at any depth, and nothing
// in a type with neither.
func TestFuncTypedRows(t *testing.T) {
	type inner struct{ F func() }
	for name, row := range map[string]struct {
		v    any
		want int
	}{
		"a func field":            {struct{ F func(string) error }{}, 1},
		"a func in a nested type": {struct{ I inner }{}, 1},
		"a slice of funcs":        {struct{ S []func() }{}, 1},
		"a map to funcs":          {struct{ M map[string]func() }{}, 1},
		"a pointer to a func":     {struct{ P *func() }{}, 1},
		"an any field":            {struct{ A any }{}, 1},
		"a named interface field": {struct {
			V interface{ Verify(string) error }
		}{}, 1},
		"an interface nested in a struct": {struct{ I struct{ V error } }{}, 1},
		"a slice of interfaces":           {struct{ S []fmt.Stringer }{}, 1},
		"data only": {struct {
			A, B string
			C    []string
		}{}, 0},
	} {
		if got := funcTyped(reflect.TypeOf(row.v), "v", map[reflect.Type]bool{}); len(got) != row.want {
			t.Errorf("%s: %v, want %d", name, got, row.want)
		}
	}
}

// headerVerifierUses reports each place f uses the core's map of header
// checks (headerVerifiers) other than as an allow-list permits: its
// declaration, or the map indexed, (parentheses unwrapped) as a read, never,
// itself or a field or element of the entry, the left-hand side of an
// assignment, an increment, or an address taken. Anything else is
// reported: an assignment to it or to an entry of it, a delete or a clear, its
// address, an alias, a range over it, or passing it anywhere (a helper,
// reflect, maps.Copy), any of which could let a check be added, changed or
// removed elsewhere.
func headerVerifierUses(fset *token.FileSet, f *ast.File) []string {
	parent := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			parent[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	// up climbs from n through enclosing parentheses, returning the outermost
	// parenthesised node and its parent.
	up := func(n ast.Node) (ast.Node, ast.Node) {
		p := parent[n]
		for {
			paren, ok := p.(*ast.ParenExpr)
			if !ok {
				return n, p
			}
			n, p = paren, parent[paren]
		}
	}
	written := func(n ast.Node, p ast.Node) bool {
		switch s := p.(type) {
		case *ast.AssignStmt:
			for _, lhs := range s.Lhs {
				if lhs == n {
					return true
				}
			}
		case *ast.IncDecStmt:
			return s.X == n
		case *ast.UnaryExpr:
			return s.Op == token.AND
		}
		return false
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != "headerVerifiers" {
			return true
		}
		if spec, ok := parent[id].(*ast.ValueSpec); ok && slices.Contains(spec.Names, id) {
			return true
		}
		node, p := up(id)
		if ix, ok := p.(*ast.IndexExpr); ok && ix.X == node {
			// The entry read, then any field or element of it: a write to
			// any of these writes the map's entry.
			outer, q := up(ix)
			for {
				switch x := q.(type) {
				case *ast.SelectorExpr:
					if x.X == outer {
						outer, q = up(x)
						continue
					}
				case *ast.IndexExpr:
					if x.X == outer {
						outer, q = up(x)
						continue
					}
				}
				break
			}
			if !written(outer, q) {
				return true
			}
		}
		out = append(out, fset.Position(id.Pos()).String())
		return true
	})
	return out
}

// The core's map of header checks is used, in the relay package's production
// files, only as an allow-list permits: declared once, and otherwise only
// indexed to read a check, so no code path can add, change or remove one.
func TestFence_HeaderVerifiersWrittenOnlyAtTheirDeclaration(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	declared, reads := 0, 0
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range headerVerifierUses(fset, f) {
			t.Errorf("%s uses headerVerifiers other than by its declaration or an indexed read", w)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for _, id := range x.Names {
					if id.Name == "headerVerifiers" {
						declared++
					}
				}
			case *ast.IndexExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "headerVerifiers" {
					reads++
				}
			}
			return true
		})
	}
	if declared != 1 || reads == 0 {
		t.Fatalf("headerVerifiers is declared %d times and read %d times in the package's production files, want one declaration and reads: this row proves nothing", declared, reads)
	}
}

// The use check reports every use the allow-list does not permit, and none
// that it does.
func TestHeaderVerifierUsesRows(t *testing.T) {
	for name, row := range map[string]struct {
		body string
		want int
	}{
		"an assignment":                    {"headerVerifiers = nil", 1},
		"an entry assigned":                {"headerVerifiers[EditCDexTaskLocationRekey] = headerVerifier{}", 1},
		"a parenthesised map written":      {"(headerVerifiers)[EditCDexTaskLocationRekey] = headerVerifier{}", 1},
		"a parenthesised entry written":    {"(headerVerifiers[EditCDexTaskLocationRekey]) = headerVerifier{}", 1},
		"two assigned at once":             {"headerVerifiers, x = nil, 1", 1},
		"a delete":                         {"delete(headerVerifiers, EditCDexTaskLocationRekey)", 1},
		"a clear":                          {"clear(headerVerifiers)", 1},
		"its address taken":                {"p := &headerVerifiers; _ = p", 1},
		"an alias":                         {"m := headerVerifiers; _ = m", 1},
		"passed to a helper":               {"use(headerVerifiers)", 1},
		"passed to reflect":                {"_ = reflect.ValueOf(headerVerifiers)", 1},
		"copied into":                      {"maps.Copy(headerVerifiers, other)", 1},
		"copied from":                      {"maps.Copy(other, headerVerifiers)", 1},
		"ranged over":                      {"for k := range headerVerifiers { _ = k }", 1},
		"its length":                       {"_ = len(headerVerifiers)", 1},
		"a read":                           {"_ = headerVerifiers[EditCDexTaskLocationRekey]", 0},
		"a lookup":                         {"_, ok := headerVerifiers[EditCDexTaskLocationRekey]; _ = ok", 0},
		"a check called":                   {"_ = headerVerifiers[EditCDexTaskLocationRekey].verify(\"v\", nil)", 0},
		"a parenthesised read":             {"_ = (headerVerifiers)[EditCDexTaskLocationRekey].inputs", 0},
		"an entry read into a local copy":  {"v := headerVerifiers[EditCDexTaskLocationRekey]; v.inputs = \"\"", 0},
		"a write through an entry's field": {"headerVerifiers[EditCDexTaskLocationRekey].inputs = \"\"", 1},
		"a write through an entry's names": {"headerVerifiers[EditCDexTaskLocationRekey].names()[0] = \"x\"", 0},
		"a write through an element":       {"headerVerifiers[EditCDexTaskLocationRekey].inputs[0] = 'x'", 1},
		"an increment through a field":     {"headerVerifiers[EditCDexTaskLocationRekey].n++", 1},
		"an entry's field's address":       {"p := &headerVerifiers[EditCDexTaskLocationRekey].inputs; _ = p", 1},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", "package relay\nfunc f() { var x int; _ = x; "+row.body+" }\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := headerVerifierUses(fset, f); len(got) != row.want {
			t.Errorf("%s: %v, want %d", name, got, row.want)
		}
	}
	// The declaration itself is not a use.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", "package relay\nvar headerVerifiers = map[EditID]headerVerifier{}\n", parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if got := headerVerifierUses(fset, f); len(got) != 0 {
		t.Errorf("the declaration: %v, want none", got)
	}
}

// sharedState reports the path of each part of t that a copy of a t value
// would share with the original: a slice, map, pointer, channel or interface,
// itself or as a field or element, at any depth. funcs are reported apart, in
// funcs, as a func value cannot be written through.
func sharedState(t reflect.Type, path string, funcs *[]string) []string {
	switch t.Kind() {
	case reflect.Slice, reflect.Map, reflect.Pointer, reflect.Chan, reflect.Interface, reflect.UnsafePointer:
		return []string{path}
	case reflect.Func:
		*funcs = append(*funcs, path)
	case reflect.Struct:
		var out []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			out = append(out, sharedState(f.Type, path+"."+f.Name, funcs)...)
		}
		return out
	case reflect.Array:
		return sharedState(t.Elem(), path+"[]", funcs)
	}
	return nil
}

// An entry of the core's map of header checks is read as a copy
// (headerVerifiers[id]), so it must share nothing a write through the copy
// could reach: its fields hold no slice, map, pointer, channel or interface,
// and its only func is the check itself. A field added later that shares state
// fails here.
func TestFence_HeaderVerifierEntryHoldsNothingShared(t *testing.T) {
	var funcs []string
	if shared := sharedState(reflect.TypeOf(headerVerifier{}), "headerVerifier", &funcs); len(shared) != 0 {
		t.Errorf("headerVerifier holds state a copy shares with the map's entry: %v", shared)
	}
	if !slices.Equal(funcs, []string{"headerVerifier.verify"}) {
		t.Errorf("headerVerifier's funcs are %v, want only its check, headerVerifier.verify", funcs)
	}
}

// The walker reports each kind of shared state, and funcs apart.
func TestSharedStateRows(t *testing.T) {
	for name, row := range map[string]struct {
		v           any
		want, funcs int
	}{
		"a slice field":            {struct{ S []string }{}, 1, 0},
		"a map field":              {struct{ M map[string]string }{}, 1, 0},
		"a pointer field":          {struct{ P *string }{}, 1, 0},
		"a channel field":          {struct{ C chan int }{}, 1, 0},
		"an interface field":       {struct{ I any }{}, 1, 0},
		"a slice in a nested type": {struct{ N struct{ S []int } }{}, 1, 0},
		"an array of slices":       {struct{ A [2][]int }{}, 1, 0},
		"a func field":             {struct{ F func() }{}, 0, 1},
		"values only": {struct {
			A string
			B [2]int
			C struct{ D bool }
		}{}, 0, 0},
	} {
		var funcs []string
		if got := sharedState(reflect.TypeOf(row.v), "v", &funcs); len(got) != row.want || len(funcs) != row.funcs {
			t.Errorf("%s: shared %v, funcs %v; want %d and %d", name, got, funcs, row.want, row.funcs)
		}
	}
}
