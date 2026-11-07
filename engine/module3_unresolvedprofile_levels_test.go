package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// At each level, through the choke point, in both directions: a profile the
// payload declares that the validator does not hold is a check that could not
// run, decided as every such check is (gateway/docs/CONFIGURATION.md,
// CONFORMANCE_ENFORCEMENT; the conformance-levels design §4): not run at
// none; recorded as unavailable and carried at observe and structural; the
// gateway's own 500 at strict, with no finding, its body naming the profile
// as a finding would. The finding names the profile (unresolvedProfiles). A profile SHN asks for explicitly is not the payload's
// declaration: refused as a structural error wherever the level refuses one.
// Beside a structural error the payload is refused for that error, and the
// finding still names the unchecked profile.
func TestValidateGovernedUndefinedDeclaredProfile(t *testing.T) {
	report := declaringReport(localReportProfile)
	alone := regexResult(profileNotCheckedIssue("DiagnosticReport.meta.profile[0]", localReportProfile), profileNotRetrievedIssue(localReportProfile))
	mixed := regexResult(profileNotCheckedIssue("DiagnosticReport.meta.profile[0]", localReportProfile), profileNotRetrievedIssue(localReportProfile),
		regexErr("Validation_VAL_Profile_Minimum", "DiagnosticReport", "DiagnosticReport.issued: minimum required = 1"))
	named := []string{namedProfile(localReportProfile)}
	// A validator outage at strict still says so, naming no profile.
	{
		g, _ := levelFindingsGateway(EnforcementStrict)
		gr := g.validateGoverned(context.Background(), findingContext{LegType: "test"}, erroringValidator{}, report, "egress", "2.2", "", false)
		if gr.Status != 500 || gr.Msg != "validator unavailable" {
			t.Errorf("an outage at strict: %d %q, want 500 \"validator unavailable\"", gr.Status, gr.Msg)
		}
	}
	for _, dir := range []string{"ingress", "egress"} {
		for _, tc := range []struct {
			name       string
			level      ConformanceEnforcement
			res        shnsdk.Result
			profile    string
			status     int
			decision   string
			verdict    string
			unresolved []string
			msg        string
		}{
			{"declared, at none", EnforcementNone, alone, "", 0, "", "", nil, ""},
			{"declared, at observe", EnforcementObserve, alone, "", 0, "relayed", "unavailable", named, ""},
			{"declared, at structural", EnforcementStructural, alone, "", 0, "relayed", "unavailable", named, ""},
			{"declared, at strict", EnforcementStrict, alone, "", 500, "", "", nil, "profile could not be checked: " + named[0]},
			{"asked for explicitly, at none", EnforcementNone, alone, localReportProfile, 0, "", "", nil, ""},
			{"asked for explicitly, at observe", EnforcementObserve, alone, localReportProfile, 0, "relayed", "", nil, ""},
			{"asked for explicitly, at structural", EnforcementStructural, alone, localReportProfile, 422, "refused", "", nil, ""},
			{"asked for explicitly, at strict", EnforcementStrict, alone, localReportProfile, 422, "refused", "", nil, ""},
			{"beside a structural error, at observe", EnforcementObserve, mixed, "", 0, "relayed", "", named, ""},
			{"beside a structural error, at structural", EnforcementStructural, mixed, "", 422, "refused", "", named, ""},
			{"beside a structural error, at strict", EnforcementStrict, mixed, "", 422, "refused", "", named, ""},
		} {
			g, captured := levelFindingsGateway(tc.level)
			v := answeringValidator{res: tc.res}
			gr := g.validateGoverned(context.Background(), findingContext{LegType: "test"}, v, report, dir, "2.2", tc.profile, false)
			g.drainObserveChecks()
			findings := *captured
			if gr.Status != tc.status {
				t.Errorf("%s %s: status %d, want %d (%s)", dir, tc.name, gr.Status, tc.status, gr.Msg)
			}
			if tc.msg != "" && gr.Msg != tc.msg {
				t.Errorf("%s %s: refusal %q, want %q", dir, tc.name, gr.Msg, tc.msg)
			}
			if tc.decision == "" {
				if len(findings) != 0 {
					t.Errorf("%s %s: want no finding, got %+v", dir, tc.name, findings)
				}
				continue
			}
			if len(findings) != 1 {
				t.Fatalf("%s %s: %d findings, want 1", dir, tc.name, len(findings))
			}
			f := findings[0]
			if f.Decision != tc.decision || f.Verdict != tc.verdict || f.Level != tc.level.String() || f.Direction != dir {
				t.Errorf("%s %s: finding decision %q verdict %q level %q direction %q, want %q %q %q %q", dir, tc.name, f.Decision, f.Verdict, f.Level, f.Direction, tc.decision, tc.verdict, tc.level, dir)
			}
			if !slices.Equal(f.UnresolvedProfiles, tc.unresolved) {
				t.Errorf("%s %s: unresolvedProfiles %v, want %v", dir, tc.name, f.UnresolvedProfiles, tc.unresolved)
			}
			if f.PayloadSHA256 != sha256hex(report) {
				t.Errorf("%s %s: the finding hashes the sender's bytes", dir, tc.name)
			}
		}
	}
}

