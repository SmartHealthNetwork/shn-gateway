//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// spliceBundleEntries places each record as given and refuses one whose bytes
// would not decode to exactly the text that was checked of it, as the SDK's
// Carried builders refuse such a record: not UTF-8, a lone surrogate escape,
// a member named twice in an object whatever its case. The body must hold one
// top-level entry array of exactly the entries it was read with.
func TestSpliceBundleEntries(t *testing.T) {
	entry := func(res string) []bundleEntryBytes {
		return []bundleEntryBytes{{fullURL: []byte(`"https://shn.example/fhir/Basic/b"`), resource: []byte(res)}}
	}
	one := []byte(`{"resourceType":"Bundle","entry":[{"fullUrl":"x","resource":{"resourceType":"Basic","id":"a"}}]}`)
	for name, row := range map[string]struct {
		body     []byte
		had      int
		resource string
		ok       bool
	}{
		"white space around the record":     {one, 1, " \n{\"resourceType\":\"Basic\",\"id\":\"b\"}\t\r\n", true},
		"a no-break space after the record": {one, 1, "{\"resourceType\":\"Basic\",\"id\":\"b\"}\u00a0", false},
		"appended":                          {one, 1, `{"resourceType":"Basic","id":"b"}`, true},
		"into an empty array":               {[]byte(`{"resourceType":"Bundle","entry":[ ]}`), 0, `{"resourceType":"Basic","id":"b"}`, true},
		"a surrogate pair":                  {one, 1, `{"resourceType":"Basic","id":"b","x":"😀"}`, true},
		"an escaped backslash before u":     {one, 1, `{"resourceType":"Basic","id":"b","x":"\\ud800"}`, true},
		"a lone high surrogate":             {one, 1, `{"resourceType":"Basic","id":"b","x":"\ud800"}`, false},
		"a lone low surrogate":              {one, 1, `{"resourceType":"Basic","id":"b","x":"\udc00"}`, false},
		"a high surrogate before a letter":  {one, 1, `{"resourceType":"Basic","id":"b","x":"\ud800a"}`, false},
		"a member named twice":              {one, 1, `{"resourceType":"Basic","id":"b","id":"c"}`, false},
		"a member in two cases":             {one, 1, `{"resourceType":"Basic","id":"b","Id":"c"}`, false},
		"a nested member in two cases":      {one, 1, `{"resourceType":"Basic","id":"b","x":[{"a":1,"A":2}]}`, false},
		"a member in two folded spellings":  {one, 1, "{\"resourceType\":\"Basic\",\"id\":\"b\",\"subject\":1,\"\u017fubject\":2}", false},
		"a Kelvin sign spelling":            {one, 1, "{\"resourceType\":\"Basic\",\"id\":\"b\",\"k\":1,\"\u212a\":2}", false},
		"two objects":                       {one, 1, `{"resourceType":"Basic","id":"b"}{"x":1}`, false},
		"not an object":                     {one, 1, `["resourceType"]`, false},
		"empty":                             {one, 1, `  `, false},
		"not UTF-8":                         {one, 1, "{\"resourceType\":\"Basic\",\"id\":\"b\",\"x\":\"\xff\"}", false},
		"a body naming entry twice":         {[]byte(`{"entry":[{"q":1}],"entry":[]}`), 0, `{"resourceType":"Basic","id":"b"}`, false},
		"a body naming entry in two cases":  {[]byte(`{"entry":[],"Entry":[]}`), 0, `{"resourceType":"Basic","id":"b"}`, false},
		"a count not the body's":            {one, 0, `{"resourceType":"Basic","id":"b"}`, false},
	} {
		out, err := spliceBundleEntries(row.body, row.had, entry(row.resource))
		if (err == nil) != row.ok {
			t.Errorf("%s: %v, want ok %v", name, err, row.ok)
			continue
		}
		// A record the splice cannot carry as read is the fidelity of the
		// gateway's own splice; a body it cannot splice into is its own failure.
		bodyFault := strings.HasPrefix(name, "a body") || strings.HasPrefix(name, "a count")
		if !row.ok && errors.Is(err, errRecordNotCarried) == bodyFault {
			t.Errorf("%s: %v, want errRecordNotCarried %v", name, err, !bodyFault)
		}
		if !row.ok {
			continue
		}
		var b struct {
			Entry []json.RawMessage `json:"entry"`
		}
		if json.Unmarshal(out, &b) != nil || len(b.Entry) != row.had+1 || !strings.Contains(string(b.Entry[row.had]), strings.TrimSpace(row.resource)) {
			t.Errorf("%s: spliced %s", name, out)
		}
	}
}
