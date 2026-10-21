package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// pastTheStrictLimits are the strict reader's own limits, each as a member a
// request carries in a field its patient is not read through.
var pastTheStrictLimits = map[string]string{
	"invalid UTF-8 in another field": "\"\xff\"",
	"a lone surrogate escape":        `"\ud800"`,
	"nesting past the strict bound":  strings.Repeat("[", 70) + strings.Repeat("]", 70),
}

// pastTheTokenBound is a member holding more tokens than the strict reader's
// bound: a body of about 2 MB, so it has rows of its own.
var pastTheTokenBound = "[" + strings.Repeat("0,", 1_000_000) + "0]"

// containedAs is body with its contained member set to v.
func containedAs(t *testing.T, body []byte, v any) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	doc["contained"] = v
	return mustJSON(t, doc)
}

// withNoteMember is body with a "note" member holding raw, added last.
func withNoteMember(body []byte, raw string) []byte {
	return append(append(append([]byte{}, body[:len(body)-1]...), []byte(`,"note":`+raw)...), '}')
}

// A request the strict reader refuses only for its own limits is read as one
// it can read: its patient reference in every form names its member, a "#id"
// one through the contained resources by their exact member names, on the
// payer's records and forward paths, at every level. (The provider's ingress
// routes only a request the strict reader reads, so it refuses such a request
// as routing, as before.)
func TestEligibility_PatientReferenceFormsPastTheStrictReadersLimits(t *testing.T) {
	for limit, raw := range pastTheStrictLimits {
		for name, form := range patientForms(dtrFrameMember) {
			for _, level := range eligibilityLevels {
				t.Run("records/own member/"+name+"/"+limit+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, false)
					body := withNoteMember(eligibilityWithPatient(t, dtrFrameMember, form.ref, form.contained...), raw)
					run := runObserved(t, p, "coverage-eligibility", body, p.pci)
					if ref, err := ParseCoverageEligibilityResponsePatient(run.answer.body); run.answer.status != http.StatusOK || err != nil || ref != "Patient/"+dtrFrameMember {
						t.Fatalf("answer %d %.300s, want answered about the member", run.answer.status, run.answer.body)
					}
					if run.differs != 0 {
						t.Fatalf("the member was bound to another patient (%d binding-differs events)", run.differs)
					}
				})
				t.Run("forward/"+name+"/"+limit+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, true)
					answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
					p.partner.respByPath[eligibilityPath] = answer
					body := withNoteMember(eligibilityWithPatient(t, dtrFrameMember, form.ref, form.contained...), raw)
					run := runObserved(t, p, "coverage-eligibility", body, p.pci)
					if run.answer.status != http.StatusOK || !bytes.Equal(run.answer.body, answer) {
						t.Fatalf("answer %d %.300s, want the payer's answer relayed", run.answer.status, run.answer.body)
					}
					if !bytes.Equal(p.partner.lastBody, body) {
						t.Fatal("the payer's endpoint did not receive the request as sent")
					}
					if run.differs != 0 {
						t.Fatalf("the member was bound to another patient (%d binding-differs events)", run.differs)
					}
				})
			}
		}
		for name, form := range patientForms(anotherHeldMember) {
			t.Run("records/another member/"+name+"/"+limit, func(t *testing.T) {
				p := eligibilityPayer(t, EnforcementStrict, false)
				body := withNoteMember(eligibilityWithPatient(t, anotherHeldMember, form.ref, form.contained...), raw)
				run := runObserved(t, p, "coverage-eligibility", body, p.pci)
				if ref, err := ParseCoverageEligibilityResponsePatient(run.answer.body); run.answer.status != http.StatusOK || err != nil || ref != "Patient/"+anotherHeldMember {
					t.Fatalf("answer %d %.300s, want carried about %s", run.answer.status, run.answer.body, anotherHeldMember)
				}
				if run.differs != 1 {
					t.Fatalf("%d binding-differs events, want the difference observed once", run.differs)
				}
			})
		}
	}
}

