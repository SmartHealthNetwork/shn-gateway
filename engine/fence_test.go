package engine

import (
	"net/http"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestFenceResponseSubject_DTR verifies the (C) fence for the dtr-questionnaire-fetch
// leg: a Questionnaire that unexpectedly carries a subject is rejected with 403;
// a patient-agnostic Questionnaire passes (status 0).
func TestFenceResponseSubject_DTR(t *testing.T) {
	g := &Gateway{}
	// A package whose Questionnaire entry carries a subject must be rejected (403) —
	// the fence walks entries (the bare-resource probe would miss it on the wrapper).
	withSubject := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"Library","id":"l1"}},` +
		`{"resource":{"resourceType":"Questionnaire","subject":{"reference":"Patient/X"}}}]}`)
	if status, _ := g.fenceResponseSubject("dtr-questionnaire-fetch", "", LegResult{Response: testResponse(withSubject)}); status != http.StatusForbidden {
		t.Fatalf("subject-bearing Questionnaire entry: got status %d, want 403", status)
	}
	// A package whose Questionnaire entry is subjectless passes (0). The Questionnaire is
	// spelled inline (patient-agnostic by construction) rather than borrowed from a
	// content library: the fence's subject matters here, nothing else about the resource.
	clean, err := testQuestionnairePackage([]byte(`{"resourceType":"Questionnaire","id":"q-fence","status":"active",` +
		`"url":"http://example.org/fhir/Questionnaire/q-fence","item":[{"linkId":"1","type":"boolean","text":"answer"}]}`))
	if err != nil {
		t.Fatalf("wrap clean questionnaire: %v", err)
	}
	if status, _ := g.fenceResponseSubject("dtr-questionnaire-fetch", "", LegResult{Response: testResponse(clean)}); status != 0 {
		t.Fatalf("subjectless package: got status %d, want 0", status)
	}
}

// claimResponseFor returns a minimal bare ClaimResponse JSON whose patient.reference is ref —
// the shape ParsePASResponsePatients reads (fhirread.go).
func claimResponseFor(t *testing.T, ref string) []byte {
	t.Helper()
	return []byte(`{"resourceType":"ClaimResponse","patient":{"reference":"` + ref + `"}}`)
}

// eobFor returns a minimal ExplanationOfBenefit JSON whose patient.reference is ref — the shape
// parseEOBPatient reads (fhirread.go).
func eobFor(t *testing.T, ref string) []byte {
	t.Helper()
	return []byte(`{"resourceType":"ExplanationOfBenefit","patient":{"reference":"` + ref + `"}}`)
}

// The converged conformant PAS legs (pas-claim / pas-claim-update) carry the (C)
// outbound fence under TWO independent flags: member-fence the
// ClaimResponse iff !ResponseSubjectForeign (R-7), and the SHN-produced EOB side-effect is fenced
// UNCONDITIONALLY. SHN-produced posture = both flags false = strict (fail-closed). Native posture =
// both set explicitly for unchanged bytes (the payer answers in its own namespace).

func TestFenceConformantPAS_SubjectSwap_Rejected(t *testing.T) {
	g := &Gateway{} // fenceResponseSubject reads no Gateway state for the PAS arm
	// SHN-produced posture: both flags false (zero value). A response naming a DIFFERENT member must 403.
	res := LegResult{Response: testResponse(claimResponseFor(t, "Patient/MBR-OTHER"))}
	if status, _ := g.fenceResponseSubject("pas-claim", "Patient/MBR-COVERED", res); status != http.StatusForbidden {
		t.Fatalf("subject swap: status=%d, want 403", status)
	}
}

func TestFenceConformantPAS_ForeignRelay_StandsDown(t *testing.T) {
	g := &Gateway{}
	// native posture: ResponseSubjectForeign=true. A foreign-namespace ClaimResponse must PASS (R-7).
	res := LegResult{Response: relayedResponse([]byte(assemblyRealPending)), ResponseSubjectForeign: true}
	if status, msg := g.fenceResponseSubject("pas-claim", "Patient/MBR-COVERED", res); status != 0 {
		t.Fatalf("foreign relay stand-down: status=%d msg=%q, want 0", status, msg)
	}
}

func TestFenceConformantPAS_ForeignRelay_WrongEOB_Rejected(t *testing.T) {
	g := &Gateway{}
	// Even under a foreign relay, the SHN-produced EOB side-effect is fenced UNCONDITIONALLY.
	res := LegResult{
		Response:               relayedResponse([]byte(assemblyRealPending)),
		SideEffectFHIR:         [][]byte{eobFor(t, "Patient/MBR-OTHER")},
		ResponseSubjectForeign: true,
	}
	if status, _ := g.fenceResponseSubject("pas-claim", "Patient/MBR-COVERED", res); status != http.StatusForbidden {
		t.Fatalf("wrong-member EOB under relay: status=%d, want 403", status)
	}
}

func TestFenceConformantPASUpdate_SubjectSwap_Rejected(t *testing.T) {
	g := &Gateway{}
	res := LegResult{Response: testResponse(claimResponseFor(t, "Patient/MBR-OTHER"))}
	if status, _ := g.fenceResponseSubject("pas-claim-update", "Patient/MBR-COVERED", res); status != http.StatusForbidden {
		t.Fatalf("update subject swap: status=%d, want 403", status)
	}
}

// TestFenceResponseSubject_Eligibility is the direct rejection-test for the (C) fence's
// coverage-eligibility arm: a CoverageEligibilityResponse whose
// patient does not match the bound request patient must 403. This is the arm's OWN unit
// test — TestAdversarial_ResponseSubjectSwap_Eligibility (test/adversarial) proves a
// DIFFERENT, complementary property (the promoted handler never lets an injected
// occupant reach this leg at all, so the fence has nothing to catch there); it does not
// exercise fenceResponseSubject's "coverage-eligibility" case directly, so it cannot
// stand in for this test.
func TestFenceResponseSubject_Eligibility(t *testing.T) {
	g := &Gateway{}
	t0 := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	// The swap: a CRR built for Patient/Y, fenced against a request bound to Patient/X.
	crrForY, err := shnsdk.BuildEligibilityResponse("corr-1", "Patient/Y", true, "", shnsdk.PayerIdentifier{}, t0)
	if err != nil {
		t.Fatalf("BuildEligibilityResponse: %v", err)
	}
	if status, msg := g.fenceResponseSubject("coverage-eligibility", "Patient/X", LegResult{Response: testResponse(crrForY)}); status != http.StatusForbidden {
		t.Fatalf("foreign-patient CRR: status=%d msg=%q, want 403", status, msg)
	}

	// Non-vacuous control: a CRR correctly built for the SAME bound patient passes (0).
	crrForX, err := shnsdk.BuildEligibilityResponse("corr-1", "Patient/X", true, "", shnsdk.PayerIdentifier{}, t0)
	if err != nil {
		t.Fatalf("BuildEligibilityResponse: %v", err)
	}
	if status, msg := g.fenceResponseSubject("coverage-eligibility", "Patient/X", LegResult{Response: testResponse(crrForX)}); status != 0 {
		t.Fatalf("matching-patient CRR: status=%d msg=%q, want 0", status, msg)
	}
}

// TestFenceResponseSubject_RepeatedMemberRefused: an answer that repeats a
// member name — exactly, or under case folding — can be read two ways, so the
// fence cannot judge it: 403, before any subject is compared. (A request that
// repeats a member is refused 400 instead: there the request is malformed; here
// the answering side sent something this gateway will not vouch for.) Each row
// changes one member of an answer the fence otherwise passes.
func TestFenceResponseSubject_RepeatedMemberRefused(t *testing.T) {
	g := &Gateway{}
	const member = "Patient/MBR-COVERED"
	eligibility := `{"resourceType":"CoverageEligibilityResponse","patient":{"reference":"` + member + `"}}`
	dtrPackage := `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"q","status":"active"}}]}`
	pas := string(claimResponseFor(t, member))
	rows := []struct {
		leg, bound, valid, find, exact, folded string
		foreign                                bool
	}{
		{"coverage-eligibility", member, eligibility, `"patient":`, `"patient":{"reference":"Patient/MBR-OTHER"},"patient":`, `"Patient":{"reference":"Patient/MBR-OTHER"},"patient":`, false},
		{"dtr-questionnaire-fetch", "", dtrPackage, `"status":"active"`, `"status":"active","status":"draft"`, `"status":"active","Status":"draft"`, false},
		{"pas-claim", member, pas, `"patient":`, `"patient":{"reference":"Patient/MBR-OTHER"},"patient":`, `"Patient":{"reference":"Patient/MBR-OTHER"},"patient":`, false},
		{"pas-claim-update", member, pas, `"patient":`, `"patient":{"reference":"Patient/MBR-OTHER"},"patient":`, `"PATIENT":{"reference":"Patient/MBR-OTHER"},"patient":`, true},
	}
	for _, r := range rows {
		res := func(body string) LegResult {
			if r.foreign && body != r.valid {
				// The payer's own answer, relayed: the repeated member is caught
				// before the relayed-graph check would run.
				return LegResult{Response: relayedResponse([]byte(body)), ResponseSubjectForeign: true}
			}
			return LegResult{Response: testResponse([]byte(body))}
		}
		t.Run(r.leg+"/control", func(t *testing.T) {
			if status, msg := g.fenceResponseSubject(r.leg, r.bound, res(r.valid)); status != 0 {
				t.Fatalf("valid answer refused: %d %s", status, msg)
			}
		})
		for name, repl := range map[string]string{"exact": r.exact, "case-folded": r.folded} {
			t.Run(r.leg+"/"+name, func(t *testing.T) {
				body := strings.Replace(r.valid, r.find, repl, 1)
				if body == r.valid {
					t.Fatal("mutation did not apply")
				}
				status, msg := g.fenceResponseSubject(r.leg, r.bound, res(body))
				if status != http.StatusForbidden || msg != "response repeats a member name" {
					t.Fatalf("got %d %q, want 403 repeated member", status, msg)
				}
			})
		}
	}
}
