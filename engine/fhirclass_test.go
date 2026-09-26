package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The issue text in these rows is what the real validator lanes reported for
// SHN's own PAS request goldens and for each kind of mistake
// (the real-validator tests in the SHN platform repository); only the
// synthetic resource ids are shortened.

func errIssue(id string, expr []string, diag string) shnsdk.Issue {
	return shnsdk.Issue{Severity: "error", Code: "processing", MessageID: id, Expression: expr, Diagnostics: diag}
}

func invalid(issues ...shnsdk.Issue) shnsdk.Result {
	var texts []string
	for _, i := range issues {
		if i.Severity == "error" || i.Severity == "fatal" {
			texts = append(texts, i.Diagnostics)
		}
	}
	return shnsdk.Result{Valid: false, Issues: texts, Details: issues}
}

const (
	pasBundleProfileTag = `{"profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle|2.2.1"]}`
	claimEntry0         = "Bundle.entry[0].resource/*Claim/c1*/"
)

// pasBundle is a PAS request Bundle whose entry 0 is claim.
func pasBundle(claim string) []byte {
	return []byte(`{"resourceType":"Bundle","meta":` + pasBundleProfileTag + `,"type":"collection","entry":[{"resource":` + claim + `},{"resource":{"resourceType":"Patient","id":"p1"}}]}`)
}

const (
	submitClaim = `{"resourceType":"Claim","id":"c1","use":"preauthorization"}`
	updateClaim = `{"resourceType":"Claim","id":"c1","use":"preauthorization","related":[{"claim":{"reference":"Claim/c0"}}]}`
)

func against(canonical, version, name string) string {
	return " (validating against " + canonical + "|" + version + " [" + name + "])"
}

// terminology is the licensed-X12 gap on the Claim, attributed to one
// candidate profile.
func terminology(canonical, version, name string) []shnsdk.Issue {
	return []shnsdk.Issue{
		errIssue("Terminology_PassThrough_TX_Message", []string{claimEntry0 + "Claim.item[0].category"},
			"CodeSystem is unknown and can't be validated: https://codesystem.x12.org/005010/1365 for 'https://codesystem.x12.org/005010/1365#1'"+against(canonical, version, name)),
		errIssue("Terminology_TX_NoValid_1_CC", []string{claimEntry0 + "Claim.item[0].category"},
			"None of the codings provided are in the value set 'X12 278 Requested Service Type'"+against(canonical, version, name)),
	}
}

// cascade22 is what the 2.2 lane reports on a submit's Claim entry.
func cascade22Submit() []shnsdk.Issue {
	out := append(terminology(pasClaimCanonical, "2.2.1", "PASClaim"), terminology(pasClaimUpdateCanonical, "2.2.1", "PASClaimUpdate")...)
	return append(out,
		errIssue(summaryBundleEntryNoMatch, []string{claimEntry0},
			"The entry resource did not match any of the allowed profiles (Type Claim: http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update|2.2.1, http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|2.2.1)"),
		errIssue(summaryProfileNoMatch, []string{"Bundle.entry[0].resource"},
			"Unable to find a match for the specified profile among choices: http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update|2.2.1, http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|2.2.1"),
		errIssue("Validation_VAL_Profile_Minimum", []string{claimEntry0},
			"Claim.related: minimum required = 1, but only found 0 (from http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update|2.2.1)"+against(pasClaimUpdateCanonical, "2.2.1", "PASClaimUpdate")),
	)
}

// cascade21Submit is the 2.1 lane's: its summaries list the candidates with
// no version, in the other order.
func cascade21Submit() []shnsdk.Issue {
	out := append(terminology(pasClaimCanonical, "2.1.0", "PASClaim"), terminology(pasClaimUpdateCanonical, "2.1.0", "PASClaimUpdate")...)
	return append(out,
		errIssue(summaryBundleEntryNoMatch, []string{claimEntry0},
			"The entry resource did not match any of the allowed profiles (Type Claim: http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update, http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim)"),
		errIssue(summaryProfileNoMatch, []string{"Bundle.entry[0].resource"},
			"Unable to find a match for the specified profile among choices: http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim, http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update"),
		errIssue("Validation_VAL_Profile_Minimum", []string{claimEntry0},
			"Claim.related: minimum required = 1, but only found 0 (from http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update|2.1.0)"+against(pasClaimUpdateCanonical, "2.1.0", "PASClaimUpdate")),
	)
}

