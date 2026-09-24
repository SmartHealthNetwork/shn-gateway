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

func TestResolveSubjectPCI_UnknownMemberRefusedByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, nil)
	if err != nil || found || pci != "" {
		t.Fatalf("default: got (%q, found=%v, err=%v), want not found and no pci", pci, found, err)
	}
}

func TestResolveSubjectPCI_UnknownMemberBindsByIDUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, nil)
	if err != nil || !found {
		t.Fatalf("seam: got (found=%v, err=%v), want found", found, err)
	}
	if pci != strangerPCI() {
		t.Fatalf("seam: pci = %q, want the id-derived %q", pci, strangerPCI())
	}
}

// A member the system of record holds binds through it, seam or not: the seam never
// replaces a resolved identity with the id-derived one.
func TestResolveSubjectPCI_KnownMemberUnchangedUnderSeam(t *testing.T) {
	sor := newCensusSoR()
	want, _, ok := sor.ResolvePatient("MBR-COVERED")
	if !ok {
		t.Fatal("fixture: MBR-COVERED must resolve")
	}
	g := &Gateway{cfg: Config{SoR: sor, AcceptUnknownMembers: true}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), "MBR-COVERED", nil)
	if err != nil || !found || pci != want {
		t.Fatalf("seam, known member: got (%q, found=%v, err=%v), want %q", pci, found, err, want)
	}
	if pci == shnsdk.ResolvePCI("MBR-COVERED", "", "") {
		t.Fatal("seam, known member: bound by id alone instead of through the system of record")
	}
}

// An unreadable system of record is never mistaken for a member it does not hold.
func TestResolveSubjectPCI_ReadFailureSurfacesUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: failingSubjectSoR{newPrefetchSoR()}, AcceptUnknownMembers: true}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, nil)
	if err == nil || found || pci != "" {
		t.Fatalf("seam, read failure: got (%q, found=%v, err=%v), want the read error", pci, found, err)
	}
}

// --- the provider ingress legs ------------------------------------------------------------

func TestIngressSubjectPCI_StrangerRefusedByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	ref := "Patient/" + strangerMember
	_, status, msg := g.ingressCRDSubjectPCIContext(context.Background(), crdReqJSON(strangerMember, ref, ref))
	if status != http.StatusBadRequest || msg != "unknown member" {
		t.Fatalf("default CRD ingress: status=%d msg=%q, want 400 unknown member", status, msg)
	}
}

func TestIngressSubjectPCI_StrangerBindsUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	ref := "Patient/" + strangerMember
	pci, status, msg := g.ingressCRDSubjectPCIContext(context.Background(), crdReqJSON(strangerMember, ref, ref))
	if status != 0 {
		t.Fatalf("seam CRD ingress: status=%d msg=%q, want bound", status, msg)
	}
	if pci != strangerPCI() {
		t.Fatalf("seam CRD ingress: pci=%q, want %q", pci, strangerPCI())
	}
}

// Rejection row: the seam binds ONE stranger; a request that mixes the stranger with another
// member — held or not — is still refused.
func TestIngressSubjectPCI_MixedMembersStillRefusedUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
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

func TestIngressDTR_StrangerRefusedByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	_, status, msg := g.prepareDTRPackageRequest(context.Background(), dtrPackageFor(strangerMember))
	if status != http.StatusForbidden || msg != "request patient does not resolve" {
		t.Fatalf("default DTR ingress: status=%d msg=%q, want 403 request patient does not resolve", status, msg)
	}
}

func TestIngressDTR_StrangerBindsUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	out, status, msg := g.prepareDTRPackageRequest(context.Background(), dtrPackageFor(strangerMember))
	if status != 0 {
		t.Fatalf("seam DTR ingress: status=%d msg=%q, want bound", status, msg)
	}
	if out.pci != strangerPCI() || out.member != strangerMember {
		t.Fatalf("seam DTR ingress: bound (%q, %q), want (%q, %q)", out.member, out.pci, strangerMember, strangerPCI())
	}
}

