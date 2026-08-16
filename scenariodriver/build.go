package scenariodriver

import (
	"bytes"
	"encoding/json"
	"fmt"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const SystemHCPCS = "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets"

// ScenarioOrder is the HCPCS code and display for a Prior Authorization scenario (UC-01…08).
type ScenarioOrder struct {
	Code    string
	Display string
}

// PersonaOrders maps each scenario key (noPA|approve|deny|pend) to its persona-selected
// HCPCS code + display. Transcribed from br-provider's order-templates.ts; SELECTION only,
// never invented. br-payer matches its PlanDefinition by code regardless of order type.
var PersonaOrders = map[string]ScenarioOrder{
	"noPA":    {Code: "E0250", Display: "Hospital Bed with Side Rails"},
	"approve": {Code: "L8000", Display: "Breast prosthesis, mastectomy bra"},
	"deny":    {Code: "J3490", Display: "Unclassified drugs"},
	"pend":    {Code: "E0424", Display: "Stationary Oxygen System"},
}

// payorOrg is the payer identity a driver-built Coverage names for one member,
// plus the contained Organization's id and display name.
type payorOrg struct {
	id          shnsdk.PayerIdentifier
	containedID string
	name        string
}

// cmsPayorOrg is the default every provider-seeded member's driver-built Coverage
// has always named (urn:oid:2.16.840.1.113883.6.300|00001, the identity br-payer
// adjudicates against).
var cmsPayorOrg = payorOrg{id: shnsdk.CMSPayerIdentity, containedID: "cms-payer", name: "Centers for Medicare and Medicaid Services"}

// payorOrgs overrides that default for members whose OWN Coverage names a
// different payer — today, the two bridging-demo personas, whose seeded
// Coverage payor is gateway/engine's BridgeDemoPayerID / BridgeRefusePayerID
// (holderdata.go's stubPayerOverrides; internal/fhirseed's origination-Coverage
// block).
//
// Why the DRIVER has to know this: the conformant lane routes payload-FIRST
// (AI-G13) — the gateway derives the payer holder from the Coverage carried in
// the INBOUND request, never from its own SoR — so a prefetch/parameters
// Coverage that always named CMS would send every conformant run to the CMS
// payer holder no matter which member it claimed to be about, silently. That
// is exactly what the mixed-version gate caught: a bridge-demo run that
// passed while quietly talking to the ordinary sandbox payer.
//
// The identities are duplicated as LITERALS rather than imported: this package
// is the lightweight partner-facing conformant driver and must not pull the
// whole engine into a partner's binary. TestPayorOrgs_MatchEngineIdentities
// (a test-only engine import) fences the duplication against drift.
var payorOrgs = map[string]payorOrg{
	"MBR-BRIDGE-DEMO": {
		id:          shnsdk.PayerIdentifier{System: "urn:shn:demo-payer", Value: "SHN-BRIDGE-DEMO"},
		containedID: "bridge-demo-payer-org",
		name:        "SHN Bridge Demo Payer",
	},
	"MBR-BRIDGE-REFUSE": {
		id:          shnsdk.PayerIdentifier{System: "urn:shn:demo-payer", Value: "SHN-BRIDGE-REFUSE"},
		containedID: "bridge-refuse-payer-org",
		name:        "SHN Bridge Refuse Demo Payer",
	},
}

// payorOrgFor returns the payer Organization a driver-built Coverage for member
// must name — the member's own payer where one is known, else the CMS default.
func payorOrgFor(member string) payorOrg {
	if p, ok := payorOrgs[member]; ok {
		return p
	}
	return cmsPayorOrg
}

// BuildCRDRequest builds a conformant CDS Hooks order-sign request for the given code on the
// given provider-seeded member. Coverage carries the member's own payor Org (payorOrgFor —
// cms-payer, urn:oid:2.16.840.1.113883.6.300|00001, for every ordinary member) which br-payer
// adjudicates against and which the payload-first ingress routes off. The ServiceRequest carrier
// enables SHN's payer-side subject-bind extraction; br-payer matches its PlanDefinition by code
// regardless of order type. hookInstance is the fixed opaque token "shn-scenariodriver" (not
// per-request identity, so runs stay deterministic and comparable against goldens); display is
// omitted from the coding when empty.
func BuildCRDRequest(member, system, code, display string) ([]byte, error) {
	ref := "Patient/" + member
	payor := payorOrgFor(member)
	coding := map[string]any{"system": system, "code": code}
	if display != "" {
		coding["display"] = display
	}
	body := map[string]any{
		"hook":         "order-sign",
		"hookInstance": "shn-scenariodriver",
		"fhirServer":   "https://provider.example/fhir",
		"context": map[string]any{
			"userId":    "Practitioner/p1",
			"patientId": member,
			"draftOrders": map[string]any{
				"resourceType": "Bundle", "type": "collection",
				"entry": []any{map[string]any{
					"fullUrl": "urn:uuid:sr1",
					"resource": map[string]any{
						"resourceType": "ServiceRequest", "id": "sr1", "status": "draft",
						"intent":    "order",
						"code":      map[string]any{"coding": []any{coding}},
						"subject":   map[string]any{"reference": ref},
						"insurance": []any{map[string]any{"reference": "Coverage/c1"}},
					},
				}},
			},
		},
		"prefetch": map[string]any{
			"patient": map[string]any{"resourceType": "Patient", "id": member},
			"coverage": map[string]any{
				"resourceType": "Coverage", "id": "c1", "status": "active",
				"beneficiary": map[string]any{"reference": ref},
				"payor":       []any{map[string]any{"reference": "#" + payor.containedID}},
				"contained": []any{map[string]any{
					"resourceType": "Organization", "id": payor.containedID,
					"identifier": []any{map[string]any{"system": payor.id.System, "value": payor.id.Value}},
					"name":       payor.name,
				}},
			},
		},
	}
	return json.Marshal(body)
}

// RebindPASPatient sets the Patient.id STRUCTURALLY (the golden is pretty-printed `"id": "…"` with a
// space, so a raw string-replace would no-op), then string-replaces every Patient/<oldID> reference
// on the freshly-marshaled (spacing-normalized) JSON. This is the deterministic rebind of a committed
// br-payer golden onto a provider-seeded member. br-payer keys its decision on the order code, not
// the patient. Returns error on unparseable JSON or if no Patient resource is found.
func RebindPASPatient(bundleJSON []byte, newID string) ([]byte, error) {
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	entries, _ := b["entry"].([]any)
	oldID := ""
	for _, e := range entries {
		r, _ := e.(map[string]any)["resource"].(map[string]any)
		if r != nil && r["resourceType"] == "Patient" {
			oldID, _ = r["id"].(string)
			r["id"] = newID
		}
	}
	if oldID == "" {
		return nil, fmt.Errorf("bundle has no Patient resource to rebind")
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal rebind: %w", err)
	}
	return bytes.ReplaceAll(out, []byte("Patient/"+oldID), []byte("Patient/"+newID)), nil
}

// AddRoutablePayor adds an inline payor identifier (CMSPayerIdentity, urn:oid:…300|00001) to the
// PAS bundle's Coverage.payor[0] so the PAS ingress can route it off the bundle's Coverage (FR-G40).
// The existing payor REFERENCE (Organization/InsurerExample) is PRESERVED — the identifier is purely
// ADDITIVE and the PAS ingress's ParsePayerIdentifier reads the inline form first. Returns error on
// unparseable JSON or if the bundle has no Coverage entry.
func AddRoutablePayor(bundleJSON []byte) ([]byte, error) {
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	entries, _ := b["entry"].([]any)
	found := false
	for _, e := range entries {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res == nil || res["resourceType"] != "Coverage" {
			continue
		}
		payors, _ := res["payor"].([]any)
		if len(payors) == 0 {
			payors = []any{map[string]any{}}
		}
		p0, _ := payors[0].(map[string]any)
		if p0 == nil {
			p0 = map[string]any{}
		}
		p0["identifier"] = map[string]any{"system": shnsdk.CMSPayerIdentity.System, "value": shnsdk.CMSPayerIdentity.Value}
		payors[0] = p0
		res["payor"] = payors
		found = true
	}
	if !found {
		return nil, fmt.Errorf("bundle has no Coverage to make routable")
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return out, nil
}

// BuildPASBundle loads a committed br-payer $submit golden, rebinds it onto member, and makes its
// Coverage routable (AddRoutablePayor) so the PAS ingress can derive the payer holder from the
// bundle's Coverage (FR-G40).
//
// AddRoutablePayor unconditionally stamps the CMS payor identifier (shnsdk.CMSPayerIdentity) —
// there is no non-CMS $submit routing implemented in this path. So before touching the golden,
// BuildPASBundle fences on member: if payorOrgFor(member) resolves to anything other than
// cmsPayorOrg (e.g. the bridge-demo personas, whose OWN Coverage names a different payer),
// stamping the CMS identifier anyway would silently misroute the PAS submission to the wrong
// payer holder. Fail closed instead (ledger item 5 / spec §3): reject loudly rather than stamp
// silently.
func BuildPASBundle(golden []byte, member string) ([]byte, error) {
	if org := payorOrgFor(member); org != cmsPayorOrg {
		return nil, fmt.Errorf("scenariodriver: BuildPASBundle routes via AddRoutablePayor's CMS payor; member %q resolves to %q — non-CMS PAS routing is not implemented (fail-closed fence)", member, org.name)
	}
	rebound, err := RebindPASPatient(golden, member)
	if err != nil {
		return nil, err
	}
	return AddRoutablePayor(rebound)
}

// InjectShnCorrelation surgically adds a Claim.identifier entry {system:"urn:shn:correlation",
// value:corr} to a PAS Bundle JSON via one-pass map edit. Deterministic. Enables the submit→amend
// correlation handoff: SHN keys the pend on the partner-supplied corr so the follow-up amended
// re-POST can reference it via Claim.related[prior].
func InjectShnCorrelation(bundleJSON []byte, corr string) ([]byte, error) {
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	entries, _ := b["entry"].([]any)
	for _, e := range entries {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res != nil && res["resourceType"] == "Claim" {
			existing, _ := res["identifier"].([]any)
			res["identifier"] = append(existing, map[string]any{
				"system": "urn:shn:correlation",
				"value":  corr,
			})
			break
		}
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return out, nil
}

// BuildQuestionnairePackageRequest builds a conformant DTR $questionnaire-package input Parameters
// with the contained-payor Coverage + valueCanonical. The Parameters is consumed by the DTR ingress
// to fetch a CRD card's questionnaire from the native payer endpoint — and, since that ingress
// routes payload-first (AI-G13), the contained payor is also what selects the payer holder, so it
// names the MEMBER's own payer (payorOrgFor), not a fixed CMS default.
func BuildQuestionnairePackageRequest(canonical, member string) ([]byte, error) {
	payor := payorOrgFor(member)
	params := map[string]any{
		"resourceType": "Parameters",
		"meta": map[string]any{
			"profile": []any{"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-qpackage-input-parameters"},
		},
		"parameter": []any{
			map[string]any{
				"name": "coverage",
				"resource": map[string]any{
					"resourceType": "Coverage", "id": "coverage-1", "status": "active",
					"beneficiary": map[string]any{"reference": "Patient/" + member},
					// The urn:shn:coverage business identifier is the SAME convention
					// shnsdk.BuildCoverageWithPayer stamps (sdk/crd.go): the MB member
					// number, bare — not a reference. Nothing derives a reference from
					// it — a payer answering $questionnaire-package at DTR 2.2 must return a
					// QuestionnaireResponse shell, and the engine derives that shell's
					// coverageRef from Coverage.id (dtrPackageCoverageSubject), which is
					// "coverage-1" here, failing closed rather than inventing one. The id
					// (and this whole prefetch Coverage) is what makes the conformant lane
					// 2.2-capable rather than silently 2.0-only — found live by the mixed-version
					// mixed-version gate against a 2.2-declaring peer. The "type" v2-0203 MB
					// coding completes the copy of that same convention (sdk/crd.go's
					// Identifier.Type): nothing in-house reads it, but a real Da Vinci peer
					// that profiles Coverage.identifier.type would otherwise see a shape gap
					// (a close-out finding).
					"identifier": []any{map[string]any{
						"type": map[string]any{
							"coding": []any{map[string]any{
								"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MB",
							}},
						},
						"system": "urn:shn:coverage", "value": member,
					}},
					"payor": []any{map[string]any{"reference": "#payor-org"}},
					"contained": []any{map[string]any{
						"resourceType": "Organization", "id": "payor-org", "active": true,
						"identifier": []any{map[string]any{"system": payor.id.System, "value": payor.id.Value}},
						"name":       payor.name,
					}},
				},
			},
			map[string]any{"name": "questionnaire", "valueCanonical": canonical},
		},
	}
	return json.Marshal(params)
}
