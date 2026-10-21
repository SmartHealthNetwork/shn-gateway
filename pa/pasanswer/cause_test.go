package pasanswer

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"
)

// NativePASRefusalCause names why the response graph rule refuses a Bundle by
// one cause of a closed set, one row per cause it can give from
// InspectNativePASResponse: each a valid answer with one change. A Bundle it
// reads has none.
func TestNativePASRefusalCauseNamesEachCause(t *testing.T) {
	valid := pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal))
	if c := NativePASRefusalCause(valid); c != "" {
		t.Fatalf("a Bundle the rule reads: cause %q", c)
	}
	mutate := func(f func(b map[string]any, cr map[string]any)) []byte {
		var b map[string]any
		if err := json.Unmarshal(valid, &b); err != nil {
			t.Fatal(err)
		}
		var cr map[string]any
		for _, e := range b["entry"].([]any) {
			if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "ClaimResponse" {
				cr = r
			}
		}
		f(b, cr)
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	entries := func(b map[string]any) []any { return b["entry"].([]any) }
	for want, body := range map[string][]byte{
		PASRefusalUndecodable:   []byte(`{"resourceType":"Bundle","resourceType":"Bundle"}`),
		PASRefusalNotCollection: mutate(func(b, _ map[string]any) { b["type"] = "searchset" }),
		PASRefusalNoEntries:     mutate(func(b, _ map[string]any) { b["entry"] = []any{} }),
		PASRefusalEntry:         mutate(func(b, _ map[string]any) { b["entry"] = append(entries(b), "not an object") }),
		PASRefusalFullURL:       mutate(func(b, _ map[string]any) { entries(b)[0].(map[string]any)["fullUrl"] = "ClaimResponse/relative" }),
		PASRefusalIdentity:      mutate(func(_, cr map[string]any) { cr["id"] = "has a space" }),
		PASRefusalRepeatedIdentity: mutate(func(b, _ map[string]any) {
			b["entry"] = append(entries(b), entries(b)[len(entries(b))-1])
		}),
		PASRefusalNoClaimResponse: mutate(func(b, _ map[string]any) {
			b["entry"] = slices.DeleteFunc(entries(b), func(e any) bool {
				return e.(map[string]any)["resource"].(map[string]any)["resourceType"] == "ClaimResponse"
			})
		}),
		PASRefusalClaimResponses: mutate(func(b, cr map[string]any) {
			again := map[string]any{}
			for k, v := range cr {
				again[k] = v
			}
			again["id"] = "second"
			b["entry"] = append(entries(b), map[string]any{"fullUrl": "urn:uuid:20000000-0000-4000-8000-000000000002", "resource": again})
		}),
		PASRefusalContained:      mutate(func(_, cr map[string]any) { cr["contained"] = "not an array" }),
		PASRefusalNested:         mutate(func(_, cr map[string]any) { cr["note"] = []any{map[string]any{"resourceType": "Bundle"}} }),
		PASRefusalEmptyReference: mutate(func(_, cr map[string]any) { cr["insurer"] = map[string]any{"reference": ""} }),
		PASRefusalReference:      mutate(func(_, cr map[string]any) { cr["insurer"] = map[string]any{"reference": "Organization/nowhere"} }),
		PASRefusalBound: mutate(func(b, _ map[string]any) {
			for i := 0; i <= pasgraph.MaxResources; i++ {
				b["entry"] = append(entries(b), map[string]any{"fullUrl": fmt.Sprintf("urn:uuid:30000000-0000-4000-8000-%012d", i),
					"resource": map[string]any{"resourceType": "Organization", "id": fmt.Sprintf("o%d", i)}})
			}
		}),
		PASRefusalNoDecision: mutate(func(_, cr map[string]any) { delete(cr, "outcome"); delete(cr, "item") }),
		PASRefusalDecision:   mutate(func(_, cr map[string]any) { delete(cr, "item"); cr["outcome"] = "not-an-outcome" }),
	} {
		if got := NativePASRefusalCause(body); got != want {
			t.Errorf("%s: cause %q", want, got)
		}
		if !slices.Contains(NativePASRefusalCauses, want) {
			t.Errorf("%s is not in NativePASRefusalCauses", want)
		}
	}
	// Nesting deeper than the rule reads is a bound, as its other bounds are,
	// though the decoder meets it before the rule walks an entry.
	deep := []byte(`{"resourceType":"Bundle","type":"collection","nested":` + strings.Repeat(`[`, pasgraph.MaxDepth+1) + `0` + strings.Repeat(`]`, pasgraph.MaxDepth+1) + `}`)
	if c := NativePASRefusalCause(deep); c != PASRefusalBound {
		t.Errorf("a Bundle nesting deeper than the rule reads: cause %q", c)
	}
	// The graph check for a Bundle that carries no ClaimResponse.
	noResponse := mutate(func(b, _ map[string]any) {
		b["entry"] = slices.DeleteFunc(entries(b), func(e any) bool {
			return e.(map[string]any)["resource"].(map[string]any)["resourceType"] == "ClaimResponse"
		})
	})
	if c := NativePASGraphRefusalCause(noResponse); c != "" {
		t.Errorf("a Bundle with no ClaimResponse the check holds on: cause %q", c)
	}
	if c := NativePASGraphRefusalCause(valid); c != PASRefusalClaimResponses {
		t.Errorf("a Bundle carrying a ClaimResponse: cause %q", c)
	}
}

// The causes are a closed set: each named once.
func TestNativePASRefusalCausesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range NativePASRefusalCauses {
		if c == "" || seen[c] {
			t.Errorf("cause %q empty or repeated", c)
		}
		seen[c] = true
	}
}

// The causes named here are the graph rule's own: each constant is the cause
// internal/pasgraph refuses with, so a cause read here names what the rule
// found.
func TestRefusalCausesAreTheGraphRules(t *testing.T) {
	graph := []string{
		pasgraph.RefusalUndecodable, pasgraph.RefusalNotCollection, pasgraph.RefusalNoEntries, pasgraph.RefusalEntry, pasgraph.RefusalFullURL,
		pasgraph.RefusalIdentity, pasgraph.RefusalRepeatedIdentity, pasgraph.RefusalNoClaimResponse, pasgraph.RefusalClaimResponses,
		pasgraph.RefusalRequest, pasgraph.RefusalContained, pasgraph.RefusalNested, pasgraph.RefusalEmptyReference, pasgraph.RefusalReference,
		pasgraph.RefusalBound, pasgraph.RefusalPolicy, pasgraph.RefusalNoDecision, pasgraph.RefusalDecision,
	}
	if !slices.Equal(NativePASRefusalCauses, graph) {
		t.Fatalf("causes %v, the graph rule's %v", NativePASRefusalCauses, graph)
	}
}
