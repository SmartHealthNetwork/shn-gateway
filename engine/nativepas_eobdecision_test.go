package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// assertNativeDecisionUntouched proves the payer's application reply remains
// canonical, including details a separate clinical action cannot record.
func assertNativeDecisionUntouched(t *testing.T, decision []byte) {
	t.Helper()
	request := originatorBuiltConformantBundle(t, "MBR-COVERED")
	want := fixturePASResponse(t, decision, true)
	srv := stubPartnerSrv(t, http.StatusOK, want)
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
	got, err := n.Handle(context.Background(), "pas-claim", "corr-decision", "PCI-1", request)
	if err != nil || got.Status != 0 || got.ApplicationStatus != http.StatusOK {
		t.Fatalf("native reply status=%d application=%d err=%v: %s", got.Status, got.ApplicationStatus, err, got.Message)
	}
	if !bytes.Equal(relay.BytesForTest(got.Response), want) {
		t.Fatal("native relay altered the payer's decision bytes")
	}
	if len(got.SideEffectFHIR) != 0 || got.Commit != nil {
		t.Fatal("native relay implicitly recorded a clinical decision")
	}
}

func decisionReviewAction(code, display, reasonSystem, reasonCode string) string {
	subs := `{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode",` +
		`"valueCodeableConcept":{"coding":[{"system":"` + shnsdk.X12ReviewDecisionSystem + `","code":"` + code + `","display":"` + display + `"}]}}`
	if reasonCode != "" {
		subs += `,{"url":"reasonCode","valueCodeableConcept":{"coding":[{"system":"` + reasonSystem + `","code":"` + reasonCode + `"}]}}`
	}
	return `[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[` + subs + `]}]`
}

func decisionClaimResponse(code, reasonSystem, reasonCode, authNumber string, notes []shnsdk.PASProcessNote) []byte {
	adjudication := []map[string]any{{
		"category":  map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/adjudication", "code": "submitted"}}},
		"extension": json.RawMessage(decisionReviewAction(code, "Payer decision", reasonSystem, reasonCode)),
	}}
	if reasonCode != "" {
		adjudication = append(adjudication, map[string]any{"category": map[string]any{"coding": []any{map[string]any{"code": "denialreason"}}},
			"reason": map[string]any{"coding": []any{map[string]any{"system": reasonSystem, "code": reasonCode}}}})
	}
	response := map[string]any{"resourceType": "ClaimResponse", "status": "active", "use": "preauthorization", "outcome": "complete",
		"item": []any{map[string]any{"itemSequence": 1, "adjudication": adjudication}}, "processNote": notes}
	if authNumber != "" {
		response["preAuthRef"] = authNumber
	}
	b, err := json.Marshal(response)
	if err != nil {
		panic(err)
	}
	return b
}

// PCV-07/08: decisions, contradictions, and unsupported detail belong to the
// payer's original ClaimResponse. They do not authorize native projection, and
// an EOB action refusing later must not convert the peer's successful reply.
func TestNativeSubmit_DecisionVariantsRelayExactlyWithoutClinicalWrite(t *testing.T) {
	for _, tc := range []struct {
		name, code, reasonSystem, reasonCode, number string
	}{
		{"approved", "A1", "", "", "AUTH-1"},
		{"denied", "A3", "", "", ""},
		{"denial with payer CARC", "A3", shnsdk.CARCSystem, "197", ""},
		{"denial with payer RARC", "A3", shnsdk.RARCSystem, "N130", ""},
		{"partial certification", "A2", "", "", "AUTH-PARTIAL"},
		{"denial with authorization contradiction", "A3", "", "", "AUTH-CONTRADICTORY"},
		{"unsupported reason system", "A3", "http://example.org/reasons", "9", ""},
		{"approval with contradictory reason", "A1", shnsdk.CARCSystem, "197", "AUTH-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertNativeDecisionUntouched(t, decisionClaimResponse(tc.code, tc.reasonSystem, tc.reasonCode, tc.number, nil))
		})
	}
}
