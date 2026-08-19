// transform_dtr.go — the pa.dtr cross-version step modules: the two
// adjacent-line bridges (2.0<->2.1,
// 2.1<->2.2) compat.go's manifest rows wire up. Every delta modeled here is
// verified from sdk/linedef.go's DTRDef fields first
// (QuestionnairePackageReturnShape, SingleCoverageConstraint,
// AutoOriginSourceCode, IntendedUseCodeSystem) and, where the def is silent,
// from a live diff of the pinned DTR 2.0.1/2.1.0/2.2.0 package differentials
// (2026-08-12, packages.simplifier.net/hl7.fhir.us.davinci-dtr) — never
// invented (transform-iff). Each step's own doc comment below carries the
// per-direction derivation this file implements.
package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// DTR extension URLs — ported byte-for-byte from sdk/dtr.go so the QR wire
// shape matched here is identical (sdk does not export these). Duplicated
// (not imported) rather than re-derived, same rationale as
// transform_pas.go's pasCorrelationSystem/pasBundleIdentifierSystem: these
// are literal citations of the existing wire contract, not a new invention.
const (
	dtrQRContextExt         = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context"
	dtrQRCoverageExt        = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage"
	dtrIntendedUseExt       = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse"
	dtrInformationOriginExt = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/information-origin"

	// dtrItemWeightExt is the core-FHIR itemWeight extension DTR 2.2 references.
	// DTR 2.2.0's dtr-questionnaireresponse differential declares a slice at
	// item.answer.extension:itemWeight, but the extension's own SD contexts it
	// to item.answer.VALUE (and Coding) — see dtrItemWeightLocus. The engine
	// reads the SD, not the differential: a profile cannot widen an extension's
	// context, so the differential's answer-level slice is unsatisfiable on the
	// wire and no conformant payload can use it.
	//
	// (StructureDefinition-dtr-questionnaireresponse.json's differential,
	// package 2.2.0, sliceName "itemWeight" — verified live 2026-08-12) —
	// genuinely ABSENT from the 2.1.0 differential (2.1's item.answer slot at
	// that position is "ordinalValue", itself never built at any line per
	// linedef.go's DTRDef "NOT a field" comment). The canonical URL is the
	// base (unversioned) http://hl7.org/fhir/StructureDefinition/itemWeight —
	// cited already at linedef.go:181 — the |5.3.0-ballot-tc1 suffix in the
	// package's own type.profile pin is a PROFILE reference, never present on
	// the wire (same unversioned-instance-URL convention transform_pas.go's
	// pasStep2021Down doc establishes for extension.url). SHN's own
	// FillQuestionnaire never stamps itemWeight (no honest per-answer weight
	// source — linedef.go's DTRDef doc), but a FOREIGN 2.2 payload MAY carry
	// one; the loss policy's never-silently-drop invariant covers forwarded
	// content SHN doesn't itself produce, exactly like PAS's
	// authorizationNumber (transform_pas.go's pas22OnlyClaimExtensions).
	dtrItemWeightExt = "http://hl7.org/fhir/StructureDefinition/itemWeight"

	// dtrItemWeightLocus is the class-level locus every itemWeight LossEntry,
	// carry wrapper path, and error string in this file names — one constant so
	// they cannot drift apart. It is answer.VALUE, not answer: the itemWeight
	// extension is core FHIR and its SD contexts it to Coding /
	// Questionnaire.item.answerOption / QuestionnaireResponse.item.answer.value.
	// A profile cannot widen an extension's context and the validator enforces
	// the context, so DTR 2.2.0's answer-level slice is unsatisfiable on the
	// wire — an upstream IG defect — and the engine reads the SD, not the
	// differential. Verified against the pinned validator: an itemWeight at
	// item.answer.extension is a context ERROR at the 2.2 line, and the same
	// element under the answer's value validates clean.
	dtrItemWeightLocus = "QuestionnaireResponse.item.answer.value.extension:itemWeight"
)

