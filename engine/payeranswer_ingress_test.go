package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The ingress check of a payer's answer on a leg a provider gateway originated
// (validateFHIRPayerIngress): skipped only for the network's reference payer
// identities on a lane that relays their bytes; for every other payer, governed at
// the gateway's level and certified against the lines this gateway supports, the
// routed line first, refused only when it conforms to none.

// scriptedVerdict is what a scripted lane answers about a payer's answer.
type scriptedVerdict int

const (
	scriptValid scriptedVerdict = iota
	scriptStructural
	scriptDeeper
	scriptOutage     // the validator call fails
	scriptNoResource // the validator answered without reading the payload
	scriptHang       // the validator answers only when its context ends
)

// laneCall is one call a scripted lane received.
type laneCall struct {
	Line string
	Body []byte
}

// laneLog is the calls every lane of one gateway received, in order.
type laneLog struct {
	mu    sync.Mutex
	calls []laneCall
}

func (l *laneLog) add(c laneCall) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, c)
}

func (l *laneLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, c := range l.calls {
		out = append(out, c.Line)
	}
	return out
}

func (l *laneLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

// scriptedLane is one line's validator lane, answering every call as scripted.
type scriptedLane struct {
	line    string
	verdict scriptedVerdict
	log     *laneLog
}

func (s *scriptedLane) Validate(ctx context.Context, body []byte, _ string) (shnsdk.Result, error) {
	s.log.add(laneCall{Line: s.line, Body: append([]byte(nil), body...)})
	if s.verdict == scriptHang {
		<-ctx.Done()
		return shnsdk.Result{}, ctx.Err()
	}
	return scriptedResult(s.verdict)
}

func scriptedResult(v scriptedVerdict) (shnsdk.Result, error) {
	switch v {
	case scriptStructural:
		return invalid(errIssue("Validation_VAL_Profile_Minimum", []string{"Bundle"}, "RAW-DIAGNOSTIC Bundle.type: minimum required = 1")), nil
	case scriptDeeper:
		return invalid(errIssue("Terminology_TX_NoValid_1_CC", []string{"Bundle.entry[0].resource.code"}, "RAW-DIAGNOSTIC None of the codings provided")), nil
	case scriptOutage:
		return shnsdk.Result{}, errors.New("validator down")
	case scriptNoResource:
		return invalid(errIssue("", nil, hapi0992)), nil
	}
	return shnsdk.Result{Valid: true}, nil
}

// payerIngressGateway is a provider gateway on profile at level whose validator
// lanes are the scripted lines in verdicts (a line absent from verdicts has no lane).
func payerIngressGateway(t *testing.T, profile string, level ConformanceEnforcement, verdicts map[string]scriptedVerdict) (*Gateway, *laneLog, *[]ConformanceFinding) {
	t.Helper()
	log := &laneLog{}
	lanes := map[string]shnsdk.Validator{}
	for line, v := range verdicts {
		lanes[line] = &scriptedLane{line: line, verdict: v, log: log}
	}
	var findings []ConformanceFinding
	g := &Gateway{cfg: Config{
		Role:                   "provider",
		HolderID:               "provider",
		OriginationProfile:     profile,
		ConformanceEnforcement: level,
		Reg:                    shnsdk.NewRegistry(),
		Validator:              lanes["2.0"],
		ValidatorsByLine:       lanes,
		Clock:                  func() time.Time { return time.Unix(0, 0).UTC() },
		Observer: func(e ObserverEvent) {
			var f ConformanceFinding
			if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil {
				findings = append(findings, f)
			}
		},
	}}
	if len(lanes) == 0 {
		// No lane at all: a configured-but-empty lane map, so no line falls back
		// to a canonical validator.
		g.cfg.Validator = nil
		g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"9.9": nil}
	}
	return g, log, &findings
}

// answerCtx is the finding context an originating handler tags a payer's answer
// with.
func answerCtx(legType string) context.Context {
	return withFindingContext(context.Background(), findingContext{
		LegType: legType, CorrelationID: "corr-answer", Seam: "originate", Whose: "peer",
	})
}

const answerBundle = `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"q"}}]}`

var (
	providerLanes  = []string{"provider-data", "demo"}
	everyLevel     = []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict}
	allLinesFailed = map[string]scriptedVerdict{"2.0": scriptStructural, "2.1": scriptStructural, "2.2": scriptStructural}
	// partner is a payer identity outside the reference set.
	partner = shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "00002"}
)

