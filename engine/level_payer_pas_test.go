package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Per-level rows for the payer-side PAS legs ($submit, the amended re-POST
// and $inquire): the request's own content, the payer's answer, and the local
// write a payer gateway skips when it relays an answer it did not read.

// ---- the local record ----

// pendOf reads this gateway's ledger row for (pci, corr).
func (p *levelPayer) pendOf(t *testing.T, corr string) (PendRecord, bool) {
	t.Helper()
	rec, found, err := p.store.PendRecordOf(p.pci, corr)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return rec, found
}

// eobCount is how many EOBs this gateway holds for the covered member.
func (p *levelPayer) eobCount() int {
	eobs, _ := p.store.EOBsForPatient(p.pci)
	return len(eobs)
}

// wantSkipped asserts the local-write-skipped record for a row: one event on
// leg naming rule and carrying no payload below strict, none at strict.
func (p *levelPayer) wantSkipped(t *testing.T, leg, corr, rule string) {
	t.Helper()
	if p.level == EnforcementStrict {
		if len(p.skipped) != 0 {
			t.Fatalf("a refused answer skips nothing, got %+v", p.skipped)
		}
		return
	}
	if len(p.skipped) != 1 {
		t.Fatalf("want one %s event, got %+v", LocalWriteSkippedEvent, p.skipped)
	}
	e := p.skipped[0]
	if e.LegType != leg || e.CorrelationID != corr || e.Direction != "ingress" || !strings.Contains(e.Detail, rule) || len(e.Payload) != 0 {
		t.Fatalf("want a %s event on %s (%s) naming %s with no payload, got %+v", LocalWriteSkippedEvent, leg, corr, rule, e)
	}
}

// ---- $submit: the request ----

// The bundle's own shape and consistency. The subject is
// Claim.patient.
func TestLevelPayerPASSubmit_RequestContent(t *testing.T) {
	base := pasIngressBundle("00001", "")
	rows := map[string]struct {
		body   string
		rule   string
		status int
		msg    string
	}{
		"no order": {levelPASBundleWithout(t, `{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}},`),
			RuleRequestShape, http.StatusBadRequest, "PAS bundle missing order (ServiceRequest or DeviceRequest)"},
		"Coverage without a beneficiary": {levelPASBundleWithout(t, `"beneficiary":{"reference":"Patient/MBR-COVERED"},`),
			RuleRequestShape, http.StatusBadRequest, "PAS bundle missing Coverage.beneficiary"},
		"order for another patient": {strings.Replace(base, `"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}`, `"ServiceRequest","subject":{"reference":"Patient/MBR-UC04"}`, 1),
			RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS bundle"},
		"QuestionnaireResponse for another patient": {levelPASBundleWithEntry(`{"resource":{"resourceType":"QuestionnaireResponse","status":"completed","subject":{"reference":"Patient/MBR-UC04"}}}`),
			RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS bundle"},
		"DiagnosticReport for another patient": {levelPASBundleWithEntry(`{"resource":{"resourceType":"DiagnosticReport","status":"final","subject":{"reference":"Patient/MBR-UC04"}}}`),
			RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS bundle"},
		"QuestionnaireResponse naming no patient": {levelPASBundleWithEntry(`{"resource":{"resourceType":"QuestionnaireResponse","status":"completed"}}`),
			RulePatientMixed, http.StatusForbidden, "PAS bundle QuestionnaireResponse missing subject"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "pas-claim", "", []byte(row.body))
				p.wantRequestRow(t, "pas-claim", got, []byte(row.body), pasSubmitPath, row.rule, row.status, row.msg)
			})
		}
	}
}

// A clinician-sourced QuestionnaireResponse item without its FR-16/FR-17
// attestation, on each PAS leg handleInbound fences.
func TestLevelPayerPAS_UnattestedItem(t *testing.T) {
	item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", shnsdk.Attestation{NPI: "1999999999", Text: "I attest these are my clinical findings.", When: "2026-06-04"})
	if err != nil {
		t.Fatal(err)
	}
	qr := `{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"},"item":[` + string(stripItemExtension(t, item)) + `]}}`
	rows := map[string]struct {
		leg, path string
		body      []byte
	}{
		"pas-claim":         {"pas-claim", pasSubmitPath, []byte(levelPASBundleWithEntry(qr))},
		"pas-claim-inquire": {"pas-claim-inquire", pasInquirePath, bytes.Replace(inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), []byte(`"entry":[`), []byte(`"entry":[`+qr+`,`), 1)},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, row.leg, "", row.body)
				p.wantRequestRow(t, row.leg, got, row.body, row.path, RuleAttestation, http.StatusForbidden, "is clinician-sourced (FR-17)")
			})
		}
	}
}

