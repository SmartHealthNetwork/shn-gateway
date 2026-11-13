//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Every module 1 completion site on the provider-data lane reads the evidence
// the order names through evidenceReadByMember with the member's record id
// (sorID) beside the member: a record the system holds about that record id
// rides the PAS request it sends, re-pointed to the member by E-06. The rows
// in this file pin each site the provider-data lane reaches, through its own
// route, with the member's Patient held under a record id of its own, beside
// the control (the record id is the member id). The PAS tail (UC-04's
// provider-data submission and the order dispatch) and the direct completion
// are pinned in workstream_pa_evidence_record_id_test.go.
//
// Three completion sites are not reached on the provider-data lane, and off
// that lane the completion edits nothing: UC-03's oxygen and bridge arms
// (handleUC03Oxygen, handleUC03Bridge), as its /scenario/uc03 is the order
// dispatch (originateDispatch); and handleUC04's pend-then-amend tail, as its
// provider-data UC-04 is a single shot through the PAS tail.

// recordIDRecordEdits fails unless the exchange record r names want as the
// edits made to the request it sent, and the leg's sealed event names them
// too, as the UC-04 row asserts them.
func recordIDRecordEdits(t *testing.T, r ExchangeRecord, events *capturedEvents, leg string, want []string) {
	t.Helper()
	if !slices.Equal(r.RequestEdits, want) || !slices.Equal(r.Edits, want) {
		t.Fatalf("recorded request edits %v edits %v, want %v", r.RequestEdits, r.Edits, want)
	}
	var sealed capturedEvents
	for _, e := range events.of("leg.sealed") {
		if e.LegType == leg {
			sealed.events = append(sealed.events, e)
		}
	}
	wantEventEdits(t, &sealed, "leg.sealed", want)
}

// wantE06 is the edits a request carrying the evidence names: E-06 when the
// system holds the member under a record id of its own, none otherwise.
func wantE06(member, recordID string) []string {
	if recordID == member {
		return nil
	}
	return []string{string(relay.EditEvidenceSubjectRekey)}
}

// lastCaptured is the last request payload capture saw on leg.
func lastCaptured(t *testing.T, capture *capturingTransport, leg string) []byte {
	t.Helper()
	got := capture.get(leg)
	if len(got) == 0 {
		t.Fatalf("no %s captured", leg)
	}
	return got[len(got)-1]
}

// attestingPHG registers a PHG on g's network and answers its patient-DTR
// leg as the PHG does: the patient-authored item for the linkId the request
// asks about, attested by the patient it names (handlePatientDTRInbound).
func attestingPHG(t *testing.T, g *Gateway, stub *pendResumeSubstrate) {
	t.Helper()
	encPub, encPriv := genKeyPair(t)
	signPub, _ := genED25519(t)
	g.cfg.Reg.Set("trust-phg", shnsdk.RegistryEntry{ID: "trust-phg", Role: "phg", EncPub: encPub, SignPub: signPub,
		RequestFrames: []string{shnsdk.RequestFrameV1, shnsdk.RequestFrameV1Op}})
	stub.encKeys["trust-phg"] = encPair{pub: encPub, priv: encPriv}
	stub.overrideResponse = func(leg string, payload []byte) []byte {
		if leg != "patient-dtr" {
			return payload
		}
		reqs := stub.requests["patient-dtr"]
		var asked patientDTRRequest
		if len(reqs) == 0 || json.Unmarshal(reqs[len(reqs)-1], &asked) != nil {
			return nil
		}
		item, err := shnsdk.BuildPatientAttestedItem(asked.LinkID, asked.Answer, asked.PatientRef, stub.clock().Format("2006-01-02"))
		if err != nil {
			return nil
		}
		out, _ := json.Marshal(patientDTRResponse{AttestedItem: item})
		return out
	}
}

// resumeRun is one provider-data pend and resume: the resume's answer, the
// exchange records it produced, and what it sent.
type resumeRun struct {
	complete *httptest.ResponseRecorder
	recs     []ExchangeRecord
	stub     *pendResumeSubstrate
	capture  *capturingTransport
	events   *capturedEvents
	// legsAtResume is the legs the pend sent, before the resume ran.
	legsAtResume int
}

