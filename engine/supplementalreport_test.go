package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// renamedReportSoR is the census system of record, except that it holds
// member's patient under sorID and holds report as member's supplemental
// report (none when report is nil).
type renamedReportSoR struct {
	*censusSoR
	member, sorID string
	report        []byte
}

func (s *renamedReportSoR) PatientFHIRRef(m string) (string, bool) {
	if m == s.member {
		return "Patient/" + s.sorID, true
	}
	return s.censusSoR.PatientFHIRRef(m)
}

func (s *renamedReportSoR) SupplementalReport(m string) ([]byte, bool) {
	if m == s.member {
		return s.report, s.report != nil
	}
	return s.censusSoR.SupplementalReport(m)
}

// failingReportSoR fails the reads the supplemental report needs.
type failingReportSoR struct {
	*prefetchSoR
	reportErr error
	noReport  bool
}

func (s *failingReportSoR) SupplementalReportContext(ctx context.Context, m string) ([]byte, bool, error) {
	if s.reportErr != nil {
		return nil, false, s.reportErr
	}
	if s.noReport {
		return nil, false, nil
	}
	return []byte(`{"resourceType":"DiagnosticReport","id":"d","subject":{"reference":"Patient/` + prefetchSoRID + `"}}`), true, nil
}

// supplementalReportAsHeld is a report laid out the way no encoder would
// write it: members out of order, whitespace, escapes, a number lexeme and a
// subject display, all of which must survive the edit.
func supplementalReportAsHeld(subjectRef string) string {
	bs := string(rune(92))
	return "{ \"subject\" : { \"display\":\"A " + bs + "u00e9 " + bs + "u003c\" ,\n  \"reference\" :  \"" + subjectRef + "\" },\n" +
		"  \"resourceType\":\"DiagnosticReport\",\"id\":\"dr-7\",\"status\":\"final\"," +
		"\"extension\":[{\"url\":\"urn:x\",\"valueDecimal\":1.50E+0}],\"code\":{\"coding\":[{\"system\":\"http://loinc.org\",\"code\":\"11504-8\"}]} }"
}

func reportGateway(sor SystemOfRecord) *Gateway { return &Gateway{cfg: Config{SoR: sor}} }

// TestSupplementalReport_SubjectRekeyedOnly: a report the provider's system
// holds under its own Patient id reaches the claim update with only its
// subject.reference changed, to the member's network patient (E-06); every
// other byte is the system's.
func TestSupplementalReport_SubjectRekeyedOnly(t *testing.T) {
	held := supplementalReportAsHeld("Patient/pat-7")
	g := reportGateway(&renamedReportSoR{censusSoR: newCensusSoR(), member: "MBR-UC04", sorID: "pat-7", report: []byte(held)})
	got, status, msg := g.supplementalReport(context.Background(), "MBR-UC04", "pat-7")
	if status != 0 {
		t.Fatalf("refused: %d %s", status, msg)
	}
	want := strings.Replace(held, `"Patient/pat-7"`, `"Patient/MBR-UC04"`, 1)
	if string(got) != want {
		t.Fatalf("the report changed beyond its subject.reference:\n got %s\nwant %s", got, want)
	}
}

// TestSupplementalReport_MemberNamedReportExact: a system that names the
// patient by the member id gets its report sent exactly as held.
func TestSupplementalReport_MemberNamedReportExact(t *testing.T) {
	held := supplementalReportAsHeld("Patient/MBR-UC04")
	g := reportGateway(&renamedReportSoR{censusSoR: newCensusSoR(), member: "MBR-UC04", sorID: "MBR-UC04", report: []byte(held)})
	got, status, msg := g.supplementalReport(context.Background(), "MBR-UC04", "MBR-UC04")
	if status != 0 || string(got) != held {
		t.Fatalf("want the report exactly as held: %d %s\n got %s", status, msg, got)
	}
}