// cascade22Update is the 2.2 lane's on an update's Claim entry: the sibling is
// profile-claim, whose Claim.related maximum is 0.
func cascade22Update() []shnsdk.Issue {
	out := append(terminology(pasClaimUpdateCanonical, "2.2.1", "PASClaimUpdate"), terminology(pasClaimCanonical, "2.2.1", "PASClaim")...)
	return append(out,
		errIssue(summaryBundleEntryNoMatch, []string{claimEntry0},
			"The entry resource did not match any of the allowed profiles (Type Claim: http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update|2.2.1, http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|2.2.1)"),
		errIssue(summaryProfileNoMatch, []string{"Bundle.entry[0].resource"},
			"Unable to find a match for the specified profile among choices: http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update|2.2.1, http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|2.2.1"),
		errIssue("Validation_VAL_Profile_Maximum", []string{claimEntry0},
			"Claim.related: max allowed = 0, but found 1 (from http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|2.2.1)"+against(pasClaimCanonical, "2.2.1", "PASClaim")),
	)
}

func verdictName(v Verdict) string {
	switch v {
	case VerdictInvalid:
		return "invalid"
	case VerdictDeeper:
		return "deeper"
	case VerdictUnavailable:
		return "unavailable"
	}
	return "valid"
}

func TestClassifyFHIRByMessageID(t *testing.T) {
	coverage := []byte(`{"resourceType":"Coverage"}`)
	for _, tc := range []struct {
		name string
		res  shnsdk.Result
		want Verdict
	}{
		{"no details is unclassified", shnsdk.Result{Issues: []string{"x"}}, VerdictInvalid},
		{"only a warning is unclassified", invalid(shnsdk.Issue{Severity: "warning", MessageID: "Terminology_TX_NoValid_1_CC"}), VerdictInvalid},
		{"missing required element", invalid(errIssue("Validation_VAL_Profile_Minimum", []string{"Coverage"},
			"Coverage.status: minimum required = 1, but only found 0 (from http://hl7.org/fhir/StructureDefinition/Coverage|4.0.1)")), VerdictInvalid},
		{"unknown element has no id", invalid(errIssue("", []string{"Coverage"}, "Unrecognized property 'notAnElement'")), VerdictInvalid},
		{"wrong JSON type", invalid(
			errIssue("", []string{"Coverage.beneficiary"}, "The property beneficiary must be an Object, not a Primitive property (at Coverage.beneficiary)"),
			errIssue("Validation_VAL_Profile_Minimum", []string{"Coverage"}, "Coverage.beneficiary: minimum required = 1, but only found 0 (from http://hl7.org/fhir/StructureDefinition/Coverage|4.0.1)")), VerdictInvalid},
		// An invalid code in a code-typed core-enumeration element is a
		// parse failure, structural at structural and never unavailable.
		{"unparseable core enumeration code", invalid(errIssue("", nil,
			`HAPI-0450: Failed to parse request body as JSON resource. Error was: HAPI-1821: [element="status"] Invalid attribute value "aktive": Unknown ClaimStatus code 'aktive'`)), VerdictInvalid},
		{"malformed date", invalid(errIssue("", nil,
			`HAPI-0450: Failed to parse request body as JSON resource. Error was: HAPI-1821: [element="start"] Invalid attribute value "2026-13-45": Invalid date/time format: "2026-13-45"`)), VerdictInvalid},
		{"invariant", invalid(errIssue("http://hl7.org/fhir/StructureDefinition/Period#per-1", []string{"Coverage.period"},
			"Constraint failed: per-1: 'If present, start SHALL have a lower value than end'")), VerdictDeeper},
		{"licensed terminology", invalid(errIssue("Terminology_PassThrough_TX_Message", []string{"Coverage.type"}, "CodeSystem is unknown")), VerdictDeeper},
		{"a structural error beside a deeper one", invalid(
			errIssue("Terminology_TX_NoValid_1_CC", []string{"Coverage.type"}, "None of the codings provided"),
			errIssue("Validation_VAL_Profile_Minimum", []string{"Coverage"}, "Coverage.status: minimum required = 1")), VerdictInvalid},
		// The classification reads the id, never the text: a structural error
		// whose diagnostics talk about a code system stays structural.
		{"structural error mentioning a code", invalid(errIssue("Validation_VAL_Profile_Minimum", []string{"Coverage.type"},
			"Coverage.type.coding: minimum required = 1 (CodeSystem is unknown and can't be validated: Terminology_TX_NoValid_1_CC)")), VerdictInvalid},
		{"an unknown id is structural", invalid(errIssue("Some_Future_Message", []string{"Coverage"}, "x")), VerdictInvalid},
		{"a lowercase terminology prefix is not the id", invalid(errIssue("terminology_TX_NoValid_1_CC", []string{"Coverage"}, "x")), VerdictInvalid},
		// Only the terminology ids for a code outside its code list are
		// deeper; another terminology id (here, a relative system URI) is not.
		{"a required Coding outside its value set", invalid(errIssue("Terminology_TX_NoValid_12", []string{"Patient.extension[0]"}, "x")), VerdictDeeper},
		{"a required code value outside its value set", invalid(errIssue("Terminology_TX_NoValid_16", []string{"Coverage.costToBeneficiary[0].value.currency"}, "x")), VerdictDeeper},
		{"a Coding whose code system the validator does not know", invalid(errIssue("Terminology_TX_System_Unknown", []string{"Questionnaire.extension[0].value.ofType(Coding)"}, "x")), VerdictDeeper},
		{"a family member no lane has shown is structural", invalid(errIssue("Terminology_TX_NoValid_4", []string{"Coverage.type"}, "x")), VerdictInvalid},
		{"another terminology id is structural", invalid(errIssue("Terminology_TX_System_Relative", []string{"Coverage.type"}, "x")), VerdictInvalid},
		{"a terminology id with a suffix is structural", invalid(errIssue("Terminology_TX_NoValid_1_CC_Extra", []string{"Coverage.type"}, "x")), VerdictInvalid},
		{"a URL id without a constraint key is structural", invalid(errIssue("http://hl7.org/fhir/StructureDefinition/Period", []string{"Coverage"}, "x")), VerdictInvalid},
		{"a fatal issue counts", invalid(shnsdk.Issue{Severity: "fatal", MessageID: "", Diagnostics: "x"}), VerdictInvalid},
		{"a fatal issue is structural whatever its id", invalid(shnsdk.Issue{Severity: "fatal", Code: "processing", MessageID: "Terminology_TX_NoValid_1_CC", Diagnostics: "x"}), VerdictInvalid},
		{"a fatal invariant is structural", invalid(shnsdk.Issue{Severity: "fatal", MessageID: "http://hl7.org/fhir/StructureDefinition/Period#per-1", Diagnostics: "x"}), VerdictInvalid},
		{"warnings beside a deeper error do not count", invalid(
			shnsdk.Issue{Severity: "warning", Diagnostics: "Unrecognized property"},
			errIssue("Terminology_TX_NoValid_1_CC", []string{"Coverage.type"}, "x")), VerdictDeeper},
	} {
		if got := classifyFHIR(tc.res, coverage, "2.0", ""); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, verdictName(got), verdictName(tc.want))
		}
	}
}

