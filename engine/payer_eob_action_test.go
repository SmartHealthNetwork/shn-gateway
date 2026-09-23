package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type payerEOBSource struct {
	*censusSoR
	records map[string][]byte
}

type changingPatientEOBSource struct {
	payerEOBSource
	patientReads int
}

func (s *changingPatientEOBSource) ResolveByReference(ref string) ([]byte, bool) {
	if ref == "Patient/member-1" {
		s.patientReads++
		if s.patientReads > 1 {
			return []byte(`{"resourceType":"Patient","id":"member-1","identifier":[{"system":"urn:shn:pci","value":"pci:member-2"}]}`), true
		}
	}
	return s.payerEOBSource.ResolveByReference(ref)
}

func (s payerEOBSource) ResolveByReference(ref string) ([]byte, bool) {
	b, ok := s.records[ref]
	return b, ok
}

func payerEOBActionFixture() (*Gateway, []byte) {
	eob := []byte(`{"resourceType":"ExplanationOfBenefit","id":"eob-own-1","status":"active","type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"professional"}]},"use":"preauthorization","patient":{"reference":"Patient/member-1"},"created":"2026-06-03T00:00:00Z","insurer":{"reference":"Organization/actual-payer"},"provider":{"reference":"Practitioner/actual-reviewer"},"insurance":[{"focal":true,"coverage":{"reference":"Coverage/actual-coverage"}}],"item":[{"sequence":1,"productOrService":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]},"adjudication":[{"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted"}]},"amount":{"value":20,"currency":"USD"}}]}],"outcome":"complete","preAuthRef":["AUTH-OWN-1"],"processNote":[{"number":1,"type":"print","text":"Approved for 12 visits."}]}`)
	s := payerEOBSource{censusSoR: newCensusSoR(), records: map[string][]byte{
		"ExplanationOfBenefit/eob-own-1": eob,
		"Patient/member-1":               []byte(`{"resourceType":"Patient","id":"member-1","identifier":[{"system":"urn:shn:pci","value":"pci:member-1"}]}`),
		"Organization/actual-payer":      []byte(`{"resourceType":"Organization","id":"actual-payer"}`),
		"Practitioner/actual-reviewer":   []byte(`{"resourceType":"Practitioner","id":"actual-reviewer"}`),
		"Coverage/actual-coverage":       []byte(`{"resourceType":"Coverage","id":"actual-coverage","beneficiary":{"reference":"Patient/member-1"}}`),
	}}
	v := NewLineFakeValidator("2.0")
	v.Evidence = syntheticEvidence()
	g := &Gateway{cfg: Config{Role: "payer", HolderID: "payer", SoR: s, Store: s.MemStore, PayerEOBValidator: v,
		SubjectReferenceResolver: subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
			if ref == (PatientReference{Holder: "payer", System: "fhir-relative", Value: "Patient/member-1"}) {
				return "pci:member-1", true, nil
			}
			return "", false, nil
		}),
	}}
	return g, eob
}

func TestRecordPayerEOBFromSource_RecordsExactOwnedResource(t *testing.T) {
	g, source := payerEOBActionFixture()
	if err := g.RecordPayerEOBFromSource(context.Background(), "pci:member-1", "ExplanationOfBenefit/eob-own-1"); err != nil {
		t.Fatal(err)
	}
	got, ok := g.cfg.Store.EOBByID("eob-own-1")
	if !ok || !bytes.Equal(got, source) {
		t.Fatalf("recorded EOB differs from payer source: found=%v", ok)
	}
}

func TestRecordPayerEOBFromSource_BindsOnePatientSnapshot(t *testing.T) {
	g, source := payerEOBActionFixture()
	changing := &changingPatientEOBSource{payerEOBSource: g.cfg.SoR.(payerEOBSource)}
	g.cfg.SoR = changing
	if err := g.RecordPayerEOBFromSource(context.Background(), "pci:member-1", "ExplanationOfBenefit/eob-own-1"); err != nil {
		t.Fatal(err)
	}
	if changing.patientReads != 1 {
		t.Fatalf("patient source read %d times; linkage and resource proof must use one snapshot", changing.patientReads)
	}
	if got, ok := g.cfg.Store.EOBByID("eob-own-1"); !ok || !bytes.Equal(got, source) {
		t.Fatal("single-snapshot action did not store exact source EOB")
	}
}

