//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The CDex data request Task this gateway builds and sends a facility, which
// no walk reaches, is checked at egress against the profile its builder
// pinned, asked for, on the leg that carries it.

// cdexTaskDataRequestProfile is the CDex profile the SDK's data request
// builder pins (sdk/cdex.go); the row below fails if the builder pins another.
const cdexTaskDataRequestProfile = "http://hl7.org/fhir/us/davinci-cdex/StructureDefinition/cdex-task-data-request"

// builtRequestRow is a request this gateway builds: the scenario that sends
// it, its leg, and the profile its check must ask for.
type builtRequestRow struct {
	name, leg, resourceType, profile string
	run                              func(t *testing.T, gw *Gateway) int
	fixture                          func(t *testing.T) (*Gateway, *pendResumeSubstrate)
}

func builtRequestRows() []builtRequestRow {
	return []builtRequestRow{
		{
			name: "CDex data request", leg: "federated-query", resourceType: "Task",
			profile: cdexTaskDataRequestProfile,
			fixture: func(t *testing.T) (*Gateway, *pendResumeSubstrate) {
				gw, stub := newPendResumeFixture(t, pendFixtureOpts{
					member: "MBR-D-UC05", birthDate: "1968-03-12", familyName: "Johansson-Demo",
					pendedItem: "operative-diagnostic-report",
					extraRoles: map[string]string{"facility": "metro-spine"},
				})
				declareOriginationProfile(gw, "demo")
				return gw, stub
			},
			run: func(t *testing.T, gw *Gateway) int {
				rec := httptest.NewRecorder()
				gw.handleUC05(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc05", strings.NewReader(testRouteBody(gw, "/scenario/uc05", ""))))
				return rec.Code
			},
		},
	}
}

// ownRequestCheck is one check of a request this gateway built on a leg.
type ownRequestCheck struct {
	resourceType, profile string
	fc                    findingContext
}

// ownRequestValidator records each check of a request this gateway built on
// leg and answers it with res; every other check is valid.
type ownRequestValidator struct {
	leg    string
	res    shnsdk.Result
	checks *[]ownRequestCheck
}

func (v ownRequestValidator) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	fc := findingContextFrom(ctx)
	if fc.LegType != v.leg || fc.Whose != "own" {
		return shnsdk.Result{Valid: true}, nil
	}
	var r struct{ ResourceType string }
	_ = json.Unmarshal(validatedBody(body), &r)
	*v.checks = append(*v.checks, ownRequestCheck{resourceType: r.ResourceType, profile: askedProfile(ctx, profile), fc: fc})
	return v.res, nil
}

// The request is checked once per send, as this gateway's own, on its leg
// and correlation, asking for the profile its builder pinned.
func TestBuiltRequestsAreCheckedAgainstTheirPin(t *testing.T) {
	for _, row := range builtRequestRows() {
		t.Run(row.name, func(t *testing.T) {
			gw, stub := row.fixture(t)
			var checks []ownRequestCheck
			gw.cfg.Validator = ownRequestValidator{leg: row.leg, res: shnsdk.Result{Valid: true}, checks: &checks}
			if code := row.run(t, gw); code != http.StatusOK {
				t.Fatalf("scenario answered %d", code)
			}
			sent := 0
			for _, l := range stub.legTypes {
				if l == row.leg {
					sent++
				}
			}
			var requests []ownRequestCheck
			for _, c := range checks {
				if c.resourceType == row.resourceType {
					requests = append(requests, c)
				}
			}
			if sent == 0 || len(requests) != sent {
				t.Fatalf("%d %s checks for %d sends of the leg (checks %+v)", len(requests), row.resourceType, sent, checks)
			}
			for _, c := range requests {
				if c.profile != row.profile {
					t.Fatalf("the check asked for %q, want the pinned %q", c.profile, row.profile)
				}
				if c.fc.Seam != "originate" || c.fc.CorrelationID == "" {
					t.Fatalf("the check's context %+v, want the originating leg's", c.fc)
				}
			}
		})
	}
}

// A defect in a request this gateway built refuses it before it is sent at
// structural and strict, is recorded and the request sent at observe, and is
// not checked at none.
func TestBuiltRequestDefectAtEachLevel(t *testing.T) {
	defect := regexResult(shnsdk.Issue{Severity: "error", Code: "structure", MessageID: "Extension_EXTP_Context_Wrong", Diagnostics: "a structural defect"})
	for _, row := range builtRequestRows() {
		for _, tc := range []struct {
			level    ConformanceEnforcement
			refused  bool
			decision string
		}{
			{EnforcementNone, false, ""},
			{EnforcementObserve, false, "relayed"},
			{EnforcementStructural, true, "refused"},
			{EnforcementStrict, true, "refused"},
		} {
			t.Run(row.name+"/"+tc.level.String(), func(t *testing.T) {
				gw, stub := row.fixture(t)
				gw.cfg.ConformanceEnforcement = tc.level
				var findings []ConformanceFinding
				gw.cfg.Observer = func(e ObserverEvent) {
					var f ConformanceFinding
					if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.LegType == row.leg && f.Whose == "own" {
						findings = append(findings, f)
					}
				}
				var checks []ownRequestCheck
				gw.cfg.Validator = ownRequestValidator{leg: row.leg, res: defect, checks: &checks}
				code := row.run(t, gw)
				gw.drainObserveChecks()
				sent := legAttempted(stub.legTypes, row.leg)
				if tc.refused {
					if code/100 == 2 || sent {
						t.Fatalf("answered %d, leg sent %v: want the request refused before it is sent", code, sent)
					}
				} else if !sent {
					t.Fatalf("answered %d and the leg was not sent (legs %v)", code, stub.legTypes)
				}
				if tc.decision == "" {
					if len(findings) != 0 || len(checks) != 0 {
						t.Fatalf("at none: %d findings, %d checks, want none", len(findings), len(checks))
					}
					return
				}
				if len(findings) == 0 || findings[0].Decision != tc.decision {
					t.Fatalf("findings %+v, want the first %q", findings, tc.decision)
				}
			})
		}
	}
}

// A request that pins no profile, more than one, or cannot be read is a
// build this gateway cannot certify: it reads no pin.
func TestPinnedProfileRefusesAnUncertifiableBuild(t *testing.T) {
	if got, err := pinnedProfile([]byte(`{"resourceType":"Task","meta":{"profile":["` + cdexTaskDataRequestProfile + `"]}}`)); err != nil || got != cdexTaskDataRequestProfile {
		t.Fatalf("one pin read %q %v", got, err)
	}
	for name, body := range map[string]string{
		"no meta":      `{"resourceType":"Task"}`,
		"no profile":   `{"resourceType":"Task","meta":{"profile":[]}}`,
		"two profiles": `{"resourceType":"Task","meta":{"profile":["http://example.org/a","http://example.org/b"]}}`,
		"unreadable":   `{"resourceType":"Task","meta":`,
	} {
		if got, err := pinnedProfile([]byte(body)); err == nil {
			t.Errorf("%s: read pin %q, want no pin", name, got)
		}
	}
}
