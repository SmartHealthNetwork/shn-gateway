// attestfence.go — the FR-16/FR-27 attestation conformance fence (R8 re-home). A
// QuestionnaireResponse item carrying the FR-17 source-attribution extension
// (dtrInformationOriginExt, source="manual") is either clinician-entered
// (author=Practitioner/…) or patient-reported (author=Patient/…): a
// clinician-entered item without a COMPLETE FR-16 clinician attestation
// extension (shnsdk.ClinicianAttestationExt: NPI + attestation text + date), or
// a patient-reported item without a COMPLETE FR-27 patient attestation
// extension (shnsdk.QRSignatureExt, the standard questionnaireresponse-
// signature: signature type + timestamp + signer + identity token), is
// nonconformant and rejected at the inbound gate. A system-sourced item (no
// manual-source marker at all) is untouched — no attestation required.
//
// COMPLETE is load-bearing: the ATTESTATION is the property, not the element.
// An extension carrying the right url but blank content attests nothing, so the
// fence reads the sub-elements FR-16/FR-27 mandate and requires each to be
// present AND non-empty. A url-only check would let an amend claim a clinician
// attestation with no NPI, no text and no date and still be adjudicated.
//
// This re-homes the retired in-process payer stub's "unattested item stays pended" policy fiction
// (DEF-4) into a real wire guard: FR-16/FR-27 are properties of any QR item, not
// only of amends, so the fence runs on BOTH the pas-claim and pas-claim-update
// entrances — the engine's inbound dispatch (inbound.go) and the provider-facing
// ingress (ingress.go) — and the standalone SDK Responder applies the identical
// check (sdk/responder.go, parity). It checks the SAME extension URLs the SDK
// builders (shnsdk.BuildManualAttestedItem / BuildPatientAttestedItem) write —
// dtrInformationOriginExt is the engine's own existing mirror of the SDK's
// unexported information-origin constant (already used by transform_dtr.go's
// dtrRemapOriginCode); ClinicianAttestationExt/QRSignatureExt are read directly
// off the SDK. No new constants.
package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// fenceAttestedItems walks EVERY QuestionnaireResponse entry in a conformant
// PAS Bundle (bundleJSON — the pas-claim / pas-claim-update request) and checks
// every item carrying the FR-17 manual-source information-origin extension: a
// clinician-entered item (author=Practitioner/…) must also carry a COMPLETE
// FR-16 clinician attestation extension; a patient-reported item
// (author=Patient/…) must also carry a COMPLETE FR-27 patient attestation
// extension — complete meaning every mandated sub-element is present and
// non-empty. System-sourced items
// (no manual-source marker) are untouched. Both conformant builders emit
// exactly one QR entry, so a bundle carrying a SECOND one only ever arises
// hand-built — exactly the door this fence exists for, so every entry is
// walked, not only the first. Returns ("", true) when every item in every
// entry conforms — including when the bundle carries no QuestionnaireResponse
// entry at all (optional on the submit leg, R-5) or every entry fails to parse
// (a malformed QR is the concern of the existing bundle-parse gates, not this
// fence). Returns a legible reason naming the failing FR, the linkId and the
// specific absent or empty field, and false, on the first nonconformant item
// found (entries checked in bundle order).
func fenceAttestedItems(bundleJSON []byte) (string, bool) {
	qrEntries, found := allQuestionnaireResponseEntries(bundleJSON)
	if !found {
		return "", true
	}
	for _, qrJSON := range qrEntries {
		var qr map[string]any
		if err := json.Unmarshal(qrJSON, &qr); err != nil {
			// A malformed QR is the concern of the existing bundle-parse
			// gates, not this fence — skip it and keep fencing the rest.
			continue
		}
		reason, ok := "", true
		walkFenceItems(qr, func(item map[string]any) bool {
			switch fenceItemOriginRole(item) {
			case "clinician":
				if defect := fenceClinicianAttestationDefect(item); defect != "" {
					reason = fmt.Sprintf("QuestionnaireResponse item %q is clinician-sourced (FR-17) but %s", fenceItemLinkID(item), defect)
					ok = false
					return false
				}
			case "patient":
				if defect := fencePatientAttestationDefect(item); defect != "" {
					reason = fmt.Sprintf("QuestionnaireResponse item %q is patient-reported (FR-17) but %s", fenceItemLinkID(item), defect)
					ok = false
					return false
				}
			}
			return true
		})
		if !ok {
			return reason, false
		}
	}
	return "", true
}

