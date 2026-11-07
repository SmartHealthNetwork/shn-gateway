package engine

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/validatorissue"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// These rows read the core's own conformance reading, so they run in the
// core-only build too. The choke point's rows at each level sit with the
// other choke-point rows (module3_unresolvedprofile_levels_test.go).

// localReportProfile is a profile a facility's own system declares that no
// package a validator lane loads defines.
const localReportProfile = "http://facility.example/fhir/StructureDefinition/local-report"

// profileNotCheckedIssue and profileNotRetrievedIssue are the two errors the
// real 2.1 and 2.2 lanes gave a DiagnosticReport declaring
// localReportProfile, as they reported them (the real-lane pin is
// TestValidatorReadsAnUndefinedDeclaredProfile in the platform's conformance
// suite).
func profileNotCheckedIssue(expr, url string) shnsdk.Issue {
	return shnsdk.Issue{Severity: "error", Code: "processing", MessageID: validatorissue.ProfileUnknownNotPolicy, Expression: []string{expr},
		Diagnostics: "Profile reference '" + url + "' has not been checked because it could not be found, and the validator is set to not fetch unknown profiles"}
}

func profileNotRetrievedIssue(url string) shnsdk.Issue {
	return shnsdk.Issue{Severity: "error", Code: "processing", MessageID: validatorissue.ProfileUnknown,
		Diagnostics: "Invalid profile. Failed to retrieve profile with url=" + url}
}

func declaringReport(profiles ...string) []byte {
	return []byte(`{"resourceType":"DiagnosticReport","id":"fac-dr-9","meta":{"profile":["` + strings.Join(profiles, `","`) +
		`"]},"status":"final","code":{"text":"operative report"},"subject":{"reference":"Patient/x"}}`)
}

