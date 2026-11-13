//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// readCall is one $validate the reading asked a lane for: the profile, and
// the body as sent.
type readCall struct {
	profile string
	body    []byte
}

// readingLane records every call and answers it with answer, valid when nil.
type readingLane struct {
	mu     sync.Mutex
	calls  []readCall
	answer func(body []byte, profile string) (shnsdk.Result, error)
}

func (l *readingLane) Validate(_ context.Context, body []byte, profile string) (shnsdk.Result, error) {
	l.mu.Lock()
	l.calls = append(l.calls, readCall{profile: profile, body: append([]byte(nil), body...)})
	l.mu.Unlock()
	if l.answer == nil {
		return shnsdk.Result{Valid: true}, nil
	}
	return l.answer(body, profile)
}

// asked is each call as "<resourceType> at <profile>".
func (l *readingLane) asked(t *testing.T) []string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, c := range l.calls {
		out = append(out, resourceTypeOf(c.body)+" at "+c.profile)
	}
	return out
}

// invalidWith is an invalid result holding exactly issues.
func invalidWith(issues ...shnsdk.Issue) shnsdk.Result {
	res := shnsdk.Result{Details: issues}
	for _, iss := range issues {
		if iss.Severity == "error" || iss.Severity == "fatal" {
			res.Issues = append(res.Issues, iss.Diagnostics)
		}
	}
	return res
}

func requestBundleProfile(t *testing.T, line string) string {
	t.Helper()
	p, ok := profileFor("PASRequestBundle", line, "pas-claim")
	if !ok {
		t.Fatalf("fixture: no %s request Bundle profile", line)
	}
	return p
}

func claimProfile(t *testing.T, line, leg string) string {
	t.Helper()
	p, ok := profileFor("Claim", line, leg)
	if !ok {
		t.Fatalf("fixture: no %s Claim profile on %s", line, leg)
	}
	return p
}

// requestBundle is line's conformant submission request Bundle, with edit
// applied to it decoded.
func requestBundle(t *testing.T, line string, edit func(b map[string]any)) []byte {
	t.Helper()
	raw := pasGolden(t, line+"/conformant/pas-submit-request.json")
	if edit == nil {
		return raw
	}
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	edit(b)
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func entries(b map[string]any) []any { return b["entry"].([]any) }

func entryAt(b map[string]any, i int) map[string]any { return entries(b)[i].(map[string]any) }

func resourceAt(b map[string]any, i int) map[string]any {
	return entryAt(b, i)["resource"].(map[string]any)
}

// anotherClaim is a copy of the Bundle's first entry, its Claim given id.
func anotherClaim(t *testing.T, b map[string]any, id string) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(entryAt(b, 0))
	var e map[string]any
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	e["fullUrl"] = "https://shn.example/fhir/Claim/" + id
	e["resource"].(map[string]any)["id"] = id
	return e
}

func readBundle(t *testing.T, lane shnsdk.Validator, line string, body []byte) shnsdk.Result {
	t.Helper()
	res, err := validateAt(context.Background(), lane, body, line, requestBundleProfile(t, line))
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	return res
}

// claimProfilesIn is, for each Claim entry of the Bundle the lane read, the
// profiles it declares.
func claimProfilesIn(t *testing.T, body []byte) [][]any {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for _, e := range entries(b) {
		r := e.(map[string]any)["resource"].(map[string]any)
		if r["resourceType"] != "Claim" {
			continue
		}
		meta, _ := r["meta"].(map[string]any)
		profiles, _ := meta["profile"].([]any)
		out = append(out, profiles)
	}
	return out
}

