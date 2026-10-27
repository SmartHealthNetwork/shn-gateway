//go:build !shn_coreonly

package engine

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// A structural marker in a payer's answer claims the lines whose packages
// define it where it sits, and a claim is met by any one of its lines judged.
// qr-context (on a QuestionnaireResponse) and serviceItemRequestType (on a
// Claim's item) are defined at every line, so they claim no line in
// particular; qr-coverage is defined at 2.2 alone, so it claims 2.2. Before,
// each of the first three named 2.1 alone, which made 2.1 decisive: a provider
// with no 2.1 lane read every such answer unavailable, refused 500 at strict,
// whatever its own lines said.

// markerAnswers are, per leg, an answer carrying a marker every line defines
// where it sits: a DTR QuestionnaireResponse with its qr-context, and a PAS
// answer echoing a Claim whose item carries serviceItemRequestType.
var markerAnswers = []struct{ legType, contract, body string }{
	{"dtr-questionnaire-fetch", "pa.dtr", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"QuestionnaireResponse","id":"qr","extension":[{"url":"` + certificationDTR + `qr-context","valueReference":{"reference":"Coverage/c"}}]}}]}`},
	{"pas-claim", "pa.pas", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Claim","id":"c","item":[{"sequence":1,"extension":[{"url":"` + certificationPAS + `extension-serviceItemRequestType","valueCodeableConcept":{"text":"x"}}]}]}}]}`},
}

// coverageAnswer is a DTR answer whose QuestionnaireResponse carries
// qr-coverage, which only 2.2's packages define.
const coverageAnswer = `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"QuestionnaireResponse","id":"qr","extension":[{"url":"` + certificationDTR + `qr-coverage","valueReference":{"reference":"Coverage/c"}}]}}]}`

// With only a 2.0 lane, an answer carrying a marker every line defines is
// judged at 2.0 and decided by 2.0's verdict at every level that checks: a
// structural defect is recorded at observe and refused 422 at structural and
// strict, never unavailable for want of a 2.1 lane.
func TestPayerAnswerIngress_EveryLineMarkerIsDecidedWhereJudged(t *testing.T) {
	for _, a := range markerAnswers {
		for _, tc := range []struct {
			level  ConformanceEnforcement
			status int
		}{{EnforcementObserve, 0}, {EnforcementStructural, http.StatusUnprocessableEntity}, {EnforcementStrict, http.StatusUnprocessableEntity}} {
			g, log, findings := payerIngressGateway(t, "provider-data", tc.level, map[string]scriptedVerdict{"2.0": scriptStructural})
			status, msg := drainedPayerIngress(g, answerCtx(a.legType), []byte(a.body), "2.0", a.contract, partner)
			if status != tc.status || tc.status != 0 && !strings.HasPrefix(msg, "ingress validation failed") {
				t.Errorf("%s/%s: %d %q, want %d, decided at 2.0", a.legType, tc.level, status, msg, tc.status)
			}
			if got := log.lines(); !reflect.DeepEqual(got, []string{"2.0"}) {
				t.Errorf("%s/%s: lines tried %v, want 2.0", a.legType, tc.level, got)
			}
			// A defect's finding carries its issues, and the line's verdict in Lines.
			if len(*findings) != 1 || (*findings)[0].Verdict != "" || (*findings)[0].Line != "2.0" ||
				!reflect.DeepEqual((*findings)[0].Lines, []LineVerdictSummary{{Line: "2.0", Verdict: "structural"}}) {
				t.Errorf("%s/%s: findings %+v, want one defect at 2.0, its only line structural", a.legType, tc.level, *findings)
				continue
			}
			for _, l := range (*findings)[0].Lines {
				if l.Line == "2.1" {
					t.Errorf("%s/%s: 2.1, which has no lane and decides nothing, is listed: %+v", a.legType, tc.level, (*findings)[0].Lines)
				}
			}
		}
	}
}