func TestRecordPayerEOBFromSource_ProfileProofWithUnprovenTerminology(t *testing.T) {
	g, source := payerEOBActionFixture()
	v := g.cfg.PayerEOBValidator.(*LineFakeValidator)
	v.Evidence.Terminology.State = shnsdk.ValidationUnavailable
	v.Evidence.Terminology.Code = "terminology-support-unproven"
	if err := g.RecordPayerEOBFromSource(context.Background(), "pci:member-1", "ExplanationOfBenefit/eob-own-1"); err != nil {
		t.Fatal(err)
	}
	got, ok := g.cfg.Store.EOBByID("eob-own-1")
	if !ok || !bytes.Equal(got, source) {
		t.Fatal("valid profile and source proof did not record EOB")
	}
}

func TestRecordPayerEOBFromSource_RejectsUnprovenOwnershipAndMissingSource(t *testing.T) {
	for _, tc := range []struct {
		name, subject, ref string
		mutate             func(*Gateway)
	}{
		{"foreign subject", "pci:another", "ExplanationOfBenefit/eob-own-1", nil},
		{"missing source", "pci:member-1", "ExplanationOfBenefit/unknown", nil},
		{"missing insurer source", "pci:member-1", "ExplanationOfBenefit/eob-own-1", func(g *Gateway) { s := g.cfg.SoR.(payerEOBSource); delete(s.records, "Organization/actual-payer") }},
		{"unavailable issued PCI", "pci:member-1", "ExplanationOfBenefit/eob-own-1", func(g *Gateway) {
			s := g.cfg.SoR.(payerEOBSource)
			s.records["Patient/member-1"] = []byte(`{"resourceType":"Patient","id":"member-1"}`)
		}},
		{"ambiguous issued PCI", "pci:member-1", "ExplanationOfBenefit/eob-own-1", func(g *Gateway) {
			s := g.cfg.SoR.(payerEOBSource)
			s.records["Patient/member-1"] = []byte(`{"resourceType":"Patient","id":"member-1","identifier":[{"system":"urn:shn:pci","value":"pci:member-1"},{"system":"urn:shn:pci","value":"pci:member-1"}]}`)
		}},
		{"no certifier", "pci:member-1", "ExplanationOfBenefit/eob-own-1", func(g *Gateway) { g.cfg.PayerEOBValidator = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := payerEOBActionFixture()
			if tc.mutate != nil {
				tc.mutate(g)
			}
			if err := g.RecordPayerEOBFromSource(context.Background(), tc.subject, tc.ref); err == nil {
				t.Fatal("action wrote or accepted an EOB without owned source proof")
			}
			if b, ok := g.cfg.Store.EOBByID("eob-own-1"); ok || len(b) > 0 {
				t.Fatal("refused action still recorded an EOB")
			}
		})
	}
}

func TestRecordPayerEOBFromSource_RejectsGuessedDecisionFields(t *testing.T) {
	g, _ := payerEOBActionFixture()
	s := g.cfg.SoR.(payerEOBSource)
	s.records["ExplanationOfBenefit/eob-own-1"] = []byte(strings.Replace(string(s.records["ExplanationOfBenefit/eob-own-1"]), `"code":"submitted"`, `"code":"denialreason"`, 1))
	if err := g.RecordPayerEOBFromSource(context.Background(), "pci:member-1", "ExplanationOfBenefit/eob-own-1"); err == nil {
		t.Fatal("contradictory payer source was accepted")
	}
	if _, ok := g.cfg.Store.EOBByID("eob-own-1"); ok {
		t.Fatal("contradictory EOB recorded")
	}
}

