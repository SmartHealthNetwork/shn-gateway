package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
)

// Per-level rows for a dependent's PAS answer: the Coverage it carries names
// the parent as a contained Patient (answer_party_test.go). At every level the
// answer is read and bound, relayed exactly, and written from as any readable
// answer is, at the payer's egress and at the provider's ingress, for $submit
// and $inquire. Each refusal row is that answer with one mutation, judged by
// patient.answer exactly as another patient's answer is (refused at strict,
// relayed and recorded below).

// dependentRequestOf returns a PAS request Bundle whose Coverage entry is a
// dependent's: the parent contained, named by the subscriber.
func dependentRequestOf(t *testing.T, bundle []byte) []byte {
	t.Helper()
	return dependentAnswerOf(t, bundle, nil)
}

// dependentRefusalNames are the refusal rows the subject rule decides (the
// graph closes), in a stable order.
func dependentRefusalNames(submit bool) []string {
	var out []string
	for name, row := range dependentAnswerRefusals {
		if submit && row.shape {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// withCoverageEntry adds to a synthetic answer (fixturePASResponse) the
// Coverage entry a payer's answer carries for its patient.
func withCoverageEntry(t *testing.T, answer []byte) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(answer, &doc); err != nil {
		t.Fatal(err)
	}
	cr := answerEntryOf(t, doc, "ClaimResponse")
	doc["entry"] = append(doc["entry"].([]any), map[string]any{"fullUrl": "https://payer.test/fhir/Coverage/dep", "resource": map[string]any{
		"resourceType": "Coverage", "id": "dep", "status": "active", "beneficiary": cr["patient"], "payor": []any{map[string]any{"display": "Payer"}}}})
	out := mustJSON(t, doc)
	if _, bad := validateNativePASResponse(out); bad.Status != 0 || pasResponseSubjectMismatch(out) != nil {
		t.Fatalf("fixture: the answer with its Coverage must be readable and bound: %+v", bad)
	}
	return out
}

// ---- payer egress ----

func TestLevelPayerPASSubmit_DependentAnswerRelayed(t *testing.T) {
	answer := dependentAnswerOf(t, []byte(assemblyRealPending), nil)
	approved := dependentAnswerOf(t, withCoverageEntry(t, fixturePASResponse(t, approvedClaimResponse(nil), true)), nil)
	request := dependentRequestOf(t, originatorBuiltConformantBundle(t, "MBR-COVERED"))
	for _, level := range allLevels {
		t.Run("pended/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.partner.respByPath[pasSubmitPath] = answer
			got := p.send(t, "pas-claim", "", request)
			if got.status != http.StatusOK || !got.framed || !bytes.Equal(got.body, answer) {
				t.Fatalf("the payer's answer must be relayed exactly: %d %s", got.status, got.body)
			}
			if !bytes.Equal(p.partner.lastBody, request) {
				t.Fatalf("the participant's system must receive the dependent's request as sent:\n%s", p.partner.lastBody)
			}
			if rec, found := p.pendOf(t, got.corr); !found || rec.State != PendStatePended {
				t.Fatalf("the pend must be recorded, got found=%v %+v", found, rec)
			}
			if len(p.skipped) != 0 || len(p.content()) != 0 {
				t.Fatalf("a readable answer records and skips nothing: %+v %+v", findingsText(p.content()), p.skipped)
			}
		})
		t.Run("decided/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.partner.respByPath[pasSubmitPath] = approved
			got := p.send(t, "pas-claim", "", request)
			if got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
				t.Fatalf("answer %d %s", got.status, got.body)
			}
			if rec, found := p.pendOf(t, got.corr); !found || rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved || p.eobCount() != 1 {
				t.Fatalf("the decision and its EOB must be written, got found=%v %+v, %d EOB(s)", found, rec, p.eobCount())
			}
			if len(p.skipped) != 0 || len(p.content()) != 0 {
				t.Fatalf("a readable answer records and skips nothing: %+v %+v", findingsText(p.content()), p.skipped)
			}
		})
	}
}

func TestLevelPayerPASSubmit_DependentAnswerPatientLinkage(t *testing.T) {
	request := dependentRequestOf(t, originatorBuiltConformantBundle(t, "MBR-COVERED"))
	for _, name := range dependentRefusalNames(true) {
		answer := dependentAnswerOf(t, []byte(assemblyRealPending), dependentAnswerRefusals[name].edit)
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.partner.respByPath[pasSubmitPath] = answer
				got := p.send(t, "pas-claim", "", request)
				p.wantAnswerRow(t, "pas-claim", got, pasSubmitPath, answer, RulePatientAnswer, http.StatusForbidden, "PAS response has inconsistent patient linkage")
				p.wantSkipped(t, "pas-claim", got.corr, RulePatientAnswer)
				if _, found := p.pendOf(t, got.corr); found || p.eobCount() != 0 {
					t.Fatalf("nothing may be written from this answer: pend found=%v, %d EOB(s)", found, p.eobCount())
				}
			})
		}
	}
}

