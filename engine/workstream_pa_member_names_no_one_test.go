//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The references that name no one: the member each reads as is blank.
var namesNoOne = []string{"", "Patient/", "Patient/ ", "Patient/Patient/"}

// soleMember makes sor read the empty member id as member, as a FHIR system
// of record holding only that member answers an identifier search with an
// empty value. Every row below is read against such a system: a route that
// read its system of record for a member naming no one would bind member.
func soleMember(t *testing.T, sor SystemOfRecord, member string) {
	t.Helper()
	switch s := sor.(type) {
	case *censusSoR:
		s.emptyIs = member
	case *fqFacilitySoR:
		s.emptyIs = member
	default:
		t.Fatalf("soleMember: a %T is not a census system of record", sor)
	}
}

// statusRefused reports whether status refuses the request as the caller's:
// a 4xx, never an answer and never the system of record's failure.
func statusRefused(status int) bool { return status >= 400 && status < 500 }

// On every route that binds a request's patient, a patient reference naming
// no one is refused, at every level, and never binds the only member the
// system of record holds; a well-formed reference to that member is bound.
func TestEveryRouteRefusesAPatientReferenceNamingNoOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, level := range allLevels {
		for _, ref := range namesNoOne {
			name := level.String() + "/" + strings.ReplaceAll(ref, " ", "_")
			t.Run("provider eligibility/"+name, func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				soleMember(t, env.originator.cfg.SoR, "MBR-COVERED")
				env.payerReturns(LegResult{Response: testResponse(payersEligibilityAnswer("Patient/MBR-COVERED"))})
				rec := httptest.NewRecorder()
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(eligibilityWithPatient(t, "MBR-COVERED", ref)))
				if x := got.only(t); x.Outcome != ExchangeRefused {
					t.Fatalf("recorded %s (rule %q), want refused", x.Outcome, x.Rule)
				}
				wantNamesNoOneAnswer(t, "provider eligibility", ref, level, rec.Code, rec.Body.String())
				if !statusRefused(rec.Code) || env.routeHitCount() != 0 {
					t.Fatalf("answer %d %s, sent %d; want refused before the network", rec.Code, rec.Body, env.routeHitCount())
				}
			})
			for _, declared := range []bool{false, true} {
				t.Run("payer eligibility/"+map[bool]string{false: "records", true: "forward"}[declared]+"/"+name, func(t *testing.T) {
					p := eligibilityPayer(t, level, declared)
					soleMember(t, p.g.cfg.SoR, dtrFrameMember)
					ans := p.send(t, "coverage-eligibility", "", eligibilityWithPatient(t, dtrFrameMember, ref))
					wantNamesNoOneAnswer(t, "payer eligibility/"+map[bool]string{false: "records", true: "forward"}[declared]+"", ref, level, ans.status, string(ans.body))
					if !statusRefused(ans.status) || p.partner.calls.Load() != 0 || bytes.Contains(ans.body, []byte("CoverageEligibilityResponse")) {
						t.Fatalf("answer %d %s; want refused, the payer's system not called", ans.status, ans.body)
					}
				})
			}
			t.Run("CRD ingress, patientId/"+name, func(t *testing.T) {
				if ref == "" {
					t.Skip("an empty patientId is refused as missing (TestIngressSubjectPCI_MissingPatientId)")
				}
				sor := newCensusSoR()
				soleMember(t, sor, "MBR-COVERED")
				g := &Gateway{cfg: Config{SoR: sor, ConformanceEnforcement: level}}
				pci, status, msg := g.ingressCRDSubjectPCIContext(ctx, crdReqJSON(ref, "Patient/MBR-COVERED", "Patient/MBR-COVERED"))
				wantNamesNoOneAnswer(t, "CRD ingress, patientId", ref, level, status, msg)
				if !statusRefused(status) || pci != "" {
					t.Fatalf("status %d %q, pci %q; want refused, no one bound", status, msg, pci)
				}
			})
			t.Run("CRD ingress, an order's subject/"+name, func(t *testing.T) {
				sor := newCensusSoR()
				soleMember(t, sor, "MBR-COVERED")
				g := &Gateway{cfg: Config{SoR: sor, ConformanceEnforcement: level}}
				pci, status, msg := g.ingressCRDSubjectPCIContext(ctx, crdReqJSON("MBR-COVERED", ref, "Patient/MBR-COVERED"))
				wantNamesNoOneAnswer(t, "CRD ingress, an order's subject", ref, level, status, msg)
				wantOrderNotThePatients(t, level, status, msg, pci)
			})
			t.Run("CRD dispatch, patientId/"+name, func(t *testing.T) {
				sor := newCensusSoR()
				soleMember(t, sor, "MBR-COVERED")
				g := &Gateway{cfg: Config{SoR: sor, ConformanceEnforcement: level}}
				_, _, bound, status, msg := g.conformantCRDDispatchBindContext(ctx, crdDispatchFor(ref, "Patient/MBR-COVERED"))
				wantNamesNoOneAnswer(t, "CRD dispatch, patientId", ref, level, status, msg)
				if !statusRefused(status) || bound != "" {
					t.Fatalf("status %d %q, bound %q; want refused, no one bound", status, msg, bound)
				}
			})
			t.Run("CRD dispatch, an order's subject/"+name, func(t *testing.T) {
				sor := newCensusSoR()
				soleMember(t, sor, "MBR-COVERED")
				g := &Gateway{cfg: Config{SoR: sor, ConformanceEnforcement: level}}
				_, _, bound, status, msg := g.conformantCRDDispatchBindContext(ctx, crdDispatchFor("MBR-COVERED", ref))
				wantNamesNoOneAnswer(t, "CRD dispatch, an order's subject", ref, level, status, msg)
				wantOrderNotThePatients(t, level, status, msg, bound)
			})
			t.Run("PAS ingress/"+name, func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				soleMember(t, env.originator.cfg.SoR, "MBR-COVERED")
				env.payerReturns(LegResult{Response: testResponse([]byte(`{}`))})
				body := strings.Replace(pasIngressBundle("00001", ""), `"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}`,
					`"resourceType":"Claim","patient":{"reference":"`+ref+`"}`, 1)
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/fhir+json")
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				env.originator.ingressRoute(RoutePAS)(rec, req)
				if x := got.only(t); x.Outcome != ExchangeRefused {
					t.Fatalf("recorded %s (rule %q), want refused", x.Outcome, x.Rule)
				}
				wantNamesNoOneAnswer(t, "PAS ingress", ref, level, rec.Code, rec.Body.String())
				if !statusRefused(rec.Code) || env.routeHitCount() != 0 {
					t.Fatalf("answer %d %s, sent %d; want refused before the network", rec.Code, rec.Body, env.routeHitCount())
				}
			})
			t.Run("FQS ingress/"+name, func(t *testing.T) {
				env := newFQExchange(t, level)
				soleMember(t, env.originator.cfg.SoR, "MBR-UC05")
				env.payerReturns(LegResult{Response: testResponse([]byte(`{}`))})
				rec := httptest.NewRecorder()
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(fqTaskNaming(t, "facility", ref)))
				if x := got.only(t); x.Outcome != ExchangeRefused {
					t.Fatalf("recorded %s (rule %q), want refused", x.Outcome, x.Rule)
				}
				wantNamesNoOneAnswer(t, "FQS ingress", ref, level, rec.Code, rec.Body.String())
				if !statusRefused(rec.Code) || env.routeHitCount() != 0 {
					t.Fatalf("answer %d %s, sent %d; want refused before the network", rec.Code, rec.Body, env.routeHitCount())
				}
			})
			t.Run("FQS inbound/"+name, func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				p.answering(t, "federated-query")
				p.g.cfg.ConsentURL = "http://consent.test"
				p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
				soleMember(t, p.g.cfg.SoR, dtrFrameMember)
				ans := sendConsented(t, p, "federated-query", cdexTaskNaming(t, ref), p.pci, "Consent/c-1")
				wantNamesNoOneAnswer(t, "FQS inbound", ref, level, ans.status, string(ans.body))
				if !statusRefused(ans.status) || bytes.Contains(ans.body, []byte("DiagnosticReport")) {
					t.Fatalf("answer %d %s; want refused, nothing disclosed", ans.status, ans.body)
				}
				x := got.only(t)
				if x.Outcome != ExchangeRefused {
					t.Fatalf("recorded %s (rule %q), want refused", x.Outcome, x.Rule)
				}
				if ref != "" {
					wantRefusal(t, x, http.StatusBadRequest, partyAnswering("federated-query"), RefusalAuthority)
				}
			})
			t.Run("patient-DTR/"+name, func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				p.answering(t, "patient-dtr")
				soleMember(t, p.g.cfg.SoR, dtrFrameMember)
				ans := p.sendAs(t, "patient-dtr", "", mustJSON(t, map[string]string{"linkId": "functional-status-oswestry", "answer": "42", "patientRef": ref}), p.pci)
				wantNamesNoOneAnswer(t, "patient-DTR", ref, level, ans.status, string(ans.body))
				if ans.status != http.StatusBadRequest || bytes.Contains(ans.body, []byte("attestedItem")) {
					t.Fatalf("answer %d %s; want refused 400, nothing attested", ans.status, ans.body)
				}
				x := got.only(t)
				if x.Outcome != ExchangeRefused {
					t.Fatalf("recorded %s (rule %q), want refused", x.Outcome, x.Rule)
				}
				if ref != "" { // an empty patientRef is the leg's own missing patient
					wantRefusal(t, x, http.StatusBadRequest, partyAnswering("patient-dtr"), RefusalAuthority)
				}
			})
		}
	}
	// Controls: the same requests naming the member are bound.
	t.Run("control/CRD ingress", func(t *testing.T) {
		sor := newCensusSoR()
		soleMember(t, sor, "MBR-COVERED")
		g := &Gateway{cfg: Config{SoR: sor}}
		if pci, status, msg := g.ingressCRDSubjectPCIContext(ctx, crdReqJSON("MBR-COVERED", "Patient/MBR-COVERED", "Patient/MBR-COVERED")); status != 0 || pci == "" {
			t.Fatalf("status %d %q, pci %q; want bound", status, msg, pci)
		}
	})
	t.Run("control/CRD dispatch", func(t *testing.T) {
		sor := newCensusSoR()
		soleMember(t, sor, "MBR-COVERED")
		g := &Gateway{cfg: Config{SoR: sor}}
		if _, _, bound, status, msg := g.conformantCRDDispatchBindContext(ctx, crdDispatchFor("MBR-COVERED", "Patient/MBR-COVERED")); status != 0 || bound == "" {
			t.Fatalf("status %d %q, bound %q; want bound", status, msg, bound)
		}
	})
	t.Run("control/FQS inbound", func(t *testing.T) {
		if status, body := sendSoleMemberFacility(t, EnforcementObserve, fqTaskNaming(t, "payer", "Patient/MBR-UC05")); status != http.StatusOK || !strings.Contains(string(body), "DiagnosticReport") {
			t.Fatalf("answer %d %s; want the records", status, body)
		}
	})
	t.Run("control/PAS ingress", func(t *testing.T) {
		env := newInProcessExchange(t)
		soleMember(t, env.originator.cfg.SoR, "MBR-COVERED")
		pci, status, msg := env.originator.ingressPASNativeSubjectPCIContext(ctx, []byte(pasIngressBundle("00001", "")))
		if status != 0 || pci == "" {
			t.Fatalf("status %d %q, pci %q; want bound", status, msg, pci)
		}
	})
}

