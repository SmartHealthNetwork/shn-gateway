package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// recordedExchange is one request a real validator lane was asked and its
// answer, as the module's lane_replay_test.go loads it from
// testdata/recordings (twin test files cannot import the recording helper).
type recordedExchange struct {
	method, path string
	query        url.Values
	body         []byte
	status       int
	contentType  string // the answer's
	answer       []byte
}

// recordedAnswer is what the real lane of row.line answered to row's request
// (testdata/recordings/lane-<line>-warm.json): its first answer when first is
// set (on 2.2 a ClaimResponse decision form first met the lane still warming),
// else its settled one.
func recordedAnswer(t *testing.T, row warmup, first bool) []byte {
	t.Helper()
	body, err := fixtureBody(row)
	if err != nil {
		t.Fatalf("%s: %v", row.identity, err)
	}
	var found []byte
	for _, ex := range laneRecording(t, "lane-"+row.line+"-warm") {
		if ex.method != http.MethodPost || ex.path != "/fhir/"+row.resourceType+"/$validate" || !sameJSON(ex.body, body) {
			continue
		}
		if row.profile == "" && len(ex.query) != 0 || row.profile != "" && !reflect.DeepEqual(ex.query, url.Values{"profile": {row.profile}}) {
			continue
		}
		if ex.status != http.StatusOK {
			t.Fatalf("%s on %s: recorded status %d", row.identity, row.line, ex.status)
		}
		found = append([]byte(nil), ex.answer...)
		if first {
			break
		}
	}
	if found == nil {
		t.Fatalf("no recorded answer for %s on line %s", row.identity, row.line)
	}
	return found
}

// primeSlicingAnswer22 is the 2.2 lane's first answer to the versioned
// approved ClaimResponse: the SLICING_CANNOT_BE_EVALUATED errors a prime row
// tolerates and a qualification row refuses.
func primeSlicingAnswer22(t *testing.T) []byte {
	t.Helper()
	answer := recordedAnswer(t, qualificationRows("2.2", "prime")[0], true)
	if !bytes.Contains(answer, []byte("SLICING_CANNOT_BE_EVALUATED")) {
		t.Fatal("the recorded first 2.2 answer no longer carries the slicing errors")
	}
	return answer
}

func sameJSON(a, b []byte) bool {
	decode := func(raw []byte) (any, bool) {
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		return v, dec.Decode(&v) == nil && !dec.More()
	}
	x, okA := decode(a)
	y, okB := decode(b)
	return okA && okB && reflect.DeepEqual(x, y)
}

// replaceOnce is a deliberate mutation of a recorded answer's bytes; old must
// occur in it, so a mutation that stopped applying fails instead of passing
// the unchanged answer.
func replaceOnce(t *testing.T, raw []byte, old, replacement string) []byte {
	t.Helper()
	if !bytes.Contains(raw, []byte(old)) {
		t.Fatalf("mutation target %q is not in the answer", old)
	}
	return bytes.Replace(raw, []byte(old), []byte(replacement), 1)
}

// mutateOutcome is a deliberate mutation of a recorded answer's issue list.
// The answer is re-encoded; member order does not change a verdict.
func mutateOutcome(t *testing.T, raw []byte, edit func(issues []any) []any) []byte {
	t.Helper()
	var outcome map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&outcome); err != nil {
		t.Fatal(err)
	}
	issues, ok := outcome["issue"].([]any)
	if !ok {
		t.Fatal("the answer has no issue list")
	}
	outcome["issue"] = edit(issues)
	out, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// issueCoded is the one issue in issues whose message id is code.
func issueCoded(t *testing.T, issues []any, code string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, value := range issues {
		issue, _ := value.(map[string]any)
		details, _ := issue["details"].(map[string]any)
		codings, _ := details["coding"].([]any)
		if len(codings) == 1 && codings[0].(map[string]any)["code"] == code {
			if found != nil {
				t.Fatalf("two issues coded %s", code)
			}
			found = issue
		}
	}
	if found == nil {
		t.Fatalf("no issue coded %s", code)
	}
	return found
}

