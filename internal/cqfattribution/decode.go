package cqfattribution

import (
	"bytes"
	"encoding/json"
	"io"
)

// Strict bounded decoder preserves numbers and rejects duplicate keys and trailing values.
func decodeObject(raw []byte, dst *map[string]any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := decodeValue(d, 0)
	if err != nil {
		return invalidResponse()
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return invalidResponse()
	}
	if _, err = d.Token(); err != io.EOF {
		return invalidResponse()
	}
	*dst = obj
	return nil
}
func decodeValue(d *json.Decoder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, invalidResponse()
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		obj := make(map[string]any)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			s, ok := key.(string)
			if !ok {
				return nil, invalidResponse()
			}
			if _, exists := obj[s]; exists {
				return nil, invalidResponse()
			}
			child, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			obj[s] = child
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, invalidResponse()
		}
		return obj, nil
	case json.Delim('['):
		arr := []any{}
		for d.More() {
			child, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, child)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, invalidResponse()
		}
		return arr, nil
	default:
		if _, ok := t.(json.Delim); ok {
			return nil, invalidResponse()
		}
		return t, nil
	}
}