const hapi0992 = `HAPI-0992: No resource supplied for $validate operation (resource is required unless mode is "delete")`

// A validator that answered without reading the payload is unavailable, never
// a structural defect of the sender's message.
func TestClassifyFHIRNoResourceSuppliedIsUnavailable(t *testing.T) {
	body := []byte(`{"resourceType":"Parameters"}`)
	for _, tc := range []struct {
		name string
		res  shnsdk.Result
		want Verdict
	}{
		{"HAPI-0992 alone", invalid(errIssue("", nil, hapi0992)), VerdictUnavailable},
		{"HAPI-0992 twice", invalid(errIssue("", nil, hapi0992), errIssue("", nil, hapi0992)), VerdictUnavailable},
		{"HAPI-0992 beside a structural error", invalid(errIssue("", nil, hapi0992), errIssue("", nil, "Unrecognized property 'x'")), VerdictInvalid},
		{"HAPI-0992 beside a deeper error", invalid(errIssue("", nil, hapi0992), errIssue("Terminology_TX_NoValid_1_CC", nil, "x")), VerdictInvalid},
		{"HAPI-0992 with a message id", invalid(errIssue("Some_ID", nil, hapi0992)), VerdictInvalid},
		{"other wording", invalid(errIssue("", nil, "HAPI-0992: No resource supplied")), VerdictInvalid},
	} {
		if got := classifyFHIR(tc.res, body, "2.0", ""); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, verdictName(got), verdictName(tc.want))
		}
	}
}

