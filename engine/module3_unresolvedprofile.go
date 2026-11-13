package engine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/validatorissue"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A participant's resource may declare in meta.profile a profile of its own
// (a facility's report profile, say) that no package a validator lane loads
// defines. The validator then says it has not checked the resource against
// that profile: a check that could not run, no verdict on the payload. It is
// read that way only for a profile the checked payload itself declares, tied
// by structure: the issue's expression must name the payload's own
// meta.profile element, holding exactly the URL the issue quotes. A profile
// SHN asks the validator for explicitly is not the payload's declaration, so
// a validator without it stays an error, as does every issue that cannot be
// tied to the element.

// profileNotCheckedText and profileNotRetrievedText are the two issues' text
// as the real 2.1 and 2.2 lanes give it, pinned there (the platform's
// conformance suite). The text is read only to confirm the URL the element
// holds; the issue is identified by its message id.
var (
	profileNotCheckedText   = regexp.MustCompile(`^Profile reference '(.+)' has not been checked because it could not be found, and the validator is set to not fetch unknown profiles$`)
	profileNotRetrievedText = regexp.MustCompile(`^Invalid profile\. Failed to retrieve profile with url=(.+)$`)
)

// classifyFHIRUnchecked is classifyFHIR, also returning the profiles the
// payload declares that the validator does not hold (sorted, as declared),
// whatever the verdict. It reads one lane alone: where a check consults
// several, a profile another lane holds is not unchecked (classifyFHIRExcusing).
func classifyFHIRUnchecked(res shnsdk.Result, body []byte, line, profile string) (Verdict, []string) {
	return classifyFHIRExcusing(res, body, line, profile, anyProfile)
}

// anyProfile excuses every declared profile the lane does not hold.
func anyProfile(string) bool { return true }

// noDeclaredProfile excuses none: every declared profile is asked for.
func noDeclaredProfile(string) bool { return false }

// classifyDeclared reads one lane's result as classifyFHIRUnchecked does,
// except for a payload whose declared profiles are the gateway's own
// (fc.OwnDeclared): none is excused, so a lane that does not hold one reads
// it as a defect, as for a profile asked for explicitly. A profile another
// lane the check consulted holds was checked there, as for any declaration.
func classifyDeclared(fc findingContext, res shnsdk.Result, body []byte, line, profile string) (Verdict, []string) {
	if fc.OwnDeclared {
		return classifyFHIRExcusing(res, body, line, profile, noDeclaredProfile)
	}
	return classifyFHIRUnchecked(res, body, line, profile)
}

// verdictExcuse is the declared-profile excuse the verdict of a check whose
// lanes held what excuse says was read with: none for a payload whose declared
// profiles are the gateway's own (classifyDeclared), else excuse.
func verdictExcuse(fc findingContext, excuse func(string) bool) func(string) bool {
	if fc.OwnDeclared {
		return noDeclaredProfile
	}
	return excuse
}

// classifyDeclaredVerdict is classifyDeclared's verdict at no line, as the
// observer reports a lane's result.
func classifyDeclaredVerdict(fc findingContext, res shnsdk.Result, body []byte, profile string) Verdict {
	v, _ := classifyDeclared(fc, res, body, "", profile)
	return v
}

// classifyFHIRExcusing is classifyFHIRUnchecked excusing only the declared
// profiles excuse accepts. A check that consults several lanes passes the
// profiles no lane it consulted holds: a profile one lane lacks and another
// holds (another IG line's, say) was checked there, so the lane that lacks it
// reads as if the profile were any other defect (heldElsewhere).
func classifyFHIRExcusing(res shnsdk.Result, body []byte, line, profile string, excuse func(string) bool) (Verdict, []string) {
	errs := fhirErrors(res)
	// An invalid result with no error or fatal detail is unclassified.
	if len(errs) == 0 {
		return VerdictInvalid, nil
	}
	if validatorDidNotJudge(errs) {
		return VerdictUnavailable, nil
	}
	couldNotRun := false
	for i := range errs {
		if validatorissue.CouldNotRun(errs[i].Issue) {
			errs[i].excused, couldNotRun = true, true
			continue
		}
		errs[i].excused = errs[i].Severity == "error" && deeperMessageID(errs[i].MessageID)
	}
	unresolved := excuseUnresolvedDeclaredProfiles(errs, body, profile, excuse)
	if !allExcused(errs, body, line, profile) {
		return VerdictInvalid, unresolved
	}
	if couldNotRun || len(unresolved) > 0 {
		return VerdictUnavailable, unresolved
	}
	// Every error is a deeper rule's. When each is a code the lane could not
	// check, or a summary or slice-match consequence of one on its own entry,
	// the validator found no defect: it could not look (certificationUnchecked,
	// the one reading certification shares).
	if _, ok := certificationUncheckedExcusing(res, body, line, profile, excuse); ok {
		return VerdictUnchecked, nil
	}
	return VerdictDeeper, nil
}