// issueOf is a fresh decoded copy of the one issue in raw coded code.
func issueOf(t *testing.T, raw []byte, code string) map[string]any {
	t.Helper()
	var outcome struct {
		Issue []any `json:"issue"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&outcome); err != nil {
		t.Fatal(err)
	}
	return issueCoded(t, outcome.Issue, code)
}

// codingOf is an issue's one message-id coding.
func codingOf(issue map[string]any) map[string]any {
	return issue["details"].(map[string]any)["coding"].([]any)[0].(map[string]any)
}

// editIssue changes the one issue coded code.
func editIssue(t *testing.T, raw []byte, code string, edit func(issue map[string]any)) []byte {
	t.Helper()
	return mutateOutcome(t, raw, func(issues []any) []any {
		edit(issueCoded(t, issues, code))
		return issues
	})
}

func TestReadinessRowsAreUniqueOrderedCorpus(t *testing.T) {
	rows := readinessRows("2.2")
	if len(rows) != 42 {
		t.Fatalf("rows=%d want 42", len(rows))
	}
	if rows[38].identity != "encounter-positive" || rows[39].identity != "encounter-target-type" {
		t.Fatalf("the encounter rows must retain their positions: %q %q", rows[38].identity, rows[39].identity)
	}
	wantPrefix := []string{"init-pas-request-bundle", "init-dtr-questionnaireresponse", "init-pdex-explanationofbenefit", "init-cdex-task"}
	for i, want := range wantPrefix {
		if rows[i].identity != want {
			t.Fatalf("row[%d]=%q want %q", i, rows[i].identity, want)
		}
	}
	wantForms := []string{"versioned-approved", "versioned-denied", "versioned-pended", "unversioned-approved", "unversioned-denied", "unversioned-pended", "meta-approved", "meta-denied", "meta-pended"}
	var want []string
	want = append(want, wantPrefix...)
	for _, pass := range []string{"prime", "qualify-1", "qualify-2"} {
		for _, form := range wantForms {
			want = append(want, pass+"-"+form)
		}
	}
	want = append(want, "negative-versioned", "negative-unversioned", "negative-meta", "full-response-positive", "full-response-negative-hcpcs", "full-response-negative-pos", "full-response-negative-encounter", "encounter-positive", "encounter-target-type", "explicit-profile-missing-version", "explicit-profile-missing-canonical")
	seen := map[string]bool{}
	for i, row := range rows {
		if row.identity != want[i] {
			t.Fatalf("identity[%d]=%q want %q", i, row.identity, want[i])
		}
		if seen[row.identity] {
			t.Fatalf("duplicate identity %q", row.identity)
		}
		seen[row.identity] = true
	}
	if qualificationRows("9.9", "prime") != nil || qualificationRows("2.2", "unknown") != nil || negativeRows("9.9") != nil {
		t.Fatal("unknown line/pass produced rows")
	}
}

func TestQualificationRowsUseExactRequestForms(t *testing.T) {
	const canonical = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"
	for line, version := range map[string]string{"2.0": "2.0.1", "2.1": "2.1.0", "2.2": "2.2.1"} {
		rows := qualificationRows(line, "qualify-1")
		if len(rows) != 9 {
			t.Fatalf("line %s rows=%d", line, len(rows))
		}
		for i, want := range []string{canonical + "|" + version, canonical + "|" + version, canonical + "|" + version, canonical, canonical, canonical, "", "", ""} {
			if rows[i].profile != want || rows[i].resourceType != "ClaimResponse" {
				t.Fatalf("line %s row %s profile=%q type=%q", line, rows[i].identity, rows[i].profile, rows[i].resourceType)
			}
		}
	}
}

func TestFixtureBodyExtractsClaimResponseAndDerivesSingleMutation(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, row := range qualificationRows(line, "qualify-1") {
			body, err := fixtureBody(row)
			if err != nil {
				t.Fatalf("%s: %v", row.identity, err)
			}
			var resource map[string]any
			if err := json.Unmarshal(body, &resource); err != nil || resource["resourceType"] != "ClaimResponse" {
				t.Fatalf("%s did not produce ClaimResponse: %v", row.identity, err)
			}
			profiles := resource["meta"].(map[string]any)["profile"].([]any)
			if !reflect.DeepEqual(profiles, []any{"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"}) {
				t.Fatalf("%s profiles=%v", row.identity, profiles)
			}
		}
		body, err := fixtureBody(negativeRows(line)[0])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(body), `"valueBoolean":true`) != 1 || strings.Contains(string(body), `"valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"A1"`) {
			t.Fatalf("line %s negative was not the single intended mutation", line)
		}
	}
}

