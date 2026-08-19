// transform_pas.go — the pa.pas cross-version step modules: the two
// adjacent-line bridges (2.0<->2.1,
// 2.1<->2.2) compat.go's manifest rows wire up. Every delta modeled here is
// verified from sdk/linedef.go's PASDef fields first (ClaimResponseRequestRequired,
// PendedResponseOutcome, ResponseBundleIdentifierRequired, ClaimItemLineDetailRequired,
// ClaimRelatedRelationshipRequired) and, where the def is silent, from a live
// diff of the pinned PAS 2.0.1/2.1.0/2.2.1 package differentials (2026-08-12,
// packages.simplifier.net/hl7.fhir.us.davinci-pas) — never invented
// (transform-iff). Each step's own doc comment below carries the
// per-direction derivation this file implements.
package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// pasCorrelationSystem / pasBundleIdentifierSystem mirror the unexported
// constants of the same name and value in sdk/pas.go (:75,80) — the wire
// convention every SHN-built PAS Claim/ClaimResponse/Bundle already uses.
// Duplicated (not imported — sdk does not export them) rather than
// re-derived: these are literal citations of the existing wire contract, not
// a new invention.
const (
	pasCorrelationSystem      = "urn:shn:correlation"
	pasBundleIdentifierSystem = "urn:shn:pas:bundle"
)

// SemanticChangeError is the typed refusal a gated CompatStep direction
// returns when the source line has no honest byte-level value for an
// element the target line requires (loss policy: refuse only on semantic
// change, never fabricate). Route callers match this with
// errors.As to distinguish a gated-step refusal from any other applyChain
// failure (e.g. chainDisconnectedError, a plain I/O/parse error).
type SemanticChangeError struct {
	Contract        string   // "pa.pas"
	From, To        string   // adjacent line pair, low->high order (e.g. "2.0", "2.1")
	Direction       string   // "up" | "down" — which TransformFunc refused
	MissingElements []string // FHIRPath-ish locators the target line requires with no honest source
}

func (e *SemanticChangeError) Error() string {
	return fmt.Sprintf("shn: semantic-change refusal: %s %s->%s (%s direction): no honest byte-level source for %s",
		e.Contract, e.From, e.To, e.Direction, strings.Join(e.MissingElements, ", "))
}

// TransformPASForTest is a thin exported wrapper around the pa.pas compat
// chain (chainFor + applyChain) for the test/conformance package — a
// DIFFERENT Go module (the substrate repo, which imports this one) that
// cannot see engine-internal symbols across the module boundary. It exists
// so that package can validate transform OUTPUT against the live per-line
// HAPI lanes. Named *ForTest to signal it is a test seam, not published
// SDK/API surface (transform.go's header: this machinery has no sdk twin).
func TransformPASForTest(from, to string, payload []byte, x ExchangeIdentity) ([]byte, []LossReport, error) {
	steps := chainFor("pa.pas", from, to)
	if steps == nil {
		return nil, nil, fmt.Errorf("engine: TransformPASForTest: no pa.pas chain %s->%s", from, to)
	}
	return applyChain(steps, from, payload, x)
}

// ---------------------------------------------------------------------------
// pa.pas 2.0 <-> 2.1
// ---------------------------------------------------------------------------

