package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// strangerMember is a member id no census persona holds — a patient from a partner's own test environment
// arriving through the provider test lane.
const strangerMember = "eXYZ123"

// failingSubjectSoR is a context system of record whose member read fails.
type failingSubjectSoR struct{ *prefetchSoR }

func (failingSubjectSoR) ResolvePatientContext(context.Context, string) (string, Demo, bool, error) {
	return "", Demo{}, false, &SoRReadError{Kind: SoRUnavailable}
}

func strangerPCI() string { return shnsdk.ResolvePCI(strangerMember, "", "") }

// --- the helper itself -------------------------------------------------------------------

func TestResolveSubjectPCI_UnknownMemberRefusedWhenKnownMembersRequired(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), RequireKnownMembers: true}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, nil)
	if err != nil || found || pci != "" {
		t.Fatalf("known members required: got (%q, found=%v, err=%v), want not found and no pci", pci, found, err)
	}
}

func TestResolveSubjectPCI_UnknownMemberBindsByIDByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, nil)
	if err != nil || !found {
		t.Fatalf("default: got (found=%v, err=%v), want found", found, err)
	}
	if pci != strangerPCI() {
		t.Fatalf("default: pci = %q, want the id-derived %q", pci, strangerPCI())
	}
}

// A member the system of record holds binds through it, whatever the setting: carrying
// unknown members never replaces a resolved identity with the id-derived one.
func TestResolveSubjectPCI_KnownMemberUnchangedByDefault(t *testing.T) {
	sor := newCensusSoR()
	want, _, ok := sor.ResolvePatient("MBR-COVERED")
	if !ok {
		t.Fatal("fixture: MBR-COVERED must resolve")
	}
	g := &Gateway{cfg: Config{SoR: sor}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), "MBR-COVERED", nil)
	if err != nil || !found || pci != want {
		t.Fatalf("default, known member: got (%q, found=%v, err=%v), want %q", pci, found, err, want)
	}
	if pci == shnsdk.ResolvePCI("MBR-COVERED", "", "") {
		t.Fatal("default, known member: bound by id alone instead of through the system of record")
	}
}

// An unreadable system of record is never mistaken for a member it does not hold.
func TestResolveSubjectPCI_ReadFailureSurfacesByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: failingSubjectSoR{newPrefetchSoR()}}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, nil)
	if err == nil || found || pci != "" {
		t.Fatalf("default, read failure: got (%q, found=%v, err=%v), want the read error", pci, found, err)
	}
}

// --- the provider ingress legs ------------------------------------------------------------

func TestIngressSubjectPCI_StrangerRefusedWhenKnownMembersRequired(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), RequireKnownMembers: true}}
	ref := "Patient/" + strangerMember
	_, status, msg := g.ingressCRDSubjectPCIContext(context.Background(), crdReqJSON(strangerMember, ref, ref))
	if status != http.StatusBadRequest || msg != "unknown member" {
		t.Fatalf("known members required, CRD ingress: status=%d msg=%q, want 400 unknown member", status, msg)
	}
}

func TestIngressSubjectPCI_StrangerBindsByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	ref := "Patient/" + strangerMember
	pci, status, msg := g.ingressCRDSubjectPCIContext(context.Background(), crdReqJSON(strangerMember, ref, ref))
	if status != 0 {
		t.Fatalf("default CRD ingress: status=%d msg=%q, want bound", status, msg)
	}
	if pci != strangerPCI() {
		t.Fatalf("default CRD ingress: pci=%q, want %q", pci, strangerPCI())
	}
}

// Rejection row: by default ONE stranger binds; a request that mixes the stranger with another
// member — held or not — is still refused.
func TestIngressSubjectPCI_MixedMembersStillRefusedByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	for name, req := range map[string][]byte{
		"held member in the order":    crdReqJSON(strangerMember, "Patient/MBR-COVERED", "Patient/"+strangerMember),
		"second stranger in coverage": crdReqJSON(strangerMember, "Patient/"+strangerMember, "Patient/eOTHER"),
	} {
		_, status, _ := g.ingressCRDSubjectPCIContext(context.Background(), req)
		if status != http.StatusForbidden {
			t.Errorf("%s: status=%d, want 403", name, status)
		}
	}
}

func dtrPackageFor(member string) []byte {
	return []byte(`{"resourceType":"Parameters","parameter":[{"name":"coverage","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/` + member + `"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00301"}}]}},{"name":"questionnaire","valueCanonical":"http://example.org/fhir/Questionnaire/HomeHealthAssessment"}]}`)
}

