//go:build !shn_coreonly

package engine

import (
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A coverage-eligibility request whose patient reference names no one (an
// empty or blank reference, "Patient/" with no id, or a contained Patient
// whose member identifier is blank) binds no patient: unknown is not empty,
// so neither gateway reads it as the member "". Each gateway refuses
// it on authority (the subject binding), at every level, before it reads its
// system of record or sends anything. A well-formed reference is unaffected.
func TestEligibility_APatientReferenceNamingNoOneIsRefused(t *testing.T) {
	t.Parallel()
	blankContained := map[string]any{"resourceType": "Patient", "id": "p",
		"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "  "}}}
	_, patient, _, _ := eligFAISParts("MBR-COVERED", "00001")
	bundled := func(ref string) []byte {
		return eligFAISBundle(t, [2]any{"", map[string]any{"resourceType": "CoverageEligibilityRequest", "status": "active",
			"patient": map[string]any{"reference": ref}, "insurer": map[string]any{"identifier": eligibilityPayorIdentifier("00001")}}},
			[2]any{"urn:uuid:patient", patient})
	}
	rows := map[string][]byte{
		"empty reference":                         eligibilityWithPatient(t, dtrFrameMember, ""),
		"the type with no id":                     eligibilityWithPatient(t, dtrFrameMember, "Patient/"),
		"a blank reference":                       eligibilityWithPatient(t, dtrFrameMember, "   "),
		"the type with a blank id":                eligibilityWithPatient(t, dtrFrameMember, "Patient/ "),
		"a contained Patient with a blank member": eligibilityWithPatient(t, dtrFrameMember, "#p", blankContained),
		"the type twice with no id":               eligibilityWithPatient(t, dtrFrameMember, "Patient/Patient/"),
		"a contained Patient whose member is the type with no id": eligibilityWithPatient(t, dtrFrameMember, "#p",
			map[string]any{"resourceType": "Patient", "id": "p", "identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "Patient/"}}}),
	}
	// In a Bundle or a batch, such a reference is refused the same way, by
	// both gateways, at every level: never bound, and nothing sent.
	rows["in a Bundle, an empty reference"] = bundled("")
	rows["in a Bundle, the type with no id"] = bundled("Patient/")
	rows["in a Bundle, the type with a blank id"] = bundled("Patient/ ")
	rows["in a Bundle, the type twice with no id"] = bundled("Patient/Patient/")
	rows["in a batch, after a well-formed request"] = eligBatch(t, eligibilityRequest(t, dtrFrameMember), eligibilityWithPatient(t, dtrFrameMember, "Patient/"))
	// A Bundle's request that names its patient by no reference at all names
	// none, which is not a reference naming no one: unknown is not empty. It
	// keeps its answer, as the same request sent alone does.
	noReference := func(named any) []byte {
		req := map[string]any{"resourceType": "CoverageEligibilityRequest", "status": "active",
			"insurer": map[string]any{"identifier": eligibilityPayorIdentifier("00001")}}
		if named != nil {
			req["patient"] = named
		}
		return eligFAISBundle(t, [2]any{"", req}, [2]any{"urn:uuid:patient", patient})
	}
	for name, body := range map[string][]byte{
		"in a Bundle, no patient":                      noReference(nil),
		"in a Bundle, a patient named by no reference": noReference(map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": dtrFrameMember}}),
	} {
		for _, level := range allLevels {
			t.Run("provider/"+name+"/"+level.String(), func(t *testing.T) {
				env, rec, _ := runEligibilityIngress(t, level, body, payersEligibilityAnswer("Patient/"+dtrFrameMember))
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "parse member failed")
			})
			t.Run("payer/"+name+"/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, false)
				ans := p.send(t, "coverage-eligibility", "", body)
				if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), "parse member failed") || p.partner.calls.Load() != 0 {
					t.Fatalf("answer %d %s, want 400 parse member failed", ans.status, ans.body)
				}
			})
		}
	}
	for name, body := range rows {
		for _, level := range allLevels {
			t.Run("provider/"+name+"/"+level.String(), func(t *testing.T) {
				env, rec, x := runEligibilityIngress(t, level, body, payersEligibilityAnswer("Patient/"+dtrFrameMember))
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, errEligibilityNamesNoOne)
				wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
				wantReasonInAnswer(t, x, rec.Body.String())
				if x.RefusalReason != string(reasonEligibilityNamesNoOne) {
					t.Fatalf("reason %q, want %q", x.RefusalReason, reasonEligibilityNamesNoOne)
				}
			})
			for _, declared := range []bool{false, true} {
				path := map[bool]string{false: "records", true: "forward"}[declared]
				t.Run("payer/"+name+"/"+path+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, declared)
					var got exchangeRecords
					p.g.cfg.ExchangeObserved = got.observe
					ans := p.send(t, "coverage-eligibility", "", body)
					if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), errEligibilityNamesNoOne) {
						t.Fatalf("answer %d %s, want 400 %q", ans.status, ans.body, errEligibilityNamesNoOne)
					}
					if n := p.partner.calls.Load(); n != 0 {
						t.Fatalf("the payer's system was called %d times for a request naming no one", n)
					}
					wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, RefusalAuthority)
				})
			}
		}
	}
	// Control: the same request naming its patient by a well-formed
	// reference is carried and answered on both gateways.
	for _, level := range allLevels {
		t.Run("control/provider/"+level.String(), func(t *testing.T) {
			env, rec, _ := runEligibilityIngress(t, level, eligibilityWithPatient(t, dtrFrameMember, "Patient/"+dtrFrameMember),
				payersEligibilityAnswer("Patient/"+dtrFrameMember))
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s, routed %d, want answered", rec.Code, rec.Body, env.routeHitCount())
			}
		})
		t.Run("control/payer/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			ans := p.send(t, "coverage-eligibility", "", eligibilityWithPatient(t, dtrFrameMember, "Patient/"+dtrFrameMember))
			if ans.status != http.StatusOK {
				t.Fatalf("answer %d %s, want answered", ans.status, ans.body)
			}
		})
	}
}
