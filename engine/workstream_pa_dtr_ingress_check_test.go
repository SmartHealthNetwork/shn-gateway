//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// mixedDTRAnswer is a payer's package answer whose parameters are a's then b's,
// so it can use both DTR name families.
func mixedDTRAnswer(t *testing.T, a, b []byte) []byte {
	t.Helper()
	var pa, pb struct {
		Parameter []json.RawMessage `json:"parameter"`
	}
	if json.Unmarshal(a, &pa) != nil || json.Unmarshal(b, &pb) != nil {
		t.Fatal("not a Parameters")
	}
	out, err := json.Marshal(map[string]any{"resourceType": "Parameters", "parameter": append(pa.Parameter, pb.Parameter...)})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// dtrAnswerCheckLevels are the conformance settings a provider can run the ingress
// at: module 3 absent, then each level.
type dtrAnswerCheckLevel struct {
	name   string
	level  ConformanceEnforcement
	absent bool
}

func dtrAnswerCheckLevels() []dtrAnswerCheckLevel {
	levels := []dtrAnswerCheckLevel{{name: "absent", level: EnforcementObserve, absent: true}}
	for _, l := range allLevels {
		levels = append(levels, dtrAnswerCheckLevel{name: l.String(), level: l})
	}
	return levels
}

// dtrAnswerCheckRow posts the EHR's package request to a provider gateway declaring
// declared at c, module 2 on or off, on the origination lane profile, the payer
// answering answer, and returns the answer's findings.
func dtrAnswerCheckRow(t *testing.T, c dtrAnswerCheckLevel, declared []string, answer []byte, module2 bool, profile string) (e08Result, []ConformanceFinding) {
	t.Helper()
	r := e08RowTuned(t, c.level, declared, answer, nil, nil, nil, func(g *Gateway) {
		g.cfg.TranslateIGLines = module2
		declareOriginationProfile(g, profile)
		if c.absent {
			disableConformanceModule(&g.cfg)
		}
	})
	var fhir []ConformanceFinding
	for _, f := range r.findings {
		if f.LegType == dtrLeg && f.Profile == dtrOutputParametersProfile {
			fhir = append(fhir, f)
		}
	}
	return r, fhir
}

// findingLines is the lines a finding tried, sorted.
func findingLines(f ConformanceFinding) []string {
	var lines []string
	for _, l := range f.Lines {
		lines = append(lines, l.Line)
	}
	slices.Sort(lines)
	return lines
}

// wantRefused checks the ingress refused the payer's answer: 502 in the
// operation's OperationOutcome, as the ingress refuses its other carried
// answers, delivered yes (the payer answered, with an answer verified as its
// own), the leg recorded as an error, and none of the payer's bytes sent.
func (r e08Result) wantRefused(t *testing.T, answer []byte) {
	t.Helper()
	wantDeliveredOutcome(t, r.rec, http.StatusBadGateway, deliveredYes, "ingress validation failed")
	if bytes.Contains(r.rec.Body.Bytes(), []byte("ackage")) {
		t.Fatalf("the refused answer reached the EHR: %s", r.rec.Body.String())
	}
	if leg, outcome := lastOutcome(t, r.env.originator); leg != dtrLeg || outcome != "error" {
		t.Fatalf("recorded %s %s", leg, outcome)
	}
}

// The provider's Da Vinci ingress judges the payer's DTR package answer at its
// conformance level, as it judges the PAS answers it carries (carriedAnswerRefused,
// D48), whether or not module 2 then renames it, and for every payer: the
// coverage names 00001, a reference payer, judged like any payer unless the
// participant lists it, on the plain and the demo origination lane. The answer is judged
// at the lines of the names it uses (PackageBundle: 2.0 and 2.1; packagebundle:
// 2.2), never at a line whose profile leaves those names in an open slice: a
// package that is not a Bundle is valid at the other family's line, and that line
// does not judge it. So for every provider line and both families it is carried
// with no finding when module 3 is absent or at none, carried and recorded at
// observe, and refused at structural (its defect is structural) and strict.
func TestDTRIngress_APackageThatIsNotABundleIsJudgedAtItsNamesLines(t *testing.T) {
	t.Parallel()
	for _, fam := range []struct {
		name   string
		answer []byte
		lines  []string
	}{
		{"2.0 names", e08Answer("PackageBundle", nil, true), []string{"2.0", "2.1"}},
		{"2.2 names", e08Answer("packagebundle", nil, true), []string{"2.2"}},
	} {
		for _, declared := range [][]string{declares20, declares21, declares22} {
			for _, module2 := range []bool{true, false} {
				for _, profile := range []string{"", "demo"} {
					for _, c := range dtrAnswerCheckLevels() {
						name := fam.name + "/" + declared[1] + "/" + c.name
						if module2 {
							name += "/module2"
						}
						if profile != "" {
							name += "/" + profile
						}
						t.Run(name, func(t *testing.T) {
							r, fhir := dtrAnswerCheckRow(t, c, declared, fam.answer, module2, profile)
							switch {
							case c.absent || c.level == EnforcementNone:
								r.wantAnswered(t, fam.answer, false)
								if len(fhir) != 0 {
									t.Fatalf("a finding with no check: %+v", fhir)
								}
							case c.level == EnforcementObserve:
								r.wantAnswered(t, fam.answer, false)
								if len(fhir) != 1 || fhir[0].Decision != Record.String() || fhir[0].Verdict == findingVerdict(VerdictValid) ||
									!slices.Equal(findingLines(fhir[0]), fam.lines) {
									t.Fatalf("want one recorded finding at %v alone, got %+v", fam.lines, fhir)
								}
							default:
								r.wantRefused(t, fam.answer)
								if len(fhir) != 1 || fhir[0].Decision != Refuse.String() || !slices.Equal(findingLines(fhir[0]), fam.lines) {
									t.Fatalf("want one refused finding at %v alone, got %+v", fam.lines, fhir)
								}
							}
						})
					}
				}
			}
		}
	}
}

// An answer using both name families must be valid at a line of each: every
// part it names is then judged by a line that profiles it. Valid at a line of
// each, it is carried with no finding; invalid at every line of either family,
// it is recorded at observe and refused at structural and strict, the finding
// naming that family's lines.
func TestDTRIngress_AnswerNamingBothFamiliesIsJudgedAtEach(t *testing.T) {
	t.Parallel()
	q, qr := "Questionnaire", "QuestionnaireResponse"
	notBundle20, notBundle22 := e08Answer("PackageBundle", nil, true), e08Answer("packagebundle", nil, true)
	ok20, ok22 := e08Answer("PackageBundle", []string{q}, false), e08Answer("packagebundle", []string{q, qr}, false)
	for _, row := range []struct {
		name  string
		a, b  []byte
		fails []string // the family's lines it is invalid at, or none
	}{
		{"valid at a line of each", ok20, ok22, nil},
		{"its 2.2 package is not a Bundle", ok20, notBundle22, []string{"2.2"}},
		{"its 2.0 package is not a Bundle", notBundle20, ok22, []string{"2.0", "2.1"}},
	} {
		answer := mixedDTRAnswer(t, row.a, row.b)
		for _, module2 := range []bool{true, false} {
			for _, c := range dtrAnswerCheckLevels() {
				t.Run(row.name+"/"+c.name, func(t *testing.T) {
					r, fhir := dtrAnswerCheckRow(t, c, declares22, answer, module2, "")
					switch {
					case row.fails == nil || c.absent || c.level == EnforcementNone:
						r.wantAnswered(t, answer, false)
						if len(fhir) != 0 {
							t.Fatalf("module2=%v: findings %+v", module2, fhir)
						}
					case c.level == EnforcementObserve:
						r.wantAnswered(t, answer, false)
						if len(fhir) != 1 || fhir[0].Decision != Record.String() || !slices.Equal(findingLines(fhir[0]), row.fails) {
							t.Fatalf("module2=%v: want one recorded finding at %v, got %+v", module2, row.fails, fhir)
						}
					default:
						r.wantRefused(t, answer)
						if len(fhir) != 1 || fhir[0].Decision != Refuse.String() || !slices.Equal(findingLines(fhir[0]), row.fails) {
							t.Fatalf("module2=%v: want one refused finding at %v, got %+v", module2, row.fails, fhir)
						}
					}
				})
			}
		}
	}
}

// An answer using no line-specific name is judged at every line, where each
// line's profile judges the absence itself: DTR 2.0.1 requires PackageBundle,
// 2.1.0 and 2.2.0 do not. So an answer naming only outcome is valid, at 2.1, and
// carried; routed at 2.0, where it fails, it is carried with a finding naming
// 2.0 and the line it passed.
func TestDTRIngress_AnswerNamingNoFamilyIsJudgedAtEveryLine(t *testing.T) {
	t.Parallel()
	answer := e08Answer("", nil, false, "outcome")
	for _, c := range dtrAnswerCheckLevels() {
		t.Run(c.name, func(t *testing.T) {
			r, fhir := dtrAnswerCheckRow(t, c, declares20, answer, true, "")
			r.wantAnswered(t, answer, false)
			if c.absent || c.level == EnforcementNone {
				if len(fhir) != 0 {
					t.Fatalf("a finding with no check: %+v", fhir)
				}
				return
			}
			if len(fhir) != 1 || fhir[0].Decision != Record.String() || fhir[0].Verdict != findingVerdict(VerdictValid) ||
				len(fhir[0].Lines) != 2 || fhir[0].Lines[0].Line != "2.0" || fhir[0].Lines[1].Verdict != findingVerdict(VerdictValid) {
				t.Fatalf("want one recorded finding failing 2.0 and passing the next line, got %+v", fhir)
			}
		})
	}
}

// Within the 2.0 and 2.1 family an answer valid at 2.1 alone is valid: DTR 2.1.0
// makes PackageBundle optional, a real verdict of a line that profiles the names.
// With module 2 on it is renamed into the 2.2 provider's names, as E-08 renames an
// answer valid at either line of its family, and carried at every level.
func TestDTRIngress_AnswerValidAtTwoPointOneAloneIsCarried(t *testing.T) {
	t.Parallel()
	answer := e08Answer("", nil, false, "operationOutcome")
	for _, c := range dtrAnswerCheckLevels() {
		t.Run(c.name, func(t *testing.T) {
			r, fhir := dtrAnswerCheckRow(t, c, declares22, answer, true, "")
			if r.rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", r.rec.Code, r.rec.Body.String())
			}
			if !slices.Contains(r.recordedEdits(), string(relay.EditDTRAnswerLineNames)) {
				t.Fatalf("not carried across: edits %v, body %s", r.recordedEdits(), r.rec.Body.String())
			}
			for _, f := range fhir {
				if f.Kind != string(KindFHIRBridged) {
					t.Fatalf("a finding for an answer valid at its own family's line: %+v", f)
				}
			}
		})
	}
}

