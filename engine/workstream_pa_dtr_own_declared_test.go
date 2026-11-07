//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A questionnaire response the gateway's own engine filled declares the
// gateway's own profiles: each is asked for at the DTR check, as the line's
// DTR profile is, so a lane that does not hold one refuses the response as a
// defect wherever the level refuses one. A response a participant's own
// $populate engine filled declares the participant's profiles, read as any
// participant declaration is: a check that could not run.

// engineProfile is a profile an engine writes into a response, which no
// lane holds.
const engineProfile = "http://engine.example/fhir/StructureDefinition/engine-qr"

// engineQR is contextQR declaring engineProfile.
var engineQR = strings.Replace(contextQR, `"resourceType"`, `"meta":{"profile":["`+engineProfile+`"]},"resourceType"`, 1)

func TestDTRCheckAsksForTheOwnEnginesProfiles(t *testing.T) {
	notHeld := regexResult(profileNotCheckedIssue("QuestionnaireResponse.meta.profile[0]", engineProfile), profileNotRetrievedIssue(engineProfile))
	named := []string{namedProfile(engineProfile)}
	for _, tc := range []struct {
		name       string
		level      ConformanceEnforcement
		own        bool
		status     int
		decision   string
		verdict    string
		unresolved []string
		msg        string
	}{
		{"the gateway's engine, at none", EnforcementNone, true, 0, "", "", nil, ""},
		{"the gateway's engine, at observe", EnforcementObserve, true, 0, "relayed", "", nil, ""},
		{"the gateway's engine, at structural", EnforcementStructural, true, http.StatusUnprocessableEntity, "refused", "", nil, ""},
		{"the gateway's engine, at strict", EnforcementStrict, true, http.StatusUnprocessableEntity, "refused", "", nil, ""},
		{"a participant's engine, at none", EnforcementNone, false, 0, "", "", nil, ""},
		{"a participant's engine, at observe", EnforcementObserve, false, 0, "relayed", "unavailable", named, ""},
		{"a participant's engine, at structural", EnforcementStructural, false, 0, "relayed", "unavailable", named, ""},
		{"a participant's engine, at strict", EnforcementStrict, false, http.StatusInternalServerError, "", "", nil, "profile could not be checked: " + named[0]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, captured := levelFindingsGateway(tc.level)
			g.cfg.Validator = failIfCalledValidator{}
			g.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.2": answeringValidator{res: notHeld}})
			status, msg := g.validateDTRQuestionnaireResponse(withFindingContext(context.Background(), findingContext{LegType: "dtr-questionnaire-fetch"}), []byte(engineQR), "2.2", tc.own)
			g.drainObserveChecks()
			if status != tc.status || (tc.msg != "" && msg != tc.msg) {
				t.Fatalf("%d %q, want %d %q", status, msg, tc.status, tc.msg)
			}
			findings := *captured
			if tc.decision == "" {
				if len(findings) != 0 {
					t.Fatalf("want no finding, got %+v", findings)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("%d findings, want 1", len(findings))
			}
			if f := findings[0]; f.Decision != tc.decision || f.Verdict != tc.verdict || !slices.Equal(f.UnresolvedProfiles, tc.unresolved) {
				t.Fatalf("finding decision %q verdict %q unresolved %v, want %q %q %v", f.Decision, f.Verdict, f.UnresolvedProfiles, tc.decision, tc.verdict, tc.unresolved)
			}
		})
	}
}

// Whose engine filled a response is decided by structure: a populator the
// gateway marks as its own engine is the gateway's; every other, the one
// that forwards to a participant's own $populate engine among them, is the
// participant's. A build source keeps it: the automatic fill
// is the gateway's own, a raw response is as its populator was.
func TestTheEngineThatFilledAResponseIsKnownByStructure(t *testing.T) {
	if !ownEngine(&managedPopulator{}) || !ownEngine(&fakePopulator{}) {
		t.Fatal("the gateway's own populators read as a participant's")
	}
	if ownEngine(&nativePopulator{}) || ownEngine(participantPopulator{}) {
		t.Fatal("a participant's $populate engine reads as the gateway's")
	}
	qc := shnsdk.QRContext{PatientRef: "Patient/synthetic", CoverageRef: "Coverage/c", OrderRef: "ServiceRequest/s"}
	if !newAutomaticDTRBuildSource(nil, nil, qc).declaresOwn() || !newRawDTRBuildSource(nil, nil, qc, true).declaresOwn() {
		t.Fatal("a response the gateway's engine filled does not declare its own profiles")
	}
	if newRawDTRBuildSource(nil, nil, qc, false).declaresOwn() || (*dtrBuildSource)(nil).declaresOwn() {
		t.Fatal("a participant's response, or none, declares the gateway's profiles")
	}
}

// askedValidator answers valid and keeps the profile each check asked for.
type askedValidator struct{ asked *[]string }

func (v askedValidator) Validate(_ context.Context, _ []byte, profile string) (shnsdk.Result, error) {
	*v.asked = append(*v.asked, profile)
	return shnsdk.Result{Valid: true}, nil
}