// The no-match summary refinement, outside the PAS rule's scope: a summary is
// deeper only when it is tied to one entry and every other error there is
// deeper.
func TestClassifyFHIRSummaryRefinement(t *testing.T) {
	bundle := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Coverage"}},{"resource":{"resourceType":"Patient"}}]}`)
	term := func(expr string) shnsdk.Issue {
		return errIssue("Terminology_TX_NoValid_1_CC", []string{expr}, "x")
	}
	summary := func(id string, expr ...string) shnsdk.Issue { return errIssue(id, expr, "did not match") }
	for _, tc := range []struct {
		name string
		res  shnsdk.Result
		want Verdict
	}{
		{"summary over a deeper entry", invalid(summary(summaryProfileNoMatch, "Bundle.entry[0].resource"), term("Bundle.entry[0].resource/*Coverage/x*/Coverage.type")), VerdictDeeper},
		{"both summaries over a deeper entry", invalid(summary(summaryProfileNoMatch, "Bundle.entry[0].resource"), summary(summaryBundleEntryNoMatch, "Bundle.entry[0].resource/*Coverage/x*/"), term("Bundle.entry[0].resource")), VerdictDeeper},
		{"summary with no expression", invalid(summary(summaryProfileNoMatch), term("Bundle.entry[0].resource")), VerdictInvalid},
		{"summary tied to two entries", invalid(summary(summaryProfileNoMatch, "Bundle.entry[0].resource", "Bundle.entry[1].resource"), term("Bundle.entry[0].resource"), term("Bundle.entry[1].resource")), VerdictInvalid},
		{"summary expression outside any entry", invalid(summary(summaryProfileNoMatch, "Bundle"), term("Bundle.entry[0].resource")), VerdictInvalid},
		{"summary alone on its entry", invalid(summary(summaryProfileNoMatch, "Bundle.entry[0].resource"), term("Bundle.entry[1].resource")), VerdictInvalid},
		{"only summaries on the entry", invalid(summary(summaryProfileNoMatch, "Bundle.entry[0].resource"), summary(summaryBundleEntryNoMatch, "Bundle.entry[0].resource")), VerdictInvalid},
		{"summary beside a structural error", invalid(summary(summaryProfileNoMatch, "Bundle.entry[0].resource"), term("Bundle.entry[0].resource"),
			errIssue("Validation_VAL_Profile_Minimum", []string{"Bundle.entry[0].resource"}, "x")), VerdictInvalid},
		{"entry index read in full", invalid(summary(summaryProfileNoMatch, "Bundle.entry[10].resource"), term("Bundle.entry[1].resource")), VerdictInvalid},
		{"a fatal summary stays structural", invalid(shnsdk.Issue{Severity: "fatal", MessageID: summaryProfileNoMatch, Expression: []string{"Bundle.entry[0].resource"}}, term("Bundle.entry[0].resource")), VerdictInvalid},
	} {
		if got := classifyFHIR(tc.res, bundle, "2.0", ""); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, verdictName(got), verdictName(tc.want))
		}
	}
}