func TestRecordPayerEOBFromSource_RequiresCompleteProfileFacts(t *testing.T) {
	for _, field := range []string{"type", "created", "outcome"} {
		t.Run(field, func(t *testing.T) {
			g, _ := payerEOBActionFixture()
			s := g.cfg.SoR.(payerEOBSource)
			var eob map[string]any
			if err := json.Unmarshal(s.records["ExplanationOfBenefit/eob-own-1"], &eob); err != nil {
				t.Fatal(err)
			}
			delete(eob, field)
			s.records["ExplanationOfBenefit/eob-own-1"], _ = json.Marshal(eob)
			if err := g.RecordPayerEOBFromSource(context.Background(), "pci:member-1", "ExplanationOfBenefit/eob-own-1"); err == nil {
				t.Fatalf("missing required payer source %s was recorded", field)
			}
			if _, ok := g.cfg.Store.EOBByID("eob-own-1"); ok {
				t.Fatal("partial EOB was stored")
			}
		})
	}
}

func TestRecordPayerEOBFromSource_DenialDetailAndNotesStayExact(t *testing.T) {
	g, _ := payerEOBActionFixture()
	s := g.cfg.SoR.(payerEOBSource)
	var eob map[string]any
	if err := json.Unmarshal(s.records["ExplanationOfBenefit/eob-own-1"], &eob); err != nil {
		t.Fatal(err)
	}
	delete(eob, "preAuthRef")
	item := eob["item"].([]any)[0].(map[string]any)
	var adjudications []any
	adjJSON := `[{"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted"}]},"amount":{"value":20,"currency":"USD"},"extension":[{"url":"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewAction","extension":[{"url":"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"A3","display":"Not Certified"}]}}]}]},{"category":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pdex/CodeSystem/PDexAdjudicationDiscriminator","code":"denialreason"}]},"reason":{"coding":[{"system":"https://x12.org/codes/claim-adjustment-reason-codes","code":"197","display":"Precertification absent"}]}}]`
	if err := json.Unmarshal([]byte(adjJSON), &adjudications); err != nil {
		t.Fatal(err)
	}
	item["adjudication"] = adjudications
	eob["processNote"] = []any{map[string]any{"number": 1, "type": "display", "text": "Payer's own reason."}, map[string]any{"number": 2, "type": "print", "text": "Appeal within 60 days."}}
	raw, _ := json.Marshal(eob)
	s.records["ExplanationOfBenefit/eob-own-1"] = raw
	if err := g.RecordPayerEOBFromSource(context.Background(), "pci:member-1", "ExplanationOfBenefit/eob-own-1"); err != nil {
		t.Fatal(err)
	}
	stored, ok := g.cfg.Store.EOBByID("eob-own-1")
	if !ok || !bytes.Equal(stored, raw) {
		t.Fatal("payer denial detail, CARC, or notes changed during source action")
	}
}

func TestRecordPayerEOBFromSource_PartialApprovalStaysExact(t *testing.T) {
	g, _ := payerEOBActionFixture()
	s := g.cfg.SoR.(payerEOBSource)
	var eob map[string]any
	if err := json.Unmarshal(s.records["ExplanationOfBenefit/eob-own-1"], &eob); err != nil {
		t.Fatal(err)
	}
	item := eob["item"].([]any)[0].(map[string]any)
	adjudication := item["adjudication"].([]any)[0].(map[string]any)
	adjudication["extension"] = []any{map[string]any{
		"url": "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewAction",
		"extension": []any{map[string]any{
			"url": "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewActionCode",
			"valueCodeableConcept": map[string]any{"coding": []any{map[string]any{
				"system": "https://codesystem.x12.org/005010/306", "code": "A2", "display": "Certified - Partial",
			}}},
		}},
	}}
	eob["processNote"] = []any{map[string]any{"number": 1, "type": "print", "text": "Six of twelve visits approved."}}
	raw, err := json.Marshal(eob)
	if err != nil {
		t.Fatal(err)
	}
	s.records["ExplanationOfBenefit/eob-own-1"] = raw
	if err := g.RecordPayerEOBFromSource(context.Background(), "pci:member-1", "ExplanationOfBenefit/eob-own-1"); err != nil {
		t.Fatal(err)
	}
	stored, ok := g.cfg.Store.EOBByID("eob-own-1")
	if !ok || !bytes.Equal(stored, raw) {
		t.Fatal("payer's partial-approval action, authorization, or note changed")
	}
}

