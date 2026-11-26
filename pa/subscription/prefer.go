package subscription

import "strings"

// ReturnPreference is the first return preference (RFC 7240 section 4.2) a
// subscriber's system sent in its Prefer field lines, values, read as one
// list as HTTP reads several lines of one field: the preference exactly as
// sent, its name, any value and parameters included, with only the spaces
// and tabs around it removed. A subscribe leg carries it, and only it: FHIR's
// return=minimal, return=representation and return=OperationOutcome, or any
// other value, which the notifier's system may ignore as RFC 7240 lets it.
// No other preference (handling, respond-async, wait or any other) crosses
// the network. ok is false when the field names no return preference. A
// preference given more than once counts once, the first (RFC 7240 section
// 2).
func ReturnPreference(values []string) (pref string, ok bool) {
	for _, p := range preferences(strings.Join(values, ",")) {
		if isReturn(p) {
			return p, true
		}
	}
	return "", false
}

// IsReturnPreference reports whether v, a Prefer value a subscribe leg's
// frame carries, is exactly one return preference, as ReturnPreference
// takes one: one preference, named return, with nothing around it.
func IsReturnPreference(v string) bool {
	ps := preferences(v)
	return len(ps) == 1 && ps[0] == v && isReturn(v)
}

// preferences splits a Prefer field value into its preferences: at each comma
// outside a quoted string (RFC 9110 section 5.6.4, a backslash quoting the
// next character), each trimmed of spaces and tabs, empty ones left out.
func preferences(field string) []string {
	var out []string
	start, quoted := 0, false
	for i := 0; i < len(field); i++ {
		switch c := field[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			if p := strings.Trim(field[start:i], " \t"); p != "" {
				out = append(out, p)
			}
			start = i + 1
		}
	}
	if p := strings.Trim(field[start:], " \t"); p != "" {
		out = append(out, p)
	}
	return out
}

// isReturn reports whether p, one preference, is named return: its name, the
// token before any "=" or ";" with the spaces and tabs around it removed,
// matched in any case, as RFC 7240 matches a preference's name.
func isReturn(p string) bool {
	name := p
	if i := strings.IndexAny(p, "=;"); i >= 0 {
		name = p[:i]
	}
	return strings.EqualFold(strings.Trim(name, " \t"), "return")
}