func TestIngressDTR_StrangerRefusedWhenKnownMembersRequired(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), RequireKnownMembers: true}}
	_, status, msg := g.prepareDTRPackageRequest(context.Background(), dtrPackageFor(strangerMember))
	if status != http.StatusForbidden || msg != "request patient does not resolve" {
		t.Fatalf("known members required, DTR ingress: status=%d msg=%q, want 403 request patient does not resolve", status, msg)
	}
}

func TestIngressDTR_StrangerBindsByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	out, status, msg := g.prepareDTRPackageRequest(context.Background(), dtrPackageFor(strangerMember))
	if status != 0 {
		t.Fatalf("default DTR ingress: status=%d msg=%q, want bound", status, msg)
	}
	if out.pci != strangerPCI() || out.member != strangerMember {
		t.Fatalf("default DTR ingress: bound (%q, %q), want (%q, %q)", out.member, out.pci, strangerMember, strangerPCI())
	}
}

func TestIngressPAS_StrangerRefusedWhenKnownMembersRequired(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), RequireKnownMembers: true}}
	_, status, msg := g.ingressPASNativeSubjectPCIContext(context.Background(), conformantPASBundleWithQR(t, strangerMember))
	if status != http.StatusBadRequest || msg != "unknown member" {
		t.Fatalf("known members required, PAS ingress: status=%d msg=%q, want 400 unknown member", status, msg)
	}
}

func TestIngressPAS_StrangerBindsByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	pci, status, msg := g.ingressPASNativeSubjectPCIContext(context.Background(), conformantPASBundleWithQR(t, strangerMember))
	if status != 0 || pci != strangerPCI() {
		t.Fatalf("default PAS ingress: status=%d msg=%q pci=%q, want bound to %q", status, msg, pci, strangerPCI())
	}
}

// The inquiry ingress binds through the same helper: a stranger is refused at
// the bind (400) when known members are required; by default it passes the bind and is refused where the control is,
// at routing (422, this fixture's Coverage names no payer), so the difference is the bind.
func TestIngressInquire_StrangerBindsByDefault(t *testing.T) {
	post := func(t *testing.T, seam bool, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		g := &Gateway{cfg: Config{ingressAuthBypass: true, SoR: newCensusSoR(), Clock: fixedClock, RequireKnownMembers: !seam}}
		w := httptest.NewRecorder()
		g.handlePASInquireIngress(w, httptest.NewRequest(http.MethodPost, "/Claim/$inquire", bytes.NewReader(body)))
		return w
	}
	stranger := inquiryBundle(strangerMember, "", "TRN-1", "72148")
	if w := post(t, false, stranger); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown member") {
		t.Fatalf("known members required, inquiry ingress: status=%d body=%s, want 400 unknown member", w.Code, w.Body)
	}
	if w := post(t, true, stranger); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("default inquiry ingress: status=%d body=%s, want 422 (past the bind, refused at routing)", w.Code, w.Body)
	}
	// Rejection row: two members are still refused at the bind under the seam.
	if w := post(t, true, inquiryBundle(strangerMember, "MBR-COVERED", "TRN-1", "72148")); w.Code != http.StatusForbidden {
		t.Fatalf("seam inquiry ingress, two members: status=%d body=%s, want 403", w.Code, w.Body)
	}
}

// --- the payer inbound legs: the payer binds the member its request names by its own
// system (the record it holds, else the Patient the request carries), whatever subject the
// leg's token names; that binding is what it records the exchange under ---------------------

func TestPayerCRDBind_StrangerBindsToDerivedSubjectByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	_, _, pci, status, msg := g.conformantCRDBindContext(context.Background(), conformantCRD(strangerMember, "72148"))
	if status != 0 || pci != strangerPCI() {
		t.Fatalf("default payer CRD: status=%d msg=%q pci=%q, want bound to %q", status, msg, pci, strangerPCI())
	}
	// Requiring known members, the payer refuses the stranger.
	g.cfg.RequireKnownMembers = true
	_, _, _, status, _ = g.conformantCRDBindContext(context.Background(), conformantCRD(strangerMember, "72148"))
	if status != http.StatusBadRequest {
		t.Fatalf("known members required, payer CRD: status=%d, want 400", status)
	}
}