// The network's reference payers, by identity, on either lane that relays their
// bytes, are not certified at any level: no validator call, no finding, the answer
// relayed — even at strict, and even when every lane would refuse it.
func TestPayerAnswerIngress_ReferencePayersAreNotCertified(t *testing.T) {
	for _, profile := range providerLanes {
		for _, payer := range ReferencePayerIdentities() {
			for _, level := range everyLevel {
				g, log, findings := payerIngressGateway(t, profile, level, allLinesFailed)
				for _, leg := range []struct{ legType, contract string }{{"dtr-questionnaire-fetch", "pa.dtr"}, {"pas-claim", "pa.pas"}, {"pas-claim-update", "pa.pas"}} {
					status, msg := g.validateFHIRPayerIngress(answerCtx(leg.legType), []byte(answerBundle), "2.0", leg.contract, payer)
					if status != 0 {
						t.Errorf("%s/%s/%s %s: status %d %q, want the reference answer relayed", profile, payer.Value, level, leg.legType, status, msg)
					}
				}
				if log.count() != 0 || len(*findings) != 0 {
					t.Errorf("%s/%s/%s: %d validator calls, %d findings; a reference payer's answer is never certified", profile, payer.Value, level, log.count(), len(*findings))
				}
			}
		}
	}
}

// The skip needs both halves: a reference payer reached from a lane that does not
// relay reference bytes is certified like any payer.
func TestPayerAnswerIngress_ReferencePayerOffTheReferenceLanesIsCertified(t *testing.T) {
	g, log, _ := payerIngressGateway(t, "unknown-lane", EnforcementStrict, allLinesFailed)
	status, _ := g.validateFHIRPayerIngress(answerCtx("dtr-questionnaire-fetch"), []byte(answerBundle), "2.0", "pa.dtr", shnsdk.CMSPayerIdentity)
	if status != http.StatusUnprocessableEntity || log.count() != 3 {
		t.Fatalf("status %d after %d calls, want 422 after every line failed", status, log.count())
	}
}

// A payer outside the reference set is governed at the gateway's level, on both
// lanes that relay reference bytes, whatever its identity resembles. One lane, so
// one line tried.
func TestPayerAnswerIngress_PartnerPayerLevelMatrix(t *testing.T) {
	type want struct {
		status   int
		msg      string
		calls    int
		decision string // "" = no finding
		verdict  string
		line     string // the one tried line's verdict
	}
	rows := []struct {
		name    string
		verdict scriptedVerdict
		want    map[ConformanceEnforcement]want
	}{
		{"structural defect", scriptStructural, map[ConformanceEnforcement]want{
			EnforcementNone:       {0, "", 0, "", "", ""},
			EnforcementObserve:    {0, "", 1, "relayed", "", "structural"},
			EnforcementStructural: {422, "ingress validation failed", 1, "refused", "", "structural"},
			EnforcementStrict:     {422, "ingress validation failed", 1, "refused", "", "structural"},
		}},
		{"deeper defect", scriptDeeper, map[ConformanceEnforcement]want{
			EnforcementNone:       {0, "", 0, "", "", ""},
			EnforcementObserve:    {0, "", 1, "relayed", "", "deeper"},
			EnforcementStructural: {0, "", 1, "relayed", "", "deeper"},
			EnforcementStrict:     {422, "ingress validation failed", 1, "refused", "", "deeper"},
		}},
		{"validator unavailable", scriptOutage, map[ConformanceEnforcement]want{
			EnforcementNone:       {0, "", 0, "", "", ""},
			EnforcementObserve:    {0, "", 1, "relayed", "unavailable", "unavailable"},
			EnforcementStructural: {0, "", 1, "relayed", "unavailable", "unavailable"},
			EnforcementStrict:     {500, "validator unavailable", 1, "", "", ""},
		}},
		{"validator answered without reading the payload", scriptNoResource, map[ConformanceEnforcement]want{
			EnforcementNone:       {0, "", 0, "", "", ""},
			EnforcementObserve:    {0, "", 1, "relayed", "unavailable", "unavailable"},
			EnforcementStructural: {0, "", 1, "relayed", "unavailable", "unavailable"},
			EnforcementStrict:     {500, "validator unavailable", 1, "", "", ""},
		}},
		{"valid answer", scriptValid, map[ConformanceEnforcement]want{
			EnforcementNone:       {0, "", 0, "", "", ""},
			EnforcementObserve:    {0, "", 1, "", "", ""},
			EnforcementStructural: {0, "", 1, "", "", ""},
			EnforcementStrict:     {0, "", 1, "", "", ""},
		}},
	}
	lookAlikes := []shnsdk.PayerIdentifier{
		partner,
		{System: "urn:oid:2.16.840.1.113883.6.301", Value: "00001"},
		{System: "", Value: "00300"},
		{System: shnsdk.CMSPayerIdentity.System, Value: "0001"},
	}
	for _, profile := range providerLanes {
		for _, payer := range lookAlikes {
			for _, row := range rows {
				for _, level := range everyLevel {
					w := row.want[level]
					g, log, findings := payerIngressGateway(t, profile, level, map[string]scriptedVerdict{"2.0": row.verdict})
					status, msg := g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", payer)
					name := profile + "/" + payer.System + "|" + payer.Value + "/" + row.name + "/" + level.String()
					if status != w.status || w.msg != "" && !strings.HasPrefix(msg, w.msg) {
						t.Errorf("%s: status %d %q, want %d %q", name, status, msg, w.status, w.msg)
					}
					if log.count() != w.calls {
						t.Errorf("%s: %d validator calls, want %d", name, log.count(), w.calls)
					}
					if w.decision == "" {
						if len(*findings) != 0 {
							t.Errorf("%s: findings %+v, want none", name, *findings)
						}
						continue
					}
					if len(*findings) != 1 {
						t.Errorf("%s: %d findings, want 1", name, len(*findings))
						continue
					}
					f := (*findings)[0]
					if f.Decision != w.decision || f.Verdict != w.verdict || f.Whose != "peer" || f.LegType != "pas-claim" ||
						f.CorrelationID != "corr-answer" || f.Seam != "originate" || f.Kind != string(KindFHIRIngress) || f.Line != "2.0" || f.Level != level.String() {
						t.Errorf("%s: finding %+v", name, f)
					}
					if f.DeclaredLine != "2.0" || !reflect.DeepEqual(f.Lines, []LineVerdictSummary{{"2.0", w.line}}) {
						t.Errorf("%s: finding names routed line %q and lines %+v", name, f.DeclaredLine, f.Lines)
					}
				}
			}
		}
	}
}

