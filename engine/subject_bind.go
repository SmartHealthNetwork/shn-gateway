package engine

import (
	"context"
	"encoding/json"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// resolveSubjectPCI binds a member id to its network patient identifier (pci) through this
// holder's OWN system of record. It is the one read the Da Vinci CRD, DTR and PAS legs make
// to bind a subject, on the ingress (origination) side and the inbound (payer) side alike.
// payload is the request the member arrived in, read only when the system of record does
// not hold the member (below); nil when the leg has none.
//
// By default a member the system of record does not hold is bound, not refused, from the
// same three facts a
// holder's record supplies — member id, birthDate and family name — read from the Patient
// the request itself carries for that member (PatientDemographics, the read this holder's
// FHIR system of record makes of its own Patient), or from the member id alone when the
// request carries no such Patient. Nothing is minted: every fact is one the partner sent.
// The payer side binds the member the same way, by its own system first
// (bindInboundSubject). A request carrying Patients for the member that disagree with
// each other binds by id alone. A participant that opts in to Config.RequireKnownMembers
// refuses such a member instead. Everything else on these legs is untouched: member-mixing
// refusals, prefetch read only from this system or the request, and the payer's own
// independent member resolution.
//
// A read failure is returned as is, with the flag set or not: an unreadable system of record
// is never mistaken for a member it does not hold.
func (g *Gateway) resolveSubjectPCI(ctx context.Context, member string, payload []byte) (pci string, found bool, readErr error) {
	pci, _, found, readErr = ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, member)
	if readErr != nil || found || g.cfg.RequireKnownMembers {
		return pci, found, readErr
	}
	demo, _ := carriedPatientDemographics(payload, member)
	return shnsdk.ResolvePCI(member, demo.BirthDate, demo.FamilyName), true, nil
}

// PatientDemographics reads the two demographics the patient identifier is derived from
// off a FHIR Patient: birthDate and the family name of the first name, both required and
// read verbatim. It is the one read of a Patient's demographics, for a holder's own record
// and for a Patient a request carries alike, so a subject derived from either is the same.
func PatientDemographics(patientJSON []byte) (Demo, bool) {
	var p struct {
		ResourceType string `json:"resourceType"`
		BirthDate    string `json:"birthDate"`
		Name         []struct {
			Family string `json:"family"`
		} `json:"name"`
	}
	if json.Unmarshal(patientJSON, &p) != nil || p.ResourceType != "Patient" || p.BirthDate == "" || len(p.Name) == 0 || p.Name[0].Family == "" {
		return Demo{}, false
	}
	return Demo{BirthDate: p.BirthDate, FamilyName: p.Name[0].Family}, true
}

// carriedPatientDemographics reads the demographics of the member's Patient wherever the
// request carries it — a prefetch value or a searchset entry, a bundle entry, a contained
// resource, an operation parameter. Zero and false when the request carries none with both
// demographics, or carries several that disagree.
func carriedPatientDemographics(payload []byte, member string) (Demo, bool) {
	var demo Demo
	var found, conflict bool
	forEachCarriedPatient(payload, member, func(patient map[string]any) {
		raw, err := json.Marshal(patient)
		if err != nil {
			return
		}
		d, ok := PatientDemographics(raw)
		if !ok {
			return
		}
		if found && d != demo {
			conflict = true
		}
		demo, found = d, true
	})
	if !found || conflict {
		return Demo{}, false
	}
	return demo, true
}

// carriesPatient reports whether the request carries a Patient resource for the member
// anywhere, with demographics or not.
func carriesPatient(payload []byte, member string) bool {
	carried := false
	forEachCarriedPatient(payload, member, func(map[string]any) { carried = true })
	return carried
}

// forEachCarriedPatient calls fn for every Patient resource the request carries for the
// member, at any depth. A Patient is the member's when its id or its member identifier is
// the member id. An empty or unreadable request carries none.
func forEachCarriedPatient(payload []byte, member string, fn func(patient map[string]any)) {
	var doc any
	if len(payload) == 0 || json.Unmarshal(payload, &doc) != nil {
		return
	}
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if v["resourceType"] == "Patient" && patientIsMember(v, member) {
				fn(v)
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
}

// patientIsMember reports whether a decoded Patient names the member by id or by member
// identifier.
func patientIsMember(patient map[string]any, member string) bool {
	if patient["id"] == member {
		return true
	}
	ids, _ := patient["identifier"].([]any)
	for _, id := range ids {
		if m, ok := id.(map[string]any); ok && m["system"] == shnsdk.MemberSystem && m["value"] == member {
			return true
		}
	}
	return false
}

// bindInboundSubject is the payer side's bind of the member its request names,
// returning the payer's own binding of that member: the PCI from its system of
// record when it holds the member, else the one derived from the member id and
// the Patient the request carries (resolveSubjectPCI). The payer handles the
// member as it would directly: it does not re-derive the leg's identity and
// refuse a difference. A request that names no member is refused, and a
// participant that requires known members (Config.RequireKnownMembers) refuses
// a member its system of record does not hold (400).
//
// Every record the payer keeps about the exchange (the pend ledger, a decision
// EOB, the correlation it claims, an inquiry's projection) is keyed by the PCI
// returned here, never by the leg token's subject. The requester chooses that
// subject, so keying a record by it would let one patient's authority file
// another patient's decision under the first patient's identity.
func (g *Gateway) bindInboundSubject(ctx context.Context, member string, payload []byte) (pci string, status int, msg string) {
	if member == "" {
		return "", http.StatusBadRequest, "unknown member"
	}
	pci, found, readErr := g.resolveSubjectPCI(ctx, member, payload)
	if readErr != nil {
		status, msg := SoRFailureResponse(readErr)
		return "", status, msg
	}
	if !found || pci == "" {
		return "", http.StatusBadRequest, "unknown member"
	}
	return pci, 0, ""
}

// SubjectBindingDiffersEvent is the observer event a payer leg raises when its
// own binding of the member the request names is not the patient the leg's
// token names. The request is handled as usual and filed under the payer's
// binding; the event tells the operator that the exchange is attributed, on the
// network, to a patient other than the one the payer answered about. It carries
// no identifier.
const SubjectBindingDiffersEvent = "subject.binding-differs"

// noteSubjectBinding raises SubjectBindingDiffersEvent when pci, the payer's
// binding, differs from the leg token's subject.
func (g *Gateway) noteSubjectBinding(leg, corrID, tokenSubject, pci string) {
	if pci != "" && pci != tokenSubject {
		g.observe(ObserverEvent{Kind: SubjectBindingDiffersEvent, Direction: "ingress", LegType: leg, CorrelationID: corrID})
	}
}
