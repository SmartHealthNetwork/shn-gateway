//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A decisive line whose lane is configured but has not qualified yet
// cannot be judged, and another line's valid does not stand in for it: the
// check is unavailable, recorded at observe and structural and refused at
// strict. Before, the answer read valid at 2.2 while its routed 2.1 lane
// was down, which made the check more permissive exactly while its checker
// was missing. A routed line with no lane configured at all keeps reading the
// first valid line (the neighbour row), and once the lane qualifies the
// answer is judged at its own line.

// qualifyingIngressGateway is payerIngressGateway with contract's lane at line
// replaced by a default lane that has not qualified; its validator answers
// verdict once the lane does.
func qualifyingIngressGateway(t *testing.T, level ConformanceEnforcement, contract, line string, verdict scriptedVerdict, others map[string]scriptedVerdict) (*Gateway, *laneLog, *[]ConformanceFinding, *DiscoveredLane) {
	t.Helper()
	g, log, findings := payerIngressGateway(t, "provider-data", level, others)
	lane := NewDiscoveredLane(line, DefaultLaneURL(line), &scriptedLane{line: line, verdict: verdict, log: log})
	g.cfg.ValidatorsByContractLine[contract+"@"+line] = lane
	return g, log, findings, lane
}

var qualifyingLegs = []struct{ legType, contract string }{{"pas-claim", "pa.pas"}, {"dtr-questionnaire-fetch", "pa.dtr"}}

func TestPayerAnswerIngress_QualifyingDecisiveLineIsUnavailable(t *testing.T) {
	for _, leg := range qualifyingLegs {
		for _, tc := range []struct {
			level  ConformanceEnforcement
			status int
		}{{EnforcementObserve, 0}, {EnforcementStructural, 0}, {EnforcementStrict, http.StatusInternalServerError}} {
			g, log, findings, _ := qualifyingIngressGateway(t, tc.level, leg.contract, "2.1", scriptValid, map[string]scriptedVerdict{"2.2": scriptValid, "2.0": scriptStructural})
			status, msg := drainedPayerIngress(g, answerCtx(leg.legType), []byte(answerBundle), "2.1", leg.contract, partner)
			if status != tc.status {
				t.Errorf("%s/%s: %d %q, want %d", leg.legType, tc.level, status, msg, tc.status)
			}
			// 2.2 reads valid, and with the routed 2.1 still qualifying the
			// check is unavailable whatever 2.0 says: no further line is called.
			if got := log.lines(); !reflect.DeepEqual(got, []string{"2.2"}) {
				t.Errorf("%s/%s: lines tried %v, want 2.2 alone: the qualifying 2.1 lane never called, and 2.0 not once the veto is certain", leg.legType, tc.level, got)
			}
			if tc.status != 0 {
				if msg != "validator unavailable" {
					t.Errorf("%s/%s: %q, want validator unavailable", leg.legType, tc.level, msg)
				}
				continue
			}
			want := []LineVerdictSummary{{Line: "2.1", Verdict: "unavailable"}, {Line: "2.2", Verdict: "valid"}}
			if len(*findings) != 1 || (*findings)[0].Verdict != "unavailable" || (*findings)[0].Decision != "relayed" ||
				(*findings)[0].Line != "2.1" || !reflect.DeepEqual((*findings)[0].Lines, want) {
				t.Errorf("%s/%s: findings %+v, want one unavailable at 2.1 listing %+v", leg.legType, tc.level, *findings, want)
			}
		}
	}
}

// The neighbour row: with NO lane configured for the routed line, the first
// valid line still decides, at every level, as before.
func TestPayerAnswerIngress_RoutedLineWithNoLaneStillReadsValidElsewhere(t *testing.T) {
	for _, leg := range qualifyingLegs {
		for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			g, _, findings := payerIngressGateway(t, "provider-data", level, map[string]scriptedVerdict{"2.2": scriptValid, "2.0": scriptStructural})
			if status, msg := drainedPayerIngress(g, answerCtx(leg.legType), []byte(answerBundle), "2.1", leg.contract, partner); status != 0 {
				t.Errorf("%s/%s: %d %q, want relayed", leg.legType, level, status, msg)
			}
			if len(*findings) != 1 || (*findings)[0].Verdict != "valid" || (*findings)[0].Line != "2.2" {
				t.Errorf("%s/%s: findings %+v, want the answer recorded valid at 2.2", leg.legType, level, *findings)
			}
		}
	}
}

// Once the lane qualifies, the same answer is judged at its own line, with no
// restart: its 2.1 lane is called, and its verdict there decides as any
// line's does (valid relayed; structural, with 2.2 structural too, refused).
func TestPayerAnswerIngress_QualifiedLaneJudgesItsLine(t *testing.T) {
	for _, leg := range qualifyingLegs {
		for _, tc := range []struct {
			verdict scriptedVerdict
			status  int
		}{{scriptValid, 0}, {scriptStructural, http.StatusUnprocessableEntity}} {
			g, log, _, lane := qualifyingIngressGateway(t, EnforcementStrict, leg.contract, "2.1", tc.verdict, map[string]scriptedVerdict{"2.2": scriptStructural})
			if err := lane.Qualify(context.Background(), func(context.Context, string, string) error { return errors.New("down") }); err == nil {
				t.Fatal("a failing qualifier passed")
			}
			if status, _ := drainedPayerIngress(g, answerCtx(leg.legType), []byte(answerBundle), "2.1", leg.contract, partner); status != http.StatusInternalServerError {
				t.Fatalf("%s: after a failed qualification: %d, want 500", leg.legType, status)
			}
			if err := lane.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			before := log.count()
			status, msg := drainedPayerIngress(g, answerCtx(leg.legType), []byte(answerBundle), "2.1", leg.contract, partner)
			if status != tc.status {
				t.Errorf("%s: qualified 2.1 answering %v: %d %q, want %d", leg.legType, tc.verdict, status, msg, tc.status)
			}
			if got := log.lines()[before:]; len(got) == 0 || got[0] != "2.1" {
				t.Errorf("%s: lines tried %v, want 2.1 first once it qualified", leg.legType, got)
			}
		}
	}
}