// An answer valid at the routed line is carried at every level with no finding,
// whether or not module 2 is on: the ingress's check asks nothing more of it.
func TestDTRIngress_AnswerValidAtTheRoutedLineIsCarriedUnremarked(t *testing.T) {
	t.Parallel()
	answer := e08Answer("packagebundle", []string{"Questionnaire", "QuestionnaireResponse"}, false)
	for _, module2 := range []bool{true, false} {
		for _, level := range allLevels {
			t.Run(level.String(), func(t *testing.T) {
				r := e08RowTuned(t, level, declares22, answer, nil, nil, nil, func(g *Gateway) { g.cfg.TranslateIGLines = module2 })
				r.wantAnswered(t, answer, false)
				for _, f := range r.findings {
					if f.LegType == dtrLeg {
						t.Fatalf("module2=%v: a finding for an answer valid at the routed line: %+v", module2, f)
					}
				}
			})
		}
	}
}

// Module 1's own DTR fetch judges a partner payer's package answer by the same
// rule: a 2.0-named answer that fails 2.0 and 2.1 is refused at strict even
// though the 2.2 line, which leaves its names unprofiled, would pass it.
func TestPayerAnswerIngress_DTRAnswerIsJudgedAtItsNamesLines(t *testing.T) {
	params := `{"resourceType":"Parameters","parameter":[{"name":"PackageBundle","resource":{"resourceType":"Patient"}}]}`
	verdicts := map[string]scriptedVerdict{"2.0": scriptStructural, "2.1": scriptStructural, "2.2": scriptValid}
	g, log, findings := payerIngressGateway(t, "provider-data", EnforcementStrict, verdicts)
	status, _ := drainedPayerIngress(g, answerCtx("dtr-questionnaire-fetch"), []byte(params), "2.2", "pa.dtr", partner)
	if status == 0 {
		t.Fatalf("carried an answer valid only at a line that does not profile its names; lines %v", log.lines())
	}
	if got := log.lines(); !slices.Equal(got, []string{"2.1", "2.0"}) {
		t.Fatalf("lines tried %v, want 2.1 and 2.0 alone", got)
	}
	if len(*findings) != 1 || (*findings)[0].Decision != Refuse.String() {
		t.Fatalf("findings %+v", *findings)
	}
}