// allQuestionnaireResponseEntries returns every QuestionnaireResponse bundle
// entry's resource bytes, in bundle order, or found=false when the bundle is
// unparseable or carries none. Renamed from the single-entry
// firstQuestionnaireResponseEntry: fenceAttestedItems must walk every QR
// entry, not only the first, or a hand-built bundle can smuggle a second,
// unfenced entry straight past the FR-16/FR-27 gate. Parity with the
// published SDK's identical rename (sdk/responder.go).
func allQuestionnaireResponseEntries(bundleJSON []byte) (resources [][]byte, found bool) {
	var probe struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundleJSON, &probe); err != nil {
		return nil, false
	}
	for _, e := range probe.Entry {
		var rt struct {
			ResourceType string `json:"resourceType"`
		}
		if json.Unmarshal(e.Resource, &rt) == nil && rt.ResourceType == "QuestionnaireResponse" {
			resources = append(resources, e.Resource)
		}
	}
	return resources, len(resources) > 0
}

// walkFenceItems calls fn for every QuestionnaireResponse.item across BOTH FHIR
// nesting axes — item.item and item.answer.item, the same two loci
// dtrWalkAnswers (transform_dtr.go) walks for the identical reason: an item
// nested either way is the SAME element and must be fenced identically.
// Stops early (returns false) the first time fn does.
func walkFenceItems(node map[string]any, fn func(item map[string]any) bool) bool {
	items, _ := node["item"].([]any)
	for _, it := range items {
		im, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if !fn(im) {
			return false
		}
		if !walkFenceItems(im, fn) {
			return false
		}
		answers, _ := im["answer"].([]any)
		for _, a := range answers {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if !walkFenceItems(am, fn) {
				return false
			}
		}
	}
	return true
}

// fenceItemOriginRole reads item's answer-level FR-17 information-origin
// extension (dtrInformationOriginExt) and returns "clinician" when
// source="manual" and the author reference is a Practitioner, "patient" when it
// is a Patient, or "" for a system-sourced item (no manual-source extension at
// all, or a non-"manual" source such as "auto") — untouched by this fence.
func fenceItemOriginRole(item map[string]any) string {
	answers, _ := item["answer"].([]any)
	for _, a := range answers {
		am, ok := a.(map[string]any)
		if !ok {
			continue
		}
		extAny, _ := am["extension"].([]any)
		for _, e := range extAny {
			em, ok := e.(map[string]any)
			if !ok || em["url"] != dtrInformationOriginExt {
				continue
			}
			var source, authorRef string
			subAny, _ := em["extension"].([]any)
			for _, s := range subAny {
				sm, ok := s.(map[string]any)
				if !ok {
					continue
				}
				switch sm["url"] {
				case "source":
					source, _ = sm["valueCode"].(string)
				case "author":
					authorSub, _ := sm["extension"].([]any)
					for _, as := range authorSub {
						asm, ok := as.(map[string]any)
						if ok && asm["url"] == "reference" {
							authorRef, _ = asm["valueString"].(string)
						}
					}
				}
			}
			if source != "manual" {
				continue
			}
			switch {
			case strings.HasPrefix(authorRef, "Practitioner/"):
				return "clinician"
			case strings.HasPrefix(authorRef, "Patient/"):
				return "patient"
			}
		}
	}
	return ""
}

// fr16AttestationFields are the sub-extension urls FR-16's clinician
// attestation must carry — the clinician's NPI, the attestation text, and the
// attestation date — in the order the fence reports them. They are exactly the
// three shnsdk.BuildManualAttestedItem writes out of shnsdk.Attestation
// (NPI/Text/When), and exactly what FR-16 mandates: an attestation is a
// clinician, a statement, and a date. Adding a mandated field here is additive;
// each field carries its own rejection row so none can regress unnoticed.
var fr16AttestationFields = []string{"npi", "text", "date"}

// fenceClinicianAttestationDefect returns a legible description of what is
// wrong with item's FR-16 clinician attestation — the extension absent
// altogether, or one of fr16AttestationFields absent or empty — or "" when the
// attestation is complete and the item conforms.
func fenceClinicianAttestationDefect(item map[string]any) string {
	att, found := fenceItemExtension(item, shnsdk.ClinicianAttestationExt)
	if !found {
		return "carries no FR-16 attestation extension"
	}
	for _, field := range fr16AttestationFields {
		if fenceSubExtensionValue(att, field) == "" {
			return fmt.Sprintf("carries an FR-16 attestation extension whose %q is absent or empty", field)
		}
	}
	return ""
}