// At 2.1 and 2.2 a request Bundle asked against its line's request Bundle
// profile is read per entry, in one call with no profile asked: the Bundle
// without the request Bundle profile, each Claim declaring, after what it
// declares, line's version of the candidate it is built to. A submission's
// Claim declares profile-claim; an update's own Claim, which names the Claim
// it changes, profile-claim-update, and the prior Claim it carries
// profile-claim; a Claim that already declares candidates, unversioned or at
// the line, declares nothing more, and one declaring a candidate at another
// line declares the line's too.
func TestPASRequestBundleReading_ReadsEachClaimAtItsOwnProfile(t *testing.T) {
	for _, line := range pasRequestBundlePerEntryLines {
		submit, update := claimProfile(t, line, "pas-claim"), claimProfile(t, line, "pas-claim-update")
		for name, c := range map[string]struct {
			edit func(t *testing.T, b map[string]any)
			want [][]any
		}{
			"a submission": {nil, [][]any{{submit}}},
			"an update and its prior Claim": {func(t *testing.T, b map[string]any) {
				prior := anotherClaim(t, b, "prior-claim")
				resourceAt(b, 0)["related"] = []any{map[string]any{"claim": map[string]any{"reference": "Claim/prior-claim"}}}
				b["entry"] = append(entries(b), prior)
			}, [][]any{{update}, {submit}}},
			"a Claim declaring both candidates": {func(t *testing.T, b map[string]any) {
				resourceAt(b, 0)["meta"] = map[string]any{"profile": []any{pasClaimCanonical, update}}
			}, [][]any{{pasClaimCanonical, update}}},
			"a Claim declaring a candidate at another line": {func(t *testing.T, b map[string]any) {
				resourceAt(b, 0)["meta"] = map[string]any{"profile": []any{pasClaimCanonical + "|9.9.9"}}
			}, [][]any{{pasClaimCanonical + "|9.9.9", submit}}},
			"a submission declaring the update candidate at another line": {func(t *testing.T, b map[string]any) {
				resourceAt(b, 0)["meta"] = map[string]any{"profile": []any{pasClaimUpdateCanonical + "|9.9.9"}}
			}, [][]any{{pasClaimUpdateCanonical + "|9.9.9", submit}}},
		} {
			t.Run(line+"/"+name, func(t *testing.T) {
				lane := &readingLane{}
				body := requestBundle(t, line, func(b map[string]any) {
					if c.edit != nil {
						c.edit(t, b)
					}
				})
				if res := readBundle(t, lane, line, body); !res.Valid {
					t.Fatalf("a valid reading read invalid: %+v", res)
				}
				if got := lane.asked(t); !slices.Equal(got, []string{"Bundle at "}) {
					t.Fatalf("asked %q, want the Bundle once with no profile", got)
				}
				if got := claimProfilesIn(t, lane.calls[0].body); fmt.Sprint(got) != fmt.Sprint(c.want) {
					t.Fatalf("the Claims declare %v, want %v", got, c.want)
				}
			})
		}
	}
}

// The Bundle the lane reads declares neither the request Bundle profile,
// unversioned or at the line, nor anything less than the body does: every
// other profile it or an entry declares stays where it was, a request Bundle
// profile at another line's version included (the lane, which does not hold
// it, says so). Nothing else of it changes.
func TestPASRequestBundleReading_TheReadBundleKeepsEveryOtherProfile(t *testing.T) {
	const other = "http://example.org/StructureDefinition/other"
	stale := pasRequestBundleCanonical + "|2.1.0"
	body := requestBundle(t, "2.2", func(b map[string]any) {
		b["meta"] = map[string]any{"profile": []any{requestBundleProfile(t, "2.2"), other, pasRequestBundleCanonical, stale}}
		resourceAt(b, 0)["meta"] = map[string]any{"profile": []any{pasClaimCanonical + "|2.2.1", other}, "versionId": "3"}
		resourceAt(b, 1)["meta"] = map[string]any{"profile": []any{pasClaimCanonical}}
	})
	lane := &readingLane{}
	readBundle(t, lane, "2.2", body)
	var read, want map[string]any
	if err := json.Unmarshal(lane.calls[0].body, &read); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}
	want["meta"] = map[string]any{"profile": []any{other, stale}}
	if fmt.Sprint(read) != fmt.Sprint(want) {
		t.Fatalf("the read Bundle is\n%v\nwant\n%v", read, want)
	}
	// A Bundle declaring only the request Bundle profile loses meta.profile,
	// and then meta.
	body = requestBundle(t, "2.2", func(b map[string]any) {
		b["meta"] = map[string]any{"profile": []any{requestBundleProfile(t, "2.2")}}
	})
	lane = &readingLane{}
	readBundle(t, lane, "2.2", body)
	var top map[string]any
	if err := json.Unmarshal(lane.calls[0].body, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["meta"]; ok {
		t.Fatalf("the read Bundle kept an emptied meta: %v", top["meta"])
	}
}

