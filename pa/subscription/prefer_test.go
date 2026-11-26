package subscription

import "testing"

// The first return preference is taken as sent, from field lines read as one
// list; no other preference is, and a comma inside a quoted string splits
// nothing.
func TestReturnPreference(t *testing.T) {
	for _, c := range []struct {
		name   string
		values []string
		want   string
		ok     bool
	}{
		{"no Prefer", nil, "", false},
		{"minimal", []string{"return=minimal"}, "return=minimal", true},
		{"representation", []string{"return=representation"}, "return=representation", true},
		{"OperationOutcome", []string{"return=OperationOutcome"}, "return=OperationOutcome", true},
		{"another value, as sent", []string{"return=other"}, "return=other", true},
		{"a quoted value, as sent", []string{`return="minimal"`}, `return="minimal"`, true},
		{"no value", []string{"return"}, "return", true},
		{"with parameters, as sent", []string{"return=minimal; foo=bar"}, "return=minimal; foo=bar", true},
		{"the name in another case", []string{"RETURN=minimal"}, "RETURN=minimal", true},
		{"spaces around the equals sign", []string{"return = minimal"}, "return = minimal", true},
		{"spaces and tabs around it", []string{" \treturn=minimal\t "}, "return=minimal", true},
		{"after another preference", []string{"respond-async, return=representation"}, "return=representation", true},
		{"on a later field line", []string{"handling=strict", "return=OperationOutcome"}, "return=OperationOutcome", true},
		{"the first of two", []string{"return=minimal, return=representation"}, "return=minimal", true},
		{"the first of two field lines", []string{"return=minimal", "return=representation"}, "return=minimal", true},
		{"after a quoted comma", []string{`foo="a,return=x", return=minimal`}, "return=minimal", true},
		{"after a quoted, escaped quote", []string{`foo="a\",return=x", return=minimal`}, "return=minimal", true},
		{"between empty list members", []string{" , ,return=minimal,"}, "return=minimal", true},
		{"only other preferences", []string{"respond-async, handling=lenient, wait=10"}, "", false},
		{"a longer name", []string{"returned=minimal"}, "", false},
		{"return inside a quoted value", []string{`foo="return=minimal"`}, "", false},
	} {
		got, ok := ReturnPreference(c.values)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// A framed Prefer is exactly one return preference, as ReturnPreference takes
// one, or it is not one the notifier's gateway carries on.
func TestIsReturnPreference(t *testing.T) {
	for v, want := range map[string]bool{
		"return=minimal":                 true,
		"RETURN=OperationOutcome":        true,
		"return=minimal; foo=bar":        true,
		`return="x,y"`:                   true,
		"return":                         true,
		"":                               false,
		" return=minimal":                false,
		"return=minimal ":                false,
		"return=minimal, respond-async":  false,
		"respond-async, return=minimal":  false,
		"return=minimal,":                false,
		"respond-async":                  false,
		"handling=strict":                false,
		"returned=minimal":               false,
		`foo="return=minimal"`:           false,
		"return=minimal, return=minimal": false,
	} {
		if got := IsReturnPreference(v); got != want {
			t.Errorf("%q: %v, want %v", v, got, want)
		}
	}
}
