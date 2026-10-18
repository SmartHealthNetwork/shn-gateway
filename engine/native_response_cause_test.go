package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
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
			for i := 0; i <= pasGraphMaxResources; i++ {
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
	deep := []byte(`{"resourceType":"Bundle","type":"collection","nested":` + strings.Repeat(`[`, pasGraphMaxDepth+1) + `0` + strings.Repeat(`]`, pasGraphMaxDepth+1) + `}`)
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

// The one per-line definition: at every line, the gateway's own check
// of a submit or amendment answer (validateNativePASResponseAt) and
// InspectNativePASResponseAt read the same answers, and the cause and the
// reference a refusal names agree with them. Over the reference payer's
// answers, each with its Claim, its insurer and its request in turn set
// aside, as the IG lines close an answer differently.
func TestOnePerLineAnswerDefinition(t *testing.T) {
	var bodies [][]byte
	for _, name := range []string{"pas-submit-response.json", "pas-submit-response-pended.json",
		"pas-update-response-amend-before-resolution.json", "pas-update-response-amend-after-resolution.json"} {
		raw, err := os.ReadFile("testdata/br-payer/" + name)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, raw)
		var b map[string]any
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		for _, change := range []func(cr map[string]any){
			func(cr map[string]any) { cr["insurer"] = map[string]any{"reference": "Organization/elsewhere"} },
			func(cr map[string]any) { cr["request"] = map[string]any{"reference": "Claim/elsewhere"} },
			func(cr map[string]any) { cr["request"] = map[string]any{"reference": "Patient/elsewhere"} },
			func(cr map[string]any) {
				cr["request"] = map[string]any{"identifier": map[string]any{"system": "urn:x", "value": "c1"}}
			},
			func(cr map[string]any) { delete(cr, "request") },
		} {
			var c map[string]any
			_ = json.Unmarshal(raw, &c)
			for _, e := range c["entry"].([]any) {
				if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "ClaimResponse" {
					change(r)
				}
			}
			out, _ := json.Marshal(c)
			bodies = append(bodies, out)
		}
	}
	// A member name repeated in the ClaimResponse, exactly or but for case:
	// the gateway refuses both, and so does the one definition, as undecodable.
	compact, err := json.Marshal(json.RawMessage(bodies[0]))
	if err != nil {
		t.Fatal(err)
	}
	repeats := map[string]bool{}
	for _, extra := range []string{`"status":"active",`, `"Status":"active",`} {
		out := bytes.Replace(compact, []byte(`"resourceType":"ClaimResponse",`), []byte(`"resourceType":"ClaimResponse",`+extra), 1)
		if bytes.Equal(out, compact) {
			t.Fatal("the submit answer carries no ClaimResponse to repeat a member in")
		}
		bodies = append(bodies, out)
		repeats[string(out)] = true
	}
	read, refused := 0, 0
	for i, body := range bodies {
		for _, line := range []string{"", "2.0", "2.1", "2.2"} {
			_, lr := validateNativePASResponseAt(body, line)
			_, err := InspectNativePASResponseAt(body, line)
			cause := NativePASRefusalCauseAt(body, line)
			_, _, _, named := NativePASUnresolvedReferenceAt(body, line)
			if (lr.Status == 0) != (err == nil) || (err == nil) != (cause == "") || named && cause != PASRefusalReference {
				t.Errorf("body %d at %q: gateway status %d, export %v, cause %q, reference named %v", i, line, lr.Status, err, cause, named)
			}
			if repeats[string(body)] && (lr.Status == 0 || cause != PASRefusalUndecodable) {
				t.Errorf("body %d at %q repeats a member: gateway status %d, cause %q", i, line, lr.Status, cause)
			}
			if err == nil {
				read++
			} else {
				refused++
			}
		}
	}
	if read == 0 || refused == 0 {
		t.Fatalf("read %d, refused %d: the rows decide nothing", read, refused)
	}
}