// A claim none of whose lines has a lane cannot be met: qr-coverage claims 2.2,
// and with no 2.2 lane an answer the other lines do not read valid is
// unavailable at every level that checks, refused 500 at strict, naming 2.2.
func TestPayerAnswerIngress_ClaimWithNoJudgeableLineIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		level  ConformanceEnforcement
		status int
	}{{EnforcementObserve, 0}, {EnforcementStructural, 0}, {EnforcementStrict, http.StatusInternalServerError}} {
		g, _, findings := payerIngressGateway(t, "provider-data", tc.level, map[string]scriptedVerdict{"2.0": scriptStructural})
		status, msg := drainedPayerIngress(g, answerCtx("dtr-questionnaire-fetch"), []byte(coverageAnswer), "2.0", "pa.dtr", partner)
		if status != tc.status {
			t.Errorf("%s: %d %q, want %d", tc.level, status, msg, tc.status)
		}
		if tc.status != 0 {
			if !strings.Contains(msg, "contract line 2.2") {
				t.Errorf("%s: %q, want the missing 2.2 lane named", tc.level, msg)
			}
			continue
		}
		if len(*findings) != 1 || (*findings)[0].Verdict != "unavailable" || (*findings)[0].Line != "2.2" {
			t.Errorf("%s: findings %+v, want one unavailable at 2.2", tc.level, *findings)
		}
	}
}

// A claim is met by any one of its lines: with 2.1's default lane still
// qualifying and 2.0 judging the answer valid, an answer carrying a marker
// every line defines is valid at every level that checks, and the qualifying
// lane is never called. (A claim of 2.1 alone, by a 2.1 profile, is
// unavailable in the same state: TestPayerAnswerIngress_QualifyingClaimedLineIsUnavailable.)
func TestPayerAnswerIngress_ClaimMetWhileOneOfItsLinesQualifies(t *testing.T) {
	for _, a := range markerAnswers {
		for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			g, log, findings, _ := qualifyingIngressGateway(t, level, a.contract, "2.1", scriptValid, map[string]scriptedVerdict{"2.0": scriptValid, "2.2": scriptStructural})
			if status, msg := drainedPayerIngress(g, answerCtx(a.legType), []byte(a.body), "2.0", a.contract, partner); status != 0 {
				t.Errorf("%s/%s: %d %q, want relayed valid", a.legType, level, status, msg)
			}
			// The claim is met at 2.0, so nothing 2.2 says could change the
			// outcome: it is not called, nor is the qualifying 2.1.
			if got := log.lines(); !reflect.DeepEqual(got, []string{"2.0"}) {
				t.Errorf("%s/%s: lines tried %v, want 2.0 alone", a.legType, level, got)
			}
			if len(*findings) != 0 {
				t.Errorf("%s/%s: findings %+v, want none (valid at the routed line)", a.legType, level, *findings)
			}
		}
	}
}

// A claim every line of which is still qualifying is not met by a line outside
// it: qr-coverage claims 2.2, and with 2.2's lane qualifying the answer is
// unavailable even though 2.0 reads it valid; refused 500 at strict.
func TestPayerAnswerIngress_ClaimOfOnlyQualifyingLinesIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		level  ConformanceEnforcement
		status int
	}{{EnforcementObserve, 0}, {EnforcementStructural, 0}, {EnforcementStrict, http.StatusInternalServerError}} {
		g, _, findings, _ := qualifyingIngressGateway(t, tc.level, "pa.dtr", "2.2", scriptValid, map[string]scriptedVerdict{"2.0": scriptValid})
		status, msg := drainedPayerIngress(g, answerCtx("dtr-questionnaire-fetch"), []byte(coverageAnswer), "2.0", "pa.dtr", partner)
		if status != tc.status {
			t.Errorf("%s: %d %q, want %d", tc.level, status, msg, tc.status)
		}
		if tc.status != 0 {
			if msg != "validator unavailable" {
				t.Errorf("%s: %q, want validator unavailable", tc.level, msg)
			}
			continue
		}
		if len(*findings) != 1 || (*findings)[0].Verdict != "unavailable" || (*findings)[0].Line != "2.2" {
			t.Errorf("%s: findings %+v, want one unavailable at 2.2", tc.level, *findings)
		}
	}
}