// excuseUnresolvedDeclaredProfiles excuses the issues saying the validator did
// not check body against a profile body declares, and returns those profiles,
// sorted. An error with ProfileUnknownNotPolicy is excused when its one
// expression names a meta.profile element of a resource in body holding
// exactly the URL its text quotes, and that profile is not the one asked for
// (profile, at any version), and excuse accepts it. An error with
// ProfileUnknown is excused when the URL its text quotes is one of those. Any
// other such issue, a fatal one included, is left to the reading.
func excuseUnresolvedDeclaredProfiles(errs []fhirIssue, body []byte, profile string, excuse func(string) bool) []string {
	var (
		doc     any
		decoded bool
		found   = map[string]bool{}
	)
	for i, e := range errs {
		if e.excused || e.Severity != "error" || e.MessageID != validatorissue.ProfileUnknownNotPolicy || len(e.Expression) != 1 {
			continue
		}
		m := profileNotCheckedText.FindStringSubmatch(e.Diagnostics)
		if m == nil {
			continue
		}
		if !decoded {
			decoded = true
			if json.Unmarshal(body, &doc) != nil {
				doc = nil
			}
		}
		declared, ok := declaredProfileAt(doc, unwrappedExpression(doc, e.Expression[0]))
		if !ok || declared != m[1] || profile != "" && canonicalOf(declared) == canonicalOf(profile) || !excuse(declared) {
			continue
		}
		errs[i].excused, errs[i].unresolvedProfile = true, true
		found[declared] = true
	}
	if len(found) == 0 {
		return nil
	}
	for i, e := range errs {
		if e.excused || e.Severity != "error" || e.MessageID != validatorissue.ProfileUnknown {
			continue
		}
		if m := profileNotRetrievedText.FindStringSubmatch(e.Diagnostics); m != nil && found[m[1]] {
			errs[i].excused, errs[i].unresolvedProfile = true, true
		}
	}
	out := make([]string, 0, len(found))
	for p := range found {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// expressionComment is the "/*Type/id*/" note HAPI writes into an expression
// after a Bundle entry's resource.
var expressionComment = regexp.MustCompile(`/\*[^*]*\*/`)

// expressionStep is one step of an expression: an element name, with an
// index when it repeats.
var expressionStep = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*)(?:\[([0-9]{1,9})\])?$`)

// declaredProfileAt is the string at expr in doc when expr is
// "<Type>[.<step>...].meta.profile[<n>]" and the element holding meta is a
// resource: the root, whose resourceType is Type, or an object with a
// resourceType reached from it. Any other expression is no declaration.
func declaredProfileAt(doc any, expr string) (string, bool) {
	steps := strings.Split(expressionComment.ReplaceAllString(expr, ""), ".")
	if len(steps) < 3 || steps[len(steps)-2] != "meta" || !strings.HasPrefix(steps[len(steps)-1], "profile[") {
		return "", false
	}
	root, ok := doc.(map[string]any)
	if !ok || root["resourceType"] != steps[0] {
		return "", false
	}
	var node any = root
	for i, step := range steps[1:] {
		if i == len(steps)-3 { // the step to meta: node must be a resource
			if r, ok := node.(map[string]any); !ok || !isNonEmptyString(r["resourceType"]) {
				return "", false
			}
		}
		m := expressionStep.FindStringSubmatch(step)
		if m == nil {
			return "", false
		}
		obj, ok := node.(map[string]any)
		if !ok {
			return "", false
		}
		node, ok = obj[m[1]]
		if !ok {
			return "", false
		}
		if m[2] != "" {
			arr, ok := node.([]any)
			n, err := strconv.Atoi(m[2])
			if !ok || err != nil || n >= len(arr) {
				return "", false
			}
			node = arr[n]
		}
	}
	s, ok := node.(string)
	return s, ok && s != ""
}

func isNonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

// namedProfile is how a finding, a refusal or the evidence names a profile
// the validator could not check: its size and digest, never the URL. The URL
// is quoted from the payload, and one the validator does not hold is no
// published definition, whatever its shape, so it may carry anything; a
// participant matches the digest by hashing its own profiles' URLs.
func namedProfile(p string) string {
	hash := sha256.Sum256([]byte(p))
	return fmt.Sprintf("profile bytes=%d sha256=%x", len(p), hash)
}

// unresolvedProfilesNamed is how many profiles a finding names.
const unresolvedProfilesNamed = 8

// unresolvedProfileNames is a finding's UnresolvedProfiles: profiles
// de-duplicated and sorted, the first eight named (namedProfile), and
// "and N more" for the rest. nil names nothing.
func unresolvedProfileNames(profiles []string) []string {
	if len(profiles) == 0 {
		return nil
	}
	sorted := append([]string(nil), profiles...)
	sort.Strings(sorted)
	sorted = compactStrings(sorted)
	out := make([]string, 0, unresolvedProfilesNamed+1)
	for i, p := range sorted {
		if i == unresolvedProfilesNamed {
			out = append(out, fmt.Sprintf("and %d more", len(sorted)-i))
			break
		}
		out = append(out, namedProfile(p))
	}
	return out
}

func compactStrings(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// uncheckedCauses is what a certification lane could not check: code systems
// (uncheckedCodeSystem), a primitive's regex, and profiles the payload
// declares that the lane does not hold.
type uncheckedCauses struct {
	systems  []string
	regex    bool
	profiles []string
}

// lacked is the set of declared profiles one lane that answered did not hold
// (classifyFHIRUnchecked's list; empty for a lane that answered valid).
type lacked map[string]bool

func lackedOf(profiles []string) lacked {
	l := lacked{}
	for _, p := range profiles {
		l[p] = true
	}
	return l
}

// heldElsewhere is the excuse for a lane that could not check some declared
// profiles, given what the other lanes the check consulted lacked: a profile
// is excused only when every one of them lacked it too. With no other lane
// answering, every profile is excused, as for a check of one lane.
func heldElsewhere(answered []lacked) func(string) bool {
	return func(p string) bool {
		for _, l := range answered {
			if !l[p] {
				return false
			}
		}
		return true
	}
}

// laneRead is what one lane said of a payload, other than valid: its result,
// its verdict and the declared profiles it lacked, and the index of its entry
// among the lines a candidate check notes (-1 when none). A verdict that is
// unavailable only for profiles another lane held is read again
// (heldElsewhere).
type laneRead struct {
	lane       lineLane
	res        shnsdk.Result
	verdict    Verdict
	unresolved []string
	noted      int
}

// declaredProfilesNotHeld is the profiles res says the lane does not hold, of
// those the payload declares: what a lane that answered lacked.
func declaredProfilesNotHeld(res shnsdk.Result, body []byte, profile string) []string {
	return excuseUnresolvedDeclaredProfiles(fhirErrors(res), body, profile, anyProfile)
}

// wrappedParameters is the start of an expression a lane writes into a
// Parameters body: the gateway sends one wrapped in another Parameters
// (wrapValidateResource), so the lane names the body as the wrapper's first
// parameter's resource.
const wrappedParameters = "Parameters.parameter[0].resource"

// unwrappedExpression is expr read against the body the gateway sent the lane
// for doc: for a Parameters body, the wrap taken off ("Parameters" and the rest
// of the path, the lane's "/*Type/id*/" note after the wrap dropped), or ""
// when expr does not start inside the wrap, so it names no element of the
// body; any other body's expression as it is.
func unwrappedExpression(doc any, expr string) string {
	root, _ := doc.(map[string]any)
	if root["resourceType"] != "Parameters" {
		return expr
	}
	rest, ok := strings.CutPrefix(expr, wrappedParameters)
	if !ok {
		return ""
	}
	if strings.HasPrefix(rest, "/*") {
		end := strings.Index(rest, "*/")
		if end < 0 {
			return ""
		}
		rest = rest[end+2:]
	}
	if !strings.HasPrefix(rest, ".") {
		return ""
	}
	return "Parameters" + rest
}

// classifyAtLines reads the results several lines' validators gave one
// payload (lines[i] answered results[i]; none valid), as a search for the
// payload's own line does: each line's verdict, a declared profile one line
// lacks and another line that judged the payload held being read at the line
// that lacks it as any other defect (heldElsewhere). lacking[i] reports that
// line i, read alone, could not check the payload and lacks declared profiles
// (whatever else it could not run): such a line does not end a search, since
// another line may hold them and its lack then reads as a defect.
func classifyAtLines(lines []string, results []shnsdk.Result, body []byte, profile string) (verdicts []Verdict, lacking []bool) {
	verdicts, lacking = make([]Verdict, len(results)), make([]bool, len(results))
	unresolved := make([][]string, len(results))
	var judged []lacked
	for i, res := range results {
		verdicts[i], unresolved[i] = classifyFHIRUnchecked(res, body, lines[i], profile)
		if !validatorDidNotJudge(fhirErrors(res)) {
			judged = append(judged, lackedOf(unresolved[i]))
		}
		lacking[i] = verdicts[i] == VerdictUnavailable && len(unresolved[i]) > 0
	}
	excuse := heldElsewhere(judged)
	for i, res := range results {
		if lacking[i] {
			verdicts[i], _ = classifyFHIRExcusing(res, body, lines[i], profile, excuse)
		}
	}
	return verdicts, lacking
}

// UnresolvedDeclaredProfilesForTest is the declared profiles the reading finds
// res did not check body against (classifyFHIRUnchecked), as written, for the
// real-validator lane tests (the platform's conformance suite), which pin how a
// lane quotes them. Test-only introspection, like ClassifyFHIRForTest.
func UnresolvedDeclaredProfilesForTest(res shnsdk.Result, body []byte, line, profile string) []string {
	_, unresolved := classifyFHIRUnchecked(res, body, line, profile)
	return unresolved
}
