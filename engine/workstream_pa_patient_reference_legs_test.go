//go:build !shn_coreonly

package engine

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The CRD, DTR and patient-DTR legs read their patient by FHIR's reference
// grammar, as the PAS legs do: by structure, never by finding "Patient/" in a
// reference's text.

// wantCarriedClean asserts a request the payer's gateway carries at every
// level, as sent, recording no content finding.
func wantCarriedClean(t *testing.T, p *levelPayer, got payerAnswer, body []byte, path string) {
	t.Helper()
	if got.status != http.StatusOK || !got.framed {
		t.Fatalf("at %s the request must be carried: answer %d %s", p.level, got.status, got.body)
	}
	if p.partner.lastPath != path || !bytes.Equal(p.partner.lastBody, body) {
		t.Fatalf("at %s the participant's system must receive the request as sent at %s, got %s", p.level, path, p.partner.lastPath)
	}
	if fs := p.content(); len(fs) != 0 {
		t.Fatalf("at %s a carried request recorded %v", p.level, findingsText(fs))
	}
}

// A CDS Hooks context.patientId is a Patient's id. One that holds a "/" other
// than the one relative "Patient/<id>" names no Patient by its structure: the
// payer's gateway refuses it 400 on authority at every level, on order-select
// and order-dispatch, before the request's consistency is checked and before
// the payer's system is asked. Earlier releases read the member from the text
// after "Patient/".
func TestPayerCRD_PatientIDNamingNoPatientIsRefusedAtEveryLevel(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"Patient/MBR-COVERED/x", "Patient/MBR-COVERED/_history/2", "Group/1", "a/b", "Patient/MBR%2FCOVERED", "Patient/$everything", "Group/1/Patient/MBR-COVERED"} {
		for _, leg := range []string{"crd-order-select", "crd-order-dispatch"} {
			for _, level := range allLevels {
				t.Run(leg+"/"+id+"/"+level.String(), func(t *testing.T) {
					p, got := newRecordingLevelPayer(t, level)
					var body []byte
					if leg == "crd-order-select" {
						body = crdSelectWith(t, `"patientId":"MBR-COVERED"`, `"patientId":"`+id+`"`)
					} else {
						body = crdDispatchWith(t, `"patientId":"MBR-COVERED"`, `"patientId":"`+id+`"`)
					}
					ans := p.sendAs(t, leg, "", body, p.pci)
					p.wantRefused(t, ans, http.StatusBadRequest, msgMemberNamesNoOne)
					wantRefusal(t, got.only(t), http.StatusBadRequest, partyAnswering(leg), RefusalAuthority)
					if fs := p.content(); len(fs) != 0 {
						t.Fatalf("a refusal on authority records no content finding, got %v", findingsText(fs))
					}
				})
			}
		}
	}
}

// The one relative "Patient/<id>" in context.patientId is read as its id, as
// earlier releases read it (the SDK's own order-select vector writes it so).
func TestPayerCRD_PatientIDAsARelativeReferenceIsItsID(t *testing.T) {
	t.Parallel()
	body := crdSelectWith(t, `"patientId":"MBR-COVERED"`, `"patientId":"Patient/MBR-COVERED"`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			wantCarriedClean(t, p, p.send(t, "crd-order-select", "", body), body, crdSelectPath)
		})
	}
}

// A context.patientId that is not a FHIR id binds as written: carried at none,
// recorded at observe, refused 400 as the request's own shape at structural and
// strict (a participant's opt-in, never the network's).
func TestPayerCRD_PatientIDNotAFHIRIDIsTheRequestsShape(t *testing.T) {
	t.Parallel()
	body := conformantCRD("MBR_COVERED", "72148")
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			got := p.send(t, "crd-order-select", "", body)
			p.wantRequestRow(t, "crd-order-select", got, body, crdSelectPath, RuleRequestShape, http.StatusBadRequest, msgCRDPatientIDNotFHIRID)
		})
	}
}