// TransformDTRForTest is a thin exported wrapper around the pa.dtr compat
// chain (chainFor + applyChain) for the test/conformance package — mirrors
// transform_pas.go's TransformPASForTest (same cross-module-boundary
// rationale: a DIFFERENT Go module cannot see engine-internal symbols).
func TransformDTRForTest(from, to string, payload []byte, x ExchangeIdentity) ([]byte, []LossReport, error) {
	steps := chainFor("pa.dtr", from, to)
	if steps == nil {
		return nil, nil, fmt.Errorf("engine: TransformDTRForTest: no pa.dtr chain %s->%s", from, to)
	}
	return applyChain(steps, from, payload, x)
}

// ---------------------------------------------------------------------------
// pa.dtr 2.0 <-> 2.1 — IDENTITY (compat.go's row: Class full, nil Up/Down).
// ---------------------------------------------------------------------------
//
// No functions live here: this adjacency's row (compat.go) intentionally
// carries nil Up/Down, matching the pa.crd rows' pattern — an identity
// CompatStep is genuine, verified content (applyChain's nil-fn branch is a
// pure byte-for-byte pass-through), not a placeholder. The justification,
// re-derived live 2026-08-12 against the pinned DTR 2.0.1/2.1.0 packages
// (an identity row is a MANIFEST CLASS claim, not a convenience default):
//
//   - DTRDef.QuestionnairePackageReturnShape differs by NAME ("unconstrained"
//     at 2.0 vs "qr-optional" at 2.1) but is tolerant BOTH WAYS for every
//     shape SHN's own $questionnaire-package responder emits: 2.0.1's
//     StructureDefinition-DTR-QPackageBundle.json differential declares NO
//     Bundle.entry slicing at all (verified live — only
//     Bundle/Bundle.type appear), so a 2.1-shaped package (which DOES add a
//     Bundle.entry:questionnaireResponse slice, min=0 max=1) downcasts to 2.0
//     unchanged and still validates (an unconstrained profile permits
//     anything a more-constrained one permits). Going UP, a 2.0 package with
//     no QuestionnaireResponse entry (testdata/golden/questionnaire-package-
//     pa-lumbar-mri.json — Questionnaire only) satisfies 2.1's min=0, and a
//     2.0 package that DOES carry one (a shape 2.0's "unconstrained" profile
//     already permitted) satisfies 2.1's max=1 the same way. Both directions
//     verified on the real golden pair (TestDTRPackageChain2020To21Identity).
//   - The QuestionnaireResponse WIRE CONTENT itself is genuinely
//     byte-identical at 2.0/2.1 for every field DTRDef tracks:
//     SingleCoverageConstraint false at both, AutoOriginSourceCode "auto" at
//     both, IntendedUseCodeSystem crdTempCodeSystem at both — confirmed on
//     bytes by testdata/golden/questionnaireresponse-autofill.json ==
//     testdata/golden/2.1/questionnaireresponse-autofill.json (per_line_uc_
//     test.go:389-407's pre-existing record; re-verified here directly,
//     TestDTRQRChain2020To21Identity). The 2.1.0-only optional slices the
//     live SD diff surfaces beyond DTRDef's tracked fields —
//     extension:coverage-information (min=0 max=*) and
//     item.answer.extension:containedReference (min=0 max=1), both absent
//     from 2.0.1's differential — are NOT built by any SHN producer (grep
//     confirms no reference); per produce-iff (transform_pas.go's addItem/
//     supportingInfo precedent) they are deliberately not modeled: identity
//     is the honest content for what this adjacency's real traffic carries,
//     and re-added the moment a producer or consumer materializes for either.

// ---------------------------------------------------------------------------
// pa.dtr 2.1 <-> 2.2
// ---------------------------------------------------------------------------