// The profile the gateway adds to a response it completes is the one the
// DTR check then asks for, at every line: the pin can never reach a payer
// without being certified at its own line.
func TestTheDTRProfileAddedIsTheOneTheCheckAsksFor(t *testing.T) {
	qc := shnsdk.QRContext{PatientRef: "Patient/synthetic", CoverageRef: "Coverage/c", OrderRef: "ServiceRequest/s"}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			var asked []string
			g := &Gateway{cfg: Config{Validator: failIfCalledValidator{}, ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{line: askedValidator{&asked}})}}
			out, status, msg := g.completeDTRContext(context.Background(), []byte(contextQR), line, qc, true)
			if status != 0 {
				t.Fatalf("%d %s", status, msg)
			}
			var qr struct {
				Meta struct{ Profile []string } `json:"meta"`
			}
			if err := json.Unmarshal(out, &qr); err != nil {
				t.Fatal(err)
			}
			if len(qr.Meta.Profile) != 1 || !slices.Equal(asked, qr.Meta.Profile) {
				t.Fatalf("added %v, the check asked for %v: want the one added", qr.Meta.Profile, asked)
			}
		})
	}
}

// ownDeclaredSeen wraps a validator and keeps, for each check of a
// questionnaire response, whether it was made as a check of the gateway's
// own profiles, the type of any other resource checked as one, and the type
// of every resource checked.
type ownDeclaredSeen struct {
	inner   shnsdk.Validator
	seen    *[]bool
	others  *[]string
	checked *[]string
}

func (v ownDeclaredSeen) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	var r struct{ ResourceType string }
	if json.Unmarshal(validatedBody(body), &r) == nil {
		own := findingContextFrom(ctx).OwnDeclared
		*v.checked = append(*v.checked, r.ResourceType)
		switch {
		case r.ResourceType == "QuestionnaireResponse":
			*v.seen = append(*v.seen, own)
		case own:
			*v.others = append(*v.others, r.ResourceType)
		}
	}
	return v.inner.Validate(ctx, body, profile)
}

// participantPopulator fills as fakePopulator does without the gateway's
// mark: it stands for a participant's own $populate engine.
type participantPopulator struct{ fill fakePopulator }

func (p participantPopulator) Populate(ctx context.Context, packageJSON []byte, pc PopulateContext) ([]byte, []FilledItem, error) {
	return p.fill.Populate(ctx, packageJSON, pc)
}

// Through a real origination, the home-oxygen route: every check of the
// questionnaire response the gateway's populator filled asks for its
// profiles, and none of one a participant's engine filled does. The mark
// stays on the response's own checks: no other check, the PAS Bundle's
// among them, is made as one of the gateway's profiles.
func TestHomeOxygenChecksTheResponseAsItsEngines(t *testing.T) {
	const canonical = "http://smarthealth.network/fhir/Questionnaire/home-oxygen"
	for name, c := range map[string]struct {
		populator Populator
		own       bool
	}{
		"the gateway's engine":   {fakePopulator{canonical: canonical}, true},
		"a participant's engine": {participantPopulator{fill: fakePopulator{canonical: canonical}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			var seen []bool
			var others, checked []string
			code, body := runHomeOxygen(t, c.populator, ownDeclaredSeen{shnsdk.NewFakeValidator(), &seen, &others, &checked})
			if code != http.StatusOK {
				t.Fatalf("origination: %d %s", code, body)
			}
			if len(seen) < 2 {
				t.Fatalf("%d checks of the response, want the populated and the completed one at least", len(seen))
			}
			if !slices.Contains(checked, "Bundle") {
				t.Fatalf("no PAS Bundle was checked (checked %v): the row cannot see the mark leak", checked)
			}
			if len(others) != 0 {
				t.Fatalf("checks of %v were made as the gateway's own profiles", others)
			}
			for i, own := range seen {
				if own != c.own {
					t.Fatalf("check %d of the response: own %v, want %v (all %v)", i, own, c.own, seen)
				}
			}
		})
	}
}

// The observer reads a lane's answer as the check does: a profile the
// gateway's own engine declared that the lane lacks is an error, one a
// participant's engine declared is a check that could not run.
func TestObserverReadsTheResponseAsItsEngines(t *testing.T) {
	notHeld := regexResult(profileNotCheckedIssue("QuestionnaireResponse.meta.profile[0]", engineProfile), profileNotRetrievedIssue(engineProfile))
	for _, tc := range []struct {
		own  bool
		want string
	}{{true, "invalid"}, {false, "validator unavailable"}} {
		var detail string
		g := &Gateway{cfg: Config{Clock: func() time.Time { return time.Unix(0, 0).UTC() }, Observer: func(e ObserverEvent) {
			if e.Kind == "validate.result" {
				detail = e.Detail
			}
		}}}
		v := observingValidator{inner: answeringValidator{notHeld}, g: g}
		if _, err := v.Validate(withOwnDeclaredProfiles(context.Background(), tc.own), []byte(engineQR), ""); err != nil {
			t.Fatal(err)
		}
		if detail != tc.want {
			t.Fatalf("own %v: validate.result %q, want %q", tc.own, detail, tc.want)
		}
	}
}
