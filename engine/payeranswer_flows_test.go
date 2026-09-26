package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// payerAnswerLegs are the legs whose payer answer validateFHIRPayerIngress checks.
var payerAnswerLegs = map[string]bool{"dtr-questionnaire-fetch": true, "pas-claim": true, "pas-claim-update": true}

// answerCheck is one validator call on a payer's answer.
type answerCheck struct {
	LegType      string
	ResourceType string
}

// answerValidator answers every check valid except the ingress check of a payer's
// answer on a leg this gateway originated — the finding context those checks
// carry (seam originate, whose peer, a payer-answer leg) over the answer's own
// shape — which it answers as scripted and records.
type answerValidator struct {
	verdict scriptedVerdict
	// leg limits which leg's answer is scripted; "" scripts every payer-answer leg.
	leg string
	// shapes limits which answers are scripted, by resourceType after the
	// Parameters wrap is removed; nil scripts every payer-answer leg's check.
	shapes map[string]bool

	mu     sync.Mutex
	checks []answerCheck
	all    []answerCheck
}

var parametersWrapPrefix = []byte(`{"resourceType":"Parameters","parameter":[{"name":"resource","resource":`)

func (v *answerValidator) Validate(ctx context.Context, body []byte, _ string) (shnsdk.Result, error) {
	fc := findingContextFrom(ctx)
	inner := body
	if bytes.HasPrefix(body, parametersWrapPrefix) {
		inner = body[len(parametersWrapPrefix) : len(body)-len(`}]}`)]
	}
	var probe struct {
		ResourceType string `json:"resourceType"`
	}
	_ = json.Unmarshal(inner, &probe)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.all = append(v.all, answerCheck{LegType: fc.LegType + "/" + fc.Seam + "/" + fc.Whose, ResourceType: probe.ResourceType})
	if fc.Seam != "originate" || fc.Whose != "peer" || !payerAnswerLegs[fc.LegType] || v.leg != "" && fc.LegType != v.leg || v.shapes != nil && !v.shapes[probe.ResourceType] {
		return shnsdk.Result{Valid: true}, nil
	}
	v.checks = append(v.checks, answerCheck{LegType: fc.LegType, ResourceType: probe.ResourceType})
	return scriptedResult(v.verdict)
}

func (v *answerValidator) answerChecks() []answerCheck {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]answerCheck(nil), v.checks...)
}

// partnerPayerID is a payer identity outside the reference set, under the same
// system the reference payers use.
var partnerPayerID = shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "00002"}

