// originate_uc03_oxygen.go — support for handleUC03's non-provider-data (demo) arm,
// re-keyed onto the HomeOxygen family (register §11 ruling (b), R3): a literal-code
// order-DISPATCH origination (mirroring originateDispatch's mechanics, but building its
// DeviceRequest + supplier Organization from the demo lane's own literal-tuple convention
// — §4.3, originate_codes.go's file comment — rather than reading a seeded SoR order),
// the hermetic FR-17 auto-fill evidence cross-check, and the item-6.1 manual attestation.
package engine

import (
	"encoding/json"
	"fmt"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// uc03OxygenOrderID / uc03OxygenSupplierID are the literal ids the demo-lane UC-03
// DeviceRequest + supplier Organization carry. Stable literals (not member-scoped): this
// arm serves exactly one member per profile (MBR-COVERED default-profile / MBR-D-UC03
// demo-profile / the demo-roster's canary twin), and the resources exist only within one
// request's build — never persisted, never resolved by a second request.
const (
	uc03OxygenOrderID    = "dr-uc03-demo"
	uc03OxygenSupplierID = "org-uc03-demo-supplier"
	// uc03OxygenSupplierNPI is a syntactically-valid placeholder — the supplier NPI is
	// verdict-IRRELEVANT to the mirrored family's outcome (originate_homeoxygen.go's own
	// comment on the provider-data twin), so no live/contracted NPI meaning is implied.
	uc03OxygenSupplierNPI = "1999999999"
)

// literalOxygenDispatchOrder builds UC-03's demo-lane DeviceRequest + supplier
// Organization LITERALLY from the given HCPCS tuple — the demo lane's own convention
// (originationCodes, §4.3): no SoR order needed, only a seeded Patient/Coverage per
// member. Mirrors orderSource's non-provider-data ServiceRequest-literal branch, for the
// order-DISPATCH shape originateDispatch's OWN callers get from the SoR instead. Built as
// a plain map (not shnsdk.BuildDeviceRequest/BuildOrganizationWithNPI): gateway/engine is
// a SEPARATE Go module from internal/fhirmap and cannot import it (same constraint
// originate_homeoxygen_test.go's buildHomeOxygenDeviceRequest/buildHomeOxygenSupplier
// document); the wire shape mirrors those functions.
func literalOxygenDispatchOrder(patientRef, code, display, dx string) (dispatchOrder, error) {
	dr := map[string]any{
		"resourceType": "DeviceRequest",
		"id":           uc03OxygenOrderID,
		"status":       "active",
		"intent":       "order",
		"subject":      map[string]string{"reference": patientRef},
		"performer":    map[string]string{"reference": "Organization/" + uc03OxygenSupplierID},
		"codeCodeableConcept": map[string]any{
			"coding": []map[string]string{{
				"system":  systemHCPCSBuild,
				"code":    code,
				"display": display,
			}},
		},
		"reasonCode": []map[string]any{{
			"coding": []map[string]string{{
				"system": systemICD10Build,
				"code":   dx,
			}},
		}},
	}
	orderJSON, err := json.Marshal(dr)
	if err != nil {
		return dispatchOrder{}, fmt.Errorf("build literal DeviceRequest: %w", err)
	}
	org := map[string]any{
		"resourceType": "Organization",
		"id":           uc03OxygenSupplierID,
		"name":         "Demo DME Supplier",
		"identifier": []map[string]string{{
			"system": "http://hl7.org/fhir/sid/us-npi",
			"value":  uc03OxygenSupplierNPI,
		}},
	}
	supplierJSON, err := json.Marshal(org)
	if err != nil {
		return dispatchOrder{}, fmt.Errorf("build literal supplier Organization: %w", err)
	}
	return dispatchOrder{
		orderJSON: orderJSON, supplierJSON: supplierJSON,
		orderRef: "DeviceRequest/" + uc03OxygenOrderID, performerRef: "Organization/" + uc03OxygenSupplierID,
	}, nil
}

// homeOxygenAutoFillEvidence is the hermetic FR-17 source=auto attribution proof (register
// §9 row 4 / §11): it independently cross-checks the operated-$populate-computed QR's
// answered Observation-backed items (2.2 O₂-sat, 2.3 PaO₂) against the member's OWN
// SEEDED Observation (via SoR.ClinicalContext) — never trusting the populate engine's own
// claim, never inventing a sourceRef. An item is attributed Origin="auto" ONLY when BOTH
// are true: the QR carries an answer for that linkId, AND it EXACTLY matches the value the
// member's seeded Observation carries (so a divergence — the populate engine computing
// something the seed does not back — is silently UNATTRIBUTED, never claimed as sourced).
// Returns nil (not an empty slice) when the member has no ClinicalContext at all, or when
// neither item cross-checks.
func (g *Gateway) homeOxygenAutoFillEvidence(member string, qrJSON []byte) []FilledItem {
	cc, ok := g.cfg.SoR.ClinicalContext(member)
	if !ok {
		return nil
	}
	qrAnswers := questionnaireResponseNumericAnswers(qrJSON)
	var out []FilledItem
	if cc.OxygenSaturationRef != "" && qrAnswers["2.2"] != "" && qrAnswers["2.2"] == cc.OxygenSaturationPct {
		out = append(out, FilledItem{LinkID: "2.2", Answer: qrAnswers["2.2"], Origin: "auto", SourceRef: cc.OxygenSaturationRef})
	}
	if cc.ArterialPaO2Ref != "" && qrAnswers["2.3"] != "" && qrAnswers["2.3"] == cc.ArterialPaO2mmHg {
		out = append(out, FilledItem{LinkID: "2.3", Answer: qrAnswers["2.3"], Origin: "auto", SourceRef: cc.ArterialPaO2Ref})
	}
	return out
}

// attestOxygenNecessity answers HomeOxygenDispatch's ONE required leaf — 6.1, "Medical
// Necessity Statement" (text) — the way its sibling UCs answer clinician/requester-supplied
// content (register §11 ruling, §3): through the requester's OWN attestation, source=
// "manual" (never "auto", never fabricated as clinical fact), merged into the ALREADY
// operated-$populate-computed QR via shnsdk.AmendQRWithItemIn so the genuine auto-origin
// items (2.2/2.3) survive byte-unchanged — unlike attestAdaptiveQuestionnaire's
// answers-map rebuild (fine for UC-04/05/06/07's 0-CQL trees, which have nothing to
// preserve; wrong here, where it would discard the real auto answers). dx/display name the
// order this attestation accompanies — administrative attribution, not an invented
// clinical finding.
func attestOxygenNecessity(qrJSON, questionnaireJSON []byte, npi, display, dx, when string) ([]byte, error) {
	statement := fmt.Sprintf("The ordering provider attests that %s (diagnosis %s) is medically necessary.", display, dx)
	itemJSON, err := shnsdk.BuildManualAttestedItem("6.1", statement, shnsdk.Attestation{
		NPI: npi, Text: "I attest this order is medically necessary.", When: when,
	})
	if err != nil {
		return nil, fmt.Errorf("build manual attested item 6.1: %w", err)
	}
	amended, err := shnsdk.AmendQRWithItemIn(qrJSON, questionnaireJSON, itemJSON)
	if err != nil {
		return nil, fmt.Errorf("amend qr with item 6.1: %w", err)
	}
	return amended, nil
}

// questionnaireResponseAnswered reports whether qrJSON's item tree carries a NON-EMPTY
// answer for linkID, anywhere on either FHIR nesting axis (item.item / item.answer.item).
// Deliberately independent of the caller's own control flow: handleUC03Oxygen uses this
// to compute uc03Resp.Attested from the ACTUAL SUBMITTED CONTENT, not from "did the
// attestation code path run" — a self-reported flag would stay true even if a future edit
// accidentally submitted the pre-attestation shell (the exact bug this slice exists to
// rule out; see uc02_uc03_test.go's TestUC03_AutoApproved mutation evidence).
func questionnaireResponseAnswered(qrJSON []byte, linkID string) bool {
	var probe struct {
		Item []qrAnyItemNode `json:"item"`
	}
	if json.Unmarshal(qrJSON, &probe) != nil {
		return false
	}
	var walk func(items []qrAnyItemNode) bool
	walk = func(items []qrAnyItemNode) bool {
		for _, it := range items {
			if it.LinkID == linkID && len(it.Answer) > 0 {
				return true
			}
			for _, a := range it.Answer {
				if walk(a.Item) {
					return true
				}
			}
			if walk(it.Item) {
				return true
			}
		}
		return false
	}
	return walk(probe.Item)
}

// qrAnyItemNode is the recursive QR item shape questionnaireResponseAnswered reads: only
// linkId + whether an answer array is present (any type), plus the two nesting axes.
type qrAnyItemNode struct {
	LinkID string `json:"linkId"`
	Answer []struct {
		Item []qrAnyItemNode `json:"item"`
	} `json:"answer"`
	Item []qrAnyItemNode `json:"item"`
}
