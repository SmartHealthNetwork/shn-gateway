// Package observationjson serializes only the value types produced by the
// gateway's bounded JSON decoder. It never pools or retains payload buffers.
package observationjson

import (
	"encoding/json"
	"sort"
	"unicode/utf8"
)

// Size returns the exact encoded size, or false for unsupported/over-limit input.
// It allocates no body buffer. Callers reserve this size before allocating dst.
func Size(v any, limit int) (int, bool) {
	n := 0
	var visit func(any) bool
	add := func(k int) bool {
		if k > limit-n {
			return false
		}
		n += k
		return true
	}
	visit = func(v any) bool {
		switch v := v.(type) {
		case nil:
			return add(4)
		case bool:
			if v {
				return add(4)
			}
			return add(5)
		case string:
			if !add(2) {
				return false
			}
			for len(v) > 0 {
				_, k, size := stringRune(v)
				if !add(size) {
					return false
				}
				v = v[k:]
			}
			return true
		case json.Number:
			if !number(v.String()) {
				return false
			}
			return add(len(v))
		case []any:
			if !add(2) {
				return false
			}
			for i, x := range v {
				if i > 0 && !add(1) {
					return false
				}
				if !visit(x) {
					return false
				}
			}
			return true
		case map[string]any:
			if !add(2) {
				return false
			}
			i := 0
			for k, x := range v {
				if i > 0 && !add(1) {
					return false
				}
				i++
				if !visit(k) || !add(1) || !visit(x) {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	ok := limit >= 0 && visit(v)
	return n, ok
}

// Append writes a previously sized, unchanged decoded value into reserved dst.
// Sorted keys match encoding/json; only key references need temporary storage.
func Append(dst []byte, v any) []byte {
	switch v := v.(type) {
	case nil:
		return append(dst, "null"...)
	case bool:
		if v {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case string:
		return appendString(dst, v)
	case json.Number:
		return append(dst, v...)
	case []any:
		dst = append(dst, '[')
		for i, x := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = Append(dst, x)
		}
		return append(dst, ']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst = append(dst, '{')
		for i, k := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendString(dst, k)
			dst = append(dst, ':')
			dst = Append(dst, v[k])
		}
		return append(dst, '}')
	default:
		panic("observation JSON value was not sized")
	}
}

func stringRune(s string) (rune, int, int) {
	r, k := utf8.DecodeRuneInString(s)
	switch r {
	case '"', '\\', '\b', '\f', '\n', '\r', '\t':
		return r, k, 2
	default:
		if r < 0x20 || r == '<' || r == '>' || r == '&' || r == 0x2028 || r == 0x2029 || (r == utf8.RuneError && k == 1) {
			return r, k, 6
		}
		return r, k, k
	}
}
func appendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for len(s) > 0 {
		r, k, size := stringRune(s)
		switch r {
		case '"', '\\':
			dst = append(dst, '\\', byte(r))
		case '\b':
			dst = append(dst, `\b`...)
		case '\f':
			dst = append(dst, `\f`...)
		case '\n':
			dst = append(dst, `\n`...)
		case '\r':
			dst = append(dst, `\r`...)
		case '\t':
			dst = append(dst, `\t`...)
		default:
			if size == 6 {
				const hex = "0123456789abcdef"
				dst = append(dst, '\\', 'u', hex[(r>>12)&15], hex[(r>>8)&15], hex[(r>>4)&15], hex[r&15])
			} else {
				dst = append(dst, s[:k]...)
			}
		}
		s = s[k:]
	}
	return append(dst, '"')
}
func number(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	if i == len(s) {
		return false
	}
	if s[i] == '0' {
		i++
	} else {
		if s[i] < '1' || s[i] > '9' {
			return false
		}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(s)
}