// dtrStep2122Up bridges a 2.1-shaped DTR payload up to 2.2. Two document
// shapes flow through this contract (a bare QuestionnaireResponse, or a
// $questionnaire-package response Bundle embedding one) — dtrCollectResources
// finds the QuestionnaireResponse(s) either way and this one Up func handles
// both.
//
//   - package-shape gate: DTR-QPackageBundle's Bundle.entry:questionnaireResponse
//     is min=1 max=1 at 2.2.0 ("qr-required" — DTRDef.QuestionnairePackageReturnShape,
//     verified live against the package differential), min=0 max=1 at 2.1.0.
//     A package Bundle (has a Questionnaire entry) with NO QuestionnaireResponse
//     entry has no honest content to mint one from — the module NEVER
//     fabricates a QR (the responder's zero-answer shell is the native fix: a real
//     responder must supply one at 2.2). Refuses (gated, typed
//     SemanticChangeError).
//   - coverage relocation: DTR 2.2.0 gains a dedicated, required (min=1
//     max=*) extension:coverage slice (StructureDefinition-qr-coverage.json,
//     url dtrQRCoverageExt — DTRDef.SingleCoverageConstraint); at 2.1 the
//     same reference rides the shared qr-context slice alongside the order
//     reference (distinguished only by reference TYPE — "Coverage/..." vs
//     "ServiceRequest/..." — the convention sdk/dtr.go's QRContext already
//     uses). Exactly one Coverage-referencing qr-context entry relocates
//     in place (full — same array index, url rewritten dtrQRContextExt ->
//     dtrQRCoverageExt, matching the golden pair's identical extension
//     ordering). Zero or more-than-one Coverage-referencing entries has no
//     single honest source for the now-required, singular-by-convention
//     qr-coverage move — refuses (gated, typed SemanticChangeError; the
//     multi-coverage case is the spec's canonical semantic-change refusal,
//     TestDTRStep2122Up_MultiCoverageGated).
//   - intendedUse system move: DocReason's "withpa" concept moved CodeSystem
//     (CRD 2.1.0's …/CodeSystem/temp -> CRD 2.2.1's …/CodeSystem/
//     coverage-information-codes — DTRDef.IntendedUseCodeSystem, verified
//     live: CodeSystem-temp no longer defines "withpa" at 2.2.1). Rewrites
//     the intendedUse coding's system only when it equals the SOURCE line's
//     system (never clobbers a foreign QR already on the target system) —
//     full, deterministic, def-driven (never a hardcoded literal pair).
//   - origin code move: the "auto" CQL/EHR-auto-populated source code
//     (CodeSystem/temp) is retired at DTR 2.2.0 in favor of "auto-client"
//     under the renamed CodeSystem/dtr-informationorigin-codes
//     (DTRDef.AutoOriginSourceCode; the value-set binding also tightens
//     extensible->required — verified live). Rewrites only answer.extension
//     source codes matching the SOURCE line's code — "manual"/"override"/
//     other origin codes (clinician/patient-authored items) are
//     line-invariant and left untouched (verified on
//     testdata/golden/{2.1,2.2}/questionnaireresponse-attested.json, whose
//     functional-status-oswestry item keeps source="manual" unchanged
//     across both lines while the auto items move auto->auto-client).
//   - restores any shn-carried-content wrapper a prior 2.2->2.1 Down step
//     carried (item.answer.value.extension:itemWeight — the other half of the
//     carry mechanism, sdk/carry.go's Restore(Carry(x))==x contract).
func dtrStep2122Up(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
	top, err := dtrParseTop(payload)
	if err != nil {
		return nil, LossReport{}, err
	}
	resources := dtrCollectResources(top)

	def21, ok := shnsdk.DTRLineDef("2.1")
	if !ok {
		return nil, LossReport{}, fmt.Errorf("engine: dtrStep2122Up: no DTRLineDef for 2.1")
	}
	def22, ok := shnsdk.DTRLineDef("2.2")
	if !ok {
		return nil, LossReport{}, fmt.Errorf("engine: dtrStep2122Up: no DTRLineDef for 2.2")
	}

	if rt, _ := top["resourceType"].(string); rt == "Bundle" &&
		len(resources["Questionnaire"]) > 0 && len(resources["QuestionnaireResponse"]) == 0 {
		if def22.QuestionnairePackageReturnShape == "qr-required" {
			return nil, LossReport{}, &SemanticChangeError{
				Contract:        "pa.dtr",
				From:            "2.1",
				To:              "2.2",
				Direction:       "up",
				MissingElements: []string{"Bundle.entry:questionnaireResponse"},
			}
		}
	}

	for _, qr := range resources["QuestionnaireResponse"] {
		if err := dtrRestoreItemWeight(qr); err != nil {
			return nil, LossReport{}, fmt.Errorf("engine: dtrStep2122Up: restore %s: %w", dtrItemWeightLocus, err)
		}
		if err := dtrRelocateCoverageUp(qr, "2.1", "2.2"); err != nil {
			return nil, LossReport{}, err
		}
		dtrRemapIntendedUseSystem(qr, def21.IntendedUseCodeSystem, def22.IntendedUseCodeSystem)
		dtrRemapOriginCode(qr, def21.AutoOriginSourceCode, def22.AutoOriginSourceCode)
	}

	out, err := json.Marshal(top)
	if err != nil {
		return nil, LossReport{}, fmt.Errorf("engine: dtrStep2122Up: marshal: %w", err)
	}
	return out, LossReport{Module: "pa.dtr 2.1->2.2", Source: "2.1", Target: "2.2"}, nil
}

