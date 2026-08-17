// compat.go — the compatibility manifest: a Go table
// consulted at runtime inside a deployed binary, NEVER JSON. This is
// deliberately NOT tools/contracts/manifest.json — that file is IG package-pin
// text consumed by contractsgen's KV/curl-site machinery (a repo-file read
// cannot serve a routing filter inside a running process, and the parser must
// never see unrelated content — the "hapi.fhir.implementationguides.<key>.
// <field>=" block-indexing gotcha, hit twice in practice).
//
// compatManifest carries one row per ADJACENT line pair per contract (the
// Stripe model: N-1 modules bridge N lines). chainFor is the pure path-walk
// over that table; ranking chains for selection (full beats carry beats
// gated, fewer steps beat more, ties broken by highest source line) lives in
// selectChain — chainFor only answers "is there a path, and what is
// it", never "which path is best".
package engine

import (
	"sort"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// compatManifest is the compatibility table. CRD rows ship fully wired
// (CRDDef has zero behavioral delta across 2.0/2.1/2.2 — verified live —
// so identity/full with nil Up/Down is the correct, non-speculative
// content, not a placeholder: transform-iff forbids dead no-op modules for
// deltas no package requires). PAS/DTR rows carry their Class (derived from a
// live delta analysis of the pinned packages) and their real Up/Down functions;
// that same derivation is the VERIFICATION authority for these
// classes — re-derived live against the real package SDs before wiring, and a
// class noted here can change if that derivation refutes it. Every row is
// wired — the CompatStep.wired exemption field has been
// removed (see transform.go's CompatStep doc), so
// TestCompatManifestCoversAdjacentNativeSteps's nil-Up/Down-implies-full
// check now applies unconditionally to every row.
var compatManifest = []CompatStep{
	// --- pa.crd: identity across all three lines (verified live, zero
	// behavioral delta in CRDDef) — wired now, nil Up/Down is the real content.
	// Re-confirmation (2026-08-12, live re-derivation against
	// packages.simplifier.net/hl7.fhir.us.davinci-crd/{2.0.1,2.1.0,2.2.1}'s
	// StructureDefinition-ext-coverage-information.json differentials): the
	// FULL extension does drift across lines (doc-needed widens 0..1->0..*
	// after 2.0.1, a 2.0.1-only "response" slice disappears, "expiry-date"
	// and, at 2.2.1, "detail.extension:category" are added) — but every
	// delta lands OUTSIDE the four sub-extensions sdk/crd.go's
	// BuildCardsAtLine actually builds (covered/pa-needed/questionnaire/
	// satisfied-pa-id), which stay min/max-IDENTICAL across all three STUs.
	// Zero behavioral delta HOLDS for what CRD's own producer/consumer touch
	// (produce-iff) — the identity row is confirmed, not just carried over.
	{Contract: "pa.crd", From: "2.0", To: "2.1", Class: StepFull},
	{Contract: "pa.crd", From: "2.1", To: "2.2", Class: StepFull},

	// --- pa.dtr: Up/Down are wired (transform_dtr.go); their live derivation
	// is the verification authority for both classes below.
	//
	// 2.0<->2.1: IDENTITY, re-derived live 2026-08-12 against
	// packages.simplifier.net/hl7.fhir.us.davinci-dtr/{2.0.1,2.1.0} as a
	// MANIFEST CLASS claim, not carried over as a
	// convenience default. DTRDef.QuestionnairePackageReturnShape differs by
	// NAME ("unconstrained" vs "qr-optional") but StructureDefinition-DTR-
	// QPackageBundle.json's differential shows 2.0.1 declares NO Bundle.entry
	// slicing at all — an unconstrained profile that tolerates whatever
	// 2.1.0's min=0/max=1 Bundle.entry:questionnaireResponse slice permits,
	// BOTH directions (verified on the real golden pair, transform_dtr_test.go's
	// TestDTRPackageChain2020To21Identity). The QuestionnaireResponse WIRE
	// CONTENT is genuinely byte-identical at 2.0/2.1 for every DTRDef-tracked
	// field (SingleCoverageConstraint/AutoOriginSourceCode/IntendedUseCodeSystem
	// all equal) — confirmed on bytes (questionnaireresponse-autofill.json
	// goldens, TestDTRQRChain2020To21Identity). See transform_dtr.go's header
	// comment for the full citation, including the 2.1.0-only optional slices
	// (extension:coverage-information, item.answer.extension:containedReference)
	// the live diff surfaces but that no SHN producer/consumer touches
	// (produce-iff — not modeled, same as PAS's addItem/supportingInfo).
	{Contract: "pa.dtr", From: "2.0", To: "2.1", Class: StepFull},
	// 2.1<->2.2, per-direction truth (worst-of collapses to the row Class,
	// per D1b's ranking — the gates below are narrow edge-case refusals on
	// foreign/anomalous traffic, not this adjacency's typical shape):
	//   Up (2.1->2.2), QR content, single Coverage-referencing qr-context
	//     entry:  FULL — dtrStep2122Up relocates it in place (qr-coverage),
	//     plus the def-driven intendedUse-system and origin-code moves.
	//   Up (2.1->2.2), QR content, MULTI-coverage source (>=2
	//     Coverage-referencing qr-context entries):  GATED — semantic-change
	//     refusal (the canonical multi-coverage example), typed SemanticChangeError;
	//     rejection-tested (TestDTRStep2122Up_MultiCoverageGated).
	//   Up (2.1->2.2), QR content, ZERO-coverage source (no
	//     Coverage-referencing qr-context entry): GATED — same typed error,
	//     symmetric defensive case (no honest source for the now-required
	//     qr-coverage min=1); tested (TestDTRStep2122Up_NoCoverageGated).
	//   Up (2.1->2.2), package-shape, QR-less package:  GATED — 2.2's
	//     qr-required shape (DTRDef.QuestionnairePackageReturnShape) has no
	//     honest QR to mint; the responder's zero-answer QR shell is the
	//     native fix, this module never fabricates.
	//   Down (2.2->2.1):  FULL for QR content (moves reversed — 2.1's
	//     qr-context slice, min=2 unbounded max, tolerates one or more
	//     relocated entries) + CARRY for the one genuine 2.2-only element
	//     with no 2.1 slot (item.answer.extension:itemWeight —
	//     StructureDefinition-dtr-questionnaireresponse.json's 2.2.0
	//     differential; ABSENT at 2.1.0, whose same slot is the never-built
	//     "ordinalValue") into shn-carried-content.
	//   Row worst-of = CARRY (matches the row Class below) — confirmed at
	//   wiring time.
	{Contract: "pa.dtr", From: "2.1", To: "2.2", Class: StepCarry, Up: dtrStep2122Up, Down: dtrStep2122Down},

	// --- pa.pas: Up/Down are wired; their live derivation is the verification
	// authority for both classes below (re-derived live 2026-08-12 against the pinned PAS
	// 2.0.1/2.1.0/2.2.1 package differentials — see transform_pas.go's step
	// doc comments for the per-direction citations).
	//
	// 2.0<->2.1, per-direction truth (worse-of-two collapses to the row Class,
	// per the chain ranking — verified SAFE, not just conservative, for this row:
	// neither sub-case the collapse hides is itself a mandatory-refusal that
	// full/carry would wrongly let through):
	//   Up (2.0->2.1), request sub-case (Claim payload):  GATED — no honest
	//     source for certificationType/requestType/location[x]/relationship.
	//   Up (2.0->2.1), response sub-case (ClaimResponse):  FULL — request
	//     synthesized from correlation identity.
	//   Down (2.1->2.0):                                    FULL — drop
	//     nothing (2.0 tolerates every 2.1-mandatory element as optional;
	//     extension canonical URLs are byte-identical, unversioned, at both
	//     lines — verified live).
	//   Row worst-of-four = GATED (matches the row Class below).
	{Contract: "pa.pas", From: "2.0", To: "2.1", Class: StepGated, Up: pasStep2021Up, Down: pasStep2021Down},
	// 2.1<->2.2, per-direction truth:
	//   Up (2.1->2.2), request sub-case:   FULL — 2.1.0/2.2.1 byte-identical
	//     on every CLAIM-scoped element this adjacency touches (verified: the
	//     2.1/2.2 conformant-submit goldens' Claim entries are byte-identical;
	//     their embedded QuestionnaireResponse entries genuinely differ, but
	//     that is pa.dtr's own row — out of pa.pas's scope).
	//   Up (2.1->2.2), response sub-case:  FULL — Bundle.identifier
	//     synthesized (ResponseBundleIdentifierRequired), pended outcome
	//     queued->complete (PendedResponseOutcome, def-driven), carried
	//     content restored.
	//   Down (2.2->2.1), response sub-case: FULL — pended outcome
	//     complete->queued (def-driven); Bundle.identifier tolerated, not
	//     stripped (2.1's profile has no constraint on it at all).
	//   Down (2.2->2.1), request+response sub-cases: CARRY — top-level
	//     2.2-only extensions (authorizationNumber and siblings, verified
	//     live against the 2.2.1 SD — the canonical carry example) have no
	//     2.1 slot.
	//   Row worst-of-four = CARRY (matches the row Class below).
	{Contract: "pa.pas", From: "2.1", To: "2.2", Class: StepCarry, Up: pasStep2122Up, Down: pasStep2122Down},

	// pa.pdex has exactly one native line (2.1) — zero adjacent pairs, no rows
	// (tolerated rowless, pinned explicitly by TestCompatManifestCoversAdjacentNativeSteps).
}

// nativeContracts returns the distinct contract names (the part before "@")
// across shnsdk.NativeContractVersions(), sorted.
func nativeContracts() []string {
	seen := map[string]bool{}
	var out []string
	for _, tok := range shnsdk.NativeContractVersions() {
		contract, _, ok := strings.Cut(tok, "@")
		if !ok || contract == "" || seen[contract] {
			continue
		}
		seen[contract] = true
		out = append(out, contract)
	}
	sort.Strings(out)
	return out
}

// nativeLinesForContract returns one contract's native lines, in NUMERIC
// ascending order (compareLines, not sort.Strings — an explicit ordered-lines
// helper rather than relying on the "2.0" < "2.1" < "2.2" lexical accident).
func nativeLinesForContract(contract string) []string {
	var lines []string
	for _, tok := range shnsdk.NativeContractVersions() {
		c, line, ok := strings.Cut(tok, "@")
		if !ok || c != contract || line == "" {
			continue
		}
		lines = append(lines, line)
	}
	sort.Slice(lines, func(i, j int) bool { return compareLines(lines[i], lines[j]) < 0 })
	return lines
}

// manifestLinesForContract returns the distinct lines the manifest table
// actually has rows for, one contract, NUMERIC ascending order. This is the
// universe chainFor walks — deliberately independent of nativeLinesForContract
// so an unknown line (never appearing in any manifest row) is a clean nil,
// not an accidental match against the native set.
func manifestLinesForContract(contract string) []string {
	seen := map[string]bool{}
	for _, s := range compatManifest {
		if s.Contract != contract {
			continue
		}
		seen[s.From] = true
		seen[s.To] = true
	}
	lines := make([]string, 0, len(seen))
	for l := range seen {
		lines = append(lines, l)
	}
	sort.Slice(lines, func(i, j int) bool { return compareLines(lines[i], lines[j]) < 0 })
	return lines
}

// manifestStep looks up the single row bridging the adjacent pair (from, to)
// — from and to must be given low, high (From, To order); it does not search
// direction-reversed.
func manifestStep(contract, from, to string) (CompatStep, bool) {
	for _, s := range compatManifest {
		if s.Contract == contract && s.From == from && s.To == to {
			return s, true
		}
	}
	return CompatStep{}, false
}

// indexOfLine returns lines' index of line, or (-1, false) if absent.
func indexOfLine(lines []string, line string) (int, bool) {
	for i, l := range lines {
		if l == line {
			return i, true
		}
	}
	return -1, false
}

// chainFor returns the ordered steps bridging from→to for contract (either
// direction), nil if the manifest has a hole. Chain ranking lives in
// selectChain; chainFor is the pure path-walk.
func chainFor(contract, from, to string) []CompatStep {
	lines := manifestLinesForContract(contract)
	fi, fok := indexOfLine(lines, from)
	ti, tok := indexOfLine(lines, to)
	if !fok || !tok {
		return nil
	}
	if fi == ti {
		return []CompatStep{}
	}

	steps := make([]CompatStep, 0, abs(ti-fi))
	if fi < ti {
		for i := fi; i < ti; i++ {
			step, ok := manifestStep(contract, lines[i], lines[i+1])
			if !ok {
				return nil
			}
			steps = append(steps, step)
		}
	} else {
		for i := fi; i > ti; i-- {
			step, ok := manifestStep(contract, lines[i-1], lines[i])
			if !ok {
				return nil
			}
			steps = append(steps, step)
		}
	}
	return steps
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