func TestNegativeMutationRejectsAmbiguousFixtures(t *testing.T) {
	match := func(values map[string]any) map[string]any {
		extension := map[string]any{"url": reviewActionCode}
		for key, value := range values {
			extension[key] = value
		}
		return extension
	}
	cases := map[string]map[string]any{
		"missing extension": {"resourceType": "ClaimResponse"},
		"duplicate extension": {"resourceType": "ClaimResponse", "extension": []any{
			match(map[string]any{"valueCodeableConcept": map[string]any{}}),
			match(map[string]any{"valueCodeableConcept": map[string]any{}}),
		}},
		"missing value": {"resourceType": "ClaimResponse", "extension": []any{
			match(nil),
		}},
		"wrong value": {"resourceType": "ClaimResponse", "extension": []any{
			match(map[string]any{"valueString": "wrong"}),
		}},
		"multiple values": {"resourceType": "ClaimResponse", "extension": []any{
			match(map[string]any{"valueCodeableConcept": map[string]any{}, "valueBoolean": false}),
		}},
	}
	for name, resource := range cases {
		t.Run(name, func(t *testing.T) {
			before, err := json.Marshal(resource)
			if err != nil {
				t.Fatal(err)
			}
			if err := mutateReviewActionCode(resource); err == nil {
				t.Fatal("ambiguous fixture mutation accepted")
			}
			after, err := json.Marshal(resource)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("rejected fixture was mutated: before=%s after=%s", before, after)
			}
		})
	}
}