// wantOrderNotThePatients holds a request whose context names the member and
// one of whose orders names its patient by a reference naming no one: that
// order is not provably the member's, never read as the only member the system
// holds, so the patient-mixed check refuses it (403) where it refuses an order
// naming another patient, at strict. Below strict that check refuses no order,
// and the request is bound to the member its context names, never by the
// order's reference.
//
// This is deliberate. The network primitive here is the leg's subject binding,
// and it is read from the request's context patient, which a reference naming
// no one can no longer bind. Whether an order's subject agrees with it is a
// conformance check: the network carries another patient's content below the
// level that refuses it, as it carries any order naming another patient. What
// this change closes is the blank reference reading as the very patient the
// leg is bound to.
func wantOrderNotThePatients(t *testing.T, level ConformanceEnforcement, status int, msg, pci string) {
	t.Helper()
	member, _, _ := newCensusSoR().ResolvePatient("MBR-COVERED")
	if level != EnforcementStrict {
		if status != 0 || pci != member {
			t.Fatalf("status %d %q, pci %q; want bound to the context's member %q", status, msg, pci, member)
		}
		return
	}
	if status != http.StatusForbidden || pci != "" {
		t.Fatalf("status %d %q, pci %q; want 403: the order is not provably the patient's", status, msg, pci)
	}
}