// routeIdentityTo makes payer the identity the member's Coverage names and routes it
// to the fixture's payer holder ("payer"). The reference identity needs nothing: every
// fixture already routes 00001 there.
func routeIdentityTo(t *testing.T, cfg *Config, payer shnsdk.PayerIdentifier) {
	t.Helper()
	if payer == shnsdk.CMSPayerIdentity {
		return
	}
	r, err := NewConfigPayerRouter([]PayerDirectoryEntry{{System: payer.System, Value: payer.Value, HolderID: "payer"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.PayerRouter = r
	switch sor := cfg.SoR.(type) {
	case *censusSoR:
		sor.payer = &payer
	case *homeOxygenSoR:
		sor.payer = payer
		sor.censusSoR.payer = &payer
	default:
		t.Fatalf("no payer identity knob on %T", cfg.SoR)
	}
}

// answerFlow drives one originating flow whose payer answer reaches
// validateFHIRPayerIngress, on a provider gateway at profile and level, routed by the
// payer identity payer. It returns the HTTP status the flow answered its caller with
// (200 when it completed) and that answer's text.
type answerFlow struct {
	name     string
	leg      string          // the answer's leg type
	shapes   map[string]bool // the answer's resourceType(s)
	profiles []string        // the lanes the flow runs on
	run      func(t *testing.T, profile string, payer shnsdk.PayerIdentifier, level ConformanceEnforcement, v *answerValidator, observe func(ObserverEvent)) (int, string)
}

// uc06Pend runs UC-06 to its pended answer: the DTR package fetch
// (runCRDThenDTR) and the pended PAS submit (scenarioToPend).
func uc06Pend(t *testing.T, profile string, payer shnsdk.PayerIdentifier, level ConformanceEnforcement, v *answerValidator, observe func(ObserverEvent)) (int, string) {
	gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC06", birthDate: "1969-07-21", familyName: "Reyes", pendedItem: "functional-status"})
	routeIdentityTo(t, &gw.cfg, payer)
	gw.cfg.OriginationProfile, gw.cfg.ConformanceEnforcement, gw.cfg.Validator, gw.cfg.Observer = profile, level, v, observe
	rec := httptest.NewRecorder()
	if _, ok := gw.scenarioToPend(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc06", nil), "uc06", "MBR-UC06"); ok {
		return http.StatusOK, rec.Body.String()
	}
	return rec.Code, rec.Body.String()
}

// homeOxygen runs the home-oxygen flow: its own DTR package fetch and the PAS
// submit tail.
func homeOxygen(t *testing.T, profile string, payer shnsdk.PayerIdentifier, level ConformanceEnforcement, v *answerValidator, observe func(ObserverEvent)) (int, string) {
	orderJSON, err := buildHomeOxygenDeviceRequest("dr-ox", "Patient/MBR-OX", "Organization/org-dme-ox")
	if err != nil {
		t.Fatal(err)
	}
	supplierJSON, err := buildHomeOxygenSupplier("org-dme-ox")
	if err != nil {
		t.Fatal(err)
	}
	fix := newDispatchFixtureWith(t, "MBR-OX", Demo{BirthDate: "1958-07-14", FamilyName: "Okafor-Oxygen"},
		orderJSON, "Organization/org-dme-ox", supplierJSON, func(c *Config) {
			routeIdentityTo(t, c, payer)
			c.OriginationProfile, c.ConformanceEnforcement, c.Validator, c.Observer = profile, level, v, observe
		})
	rec := httptest.NewRecorder()
	fix.gw.handleHomeOxygen(rec, httptest.NewRequest(http.MethodPost, "/scenario/homeoxygen", nil))
	return rec.Code, rec.Body.String()
}

// uc05 runs UC-05 on the demo lane: the pended PAS submit and, after the
// facility's records, the PAS update.
func uc05(t *testing.T, profile string, payer shnsdk.PayerIdentifier, level ConformanceEnforcement, v *answerValidator, observe func(ObserverEvent)) (int, string) {
	gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-D-UC05", birthDate: "1968-03-12", familyName: "Johansson-Demo",
		pendedItem: "operative-diagnostic-report", extraRoles: map[string]string{"facility": "metro-spine"}})
	routeIdentityTo(t, &gw.cfg, payer)
	gw.cfg.OriginationProfile, gw.cfg.ConformanceEnforcement, gw.cfg.Validator, gw.cfg.Observer = profile, level, v, observe
	rec := httptest.NewRecorder()
	gw.handleUC05(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc05", nil))
	return rec.Code, rec.Body.String()
}

// nextQuestion runs one adaptive $next-question round for a member routed by payer.
func nextQuestion(t *testing.T, profile string, payer shnsdk.PayerIdentifier, level ConformanceEnforcement, v *answerValidator, observe func(ObserverEvent)) (int, string) {
	const subject = "Patient/MBR-COVERED"
	env := newInProcessExchange(t)
	declareFramedDTR(t, env, true)
	env.originator.cfg.OriginationProfile, env.originator.cfg.ConformanceEnforcement = profile, level
	env.originator.cfg.Validator, env.originator.cfg.Observer = v, observe
	route, err := env.originator.selectLegLine(env.payerID, "dtr-questionnaire-fetch", "corr-0")
	if err != nil {
		t.Fatal(err)
	}
	env.payerReturns(LegResult{Response: testResponse(nextQuestionAnswer(t, subject, rawItems(t, adaptiveTree(t, "1", "3"))))})
	res := crdDtrResult{recipient: env.payerID, payer: payer, pci: "pci-covered", patientRef: subject, dtrLine: shnsdk.LineOf(route.Token)}
	reqQR := []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"` + subject + `"}}`)
	items, status, msg, _ := env.originator.nextQuestionLeg(context.Background(), env.req, res, testAdaptiveCanonical, reqQR)
	if status == 0 && len(items) == 2 {
		return http.StatusOK, ""
	}
	return status, msg
}

var answerFlows = []answerFlow{
	{"DTR package", "dtr-questionnaire-fetch", map[string]bool{"Bundle": true, "Parameters": true}, providerLanes, uc06Pend},
	{"PAS submit, pended", "pas-claim", nil, providerLanes, uc06Pend},
	{"home-oxygen DTR package", "dtr-questionnaire-fetch", map[string]bool{"Bundle": true, "Parameters": true}, providerLanes, homeOxygen},
	{"PAS submit tail", "pas-claim", nil, providerLanes, homeOxygen},
	{"PAS submit, pended, UC-05", "pas-claim", nil, []string{"demo"}, uc05},
	{"PAS update", "pas-claim-update", nil, []string{"demo"}, uc05},
	{"adaptive next-question", "dtr-questionnaire-fetch", map[string]bool{"Parameters": true}, providerLanes, nextQuestion},
}

// findingsFor returns the ingress findings a flow recorded for leg.
func findingsFor(events []ObserverEvent, leg string) []ConformanceFinding {
	var out []ConformanceFinding
	for _, e := range events {
		var f ConformanceFinding
		if e.Kind != ConformanceObservedEvent || json.Unmarshal([]byte(e.Detail), &f) != nil {
			continue
		}
		if f.Kind == string(KindFHIRIngress) && f.LegType == leg && f.Whose == "peer" && f.Seam == "originate" {
			out = append(out, f)
		}
	}
	return out
}

// A partner payer's answer on every originating family, on both lanes that relay
// reference bytes, is governed at the gateway's level. The fixture's one validator
// serves every line, so a failing answer is checked once, at the routed line: none makes no validator call; observe
// records and relays; structural refuses a structural defect and records a deeper
// one; strict refuses a defect and a check that could not run.
func TestPayerAnswerFlows_PartnerPayerIsGovernedAtItsLevel(t *testing.T) {
	type want struct {
		status   int
		msg      string
		checks   int
		decision string // "" = no finding for the answer
		verdict  string
	}
	rows := []struct {
		level   ConformanceEnforcement
		verdict scriptedVerdict
		want    want
	}{
		{EnforcementNone, scriptStructural, want{200, "", 0, "", ""}},
		{EnforcementObserve, scriptStructural, want{200, "", 1, "relayed", ""}},
		{EnforcementObserve, scriptOutage, want{200, "", 1, "relayed", "unavailable"}},
		{EnforcementStructural, scriptStructural, want{422, "ingress validation failed", 1, "refused", ""}},
		{EnforcementStructural, scriptDeeper, want{200, "", 1, "relayed", ""}},
		{EnforcementStructural, scriptOutage, want{200, "", 1, "relayed", "unavailable"}},
		{EnforcementStrict, scriptDeeper, want{422, "ingress validation failed", 1, "refused", ""}},
		{EnforcementStrict, scriptOutage, want{500, "validator unavailable", 1, "", ""}},
		{EnforcementStrict, scriptValid, want{200, "", 1, "", ""}},
	}
	for _, flow := range answerFlows {
		for _, profile := range flow.profiles {
			for _, row := range rows {
				name := flow.name + "/" + profile + "/" + row.level.String() + "/" + [...]string{"valid", "structural", "deeper", "outage", "no-resource"}[row.verdict]
				t.Run(name, func(t *testing.T) {
					v := &answerValidator{verdict: row.verdict, leg: flow.leg, shapes: flow.shapes}
					var events []ObserverEvent
					status, body := flow.run(t, profile, partnerPayerID, row.level, v, func(e ObserverEvent) { events = append(events, e) })
					if status != row.want.status || row.want.msg != "" && !strings.Contains(body, row.want.msg) {
						t.Fatalf("status %d %s, want %d naming %q", status, body, row.want.status, row.want.msg)
					}
					if got := v.answerChecks(); len(got) != row.want.checks {
						t.Fatalf("%d checks of the partner's answer (%+v), want %d", len(got), got, row.want.checks)
					}
					findings := findingsFor(events, flow.leg)
					if row.want.decision == "" {
						if len(findings) != 0 {
							t.Fatalf("findings %+v, want none", findings)
						}
						return
					}
					if len(findings) != 1 {
						t.Fatalf("%d findings for the answer (%+v), want 1", len(findings), findings)
					}
					f := findings[0]
					if f.Decision != row.want.decision || f.Verdict != row.want.verdict || f.CorrelationID == "" || f.Level != row.level.String() ||
						f.DeclaredLine == "" || len(f.Lines) != 1 || f.Lines[0].Line != f.DeclaredLine {
						t.Fatalf("finding %+v, want decision %q verdict %q with a correlation id and the routed line alone", f, row.want.decision, row.want.verdict)
					}
				})
			}
		}
	}
}

// The network's reference payers, by identity, keep the skip on every family and
// both lanes: no validator call on their answer at any level, even at strict against
// a validator that would refuse everything.
func TestPayerAnswerFlows_ReferencePayersAreNotCertified(t *testing.T) {
	for _, flow := range answerFlows {
		for _, profile := range flow.profiles {
			for _, payer := range ReferencePayerIdentities() {
				for _, level := range everyLevel {
					t.Run(flow.name+"/"+profile+"/"+payer.Value+"/"+level.String(), func(t *testing.T) {
						v := &answerValidator{verdict: scriptStructural, leg: flow.leg, shapes: flow.shapes}
						var events []ObserverEvent
						status, body := flow.run(t, profile, payer, level, v, func(e ObserverEvent) { events = append(events, e) })
						if status != http.StatusOK {
							t.Fatalf("status %d %s, want the reference answer relayed", status, body)
						}
						if got := v.answerChecks(); len(got) != 0 {
							t.Fatalf("the reference payer's answer was checked %d times", len(got))
						}
						if f := findingsFor(events, flow.leg); len(f) != 0 {
							t.Fatalf("findings %+v for a reference answer", f)
						}
					})
				}
			}
		}
	}
}
