// payoredge.go — the payer-edge identity mapping seam (owner-adjudicated design,
// 2026-08-26): a payer-role gateway translating its network tenant identity into its
// backend engine's identifier, on its own private backend leg (nativeResponder).
//
// Semantics (A1/A2, binding):
//   - A1 — ownership assertion, not a byte-rewriter. Re-stamp ONLY when the inbound
//     Coverage's payor identifier equals the gateway's OWN configured payer identity
//     (payorEdgeOwn). A different identifier, or no resolvable payor identifier at all,
//     refuses loudly — a 400-class LegResult naming the mismatch, never a bare error
//     (which the engine would map to an opaque "hub routing failed" 500).
//   - A2 — exactly one field, exactly one leg-side. Only the Coverage payor identity
//     (the payor-referenced Organization's identifier system|value) is touched, only on
//     this responder's own backend leg. Everything else in the payload — including the
//     payer Organization's `name` — is left verbatim (the 2026-08-26 live probe proves
//     identifier-only re-stamping is sufficient against the real reference payer).
//
// Coverage carriage spans all three legs this seam covers: crd-order-select
// (prefetch.coverage, a bare Coverage), dtr-questionnaire-fetch (fetch.Coverage, the
// same bare Coverage shape), and pas-claim/pas-claim-update (the $submit Bundle's
// Coverage.payor + Claim.insurer, which reference the SAME payer Organization in the
// demo/kit conformant shape — PayerOrgEntry, sdk/pas.go). crd-order-dispatch is
// deliberately NOT covered: no seam-mapped deployment wires a dispatch pair today
// (the demo peers fail that leg closed unconfigured), while the reference payer's
// CRD payor fence applies to BOTH hook shapes — so a future deployment that routes
// crd-order-dispatch through a mapped backend must extend the seam to that leg's
// prefetch coverage too, or the backend will refuse it the same way.
package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// payorEdgeRefusal builds the A1 fail-closed LegResult: a 400-class, legible refusal
// naming the mismatch, never a bare error (which the engine maps to an opaque
// "hub routing failed" 500 — this refusal must be visible to the caller as a policy
// decision, not a transport fault).
func payorEdgeRefusal(own, got shnsdk.PayerIdentifier, gotOK bool) LegResult {
	if !gotOK {
		return LegResult{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"payer backend identity mapping: inbound Coverage carries no resolvable payor identifier (this gateway's own payer identity is %s|%s); refusing rather than adjudicating for an unrecognized payer",
				own.System, own.Value),
		}
	}
	return LegResult{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf(
			"payer backend identity mapping: inbound Coverage payor %s|%s does not match this gateway's own payer identity %s|%s; refusing rather than adjudicating as a different payer",
			got.System, got.Value, own.System, own.Value),
	}
}

// restampBareCoveragePayor is the CRD/DTR payer-edge identity mapping seam over a bare
// Coverage resource (A1/A2): got/gotOK report what payor identity (if any) the inbound
// Coverage resolved to (shnsdk.ParsePayerIdentifier, contained + inline forms only — a
// bare Coverage from CRD/DTR carries no bundle to resolve an external reference
// against). matched is true only when got==own AND the restamp succeeded, in which case
// out carries the re-stamped bytes; otherwise out is the ORIGINAL bytes and the caller
// must refuse (payorEdgeRefusal).
func restampBareCoveragePayor(coverageJSON []byte, own, backend shnsdk.PayerIdentifier) (out []byte, got shnsdk.PayerIdentifier, gotOK bool, matched bool, err error) {
	got, gotOK = shnsdk.ParsePayerIdentifier(coverageJSON, nil)
	if !gotOK || got != own {
		return coverageJSON, got, gotOK, false, nil
	}
	out, err = restampCoveragePayorTarget(coverageJSON, backend, nil)
	if err != nil {
		return nil, got, gotOK, false, err
	}
	return out, got, gotOK, true, nil
}

