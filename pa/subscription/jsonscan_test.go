package subscription

import (
	"testing"
)

// A member is removed with exactly one adjacent comma and inserted after the
// last one, wherever it sits, and every other byte stays as it was.
func TestRemoveAndInsertMember(t *testing.T) {
	for _, row := range []struct {
		body, name, want string
	}{
		{`{"a":1,"b":2,"c":3}`, "a", `{"b":2,"c":3}`},
		{`{"a":1,"b":2,"c":3}`, "b", `{"a":1,"c":3}`},
		{`{"a":1,"b":2,"c":3}`, "c", `{"a":1,"b":2}`},
		{`{ "a" : [1, {"x":true}] }`, "a", `{  }`},
		{`{"a":1, "b" : null}`, "b", `{"a":1}`},
	} {
		root, err := scanJSON([]byte(row.body))
		if err != nil {
			t.Fatal(err)
		}
		out, err := applySplices([]byte(row.body), []splice{removeMember(root, root.memberIndex(row.name))})
		if err != nil || string(out) != row.want {
			t.Errorf("remove %s from %s = %s, %v; want %s", row.name, row.body, out, err, row.want)
		}
	}
	for body, want := range map[string]string{
		`{}`:            `{"k":["v"]}`,
		`{ }`:           `{"k":["v"] }`,
		`{"a":1}`:       `{"a":1,"k":["v"]}`,
		`{"a":{"b":2}}`: `{"a":{"b":2},"k":["v"]}`,
	} {
		root, _ := scanJSON([]byte(body))
		out, err := applySplices([]byte(body), []splice{insertMember(root, "k", jsonStrings([]string{"v"}))})
		if err != nil || string(out) != want {
			t.Errorf("insert into %s = %s, %v; want %s", body, out, err, want)
		}
	}
}

// Splices that overlap, or fall outside the body, are refused, never applied.
func TestApplySplices_RefusesOverlap(t *testing.T) {
	src := []byte(`0123456789`)
	for name, sp := range map[string][]splice{
		"overlapping":  {{start: 1, end: 5}, {start: 4, end: 6}},
		"past the end": {{start: 8, end: 11}},
		"backwards":    {{start: 5, end: 4}},
	} {
		if out, err := applySplices(src, sp); err == nil {
			t.Errorf("%s: applied %s", name, out)
		}
	}
	if out, err := applySplices(src, []splice{{start: 6, end: 7, repl: []byte("x")}, {start: 1, end: 2, repl: []byte("y")}}); err != nil || string(out) != "0y2345x789" {
		t.Errorf("out of order: %s, %v", out, err)
	}
	if string(src) != "0123456789" {
		t.Error("the source was changed")
	}
}

// Strings keep what was written: no HTML escaping, and the decoded value of
// an escaped name or value.
func TestJSONStrings(t *testing.T) {
	if got := string(jsonString(`a<b>&"c"`)); got != `"a<b>&\"c\""` {
		t.Errorf("jsonString = %s", got)
	}
	root, err := scanJSON([]byte(`{"a":"é\n", "t":true, "f":false, "n":null, "x":-1.5e3}`))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := root.member("a").stringOf(); !ok || v != "é\n" {
		t.Errorf("an escaped member = %q, %v", v, ok)
	}
	for name, kind := range map[string]byte{"t": 't', "f": 'f', "n": 'n', "x": '0'} {
		if root.member(name).kind != kind {
			t.Errorf("%s kind %c", name, root.member(name).kind)
		}
	}
	if root.member("t").member("x") != nil || root.member("t").memberIndex("x") != -1 {
		t.Error("a member of a non-object")
	}
}
