//go:build !shn_coreonly

package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// relPatient is another Patient for the member: under another id, carrying the member's
// number as its member identifier, with demographics of its own, which disagree with the
// member's own Patient (withMembersOwnPatient's).
func relPatient() map[string]any {
	return map[string]any{
		"resourceType": "Patient", "id": "rel",
		"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
		"name":       []any{map[string]any{"family": "Othername"}}, "birthDate": "1999-01-01",
	}
}

// subjectLeg is one leg's request with the member's own Patient as its subject resource,
// how a second Patient is added to it at the top level, nested and contained, and the
// leg's binding on the requester's gateway and on the payer's.
type subjectLeg struct {
	request  func(t *testing.T) map[string]any
	top      func(doc, p map[string]any)
	nested   func(doc, p map[string]any)
	provider func(g *Gateway, body []byte) (string, int, string)
	payer    func(g *Gateway, body []byte) (string, int, string)
	// subject is the leg's own subject-resource reader, for the binding's own answer
	// where strict refuses the request first for a reason of its own.
	subject func(body []byte) []byte
}

// inBundle is p as the one entry of a collection Bundle.
func inBundle(p map[string]any) map[string]any {
	return map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{map[string]any{"resource": p}}}
}

// inBasic is p contained in a Basic resource (no Coverage: it names no party).
func inBasic(p map[string]any) map[string]any {
	return map[string]any{"resourceType": "Basic", "id": "holder", "code": map[string]any{"text": "x"}, "contained": []any{p}}
}

