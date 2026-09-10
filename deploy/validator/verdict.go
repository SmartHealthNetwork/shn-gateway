package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

const (
	pasClaimResponseProfile = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"
	reviewActionProfile     = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction"
	reviewActionCode        = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode"
	negativeExpression      = "ClaimResponse.item[0].adjudication[0].extension[0].extension[0]"
	messageIDSystem         = "http://hl7.org/fhir/java-core-messageId"
)

type verdictMode uint8

const (
	verdictInitialize verdictMode = iota
	verdictPrime
	verdictPositive
	verdictNegative
	verdictSupportNegative
)

var pasVersions = map[string]string{"2.0": "2.0.1", "2.1": "2.1.0", "2.2": "2.2.1"}

func pasVersion(line string) (string, bool) {
	v, ok := pasVersions[line]
	return v, ok
}

func readinessRows(line string) []warmup {
	if _, ok := pasVersion(line); !ok {
		return nil
	}
	rows := append([]warmup(nil), warmups(line)...)
	rows = append(rows, qualificationRows(line, "prime")...)
	rows = append(rows, qualificationRows(line, "qualify-1")...)
	rows = append(rows, qualificationRows(line, "qualify-2")...)
	rows = append(rows, negativeRows(line)...)
	rows = append(rows, fullResponseRows(line)...)
	return rows
}

func qualificationRows(line string, pass string) []warmup {
	version, ok := pasVersion(line)
	if !ok || (pass != "prime" && pass != "qualify-1" && pass != "qualify-2" && pass != "verify") {
		return nil
	}
	dir := "testdata"
	if line != "2.0" {
		dir += "/" + line
	}
	mode := verdictPositive
	if pass == "prime" {
		mode = verdictPrime
	}
	fixtures := []struct{ name, file string }{
		{"approved", "claimresponse-approved.json"},
		{"denied", "claimresponse-denied-uc08.json"},
		{"pended", "claimresponse-pended.json"},
	}
	forms := []struct{ name, profile string }{
		{"versioned", pasClaimResponseProfile + "|" + version},
		{"unversioned", pasClaimResponseProfile},
		{"meta", ""},
	}
	rows := make([]warmup, 0, 9)
	for _, form := range forms {
		for _, fixture := range fixtures {
			rows = append(rows, warmup{
				identity:     pass + "-" + form.name + "-" + fixture.name,
				file:         dir + "/" + fixture.file,
				resourceType: "ClaimResponse",
				profile:      form.profile,
				mode:         mode,
				line:         line,
			})
		}
	}
	return rows
}

func negativeRows(line string) []warmup {
	version, ok := pasVersion(line)
	if !ok {
		return nil
	}
	dir := "testdata"
	if line != "2.0" {
		dir += "/" + line
	}
	return []warmup{
		{identity: "negative-versioned", file: dir + "/claimresponse-approved.json", resourceType: "ClaimResponse", profile: pasClaimResponseProfile + "|" + version, mode: verdictNegative, line: line},
		{identity: "negative-unversioned", file: dir + "/claimresponse-approved.json", resourceType: "ClaimResponse", profile: pasClaimResponseProfile, mode: verdictNegative, line: line},
		{identity: "negative-meta", file: dir + "/claimresponse-approved.json", resourceType: "ClaimResponse", mode: verdictNegative, line: line},
	}
}

func fixtureBody(row warmup) ([]byte, error) {
	raw, err := fixtures.ReadFile(row.file)
	if err != nil {
		return nil, errors.New("fixture unavailable")
	}
	if row.mode == verdictInitialize {
		return raw, nil
	}
	if row.resourceType == "Bundle" {
		return fullResponseBody(raw, row.mutation)
	}
	resource, extracted, err := claimResponseFixture(raw)
	if err != nil {
		return nil, errors.New("fixture unavailable")
	}
	if !hasExactProfile(resource, pasClaimResponseProfile) {
		return nil, errors.New("fixture unavailable")
	}
	if row.mode == verdictNegative {
		if err := mutateReviewActionCode(resource); err != nil {
			return nil, errors.New("fixture unavailable")
		}
		return json.Marshal(resource)
	}
	if !extracted {
		return raw, nil
	}
	return json.Marshal(resource)
}