// ---- the amended re-POST ----

// updateBundle is the conformant amended re-POST with the entries of the
// named resource types removed, and the prior authorization it amends.
func updateBundle(t *testing.T, removeTypes ...string) ([]byte, string) {
	t.Helper()
	raw := originatorBuiltConformantUpdateBundle(t)
	f, status, msg := parseConformantPASUpdateFacts(raw)
	if status != 0 || f.relatedClaim == "" {
		t.Fatalf("fixture: %d %s, related %q", status, msg, f.relatedClaim)
	}
	if len(removeTypes) == 0 {
		return raw, f.relatedClaim
	}
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	var kept []any
	for _, e := range b["entry"].([]any) {
		rt := e.(map[string]any)["resource"].(map[string]any)["resourceType"]
		drop := false
		for _, want := range removeTypes {
			drop = drop || rt == want
		}
		if !drop {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(b["entry"].([]any)) {
		t.Fatalf("fixture: nothing of %v removed", removeTypes)
	}
	b["entry"] = kept
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out, f.relatedClaim
}

// seedPend records the prior authorization an amendment binds to.
func (p *levelPayer) seedPend(t *testing.T, corr string) {
	t.Helper()
	if err := p.store.RecordPendedClaim(p.pci, corr); err != nil {
		t.Fatal(err)
	}
}

// The FR-32 attribution of the amendment's supplemental evidence.
func TestLevelPayerPASUpdate_Provenance(t *testing.T) {
	missing, related := updateBundle(t, "Provenance")
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.seedPend(t, related)
			got := p.send(t, "pas-claim-update", "", missing)
			p.wantRequestRow(t, "pas-claim-update", got, missing, pasSubmitPath, RuleUpdateProvenance, http.StatusForbidden, "ClaimUpdate missing Provenance")
		})
	}
}

// updateBundleSetting is the conformant amended re-POST with key set to value
// on its first entry of resource type rt.
func updateBundleSetting(t *testing.T, rt, key string, value any) []byte {
	t.Helper()
	raw, _ := updateBundle(t)
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	for _, e := range b["entry"].([]any) {
		if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == rt {
			r[key] = value
			return mustJSON(t, b)
		}
	}
	t.Fatalf("fixture: the amendment carries no %s", rt)
	return nil
}

// A value of the wrong type in the amendment's entries, off what the subject
// bind and the leg read (resourceType, Claim.related), is the update's own
// shape: below strict it is forwarded as sent and the FR-32 check reads what
// fit; strict refuses with the status and body it has always given.
func TestLevelPayerPASUpdate_WrongTypedContent(t *testing.T) {
	_, related := updateBundle(t)
	rows := map[string]struct {
		body []byte
		msg  string
	}{
		"Provenance.policy":        {updateBundleSetting(t, "Provenance", "policy", "x"), "parse update Provenance entry failed"},
		"QuestionnaireResponse.id": {updateBundleSetting(t, "QuestionnaireResponse", "id", 7), "parse update QR entry failed"},
		"Claim.identifier":         {updateBundleSetting(t, "Claim", "identifier", 5), "parse update Claim entry failed"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.seedPend(t, related)
				got := p.send(t, "pas-claim-update", "", row.body)
				p.wantRequestRow(t, "pas-claim-update", got, row.body, pasSubmitPath, RuleRequestShape, http.StatusBadRequest, row.msg)
			})
		}
	}
}

