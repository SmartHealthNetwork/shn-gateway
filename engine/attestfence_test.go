package engine

import (
	"encoding/json"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// wrapQRItemBundle wraps a single QuestionnaireResponse.item (produced by a real
// shnsdk builder, or a plain system-sourced item literal) into the minimal
// conformant PAS Bundle shape fenceAttestedItems walks: a Bundle carrying one
// QuestionnaireResponse entry with one item. No extension URL is hand-typed
// here — only generic FHIR envelope keys (resourceType/entry/item/status).
func wrapQRItemBundle(t *testing.T, itemJSON []byte) []byte {
	t.Helper()
	var item map[string]any
	if err := json.Unmarshal(itemJSON, &item); err != nil {
		t.Fatalf("wrapQRItemBundle: unmarshal item: %v", err)
	}
	qr := map[string]any{
		"resourceType": "QuestionnaireResponse",
		"status":       "completed",
		"item":         []any{item},
	}
	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "collection",
		"entry":        []any{map[string]any{"resource": qr}},
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("wrapQRItemBundle: marshal bundle: %v", err)
	}
	return raw
}

// stripItemExtension removes a real builder's item-level "extension" field
// (FR-16's clinician attestation / FR-27's patient signature) — the ONLY
// mutation the negative fixtures make on top of BuildManualAttestedItem /
// BuildPatientAttestedItem's output. The item.answer-level FR-17 source
// extension is left intact, exactly the shape an amend that dropped its
// attestation but kept its source-attribution would carry.
func stripItemExtension(t *testing.T, itemJSON []byte) []byte {
	t.Helper()
	var item map[string]any
	if err := json.Unmarshal(itemJSON, &item); err != nil {
		t.Fatalf("stripItemExtension: unmarshal: %v", err)
	}
	delete(item, "extension")
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("stripItemExtension: marshal: %v", err)
	}
	return raw
}

// TestFenceAttestedItems is the fenceAttestedItems unit suite: attested/
// unattested clinician and patient items (naming FR-16/FR-27 on rejection),
// plus the fence's other documented behaviors (system-sourced items untouched,
// no-QR-entry a pass-through).
func TestFenceAttestedItems(t *testing.T) {
	att := shnsdk.Attestation{NPI: "1999999999", Text: "I attest these are my clinical findings.", When: "2026-06-04"}

	// Case 1: clinician item WITH attestation passes.
	t.Run("clinician-attested-passes", func(t *testing.T) {
		item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", att)
		if err != nil {
			t.Fatalf("BuildManualAttestedItem: %v", err)
		}
		reason, ok := fenceAttestedItems(wrapQRItemBundle(t, item))
		if !ok {
			t.Fatalf("clinician item WITH attestation: want ok=true, got reject reason=%q", reason)
		}
	})

	// Case 2: clinician item WITHOUT attestation rejects, naming FR-16.
	t.Run("clinician-unattested-rejects-FR16", func(t *testing.T) {
		item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", att)
		if err != nil {
			t.Fatalf("BuildManualAttestedItem: %v", err)
		}
		stripped := stripItemExtension(t, item)
		reason, ok := fenceAttestedItems(wrapQRItemBundle(t, stripped))
		if ok {
			t.Fatalf("clinician item WITHOUT attestation: want ok=false, got accept")
		}
		if !strings.Contains(reason, "FR-16") {
			t.Fatalf("reject reason %q does not name FR-16", reason)
		}
	})

	// Case 3: patient item without patient attestation rejects, naming FR-27.
	t.Run("patient-unattested-rejects-FR27", func(t *testing.T) {
		item, err := shnsdk.BuildPatientAttestedItem("functional-status-oswestry", "42", "Patient/MBR-UC07", "2026-06-04")
		if err != nil {
			t.Fatalf("BuildPatientAttestedItem: %v", err)
		}
		stripped := stripItemExtension(t, item)
		reason, ok := fenceAttestedItems(wrapQRItemBundle(t, stripped))
		if ok {
			t.Fatalf("patient item WITHOUT attestation: want ok=false, got accept")
		}
		if !strings.Contains(reason, "FR-27") {
			t.Fatalf("reject reason %q does not name FR-27", reason)
		}
	})

	// Non-vacuous twin of case 3: the SAME builder output, undisturbed, passes —
	// proving the rejection above is the stripped extension, not some other
	// property of a patient-sourced item.
	t.Run("patient-attested-passes", func(t *testing.T) {
		item, err := shnsdk.BuildPatientAttestedItem("functional-status-oswestry", "42", "Patient/MBR-UC07", "2026-06-04")
		if err != nil {
			t.Fatalf("BuildPatientAttestedItem: %v", err)
		}
		reason, ok := fenceAttestedItems(wrapQRItemBundle(t, item))
		if !ok {
			t.Fatalf("patient item WITH attestation: want ok=true, got reject reason=%q", reason)
		}
	})

	// A system-sourced item (no FR-17 manual-source marker at all — the ordinary
	// auto-filled shape) is untouched: no attestation is required of it.
	t.Run("system-sourced-untouched", func(t *testing.T) {
		item := []byte(`{"linkId":"conservative-therapy-weeks","answer":[{"valueInteger":6}]}`)
		reason, ok := fenceAttestedItems(wrapQRItemBundle(t, item))
		if !ok {
			t.Fatalf("system-sourced item: want ok=true (untouched), got reject reason=%q", reason)
		}
	})

	// A Bundle carrying no QuestionnaireResponse entry at all (optional on the
	// submit leg, R-5) is a pass-through — nothing to fence.
	t.Run("no-qr-entry-passes", func(t *testing.T) {
		bundle := []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
		reason, ok := fenceAttestedItems(bundle)
		if !ok {
			t.Fatalf("bundle with no QR entry: want ok=true, got reject reason=%q", reason)
		}
	})
}