func TestIngressPAS_StrangerRefusedByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	_, status, msg := g.ingressPASNativeSubjectPCIContext(context.Background(), conformantPASBundleWithQR(t, strangerMember))
	if status != http.StatusBadRequest || msg != "unknown member" {
		t.Fatalf("default PAS ingress: status=%d msg=%q, want 400 unknown member", status, msg)
	}
}

func TestIngressPAS_StrangerBindsUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	pci, status, msg := g.ingressPASNativeSubjectPCIContext(context.Background(), conformantPASBundleWithQR(t, strangerMember))
	if status != 0 || pci != strangerPCI() {
		t.Fatalf("seam PAS ingress: status=%d msg=%q pci=%q, want bound to %q", status, msg, pci, strangerPCI())
	}
}

// The inquiry ingress binds through the same helper: by default a stranger is refused at
// the bind (400); under the seam it passes the bind and is refused where the control is,
// at routing (422, this fixture's Coverage names no payer), so the difference is the bind.
func TestIngressInquire_StrangerBindsUnderSeam(t *testing.T) {
	post := func(t *testing.T, seam bool, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		g := &Gateway{cfg: Config{ingressAuthBypass: true, SoR: newCensusSoR(), Clock: fixedClock, AcceptUnknownMembers: seam}}
		w := httptest.NewRecorder()
		g.handlePASInquireIngress(w, httptest.NewRequest(http.MethodPost, "/Claim/$inquire", bytes.NewReader(body)))
		return w
	}
	stranger := inquiryBundle(strangerMember, "", "TRN-1", "72148")
	if w := post(t, false, stranger); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown member") {
		t.Fatalf("default inquiry ingress: status=%d body=%s, want 400 unknown member", w.Code, w.Body)
	}
	if w := post(t, true, stranger); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("seam inquiry ingress: status=%d body=%s, want 422 (past the bind, refused at routing)", w.Code, w.Body)
	}
	// Rejection row: two members are still refused at the bind under the seam.
	if w := post(t, true, inquiryBundle(strangerMember, "MBR-COVERED", "TRN-1", "72148")); w.Code != http.StatusForbidden {
		t.Fatalf("seam inquiry ingress, two members: status=%d body=%s, want 403", w.Code, w.Body)
	}
}

// --- the payer inbound legs: the token subject the ingress side derived must bind here -----

func TestPayerCRDBind_StrangerBindsToDerivedSubjectUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	_, _, status, msg := g.conformantCRDBindContext(context.Background(), conformantCRD(strangerMember, "72148"), strangerPCI())
	if status != 0 {
		t.Fatalf("seam payer CRD: status=%d msg=%q, want bound", status, msg)
	}
	// Rejection row: a token for someone else still does not bind the stranger.
	_, _, status, _ = g.conformantCRDBindContext(context.Background(), conformantCRD(strangerMember, "72148"), "pci:someone-else")
	if status != http.StatusForbidden {
		t.Fatalf("seam payer CRD, wrong token subject: status=%d, want 403", status)
	}
	// And without the seam the payer refuses the stranger as before.
	g.cfg.AcceptUnknownMembers = false
	_, _, status, _ = g.conformantCRDBindContext(context.Background(), conformantCRD(strangerMember, "72148"), strangerPCI())
	if status != http.StatusBadRequest {
		t.Fatalf("default payer CRD: status=%d, want 400", status)
	}
}

func TestPayerPASBind_StrangerBindsToDerivedSubjectUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	bundle := conformantPASBundleWithQR(t, strangerMember)
	memberRef, status, msg := g.conformantPASBindContext(context.Background(), bundle, strangerPCI())
	if status != 0 || memberRef != "Patient/"+strangerMember {
		t.Fatalf("seam payer PAS: status=%d msg=%q memberRef=%q, want bound", status, msg, memberRef)
	}
	if _, status, _ := g.conformantPASBindContext(context.Background(), bundle, "pci:someone-else"); status != http.StatusForbidden {
		t.Fatalf("seam payer PAS, wrong token subject: status=%d, want 403", status)
	}
	g.cfg.AcceptUnknownMembers = false
	if _, status, _ := g.conformantPASBindContext(context.Background(), bundle, strangerPCI()); status != http.StatusBadRequest {
		t.Fatalf("default payer PAS: status=%d, want 400", status)
	}
}