// pasStep2021Up bridges a 2.0-shaped PAS payload up to 2.1. Two payload
// shapes flow through this contract, and this ONE Up func must handle
// whichever arrives (a CompatStep row bridges an adjacent line pair for
// BOTH traffic directions of the contract, not one FHIR resource type):
//
//   - request-direction (a Claim, bare or Bundle-embedded): PAS 2.1's
//     profile-claim.json/profile-claim-update.json differential makes
//     Claim.item.extension:certificationType, :requestType, and
//     Claim.item.location[x] min=1 (PASDef.ClaimItemLineDetailRequired, verified
//     unchanged 2.1.0->2.2.1, absent/unconstrained at 2.0.1), and
//     profile-claim-update.json additionally makes Claim.related.relationship
//     min=1 (PASDef.ClaimRelatedRelationshipRequired, absent at 2.0.1). A
//     2.0-native Claim (PASDef "2.0": both flags false — the 2.0 builder never
//     populates them) has NO honest byte-level value to mint for any of the
//     four: certificationType/requestType are X12-coded medical-necessity
//     classifications, location[x] is a CMS place-of-service code, and
//     relationship links to a prior claim — none derivable from what a 2.0
//     payload carries. This direction UNCONDITIONALLY refuses (gated) — the
//     native 2.1 builders (sdk/pas.go) remain the honest 2.1 producers.
//   - response-direction (a ClaimResponse, bare or Bundle-embedded next to a
//     Task for the pended shape): PAS 2.1 makes ClaimResponse.request min=1
//     (PASDef.ClaimResponseRequestRequired). The response builders already
//     populate this with Reference.identifier = {urn:shn:correlation,
//     correlationID} — the SAME correlation id every request Claim carries
//     (sdk/linedef.go's ClaimResponseRequestRequired doc, sdk/pasresponder.go:
//     331-336) — so synthesizing it from ExchangeIdentity.CorrelationID is a
//     deterministic, lossless upcast (full, not gated). Recorded as a
//     Synthesized LossEntry.
func pasStep2021Up(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
	top, err := pasParseTop(payload)
	if err != nil {
		return nil, LossReport{}, err
	}
	resources := pasCollectResources(top)

	if len(resources["Claim"]) > 0 {
		return nil, LossReport{}, &SemanticChangeError{
			Contract:  "pa.pas",
			From:      "2.0",
			To:        "2.1",
			Direction: "up",
			MissingElements: []string{
				"Claim.item.extension:certificationType",
				"Claim.item.extension:requestType",
				"Claim.item.location[x]",
				"Claim.related.relationship",
			},
		}
	}

	def, ok := shnsdk.PASLineDef("2.1")
	if !ok {
		return nil, LossReport{}, fmt.Errorf("engine: pasStep2021Up: no PASLineDef for 2.1")
	}
	var synth []LossEntry
	if def.ClaimResponseRequestRequired {
		for _, cr := range resources["ClaimResponse"] {
			if _, present := cr["request"]; present {
				continue // already honest (e.g. a foreign 2.0 payload that opportunistically set it) — never clobber real data.
			}
			if x.CorrelationID == "" {
				return nil, LossReport{}, fmt.Errorf("engine: pasStep2021Up: ClaimResponse.request synthesis requires a non-empty ExchangeIdentity.CorrelationID")
			}
			cr["request"] = map[string]any{
				"identifier": map[string]any{
					"system": pasCorrelationSystem,
					"value":  x.CorrelationID,
				},
			}
			synth = append(synth, LossEntry{
				Path:   "ClaimResponse.request",
				Detail: "synthesized from correlation id (ClaimResponseRequestRequired at 2.1)",
			})
		}
	}

	out, err := json.Marshal(top)
	if err != nil {
		return nil, LossReport{}, fmt.Errorf("engine: pasStep2021Up: marshal: %w", err)
	}
	return out, LossReport{Module: "pa.pas 2.0->2.1", Source: "2.0", Target: "2.1", Synthesized: synth}, nil
}

// pasStep2021Down bridges a 2.1-shaped PAS payload down to 2.0. Verified
// live (2026-08-12) against both package differentials: the item extensions'
// canonical URLs (extension-certificationType, extension-
// serviceItemRequestType, both under StructureDefinition-extension-*.json)
// are IDENTICAL, UNVERSIONED strings at 2.0.1 and 2.1.0 (the instance-level
// Extension.url — the |2.1.0/|2.2.1 suffix only appears in a profile's OWN
// differential.type.profile PIN, never on the wire), and
// profile-claim-base.json's Claim.extension slicing is OPEN at every line
// (discriminator on url, rules:"open") — so a 2.1 payload carrying these
// extensions (plus location[x] / related.relationship, likewise unconstrained
// at 2.0) downcasts to 2.0 unchanged and still validates: 2.0's profile
// simply does not require what 2.1 requires, but tolerates it (superset
// tolerance). ClaimResponse.request is symmetric: 2.0's profile leaves it
// min=0 (never min=0-forbidding-max=0), so leaving a 2.1-synthesized request
// field in place downcasting to 2.0 is likewise tolerated, not dropped.
// Nothing to drop, nothing to carry — pure pass-through (full).
func pasStep2021Down(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
	out := append([]byte(nil), payload...)
	return out, LossReport{Module: "pa.pas 2.1->2.0", Source: "2.1", Target: "2.0"}, nil
}

