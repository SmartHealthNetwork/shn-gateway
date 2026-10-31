//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// CONFORMANCE_TOLERATE_PEERS is payer identities as <system>|<value>,
// comma-separated; empty tolerates no one, and an entry without both parts, or
// listed twice, is refused.
func TestParseToleratePeers(t *testing.T) {
	sys := shnsdk.CMSPayerIdentity.System
	got, err := ParseToleratePeers(" " + sys + "|00001 , " + sys + "|00300,urn:x|SHN-BRIDGE-DEMO ")
	want := []shnsdk.PayerIdentifier{{System: sys, Value: "00001"}, {System: sys, Value: "00300"}, {System: "urn:x", Value: "SHN-BRIDGE-DEMO"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v %v", got, err)
	}
	for _, empty := range []string{"", "  "} {
		if got, err := ParseToleratePeers(empty); err != nil || got != nil {
			t.Fatalf("%q: %v %v, want none", empty, got, err)
		}
	}
	for _, bad := range []string{"00001", sys + "|", "|00001", sys + "|00001,", sys + "|00001|x", sys + "|00001," + sys + "|00001"} {
		if _, err := ParseToleratePeers(bad); err == nil || !strings.Contains(err.Error(), "CONFORMANCE_TOLERATE_PEERS") {
			t.Errorf("%q: %v, want a refusal naming the setting", bad, err)
		}
	}
}

// The carried answers tolerate the payers the participant lists, matched against
// the identity the exchange was routed by (the coverage here names 00001): a
// defect strict would refuse is recorded and carried, its finding marked
// tolerated; unlisted, the same answer is refused.
func TestCarriedAnswersTolerateListedPayers(t *testing.T) {
	tolerate := func(g *Gateway) { g.cfg.ConformanceToleratePeers = ReferencePayerIdentities() }
	t.Run("DTR", func(t *testing.T) {
		answer := e08Answer("PackageBundle", nil, true)
		strict := dtrAnswerCheckLevel{name: "strict", level: EnforcementStrict}
		r, fhir := dtrAnswerCheckRow(t, strict, declares22, answer, true, "")
		r.wantRefused(t, answer)
		if len(fhir) != 1 || fhir[0].Tolerated {
			t.Fatalf("unlisted: findings %+v", fhir)
		}
		r = e08RowTuned(t, EnforcementStrict, declares22, answer, nil, nil, nil, tolerate)
		r.wantAnswered(t, answer, false)
		fhir = nil
		for _, f := range r.findings {
			if f.LegType == dtrLeg && f.Profile == dtrOutputParametersProfile {
				fhir = append(fhir, f)
			}
		}
		if len(fhir) != 1 || !fhir[0].Tolerated || fhir[0].Decision != Record.String() {
			t.Fatalf("listed: want one recorded, tolerated finding, got %+v", fhir)
		}
	})
	for _, leg := range carriedAnswerLegs() {
		t.Run(leg.leg, func(t *testing.T) {
			v := &shnsdk.FakeValidator{RejectIfContains: `"ClaimResponse"`}
			_, rec, findings, _, _ := carriedAnswerRun(t, leg, EnforcementStrict, "demo", v)
			if rec.Code != http.StatusBadGateway || len(findings) != 1 || findings[0].Tolerated {
				t.Fatalf("unlisted: %d %s %+v", rec.Code, rec.Body, findings)
			}
			_, rec, findings, _, _ = carriedAnswerRun(t, leg, EnforcementStrict, "demo", v, tolerate)
			if rec.Code != http.StatusOK || len(findings) != 1 || !findings[0].Tolerated || findings[0].Decision != Record.String() {
				t.Fatalf("listed: %d %s %+v, want carried with a tolerated finding", rec.Code, rec.Body, findings)
			}
		})
	}
}

// Tolerance is module 3's alone: a refusal of SHN's own edit is a network rule
// and refuses a listed payer's answer at every level. E-08's carried-across
// answer that is not valid at the provider's line is refused as ever. Module 2's
// check never receives the tolerant module today; this row guards that it never
// does.
func TestToleranceNeverReachesANetworkRule(t *testing.T) {
	answer := e08Answer("PackageBundle", []string{"Questionnaire"}, false)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			r := e08RowTuned(t, level, declares22, answer, nil, nil, nil, func(g *Gateway) { g.cfg.ConformanceToleratePeers = ReferencePayerIdentities() })
			if r.rec.Code == http.StatusOK || !strings.Contains(r.rec.Body.String(), "E-08") {
				t.Fatalf("a listed payer's answer carried past SHN's own edit check: %d %s", r.rec.Code, r.rec.Body.String())
			}
			for _, f := range r.findings {
				if f.Tolerated {
					t.Fatalf("a network rule's finding marked tolerated: %+v", f)
				}
			}
		})
	}
}