func mutateReviewActionCode(resource map[string]any) error {
	matches := findReviewActionCode(resource)
	if len(matches) != 1 || soleValueField(matches[0]) != "valueCodeableConcept" {
		return errors.New("invalid reviewActionCode extension")
	}
	delete(matches[0], "valueCodeableConcept")
	matches[0]["valueBoolean"] = true
	return nil
}

func claimResponseFixture(raw []byte) (map[string]any, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var resource map[string]any
	if err := dec.Decode(&resource); err != nil {
		return nil, false, errors.New("invalid fixture")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, false, errors.New("invalid fixture")
	}
	switch resource["resourceType"] {
	case "ClaimResponse":
		return resource, false, nil
	case "Bundle":
		entries, ok := resource["entry"].([]any)
		if !ok || len(entries) == 0 {
			return nil, false, errors.New("invalid bundle")
		}
		var selected map[string]any
		for _, value := range entries {
			entry, ok := value.(map[string]any)
			if !ok {
				return nil, false, errors.New("invalid bundle entry")
			}
			candidate, ok := entry["resource"].(map[string]any)
			if !ok {
				return nil, false, errors.New("invalid bundle resource")
			}
			if candidate["resourceType"] == "ClaimResponse" {
				if selected != nil {
					return nil, false, errors.New("duplicate ClaimResponse")
				}
				selected = candidate
			}
		}
		if selected == nil {
			return nil, false, errors.New("missing ClaimResponse")
		}
		return selected, true, nil
	default:
		return nil, false, errors.New("wrong resource type")
	}
}

func hasExactProfile(resource map[string]any, want string) bool {
	meta, ok := resource["meta"].(map[string]any)
	if !ok {
		return false
	}
	profiles, ok := meta["profile"].([]any)
	return ok && len(profiles) == 1 && profiles[0] == want
}

func findReviewActionCode(value any) []map[string]any {
	var found []map[string]any
	var visit func(any)
	visit = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if typed["url"] == reviewActionCode {
				found = append(found, typed)
			}
			for _, child := range typed {
				visit(child)
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(value)
	return found
}

func soleValueField(extension map[string]any) string {
	field := ""
	for key := range extension {
		if !strings.HasPrefix(key, "value") {
			continue
		}
		if field != "" {
			return ""
		}
		field = key
	}
	return field
}

type operationOutcome struct {
	ResourceType string         `json:"resourceType"`
	Issue        []outcomeIssue `json:"issue"`
}

type outcomeIssue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Details  struct {
		Coding []struct {
			System string `json:"system"`
			Code   string `json:"code"`
		} `json:"coding"`
	} `json:"details"`
	Diagnostics string   `json:"diagnostics"`
	Expression  []string `json:"expression"`
}

var canonicalOutcomeMembers = []string{
	"resourceType", "issue", "severity", "code", "details", "coding", "system", "diagnostics", "expression",
}

func decodeOperationOutcome(raw []byte) (operationOutcome, error) {
	var outcome operationOutcome
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := validateJSONValue(dec); err != nil {
		return outcome, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return outcome, errors.New("trailing JSON")
	}
	if err := json.Unmarshal(raw, &outcome); err != nil {
		return outcome, err
	}
	return outcome, nil
}

func validateJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		folded := map[string]struct{}{}
		for dec.More() {
			memberToken, err := dec.Token()
			if err != nil {
				return err
			}
			member, ok := memberToken.(string)
			if !ok {
				return errors.New("invalid object member")
			}
			if _, duplicate := seen[member]; duplicate {
				return errors.New("duplicate object member")
			}
			seen[member] = struct{}{}
			fold := strings.ToLower(member)
			if _, collision := folded[fold]; collision {
				return errors.New("ambiguous object member")
			}
			folded[fold] = struct{}{}
			for _, canonical := range canonicalOutcomeMembers {
				if member != canonical && strings.EqualFold(member, canonical) {
					return errors.New("noncanonical object member")
				}
			}
			if err := validateJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for dec.More() {
			if err := validateJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return errors.New("unexpected delimiter")
	}
	return nil
}