// The PAS two-candidate slice rule: closed scope, the declared profile read
// from the Claim, and every issue on the entry accounted for.
func TestClassifyFHIRPASClaimSliceMatch(t *testing.T) {
	plainBundle := func(claim string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + claim + `}]}`)
	}
	withMeta := func(profiles string, related bool) string {
		c := `{"resourceType":"Claim","id":"c1","meta":{"profile":[` + profiles + `]}`
		if related {
			c += `,"related":[{"claim":{"reference":"Claim/c0"}}]`
		}
		return c + `}`
	}
	q := func(s string) string { return `"` + s + `"` }
	insurerMissing := errIssue("Validation_VAL_Profile_Minimum", []string{claimEntry0},
		"Claim.insurer: minimum required = 1, but only found 0 (from http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|2.2.1)"+
			against(pasClaimCanonical, "2.2.1", "PASClaim")+against(pasClaimUpdateCanonical, "2.2.1", "PASClaimUpdate"))
	declaredOnly := errIssue("Validation_VAL_Profile_Minimum", []string{claimEntry0},
		"Claim.provider: minimum required = 1, but only found 0 (from http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|2.2.1)"+against(pasClaimCanonical, "2.2.1", "PASClaim"))
	unattributedTerm := errIssue("Terminology_TX_NoValid_1_CC", []string{claimEntry0 + "Claim.item[0].category"}, "None of the codings provided are in the value set")
	oddAttribution := errIssue("Validation_VAL_Profile_Minimum", []string{claimEntry0},
		"Claim.related: minimum required = 1"+against(pasClaimUpdateCanonical, "2.2.1", "PASClaimUpdate")+" (validating against "+pasClaimCanonical+"|2.2.1)")
	unattributedMinimum := errIssue("Validation_VAL_Profile_Minimum", []string{claimEntry0}, "Claim.provider: minimum required = 1, but only found 0")
	siblingSlice := errIssue("Validation_VAL_Profile_Minimum_SLICE", []string{claimEntry0},
		"Slice 'Claim.supportingInfo:x': a matching slice is required, but not found"+against(pasClaimUpdateCanonical, "2.2.1", "PASClaimUpdate"))
	unversioned := errIssue("Validation_VAL_Profile_Minimum", []string{claimEntry0},
		"Claim.related: minimum required = 1, but only found 0 (validating against "+pasClaimUpdateCanonical+" [PASClaimUpdate])")
	siblingAndAnother := errIssue("Validation_VAL_Profile_Minimum", []string{claimEntry0},
		"Claim.related: minimum required = 1, but only found 0"+against(pasClaimUpdateCanonical, "2.2.1", "PASClaimUpdate")+
			against("http://example.org/StructureDefinition/other", "1.0.0", "Other"))
	otherEntry := errIssue("Validation_VAL_Profile_Minimum_SLICE", []string{"Bundle.entry[1].resource/*Patient/p1*/"},
		"Slice 'Patient.identifier:memberIdentifier': a matching slice is required, but not found")
	withoutLast := func(is []shnsdk.Issue) []shnsdk.Issue { return is[:len(is)-1] }
	withoutSummaries := func(is []shnsdk.Issue) []shnsdk.Issue {
		var out []shnsdk.Issue
		for _, i := range is {
			if !isNoMatchSummary(i.MessageID) {
				out = append(out, i)
			}
		}
		return out
	}
	fatalSibling := func(is []shnsdk.Issue) []shnsdk.Issue {
		out := append([]shnsdk.Issue(nil), is...)
		out[len(out)-1].Severity = "fatal"
		return out
	}
	// atEntry moves every issue from entry 0 to entry n.
	atEntry := func(is []shnsdk.Issue, n string) []shnsdk.Issue {
		out := append([]shnsdk.Issue(nil), is...)
		for i := range out {
			exprs := append([]string(nil), out[i].Expression...)
			for j := range exprs {
				exprs[j] = strings.Replace(exprs[j], "Bundle.entry[0]", "Bundle.entry["+n+"]", 1)
			}
			out[i].Expression = exprs
		}
		return out
	}
	unexpressed := errIssue("Validation_VAL_Profile_Minimum", nil, "Bundle.identifier: minimum required = 1")
	spanning := errIssue("Validation_VAL_Profile_Minimum", []string{claimEntry0, "Bundle.entry[1].resource"},
		"Claim.related: minimum required = 1"+against(pasClaimUpdateCanonical, "2.2.1", "PASClaimUpdate"))
	twoClaims := []byte(`{"resourceType":"Bundle","meta":` + pasBundleProfileTag + `,"type":"collection","entry":[{"resource":` + submitClaim + `},{"resource":` + submitClaim + `}]}`)
	claimSecond := []byte(`{"resourceType":"Bundle","meta":` + pasBundleProfileTag + `,"type":"collection","entry":[{"resource":{"resourceType":"Patient","id":"p1"}},{"resource":` + submitClaim + `}]}`)
	replaceSummary := func(is []shnsdk.Issue, diag string) []shnsdk.Issue {
		out := append([]shnsdk.Issue(nil), is...)
		for i := range out {
			if out[i].MessageID == summaryProfileNoMatch {
				out[i].Diagnostics = diag
			}
		}
		return out
	}

	for _, tc := range []struct {
		name    string
		body    []byte
		line    string
		profile string
		issues  []shnsdk.Issue
		want    Verdict
	}{
		// SHN's own goldens: every issue is a deeper rule or a consequence.
		{"2.2 submit golden", pasBundle(submitClaim), "2.2", "", cascade22Submit(), VerdictDeeper},
		{"2.1 submit golden, unversioned summaries in the other order", pasBundle(submitClaim), "2.1", "", cascade21Submit(), VerdictDeeper},
		{"2.2 update golden", pasBundle(updateClaim), "2.2", "", cascade22Update(), VerdictDeeper},
		{"attribution without a version", pasBundle(submitClaim), "2.2", "", append(withoutLast(cascade22Submit()), unversioned), VerdictDeeper},

		// Closed scope.
		{"not at 2.0", pasBundle(submitClaim), "2.0", "", cascade22Submit(), VerdictInvalid},
		{"no line", pasBundle(submitClaim), "", "", cascade22Submit(), VerdictInvalid},
		{"a Bundle that is not a PAS request", plainBundle(submitClaim), "2.2", "", cascade22Submit(), VerdictInvalid},
		{"a PAS request by the requested profile", plainBundle(submitClaim), "2.2", pasRequestBundleCanonical + "|2.2.1", cascade22Submit(), VerdictDeeper},
		{"the entry is not a Claim", pasBundle(`{"resourceType":"Coverage","id":"c1"}`), "2.2", "", cascade22Submit(), VerdictInvalid},

		// Real defects still refuse.
		{"a Claim missing its insurer", pasBundle(submitClaim), "2.2", "", append(cascade22Submit(), insurerMissing), VerdictInvalid},
		{"a declared-profile error beside the cascade", pasBundle(submitClaim), "2.2", "", append(cascade22Submit(), declaredOnly), VerdictInvalid},
		// A recorded kind is recorded whatever its attribution (a Claim that
		// declares its own profile is checked against it directly, and those
		// issues carry none); a structural id refuses, attributed or not.
		{"unattributed terminology beside the cascade", pasBundle(submitClaim), "2.2", "", append(cascade22Submit(), unattributedTerm), VerdictDeeper},
		{"unattributed structural beside the cascade", pasBundle(submitClaim), "2.2", "", append(cascade22Submit(), unattributedMinimum), VerdictInvalid},
		{"a sibling requirement with an attribution in another shape", pasBundle(submitClaim), "2.2", "", append(withoutLast(cascade22Submit()), oddAttribution), VerdictInvalid},
		{"another structural id attributed only to the sibling", pasBundle(submitClaim), "2.2", "", append(cascade22Submit(), siblingSlice), VerdictInvalid},
		{"an issue attributed to the sibling and another profile", pasBundle(submitClaim), "2.2", "", append(withoutLast(cascade22Submit()), siblingAndAnother), VerdictInvalid},
		{"a structural error on another entry", pasBundle(submitClaim), "2.2", "", append(cascade22Submit(), otherEntry), VerdictInvalid},
		{"the sibling's error read against the wrong Claim", pasBundle(updateClaim), "2.2", "", cascade22Submit(), VerdictInvalid},

		// The declared profile.
		{"meta.profile names the submit profile", pasBundle(withMeta(q(pasClaimCanonical+"|2.2.1"), false)), "2.2", "", cascade22Submit(), VerdictDeeper},
		{"meta.profile names the update profile", pasBundle(withMeta(q(pasClaimUpdateCanonical), true)), "2.2", "", cascade22Update(), VerdictDeeper},
		{"meta.profile names both", pasBundle(withMeta(q(pasClaimCanonical)+","+q(pasClaimUpdateCanonical), false)), "2.2", "", cascade22Submit(), VerdictInvalid},
		{"meta.profile contradicts Claim.related", pasBundle(withMeta(q(pasClaimUpdateCanonical), false)), "2.2", "", cascade22Submit(), VerdictInvalid},
		{"meta.profile names neither", pasBundle(withMeta(q("http://example.org/Claim"), false)), "2.2", "", cascade22Submit(), VerdictDeeper},

		// A fatal issue is never a consequence.
		{"a fatal issue attributed to the sibling", pasBundle(submitClaim), "2.2", "", fatalSibling(cascade22Submit()), VerdictInvalid},

		// The cascade must be present: no summary, no rescue.
		{"the sibling's requirement with no summary", pasBundle(submitClaim), "2.2", "", withoutSummaries(cascade22Submit()), VerdictInvalid},

		// Where the Claim sits.
		{"a Claim at entry 1", claimSecond, "2.2", "", atEntry(cascade22Submit(), "1"), VerdictDeeper},
		{"a Claim at entry 1, issues read at entry 0", claimSecond, "2.2", "", cascade22Submit(), VerdictInvalid},
		{"two Claims, each with its cascade", twoClaims, "2.2", "", append(cascade22Submit(), atEntry(cascade22Submit(), "1")...), VerdictDeeper},
		{"two Claims, a defect on the second", twoClaims, "2.2", "", append(append(cascade22Submit(), atEntry(cascade22Submit(), "1")...), atEntry([]shnsdk.Issue{declaredOnly}, "1")...), VerdictInvalid},
		{"a structural issue with no expression beside the cascade", pasBundle(submitClaim), "2.2", "", append(cascade22Submit(), unexpressed), VerdictInvalid},
		{"a sibling issue spanning two entries", pasBundle(submitClaim), "2.2", "", append(withoutLast(cascade22Submit()), spanning), VerdictInvalid},

		// The summaries name exactly the two candidates.
		{"a summary naming one candidate", pasBundle(submitClaim), "2.2", "",
			replaceSummary(cascade22Submit(), "Unable to find a match for the specified profile among choices: "+pasClaimCanonical+"|2.2.1"), VerdictInvalid},
		{"a summary naming another profile", pasBundle(submitClaim), "2.2", "",
			replaceSummary(cascade22Submit(), "Unable to find a match for the specified profile among choices: http://example.org/StructureDefinition/other"), VerdictInvalid},
		{"a summary naming a profile that starts like a candidate", pasBundle(submitClaim), "2.2", "",
			replaceSummary(cascade22Submit(), "Unable to find a match for the specified profile among choices: http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-inquiry|2.2.1, "+pasClaimUpdateCanonical+"|2.2.1"), VerdictInvalid},
		{"a summary naming both candidates and a third", pasBundle(submitClaim), "2.2", "",
			replaceSummary(cascade22Submit(), "Unable to find a match for the specified profile among choices: "+pasClaimCanonical+"|2.2.1, "+pasClaimUpdateCanonical+"|2.2.1, http://example.org/StructureDefinition/other"), VerdictInvalid},
	} {
		if got := classifyFHIR(invalid(tc.issues...), tc.body, tc.line, tc.profile); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, verdictName(got), verdictName(tc.want))
		}
	}
}

