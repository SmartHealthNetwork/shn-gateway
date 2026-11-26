//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// pasRefBody is the payer-side body of leg (the inquiry, a submission or an
// update) whose Claim.patient is ref, with claimExtra written into the Claim
// before it (a contained list, say), its other resources naming member.
func pasRefBody(t *testing.T, leg, member, ref, claimExtra string) []byte {
	t.Helper()
	var body, old, repl string
	switch leg {
	case "pas-claim-inquire":
		body = string(inquiryBundle(member, "", "TRN-1", "72148"))
		old = `"patient":{"reference":"Patient/` + member + `"},`
		repl = claimExtra + `"patient":{"reference":"` + ref + `"},`
	case "pas-claim":
		body = strings.ReplaceAll(pasIngressBundle("00001", ""), "MBR-COVERED", member)
		old = `"resourceType":"Claim","patient":{"reference":"Patient/` + member + `"}`
		repl = `"resourceType":"Claim",` + claimExtra + `"patient":{"reference":"` + ref + `"}`
	default:
		t.Fatalf("no fixture for %s", leg)
	}
	if strings.Count(body, old) != 1 {
		t.Fatalf("fixture: %s's Claim.patient is not written once", leg)
	}
	return []byte(strings.Replace(body, old, repl, 1))
}

// A Claim.patient that names no Patient by FHIR's reference grammar names no
// one: a payer's gateway refuses it 400 on authority at every level,
// binds nothing, records no content finding and reaches nothing of the
// participant's, whatever else the request carries and whatever the token's
// patient is. "Patient/" found inside another reference is no Patient
// reference.
func TestPASClaimPatient_NamingNoPatientIsRefusedAtEveryLevel(t *testing.T) {
	t.Parallel()
	const a, b = dtrFrameMember, anotherHeldMember // a is the token's patient
	containedB := `"contained":[{"resourceType":"Patient","id":"Patient/` + b + `","identifier":[{"system":"urn:shn:member","value":"` + b + `"}]}],`
	containedOrg := `"contained":[{"resourceType":"Organization","id":"o1"}],`
	twoContained := `"contained":[{"resourceType":"Patient","id":"p1","identifier":[{"system":"urn:shn:member","value":"` + b + `"}]},{"resourceType":"Patient","id":"p1"}],`
	for _, row := range []struct{ name, ref, extra string }{
		{"XPatient/a", "XPatient/" + a, ""},
		{"urn:Patient/b", "urn:Patient/" + b, ""},
		{"Basic/1/_history/Patient/b", "Basic/1/_history/Patient/" + b, ""},
		{"Group/1/Patient/b", "Group/1/Patient/" + b, ""},
		{"#Patient/b, nothing contained", "#Patient/" + b, ""},
		{"#Patient/b, a Patient contained under that id", "#Patient/" + b, containedB},
		{"Patient/Patient/", "Patient/Patient/", ""},
		{"Patient/", "Patient/", ""},
		{"Patient/b/x, a trailing path", "Patient/" + b + "/x", ""},
		{"Patient/$everything, an operation", "Patient/$everything", ""},
		{"Patient/b%2Fx, an escaped character", "Patient/" + b + "%2Fx", ""},
		{"a URL whose base holds Patient, naming a Practitioner", "https://h.example/Patient/fhir/Practitioner/9", ""},
		{"Practitioner/Patient/_history/7", "Practitioner/Patient/_history/7", ""},
		{"Group/b, another type", "Group/" + b, ""},
		{"#o1, a contained Organization", "#o1", containedOrg},
		{"#p1, two contained resources", "#p1", twoContained},
		{"#p9, nothing contained under it", "#p9", ""},
		{"a urn:uuid: no entry carries", "urn:uuid:9b2c6a1e-0d2f-4b8e-9a51-3c1f2e7d8a10", ""},
	} {
		for _, leg := range []string{"pas-claim-inquire", "pas-claim"} {
			for _, level := range allLevels {
				t.Run(row.name+"/"+leg+"/"+level.String(), func(t *testing.T) {
					p, got := newRecordingLevelPayer(t, level)
					ans := p.sendAs(t, leg, "", pasRefBody(t, leg, a, row.ref, row.extra), p.pci)
					p.wantRefused(t, ans, http.StatusBadRequest, msgMemberNamesNoOne)
					wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, RefusalAuthority)
					if fs := p.content(); len(fs) != 0 {
						t.Fatalf("a refusal on authority records no content finding, got %v", findingsText(fs))
					}
				})
			}
		}
	}
}