func TestPayerPASBind_StrangerBindsToDerivedSubjectByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	bundle := conformantPASBundleWithQR(t, strangerMember)
	memberRef, pci, status, msg := g.conformantPASBindContext(context.Background(), bundle)
	if status != 0 || memberRef != "Patient/"+strangerMember || pci != strangerPCI() {
		t.Fatalf("default payer PAS: status=%d msg=%q memberRef=%q pci=%q, want bound to %q", status, msg, memberRef, pci, strangerPCI())
	}
	g.cfg.RequireKnownMembers = true
	if _, _, status, _ := g.conformantPASBindContext(context.Background(), bundle); status != http.StatusBadRequest {
		t.Fatalf("known members required, payer PAS: status=%d, want 400", status)
	}
}

func TestPayerDTRBind_StrangerBindsToDerivedSubjectByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	if pci, status, msg := g.bindNextQuestionSubjectContext(context.Background(), "Patient/"+strangerMember, nil); status != 0 || pci != strangerPCI() {
		t.Fatalf("default payer DTR: status=%d msg=%q pci=%q, want bound to %q", status, msg, pci, strangerPCI())
	}
	g.cfg.RequireKnownMembers = true
	if _, status, _ := g.bindNextQuestionSubjectContext(context.Background(), "Patient/"+strangerMember, nil); status != http.StatusBadRequest {
		t.Fatalf("known members required, payer DTR: status=%d, want 400", status)
	}
}

// A payer bind with no member to bind refuses, whatever the posture.
func TestBindInboundSubject_NoMemberRefused(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	if pci, status, _ := g.bindInboundSubject(context.Background(), "", nil); status != http.StatusBadRequest || pci != "" {
		t.Fatalf("no member: pci=%q status=%d, want 400", pci, status)
	}
}

// strangerEHRRequest is ehrRequest for a member no system of record holds: every
// reference the EHR's layout makes to prefetchMember names the stranger instead.
func strangerEHRRequest(prefetch string) []byte {
	r := strings.NewReplacer(
		"Patient/example", "Patient/"+strangerMember,
		`"patientId" : "example"`, `"patientId" : "`+strangerMember+`"`,
		`"id":"example"`, `"id":"`+strangerMember+`"`,
	)
	return []byte(r.Replace(string(ehrRequest(r.Replace(prefetch)))))
}

// Under the seam a member the system of record does not hold routes with the patient
// and coverage the request carries; the absent history keys are left out with the
// reason recorded and nothing is read for it. Without patient or coverage in the request
// it is refused before anything is read; without the seam it never reaches prefetch.
func TestPrefetch_HistoryOmittedForMemberNotHeld(t *testing.T) {
	s := newPrefetchSoR()
	obs := &observed{}
	env := newInProcessExchange(t)
	env.originator.cfg.EnrichNativeRequests = true // the prefetch fill this row pins
	env.originator.cfg.SoR = s.sor()
	env.originator.cfg.Observer = obs.observe
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(strangerEHRRequest(supported+`,"deviceHistory":null`)))
	if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 {
		t.Fatalf("the system of record was read for a member it does not hold: searched %v, read %v", searched, read)
	}
	sent := sentRequest(t, env)
	if got := names(membersOf(t, sent, "prefetch")); !slices.Equal(got, []string{"patient", "coverage", "deviceHistory"}) {
		t.Fatalf("prefetch keys %v, want only the EHR's", got)
	}
	events := obs.prefetch(t)
	if len(events) != 3 {
		t.Fatalf("events %+v, want one per omitted history key", events)
	}
	for _, k := range []string{"serviceHistory", "medicationHistory", "questionnaireResponses"} {
		e := events[k]
		if e.Outcome != SearchNotRun || e.Reason != historyMemberNotHeld || e.Count != 0 || !strings.Contains(e.Query, strangerMember) {
			t.Errorf("%s recorded as %+v", k, e)
		}
	}

	for name, body := range map[string][]byte{
		"patient absent":  strangerEHRRequest(`"coverage":` + ehrCoverage),
		"coverage absent": strangerEHRRequest(patientOnly),
		"no prefetch":     strangerEHRRequest("-"),
	} {
		t.Run(name, func(t *testing.T) {
			s := newPrefetchSoR()
			env := newInProcessExchange(t)
			env.originator.cfg.EnrichNativeRequests = true
			env.originator.cfg.SoR = s.sor()
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "patient not found in system of record")
		})
	}
	// Without enrichment nothing is filled, so a request without the patient is
	// carried as sent; one without the coverage still has nothing to be routed
	// by and is refused.
	t.Run("by default, patient absent is carried as sent", func(t *testing.T) {
		s := newPrefetchSoR()
		env := newInProcessExchange(t)
		env.originator.cfg.SoR = s.sor()
		body := strangerEHRRequest(`"coverage":` + ehrCoverage)
		rec := httptest.NewRecorder()
		env.originator.handleCRDIngress(rec, crdIngressPost(body))
		if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		if _, ok := valueOf(t, sentRequest(t, env), "prefetch", "patient"); ok {
			t.Fatal("nothing may be added without enrichment")
		}
	})
	for name, body := range map[string][]byte{
		"by default, coverage absent": strangerEHRRequest(patientOnly),
		"by default, no prefetch":     strangerEHRRequest("-"),
	} {
		t.Run(name, func(t *testing.T) {
			s := newPrefetchSoR()
			env := newInProcessExchange(t)
			env.originator.cfg.SoR = s.sor()
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "patient not found in system of record")
		})
	}
	t.Run("known members required", func(t *testing.T) {
		s := newPrefetchSoR()
		env := newInProcessExchange(t)
		env.originator.cfg.SoR = s.sor()
		env.originator.cfg.RequireKnownMembers = true
		rec := httptest.NewRecorder()
		env.originator.handleCRDIngress(rec, crdIngressPost(strangerEHRRequest(supported)))
		refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "unknown member")
	})
}