// Everywhere else the reading is the lane's one call, as asked: 2.0, whose
// Claim slice has one candidate; an inquiry's request Bundle; a request
// Bundle profile at another line's version; no profile; a body that is no
// Bundle.
func TestPASRequestBundleReading_IsTheOneCallElsewhere(t *testing.T) {
	inquiry, _ := profileFor("PASRequestBundle", "2.2", "pas-claim-inquire")
	for name, c := range map[string]struct {
		line, profile string
		body          []byte
	}{
		"2.0":                         {"2.0", requestBundleProfile(t, "2.0"), pasGolden(t, "conformant/pas-submit-request.json")},
		"an inquiry":                  {"2.2", inquiry, requestBundle(t, "2.2", nil)},
		"another line's version":      {"2.2", requestBundleProfile(t, "2.1"), requestBundle(t, "2.2", nil)},
		"no profile":                  {"2.2", "", requestBundle(t, "2.2", nil)},
		"a body that is no Bundle":    {"2.2", requestBundleProfile(t, "2.2"), []byte(`{"resourceType":"Claim"}`)},
		"a body that is not JSON":     {"2.1", requestBundleProfile(t, "2.1"), []byte(`not json`)},
		"another workstream's answer": {"2.2", "http://hl7.org/fhir/StructureDefinition/Bundle", requestBundle(t, "2.2", nil)},
	} {
		t.Run(name, func(t *testing.T) {
			lane := &readingLane{}
			if _, err := validateAt(context.Background(), lane, c.body, c.line, c.profile); err != nil {
				t.Fatal(err)
			}
			if len(lane.calls) != 1 || lane.calls[0].profile != c.profile || string(lane.calls[0].body) != string(c.body) {
				t.Fatalf("asked %q, want the one call at %q", lane.asked(t), c.profile)
			}
		})
	}
	// The unversioned canonical is the line's own.
	lane := &readingLane{}
	if _, err := validateAt(context.Background(), lane, requestBundle(t, "2.2", nil), "2.2", pasRequestBundleCanonical); err != nil {
		t.Fatal(err)
	}
	if len(lane.calls) != 1 || lane.calls[0].profile != "" {
		t.Fatalf("the unversioned request Bundle profile was not read per entry: %q", lane.asked(t))
	}
}