// crdDispatchFor is an order-dispatch request for patientID with one
// dispatched order whose subject is orderSubject.
func crdDispatchFor(patientID, orderSubject string) []byte {
	return []byte(`{"hook":"order-dispatch","context":{"patientId":"` + patientID + `","dispatchedOrders":["DeviceRequest/dr1"],"performer":"Organization/dme1"},` +
		`"prefetch":{"deviceHistory":{"resourceType":"Bundle","entry":[{"fullUrl":"DeviceRequest/dr1","resource":{"resourceType":"DeviceRequest","id":"dr1","subject":{"reference":"` + orderSubject + `"}}}]},` +
		`"coverage":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}]}}}`)
}

// fqTaskNaming is the federated query for MBR-UC05's DiagnosticReports owned
// by owner, with its Task.for and its query's patient both naming ref.
func fqTaskNaming(t *testing.T, owner, ref string) []byte {
	t.Helper()
	b, err := shnsdk.BuildCDexTaskDataRequest("Patient/MBR-UC05", "DiagnosticReport", "2020-01-01", "2030-01-01",
		shnsdk.CDexTaskMeta{AuthoredOn: time.Unix(1700000000, 0).UTC(), Requester: "requester", Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(b, []byte("Patient/MBR-UC05"), []byte(ref))
}

// sendSoleMemberFacility sends query to a facility gateway
// (TestPinnedFindingContext_FederatedQueryEgress's) whose system of record reads the
// empty member as MBR-UC05, with a token for MBR-UC05, and returns the status and body
// of its framed answer.
func sendSoleMemberFacility(t *testing.T, level ConformanceEnforcement, query []byte) (int, []byte) {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	_, paySignPriv := genED25519(t)
	payEncPub, payEncPriv := genKeyPair(t)
	reqEncPub, reqEncPriv := genKeyPair(t)
	reqSignPub, _ := genED25519(t)
	reg := shnsdk.NewRegistry()
	reg.Set("requester", shnsdk.RegistryEntry{ID: "requester", Role: "provider", EncPub: reqEncPub, SignPub: reqSignPub, MessageFrames: shnsdk.SupportedMessageFrames()})
	sor := &fqFacilitySoR{censusSoR: newCensusSoR()}
	soleMember(t, sor, "MBR-UC05")
	g := mustNew(t, Config{
		Role: "payer", HolderID: "payer",
		Identity: shnsdk.Identity{HolderID: "payer", SignPriv: paySignPriv, EncPub: payEncPub, EncPriv: payEncPriv},
		AuthzURL: "http://stub.test", AuthzPub: authzPub, HubTransportPub: authzPub, ConsentURL: "http://stub.test/consent",
		Reg: reg, SoR: sor, Store: newCensusSoR(), Responder: unusedResponder{}, Clock: fixedClock,
		ConformanceEnforcement: level,
		Client:                 &http.Client{Transport: &consentGrantStub{authz: inboundAuthzStub{authzPriv: authzPriv, clock: fixedClock}, consentRef: "consent-ref-1"}},
	})
	pci, _, _ := sor.ResolvePatient("MBR-UC05")
	rec := httptest.NewRecorder()
	g.handleFederatedQueryInbound(rec, fqInboundRequest(), fqInboundEnvelope(), shnsdk.Token{Subject: pci}, query, "")
	if rec.Code != http.StatusOK {
		return rec.Code, rec.Body.Bytes()
	}
	hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, inboundTestRequester{EncPub: reqEncPub, EncPriv: reqEncPriv}, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode the answer frame: %v", err)
	}
	return hdr.Status, answer
}

func fqInboundEnvelope() shnsdk.Envelope {
	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender, env.Metadata.ConsentRef = "corr-fq-1", "requester", "consent-ref-1"
	return env
}

func fqInboundRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	return r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "federated-query", CorrelationID: "corr-fq-1",
		Seam: fqsAdapter.catalog()["federated-query"].Seam, Whose: "peer",
	}))
}