// A qualifying line the answer is neither routed at nor claims is listed as
// unavailable rather than dropped, and decides nothing: the answer is decided
// on the lines that answered.
func TestPayerAnswerIngress_QualifyingExtraLineIsListedNotDecisive(t *testing.T) {
	for _, leg := range qualifyingLegs {
		g, _, findings, _ := qualifyingIngressGateway(t, EnforcementStrict, leg.contract, "2.1", scriptValid, map[string]scriptedVerdict{"2.2": scriptStructural, "2.0": scriptStructural})
		status, msg := drainedPayerIngress(g, answerCtx(leg.legType), []byte(answerBundle), "2.2", leg.contract, partner)
		if status != http.StatusUnprocessableEntity || !strings.HasPrefix(msg, "ingress validation failed") {
			t.Errorf("%s: %d %q, want refused as structural", leg.legType, status, msg)
		}
		var lines []string
		for _, f := range *findings {
			for _, l := range f.Lines {
				lines = append(lines, l.Line+"="+l.Verdict)
			}
		}
		if !strings.Contains(strings.Join(lines, ","), "2.1=unavailable") {
			t.Errorf("%s: lines %v, want the qualifying 2.1 listed as unavailable", leg.legType, lines)
		}
	}
}

// A line the answer CLAIMS is decisive too: an answer routed at 2.2, valid
// there, that claims 2.1 while 2.1's lane is still qualifying is unavailable at
// every level that checks, refused at strict. Passing at its routed line does
// not certify the line it says it is at, which nothing has judged yet.
func TestPayerAnswerIngress_QualifyingClaimedLineIsUnavailable(t *testing.T) {
	d21, _ := shnsdk.DTRLineDef("2.1")
	claimed21 := `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"q","meta":{"profile":["` +
		certificationDTR + `dtr-std-questionnaire|` + d21.PackageVersion + `"]}}}]}`
	for _, leg := range qualifyingLegs {
		for _, tc := range []struct {
			level  ConformanceEnforcement
			status int
		}{{EnforcementObserve, 0}, {EnforcementStructural, 0}, {EnforcementStrict, http.StatusInternalServerError}} {
			g, log, findings, _ := qualifyingIngressGateway(t, tc.level, leg.contract, "2.1", scriptValid, map[string]scriptedVerdict{"2.2": scriptValid, "2.0": scriptStructural})
			status, msg := drainedPayerIngress(g, answerCtx(leg.legType), []byte(claimed21), "2.2", leg.contract, partner)
			if status != tc.status {
				t.Errorf("%s/%s: %d %q, want %d", leg.legType, tc.level, status, msg, tc.status)
			}
			for _, l := range log.lines() {
				if l == "2.1" {
					t.Errorf("%s/%s: the qualifying 2.1 lane was called", leg.legType, tc.level)
				}
			}
			if tc.status != 0 {
				if msg != "validator unavailable" {
					t.Errorf("%s/%s: %q, want validator unavailable", leg.legType, tc.level, msg)
				}
				continue
			}
			if len(*findings) != 1 || (*findings)[0].Verdict != "unavailable" || (*findings)[0].Line != "2.1" {
				t.Errorf("%s/%s: findings %+v, want one unavailable at the claimed 2.1", leg.legType, tc.level, *findings)
			}
		}
	}
}

// With the routed line's lane still qualifying and no other lane, the check
// says the validator is unavailable, not that no lane is configured.
func TestPayerAnswerIngress_OnlyAQualifyingLaneSaysUnavailable(t *testing.T) {
	for _, leg := range qualifyingLegs {
		g, _, _, _ := qualifyingIngressGateway(t, EnforcementStrict, leg.contract, "2.1", scriptValid, nil)
		status, msg := drainedPayerIngress(g, answerCtx(leg.legType), []byte(answerBundle), "2.1", leg.contract, partner)
		if status != http.StatusInternalServerError || msg != "validator unavailable" {
			t.Errorf("%s: %d %q, want 500 validator unavailable", leg.legType, status, msg)
		}
	}
}

// A configured default lane is, in one read, either qualifying with no
// validator or ready with one; never "no lane" (a nil validator that is not
// qualifying), which would let another line's valid decide.
func TestCandidateLaneAtReadsTheLaneOnce(t *testing.T) {
	g, _, _, lane := qualifyingIngressGateway(t, EnforcementStrict, "pa.pas", "2.1", scriptValid, map[string]scriptedVerdict{"2.2": scriptValid})
	if v, qualifying := g.candidateLaneAt("pa.pas", "2.1"); v != nil || !qualifying {
		t.Fatalf("unqualified lane: v=%v qualifying=%v, want none and qualifying", v, qualifying)
	}
	if err := lane.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if v, qualifying := g.candidateLaneAt("pa.pas", "2.1"); v == nil || qualifying {
		t.Fatalf("qualified lane: v=%v qualifying=%v, want its validator", v, qualifying)
	}
	// A line with no lane configured is no lane, not qualifying.
	if v, qualifying := g.candidateLaneAt("pa.pas", "2.0"); v != nil || qualifying {
		t.Fatalf("unconfigured line: v=%v qualifying=%v", v, qualifying)
	}
}