// runProviderDataResume drives scenario ("uc06" or "uc07") on the
// provider-data lane through its own routes: /start to the payer's pend, then
// /complete, whose ClaimUpdate is completed with the evidence the order
// names. The system holds the member's Patient under recordID and the
// evidence about Patient/<recordID>; atResume, when set, is the evidence it
// holds by the time the resume reads it, and when set but empty, the system
// holds none by then.
func runProviderDataResume(t *testing.T, scenario, member, recordID string, level ConformanceEnforcement, atResume []byte) resumeRun {
	return runProviderDataResumeFailing(t, scenario, member, recordID, level, atResume, nil)
}

// runProviderDataResumeFailing is runProviderDataResume whose system, by the
// time the resume reads it, fails the evidence read with readFails when it is
// set.
func runProviderDataResumeFailing(t *testing.T, scenario, member, recordID string, level ConformanceEnforcement, atResume []byte, readFails error) resumeRun {
	t.Helper()
	demo := Demo{BirthDate: "1957-11-03", FamilyName: "Okonkwo-HomeHealth"}
	tuple := DemoOrderCodes().UC06
	if scenario == "uc07" {
		demo = Demo{BirthDate: "1960-08-14", FamilyName: "Nwosu-PatientAttest"}
		tuple = DemoOrderCodes().UC07
	}
	order := withReason(t, buildAuthoredQROrderWithID(t, "sr-"+scenario+"-evidence", "Patient/"+member, tuple.System, tuple.Code, tuple.Display, tuple.Dx))
	gw, stub, capture := newAuthoredQRPendFixture(t, member, demo, order, "functional-status")
	if scenario == "uc07" {
		attestingPHG(t, gw, stub)
	}
	mountOriginationRoutes(gw)
	gw.cfg.ConformanceEnforcement = level
	useRecordIDSoR(t, gw, member, recordID, demo, "member's own Coverage", "Patient/"+recordID)

	start, _ := serveRecorded(t, gw, "POST /scenario/"+scenario+"/start", testRouteBody(gw, "/scenario/"+scenario+"/start", ""))
	var started startResp
	if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
		t.Fatalf("POST /scenario/%s/start = %d %s, want 200 pended with a resume token", scenario, start.Code, start.Body)
	}
	carriesEvidenceFor(t, "the pended PAS claim", lastCaptured(t, capture, "pas-claim"), member)
	// The start's observe-level checks run after its answer and read the
	// gateway's config; let them finish before the resume's system and sink
	// replace it.
	gw.drainObserveChecks()

	switch {
	case atResume == nil:
	case len(atResume) == 0:
		delete(gw.cfg.SoR.(*recordIDSoR).evidence, "Condition/cond-reason")
	default:
		gw.cfg.SoR.(*recordIDSoR).evidence["Condition/cond-reason"] = atResume
	}
	if readFails != nil {
		gw.cfg.SoR = evidenceReadFails{refReadFails{SystemOfRecord: gw.cfg.SoR, ref: "Condition/cond-reason"}, readFails}
	}
	run := resumeRun{stub: stub, capture: capture, events: &capturedEvents{}, legsAtResume: len(stub.legTypes)}
	gw.cfg.Diagnostic = run.events.sink
	run.complete, run.recs = serveRecorded(t, gw, "POST /scenario/"+scenario+"/complete",
		testRouteBody(gw, "/scenario/"+scenario+"/complete", `"resumeToken":`+jsonQuote(started.ResumeToken)))
	return run
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The resume's ClaimUpdate (completeClinician for UC-06, completePatient for
// UC-07) reads the order's evidence the way the submission did, with the
// record id the pend kept: on the provider-data lane, the member's Patient
// held under a record id of its own, the update carries the record about that
// id re-pointed to the member, and its exchange record and sealed leg name
// E-06. Its control: the system holds the member under the member id, and
// the update carries the record as held, naming no edit.
func TestProviderData_TheResumeCarriesTheEvidenceUnderTheMembersRecordID(t *testing.T) {
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	for _, row := range []struct{ scenario, member string }{
		{"uc06", "MBR-PD-UC06"},
		{"uc07", "MBR-PD-UC07"},
	} {
		for _, recordID := range []string{"pat-66", row.member} {
			t.Run(row.scenario+"/record "+recordID, func(t *testing.T) {
				t.Parallel()
				run := runProviderDataResume(t, row.scenario, row.member, recordID, EnforcementNone, nil)
				if run.complete.Code != http.StatusOK || !legAttempted(run.stub.legTypes[run.legsAtResume:], "pas-claim-update") {
					t.Fatalf("POST /scenario/%s/complete = %d %s legs %v, want 200 with the ClaimUpdate sent", row.scenario, run.complete.Code, run.complete.Body, run.stub.legTypes)
				}
				carriesEvidenceFor(t, "the ClaimUpdate", lastCaptured(t, run.capture, "pas-claim-update"), row.member)
				r := onlyRecord(t, run.recs)
				if r.Outcome != ExchangeAnswered {
					t.Fatalf("recorded %s on %q, want the payer's answer", r.Outcome, r.Rule)
				}
				if r.Ownership != "authored" || r.Builder != string(relay.BuilderSDKPASUpdate) {
					t.Fatalf("recorded ownership %q builder %q, want authored by %s", r.Ownership, r.Builder, relay.BuilderSDKPASUpdate)
				}
				recordIDRecordEdits(t, r, run.events, "pas-claim-update", wantE06(row.member, recordID))
			})
		}
	}
}