// An inquiry entry's id of the wrong type is the inquiry's own shape on the
// payer side too; a Patient entry's own id is a patient identity and must
// read at every level.
func TestLevelPayerPASInquire_WrongTypedEntryID(t *testing.T) {
	base := inquiryBundle("MBR-COVERED", "", "TRN-1", "72148")
	coverageID := bytes.Replace(base, []byte(`"resourceType":"Coverage",`), []byte(`"resourceType":"Coverage","id":7,`), 1)
	if bytes.Equal(coverageID, base) {
		t.Fatal("fixture: the Coverage gained no id")
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			got := p.send(t, "pas-claim-inquire", "", coverageID)
			p.wantRequestRow(t, "pas-claim-inquire", got, coverageID, pasInquirePath, RuleRequestShape, http.StatusBadRequest, "parse inquiry bundle entry failed")
		})
	}
	patientID := bytes.Replace(base, []byte(`"resourceType":"Patient","id":"MBR-COVERED"`), []byte(`"resourceType":"Patient","id":7`), 1)
	if bytes.Equal(patientID, base) {
		t.Fatal("fixture: the Patient id was not replaced")
	}
	runPayerNetworkRows(t, map[string]payerNetworkRow{
		"Patient id of the wrong type": {leg: "pas-claim-inquire", body: patientID,
			status: http.StatusBadRequest, msg: "parse inquiry bundle entry failed"},
	})
}

