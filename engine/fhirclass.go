package engine

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// classifyFHIR reads an invalid $validate result for the structural level:
// VerdictInvalid when any error or fatal issue is structural or cannot be
// classified, VerdictDeeper when every one is a deeper rule's, and
// VerdictUnavailable when the validator did not judge the payload at all. A
// fatal issue is always structural, whatever its message id, except a call
// the validator answered without reading the payload (HAPI-0992), which is
// unavailable at either severity.
//
// A HAPI validator reports issue.code "processing" for nearly every issue, so
// the kind of issue is read from its message id (shnsdk.Issue.MessageID), never
// from its code. The reading fails closed: an issue with no message id, or with
// one this table does not name as deeper, is structural. That covers every
// structural mistake the real validator lanes report
// (TestValidatorIssueCodesPinned): a missing required element
// (Validation_VAL_Profile_Minimum), and an unknown element, a wrong JSON type or
// an unparseable value, which HAPI reports with no id. An invalid code in a
// code-typed core-enumeration element is one of those unparseable values
// (HAPI-0450/HAPI-1821): structural, never unavailable.
//
// body is the checked message, line its IG line and profile the profile it was
// checked against ("" for its own meta.profile). Nothing is logged; the
// issues' text is read only to classify.
func classifyFHIR(res shnsdk.Result, body []byte, line, profile string) Verdict {
	var errs []fhirIssue
	for _, d := range res.Details {
		if d.Severity == "error" || d.Severity == "fatal" {
			errs = append(errs, fhirIssue{Issue: d, entry: bundleEntryOf(d.Expression)})
		}
	}
	// An invalid result with no error or fatal detail is unclassified.
	if len(errs) == 0 {
		return VerdictInvalid
	}
	invocation := 0
	for _, e := range errs {
		if e.MessageID == "" && hapiNoResourceSupplied.MatchString(e.Diagnostics) {
			invocation++
		}
	}
	if invocation == len(errs) {
		return VerdictUnavailable
	}
	for i := range errs {
		errs[i].deeper = errs[i].Severity == "error" && deeperMessageID(errs[i].MessageID)
	}
	rescuePASClaimSliceMatch(errs, body, line, profile)
	refineSummaries(errs)
	for _, e := range errs {
		if !e.deeper {
			return VerdictInvalid
		}
	}
	return VerdictDeeper
}

// fhirIssue is one error or fatal issue as classifyFHIR reads it. entry is the
// one Bundle.entry index its expressions name, or -1.
type fhirIssue struct {
	shnsdk.Issue
	entry  int
	deeper bool
}

// hapiNoResourceSupplied is HAPI's answer when a $validate call carries no
// resource: the validator never read the payload. The gateway wraps a
// Parameters body in the resource parameter (wrapValidateResource), so after
// the wrap this answer means the call itself is broken, which is the
// validator being unavailable. The wording is pinned from the real validator
// lanes.
var hapiNoResourceSupplied = regexp.MustCompile(`^HAPI-0992: No resource supplied for \$validate operation \(resource is required unless mode is "delete"\)$`)

// invariantMessageID is the message id HAPI gives a failed invariant: the
// constraint's canonical, "<StructureDefinition url>#<key>" (for example
// http://hl7.org/fhir/StructureDefinition/Period#per-1).
var invariantMessageID = regexp.MustCompile(`^https?://[^\s#]+#[A-Za-z][A-Za-z0-9.-]*$`)

