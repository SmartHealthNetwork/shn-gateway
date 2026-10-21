package engine

import (
	"bytes"
	"net/http"
	"testing"
)

// eligibilityWithOtherParameter is a Parameters holding cer as its resource
// parameter, after another parameter that spells its own members in another
// case: a value member named Resource, as the resource parameter's member is
// named in another case, and another.
func eligibilityWithOtherParameter(cer []byte) []byte {
	return []byte(`{"resourceType":"Parameters","parameter":[{"name":"other","Resource":{"resourceType":"Patient","id":"x"},"ValueString":"y"},{"name":"resource","resource":` + string(cer) + `}]}`)
}

// A Parameters' other parameters are carried as sent, and no check reads
// any of their members but their name: one spelling its members in another
// case, a value member named Resource among them, is carried and answered as
// the request it holds, on the payer's records and forward paths and at the
// provider's ingress, at every level. Only the resource parameter is read for
// the patient, so only its members, and every parameter's name, are read by
// their exact names (TestEligibilityIngress_RefusesWhatIsNoEligibilityRequest).
func TestEligibilityParameters_OtherParametersAreCarried(t *testing.T) {
	payerBody := eligibilityWithOtherParameter(eligibilityRequest(t, dtrFrameMember))
	for _, level := range eligibilityLevels {
		t.Run("records/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			ans := p.send(t, "coverage-eligibility", "", payerBody)
			if ref, err := ParseCoverageEligibilityResponsePatient(ans.body); ans.status != http.StatusOK || err != nil || ref != "Patient/"+dtrFrameMember {
				t.Fatalf("answer %d %s, want answered about the request's member", ans.status, ans.body)
			}
		})
		t.Run("forward/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
			p.partner.respByPath[eligibilityPath] = answer
			ans := p.send(t, "coverage-eligibility", "", payerBody)
			if ans.status != http.StatusOK || !bytes.Equal(ans.body, answer) || !bytes.Equal(p.partner.lastBody, payerBody) {
				t.Fatalf("answer %d %s, payer's system received %s: want the input carried as sent and the payer's answer relayed", ans.status, ans.body, p.partner.lastBody)
			}
		})
	}
	ingressBody := eligibilityWithOtherParameter(eligibilityRequest(t, "MBR-COVERED"))
	answer := payersEligibilityAnswer("Patient/MBR-COVERED")
	for _, level := range allLevels {
		t.Run("ingress/"+level.String(), func(t *testing.T) {
			env, rec, x := runEligibilityIngress(t, level, ingressBody, answer)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) || !bytes.Equal(env.lastRequestPayload(), ingressBody) || len(x.Edits) != 0 {
				t.Fatalf("answer %d %s, want the input carried as sent and the payer's answer relayed", rec.Code, rec.Body)
			}
		})
	}
}

// A parameter's name is read by its exact member name in every parameter, and
// the resource parameter's resource by its own: a reader folding either could
// take another value for the request. Refused with the payer gateway's text
// on the records and forward paths, before the payer's system, at every
// level.
func TestEligibilityParameters_NamesAndTheResourceAreReadExactly(t *testing.T) {
	cer := string(eligibilityRequest(t, dtrFrameMember))
	const noBind = `: member names are case-sensitive, so the request names no patient to bind`
	for name, row := range map[string]struct{ body, want string }{
		"another parameter's name in another case":      {`{"resourceType":"Parameters","parameter":[{"Name":"other","valueString":"y"},{"name":"resource","resource":` + cer + `}]}`, `eligibility request names \"Name\" for \"name\"` + noBind},
		"the resource parameter's name in another case": {`{"resourceType":"Parameters","parameter":[{"Name":"resource","resource":` + cer + `}]}`, `eligibility request names \"Name\" for \"name\"` + noBind},
		"the resource in another case":                  {`{"resourceType":"Parameters","parameter":[{"name":"other","valueString":"y"},{"name":"resource","Resource":` + cer + `}]}`, `eligibility request names \"Resource\" for \"resource\"` + noBind},
	} {
		for _, declared := range []bool{false, true} {
			for _, level := range eligibilityLevels {
				t.Run(name+"/"+map[bool]string{false: "records", true: "forward"}[declared]+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, declared)
					ans := p.send(t, "coverage-eligibility", "", []byte(row.body))
					if ans.status != http.StatusBadRequest || !bytes.Contains(ans.body, []byte(row.want)) || p.partner.calls.Load() != 0 {
						t.Fatalf("answer %d %s (payer system calls %d), want 400 %s before the payer's system", ans.status, ans.body, p.partner.calls.Load(), row.want)
					}
				})
			}
		}
	}
}
