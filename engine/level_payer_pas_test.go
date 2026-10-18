package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
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
	p.wantSkippedWhen(t, leg, corr, rule, refusesAt(p.level, rule))
}

// wantSkippedWhen is wantSkipped for a row whose refusal the caller states:
// an answer refused by its own FHIR check rather than by rule's content table.
func (p *levelPayer) wantSkippedWhen(t *testing.T, leg, corr, rule string, refused bool) {
	t.Helper()
	if refused {
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

// unattestedItems are the two QuestionnaireResponse items the attestation
// fence judges, each without the attestation it requires: a clinician-sourced
// item (FR-16/FR-17) and a patient-reported one (FR-27), with the refusal
// each draws at strict.
func unattestedItems(t *testing.T) map[string]struct{ qr, msg string } {
	t.Helper()
	clinician, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", shnsdk.Attestation{NPI: "1999999999", Text: "I attest these are my clinical findings.", When: "2026-06-04"})
	if err != nil {
		t.Fatal(err)
	}
	patient, err := shnsdk.BuildPatientAttestedItem("functional-status-oswestry", "42", "Patient/MBR-COVERED", "2026-06-04")
	if err != nil {
		t.Fatal(err)
	}
	qr := func(item []byte) string {
		return `{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"},"item":[` + string(stripItemExtension(t, item)) + `]}}`
	}
	return map[string]struct{ qr, msg string }{
		"clinician-sourced": {qr(clinician), "is clinician-sourced (FR-17)"},
		"patient-reported":  {qr(patient), "is patient-reported (FR-17)"},
	}
}

// A QuestionnaireResponse item without the attestation its source requires,
// on each PAS leg handleInbound fences. The attestation is the item's own
// content (qr.attestation): not checked at none, recorded and carried at
// observe and structural, refused at strict.
func TestLevelPayerPAS_UnattestedItem(t *testing.T) {
	for kind, item := range unattestedItems(t) {
		rows := map[string]struct {
			leg, path string
			body      []byte
		}{
			"pas-claim":         {"pas-claim", pasSubmitPath, []byte(levelPASBundleWithEntry(item.qr))},
			"pas-claim-inquire": {"pas-claim-inquire", pasInquirePath, bytes.Replace(inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), []byte(`"entry":[`), []byte(`"entry":[`+item.qr+`,`), 1)},
		}
		for name, row := range rows {
			for _, level := range allLevels {
				t.Run(kind+"/"+name+"/"+level.String(), func(t *testing.T) {
					p := newLevelPayer(t, level)
					got := p.send(t, row.leg, "", row.body)
					p.wantRequestRow(t, row.leg, got, row.body, row.path, RuleAttestation, http.StatusForbidden, item.msg)
				})
			}
		}
	}
}

// The same fence on the amended re-POST: an amendment carrying an item
// without its attestation is carried below strict and refused at strict.
func TestLevelPayerPASUpdate_UnattestedItem(t *testing.T) {
	for kind, item := range unattestedItems(t) {
		base, related := updateBundle(t)
		body := bytes.Replace(base, []byte(`"entry":[`), []byte(`"entry":[`+item.qr+`,`), 1)
		if bytes.Equal(body, base) {
			t.Fatal("the amended re-POST has no entry list to add the item to")
		}
		for _, level := range allLevels {
			t.Run(kind+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.seedPend(t, related)
				got := p.send(t, "pas-claim-update", "", body)
				p.wantRequestRow(t, "pas-claim-update", got, body, pasSubmitPath, RuleAttestation, http.StatusForbidden, item.msg)
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
	return map[string]struct {
		answer []byte
		rule   string
		status int
		msg    string
	}{
		"graph":           {[]byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`), RuleAnswerShape, http.StatusBadGateway, "invalid native PAS response Bundle"},
		"decision":        {[]byte(strings.Replace(assemblyRealPending, `"outcome":"queued"`, `"outcome":7`, 1)), RuleAnswerShape, http.StatusBadGateway, "invalid native PAS response decision"},
		"patient linkage": {[]byte(otherPatient), RulePatientAnswer, http.StatusForbidden, "PAS response has inconsistent patient linkage"},
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

// TestLevelPayerPAS_DecisionTheSideRecordCannotHold: a payer
// decision this gateway's own record cannot hold is relayed exactly at every
// level, never refused, and the operator is told which detail. A decision read
// as a denial yet stating preAuthRef writes nothing (no decision, no EOB) on
// $submit and on the amended re-POST; a note typed outside NoteType records
// the decision with no EOB.
func TestLevelPayerPAS_DecisionTheSideRecordCannotHold(t *testing.T) {
	// Still open (the decision reader): the reference payer's own denial shape (A2
	// "Not Certified" on its submitted entry, testdata/brpayer-current/
	// known-member-graph2/repeat-submission.response.json) with a top-level
	// preAuthRef. The reader still takes A2 without a number as a denial, so
	// the gateway relays it and writes nothing, which is honest.
	denialWithNumber := fixturePASResponse(t, withPreAuthRef(deniedClaimResponse(reviewActionExt("A2", "Not Certified", "", ""), nil), "PA-PARTIAL"), true)
	uncarriable := fixturePASResponse(t, withNote(deniedClaimResponse(reviewActionExt("A3", "Not Certified", "", ""), nil), "email", "Reply by email."), true)
	request := originatorBuiltConformantBundle(t, "MBR-COVERED")
	updateReq, related := updateBundle(t)
	for _, level := range allLevels {
		t.Run("submit, denial with number/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.partner.respByPath[pasSubmitPath] = denialWithNumber
			got := p.send(t, "pas-claim", "", request)
			if got.status != http.StatusOK || !bytes.Equal(got.body, denialWithNumber) {
				t.Fatalf("the payer's decision must be relayed exactly, got %d %s", got.status, got.body)
			}
			p.wantSkippedWhen(t, "pas-claim", got.corr, RuleEOBDecision, false)
			p.wantSideRecord(t, "pas-claim", pendDecisionNotRecordedEvent, decisionReadAsDenialWithAuthNumber)
			if _, found := p.pendOf(t, got.corr); found || p.eobCount() != 0 {
				t.Fatalf("nothing may be written from this answer: pend found=%v, %d EOB(s)", found, p.eobCount())
			}
		})
		t.Run("update, denial with number/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.seedPend(t, related)
			p.partner.respByPath[pasSubmitPath] = denialWithNumber
			got := p.send(t, "pas-claim-update", "", updateReq)
			if got.status != http.StatusOK || !bytes.Equal(got.body, denialWithNumber) {
				t.Fatalf("the payer's decision must be relayed exactly, got %d %s", got.status, got.body)
			}
			p.wantSkippedWhen(t, "pas-claim-update", got.corr, RuleEOBDecision, false)
			p.wantSideRecord(t, "pas-claim-update", pendDecisionNotRecordedEvent, decisionReadAsDenialWithAuthNumber)
			if rec, found := p.pendOf(t, related); !found || rec.State != PendStatePended {
				t.Fatalf("the prior authorization must stay pended, got found=%v %+v", found, rec)
			}
		})
		t.Run("submit, note outside NoteType/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.partner.respByPath[pasSubmitPath] = uncarriable
			got := p.send(t, "pas-claim", "", request)
			if got.status != http.StatusOK || !bytes.Equal(got.body, uncarriable) {
				t.Fatalf("the payer's decision must be relayed exactly, got %d %s", got.status, got.body)
			}
			p.wantSideRecord(t, "pas-claim", DecisionEOBNotWrittenEvent, "decision EOB not written: process note 1 type is not display, print or printoper")
			if rec, found := p.pendOf(t, got.corr); !found || rec.State != PendStateDecided || p.eobCount() != 0 {
				t.Fatalf("the decision must be recorded with no EOB: found=%v %+v, %d EOB(s)", found, rec, p.eobCount())
			}
		})
	}
}

// wantSideRecord asserts exactly one event of kind on leg with detail.
func (p *levelPayer) wantSideRecord(t *testing.T, leg, kind, detail string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sideRecord) != 1 || p.sideRecord[0].Kind != kind || p.sideRecord[0].LegType != leg || p.sideRecord[0].Detail != detail {
		t.Fatalf("want one %s on %s naming %q, got %+v", kind, leg, detail, p.sideRecord)
	}
}

// On the amended re-POST: the amendment's claim is released,
// so the prior authorization stays pended exactly as before the amendment.
func TestLevelPayerPASUpdate_AnswerContent(t *testing.T) {
	request, related := updateBundle(t)
	rows := pasAnswerRows(t)
	for _, name := range []string{"graph", "patient linkage"} {
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
// answer was relayed unread finds no pend, since none was written, and reaches
// the payer all the same: the ledger records and never gates.
func TestLevelPayerPAS_DecidedAnswerRelayedUnreadWritesNothing(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	foreign := bytes.Replace(approved, []byte(`"entry":[`), []byte(`"entry":[{"fullUrl":"http://localhost:8081/fhir/Patient/Other","resource":{"resourceType":"Patient","id":"Other"}},`), 1)
	if bytes.Equal(foreign, approved) {
		t.Fatal("fixture: no foreign Patient entry added")
	}
	if pasResponseSubjectMismatch(foreign) == nil {
		t.Fatal("fixture: the answer's subjects must not bind")
	}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural} {
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
			if update.status != http.StatusOK || !bytes.Equal(update.body, foreign) {
				t.Fatalf("an amendment that finds no pend still reaches the payer and gets its answer: %d %s (related %s)", update.status, update.body, related)
			}
			if rec, found := p.pendOf(t, related); found {
				t.Fatalf("the amendment's unread answer wrote a pend: %+v", rec)
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

// A decision EOB that fails its own validation: decisioneob_gate_test.go
// (TestDecisionEOBGate_InvalidNeverWritten), for relayed and produced answers.

// Strict is not weakened by the rows above: on the same leg, a participant's
// own non-conformant payload (the payer's PAS answer, checked under the
// participant's opt-in) is still refused at strict, as it is today.
func TestLevelPayerPASSubmit_StrictStillRefusesThePayload(t *testing.T) {
	request := originatorBuiltConformantBundle(t, "MBR-COVERED")
	row := pasAnswerRows(t)["graph"]
	p := newLevelPayer(t, EnforcementStrict)
	p.partner.respByPath[pasSubmitPath] = row.answer
	got := p.send(t, "pas-claim", "", request)
	p.wantAnswerRow(t, "pas-claim", got, pasSubmitPath, row.answer, row.rule, row.status, row.msg)
	if got.status != row.status {
		t.Fatalf("strict must still refuse the participant's non-conformant answer with %d, got %d", row.status, got.status)
	}
}

// At structural the kind of FHIR issue decides. A decision EOB whose only
// issue is a deeper one (a code outside its code list) is recorded, not
// refused: the payer's answer is relayed exactly, and, as for any decision
// whose own EOB failed, the decision is recorded without the EOB.
func TestLevelPayerPASSubmit_DeeperEOBAtStructural(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	p := newLevelPayer(t, EnforcementStructural)
	p.g.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: `"ExplanationOfBenefit"`,
		RejectIssue: &shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "Terminology_TX_NoValid_1_CC"}}
	p.partner.respByPath[pasSubmitPath] = approved
	got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
	if got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
		t.Fatalf("a deeper EOB issue is recorded and the payer's answer relayed exactly: %d %s", got.status, got.body)
	}
	var eobFindings []ConformanceFinding
	for _, f := range p.findings {
		if f.Kind == string(KindFHIREgress) {
			eobFindings = append(eobFindings, f)
		}
	}
	if len(eobFindings) != 1 || eobFindings[0].Decision != "relayed" || eobFindings[0].Level != "structural" || eobFindings[0].LegType != "pas-claim" {
		t.Fatalf("want one relayed EOB finding at structural on pas-claim, got %+v", eobFindings)
	}
	if rec, found := p.pendOf(t, got.corr); !found || rec.State != PendStateDecided || p.eobCount() != 0 || len(p.skipped) != 0 {
		t.Fatalf("the decision is recorded without its failed EOB: found=%v %+v, %d EOB(s), skipped %+v", found, rec, p.eobCount(), p.skipped)
	}
}

// The validator cannot answer for the decision EOB this gateway builds. At
// observe and structural a check that could not run is recorded as unavailable
// and does not gate, as everywhere else: the payer's answer is relayed and the
// decision and its EOB are written, as at none. At strict, where a verdict is
// required before the EOB is written, the EOB is not written — the payer's
// decision is still relayed exactly and recorded, never refused over this side
// record (decisioneob_gate_test.go pins strict's finding and payload check).
func TestLevelPayerPASSubmit_EOBValidatorOutage(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementStructural, EnforcementStrict} {
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
				if got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
					t.Fatalf("the payer's decision must be relayed exactly: %d %s", got.status, got.body)
				}
				if !found || rec.State != PendStateDecided || p.eobCount() != 0 {
					t.Fatalf("an EOB whose validation could not run at strict is not written; its decision is: found=%v %+v, %d EOB(s)", found, rec, p.eobCount())
				}
				p.wantSideRecord(t, "pas-claim", DecisionEOBNotWrittenEvent, "decision EOB not written: its validation could not run (validator unavailable)")
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
	if _, err := p.store.RecordPendedKeyed(p.pci, corr, fixedClock(), PendKeys{RequesterHolder: p.requester.ID, ClaimResponseIDs: []string{inquiryCRKey}, ItemTraceNumbers: []string{inquiryTraceKey}}); err != nil {
		t.Fatal(err)
	}
}

// An inquiry answer this gateway cannot read, whose subjects do
// not bind, or whose decision EOB fails validation. Each answer decides the
// seeded authorization if read, so an unchanged ledger below strict is the
// skip, not an answer that decided nothing. A failed decision EOB skips
// nothing: the decision is recorded without it.
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
		"invalid EOB":       {decided, RuleEOBDecision, http.StatusOK, "", &shnsdk.FakeValidator{RejectIfContains: `"ExplanationOfBenefit"`}},
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
				refused := refusesAt(level, row.rule)
				if row.rule == RuleEOBDecision {
					// The EOB is this gateway's side record: its failed
					// validation never refuses the payer's decision, at any
					// level. Its own validation finding is the record.
					refused = false
					if level == EnforcementNone {
						if rec, _ := p.pendOf(t, "corr-submit-1"); got.status != http.StatusOK || rec.State != PendStateDecided || p.eobCount() != 1 {
							t.Fatalf("at none nothing is checked and the decision is written as before: %d, %+v, %d EOB(s)", got.status, rec, p.eobCount())
						}
						return
					}
					if got.status != http.StatusOK || !bytes.Equal(got.body, row.answer) {
						t.Fatalf("the payer's answer must be relayed exactly: %d %s", got.status, got.body)
					}
					p.wantSideRecord(t, "pas-claim-inquire", DecisionEOBNotWrittenEvent, "authorization corr-submit-1: decision EOB not written: it fails its own validation (an unclassified issue)")
				} else {
					p.wantAnswerRow(t, "pas-claim-inquire", got, pasInquirePath, row.answer, row.rule, row.status, row.msg)
				}
				if row.rule == RuleEOBDecision {
					// The decision is recorded without its failed EOB.
					if rec, found := p.pendOf(t, "corr-submit-1"); !found || rec.State != PendStateDecided || p.eobCount() != 0 || len(p.skipped) != 0 {
						t.Fatalf("the decision is recorded without its failed EOB: %+v, %d EOB(s), skipped %+v", rec, p.eobCount(), p.skipped)
					}
					return
				}
				p.wantSkippedWhen(t, "pas-claim-inquire", got.corr, row.rule, refused)
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
	// Its item echoes the trace number the inquiry asks by (the submitted
	// claim's own), as the payer answering that line does.
	traceSystem2, traceValue := submittedItemTrace(t, originatorBuiltConformantBundle(t, "MBR-COVERED"))
	echoing := answerEchoingTrace(t, inquiryFixture(t, "pas-inquiry-response-2.0.json"), traceSystem2, traceValue)
	decided := bytes.Replace(echoing, []byte(`"value": "111099"`), []byte(`"value": "`+pendedTrace+`"`), 1)
	if bytes.Equal(decided, echoing) {
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
			submitted := originatorBuiltConformantBundle(t, "MBR-COVERED")
			submit := p.send(t, "pas-claim", "", submitted)
			if submit.status != http.StatusOK || !bytes.Equal(submit.body, tc.answer) {
				t.Fatalf("submit answer %d %s", submit.status, submit.body)
			}
			if (len(p.skipped) == 1) != tc.skipped {
				t.Fatalf("submit skipped = %+v, want %v", p.skipped, tc.skipped)
			}
			p.partner.respByPath[pasInquirePath] = decided
			// The inquiry asks about the claim just submitted, by that claim's own
			// item trace number, as a requester's follow-up does.
			inquiry := p.send(t, "pas-claim-inquire", "", inquiryAbout(t, submitted))
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

// inquiryAbout is the fixture inquiry for MBR-COVERED, asking about the
// submitted claim by its own item trace number.
func inquiryAbout(t *testing.T, submitted []byte) []byte {
	t.Helper()
	return withSubmittedTrace(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), submitted)
}

// withTrace replaces a fixture inquiry's "TRN-1" line trace number with
// system|value.
func withTrace(t *testing.T, inquiry []byte, system, value string) []byte {
	t.Helper()
	out := bytes.Replace(inquiry,
		[]byte(`"system":"http://provider.example/trn","value":"TRN-1"`),
		[]byte(`"system":"`+system+`","value":"`+value+`"`), 1)
	if bytes.Contains(out, []byte(`"TRN-1"`)) {
		t.Fatal("fixture: the inquiry's trace number was not replaced")
	}
	return out
}

// withSubmittedTrace replaces a fixture inquiry's "TRN-1" line trace number
// with the submitted claim's own, so the inquiry asks about that claim.
func withSubmittedTrace(t *testing.T, inquiry, submitted []byte) []byte {
	t.Helper()
	system, value := submittedItemTrace(t, submitted)
	return withTrace(t, inquiry, system, value)
}

// submittedItemTrace reads the first item trace number a submission's Claim
// states.
func submittedItemTrace(t *testing.T, submitted []byte) (system, value string) {
	t.Helper()
	var b struct {
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
				Item         []struct {
					Extension []struct {
						URL             string            `json:"url"`
						ValueIdentifier map[string]string `json:"valueIdentifier"`
					} `json:"extension"`
				} `json:"item"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(submitted, &b); err != nil {
		t.Fatalf("read the submission: %v", err)
	}
	for _, e := range b.Entry {
		if e.Resource.ResourceType != "Claim" {
			continue
		}
		for _, it := range e.Resource.Item {
			for _, x := range it.Extension {
				if x.URL == pasExtItemTraceNumberURL {
					return x.ValueIdentifier["system"], x.ValueIdentifier["value"]
				}
			}
		}
	}
	t.Fatal("fixture: the submission carries no item trace number")
	return "", ""
}

// PAS 2.2.1 §spec-44: an inquiry answer's ClaimResponse names the Claim it
// answers, or gives a Data Absent Reason. The rule is 2.2.1's alone, and an
// answer is read at its candidate lines (FR-G29: valid on any line is valid),
// so it refuses only on a payer whose every candidate line is 2.2: there it is
// the answer.shape rule (relayed unread at none, recorded at observe, refused
// at structural and strict). That is a payer with a 2.2 lane only, and one
// whose one validator serves every line, which certification judges once, at
// the routed line. On a payer with a validator for each line the answer is
// carried at every level, and the line it was read at is recorded.
func TestLevelPayerInquire_AnswerNamesNoClaim(t *testing.T) {
	var b map[string]any
	if err := json.Unmarshal(decidedAnswer(t), &b); err != nil {
		t.Fatal(err)
	}
	named := 0
	for _, e := range b["entry"].([]any) {
		if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "ClaimResponse" {
			delete(r, "request")
			named++
		}
	}
	if named == 0 {
		t.Fatal("fixture: the answer carries no ClaimResponse")
	}
	noClaim := mustJSON(t, b)
	for _, lanes := range []string{"2.2 only", "one validator for every line", "a validator for each line"} {
		onlyAt22 := lanes != "a validator for each line"
		for _, level := range allLevels {
			t.Run(lanes+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.g.cfg.DeclaredContractVersions = []string{"pa.pas@2.2"}
				switch lanes {
				case "2.2 only":
					p.g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.2": p.g.cfg.Validator}
				case "a validator for each line":
					p.g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": NewLineFakeValidator("2.0"), "2.1": NewLineFakeValidator("2.1"), "2.2": NewLineFakeValidator("2.2")}
				}
				p.seedInquiryPend(t, "corr-submit-1")
				p.partner.respByPath[pasInquirePath] = noClaim
				got := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
				if onlyAt22 {
					p.wantAnswerRow(t, "pas-claim-inquire", got, pasInquirePath, noClaim, RuleAnswerShape, http.StatusBadGateway, "names no Claim and gives no data-absent-reason for it (PAS 2.2.1)")
					return
				}
				if got.status != http.StatusOK || !bytes.Equal(got.body, noClaim) {
					t.Fatalf("the payer's answer must be relayed exactly: %d %s", got.status, got.body)
				}
				p.g.WaitObserveChecksForTest()
				var later []ConformanceFinding
				p.mu.Lock()
				for _, f := range p.findings {
					if f.Rule == RuleAnswerShape {
						later = append(later, f)
					}
				}
				p.mu.Unlock()
				if level == EnforcementNone {
					if len(later) != 0 {
						t.Fatalf("none records nothing: %+v", later)
					}
					return
				}
				if len(later) != 1 || later[0].Verdict != "valid" || later[0].Line != "2.1" || later[0].DeclaredLine != "2.2" {
					t.Fatalf("want one finding: read at 2.1 after 2.2, got %+v", later)
				}
			})
		}
	}
}

// TestLevelPayerPAS_EOBNotWrittenEchoesNoPayload: what a payer
// stated is never echoed into a finding, an observer event or a log line
// about the decision EOB not written: the detail names the element and its
// position only. A marker planted in a note type and in an unmapped review
// action sub-extension appears nowhere this gateway reports.
func TestLevelPayerPAS_EOBNotWrittenEchoesNoPayload(t *testing.T) {
	const marker = "PLANTED-MARKER-X9"
	unmapped := `[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[` +
		`{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"` + shnsdk.X12ReviewDecisionSystem + `","code":"A3"}]}},` +
		`{"url":"http://payer.example/` + marker + `","valueString":"` + marker + `"}]}]`
	rows := map[string]struct {
		answer []byte
		detail string
	}{
		"note type": {fixturePASResponse(t, withNote(deniedClaimResponse(reviewActionExt("A3", "Not Certified", "", ""), nil), marker, "Reply."), true),
			"decision EOB not written: process note 1 type is not display, print or printoper"},
		"unmapped sub-extension": {fixturePASResponse(t, deniedClaimResponse(unmapped, nil), true),
			"decision EOB not written: review action sub-extension 2 has no counterpart in the EOB review action"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				var logs bytes.Buffer
				prev := log.Writer()
				log.SetOutput(&logs)
				defer log.SetOutput(prev)
				p := newLevelPayer(t, level)
				p.partner.respByPath[pasSubmitPath] = row.answer
				got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
				if got.status != http.StatusOK || !bytes.Equal(got.body, row.answer) {
					t.Fatalf("the payer's decision must be relayed exactly: %d %s", got.status, got.body)
				}
				p.wantSideRecord(t, "pas-claim", DecisionEOBNotWrittenEvent, row.detail)
				p.mu.Lock()
				reported, err := json.Marshal(struct {
					F []ConformanceFinding
					S []ObserverEvent
					K []ObserverEvent
				}{p.findings, p.sideRecord, p.skipped})
				p.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(reported, []byte(marker)) || bytes.Contains(logs.Bytes(), []byte(marker)) {
					t.Fatalf("a payer value is echoed:\nreported %s\nlogs %s", reported, logs.Bytes())
				}
			})
		}
	}
}

// TestLevelPayerPASSubmit_NoPayerItemAnswersTheRequestItemWritesNoEOB: on
// submit, a payer whose answer decides the authorization on an item that
// answers none of the request item's sequence (here its one item answers
// sequence 2; the request's order line is sequence 1) states its decision on
// no adjudication the EOB could carry. At every level the decision is relayed
// exactly and recorded, no EOB is written, and the gateway records
// pa.decision-eob-not-written naming the position only.
func TestLevelPayerPASSubmit_NoPayerItemAnswersTheRequestItemWritesNoEOB(t *testing.T) {
	var cr map[string]any
	if err := json.Unmarshal(approvedClaimResponse(nil), &cr); err != nil {
		t.Fatal(err)
	}
	cr["item"].([]any)[0].(map[string]any)["itemSequence"] = 2
	decision, err := json.Marshal(cr)
	if err != nil {
		t.Fatal(err)
	}
	answer := fixturePASResponse(t, decision, true)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.partner.respByPath[pasSubmitPath] = answer
			got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
				t.Fatalf("the payer's decision must be relayed exactly: %d %s", got.status, got.body)
			}
			if rec, found := p.pendOf(t, got.corr); !found || rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved {
				t.Fatalf("the decision was not recorded: found=%v %+v", found, rec)
			}
			if n := p.eobCount(); n != 0 {
				t.Fatalf("%d EOB(s) written that carry none of the payer's decision on the request item", n)
			}
			p.wantSideRecord(t, "pas-claim", DecisionEOBNotWrittenEvent, "decision EOB not written: "+eobNoSubmitItemAnswered)
		})
	}
}
