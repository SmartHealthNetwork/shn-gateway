package engine

import (
	"context"
	"encoding/json"
	"net/http"
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

// --- the helper itself -------------------------------------------------------------------

func TestResolveSubjectPCI_UnknownMemberRefusedByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, nil)
	if err != nil || found || pci != "" {
		t.Fatalf("default: got (%q, found=%v, err=%v), want not found and no pci", pci, found, err)
	}
}

func TestResolveSubjectPCI_UnknownMemberRefusesUnknownUnderCompatibility(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, nil)
	if err != nil || found || pci != "" {
		t.Fatalf("seam: got (found=%v, err=%v), want unlinked", found, err)
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

// Local construction refuses a mixed-patient request after a real source lookup.
func TestIngressSubjectPCI_MixedMembersStillRefusedUnderSeam(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	for name, req := range map[string][]byte{
		"held member in the order":    crdReqJSON("MBR-COVERED", "Patient/MBR-UC04", "Patient/MBR-COVERED"),
		"second stranger in coverage": crdReqJSON("MBR-COVERED", "Patient/MBR-COVERED", "Patient/eOTHER"),
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

func TestIngressPAS_StrangerRefusedByDefault(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	_, status, msg := g.ingressPASNativeSubjectPCIContext(context.Background(), conformantPASBundleWithQR(t, strangerMember))
	if status != http.StatusBadRequest || msg != "unknown member" {
		t.Fatalf("default PAS ingress: status=%d msg=%q, want 400 unknown member", status, msg)
	}
}

// --- the payer inbound legs: the token subject the ingress side derived must bind here -----

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

func TestResolveSubjectPCI_UnheldMemberRejectsPayloadIdentityUnderCompatibility(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	payload := []byte(`{"prefetch":{"patient":` + requestPatient(strangerMember, "1962-03-11", "Nakamura") + `}}`)
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, payload)
	if err != nil || found || pci != "" {
		t.Fatalf("seam, request patient: got (found=%v, err=%v), want unlinked", found, err)
	}
}

// No carried shape or demographics establish an unprovisioned local identity.
func TestResolveSubjectPCI_AnyUnheldPayloadRemainsUnlinked(t *testing.T) {
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
			if err != nil || found || pci != "" {
				t.Fatalf("got (%q, found=%v, err=%v), want no identity", pci, found, err)
			}
		})
	}
}

// Agreement between carried Patients is not authoritative linkage.
func TestResolveSubjectPCI_AgreeingRequestPatientsCannotSupplyIdentity(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: true}}
	payload := []byte(`{"prefetch":{"patient":` + requestPatient(strangerMember, "1962-03-11", "Nakamura") + `},"other":{"patient":` + requestPatient(strangerMember, "1962-03-11", "Nakamura") + `}}`)
	pci, _, _ := g.resolveSubjectPCI(context.Background(), strangerMember, payload)
	if want := ""; pci != want {
		t.Fatalf("pci = %q, want %q", pci, want)
	}
}

// A member the system of record holds binds through it: the request's Patient never
// overrides the holder's own record, so a request that disagrees with it does not bind to
// what the request says.
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

// These legacy helpers serve explicit local construction/consumption. A known
// source is required independently of native carriage and the compatibility flag.
func TestLocalSubjectBindingCompatibility(t *testing.T) {
	for _, compatibility := range []bool{false, true} {
		g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: compatibility}}
		for _, member := range []string{strangerMember, "MBR-COVERED"} {
			expected := http.StatusBadRequest
			pci := "pci:unlinked"
			if member == "MBR-COVERED" {
				expected = 0
				pci, _, _ = newCensusSoR().ResolvePatient(member)
			}
			patient := requestPatient(member, "1962-03-11", "Nakamura")
			crd := conformantCRDWithPatient(member, "72148", patient)
			pas := pasBundleWithPatient(t, member, patient, false)
			for name, bind := range map[string]func(string) int{
				"crd": func(subject string) int {
					_, _, status, _ := g.conformantCRDBindContext(context.Background(), crd, subject)
					return status
				},
				"pas": func(subject string) int {
					_, status, _ := g.conformantPASBindContext(context.Background(), pas, subject)
					return status
				},
				"dtr": func(subject string) int {
					status, _ := g.bindNextQuestionSubjectContext(context.Background(), "Patient/"+member, subject, nil)
					return status
				},
			} {
				if status := bind(pci); status != expected {
					t.Errorf("%s compat=%v member=%s: status=%d want=%d", name, compatibility, member, status, expected)
				}
				if member == "MBR-COVERED" && bind("pci:foreign") != http.StatusForbidden {
					t.Errorf("%s lost independent token-subject guard", name)
				}
			}
		}
	}
}