func TestClaimResponseFixtureRejectsWrongShapesAndProfiles(t *testing.T) {
	bad := map[string]string{
		"malformed":          `{"resourceType":`,
		"wrong resource":     `{"resourceType":"Patient"}`,
		"bundle no entry":    `{"resourceType":"Bundle"}`,
		"missing response":   `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Task"}}]}`,
		"duplicate response": `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}},{"resource":{"resourceType":"ClaimResponse"}}]}`,
		"bad entry":          `{"resourceType":"Bundle","entry":[true]}`,
		"trailing JSON":      `{"resourceType":"ClaimResponse"}{}`,
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if _, _, err := claimResponseFixture([]byte(raw)); err == nil {
				t.Fatal("invalid fixture accepted")
			}
		})
	}
	for name, raw := range map[string]string{
		"missing meta":    `{"resourceType":"ClaimResponse"}`,
		"missing profile": `{"resourceType":"ClaimResponse","meta":{}}`,
		"unknown profile": `{"resourceType":"ClaimResponse","meta":{"profile":["http://example.test/wrong"]}}`,
		"extra profile":   `{"resourceType":"ClaimResponse","meta":{"profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse","http://example.test/extra"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			resource, _, err := claimResponseFixture([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if hasExactProfile(resource, pasClaimResponseProfile) {
				t.Fatal("invalid profile set accepted")
			}
		})
	}
}

// The verdicts the 2.2 lane really gave pass; each rejection row is either
// one of those answers given to the wrong row, one deliberately mutated, or a
// malformed body no lane sends.
func TestStrictVerdictAssertions(t *testing.T) {
	positive := qualificationRows("2.2", "qualify-1")[0]
	prime := qualificationRows("2.2", "prime")[0]
	negative := negativeRows("2.2")[0]
	initialization := warmups("2.2")[0]
	settled := recordedAnswer(t, positive, false)
	slicing := primeSlicingAnswer22(t)
	targeted := recordedAnswer(t, negative, false)
	if err := assertVerdict(positive, 200, settled); err != nil {
		t.Fatalf("recorded positive: %v", err)
	}
	if err := assertVerdict(prime, 200, slicing); err != nil {
		t.Fatalf("recorded prime slicing allowance: %v", err)
	}
	if err := assertVerdict(prime, 200, settled); err != nil {
		t.Fatalf("recorded settled prime: %v", err)
	}
	if err := assertVerdict(negative, 200, targeted); err != nil {
		t.Fatalf("recorded targeted negative: %v", err)
	}
	if err := assertVerdict(initialization, 200, recordedAnswer(t, initialization, false)); err != nil {
		t.Fatalf("recorded initialization: %v", err)
	}
	extensionType := "Extension_EXT_Type"
	cases := map[string]struct {
		row    warmup
		status int
		body   []byte
	}{
		"wrong status":                   {positive, 422, settled},
		"malformed":                      {positive, 200, []byte(`{"resourceType":`)},
		"not outcome":                    {positive, 200, []byte(`{"resourceType":"Bundle","issue":[{"severity":"information"}]}`)},
		"missing issues":                 {positive, 200, []byte(`{"resourceType":"OperationOutcome"}`)},
		"empty issues":                   {positive, 200, []byte(`{"resourceType":"OperationOutcome","issue":[]}`)},
		"invalid severity":               {positive, 200, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"success","code":"informational"}]}`)},
		"missing issue code":             {positive, 200, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information"}]}`)},
		"null issue code":                {positive, 200, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":null}]}`)},
		"empty issue code":               {positive, 200, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":""}]}`)},
		"unknown issue code":             {positive, 200, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"not-a-fhir-issue-type"}]}`)},
		"positive error":                 {positive, 200, targeted},
		"positive slicing":               {positive, 200, slicing},
		"missing profile":                {positive, 200, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"processing","diagnostics":"Invalid profile. Failed to retrieve profile with url=http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"}]}`)},
		"initialization missing profile": {initialization, 200, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"Invalid profile. Failed to retrieve profile with url=http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"}]}`)},
		"dirty prime":                    {prime, 200, targeted},
		"wrong prime slice":              {prime, 200, replaceOnce(t, slicing, "Extension.extension:number", "Extension.extension:unknown")},
		"wrong prime version":            {prime, 200, replaceOnce(t, slicing, "extension-reviewAction|2.2.1", "extension-reviewAction|2.1.0")},
		"clean negative":                 {negative, 200, settled},
		"wrong negative code": {negative, 200, editIssue(t, targeted, extensionType, func(i map[string]any) {
			codingOf(i)["code"] = "Wrong_Code"
		})},
		"wrong negative coding system": {negative, 200, editIssue(t, targeted, extensionType, func(i map[string]any) {
			codingOf(i)["system"] = "http://example.test/wrong"
		})},
		"wrong negative path": {negative, 200, editIssue(t, targeted, extensionType, func(i map[string]any) {
			i["expression"] = []any{"ClaimResponse.item[1].adjudication[0].extension[0].extension[0]"}
		})},
		"wrong negative detail": {negative, 200, replaceOnce(t, targeted, "found type boolean", "found type string")},
		"extra negative error":  {negative, 200, replaceOnce(t, targeted, `"severity":"warning"`, `"severity":"error"`)},
		"duplicate targeted error": {negative, 200, mutateOutcome(t, targeted, func(issues []any) []any {
			return append(issues, issueCoded(t, issues, extensionType))
		})},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := assertVerdict(tc.row, tc.status, tc.body); err == nil {
				t.Fatal("invalid verdict accepted")
			}
		})
	}
}

func TestStrictVerdictRejectsDuplicateAndAliasedMembers(t *testing.T) {
	positive := qualificationRows("2.2", "qualify-1")[0]
	negative := negativeRows("2.2")[0]
	targeted := recordedAnswer(t, negative, false)
	cases := map[string]struct {
		row  warmup
		body []byte
	}{
		"duplicate top-level issue": {positive, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}],"issue":[{"severity":"information","code":"informational"}]}`)},
		"aliased top-level issue":   {positive, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}],"Issue":[{"severity":"information","code":"informational"}]}`)},
		"duplicate severity":        {positive, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","severity":"information","code":"processing"}]}`)},
		"aliased severity":          {positive, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","Severity":"information","code":"processing"}]}`)},
		"duplicate code":            {positive, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"invalid","code":"informational"}]}`)},
		"aliased code":              {positive, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"invalid","Code":"informational"}]}`)},
		"duplicate diagnostics":     {positive, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"processing","diagnostics":"Failed to retrieve profile","diagnostics":"clean"}]}`)},
		"duplicate details":         {positive, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"processing","details":{"coding":[{"system":"http://hl7.org/fhir/java-core-messageId","code":"SLICING_CANNOT_BE_EVALUATED"}]},"details":{}}]}`)},
		"duplicate expression":      {negative, replaceOnce(t, targeted, `"expression":["ClaimResponse.item[0]`, `"expression":["ClaimResponse.item[1].wrong"],"expression":["ClaimResponse.item[0]`)},
		"aliased coding system":     {negative, replaceOnce(t, targeted, `"system":"http://hl7.org/fhir/java-core-messageId"`, `"system":"wrong","System":"http://hl7.org/fhir/java-core-messageId"`)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := assertVerdict(tc.row, 200, tc.body); err == nil {
				t.Fatal("ambiguous verdict accepted")
			}
		})
	}
}