func TestPayerDTRBind_StrangerBindsToDerivedSubjectUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	if status, msg := g.bindNextQuestionSubjectContext(context.Background(), "Patient/"+strangerMember, strangerPCI(), nil); status != 0 {
		t.Fatalf("seam payer DTR: status=%d msg=%q, want bound", status, msg)
	}
	if status, _ := g.bindNextQuestionSubjectContext(context.Background(), "Patient/"+strangerMember, "pci:someone-else", nil); status != http.StatusForbidden {
		t.Fatalf("seam payer DTR, wrong token subject: status=%d, want 403", status)
	}
	g.cfg.AcceptUnknownMembers = false
	if status, _ := g.bindNextQuestionSubjectContext(context.Background(), "Patient/"+strangerMember, strangerPCI(), nil); status != http.StatusBadRequest {
		t.Fatalf("default payer DTR: status=%d, want 400", status)
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
	env.originator.cfg.SoR = s.sor()
	env.originator.cfg.Observer = obs.observe
	env.originator.cfg.AcceptUnknownMembers = true
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
			env.originator.cfg.SoR = s.sor()
			env.originator.cfg.AcceptUnknownMembers = true
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "patient not found in system of record")
		})
	}
	t.Run("without the seam", func(t *testing.T) {
		s := newPrefetchSoR()
		env := newInProcessExchange(t)
		env.originator.cfg.SoR = s.sor()
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

func TestResolveSubjectPCI_UnheldMemberBindsFromRequestPatientUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	payload := []byte(`{"prefetch":{"patient":` + requestPatient(strangerMember, "1962-03-11", "Nakamura") + `}}`)
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, payload)
	if err != nil || !found {
		t.Fatalf("seam, request patient: got (found=%v, err=%v), want found", found, err)
	}
	if want := shnsdk.ResolvePCI(strangerMember, "1962-03-11", "Nakamura"); pci != want {
		t.Fatalf("seam, request patient: pci = %q, want the demographics-derived %q (bare id would be %q)", pci, want, strangerPCI())
	}
}

// Without both demographics in a Patient the request carries for THIS member, the
// member binds by id alone, as before.
func TestResolveSubjectPCI_RequestWithoutDemographicsBindsByIDUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
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
func TestResolveSubjectPCI_AgreeingRequestPatientsBindUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
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
	g := &Gateway{cfg: Config{SoR: sor, AcceptUnknownMembers: true}}
	payload := []byte(`{"prefetch":{"patient":` + requestPatient("MBR-COVERED", "1900-01-01", "Other") + `}}`)
	pci, found, err := g.resolveSubjectPCI(context.Background(), "MBR-COVERED", payload)
	if err != nil || !found || pci != want {
		t.Fatalf("held member with a disagreeing request patient: got (%q, found=%v, err=%v), want the record's %q", pci, found, err, want)
	}
}

