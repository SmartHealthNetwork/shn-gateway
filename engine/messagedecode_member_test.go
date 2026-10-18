package engine

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
			got, ok := repeatedMember([]byte(c.body))
			if !ok || got != c.want {
				t.Fatalf("repeatedMember(%s) = %q, %v; want %q", c.body, got, ok, c.want)
			}
		})
	}
	for _, body := range []string{`{"a":1,"b":2}`, `[1,2]`, `not json`} {
		if got, ok := repeatedMember([]byte(body)); ok {
			t.Errorf("repeatedMember(%s) = %q; it repeats no member", body, got)
		}
	}
	// An offset that starts no member name names no object.
	if p, ok := memberObjectPath([]byte(`{"a":{"b":1}}`), 9); ok {
		t.Errorf("an offset inside a value named %q", p)
	}
}
