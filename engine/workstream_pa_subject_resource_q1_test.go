//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"net/http"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// q1Patient is a Patient for the member under id, by its member identifier, with the
// given demographics ("" for none).
func q1Patient(id, family, birth string) map[string]any {
	p := map[string]any{"resourceType": "Patient", "id": id,
		"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}}
	if family != "" {
		p["name"], p["birthDate"] = []any{map[string]any{"family": family}}, birth
	}
	return p
}

// q1Coverage names rel as its beneficiary and, with party, a parent under the member's
// number as its subscriber.
func q1Coverage(cov map[string]any, beneficiary string, party bool) {
	cov["beneficiary"] = map[string]any{"reference": beneficiary}
	if party {
		cov["subscriber"] = map[string]any{"reference": "#parent"}
		cov["contained"] = []any{q1Patient("parent", "Parentname", "1970-01-01")}
	}
}

// A request whose own subject resource carries no birth date or family name, beside
// another Patient for the member that carries both (the Coverage's beneficiary), binds
// by the member id alone: earlier releases bound it from that other Patient. Where the
// request also carries another person under the member's number (a Coverage's
// subscriber), the member id alone identifies no one patient, and the request is
// refused 400, recorded as authority, at every level, on the requester's gateway and the
// payer's. At strict each leg's own patient-consistency check, which earlier releases
// made too, refuses the request first: the binding's own answer is then still the
// refusal (resolveSubjectPCIFrom).
func TestSubjectResource_TheSubjectWithoutDemographicsBindsByIDAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rel := derivedPCI(strangerMember, "1999-01-01", "Othername")
	type leg struct {
		build    func(t *testing.T, party bool) []byte
		provider func(g *Gateway, b []byte) (string, int, string)
		payer    func(g *Gateway, b []byte) (string, int, string)
		subject  func(b []byte) []byte
		// strict is each side's own patient-consistency refusal, keyed side or
		// side/variant, which strict answers before the binding refuses. A side
		// it does not name binds first, so strict answers as every level does.
		strict map[string]refusal
	}
	legs := map[string]leg{
		"pas-claim": {
			build: func(t *testing.T, party bool) []byte {
				d := decodeDoc(t, conformantPASBundleWithQR(t, strangerMember))
				entries := d["entry"].([]any)
				for _, e := range entries {
					if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "Coverage" {
						q1Coverage(r, "urn:uuid:rel", party)
					}
				}
				d["entry"] = append(entries, map[string]any{"fullUrl": "urn:uuid:rel", "resource": q1Patient("rel", "Othername", "1999-01-01")})
				return mustJSON(t, d)
			},
			provider: func(g *Gateway, b []byte) (string, int, string) { return g.ingressPASNativeSubjectPCIContext(ctx, b) },
			payer: func(g *Gateway, b []byte) (string, int, string) {
				_, pci, status, msg := g.conformantPASBindContext(ctx, b)
				return pci, status, msg
			},
			subject: pasSubjectResource,
			strict: map[string]refusal{
				"provider": {http.StatusForbidden, "inconsistent patient in PAS bundle"},
				"payer":    {http.StatusForbidden, "inconsistent patient in PAS bundle"},
			},
		},
		"crd-order-select": {
			build: func(t *testing.T, party bool) []byte {
				d := decodeDoc(t, crdReqJSON(strangerMember, "Patient/"+strangerMember, "Patient/rel"))
				pf := d["prefetch"].(map[string]any)
				q1Coverage(pf["coverage"].(map[string]any), "Patient/rel", party)
				pf["relative"] = q1Patient("rel", "Othername", "1999-01-01")
				return mustJSON(t, d)
			},
			provider: func(g *Gateway, b []byte) (string, int, string) { return g.ingressCRDSubjectPCIContext(ctx, b) },
			payer: func(g *Gateway, b []byte) (string, int, string) {
				_, _, pci, status, msg := g.conformantCRDBindContext(ctx, b)
				return pci, status, msg
			},
			subject: func(b []byte) []byte { return crdSubjectResource(b, strangerMember) },
			strict: map[string]refusal{
				// The requester's CRD ingress binds before its consistency check.
				"provider/no party": {http.StatusForbidden, "inconsistent patient reference in ingress payload"},
				"payer":             {http.StatusBadRequest, "inconsistent patient in order-select"},
			},
		},
	}
	legs["pas-claim-inquire"] = leg{
		build: func(t *testing.T, party bool) []byte {
			d := decodeDoc(t, inquiryBundle(strangerMember, "", "TRN-1", "72148"))
			for _, e := range d["entry"].([]any) {
				if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "Coverage" {
					q1Coverage(r, "urn:uuid:rel", party)
				}
			}
			d["entry"] = append(d["entry"].([]any), map[string]any{"fullUrl": "urn:uuid:rel", "resource": q1Patient("rel", "Othername", "1999-01-01")})
			return mustJSON(t, d)
		},
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
	}
	legs["coverage-eligibility"] = leg{
		build: func(t *testing.T, party bool) []byte {
			e := decodeDoc(t, eligibilityRequest(t, strangerMember))
			e["patient"] = map[string]any{"reference": "Patient/" + strangerMember}
			c := map[string]any{"resourceType": "Coverage", "id": "cov1", "status": "active"}
			q1Coverage(c, "Patient/rel", party)
			e["insurance"] = []any{map[string]any{"coverage": map[string]any{"reference": "Coverage/cov1"}}}
			return mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
				map[string]any{"fullUrl": "urn:uuid:req", "resource": e},
				map[string]any{"fullUrl": "http://ehr.example/fhir/Patient/" + strangerMember, "resource": map[string]any{"resourceType": "Patient", "id": strangerMember}},
				map[string]any{"fullUrl": "http://ehr.example/fhir/Patient/rel", "resource": q1Patient("rel", "Othername", "1999-01-01")},
				map[string]any{"fullUrl": "http://ehr.example/fhir/Coverage/cov1", "resource": c},
			}})
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
	}
	for name, l := range legs {
		for _, party := range []bool{false, true} {
			b := l.build(t, party)
			for _, level := range allLevels {
				variant := "no party"
				if party {
					variant = "a party under the member's number"
				}
				t.Run(name+"/"+variant+"/"+level.String(), func(t *testing.T) {
					g := &Gateway{cfg: Config{SoR: newCensusSoR(), ConformanceEnforcement: level}}
					// The binding's own answer, whatever a check before it answers.
					pci, found, err := g.resolveSubjectPCIFrom(ctx, strangerMember, b, l.subject(b))
					switch {
					case party && (!errors.Is(err, errSubjectUnidentifiable) || pci != "" || found):
						t.Fatalf("binding: (%q, %v, %v), want %v", pci, found, err, errSubjectUnidentifiable)
					case !party && (err != nil || pci != strangerPCI()):
						t.Fatalf("binding: (%q, %v, %v), want the member id alone %q", pci, found, err, strangerPCI())
					}
					for side, bind := range map[string]func(*Gateway, []byte) (string, int, string){"provider": l.provider, "payer": l.payer} {
						pci, status, msg := bind(g, b)
						want, first := l.strict[side]
						if w, ok := l.strict[side+"/"+variant]; ok {
							want, first = w, true
						}
						if pci == rel {
							t.Fatalf("%s: bound from the other Patient's demographics", side)
						}
						switch {
						case level == EnforcementStrict && first:
							if status != want.status || msg != want.msg {
								t.Fatalf("%s at strict: (%q, %d, %q), want its own %d %q", side, pci, status, msg, want.status, want.msg)
							}
						case party:
							if status != http.StatusBadRequest || msg != msgSubjectUnidentifiable {
								t.Fatalf("%s: (%q, %d, %q), want 400 %q", side, pci, status, msg, msgSubjectUnidentifiable)
							}
						default:
							if status != 0 || pci != strangerPCI() {
								t.Fatalf("%s: (%q, %d, %q), want bound by the member id alone %q", side, pci, status, msg, strangerPCI())
							}
						}
					}
				})
			}
		}
	}
}

// refusal is a status and its text.
type refusal struct {
	status int
	msg    string
}