func TestStrictVerdictAcceptsFHIRR4IssueTypeCodes(t *testing.T) {
	row := qualificationRows("2.2", "qualify-1")[0]
	codes := []string{
		"invalid", "structure", "required", "value", "invariant",
		"security", "login", "unknown", "expired", "forbidden", "suppressed",
		"processing", "not-supported", "duplicate", "multiple-matches", "not-found", "deleted", "too-long", "code-invalid", "extension", "too-costly", "business-rule", "conflict",
		"transient", "lock-error", "no-store", "exception", "timeout", "incomplete", "throttled",
		"informational",
	}
	for _, code := range codes {
		body := fmt.Sprintf(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":%q}]}`, code)
		if err := assertVerdict(row, 200, []byte(body)); err != nil {
			t.Fatalf("FHIR R4 issue-type code %q rejected: %v", code, err)
		}
	}
}

func TestSubmitValidationFailureCarriesTheWholeAnswerOnOneLine(t *testing.T) {
	// The 2.0 lane's real answer to the negative control, given to a positive
	// row, with line breaks and tabs added to show they are folded.
	answer := string(replaceOnce(t, recordedAnswer(t, negativeRows("2.0")[0], false), `,"issue":[`, ",\n\t\"issue\":[")) + strings.Repeat(" ", 3000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	row := qualificationRows("2.0", "verify")[0]
	err := submitValidation(context.Background(), httpClient(), srv.URL+"/fhir", row, []byte(`{"resourceType":"ClaimResponse"}`))
	if err == nil || err.Error() != "unexpected verdict" {
		t.Fatalf("submitValidation = %v, want the bounded class \"unexpected verdict\" as Error()", err)
	}
	var oe *outcomeError
	if !errors.As(err, &oe) {
		t.Fatalf("error %T does not carry the outcome excerpt", err)
	}
	if len(oe.excerpt) != len(answer) || !strings.HasPrefix(oe.excerpt, `{"resourceType":"OperationOutcome"`) {
		t.Fatalf("excerpt = %d bytes %q, want the whole %d-byte answer", len(oe.excerpt), oe.excerpt[:40], len(answer))
	}
	if strings.ContainsAny(oe.excerpt, "\n\r\t") {
		t.Fatal("excerpt must be single-line")
	}
}

// Complete response graphs must qualify independently of bare decisions.
func TestFullResponseCorpus(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		rows := fullResponseRows(line)
		if len(rows) != 4 {
			t.Fatalf("%s full rows=%d", line, len(rows))
		}
		for i, row := range rows {
			body, err := fixtureBody(row)
			if err != nil {
				t.Fatal(err)
			}
			var bundle map[string]any
			if err := json.Unmarshal(body, &bundle); err != nil || bundle["resourceType"] != "Bundle" || len(bundle["entry"].([]any)) != 9 {
				t.Fatalf("incomplete response: %s", row.identity)
			}
			// What the lane of line really answered this row passes.
			if err := assertVerdict(row, 200, recordedAnswer(t, row, false)); err != nil {
				t.Fatalf("%s %s: recorded answer refused: %v", line, row.identity, err)
			}
			if i == 0 {
				continue
			}
			expected, err := fixtures.ReadFile(row.expectedOutcome)
			if err != nil {
				t.Fatal(err)
			}
			if err := assertVerdict(row, 200, expected); err != nil {
				t.Fatalf("%s: %v", row.identity, err)
			}
			for name, bad := range map[string][]byte{
				"accepted invalid":   recordedAnswer(t, rows[0], false),
				"wrong code":         bytes.ReplaceAll(expected, []byte("processing"), []byte("invalid")),
				"wrong path":         bytes.ReplaceAll(expected, []byte("Bundle.entry[4]"), []byte("Bundle.entry[5]")),
				"unknown definition": []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"Unknown extension"}]}`),
			} {
				if assertVerdict(row, 200, bad) == nil {
					t.Fatalf("%s accepted %s", row.identity, name)
				}
			}
		}
	}
	if fullResponseRows("wrong") != nil {
		t.Fatal("unknown lane admitted")
	}
}

func TestFullResponseMutationsAreIsolated(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		rows := fullResponseRows(line)
		raw, err := fixtureBody(rows[0])
		if err != nil {
			t.Fatal(err)
		}
		var original map[string]any
		_ = json.Unmarshal(raw, &original)
		for _, row := range rows[1:] {
			body, err := fixtureBody(row)
			if err != nil {
				t.Fatal(err)
			}
			var mutated map[string]any
			_ = json.Unmarshal(body, &mutated)
			claim := mutated["entry"].([]any)[4].(map[string]any)["resource"].(map[string]any)
			before := original["entry"].([]any)[4].(map[string]any)["resource"].(map[string]any)
			switch row.mutation {
			case "hcpcs", "pos":
				key, want := "productOrService", "L9999"
				if row.mutation == "pos" {
					key, want = "locationCodeableConcept", "98"
				}
				item := claim["item"].([]any)[0].(map[string]any)
				coding := item[key].(map[string]any)["coding"].([]any)[0].(map[string]any)
				if coding["code"] != want {
					t.Fatal("wrong mutation")
				}
				claim["item"] = before["item"]
			case "encounter":
				ext := claim["extension"].([]any)
				if len(ext) != len(before["extension"].([]any))+1 || ext[len(ext)-1].(map[string]any)["valueString"] != "invalid-reference-type" {
					t.Fatal("wrong extension mutation")
				}
				claim["extension"] = before["extension"]
			}
			if !reflect.DeepEqual(original, mutated) {
				t.Fatalf("%s changed unrelated data", row.identity)
			}
		}
	}
}

func TestFullResponseBodyRejectsMalformedMutationInputs(t *testing.T) {
	cases := map[string]string{
		"invalid JSON": "{", "wrong type": `{"resourceType":"Claim"}`,
		"no Claim":         `{"resourceType":"Bundle","entry":[]}`,
		"duplicate Claim":  `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}},{"resource":{"resourceType":"Claim"}}]}`,
		"no items":         `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`,
		"missing concept":  `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","item":[{}]}}]}`,
		"nonobject coding": `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","item":[{"productOrService":{"coding":[false]}}]}}]}`,
	}
	for name, raw := range cases {
		if _, err := fullResponseBody([]byte(raw), "hcpcs"); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	raw, _ := fixtures.ReadFile(fullResponseRows("2.0")[0].file)
	if _, err := fullResponseBody(raw, "wrong"); err == nil {
		t.Fatal("unknown mutation accepted")
	}
}