// restampInlineIdentifier rewrites p["identifier"].system/value to backend's, leaving
// every other field of p (including "identifier"'s own other sub-fields, e.g. "type" or
// "period") untouched. changed=false means p carries no inline identifier with a
// non-empty system+value — the caller falls through to the next candidate form
// (contained fragment / bundle entry).
func restampInlineIdentifier(p map[string]json.RawMessage, backend shnsdk.PayerIdentifier) (map[string]json.RawMessage, bool, error) {
	idRaw, ok := p["identifier"]
	if !ok || len(idRaw) == 0 {
		return p, false, nil
	}
	var idm map[string]json.RawMessage
	if err := json.Unmarshal(idRaw, &idm); err != nil {
		return p, false, nil
	}
	var sys, val string
	_ = json.Unmarshal(idm["system"], &sys)
	_ = json.Unmarshal(idm["value"], &val)
	if sys == "" || val == "" {
		return p, false, nil
	}
	sysJSON, err := json.Marshal(backend.System)
	if err != nil {
		return p, false, err
	}
	valJSON, err := json.Marshal(backend.Value)
	if err != nil {
		return p, false, err
	}
	idm["system"] = sysJSON
	idm["value"] = valJSON
	newIDJSON, err := json.Marshal(idm)
	if err != nil {
		return p, false, err
	}
	out := make(map[string]json.RawMessage, len(p))
	for k, v := range p {
		out[k] = v
	}
	out["identifier"] = newIDJSON
	return out, true, nil
}

// orgHasID reports whether orgJSON is a FHIR Organization with the given id — used to
// locate a contained fragment ("#id") a Coverage/Claim's own "contained" array carries.
func orgHasID(orgJSON []byte, id string) bool {
	var o struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if json.Unmarshal(orgJSON, &o) != nil {
		return false
	}
	return o.ResourceType == "Organization" && o.ID == id
}

// restampOrganizationIdentifier rewrites the FIRST identifier entry of orgJSON that
// carries a non-empty system+value to backend's — the same "first match" rule
// shnsdk.ParsePayerIdentifier's orgIdentifier reads by, so the entry restamped here is
// exactly the one an A1 assertion would have read. Every other field of the
// Organization (name, id, resourceType, any other identifier entries) is left
// untouched (A2).
func restampOrganizationIdentifier(orgJSON []byte, backend shnsdk.PayerIdentifier) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(orgJSON, &m); err != nil {
		return nil, fmt.Errorf("engine: payor edge: parse organization: %w", err)
	}
	var idents []map[string]json.RawMessage
	if err := json.Unmarshal(m["identifier"], &idents); err != nil || len(idents) == 0 {
		return nil, fmt.Errorf("engine: payor edge: organization has no identifier to restamp")
	}
	found := false
	for i, id := range idents {
		var sys, val string
		_ = json.Unmarshal(id["system"], &sys)
		_ = json.Unmarshal(id["value"], &val)
		if sys == "" || val == "" {
			continue
		}
		sysJSON, err := json.Marshal(backend.System)
		if err != nil {
			return nil, err
		}
		valJSON, err := json.Marshal(backend.Value)
		if err != nil {
			return nil, err
		}
		id["system"] = sysJSON
		id["value"] = valJSON
		idents[i] = id
		found = true
		break
	}
	if !found {
		return nil, fmt.Errorf("engine: payor edge: organization identifier has no system/value to restamp")
	}
	identsJSON, err := json.Marshal(idents)
	if err != nil {
		return nil, err
	}
	m["identifier"] = identsJSON
	return json.Marshal(m)
}

