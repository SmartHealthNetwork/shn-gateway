package engine

import (
	"bytes"
	"encoding/json"
	"sort"
)

// jsonOccurrence is an offset into CheckInput.Body. Structural decoding has
// already checked syntax, duplicate keys, depth, tokens, and the body limit.
// This scanner only locates schema-owned slots and lexically skips everything
// else; it never materializes a RawMessage copy or searches by content.
type jsonOccurrence struct{ begin, end int }

func jsonSpace(b byte) bool { return b == ' ' || b == '\n' || b == '\r' || b == '\t' }

func skipJSONSpace(body []byte, p int) int {
	for p < len(body) && jsonSpace(body[p]) {
		p++
	}
	return p
}

func nextJSONOccurrence(body []byte, p int) (jsonOccurrence, bool) {
	p = skipJSONSpace(body, p)
	if p >= len(body) {
		return jsonOccurrence{}, false
	}
	start := p
	if body[p] != '{' && body[p] != '[' && body[p] != '"' {
		for p < len(body) && body[p] != ',' && body[p] != '}' && body[p] != ']' && !jsonSpace(body[p]) {
			p++
		}
		return jsonOccurrence{start, p}, p > start
	}
	depth, quoted, escape := 0, false, false
	for ; p < len(body); p++ {
		b := body[p]
		if quoted {
			if escape {
				escape = false
				continue
			}
			if b == '\\' {
				escape = true
				continue
			}
			if b == '"' {
				quoted = false
				if depth == 0 {
					return jsonOccurrence{start, p + 1}, true
				}
			}
			continue
		}
		switch b {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return jsonOccurrence{start, p + 1}, true
			}
		}
	}
	return jsonOccurrence{}, false
}

func jsonObjectFields(body []byte, object jsonOccurrence, visit func(string, jsonOccurrence) bool) bool {
	if object.begin < 0 || object.end > len(body) || body[object.begin] != '{' {
		return false
	}
	p := object.begin + 1
	for {
		p = skipJSONSpace(body, p)
		if p >= object.end {
			return false
		}
		if body[p] == '}' {
			return p+1 == object.end
		}
		key, ok := nextJSONOccurrence(body, p)
		if !ok || body[key.begin] != '"' || key.end >= object.end {
			return false
		}
		var name string
		if err := json.Unmarshal(body[key.begin:key.end], &name); err != nil {
			return false
		}
		p = skipJSONSpace(body, key.end)
		if p >= object.end || body[p] != ':' {
			return false
		}
		value, ok := nextJSONOccurrence(body, p+1)
		if !ok || value.end >= object.end {
			return false
		}
		if !visit(name, value) {
			return true
		}
		p = skipJSONSpace(body, value.end)
		if p < object.end && body[p] == ',' {
			p++
			continue
		}
		return p+1 == object.end && body[p] == '}'
	}
}

func jsonArrayItems(body []byte, array jsonOccurrence, visit func(jsonOccurrence) bool) bool {
	if array.begin < 0 || array.end > len(body) || body[array.begin] != '[' {
		return false
	}
	p := array.begin + 1
	for {
		p = skipJSONSpace(body, p)
		if p >= array.end {
			return false
		}
		if body[p] == ']' {
			return p+1 == array.end
		}
		value, ok := nextJSONOccurrence(body, p)
		if !ok || value.end >= array.end {
			return false
		}
		if !visit(value) {
			return false
		}
		p = skipJSONSpace(body, value.end)
		if p < array.end && body[p] == ',' {
			p++
			continue
		}
		return p+1 == array.end && body[p] == ']'
	}
}

func jsonField(body []byte, object jsonOccurrence, key string) (jsonOccurrence, bool) {
	var found jsonOccurrence
	ok := jsonObjectFields(body, object, func(name string, value jsonOccurrence) bool {
		if name == key {
			found = value
			return false
		}
		return true
	})
	return found, ok && found.end > found.begin
}

func sourceValidationTargets(in CheckInput, contract string, inquiryWrapper bool) ([][]byte, bool) {
	root := jsonOccurrence{0, len(in.Body)}
	// Root validation includes any source whitespace exactly as received.
	if contract != "pa.crd" && !inquiryWrapper {
		return [][]byte{in.Body}, true
	}
	value, ok := nextJSONOccurrence(in.Body, 0)
	if !ok {
		return nil, false
	}
	root = value
	var spans []jsonOccurrence
	add := func(s jsonOccurrence) bool {
		if len(spans) == crdEmbeddedValidationMax {
			return false
		}
		if s.begin >= s.end || s.end > len(in.Body) || in.Body[s.begin] != '{' {
			return false
		}
		spans = append(spans, s)
		return true
	}
	if contract == "pa.pas" {
		// Only a Parameters inquiry wrapper contains returned Bundle targets.
		params, has := jsonField(in.Body, root, "parameter")
		if !has {
			return [][]byte{in.Body}, true
		}
		ok = jsonArrayItems(in.Body, params, func(p jsonOccurrence) bool {
			r, found := jsonField(in.Body, p, "resource")
			return found && add(r)
		})
	} else if in.Direction == "request" {
		if context, found := jsonField(in.Body, root, "context"); found {
			for _, key := range []string{"draftOrders", "orders"} {
				if r, found := jsonField(in.Body, context, key); found && !bytes.Equal(in.Body[r.begin:r.end], []byte("null")) && !add(r) {
					return nil, false
				}
			}
		}
		if prefetch, found := jsonField(in.Body, root, "prefetch"); found {
			fields := map[string]jsonOccurrence{}
			overflow := false
			ok = jsonObjectFields(in.Body, prefetch, func(name string, value jsonOccurrence) bool {
				if bytes.Equal(in.Body[value.begin:value.end], []byte("null")) {
					return true
				}
				if len(fields) == crdEmbeddedValidationMax {
					overflow = true
					return false
				}
				fields[name] = value
				return true
			})
			if !ok || overflow {
				return nil, false
			}
			keys := make([]string, 0, len(fields))
			for key := range fields {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				r := fields[key]
				if !add(r) {
					return nil, false
				}
			}
		}
	} else {
		collectActions := func(array jsonOccurrence) bool {
			return jsonArrayItems(in.Body, array, func(action jsonOccurrence) bool {
				if r, found := jsonField(in.Body, action, "resource"); found && !bytes.Equal(in.Body[r.begin:r.end], []byte("null")) {
					return add(r)
				}
				return true
			})
		}
		if actions, found := jsonField(in.Body, root, "systemActions"); found && !collectActions(actions) {
			return nil, false
		}
		if cards, found := jsonField(in.Body, root, "cards"); found {
			ok = jsonArrayItems(in.Body, cards, func(card jsonOccurrence) bool {
				suggestions, found := jsonField(in.Body, card, "suggestions")
				if !found {
					return true
				}
				return jsonArrayItems(in.Body, suggestions, func(suggestion jsonOccurrence) bool {
					actions, found := jsonField(in.Body, suggestion, "actions")
					return !found || collectActions(actions)
				})
			})
		}
	}
	if !ok {
		return nil, false
	}
	out := make([][]byte, len(spans))
	for i, s := range spans {
		out[i] = in.Body[s.begin:s.end]
	}
	return out, true
}