// REJECTION, at every level: by the time the resume reads it, the system's
// record the order names has one of the shapes the submission's completion
// decides (evidenceSubjectRows), under either record id. The resume's
// completion answers it as the submission's does
// (TestProviderData_EvidenceAboutAnotherPatientIsRefused), recorded the same
// way whatever the resume sent before it, and no ClaimUpdate is sent. The
// UC-06 resume sends no leg before its completion; the UC-07 resume's
// patient-DTR leg, sent before, stands.
func TestProviderData_TheResumeRefusesEvidenceAboutAnotherPatient(t *testing.T) {
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	for _, sc := range []struct{ scenario, member string }{
		{"uc06", "MBR-PD-UC06"},
		{"uc07", "MBR-PD-UC07"},
	} {
		for _, row := range evidenceSubjectRows(sc.member) {
			ids := []string{"pat-66", sc.member}
			if row.ownRecordIDOnly {
				ids = ids[:1]
			}
			for _, recordID := range ids {
				for _, level := range allLevels {
					t.Run(sc.scenario+"/"+row.name+"/record "+recordID+"/"+level.String(), func(t *testing.T) {
						t.Parallel()
						atResume, readFails := mutatedEvidence(t, row, recordID)
						if atResume == nil {
							atResume = []byte{} // the system holds none by then
						}
						run := runProviderDataResumeFailing(t, sc.scenario, sc.member, recordID, level, atResume, readFails)
						wantEvidenceAnswer(t, "POST /scenario/"+sc.scenario+"/complete", run.complete.Code, run.complete.Body.String(), row)
						resumed := run.stub.legTypes[run.legsAtResume:]
						if legAttempted(resumed, "pas-claim-update") || len(run.capture.get("pas-claim-update")) != 0 {
							t.Fatalf("resume legs %v: want no ClaimUpdate sent", resumed)
						}
						wantCarried(t, run.complete.Body.String(), resumed...)
						wantEvidenceRecorded(t, onlyRecord(t, run.recs), row)
					})
				}
			}
		}
	}
}

// siteRun is one provider-data origination route's answer, the records it
// produced, and the legs it sent.
type siteRun struct {
	rec     *httptest.ResponseRecorder
	recs    []ExchangeRecord
	events  *capturedEvents
	capture *capturingTransport
	legs    func() []string
}

