package engine

// nativepas_decision_test.go — the payer's own ClaimResponse decisions the
// submit-leg rows answer with, and the decision this gateway's reading cannot
// hold.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// deniedClaimResponse is a payer's denial carrying the decision detail it
// states: its X12 review decision (with the reasons it coded) and the
// adjudication reason codes it supplied.
func deniedClaimResponse(reviewAction string, reasons []map[string]any) []byte {
	adj := []map[string]any{{
		"category":  map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/adjudication", "code": "submitted"}}},
		"extension": json.RawMessage(reviewAction),
	}}
	for _, r := range reasons {
		adj = append(adj, map[string]any{
			"category": map[string]any{"coding": []any{map[string]any{"code": "denialreason"}}},
			"reason":   map[string]any{"coding": []any{r}},
		})
	}
	b, err := json.Marshal(map[string]any{
		"resourceType": "ClaimResponse",
		"status":       "active",
		"use":          "preauthorization",
		"outcome":      "complete",
		"disposition":  "Not covered under the member's plan.",
		"item":         []any{map[string]any{"itemSequence": 1, "adjudication": adj}},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// approvedClaimResponse is a payer's approval carrying the detail it states:
// its authorization number, its own note, its A1 review decision, and the
// adjudication reason codes it supplied.
func approvedClaimResponse(reasons []map[string]any) []byte {
	adj := []map[string]any{{
		"category":  map[string]any{"coding": []any{map[string]any{"code": "submitted"}}},
		"extension": json.RawMessage(reviewActionExt("A1", "Certified in total", "", "")),
	}}
	for _, r := range reasons {
		adj = append(adj, map[string]any{
			"category": map[string]any{"coding": []any{map[string]any{"code": "denialreason"}}},
			"reason":   map[string]any{"coding": []any{r}},
		})
	}
	b, err := json.Marshal(map[string]any{
		"resourceType":  "ClaimResponse",
		"status":        "active",
		"use":           "preauthorization",
		"outcome":       "complete",
		"preAuthRef":    "PA-DECISION",
		"preAuthPeriod": map[string]any{"end": "2030-01-01"},
		"processNote":   []any{map[string]any{"number": 1, "type": "display", "text": "Approved for 12 visits."}},
		"item":          []any{map[string]any{"itemSequence": 1, "adjudication": adj}},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// withPreAuthRef states an authorization number in the ClaimResponse's own
// top-level preAuthRef field.
func withPreAuthRef(claimResponse []byte, authNumber string) []byte {
	var cr map[string]any
	if err := json.Unmarshal(claimResponse, &cr); err != nil {
		panic(err)
	}
	cr["preAuthRef"] = authNumber
	b, err := json.Marshal(cr)
	if err != nil {
		panic(err)
	}
	return b
}

// reviewActionExt renders the payer's review decision extension: the X12 306
// decision code and, when given, the X12 886 reason it coded.
func reviewActionExt(code, display, reasonSystem, reasonCode string) string {
	subs := `{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode",` +
		`"valueCodeableConcept":{"coding":[{"system":"` + shnsdk.X12ReviewDecisionSystem + `","code":"` + code + `","display":"` + display + `"}]}}`
	if reasonCode != "" {
		subs += `,{"url":"reasonCode","valueCodeableConcept":{"coding":[{"system":"` + reasonSystem + `","code":"` + reasonCode + `"}]}}`
	}
	return `[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[` + subs + `]}]`
}

// withNote adds a processNote the payer typed as given.
func withNote(claimResponse []byte, typ, text string) []byte {
	var cr map[string]any
	if err := json.Unmarshal(claimResponse, &cr); err != nil {
		panic(err)
	}
	cr["processNote"] = []any{map[string]any{"number": 1, "type": typ, "text": text}}
	b, err := json.Marshal(cr)
	if err != nil {
		panic(err)
	}
	return b
}

// TestNativeSubmit_DenialStatingAnAuthorizationNumberIsRelayed: a
// payer answer that reads as a denial yet states an authorization number in
// its own preAuthRef field may be a partial certification; this gateway's
// reading cannot tell, so it writes no decision from it —
// and relays the payer's decision exactly, naming why. It never refuses it.
func TestNativeSubmit_DenialStatingAnAuthorizationNumberIsRelayed(t *testing.T) {
	for _, reviewAction := range []string{"A2", "A3"} {
		t.Run(reviewAction, func(t *testing.T) {
			body := fixturePASResponse(t, withPreAuthRef(deniedClaimResponse(reviewActionExt(reviewAction, "Not Certified", "", ""), nil), "PA-PARTIAL"), true)
			srv := stubPartnerSrv(t, http.StatusOK, body)
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
			ctx, leg := withPASLeg(context.Background(), "provider")
			res, err := n.Handle(ctx, "pas-claim", "corr-decision", "PCI-1", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			if err != nil || res.Status != 0 || !bytes.Equal(responseBytes(res), body) {
				t.Fatalf("the payer's decision must be relayed exactly: err=%v status=%d (%s)", err, res.Status, res.Message)
			}
			if res.Commit != nil {
				t.Fatal("nothing may be written")
			}
			if leg.unreadRule() != RuleEOBDecision || len(leg.detailed) != 1 || leg.detailed[0].kind != pendDecisionNotRecordedEvent || leg.detailed[0].detail != decisionReadAsDenialWithAuthNumber {
				t.Fatalf("leg unread=%q notes=%+v, want the decision named as not recorded", leg.unreadRule(), leg.detailed)
			}
		})
	}
}