// fencePatientAttestationDefect returns a legible description of what is wrong
// with item's FR-27 patient attestation — the questionnaireresponse-signature
// extension absent altogether, or a valueSignature whose signature type,
// timestamp, signer identity, or identity token is absent or unusable — or ""
// when the attestation is complete. Those are the elements FR-27 names ("these
// are my own responses" carried as the typed Author's Signature assertion, an
// identity token, and a timestamp) plus the signer the assertion is about, and
// exactly what shnsdk.BuildPatientAttestedItem writes. The signer identity
// (Signature.who) accepts FHIR R4's two legal forms — a Reference or an
// Identifier — so a conformant partner PHG (e.g. DHIN) whose who carries only
// an identifier, with no resolvable reference, is not wrongly refused.
func fencePatientAttestationDefect(item map[string]any) string {
	sig, found := fenceItemExtension(item, shnsdk.QRSignatureExt)
	if !found {
		return "carries no FR-27 patient attestation extension"
	}
	value, _ := sig["valueSignature"].(map[string]any)
	if len(value) == 0 {
		return "carries an FR-27 patient attestation extension with no valueSignature"
	}
	if fenceSignatureTypeCode(value) == "" {
		return `carries an FR-27 patient attestation whose "type" names no signature code`
	}
	if fenceStringField(value, "when") == "" {
		return `carries an FR-27 patient attestation whose "when" is absent or empty`
	}
	who, _ := value["who"].(map[string]any)
	if !fenceWhoUsable(who) {
		return `carries an FR-27 patient attestation whose "who" has neither a non-empty "reference" nor an "identifier" with a non-empty "value" (FHIR R4's two legal forms for Signature.who — identifier.system is not required)`
	}
	if fenceStringField(value, "data") == "" {
		return `carries an FR-27 patient attestation whose "data" (identity token) is absent or empty`
	}
	return ""
}

// fenceWhoUsable reports whether who — a FHIR Signature.who, typed by FHIR R4 as
// Reference(Practitioner|RelatedPerson|Patient|Device|Organization) OR Identifier
// — carries a usable signer identity: a non-empty "reference", or an "identifier"
// with a non-empty "value". FR-27 requires an identity token for the signer, not
// a resolvable reference, so both legal forms are accepted (a conformant partner
// PHG, e.g. DHIN, may sign with an identifier alone). identifier.system is NOT
// required: FHIR R4's Identifier element requires no sub-element structurally,
// and a bare value still names the signer within an implicit/local system — the
// minimum that makes an identifier usable is a non-empty value.
func fenceWhoUsable(who map[string]any) bool {
	if fenceStringField(who, "reference") != "" {
		return true
	}
	ident, _ := who["identifier"].(map[string]any)
	return fenceStringField(ident, "value") != ""
}

// fenceItemExtension returns item's first item-level extension (not an
// answer-level one — FR-16's clinician attestation and FR-27's
// questionnaireresponse-signature both declare item as their context, exactly
// where shnsdk.BuildManualAttestedItem / BuildPatientAttestedItem place them)
// whose url == want.
func fenceItemExtension(item map[string]any, want string) (map[string]any, bool) {
	extAny, _ := item["extension"].([]any)
	for _, e := range extAny {
		em, ok := e.(map[string]any)
		if ok && em["url"] == want {
			return em, true
		}
	}
	return nil, false
}

// fenceSubExtensionValue returns the non-empty string value carried by ext's
// sub-extension whose url == want, across the value[x] flavours the attestation
// builders write (valueString for npi/text, valueDate for date), or "" when the
// sub-extension is absent, carries a non-string value, or carries a blank one.
// Whitespace-only counts as blank: " " attests no more than "".
func fenceSubExtensionValue(ext map[string]any, want string) string {
	subAny, _ := ext["extension"].([]any)
	for _, sub := range subAny {
		sm, ok := sub.(map[string]any)
		if !ok || sm["url"] != want {
			continue
		}
		for _, key := range []string{"valueString", "valueDate", "valueDateTime", "valueInstant"} {
			if v := fenceStringField(sm, key); v != "" {
				return v
			}
		}
	}
	return ""
}

// fenceSignatureTypeCode returns the first non-empty Signature.type coding code
// on a valueSignature, or "" when the signature declares no typed assertion at
// all — a signature that asserts nothing is not an attestation.
func fenceSignatureTypeCode(value map[string]any) string {
	typeAny, _ := value["type"].([]any)
	for _, c := range typeAny {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if code := fenceStringField(cm, "code"); code != "" {
			return code
		}
	}
	return ""
}

// fenceStringField returns node[key] when it is a string with non-whitespace
// content, and "" otherwise (absent, wrong type, or blank).
func fenceStringField(node map[string]any, key string) string {
	v, _ := node[key].(string)
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return v
}

// fenceItemLinkID returns item's linkId, or "" when absent — used only to make
// a rejection reason legible, never to key logic.
func fenceItemLinkID(item map[string]any) string {
	id, _ := item["linkId"].(string)
	return id
}