// ---------------------------------------------------------------------------
// pa.pas 2.1 <-> 2.2
// ---------------------------------------------------------------------------

// pas22OnlyClaimExtensions / pas22OnlyClaimResponseExtensions: TOP-LEVEL
// (Claim.extension / ClaimResponse.extension) extension slices PAS 2.2.1
// introduces with NO 2.1.0 slot — verified via a live diff of the 2.1.0 vs
// 2.2.1 package differentials for profile-claim-base.json /
// profile-claimresponse-base.json (2026-08-12): every slice below is min=0 at
// 2.2.1 and ABSENT from the 2.1.0 differential entirely (not merely
// optional-and-present — genuinely new).
//
// Membership is BOTH-CONDITIONS: the 2.2.1 profile must declare the top-level
// slice AND the extension's own SD context[] must permit that host. PAS 2.2.1
// declares three slices that fail the second test —
// Claim.extension:{authorizationNumber,administrationReferenceNumber} and
// ClaimResponse.extension:authorizedProvider — whose extension SDs are
// contexted to Claim.item / ClaimResponse.item / ClaimResponse.addItem /
// ExplanationOfBenefit(.item). A profile cannot widen an extension's context
// and the validator enforces context, so those slices cannot appear on a
// conformant wire. At their only legal (item) locus they are present
// identically at 2.1.0 and 2.2.1 — line-invariant, nothing to bridge, nothing
// to carry. Verified live against the 2.2 lane and by reading both packages
// This is an upstream IG self-contradiction, not an SHN modelling
// choice; re-add only if a future PAS release makes the top-level slice
// context-legal AND absent at the lower line.
//
// Map values are the FHIR StructureDefinition slice NAME (for LossEntry.Path
// / CarryElement's path locator), NOT the canonical URL (the map key).
//
// Deliberately NOT modeled: the 2.2.1-only ITEM-/addItem-level extensions
// found in the same diff (ClaimResponse.item.extension:admissionDates/
// dischargeDate, ClaimResponse.addItem.extension:* — admissionDates,
// category, certificationType, dischargeDate, requestType) and the
// processNote/supportingInfo:AdditionalInformation additions. SHN never
// builds ClaimResponse.addItem or Claim.supportingInfo (grep confirms no
// producer), so per produce-iff these have no current producer OR consumer —
// carrying them would be untested, unverified-by-use scope creep. Re-add
// iff a real (native or forwarded) payload exercises one.
var pas22OnlyClaimExtensions = map[string]string{
	"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers": "transmissionIdentifiers",
}
var pas22OnlyClaimResponseExtensions = map[string]string{
	"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-claimResponseReviewer":   "claimResponseReviewer",
	"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers": "transmissionIdentifiers",
}