// wrapTwoQRItemBundle wraps TWO QuestionnaireResponse.item values into a Bundle
// carrying TWO separate QuestionnaireResponse entries (first, second) — the
// hand-built-bundle shape the FR-16/FR-27 fence must walk in full, not only its
// first entry.
func wrapTwoQRItemBundle(t *testing.T, firstItemJSON, secondItemJSON []byte) []byte {
	t.Helper()
	qrEntry := func(itemJSON []byte) map[string]any {
		var item map[string]any
		if err := json.Unmarshal(itemJSON, &item); err != nil {
			t.Fatalf("wrapTwoQRItemBundle: unmarshal item: %v", err)
		}
		return map[string]any{"resource": map[string]any{
			"resourceType": "QuestionnaireResponse",
			"status":       "completed",
			"item":         []any{item},
		}}
	}
	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "collection",
		"entry":        []any{qrEntry(firstItemJSON), qrEntry(secondItemJSON)},
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("wrapTwoQRItemBundle: marshal bundle: %v", err)
	}
	return raw
}

// TestFenceAttestedItems_SecondQREntryFenced closes the multi-QR bypass: a
// hand-built bundle can carry a SECOND QuestionnaireResponse bundle entry, and
// the fence must walk it exactly as it walks the first — nothing in the FR-16/
// FR-27 conformance property is scoped to "the first QR entry only". Both
// conformant builders emit exactly one QR entry, so this path is unreachable
// through the shipped path; it is exactly where a hand-built bundle would
// smuggle a second, unfenced entry.
func TestFenceAttestedItems_SecondQREntryFenced(t *testing.T) {
	att := shnsdk.Attestation{NPI: "1999999999", Text: "I attest these are my clinical findings.", When: "2026-06-04"}

	cleanFirst, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", att)
	if err != nil {
		t.Fatalf("BuildManualAttestedItem (first, clean): %v", err)
	}

	// The load-bearing new row: first QR clean, second QR carries a
	// clinician-sourced item with a defective (stripped) attestation. Must be
	// refused — a fence scoped to the first entry only would accept this.
	t.Run("second-qr-defective-rejects", func(t *testing.T) {
		defectiveSecond, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "43", att)
		if err != nil {
			t.Fatalf("BuildManualAttestedItem (second, defective): %v", err)
		}
		defectiveSecond = stripItemExtension(t, defectiveSecond)
		reason, ok := fenceAttestedItems(wrapTwoQRItemBundle(t, cleanFirst, defectiveSecond))
		if ok {
			t.Fatalf("second QR entry carries an unattested clinician item: want ok=false, got accept")
		}
		if !strings.Contains(reason, "FR-16") {
			t.Fatalf("reject reason %q does not name FR-16", reason)
		}
	})

	// Positive control: two CLEAN QR entries must not be turned into a false
	// rejection by walking both — the fix must not punish a legitimate
	// multi-QR bundle.
	t.Run("two-clean-qr-entries-pass", func(t *testing.T) {
		cleanSecond, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "43", att)
		if err != nil {
			t.Fatalf("BuildManualAttestedItem (second, clean): %v", err)
		}
		reason, ok := fenceAttestedItems(wrapTwoQRItemBundle(t, cleanFirst, cleanSecond))
		if !ok {
			t.Fatalf("two clean QR entries: want ok=true, got reject reason=%q", reason)
		}
	})
}