// serveSite serves route on g with the member's Patient held under recordID
// and the evidence about Patient/<recordID>, at level.
func serveSite(t *testing.T, g *Gateway, capture *capturingTransport, legs func() []string, route, member, recordID string, demo Demo, level ConformanceEnforcement) siteRun {
	t.Helper()
	mountOriginationRoutes(g)
	g.cfg.ConformanceEnforcement = level
	useRecordIDSoR(t, g, member, recordID, demo, "member's own Coverage", "Patient/"+recordID)
	run := siteRun{events: &capturedEvents{}, capture: capture, legs: legs}
	g.cfg.Diagnostic = run.events.sink
	_, path, _ := strings.Cut(route, " ")
	run.rec, run.recs = serveRecorded(t, g, route, testRouteBody(g, path, ""))
	return run
}

// wantCarriedOn fails unless the route answered 200 having sent each of legs,
// each carrying the evidence about the member, and its exchange record names
// E-06 exactly when the system holds the member under a record id of its own,
// as does each leg's sealed event.
func (run siteRun) wantCarriedOn(t *testing.T, member, recordID string, legs ...string) {
	t.Helper()
	if run.rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s legs %v, want 200", run.rec.Code, run.rec.Body, run.legs())
	}
	want := wantE06(member, recordID)
	for _, leg := range legs {
		if !legAttempted(run.legs(), leg) {
			t.Fatalf("legs %v: no %s sent", run.legs(), leg)
		}
		carriesEvidenceFor(t, "the "+leg, lastCaptured(t, run.capture, leg), member)
		var sealed capturedEvents
		for _, e := range run.events.of("leg.sealed") {
			if e.LegType == leg {
				sealed.events = append(sealed.events, e)
			}
		}
		wantEventEdits(t, &sealed, "leg.sealed", want)
	}
	r := onlyRecord(t, run.recs)
	if !slices.Equal(r.RequestEdits, want) || !slices.Equal(r.Edits, want) {
		t.Fatalf("recorded request edits %v edits %v, want %v", r.RequestEdits, r.Edits, want)
	}
}

