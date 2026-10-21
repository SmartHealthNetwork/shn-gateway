package engine

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
	"github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer"
)

// errText is an error as a comparison reads it: its text, "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Every deprecated engine reader forwards to package pasanswer: over the
// response graph fixtures, read, refused and refused for each kind of cause,
// at every line, each returns exactly what the pasanswer reader returns, and
// the constants and the cause list are pasanswer's.
func TestDeprecatedReadersForwardToPasanswer(t *testing.T) {
	encode := func(b map[string]any) []byte { raw, _ := json.Marshal(b); return raw }
	open := assemblySmallGraph()
	open["entry"] = open["entry"].([]any)[:1]
	noResponse := assemblySmallGraph()
	noResponse["entry"] = noResponse["entry"].([]any)[1:]
	// decided is the approved answer with its ClaimResponse's decision edited:
	// the graph reads, so the refusal is the decision's own branch.
	decided := func(edit func(cr map[string]any)) []byte {
		var b map[string]any
		if err := json.Unmarshal(pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal)), &b); err != nil {
			t.Fatal(err)
		}
		for _, e := range b["entry"].([]any) {
			if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "ClaimResponse" {
				edit(r)
			}
		}
		return encode(b)
	}
	bodies := map[string][]byte{
		"no decision":      decided(func(cr map[string]any) { delete(cr, "outcome"); delete(cr, "item") }),
		"unread decision":  decided(func(cr map[string]any) { delete(cr, "item"); cr["outcome"] = "not-an-outcome" }),
		"pending":          []byte(assemblyRealPending),
		"approved":         pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal)),
		"small closed":     encode(assemblySmallGraph()),
		"small open":       encode(open),
		"no ClaimResponse": encode(noResponse),
		"repeated member":  []byte(`{"resourceType":"Bundle","resourceType":"Bundle"}`),
		"not a Bundle":     []byte(`{"resourceType":"Parameters"}`),
		"bare":             []byte(`{"resourceType":"ClaimResponse","id":"bare","outcome":"queued"}`),
		"not JSON":         []byte(`not json`),
	}
	for _, name := range []string{"pas-submit-response.json", "pas-submit-response-pended.json",
		"pas-update-response-amend-before-resolution.json", "pas-inquire-response.json"} {
		raw, err := os.ReadFile("testdata/br-payer/" + name)
		if err != nil {
			t.Fatal(err)
		}
		bodies[name] = raw
	}
	read, refused := 0, 0
	causes := map[string]bool{}
	for name, body := range bodies {
		causes[pasanswer.NativePASRefusalCause(body)] = true
		if got, want := NativePASRefusalCause(body), pasanswer.NativePASRefusalCause(body); got != want {
			t.Errorf("%s: NativePASRefusalCause %q, pasanswer %q", name, got, want)
		}
		if got, want := NativePASGraphRefusalCause(body), pasanswer.NativePASGraphRefusalCause(body); got != want {
			t.Errorf("%s: NativePASGraphRefusalCause %q, pasanswer %q", name, got, want)
		}
		if got, want := errText(CheckNativePASGraphWithoutClaimResponse(body)), errText(pasanswer.CheckNativePASGraphWithoutClaimResponse(body)); got != want {
			t.Errorf("%s: CheckNativePASGraphWithoutClaimResponse %q, pasanswer %q", name, got, want)
		}
		f, err := InspectNativePASResponse(body)
		pf, perr := pasanswer.InspectNativePASResponse(body)
		if f != NativePASFacts(pf) || errText(err) != errText(perr) {
			t.Errorf("%s: InspectNativePASResponse %+v %v, pasanswer %+v %v", name, f, err, pf, perr)
		}
		h, e, r, ok := NativePASUnresolvedReference(body)
		ph, pe, pr, pok := pasanswer.NativePASUnresolvedReference(body)
		if h != ph || e != pe || r != pr || ok != pok {
			t.Errorf("%s: NativePASUnresolvedReference %q %q %q %v, pasanswer %q %q %q %v", name, h, e, r, ok, ph, pe, pr, pok)
		}
		for _, line := range []string{"", "2.0", "2.1", "2.2", "2.3"} {
			f, err := InspectNativePASResponseAt(body, line)
			pf, perr := pasanswer.InspectNativePASResponseAt(body, line)
			if f != NativePASFacts(pf) || errText(err) != errText(perr) {
				t.Errorf("%s at %q: InspectNativePASResponseAt %+v %v, pasanswer %+v %v", name, line, f, err, pf, perr)
			}
			if got, want := NativePASRefusalCauseAt(body, line), pasanswer.NativePASRefusalCauseAt(body, line); got != want {
				t.Errorf("%s at %q: NativePASRefusalCauseAt %q, pasanswer %q", name, line, got, want)
			}
			h, e, r, ok := NativePASUnresolvedReferenceAt(body, line)
			ph, pe, pr, pok := pasanswer.NativePASUnresolvedReferenceAt(body, line)
			if h != ph || e != pe || r != pr || ok != pok {
				t.Errorf("%s at %q: NativePASUnresolvedReferenceAt %q %q %q %v, pasanswer %q %q %q %v", name, line, h, e, r, ok, ph, pe, pr, pok)
			}
			if err == nil {
				read++
			} else {
				refused++
			}
		}
		dup, dok := repeatedMember(body)
		pdup, pdok := jsonmember.Repeated(body)
		if dup != pdup || dok != pdok {
			t.Errorf("%s: repeatedMember %q %v, jsonmember %q %v", name, dup, dok, pdup, pdok)
		}
	}
	// The rows reach the decision branches, not only the graph's refusals.
	for _, want := range []string{"", pasanswer.PASRefusalNoDecision, pasanswer.PASRefusalDecision, pasanswer.PASRefusalReference, pasanswer.PASRefusalUndecodable} {
		if !causes[want] {
			t.Errorf("no row reaches cause %q (reached %v)", want, causes)
		}
	}
	if read == 0 || refused == 0 {
		t.Fatalf("read %d, refused %d: the rows compare one branch only", read, refused)
	}
	for _, c := range []struct{ engine, pasanswer string }{
		{PASRefusalUndecodable, pasanswer.PASRefusalUndecodable}, {PASRefusalNotCollection, pasanswer.PASRefusalNotCollection},
		{PASRefusalNoEntries, pasanswer.PASRefusalNoEntries}, {PASRefusalEntry, pasanswer.PASRefusalEntry},
		{PASRefusalFullURL, pasanswer.PASRefusalFullURL}, {PASRefusalIdentity, pasanswer.PASRefusalIdentity},
		{PASRefusalRepeatedIdentity, pasanswer.PASRefusalRepeatedIdentity}, {PASRefusalNoClaimResponse, pasanswer.PASRefusalNoClaimResponse},
		{PASRefusalClaimResponses, pasanswer.PASRefusalClaimResponses}, {PASRefusalRequest, pasanswer.PASRefusalRequest},
		{PASRefusalContained, pasanswer.PASRefusalContained}, {PASRefusalNested, pasanswer.PASRefusalNested},
		{PASRefusalEmptyReference, pasanswer.PASRefusalEmptyReference}, {PASRefusalReference, pasanswer.PASRefusalReference},
		{PASRefusalBound, pasanswer.PASRefusalBound}, {PASRefusalPolicy, pasanswer.PASRefusalPolicy},
		{PASRefusalNoDecision, pasanswer.PASRefusalNoDecision}, {PASRefusalDecision, pasanswer.PASRefusalDecision},
	} {
		if c.engine != c.pasanswer {
			t.Errorf("engine cause %q, pasanswer %q", c.engine, c.pasanswer)
		}
	}
	if !slices.Equal(NativePASRefusalCauses, pasanswer.NativePASRefusalCauses) || len(NativePASRefusalCauses) != 18 {
		t.Fatalf("engine causes %v, pasanswer %v", NativePASRefusalCauses, pasanswer.NativePASRefusalCauses)
	}
	// The engine's list is its own copy: the two share no backing array.
	if &NativePASRefusalCauses[0] == &pasanswer.NativePASRefusalCauses[0] {
		t.Fatal("engine.NativePASRefusalCauses shares pasanswer's backing array")
	}
	// The engine's facts type keeps its own identity (an alias would change it,
	// an incompatible change), field for field the same as the new package's.
	et, pt := reflect.TypeOf(NativePASFacts{}), reflect.TypeOf(pasanswer.NativePASFacts{})
	if et.NumField() != pt.NumField() {
		t.Fatalf("engine.NativePASFacts has %d fields, pasanswer's %d", et.NumField(), pt.NumField())
	}
	for i := 0; i < et.NumField(); i++ {
		if ef, pf := et.Field(i), pt.Field(i); ef.Name != pf.Name || ef.Type != pf.Type {
			t.Errorf("field %d: engine %s %s, pasanswer %s %s", i, ef.Name, ef.Type, pf.Name, pf.Type)
		}
	}
}
