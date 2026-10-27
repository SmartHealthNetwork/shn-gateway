//go:build !shn_coreonly

package engine

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// paExtensionLinesJSON is every extension the Da Vinci PAS and DTR packages
// of each line define, with the contexts it may be used in, generated from
// the packages the validator lanes load (tools/pamarkergen in the platform
// repository; its header says how it is regenerated and checked).
//
//go:embed workstream_pa_extension_lines.json
var paExtensionLinesJSON []byte

// paExtensionContexts is, per extension URL and line, the contexts that
// line's packages define the extension for.
var paExtensionContexts = sync.OnceValue(func() map[string]map[string][]string {
	contexts, err := parsePAExtensionLines(paExtensionLinesJSON)
	if err != nil {
		// The extract is compiled in and its tests parse it: a failure here is
		// a broken build, never a runtime condition to carry on through.
		panic(err)
	}
	return contexts
})

func parsePAExtensionLines(data []byte) (map[string]map[string][]string, error) {
	var x struct {
		Lines map[string]struct {
			Extensions []struct {
				URL     string   `json:"url"`
				Context []string `json:"context"`
			} `json:"extensions"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("workstream_pa_extension_lines.json: %w", err)
	}
	if len(x.Lines) == 0 {
		return nil, fmt.Errorf("workstream_pa_extension_lines.json: no lines")
	}
	out := map[string]map[string][]string{}
	for line, l := range x.Lines {
		for _, e := range l.Extensions {
			if out[e.URL] == nil {
				out[e.URL] = map[string][]string{}
			}
			out[e.URL][line] = e.Context
		}
	}
	return out, nil
}

// paLinesDefining is the lines, of order, whose packages define extension
// for use at position (a resource type, or an element path such as
// Claim.item), in order's order.
func paLinesDefining(extension, position string, order []string) []string {
	var out []string
	for _, line := range order {
		for _, c := range paExtensionContexts()[extension][line] {
			if c == position {
				out = append(out, line)
				break
			}
		}
	}
	return out
}