// pasStep2122Up bridges a 2.1-shaped PAS payload up to 2.2.
//
//   - response-direction: when the Bundle carries a Task entry (SHN's pend
//     discriminator — PASDef's PendedResponseOutcome doc: "the pend is
//     carried by the Bundle's Task entry, never by the outcome code"), the
//     inner ClaimResponse.outcome is rewritten to the TARGET line's
//     PendedResponseOutcome ("complete" at 2.2 — PAS 2.2.1 replaces the base
//     remittance-outcome binding, which included "queued", with its own
//     required ValueSet-ClaimResponseOutcome = {complete,error,partial},
//     verified live). When the document is a response Bundle
//     (top-level resourceType=="Bundle" containing a ClaimResponse) and 2.2's
//     ResponseBundleIdentifierRequired is true (profile-pas-response-bundle.json
//     Bundle.identifier min=1 at 2.2.1 only, verified live — absent at
//     2.0.1/2.1.0), Bundle.identifier is synthesized from the correlation id
//     (same value the embedded ClaimResponse.identifier already carries) if
//     absent. Both are deterministic, lossless (full) — recorded as
//     Synthesized where content is minted; the outcome rebinding mints no NEW
//     content (existing value remapped 1:1 via PASLineDef, not modeled as a
//     LossEntry).
//   - request-direction: PAS 2.1.0 and 2.2.1 are IDENTICAL on every
//     CLAIM-scoped element this adjacency could touch (ClaimItemLineDetailRequired,
//     ClaimRelatedRelationshipRequired both true, UNCHANGED 2.1.0->2.2.1 per
//     PASDef — verified live: the Claim/Patient/Coverage/ServiceRequest
//     entries of testdata/golden/2.1/conformant/pas-submit-request.json and
//     testdata/golden/2.2/conformant/pas-submit-request.json are
//     byte-identical) — pass through unchanged. The submit Bundle's embedded
//     QuestionnaireResponse entry (DTR-owned — DTRDef's
//     SingleCoverageConstraint/IntendedUseCodeSystem/AutoOriginSourceCode
//     deltas, which DO differ 2.1->2.2, verified live) is deliberately NOT
//     touched here: it is pa.dtr's own compat row, a SEPARATE
//     contract negotiated independently of pa.pas per the token grammar
//     ("pa.dtr@<line>" vs "pa.pas@<line>") — bridging it is out of this
//     step's scope even though it happens to ride in the same wire Bundle.
//   - restores any shn-carried-content extension a PRIOR 2.2->2.1 Down step
//     carried (the other half of the carry mechanism — sdk/carry.go's
//     Restore(Carry(x))==x contract), on both Claim.extension and
//     ClaimResponse.extension.
func pasStep2122Up(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
	top, err := pasParseTop(payload)
	if err != nil {
		return nil, LossReport{}, err
	}
	resources := pasCollectResources(top)

	if _, hasTask := resources["Task"]; hasTask {
		def, ok := shnsdk.PASLineDef("2.2")
		if !ok {
			return nil, LossReport{}, fmt.Errorf("engine: pasStep2122Up: no PASLineDef for 2.2")
		}
		for _, cr := range resources["ClaimResponse"] {
			// NOT a LossEntry: LossReport models CONTENT that was moved
			// (Carried) or MINTED (Synthesized) — this 1:1 code rebinding
			// (queued<->complete, both spellings of the SAME pend state) does
			// neither; the value is exactly recoverable from PASLineDef at
			// either end, so nothing is lost or fabricated to declare.
			cr["outcome"] = def.PendedResponseOutcome
		}
	}

	var synth []LossEntry
	if rt, _ := top["resourceType"].(string); rt == "Bundle" && len(resources["ClaimResponse"]) > 0 {
		def, ok := shnsdk.PASLineDef("2.2")
		if !ok {
			return nil, LossReport{}, fmt.Errorf("engine: pasStep2122Up: no PASLineDef for 2.2")
		}
		if def.ResponseBundleIdentifierRequired {
			if _, present := top["identifier"]; !present {
				if x.CorrelationID == "" {
					return nil, LossReport{}, fmt.Errorf("engine: pasStep2122Up: Bundle.identifier synthesis requires a non-empty ExchangeIdentity.CorrelationID")
				}
				top["identifier"] = map[string]any{
					"system": pasBundleIdentifierSystem,
					"value":  x.CorrelationID,
				}
				synth = append(synth, LossEntry{
					Path:   "Bundle.identifier",
					Detail: "synthesized from correlation id (ResponseBundleIdentifierRequired at 2.2)",
				})
			}
		}
	}

	for _, c := range resources["Claim"] {
		if err := pasRestoreCarriedExtensions(c); err != nil {
			return nil, LossReport{}, fmt.Errorf("engine: pasStep2122Up: restore Claim.extension: %w", err)
		}
	}
	for _, cr := range resources["ClaimResponse"] {
		if err := pasRestoreCarriedExtensions(cr); err != nil {
			return nil, LossReport{}, fmt.Errorf("engine: pasStep2122Up: restore ClaimResponse.extension: %w", err)
		}
	}

	out, err := json.Marshal(top)
	if err != nil {
		return nil, LossReport{}, fmt.Errorf("engine: pasStep2122Up: marshal: %w", err)
	}
	return out, LossReport{Module: "pa.pas 2.1->2.2", Source: "2.1", Target: "2.2", Synthesized: synth}, nil
}