// dtrStep2122Down bridges a 2.2-shaped DTR payload down to 2.1 — the mirror
// of dtrStep2122Up: coverage/system/origin moves reversed (full — 2.1
// tolerates a QR entry present in a package Bundle even though its own
// profile only requires min=0, and its qr-context slice's cardinality,
// min=2 unbounded max, tolerates one or more relocated coverage entries), and
// any 2.2-only item.answer.value.extension:itemWeight carried into
// shn-carried-content (no 2.1 slot — dtrItemWeightExt's doc comment).
func dtrStep2122Down(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
	top, err := dtrParseTop(payload)
	if err != nil {
		return nil, LossReport{}, err
	}
	resources := dtrCollectResources(top)

	def21, ok := shnsdk.DTRLineDef("2.1")
	if !ok {
		return nil, LossReport{}, fmt.Errorf("engine: dtrStep2122Down: no DTRLineDef for 2.1")
	}
	def22, ok := shnsdk.DTRLineDef("2.2")
	if !ok {
		return nil, LossReport{}, fmt.Errorf("engine: dtrStep2122Down: no DTRLineDef for 2.2")
	}

	var carried []LossEntry
	for _, qr := range resources["QuestionnaireResponse"] {
		dtrRelocateCoverageDown(qr)
		dtrRemapIntendedUseSystem(qr, def22.IntendedUseCodeSystem, def21.IntendedUseCodeSystem)
		dtrRemapOriginCode(qr, def22.AutoOriginSourceCode, def21.AutoOriginSourceCode)
		entries, err := dtrCarryItemWeight(qr)
		if err != nil {
			return nil, LossReport{}, fmt.Errorf("engine: dtrStep2122Down: carry %s: %w", dtrItemWeightLocus, err)
		}
		carried = append(carried, entries...)
	}

	out, err := json.Marshal(top)
	if err != nil {
		return nil, LossReport{}, fmt.Errorf("engine: dtrStep2122Down: marshal: %w", err)
	}
	return out, LossReport{Module: "pa.dtr 2.2->2.1", Source: "2.2", Target: "2.1", Carried: carried}, nil
}

// ---------------------------------------------------------------------------
// QuestionnaireResponse element-level helpers
// ---------------------------------------------------------------------------

