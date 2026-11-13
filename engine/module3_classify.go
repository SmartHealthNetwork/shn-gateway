package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
// An error the validator gives because it could not run a primitive's regex
// check (validatorissue.CouldNotRun) is no verdict on the payload. It never makes
// the result invalid: beside a structural error the result is invalid for that
// error; otherwise, alone or beside deeper rules' errors only, the result is
// unavailable, never deeper, since the primitive it could not check may be
// malformed. A profile the payload declares that the validator does not hold
// is read the same way (excuseUnresolvedDeclaredProfiles): the payload was not
// checked against it.
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
	v, _ := classifyFHIRUnchecked(res, body, line, profile)
	return v
}

// fhirIssue is one error or fatal issue as a reading of a $validate result
// sees it. entry is the one Bundle.entry index its expressions name, or -1.
// excused marks an issue the reading in progress does not count against the
// payload: for classifyFHIR a deeper rule's, a regex check that could not run
// or a declared profile the validator does not hold, for
// certificationUnchecked terminology the validator could not check, a regex
// check that could not run or such a profile; the summaries and slice-match
// consequences that follow from such issues are then excused alike.
type fhirIssue struct {
	shnsdk.Issue
	entry   int
	excused bool
	// unresolvedProfile marks an issue excused because the validator did not
	// hold a profile the payload declares (excuseUnresolvedDeclaredProfiles):
	// a check that did not run, not a defect, so it never counts as one a
	// summary follows from (refineSummaries).
	unresolvedProfile bool
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
			// An unresolved declared profile is no defect underneath the
			// summary: the entry's other profiles still did not match.
			if j == i || o.entry != e.entry || isNoMatchSummary(o.MessageID) || o.unresolvedProfile {
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
// this gateway's own validator, never a relayed byte.
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
// (the platform's conformance suite), which read what the validator actually reports and must
// see the gateway's own reading of it: "valid" for a valid result, else
// "structural", "deeper", "unchecked" or "unavailable". Test-only introspection, like
// ConformanceLevelForTest.
func ClassifyFHIRForTest(res shnsdk.Result, body []byte, line, profile string) string {
	if res.Valid {
		return "valid"
	}
	switch classifyFHIR(res, body, line, profile) {
	case VerdictDeeper:
		return "deeper"
	case VerdictUnchecked:
		return "unchecked"
	case VerdictUnavailable:
		return "unavailable"
	}
	return "structural"
}

// embeddedValidationOutcome names a lane's reading of an invalid result for an
// observation that only records it: "unavailable" when the check could not
// run, "unchecked" when every error is a code the lane could not check (no
// verdict that the payload is invalid), else "invalid".
func embeddedValidationOutcome(res shnsdk.Result, body []byte, line, profile string) string {
	switch classifyFHIR(res, body, line, profile) {
	case VerdictUnavailable:
		return "unavailable"
	case VerdictUnchecked:
		return "unchecked"
	}
	return "invalid"
}

// answeredRank orders the readings of a line that judged a payload, from a
// structural defect (0) to a terminology gap (2): judgeLines decides by the
// line closest to valid.
func answeredRank(v Verdict) int {
	switch v {
	case VerdictUnchecked:
		return 2
	case VerdictDeeper:
		return 1
	}
	return 0
}

// uncheckedVerdict is a finding's verdict for a line's reading: "unchecked"
// for a terminology gap, else none (an invalid verdict carries none).
func uncheckedVerdict(v Verdict) string {
	if v == VerdictUnchecked {
		return findingVerdict(v)
	}
	return ""
}

// uncheckedSystemsOf names the code systems a terminology gap's line could
// not check (certificationUnchecked), as certification names them
// (namedCodeSystem: an X12 canonical as written, any other by its size and
// digest, since the name quotes the payload's coding), sorted, at most
// unresolvedProfilesNamed named and the rest counted; nil for any other
// reading.
func uncheckedSystemsOf(v Verdict, res shnsdk.Result, body []byte, line, profile string, excuse func(string) bool) []string {
	if v != VerdictUnchecked {
		return nil
	}
	c, _ := certificationUncheckedExcusing(res, body, line, profile, excuse)
	var out []string
	for i, system := range c.systems {
		if i == unresolvedProfilesNamed {
			out = append(out, fmt.Sprintf("and %d more", len(c.systems)-i))
			break
		}
		out = append(out, namedCodeSystem(system))
	}
	return out
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

// askedProfileKey carries, on a $validate call's context, the profile the
// gateway asked for when validateRequest asserted it into the wrapped body's
// own meta.profile (a Parameters body): below the wrap the asked-for profile
// reads as one the payload declares. The observer reads it from here, so a
// profile the gateway asked for stays an error there as in the governed check.
type askedProfileKey struct{}

// validateRequestAsking is validateRequest, with the context its $validate
// call is made under: one carrying the asked-for profile when the wrap
// asserted it into the body (askedProfileKey). The caller makes the call
// itself, so each call site keeps its own place in validateCallSites.
func validateRequestAsking(ctx context.Context, body []byte, profile string) (context.Context, []byte, string) {
	req, reqProfile := validateRequest(body, profile)
	if profile != "" && reqProfile == "" {
		ctx = context.WithValue(ctx, askedProfileKey{}, profile)
	}
	return ctx, req, reqProfile
}

// askedProfile is the profile a $validate call asked for: the one it names, or
// the one validateRequest asserted into its body (askedProfileKey).
func askedProfile(ctx context.Context, profile string) string {
	if profile != "" {
		return profile
	}
	asked, _ := ctx.Value(askedProfileKey{}).(string)
	return asked
}

// validatedBody is the body a $validate request the gateway sent carries: for
// a Parameters request, which is always the gateway's own wrap
// (wrapValidateResource), the resource its "resource" parameter holds; any
// other request as it is. A reading of the validator's answer reads the body
// the governed check read.
func validatedBody(req []byte) []byte {
	var w struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name     string          `json:"name"`
			Resource json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if json.Unmarshal(req, &w) != nil || w.ResourceType != "Parameters" || len(w.Parameter) != 1 ||
		w.Parameter[0].Name != "resource" || len(w.Parameter[0].Resource) == 0 {
		return req
	}
	return w.Parameter[0].Resource
}

// WrapValidateResourceForTest is wrapValidateResource, for the same tests.
func WrapValidateResourceForTest(body []byte) []byte { return wrapValidateResource(body) }

// UncheckedCodeSystemsForTest is the code systems a result the reading finds
// unchecked names (uncheckedSystemsOf, as a finding names them), for the same
// real-validator lane tests; nil for any other reading.
func UncheckedCodeSystemsForTest(res shnsdk.Result, body []byte, line, profile string) []string {
	v, _ := classifyFHIRUnchecked(res, body, line, profile)
	return uncheckedSystemsOf(v, res, body, line, profile, anyProfile)
}