func TestPayerEOBRecordRoute_AuthenticatedExplicitAction(t *testing.T) {
	g, source := payerEOBActionFixture()
	g.cfg.PayerEOBActionsEnabled = true
	_, public := newTestClientKey(t)
	g.ingressAuth = newTestAuthServer(t, "payer-source", public, "ES384")
	registration := g.ingressAuth.clients["payer-source"]
	registration.PayerEOBRecord = true
	g.ingressAuth.clients["payer-source"] = registration
	kid, key, err := g.ingressAuth.keys.SigningKey(ingressFixedClock()())
	if err != nil {
		t.Fatal(err)
	}
	bearer, err := signBearer(kid, key, "payer-source", "system/ExplanationOfBenefit.write", testIngressBaseURL, ingressFixedClock()())
	if err != nil {
		t.Fatal(err)
	}
	request := func(auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/local/payer/eob-record", strings.NewReader(`{"subjectPCI":"pci:member-1","sourceRef":"ExplanationOfBenefit/eob-own-1"}`))
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, r)
		return w
	}
	if got := request(""); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d, want 401", got.Code)
	}
	if _, ok := g.cfg.Store.EOBByID("eob-own-1"); ok {
		t.Fatal("unauthenticated write")
	}
	registration.PayerEOBRecord = false
	g.ingressAuth.clients["payer-source"] = registration
	if got := request(bearer); got.Code != http.StatusForbidden {
		t.Fatalf("registered client without action grant status=%d, want 403", got.Code)
	}
	if _, ok := g.cfg.Store.EOBByID("eob-own-1"); ok {
		t.Fatal("ungranted client wrote EOB")
	}
	registration.PayerEOBRecord = true
	g.ingressAuth.clients["payer-source"] = registration
	wrongScope, err := signBearer(kid, key, "payer-source", ingressScope, testIngressBaseURL, ingressFixedClock()())
	if err != nil {
		t.Fatal(err)
	}
	if got := request(wrongScope); got.Code != http.StatusForbidden {
		t.Fatalf("wrong scope status=%d, want 403", got.Code)
	}
	if got := request(bearer); got.Code != http.StatusCreated {
		t.Fatalf("authenticated status=%d body=%s", got.Code, got.Body.String())
	}
	if got, ok := g.cfg.Store.EOBByID("eob-own-1"); !ok || !bytes.Equal(got, source) {
		t.Fatal("authenticated action did not record exact source EOB")
	}
}