// A payer routed at a line its answer fails, but valid on another line this gateway
// supports, is relayed at every level: the declared line may be this gateway's
// default rather than the payer's claim. The finding names the routed line and both
// lines tried.
func TestPayerAnswerIngress_DeclaredLineFailedButValidOnAnother(t *testing.T) {
	for _, profile := range providerLanes {
		for _, level := range []ConformanceEnforcement{EnforcementStructural, EnforcementStrict} {
			verdicts := map[string]scriptedVerdict{"2.0": scriptStructural, "2.1": scriptValid, "2.2": scriptValid}
			g, log, findings := payerIngressGateway(t, profile, level, verdicts)
			status, msg := g.validateFHIRPayerIngress(answerCtx("dtr-questionnaire-fetch"), []byte(answerBundle), "2.0", "pa.dtr", partner)
			if status != 0 {
				t.Errorf("%s/%s: status %d %q, want relayed", profile, level, status, msg)
			}
			if got := log.lines(); !reflect.DeepEqual(got, []string{"2.0", "2.2"}) {
				t.Errorf("%s/%s: lines tried %v, want the routed 2.0 then 2.2", profile, level, got)
			}
			want := []LineVerdictSummary{{"2.0", "structural"}, {"2.2", "valid"}}
			if len(*findings) != 1 || (*findings)[0].DeclaredLine != "2.0" || (*findings)[0].Verdict != "valid" || !reflect.DeepEqual((*findings)[0].Lines, want) {
				t.Errorf("%s/%s: findings %+v, want one naming 2.0 with lines %+v", profile, level, *findings, want)
			}
		}
	}
}

// The shape a partner payer that declares 2.0 but answers at 2.1 takes: structural
// at its declared 2.0, valid at 2.1, relayed at structural and strict.
func TestPayerAnswerIngress_DeclaredTwoZeroAnswersTwoOne(t *testing.T) {
	for _, profile := range providerLanes {
		for _, level := range []ConformanceEnforcement{EnforcementStructural, EnforcementStrict} {
			g, _, findings := payerIngressGateway(t, profile, level, map[string]scriptedVerdict{"2.0": scriptStructural, "2.1": scriptValid})
			if status, msg := g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", partner); status != 0 {
				t.Errorf("%s/%s: status %d %q, want relayed", profile, level, status, msg)
			}
			want := []LineVerdictSummary{{"2.0", "structural"}, {"2.1", "valid"}}
			if len(*findings) != 1 || (*findings)[0].DeclaredLine != "2.0" || (*findings)[0].Line != "2.1" || !reflect.DeepEqual((*findings)[0].Lines, want) {
				t.Errorf("%s/%s: findings %+v, want lines %+v", profile, level, *findings, want)
			}
		}
	}
}

// A valid first line makes no further call and records nothing.
func TestPayerAnswerIngress_FirstValidLineEndsTheCheck(t *testing.T) {
	g, log, findings := payerIngressGateway(t, "demo", EnforcementStrict, map[string]scriptedVerdict{"2.2": scriptStructural, "2.1": scriptStructural, "2.0": scriptValid})
	if status, _ := g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", partner); status != 0 {
		t.Fatalf("status %d, want relayed", status)
	}
	if log.count() != 1 || len(*findings) != 0 {
		t.Fatalf("%d calls, %d findings: a valid routed line makes no further call and records nothing", log.count(), len(*findings))
	}
}

