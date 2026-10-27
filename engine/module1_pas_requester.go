//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The clinician an order names travels with every prior-authorization request
// this gateway builds about that order.
//
// An order's requester is a reference into the participant's own system. A
// PAS request that carries the order but not the record its requester names
// hands the payer a reference nothing in the request resolves, and a Da Vinci
// payer walks the request graph from the Claim and refuses it. So the records
// are read from the participant's own system of record, once, before any leg,
// and carried, their content unchanged, as Bundle entries. A record the system cannot supply is
// refused by name, never minted: the fidelity of SHN's own build.

// requesterCarryTypes are the records carried for the clinician an order
// names: the requester and the records it leads to.
var requesterCarryTypes = map[string]bool{
	"Practitioner": true, "PractitionerRole": true, "Organization": true, "Location": true,
}

const (
	// requesterCarryDepth is how many references away from the requester a
	// carried record may be: a Practitioner's qualification issuer, or a
	// PractitionerRole's practitioner and that practitioner's issuer.
	requesterCarryDepth = 2
	// requesterCarryMax bounds the records one order's requester leads to.
	requesterCarryMax = 16
)

// requesterCarryTypeList names requesterCarryTypes in refusals.
const requesterCarryTypeList = "Practitioner, PractitionerRole, Organization and Location"

// requesterRefusal is a refusal of the requester read. fidelity marks the
// fidelity of SHN's own build (422, before any leg); otherwise the system of
// record failed and status and msg are its own.
type requesterRefusal struct {
	status   int
	msg      string
	fidelity bool
}

// orderRequesterRecords reads the records the order's requester leads to
// from the participant's own system of record: the requester, then each
// record of requesterCarryTypes it references, to requesterCarryDepth, each
// once. They come back in read order, requester first; an order whose
// requester names no record (none stated, or a contained one) yields none.
func (g *Gateway) orderRequesterRecords(ctx context.Context, order []byte) ([][]byte, *requesterRefusal) {
	var o struct {
		Requester struct {
			Reference string `json:"reference"`
		} `json:"requester"`
	}
	if decodeMessage(order, &o) != nil {
		return nil, nil
	}
	root := o.Requester.Reference
	if root == "" || strings.HasPrefix(root, "#") {
		return nil, nil
	}
	refuse := func(msg string) ([][]byte, *requesterRefusal) {
		return nil, &requesterRefusal{status: http.StatusUnprocessableEntity, msg: msg, fidelity: true}
	}
	type pending struct {
		ref   string
		depth int
	}
	var records [][]byte
	queue := []pending{{root, 0}}
	seen := map[string]bool{root: true}
	sor := ReadSystemOfRecord(g.cfg.SoR)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		typ, id, ok := requesterCarryRef(next.ref)
		switch {
		case !ok && next.depth == 0:
			return refuse(fmt.Sprintf("the order's requester %s is not a Type/id reference into your system of record, so a prior-authorization request about this order cannot carry the clinician it names", next.ref))
		case !ok:
			return refuse(fmt.Sprintf("the order's requester %s leads to %s, which is not a Type/id reference into your system of record, so a prior-authorization request about this order cannot carry it", root, next.ref))
		case !requesterCarryTypes[typ] && next.depth == 0:
			return refuse(fmt.Sprintf("the order's requester %s is a %s; a prior-authorization request this gateway builds carries only %s records for the clinician an order names", next.ref, typ, requesterCarryTypeList))
		case !requesterCarryTypes[typ]:
			return refuse(fmt.Sprintf("the order's requester %s leads to %s, a %s; a prior-authorization request this gateway builds carries only %s records for the clinician an order names", root, next.ref, typ, requesterCarryTypeList))
		case next.depth > requesterCarryDepth:
			return refuse(fmt.Sprintf("the order's requester %s leads to %s, more than %d references away from the requester; a prior-authorization request this gateway builds carries the requester's records to that depth", root, next.ref, requesterCarryDepth))
		case len(records) == requesterCarryMax:
			return refuse(fmt.Sprintf("the order's requester %s leads to more than %d records; a prior-authorization request this gateway builds carries at most %d for the clinician an order names", root, requesterCarryMax, requesterCarryMax))
		}
		raw, found, err := sor.ResolveByReferenceContext(ctx, next.ref)
		if err != nil {
			status, msg := SoRFailureResponse(err)
			return nil, &requesterRefusal{status: status, msg: msg}
		}
		if !found && next.depth == 0 {
			return refuse(fmt.Sprintf("the order's requester %s is not in your system of record, so a prior-authorization request about this order cannot carry the clinician it names", next.ref))
		}
		if !found {
			return refuse(fmt.Sprintf("the order's requester %s leads to %s, which is not in your system of record, so a prior-authorization request about this order cannot carry it", root, next.ref))
		}
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		if decodeMessage(raw, &head) != nil || head.ResourceType != typ || head.ID != id {
			status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
			return nil, &requesterRefusal{status: status, msg: msg}
		}
		records = append(records, raw)
		var tree map[string]any
		if decodePASObject(raw, &tree) != nil {
			status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
			return nil, &requesterRefusal{status: status, msg: msg}
		}
		for _, ref := range resourceReferences(tree) {
			if strings.HasPrefix(ref, "#") || seen[ref] {
				continue
			}
			seen[ref] = true
			queue = append(queue, pending{ref, next.depth + 1})
		}
	}
	return records, nil
}