// pasStep2122Down bridges a 2.2-shaped PAS payload down to 2.1.
//
//   - response-direction: when a Task entry is present (pend), rewrite
//     ClaimResponse.outcome to 2.1's PendedResponseOutcome ("queued") —
//     symmetric with pasStep2122Up, both def-driven (PASLineDef), never a
//     hardcoded literal pair. A non-pended "complete" (approve/deny) is left
//     unchanged — 2.1's outcome binding is unconstrained (inherits base R4
//     remittance-outcome, which already includes "complete"), so no loss.
//     Bundle.identifier is NOT stripped: 2.1's profile-pas-response-bundle.json
//     has no differential constraint on Bundle.identifier at all (verified
//     live — base R4's Bundle.identifier 0..1 applies), so an extra
//     identifier downcasting from 2.2 is tolerated, not dropped (superset
//     tolerance, same principle as pasStep2021Down).
//   - request- AND response-direction: carries any TOP-LEVEL 2.2-only
//     extension (pas22OnlyClaimExtensions / pas22OnlyClaimResponseExtensions)
//     into shn-carried-content (sdk/carry.go) — the spec's own
//     authorizationNumber example, verified against the 2.2.1 SD before any
//     code (see the two maps' doc comment). Recorded as Carried LossEntries.
func pasStep2122Down(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
	top, err := pasParseTop(payload)
	if err != nil {
		return nil, LossReport{}, err
	}
	resources := pasCollectResources(top)

	if _, hasTask := resources["Task"]; hasTask {
		def, ok := shnsdk.PASLineDef("2.1")
		if !ok {
			return nil, LossReport{}, fmt.Errorf("engine: pasStep2122Down: no PASLineDef for 2.1")
		}
		for _, cr := range resources["ClaimResponse"] {
			// NOT a LossEntry — same exemption as pasStep2122Up's mirror
			// rebinding: a 1:1 code swap, recoverable either direction from
			// PASLineDef, is neither content moved (Carried) nor minted
			// (Synthesized).
			cr["outcome"] = def.PendedResponseOutcome
		}
	}

	var carried []LossEntry
	for _, c := range resources["Claim"] {
		entries, err := pasCarryExtensions(c, "Claim", pas22OnlyClaimExtensions, "2.2")
		if err != nil {
			return nil, LossReport{}, fmt.Errorf("engine: pasStep2122Down: carry Claim.extension: %w", err)
		}
		carried = append(carried, entries...)
	}
	for _, cr := range resources["ClaimResponse"] {
		entries, err := pasCarryExtensions(cr, "ClaimResponse", pas22OnlyClaimResponseExtensions, "2.2")
		if err != nil {
			return nil, LossReport{}, fmt.Errorf("engine: pasStep2122Down: carry ClaimResponse.extension: %w", err)
		}
		carried = append(carried, entries...)
	}

	out, err := json.Marshal(top)
	if err != nil {
		return nil, LossReport{}, fmt.Errorf("engine: pasStep2122Down: marshal: %w", err)
	}
	return out, LossReport{Module: "pa.pas 2.2->2.1", Source: "2.2", Target: "2.1", Carried: carried}, nil
}

// ---------------------------------------------------------------------------
// generic JSON-document helpers
// ---------------------------------------------------------------------------

// pasParseTop decodes the top-level PAS payload (a bare Claim/ClaimResponse
// or a Bundle) into a mutable map. Fails loudly on invalid JSON — never
// returns a zero-value success.
func pasParseTop(payload []byte) (map[string]any, error) {
	var top map[string]any
	if err := json.Unmarshal(payload, &top); err != nil {
		return nil, fmt.Errorf("engine: pas transform: invalid JSON: %w", err)
	}
	return top, nil
}

// pasCollectResources returns every resource found in the document, keyed by
// FHIR resourceType: the document itself when it is a bare resource, or each
// Bundle.entry[].resource when the document is a Bundle. The returned maps
// are the SAME map values referenced by top (Go maps are reference types) —
// mutating a returned map mutates the document, so callers edit fields
// directly and re-marshal top to get the transformed payload back.
func pasCollectResources(top map[string]any) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	add := func(m map[string]any) {
		rt, _ := m["resourceType"].(string)
		if rt == "" {
			return
		}
		out[rt] = append(out[rt], m)
	}
	if rt, _ := top["resourceType"].(string); rt == "Bundle" {
		entries, _ := top["entry"].([]any)
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			rm, ok := em["resource"].(map[string]any)
			if !ok {
				continue
			}
			add(rm)
		}
		return out
	}
	add(top)
	return out
}