func TestSupportNegativeRequiresExactErrorMultiset(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, row := range fullResponseRows(line)[1:] {
			raw, _ := fixtures.ReadFile(row.expectedOutcome)
			expected, _ := decodeOperationOutcome(raw)
			changed := func(mutate func(*operationOutcome)) []byte {
				var out operationOutcome
				_ = json.Unmarshal(raw, &out)
				mutate(&out)
				body, _ := json.Marshal(out)
				return body
			}
			cases := map[string][]byte{
				"missing one":            changed(func(o *operationOutcome) { o.Issue = o.Issue[:1] }),
				"duplicate":              changed(func(o *operationOutcome) { o.Issue = append(o.Issue, o.Issue[0]) }),
				"wrong severity":         changed(func(o *operationOutcome) { o.Issue[0].Severity = "warning" }),
				"wrong diagnostic":       changed(func(o *operationOutcome) { o.Issue[0].Diagnostics += " changed" }),
				"wrong message identity": changed(func(o *operationOutcome) { o.Issue[0].Details.Coding[0].Code = "wrong" }),
				"wrong message system":   changed(func(o *operationOutcome) { o.Issue[0].Details.Coding[0].System = "wrong" }),
				"extra error": changed(func(o *operationOutcome) {
					o.Issue = append(o.Issue, outcomeIssue{Severity: "error", Code: "processing", Diagnostics: "unrelated"})
				}),
				"unknown profile warning": changed(func(o *operationOutcome) {
					o.Issue = append(o.Issue, outcomeIssue{Severity: "warning", Code: "processing", Diagnostics: "Failed to retrieve profile"})
				}),
			}
			for name, bad := range cases {
				if assertVerdict(row, 200, bad) == nil {
					t.Fatalf("%s %s admitted %s", line, row.identity, name)
				}
			}
			expected.Issue[0], expected.Issue[1] = expected.Issue[1], expected.Issue[0]
			reversed, _ := json.Marshal(expected)
			if err := assertVerdict(row, 200, reversed); err != nil {
				t.Fatal("error order should not change verdict", err)
			}
			positive := fullResponseRows(line)[0]
			if err := assertVerdict(positive, 200, raw); err == nil {
				t.Fatal("complete positive accepted missing/invalid support errors")
			}
		}
	}
	row := fullResponseRows("2.0")[1]
	answer := recordedAnswer(t, row, false)
	row.expectedOutcome = "missing.json"
	if assertVerdict(row, 200, answer) == nil {
		t.Fatal("missing expectation admitted")
	}
}