// An exchange routed by several payer identities (a DTR request whose coverages
// name more than one, routed to one holder) is tolerated only when every one is
// listed; none, or an empty list, tolerates no one.
func TestToleratesEveryRoutedPayerOrNone(t *testing.T) {
	a := shnsdk.PayerIdentifier{System: "urn:x", Value: "a"}
	b := shnsdk.PayerIdentifier{System: "urn:x", Value: "b"}
	g := &Gateway{cfg: Config{ConformanceToleratePeers: []shnsdk.PayerIdentifier{a}}}
	for _, c := range []struct {
		payers []shnsdk.PayerIdentifier
		want   bool
	}{
		{[]shnsdk.PayerIdentifier{a}, true},
		{[]shnsdk.PayerIdentifier{a, b}, false},
		{[]shnsdk.PayerIdentifier{b}, false},
		{nil, false},
	} {
		if got := g.toleratesAll(c.payers); got != c.want {
			t.Errorf("tolerates %v = %v, want %v", c.payers, got, c.want)
		}
	}
	if (&Gateway{}).toleratesAll([]shnsdk.PayerIdentifier{a}) {
		t.Error("an empty list tolerated a payer")
	}
}

// The marker says tolerance changed the decision, never more: a listed payer's
// answer with only a deeper defect is recorded at structural by the level itself,
// so its finding is not marked tolerated.
func TestToleratedMarksOnlyAChangedDecision(t *testing.T) {
	deeper := map[string]scriptedVerdict{"2.0": scriptDeeper, "2.1": scriptDeeper, "2.2": scriptDeeper}
	g, _, findings := payerIngressGateway(t, "provider-data", EnforcementStructural, deeper)
	g.cfg.ConformanceToleratePeers = []shnsdk.PayerIdentifier{partner}
	if status, msg := drainedPayerIngress(g, answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", partner); status != 0 {
		t.Fatalf("status %d %q", status, msg)
	}
	if len(*findings) != 1 || (*findings)[0].Decision != Record.String() || (*findings)[0].Tolerated {
		t.Fatalf("findings %+v, want one record not marked tolerated", *findings)
	}
}

// With no lane for any candidate line the answer is judged at the routed line
// alone, by the same module 3: a listed payer's defect is recorded there too.
func TestToleranceReachesTheNoLaneFallback(t *testing.T) {
	for _, listed := range []bool{false, true} {
		// A lane for another contract only: no pa.pas line is laned, so the routed
		// line ("", the gateway's own validator) judges it alone.
		g := &Gateway{cfg: Config{Validator: &recordingValidator{valid: false},
			ValidatorsByContractLine: map[string]shnsdk.Validator{"pa.crd@2.0": &recordingValidator{valid: true}}}}
		if listed {
			g.cfg.ConformanceToleratePeers = []shnsdk.PayerIdentifier{partner}
		}
		status, msg := g.validateFHIRPayerIngress(context.Background(), []byte(`{"resourceType":"Bundle"}`), "", "pa.pas", partner)
		if listed && status != 0 || !listed && status != http.StatusUnprocessableEntity {
			t.Fatalf("listed=%v: status %d %q", listed, status, msg)
		}
	}
}