// --- binding from the Patient the request carries ------------------------------------------
//
// A member one side holds and the other does not: the holder derives the subject from its
// own record (member id + birthDate + family), so the other side must derive it from the
// same demographics — the Patient the request carries — or the token-subject check fails.

// requestPatient is a Patient as a request carries it: the member id as its id and its
// member identifier, with the demographics the derivation reads.
func requestPatient(member, birth, family string) string {
	return `{"resourceType":"Patient","id":"` + member + `","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + member + `"}],"name":[{"family":"` + family + `","given":["Iris"]}],"birthDate":"` + birth + `"}`
}

// noMemberSoR is a context system of record that holds no member at all.
type noMemberSoR struct{ *prefetchSoR }

func (noMemberSoR) ResolvePatientContext(context.Context, string) (string, Demo, bool, error) {
	return "", Demo{}, false, nil
}

// conformantCRDWithPatient is conformantCRD with a prefetch patient value (a bare
// Patient or a searchset carrying it).
func conformantCRDWithPatient(member, cpt, prefetchPatient string) []byte {
	return []byte(strings.Replace(string(conformantCRD(member, cpt)), `"prefetch":{`, `"prefetch":{"patient":`+prefetchPatient+`,`, 1))
}

// pasBundleWithPatient is conformantPASBundleWithQR with the Patient carried either as
// its own entry (replacing the bare one) or contained in the Claim.
func pasBundleWithPatient(t *testing.T, member, patient string, contained bool) []byte {
	t.Helper()
	var b struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Entry        []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(conformantPASBundleWithQR(t, member), &b); err != nil {
		t.Fatal(err)
	}
	for i := range b.Entry {
		var head struct {
			ResourceType string `json:"resourceType"`
		}
		_ = json.Unmarshal(b.Entry[i].Resource, &head)
		switch {
		case head.ResourceType == "Patient" && !contained:
			b.Entry[i].Resource = json.RawMessage(patient)
		case head.ResourceType == "Claim" && contained:
			var claim map[string]json.RawMessage
			if err := json.Unmarshal(b.Entry[i].Resource, &claim); err != nil {
				t.Fatal(err)
			}
			claim["contained"] = json.RawMessage(`[` + patient + `]`)
			out, err := json.Marshal(claim)
			if err != nil {
				t.Fatal(err)
			}
			b.Entry[i].Resource = out
		}
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestResolveSubjectPCI_UnheldMemberBindsFromRequestPatientByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	payload := []byte(`{"prefetch":{"patient":` + requestPatient(strangerMember, "1962-03-11", "Nakamura") + `}}`)
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, payload)
	if err != nil || !found {
		t.Fatalf("default, request patient: got (found=%v, err=%v), want found", found, err)
	}
	if want := shnsdk.ResolvePCI(strangerMember, "1962-03-11", "Nakamura"); pci != want {
		t.Fatalf("default, request patient: pci = %q, want the demographics-derived %q (bare id would be %q)", pci, want, strangerPCI())
	}
}

