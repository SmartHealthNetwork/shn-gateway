//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// oneSpellingPerMemberByTokens is the duplicate-member check as
// encoding/json's Decoder reads a document, a token at a time: the reference
// the one-pass walk (oneSpellingPerMember) must agree with on every document.
func oneSpellingPerMemberByTokens(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		object  bool
		wantKey bool
		names   map[string]bool
	}
	var stack []*frame
	for {
		t, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("is not JSON: %w", err)
		}
		top := (*frame)(nil)
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.wantKey {
			if d, ok := t.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				if n := len(stack); n > 0 && stack[n-1].object {
					stack[n-1].wantKey = true
				}
				continue
			}
			name := foldName(t.(string))
			if top.names[name] {
				return fmt.Errorf("names the member %q twice, or in another case", t.(string))
			}
			top.names[name] = true
			top.wantKey = false
			continue
		}
		switch d, _ := t.(json.Delim); d {
		case '{':
			stack = append(stack, &frame{object: true, wantKey: true, names: map[string]bool{}})
			continue
		case '[':
			stack = append(stack, &frame{})
			continue
		case ']':
			stack = stack[:len(stack)-1]
		}
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
}