func TestPayerEOBRecordRoute_PatientAccessReadsSourceDecision(t *testing.T) {
	g, source := payerEOBActionFixture()
	g.cfg.PayerEOBActionsEnabled = true
	_, public := newTestClientKey(t)
	g.ingressAuth = newTestAuthServer(t, "payer-source", public, "ES384")
	registration := g.ingressAuth.clients["payer-source"]
	registration.PayerEOBRecord = true
	g.ingressAuth.clients["payer-source"] = registration
	kid, key, err := g.ingressAuth.keys.SigningKey(ingressFixedClock()())
	if err != nil {
		t.Fatal(err)
	}
	bearer, err := signBearer(kid, key, "payer-source", payerEOBWriteScope, testIngressBaseURL, ingressFixedClock()())
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewRequest(http.MethodPost, "/local/payer/eob-record", strings.NewReader(`{"subjectPCI":"pci:member-1","sourceRef":"ExplanationOfBenefit/eob-own-1"}`))
	local.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, local)
	if w.Code != http.StatusCreated {
		t.Fatalf("record status=%d body=%s", w.Code, w.Body.String())
	}

	audit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	t.Cleanup(audit.Close)
	authzPub, authzPriv := genED25519(t)
	_, payerSign := genED25519(t)
	g.cfg.Identity = shnsdk.Identity{HolderID: "payer", SignPriv: payerSign}
	g.cfg.AuthzPub = authzPub
	g.cfg.Clock = fixedClock
	g.cfg.Client = audit.Client()
	g.cfg.AuditURL = audit.URL
	g.replay = NewInMemoryReplayStore()
	tok := signTestToken(shnsdk.Token{Operation: "patient-access-read", Scope: "patient-access-only", Subject: "pci:member-1", Frame: "patient-access", Holder: "phg", CorrelationID: "payer-eob-view-1", Expiry: fixedClock().Add(time.Hour)}, authzPriv)
	tokJSON, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	read := httptest.NewRequest(http.MethodGet, "/ExplanationOfBenefit?patient=pci:member-1", nil)
	read.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(tokJSON))
	view := httptest.NewRecorder()
	g.Handler().ServeHTTP(view, read)
	if view.Code != http.StatusOK {
		t.Fatalf("patient access status=%d body=%s", view.Code, view.Body.String())
	}
	if !bytes.Contains(view.Body.Bytes(), source) {
		t.Fatalf("patient access omitted payer's source EOB: %s", view.Body.String())
	}

	// The payer's source later corrects this same EOB id to another patient.
	// Recording it for B must fail atomically; A's patient-bound search keeps
	// the original bytes, while B's search and instance read disclose nothing.
	s := g.cfg.SoR.(payerEOBSource)
	second := bytes.ReplaceAll(source, []byte("Patient/member-1"), []byte("Patient/member-2"))
	second = bytes.ReplaceAll(second, []byte("Coverage/actual-coverage"), []byte("Coverage/second-coverage"))
	s.records["ExplanationOfBenefit/eob-own-1"] = second
	s.records["Patient/member-2"] = []byte(`{"resourceType":"Patient","id":"member-2","identifier":[{"system":"urn:shn:pci","value":"pci:member-2"}]}`)
	s.records["Coverage/second-coverage"] = []byte(`{"resourceType":"Coverage","id":"second-coverage","beneficiary":{"reference":"Patient/member-2"}}`)
	g.cfg.SubjectReferenceResolver = subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
		if ref.Holder != "payer" || ref.System != "fhir-relative" {
			return "", false, nil
		}
		switch ref.Value {
		case "Patient/member-1":
			return "pci:member-1", true, nil
		case "Patient/member-2":
			return "pci:member-2", true, nil
		}
		return "", false, nil
	})
	corrected := httptest.NewRequest(http.MethodPost, "/local/payer/eob-record", strings.NewReader(`{"subjectPCI":"pci:member-2","sourceRef":"ExplanationOfBenefit/eob-own-1"}`))
	corrected.Header.Set("Authorization", "Bearer "+bearer)
	refusal := httptest.NewRecorder()
	g.Handler().ServeHTTP(refusal, corrected)
	if refusal.Code != http.StatusConflict {
		t.Fatalf("cross-patient correction status=%d body=%s, want conflict", refusal.Code, refusal.Body.String())
	}
	readAs := func(subject, corr, path string) *httptest.ResponseRecorder {
		t.Helper()
		token := signTestToken(shnsdk.Token{Operation: "patient-access-read", Scope: "patient-access-only", Subject: subject, Frame: "patient-access", Holder: "phg", CorrelationID: corr, Expiry: fixedClock().Add(time.Hour)}, authzPriv)
		encoded, err := json.Marshal(token)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(encoded))
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, r)
		return w
	}
	if own := readAs("pci:member-1", "payer-eob-view-2", "/ExplanationOfBenefit?patient=pci:member-1"); own.Code != http.StatusOK || !bytes.Contains(own.Body.Bytes(), source) || bytes.Contains(own.Body.Bytes(), second) {
		t.Fatalf("first patient's search leaked replacement: status=%d body=%s", own.Code, own.Body.String())
	}
	if foreign := readAs("pci:member-2", "payer-eob-view-3", "/ExplanationOfBenefit?patient=pci:member-2"); foreign.Code != http.StatusOK || bytes.Contains(foreign.Body.Bytes(), source) || bytes.Contains(foreign.Body.Bytes(), second) {
		t.Fatalf("second patient's search disclosed refused EOB: status=%d body=%s", foreign.Code, foreign.Body.String())
	}
	if foreign := readAs("pci:member-2", "payer-eob-view-4", "/ExplanationOfBenefit/eob-own-1"); foreign.Code != http.StatusForbidden {
		t.Fatalf("second patient's instance read status=%d body=%s, want 403", foreign.Code, foreign.Body.String())
	}
}