// A canonical is compared whole, so profile-claim never matches inside
// profile-claim-update.
func TestCanonicalOfComparesWhole(t *testing.T) {
	if canonicalOf(pasClaimUpdateCanonical+"|2.2.1") == pasClaimCanonical {
		t.Fatal("profile-claim-update must not read as profile-claim")
	}
	if canonicalOf(pasClaimCanonical+"|2.1.0") != pasClaimCanonical || canonicalOf(pasClaimCanonical) != pasClaimCanonical {
		t.Fatal("a canonical reads the same with or without its version")
	}
}

func TestBundleEntryOf(t *testing.T) {
	for _, tc := range []struct {
		exprs []string
		want  int
	}{
		{nil, -1},
		{[]string{"Bundle.entry[0].resource"}, 0},
		{[]string{"Bundle.entry[3].resource/*Claim/x*/Claim.item[0]", "Bundle.entry[3].resource"}, 3},
		{[]string{"Bundle.entry[0].resource", "Bundle.entry[1].resource"}, -1},
		{[]string{"Bundle.entry[0].resource", "Claim.item"}, -1},
		{[]string{"Bundle.type"}, -1},
		{[]string{"Bundle.entry[x].resource"}, -1},
	} {
		if got := bundleEntryOf(tc.exprs); got != tc.want {
			t.Errorf("bundleEntryOf(%q) = %d, want %d", tc.exprs, got, tc.want)
		}
	}
}