// A family line whose validator is down does not decide for the family lines
// that answered: a 2.0-named package that is not a Bundle, its 2.1 lane down,
// is judged by 2.0's structural verdict and refused at structural, as at strict.
// With no lane for either of its family's lines it is never judged at 2.2, whose
// profile leaves its names unprofiled: the check cannot run, refused 500 at
// strict.
func TestDTRIngress_AnUnjudgedFamilyLineNeverDecidesOrWidens(t *testing.T) {
	answer := e08Answer("PackageBundle", nil, true)
	for _, level := range []ConformanceEnforcement{EnforcementStructural, EnforcementStrict} {
		t.Run("2.1 down/"+level.String(), func(t *testing.T) {
			r := e08RowWith(t, level, declares22, answer, func(m map[string]shnsdk.Validator) { m["2.1"] = failingValidator{} })
			r.wantRefused(t, answer)
		})
	}
	t.Run("no family lane/strict", func(t *testing.T) {
		r := e08RowWith(t, EnforcementStrict, declares22, answer, func(m map[string]shnsdk.Validator) { delete(m, "2.0"); delete(m, "2.1") })
		if r.rec.Code != http.StatusInternalServerError {
			t.Fatalf("judged at a line that does not profile its names: %d %s", r.rec.Code, r.rec.Body.String())
		}
	})
}

