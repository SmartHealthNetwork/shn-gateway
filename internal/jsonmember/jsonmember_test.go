package jsonmember

import (
	"strings"
	"testing"
)

// A refusal of a body that repeats a member name names the member and the
// object that repeats it, by its path from the top-level value, so the sender
// can find and fix it.
func TestRepeatedMemberNamesTheMemberAndWhere(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"top level", `{"a":1,"a":2}`, `the JSON member "a" in the top-level object`},
		{"nested object", `{"x":{"y":{"k":1,"k":2}}}`, `the JSON member "k" in x.y`},
		{"in an array", `{"entry":[{"r":1},{"resource":{"s":"a","s":"b"}}]}`, `the JSON member "s" in entry[1].resource`},
		{"after a nested value", `{"a":[1,{"b":2}],"c":{"d":[3],"d":4}}`, `the JSON member "d" in c`},
		{"an escaped name", `{"q":{"\u0041":1,"A":2}}`, `the JSON member "A" in q`},
		{"a number beyond float64 before it", `{"a":1e999,"x":{"k":1,"k":2}}`, `the JSON member "k" in x`},
		{"a name holding a dot", `{"a.b":{"k":1,"k":2}}`, `the JSON member "k" in ["a.b"]`},
		{"a nested name holding a bracket", `{"x":{"y[0]":{"k":1,"k":2}}}`, `the JSON member "k" in x["y[0]"]`},
		{"a long name", `{"` + strings.Repeat("n", 70) + `":1,"` + strings.Repeat("n", 70) + `":2}`, `the JSON member "` + strings.Repeat("n", 64) + `…" in the top-level object`},
		{"case folded", `{"q":{"Name":1,"name":2}}`, `the JSON member "name" in q`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Repeated([]byte(c.body))
			if !ok || got != c.want {
				t.Fatalf("Repeated(%s) = %q, %v; want %q", c.body, got, ok, c.want)
			}
		})
	}
	for _, body := range []string{`{"a":1,"b":2}`, `[1,2]`, `not json`} {
		if got, ok := Repeated([]byte(body)); ok {
			t.Errorf("Repeated(%s) = %q; it repeats no member", body, got)
		}
	}
	// An offset that starts no member name names no object.
	if p, ok := memberObjectPath([]byte(`{"a":{"b":1}}`), 9); ok {
		t.Errorf("an offset inside a value named %q", p)
	}
}

// A repeat is found however deep it is nested, up to encoding/json's own
// bound (MaxDepth) and past the strict reader's; a body nested deeper is read
// by no decoder, so it is no repeat, and the walk holds no more than MaxDepth
// levels. Names equal under case folding repeat.
func TestRepeatsAName_DepthBoundAndFolding(t *testing.T) {
	nested := func(depth int, inner string) string {
		// depth counts every open object and array, inner's included.
		return strings.Repeat(`{"a":`, depth-1) + inner + strings.Repeat(`}`, depth-1)
	}
	for name, row := range map[string]struct {
		body string
		want bool
	}{
		"a repeat at the bound":          {nested(MaxDepth, `{"k":1,"k":2}`), true},
		"a repeat past the bound":        {nested(MaxDepth+1, `{"k":1,"k":2}`), false},
		"no repeat at the bound":         {nested(MaxDepth, `{"k":1}`), false},
		"an array past the bound":        {nested(MaxDepth, `[{"k":1,"k":2}]`), false},
		"a repeat under case folding":    {`{"Name":1,"nAME":2}`, true},
		"names in sibling objects":       {`[{"k":1},{"k":2}]`, false},
		"a repeat past a lone surrogate": {`{"s":"\ud800","k":1,"k":2}`, true},
	} {
		if got := RepeatsAName([]byte(row.body)); got != row.want {
			t.Errorf("%s: RepeatsAName = %v, want %v", name, got, row.want)
		}
	}
	if FoldedName("Name") != FoldedName("nAME") || FoldedName("name") == FoldedName("names") {
		t.Error("FoldedName does not fold as EqualFold does")
	}
}
