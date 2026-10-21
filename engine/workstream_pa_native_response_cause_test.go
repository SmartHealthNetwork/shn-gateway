package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

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