// ---- $submit and the amended re-POST: the payer's answer ----

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pasAnswerRows are the answers a payer gateway cannot read or state, with
// the refusal strict gives them.
func pasAnswerRows(t *testing.T) map[string]struct {
	answer []byte
	rule   string
	status int
	msg    string
} {
	t.Helper()
	otherPatient := strings.Replace(assemblyRealPending, `"entry":[`, `"entry":[{"fullUrl":"http://localhost:8081/fhir/Patient/Other","resource":{"resourceType":"Patient","id":"Other"}},`, 1)
	denialWithNumber := fixturePASResponse(t, withPreAuthRef(deniedClaimResponse(reviewActionExt("A3", "Not Certified", "", ""), nil), "PA-PARTIAL"), true)
	uncarriable := fixturePASResponse(t, deniedClaimResponse(reviewActionExt("A3", "Not Certified", "http://example.org/reasons", "9"), nil), true)
	return map[string]struct {
		answer []byte
		rule   string
		status int
		msg    string
	}{
		"graph":              {[]byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`), RuleAnswerShape, http.StatusBadGateway, "invalid native PAS response Bundle"},
		"decision":           {[]byte(strings.Replace(assemblyRealPending, `"outcome":"queued"`, `"outcome":7`, 1)), RuleAnswerShape, http.StatusBadGateway, "invalid native PAS response decision"},
		"denial with number": {denialWithNumber, RuleEOBDecision, http.StatusBadGateway, "payer decision states both a denial and an authorization number"},
		"uncarriable detail": {uncarriable, RuleEOBDecision, http.StatusBadGateway, "payer decision detail cannot be stated on a decision EOB"},
		"patient linkage":    {[]byte(otherPatient), RulePatientAnswer, http.StatusForbidden, "PAS response has inconsistent patient linkage"},
	}
}

// On $submit: below strict the payer's answer is
// relayed exactly and nothing is written from it; strict refuses as before.
func TestLevelPayerPASSubmit_AnswerContent(t *testing.T) {
	request := originatorBuiltConformantBundle(t, "MBR-COVERED")
	for name, row := range pasAnswerRows(t) {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.partner.respByPath[pasSubmitPath] = row.answer
				got := p.send(t, "pas-claim", "", request)
				p.wantAnswerRow(t, "pas-claim", got, pasSubmitPath, row.answer, row.rule, row.status, row.msg)
				p.wantSkipped(t, "pas-claim", got.corr, row.rule)
				if _, found := p.pendOf(t, got.corr); found || p.eobCount() != 0 {
					t.Fatalf("nothing may be written from this answer: pend found=%v, %d EOB(s)", found, p.eobCount())
				}
			})
		}
	}
}

// On the amended re-POST: the amendment's claim is released,
// so the prior authorization stays pended exactly as before the amendment.
func TestLevelPayerPASUpdate_AnswerContent(t *testing.T) {
	request, related := updateBundle(t)
	rows := pasAnswerRows(t)
	for _, name := range []string{"graph", "denial with number", "patient linkage"} {
		row := rows[name]
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.seedPend(t, related)
				p.partner.respByPath[pasSubmitPath] = row.answer
				got := p.send(t, "pas-claim-update", "", request)
				p.wantAnswerRow(t, "pas-claim-update", got, pasSubmitPath, row.answer, row.rule, row.status, row.msg)
				p.wantSkipped(t, "pas-claim-update", got.corr, row.rule)
				rec, found := p.pendOf(t, related)
				if !found || rec.State != PendStatePended || rec.Outcome != "" {
					t.Fatalf("the prior authorization must stay pended and released, got found=%v %+v", found, rec)
				}
				if claimed, why, err := p.store.BeginClaimUpdateReason(p.pci, related); err != nil || !claimed {
					t.Fatalf("the amendment's claim was not released: claimed=%v why=%v err=%v", claimed, why, err)
				}
			})
		}
	}
}

// A DECIDED answer relayed unread below strict writes nothing: the answer
// approves, and read it would decide the authorization and write its EOB
// (TestLevelPayerPAS_ReadableAnswerIsWritten), but it also carries another
// patient's record, so below strict the payer's bytes are relayed exactly and
// no decision or EOB is written. On the amended re-POST the seeded prior
// authorization stays pended. A later amended re-POST of a $submit whose
// answer was relayed unread finds no pend (409), since none was written.
func TestLevelPayerPAS_DecidedAnswerRelayedUnreadWritesNothing(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	foreign := bytes.Replace(approved, []byte(`"entry":[`), []byte(`"entry":[{"fullUrl":"http://localhost:8081/fhir/Patient/Other","resource":{"resourceType":"Patient","id":"Other"}},`), 1)
	if bytes.Equal(foreign, approved) {
		t.Fatal("fixture: no foreign Patient entry added")
	}
	if pasResponseSubjectMismatch(foreign) == nil {
		t.Fatal("fixture: the answer's subjects must not bind")
	}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
		t.Run("submit/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.partner.respByPath[pasSubmitPath] = foreign
			got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			if got.status != http.StatusOK || !bytes.Equal(got.body, foreign) {
				t.Fatalf("the payer's answer must be relayed exactly: %d %s", got.status, got.body)
			}
			p.wantSkipped(t, "pas-claim", got.corr, RulePatientAnswer)
			if rec, found := p.pendOf(t, got.corr); found || p.eobCount() != 0 {
				t.Fatalf("no decision or EOB may be written: found=%v %+v, %d EOB(s)", found, rec, p.eobCount())
			}
			request, related := updateBundle(t)
			update := p.send(t, "pas-claim-update", "", request)
			if update.status != http.StatusConflict || !strings.Contains(string(update.body), "ClaimUpdate references no pending claim") {
				t.Fatalf("an amendment of an exchange whose answer was relayed unread finds no pend: %d %s (related %s)", update.status, update.body, related)
			}
		})
		t.Run("update/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			request, related := updateBundle(t)
			p.seedPend(t, related)
			p.partner.respByPath[pasSubmitPath] = foreign
			got := p.send(t, "pas-claim-update", "", request)
			if got.status != http.StatusOK || !bytes.Equal(got.body, foreign) {
				t.Fatalf("the payer's answer must be relayed exactly: %d %s", got.status, got.body)
			}
			p.wantSkipped(t, "pas-claim-update", got.corr, RulePatientAnswer)
			if rec, found := p.pendOf(t, related); !found || rec.State != PendStatePended || rec.Outcome != "" || p.eobCount() != 0 {
				t.Fatalf("the prior authorization must stay pended with no EOB: found=%v %+v, %d EOB(s)", found, rec, p.eobCount())
			}
		})
	}
}

// A readable, bound answer is written as before at every level, and nothing
// is skipped: the rows above are not vacuous.
func TestLevelPayerPAS_ReadableAnswerIsWritten(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	for _, level := range allLevels {
		t.Run("submit pended/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			if got.status != http.StatusOK || !bytes.Equal(got.body, []byte(assemblyRealPending)) {
				t.Fatalf("answer %d %s", got.status, got.body)
			}
			if rec, found := p.pendOf(t, got.corr); !found || rec.State != PendStatePended {
				t.Fatalf("the pend must be recorded, got found=%v %+v", found, rec)
			}
			if len(p.skipped) != 0 || len(p.content()) != 0 {
				t.Fatalf("a readable answer records and skips nothing: %+v %+v", p.content(), p.skipped)
			}
		})
		t.Run("submit decided/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.partner.respByPath[pasSubmitPath] = approved
			got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			if got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
				t.Fatalf("answer %d %s", got.status, got.body)
			}
			if rec, found := p.pendOf(t, got.corr); !found || rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved || p.eobCount() != 1 {
				t.Fatalf("the decision and its EOB must be written, got found=%v %+v, %d EOB(s)", found, rec, p.eobCount())
			}
			if len(p.skipped) != 0 {
				t.Fatalf("a readable answer skips nothing: %+v", p.skipped)
			}
		})
		t.Run("update decided/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			request, related := updateBundle(t)
			p.seedPend(t, related)
			p.partner.respByPath[pasSubmitPath] = approved
			if got := p.send(t, "pas-claim-update", "", request); got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
				t.Fatalf("answer %d %s", got.status, got.body)
			}
			if rec, found := p.pendOf(t, related); !found || rec.State != PendStateDecided {
				t.Fatalf("the amendment's decision must be written, got found=%v %+v", found, rec)
			}
		})
	}
}

// The decision EOB this gateway builds from a relayed answer fails
// validation. Strict holds the answer back (422); observe records the EOB's
// finding, relays the payer's answer and writes nothing; none checks nothing
// and writes the decision as it always has.
func TestLevelPayerPASSubmit_InvalidEOB(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.g.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: `"ExplanationOfBenefit"`}
			p.partner.respByPath[pasSubmitPath] = approved
			got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			var eobFindings []ConformanceFinding
			for _, f := range p.findings {
				if f.Kind == string(KindFHIREgress) {
					eobFindings = append(eobFindings, f)
				}
			}
			rec, found := p.pendOf(t, got.corr)
			switch level {
			case EnforcementStrict:
				if got.status != http.StatusUnprocessableEntity || !strings.Contains(string(got.body), "egress validation failed") {
					t.Fatalf("answer %d %s, want the 422", got.status, got.body)
				}
				if len(eobFindings) != 1 || eobFindings[0].Decision != "refused" {
					t.Fatalf("want one refused EOB finding, got %+v", eobFindings)
				}
				if found || p.eobCount() != 0 {
					t.Fatal("a refused answer writes nothing")
				}
			case EnforcementObserve:
				if got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
					t.Fatalf("the payer's answer must be relayed exactly: %d %s", got.status, got.body)
				}
				if len(eobFindings) != 1 || eobFindings[0].Decision != "relayed" || eobFindings[0].LegType != "pas-claim" || eobFindings[0].Whose != "own" {
					t.Fatalf("want one relayed EOB finding on pas-claim, got %+v", eobFindings)
				}
				if found || p.eobCount() != 0 {
					t.Fatalf("nothing may be written from an answer whose EOB failed: pend found=%v, %d EOB(s)", found, p.eobCount())
				}
			case EnforcementNone:
				if got.status != http.StatusOK || !bytes.Equal(got.body, approved) || len(eobFindings) != 0 {
					t.Fatalf("at none nothing is checked: %d %s, findings %+v", got.status, got.body, eobFindings)
				}
				if !found || rec.State != PendStateDecided || p.eobCount() != 1 {
					t.Fatalf("at none the decision and its EOB are written as before, got found=%v %+v, %d EOB(s)", found, rec, p.eobCount())
				}
				if len(p.skipped) != 0 {
					t.Fatalf("at none nothing is skipped: %+v", p.skipped)
				}
				return
			}
			p.wantSkipped(t, "pas-claim", got.corr, RuleEOBDecision)
		})
	}
}

// The validator cannot answer for the decision EOB this gateway builds. At
// observe a check that could not run is recorded as unavailable and does not
// gate, as everywhere else: the payer's answer is relayed and the decision and
// its EOB are written, as at none. Strict answers 500, as it always has.
func TestLevelPayerPASSubmit_EOBValidatorOutage(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.g.cfg.Validator = validatorFunc(func(b []byte) (shnsdk.Result, error) {
				if bytes.Contains(b, []byte(`"ExplanationOfBenefit"`)) {
					return shnsdk.Result{}, context.DeadlineExceeded
				}
				return shnsdk.Result{Valid: true}, nil
			})
			p.partner.respByPath[pasSubmitPath] = approved
			got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			var eobFindings []ConformanceFinding
			for _, f := range p.findings {
				if f.Kind == string(KindFHIREgress) {
					eobFindings = append(eobFindings, f)
				}
			}
			rec, found := p.pendOf(t, got.corr)
			if level == EnforcementStrict {
				if got.status != http.StatusInternalServerError || !strings.Contains(string(got.body), "validator unavailable") {
					t.Fatalf("answer %d %s, want the 500", got.status, got.body)
				}
				if found || p.eobCount() != 0 {
					t.Fatal("a refused answer writes nothing")
				}
				return
			}
			if got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
				t.Fatalf("the payer's answer must be relayed exactly: %d %s", got.status, got.body)
			}
			if len(eobFindings) != 1 || eobFindings[0].Verdict != "unavailable" || eobFindings[0].Decision != "relayed" {
				t.Fatalf("want one unavailable EOB finding, got %+v", eobFindings)
			}
			if !found || rec.State != PendStateDecided || p.eobCount() != 1 || len(p.skipped) != 0 {
				t.Fatalf("the decision and its EOB are written: found=%v %+v, %d EOB(s), skipped %+v", found, rec, p.eobCount(), p.skipped)
			}
		})
	}
}

// ---- $inquire ----

func inquiryWith(t *testing.T, old, new string) []byte {
	t.Helper()
	body := inquiryBundle("MBR-COVERED", "", "TRN-1", "72148")
	out := bytes.Replace(body, []byte(old), []byte(new), 1)
	if bytes.Equal(out, body) {
		t.Fatalf("fixture: %q not replaced", old)
	}
	return out
}

// The inquiry's own shape and consistency. The subject is
// Claim.patient.
func TestLevelPayerInquire_RequestContent(t *testing.T) {
	rows := map[string]struct {
		body   []byte
		rule   string
		status int
		msg    string
	}{
		"not a collection":     {inquiryWith(t, `"type":"collection"`, `"type":"searchset"`), RuleRequestShape, http.StatusBadRequest, "PAS inquiry Bundle is not a collection"},
		"entry request":        {inquiryWith(t, `{"fullUrl":"urn:uuid:patient",`, `{"fullUrl":"urn:uuid:patient","request":{"method":"GET","url":"Patient/MBR-COVERED"},`), RuleRequestShape, http.StatusBadRequest, "PAS inquiry Bundle entries carry no request or response"},
		"not preauthorization": {inquiryWith(t, `"use":"preauthorization"`, `"use":"claim"`), RuleRequestShape, http.StatusBadRequest, "PAS inquiry Claim is not a preauthorization"},
		"another patient":      {inquiryBundle("MBR-COVERED", "MBR-UC04", "TRN-1", "72148"), RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS inquiry"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "pas-claim-inquire", "", row.body)
				p.wantRequestRow(t, "pas-claim-inquire", got, row.body, pasInquirePath, row.rule, row.status, row.msg)
			})
		}
	}
}

// seedInquiryPend records the authorization decidedAnswer names, under this
// payer's requester, so an inquiry answer read for the ledger decides it.
func (p *levelPayer) seedInquiryPend(t *testing.T, corr string) {
	t.Helper()
	if _, err := p.store.RecordPendedKeyed(p.pci, corr, fixedClock(), PendKeys{RequesterHolder: p.requester.ID, ClaimResponseIDs: []string{inquiryCRKey}}); err != nil {
		t.Fatal(err)
	}
}

// An inquiry answer this gateway cannot read, whose subjects do
// not bind, or whose decision EOB fails validation. Each answer decides the
// seeded authorization if read, so an unchanged ledger below strict is the
// skip, not an answer that decided nothing.
func TestLevelPayerInquire_AnswerContent(t *testing.T) {
	decided := decidedAnswer(t)
	var b map[string]any
	if err := json.Unmarshal(decided, &b); err != nil {
		t.Fatal(err)
	}
	b["entry"] = append(b["entry"].([]any), map[string]any{"resource": map[string]any{"resourceType": "Patient", "id": "OTHER-PATIENT"}})
	twoPatients := mustJSON(t, b)
	if validatePASInquiryAnswer(twoPatients).Status != 0 || consistentPASInquiryAnswerSubjects(twoPatients) {
		t.Fatal("fixture: want a readable answer naming two patients")
	}
	rows := map[string]struct {
		answer    []byte
		rule      string
		status    int
		msg       string
		validator *shnsdk.FakeValidator
	}{
		"unreadable answer": {[]byte(`{"resourceType":"Patient","id":"x"}`), RuleAnswerShape, http.StatusBadGateway, "invalid prior-authorization inquiry answer", nil},
		"patient linkage":   {twoPatients, RulePatientAnswer, http.StatusForbidden, "PAS inquiry answer has inconsistent patient linkage", nil},
		"invalid EOB":       {decided, RuleEOBDecision, http.StatusUnprocessableEntity, "egress validation failed", &shnsdk.FakeValidator{RejectIfContains: `"ExplanationOfBenefit"`}},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				if row.validator != nil {
					p.g.cfg.Validator = row.validator
				}
				p.seedInquiryPend(t, "corr-submit-1")
				p.partner.respByPath[pasInquirePath] = row.answer
				got := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
				if row.rule == RuleEOBDecision {
					// The EOB's own validation finding is the record (fhir-egress).
					if level == EnforcementNone {
						if rec, _ := p.pendOf(t, "corr-submit-1"); got.status != http.StatusOK || rec.State != PendStateDecided || p.eobCount() != 1 {
							t.Fatalf("at none nothing is checked and the decision is written as before: %d, %+v, %d EOB(s)", got.status, rec, p.eobCount())
						}
						return
					}
					if level == EnforcementStrict {
						if got.status != row.status || !strings.Contains(string(got.body), row.msg) {
							t.Fatalf("answer %d %s", got.status, got.body)
						}
					} else if got.status != http.StatusOK || !bytes.Equal(got.body, row.answer) {
						t.Fatalf("the payer's answer must be relayed exactly: %d %s", got.status, got.body)
					}
				} else {
					p.wantAnswerRow(t, "pas-claim-inquire", got, pasInquirePath, row.answer, row.rule, row.status, row.msg)
				}
				p.wantSkipped(t, "pas-claim-inquire", got.corr, row.rule)
				if rec, found := p.pendOf(t, "corr-submit-1"); !found || rec.State != PendStatePended || p.eobCount() != 0 {
					t.Fatalf("nothing may be written from this answer: %+v, %d EOB(s)", rec, p.eobCount())
				}
			})
		}
	}
}

// A readable inquiry answer decides the seeded authorization at every level:
// the rows above are not vacuous.
func TestLevelPayerInquire_ReadableAnswerIsWritten(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.seedInquiryPend(t, "corr-submit-1")
			got := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
			if got.status != http.StatusOK || !bytes.Equal(got.body, decidedAnswer(t)) {
				t.Fatalf("answer %d %s", got.status, got.body)
			}
			if rec, _ := p.pendOf(t, "corr-submit-1"); rec.State != PendStateDecided || p.eobCount() != 1 || len(p.skipped) != 0 {
				t.Fatalf("a readable answer decides: %+v, %d EOB(s), skipped %+v", rec, p.eobCount(), p.skipped)
			}
		})
	}
}

// A later $inquire about an exchange whose $submit answer was relayed unread
// behaves sanely: it finds nothing in the ledger, relays the payer's answer,
// and records no decision — not from the skipped answer, and not from the
// inquiry's. The control shows the same inquiry deciding the authorization
// when the $submit answer was read.
func TestLevelPayerInquire_AfterSkippedSubmit(t *testing.T) {
	// The payer's inquiry answer decides the authorization its pended $submit
	// answer named: the same ClaimResponse identifier.
	// The pended answer's ClaimResponse identifier, read from the fixture.
	const traceSystem = `"system":"http://example.org/PATIENT_EVENT_TRACE_NUMBER","value":"`
	start := strings.Index(assemblyRealPending, traceSystem)
	if start < 0 {
		t.Fatal("fixture: the pended answer carries no trace identifier")
	}
	pendedTrace := assemblyRealPending[start+len(traceSystem):]
	pendedTrace = pendedTrace[:strings.IndexByte(pendedTrace, '"')]
	decided := bytes.Replace(decidedAnswer(t), []byte(`"value": "111099"`), []byte(`"value": "`+pendedTrace+`"`), 1)
	if bytes.Equal(decided, decidedAnswer(t)) {
		t.Fatal("fixture: the inquiry answer's identifier was not replaced")
	}
	otherPatient := []byte(strings.Replace(assemblyRealPending, `"entry":[`, `"entry":[{"fullUrl":"http://localhost:8081/fhir/Patient/Other","resource":{"resourceType":"Patient","id":"Other"}},`, 1))
	for _, tc := range []struct {
		name     string
		level    ConformanceEnforcement
		answer   []byte
		skipped  bool
		decision bool
	}{
		{"control: submit read", EnforcementObserve, []byte(assemblyRealPending), false, true},
		{"observe: submit relayed unread", EnforcementObserve, otherPatient, true, false},
		{"none: submit relayed unread", EnforcementNone, otherPatient, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newLevelPayer(t, tc.level)
			p.partner.respByPath[pasSubmitPath] = tc.answer
			submit := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			if submit.status != http.StatusOK || !bytes.Equal(submit.body, tc.answer) {
				t.Fatalf("submit answer %d %s", submit.status, submit.body)
			}
			if (len(p.skipped) == 1) != tc.skipped {
				t.Fatalf("submit skipped = %+v, want %v", p.skipped, tc.skipped)
			}
			p.partner.respByPath[pasInquirePath] = decided
			inquiry := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
			if inquiry.status != http.StatusOK || !bytes.Equal(inquiry.body, decided) {
				t.Fatalf("the later inquiry must relay the payer's answer: %d %s", inquiry.status, inquiry.body)
			}
			for _, e := range p.skipped {
				if e.LegType == "pas-claim-inquire" {
					t.Fatalf("the inquiry's own answer was read; nothing is skipped for it: %+v", e)
				}
			}
			rec, found := p.pendOf(t, submit.corr)
			if tc.decision {
				if !found || rec.State != PendStateDecided || p.eobCount() != 1 {
					t.Fatalf("control: the inquiry must decide the pend, got found=%v %+v, %d EOB(s)", found, rec, p.eobCount())
				}
				return
			}
			if found || p.eobCount() != 0 {
				t.Fatalf("nothing may be recorded for a skipped exchange: found=%v %+v, %d EOB(s)", found, rec, p.eobCount())
			}
		})
	}
}
