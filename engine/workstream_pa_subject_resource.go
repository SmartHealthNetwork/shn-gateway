//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
)

// The subject resource of a request on a PA or federated-query leg is the one Patient
// the leg's subject reference resolves to inside the request: the Patient whose
// birth date and family name alone decide the binding of a member the participant's
// system of record does not hold (resolveSubjectPCIFrom). Each reader below returns its
// leg's subject resource as JSON, or nil when the request carries none for the
// reference, or several that differ. A nil subject leaves the binding to the rule for a
// request with no subject resource.

// oneSubjectPatient is the Patient candidates hold when they are all one resource: each
// is marshalled canonically (sorted members), and a candidate that differs from another
// makes the reference resolve to none.
func oneSubjectPatient(candidates []map[string]any) []byte {
	var one []byte
	for _, c := range candidates {
		raw, err := json.Marshal(c)
		if err != nil {
			return nil
		}
		if one != nil && !bytes.Equal(one, raw) {
			return nil
		}
		one = raw
	}
	return one
}

// crdSubjectResource is a CDS Hooks order request's subject resource for the member its
// context.patientId (or an order's or Coverage's reference) names: the Patient with that
// id among its prefetch values, each a resource or a searchset Bundle of them.
func crdSubjectResource(body []byte, member string) []byte {
	var req struct {
		Prefetch map[string]json.RawMessage `json:"prefetch"`
	}
	if member == "" || json.Unmarshal(body, &req) != nil {
		return nil
	}
	var found []map[string]any
	take := func(r map[string]any) {
		if r["resourceType"] == "Patient" {
			if id, _ := r["id"].(string); id == member {
				found = append(found, r)
			}
		}
	}
	for _, raw := range req.Prefetch {
		var r map[string]any
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		take(r)
		if r["resourceType"] == "Bundle" {
			entries, _ := r["entry"].([]any)
			for _, e := range entries {
				em, _ := e.(map[string]any)
				if res, _ := em["resource"].(map[string]any); res != nil {
					take(res)
				}
			}
		}
	}
	return oneSubjectPatient(found)
}

// dtrSubjectResource is a DTR request's subject resource for the member its subject
// reference names (the package's first beneficiary that reads, else the first patient
// named; a $next-question round's QuestionnaireResponse.subject): the Patient with that id
// the request carries, in a parameter, a part, a Bundle's entry or a nested Parameters, at
// any depth. A contained Patient is not one: its id is local to the resource containing it.
func dtrSubjectResource(body []byte, member string) []byte {
	var doc any
	if member == "" || json.Unmarshal(body, &doc) != nil {
		return nil
	}
	var found []map[string]any
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if v["resourceType"] == "Patient" {
				if id, _ := v["id"].(string); id == member {
					found = append(found, v)
				}
			}
			for k, child := range v {
				if k != "contained" {
					walk(child)
				}
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	return oneSubjectPatient(found)
}

// eligibilitySubjectResource is a coverage-eligibility request's subject resource: the
// Patient its CoverageEligibilityRequest.patient names, a "#<id>" among the request's
// contained resources, or, in a Bundle, the one entry that resolveEntry resolves it to.
func eligibilitySubjectResource(sub eligibilitySubmission) []byte {
	var req struct {
		Patient struct {
			Reference string `json:"reference"`
		} `json:"patient"`
	}
	if json.Unmarshal(sub.request, &req) != nil || req.Patient.Reference == "" {
		return nil
	}
	ref := req.Patient.Reference
	if ref[0] == '#' {
		contained, err := eligibilityContainedFor(sub.request, ref)
		if err != nil {
			return nil
		}
		var found []map[string]any
		for _, c := range containedObjects(contained) {
			if id, _ := c["id"].(string); "#"+id == ref {
				found = append(found, c)
			}
		}
		if len(found) != 1 || found[0]["resourceType"] != "Patient" {
			return nil
		}
		return oneSubjectPatient(found)
	}
	if !sub.bundled {
		return nil
	}
	patient, how := resolveEntry(sub.entries, ref, "Patient")
	if how != refResolved {
		return nil
	}
	return patient
}

// eligibilityIngressSubject binds one coverage-eligibility request's member on the
// requester's gateway, from its own scope and subject resource (eligibilitySubjectResource).
func (g *Gateway) eligibilityIngressSubject(ctx context.Context, member string, sub eligibilitySubmission) (pci string, found bool, readErr error) {
	return g.resolveSubjectPCIFrom(ctx, member, sub.scope, eligibilitySubjectResource(sub))
}

// eligibilityInboundSubject is eligibilityIngressSubject on the payer's gateway.
func (g *Gateway) eligibilityInboundSubject(ctx context.Context, member string, sub eligibilitySubmission) (pci string, status int, msg string) {
	return g.bindInboundSubjectFrom(ctx, member, sub.scope, eligibilitySubjectResource(sub))
}

// legSubjectResource is the subject resource of a request on leg (resolveSubjectPCIFrom)
// when it is member's own Patient, as the leg's binding reads it; nil otherwise, and on a
// leg that binds with none (the CDex legs) or names no involved patients.
func legSubjectResource(leg string, payload []byte, member string) []byte {
	var r []byte
	switch leg {
	case "crd-order-select", "crd-order-dispatch":
		r = crdSubjectResource(payload, member)
	case "dtr-questionnaire-fetch":
		r = dtrSubjectResource(payload, member)
	case "pas-claim", "pas-claim-update", "pas-claim-inquire":
		r = pasSubjectResource(payload)
	}
	return memberSubjectResource(r, member)
}

// memberSubjectResource is subject when it is member's own Patient (patientIsMember: its
// id, or its member identifier), nil otherwise.
func memberSubjectResource(subject []byte, member string) []byte {
	var p map[string]any
	if subject == nil || json.Unmarshal(subject, &p) != nil || !patientIsMember(p, member) {
		return nil
	}
	return subject
}
