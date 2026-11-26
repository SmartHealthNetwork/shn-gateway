//go:build !shn_coreonly

package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A federated query's Task.for is read by FHIR's reference grammar on both
// sides: the requester's gateway and the facility's.

// A Task.for that names no Patient by FHIR's reference grammar names no one:
// the requester's gateway refuses it 400 on authority at every level before
// anything is sent, and the facility's gateway refuses it the same way before
// any record is read. Earlier releases read the member from the text after
// "Patient/", or answered 400 federated query rejected (requester) and 403
// query rejected (facility).
func TestFederatedQuery_TaskForNamingNoPatientIsRefusedAtEveryLevel(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"#pt", "urn:Patient/MBR-UC05", "Group/1", "Group/1/Patient/MBR-UC05", "Patient/MBR-UC05/x", "Patient/MBR-UC05%2Fx", "Patient/Patient/MBR-UC05", " Patient/MBR-UC05"} {
		for _, level := range allLevels {
			t.Run("requester/"+ref+"/"+level.String(), func(t *testing.T) {
				env, rec, x := runFQIngress(t, level, fqTaskWith(t, "MBR-UC05", map[string]any{"for": map[string]any{"reference": ref}}), nil)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, msgMemberNamesNoOne)
				wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
				if x.RefusalReason != string(reasonMemberNamesNoOne) {
					t.Fatalf("reason %q, want %q", x.RefusalReason, reasonMemberNamesNoOne)
				}
			})
		}
	}
	for _, ref := range []string{"urn:Patient/" + dtrFrameMember, "Group/1/Patient/" + dtrFrameMember, " Patient/" + dtrFrameMember} {
		for _, level := range allLevels {
			t.Run("facility/"+ref+"/"+level.String(), func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				p.answering(t, "federated-query")
				p.g.cfg.ConsentURL = "http://consent.test"
				p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
				reads := &recordReadCounter{SystemOfRecord: p.g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(p.g.cfg.SoR)}
				p.g.cfg.SoR = reads
				ans := sendConsented(t, p, "federated-query", cdexTaskWithPatient(t, dtrFrameMember, ref, "Patient/"+dtrFrameMember), p.pci, "Consent/c-1")
				p.wantRefused(t, ans, http.StatusBadRequest, msgMemberNamesNoOne)
				wantRefusal(t, got.only(t), http.StatusBadRequest, partyAnswering("federated-query"), RefusalAuthority)
				if n := reads.n.Load(); n != 0 {
					t.Fatalf("the facility read records %d times for a refused query", n)
				}
			})
		}
	}
}

// A Task.for naming its Patient absolute, by an id that is not a FHIR id, names
// that Patient as written: the requester's gateway carries it at none and
// observe (observe records the request's shape), where earlier releases refused
// it 400 federated query rejected; structural and strict refuse it 400 as the
// request's own shape, before anything is sent.
func TestFederatedQueryIngress_AbsoluteTaskForNotAFHIRIDIsTheRequestsShape(t *testing.T) {
	t.Parallel()
	const member = "MBR_UC05"
	task := fqTaskWith(t, member, map[string]any{"for": map[string]any{"reference": "https://ehr.example/fhir/Patient/" + member}})
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newFQExchange(t, level)
			env.payerReturns(LegResult{Response: testResponse(fqAnswer(t, task, fqRecords("Patient/"+member)))})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(task))
			switch level {
			case EnforcementNone, EnforcementObserve:
				if !bytes.Equal(env.lastRequestPayload(), task) {
					t.Fatalf("answer %d %s, want the Task carried as sent", rec.Code, rec.Body)
				}
			default:
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, msgFederatedQueryPatientIDNotFHIRID)
			}
		})
	}
}