// The patient-consistency check reads an order subject and a Coverage
// beneficiary by FHIR's reference grammar. One that names the request's patient
// versioned, absolute on the EHR's own server (fhirServer), or as the order's one
// contained Patient for the member, is the request's patient: carried at every
// level with nothing recorded, where earlier releases compared it as written and
// strict refused it.
func TestPayerCRD_SubjectsNamingThePatientAreConsistent(t *testing.T) {
	t.Parallel()
	const subject = `"subject":{"reference":"Patient/MBR-COVERED"},"code"`
	withServer := func(body []byte) []byte {
		return bytes.Replace(body, []byte(`"hook":"order-select",`), []byte(`"hook":"order-select","fhirServer":"https://ehr.example/fhir",`), 1)
	}
	contained := `"contained":[{"resourceType":"Patient","id":"p1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-COVERED"}]}],"subject":{"reference":"#p1"},"code"`
	for name, body := range map[string][]byte{
		"versioned":                      crdSelectWith(t, subject, `"subject":{"reference":"Patient/MBR-COVERED/_history/2"},"code"`),
		"absolute on the EHR's server":   withServer(crdSelectWith(t, subject, `"subject":{"reference":"https://ehr.example/fhir/Patient/MBR-COVERED"},"code"`)),
		"versioned absolute, own server": withServer(crdSelectWith(t, subject, `"subject":{"reference":"https://ehr.example/fhir/Patient/MBR-COVERED/_history/2"},"code"`)),
		"the order's contained Patient":  crdSelectWith(t, subject, contained),
		"versioned beneficiary":          crdSelectWith(t, `"beneficiary":{"reference":"Patient/MBR-COVERED"}`, `"beneficiary":{"reference":"Patient/MBR-COVERED/_history/1"}`),
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				wantCarriedClean(t, p, p.send(t, "crd-order-select", "", body), body, crdSelectPath)
			})
		}
	}
}

// An absolute reference on another server names a resource there, another
// patient, as before; one that only looks like a reference to a patient is
// compared as written, so it is never the request's patient. Both are the
// request's own consistency: not checked at none, recorded at observe and
// structural, refused at strict.
func TestPayerCRD_SubjectsNamingAnotherOrNoPatientAreTheRequestsConsistency(t *testing.T) {
	t.Parallel()
	const subject = `"subject":{"reference":"Patient/MBR-COVERED"},"code"`
	for name, ref := range map[string]string{
		"another server":     "https://elsewhere.example/fhir/Patient/MBR-COVERED",
		"urn:Patient":        "urn:Patient/MBR-COVERED",
		"#Patient":           "#Patient/MBR-COVERED",
		"behind white space": " Patient/MBR-COVERED",
		"Patient/ inside":    "Group/1/Patient/MBR-COVERED",
	} {
		body := crdSelectWith(t, subject, `"subject":{"reference":"`+ref+`"},"code"`)
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "crd-order-select", "", body)
				p.wantRequestRow(t, "crd-order-select", got, body, crdSelectPath, RulePatientMixed, http.StatusBadRequest, "inconsistent patient in order-select")
			})
		}
	}
}

// On the provider's side, a context.patientId that names no Patient by its
// structure is refused 400 on authority at every level, before anything is sent.
func TestCRDIngress_PatientIDNamingNoPatientIsRefusedAtEveryLevel(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"Patient/example/x", "Patient/example/_history/2", "Group/1", "a/b", "Patient/$everything"} {
		body := bytes.Replace(ehrRequest(supported), []byte(`"patientId" : "example"`), []byte(`"patientId" : "`+id+`"`), 1)
		if bytes.Equal(body, ehrRequest(supported)) {
			t.Fatal("fixture: patientId not replaced")
		}
		for _, level := range allLevels {
			t.Run(id+"/"+level.String(), func(t *testing.T) {
				env, rec, findings := levelIngressRow(t, newPrefetchSoR(), level, body)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, msgMemberNamesNoOne)
				if len(findings) != 0 {
					t.Fatalf("a refusal on authority records no content finding, got %+v", findings)
				}
			})
		}
	}
}

// The DTR legs read a patient reference by FHIR's reference grammar.
// "Patient/" found inside another reference, or followed by a trailing path,
// names no Patient: where nothing else names the request's patient the leg's
// own refusal answers it at every level, recorded as authority (earlier
// releases read the id after the last "Patient/"); beside a reference that does
// name it, it is the request's own shape, refused at strict and structural.
func TestPayerDTRPackage_PatientReferencesAreReadByTheGrammar(t *testing.T) {
	t.Parallel()
	own := resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember))
	for _, ref := range []string{"Group/1/Patient/" + dtrFrameMember, "Basic/1/_history/x/Patient/" + dtrFrameMember, "Patient/" + dtrFrameMember + "/x", "Patient/" + dtrFrameMember + "%2Fx"} {
		sole := dtrPackage(resourceParam("coverage", strings.Replace(dtrCoverage("cov-1", dtrFrameMember), `"Patient/`+dtrFrameMember+`"`, `"`+ref+`"`, 1)))
		for _, level := range allLevels {
			t.Run("sole/"+ref+"/"+level.String(), func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				ans := p.sendAs(t, "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, sole, p.pci)
				p.wantRefused(t, ans, http.StatusBadRequest, errNotPatientRef.msg)
				wantRefusal(t, got.only(t), http.StatusBadRequest, partyAnswering("dtr-questionnaire-fetch"), RefusalAuthority)
			})
		}
		beside := dtrPackage(own, resourceParam("order", strings.Replace(dtrOrder(dtrFrameMember), `"Patient/`+dtrFrameMember+`"`, `"`+ref+`"`, 1)))
		for _, level := range allLevels {
			t.Run("beside/"+ref+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, beside)
				p.wantRequestRow(t, "dtr-questionnaire-fetch", got, beside, packagePath, RuleRequestShape, http.StatusBadRequest, "not a Patient reference")
			})
		}
	}
}