// OperationOutcome.issue.code has a required binding to the normative FHIR R4
// 4.0.1 IssueType code system: https://hl7.org/fhir/R4/codesystem-issue-type.html.
func validIssueType(code string) bool {
	switch code {
	case "invalid", "structure", "required", "value", "invariant",
		"security", "login", "unknown", "expired", "forbidden", "suppressed",
		"processing", "not-supported", "duplicate", "multiple-matches", "not-found", "deleted", "too-long", "code-invalid", "extension", "too-costly", "business-rule", "conflict",
		"transient", "lock-error", "no-store", "exception", "timeout", "incomplete", "throttled",
		"informational":
		return true
	default:
		return false
	}
}

func assertVerdict(row warmup, status int, raw []byte) error {
	if status != 200 {
		return errors.New("wrong response status")
	}
	outcome, err := decodeOperationOutcome(raw)
	if err != nil || outcome.ResourceType != "OperationOutcome" {
		return errors.New("not an OperationOutcome")
	}
	if len(outcome.Issue) == 0 {
		return errors.New("invalid outcome")
	}
	for _, issue := range outcome.Issue {
		if (issue.Severity != "fatal" && issue.Severity != "error" && issue.Severity != "warning" && issue.Severity != "information") || !validIssueType(issue.Code) {
			return errors.New("invalid outcome")
		}
	}
	switch row.mode {
	case verdictInitialize:
		for _, issue := range outcome.Issue {
			if suspiciousProfileIssue(issue) {
				return errors.New("unexpected verdict")
			}
		}
		return nil
	case verdictPrime:
		for _, issue := range outcome.Issue {
			if suspiciousProfileIssue(issue) && !allowedPrimeSlicing(row.line, issue) {
				return errors.New("unexpected verdict")
			}
			if (issue.Severity == "error" || issue.Severity == "fatal") && !allowedPrimeSlicing(row.line, issue) {
				return errors.New("unexpected verdict")
			}
		}
		return nil
	case verdictPositive:
		for _, issue := range outcome.Issue {
			if issue.Severity == "error" || issue.Severity == "fatal" || suspiciousProfileIssue(issue) {
				return errors.New("unexpected verdict")
			}
		}
		return nil
	case verdictSupportNegative:
		return assertSupportNegative(row, outcome)
	case verdictNegative:
		matched := 0
		for _, issue := range outcome.Issue {
			if issue.Severity == "error" || issue.Severity == "fatal" {
				if targetedNegative(issue) {
					matched++
				} else {
					return errors.New("unexpected verdict")
				}
			} else if suspiciousProfileIssue(issue) {
				return errors.New("unexpected verdict")
			}
		}
		if matched != 1 {
			return errors.New("unexpected verdict")
		}
		return nil
	default:
		return errors.New("unexpected verdict")
	}
}

func suspiciousProfileIssue(issue outcomeIssue) bool {
	text := strings.ToLower(issue.Diagnostics)
	if strings.Contains(text, "slicing cannot be evaluated") || strings.Contains(text, "failed to retrieve profile") {
		return true
	}
	if strings.Contains(issue.Diagnostics, pasClaimResponseProfile) && (strings.Contains(text, "could not") || strings.Contains(text, "unable to resolve") || strings.Contains(text, "invalid profile")) {
		return true
	}
	for _, coding := range issue.Details.Coding {
		if coding.System == messageIDSystem && coding.Code == "SLICING_CANNOT_BE_EVALUATED" {
			return true
		}
	}
	return false
}

func allowedPrimeSlicing(line string, issue outcomeIssue) bool {
	version, ok := pasVersion(line)
	if !ok || issue.Severity != "error" || issue.Code != "processing" || len(issue.Details.Coding) != 1 || issue.Details.Coding[0].System != messageIDSystem || issue.Details.Coding[0].Code != "SLICING_CANNOT_BE_EVALUATED" || len(issue.Expression) != 1 || issue.Expression[0] != negativeExpression {
		return false
	}
	for _, slice := range []string{"number", "reasonCode", "secondSurgicalOpinionFlag"} {
		want := fmt.Sprintf("Slicing cannot be evaluated: Could not match discriminator (url) for slice Extension.extension:%s in profile %s|%s - the discriminator [url] does not have fixed value, binding or existence assertions", slice, reviewActionProfile, version)
		if issue.Diagnostics == want {
			return true
		}
	}
	return false
}