// cdexTaskNaming is the federated query for dtrFrameMember's DiagnosticReports,
// owned by the payer, with its Task.for and its query's patient naming ref.
func cdexTaskNaming(t *testing.T, ref string) []byte {
	t.Helper()
	return bytes.ReplaceAll(cdexTaskFor(t, dtrFrameMember), []byte("Patient/"+dtrFrameMember), []byte(ref))
}

// A federated query whose Task.for (and its query's patient) carries search
// syntax names that member exactly: the gateway reads its system of record for
// it as written and binds no one else (the FHIR connector escapes it, rows in
// internal/fixturesor and connectors/fhirsor). It is refused on consent where
// the token names another patient, and sends nothing for the one member. Its id
// is not a FHIR id: a participant that opted into the request's own shape
// (structural, strict) refuses it 400 before anything is read.
func TestFederatedQuery_ATaskNamingSearchSyntaxReadsThatMemberOnly(t *testing.T) {
	t.Parallel()
	const member = "x," + shnsdk.MemberSystem + "|"
	for _, level := range allLevels {
		shape := level == EnforcementStructural || level == EnforcementStrict
		t.Run("inbound/"+level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.answering(t, "federated-query")
			p.g.cfg.ConsentURL = "http://consent.test"
			p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
			var reads []string
			p.g.cfg.SoR.(*censusSoR).reads = &reads
			ans := sendConsented(t, p, "federated-query", cdexTaskNaming(t, "Patient/"+member), p.pci, "Consent/c-1")
			if shape {
				p.wantRefused(t, ans, http.StatusBadRequest, msgFederatedQueryPatientIDNotFHIRID)
				if len(reads) != 0 {
					t.Fatalf("read the system of record for %q before refusing the request's shape", reads)
				}
				return
			}
			if ans.status != http.StatusForbidden || bytes.Contains(ans.body, []byte("DiagnosticReport")) {
				t.Fatalf("answer %d %s; want refused on consent, nothing disclosed", ans.status, ans.body)
			}
			wantRefusal(t, got.only(t), http.StatusForbidden, partyAnswering("federated-query"), RefusalConsent)
			if len(reads) != 1 || reads[0] != member {
				t.Fatalf("read the system of record for %q, want exactly %q once", reads, member)
			}
		})
		t.Run("ingress/"+level.String(), func(t *testing.T) {
			env := newFQExchange(t, level)
			var reads []string
			env.originator.cfg.SoR.(*censusSoR).reads = &reads
			env.payerReturns(LegResult{Response: testResponse([]byte(`{}`))})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(fqTaskNaming(t, "facility", "Patient/"+member)))
			if shape {
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, msgFederatedQueryPatientIDNotFHIRID)
				if len(reads) != 0 {
					t.Fatalf("read the system of record for %q before refusing the request's shape", reads)
				}
				return
			}
			if len(reads) == 0 || reads[0] != member {
				t.Fatalf("read the system of record for %q, want %q first (answer %d %s)", reads, member, rec.Code, rec.Body)
			}
		})
	}
}