// A DTR patient reference naming its Patient by an id that is not a FHIR id
// binds as written: carried at none, recorded at observe, refused 400 as the
// request's own shape at structural and strict.
func TestPayerDTRPackage_PatientIDNotAFHIRIDIsTheRequestsShape(t *testing.T) {
	t.Parallel()
	body := dtrPackage(resourceParam("coverage", dtrCoverage("cov-1", "MBR_COVERED")))
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			got := p.send(t, "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, body)
			p.wantRequestRow(t, "dtr-questionnaire-fetch", got, body, packagePath, RuleRequestShape, http.StatusBadRequest, msgDTRPatientIDNotFHIRID)
		})
	}
}

// patientDTRNaming is the patient-authored DTR request naming its patient ref.
func patientDTRNaming(t *testing.T, ref string) []byte {
	t.Helper()
	return mustJSON(t, map[string]string{"linkId": "functional-status-oswestry", "answer": "42", "patientRef": ref})
}

// The PHG reads the patient-authored DTR request's patient by FHIR's reference
// grammar. One that names no Patient so is refused 400 on authority at every
// level, before anything is read (earlier releases read the text after
// "Patient/" and answered 403 not authorized); an absolute reference names its
// id, so one for the token's patient is answered (earlier releases refused it
// 403).
func TestPatientDTR_PatientReferencesAreReadByTheGrammar(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"urn:Patient/" + dtrFrameMember, "Patient/" + dtrFrameMember + "/x", "Group/1/Patient/" + dtrFrameMember, " Patient/" + dtrFrameMember, "Patient/" + dtrFrameMember + "%2Fx"} {
		for _, level := range allLevels {
			t.Run("names no one/"+ref+"/"+level.String(), func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				p.answering(t, "patient-dtr")
				ans := p.sendAs(t, "patient-dtr", "", patientDTRNaming(t, ref), p.pci)
				p.wantRefused(t, ans, http.StatusBadRequest, msgMemberNamesNoOne)
				wantRefusal(t, got.only(t), http.StatusBadRequest, partyAnswering("patient-dtr"), RefusalAuthority)
			})
		}
	}
	for _, ref := range []string{"https://ehr.example/fhir/Patient/" + dtrFrameMember, "Patient/" + dtrFrameMember + "/_history/3"} {
		for _, level := range allLevels {
			t.Run("names the patient/"+ref+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.answering(t, "patient-dtr")
				ans := p.sendAs(t, "patient-dtr", "", patientDTRNaming(t, ref), p.pci)
				if ans.status != http.StatusOK {
					t.Fatalf("answer %d %s, want the patient's item attested", ans.status, ans.body)
				}
				if fs := p.content(); len(fs) != 0 {
					t.Fatalf("recorded %v", findingsText(fs))
				}
			})
		}
	}
	// An id that is not a FHIR id binds as written: the PHG holds no such
	// patient (403) at none and observe, observe records the request's shape,
	// and structural and strict refuse it as that shape.
	for _, level := range allLevels {
		t.Run("not a FHIR id/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.answering(t, "patient-dtr")
			ans := p.sendAs(t, "patient-dtr", "", patientDTRNaming(t, "Patient/MBR_COVERED"), p.pci)
			switch level {
			case EnforcementNone, EnforcementObserve:
				if ans.status != http.StatusForbidden || !strings.Contains(string(ans.body), refusalPatientNotAuthorized) {
					t.Fatalf("answer %d %s, want the member bound as written and not authorized", ans.status, ans.body)
				}
				if fs := p.content(); level == EnforcementObserve && !hasFinding(fs, RuleRequestShape) {
					t.Fatalf("at observe want a %s finding, got %v", RuleRequestShape, findingsText(fs))
				}
			default:
				p.wantRefused(t, ans, http.StatusBadRequest, msgPatientDTRPatientIDNotFHIRID)
			}
		})
	}
}
