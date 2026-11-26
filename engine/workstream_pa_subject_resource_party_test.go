//go:build !shn_coreonly

package engine

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A subject Patient with a birth date and family name identifies the request's patient
// even where the request also carries another person under the member's number (a
// Coverage's subscriber): v1, that person contained under the member's own id; v2, under
// the member's number, beside a same-id Patient contained in another resource. The
// request is bound from the subject Patient at every level, on the requester's gateway
// and the payer's; earlier releases refused these "cannot identify the patient", reading
// the member's name, not the subject reference. With the subject Patient carrying neither
// fact, the same requests are still refused "cannot identify the patient" (the control).
// Where strict refuses first for the leg's own patient-consistency check, the row pins
// that refusal (partyStrictFirst).
func TestSubjectResource_ASubjectWithDemographicsBesideAParty(t *testing.T) {
	t.Parallel()
	for variant := 1; variant < 3; variant++ {
		for _, demo := range []bool{true, false} {
			ctx := context.Background()
			rel := func() map[string]any {
				if variant == 2 {
					return map[string]any{"resourceType": "Basic", "id": "holder", "code": map[string]any{"text": "x"}, "contained": []any{
						map[string]any{"resourceType": "Patient", "id": strangerMember, "name": []any{map[string]any{"family": "Othername"}}, "birthDate": "1999-01-01"}}}
				}
				return map[string]any{"resourceType": "Patient", "id": "rel",
					"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
					"name":       []any{map[string]any{"family": "Othername"}}, "birthDate": "1999-01-01"}
			}
			parent := func() map[string]any {
				if variant == 1 {
					return map[string]any{"resourceType": "Patient", "id": strangerMember,
						"name": []any{map[string]any{"family": "Parentname"}}, "birthDate": "1970-01-01"}
				}
				return map[string]any{"resourceType": "Patient", "id": "parent",
					"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
					"name":       []any{map[string]any{"family": "Parentname"}}, "birthDate": "1970-01-01"}
			}
			withParty := func(cov map[string]any) {
				if variant == 1 {
					cov["subscriber"] = map[string]any{"reference": "#" + strangerMember}
				} else {
					cov["subscriber"] = map[string]any{"reference": "#parent"}
				}
				cov["contained"] = []any{parent()}
			}
			check := func(leg, side string, withRel bool, level ConformanceEnforcement, pci string, status int, msg string) {
				t.Helper()
				key := fmt.Sprintf("v%d %s rel=%v %s %s", variant, leg, withRel, level, side)
				if want, ok := partyStrictFirst[fmt.Sprintf("v%d %s rel=%v demo=%v %s", variant, leg, withRel, demo, side)]; ok && level == EnforcementStrict {
					if status != want.status || msg != want.msg {
						t.Errorf("%s demo=%v: (%q, %d, %q), want its own %d %q", key, demo, pci, status, msg, want.status, want.msg)
					}
					return
				}
				if demo {
					if status != 0 || pci != pinnedChildIdentity {
						t.Errorf("%s: (%q, %d, %q), want bound from the subject Patient %q", key, pci, status, msg, pinnedChildIdentity)
					}
					return
				}
				if status != http.StatusBadRequest || msg != msgPatientUnidentifiable {
					t.Errorf("%s, a subject without demographics: (%q, %d, %q), want 400 %q", key, pci, status, msg, msgPatientUnidentifiable)
				}
			}
			levels := []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict}
			for _, withRel := range []bool{false, true} {
				// PAS
				d := decodeDoc(t, conformantPASBundleWithQR(t, strangerMember))
				if demo {
					d = decodeDoc(t, withMembersOwnPatient(t, conformantPASBundleWithQR(t, strangerMember), nil))
				}
				for _, e := range d["entry"].([]any) {
					r := e.(map[string]any)["resource"].(map[string]any)
					if r["resourceType"] == "Coverage" {
						withParty(r)
					}
				}
				if withRel {
					d["entry"] = append(d["entry"].([]any), map[string]any{"fullUrl": "urn:uuid:relative", "resource": rel()})
				}
				b := mustJSON(t, d)
				for _, level := range levels {
					g := &Gateway{cfg: Config{SoR: newCensusSoR(), ConformanceEnforcement: level}}
					ppci, ps, pm := g.ingressPASNativeSubjectPCIContext(ctx, b)
					check("pas", "provider", withRel, level, ppci, ps, pm)
					_, qpci, qs, qm := g.conformantPASBindContext(ctx, b)
					check("pas", "payer", withRel, level, qpci, qs, qm)
				}
				// CRD
				c := decodeDoc(t, crdReqJSON(strangerMember, "Patient/"+strangerMember, "Patient/"+strangerMember))
				pf := c["prefetch"].(map[string]any)
				pf["patient"] = map[string]any{"resourceType": "Patient", "id": strangerMember}
				if demo {
					pf["patient"] = map[string]any{"resourceType": "Patient", "id": strangerMember, "name": []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"}
				}
				withParty(pf["coverage"].(map[string]any))
				if withRel {
					pf["relative"] = rel()
				}
				cb := mustJSON(t, c)
				for _, level := range levels {
					g := &Gateway{cfg: Config{SoR: newCensusSoR(), ConformanceEnforcement: level}}
					ppci, ps, pm := g.ingressCRDSubjectPCIContext(ctx, cb)
					check("crd", "provider", withRel, level, ppci, ps, pm)
					_, _, qpci, qs, qm := g.conformantCRDBindContext(ctx, cb)
					check("crd", "payer", withRel, level, qpci, qs, qm)
				}
			}
		}
	}
}

// partyStrictFirst are the requests above that strict refuses for the leg's own
// patient-consistency check, keyed "v<variant> <leg> rel=<extra Patient> demo=<subject
// demographics> <side>".
var partyStrictFirst = map[string]refusal{
	"v1 crd rel=true demo=true provider":  {http.StatusForbidden, "inconsistent patient reference in ingress payload"},
	"v2 pas rel=true demo=true provider":  {http.StatusForbidden, "inconsistent patient in PAS bundle"},
	"v2 pas rel=true demo=true payer":     {http.StatusForbidden, "inconsistent patient in PAS bundle"},
	"v2 pas rel=true demo=false provider": {http.StatusForbidden, "inconsistent patient in PAS bundle"},
	"v2 pas rel=true demo=false payer":    {http.StatusForbidden, "inconsistent patient in PAS bundle"},
}