// restampCoveragePayorTarget mutates coverageJSON's payor[0] target to backend's
// identity, trying (in the SAME priority order shnsdk.ParsePayerIdentifier reads):
//  1. an inline identifier (Coverage.payor[0].identifier) — rewritten in place;
//  2. a contained fragment ("#id") — the matching entry in Coverage's OWN "contained"
//     array is rewritten in place;
//  3. only when entryRestamp is non-nil (the PAS bundle case — see
//     restampPASBundlePayor), a resolvable bundle-entry reference (the demo/kit
//     conformant PayerOrgEntry shape, sdk/pas.go's repointPayorToEntry): the mutation
//     lands on the OTHER bundle entry, so coverageJSON itself is returned unchanged.
//
// A bare CRD/DTR Coverage never reaches form 3 (entryRestamp is nil there — there is no
// bundle to resolve an external reference against), matching the fact that
// shnsdk.ParsePayerIdentifier(coverageJSON, nil) could not have resolved that form
// either, so restampBareCoveragePayor never calls this function with a target it
// can't reach.
func restampCoveragePayorTarget(coverageJSON []byte, backend shnsdk.PayerIdentifier, entryRestamp func(ref string) (bool, error)) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(coverageJSON, &m); err != nil {
		return nil, fmt.Errorf("engine: payor edge: parse coverage: %w", err)
	}
	var payor []map[string]json.RawMessage
	if err := json.Unmarshal(m["payor"], &payor); err != nil || len(payor) == 0 {
		return nil, fmt.Errorf("engine: payor edge: coverage has no payor to restamp")
	}
	p := payor[0]

	if newP, changed, err := restampInlineIdentifier(p, backend); err != nil {
		return nil, err
	} else if changed {
		payor[0] = newP
		payorJSON, err := json.Marshal(payor)
		if err != nil {
			return nil, err
		}
		m["payor"] = payorJSON
		return json.Marshal(m)
	}

	var ref string
	if refRaw, ok := p["reference"]; ok {
		_ = json.Unmarshal(refRaw, &ref)
	}
	if strings.HasPrefix(ref, "#") {
		id := strings.TrimPrefix(ref, "#")
		var contained []json.RawMessage
		_ = json.Unmarshal(m["contained"], &contained)
		for i, c := range contained {
			if orgHasID(c, id) {
				newOrg, err := restampOrganizationIdentifier(c, backend)
				if err != nil {
					return nil, err
				}
				contained[i] = newOrg
				cb, err := json.Marshal(contained)
				if err != nil {
					return nil, err
				}
				m["contained"] = cb
				return json.Marshal(m)
			}
		}
		return nil, fmt.Errorf("engine: payor edge: contained Organization %q referenced by coverage.payor not found", id)
	}

	if ref != "" && entryRestamp != nil {
		if changed, err := entryRestamp(ref); err != nil {
			return nil, err
		} else if changed {
			return coverageJSON, nil // the mutation landed on a different bundle entry
		}
	}

	return nil, fmt.Errorf("engine: payor edge: coverage.payor is not restampable (no inline identifier, contained fragment, or resolvable bundle entry)")
}

// resourceTypeAndID reads {resourceType, id} off a raw FHIR resource — best-effort
// (empty strings on any parse failure).
func resourceTypeAndID(raw json.RawMessage) (string, string) {
	var p struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.ResourceType, p.ID
}

// payorEdgeBundleEntry is the minimal Bundle.entry shape the PAS seam reads/writes.
type payorEdgeBundleEntry struct {
	FullURL  string          `json:"fullUrl,omitempty"`
	Resource json.RawMessage `json:"resource"`
}

// payorEdgeBundle is a parsed $submit collection Bundle, held open for the PAS seam's
// read (identity resolution) + write (restamp) passes over the SAME entry set.
type payorEdgeBundle struct {
	raw     map[string]json.RawMessage
	entries []payorEdgeBundleEntry
}

func parsePayorEdgeBundle(bundleJSON []byte) (*payorEdgeBundle, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(bundleJSON, &m); err != nil {
		return nil, fmt.Errorf("engine: payor edge: parse bundle: %w", err)
	}
	var entries []payorEdgeBundleEntry
	if raw, ok := m["entry"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("engine: payor edge: parse bundle entries: %w", err)
		}
	}
	return &payorEdgeBundle{raw: m, entries: entries}, nil
}

func (b *payorEdgeBundle) marshal() ([]byte, error) {
	entriesJSON, err := json.Marshal(b.entries)
	if err != nil {
		return nil, err
	}
	b.raw["entry"] = entriesJSON
	return json.Marshal(b.raw)
}

// findIndex returns the index of the first entry whose resource is of resourceType, or
// -1.
func (b *payorEdgeBundle) findIndex(resourceType string) int {
	for i, e := range b.entries {
		if rt, _ := resourceTypeAndID(e.Resource); rt == resourceType {
			return i
		}
	}
	return -1
}

// findByRef returns the index of the entry ref resolves to — either an absolute
// fullUrl match or a relative "ResourceType/id" match (both forms a conformant PAS
// bundle can carry, AbsoluteRefs on or off) — or -1.
func (b *payorEdgeBundle) findByRef(ref string) int {
	for i, e := range b.entries {
		rt, id := resourceTypeAndID(e.Resource)
		if ref == e.FullURL || (rt != "" && id != "" && ref == rt+"/"+id) {
			return i
		}
	}
	return -1
}