func subjectLegs() map[string]subjectLeg {
	ctx := context.Background()
	own := func(t *testing.T, body []byte, add func(map[string]any, map[string]any)) map[string]any {
		return decodeDoc(t, withMembersOwnPatient(t, body, add))
	}
	entry := func(url string) func(doc, p map[string]any) { return asBundleEntry(url) }
	parameter := func(name string) func(doc, p map[string]any) {
		return func(doc, p map[string]any) {
			list, _ := doc["parameter"].([]any)
			doc["parameter"] = append(list, map[string]any{"name": name, "resource": p})
		}
	}
	prefetch := func(key string) func(doc, p map[string]any) {
		return func(doc, p map[string]any) {
			pf, _ := doc["prefetch"].(map[string]any)
			pf[key] = p
		}
	}
	return map[string]subjectLeg{
		"crd-order-select": {
			request: func(t *testing.T) map[string]any {
				return own(t, crdReqJSON(strangerMember, "Patient/"+strangerMember, "Patient/"+strangerMember), nil)
			},
			top:    prefetch("relative"),
			nested: func(doc, p map[string]any) { prefetch("relatives")(doc, inBundle(p)) },
			provider: func(g *Gateway, b []byte) (string, int, string) {
				return g.ingressCRDSubjectPCIContext(ctx, b)
			},
			payer: func(g *Gateway, b []byte) (string, int, string) {
				_, _, pci, status, msg := g.conformantCRDBindContext(ctx, b)
				return pci, status, msg
			},
			subject: func(b []byte) []byte { return crdSubjectResource(b, strangerMember) },
		},
		"crd-order-dispatch": {
			request: func(t *testing.T) map[string]any {
				return decodeDoc(t, []byte(`{"hook":"order-dispatch","hookInstance":"hi-2","fhirServer":"https://provider.example/fhir","context":{"patientId":"`+strangerMember+`","dispatchedOrders":["DeviceRequest/dr1"],"performer":"Organization/dme1"},"prefetch":{"patient":{"resourceType":"Patient","id":"`+strangerMember+`","name":[{"family":"Childname"}],"birthDate":"2015-06-01"},"deviceHistory":{"resourceType":"Bundle","entry":[{"fullUrl":"DeviceRequest/dr1","resource":{"resourceType":"DeviceRequest","id":"dr1","subject":{"reference":"Patient/`+strangerMember+`"}}}]},"coverage":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/`+strangerMember+`"}}}]}}}`))
			},
			top:    prefetch("relative"),
			nested: func(doc, p map[string]any) { prefetch("relatives")(doc, inBundle(p)) },
			payer: func(g *Gateway, b []byte) (string, int, string) {
				_, _, pci, status, msg := g.conformantCRDDispatchBindContext(ctx, b)
				return pci, status, msg
			},
			subject: func(b []byte) []byte { return crdSubjectResource(b, strangerMember) },
		},
		// The subject Patient as a searchset Bundle's entry in the prefetch.
		"crd-order-select, a searchset prefetch": {
			request: func(t *testing.T) map[string]any {
				d := own(t, crdReqJSON(strangerMember, "Patient/"+strangerMember, "Patient/"+strangerMember), nil)
				pf := d["prefetch"].(map[string]any)
				pf["patient"] = map[string]any{"resourceType": "Bundle", "type": "searchset", "entry": []any{map[string]any{"resource": pf["patient"]}}}
				return d
			},
			top:    prefetch("relative"),
			nested: func(doc, p map[string]any) { prefetch("relatives")(doc, inBundle(p)) },
			provider: func(g *Gateway, b []byte) (string, int, string) {
				return g.ingressCRDSubjectPCIContext(ctx, b)
			},
			payer: func(g *Gateway, b []byte) (string, int, string) {
				_, _, pci, status, msg := g.conformantCRDBindContext(ctx, b)
				return pci, status, msg
			},
			subject: func(b []byte) []byte { return crdSubjectResource(b, strangerMember) },
		},
		"dtr-questionnaire-fetch package": {
			request: func(t *testing.T) map[string]any {
				doc := decodeDoc(t, dtrPackageFor(strangerMember))
				parameter("patient")(doc, map[string]any{"resourceType": "Patient", "id": strangerMember,
					"name": []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"})
				return doc
			},
			top:    parameter("relative"),
			nested: func(doc, p map[string]any) { parameter("relatives")(doc, inBundle(p)) },
			provider: func(g *Gateway, b []byte) (string, int, string) {
				out, status, msg := g.prepareDTRPackageRequest(ctx, b)
				return out.pci, status, msg
			},
			payer:   func(g *Gateway, b []byte) (string, int, string) { return g.bindPackageParameters(ctx, b) },
			subject: func(b []byte) []byte { return dtrSubjectResource(b, strangerMember) },
		},
		"dtr-questionnaire-fetch next question": {
			request: func(t *testing.T) map[string]any {
				return decodeDoc(t, []byte(`{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":{"resourceType":"QuestionnaireResponse","id":"qr1","status":"in-progress","questionnaire":"http://example.org/fhir/Questionnaire/HomeHealthAssessment","subject":{"reference":"Patient/`+strangerMember+`"}}},{"name":"patient","resource":{"resourceType":"Patient","id":"`+strangerMember+`","name":[{"family":"Childname"}],"birthDate":"2015-06-01"}}]}`))
			},
			top:    parameter("relative"),
			nested: func(doc, p map[string]any) { parameter("relatives")(doc, inBundle(p)) },
			provider: func(g *Gateway, b []byte) (string, int, string) {
				return g.bindNextQuestionRound(httptest.NewRequest(http.MethodPost, "/", nil), b)
			},
			payer: func(g *Gateway, b []byte) (string, int, string) {
				return g.bindNextQuestionSubjectContext(ctx, "Patient/"+strangerMember, b)
			},
			subject: func(b []byte) []byte { return dtrSubjectResource(b, strangerMember) },
		},
		"pas-claim": {
			request: func(t *testing.T) map[string]any { return own(t, conformantPASBundleWithQR(t, strangerMember), nil) },
			top:     entry("urn:uuid:relative"),
			nested:  func(doc, p map[string]any) { entry("urn:uuid:relatives")(doc, inBundle(p)) },
			provider: func(g *Gateway, b []byte) (string, int, string) {
				return g.ingressPASNativeSubjectPCIContext(ctx, b)
			},
			payer: func(g *Gateway, b []byte) (string, int, string) {
				_, pci, status, msg := g.conformantPASBindContext(ctx, b)
				return pci, status, msg
			},
			subject: pasSubjectResource,
		},
		"pas-claim-inquire": {
			request: func(t *testing.T) map[string]any {
				return own(t, inquiryBundle(strangerMember, "", "TRN-1", "72148"), nil)
			},
			top:    entry("urn:uuid:relative"),
			nested: func(doc, p map[string]any) { entry("urn:uuid:relatives")(doc, inBundle(p)) },
			provider: func(g *Gateway, b []byte) (string, int, string) {
				pci, found, err := g.inquiryIngressSubject(ctx, strangerMember, b)
				if err != nil || !found {
					status, msg := subjectBindingFailure(ctx, err)
					return "", status, msg
				}
				return pci, 0, ""
			},
			payer: func(g *Gateway, b []byte) (string, int, string) {
				return g.inquiryInboundSubject(ctx, strangerMember, b)
			},
			subject: pasSubjectResource,
		},
		// Claim.patient as the urn:uuid: fullUrl of the Patient entry.
		"pas-claim-inquire, a urn Claim.patient": {
			request: func(t *testing.T) map[string]any {
				d := own(t, inquiryBundle(strangerMember, "", "TRN-1", "72148"), nil)
				for _, e := range d["entry"].([]any) {
					if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "Claim" {
						r["patient"] = map[string]any{"reference": "urn:uuid:patient"}
					}
				}
				return d
			},
			top:    entry("urn:uuid:relative"),
			nested: func(doc, p map[string]any) { entry("urn:uuid:relatives")(doc, inBundle(p)) },
			provider: func(g *Gateway, b []byte) (string, int, string) {
				pci, found, err := g.inquiryIngressSubject(ctx, strangerMember, b)
				if err != nil || !found {
					status, msg := subjectBindingFailure(ctx, err)
					return "", status, msg
				}
				return pci, 0, ""
			},
			payer: func(g *Gateway, b []byte) (string, int, string) {
				return g.inquiryInboundSubject(ctx, strangerMember, b)
			},
			subject: pasSubjectResource,
		},
		// Claim.patient as a "#<id>" naming the Patient the Claim contains.
		"pas-claim-inquire, a contained Claim.patient": {
			request: func(t *testing.T) map[string]any {
				d := decodeDoc(t, inquiryBundle(strangerMember, "", "TRN-1", "72148"))
				for _, e := range d["entry"].([]any) {
					if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "Claim" {
						r["patient"] = map[string]any{"reference": "#p"}
						r["contained"] = []any{map[string]any{"resourceType": "Patient", "id": "p",
							"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
							"name":       []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"}}
					}
				}
				return d
			},
			top:    entry("urn:uuid:relative"),
			nested: func(doc, p map[string]any) { entry("urn:uuid:relatives")(doc, inBundle(p)) },
			provider: func(g *Gateway, b []byte) (string, int, string) {
				pci, found, err := g.inquiryIngressSubject(ctx, strangerMember, b)
				if err != nil || !found {
					status, msg := subjectBindingFailure(ctx, err)
					return "", status, msg
				}
				return pci, 0, ""
			},
			payer: func(g *Gateway, b []byte) (string, int, string) {
				return g.inquiryInboundSubject(ctx, strangerMember, b)
			},
			subject: pasSubjectResource,
		},
		"coverage-eligibility": {
			request: func(t *testing.T) map[string]any {
				req := decodeDoc(t, eligibilityRequest(t, strangerMember))
				req["patient"] = map[string]any{"reference": "#p"}
				req["contained"] = []any{map[string]any{"resourceType": "Patient", "id": "p",
					"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
					"name":       []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"}}
				return req
			},
			top: func(doc, p map[string]any) {
				list, _ := doc["contained"].([]any)
				doc["contained"] = append(list, p)
			},
			nested: func(doc, p map[string]any) {
				list, _ := doc["contained"].([]any)
				doc["contained"] = append(list, inBasic(p))
			},
			provider: func(g *Gateway, b []byte) (string, int, string) {
				sub, member, status, msg := firstEligibility(b)
				if status != 0 {
					return "", status, msg
				}
				pci, found, err := g.eligibilityIngressSubject(ctx, member, sub)
				if err != nil || !found {
					status, msg := subjectBindingFailure(ctx, err)
					return "", status, msg
				}
				return pci, 0, ""
			},
			payer: func(g *Gateway, b []byte) (string, int, string) {
				sub, member, status, msg := firstEligibility(b)
				if status != 0 {
					return "", status, msg
				}
				return g.eligibilityInboundSubject(ctx, member, sub)
			},
			subject: func(b []byte) []byte {
				sub, _, _, _ := firstEligibility(b)
				return eligibilitySubjectResource(sub)
			},
		},
		"federated-query": {
			request: func(t *testing.T) map[string]any {
				task := decodeDoc(t, cdexTaskFor(t, strangerMember))
				task["for"] = map[string]any{"reference": "#p"}
				task["contained"] = []any{map[string]any{"resourceType": "Patient", "id": "p",
					"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
					"name":       []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"}}
				return task
			},
			top: func(doc, p map[string]any) {
				list, _ := doc["contained"].([]any)
				doc["contained"] = append(list, p)
			},
			nested: func(doc, p map[string]any) {
				list, _ := doc["contained"].([]any)
				doc["contained"] = append(list, inBasic(p))
			},
			provider: func(g *Gateway, b []byte) (string, int, string) {
				pci, found, err := g.fqsIngressSubject(ctx, strangerMember, b)
				if err != nil || !found {
					status, msg := subjectBindingFailure(ctx, err)
					return "", status, msg
				}
				return pci, 0, ""
			},
			subject: fqsSubjectResource,
		},
	}
}

// On every leg that has one, the request's own subject resource decides the binding of a
// member the participant's system of record does not hold, on the requester's gateway and
// the payer's alike: another Patient the request carries for the member, at the top
// level, nested in a Bundle or contained in another resource, whose demographics
// disagree, no longer makes the member bind by its id alone. It is bound so at every
// level. Where strict refuses such a request first, for a check of the leg's own that
// earlier releases made too (strictRefusesFirst), the row pins that refusal, and the
// leg's binding of it is still the subject's.
func TestSubjectResource_EveryLegBindsFromItsSubject(t *testing.T) {
	t.Parallel()
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	for name, l := range subjectLegs() {
		for where, add := range map[string]func(doc, p map[string]any){
			"at the top level": l.top,
			"nested":           l.nested,
			"contained":        func(doc, p map[string]any) { l.top(doc, inBasic(p)) },
		} {
			for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
				t.Run(name+"/"+where+"/"+level.String(), func(t *testing.T) {
					doc := l.request(t)
					add(doc, relPatient())
					body := mustJSON(t, doc)
					gl := &Gateway{cfg: g.cfg}
					gl.cfg.ConformanceEnforcement = level
					for side, bind := range map[string]func(*Gateway, []byte) (string, int, string){"provider": l.provider, "payer": l.payer} {
						if bind == nil {
							continue
						}
						pci, status, msg := bind(gl, body)
						if want, first := strictRefusesFirst[name+"/"+where+"/"+side]; first && level == EnforcementStrict {
							if status != want.status || msg != want.msg {
								t.Fatalf("%s at strict: (%q, %d, %q), want its own %d %q", side, pci, status, msg, want.status, want.msg)
							}
							continue
						}
						if status != 0 || pci != pinnedChildIdentity {
							t.Fatalf("%s: (%q, %d, %q), want bound from the subject resource %q", side, pci, status, msg, pinnedChildIdentity)
						}
					}
					// The leg's binding of the request, whatever a check before it answers.
					if pci, found, err := gl.resolveSubjectPCIFrom(context.Background(), strangerMember, body, l.subject(body)); err != nil || !found || pci != pinnedChildIdentity {
						t.Fatalf("binding: (%q, %v, %v), want %q", pci, found, err, pinnedChildIdentity)
					}
				})
			}
		}
	}
}

// The control: with no second Patient, every leg binds from its subject resource as before.
func TestSubjectResource_EveryLegControl(t *testing.T) {
	t.Parallel()
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	for name, l := range subjectLegs() {
		t.Run(name, func(t *testing.T) {
			body := mustJSON(t, l.request(t))
			for side, bind := range map[string]func(*Gateway, []byte) (string, int, string){"provider": l.provider, "payer": l.payer} {
				if bind == nil {
					continue
				}
				if pci, status, msg := bind(g, body); status != 0 || pci != pinnedChildIdentity {
					t.Fatalf("%s: (%q, %d, %q), want %q", side, pci, status, msg, pinnedChildIdentity)
				}
			}
		})
	}
	if !strings.HasPrefix(pinnedChildIdentity, "pci:") {
		t.Fatal("pinnedChildIdentity")
	}
}

// firstEligibility is an eligibility input's first request and its member, as the leg reads
// them (eligibilitySubmissions, eligibilitySubmissionMember).
func firstEligibility(body []byte) (eligibilitySubmission, string, int, string) {
	subs, err := eligibilitySubmissions(body)
	if err != nil || len(subs) == 0 {
		return eligibilitySubmission{}, "", http.StatusBadRequest, "parse eligibility failed"
	}
	member, err := eligibilitySubmissionMember(subs[0])
	if err != nil {
		return eligibilitySubmission{}, "", http.StatusBadRequest, err.Error()
	}
	return subs[0], member, 0, ""
}

// strictRefusesFirst are the requests above that strict refuses before or beside their
// binding, for a check of the leg's own that earlier releases made of the same request
// (the same answer before and after the subject resource decides), keyed leg/where/side.
var strictRefusesFirst = map[string]struct {
	status int
	msg    string
}{
	"crd-order-select/at the top level/provider":                       {http.StatusForbidden, "inconsistent patient reference in ingress payload"},
	"crd-order-select, a searchset prefetch/at the top level/provider": {http.StatusForbidden, "inconsistent patient reference in ingress payload"},
	"dtr-questionnaire-fetch next question/contained/provider":         {http.StatusForbidden, "inconsistent patient reference in ingress payload"},
	"dtr-questionnaire-fetch package/contained/provider":               {http.StatusForbidden, "parameter relative refused: Basic refused: no reference to the patient"},
	"dtr-questionnaire-fetch package/at the top level/payer":           {http.StatusForbidden, "questionnaire-package request covers more than one patient"},
	"dtr-questionnaire-fetch package/nested/payer":                     {http.StatusForbidden, "questionnaire-package request covers more than one patient"},
}

// A subject reference that resolves to two Patients that differ resolves to none: the
// request is bound as one with no subject resource (from the Patients it carries for the
// member, which here disagree: by the member id alone). A Patient contained in another
// resource is not one a DTR subject reference resolves to: its id is local.
func TestSubjectResource_TwoThatDifferAndContainedIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), ConformanceEnforcement: EnforcementNone}}
	other := map[string]any{"resourceType": "Patient", "id": strangerMember, "name": []any{map[string]any{"family": "Othername"}}, "birthDate": "1999-01-01"}
	crd := decodeDoc(t, crdReqJSON(strangerMember, "Patient/"+strangerMember, "Patient/"+strangerMember))
	pf := crd["prefetch"].(map[string]any)
	pf["patient"] = map[string]any{"resourceType": "Patient", "id": strangerMember, "name": []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"}
	pf["again"] = other
	crdB := mustJSON(t, crd)
	if crdSubjectResource(crdB, strangerMember) != nil {
		t.Fatal("two differing prefetch Patients under the subject's id resolved to one")
	}
	if pci, _, _ := g.ingressCRDSubjectPCIContext(ctx, crdB); pci != strangerPCI() {
		t.Fatalf("CRD, two that differ: %q, want the member id alone %q", pci, strangerPCI())
	}
	dtr := decodeDoc(t, dtrPackageFor(strangerMember))
	dtr["parameter"] = append(dtr["parameter"].([]any),
		map[string]any{"name": "patient", "resource": map[string]any{"resourceType": "Patient", "id": strangerMember, "name": []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"}},
		map[string]any{"name": "holder", "resource": inBasic(other)})
	dtrB := mustJSON(t, dtr)
	if pci, status, msg := g.bindPackageParameters(ctx, dtrB); status != 0 || pci != pinnedChildIdentity {
		t.Fatalf("DTR, a contained Patient under the subject's id: (%q, %d, %q), want the subject's %q", pci, status, msg, pinnedChildIdentity)
	}
	if out, status, msg := g.prepareDTRPackageRequest(ctx, dtrB); status != 0 || out.pci != pinnedChildIdentity {
		t.Fatalf("DTR ingress, a contained Patient under the subject's id: (%q, %d, %q), want the subject's %q", out.pci, status, msg, pinnedChildIdentity)
	}
}