// dropSubExtension deletes the sub-extension whose url == sub from the
// item-level extension whose url == parent — the "field absent" mutation, the
// twin of the "field present but blank" one the real builder produces from an
// empty Attestation field. Both must reject: a fence that only rejects the
// blank form is defeated by omitting the element instead.
func dropSubExtension(t *testing.T, itemJSON []byte, parent, sub string) []byte {
	t.Helper()
	var item map[string]any
	if err := json.Unmarshal(itemJSON, &item); err != nil {
		t.Fatalf("dropSubExtension: unmarshal: %v", err)
	}
	dropped := false
	extAny, _ := item["extension"].([]any)
	for _, e := range extAny {
		em, ok := e.(map[string]any)
		if !ok || em["url"] != parent {
			continue
		}
		subAny, _ := em["extension"].([]any)
		kept := make([]any, 0, len(subAny))
		for _, s := range subAny {
			sm, ok := s.(map[string]any)
			if ok && sm["url"] == sub {
				dropped = true
				continue
			}
			kept = append(kept, s)
		}
		em["extension"] = kept
	}
	if !dropped {
		t.Fatalf("dropSubExtension: sub-extension %q not found under %q", sub, parent)
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("dropSubExtension: marshal: %v", err)
	}
	return raw
}

// TestFenceAttestedItems_EmptyClinicianAttestationRejected is the unit-level
// half of the FR-16 CONTENT guard: an attestation extension carrying the right
// url but an empty field attests nothing, and must be rejected exactly as a
// missing extension is. One row per mandated field, in BOTH mutation forms
// (present-but-blank, produced by the real builder from an empty Attestation
// field; and absent, produced by deleting the sub-extension), so no single
// field can regress unnoticed — plus the all-empty attestation, which is the
// shape that motivated the guard, and a positive control proving a well-formed
// attestation still passes.
func TestFenceAttestedItems_EmptyClinicianAttestationRejected(t *testing.T) {
	const (
		goodNPI  = "1999999999"
		goodText = "I attest these are my clinical findings."
		goodWhen = "2026-06-04"
	)
	good := shnsdk.Attestation{NPI: goodNPI, Text: goodText, When: goodWhen}

	// Positive control FIRST: the fence must not simply reject everything.
	t.Run("complete-attestation-passes", func(t *testing.T) {
		item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", good)
		if err != nil {
			t.Fatalf("BuildManualAttestedItem: %v", err)
		}
		if reason, ok := fenceAttestedItems(wrapQRItemBundle(t, item)); !ok {
			t.Fatalf("complete attestation: want ok=true, got reject reason=%q", reason)
		}
	})

	for _, tc := range []struct {
		field string
		att   shnsdk.Attestation
	}{
		{"npi", shnsdk.Attestation{NPI: "", Text: goodText, When: goodWhen}},
		{"text", shnsdk.Attestation{NPI: goodNPI, Text: "", When: goodWhen}},
		{"date", shnsdk.Attestation{NPI: goodNPI, Text: goodText, When: ""}},
	} {
		t.Run("blank-"+tc.field+"-rejects", func(t *testing.T) {
			item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", tc.att)
			if err != nil {
				t.Fatalf("BuildManualAttestedItem: %v", err)
			}
			reason, ok := fenceAttestedItems(wrapQRItemBundle(t, item))
			if ok {
				t.Fatalf("attestation with blank %q: want ok=false, got accept", tc.field)
			}
			if !strings.Contains(reason, "FR-16") || !strings.Contains(reason, tc.field) {
				t.Fatalf("reject reason %q does not name both FR-16 and %q", reason, tc.field)
			}
		})

		t.Run("absent-"+tc.field+"-rejects", func(t *testing.T) {
			item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", good)
			if err != nil {
				t.Fatalf("BuildManualAttestedItem: %v", err)
			}
			mutated := dropSubExtension(t, item, shnsdk.ClinicianAttestationExt, tc.field)
			reason, ok := fenceAttestedItems(wrapQRItemBundle(t, mutated))
			if ok {
				t.Fatalf("attestation with %q deleted: want ok=false, got accept", tc.field)
			}
			if !strings.Contains(reason, "FR-16") || !strings.Contains(reason, tc.field) {
				t.Fatalf("reject reason %q does not name both FR-16 and %q", reason, tc.field)
			}
		})
	}

	// The whole-attestation form: every field blank. This is the shape a
	// url-only fence accepted.
	t.Run("all-fields-blank-rejects", func(t *testing.T) {
		item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", shnsdk.Attestation{})
		if err != nil {
			t.Fatalf("BuildManualAttestedItem: %v", err)
		}
		reason, ok := fenceAttestedItems(wrapQRItemBundle(t, item))
		if ok {
			t.Fatalf("wholly empty attestation: want ok=false, got accept")
		}
		if !strings.Contains(reason, "FR-16") {
			t.Fatalf("reject reason %q does not name FR-16", reason)
		}
	})
}