// An inquiry whose Claim.patient's id is only whitespace while it also names
// another patient names no one: refused 400 on authority at every level,
// before the inquiry's consistency is checked, as the SDK's Responder
// refuses it. Before, strict answered 403
// "inconsistent patient in PAS inquiry" first.
func TestPASInquiry_BlankPatientBesideAnotherIsRefusedAsNamingNoOne(t *testing.T) {
	t.Parallel()
	body := strings.Replace(string(pasRefBody(t, "pas-claim-inquire", dtrFrameMember, "Patient/ ", "")),
		`"beneficiary":{"reference":"Patient/`+dtrFrameMember+`"}`, `"beneficiary":{"reference":"Patient/`+anotherHeldMember+`"}`, 1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			ans := p.sendAs(t, "pas-claim-inquire", "", []byte(body), p.pci)
			p.wantRefused(t, ans, http.StatusBadRequest, msgMemberNamesNoOne)
			wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, RefusalAuthority)
		})
	}
}

// A Claim.patient naming one Patient by structure, by an id that is not a FHIR
// id, binds the member as written: carried at none and observe (where
// observe records the inquiry's or submission's own shape), and refused 400 as
// that shape at structural and strict. No PAS reference carried before is
// refused below structural.
func TestPASClaimPatient_IDNotAFHIRIDIsTheRequestsShape(t *testing.T) {
	t.Parallel()
	const member = "MBR_COVERED" // not held: bound from the request
	for _, tc := range []struct{ leg, msg string }{
		{"pas-claim-inquire", msgInquiryPatientIDNotFHIRID},
		{"pas-claim", msgPASPatientIDNotFHIRID},
	} {
		for _, level := range allLevels {
			t.Run(tc.leg+"/"+level.String(), func(t *testing.T) {
				p, _ := newRecordingLevelPayer(t, level)
				ans := p.sendAs(t, tc.leg, "", pasRefBody(t, tc.leg, member, "Patient/"+member, ""), p.pci)
				switch level {
				case EnforcementNone, EnforcementObserve:
					if ans.status != http.StatusOK || p.partner.lastPath == "" {
						t.Fatalf("answer %d %s (reached %q), want it carried", ans.status, ans.body, p.partner.lastPath)
					}
					if fs := p.content(); level == EnforcementObserve && !hasFinding(fs, RuleRequestShape) {
						t.Fatalf("at observe want a %s finding, got %v", RuleRequestShape, findingsText(fs))
					}
				default:
					p.wantRefused(t, ans, http.StatusBadRequest, tc.msg)
				}
			})
		}
	}
}

func hasFinding(fs []ConformanceFinding, rule string) bool {
	for _, f := range fs {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

// An inquiry's Claim.patient may name its Patient as FHIR resolves a
// reference inside a Bundle: by the urn:uuid: fullUrl of the one
// Patient entry, or by "#<id>" for the one Patient the Claim contains. Each
// binds that Patient's member at every level, as "Patient/<id>" does.
func TestPASInquiry_ClaimPatientResolvedInTheBundleBindsItsMember(t *testing.T) {
	t.Parallel()
	const m = dtrFrameMember
	contained := `"contained":[{"resourceType":"Patient","id":"p1","identifier":[{"system":"urn:shn:member","value":"` + m + `"}]}],`
	containedNamedPatient := `"contained":[{"resourceType":"Patient","id":"Patient","identifier":[{"system":"urn:shn:member","value":"` + m + `"}]}],`
	for _, row := range []struct{ name, ref, extra string }{
		{"the urn:uuid: of the Patient entry", "urn:uuid:patient", ""},
		{"a contained Patient", "#p1", contained},
		{"a contained Patient whose id is Patient", "#Patient", containedNamedPatient},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				ans := p.sendAs(t, "pas-claim-inquire", "", pasRefBody(t, "pas-claim-inquire", m, row.ref, row.extra), p.pci)
				if ans.status != http.StatusOK || p.partner.lastPath == "" {
					t.Fatalf("answer %d %s (reached %q), want it bound and carried", ans.status, ans.body, p.partner.lastPath)
				}
				if rec := got.only(t); rec.Outcome != ExchangeAnswered {
					t.Fatalf("record %+v", rec)
				}
			})
		}
	}
	// A urn:uuid: naming an entry that is no Patient, or two entries, names no one.
	for _, row := range []struct{ name, ref, from, to string }{
		{"an Organization entry", "urn:uuid:provider-org", "", ""},
		{"two entries under one fullUrl", "urn:uuid:patient", `{"fullUrl":"urn:uuid:provider-org"`, `{"fullUrl":"urn:uuid:patient"`},
	} {
		t.Run(row.name, func(t *testing.T) {
			body := string(pasRefBody(t, "pas-claim-inquire", m, row.ref, ""))
			if row.from != "" {
				body = strings.Replace(body, row.from, row.to, 1)
			}
			p, _ := newRecordingLevelPayer(t, EnforcementNone)
			p.wantRefused(t, p.sendAs(t, "pas-claim-inquire", "", []byte(body), p.pci), http.StatusBadRequest, msgMemberNamesNoOne)
		})
	}
}