// A profile the checked payload itself declares in meta.profile, which the
// validator does not hold, is a check that could not run: the payload was not
// checked against it, so the result is unavailable, never valid and never a
// defect of the payload. Only the declared profile is read this way: the
// validator's issue must name the payload's own meta.profile element, holding
// exactly the URL its text quotes. A profile SHN asks for explicitly, an
// issue naming any other element or URL, a fatal issue, and the text without
// its message id stay structural. Beside a structural error the result is
// invalid for that error; beside deeper errors or a regex check that could not
// run, it is unavailable.
func TestClassifyFHIRUndefinedDeclaredProfile(t *testing.T) {
	report := declaringReport(localReportProfile)
	notChecked := profileNotCheckedIssue("DiagnosticReport.meta.profile[0]", localReportProfile)
	notRetrieved := profileNotRetrievedIssue(localReportProfile)
	structural := regexErr("Validation_VAL_Profile_Minimum", "DiagnosticReport", "DiagnosticReport.issued: minimum required = 1")
	deeper := regexErr("Terminology_TX_NoValid_1_CC", "DiagnosticReport.code", "x")
	regex := regexTimeoutIssue("DiagnosticReport.subject.reference")

	versioned := localReportProfile + "|1.0.0"
	bundle := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"fullUrl":"http://example.org/fhir/Claim/c1","resource":{"resourceType":"Claim","id":"c1","meta":{"profile":["http://example.org/other"]}}},` +
		`{"fullUrl":"http://example.org/fhir/DiagnosticReport/fac-dr-9","meta":{"profile":["` + localReportProfile + `"]},"resource":` + string(report) + `}]}`)
	entryExpr := "Bundle.entry[1].resource/*DiagnosticReport/fac-dr-9*/.meta.profile[0]"

	for _, tc := range []struct {
		name       string
		body       []byte
		profile    string
		res        shnsdk.Result
		want       string
		unresolved []string
	}{
		{"the lane's two issues", report, "", regexResult(notChecked, notRetrieved), "unavailable", []string{localReportProfile}},
		{"the not-checked issue alone", report, "", regexResult(notChecked), "unavailable", []string{localReportProfile}},
		{"the retrieval issue alone: nothing ties it to a declared profile", report, "", regexResult(notRetrieved), "structural", nil},
		{"beside a deeper error", report, "", regexResult(notChecked, notRetrieved, deeper), "unavailable", []string{localReportProfile}},
		{"beside a regex check that could not run", report, "", regexResult(notChecked, notRetrieved, regex), "unavailable", []string{localReportProfile}},
		{"beside a structural error", report, "", regexResult(notChecked, notRetrieved, structural), "structural", []string{localReportProfile}},
		{"a profile SHN asks for explicitly", report, localReportProfile, regexResult(notChecked, notRetrieved), "structural", nil},
		{"a profile SHN asks for explicitly, at another version", report, versioned, regexResult(notChecked, notRetrieved), "structural", nil},
		{"an element the payload does not have", report, "", regexResult(profileNotCheckedIssue("DiagnosticReport.meta.profile[1]", localReportProfile), notRetrieved), "structural", nil},
		{"an element other than meta.profile", report, "", regexResult(profileNotCheckedIssue("DiagnosticReport.code", localReportProfile), notRetrieved), "structural", nil},
		{"another resource type at the root", report, "", regexResult(profileNotCheckedIssue("Claim.meta.profile[0]", localReportProfile), notRetrieved), "structural", nil},
		{"text quoting another URL than the element holds", report, "", regexResult(profileNotCheckedIssue("DiagnosticReport.meta.profile[0]", "http://example.org/other"), notRetrieved), "structural", nil},
		{"a retrieval issue for a URL the payload does not declare", report, "", regexResult(notChecked, profileNotRetrievedIssue("http://example.org/other")), "structural", []string{localReportProfile}},
		{"two expressions", report, "", regexResult(shnsdk.Issue{Severity: "error", MessageID: validatorissue.ProfileUnknownNotPolicy, Expression: []string{"DiagnosticReport.meta.profile[0]", "DiagnosticReport"}, Diagnostics: notChecked.Diagnostics}), "structural", nil},
		{"a fatal issue with the id", report, "", regexResult(shnsdk.Issue{Severity: "fatal", MessageID: validatorissue.ProfileUnknownNotPolicy, Expression: notChecked.Expression, Diagnostics: notChecked.Diagnostics}), "structural", nil},
		{"the text without the id", report, "", regexResult(regexErr("", notChecked.Expression[0], notChecked.Diagnostics)), "structural", nil},
		{"a versioned declaration, quoted as written", declaringReport(versioned), "", regexResult(profileNotCheckedIssue("DiagnosticReport.meta.profile[0]", versioned), profileNotRetrievedIssue(versioned)), "unavailable", []string{versioned}},
		{"the second of two declared profiles", declaringReport("http://example.org/known", localReportProfile), "", regexResult(profileNotCheckedIssue("DiagnosticReport.meta.profile[1]", localReportProfile), notRetrieved), "unavailable", []string{localReportProfile}},
		{"a Bundle entry's own profile", bundle, "", regexResult(profileNotCheckedIssue(entryExpr, localReportProfile), notRetrieved), "unavailable", []string{localReportProfile}},
		// The entry's summary follows from no defect the reading excused: the
		// unresolved profile is a check that did not run, not a cause of it.
		{"a Bundle entry's profile and its entry's summary", bundle, "", regexResult(regexErr(summaryProfileNoMatch, "Bundle.entry[1].resource", "did not match"), profileNotCheckedIssue(entryExpr, localReportProfile), notRetrieved), "structural", []string{localReportProfile}},
		{"a Bundle entry's profile, a deeper error there and its entry's summary", bundle, "", regexResult(regexErr(summaryProfileNoMatch, "Bundle.entry[1].resource", "did not match"), regexErr("Terminology_TX_NoValid_1_CC", "Bundle.entry[1].resource/*DiagnosticReport/fac-dr-9*/.code", "x"), profileNotCheckedIssue(entryExpr, localReportProfile), notRetrieved), "unavailable", []string{localReportProfile}},
		{"an expression naming another entry than the one declaring the URL", bundle, "", regexResult(profileNotCheckedIssue("Bundle.entry[0].resource.meta.profile[0]", localReportProfile), notRetrieved), "structural", nil},
		{"an expression through an element that is not a resource", bundle, "", regexResult(profileNotCheckedIssue("Bundle.entry[1].meta.profile[0]", localReportProfile), notRetrieved), "structural", nil},
	} {
		for _, line := range []string{"2.0", "2.1", "2.2"} {
			if got := ClassifyFHIRForTest(tc.res, tc.body, line, tc.profile); got != tc.want {
				t.Errorf("%s at %s: %s, want %s", tc.name, line, got, tc.want)
			}
			if _, unresolved := classifyFHIRUnchecked(tc.res, tc.body, line, tc.profile); !slices.Equal(unresolved, tc.unresolved) {
				t.Errorf("%s at %s: unresolved %v, want %v", tc.name, line, unresolved, tc.unresolved)
			}
		}
	}
	// A Parameters body reaches the validator wrapped in one more Parameters
	// (wrapValidateResource), and a real lane writes the expression against that
	// wrap; the reading takes the wrap off before reading the body.
	params := []byte(`{"resourceType":"Parameters","parameter":[{"name":"report","resource":` + string(report) + `}]}`)
	wrappedExpr := "Parameters.parameter[0].resource/*Parameters/null*/.parameter[0].resource/*DiagnosticReport/fac-dr-9*/.meta.profile[0]"
	if got := ClassifyFHIRForTest(regexResult(profileNotCheckedIssue(wrappedExpr, localReportProfile)), params, "2.2", ""); got != "unavailable" {
		t.Errorf("a Parameters body's own declaration, as the lane writes it: %s, want unavailable", got)
	}
	if got := ClassifyFHIRForTest(regexResult(profileNotCheckedIssue("Parameters.parameter[0].resource.meta.profile[0]", localReportProfile)), params, "2.2", ""); got != "structural" {
		t.Errorf("an expression naming the wrapper's own element: %s, want structural", got)
	}
	// A body that is not JSON ties no issue to a declared profile.
	if got := ClassifyFHIRForTest(regexResult(notChecked, notRetrieved), []byte(`not json`), "2.2", ""); got != "structural" {
		t.Errorf("a body that is not JSON: %s, want structural", got)
	}
}

// TestClassifyAtLines_ALaneThatNeverJudgedHoldsNothing: a line whose validator
// stopped without a verdict (HAPI-0389) never judged the payload, so it held
// none of its declared profiles: a line lacking one stays a check that could
// not run beside it, and reads as a defect only beside a line that judged the
// payload and held it.
func TestClassifyAtLines_ALaneThatNeverJudgedHoldsNothing(t *testing.T) {
	report := declaringReport(localReportProfile)
	lacks := regexResult(profileNotCheckedIssue("DiagnosticReport.meta.profile[0]", localReportProfile), profileNotRetrievedIssue(localReportProfile))
	stopped := regexResult(didNotFinishIssue)
	held := regexResult(regexErr("Validation_VAL_Profile_Minimum", "DiagnosticReport", "DiagnosticReport.issued: minimum required = 1"))
	verdicts, lacking := classifyAtLines([]string{"2.1", "2.2"}, []shnsdk.Result{lacks, stopped}, report, "")
	if verdicts[0] != VerdictUnavailable || !lacking[0] {
		t.Errorf("beside a lane that never judged: %v (lacking %v), want unavailable", verdicts[0], lacking[0])
	}
	verdicts, _ = classifyAtLines([]string{"2.1", "2.2"}, []shnsdk.Result{lacks, held}, report, "")
	if verdicts[0] != VerdictInvalid {
		t.Errorf("beside a lane that judged it and held the profile: %v, want invalid", verdicts[0])
	}
}

// A finding names an unchecked profile by its size and digest, whatever its
// shape: the URL is quoted from the payload, and one the validator does not
// hold is no published definition, so even an HL7-shaped one may carry
// anything in its id. At most eight are named, sorted by URL; the rest are
// counted.
func TestUnresolvedProfileNames(t *testing.T) {
	digest := func(s string) string {
		return fmt.Sprintf("profile bytes=%d sha256=%x", len(s), sha256.Sum256([]byte(s)))
	}
	for _, in := range []string{
		"http://hl7.org/fhir/us/core/StructureDefinition/us-core-diagnosticreport-note",
		"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|2.1.0",
		"http://hl7.org/fhir/StructureDefinition/Jane-Doe-1970-01-01",
		localReportProfile,
		"http://hl7.org/fhir/us/core/StructureDefinition/x?patient=Jane",
	} {
		if got := namedProfile(in); got != digest(in) {
			t.Errorf("namedProfile(%q) = %q, want %q", in, got, digest(in))
		}
	}
	var many []string
	for i := 11; i >= 0; i-- {
		many = append(many, fmt.Sprintf("http://hl7.org/fhir/StructureDefinition/p%02d", i))
	}
	got := unresolvedProfileNames(many)
	if len(got) != 9 || got[0] != digest("http://hl7.org/fhir/StructureDefinition/p00") || got[7] != digest("http://hl7.org/fhir/StructureDefinition/p07") || got[8] != "and 4 more" {
		t.Errorf("twelve profiles named as %v", got)
	}
	if got := unresolvedProfileNames(nil); got != nil {
		t.Errorf("no profile named as %v, want nil", got)
	}
}

// The certification reading: a lane whose only errors are a profile the
// payload declares and the lane does not hold has no verdict on the payload
// (unavailable), named in the reason; beside any other error the answer stays
// a verdict, invalid; a profile the certification asks for explicitly is a
// verdict too. With other causes the profiles are counted, so the reason stays
// under 256 bytes.
func TestCertificationUndefinedDeclaredProfile(t *testing.T) {
	report := declaringReport(localReportProfile)
	notChecked := profileNotCheckedIssue("DiagnosticReport.meta.profile[0]", localReportProfile)
	notRetrieved := profileNotRetrievedIssue(localReportProfile)
	for _, tc := range []struct {
		name      string
		res       shnsdk.Result
		profile   string
		unchecked bool
		reason    string
	}{
		{"the lane's two issues", regexResult(notChecked, notRetrieved), "", true, "profile unavailable: " + namedProfile(localReportProfile)},
		{"beside a regex check that could not run", regexResult(notChecked, notRetrieved, regexTimeoutIssue("DiagnosticReport.subject.reference")), "", true, "profiles unavailable: 1; a primitive regex check could not run"},
		{"beside a structural error", regexResult(notChecked, notRetrieved, regexErr("Validation_VAL_Profile_Minimum", "DiagnosticReport", "x")), "", false, ""},
		{"a profile the certification asks for", regexResult(notChecked, notRetrieved), localReportProfile, false, ""},
	} {
		got, ok := certificationUnchecked(tc.res, report, "2.2", tc.profile)
		if ok != tc.unchecked {
			t.Errorf("%s: unchecked=%v, want %v", tc.name, ok, tc.unchecked)
			continue
		}
		if ok {
			if r := uncheckedReason(got); r != tc.reason {
				t.Errorf("%s: reason %q, want %q", tc.name, r, tc.reason)
			}
		}
	}
	// A Bundle entry whose profiles did not match, beside a profile it
	// declares that the lane does not hold, is a verdict: the summary follows
	// from no check the lane could not run.
	bundle := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"fullUrl":"http://example.org/fhir/Claim/c1","resource":{"resourceType":"Claim","id":"c1"}},` +
		`{"fullUrl":"http://example.org/fhir/DiagnosticReport/fac-dr-9","resource":` + string(report) + `}]}`)
	entryExpr := "Bundle.entry[1].resource/*DiagnosticReport/fac-dr-9*/.meta.profile[0]"
	if _, ok := certificationUnchecked(regexResult(regexErr(summaryProfileNoMatch, "Bundle.entry[1].resource", "did not match"), profileNotCheckedIssue(entryExpr, localReportProfile), notRetrieved), bundle, "2.2", ""); ok {
		t.Error("a Bundle entry's summary beside its unresolved profile: unchecked, want a verdict")
	}
	if _, ok := certificationUnchecked(regexResult(profileNotCheckedIssue(entryExpr, localReportProfile), notRetrieved), bundle, "2.2", ""); !ok {
		t.Error("a Bundle entry's unresolved profile alone: a verdict, want unchecked")
	}
	long := func(i int) string {
		return "https://example.org/" + strings.Repeat("c", 200) + "/" + strings.Repeat("x", i)
	}
	var manyProfiles, manySystems []string
	for i := 0; i < 5000; i++ {
		manyProfiles = append(manyProfiles, long(i%9)+"/p")
		manySystems = append(manySystems, long(i%7))
	}
	for _, c := range []uncheckedCauses{
		{profiles: manyProfiles},
		{profiles: manyProfiles, regex: true},
		{profiles: manyProfiles, systems: manySystems, regex: true},
		{profiles: manyProfiles, systems: manySystems},
	} {
		if r := uncheckedReason(c); len(r) >= 256 {
			t.Errorf("%d profiles, %d systems, regex %v: %d bytes: %q", len(c.profiles), len(c.systems), c.regex, len(r), r)
		}
	}
	if r := uncheckedReason(uncheckedCauses{profiles: manyProfiles}); !strings.HasPrefix(r, "profile unavailable: profile bytes=") || !strings.HasSuffix(r, fmt.Sprintf(" and %d more", len(manyProfiles)-2)) {
		t.Errorf("profiles alone: %q", r)
	}
}