// resolveRef is the shnsdk.ParsePayerIdentifier external-resolver callback: it resolves
// a Coverage.payor[0].reference against this bundle's own entries (the demo/kit
// PayerOrgEntry shape).
func (b *payorEdgeBundle) resolveRef(ref string) ([]byte, bool) {
	if i := b.findByRef(ref); i >= 0 {
		return b.entries[i].Resource, true
	}
	return nil, false
}

// restampReferencedOrg re-stamps the Organization ref resolves to (a bundle entry
// only) with backend's identity. changed=false, nil error is the benign "nothing to do
// here" outcome — a contained fragment, an inline identifier, or a reference that
// resolves to no entry, or to a non-Organization entry — all fall through silently:
// this is a BEST-EFFORT companion restamp (Claim.insurer in restampPASBundlePayor),
// never a second gate (the identity assertion already ran against Coverage.payor).
func (b *payorEdgeBundle) restampReferencedOrg(ref string, backend shnsdk.PayerIdentifier) (changed bool, err error) {
	if ref == "" || strings.HasPrefix(ref, "#") {
		return false, nil
	}
	idx := b.findByRef(ref)
	if idx < 0 {
		return false, nil
	}
	if rt, _ := resourceTypeAndID(b.entries[idx].Resource); rt != "Organization" {
		return false, nil
	}
	newOrg, err := restampOrganizationIdentifier(b.entries[idx].Resource, backend)
	if err != nil {
		return false, err
	}
	b.entries[idx].Resource = newOrg
	return true, nil
}

// restampPASBundlePayor is the PAS-leg (pas-claim / pas-claim-update) payer-edge
// identity mapping seam (A1/A2): it re-stamps the $submit Bundle's Coverage.payor +
// Claim.insurer identity from own to backend, ONLY when the inbound Coverage's payor
// resolves to own. got/gotOK/matched mirror restampBareCoveragePayor's contract
// (payorEdgeRefusal reads them the same way); matched=false leaves the bundle
// UNCHANGED (out is the caller's own bytes, safe to ignore).
//
// Claim.insurer is restamped BEST-EFFORT (restampReferencedOrg): in the demo/kit
// conformant shape (PayerOrgEntry) it references the SAME Organization entry
// Coverage.payor does, so this is a no-op there once the Coverage-side restamp has
// already flipped that shared entry; it exists for a bundle shape where the two
// references resolve to distinct entries.
func restampPASBundlePayor(bundleJSON []byte, own, backend shnsdk.PayerIdentifier) (out []byte, got shnsdk.PayerIdentifier, gotOK bool, matched bool, err error) {
	b, perr := parsePayorEdgeBundle(bundleJSON)
	if perr != nil {
		return nil, shnsdk.PayerIdentifier{}, false, false, perr
	}
	covIdx := b.findIndex("Coverage")
	if covIdx < 0 {
		return bundleJSON, shnsdk.PayerIdentifier{}, false, false, nil
	}
	coverageJSON := b.entries[covIdx].Resource
	got, gotOK = shnsdk.ParsePayerIdentifier(coverageJSON, b.resolveRef)
	if !gotOK || got != own {
		return bundleJSON, got, gotOK, false, nil
	}

	newCov, cerr := restampCoveragePayorTarget(coverageJSON, backend, func(ref string) (bool, error) {
		return b.restampReferencedOrg(ref, backend)
	})
	if cerr != nil {
		return nil, got, gotOK, false, cerr
	}
	b.entries[covIdx].Resource = newCov

	if claimIdx := b.findIndex("Claim"); claimIdx >= 0 {
		var claim struct {
			Insurer struct {
				Reference string `json:"reference"`
			} `json:"insurer"`
		}
		if json.Unmarshal(b.entries[claimIdx].Resource, &claim) == nil {
			if _, ierr := b.restampReferencedOrg(claim.Insurer.Reference, backend); ierr != nil {
				return nil, got, gotOK, false, ierr
			}
		}
	}

	outBytes, merr := b.marshal()
	if merr != nil {
		return nil, got, gotOK, false, merr
	}
	return outBytes, got, gotOK, true, nil
}