// pasCarryExtensions scans res[hostType].extension (a top-level FHIR
// extension array) for entries whose url is a key of allow, replacing each
// match with an shn-carried-content wrapper (sdk/carry.go's CarryElement) and
// returning one LossEntry per element carried. Non-matching extensions are
// left untouched (open slicing at every PAS line permits shn-carried-content
// alongside any other extension). A no-op (nil, nil) when the array is empty
// or absent.
func pasCarryExtensions(res map[string]any, hostType string, allow map[string]string, sourceLine string) ([]LossEntry, error) {
	extAny, _ := res["extension"].([]any)
	if len(extAny) == 0 {
		return nil, nil
	}
	kept := make([]any, 0, len(extAny))
	var entries []LossEntry
	for _, e := range extAny {
		em, ok := e.(map[string]any)
		if !ok {
			kept = append(kept, e)
			continue
		}
		url, _ := em["url"].(string)
		sliceName, matched := allow[url]
		if !matched {
			kept = append(kept, e)
			continue
		}
		raw, err := json.Marshal(em)
		if err != nil {
			return nil, fmt.Errorf("marshal %s.extension:%s: %w", hostType, sliceName, err)
		}
		carriedExt, err := shnsdk.CarryElement(hostType+".extension:"+sliceName, raw, sourceLine)
		if err != nil {
			return nil, fmt.Errorf("carry %s.extension:%s: %w", hostType, sliceName, err)
		}
		var carriedExtAny any
		if err := json.Unmarshal(carriedExt, &carriedExtAny); err != nil {
			return nil, fmt.Errorf("decode carried %s.extension:%s: %w", hostType, sliceName, err)
		}
		kept = append(kept, carriedExtAny)
		entries = append(entries, LossEntry{
			Path:   hostType + ".extension:" + sliceName,
			Detail: fmt.Sprintf("carried; source line %s (no 2.1 slot)", sourceLine),
		})
	}
	res["extension"] = kept
	return entries, nil
}

// pasRestoreCarriedExtensions is pasCarryExtensions's inverse: scans
// res[...].extension for shn-carried-content wrappers and replaces each with
// the ORIGINAL element it carried (sdk/carry.go's RestoreCarried), byte-
// identical to what CarryElement was given. A no-op when the array is empty
// or carries no shn-carried-content entries.
func pasRestoreCarriedExtensions(res map[string]any) error {
	extAny, _ := res["extension"].([]any)
	if len(extAny) == 0 {
		return nil
	}
	kept := make([]any, 0, len(extAny))
	for _, e := range extAny {
		em, ok := e.(map[string]any)
		if !ok {
			kept = append(kept, e)
			continue
		}
		if url, _ := em["url"].(string); url != shnsdk.CarriedContentExtURL {
			kept = append(kept, e)
			continue
		}
		raw, err := json.Marshal(em)
		if err != nil {
			return fmt.Errorf("marshal carried-content wrapper: %w", err)
		}
		_, element, _, err := shnsdk.RestoreCarried(raw)
		if err != nil {
			return fmt.Errorf("restore carried-content wrapper: %w", err)
		}
		// Push element back as json.RawMessage, NOT decoded-then-Go-value: a
		// decode-to-any/re-Marshal round trip is only JSON-EQUIVALENT (Go's
		// map marshaling always re-sorts keys alphabetically), which would
		// silently defeat Restore(Carry(x))==x's BYTE-level guarantee
		// (sdk/carry.go's contract, pinned by sdk/carry_test.go's
		// TestCarryRestoreRoundTrip) the moment this element sits inside a
		// larger document that gets re-marshaled. json.RawMessage implements
		// json.Marshaler (MarshalJSON returns its own bytes verbatim), so
		// even boxed in this []any slice, encoding/json emits it UNCHANGED
		// when the enclosing document is marshaled — true byte-fidelity, not
		// mere structural equivalence.
		kept = append(kept, json.RawMessage(element))
	}
	res["extension"] = kept
	return nil
}