func TestLevelPayerInquire_DependentAnswerDecides(t *testing.T) {
	answer := dependentAnswerOf(t, decidedAnswer(t), nil)
	request := dependentRequestOf(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.seedInquiryPend(t, "corr-submit-1")
			p.partner.respByPath[pasInquirePath] = answer
			got := p.send(t, "pas-claim-inquire", "", request)
			if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
				t.Fatalf("answer %d %s", got.status, got.body)
			}
			if rec, _ := p.pendOf(t, "corr-submit-1"); rec.State != PendStateDecided || p.eobCount() != 1 || len(p.skipped) != 0 || len(p.content()) != 0 {
				t.Fatalf("a readable answer decides: %+v, %d EOB(s), skipped %+v, findings %v", rec, p.eobCount(), p.skipped, findingsText(p.content()))
			}
		})
	}
}

func TestLevelPayerInquire_DependentAnswerPatientLinkage(t *testing.T) {
	request := dependentRequestOf(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	for _, name := range dependentRefusalNames(false) {
		answer := dependentAnswerOf(t, decidedAnswer(t), dependentAnswerRefusals[name].edit)
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.seedInquiryPend(t, "corr-submit-1")
				p.partner.respByPath[pasInquirePath] = answer
				got := p.send(t, "pas-claim-inquire", "", request)
				p.wantAnswerRow(t, "pas-claim-inquire", got, pasInquirePath, answer, RulePatientAnswer, http.StatusForbidden, "PAS inquiry answer has inconsistent patient linkage")
				p.wantSkipped(t, "pas-claim-inquire", got.corr, RulePatientAnswer)
				if rec, found := p.pendOf(t, "corr-submit-1"); !found || rec.State != PendStatePended || p.eobCount() != 0 {
					t.Fatalf("nothing may be written from this answer: %+v, %d EOB(s)", rec, p.eobCount())
				}
			})
		}
	}
}

// ---- provider ingress ----

// levelPASDependentIngress is the provider's PAS ingress request, its
// Coverage a dependent's.
func levelPASDependentIngress(t *testing.T) string {
	t.Helper()
	return string(dependentRequestOf(t, []byte(pasIngressBundle("00001", ""))))
}

func TestLevelPASIngress_DependentAnswerRecordsItsDecision(t *testing.T) {
	answer := dependentAnswerOf(t, []byte(assemblyRealPending), nil)
	body := levelPASDependentIngress(t)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelPASRow(t, level, body, answer)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			wantCarriedExactly(t, env, body)
			if o := lastLegOutcome(t, env.originator); o != "pended" {
				t.Fatalf("the leg outcome is read from the decision, got %q", o)
			}
			if len(ev.findings) != 0 || len(ev.skipped) != 0 {
				t.Fatalf("a readable answer records nothing and skips nothing: %+v %+v", ev.findings, ev.skipped)
			}
		})
	}
}

func TestLevelPASIngress_DependentAnswerPatientLinkage(t *testing.T) {
	body := levelPASDependentIngress(t)
	for _, name := range dependentRefusalNames(true) {
		answer := dependentAnswerOf(t, []byte(assemblyRealPending), dependentAnswerRefusals[name].edit)
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelPASRow(t, level, body, answer)
				wantAnswerLevelOutcome(t, "pas-claim", level, env, rec, ev, answer, RulePatientAnswer, http.StatusBadGateway, "PAS response has inconsistent patient linkage")
			})
		}
	}
}

// levelInquiryDependent is the provider's inquiry, its Coverage a
// dependent's.
func levelInquiryDependent(t *testing.T) string {
	t.Helper()
	return string(dependentRequestOf(t, []byte(levelInquiry(t, "MBR-COVERED", ""))))
}

func TestLevelInquireIngress_DependentAnswerRecordedOK(t *testing.T) {
	answer := dependentAnswerOf(t, levelInquiryPayerAnswer(t), nil)
	body := levelInquiryDependent(t)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelInquireRow(t, level, body, answer)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if o := lastLegOutcome(t, env.originator); o != "ok" {
				t.Fatalf("a readable answer is recorded ok, got %q", o)
			}
			if len(ev.findings) != 0 || len(ev.skipped) != 0 {
				t.Fatalf("a readable answer records nothing and skips nothing: %+v %+v", ev.findings, ev.skipped)
			}
		})
	}
}

func TestLevelInquireIngress_DependentAnswerPatientLinkage(t *testing.T) {
	body := levelInquiryDependent(t)
	for _, name := range dependentRefusalNames(false) {
		answer := dependentAnswerOf(t, levelInquiryPayerAnswer(t), dependentAnswerRefusals[name].edit)
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelInquireRow(t, level, body, answer)
				wantAnswerLevelOutcome(t, "pas-claim-inquire", level, env, rec, ev, answer, RulePatientAnswer, http.StatusBadGateway, "PAS inquiry answer has inconsistent patient linkage")
			})
		}
	}
}

// The dependent request fixtures are a dependent's: the parent rides in the
// Coverage, named by its subscriber.
func TestLevelDependentFixtures(t *testing.T) {
	for name, body := range map[string][]byte{
		"payer submit":     dependentRequestOf(t, originatorBuiltConformantBundle(t, "MBR-COVERED")),
		"payer inquiry":    dependentRequestOf(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148")),
		"provider submit":  []byte(levelPASDependentIngress(t)),
		"provider inquiry": []byte(levelInquiryDependent(t)),
	} {
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		cov := answerEntryOf(t, doc, "Coverage")
		if sub, _ := cov["subscriber"].(map[string]any); sub["reference"] != "#parent" || len(cov["contained"].([]any)) != 1 {
			t.Fatalf("%s: the Coverage is not a dependent's: %v", name, cov)
		}
	}
}