// The one answer now carried at structural that E-08 refused: valid at neither
// line of its names, structural at 2.0 and only deeper at 2.1. E-08 judged it at
// 2.0 alone and refused it at structural; the ingress classifies it by its best
// verdict across both lines, as it does a PAS answer across its lines, so it is
// recorded at structural, the finding at 2.1, and refused at strict as before.
func TestDTRIngress_BestVerdictAcrossItsFamilyDecidesTheLevel(t *testing.T) {
	answer := e08Answer("PackageBundle", nil, true)
	log := &laneLog{}
	lanes := func(m map[string]shnsdk.Validator) {
		m["2.0"] = &scriptedLane{line: "2.0", verdict: scriptStructural, log: log}
		m["2.1"] = &scriptedLane{line: "2.1", verdict: scriptDeeper, log: log}
	}
	t.Run("structural", func(t *testing.T) {
		r := e08RowWith(t, EnforcementStructural, declares22, answer, lanes)
		r.wantAnswered(t, answer, false)
		var fhir []ConformanceFinding
		for _, f := range r.findings {
			if f.LegType == dtrLeg && f.Profile == dtrOutputParametersProfile {
				fhir = append(fhir, f)
			}
		}
		if len(fhir) != 1 || fhir[0].Decision != Record.String() || fhir[0].Line != "2.1" || !slices.Equal(findingLines(fhir[0]), []string{"2.0", "2.1"}) {
			t.Fatalf("want one recorded finding at 2.1 over 2.0 and 2.1, got %+v", fhir)
		}
	})
	t.Run("strict", func(t *testing.T) {
		e08RowWith(t, EnforcementStrict, declares22, answer, lanes).wantRefused(t, answer)
	})
}

// e08ClaimsLine21 is a 2.0-named package whose Questionnaire declares a DTR
// profile at 2.1.0, a claim on 2.1 alone. (A qr-context claims every line, the
// lines whose packages define it, so it constrains nothing.)
func e08ClaimsLine21() []byte {
	return bytes.Replace(e08Answer("PackageBundle", []string{"Questionnaire"}, false),
		[]byte(`"resourceType" : "Questionnaire", "status"`),
		[]byte(`"resourceType" : "Questionnaire", "meta" : { "profile" : [ "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-std-questionnaire|2.1.0" ] }, "status"`), 1)
}

// A claim the answer itself makes decides when none of its lines can be judged,
// as for any payer's answer: a 2.0-named package declaring a 2.1 profile,
// structural at 2.0, with no 2.1 lane, is unavailable rather than decided by
// 2.0: recorded at structural with the finding at 2.1, refused 500 at strict.
// E-08 judged it at 2.0 alone and refused it 422.
func TestDTRIngress_AnUnjudgedClaimedLineMakesTheCheckUnavailable(t *testing.T) {
	answer := e08ClaimsLine21()
	if !bytes.Contains(answer, []byte("|2.1.0")) {
		t.Fatal("the answer declares no 2.1 profile")
	}
	lanes := func(m map[string]shnsdk.Validator) {
		m["2.0"] = &scriptedLane{line: "2.0", verdict: scriptStructural, log: &laneLog{}}
		delete(m, "2.1")
	}
	t.Run("structural", func(t *testing.T) {
		r := e08RowWith(t, EnforcementStructural, declares22, answer, lanes)
		if r.rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s", r.rec.Code, r.rec.Body.String())
		}
		var fhir []ConformanceFinding
		for _, f := range r.findings {
			if f.LegType == dtrLeg && f.Profile == dtrOutputParametersProfile {
				fhir = append(fhir, f)
			}
		}
		if len(fhir) != 1 || fhir[0].Verdict != "unavailable" || fhir[0].Decision != Record.String() || fhir[0].Line != "2.1" {
			t.Fatalf("want one unavailable finding at 2.1, got %+v", fhir)
		}
	})
	t.Run("strict", func(t *testing.T) {
		r := e08RowWith(t, EnforcementStrict, declares22, answer, lanes)
		if r.rec.Code != http.StatusInternalServerError {
			t.Fatalf("answer %d %s", r.rec.Code, r.rec.Body.String())
		}
	})
}

