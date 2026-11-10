//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// module1_dtr_as_built_test.go: a DTR request module 1 builds at the payer's line is
// sent as built, so no translation is recorded for it (carryUnchanged,
// nextQuestionLeg), and the DTR exchange's checks and composition run at that
// line, never the build line.

// A questionnaire request module 2 routes by a chain (this gateway reaches the
// payer's 2.1 only by walking from its 2.0) is built at the payer's line, the
// line selected for it, and sent as built: nothing translates it, so nothing
// records a translation. At structural and strict, as at every level, the
// questionnaire leg goes at the payer's line with no chain, and no
// leg.transformed (and so no transform Provenance) is recorded for it.
func TestDTRRequestBuiltAtThePayersLineRecordsNoTranslation(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			fake := shnsdk.NewFakeValidator()
			gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
			var events []ObserverEvent
			gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			gw.cfg.ConformanceEnforcement = level
			gw.cfg.DeclaredContractVersions = []string{shnsdk.ContractPACRD20, shnsdk.ContractPADTR20, shnsdk.ContractPAPAS20}
			gw.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.1": fake})
			gw.cfg.EgressNativeLines = []string{"2.0"}
			gw.cfg.TranslateIGLines = true
			entry, ok := gw.cfg.Reg.Lookup("payer")
			if !ok {
				t.Fatal("payer not registered")
			}
			entry.ContractVersions = []string{shnsdk.ContractPACRD20, "pa.dtr@2.1", shnsdk.ContractPAPAS20}
			gw.cfg.Reg.Set("payer", entry)
			if route, err := gw.selectLegLine("payer", "dtr-questionnaire-fetch", "corr-probe", relay.BuilderSDKDTRPackage); err != nil || len(route.Chain) == 0 {
				t.Fatalf("the fixture routes the questionnaire leg %+v %v: want a chain, the case under test", route, err)
			}
			rec := callRunCRDThenDTROrder(t, gw, stub)
			var sent []ObserverEvent
			for _, e := range events {
				if e.LegType != "dtr-questionnaire-fetch" {
					continue
				}
				switch e.Kind {
				case "leg.originated":
					sent = append(sent, e)
				case "leg.transformed":
					t.Fatalf("a translation was recorded for a request built at the payer's line: %+v", e)
				}
			}
			if len(sent) != 1 || sent[0].Route == nil || sent[0].Route.Token != "pa.dtr@2.1" || sent[0].Route.BuildLine != "2.1" || len(sent[0].Route.Chain) != 0 {
				t.Fatalf("questionnaire requests %+v (answer %d %s), want one at the payer's line pa.dtr@2.1, built at 2.1, with no chain", sent, rec.Code, rec.Body)
			}
		})
	}
}