// requesterCarryRef splits a relative "Type/id" reference.
func requesterCarryRef(ref string) (typ, id string, ok bool) {
	typ, id, ok = strings.Cut(ref, "/")
	if !ok || typ == "" || strings.ContainsAny(typ, ":/") || !fhirIDRE.MatchString(id) {
		return "", "", false
	}
	return typ, id, true
}

// resourceReferences lists every reference string in a decoded resource, in
// a stable order (object members sorted, arrays in order).
func resourceReferences(v any) []string {
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if s, ok := x["reference"].(string); ok && s != "" {
				out = append(out, s)
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(x[k])
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	return out
}

// carryOrderRequester carries the records the order's requester leads to
// (orderRequesterRecords) into a built PAS submit or update Bundle, each as an
// entry on the order entry's base. The order must name the first record as its
// requester. A record the Bundle already holds (a PractitionerRole or
// Organization the request names as its provider) is carried once, and must
// be the same record. With absolute references (the reference payer's
// lane), the references these entries make resolvable are written as the
// entries' fullUrls, as the builder writes every other reference that names
// an entry.
func carryOrderRequester(body []byte, records [][]byte, absolute bool) ([]byte, error) {
	if len(records) == 0 {
		return body, nil
	}
	var bundle map[string]json.RawMessage
	if err := json.Unmarshal(body, &bundle); err != nil {
		return nil, err
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(bundle["entry"], &entries); err != nil {
		return nil, err
	}
	type head struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		Requester    struct {
			Reference string `json:"reference"`
		} `json:"requester"`
	}
	headOf := func(raw []byte) (head, error) {
		var h head
		if err := json.Unmarshal(raw, &h); err != nil {
			return h, err
		}
		if h.ResourceType == "" || h.ID == "" {
			return h, fmt.Errorf("resource has no type or id")
		}
		return h, nil
	}
	orderAt, base := -1, ""
	var order head
	byKey := map[string]int{}
	for i, e := range entries {
		h, err := headOf(e["resource"])
		if err != nil {
			return nil, err
		}
		byKey[h.ResourceType+"/"+h.ID] = i
		if h.ResourceType != "ServiceRequest" && h.ResourceType != "DeviceRequest" {
			continue
		}
		if orderAt >= 0 {
			return nil, fmt.Errorf("more than one order in the request")
		}
		var full string
		if err := json.Unmarshal(e["fullUrl"], &full); err != nil {
			return nil, err
		}
		suffix := "/" + h.ResourceType + "/" + h.ID
		if !strings.HasSuffix(full, suffix) {
			return nil, fmt.Errorf("order entry fullUrl does not name the order")
		}
		orderAt, base, order = i, strings.TrimSuffix(full, suffix), h
	}
	if orderAt < 0 {
		return nil, fmt.Errorf("no order in the request")
	}
	first, err := headOf(records[0])
	if err != nil {
		return nil, err
	}
	rootKey := first.ResourceType + "/" + first.ID
	if ref := order.Requester.Reference; ref != rootKey && ref != base+"/"+rootKey {
		return nil, fmt.Errorf("the order's requester %q does not name the carried record %s", ref, rootKey)
	}
	carried := map[int]bool{}
	for _, rec := range records {
		h, err := headOf(rec)
		if err != nil {
			return nil, err
		}
		key := h.ResourceType + "/" + h.ID
		full := base + "/" + key
		if at, ok := byKey[key]; ok {
			var existing string
			if err := json.Unmarshal(entries[at]["fullUrl"], &existing); err != nil {
				return nil, err
			}
			if existing != full || !sameJSONResource(entries[at]["resource"], rec) {
				return nil, fmt.Errorf("conflicting entry for %s", key)
			}
			carried[at] = true
			continue
		}
		encodedURL, _ := json.Marshal(full)
		entries = append(entries, map[string]json.RawMessage{"fullUrl": encodedURL, "resource": append(json.RawMessage(nil), rec...)})
		byKey[key] = len(entries) - 1
		carried[len(entries)-1] = true
	}
	if absolute {
		rewrite := func(ref string) (string, bool) {
			if _, ok := byKey[ref]; ok && !strings.HasPrefix(ref, "#") {
				return base + "/" + ref, true
			}
			return ref, false
		}
		// The order: only its requester, the reference this change resolves.
		var o map[string]any
		if err := decodePASObject(entries[orderAt]["resource"], &o); err != nil {
			return nil, err
		}
		if req, ok := o["requester"].(map[string]any); ok {
			if s, ok := req["reference"].(string); ok {
				if abs, changed := rewrite(s); changed {
					req["reference"] = abs
					if entries[orderAt]["resource"], err = json.Marshal(o); err != nil {
						return nil, err
					}
				}
			}
		}
		// The carried records: every reference that names an entry.
		for at := range carried {
			var r map[string]any
			if err := decodePASObject(entries[at]["resource"], &r); err != nil {
				return nil, err
			}
			if rewriteReferences(r, rewrite) {
				if entries[at]["resource"], err = json.Marshal(r); err != nil {
					return nil, err
				}
			}
		}
	}
	if bundle["entry"], err = json.Marshal(entries); err != nil {
		return nil, err
	}
	return json.Marshal(bundle)
}

// rewriteReferences applies rewrite to every reference string in a decoded
// resource and reports whether any changed.
func rewriteReferences(v any, rewrite func(string) (string, bool)) bool {
	changed := false
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if s, ok := c.(string); ok && k == "reference" {
				if out, ok := rewrite(s); ok {
					x[k], changed = out, true
				}
				continue
			}
			if rewriteReferences(c, rewrite) {
				changed = true
			}
		}
	case []any:
		for _, c := range x {
			if rewriteReferences(c, rewrite) {
				changed = true
			}
		}
	}
	return changed
}