func TestPayerAnswerIngress_StructuralOnEveryLine(t *testing.T) {
	for _, profile := range providerLanes {
		for _, tc := range []struct {
			level    ConformanceEnforcement
			status   int
			decision string
		}{
			{EnforcementNone, 0, ""},
			{EnforcementObserve, 0, "relayed"},
			{EnforcementStructural, 422, "refused"},
			{EnforcementStrict, 422, "refused"},
		} {
			g, log, findings := payerIngressGateway(t, profile, tc.level, allLinesFailed)
			status, msg := g.validateFHIRPayerIngress(answerCtx("pas-claim-update"), []byte(answerBundle), "2.0", "pa.pas", partner)
			if status != tc.status || tc.status != 0 && !strings.HasPrefix(msg, "ingress validation failed") {
				t.Errorf("%s/%s: status %d %q, want %d", profile, tc.level, status, msg, tc.status)
			}
			if tc.decision == "" {
				if log.count() != 0 || len(*findings) != 0 {
					t.Errorf("%s/%s: none runs nothing, got %d calls %d findings", profile, tc.level, log.count(), len(*findings))
				}
				continue
			}
			if got := log.lines(); !reflect.DeepEqual(got, []string{"2.0", "2.2", "2.1"}) {
				t.Errorf("%s/%s: lines tried %v", profile, tc.level, got)
			}
			if len(*findings) != 1 {
				t.Errorf("%s/%s: %d findings, want one for the check", profile, tc.level, len(*findings))
				continue
			}
			f := (*findings)[0]
			want := []LineVerdictSummary{{"2.0", "structural"}, {"2.2", "structural"}, {"2.1", "structural"}}
			if f.Decision != tc.decision || f.DeclaredLine != "2.0" || !reflect.DeepEqual(f.Lines, want) {
				t.Errorf("%s/%s: finding %+v", profile, tc.level, f)
			}
		}
	}
}

// The best verdict across the lines that answered decides: deeper at 2.1 beats
// structural at 2.0 and 2.2, so structural records it and strict refuses it.
func TestPayerAnswerIngress_BestVerdictDecides(t *testing.T) {
	verdicts := map[string]scriptedVerdict{"2.2": scriptStructural, "2.1": scriptDeeper, "2.0": scriptStructural}
	for _, profile := range providerLanes {
		for _, tc := range []struct {
			level    ConformanceEnforcement
			status   int
			decision string
		}{
			{EnforcementObserve, 0, "relayed"},
			{EnforcementStructural, 0, "relayed"},
			{EnforcementStrict, 422, "refused"},
		} {
			g, _, findings := payerIngressGateway(t, profile, tc.level, verdicts)
			status, _ := g.validateFHIRPayerIngress(answerCtx("dtr-questionnaire-fetch"), []byte(answerBundle), "2.0", "pa.dtr", partner)
			if status != tc.status {
				t.Errorf("%s/%s: status %d, want %d", profile, tc.level, status, tc.status)
			}
			if len(*findings) != 1 || (*findings)[0].Decision != tc.decision || (*findings)[0].Line != "2.1" {
				t.Errorf("%s/%s: findings %+v, want one %s naming 2.1", profile, tc.level, *findings, tc.decision)
			}
		}
	}
}

// When no candidate line's validator answers, the check is unavailable: recorded
// below strict, refused at strict.
func TestPayerAnswerIngress_NoLaneAnswers(t *testing.T) {
	outage := map[string]scriptedVerdict{"2.2": scriptOutage, "2.1": scriptNoResource, "2.0": scriptOutage}
	for _, profile := range providerLanes {
		for _, tc := range []struct {
			level  ConformanceEnforcement
			status int
			msg    string
		}{
			{EnforcementObserve, 0, ""},
			{EnforcementStructural, 0, ""},
			{EnforcementStrict, 500, "validator unavailable"},
		} {
			g, log, findings := payerIngressGateway(t, profile, tc.level, outage)
			status, msg := g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", partner)
			if status != tc.status || msg != tc.msg {
				t.Errorf("%s/%s: %d %q, want %d %q", profile, tc.level, status, msg, tc.status, tc.msg)
			}
			if log.count() != 3 {
				t.Errorf("%s/%s: %d calls, want every candidate line tried", profile, tc.level, log.count())
			}
			if tc.status != 0 {
				if len(*findings) != 0 {
					t.Errorf("%s/%s: a refused unavailable check records nothing, got %+v", profile, tc.level, *findings)
				}
				continue
			}
			want := []LineVerdictSummary{{"2.0", "unavailable"}, {"2.2", "unavailable"}, {"2.1", "unavailable"}}
			if len(*findings) != 1 || (*findings)[0].Verdict != "unavailable" || !reflect.DeepEqual((*findings)[0].Lines, want) {
				t.Errorf("%s/%s: findings %+v", profile, tc.level, *findings)
			}
		}
	}
	// No lane configured for any line: the routed line's missing lane is the
	// unavailable check.
	for _, tc := range []struct {
		level  ConformanceEnforcement
		status int
	}{{EnforcementObserve, 0}, {EnforcementStructural, 0}, {EnforcementStrict, 500}} {
		g, _, findings := payerIngressGateway(t, "provider-data", tc.level, nil)
		status, msg := g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", partner)
		if status != tc.status {
			t.Errorf("no lanes/%s: %d %q, want %d", tc.level, status, msg, tc.status)
		}
		if tc.status == 0 && (len(*findings) != 1 || (*findings)[0].Verdict != "unavailable") {
			t.Errorf("no lanes/%s: findings %+v, want one unavailable", tc.level, *findings)
		}
		if tc.status != 0 && !strings.Contains(msg, "no FHIR validator lane") {
			t.Errorf("no lanes/%s: %q, want the missing lane named", tc.level, msg)
		}
	}
}