// pasRequestBundleRuleRows is one mutation per Go rule of
// pasRequestBundleRules, keyed by its element: the valid request Bundle with
// that one constraint broken, the issue the rule writes, and the verdict the
// reading then gives (structural for a cardinality or pattern, deeper for
// ClaimFirst, an invariant).
var pasRequestBundleRuleRows = map[string][]struct {
	name    string
	mutate  func(t *testing.T, b map[string]any)
	id      string
	at      string
	says    string
	verdict Verdict
}{
	"Bundle": {{"the Claim not first", func(t *testing.T, b map[string]any) {
		e := entries(b)
		b["entry"] = append(append([]any{}, e[1:]...), e[0])
	}, "#ClaimFirst", "Bundle", "must have the PAS Request as the first resource", VerdictDeeper}},
	"Bundle.identifier": {
		{"no identifier", func(_ *testing.T, b map[string]any) { delete(b, "identifier") }, "Validation_VAL_Profile_Minimum", "Bundle", "Bundle.identifier: minimum required = 1", VerdictInvalid},
		{"a null identifier", func(_ *testing.T, b map[string]any) { b["identifier"] = nil }, "Validation_VAL_Profile_Minimum", "Bundle", "Bundle.identifier: minimum required = 1", VerdictInvalid},
	},
	"Bundle.type":           {{"a transaction", func(_ *testing.T, b map[string]any) { b["type"] = "transaction" }, "_DT_Fixed_Wrong", "Bundle.type", "Value is \"transaction\" but is fixed to 'collection'", VerdictInvalid}},
	"Bundle.timestamp":      {{"no timestamp", func(_ *testing.T, b map[string]any) { delete(b, "timestamp") }, "Validation_VAL_Profile_Minimum", "Bundle", "Bundle.timestamp: minimum required = 1", VerdictInvalid}},
	"Bundle.entry":          {{"no entry", func(_ *testing.T, b map[string]any) { b["entry"] = []any{} }, "Validation_VAL_Profile_Minimum", "Bundle", "Bundle.entry: minimum required = 1", VerdictInvalid}},
	"Bundle.entry.fullUrl":  {{"an entry with no fullUrl", func(_ *testing.T, b map[string]any) { delete(entryAt(b, 2), "fullUrl") }, "Validation_VAL_Profile_Minimum", "Bundle.entry[2]", "Bundle.entry.fullUrl: minimum required = 1", VerdictInvalid}},
	"Bundle.entry.resource": {{"an entry with no resource", func(_ *testing.T, b map[string]any) { delete(entryAt(b, 2), "resource") }, "Validation_VAL_Profile_Minimum", "Bundle.entry[2]", "Bundle.entry.resource: minimum required = 1", VerdictInvalid}},
	"Bundle.entry.search":   {{"an entry with a search", func(_ *testing.T, b map[string]any) { entryAt(b, 2)["search"] = map[string]any{"mode": "match"} }, "Validation_VAL_Profile_Maximum", "Bundle.entry[2]", "Bundle.entry.search: max allowed = 0", VerdictInvalid}},
	"Bundle.entry.request": {{"an entry with a request", func(_ *testing.T, b map[string]any) {
		entryAt(b, 2)["request"] = map[string]any{"method": "POST", "url": "Coverage"}
	}, "Validation_VAL_Profile_Maximum", "Bundle.entry[2]", "Bundle.entry.request: max allowed = 0", VerdictInvalid}},
	"Bundle.entry.response": {{"an entry with a response", func(_ *testing.T, b map[string]any) { entryAt(b, 2)["response"] = map[string]any{"status": "201"} }, "Validation_VAL_Profile_Maximum", "Bundle.entry[2]", "Bundle.entry.response: max allowed = 0", VerdictInvalid}},
	"Bundle.entry:Claim": {
		{"no Claim", func(_ *testing.T, b map[string]any) { b["entry"] = entries(b)[1:] }, "Validation_VAL_Profile_Minimum_SLICE", "Bundle", "Slice 'Bundle.entry:Claim': a matching slice is required", VerdictInvalid},
		{"a third Claim", func(t *testing.T, b map[string]any) {
			b["entry"] = append(entries(b), anotherClaim(t, b, "second-claim"), anotherClaim(t, b, "third-claim"))
		}, "Validation_VAL_Profile_Maximum", "Bundle", "Bundle.entry:Claim: max allowed = 2, but found 3", VerdictInvalid},
	},
}

// Every Go rule refuses its one mutation, where the profile states it, with
// the issue the validator writes for that constraint, and the reading's
// verdict is the one the validator's own issue gives; the valid Bundle draws
// none. Every rule has a row, and every row a rule.
func TestPASRequestBundleRules_EachFindsItsOneMutation(t *testing.T) {
	var ruled []string
	for _, r := range pasRequestBundleRules {
		if r.check != nil {
			ruled = append(ruled, r.element)
		}
	}
	var rowed []string
	for el := range pasRequestBundleRuleRows {
		rowed = append(rowed, el)
	}
	slices.Sort(ruled)
	slices.Sort(rowed)
	if !slices.Equal(ruled, rowed) {
		t.Fatalf("rules %q, rows %q: every Go rule needs its mutation row", ruled, rowed)
	}
	for _, line := range pasRequestBundlePerEntryLines {
		if res := readBundle(t, &readingLane{}, line, requestBundle(t, line, nil)); !res.Valid {
			t.Fatalf("%s: the valid request Bundle drew %+v", line, res.Details)
		}
		for _, r := range pasRequestBundleRules {
			for _, row := range pasRequestBundleRuleRows[r.element] {
				t.Run(line+"/"+row.name, func(t *testing.T) {
					body := requestBundle(t, line, func(b map[string]any) { row.mutate(t, b) })
					res := readBundle(t, &readingLane{}, line, body)
					stated := r.lines == nil || slices.Contains(r.lines, line)
					found := slices.ContainsFunc(res.Details, func(iss shnsdk.Issue) bool {
						id := row.id
						if strings.HasPrefix(id, "#") {
							id = requestBundleProfile(t, line) + id
						}
						return iss.Severity == "error" && iss.MessageID == id && slices.Equal(iss.Expression, []string{row.at}) &&
							strings.Contains(iss.Diagnostics, row.says) && strings.Contains(iss.Diagnostics, requestBundleProfile(t, line))
					})
					switch {
					case !stated && (found || !res.Valid):
						t.Fatalf("%s is not stated at %s, but was read: %+v", r.element, line, res.Details)
					case !stated:
						return
					case !found || res.Valid:
						t.Fatalf("%s: no %s issue at %s saying %q: %+v", r.element, row.id, row.at, row.says, res.Details)
					}
					if got := classifyFHIR(res, body, line, requestBundleProfile(t, line)); got != row.verdict {
						t.Fatalf("%s reads %v, want %v", row.name, got, row.verdict)
					}
				})
			}
		}
	}
}