// TestEncounterRows: the positive row posts the committed Claim unchanged; the
// target-type control swaps the contained Encounter for a Patient of the same
// id; any other mutation is a fixture error. The pinned outcome carries exactly
// the target-type error; an outcome with that error passes, one with an extra
// error or a different error is refused, and so is one with no error.
func TestEncounterRows(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		rows := encounterRows(line)
		if len(rows) != 2 || rows[0].resourceType != "Claim" || rows[0].profile != "http://hl7.org/fhir/StructureDefinition/Claim" {
			t.Fatalf("%s: rows=%+v", line, rows)
		}
		raw, err := fixtures.ReadFile(rows[0].file)
		if err != nil {
			t.Fatal(err)
		}
		body, err := fixtureBody(rows[0])
		if err != nil || !bytes.Equal(body, raw) {
			t.Fatalf("%s: positive body must be the committed Claim: %v", line, err)
		}
		mutated, err := fixtureBody(rows[1])
		if err != nil {
			t.Fatal(err)
		}
		var claim map[string]any
		if err := json.Unmarshal(mutated, &claim); err != nil {
			t.Fatal(err)
		}
		contained := claim["contained"].([]any)
		patient := contained[0].(map[string]any)
		if len(contained) != 1 || patient["resourceType"] != "Patient" || patient["id"] != "probe-encounter" {
			t.Fatalf("%s: target-type mutation did not swap the contained Encounter: %v", line, contained)
		}
		if _, err := claimEncounterBody(raw, "other"); err == nil {
			t.Fatalf("%s: unknown mutation accepted", line)
		}
		expected, err := fixtures.ReadFile(rows[1].expectedOutcome)
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := decodeOperationOutcome(expected)
		if err != nil || len(outcome.Issue) != 1 || outcome.Issue[0].Details.Coding[0].Code != "Reference_REF_BadTargetType" {
			t.Fatalf("%s: pinned outcome = %+v (%v)", line, outcome, err)
		}
		if err := assertVerdict(rows[1], 200, expected); err != nil {
			t.Fatalf("%s: exact target-type error refused: %v", line, err)
		}
		// The lane's real answers to both rows pass; each rejection row
		// mutates one of them.
		const badTarget = "Reference_REF_BadTargetType"
		refused := recordedAnswer(t, rows[1], false)
		clean := recordedAnswer(t, rows[0], false)
		if err := assertVerdict(rows[1], 200, refused); err != nil {
			t.Fatalf("%s: recorded target-type answer refused: %v", line, err)
		}
		if err := assertVerdict(rows[0], 200, clean); err != nil {
			t.Fatalf("%s: recorded positive answer refused: %v", line, err)
		}
		unrelated := func(issue map[string]any) {
			codingOf(issue)["code"] = "Other"
			issue["diagnostics"] = "unrelated"
		}
		for name, tc := range map[string]struct {
			row  warmup
			body []byte
		}{
			"an extra error": {rows[1], mutateOutcome(t, refused, func(issues []any) []any {
				extra := issueOf(t, refused, badTarget)
				unrelated(extra)
				return append(issues, extra)
			})},
			"a clean outcome for the target-type control": {rows[1], clean},
			"a different error":                           {rows[1], editIssue(t, refused, badTarget, unrelated)},
			"the target-type error on the positive row": {rows[0], mutateOutcome(t, clean, func(issues []any) []any {
				return append(issues, issueOf(t, refused, badTarget))
			})},
		} {
			if err := assertVerdict(tc.row, 200, tc.body); err == nil {
				t.Fatalf("%s: %s was accepted", line, name)
			}
		}
	}
}