// dtrRelocateCoverageUp finds the qr-context extension entries in qr whose
// valueReference targets a Coverage (reference prefix "Coverage/" — the
// convention sdk/dtr.go's QRContext.CoverageRef/dtrQRContextExtensions
// already establishes) and relocates the SINGLE unambiguous match's url to
// dtrQRCoverageExt IN PLACE (same array index — matches the golden fixtures'
// extension ordering exactly, so this reads as a true full mapping, not a
// remove+append). Zero matches (no honest source for the now-required
// qr-coverage) or more than one (ambiguous which is authoritative — DTR 2.1
// carries no per-entry discriminator beyond reference type) both refuse via
// the typed SemanticChangeError, naming the constraint.
//
// Refusing on ambiguity rather than relocating ALL matches (which 2.2's
// qr-coverage cardinality, min=1 max=*, would technically accept) is
// deliberate, not just SD-driven caution: SHN's own QR construction
// (sdk/dtr.go's QRContext.CoverageRef, singular — dtrQRContextExtensions
// stamps exactly one coverage reference per QR) never produces a
// multi-coverage QR, so a source with >=2 Coverage-referencing entries is
// necessarily FOREIGN content this module has no basis to interpret —
// silently relocating all of them would assert a "these N coverages are
// jointly the subject's coverage-information" reading the source payload
// never actually declared.
func dtrRelocateCoverageUp(qr map[string]any, from, to string) error {
	extAny, _ := qr["extension"].([]any)
	matchIdx := -1
	count := 0
	for i, e := range extAny {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if url, _ := em["url"].(string); url != dtrQRContextExt {
			continue
		}
		ref, _ := em["valueReference"].(map[string]any)
		r, _ := ref["reference"].(string)
		if strings.HasPrefix(r, "Coverage/") {
			count++
			matchIdx = i
		}
	}
	switch {
	case count == 0:
		return &SemanticChangeError{
			Contract: "pa.dtr", From: from, To: to, Direction: "up",
			MissingElements: []string{"QuestionnaireResponse.extension:qr-coverage (no Coverage-referencing qr-context entry found)"},
		}
	case count > 1:
		return &SemanticChangeError{
			Contract: "pa.dtr", From: from, To: to, Direction: "up",
			MissingElements: []string{fmt.Sprintf("QuestionnaireResponse.extension:qr-coverage (ambiguous: %d Coverage-referencing qr-context entries, multi-coverage source)", count)},
		}
	}
	extAny[matchIdx].(map[string]any)["url"] = dtrQRCoverageExt
	return nil
}

// dtrRelocateCoverageDown is dtrRelocateCoverageUp's mirror: every
// dtrQRCoverageExt entry's url is rewritten back to dtrQRContextExt in
// place. Always succeeds (full) — 2.1's qr-context slice (min=2, unbounded
// max) tolerates one or more relocated entries the same way it always has.
func dtrRelocateCoverageDown(qr map[string]any) {
	extAny, _ := qr["extension"].([]any)
	for _, e := range extAny {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if em["url"] == dtrQRCoverageExt {
			em["url"] = dtrQRContextExt
		}
	}
}

// dtrRemapIntendedUseSystem rewrites the QuestionnaireResponse-level
// intendedUse extension's coding[].system from "from" to "to", but ONLY when
// the coding's current system equals "from" — never clobbers a foreign QR
// already carrying the target system (idempotent/re-entrant safe, same
// posture as transform_pas.go's ClaimResponse.request synthesis guard).
func dtrRemapIntendedUseSystem(qr map[string]any, from, to string) {
	extAny, _ := qr["extension"].([]any)
	for _, e := range extAny {
		em, ok := e.(map[string]any)
		if !ok || em["url"] != dtrIntendedUseExt {
			continue
		}
		cc, ok := em["valueCodeableConcept"].(map[string]any)
		if !ok {
			continue
		}
		codings, _ := cc["coding"].([]any)
		for _, c := range codings {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if cm["system"] == from {
				cm["system"] = to
			}
		}
	}
}