// TestSupplementalReport_Refusals: E-06 re-points only a report that names
// the member's patient in subject.reference; everything else is refused,
// with the reason and without a patient in it.
func TestSupplementalReport_Refusals(t *testing.T) {
	held := supplementalReportAsHeld("Patient/pat-7")
	for _, tc := range []struct {
		name   string
		report string
		status int
		msg    string
	}{
		{"another patient", supplementalReportAsHeld("Patient/pat-8"), http.StatusUnprocessableEntity, msgSupplementalOtherSubject},
		{"the member id, where the system names the patient otherwise", supplementalReportAsHeld("Patient/MBR-UC04"), http.StatusUnprocessableEntity, msgSupplementalOtherSubject},
		{"an absolute reference", supplementalReportAsHeld("https://ehr.example/fhir/Patient/pat-7"), http.StatusUnprocessableEntity, msgSupplementalOtherSubject},
		{"a Group", supplementalReportAsHeld("Group/pat-7"), http.StatusUnprocessableEntity, msgSupplementalOtherSubject},
		{"no subject", `{"resourceType":"DiagnosticReport","id":"dr-7","status":"final"}`, http.StatusUnprocessableEntity, msgSupplementalNoSubject},
		{"a subject with no reference", `{"resourceType":"DiagnosticReport","id":"dr-7","subject":{"identifier":{"system":"urn:shn:member","value":"MBR-UC04"}}}`, http.StatusUnprocessableEntity, msgSupplementalNoSubject},
		{"a reference that is not a string", `{"resourceType":"DiagnosticReport","id":"dr-7","subject":{"reference":7}}`, http.StatusUnprocessableEntity, msgSupplementalNoSubject},
		{"a subject that is not an object", `{"resourceType":"DiagnosticReport","id":"dr-7","subject":["Patient/pat-7"]}`, http.StatusUnprocessableEntity, msgSupplementalNoSubject},
		{"not a resource", `["Patient/pat-7"]`, http.StatusBadGateway, msgSupplementalUnreadable},
		{"not JSON", `{"resourceType":`, http.StatusBadGateway, msgSupplementalUnreadable},
		{"a repeated member", strings.Replace(held, `"status":"final"`, `"status":"final","status":"final"`, 1), http.StatusBadGateway, msgSupplementalUnreadable},
		{"a signature over the report", strings.Replace(held, `"status":"final"`,
			`"status":"final","modifierExtension":[{"url":"urn:x","valueSignature":{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-01-01T00:00:00Z","who":{"display":"a clinician"}}}]`, 1),
			http.StatusUnprocessableEntity, "signed content cannot be edited (E-06, Signature in DiagnosticReport)"},
		{"a contained signed Provenance that targets the report", strings.Replace(held, `"status":"final"`,
			`"status":"final","contained":[`+signedProvenance("DiagnosticReport/dr-7")+`]`, 1),
			http.StatusUnprocessableEntity, "signed content cannot be edited (E-06, Provenance.signature)"},
		{"a repeated subject", strings.Replace(held, `"status":"final"`, `"subject":{"reference":"Patient/pat-7"}`, 1), http.StatusBadGateway, msgSupplementalUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := reportGateway(&renamedReportSoR{censusSoR: newCensusSoR(), member: "MBR-UC04", sorID: "pat-7", report: []byte(tc.report)})
			got, status, msg := g.supplementalReport(context.Background(), "MBR-UC04", "pat-7")
			if status != tc.status || msg != tc.msg || got != nil {
				t.Fatalf("got %d %q %s, want %d %q", status, msg, got, tc.status, tc.msg)
			}
			if strings.Contains(msg, "pat-") || strings.Contains(msg, "MBR-") {
				t.Fatalf("a refusal names no patient: %q", msg)
			}
		})
	}
}

// signedProvenance is a signed Provenance that targets ref.
func signedProvenance(ref string) string {
	return `{"resourceType":"Provenance","id":"prov-1","target":[{"reference":"` + ref + `"}],"recorded":"2026-01-01T00:00:00Z",` +
		`"agent":[{"who":{"display":"a clinician"}}],"signature":[{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],` +
		`"when":"2026-01-01T00:00:00Z","who":{"display":"a clinician"}}]}`
}