func TestWrapValidateResource(t *testing.T) {
	params := []byte(`{"resourceType":"Parameters","parameter":[{"name":"coverage"}]}`)
	want := `{"resourceType":"Parameters","parameter":[{"name":"resource","resource":` + string(params) + `}]}`
	if got := string(wrapValidateResource(params)); got != want {
		t.Fatalf("a Parameters body rides in the resource parameter:\n got %s\nwant %s", got, want)
	}
	for _, body := range []string{`{"resourceType":"Claim"}`, `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Parameters"}}]}`, `not json`, ``} {
		if got := wrapValidateResource([]byte(body)); !bytes.Equal(got, []byte(body)) {
			t.Errorf("%q is sent as it is, got %s", body, got)
		}
	}
}

// findingsGateway is a gateway at level whose conformance findings are
// captured from the observer carrier.
func findingsGateway(level ConformanceEnforcement) (*Gateway, *[]ConformanceFinding) {
	var findings []ConformanceFinding
	g := &Gateway{cfg: Config{
		ConformanceEnforcement: level,
		Clock:                  func() time.Time { return time.Unix(0, 0).UTC() },
		Observer: func(e ObserverEvent) {
			var f ConformanceFinding
			if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil {
				findings = append(findings, f)
			}
		},
	}}
	return g, &findings
}