// The routed line is tried first, then the lines the answer itself claims, then
// the rest in 2.2, 2.1, 2.0 order, each once.
func TestPayerAnswerIngress_CandidateOrder(t *testing.T) {
	d20, _ := shnsdk.DTRLineDef("2.0")
	claimed20 := `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"q","meta":{"profile":["` +
		certificationDTR + `dtr-std-questionnaire|` + d20.PackageVersion + `"]}}}]}`
	params := `{"resourceType":"Parameters","parameter":[{"name":"PackageBundle","resource":` + claimed20 + `}]}`
	for _, tc := range []struct {
		routed, body string
		want         []string
	}{
		{"2.1", claimed20, []string{"2.1", "2.0", "2.2"}},
		{"2.1", params, []string{"2.1", "2.0", "2.2"}},
		{"2.2", claimed20, []string{"2.2", "2.0", "2.1"}},
		{"2.0", claimed20, []string{"2.0", "2.2", "2.1"}},
		{"2.1", answerBundle, []string{"2.1", "2.2", "2.0"}},
	} {
		g, log, _ := payerIngressGateway(t, "provider-data", EnforcementObserve, allLinesFailed)
		g.validateFHIRPayerIngress(answerCtx("dtr-questionnaire-fetch"), []byte(tc.body), tc.routed, "pa.dtr", partner)
		if got := log.lines(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("routed %s: lines tried %v, want %v", tc.routed, got, tc.want)
		}
	}
}

// A line with no lane the answer is not routed at and does not claim is not a
// candidate: only laned lines are tried.
func TestPayerAnswerIngress_TriesOnlyLanedLines(t *testing.T) {
	g, log, findings := payerIngressGateway(t, "provider-data", EnforcementStructural, map[string]scriptedVerdict{"2.2": scriptStructural, "2.0": scriptDeeper})
	status, _ := g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", partner)
	if status != 0 || !reflect.DeepEqual(log.lines(), []string{"2.0", "2.2"}) {
		t.Fatalf("status %d, lines %v", status, log.lines())
	}
	if len(*findings) != 1 || (*findings)[0].DeclaredLine != "2.0" || !reflect.DeepEqual((*findings)[0].Lines, []LineVerdictSummary{{"2.0", "deeper"}, {"2.2", "structural"}}) {
		t.Fatalf("findings %+v", *findings)
	}
}

// An outage on the routed line is not decided by the other lines: a 2.2 answer
// whose 2.2 lane is unavailable would fail 2.1's and 2.0's profiles whatever it is.
// With no line valid, the check is unavailable — recorded at structural, refused at
// strict as unavailable, never as a structural defect.
func TestPayerAnswerIngress_RoutedLineUnavailableDecidesUnavailable(t *testing.T) {
	verdicts := map[string]scriptedVerdict{"2.2": scriptOutage, "2.1": scriptStructural, "2.0": scriptStructural}
	for _, profile := range providerLanes {
		for _, tc := range []struct {
			level  ConformanceEnforcement
			status int
			msg    string
		}{
			{EnforcementObserve, 0, ""},
			{EnforcementStructural, 0, ""},
			{EnforcementStrict, 500, "validator unavailable"},
		} {
			g, log, findings := payerIngressGateway(t, profile, tc.level, verdicts)
			status, msg := g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.2", "pa.pas", partner)
			if status != tc.status || msg != tc.msg {
				t.Errorf("%s/%s: %d %q, want %d %q", profile, tc.level, status, msg, tc.status, tc.msg)
			}
			if got := log.lines(); !reflect.DeepEqual(got, []string{"2.2", "2.1", "2.0"}) {
				t.Errorf("%s/%s: lines tried %v", profile, tc.level, got)
			}
			if tc.status != 0 {
				if len(*findings) != 0 {
					t.Errorf("%s/%s: a refused unavailable check records nothing, got %+v", profile, tc.level, *findings)
				}
				continue
			}
			want := []LineVerdictSummary{{"2.2", "unavailable"}, {"2.1", "structural"}, {"2.0", "structural"}}
			if len(*findings) != 1 || (*findings)[0].Verdict != "unavailable" || (*findings)[0].Decision != "relayed" || !reflect.DeepEqual((*findings)[0].Lines, want) {
				t.Errorf("%s/%s: findings %+v, want one unavailable listing %+v", profile, tc.level, *findings, want)
			}
		}
	}
}