// terminologyMessageIDs are the validator's message ids for a code outside
// its code list, including a code system the validator cannot check (a
// licensed one such as X12): the terminology issues the structural level
// records. They come from the validator's message catalog, and each is
// pinned from the real validator lanes, one mutation per shape: a
// CodeableConcept under a required binding (NoValid_1_CC), a Coding under a
// required binding whose code system the validator cannot check (NoValid_12;
// no base or loaded-IG required Coding binding draws on a code system the
// lanes load), a plain code (NoValid_16), a Coding whose code system the
// validator does not know (System_Unknown: a system in the HL7 FHIR namespace
// it has not loaded; that includes a misspelled HL7 system URL, which the
// validator cannot tell apart from one it has not loaded, so it too is
// recorded), and the terminology
// service's own verdict passed through (an unknown code, or a code system it
// cannot check). A member of the family that no lane produces
// stays out until a lane shows it. Any other terminology message id (a
// relative or malformed system, a near-miss of a known one, a value set used
// as a system, a missing binding) refuses, like every id this table does
// not name.
var terminologyMessageIDs = map[string]bool{
	"Terminology_PassThrough_TX_Message": true,
	"Terminology_TX_NoValid_1_CC":        true,
	"Terminology_TX_NoValid_12":          true,
	"Terminology_TX_NoValid_16":          true,
	"Terminology_TX_System_Unknown":      true,
}

// deeperMessageID reports whether a message id names a deeper rule: a
// terminology issue in terminologyMessageIDs, or an invariant. Every other
// id, and no id, is structural.
func deeperMessageID(id string) bool {
	return terminologyMessageIDs[id] || invariantMessageID.MatchString(id)
}

// The two profile-match summaries HAPI adds when a resource matches none of
// the profiles a slice allows. On their own they say only that something
// under them failed.
const (
	summaryBundleEntryNoMatch = "BUNDLE_BUNDLE_ENTRY_MULTIPLE_PROFILES_NO_MATCH"
	summaryProfileNoMatch     = "Validation_VAL_Profile_NoMatch"
)

func isNoMatchSummary(id string) bool {
	return id == summaryBundleEntryNoMatch || id == summaryProfileNoMatch
}

var bundleEntryExpression = regexp.MustCompile(`^Bundle\.entry\[(\d+)\]`)

