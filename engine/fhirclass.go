package engine

import (
	"bytes"
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
// the validator answered without reading the payload (HAPI-0992) or a
// validation it stopped without a verdict (HAPI-0389), which are unavailable
// at either severity (validatorDidNotJudge).
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
	errs := fhirErrors(res)
	// An invalid result with no error or fatal detail is unclassified.
	if len(errs) == 0 {
		return VerdictInvalid
	}
	if validatorDidNotJudge(errs) {
		return VerdictUnavailable
	}
	for i := range errs {
		errs[i].excused = errs[i].Severity == "error" && deeperMessageID(errs[i].MessageID)
	}
	if !allExcused(errs, body, line, profile) {
		return VerdictInvalid
	}
	return VerdictDeeper
}

// fhirIssue is one error or fatal issue as a reading of a $validate result
// sees it. entry is the one Bundle.entry index its expressions name, or -1.
// excused marks an issue the reading in progress does not count against the
// payload: for classifyFHIR a deeper rule's, for certificationUnchecked
// terminology the validator could not check; the summaries and slice-match
// consequences that follow from such issues are then excused alike.
type fhirIssue struct {
	shnsdk.Issue
	entry   int
	excused bool
}

// fhirErrors is every error and fatal issue of res, in order.
func fhirErrors(res shnsdk.Result) []fhirIssue {
	var errs []fhirIssue
	for _, d := range res.Details {
		if d.Severity == "error" || d.Severity == "fatal" {
			errs = append(errs, fhirIssue{Issue: d, entry: bundleEntryOf(d.Expression)})
		}
	}
	return errs
}

// allExcused extends the issues a reading has excused to the workstreams'
// known validator consequences (certificationExcuse) and the summaries that
// follow from them (refineSummaries), and reports whether every issue is then
// excused. The two readings share it, so a summary follows from the same
// evidence in both.
func allExcused(errs []fhirIssue, body []byte, line, profile string) bool {
	certificationExcuse(errs, body, line, profile)
	refineSummaries(errs)
	for _, e := range errs {
		if !e.excused {
			return false
		}
	}
	return true
}

// hapiNoResourceSupplied is HAPI's answer when a $validate call carries no
// resource: the validator never read the payload. The gateway wraps a
// Parameters body in the resource parameter (wrapValidateResource), so after
// the wrap this answer means the call itself is broken, which is the
// validator being unavailable. The wording is pinned from the real validator
// lanes.
var hapiNoResourceSupplied = regexp.MustCompile(`^HAPI-0992: No resource supplied for \$validate operation \(resource is required unless mode is "delete"\)$`)

// hapiDidNotFinish is HAPI's answer when its validator stopped without a
// verdict: matching a Bundle entry against the profiles its slice allows
// failed with no error to report, and the validation ended there, with no
// other entry checked. It is seen on PAS 2.1 and 2.2 request Bundles that
// declare their profile. The issue's text goes on to list what the validator
// had found, so only its opening is matched. The wording is pinned from the
// real validator lanes.
var hapiDidNotFinish = regexp.MustCompile(`^HAPI-0389: Failed to call access method: java\.lang\.Error: failed to validate, but no errors\.`)

// validatorDidNotJudge reports whether every error issue says the validator
// never judged the payload: it answered without reading it (HAPI-0992), or
// stopped without a verdict (HAPI-0389). Such a result is the validator being
// unavailable, never a defect of the message, and never a sign it is valid.
// One issue of any other kind beside them is read as usual.
func validatorDidNotJudge(errs []fhirIssue) bool {
	for _, e := range errs {
		if e.MessageID != "" || !hapiNoResourceSupplied.MatchString(e.Diagnostics) && !hapiDidNotFinish.MatchString(e.Diagnostics) {
			return false
		}
	}
	return len(errs) > 0
}

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

// refineSummaries excuses a no-match summary when it is tied to one Bundle
// entry and every other error issue on that entry is excused: the summary
// then reports nothing the entry's own issues do not. A summary with no
// entry, or on an entry with no other error issue or with one not excused,
// stays counted.
func refineSummaries(errs []fhirIssue) {
	for i, e := range errs {
		if e.excused || e.Severity != "error" || !isNoMatchSummary(e.MessageID) || e.entry < 0 {
			continue
		}
		others, allExcused := 0, true
		for j, o := range errs {
			if j == i || o.entry != e.entry || isNoMatchSummary(o.MessageID) {
				continue
			}
			others++
			allExcused = allExcused && o.excused
		}
		errs[i].excused = others > 0 && allExcused
	}
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
// sender sees. A check against an explicit profile must then still assert the
// profile in the wrapped copy's meta.profile and send no ?profile=
// (validateRequest).
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

// validateRequest is the request a check of body against profile sends this
// gateway's validator: the body and profile to give Validate. A Parameters is
// wrapped (wrapValidateResource). Against an explicit profile, the wrapped copy
// asserts that profile as its own meta.profile, in place of any it declares,
// and no ?profile= is sent: HAPI reads a ?profile= as naming the request's
// Parameters, so it would judge the wrapper, whose one parameter is named
// "resource", instead of the resource inside it. Like the wrap, this changes
// only the request to this gateway's own validator, never a relayed byte; a
// finding's PayloadSHA256 is still the checked body's.
func validateRequest(body []byte, profile string) ([]byte, string) {
	wrapped := wrapValidateResource(body)
	if profile == "" || bytes.Equal(wrapped, body) {
		return wrapped, profile
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return wrapped, profile
	}
	meta := map[string]json.RawMessage{}
	if m, ok := top["meta"]; ok && json.Unmarshal(m, &meta) != nil {
		return wrapped, profile
	}
	if meta == nil { // "meta": null
		meta = map[string]json.RawMessage{}
	}
	p, _ := json.Marshal([]string{profile})
	meta["profile"] = p
	m, _ := json.Marshal(meta)
	top["meta"] = m
	asserted, err := json.Marshal(top)
	if err != nil {
		return wrapped, profile
	}
	return wrapValidateResource(asserted), ""
}

// WrapValidateResourceForTest is wrapValidateResource, for the same tests.
func WrapValidateResourceForTest(body []byte) []byte { return wrapValidateResource(body) }