// buildModuleOnePASSubmit is module 1's PAS submit: the authored submit
// (buildAuthoredPASSubmit), carrying the records the order's requester leads
// to (carryOrderRequester) before the authored attachment is completed, so a
// questionnaire response naming the clinician resolves to the same entry.
func buildModuleOnePASSubmit(line string, in shnsdk.ConformantClaimInputs, requester [][]byte) ([]byte, error) {
	return buildAuthoredPASSubmit(line, in, func(body []byte) ([]byte, error) {
		return carryOrderRequester(body, requester, in.AbsoluteRefs)
	})
}

// buildModuleOnePASUpdate is module 1's PAS update, carrying the same records
// as the submission it amends.
func buildModuleOnePASUpdate(line string, in shnsdk.ConformantClaimUpdateInputs, requester [][]byte) ([]byte, error) {
	return buildAuthoredPASUpdate(line, in, func(body []byte) ([]byte, error) {
		return carryOrderRequester(body, requester, in.AbsoluteRefs)
	})
}

// relativeReferences is resource with every reference under base written
// relative ("Type/id"): the inverse of the absolute form carryOrderRequester
// and the builder write. A resource it cannot read comes back unchanged.
func relativeReferences(resource []byte, base string) []byte {
	var r map[string]any
	if decodePASObject(resource, &r) != nil {
		return resource
	}
	rewriteReferences(r, func(ref string) (string, bool) {
		rel, ok := strings.CutPrefix(ref, base+"/")
		return rel, ok
	})
	out, err := json.Marshal(r)
	if err != nil {
		return resource
	}
	return out
}
