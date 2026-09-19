package engine

import (
	"bytes"
	"context"
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
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember)
	if err != nil || found || pci != "" {
		t.Fatalf("default: got (%q, found=%v, err=%v), want not found and no pci", pci, found, err)
	}
}

func TestResolveSubjectPCI_UnknownMemberBindsByIDUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember)
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
	pci, found, err := g.resolveSubjectPCI(context.Background(), "MBR-COVERED")
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
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember)
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
	if status, msg := g.bindNextQuestionSubjectContext(context.Background(), "Patient/"+strangerMember, strangerPCI()); status != 0 {
		t.Fatalf("seam payer DTR: status=%d msg=%q, want bound", status, msg)
	}
	if status, _ := g.bindNextQuestionSubjectContext(context.Background(), "Patient/"+strangerMember, "pci:someone-else"); status != http.StatusForbidden {
		t.Fatalf("seam payer DTR, wrong token subject: status=%d, want 403", status)
	}
	g.cfg.AcceptUnknownMembers = false
	if status, _ := g.bindNextQuestionSubjectContext(context.Background(), "Patient/"+strangerMember, strangerPCI()); status != http.StatusBadRequest {
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
