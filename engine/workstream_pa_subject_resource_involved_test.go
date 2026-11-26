//go:build !shn_coreonly

package engine

import (
	"context"
	"fmt"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The involved list is unchanged by the subject resource deciding the leg's binding: each
// named member is identified as the leg's binding identifies it (namedMember's
// subjectResource), so the leg's own patient is never named or counted, and another
// Patient carried for the member is counted unknown-member as before, never named.
func TestSubjectResource_TheInvolvedListIsUnchanged(t *testing.T) {
	ctx := context.Background()
	rel := func() map[string]any {
		return map[string]any{"resourceType": "Patient", "id": "rel",
			"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
			"name":       []any{map[string]any{"family": "Othername"}}, "birthDate": "1999-01-01"}
	}
	child := func(id string, ident, demo bool) map[string]any {
		p := map[string]any{"resourceType": "Patient", "id": id}
		if demo {
			p["name"], p["birthDate"] = []any{map[string]any{"family": "Childname"}}, "2015-06-01"
		}
		if ident {
			p["identifier"] = []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
		}
		return p
	}
	nm := func(pci string) string {
		switch pci {
		case pinnedChildIdentity:
			return "SUBJECT"
		case derivedPCI(strangerMember, "", ""):
			return "MEMBER-ALONE"
		case derivedPCI(strangerMember, "1999-01-01", "Othername"):
			return "REL"
		}
		return pci
	}
	for _, demo := range []bool{true, false} {
		for _, withRel := range []bool{false, true} {
			var omitted []string
			g := &Gateway{cfg: Config{SoR: newCensusSoR(), ConformanceEnforcement: EnforcementNone, HubAcceptsInvolved: true, InvolvedMetric: func(r string) { omitted = append(omitted, r) }}}
			run := func(leg string, b []byte, legSubject string, _ error) {
				omitted = nil
				got := g.requestNamedPatients(ctx, leg, "corr-1", b, legSubject)
				var names []string
				for _, p := range got {
					names = append(names, nm(p.pci)+"/"+string(p.involvement))
				}
				// What earlier releases recorded for the same request: no one named,
				// and on CRD, DTR, PAS and the inquiry the extra Patient under the
				// subject's names counted twice (its id and its member identifier).
				want := 0
				if withRel && leg != "coverage-eligibility" {
					want = 2
				}
				unknown := 0
				for _, r := range omitted {
					if r == involvedOmitUnknown {
						unknown++
					}
				}
				if len(names) != 0 || unknown != want || len(omitted) != want {
					t.Errorf("demo=%v rel=%v %s (bound %s): named %v, omitted %v; want no one named and %d unknown-member", demo, withRel, leg, nm(legSubject), names, omitted, want)
				}
			}
			// eligibility
			e := decodeDoc(t, eligibilityRequest(t, strangerMember))
			e["patient"] = map[string]any{"reference": "#p"}
			e["contained"] = []any{child("p", true, demo)}
			if withRel {
				e["contained"] = append(e["contained"].([]any), rel())
			}
			eb := mustJSON(t, e)
			subs, err := eligibilitySubmissions(eb)
			if err != nil {
				t.Fatal(err)
			}
			pci, _, err := eligibilityIngressSubjectProbe(g, ctx, subs[0])
			run("coverage-eligibility", eb, pci, err)
			// CRD
			c := decodeDoc(t, crdReqJSON(strangerMember, "Patient/"+strangerMember, "Patient/"+strangerMember))
			c["prefetch"].(map[string]any)["patient"] = child(strangerMember, false, demo)
			if withRel {
				c["prefetch"].(map[string]any)["relative"] = rel()
			}
			cb := mustJSON(t, c)
			cp, s, m := g.ingressCRDSubjectPCIContext(ctx, cb)
			run("crd-order-select", cb, cp, fmt.Errorf("%d %s", s, m))
			// DTR package
			dd := decodeDoc(t, dtrPackageFor(strangerMember))
			dd["parameter"] = append(dd["parameter"].([]any), map[string]any{"name": "patient", "resource": child(strangerMember, false, demo)})
			if withRel {
				dd["parameter"] = append(dd["parameter"].([]any), map[string]any{"name": "relative", "resource": rel()})
			}
			db := mustJSON(t, dd)
			out, s2, m2 := g.prepareDTRPackageRequest(ctx, db)
			run("dtr-questionnaire-fetch", db, out.pci, fmt.Errorf("%d %s", s2, m2))
			// PAS
			var pd map[string]any
			if demo {
				pd = decodeDoc(t, withMembersOwnPatient(t, conformantPASBundleWithQR(t, strangerMember), nil))
			} else {
				pd = decodeDoc(t, conformantPASBundleWithQR(t, strangerMember))
			}
			if withRel {
				pd["entry"] = append(pd["entry"].([]any), map[string]any{"fullUrl": "urn:uuid:relative", "resource": rel()})
			}
			pb := mustJSON(t, pd)
			pp, s3, m3 := g.ingressPASNativeSubjectPCIContext(ctx, pb)
			run("pas-claim", pb, pp, fmt.Errorf("%d %s", s3, m3))
			// inquiry
			iq := decodeDoc(t, inquiryBundle(strangerMember, "", "TRN-1", "72148"))
			if demo {
				iq = decodeDoc(t, withMembersOwnPatient(t, inquiryBundle(strangerMember, "", "TRN-1", "72148"), nil))
			}
			if withRel {
				iq["entry"] = append(iq["entry"].([]any), map[string]any{"fullUrl": "urn:uuid:relative", "resource": rel()})
			}
			ib := mustJSON(t, iq)
			ip, _, ierr := g.inquiryIngressSubject(ctx, strangerMember, ib)
			run("pas-claim-inquire", ib, ip, ierr)
		}
	}
}

// eligibilityIngressSubjectProbe binds an eligibility request's member as the leg's ingress does.
func eligibilityIngressSubjectProbe(g *Gateway, ctx context.Context, sub eligibilitySubmission) (string, bool, error) {
	m, err := eligibilitySubmissionMember(sub)
	if err != nil {
		return "", false, err
	}
	return g.eligibilityIngressSubject(ctx, m, sub)
}

// In a coverage-eligibility batch, another request's member is named on the involved list
// only under an identity derived from its own subject Patient's birth date and family
// name, never from its member id alone: a request whose subject Patient carries neither
// fact, beside another Patient for its member with both, is counted unknown-member, where
// earlier releases named it under that other Patient's identity.
func TestSubjectResource_ABatchsOtherRequestIsNeverNamedByItsIDAlone(t *testing.T) {
	ctx := context.Background()
	rel := map[string]any{"resourceType": "Patient", "id": "rel",
		"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
		"name":       []any{map[string]any{"family": "Othername"}}, "birthDate": "1999-01-01"}
	child := func(id, member string, demo bool) map[string]any {
		p := map[string]any{"resourceType": "Patient", "id": id,
			"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": member}}}
		if demo {
			p["name"], p["birthDate"] = []any{map[string]any{"family": "Childname"}}, "2015-06-01"
		}
		return p
	}
	nm := func(pci string) string {
		switch pci {
		case pinnedChildIdentity:
			return "SUBJECT2"
		case derivedPCI(strangerMember, "", ""):
			return "MEMBER2-ALONE"
		case derivedPCI(strangerMember, "1999-01-01", "Othername"):
			return "REL"
		case derivedPCI("eFIRST", "2015-06-01", "Childname"):
			return "FIRST"
		}
		return pci
	}
	for _, demo := range []bool{true, false} {
		for _, withRel := range []bool{false, true} {
			var omitted []string
			g := &Gateway{cfg: Config{SoR: newCensusSoR(), ConformanceEnforcement: EnforcementNone, HubAcceptsInvolved: true, InvolvedMetric: func(r string) { omitted = append(omitted, r) }}}
			e1 := decodeDoc(t, eligibilityRequest(t, "eFIRST"))
			e1["patient"] = map[string]any{"reference": "#p"}
			e1["contained"] = []any{child("p", "eFIRST", true)}
			e2 := decodeDoc(t, eligibilityRequest(t, strangerMember))
			e2["patient"] = map[string]any{"reference": "#p"}
			e2["contained"] = []any{child("p", strangerMember, demo)}
			if withRel {
				e2["contained"] = append(e2["contained"].([]any), rel)
			}
			b := mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
				map[string]any{"fullUrl": "urn:uuid:r1", "resource": e1},
				map[string]any{"fullUrl": "urn:uuid:r2", "resource": e2},
			}})
			subs, err := eligibilitySubmissions(b)
			if err != nil || len(subs) != 2 {
				t.Fatalf("subs %d %v", len(subs), err)
			}
			p1, _, err1 := eligibilityIngressSubjectProbe(g, ctx, subs[0])
			p2, _, err2 := eligibilityIngressSubjectProbe(g, ctx, subs[1])
			omitted = nil
			got := g.requestNamedPatients(ctx, "coverage-eligibility", "corr-1", b, p1)
			names := []string{}
			for _, p := range got {
				names = append(names, nm(p.pci)+"/"+string(p.involvement))
			}
			if err1 != nil || err2 != nil {
				t.Fatalf("binding: %v %v", err1, err2)
			}
			// The other request is named only under an identity derived from its own
			// subject Patient's demographics, never from its member id alone, and
			// otherwise counted.
			want, wantOmitted := []string{}, []string{involvedOmitUnknown}
			if demo && !withRel {
				want, wantOmitted = []string{"SUBJECT2/request-named"}, nil
			}
			if fmt.Sprint(names) != fmt.Sprint(want) || fmt.Sprint(omitted) != fmt.Sprint(wantOmitted) {
				t.Errorf("demo=%v rel=%v (request 2 bound %s): named %v, omitted %v; want %v, %v", demo, withRel, nm(p2), names, omitted, want, wantOmitted)
			}
		}
	}
}
