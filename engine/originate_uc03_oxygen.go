// originate_uc03_oxygen.go — support for handleUC03's non-provider-data (demo) arm,
// re-keyed onto the HomeOxygen family (register §11 ruling (b), R3): the hermetic FR-17
// auto-fill evidence cross-check and the item-6.1 manual attestation. The order itself
// is the member's own seeded DeviceRequest, read through dispatchOrderOfRecord like
// every other order-DISPATCH origination — this arm used to BUILD its DeviceRequest and
// supplier from a literal tuple, which left the authorization it produced naming an
// order no participant's system held.
package engine

import (
	"context"
	"encoding/json"
	"fmt"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

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
func (g *Gateway) homeOxygenAutoFillEvidenceContext(ctx context.Context, member string, qrJSON []byte) ([]FilledItem, error) {
	cc, ok, readErr := ReadSystemOfRecord(g.cfg.SoR).ClinicalContextContext(ctx, member)
	if readErr != nil {
		return nil, safeSoRError(readErr)
	}
	if !ok {
		return nil, nil
	}
	qrAnswers := questionnaireResponseNumericAnswers(qrJSON)
	var out []FilledItem
	if cc.OxygenSaturationRef != "" && qrAnswers["2.2"] != "" && qrAnswers["2.2"] == cc.OxygenSaturationPct {
		out = append(out, FilledItem{LinkID: "2.2", Answer: qrAnswers["2.2"], Origin: "auto", SourceRef: cc.OxygenSaturationRef})
	}
	if cc.ArterialPaO2Ref != "" && qrAnswers["2.3"] != "" && qrAnswers["2.3"] == cc.ArterialPaO2mmHg {
		out = append(out, FilledItem{LinkID: "2.3", Answer: qrAnswers["2.3"], Origin: "auto", SourceRef: cc.ArterialPaO2Ref})
	}
	return out, nil
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

// buildOxygenNecessityItem captures the existing single manual attestation event.
func buildOxygenNecessityItem(npi, display, dx, when string) ([]byte, error) {
	statement := fmt.Sprintf("The ordering provider attests that %s (diagnosis %s) is medically necessary.", display, dx)
	itemJSON, err := shnsdk.BuildManualAttestedItem("6.1", statement, shnsdk.Attestation{
		NPI: npi, Text: "I attest this order is medically necessary.", When: when,
	})
	if err != nil {
		return nil, fmt.Errorf("build manual attested item 6.1: %w", err)
	}
	return itemJSON, nil
}