// An issue the lane writes of the Bundle's own meta.profile names the index
// that profile has in the body, not in the Bundle the lane read, so a
// declared profile the lane does not hold is read as one: unavailable, never
// a defect of whatever profile sits at that index in the body. Every other
// expression is kept as written.
func TestPASRequestBundleReading_NamesTheBodysOwnProfileIndex(t *testing.T) {
	const unheld = "http://example.org/StructureDefinition/not-held"
	body := requestBundle(t, "2.2", func(b map[string]any) {
		b["meta"] = map[string]any{"profile": []any{requestBundleProfile(t, "2.2"), unheld}}
	})
	other := shnsdk.Issue{Severity: "warning", Code: "processing", Expression: []string{"Bundle.entry[0].resource/*Claim/convergence-claim*/.item[0]", "Bundle.meta"}, Diagnostics: "w"}
	lane := &readingLane{answer: func([]byte, string) (shnsdk.Result, error) {
		return invalidWith(profileNotCheckedIssue("Bundle.meta.profile[0]", unheld), other), nil
	}}
	res := readBundle(t, lane, "2.2", body)
	if got := res.Details[0].Expression; !slices.Equal(got, []string{"Bundle.meta.profile[1]"}) {
		t.Fatalf("the unheld profile is named at %v, want Bundle.meta.profile[1]", got)
	}
	if !slices.Equal(res.Details[1].Expression, other.Expression) {
		t.Fatalf("another expression was renamed: %v", res.Details[1].Expression)
	}
	if got := classifyFHIR(res, body, "2.2", requestBundleProfile(t, "2.2")); got != VerdictUnavailable {
		t.Fatalf("a declared profile the lane does not hold reads %v, want unavailable", got)
	}
}