// Without both demographics in a Patient the request carries for THIS member, the
// member binds by id alone, as before.
func TestResolveSubjectPCI_RequestWithoutDemographicsBindsByIDByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	for name, payload := range map[string][]byte{
		"no payload":          nil,
		"no patient":          []byte(`{"prefetch":{"coverage":{"resourceType":"Coverage","id":"c1"}}}`),
		"id only":             []byte(`{"prefetch":{"patient":{"resourceType":"Patient","id":"` + strangerMember + `"}}}`),
		"birthDate only":      []byte(`{"prefetch":{"patient":{"resourceType":"Patient","id":"` + strangerMember + `","birthDate":"1962-03-11"}}}`),
		"family only":         []byte(`{"prefetch":{"patient":{"resourceType":"Patient","id":"` + strangerMember + `","name":[{"family":"Nakamura"}]}}}`),
		"another member's":    []byte(`{"prefetch":{"patient":` + requestPatient("eOTHER", "1962-03-11", "Nakamura") + `}}`),
		"not a Patient":       []byte(`{"prefetch":{"patient":{"resourceType":"RelatedPerson","id":"` + strangerMember + `","name":[{"family":"Nakamura"}],"birthDate":"1962-03-11"}}}`),
		"two that disagree":   []byte(`{"prefetch":{"patient":` + requestPatient(strangerMember, "1962-03-11", "Nakamura") + `},"other":{"patient":` + requestPatient(strangerMember, "1962-03-12", "Nakamura") + `}}`),
		"unparseable payload": []byte(`{"prefetch":`),
	} {
		t.Run(name, func(t *testing.T) {
			pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, payload)
			if err != nil || !found || pci != strangerPCI() {
				t.Fatalf("got (%q, found=%v, err=%v), want bound by id alone %q", pci, found, err, strangerPCI())
			}
		})
	}
}

// Two carried Patients for the member that agree bind as one.
func TestResolveSubjectPCI_AgreeingRequestPatientsBindByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	payload := []byte(`{"prefetch":{"patient":` + requestPatient(strangerMember, "1962-03-11", "Nakamura") + `},"other":{"patient":` + requestPatient(strangerMember, "1962-03-11", "Nakamura") + `}}`)
	pci, _, _ := g.resolveSubjectPCI(context.Background(), strangerMember, payload)
	if want := shnsdk.ResolvePCI(strangerMember, "1962-03-11", "Nakamura"); pci != want {
		t.Fatalf("pci = %q, want %q", pci, want)
	}
}

// A member the system of record holds binds through it: the request's Patient never
// overrides the holder's own record, so a request that disagrees with it does not bind to
// what the request says (the other side, deriving from the request, then fails to match).
func TestResolveSubjectPCI_RequestPatientNeverOverridesHeldMember(t *testing.T) {
	sor := newCensusSoR()
	want, _, _ := sor.ResolvePatient("MBR-COVERED")
	g := &Gateway{cfg: Config{SoR: sor}}
	payload := []byte(`{"prefetch":{"patient":` + requestPatient("MBR-COVERED", "1900-01-01", "Other") + `}}`)
	pci, found, err := g.resolveSubjectPCI(context.Background(), "MBR-COVERED", payload)
	if err != nil || !found || pci != want {
		t.Fatalf("held member with a disagreeing request patient: got (%q, found=%v, err=%v), want the record's %q", pci, found, err, want)
	}
}