// dtrWalkAnswers calls fn for every answer in the subtree under node, at every
// nesting depth. node may be a QuestionnaireResponse, an item, or an answer —
// all three can hold an "item" array, which is why one function covers both of
// FHIR's recursion axes:
//
//	item.item        — a group item's child questions
//	item.answer.item — an answer's child questions
//
// Both carry contentReference back to QuestionnaireResponse.item, so a nested
// item's answer is the SAME element as a top-level one and every rule that
// applies to one applies to the other.
//
// SHARED by all three QR walkers for the reason dtrAnswerValueContainers is
// shared by carry and restore: they must not be able to disagree about where an
// answer is, or a round trip can lose an element by restoring somewhere the
// carry never looked.
//
// Deliberately NOT depth-capped. A cap would silently stop carrying below it,
// which is the exact silent-loss shape this walker exists to close; recursion
// depth is bounded by the document, which the caller has already fully decoded
// into maps before the walk begins.
func dtrWalkAnswers(node map[string]any, fn func(answer map[string]any)) {
	items, _ := node["item"].([]any)
	for _, it := range items {
		im, ok := it.(map[string]any)
		if !ok {
			continue
		}
		answers, _ := im["answer"].([]any)
		for _, a := range answers {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			fn(am)
			dtrWalkAnswers(am, fn)
		}
		dtrWalkAnswers(im, fn)
	}
}

// dtrRemapOriginCode rewrites the source sub-extension's valueCode from
// "from" to "to" on every QuestionnaireResponse.item.answer information-
// origin extension, but ONLY where the current code equals "from" —
// "manual"/"override"/other origin codes (clinician- or patient-authored
// items) are line-invariant and left untouched (verified on the
// questionnaireresponse-attested.json golden pair: the auto items move,
// the manual-sourced functional-status-oswestry item does not).
//
// Walks every answer at every nesting depth (dtrWalkAnswers): FHIR nests
// QuestionnaireResponse.item on two axes and a nested item's answer is the same
// element as a top-level one.
func dtrRemapOriginCode(qr map[string]any, from, to string) {
	dtrWalkAnswers(qr, func(am map[string]any) {
		extAny, _ := am["extension"].([]any)
		for _, e := range extAny {
			em, ok := e.(map[string]any)
			if !ok || em["url"] != dtrInformationOriginExt {
				continue
			}
			subAny, _ := em["extension"].([]any)
			for _, s := range subAny {
				sm, ok := s.(map[string]any)
				if !ok || sm["url"] != "source" {
					continue
				}
				if sm["valueCode"] == from {
					sm["valueCode"] = to
				}
			}
		}
	})
}