// erroringValidator is a validator that cannot be reached.
type erroringValidator struct{}

func (erroringValidator) Validate(context.Context, []byte, string) (shnsdk.Result, error) {
	return shnsdk.Result{}, errors.New("connection refused")
}

// levelFindingsGateway is a gateway at level whose conformance findings are
// captured from the observer carrier. Core-only: it needs no workstream.
func levelFindingsGateway(level ConformanceEnforcement) (*Gateway, *[]ConformanceFinding) {
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

// answeringValidator answers every check with res.
type answeringValidator struct{ res shnsdk.Result }

func (v answeringValidator) Validate(context.Context, []byte, string) (shnsdk.Result, error) {
	return v.res, nil
}

// A check that consults several lanes (a payer's answer judged at its
// candidate lines): a declared profile one lane lacks and another holds, as
// another IG line's versioned profile is, was checked at the lane that holds
// it, so the lane that lacks it reads as before, a structural defect, and the
// answer is judged on what the lanes said. Only a profile no lane consulted
// holds is a check that could not run.
// didNotFinishIssue is HAPI's answer when its validator stopped without a
// verdict (hapiDidNotFinish): the lane never judged the payload.
var didNotFinishIssue = shnsdk.Issue{Severity: "error", Diagnostics: "HAPI-0389: Failed to call access method: java.lang.Error: failed to validate, but no errors. profile = x"}

func TestValidateGovernedLines_AProfileAnotherLaneHolds(t *testing.T) {
	const pas22 = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|2.2.1"
	answer := []byte(`{"resourceType":"ClaimResponse","id":"cr-1","meta":{"profile":["` + pas22 + `"]},"status":"active"}`)
	lacks := answeringValidator{regexResult(profileNotCheckedIssue("ClaimResponse.meta.profile[0]", pas22), profileNotRetrievedIssue(pas22))}
	brokenHere := answeringValidator{regexResult(regexErr("Validation_VAL_Profile_Minimum", "ClaimResponse", "ClaimResponse.outcome: minimum required = 1"))}
	for _, row := range []struct {
		name      string
		lanes     []lineLane
		level     ConformanceEnforcement
		status    int
		verdict   string
		decision  string
		lineNotes []LineVerdictSummary
	}{
		{"the routed line lacks it, another holds it with a structural error, at structural",
			[]lineLane{{Line: "2.1", V: lacks, Decisive: true}, {Line: "2.2", V: brokenHere, Extra: true}},
			EnforcementStructural, 422, "", "refused",
			[]LineVerdictSummary{{Line: "2.1", Verdict: "structural"}, {Line: "2.2", Verdict: "structural"}}},
		{"the same at strict",
			[]lineLane{{Line: "2.1", V: lacks, Decisive: true}, {Line: "2.2", V: brokenHere, Extra: true}},
			EnforcementStrict, 422, "", "refused", nil},
		{"no lane holds it: a check that could not run, at structural",
			[]lineLane{{Line: "2.1", V: lacks, Decisive: true}, {Line: "2.2", V: lacks, Extra: true}},
			EnforcementStructural, 0, "unavailable", "relayed", nil},
		// A lane that stopped without a verdict (HAPI-0389) never judged the
		// payload, so it held nothing: the routed line's lack stays a check
		// that could not run.
		{"the other lane never judged the payload, at structural",
			[]lineLane{{Line: "2.1", V: lacks, Decisive: true}, {Line: "2.2", V: answeringValidator{regexResult(didNotFinishIssue)}, Extra: true}},
			EnforcementStructural, 0, "unavailable", "relayed", nil},
	} {
		g, captured := levelFindingsGateway(row.level)
		gr := g.validateGovernedLines(context.Background(), findingContext{LegType: "test"}, row.lanes, answer, "ingress", "2.1", "", false, true)
		if gr.Status != row.status {
			t.Errorf("%s: status %d (%s), want %d", row.name, gr.Status, gr.Msg, row.status)
			continue
		}
		findings := *captured
		if len(findings) != 1 {
			t.Errorf("%s: %d findings, want 1", row.name, len(findings))
			continue
		}
		f := findings[0]
		if f.Verdict != row.verdict || f.Decision != row.decision {
			t.Errorf("%s: finding verdict %q decision %q, want %q %q", row.name, f.Verdict, f.Decision, row.verdict, row.decision)
		}
		// The routed line that lacked the profile reads as before: it is
		// judged first, so it decides.
		if row.status == 422 && f.Line != "2.1" {
			t.Errorf("%s: the finding names line %q, want the routed line 2.1", row.name, f.Line)
		}
		// A profile another lane held is no unresolved profile.
		if row.status == 422 && len(f.UnresolvedProfiles) != 0 {
			t.Errorf("%s: unresolvedProfiles %v, want none: the profile was checked at 2.2", row.name, f.UnresolvedProfiles)
		}
		if row.lineNotes != nil {
			var got []LineVerdictSummary
			for _, l := range f.Lines {
				got = append(got, LineVerdictSummary{Line: l.Line, Verdict: l.Verdict})
			}
			if !slices.Equal(got, row.lineNotes) {
				t.Errorf("%s: lines %v, want %v", row.name, got, row.lineNotes)
			}
		}
	}
}

// At strict, when one line lacks a declared profile and another's validator
// did not answer, the refusal says the validator was unavailable: the profile
// is not why the check could not run there.
func TestValidateGovernedLines_AnOutageBesideAnUnresolvedProfile(t *testing.T) {
	answer := declaringReport(localReportProfile)
	lacks := answeringValidator{regexResult(profileNotCheckedIssue("DiagnosticReport.meta.profile[0]", localReportProfile), profileNotRetrievedIssue(localReportProfile))}
	g, _ := levelFindingsGateway(EnforcementStrict)
	gr := g.validateGovernedLines(context.Background(), findingContext{LegType: "test"},
		[]lineLane{{Line: "2.1", V: lacks, Decisive: true}, {Line: "2.2", V: erroringValidator{}, Extra: true}}, answer, "ingress", "2.1", "", false, true)
	if gr.Status != 500 || gr.Msg != "validator unavailable" {
		t.Fatalf("%d %q, want 500 \"validator unavailable\"", gr.Status, gr.Msg)
	}
}

// Two lanes that each lack the profile the other holds (a Bundle carrying a
// 2.1 and a 2.2 resource, say): each profile was checked at one lane, so
// neither lane's lack is a check that could not run, and the payload is
// judged on what the lanes said.
func TestValidateGovernedLines_EachLaneLacksWhatTheOtherHolds(t *testing.T) {
	const p21 = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|2.1.0"
	const p22 = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|2.2.1"
	answer := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"ClaimResponse","id":"a","meta":{"profile":["` + p21 + `"]}}},` +
		`{"resource":{"resourceType":"ClaimResponse","id":"b","meta":{"profile":["` + p22 + `"]}}}]}`)
	at := func(i int) string { return "Bundle.entry[" + string(rune('0'+i)) + "].resource.meta.profile[0]" }
	lacks21 := answeringValidator{regexResult(profileNotCheckedIssue(at(1), p22), profileNotRetrievedIssue(p22))}
	lacks22 := answeringValidator{regexResult(profileNotCheckedIssue(at(0), p21), profileNotRetrievedIssue(p21))}
	for _, level := range []ConformanceEnforcement{EnforcementStructural, EnforcementStrict} {
		g, captured := levelFindingsGateway(level)
		gr := g.validateGovernedLines(context.Background(), findingContext{LegType: "test"},
			[]lineLane{{Line: "2.1", V: lacks21, Decisive: true}, {Line: "2.2", V: lacks22, Extra: true}}, answer, "ingress", "2.1", "", false, true)
		if gr.Status != 422 {
			t.Errorf("%s: %d %q, want 422: each profile was checked at one lane", level, gr.Status, gr.Msg)
			continue
		}
		if f := (*captured)[0]; f.Line != "2.1" || f.Verdict != "" {
			t.Errorf("%s: finding line %q verdict %q, want the routed line 2.1 deciding", level, f.Line, f.Verdict)
		}
	}
}

// A valid line decides; a line that lacks a profile the valid line held is
// judged (it was checked there), so a claim it is one of is met, and a line
// still qualifying in that claim does not veto the valid answer.
func TestValidateGovernedLines_AValidLineHeldWhatAnotherLacked(t *testing.T) {
	const p20 = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|2.0.1"
	answer := []byte(`{"resourceType":"ClaimResponse","id":"cr","meta":{"profile":["` + p20 + `"]}}`)
	lacks := answeringValidator{regexResult(profileNotCheckedIssue("ClaimResponse.meta.profile[0]", p20), profileNotRetrievedIssue(p20))}
	valid := answeringValidator{shnsdk.Result{Valid: true}}
	g, _ := levelFindingsGateway(EnforcementStrict)
	gr := g.validateGovernedLines(context.Background(), findingContext{LegType: "test"}, []lineLane{
		{Line: "2.0", V: valid, Decisive: true},
		{Line: "2.2", V: lacks, Claims: []int{0}, Extra: true},
		{Line: "2.1", Qualifying: true, Claims: []int{0}},
	}, answer, "ingress", "2.0", "", false, true)
	if gr.Status != 0 {
		t.Fatalf("%d %q, want the valid answer carried", gr.Status, gr.Msg)
	}
}

// A lane that could not run a regex check beside lacking a profile could not
// check the payload for another reason too: at strict the refusal says the
// validator was unavailable.
func TestValidateGovernedLines_ARegexCheckBesideAnUnresolvedProfile(t *testing.T) {
	answer := declaringReport(localReportProfile)
	both := answeringValidator{regexResult(profileNotCheckedIssue("DiagnosticReport.meta.profile[0]", localReportProfile), profileNotRetrievedIssue(localReportProfile), regexTimeoutIssue("DiagnosticReport.subject.reference"))}
	g, _ := levelFindingsGateway(EnforcementStrict)
	gr := g.validateGoverned(context.Background(), findingContext{LegType: "test"}, both, answer, "ingress", "2.2", "", false)
	if gr.Status != 500 || gr.Msg != "validator unavailable" {
		t.Fatalf("%d %q, want 500 \"validator unavailable\"", gr.Status, gr.Msg)
	}
}

// The observer reads the validator's answer to the request the gateway sent
// it: for a Parameters body, the gateway's own wrap. It reads the body inside
// the wrap, as the governed check does, so an undefined profile a parameter's
// resource declares is "validator unavailable" there too.
func TestObservingValidator_ReadsAWrappedParametersBody(t *testing.T) {
	params := []byte(`{"resourceType":"Parameters","parameter":[{"name":"report","resource":` + string(declaringReport(localReportProfile)) + `}]}`)
	wrapped := "Parameters.parameter[0].resource/*Parameters/null*/.parameter[0].resource/*DiagnosticReport/fac-dr-9*/.meta.profile[0]"
	var detail string
	g := &Gateway{cfg: Config{Clock: func() time.Time { return time.Unix(0, 0).UTC() }, Observer: func(e ObserverEvent) {
		if e.Kind == "validate.result" {
			detail = e.Detail
		}
	}}}
	v := observingValidator{inner: answeringValidator{regexResult(profileNotCheckedIssue(wrapped, localReportProfile))}, g: g}
	req, profile := validateRequest(params, "")
	if _, err := v.Validate(context.Background(), req, profile); err != nil {
		t.Fatal(err)
	}
	if detail != "validator unavailable" {
		t.Fatalf("validate.result %q, want \"validator unavailable\"", detail)
	}
}

// A profile the gateway asks for is asserted into the wrapped Parameters
// body's own meta.profile (validateRequest); the observer, below the wrap,
// still reads a lane that lacks it as an error, as the governed check does,
// never as a check that could not run.
func TestObservingValidator_AnAskedForProfileOnAWrappedBodyIsInvalid(t *testing.T) {
	// A profile the gateway asks for, as module 2 asks for its DTR output
	// Parameters profile.
	const asked = "http://example.org/fhir/StructureDefinition/asked-parameters"
	params := []byte(`{"resourceType":"Parameters","parameter":[{"name":"report","resource":` + string(declaringReport(localReportProfile)) + `}]}`)
	asserted := "Parameters.parameter[0].resource/*Parameters/null*/.meta.profile[0]"
	var detail string
	g := &Gateway{cfg: Config{Clock: func() time.Time { return time.Unix(0, 0).UTC() }, Observer: func(e ObserverEvent) {
		if e.Kind == "validate.result" {
			detail = e.Detail
		}
	}}}
	inner := answeringValidator{regexResult(profileNotCheckedIssue(asserted, asked), profileNotRetrievedIssue(asked))}
	askCtx, req, profile := validateRequestAsking(context.Background(), params, asked)
	if v, err := (observingValidator{inner: inner, g: g}).Validate(askCtx, req, profile); err != nil || v.Valid {
		t.Fatalf("%+v %v", v, err)
	}
	if detail != "invalid" {
		t.Fatalf("validate.result %q, want \"invalid\": the gateway asked for the profile", detail)
	}
	// The governed reading of the same answer agrees.
	if got := classifyFHIR(regexResult(profileNotCheckedIssue(asserted, asked), profileNotRetrievedIssue(asked)), params, "", asked); got != VerdictInvalid {
		t.Fatalf("governed verdict %v, want invalid", got)
	}
}

// Once a lane that could not run a regex check is read again as judged (it
// lacked a profile another lane held), its regex check no longer stands for
// the check: the routed line, unavailable only for a profile no lane held,
// says so at strict.
func TestValidateGovernedLines_AReReadLaneLeavesNoOutage(t *testing.T) {
	const pX = "http://example.org/fhir/StructureDefinition/x"
	const pY = "http://example.org/fhir/StructureDefinition/y"
	answer := []byte(`{"resourceType":"ClaimResponse","id":"cr","meta":{"profile":["` + pX + `","` + pY + `"]}}`)
	lacksY := answeringValidator{regexResult(profileNotCheckedIssue("ClaimResponse.meta.profile[1]", pY), profileNotRetrievedIssue(pY))}
	lacksBothAndRegex := answeringValidator{regexResult(profileNotCheckedIssue("ClaimResponse.meta.profile[0]", pX), profileNotRetrievedIssue(pX),
		profileNotCheckedIssue("ClaimResponse.meta.profile[1]", pY), profileNotRetrievedIssue(pY), regexTimeoutIssue("ClaimResponse.id"))}
	g, _ := levelFindingsGateway(EnforcementStrict)
	gr := g.validateGovernedLines(context.Background(), findingContext{LegType: "test"},
		[]lineLane{{Line: "2.1", V: lacksY, Decisive: true}, {Line: "2.2", V: lacksBothAndRegex, Extra: true}}, answer, "ingress", "2.1", "", false, true)
	if gr.Status != 500 || gr.Msg != "profile could not be checked: "+namedProfile(pY) {
		t.Fatalf("%d %q, want 500 naming only the profile no lane held", gr.Status, gr.Msg)
	}
}