// An unavailable finding names the line that could not be judged, never one
// that was not tried: a claimed line whose validator fails (a 2.1 profile claims
// 2.1), and, when no decisive line was tried, the first family line tried.
func TestDTRIngress_AnUnavailableFindingNamesTheLineThatCouldNotBeJudged(t *testing.T) {
	for _, row := range []struct {
		name   string
		answer []byte
		lanes  func(map[string]shnsdk.Validator)
		line   string
	}{
		{"a claimed line's validator fails", e08ClaimsLine21(), func(m map[string]shnsdk.Validator) {
			m["2.0"] = &scriptedLane{line: "2.0", verdict: scriptStructural, log: &laneLog{}}
			m["2.1"] = failingValidator{}
		}, "2.1"},
		{"every family line's validator fails", e08Answer("PackageBundle", nil, true), func(m map[string]shnsdk.Validator) {
			m["2.0"], m["2.1"] = failingValidator{}, failingValidator{}
		}, "2.1"},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := e08RowWith(t, EnforcementStructural, declares22, row.answer, row.lanes)
			var fhir []ConformanceFinding
			for _, f := range r.findings {
				if f.LegType == dtrLeg && f.Profile == dtrOutputParametersProfile {
					fhir = append(fhir, f)
				}
			}
			if len(fhir) != 1 || fhir[0].Verdict != "unavailable" || fhir[0].Line != row.line {
				t.Fatalf("want one unavailable finding at %s, got %+v", row.line, fhir)
			}
		})
	}
}

// A group whose only lane is still qualifying says the validator is unavailable
// at that line, never that no lane is configured at the routed line outside the
// group: a 2.0-named package routed at 2.2, the 2.1 lane qualifying and no 2.0
// lane, is refused 500 "validator unavailable" at strict and recorded unavailable
// at 2.1 at structural.
func TestDTRIngress_AQualifyingGroupLaneSaysUnavailableAtItsLine(t *testing.T) {
	answer := e08Answer("PackageBundle", nil, true)
	lanes := func(m map[string]shnsdk.Validator) {
		delete(m, "2.0")
		m["2.1"] = NewDiscoveredLane("2.1", DefaultLaneURL("2.1"), &scriptedLane{line: "2.1", verdict: scriptValid, log: &laneLog{}})
	}
	t.Run("strict", func(t *testing.T) {
		r := e08RowWith(t, EnforcementStrict, declares22, answer, lanes)
		if r.rec.Code != http.StatusInternalServerError || strings.Contains(r.rec.Body.String(), "no FHIR validator lane") || !strings.Contains(r.rec.Body.String(), "validator unavailable") {
			t.Fatalf("answer %d %s, want 500 validator unavailable", r.rec.Code, r.rec.Body.String())
		}
	})
	t.Run("structural", func(t *testing.T) {
		r := e08RowWith(t, EnforcementStructural, declares22, answer, lanes)
		var fhir []ConformanceFinding
		for _, f := range r.findings {
			if f.LegType == dtrLeg && f.Profile == dtrOutputParametersProfile {
				fhir = append(fhir, f)
			}
		}
		if len(fhir) != 1 || fhir[0].Verdict != "unavailable" || fhir[0].Line != "2.1" {
			t.Fatalf("want one unavailable finding at 2.1, got %+v", fhir)
		}
	})
}

// Only a body that is not there is "nothing to judge": the decision is the
// answer's own structure, never a validator's reply. A non-empty answer whose
// family lines' validators all fail is unavailable, refused 500 at strict.
func TestDTRIngress_ANonEmptyAnswerWithNoVerdictIsUnavailable(t *testing.T) {
	answer := e08Answer("PackageBundle", nil, true)
	r := e08RowWith(t, EnforcementStrict, declares22, answer, func(m map[string]shnsdk.Validator) {
		m["2.0"], m["2.1"] = failingValidator{}, failingValidator{}
	})
	if r.rec.Code != http.StatusInternalServerError || !bytes.Contains(r.rec.Body.Bytes(), []byte("validator unavailable")) {
		t.Fatalf("answer %d %s, want 500 validator unavailable", r.rec.Code, r.rec.Body.String())
	}
}