// capturingValidator records the body it was sent and answers res.
type capturingValidator struct {
	sent []byte
	res  shnsdk.Result
}

func (c *capturingValidator) Validate(_ context.Context, body []byte, _ string) (shnsdk.Result, error) {
	c.sent = append([]byte(nil), body...)
	return c.res, nil
}

// The choke point at each level: what a structural, a deeper and an
// unavailable verdict do, and that the finding records the sender's own
// bytes, never the wrapped request.
func TestValidateGovernedStructural(t *testing.T) {
	coverage := []byte(`{"resourceType":"Coverage"}`)
	structural := invalid(errIssue("Validation_VAL_Profile_Minimum", []string{"Coverage"}, "Coverage.status: minimum required = 1"))
	deeper := invalid(errIssue("Terminology_TX_NoValid_1_CC", []string{"Coverage.type"}, "None of the codings provided"))
	noResource := invalid(errIssue("", nil, hapi0992))
	for _, tc := range []struct {
		name     string
		level    ConformanceEnforcement
		res      shnsdk.Result
		status   int
		decision string
		verdict  string
	}{
		{"structural at structural", EnforcementStructural, structural, 422, "refused", ""},
		{"deeper at structural", EnforcementStructural, deeper, 0, "relayed", ""},
		{"no resource at structural", EnforcementStructural, noResource, 0, "relayed", "unavailable"},
		{"structural at observe", EnforcementObserve, structural, 0, "relayed", ""},
		{"deeper at observe", EnforcementObserve, deeper, 0, "relayed", ""},
		{"no resource at observe", EnforcementObserve, noResource, 0, "relayed", "unavailable"},
		{"structural at strict", EnforcementStrict, structural, 422, "refused", ""},
		{"deeper at strict", EnforcementStrict, deeper, 422, "refused", ""},
		{"no resource at strict", EnforcementStrict, noResource, 500, "", ""},
	} {
		g, captured := findingsGateway(tc.level)
		v := &capturingValidator{res: tc.res}
		gr := g.validateGoverned(context.Background(), findingContext{LegType: "test"}, v, coverage, "ingress", "2.0", "", false)
		findings := *captured
		if gr.Status != tc.status {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, gr.Status, tc.status, gr.Msg)
		}
		if tc.decision == "" {
			if len(findings) != 0 {
				t.Errorf("%s: a refused unavailable check records no finding, got %+v", tc.name, findings)
			}
			continue
		}
		if len(findings) != 1 {
			t.Fatalf("%s: %d findings, want 1", tc.name, len(findings))
		}
		f := findings[0]
		if f.Decision != tc.decision || f.Verdict != tc.verdict || f.Level != tc.level.String() {
			t.Errorf("%s: finding decision %q verdict %q level %q, want %q %q %q", tc.name, f.Decision, f.Verdict, f.Level, tc.decision, tc.verdict, tc.level)
		}
		if f.PayloadSHA256 != sha256hex(coverage) {
			t.Errorf("%s: the finding hashes the sender's bytes", tc.name)
		}
	}
}

// A Parameters body reaches the validator wrapped, and the finding still
// hashes the sender's own bytes.
func TestValidateGovernedWrapsParameters(t *testing.T) {
	params := []byte(`{"resourceType":"Parameters","parameter":[{"name":"PackageBundle"}]}`)
	g, captured := findingsGateway(EnforcementObserve)
	v := &capturingValidator{res: invalid(errIssue("Terminology_TX_NoValid_1_CC", nil, "x"))}
	g.validateGoverned(context.Background(), findingContext{LegType: "test"}, v, params, "ingress", "2.0", "", false)
	findings := *captured
	if !strings.HasPrefix(string(v.sent), `{"resourceType":"Parameters","parameter":[{"name":"resource","resource":{"resourceType":"Parameters"`) {
		t.Fatalf("the validator must receive the wrapped Parameters, got %s", v.sent)
	}
	if len(findings) != 1 || findings[0].PayloadSHA256 != sha256hex(params) {
		t.Fatalf("the finding hashes the sender's bytes, got %+v", findings)
	}
}