// The payer inbound legs bind an unheld member from the Patient where each leg carries
// it: a CRD prefetch value (bare or searchset), a PAS bundle entry or a contained
// resource, a questionnaire parameter.
func TestPayerBind_UnheldMemberBindsFromCarriedPatientByDefault(t *testing.T) {
	const birth, family = "1962-03-11", "Nakamura"
	patient := requestPatient(strangerMember, birth, family)
	want := shnsdk.ResolvePCI(strangerMember, birth, family)
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	ctx := context.Background()

	bind := map[string]func() (string, int, string){
		"CRD prefetch patient, bare": func() (string, int, string) {
			_, _, pci, status, msg := g.conformantCRDBindContext(ctx, conformantCRDWithPatient(strangerMember, "72148", patient))
			return pci, status, msg
		},
		"CRD prefetch patient, searchset": func() (string, int, string) {
			_, _, pci, status, msg := g.conformantCRDBindContext(ctx, conformantCRDWithPatient(strangerMember, "72148", string(searchsetOf(matchOf(patient)))))
			return pci, status, msg
		},
		"PAS bundle entry": func() (string, int, string) {
			_, pci, status, msg := g.conformantPASBindContext(ctx, pasBundleWithPatient(t, strangerMember, patient, false))
			return pci, status, msg
		},
		"PAS contained in the Claim": func() (string, int, string) {
			_, pci, status, msg := g.conformantPASBindContext(ctx, pasBundleWithPatient(t, strangerMember, patient, true))
			return pci, status, msg
		},
		"DTR questionnaire parameter": func() (string, int, string) {
			body := []byte(`{"resourceType":"Parameters","parameter":[{"name":"patient","resource":` + patient + `}]}`)
			return g.bindNextQuestionSubjectContext(ctx, "Patient/"+strangerMember, body)
		},
	}
	for name, f := range bind {
		t.Run(name, func(t *testing.T) {
			if pci, status, msg := f(); status != 0 || pci != want {
				t.Fatalf("status=%d msg=%q pci=%q, want bound to %q", status, msg, pci, want)
			}
		})
	}
}

// The provider ingress derives the same subject from the same carried Patient, so a
// member neither side holds binds identically at both ends.
func TestIngressSubjectPCI_UnheldMemberBindsFromCarriedPatientByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	req := conformantCRDWithPatient(strangerMember, "72148", requestPatient(strangerMember, "1962-03-11", "Nakamura"))
	pci, status, msg := g.ingressCRDSubjectPCIContext(context.Background(), req)
	if status != 0 {
		t.Fatalf("status=%d msg=%q, want bound", status, msg)
	}
	if want := shnsdk.ResolvePCI(strangerMember, "1962-03-11", "Nakamura"); pci != want {
		t.Fatalf("pci=%q, want %q", pci, want)
	}
}

// The pair this fixes: the provider side holds the member, the payer side does not. The
// payer binds the member by the Patient the request carries, as it would directly: to
// the provider's subject when the request agrees with the provider's record, and to its
// own derivation, not the provider's, when it disagrees. Either way the request reaches
// the payer.
func TestSubjectBind_HeldOnIngressUnheldOnPayer(t *testing.T) {
	const member, birth, family = "MBR-COVERED", "1975-04-02", "Johansson" // the census record
	ctx := context.Background()
	provider := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	payer := &Gateway{cfg: Config{SoR: noMemberSoR{newPrefetchSoR()}}}
	recordPCI, _, _ := newCensusSoR().ResolvePatient(member)

	agree := conformantCRDWithPatient(member, "72148", requestPatient(member, birth, family))
	token, status, msg := provider.ingressCRDSubjectPCIContext(ctx, agree)
	if status != 0 || token != recordPCI {
		t.Fatalf("provider bind: status=%d msg=%q pci=%q, want the record's %q", status, msg, token, recordPCI)
	}
	if _, _, pci, status, msg := payer.conformantCRDBindContext(ctx, agree); status != 0 || pci != recordPCI {
		t.Fatalf("payer bind, request agrees with the provider's record: status=%d msg=%q pci=%q, want %q", status, msg, pci, recordPCI)
	}

	// The request's demographics disagree with the provider's record: the
	// provider still binds through its record, and the payer, which does not
	// hold the member, binds by what the request carries.
	disagree := conformantCRDWithPatient(member, "72148", requestPatient(member, "1975-04-03", family))
	token, status, _ = provider.ingressCRDSubjectPCIContext(ctx, disagree)
	if status != 0 || token != recordPCI {
		t.Fatalf("provider bind still through its record: status=%d pci=%q", status, token)
	}
	want := shnsdk.ResolvePCI(member, "1975-04-03", family)
	if _, _, pci, status, msg := payer.conformantCRDBindContext(ctx, disagree); status != 0 || pci != want {
		t.Fatalf("payer bind, request disagrees with the provider's record: status=%d msg=%q pci=%q, want its own %q", status, msg, pci, want)
	}

	// Control: requiring known members, the payer refuses the member it does not hold.
	payer.cfg.RequireKnownMembers = true
	if _, _, _, status, _ := payer.conformantCRDBindContext(ctx, agree); status != http.StatusBadRequest {
		t.Fatalf("known members required, payer bind: status=%d, want 400", status)
	}
}