// Past the strict reader's limits, as within them, a "#id" reference that no
// contained Patient answers names no patient (contained resources of another
// JSON type hold none), and contained resources named only in another case
// are read one way by no reader: each is refused before anything is read for
// the patient, at every level, by each gateway.
func TestEligibility_UnresolvedContainedPatientPastTheStrictReadersLimitsIsRefused(t *testing.T) {
	const parse = "parse member failed"
	const named = "member names are case-sensitive, so the request names no patient to bind"
	rows := []struct {
		name, want, rule string
		body             func(member string) []byte
	}{
		{"contained id not carried", parse, ExchangeOther, func(member string) []byte {
			return eligibilityWithPatient(t, member, "#nobody", containedMember("pat", member))
		}},
		{"no contained resources", parse, ExchangeOther, func(member string) []byte {
			return eligibilityWithPatient(t, member, "#pat")
		}},
		{"contained named in another case", named, RefusalAuthority, func(member string) []byte {
			return bytes.Replace(eligibilityWithPatient(t, member, "#pat", containedMember("pat", member)), []byte(`"contained":`), []byte(`"Contained":`), 1)
		}},
		{"contained an object", parse, ExchangeOther, func(member string) []byte {
			return containedAs(t, eligibilityWithPatient(t, member, "#pat"), containedMember("pat", member))
		}},
		{"contained a string", parse, ExchangeOther, func(member string) []byte {
			return containedAs(t, eligibilityWithPatient(t, member, "#pat"), "pat")
		}},
	}
	limits := map[string]string{"within the strict limits": `"a note"`}
	for limit, raw := range pastTheStrictLimits {
		limits[limit] = raw
	}
	for limit, raw := range limits {
		for _, row := range rows {
			for _, level := range eligibilityLevels {
				t.Run(limit+"/"+row.name+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, false)
					var got exchangeRecords
					p.g.cfg.ExchangeObserved = got.observe
					body := withNoteMember(row.body(dtrFrameMember), raw)
					ans := p.sendAs(t, "coverage-eligibility", "", body, p.pci)
					if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), row.want) {
						t.Fatalf("answer %d %s, want 400 %s", ans.status, ans.body, row.want)
					}
					if row.rule != ExchangeOther {
						wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, row.rule)
					}
				})
				t.Run("forward/"+limit+"/"+row.name+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, true)
					p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + dtrFrameMember)
					ans := p.sendAs(t, "coverage-eligibility", "", withNoteMember(row.body(dtrFrameMember), raw), p.pci)
					if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), row.want) {
						t.Fatalf("answer %d %s, want 400 %s", ans.status, ans.body, row.want)
					}
					if p.partner.lastBody != nil {
						t.Fatal("the refused request reached the payer's system")
					}
				})
				t.Run("provider/"+limit+"/"+row.name+"/"+level.String(), func(t *testing.T) {
					env, rec, x := runEligibilityIngress(t, level, withNoteMember(row.body("MBR-COVERED"), raw), nil)
					refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, row.want)
					if row.rule != ExchangeOther {
						wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, row.rule)
					}
				})
			}
		}
	}
}

// Past the strict reader's token bound, as past its other limits, a "#id"
// patient resolves through the contained resources, and one no contained
// Patient answers is refused, at every level.
func TestEligibility_ContainedPatientPastTheTokenBound(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run("resolved/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			body := withNoteMember(eligibilityWithPatient(t, dtrFrameMember, "#pat", containedMember("pat", dtrFrameMember)), pastTheTokenBound)
			run := runObserved(t, p, "coverage-eligibility", body, p.pci)
			if ref, err := ParseCoverageEligibilityResponsePatient(run.answer.body); run.answer.status != http.StatusOK || err != nil || ref != "Patient/"+dtrFrameMember || run.differs != 0 {
				t.Fatalf("answer %d %.300s (%d binding-differs events), want answered about the member", run.answer.status, run.answer.body, run.differs)
			}
		})
		t.Run("unresolved/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			body := withNoteMember(eligibilityWithPatient(t, dtrFrameMember, "#nobody", containedMember("pat", dtrFrameMember)), pastTheTokenBound)
			ans := p.sendAs(t, "coverage-eligibility", "", body, p.pci)
			if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), "parse member failed") {
				t.Fatalf("answer %d %.300s, want 400 parse member failed", ans.status, ans.body)
			}
		})
	}
}

// msgReplacedCharacter is the refusal of a request whose member value carries a character
// a reader replaced.
const msgReplacedCharacter = "eligibility request names its patient by a value carrying a replaced character (not well-formed UTF-8 or a lone surrogate escape): readers read it two ways, so the request names no patient to bind"