// The payer inbound legs bind an unheld member from the Patient where each leg carries
// it: a CRD prefetch value (bare or searchset), a PAS bundle entry or a contained
// resource, a questionnaire parameter. The bare id no longer matches once demographics
// ride along, so a token derived by id alone is refused.
func TestPayerBind_UnheldMemberBindsFromCarriedPatientUnderSeam(t *testing.T) {
	const birth, family = "1962-03-11", "Nakamura"
	patient := requestPatient(strangerMember, birth, family)
	want := shnsdk.ResolvePCI(strangerMember, birth, family)
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	ctx := context.Background()

	bind := map[string]func(token string) (int, string){
		"CRD prefetch patient, bare": func(token string) (int, string) {
			_, _, status, msg := g.conformantCRDBindContext(ctx, conformantCRDWithPatient(strangerMember, "72148", patient), token)
			return status, msg
		},
		"CRD prefetch patient, searchset": func(token string) (int, string) {
			_, _, status, msg := g.conformantCRDBindContext(ctx, conformantCRDWithPatient(strangerMember, "72148", string(searchsetOf(matchOf(patient)))), token)
			return status, msg
		},
		"PAS bundle entry": func(token string) (int, string) {
			_, status, msg := g.conformantPASBindContext(ctx, pasBundleWithPatient(t, strangerMember, patient, false), token)
			return status, msg
		},
		"PAS contained in the Claim": func(token string) (int, string) {
			_, status, msg := g.conformantPASBindContext(ctx, pasBundleWithPatient(t, strangerMember, patient, true), token)
			return status, msg
		},
		"DTR questionnaire parameter": func(token string) (int, string) {
			body := []byte(`{"resourceType":"Parameters","parameter":[{"name":"patient","resource":` + patient + `}]}`)
			return g.bindNextQuestionSubjectContext(ctx, "Patient/"+strangerMember, token, body)
		},
	}
	for name, f := range bind {
		t.Run(name, func(t *testing.T) {
			if status, msg := f(want); status != 0 {
				t.Fatalf("token derived from the carried demographics: status=%d msg=%q, want bound", status, msg)
			}
			if status, _ := f(strangerPCI()); status != http.StatusForbidden {
				t.Fatalf("token derived by id alone while demographics ride along: status=%d, want 403", status)
			}
		})
	}
}

// The provider ingress derives the same subject from the same carried Patient, so a
// member neither side holds binds identically at both ends.
func TestIngressSubjectPCI_UnheldMemberBindsFromCarriedPatientUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
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
// subject the provider derives from its record and the one the payer derives from the
// carried Patient agree when the request agrees with the record — and a request that
// disagrees with the holder's record is still refused at the payer (403).
func TestSubjectBind_HeldOnIngressUnheldOnPayer(t *testing.T) {
	const member, birth, family = "MBR-COVERED", "1975-04-02", "Johansson" // the census record
	ctx := context.Background()
	provider := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	payer := &Gateway{cfg: Config{SoR: noMemberSoR{newPrefetchSoR()}, AcceptUnknownMembers: true}}
	recordPCI, _, _ := newCensusSoR().ResolvePatient(member)

	agree := conformantCRDWithPatient(member, "72148", requestPatient(member, birth, family))
	token, status, msg := provider.ingressCRDSubjectPCIContext(ctx, agree)
	if status != 0 || token != recordPCI {
		t.Fatalf("provider bind: status=%d msg=%q pci=%q, want the record's %q", status, msg, token, recordPCI)
	}
	if _, _, status, msg := payer.conformantCRDBindContext(ctx, agree, token); status != 0 {
		t.Fatalf("payer bind, request agrees with the provider's record: status=%d msg=%q, want bound", status, msg)
	}

	// Rejection row: the request's demographics disagree with the holder's record.
	disagree := conformantCRDWithPatient(member, "72148", requestPatient(member, "1975-04-03", family))
	token, status, _ = provider.ingressCRDSubjectPCIContext(ctx, disagree)
	if status != 0 || token != recordPCI {
		t.Fatalf("provider bind still through its record: status=%d pci=%q", status, token)
	}
	if _, _, status, msg := payer.conformantCRDBindContext(ctx, disagree, token); status != http.StatusForbidden || msg != "token subject does not match request patient" {
		t.Fatalf("payer bind, request disagrees with the provider's record: status=%d msg=%q, want 403", status, msg)
	}

	// Control: without the seam the payer refuses the member it does not hold, as before.
	payer.cfg.AcceptUnknownMembers = false
	if _, _, status, _ := payer.conformantCRDBindContext(ctx, agree, recordPCI); status != http.StatusBadRequest {
		t.Fatalf("default payer bind: status=%d, want 400", status)
	}
}