// dtrAnswerValueContainers returns the extension-bearing containers for an
// answer's value[x] — the locus an extension contexted to
// QuestionnaireResponse.item.answer.value must occupy.
//
// FHIR encodes that one locus two ways, and both are real itemWeight contexts
// (the SD names Coding explicitly alongside the answer.value expression):
//
//   - complex value[x] (valueCoding, valueQuantity, …): extensions live on the
//     value object itself.
//   - primitive value[x] (valueInteger, valueDecimal, …): a JSON primitive
//     cannot carry children, so FHIR puts them on the sibling "_value[x]".
//
// SHARED by carry and restore on purpose: the two must agree exactly about what
// "the answer's value" means, or a round trip can lose an element by restoring
// somewhere the carry never looks — which is what makes restore strict rather
// than relocating (see dtrRestoreItemWeight).
//
// NEVER CREATES a missing container — this is a read-side walker for both
// directions. (tools/xmatrix/inject.go's valueOf, which models the same locus,
// does create one; it is an injector, not a walker.)
//
// The key set is SORTED before use: Go map iteration order is randomized, and a
// conformant answer has exactly one value[x], but a malformed one with two must
// still walk deterministically — flake is a bug, not something to retry.
func dtrAnswerValueContainers(answer map[string]any) []map[string]any {
	seen := map[string]bool{}
	var names []string
	note := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for k, v := range answer {
		switch {
		case dtrIsValueChoiceKey(k):
			if _, isObj := v.(map[string]any); isObj {
				note(k) // complex value[x]: extensions on the value object
			} else {
				note("_" + k) // primitive value[x]: extensions on the sibling
			}
		case strings.HasPrefix(k, "_") && dtrIsValueChoiceKey(k[1:]):
			// A "_value[x]" with no value[x] beside it is legal FHIR (an absent
			// value that still carries extensions), so it is reachable without
			// the sibling lookup above.
			note(k)
		}
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		if m, ok := answer[n].(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// dtrIsValueChoiceKey reports whether k is a FHIR value[x] choice key —
// "value" followed by an uppercase-led type name. The uppercase requirement is
// load-bearing: it keeps real field names that merely begin with "value" out.
// (Mirrored, with the same rationale, by tools/xmatrix/locus.go's
// valueChoiceKey regexp, which folds the same keys onto the grid's "value"
// segment.)
func dtrIsValueChoiceKey(k string) bool {
	// Spelled as an explicit set rather than a byte-range comparison so the
	// ASCII bound is visible: FHIR datatype names are ASCII by definition, and
	// the xmatrix mirror's [A-Z] means exactly these 26 bytes.
	const asciiUpper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const prefix = "value"
	if !strings.HasPrefix(k, prefix) || len(k) == len(prefix) {
		return false
	}
	return strings.IndexByte(asciiUpper, k[len(prefix)]) >= 0
}

// dtrCarryItemWeight scans every answer's VALUE container(s)
// (dtrAnswerValueContainers) for dtrItemWeightExt entries, replacing each with
// an shn-carried-content wrapper (sdk/carry.go's CarryElement) IN PLACE and
// returning one LossEntry per element carried. A no-op (nil, nil) when none are
// found — most QRs never carry one (SHN's own FillQuestionnaire never stamps
// it, dtrItemWeightExt's doc comment).
//
// In place is the design, not an implementation detail: the wrapper's position
// is what tells dtrRestoreItemWeight which JSON encoding of answer.value the
// element came from, so the class-level LossEntry.Path never has to.
//
// answer.extension is deliberately NOT scanned. DTR 2.2.0's profile declares a
// slice there, but the itemWeight SD's context forbids it, so no conformant
// payload can use it — see dtrItemWeightLocus.
//
// Walks every answer at every nesting depth (dtrWalkAnswers): FHIR nests
// QuestionnaireResponse.item on two axes and a nested item's answer is the same
// element as a top-level one.
func dtrCarryItemWeight(qr map[string]any) ([]LossEntry, error) {
	var carried []LossEntry
	var walkErr error
	dtrWalkAnswers(qr, func(am map[string]any) {
		if walkErr != nil {
			return
		}
		for _, container := range dtrAnswerValueContainers(am) {
			entries, err := dtrCarryItemWeightIn(container)
			if err != nil {
				walkErr = err
				return
			}
			carried = append(carried, entries...)
		}
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return carried, nil
}

// dtrCarryItemWeightIn is dtrCarryItemWeight's per-container half: it rewrites
// one extension array, swapping each itemWeight entry for its wrapper at the
// SAME index and leaving every other entry untouched.
func dtrCarryItemWeightIn(container map[string]any) ([]LossEntry, error) {
	extAny, _ := container["extension"].([]any)
	if len(extAny) == 0 {
		return nil, nil
	}
	var carried []LossEntry
	kept := make([]any, 0, len(extAny))
	for _, e := range extAny {
		em, ok := e.(map[string]any)
		if !ok {
			kept = append(kept, e)
			continue
		}
		if url, _ := em["url"].(string); url != dtrItemWeightExt {
			kept = append(kept, e)
			continue
		}
		raw, err := json.Marshal(em)
		if err != nil {
			return nil, fmt.Errorf("marshal %s: %w", dtrItemWeightLocus, err)
		}
		carriedExt, err := shnsdk.CarryElement(dtrItemWeightLocus, raw, "2.2")
		if err != nil {
			return nil, fmt.Errorf("carry %s: %w", dtrItemWeightLocus, err)
		}
		var carriedExtAny any
		if err := json.Unmarshal(carriedExt, &carriedExtAny); err != nil {
			return nil, fmt.Errorf("decode carried %s: %w", dtrItemWeightLocus, err)
		}
		kept = append(kept, carriedExtAny)
		carried = append(carried, LossEntry{
			Path:   dtrItemWeightLocus,
			Detail: "carried; source line 2.2 (no 2.1 slot)",
		})
	}
	container["extension"] = kept
	return carried, nil
}

// dtrRestoreItemWeight is dtrCarryItemWeight's inverse, walking the SAME
// containers (dtrAnswerValueContainers) so the two cannot disagree about where
// a carried element lives. Each wrapper is replaced by its element at the same
// array index, which is why neither side needs to record or recover the JSON
// encoding of answer.value.
//
// Includes dtrCarryItemWeight's byte-fidelity discipline: the restored element
// is pushed back as json.RawMessage (never decoded-then-Go-value), so
// Restore(Carry(x))==x holds even nested inside a larger re-marshaled document
// (transform_pas.go:501-511's comment explains why this matters).
//
// STRICT: a wrapper sitting at answer.extension — what an engine that read the
// profile differential instead of the extension's SD produced, reachable only
// in a split-version round trip across a rolling upgrade — is left wrapped
// rather than unwrapped there (which would emit a payload the 2.2 line rejects
// on the extension's context) or relocated (which would silently move content
// relative to what was carried). Leaving it wrapped loses nothing:
// shn-carried-content's own context is Element, so it validates where it sits.
//
// Walks every answer at every nesting depth (dtrWalkAnswers): FHIR nests
// QuestionnaireResponse.item on two axes and a nested item's answer is the same
// element as a top-level one.
func dtrRestoreItemWeight(qr map[string]any) error {
	var walkErr error
	dtrWalkAnswers(qr, func(am map[string]any) {
		if walkErr != nil {
			return
		}
		for _, container := range dtrAnswerValueContainers(am) {
			if err := dtrRestoreItemWeightIn(container); err != nil {
				walkErr = err
				return
			}
		}
	})
	return walkErr
}

// dtrRestoreItemWeightIn is dtrRestoreItemWeight's per-container half.
func dtrRestoreItemWeightIn(container map[string]any) error {
	extAny, _ := container["extension"].([]any)
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
		kept = append(kept, json.RawMessage(element))
	}
	container["extension"] = kept
	return nil
}

// ---------------------------------------------------------------------------
// generic JSON-document helpers — duplicated from transform_pas.go's
// pasParseTop/pasCollectResources (module self-containment, same rationale
// as the constants above) rather than cross-imported.
// ---------------------------------------------------------------------------

// dtrParseTop decodes the top-level DTR payload (a bare QuestionnaireResponse
// or a $questionnaire-package response Bundle) into a mutable map. Fails
// loudly on invalid JSON — never returns a zero-value success.
func dtrParseTop(payload []byte) (map[string]any, error) {
	var top map[string]any
	if err := json.Unmarshal(payload, &top); err != nil {
		return nil, fmt.Errorf("engine: dtr transform: invalid JSON: %w", err)
	}
	return top, nil
}

// dtrCollectResources returns every resource found in the document, keyed by
// FHIR resourceType: the document itself when it is a bare resource, or each
// Bundle.entry[].resource when the document is a Bundle. The returned maps
// are the SAME map values referenced by top (Go maps are reference types) —
// mutating a returned map mutates the document, so callers edit fields
// directly and re-marshal top to get the transformed payload back.
func dtrCollectResources(top map[string]any) map[string][]map[string]any {
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