func targetedNegative(issue outcomeIssue) bool {
	wantDiagnostic := "The Extension '" + reviewActionCode + "' definition allows for the types [CodeableConcept] but found type boolean"
	return issue.Severity == "error" && issue.Code == "processing" &&
		len(issue.Details.Coding) == 1 && issue.Details.Coding[0].System == messageIDSystem && issue.Details.Coding[0].Code == "Extension_EXT_Type" &&
		issue.Diagnostics == wantDiagnostic && len(issue.Expression) == 1 && issue.Expression[0] == negativeExpression
}

// fullResponseRows binds readiness to the complete graph and its offline support
// definitions. The unchanged 2.0 reference-payer response is synthetic;
// 2.1 adds the subscriber member-identifier type required by that PAS version.
func fullResponseRows(line string) []warmup {
	version, ok := pasVersion(line)
	if !ok {
		return nil
	}
	dir := "testdata"
	if line != "2.0" {
		dir += "/" + line
	}
	base := warmup{identity: "full-response-positive", file: dir + "/pas-response-complete.json", resourceType: "Bundle", profile: "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-response-bundle|" + version, mode: verdictPositive, line: line}
	rows := []warmup{base}
	for _, mutation := range []string{"hcpcs", "pos", "encounter"} {
		row := base
		row.identity = "full-response-negative-" + mutation
		row.mode = verdictSupportNegative
		row.mutation = mutation
		row.expectedOutcome = dir + "/pas-response-" + mutation + "-errors.json"
		rows = append(rows, row)
	}
	return rows
}

func fullResponseBody(raw []byte, mutation string) ([]byte, error) {
	var resource map[string]any
	if err := json.Unmarshal(raw, &resource); err != nil || resource["resourceType"] != "Bundle" {
		return nil, errors.New("fixture unavailable")
	}
	if mutation == "" {
		return raw, nil
	}
	// The fixture is committed and pinned by the corpus tests. Mutation paths
	// deliberately select the original Claim, never the response decision.
	entries, _ := resource["entry"].([]any)
	var claim map[string]any
	for _, value := range entries {
		entry, _ := value.(map[string]any)
		candidate, _ := entry["resource"].(map[string]any)
		if candidate["resourceType"] == "Claim" {
			if claim != nil {
				return nil, errors.New("fixture unavailable")
			}
			claim = candidate
		}
	}
	if claim == nil {
		return nil, errors.New("fixture unavailable")
	}
	if mutation == "encounter" {
		extensions, _ := claim["extension"].([]any)
		claim["extension"] = append(extensions, map[string]any{"url": "http://hl7.org/fhir/5.0/StructureDefinition/extension-Claim.encounter", "valueString": "invalid-reference-type"})
	} else {
		key, code := "productOrService", "L9999"
		if mutation == "pos" {
			key, code = "locationCodeableConcept", "98"
		} else if mutation != "hcpcs" {
			return nil, errors.New("fixture unavailable")
		}
		items, _ := claim["item"].([]any)
		if len(items) != 1 {
			return nil, errors.New("fixture unavailable")
		}
		item, _ := items[0].(map[string]any)
		concept, _ := item[key].(map[string]any)
		codings, _ := concept["coding"].([]any)
		if len(codings) != 1 {
			return nil, errors.New("fixture unavailable")
		}
		coding, _ := codings[0].(map[string]any)
		if coding == nil {
			return nil, errors.New("fixture unavailable")
		}
		coding["code"] = code
	}
	return json.Marshal(resource)
}

func assertSupportNegative(row warmup, outcome operationOutcome) error {
	raw, err := fixtures.ReadFile(row.expectedOutcome)
	if err != nil {
		return errors.New("fixture unavailable")
	}
	expected, err := decodeOperationOutcome(raw)
	if err != nil || expected.ResourceType != "OperationOutcome" || len(expected.Issue) != 2 {
		return errors.New("fixture unavailable")
	}
	remaining := append([]outcomeIssue(nil), expected.Issue...)
	for _, issue := range outcome.Issue {
		if suspiciousProfileIssue(issue) {
			return errors.New("unexpected verdict")
		}
		if issue.Severity != "error" && issue.Severity != "fatal" {
			continue
		}
		match := -1
		for i, want := range remaining {
			if reflect.DeepEqual(issue, want) {
				match = i
				break
			}
		}
		if match < 0 {
			return errors.New("unexpected verdict")
		}
		remaining = append(remaining[:match], remaining[match+1:]...)
	}
	if len(remaining) != 0 {
		return errors.New("unexpected verdict")
	}
	return nil
}