// Only the registered markers claim lines. An extension the packages define at
// one line alone, but which is not a registered marker, claims nothing: the
// extract is data, and widening what claims a line would add strict
// refusals. extension-TransmissionIdentifiers (on a Claim, 2.2 alone) and
// extension-reviewAction (on a Claim's item, 2.0 alone) claim no line, so an
// answer carrying them is decided where it is judged.
func TestUnregisteredSingleLineExtensionClaimsNothing(t *testing.T) {
	for _, url := range []string{certificationPAS + "extension-TransmissionIdentifiers", certificationPAS + "extension-reviewAction"} {
		if paLineMarkers[url] {
			t.Fatalf("%s is a registered marker; this row needs one that is not", url)
		}
	}
	if got := paLinesDefining(certificationPAS+"extension-TransmissionIdentifiers", "Claim", []string{"2.2", "2.1", "2.0"}); !reflect.DeepEqual(got, []string{"2.2"}) {
		t.Fatalf("the extract defines TransmissionIdentifiers on a Claim at %v; this row needs a single line", got)
	}
	if got := paLinesDefining(certificationPAS+"extension-reviewAction", "Claim.item", []string{"2.2", "2.1", "2.0"}); !reflect.DeepEqual(got, []string{"2.0"}) {
		t.Fatalf("the extract defines reviewAction on a Claim item at %v; this row needs a single line", got)
	}
	body := `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Claim","id":"c","extension":[{"url":"` + certificationPAS + `extension-TransmissionIdentifiers"}],"item":[{"sequence":1,"extension":[{"url":"` + certificationPAS + `extension-reviewAction"}]}]}}]}`
	if order, claims := (paCertification{}).lineClaims([]byte(body)); !reflect.DeepEqual(order, []string{"2.2", "2.1", "2.0"}) || len(claims) != 0 {
		t.Fatalf("order %v claims %v, want the default order and no claim", order, claims)
	}
	g, log, _ := payerIngressGateway(t, "provider-data", EnforcementStrict, map[string]scriptedVerdict{"2.0": scriptStructural})
	if status, msg := drainedPayerIngress(g, answerCtx("pas-claim"), []byte(body), "2.0", "pa.pas", partner); status != http.StatusUnprocessableEntity || !strings.HasPrefix(msg, "ingress validation failed") {
		t.Errorf("strict: %d %q, want refused 422 as judged at 2.0, not unavailable for want of a 2.2 lane", status, msg)
	}
	if got := log.lines(); !reflect.DeepEqual(got, []string{"2.0"}) {
		t.Errorf("lines tried %v, want 2.0", got)
	}
}

// After a valid answer, a later line is still tried while it could meet a
// claim a qualifying line has left unmet: here 2.0 (routed) reads valid, and
// the claim of 2.1 (qualifying) and 2.2 is met only if 2.2 is judged. If 2.2
// answers, valid or not, the claim is met and the answer is valid; if 2.2
// cannot answer, the qualifying 2.1 keeps the valid answer from deciding.
func TestValidAnswerWaitsForALaterLineThatCanMeetAClaim(t *testing.T) {
	for _, tc := range []struct {
		at22   scriptedVerdict
		status int
	}{{scriptValid, 0}, {scriptStructural, 0}, {scriptOutage, http.StatusInternalServerError}} {
		g, log, _ := payerIngressGateway(t, "provider-data", EnforcementStrict, nil)
		lanes := []lineLane{
			{Line: "2.0", V: &scriptedLane{line: "2.0", verdict: scriptValid, log: log}, Decisive: true},
			{Line: "2.1", Qualifying: true, Claims: []int{0}, Extra: true},
			{Line: "2.2", V: &scriptedLane{line: "2.2", verdict: tc.at22, log: log}, Claims: []int{0}, Extra: true},
		}
		ctx := answerCtx("pas-claim")
		res := g.validateGovernedLines(ctx, findingContextFrom(ctx), lanes, []byte(answerBundle), "ingress", "2.0", "", false, true)
		if res.Status != tc.status {
			t.Errorf("2.2 answering %v: status %d %q, want %d", tc.at22, res.Status, res.Msg, tc.status)
		}
		if got := log.lines(); !reflect.DeepEqual(got, []string{"2.0", "2.2"}) {
			t.Errorf("2.2 answering %v: lines tried %v, want 2.0 then 2.2", tc.at22, got)
		}
	}
}
