package engine

import (
	"net/http"
	"strings"
	"testing"
)

// Each gateway reads a coverage-eligibility request one way only: the
// provider's gateway at its ingress, and the payer's gateway on the leg, on
// the records path and the forward path, in each of the operation's forms, at
// every level. A request that repeats a member name, exactly or under case
// folding, can be read two ways: it is refused as message integrity, before
// anything crosses the network or the payer's system is called. A request
// whose type or patient is named only in another case names no patient
// (member names are case-sensitive): it is refused, never bound (the
// patient-match rule). Both gateways give the same text, naming members and
// never a value. Each row changes one member of a request the control shows
// is answered.
func TestEligibility_EachGatewayReadsTheRequestOneWayOnly(t *testing.T) {
	cer := string(eligibilityRequest(t, dtrFrameMember))
	const (
		patient = `"patient":{"reference":"Patient/` + dtrFrameMember + `"}`
		typ     = `"resourceType":"CoverageEligibilityRequest"`
	)
	const caseSensitive = "member names are case-sensitive, so the request names no patient to bind"
	rows := []struct {
		name, find, replace string
		repeats             bool
		message             string
	}{
		{name: "control"},
		{"patient repeated", patient, patient + `,"patient":{"reference":"Patient/` + anotherHeldMember + `"}`, true,
			`eligibility request repeats the JSON member \"patient\"`},
		{"patient repeated under case folding", patient, patient + `,"Patient":{"reference":"Patient/` + anotherHeldMember + `"}`, true,
			`eligibility request repeats the JSON member \"Patient\"`},
		{"type repeated under case folding", typ, `"resourceType":"Parameters","ResourceType":"CoverageEligibilityRequest"`, true,
			`eligibility request repeats the JSON member \"ResourceType\"`},
		{"reference repeated", `"reference":"Patient/` + dtrFrameMember + `"`, `"reference":"Patient/` + dtrFrameMember + `","reference":"Patient/` + anotherHeldMember + `"`, true,
			`eligibility request repeats the JSON member \"reference\" in `},
		{"a member read for nothing repeated", `"status":"active"`, `"status":"active","status":"cancelled"`, true,
			`eligibility request repeats the JSON member \"status\"`},
		{"patient named in another case", `"patient":`, `"Patient":`, false,
			`eligibility request names \"Patient\" for \"patient\": ` + caseSensitive},
		{"reference named in another case", `"reference":"Patient/` + dtrFrameMember, `"Reference":"Patient/` + dtrFrameMember, false,
			`eligibility request names \"Reference\" for \"reference\": ` + caseSensitive},
		{"type named in another case", typ, `"ResourceType":"CoverageEligibilityRequest"`, false,
			`eligibility request names \"ResourceType\" for \"resourceType\": ` + caseSensitive},
	}
	forms := []struct {
		name string
		wrap func(string) string
	}{
		{"alone", func(r string) string { return r }},
		{"in a Parameters", func(r string) string { return string(eligibilityAsParameters(t, []byte(r))) }},
		{"in a Bundle", func(r string) string {
			return `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + r + `}]}`
		}},
	}
	// The provider's gateway reads the same body the same way: each refused
	// before anything crosses the network, with the payer's text and rule.
	for _, row := range rows {
		req := cer
		if row.find != "" {
			req = strings.Replace(cer, row.find, row.replace, 1)
		}
		for _, form := range forms {
			for _, level := range allLevels {
				t.Run("provider/"+row.name+"/"+form.name+"/"+level.String(), func(t *testing.T) {
					env, rec, x := runEligibilityIngress(t, level, []byte(form.wrap(req)), payersEligibilityAnswer("Patient/"+dtrFrameMember))
					if row.find == "" {
						if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
							t.Fatalf("control: answer %d %s, routed %d", rec.Code, rec.Body.String(), env.routeHitCount())
						}
						return
					}
					want, rule := row.message, RefusalAuthority
					switch {
					case row.repeats:
						rule = RefusalIntegrity
					case form.name == "in a Bundle" && row.name == "type named in another case":
						want, rule = "the Bundle's entries cannot be read one way only", ExchangeOther
					}
					refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, want)
					if strings.Contains(rec.Body.String(), anotherHeldMember) {
						t.Fatalf("the refusal echoes a value: %s", rec.Body.String())
					}
					wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, rule)
				})
			}
		}
	}
	for _, row := range rows {
		req := cer
		if row.find != "" {
			req = strings.Replace(cer, row.find, row.replace, 1)
			if req == cer {
				t.Fatalf("%s: the mutation did not apply", row.name)
			}
		}
		for _, form := range forms {
			for _, level := range eligibilityLevels {
				for _, declared := range []bool{false, true} {
					path := "records"
					if declared {
						path = "forward"
					}
					t.Run(row.name+"/"+form.name+"/"+path+"/"+level.String(), func(t *testing.T) {
						p := eligibilityPayer(t, level, declared)
						var got exchangeRecords
						p.g.cfg.ExchangeObserved = got.observe
						if declared {
							p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + dtrFrameMember)
						}
						ans := p.send(t, "coverage-eligibility", "", []byte(form.wrap(req)))
						rec := got.only(t)
						if row.find == "" {
							if ans.status != http.StatusOK {
								t.Fatalf("control: answer %d %s", ans.status, ans.body)
							}
							return
						}
						if n := p.partner.calls.Load(); n != 0 {
							t.Fatalf("the payer's system was called %d times", n)
						}
						want, rule := row.message, RefusalAuthority
						switch {
						case row.repeats:
							rule = RefusalIntegrity
						case form.name == "in a Bundle" && row.name == "type named in another case":
							// An entry with no typed resource: the Bundle was
							// refused so before.
							want, rule = "parse member failed", ExchangeOther
						}
						if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), want) {
							t.Fatalf("answer %d %s, want 400 %s", ans.status, ans.body, want)
						}
						if strings.Contains(string(ans.body), anotherHeldMember) {
							t.Fatalf("the refusal echoes a value: %s", ans.body)
						}
						wantRefusal(t, rec, http.StatusBadRequest, RefusedByPayerGateway, rule)
					})
				}
			}
		}
	}
}

// A request's contained resources are read only through a "#id" patient
// reference. A request whose patient is named by such a reference, with its
// contained resources named in another case, names no patient: both
// gateways refuse it, at every level. A request naming its patient otherwise
// is not read through them: named in another case, they are a member no
// gateway reads for its patient, so the payer's gateway carries the request
// at every level (its conformance level judges the request's content). The
// provider's gateway reads them to route the request and refuses it there,
// as before, never routing it by another reading.
func TestEligibility_ContainedNamedInAnotherCase(t *testing.T) {
	const patient = `"patient":{"reference":"Patient/` + dtrFrameMember + `"}`
	byID := strings.Replace(string(eligibilityWithPatient(t, dtrFrameMember, "#p", containedMember("p", dtrFrameMember))), `"contained":`, `"Contained":`, 1)
	literal := strings.Replace(string(eligibilityRequest(t, dtrFrameMember)), patient, patient+`,"Contained":[{"resourceType":"Organization","id":"o"}]`, 1)
	const noPatient = `eligibility request names \"Contained\" for \"contained\": member names are case-sensitive, so the request names no patient to bind`
	for _, level := range allLevels {
		for _, declared := range []bool{false, true} {
			path := map[bool]string{false: "records", true: "forward"}[declared]
			t.Run("payer/#id/"+path+"/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, declared)
				var got exchangeRecords
				p.g.cfg.ExchangeObserved = got.observe
				ans := p.send(t, "coverage-eligibility", "", []byte(byID))
				if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), noPatient) || p.partner.calls.Load() != 0 {
					t.Fatalf("answer %d %s, want 400 %s", ans.status, ans.body, noPatient)
				}
				wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, RefusalAuthority)
			})
			t.Run("payer/literal/"+path+"/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, declared)
				answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
				p.partner.respByPath[eligibilityPath] = answer
				ans := p.send(t, "coverage-eligibility", "", []byte(literal))
				if ref, err := ParseCoverageEligibilityResponsePatient(ans.body); ans.status != http.StatusOK || err != nil || ref != "Patient/"+dtrFrameMember {
					t.Fatalf("answer %d %s, want answered about the member", ans.status, ans.body)
				}
				if declared && string(p.partner.lastBody) != literal {
					t.Fatalf("the payer's system received %s, want the request as sent", p.partner.lastBody)
				}
			})
		}
		t.Run("provider/#id/"+level.String(), func(t *testing.T) {
			env, rec, x := runEligibilityIngress(t, level, []byte(byID), nil)
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, noPatient)
			wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
		})
		t.Run("provider/literal/"+level.String(), func(t *testing.T) {
			env, rec, _ := runEligibilityIngress(t, level, []byte(literal), nil)
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, `"parse eligibility request failed"`)
		})
	}
}