// TestSupplementalReport_SignatureCoveringOnlyAContainedResource: a signature
// that covers only a resource the report contains, not the report, leaves
// the report's subject free to re-point, and the contained resource exact.
func TestSupplementalReport_SignatureCoveringOnlyAContainedResource(t *testing.T) {
	for _, tc := range []struct{ name, contained string }{
		{"a Signature in a contained resource", `{"resourceType":"Observation","id":"obs-1","status":"final",` +
			`"modifierExtension":[{"url":"urn:x","valueSignature":{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-01-01T00:00:00Z","who":{"display":"a clinician"}}}]}`},
		{"a signed Provenance that targets a contained resource", `{"resourceType":"Observation","id":"obs-1","status":"final"},` + signedProvenance("#obs-1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held := strings.Replace(supplementalReportAsHeld("Patient/pat-7"), `"status":"final"`, `"status":"final","contained":[`+tc.contained+`]`, 1)
			g := reportGateway(&renamedReportSoR{censusSoR: newCensusSoR(), member: "MBR-UC04", sorID: "pat-7", report: []byte(held)})
			got, status, msg := g.supplementalReport(context.Background(), "MBR-UC04", "pat-7")
			if status != 0 {
				t.Fatalf("refused: %d %s", status, msg)
			}
			if want := strings.Replace(held, `"Patient/pat-7"`, `"Patient/MBR-UC04"`, 1); string(got) != want {
				t.Fatalf("the report changed beyond its subject.reference:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestSupplementalReport_SystemOfRecordFailures: a failed or empty read
// refuses the update with the system-of-record answer.
func TestSupplementalReport_SystemOfRecordFailures(t *testing.T) {
	unavailable := &SoRReadError{Kind: SoRUnavailable}
	for _, tc := range []struct {
		name   string
		sor    *failingReportSoR
		status int
		msg    string
	}{
		{"report read fails", &failingReportSoR{prefetchSoR: newPrefetchSoR(), reportErr: unavailable}, http.StatusServiceUnavailable, unavailable.Error()},
		{"no report", &failingReportSoR{prefetchSoR: newPrefetchSoR(), noReport: true}, http.StatusInternalServerError, "no supplemental report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, status, msg := reportGateway(tc.sor).supplementalReport(context.Background(), prefetchMember, prefetchSoRID)
			if status != tc.status || msg != tc.msg {
				t.Fatalf("got %d %q, want %d %q", status, msg, tc.status, tc.msg)
			}
		})
	}
}

// TestSupplementalReport_CheckAdmitsOnlyE06: the report leaves the edit
// either exactly as held or with E-06 alone.
func TestSupplementalReport_CheckAdmitsOnlyE06(t *testing.T) {
	body := relay.NewBody([]byte(`{"subject":{"reference":"Patient/a"},"x":1}`), relay.OriginUpstreamResponse)
	doc, err := relay.Doc(body)
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := doc.Member(doc.Root(), "subject")
	ref, _ := doc.Member(subject, "reference")
	x, _ := doc.Member(doc.Root(), "x")
	if err := supplementalReportCheck(relay.Exact(body, "application/fhir+json")); err != nil {
		t.Fatalf("the report as held: %v", err)
	}
	e06, err := relay.Apply(body, "application/fhir+json", relay.EditEvidenceSubjectRekey, doc.Replace(ref, []byte(`"Patient/b"`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := supplementalReportCheck(e06); err != nil {
		t.Fatalf("E-06 alone: %v", err)
	}
	for name, p := range map[string]func() (relay.Payload, error){
		"another edit": func() (relay.Payload, error) {
			return relay.Apply(body, "application/fhir+json", relay.EditPayorEdgeRestamp, doc.Replace(ref, []byte(`"Patient/b"`)))
		},
		"E-06 with another edit": func() (relay.Payload, error) {
			return relay.ApplyChanges(body, "application/fhir+json",
				relay.Change{Edit: relay.EditEvidenceSubjectRekey, Ops: []relay.Op{doc.Replace(ref, []byte(`"Patient/b"`))}},
				relay.Change{Edit: relay.EditPayorEdgeRestamp, Ops: []relay.Op{doc.Replace(x, []byte(`2`))}})
		},
	} {
		pl, err := p()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := supplementalReportCheck(pl); err == nil {
			t.Fatalf("%s was admitted", name)
		}
		if _, err := relay.Transmit(pl, supplementalReportCheck); err == nil {
			t.Fatalf("%s was transmitted", name)
		}
	}
}

// TestSupplementalReport_NativeUpdateNeverRekeyed: a claim update the
// provider's own client sends through the provider ingress is carried
// exactly below strict, even when its report names the Patient the
// provider's system holds the member under, and refused at strict by the
// provider's content rule rather than re-pointed: E-06 is made only to a
// claim update this gateway builds.
func TestSupplementalReport_NativeUpdateNeverRekeyed(t *testing.T) {
	report := supplementalReportAsHeld("Patient/pat-covered")
	body := levelPASBundleWithEntry(`{"resource":` + report + `}`)
	body = strings.Replace(body, `{"resourceType":"Claim",`, `{"resourceType":"Claim","related":[{"claim":{"identifier":{"value":"PRIOR-1"}}}],`, 1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.SoR = &renamedReportSoR{censusSoR: newCensusSoR(), member: "MBR-COVERED", sorID: "pat-covered", report: []byte(report)}
			env.originator.cfg.ConformanceEnforcement = level
			env.payerReturns(LegResult{Response: testResponse(levelPASPayerAnswer)})
			rec := httptest.NewRecorder()
			env.originator.handlePASIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(body)))
			if refusesAt(level, RulePatientMixed) {
				// The provider's own opt-in content rule refuses a bundle whose
				// report names another patient reference; nothing is sent, and
				// nothing is re-pointed to make it pass.
				if rec.Code != http.StatusForbidden || env.routeHitCount() != 0 {
					t.Fatalf("at %s want the %s refusal before the network: %d %s, network hits %d",
						level, RulePatientMixed, rec.Code, rec.Body.String(), env.routeHitCount())
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("the update was not carried: %d %s", rec.Code, rec.Body.String())
			}
			exs := env.originator.ExchangeSnapshot()
			if legs := exs[len(exs)-1].Legs; legs[len(legs)-1].Type != "pas-claim-update" {
				t.Fatalf("the amendment routed %s, want pas-claim-update", legs[len(legs)-1].Type)
			}
			wantCarriedExactly(t, env, body)
		})
	}
}