// A line the answer claims that no lane serves is not decided by the other lines
// either: the check is unavailable.
func TestPayerAnswerIngress_ClaimedLineWithNoLaneDecidesUnavailable(t *testing.T) {
	d20, _ := shnsdk.DTRLineDef("2.0")
	claimed20 := `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"q","meta":{"profile":["` +
		certificationDTR + `dtr-std-questionnaire|` + d20.PackageVersion + `"]}}}]}`
	for _, tc := range []struct {
		level  ConformanceEnforcement
		status int
	}{{EnforcementStructural, 0}, {EnforcementStrict, 500}} {
		g, log, findings := payerIngressGateway(t, "demo", tc.level, map[string]scriptedVerdict{"2.2": scriptStructural, "2.1": scriptStructural})
		status, msg := g.validateFHIRPayerIngress(answerCtx("dtr-questionnaire-fetch"), []byte(claimed20), "2.2", "pa.dtr", partner)
		if status != tc.status {
			t.Errorf("%s: %d %q, want %d", tc.level, status, msg, tc.status)
		}
		if tc.status != 0 && !strings.Contains(msg, "no FHIR validator lane configured for contract line 2.0") {
			t.Errorf("%s: %q, want the claimed line's missing lane named", tc.level, msg)
		}
		if tc.status == 0 && (len(*findings) != 1 || (*findings)[0].Line != "2.0") {
			t.Errorf("%s: findings %+v, want one naming the claimed 2.0 line", tc.level, *findings)
		}
		if got := log.lines(); !reflect.DeepEqual(got, []string{"2.2", "2.1"}) {
			t.Errorf("%s: lines tried %v", tc.level, got)
		}
		if tc.status == 0 {
			want := []LineVerdictSummary{{"2.2", "structural"}, {"2.0", "unavailable"}, {"2.1", "structural"}}
			if len(*findings) != 1 || (*findings)[0].Verdict != "unavailable" || !reflect.DeepEqual((*findings)[0].Lines, want) {
				t.Errorf("%s: findings %+v, want one unavailable listing %+v", tc.level, *findings, want)
			}
		}
	}
}

// An outage on a line the answer is neither routed at nor claims does not rescue
// it: the routed and claimed lines answered structural, so it is refused as
// structural.
func TestPayerAnswerIngress_UnclaimedLineOutageDoesNotRescue(t *testing.T) {
	d20, _ := shnsdk.DTRLineDef("2.0")
	claimed20 := `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"q","meta":{"profile":["` +
		certificationDTR + `dtr-std-questionnaire|` + d20.PackageVersion + `"]}}}]}`
	for _, level := range []ConformanceEnforcement{EnforcementStructural, EnforcementStrict} {
		g, _, findings := payerIngressGateway(t, "provider-data", level, map[string]scriptedVerdict{"2.2": scriptOutage, "2.1": scriptStructural, "2.0": scriptStructural})
		status, msg := g.validateFHIRPayerIngress(answerCtx("dtr-questionnaire-fetch"), []byte(claimed20), "2.1", "pa.dtr", partner)
		if status != http.StatusUnprocessableEntity || !strings.HasPrefix(msg, "ingress validation failed") {
			t.Errorf("%s: %d %q, want refused as structural", level, status, msg)
		}
		want := []LineVerdictSummary{{"2.1", "structural"}, {"2.0", "structural"}, {"2.2", "unavailable"}}
		if len(*findings) != 1 || (*findings)[0].Decision != "refused" || !reflect.DeepEqual((*findings)[0].Lines, want) {
			t.Errorf("%s: findings %+v, want one refused listing %+v", level, *findings, want)
		}
	}
}

// One validator object serving two lines is called once, for the first of them.
func TestPayerAnswerIngress_SharedValidatorIsCalledOnce(t *testing.T) {
	log := &laneLog{}
	shared := &scriptedLane{line: "2.0+2.2", verdict: scriptStructural, log: log}
	other := &scriptedLane{line: "2.1", verdict: scriptStructural, log: log}
	var findings []ConformanceFinding
	g := &Gateway{cfg: Config{
		OriginationProfile: "demo", ConformanceEnforcement: EnforcementObserve, Reg: shnsdk.NewRegistry(),
		Clock:            func() time.Time { return time.Unix(0, 0).UTC() },
		ValidatorsByLine: map[string]shnsdk.Validator{"2.0": shared, "2.2": shared, "2.1": other},
		Observer: func(e ObserverEvent) {
			var f ConformanceFinding
			if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil {
				findings = append(findings, f)
			}
		},
	}}
	g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", partner)
	if got := log.lines(); !reflect.DeepEqual(got, []string{"2.0+2.2", "2.1"}) {
		t.Fatalf("calls %v, want the shared validator once and the other once", got)
	}
	want := []LineVerdictSummary{{"2.0", "structural"}, {"2.1", "structural"}}
	if len(findings) != 1 || !reflect.DeepEqual(findings[0].Lines, want) {
		t.Fatalf("findings %+v, want lines %+v with no phantom 2.2", findings, want)
	}
}