// Past the strict reader's limits a request is read by a reader that replaces each byte of
// not-well-formed UTF-8, and each lone surrogate escape, with U+FFFD, so member values that
// differ on the wire would bind as one. A request whose member value carries such a
// character names no patient one way only: it is refused with the patient-match rule, on
// the literal and the contained path, at every level, by each gateway, before anything is
// read. A member value carrying U+FFFD as sent, in a body past only the nesting bound, is
// read as before.
func TestEligibility_ReplacedCharacterInTheMemberPastTheStrictLimits(t *testing.T) {
	const placeholder = "MBR-PLACEHOLDER-2581"
	paths := map[string]func() []byte{
		"literal": func() []byte {
			return eligibilityWithPatient(t, placeholder, "Patient/"+placeholder)
		},
		"contained": func() []byte {
			return eligibilityWithPatient(t, placeholder, "#pat", containedMember("pat", placeholder))
		},
	}
	replaced := map[string]string{
		"a lone surrogate escape": dtrFrameMember + `\ud800`,
		"invalid UTF-8":           dtrFrameMember + "\xff",
	}
	for path, body := range paths {
		for form, value := range replaced {
			req := bytes.ReplaceAll(body(), []byte(placeholder), []byte(value))
			for _, level := range eligibilityLevels {
				t.Run("records/"+path+"/"+form+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, false)
					var got exchangeRecords
					p.g.cfg.ExchangeObserved = got.observe
					ans := p.sendAs(t, "coverage-eligibility", "", req, p.pci)
					if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), msgReplacedCharacter) {
						t.Fatalf("answer %d %s, want 400 %s", ans.status, ans.body, msgReplacedCharacter)
					}
					wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, RefusalAuthority)
				})
				t.Run("forward/"+path+"/"+form+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, true)
					p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + dtrFrameMember)
					ans := p.sendAs(t, "coverage-eligibility", "", req, p.pci)
					if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), msgReplacedCharacter) {
						t.Fatalf("answer %d %s, want 400 %s", ans.status, ans.body, msgReplacedCharacter)
					}
					if p.partner.lastBody != nil {
						t.Fatal("the refused request reached the payer's system")
					}
				})
				t.Run("provider/"+path+"/"+form+"/"+level.String(), func(t *testing.T) {
					env, rec, x := runEligibilityIngress(t, level, req, nil)
					refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, msgReplacedCharacter)
					wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
				})
			}
		}
		// U+FFFD as sent reads one way, wherever else the body is past the strict reader's
		// limits: in a body past only the nesting bound, and beside a byte or escape a
		// reader replaces in a field the patient is not read through. The request is read
		// as before: carried to the payer's system, and refused for no replaced character.
		for limit, note := range map[string]string{
			"past only the nesting bound":       pastTheStrictLimits["nesting past the strict bound"],
			"beside a lone surrogate elsewhere": `"\ud800"`,
			"beside invalid UTF-8 elsewhere":    "\"\xff\"",
		} {
			req := withNoteMember(bytes.ReplaceAll(body(), []byte(placeholder), []byte(dtrFrameMember+`\ufffd`)), note)
			for _, level := range eligibilityLevels {
				t.Run("U+FFFD as sent/"+limit+"/"+path+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, true)
					p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + dtrFrameMember)
					ans := p.sendAs(t, "coverage-eligibility", "", req, p.pci)
					if strings.Contains(string(ans.body), msgReplacedCharacter) || !bytes.Equal(p.partner.lastBody, req) {
						t.Fatalf("forward: answer %d %s, want the request carried to the payer's system", ans.status, ans.body)
					}
					records := eligibilityPayer(t, level, false)
					if ans := records.sendAs(t, "coverage-eligibility", "", req, records.pci); strings.Contains(string(ans.body), msgReplacedCharacter) {
						t.Fatalf("records: answer %d %s, want no replaced-character refusal", ans.status, ans.body)
					}
					if _, rec, _ := runEligibilityIngress(t, level, req, nil); strings.Contains(rec.Body.String(), msgReplacedCharacter) {
						t.Fatalf("provider: answer %d %s, want no replaced-character refusal", rec.Code, rec.Body)
					}
				})
			}
		}
	}
}