// blankSignatureField blanks one string field of the FR-27 valueSignature —
// "when" / "data" directly, "who.reference" through the nested object, and
// "type" by emptying the coding array. One mutation on top of
// BuildPatientAttestedItem's real output.
func blankSignatureField(t *testing.T, itemJSON []byte, field string) []byte {
	t.Helper()
	var item map[string]any
	if err := json.Unmarshal(itemJSON, &item); err != nil {
		t.Fatalf("blankSignatureField: unmarshal: %v", err)
	}
	blanked := false
	extAny, _ := item["extension"].([]any)
	for _, e := range extAny {
		em, ok := e.(map[string]any)
		if !ok || em["url"] != shnsdk.QRSignatureExt {
			continue
		}
		value, ok := em["valueSignature"].(map[string]any)
		if !ok {
			continue
		}
		switch field {
		case "type":
			value["type"] = []any{}
		case "who.reference":
			who, _ := value["who"].(map[string]any)
			if who == nil {
				t.Fatalf("blankSignatureField: valueSignature has no who")
			}
			who["reference"] = ""
		default:
			value[field] = ""
		}
		blanked = true
	}
	if !blanked {
		t.Fatalf("blankSignatureField: no patient signature extension found")
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("blankSignatureField: marshal: %v", err)
	}
	return raw
}