// The routed line with no lane, while other lines have one: with no line valid the
// check is unavailable — recorded at observe and structural naming the missing
// routed lane beside the other lines' verdicts, refused at strict as the missing
// lane. An answer valid on another line is still relayed.
func TestPayerAnswerIngress_RoutedLineWithNoLane(t *testing.T) {
	for _, profile := range providerLanes {
		for _, tc := range []struct {
			level  ConformanceEnforcement
			status int
		}{{EnforcementObserve, 0}, {EnforcementStructural, 0}, {EnforcementStrict, 500}} {
			g, log, findings := payerIngressGateway(t, profile, tc.level, map[string]scriptedVerdict{"2.2": scriptStructural, "2.0": scriptStructural})
			status, msg := g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.1", "pa.pas", partner)
			if status != tc.status {
				t.Errorf("%s/%s: %d %q, want %d", profile, tc.level, status, msg, tc.status)
			}
			if got := log.lines(); !reflect.DeepEqual(got, []string{"2.2", "2.0"}) {
				t.Errorf("%s/%s: lines tried %v", profile, tc.level, got)
			}
			if tc.status != 0 {
				if !strings.Contains(msg, "no FHIR validator lane configured for contract line 2.1") {
					t.Errorf("%s/%s: %q, want the missing routed lane named", profile, tc.level, msg)
				}
				continue
			}
			want := []LineVerdictSummary{{"2.1", "unavailable"}, {"2.2", "structural"}, {"2.0", "structural"}}
			if len(*findings) != 1 || (*findings)[0].Verdict != "unavailable" || (*findings)[0].Decision != "relayed" ||
				(*findings)[0].Line != "2.1" || (*findings)[0].DeclaredLine != "2.1" || !reflect.DeepEqual((*findings)[0].Lines, want) {
				t.Errorf("%s/%s: findings %+v, want one unavailable at 2.1 listing %+v", profile, tc.level, *findings, want)
			}
		}
		g, _, findings := payerIngressGateway(t, profile, EnforcementStrict, map[string]scriptedVerdict{"2.2": scriptValid, "2.0": scriptStructural})
		if status, msg := g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.1", "pa.pas", partner); status != 0 {
			t.Errorf("%s: valid at 2.2 with no 2.1 lane: %d %q, want relayed", profile, status, msg)
		}
		if len(*findings) != 1 || (*findings)[0].Verdict != "valid" || (*findings)[0].Line != "2.2" {
			t.Errorf("%s: findings %+v, want the answer recorded valid at 2.2", profile, *findings)
		}
	}
}

// An extra line whose lane hangs is bounded by payerAnswerCandidateTimeout and
// recorded as unavailable for that line; the answer is decided on the others. The
// bound is shortened here, and a watchdog fails the test rather than letting a lost
// bound hang it.
func TestPayerAnswerIngress_HungExtraLaneIsBounded(t *testing.T) {
	saved := payerAnswerCandidateTimeout
	payerAnswerCandidateTimeout = 100 * time.Millisecond
	t.Cleanup(func() { payerAnswerCandidateTimeout = saved })
	g, _, findings := payerIngressGateway(t, "provider-data", EnforcementStrict, map[string]scriptedVerdict{"2.0": scriptStructural, "2.2": scriptHang, "2.1": scriptValid})
	type outcome struct {
		status int
		msg    string
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		status, msg := g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", partner)
		done <- outcome{status, msg}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no decision after 5s: the hung extra lane was not bounded")
	}
	if elapsed := time.Since(start); elapsed > payerAnswerCandidateTimeout+time.Second {
		t.Fatalf("decided after %s, want within the %s candidate bound", elapsed, payerAnswerCandidateTimeout)
	}
	if got.status != 0 {
		t.Fatalf("status %d %q, want relayed on 2.1", got.status, got.msg)
	}
	want := []LineVerdictSummary{{"2.0", "structural"}, {"2.2", "unavailable"}, {"2.1", "valid"}}
	if len(*findings) != 1 || !reflect.DeepEqual((*findings)[0].Lines, want) {
		t.Fatalf("findings %+v, want lines %+v", *findings, want)
	}
}