// bundleEntryOf is the one Bundle.entry index every expression names, or -1
// when there is no expression, one names no entry, or they name more than
// one.
func bundleEntryOf(exprs []string) int {
	entry := -1
	for _, x := range exprs {
		m := bundleEntryExpression.FindStringSubmatch(x)
		if m == nil {
			return -1
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || (entry >= 0 && n != entry) {
			return -1
		}
		entry = n
	}
	return entry
}

// refineSummaries reads a no-match summary as deeper when it is tied to one
// Bundle entry and every other error issue on that entry is deeper: the
// summary then reports nothing the entry's own issues do not. A summary with
// no entry, or on an entry with no other error issue or with a structural
// one, stays structural.
func refineSummaries(errs []fhirIssue) {
	for i, e := range errs {
		if e.deeper || e.Severity != "error" || !isNoMatchSummary(e.MessageID) || e.entry < 0 {
			continue
		}
		others, allDeeper := 0, true
		for j, o := range errs {
			if j == i || o.entry != e.entry || isNoMatchSummary(o.MessageID) {
				continue
			}
			others++
			allDeeper = allDeeper && o.deeper
		}
		errs[i].deeper = others > 0 && allDeeper
	}
}

// The PAS request Bundle's Claim entry allows two candidate profiles, and
// PAS 2.1 and 2.2 validate the Claim against both. When a deeper rule fails
// the Claim's own profile (licensed X12 terminology the validator cannot load,
// on every real request), the Claim matches neither, and HAPI adds the two
// no-match summaries and the other candidate's own requirements: a submit's
// Claim is reported against profile-claim-update (Claim.related minimum 1), an
// update's against profile-claim (Claim.related maximum 0). None of that is a
// defect in the message; the IG's own example bundles fail the same way.
const (
	pasRequestBundleCanonical = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"
	pasClaimCanonical         = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim"
	pasClaimUpdateCanonical   = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update"
)

// validatingAgainst is HAPI's attribution of an issue to the profile it was
// checking: "(validating against <canonical>[|<version>] [<Name>])". An issue
// can carry more than one.
var validatingAgainst = regexp.MustCompile(`\(validating against (\S+) \[[^\]]*\]\)`)

// canonicalReference is any http(s) canonical a summary names, with its
// "|version" if it carries one.
var canonicalReference = regexp.MustCompile(`https?://[^\s,()\[\]]+`)

// rescuePASClaimSliceMatch marks the slice-match consequences on a PAS 2.1 or
// 2.2 request Bundle's Claim entries as deeper. It applies to one Claim entry
// only when at least one no-match summary sits on that entry (the cascade is
// present), and every error issue on that entry is one of:
//   - a no-match summary naming exactly the two candidate profiles;
//   - the sibling candidate's own cardinality requirement
//     (Validation_VAL_Profile_Minimum or _Maximum) attributed only to that
//     sibling, the candidate the Claim is not;
//   - an issue of a recorded kind (deeper), whatever it is attributed to: the
//     validator checks a Claim that declares its own profile against it
//     directly, and those issues carry no attribution.
//
// Attribution matters only for the sibling's requirement. Any other
// structural id on the entry, attributed or not, leaves the whole entry to
// the ordinary reading, where the sibling's requirement is structural and
// refuses.
// Scope is closed: nothing outside this Bundle profile, these lines and this
// slice is touched.
func rescuePASClaimSliceMatch(errs []fhirIssue, body []byte, line, profile string) {
	if line != "2.1" && line != "2.2" {
		return
	}
	var bundle struct {
		ResourceType string `json:"resourceType"`
		Meta         struct {
			Profile []string `json:"profile"`
		} `json:"meta"`
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if json.Unmarshal(body, &bundle) != nil || bundle.ResourceType != "Bundle" ||
		!declaresProfile(append([]string{profile}, bundle.Meta.Profile...), pasRequestBundleCanonical) {
		return
	}
	for n, e := range bundle.Entry {
		declared, ok := declaredPASClaimProfile(e.Resource)
		if !ok {
			continue
		}
		sibling := pasClaimUpdateCanonical
		if declared == pasClaimUpdateCanonical {
			sibling = pasClaimCanonical
		}
		var onEntry []int
		rescuable, summarized := true, false
		for i, iss := range errs {
			if iss.entry != n {
				continue
			}
			onEntry = append(onEntry, i)
			summarized = summarized || isNoMatchSummary(iss.MessageID)
			if !sliceMatchConsequence(iss, sibling) {
				rescuable = false
			}
		}
		if !rescuable || !summarized {
			continue
		}
		for _, i := range onEntry {
			errs[i].deeper = true
		}
	}
}

// sliceMatchConsequence is one issue's test for rescuePASClaimSliceMatch. A
// fatal issue is never a consequence.
func sliceMatchConsequence(iss fhirIssue, sibling string) bool {
	if iss.Severity != "error" {
		return false
	}
	if isNoMatchSummary(iss.MessageID) {
		named := map[string]bool{}
		for _, m := range canonicalReference.FindAllString(iss.Diagnostics, -1) {
			named[canonicalOf(m)] = true
		}
		return len(named) == 2 && named[pasClaimCanonical] && named[pasClaimUpdateCanonical]
	}
	if iss.deeper {
		return true
	}
	if iss.MessageID != siblingMinimum && iss.MessageID != siblingMaximum {
		return false
	}
	// Every "(validating against" must be one the attribution pattern reads:
	// an attribution in any other shape could name the Claim's own profile.
	matches := validatingAgainst.FindAllStringSubmatch(iss.Diagnostics, -1)
	if strings.Count(iss.Diagnostics, "(validating against ") != len(matches) {
		return false
	}
	attributed := map[string]bool{}
	for _, m := range matches {
		attributed[canonicalOf(m[1])] = true
	}
	return len(attributed) == 1 && attributed[sibling]
}

// The sibling candidate's own cardinality requirements HAPI reports on a PAS
// Claim that matches neither candidate: Claim.related minimum 1 (a submit
// checked against profile-claim-update) or maximum 0 (an update checked
// against profile-claim).
const (
	siblingMinimum = "Validation_VAL_Profile_Minimum"
	siblingMaximum = "Validation_VAL_Profile_Maximum"
)

// declaredPASClaimProfile is the candidate profile a Claim entry is built to:
// its meta.profile when that names exactly one candidate, else Claim.related
// (present on an update, absent on a submit). ok is false when the resource is
// not a Claim, when meta.profile names both candidates, or when it names one
// that contradicts Claim.related: the entry is then read without the rescue.
func declaredPASClaimProfile(raw json.RawMessage) (string, bool) {
	var claim struct {
		ResourceType string `json:"resourceType"`
		Meta         struct {
			Profile []string `json:"profile"`
		} `json:"meta"`
		Related []json.RawMessage `json:"related"`
	}
	if json.Unmarshal(raw, &claim) != nil || claim.ResourceType != "Claim" {
		return "", false
	}
	byRelated := pasClaimCanonical
	if len(claim.Related) > 0 {
		byRelated = pasClaimUpdateCanonical
	}
	names := map[string]bool{}
	for _, p := range claim.Meta.Profile {
		if c := canonicalOf(p); c == pasClaimCanonical || c == pasClaimUpdateCanonical {
			names[c] = true
		}
	}
	switch len(names) {
	case 0:
		return byRelated, true
	case 1:
		if !names[byRelated] {
			return "", false
		}
		return byRelated, true
	}
	return "", false
}

// declaresProfile reports whether profiles names canonical, with or without a
// version.
func declaresProfile(profiles []string, canonical string) bool {
	for _, p := range profiles {
		if canonicalOf(p) == canonical {
			return true
		}
	}
	return false
}

// canonicalOf strips a canonical's "|version".
func canonicalOf(ref string) string {
	c, _, _ := strings.Cut(ref, "|")
	return c
}

// wrapValidateResource makes a Parameters resource checkable by $validate. A
// bare Parameters body sent to /Parameters/$validate is read as the
// operation's own input, and HAPI answers HAPI-0992 without judging it; the
// resource under test rides in the operation's resource parameter instead.
// Every other resource is sent as it is. The wrap changes only the request to
// this gateway's own validator, never a relayed byte. An observing validator
// (the validate.result event) sees the wrapped request and the validator's
// own verdict, so for a HAPI-0992 answer that event says invalid where the
// conformance finding says unavailable.
//
// Temporary seam: shnsdk.OperationValidator should wrap it itself (tracked in
// the SHN platform repository). Remove this once a released SDK does; the
// wrapped request is the same either way, so removing it changes nothing a
// sender sees.
func wrapValidateResource(body []byte) []byte {
	var probe struct {
		ResourceType string `json:"resourceType"`
	}
	if json.Unmarshal(body, &probe) != nil || probe.ResourceType != "Parameters" {
		return body
	}
	out := make([]byte, 0, len(body)+80)
	out = append(out, `{"resourceType":"Parameters","parameter":[{"name":"resource","resource":`...)
	out = append(out, body...)
	return append(out, `}]}`...)
}

// ClassifyFHIRForTest is classifyFHIR for the real-validator lane tests
// (test/conformance), which read what the validator actually reports and must
// see the gateway's own reading of it: "valid" for a valid result, else
// "structural", "deeper" or "unavailable". Test-only introspection, like
// ConformanceLevelForTest.
func ClassifyFHIRForTest(res shnsdk.Result, body []byte, line, profile string) string {
	if res.Valid {
		return "valid"
	}
	switch classifyFHIR(res, body, line, profile) {
	case VerdictDeeper:
		return "deeper"
	case VerdictUnavailable:
		return "unavailable"
	}
	return "structural"
}

// WrapValidateResourceForTest is wrapValidateResource, for the same tests.
func WrapValidateResourceForTest(body []byte) []byte { return wrapValidateResource(body) }