func TestExplicitProfileRefusalRows(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		rows := explicitProfileRows(line)
		if len(rows) != 2 {
			t.Fatalf("%s explicit profile rows=%d", line, len(rows))
		}
		for _, row := range rows {
			body, err := fixtureBody(row)
			if err != nil {
				t.Fatal(err)
			}
			var resource map[string]any
			if err := json.Unmarshal(body, &resource); err != nil || !hasExactProfile(resource, pasClaimResponseProfile) {
				t.Fatal("explicit refusal must retain valid in-band profile")
			}
			// The lane's real refusal of the unavailable profile passes.
			outcome := recordedAnswer(t, row, false)
			if err := assertVerdict(row, 200, outcome); err != nil {
				t.Fatalf("%s %s intended absence refused: %v", line, row.identity, err)
			}
			unknown := "Validation_VAL_Profile_Unknown"
			for name, invalid := range map[string][]byte{
				// The same fixture validated against the resolvable profile, as a
				// lane that ignored the requested one would answer.
				"validated clean":  recordedAnswer(t, qualificationRows(line, "qualify-1")[0], false),
				"warning severity": editIssue(t, outcome, unknown, func(i map[string]any) { i["severity"] = "warning" }),
				"different profile": editIssue(t, outcome, unknown, func(i map[string]any) {
					i["diagnostics"] = strings.Replace(i["diagnostics"].(string), row.profile, "https://example.org/different", 1)
				}),
				"unrelated code": editIssue(t, outcome, unknown, func(i map[string]any) { codingOf(i)["code"] = "unrelated" }),
				"exception":      editIssue(t, outcome, unknown, func(i map[string]any) { i["code"] = "exception" }),
				"resolver unavailable": editIssue(t, outcome, unknown, func(i map[string]any) {
					i["diagnostics"] = strings.Replace(i["diagnostics"].(string), "Invalid profile. Failed to retrieve explicitly requested profile with url=", "Resolver unavailable: ", 1)
				}),
			} {
				if err := assertVerdict(row, 200, invalid); err == nil {
					t.Fatalf("%s %s accepted a %s refusal", line, row.identity, name)
				}
			}
			if err := assertVerdict(row, 500, outcome); err == nil {
				t.Fatal("execution failure qualified as profile absence")
			}
		}
	}
	if explicitProfileRows("unknown") != nil {
		t.Fatal("unknown lane accepted")
	}
}