// facilityAnswer is metro-spine's answer to the federated query task asks, as
// the facility's gateway builds it (workstream_fqs_records.go): the operative
// report about patientRef and its source Provenance, whose agent is the
// facility's holder identifier, citing the consent.
func facilityAnswer(t *testing.T, task []byte, patientRef string, now time.Time) []byte {
	t.Helper()
	report, err := testOwnReport("dr-uc05-operative", patientRef, "72148", "MRI lumbar spine w/o contrast")
	if err != nil {
		t.Fatal(err)
	}
	prov, err := buildEvidenceProvenance("record-provenance-0", "DiagnosticReport/dr-uc05-operative",
		"http://smarthealth.network/ids/holder", "metro-spine", "Consent/consent-linda-treat", shnsdk.PurposeTreatment, now)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := shnsdk.BuildRecordsBundle([][]byte{report, prov})
	if err != nil {
		t.Fatal(err)
	}
	out, err := shnsdk.BuildCDexQueryResult(task, inner)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// UC-05 on the provider-data lane: the submission the payer pends and the
// ClaimUpdate carrying the facility's report (handleUC05's two completions)
// each carry the order's evidence about the member's record id, re-pointed
// to the member; the control, the member id.
func TestProviderData_UC05CarriesTheEvidenceUnderTheMembersRecordID(t *testing.T) {
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	const member = "MBR-PD-UC05"
	demo := Demo{BirthDate: "1955-02-09", FamilyName: "Velazquez-Federated"}
	uc05 := DemoOrderCodes().UC05
	for _, recordID := range []string{"pat-55", member} {
		t.Run("record "+recordID, func(t *testing.T) {
			t.Parallel()
			order := withReason(t, buildAuthoredQROrderWithID(t, "sr-uc05-evidence", "Patient/"+member, uc05.System, uc05.Code, uc05.Display, uc05.Dx))
			g, stub, capture := newAuthoredQRPendFixture(t, member, demo, order, "operative-diagnostic-report")
			encPub, encPriv := genKeyPair(t)
			signPub, _ := genED25519(t)
			g.cfg.Reg.Set("metro-spine", shnsdk.RegistryEntry{ID: "metro-spine", Role: "facility", EncPub: encPub, SignPub: signPub,
				RequestFrames: []string{shnsdk.RequestFrameV1, shnsdk.RequestFrameV1Op}})
			stub.encKeys["metro-spine"] = encPair{pub: encPub, priv: encPriv}
			stub.overrideResponse = func(leg string, payload []byte) []byte {
				if leg != "federated-query" {
					return payload
				}
				reqs := stub.requests["federated-query"]
				return facilityAnswer(t, reqs[len(reqs)-1], stub.patientRef, stub.clock())
			}
			run := serveSite(t, g, capture, func() []string { return stub.legTypes }, "POST /scenario/uc05", member, recordID, demo, EnforcementNone)
			run.wantCarriedOn(t, member, recordID, "pas-claim", "pas-claim-update")
		})
	}
}

// UC-07's HCPCS arm on the provider-data lane (handleUC07HCPCS): its PAS
// claim carries the order's evidence about the member's record id,
// re-pointed to the member; the control, the member id.
func TestProviderData_UC07HCPCSCarriesTheEvidenceUnderTheMembersRecordID(t *testing.T) {
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	const member = "MBR-UC07HCPCS"
	demo := Demo{BirthDate: "1977-01-30", FamilyName: "Nakamura"}
	tuple := DemoOrderCodes().UC07HCPCS
	for _, recordID := range []string{"pat-70", member} {
		t.Run("record "+recordID, func(t *testing.T) {
			t.Parallel()
			order := withReason(t, buildAuthoredQROrderWithID(t, "sr-uc07hcpcs-evidence", "Patient/"+member, tuple.System, tuple.Code, tuple.Display, tuple.Dx))
			g, stub, capture := newAuthoredQRSingleShotFixture(t, member, demo, order)
			run := serveSite(t, g, capture, func() []string { return stub.legTypes }, "POST /scenario/uc07hcpcs", member, recordID, demo, EnforcementNone)
			run.wantCarriedOn(t, member, recordID, "pas-claim")
		})
	}
}

// UC-08 on the provider-data lane (handleUC08): the payer's coverage answer
// is not-covered, so no questionnaire is fetched and the PAS claim, which
// the payer denies, is completed with no questionnaire response
// (completeAuthoredPASRequest's completePASRequest). It carries the order's
// evidence about the member's record id, re-pointed to the member; the
// control, the member id.
func TestProviderData_UC08CarriesTheEvidenceUnderTheMembersRecordID(t *testing.T) {
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	const member = "MBR-PD-UC08"
	demo := Demo{BirthDate: "1962-07-14", FamilyName: "Nakamura"}
	uc08 := DemoOrderCodes().UC08
	for _, recordID := range []string{"pat-88", member} {
		t.Run("record "+recordID, func(t *testing.T) {
			t.Parallel()
			order := withReason(t, buildAuthoredQROrderWithID(t, "sr-uc08-evidence", "Patient/"+member, uc08.System, uc08.Code, uc08.Display, uc08.Dx))
			g, stub, capture := newAuthoredQRPendFixture(t, member, demo, order, "")
			stub.overrideResponse = func(leg string, payload []byte) []byte {
				switch leg {
				case "crd-order-select":
					return crdAnswerFor(shnsdk.CardCoverage{Covered: shnsdk.CoveredNotCovered, PANeeded: shnsdk.PANeededNoAuth})
				case "pas-claim":
					return deniedClaimResponse(reviewActionExt("A3", "Not Certified", "", ""), nil)
				}
				return payload
			}
			run := serveSite(t, g, capture, func() []string { return stub.legTypes }, "POST /scenario/uc08", member, recordID, demo, EnforcementNone)
			if legAttempted(stub.legTypes, "dtr-questionnaire-fetch") {
				t.Fatalf("legs %v: a not-covered order fetches no questionnaire", stub.legTypes)
			}
			run.wantCarriedOn(t, member, recordID, "pas-claim")
			if !strings.Contains(run.rec.Body.String(), `"denied":true`) {
				t.Fatalf("answer %s, want the payer's denial", run.rec.Body)
			}
		})
	}
}