// TestFenceAttestedItems_EmptyPatientAttestationRejected is the FR-27 twin of
// the FR-16 content rows: a questionnaireresponse-signature whose typed
// assertion, timestamp, signer or identity token is blank is not an
// attestation, and is rejected. Positive control included.
func TestFenceAttestedItems_EmptyPatientAttestationRejected(t *testing.T) {
	build := func(t *testing.T) []byte {
		t.Helper()
		item, err := shnsdk.BuildPatientAttestedItem("functional-status-oswestry", "42", "Patient/MBR-UC07", "2026-06-04")
		if err != nil {
			t.Fatalf("BuildPatientAttestedItem: %v", err)
		}
		return item
	}

	t.Run("complete-signature-passes", func(t *testing.T) {
		if reason, ok := fenceAttestedItems(wrapQRItemBundle(t, build(t))); !ok {
			t.Fatalf("complete patient attestation: want ok=true, got reject reason=%q", reason)
		}
	})

	// who.reference's wantSubstr is "who", not "who.reference": since Task D (register §15(b))
	// the fence accepts FHIR R4's two legal forms for Signature.who (reference OR identifier),
	// so blanking ONLY who.reference — leaving who with neither a usable reference nor an
	// identifier — is refused under the combined "who" reason. See
	// TestFenceWhoUsable for the fence's actual new decision boundary (both legal forms,
	// the identifier-empty-value refusal, and the identifier-only acceptance).
	fields := []struct{ field, wantSubstr string }{
		{"type", "type"},
		{"when", "when"},
		{"who.reference", "who"},
		{"data", "data"},
	}
	for _, tc := range fields {
		t.Run("blank-"+tc.field+"-rejects", func(t *testing.T) {
			mutated := blankSignatureField(t, build(t), tc.field)
			reason, ok := fenceAttestedItems(wrapQRItemBundle(t, mutated))
			if ok {
				t.Fatalf("patient attestation with blank %q: want ok=false, got accept", tc.field)
			}
			if !strings.Contains(reason, "FR-27") || !strings.Contains(reason, tc.wantSubstr) {
				t.Fatalf("reject reason %q does not name both FR-27 and %q", reason, tc.wantSubstr)
			}
		})
	}
}

// TestFenceWhoUsable is the register §15(b) / Task D unit test: the FR-27 fence's decision
// boundary for Signature.who now accepts FHIR R4's two legal forms — reference OR identifier
// — rather than reference-only. who absent, or with neither a usable reference nor a usable
// identifier, is still refused; an identifier with an empty "value" is refused (Task D's
// ruling: "value" is required, "system" is not); and a who carrying ONLY an identifier (no
// reference at all) — the shape a conformant partner PHG such as DHIN's may legally send — is
// accepted.
func TestFenceWhoUsable(t *testing.T) {
	setWho := func(t *testing.T, who map[string]any) []byte {
		t.Helper()
		item, err := shnsdk.BuildPatientAttestedItem("functional-status-oswestry", "42", "Patient/MBR-UC07", "2026-06-04")
		if err != nil {
			t.Fatalf("BuildPatientAttestedItem: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(item, &parsed); err != nil {
			t.Fatalf("unmarshal built item: %v", err)
		}
		extAny, _ := parsed["extension"].([]any)
		found := false
		for _, e := range extAny {
			em, ok := e.(map[string]any)
			if !ok || em["url"] != shnsdk.QRSignatureExt {
				continue
			}
			value, ok := em["valueSignature"].(map[string]any)
			if !ok {
				continue
			}
			if who == nil {
				delete(value, "who")
			} else {
				value["who"] = who
			}
			found = true
		}
		if !found {
			t.Fatalf("setWho: no patient signature extension found")
		}
		raw, err := json.Marshal(parsed)
		if err != nil {
			t.Fatalf("marshal mutated item: %v", err)
		}
		return raw
	}

	rejects := map[string]map[string]any{
		"who-absent":                           nil,
		"who-empty-object":                     {},
		"who-neither-reference-nor-identifier": {"type": "Practitioner"},
		"who-identifier-empty-value":           {"identifier": map[string]any{"system": "urn:dhin:signer", "value": ""}},
	}
	for name, who := range rejects {
		t.Run(name+"-rejects", func(t *testing.T) {
			reason, ok := fenceAttestedItems(wrapQRItemBundle(t, setWho(t, who)))
			if ok {
				t.Fatalf("who=%v: want ok=false, got accept", who)
			}
			if !strings.Contains(reason, "FR-27") || !strings.Contains(reason, "who") {
				t.Fatalf("reject reason %q does not name both FR-27 and \"who\"", reason)
			}
		})
	}

	t.Run("who-identifier-only-accepts", func(t *testing.T) {
		who := map[string]any{"identifier": map[string]any{"system": "urn:dhin:signer", "value": "dhin-signer-88f21c"}}
		if reason, ok := fenceAttestedItems(wrapQRItemBundle(t, setWho(t, who))); !ok {
			t.Fatalf("who carrying only an identifier (no reference): want ok=true, got reject reason=%q", reason)
		}
	})
}