// The federated query reads a Task whose Task.for names no one as such before
// any SDK reader reads it, whatever that reader answers: errMemberNamesNoOne,
// never a blank member. A Task.for that names a member, however written, is
// read as before, and an absent or empty one is left to the SDK's reader.
func TestCDexTaskQuery_ATaskForNamingNoOneIsRefusedBeforeTheReader(t *testing.T) {
	// A Task.for that names no Patient by FHIR's reference grammar names no
	// one too: "Patient/" found inside another reference, a trailing path, an
	// operation, an escape, another type, a URN.
	for _, ref := range []string{"Patient/", "Patient/ ", "Patient/Patient/", "Patient/Patient/ ", " ",
		"Patient/Patient/X", "Patient/X/y", "Patient/$everything", "Patient/a%2Fb", "urn:Patient/X", "Group/1", "Group/1/Patient/X", " Patient/X"} {
		if _, member, err := cdexTaskQuery(cdexTaskNaming(t, ref)); !errors.Is(err, errMemberNamesNoOne) {
			t.Errorf("Task.for %q: member %q, err %v; want errMemberNamesNoOne", ref, member, err)
		}
	}
	for ref, want := range map[string]string{
		"Patient/" + dtrFrameMember:                          dtrFrameMember,
		"https://ehr.example/fhir/Patient/" + dtrFrameMember: dtrFrameMember,
		// An id that is not a FHIR id names its Patient as written.
		"Patient/MBR_X":                          "MBR_X",
		"https://ehr.example/fhir/Patient/MBR_X": "MBR_X",
	} {
		if _, member, err := cdexTaskQuery(cdexTaskNaming(t, ref)); err != nil || member != want {
			t.Errorf("Task.for %q: member %q, err %v; want %q", ref, member, err, want)
		}
	}
	if _, _, err := cdexTaskQuery(cdexTaskNaming(t, "")); err == nil || errors.Is(err, errMemberNamesNoOne) {
		t.Errorf("an empty Task.for: err %v; want the SDK reader's refusal", err)
	}
}