// A DTR request whose coverages name two payer identities, both routed to one
// holder, has its answer tolerated only when the participant lists both: listing
// one leaves the other payer's answer judged and refused at strict.
func TestCarriedDTRAnswerToleratesOnlyWhenEveryRoutedPayerIsListed(t *testing.T) {
	sys := shnsdk.CMSPayerIdentity.System
	p1 := shnsdk.PayerIdentifier{System: sys, Value: "00001"}
	p2 := shnsdk.PayerIdentifier{System: sys, Value: "00300"}
	body := dtrFixture(t, "dtr-package-params-2.0.json")
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	params := req["parameter"].([]any)
	second, err := json.Marshal(params[0])
	if err != nil {
		t.Fatal(err)
	}
	second = bytes.ReplaceAll(bytes.ReplaceAll(second, []byte(`"coverage-1"`), []byte(`"coverage-2"`)), []byte(`"00001"`), []byte(`"00300"`))
	var cov any
	if err := json.Unmarshal(second, &cov); err != nil {
		t.Fatal(err)
	}
	req["parameter"] = append([]any{params[0], cov}, params[1:]...)
	twoCoverages, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	answer := e08Answer("PackageBundle", nil, true)
	for _, c := range []struct {
		name   string
		listed []shnsdk.PayerIdentifier
		carry  bool
	}{
		{"one listed", []shnsdk.PayerIdentifier{p1}, false},
		{"the other listed", []shnsdk.PayerIdentifier{p2}, false},
		{"both listed", []shnsdk.PayerIdentifier{p1, p2}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := e08RowRequest(t, EnforcementStrict, declares22, answer, nil, nil, nil, func(g *Gateway) {
				router, err := NewConfigPayerRouter([]PayerDirectoryEntry{
					{System: p1.System, Value: p1.Value, HolderID: "payer"},
					{System: p2.System, Value: p2.Value, HolderID: "payer"},
				})
				if err != nil {
					t.Fatal(err)
				}
				g.cfg.PayerRouter = router
				g.cfg.ConformanceToleratePeers = c.listed
			}, twoCoverages)
			if c.carry {
				r.wantAnswered(t, answer, false)
				return
			}
			r.wantRefused(t, answer)
		})
	}
}

// regexAnswerValidator answers every ClaimResponse-bearing body with res and every
// other body as valid.
type regexAnswerValidator struct{ res shnsdk.Result }

func (a regexAnswerValidator) Validate(_ context.Context, body []byte, _ string) (shnsdk.Result, error) {
	if bytes.Contains(body, []byte(`"ClaimResponse"`)) {
		return a.res, nil
	}
	return shnsdk.Result{Valid: true}, nil
}

// A payer the participant tolerates: at strict, a defect in its answer is
// recorded and carried, but a check that could not run is never tolerated.
// The timeout alone refuses the answer; beside a structural error the answer
// is invalid for that error, which tolerance carries as before.
func TestToleranceNeverCoversARegexCheckThatCouldNotRun(t *testing.T) {
	tolerate := func(g *Gateway) { g.cfg.ConformanceToleratePeers = ReferencePayerIdentities() }
	alone := regexAnswerValidator{invalid(regexTimeoutIssue("ClaimResponse.extension[0].url"))}
	mixed := regexAnswerValidator{invalid(regexTimeoutIssue("ClaimResponse.extension[0].url"), errIssue("Validation_VAL_Profile_Minimum", []string{"ClaimResponse"}, "x"))}
	for _, leg := range carriedAnswerLegs() {
		t.Run(leg.leg, func(t *testing.T) {
			_, rec, findings, _, _ := carriedAnswerRun(t, leg, EnforcementStrict, "demo", alone, tolerate)
			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "validator unavailable") {
				t.Fatalf("the timeout alone: %d %s, want the gateway's own 500", rec.Code, rec.Body)
			}
			for _, f := range findings {
				if f.Tolerated {
					t.Fatalf("a check that could not run was tolerated: %+v", f)
				}
			}
			_, rec, findings, _, _ = carriedAnswerRun(t, leg, EnforcementStrict, "demo", mixed, tolerate)
			if rec.Code != http.StatusOK || len(findings) != 1 || !findings[0].Tolerated || findings[0].Decision != Record.String() || findings[0].Verdict != "" {
				t.Fatalf("beside a structural error: %d %s %+v, want carried with a tolerated invalid finding", rec.Code, rec.Body, findings)
			}
		})
	}
}
