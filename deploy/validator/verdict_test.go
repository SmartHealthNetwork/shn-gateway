package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const cleanOutcome = `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"Validation successful"}]}`

const targetedNegativeOutcome = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","details":{"coding":[{"system":"http://hl7.org/fhir/java-core-messageId","code":"Extension_EXT_Type"}]},"diagnostics":"The Extension 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode' definition allows for the types [CodeableConcept] but found type boolean","expression":["ClaimResponse.item[0].adjudication[0].extension[0].extension[0]"]},{"severity":"warning","code":"processing","diagnostics":"licensed terminology unavailable"}]}`

const primeSlicingOutcome22 = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","details":{"coding":[{"system":"http://hl7.org/fhir/java-core-messageId","code":"SLICING_CANNOT_BE_EVALUATED"}]},"diagnostics":"Slicing cannot be evaluated: Could not match discriminator (url) for slice Extension.extension:number in profile http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction|2.2.1 - the discriminator [url] does not have fixed value, binding or existence assertions","expression":["ClaimResponse.item[0].adjudication[0].extension[0].extension[0]"]}]}`

func TestReadinessRowsAreUniqueOrderedCorpus(t *testing.T) {
	rows := readinessRows("2.2")
	if len(rows) != 34 {
		t.Fatalf("rows=%d want 34", len(rows))
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
	want = append(want, "negative-versioned", "negative-unversioned", "negative-meta")
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

func TestStrictVerdictAssertions(t *testing.T) {
	positive := qualificationRows("2.2", "qualify-1")[0]
	prime := qualificationRows("2.2", "prime")[0]
	negative := negativeRows("2.2")[0]
	initialization := warmups("2.2")[0]
	if err := assertVerdict(positive, 200, []byte(cleanOutcome)); err != nil {
		t.Fatalf("clean positive: %v", err)
	}
	if err := assertVerdict(prime, 200, []byte(primeSlicingOutcome22)); err != nil {
		t.Fatalf("exact prime slicing allowance: %v", err)
	}
	if err := assertVerdict(negative, 200, []byte(targetedNegativeOutcome)); err != nil {
		t.Fatalf("targeted negative: %v", err)
	}
	cases := map[string]struct {
		row    warmup
		status int
		body   string
	}{
		"wrong status":                   {positive, 422, cleanOutcome},
		"malformed":                      {positive, 200, `{"resourceType":`},
		"not outcome":                    {positive, 200, `{"resourceType":"Bundle","issue":[{"severity":"information"}]}`},
		"missing issues":                 {positive, 200, `{"resourceType":"OperationOutcome"}`},
		"empty issues":                   {positive, 200, `{"resourceType":"OperationOutcome","issue":[]}`},
		"invalid severity":               {positive, 200, `{"resourceType":"OperationOutcome","issue":[{"severity":"success","code":"informational"}]}`},
		"missing issue code":             {positive, 200, `{"resourceType":"OperationOutcome","issue":[{"severity":"information"}]}`},
		"null issue code":                {positive, 200, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":null}]}`},
		"empty issue code":               {positive, 200, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":""}]}`},
		"unknown issue code":             {positive, 200, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"not-a-fhir-issue-type"}]}`},
		"positive error":                 {positive, 200, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"bad"}]}`},
		"positive slicing":               {positive, 200, primeSlicingOutcome22},
		"missing profile":                {positive, 200, `{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"processing","diagnostics":"Invalid profile. Failed to retrieve profile with url=http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"}]}`},
		"initialization missing profile": {initialization, 200, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"Invalid profile. Failed to retrieve profile with url=http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"}]}`},
		"dirty prime":                    {prime, 200, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"unrelated"}]}`},
		"wrong prime slice":              {prime, 200, strings.Replace(primeSlicingOutcome22, "Extension.extension:number", "Extension.extension:unknown", 1)},
		"wrong prime version":            {prime, 200, strings.Replace(primeSlicingOutcome22, "extension-reviewAction|2.2.1", "extension-reviewAction|2.1.0", 1)},
		"clean negative":                 {negative, 200, cleanOutcome},
		"wrong negative code":            {negative, 200, strings.Replace(targetedNegativeOutcome, "Extension_EXT_Type", "Wrong_Code", 2)},
		"wrong negative coding system":   {negative, 200, strings.Replace(targetedNegativeOutcome, messageIDSystem, "http://example.test/wrong", 1)},
		"wrong negative path":            {negative, 200, strings.Replace(targetedNegativeOutcome, "ClaimResponse.item[0]", "ClaimResponse.item[1]", 1)},
		"wrong negative detail":          {negative, 200, strings.Replace(targetedNegativeOutcome, "found type boolean", "found type string", 1)},
		"extra negative error":           {negative, 200, strings.Replace(targetedNegativeOutcome, `{"severity":"warning"`, `{"severity":"error"`, 1)},
		"duplicate targeted error":       {negative, 200, strings.Replace(targetedNegativeOutcome, `,{"severity":"warning"`, `,{"severity":"error","code":"processing","details":{"coding":[{"system":"http://hl7.org/fhir/java-core-messageId","code":"Extension_EXT_Type"}]},"diagnostics":"The Extension 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode' definition allows for the types [CodeableConcept] but found type boolean","expression":["ClaimResponse.item[0].adjudication[0].extension[0].extension[0]"]},{"severity":"warning"`, 1)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := assertVerdict(tc.row, tc.status, []byte(tc.body)); err == nil {
				t.Fatal("invalid verdict accepted")
			}
		})
	}
}

func TestStrictVerdictRejectsDuplicateAndAliasedMembers(t *testing.T) {
	positive := qualificationRows("2.2", "qualify-1")[0]
	negative := negativeRows("2.2")[0]
	cases := map[string]struct {
		row  warmup
		body string
	}{
		"duplicate top-level issue": {positive, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}],"issue":[{"severity":"information","code":"informational"}]}`},
		"aliased top-level issue":   {positive, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}],"Issue":[{"severity":"information","code":"informational"}]}`},
		"duplicate severity":        {positive, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","severity":"information","code":"processing"}]}`},
		"aliased severity":          {positive, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","Severity":"information","code":"processing"}]}`},
		"duplicate code":            {positive, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"invalid","code":"informational"}]}`},
		"aliased code":              {positive, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"invalid","Code":"informational"}]}`},
		"duplicate diagnostics":     {positive, `{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"processing","diagnostics":"Failed to retrieve profile","diagnostics":"clean"}]}`},
		"duplicate details":         {positive, `{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"processing","details":{"coding":[{"system":"http://hl7.org/fhir/java-core-messageId","code":"SLICING_CANNOT_BE_EVALUATED"}]},"details":{}}]}`},
		"duplicate expression":      {negative, strings.Replace(targetedNegativeOutcome, `"expression":["ClaimResponse.item[0]`, `"expression":["ClaimResponse.item[1].wrong"],"expression":["ClaimResponse.item[0]`, 1)},
		"aliased coding system":     {negative, strings.Replace(targetedNegativeOutcome, `"system":"http://hl7.org/fhir/java-core-messageId"`, `"system":"wrong","System":"http://hl7.org/fhir/java-core-messageId"`, 1)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := assertVerdict(tc.row, 200, []byte(tc.body)); err == nil {
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
	answer := strings.Replace(targetedNegativeOutcome, `,"issue":[`, ",\n\t\"issue\":[", 1) + strings.Repeat(" ", 3000)
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