// What the validator could not check in a Claim reads as the classifier
// reads it anywhere: a code system the lane does not hold is unchecked, and a
// defect beside it decides. A reading the lane could not finish (HAPI-0389)
// is unavailable, never a pass. A lane that fails the call fails the reading,
// and an invalid answer naming no issue stays unclassified, the most serious
// kind.
func TestPASRequestBundleReading_ReadsEachAnswerAsTheClassifierDoes(t *testing.T) {
	at := "Bundle.entry[0].resource/*Claim/convergence-claim*/.item[0].category"
	invariant := shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "http://hl7.org/fhir/StructureDefinition/Period#per-1",
		Expression: []string{"Bundle.entry[0].resource/*Claim/convergence-claim*/.billablePeriod"}, Diagnostics: "Constraint failed: per-1"}
	minimum := shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "Validation_VAL_Profile_Minimum", Expression: []string{"Bundle.entry[0].resource/*Claim/convergence-claim*/"}, Diagnostics: "Claim.insurer: minimum required = 1, but only found 0"}
	stopped := shnsdk.Issue{Severity: "error", Code: "processing", Diagnostics: "HAPI-0389: Failed to call access method: java.lang.Error: failed to validate, but no errors. profile = " + pasClaimCanonical + "|2.2.1"}
	body := requestBundle(t, "2.2", nil)
	profile := requestBundleProfile(t, "2.2")
	for name, c := range map[string]struct {
		answer shnsdk.Result
		want   Verdict
	}{
		"a code the lane could not check":   {invalidWith(reasonCodeGap(at)...), VerdictUnchecked},
		"a gap beside an invariant":         {invalidWith(append(reasonCodeGap(at), invariant)...), VerdictDeeper},
		"a gap beside a missing element":    {invalidWith(append(reasonCodeGap(at), minimum)...), VerdictInvalid},
		"a reading the lane did not finish": {invalidWith(stopped), VerdictUnavailable},
		"an invalid answer naming nothing":  {shnsdk.Result{}, VerdictInvalid},
		"an invalid answer with text alone": {shnsdk.Result{Issues: []string{"bad"}}, VerdictInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			lane := &readingLane{answer: func([]byte, string) (shnsdk.Result, error) { return c.answer, nil }}
			res := readBundle(t, lane, "2.2", body)
			if got := classifyFHIR(res, body, "2.2", profile); res.Valid || got != c.want {
				t.Fatalf("reads %v (valid %v), want %v: %+v", got, res.Valid, c.want, res.Details)
			}
		})
	}
	down := errors.New("lane down")
	lane := &readingLane{answer: func([]byte, string) (shnsdk.Result, error) { return shnsdk.Result{}, down }}
	if _, err := validateAt(context.Background(), lane, body, "2.2", profile); !errors.Is(err, down) {
		t.Fatalf("a lane that failed the call: %v, want its error", err)
	}
}

// A request Bundle that declares its line's request Bundle profile, checked
// with no profile asked (a participant's own declaration, module 3), is read
// per entry as one asked is. One declaring it at another line's version, or
// declaring nothing, is the lane's one call.
func TestPASRequestBundleReading_ADeclaredRequestBundleIsReadTheSameWay(t *testing.T) {
	for name, c := range map[string]struct {
		declares any
		perEntry bool
	}{
		"its line's version":     {requestBundleProfile(t, "2.2"), true},
		"unversioned":            {pasRequestBundleCanonical, true},
		"another line's version": {requestBundleProfile(t, "2.1"), false},
		"another profile":        {"http://example.org/StructureDefinition/other", false},
		"nothing":                {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			body := requestBundle(t, "2.2", func(b map[string]any) {
				if c.declares != nil {
					b["meta"] = map[string]any{"profile": []any{c.declares}}
				}
			})
			lane := &readingLane{}
			if _, err := validateAt(context.Background(), lane, body, "2.2", ""); err != nil {
				t.Fatal(err)
			}
			if len(lane.calls) != 1 || lane.calls[0].profile != "" {
				t.Fatalf("asked %q, want one call with no profile", lane.asked(t))
			}
			sentAsIs := string(lane.calls[0].body) == string(body)
			if c.perEntry == sentAsIs {
				t.Fatalf("read per entry %v, want %v: %s", !sentAsIs, c.perEntry, lane.calls[0].body)
			}
			if c.perEntry {
				if got := claimProfilesIn(t, lane.calls[0].body); fmt.Sprint(got) != fmt.Sprint([][]any{{claimProfile(t, "2.2", "pas-claim")}}) {
					t.Fatalf("the Claim read declares %v", got)
				}
			}
		})
	}
}

// The reading changes only the copy the lane reads: the body it was given is
// the same bytes after it.
func TestPASRequestBundleReading_LeavesTheBodyItReads(t *testing.T) {
	body := requestBundle(t, "2.2", func(b map[string]any) {
		b["meta"] = map[string]any{"profile": []any{requestBundleProfile(t, "2.2")}}
	})
	before := append([]byte(nil), body...)
	for _, profile := range []string{requestBundleProfile(t, "2.2"), ""} {
		readBundle(t, &readingLane{}, "2.2", body)
		if _, err := validateAt(context.Background(), &readingLane{}, body, "2.2", profile); err != nil {
			t.Fatal(err)
		}
	}
	if string(body) != string(before) {
		t.Fatal("the reading changed the body it read")
	}
}