// The $next-question round answers the package fetched at the payer's line
// and is built there: on a route module 2 reaches the payer's 2.1 by walking
// from this gateway's 2.0, it is sent as built, at 2.1 with no chain, and no
// translation is recorded for it.
func TestDTRNextQuestionBuiltAtThePayersLineRecordsNoTranslation(t *testing.T) {
	env := newInProcessExchange(t)
	g := env.originator
	var events []ObserverEvent
	g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	fake := shnsdk.NewFakeValidator()
	g.cfg.DeclaredContractVersions = []string{shnsdk.ContractPACRD20, shnsdk.ContractPADTR20, shnsdk.ContractPAPAS20}
	g.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.1": fake})
	g.cfg.EgressNativeLines = []string{"2.0"}
	g.cfg.TranslateIGLines = true
	entry, ok := g.cfg.Reg.Lookup(env.payerID)
	if !ok {
		t.Fatalf("harness payer %q not registered", env.payerID)
	}
	entry.RequestFrames = dtrOperationFrames
	entry.ContractVersions = []string{shnsdk.ContractPACRD20, "pa.dtr@2.1", shnsdk.ContractPAPAS20}
	g.cfg.Reg.Set(env.payerID, entry)
	if route, err := g.selectLegLine(env.payerID, "dtr-questionnaire-fetch", "corr-probe", relay.BuilderDTRNextQuestion); err != nil || len(route.Chain) == 0 || shnsdk.LineOf(route.Token) != "2.1" {
		t.Fatalf("the fixture routes the round %+v %v: want a chain to 2.1, the case under test", route, err)
	}

	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	res := crdDtrResult{recipient: env.payerID, dtrLine: "2.1", patientRef: "Patient/MBR-COVERED", pci: pci}
	answer, err := json.Marshal(map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{{
			"name": "questionnaire-response",
			"resource": map[string]any{
				"resourceType": "QuestionnaireResponse",
				"status":       "in-progress",
				"subject":      map[string]string{"reference": res.patientRef},
				"contained": []map[string]any{{
					"resourceType": "Questionnaire",
					"id":           "contained-questionnaire",
					"item":         []map[string]any{{"linkId": "2", "type": "group"}},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.payerReturns(LegResult{Response: testResponse(answer)})
	reqQR := json.RawMessage(`{"resourceType":"QuestionnaireResponse","status":"in-progress"}`)
	if _, status, msg, lerr := g.nextQuestionLeg(env.ctx, env.req, res, "http://example.org/Questionnaire/adaptive", reqQR); status != 0 {
		t.Fatalf("nextQuestionLeg refused: %d %s (%v)", status, msg, lerr)
	}
	var sent []ObserverEvent
	for _, e := range events {
		if e.LegType != "dtr-questionnaire-fetch" {
			continue
		}
		switch e.Kind {
		case "leg.originated":
			sent = append(sent, e)
		case "leg.transformed":
			t.Fatalf("a translation was recorded for a round built at the payer's line: %+v", e)
		}
	}
	if len(sent) != 1 || sent[0].Route == nil || sent[0].Route.Token != "pa.dtr@2.1" || sent[0].Route.BuildLine != "2.1" || len(sent[0].Route.Chain) != 0 {
		t.Fatalf("rounds sent %+v, want one at the payer's line pa.dtr@2.1, built at 2.1, with no chain", sent)
	}
}

// dtrCall is one call a DTR lane was asked: the resource's type, the profile
// asked for, and the payload.
type dtrCall struct {
	resourceType, profile string
	payload               []byte
}

// dtrLineLane is one DTR line's validator lane. It records every call, and
// refuses each with a structural issue unless pass says the call is one this
// line serves.
type dtrLineLane struct {
	pass  func(dtrCall) bool
	calls []dtrCall
}

func (l *dtrLineLane) Validate(_ context.Context, resourceJSON []byte, profile string) (shnsdk.Result, error) {
	var head struct {
		ResourceType string `json:"resourceType"`
	}
	_ = json.Unmarshal(resourceJSON, &head)
	c := dtrCall{resourceType: head.ResourceType, profile: profile, payload: resourceJSON}
	l.calls = append(l.calls, c)
	if l.pass(c) {
		return shnsdk.Result{Valid: true}, nil
	}
	return shnsdk.Result{Valid: false, Issues: []string{"not this line's"}, Details: []shnsdk.Issue{{
		Severity: "error", Code: "structure", MessageID: "Validation_VAL_Profile_Minimum",
		Expression: []string{"Parameters.parameter[0]"}, Diagnostics: "not this line's",
	}}}, nil
}

// A DTR exchange module 2 routes by a chain (this gateway's build line is 2.0;
// the payer's DTR line, 2.2, is reached only by walking) runs every check and
// composition at the line the exchange ran at, the payer's, not the build line,
// at both sites that originate it: order-select and order-dispatch. The 2.0 DTR
// lane refuses everything the DTR exchange could ask of it but the PAS
// exchange's own call, and each lane records what it was asked. The response's
// egress check at the build line refuses the exchange at structural and
// strict; the fill is caught by the line it was composed at; the payer's
// answer (read at 2.2 after 2.0 refuses it, as it is read at its candidate
// lines) and the response's context completion (asked at the 2.0 DTR profile,
// which the 2.0 lane serves for the PAS exchange) are caught by what each lane
// was asked. The PAS exchange's own call is the response the PAS submission
// (and an update) carries, built at the PAS line, 2.0, and checked there.
func TestDTRExchangeChecksAtThePayersLineNotTheBuildLine(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementStructural, EnforcementStrict} {
		t.Run("order-select/"+level.String(), func(t *testing.T) {
			gw, _ := newPendResumeFixture(t, pendFixtureOpts{
				member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen",
				pendedItem: "operative-diagnostic-report",
			})
			lanes := routeDTRByChainTo22(t, gw, level)
			rec := httptest.NewRecorder()
			gw.handleUC04(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc04", strings.NewReader(testRouteBody(gw, "/scenario/uc04", ""))))
			lanes.assertAtThePayersLine(t, rec)
		})
		t.Run("order-dispatch/"+level.String(), func(t *testing.T) {
			_, _, gw := homeOxygenRig(t, fakePopulator{canonical: "http://smarthealth.network/fhir/Questionnaire/home-oxygen"}, shnsdk.NewFakeValidator())
			lanes := routeDTRByChainTo22(t, gw, level)
			rec := httptest.NewRecorder()
			gw.handleHomeOxygen(rec, httptest.NewRequest(http.MethodPost, "/scenario/homeoxygen", strings.NewReader(testRouteBody(gw, "/scenario/homeoxygen", ""))))
			lanes.assertAtThePayersLine(t, rec)
		})
	}
}

// chainedDTRLanes is a gateway whose DTR exchange module 2 routes from its
// build line, 2.0, to the payer's, 2.2, with each line's DTR lane and the fill
// recorded.
type chainedDTRLanes struct {
	buildLine, payersLine *dtrLineLane
	fill                  *linePopulator
	qr20, qr22            string
}

func routeDTRByChainTo22(t *testing.T, gw *Gateway, level ConformanceEnforcement) chainedDTRLanes {
	t.Helper()
	qr20, err := dtrQRProfile("2.0")
	if err != nil {
		t.Fatal(err)
	}
	qr22, err := dtrQRProfile("2.2")
	if err != nil {
		t.Fatal(err)
	}
	c := chainedDTRLanes{
		buildLine: &dtrLineLane{pass: func(call dtrCall) bool {
			return call.resourceType == "QuestionnaireResponse" && call.profile == qr20
		}},
		payersLine: &dtrLineLane{pass: func(dtrCall) bool { return true }},
		qr20:       qr20, qr22: qr22,
	}
	gw.cfg.ConformanceEnforcement = level
	gw.cfg.DeclaredContractVersions = []string{shnsdk.ContractPACRD20, shnsdk.ContractPADTR20, shnsdk.ContractPAPAS20}
	fake := shnsdk.NewFakeValidator()
	lanes := contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.2": fake})
	lanes["pa.dtr@2.0"] = c.buildLine
	lanes["pa.dtr@2.2"] = c.payersLine
	gw.cfg.ValidatorsByContractLine = lanes
	gw.cfg.EgressNativeLines = []string{"2.0"}
	gw.cfg.TranslateIGLines = true
	entry, ok := gw.cfg.Reg.Lookup("payer")
	if !ok {
		t.Fatal("payer not registered")
	}
	entry.ContractVersions = []string{shnsdk.ContractPACRD20, "pa.dtr@2.2", shnsdk.ContractPAPAS20}
	gw.cfg.Reg.Set("payer", entry)
	if route, err := gw.selectLegLine("payer", "dtr-questionnaire-fetch", "corr-probe", relay.BuilderSDKDTRPackage); err != nil || len(route.Chain) == 0 || shnsdk.LineOf(route.Token) != "2.2" {
		t.Fatalf("the fixture routes the questionnaire leg %+v %v: want a chain to 2.2, the case under test", route, err)
	}
	if !ownEngine(gw.populator()) {
		t.Fatal("the fixture's fill is not the gateway's own engine; linePopulator keeps that mark")
	}
	c.fill = &linePopulator{inner: gw.populator()}
	gw.paState().populator = c.fill
	return c
}

func (c chainedDTRLanes) assertAtThePayersLine(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("want the exchange completed, got %d body=%s (build line asked %s; payer's line asked %s)", rec.Code, rec.Body.String(), describeDTRCalls(c.buildLine.calls), describeDTRCalls(c.payersLine.calls))
	}
	// At the payer's line: its answer, the response's own check, and the
	// response checked at that line's DTR profile, declaring it and built at
	// that line.
	got := c.payersLine.calls
	if len(got) != 3 || got[0].resourceType != "Bundle" ||
		got[1].resourceType != "QuestionnaireResponse" || got[1].profile != baseQRProfile ||
		got[2].resourceType != "QuestionnaireResponse" || got[2].profile != c.qr22 {
		t.Fatalf("payer's line asked %s, want the package answer, the response's check and the response at %s", describeDTRCalls(got), c.qr22)
	}
	if !strings.Contains(string(got[2].payload), c.qr22) {
		t.Fatalf("the response checked at the payer's line does not declare %s:\n%s", c.qr22, got[2].payload)
	}
	assert22WireMarkers(t, "the response checked at the payer's line", got[2].payload)
	// The response is composed at the payer's line.
	if len(c.fill.lines) != 1 || c.fill.lines[0] != "2.2" {
		t.Fatalf("the response was composed at %q, want once at the payer's line 2.2", c.fill.lines)
	}
	// At the build line: only the response the PAS submission (and an update)
	// carries, at the PAS exchange's line.
	if len(c.buildLine.calls) == 0 {
		t.Fatal("the build line's lane was never asked: the PAS submission's response is checked there, so the row did not reach it")
	}
	for _, call := range c.buildLine.calls {
		if call.resourceType != "QuestionnaireResponse" || call.profile != c.qr20 {
			t.Fatalf("build line asked %s, want only the PAS submission's response at %s", describeDTRCalls(c.buildLine.calls), c.qr20)
		}
	}
}

// linePopulator records the line each fill is composed at and the response
// it returns, and fills by the gateway's own engine it wraps (it keeps that
// engine's mark, gatewayEngine).
type linePopulator struct {
	inner Populator
	lines []string
	out   [][]byte
}

func (p *linePopulator) Populate(ctx context.Context, packageJSON []byte, pc PopulateContext) ([]byte, []FilledItem, error) {
	p.lines = append(p.lines, pc.Line)
	qr, fill, err := p.inner.Populate(ctx, packageJSON, pc)
	p.out = append(p.out, qr)
	return qr, fill, err
}

func (*linePopulator) gatewayEngine() {}

func describeDTRCalls(calls []dtrCall) string {
	var parts []string
	for _, c := range calls {
		parts = append(parts, c.resourceType+"@"+c.profile)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