// replacesCharacters finds the bodies a reader replaces characters of: not well-formed
// UTF-8, or a lone surrogate escape; a pair, an escaped backslash and U+FFFD as sent are
// none.
func TestReplacesCharacters(t *testing.T) {
	for body, want := range map[string]bool{
		`{"a":"MBR"}`:                 false,
		"{\"a\":\"MBR\xff\"}":         true,
		`{"a":"MBR\ud800"}`:           true,
		`{"a":"MBR\udc00"}`:           true,
		`{"a":"MBR\ud800x"}`:          true,
		`{"a":"MBR\ud83d\ude00"}`:     false,
		`{"a":"MBR\\ud800"}`:          false,
		`{"a":"MBR\\\ud800"}`:         true,
		`{"a":"MBR\ufffd"}`:           false,
		`{"a":"MBR\uD800"}`:           true,
		`{"a":"MBR\uD83D\uDE00"}`:     false,
		`{"\ud800":"MBR"}`:            true,
		`{"a":"\ud800\ud800\udc00"}`:  true,
		`{"a":"\ud800\\u"}`:           true,
		`{"a":"\udbff\udfff"}`:        false,
		"{\"a\":\"\xed\xa0\x80\"}":    true,
		"{\"a\":\"MBR\xef\xbf\xbd\"}": false,
	} {
		if got := replacesCharacters([]byte(body)); got != want {
			t.Errorf("%q: %v, want %v", body, got, want)
		}
	}
}

// Only the bytes a member value is read from decide whether a reader replaced a character
// in it: a member carrying U+FFFD as sent binds, in a body past the strict reader's limits,
// whatever another field holds (a value of another JSON type, a name in another case, or
// another Patient's replaced value); a member read from a replaced byte or escape, on
// either path, is refused.
func TestEligibilityRequestMember_ReadsOnlyTheMemberValueForReplacedCharacters(t *testing.T) {
	binds := map[string]string{
		"literal, contained an object":                             `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"Patient/MBR\ufffd"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":{"x":1},"note":"\ud800"}`,
		"literal, contained a list of strings":                     `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"Patient/MBR\ufffd"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":["x"],"note":"\ud800"}`,
		"#id, a string beside the Patient":                         `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"#pat"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":["x",{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR\ufffd"}]}],"note":"\ud800"}`,
		"#id, an Observation with a numeric id beside the Patient": `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"#pat"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR\ufffd"}]},{"resourceType":"Observation","id":7}],"note":"\ud800"}`,
		"#id, an identifier with a numeric system":                 `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"#pat"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR\ufffd"},{"system":5,"value":"x"}]}],"note":"\ud800"}`,
		"#id, a member-system value that is an object":             `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"#pat"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR\ufffd"},{"system":"urn:shn:member","value":{"x":"\udc00"}}]}],"note":"\ud800"}`,
		"#id, a Value spelled in another case":                     `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"#pat"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR\ufffd"},{"system":"urn:shn:member","Value":"\udc00"}]}],"note":"\ud800"}`,
		"#id, an identifier in another system replaced":            `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"#pat"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR\ufffd"},{"system":"urn:example:mrn","value":"MRN\udc00"}]}],"note":"\ud800"}`,
		"#id, another Patient's member value replaced":             `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"#pat"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR\ufffd"}]},{"resourceType":"Patient","id":"other","identifier":[{"system":"urn:shn:member","value":"MBR\udc00"}]}],"note":"\ud800"}`,
	}
	for name, body := range binds {
		if member, err := eligibilityRequestMember([]byte(body)); err != nil || member != "MBR\ufffd" {
			t.Errorf("%s: member %q (err %v), want the member as sent", name, member, err)
		}
	}
	refused := map[string]string{
		"literal, a lone surrogate in the member":                    `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"Patient/MBR\ud800"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"note":"\ud800"}`,
		"#id, a lone surrogate in the member":                        `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"#pat"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR\ud800"}]}],"note":"\ud800"}`,
		"#id, a second member identifier replaced to the same value": `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],"patient":{"reference":"#pat"},"created":"2026-10-19","insurer":{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}},"contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR\ufffd"},{"system":"urn:shn:member","value":"MBR\ud800"}]}],"note":"\ud800"}`,
	}
	for name, body := range refused {
		if member, err := eligibilityRequestMember([]byte(body)); !errors.Is(err, errReplacedCharacter) {
			t.Errorf("%s: member %q (err %v), want refused for a replaced character", name, member, err)
		}
	}
}
