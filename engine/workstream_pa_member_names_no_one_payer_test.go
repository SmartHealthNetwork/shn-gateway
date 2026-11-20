//go:build !shn_coreonly

package engine

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// On the payer's inbound legs, a request whose patient is named by a
// reference that names no one is refused before the payer's system is asked,
// at every level. Where the payer binds the member the request names
// (bindInboundSubject: a PAS submission, a CRD order-dispatch) it answers
// 400 msgMemberNamesNoOne recorded as authority, where earlier releases
// answered "unknown member" with no rule for "Patient/". Where the leg reads
// its patient first, its own refusal answers: order-select's patient-mixed
// check, the PAS inquiry's missing Claim.patient, and the questionnaire
// package's Patient reference; a PAS bundle at strict is refused by the
// patient-mixed check first.
func TestPayerInboundRefusesAPatientReferenceNamingNoOne(t *testing.T) {
	t.Parallel()
	sel := conformantCRD("MBR-COVERED", "72148")
	base := pasIngressBundle("00001", "")
	type want struct {
		status int
		msg    string
	}
	none := want{http.StatusBadRequest, msgMemberNamesNoOne}
	for _, ref := range []string{"Patient/", "Patient/ ", "Patient/Patient/", " "} {
		id := strings.TrimPrefix(ref, "Patient/")
		readsAsNoPatient := ref == "Patient/" || ref == "Patient/Patient/" // no id once the last "Patient/" is read
		rows := map[string]struct {
			leg, operation string
			body           []byte
			want           func(level ConformanceEnforcement) want
		}{
			"crd-order-dispatch": {leg: "crd-order-dispatch", body: crdDispatchWith(t, `"patientId":"MBR-COVERED"`, `"patientId":"`+ref+`"`),
				want: func(ConformanceEnforcement) want { return none }},
			"pas-claim": {leg: "pas-claim", body: []byte(strings.Replace(base, `"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}`, `"resourceType":"Claim","patient":{"reference":"`+ref+`"}`, 1)),
				want: func(level ConformanceEnforcement) want {
					if level == EnforcementStrict {
						return want{http.StatusForbidden, "inconsistent patient in PAS bundle"}
					}
					return none
				}},
			"pas-claim-update": {leg: "pas-claim-update", body: updateBundleSetting(t, "Claim", "patient", map[string]any{"reference": ref}),
				want: func(level ConformanceEnforcement) want {
					if level == EnforcementStrict {
						return want{http.StatusForbidden, "inconsistent patient in PAS bundle"}
					}
					return none
				}},
			"crd-order-select": {leg: "crd-order-select", body: bytes.Replace(sel, []byte(`"patientId":"MBR-COVERED"`), []byte(`"patientId":"`+ref+`"`), 1),
				want: func(level ConformanceEnforcement) want {
					if ref == "Patient/" || level == EnforcementStrict {
						return want{http.StatusBadRequest, "inconsistent patient in order-select"}
					}
					return none
				}},
			"pas-claim-inquire": {leg: "pas-claim-inquire", body: inquiryBundle(id, id, "TRN-1", "72148"),
				want: func(ConformanceEnforcement) want {
					if readsAsNoPatient {
						return want{http.StatusBadRequest, "PAS inquiry Bundle missing Claim.patient"}
					}
					return none
				}},
			"dtr-questionnaire-package": {leg: "dtr-questionnaire-fetch", operation: "questionnaire-package", body: dtrPackage(resourceParam("coverage", dtrCoverage("cov-1", id))),
				want: func(ConformanceEnforcement) want {
					if readsAsNoPatient {
						return want{http.StatusBadRequest, "questionnaire-package request names a coverage beneficiary or order subject that is not a Patient reference"}
					}
					return none
				}},
		}
		for name, row := range rows {
			for _, level := range allLevels {
				t.Run(name+"/"+strings.ReplaceAll(ref, " ", "_")+"/"+level.String(), func(t *testing.T) {
					p, got := newRecordingLevelPayer(t, level)
					soleMember(t, p.g.cfg.SoR, "MBR-COVERED")
					ans := p.sendAs(t, row.leg, row.operation, row.body, p.pci)
					w := row.want(level)
					if text := answerText(string(ans.body)); ans.status != w.status || text != w.msg {
						t.Fatalf("answer %d %q; want %d %q", ans.status, text, w.status, w.msg)
					}
					if p.partner.lastPath != "" {
						t.Fatalf("a refused request reached the payer's system at %s", p.partner.lastPath)
					}
					x := got.only(t)
					if x.Outcome != ExchangeRefused {
						t.Fatalf("recorded %s (rule %q), want refused", x.Outcome, x.Rule)
					}
					if w == none {
						wantRefusal(t, x, http.StatusBadRequest, partyAnswering(row.leg), RefusalAuthority)
					}
				})
			}
		}
	}
}