// The finding names the routed line and each line's verdict only: no validator
// diagnostic reaches either carrier through them.
func TestPayerAnswerIngress_LinesCarryNoDiagnostics(t *testing.T) {
	var details []string
	g, _, _ := payerIngressGateway(t, "provider-data", EnforcementObserve, allLinesFailed)
	g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == ConformanceObservedEvent {
			details = append(details, e.Detail)
		}
	}
	g.validateFHIRPayerIngress(answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", partner)
	if len(details) != 1 {
		t.Fatalf("%d findings", len(details))
	}
	if strings.Contains(details[0], "RAW-DIAGNOSTIC") {
		t.Fatalf("a validator diagnostic leaked into the finding: %s", details[0])
	}
	if !strings.Contains(details[0], `"declaredLine":"2.0","lines":[{"line":"2.0","verdict":"structural"},{"line":"2.2","verdict":"structural"},{"line":"2.1","verdict":"structural"}]`) {
		t.Fatalf("finding %s does not name the routed line and the lines tried", details[0])
	}
}

// A Parameters package answer reaches the validator wrapped in the resource
// parameter; a Bundle arrives bare.
func TestPayerAnswerIngress_ParametersPackageIsWrapped(t *testing.T) {
	params := `{"resourceType":"Parameters","parameter":[{"name":"PackageBundle","resource":` + answerBundle + `}]}`
	for _, tc := range []struct{ body, want string }{
		{params, `{"resourceType":"Parameters","parameter":[{"name":"resource","resource":` + params + `}]}`},
		{answerBundle, answerBundle},
	} {
		g, log, _ := payerIngressGateway(t, "provider-data", EnforcementObserve, map[string]scriptedVerdict{"2.0": scriptValid, "2.1": scriptValid, "2.2": scriptValid})
		g.validateFHIRPayerIngress(answerCtx("dtr-questionnaire-fetch"), []byte(tc.body), "2.0", "pa.dtr", partner)
		if log.count() != 1 || string(log.calls[0].Body) != tc.want {
			t.Fatalf("validator received %d calls, first %s; want %s", log.count(), log.calls, tc.want)
		}
	}
}

// The two SHN-operated bridging-demo payers front the reference payer, so their
// answers are its bytes: no validator call at any level, on either lane.
func TestPayerAnswerIngress_BridgeDemoPayersAreNotCertified(t *testing.T) {
	for _, profile := range providerLanes {
		for _, payer := range []shnsdk.PayerIdentifier{BridgeDemoPayerID, BridgeRefusePayerID} {
			for _, level := range everyLevel {
				g, log, findings := payerIngressGateway(t, profile, level, allLinesFailed)
				for _, leg := range []struct{ legType, contract string }{{"dtr-questionnaire-fetch", "pa.dtr"}, {"pas-claim", "pa.pas"}, {"pas-claim-update", "pa.pas"}} {
					if status, msg := g.validateFHIRPayerIngress(answerCtx(leg.legType), []byte(answerBundle), "2.0", leg.contract, payer); status != 0 {
						t.Errorf("%s/%s/%s %s: status %d %q, want relayed", profile, payer.Value, level, leg.legType, status, msg)
					}
				}
				if log.count() != 0 || len(*findings) != 0 {
					t.Errorf("%s/%s/%s: %d validator calls, %d findings, want none", profile, payer.Value, level, log.count(), len(*findings))
				}
			}
		}
	}
}

// The engine's reference set is closed and exactly the published identities: the
// three reference payers and the two demo payers that front 00001.
func TestReferencePayerIdentities(t *testing.T) {
	sys := shnsdk.CMSPayerIdentity.System
	want := []shnsdk.PayerIdentifier{{System: sys, Value: "00001"}, {System: sys, Value: "00300"}, {System: sys, Value: "00301"}, BridgeDemoPayerID, BridgeRefusePayerID}
	if got := ReferencePayerIdentities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reference payers %v", got)
	}
	if !isReferencePayer(shnsdk.CMSPayerIdentity) {
		t.Fatal("shnsdk.CMSPayerIdentity is the 2.0 reference payer")
	}
	ids := ReferencePayerIdentities()
	ids[0] = partner
	if isReferencePayer(partner) || !isReferencePayer(want[0]) {
		t.Fatal("ReferencePayerIdentities must return a copy")
	}
	for _, p := range []shnsdk.PayerIdentifier{
		{}, partner, {System: sys, Value: "00302"}, {System: sys, Value: "0001"}, {System: sys, Value: " 00001"},
		{System: "urn:oid:2.16.840.1.113883.6.301", Value: "00001"}, {System: "", Value: "00001"}, {System: sys + " ", Value: "00001"},
		{System: BridgeDemoPayerID.System, Value: "SHN-BRIDGE-OTHER"}, {System: sys, Value: BridgeDemoPayerID.Value},
	} {
		if isReferencePayer(p) {
			t.Errorf("%+v is not a reference payer", p)
		}
	}
}