// The structural reading does not over-reach: a Claim.patient in FHIR's
// absolute form, under any base (no base is matched against the sender's or
// this gateway's), versioned or not, and the relative versioned form, each
// bind the member they name on a submission, an update and an inquiry, at
// none and observe. The payer requires known members here, so a reference
// read as any other member would be refused "unknown member".
func TestPASClaimPatient_WellFormedFormsUnderAnyBaseBindTheirMember(t *testing.T) {
	t.Parallel()
	updateRef := func(t *testing.T) string {
		raw, _ := updateBundle(t)
		var b struct {
			Entry []struct {
				Resource struct {
					ResourceType string `json:"resourceType"`
					Patient      struct {
						Reference string `json:"reference"`
					} `json:"patient"`
				} `json:"resource"`
			} `json:"entry"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		for _, e := range b.Entry {
			if e.Resource.ResourceType == "Claim" {
				return e.Resource.Patient.Reference
			}
		}
		t.Fatal("fixture: the amendment carries no Claim")
		return ""
	}
	for _, form := range []struct{ name, prefix, suffix string }{
		{"absolute", "https://ehr.example/fhir/Patient/", ""},
		{"absolute, versioned", "https://ehr.example/fhir/Patient/", "/_history/3"},
		{"absolute under another base, with a port", "https://other-host.example:8443/r4/base/Patient/", ""},
		{"absolute under another base, versioned", "http://10.0.0.5/Patient/", "/_history/3"},
		{"absolute under a base with an escape", "https://h.example/fhir%20r4/Patient/", ""},
		{"relative, versioned", "Patient/", "/_history/3"},
	} {
		for _, leg := range []string{"pas-claim", "pas-claim-update", "pas-claim-inquire"} {
			for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
				t.Run(form.name+"/"+leg+"/"+level.String(), func(t *testing.T) {
					p := newLevelPayer(t, level)
					p.g.cfg.RequireKnownMembers = true
					var body []byte
					switch leg {
					case "pas-claim-update":
						old := updateRef(t)
						member := old[strings.LastIndex(old, "/")+1:]
						if old != "Patient/"+member {
							t.Fatalf("fixture: the amendment names its patient %q", old)
						}
						var related string
						body = updateBundleSetting(t, "Claim", "patient", map[string]any{"reference": form.prefix + member + form.suffix})
						_, related = updateBundle(t)
						p.seedPend(t, related)
					default:
						body = pasRefBody(t, leg, dtrFrameMember, form.prefix+dtrFrameMember+form.suffix, "")
					}
					ans := p.send(t, leg, "", body)
					if ans.status != http.StatusOK || p.partner.lastPath == "" {
						t.Fatalf("answer %d %s (reached %q), want the member bound and the request carried", ans.status, ans.body, p.partner.lastPath)
					}
					if fs := p.content(); len(fs) != 0 {
						t.Fatalf("a well-formed reference records no content finding, got %v", findingsText(fs))
					}
				})
			}
		}
	}
}

// A patient reference elsewhere in a PAS request that carries a "Patient"
// segment but names no Patient by FHIR's reference grammar is compared as
// written by the patient-consistency check, so it is another patient than
// the member: refused 403 where the participant opts into that check
// (strict), and carried where it does not.
func TestPASClaimPatient_MalformedPatientReferenceElsewhereIsAnotherPatient(t *testing.T) {
	t.Parallel()
	const m = dtrFrameMember
	for _, ref := range []string{"Patient/OTHER/", "Basic/1/Patient/OTHER", "Patient/OTHER/_history/", "Patient/%2F" + m} {
		for _, tc := range []struct{ leg, msg string }{
			{"pas-claim-inquire", "inconsistent patient in PAS inquiry"},
			{"pas-claim", "inconsistent patient in PAS bundle"},
		} {
			body := string(pasRefBody(t, tc.leg, m, "Patient/"+m, ""))
			old := `"beneficiary":{"reference":"Patient/` + m + `"}`
			if strings.Count(body, old) != 1 {
				t.Fatalf("fixture: %s's beneficiary is not written once", tc.leg)
			}
			body = strings.Replace(body, old, `"beneficiary":{"reference":"`+ref+`"}`, 1)
			for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementStrict} {
				t.Run(tc.leg+"/"+ref+"/"+level.String(), func(t *testing.T) {
					p, _ := newRecordingLevelPayer(t, level)
					ans := p.sendAs(t, tc.leg, "", []byte(body), p.pci)
					if level == EnforcementNone {
						if ans.status != http.StatusOK || p.partner.lastPath == "" {
							t.Fatalf("answer %d %s (reached %q), want it carried", ans.status, ans.body, p.partner.lastPath)
						}
						return
					}
					p.wantRefused(t, ans, http.StatusForbidden, tc.msg)
				})
			}
		}
	}
}

// An inquiry whose only reference to its patient is the Claim's "#<id>" to
// the one Patient it contains binds that Patient's member at every level: the
// contained Patient is a patient the inquiry names, so the patient-consistency
// check at strict finds that one member and no other.
func TestPASInquiry_OnlyAContainedPatientNamesTheMember(t *testing.T) {
	t.Parallel()
	const m = dtrFrameMember
	contained := `"contained":[{"resourceType":"Patient","id":"p1","identifier":[{"system":"urn:shn:member","value":"` + m + `"}]}],`
	body := string(pasRefBody(t, "pas-claim-inquire", m, "#p1", contained))
	for _, cut := range []struct{ old, new string }{
		{`{"fullUrl":"urn:uuid:patient","resource":{"resourceType":"Patient","id":"` + m + `"}},`, ""},
		{`"beneficiary":{"reference":"Patient/` + m + `"}`, `"beneficiary":{"identifier":{"system":"urn:shn:member","value":"` + m + `"}}`},
	} {
		if strings.Count(body, cut.old) != 1 {
			t.Fatalf("fixture: %q is not written once", cut.old)
		}
		body = strings.Replace(body, cut.old, cut.new, 1)
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, _ := newRecordingLevelPayer(t, level)
			ans := p.sendAs(t, "pas-claim-inquire", "", []byte(body), p.pci)
			if ans.status != http.StatusOK || p.partner.lastPath == "" {
				t.Fatalf("answer %d %s (reached %q), want it bound and carried", ans.status, ans.body, p.partner.lastPath)
			}
		})
	}
}

// A reference to a patient that names no Patient by FHIR's reference grammar,
// and resolves to none the request carries, leaves that patient unknown: on
// every leg that takes the involved list, the DTR questionnaire package
// included, the list names the patients the request does name, and the omission is
// recorded once, request-unreadable, never read as a request naming fewer.
// Another type's reference names no patient and records nothing.
func TestInvolved_AnUnreadablePatientReferenceIsAnOmission(t *testing.T) {
	body := func(ref string) []byte {
		return []byte(`{"hook":"order-select","context":{"patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-OX"}}},` +
			`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"` + ref + `"}}}]}}}`)
	}
	for _, row := range []struct {
		ref     string
		omitted []string
	}{
		{"Patient/OTHER/", []string{involvedOmitRequestUnreadable}},
		{"Basic/1/Patient/OTHER", []string{involvedOmitRequestUnreadable}},
		{"urn:Patient/OTHER", []string{involvedOmitRequestUnreadable}},
		{"#Patient/OTHER", []string{involvedOmitRequestUnreadable}},
		{"Patient/%2FOTHER", []string{involvedOmitRequestUnreadable}},
		{" Patient/OTHER", []string{involvedOmitRequestUnreadable}},
		{`\nPatient/OTHER`, []string{involvedOmitRequestUnreadable}}, // a JSON-escaped newline
		{"https://ehr.example/fhir/Patient%2FOTHER", []string{involvedOmitRequestUnreadable}},
		{"Patient%2FOTHER", []string{involvedOmitRequestUnreadable}},
		{"Group/1", nil},
		{"Practitioner/Patient", nil},
		{"https://h.example/Patient/fhir/Group/1", nil},
		{"#Patient", nil},
		{"https://ehr.example/fhir/Patient/MBR-OX", nil},
	} {
		for _, leg := range involvedListLegs {
			t.Run(leg+"/"+row.ref, func(t *testing.T) {
				g, _, _, omitted := involvedGateway(t, true)
				r := httptest.NewRequest(http.MethodPost, "/", nil)
				if list := g.involvedForRequest(context.Background(), r, leg, "provider-tpo", "pas-submit", "corr-1", "hash-1", body(row.ref), heldPCI(t, "MBR-COVERED")); list == "" {
					t.Fatal("no list: the patients the request names are not named")
				}
				if !slices.Equal(*omitted, row.omitted) {
					t.Fatalf("omitted %v, want %v", *omitted, row.omitted)
				}
			})
		}
	}
}