// A CRD order-select request that names its patient nowhere (its order and
// coverage read as no one too): an absent or empty context.patientId is
// refused 400 "missing context.patientId" on authority, as order-dispatch
// answers it; a context.patientId that names no one ("Patient/") is the
// binding's, 400 msgMemberNamesNoOne on authority. At every level, before the
// payer's system is asked.
func TestPayerOrderSelectNamingItsPatientNowhere(t *testing.T) {
	t.Parallel()
	blank := conformantCRD("", "72148") // patientId "", order subject and beneficiary "Patient/"
	for name, row := range map[string]struct {
		body []byte
		msg  string
	}{
		"empty patientId":     {blank, "missing context.patientId"},
		"absent patientId":    {bytes.Replace(blank, []byte(`"patientId":"",`), nil, 1), "missing context.patientId"},
		"patientId Patient/":  {bytes.Replace(blank, []byte(`"patientId":""`), []byte(`"patientId":"Patient/"`), 1), msgMemberNamesNoOne},
		"patientId Patient/ ": {bytes.Replace(blank, []byte(`"patientId":""`), []byte(`"patientId":"Patient/ "`), 1), msgMemberNamesNoOne},
	} {
		if bytes.Equal(row.body, blank) != (name == "empty patientId") {
			t.Fatalf("%s: the fixture was not edited", name)
		}
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				soleMember(t, p.g.cfg.SoR, "MBR-COVERED")
				ans := p.sendAs(t, "crd-order-select", "", row.body, p.pci)
				msg := row.msg
				// A blank patientId differs from the blank member its order and
				// coverage read as: at strict the patient-mixed check refuses it
				// first.
				mixed := name == "patientId Patient/ " && level == EnforcementStrict
				if mixed {
					msg = "inconsistent patient in order-select"
				}
				if text := answerText(string(ans.body)); ans.status != http.StatusBadRequest || text != msg {
					t.Fatalf("answer %d %q; want 400 %q", ans.status, text, msg)
				}
				if p.partner.lastPath != "" {
					t.Fatalf("a refused request reached the payer's system at %s", p.partner.lastPath)
				}
				if !mixed {
					wantRefusal(t, got.only(t), http.StatusBadRequest, partyAnswering("crd-order-select"), RefusalAuthority)
				}
			})
		}
	}
}

// A CRD order-dispatch whose context.patientId is absent or empty is refused
// 400 "missing context.patientId" on authority, at every level, as order-select
// refuses one naming its patient nowhere.
func TestPayerOrderDispatchMissingPatientIsRefusedOnAuthority(t *testing.T) {
	t.Parallel()
	for name, body := range map[string][]byte{
		"empty patientId":  crdDispatchWith(t, `"patientId":"MBR-COVERED"`, `"patientId":""`),
		"absent patientId": crdDispatchWith(t, `"patientId":"MBR-COVERED",`, ``),
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				ans := p.sendAs(t, "crd-order-dispatch", "", body, p.pci)
				if text := answerText(string(ans.body)); ans.status != http.StatusBadRequest || text != "missing context.patientId" {
					t.Fatalf("answer %d %q; want 400 %q", ans.status, text, "missing context.patientId")
				}
				if p.partner.lastPath != "" {
					t.Fatalf("a refused request reached the payer's system at %s", p.partner.lastPath)
				}
				wantRefusal(t, got.only(t), http.StatusBadRequest, partyAnswering("crd-order-dispatch"), RefusalAuthority)
			})
		}
	}
}
