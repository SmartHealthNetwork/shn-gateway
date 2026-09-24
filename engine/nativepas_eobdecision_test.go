package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// eobAdjudications decodes the adjudications of an EOB's single item: each
// one's category code, its reason coding, and the review decision the payer
// stated on it.
type eobAdjudicationView struct {
	Category struct {
		Coding []struct {
			System string `json:"system"`
			Code   string `json:"code"`
		} `json:"coding"`
	} `json:"category"`
	Reason *struct {
		Coding []struct {
			System  string `json:"system"`
			Code    string `json:"code"`
			Display string `json:"display"`
		} `json:"coding"`
	} `json:"reason"`
	Extension []struct {
		URL       string `json:"url"`
		Extension []struct {
			URL                  string `json:"url"`
			ValueCodeableConcept *struct {
				Coding []struct {
					System  string `json:"system"`
					Code    string `json:"code"`
					Display string `json:"display"`
				} `json:"coding"`
			} `json:"valueCodeableConcept"`
		} `json:"extension"`
	} `json:"extension"`
}

func eobAdjudications(t *testing.T, eob []byte) []eobAdjudicationView {
	t.Helper()
	var e struct {
		Item []struct {
			Adjudication []eobAdjudicationView `json:"adjudication"`
		} `json:"item"`
	}
	if err := json.Unmarshal(eob, &e); err != nil {
		t.Fatalf("decode EOB: %v", err)
	}
	if len(e.Item) != 1 {
		t.Fatalf("EOB has %d items, want 1", len(e.Item))
	}
	return e.Item[0].Adjudication
}

// eobReviewDecision returns the X12 306 review decision (code, display) the
// EOB's adjudications state, and whether any state one.
func eobReviewDecision(t *testing.T, eob []byte) (string, string, bool) {
	t.Helper()
	for _, adj := range eobAdjudications(t, eob) {
		for _, ext := range adj.Extension {
			if ext.URL != "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewAction" {
				continue
			}
			for _, sub := range ext.Extension {
				if sub.URL != "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewActionCode" || sub.ValueCodeableConcept == nil {
					continue
				}
				for _, c := range sub.ValueCodeableConcept.Coding {
					if c.System == shnsdk.X12ReviewDecisionSystem {
						return c.Code, c.Display, true
					}
				}
			}
		}
	}
	return "", "", false
}

// eobDenialReasons returns the reason codings of the EOB's denialreason
// adjudications, in order.
func eobDenialReasons(t *testing.T, eob []byte) [][2]string {
	t.Helper()
	var out [][2]string
	for _, adj := range eobAdjudications(t, eob) {
		denial := false
		for _, c := range adj.Category.Coding {
			if c.Code == "denialreason" {
				denial = true
			}
		}
		if !denial || adj.Reason == nil {
			continue
		}
		for _, c := range adj.Reason.Coding {
			out = append(out, [2]string{c.System, c.Code})
		}
	}
	return out
}

// projectFromDecision runs a payer gateway's PAS submit leg against a payer
// that answers with the given decision, and returns the leg result.
func projectFromDecision(t *testing.T, decision []byte) LegResult {
	t.Helper()
	conformant := originatorBuiltConformantBundle(t, "MBR-COVERED")
	srv := stubPartnerSrv(t, http.StatusOK, fixturePASResponse(t, decision, true))
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
	res, err := n.Handle(context.Background(), "pas-claim", "corr-decision", "PCI-1", conformant)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return res
}

// projectedEOB runs the leg and returns the single decision EOB it projected.
func projectedEOB(t *testing.T, decision []byte) []byte {
	t.Helper()
	res := projectFromDecision(t, decision)
	if res.Status != 0 {
		t.Fatalf("submit: status=%d msg=%s", res.Status, res.Message)
	}
	if len(res.SideEffectFHIR) != 1 {
		t.Fatalf("want 1 EOB side-effect, got %d", len(res.SideEffectFHIR))
	}
	return res.SideEffectFHIR[0]
}

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

// TestNativeSubmit_DeniedEOBStatesThePayersOwnDecision: the decision EOB a
// payer gateway projects states the payer's OWN decision — its review decision
// and the reason codes it supplied — and never a reason code the payer did not
// send.
func TestNativeSubmit_DeniedEOBStatesThePayersOwnDecision(t *testing.T) {
	t.Run("no reason code from the payer, no reason code on the EOB", func(t *testing.T) {
		denial, err := shnsdk.BuildDeniedResponseWithNotesAtLine("2.0", "Patient/MBR-COVERED", "corr-decision",
			"Not covered under the member's plan.", nil, fixedClock())
		if err != nil {
			t.Fatal(err)
		}
		eob := projectedEOB(t, denial)
		if reasons := eobDenialReasons(t, eob); len(reasons) != 0 {
			t.Errorf("EOB states reason codes the payer never sent: %v", reasons)
		}
		code, display, ok := eobReviewDecision(t, eob)
		if !ok || code != "A3" {
			t.Fatalf("EOB review decision = %q (present=%v), want the payer's A3", code, ok)
		}
		if display != "Not Certified" {
			t.Errorf("EOB review decision display = %q, want the payer's own", display)
		}
	})

	t.Run("the payer's own reason codes, in order", func(t *testing.T) {
		eob := projectedEOB(t, deniedClaimResponse(reviewActionExt("A3", "Not Certified", "", ""), []map[string]any{
			{"system": shnsdk.CARCSystem, "code": "197", "display": "Precertification absent"},
			{"system": shnsdk.RARCSystem, "code": "N130"},
		}))
		want := [][2]string{{shnsdk.CARCSystem, "197"}, {shnsdk.RARCSystem, "N130"}}
		got := eobDenialReasons(t, eob)
		if len(got) != len(want) {
			t.Fatalf("EOB reason codes = %v, want the payer's %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("EOB reason codes = %v, want the payer's %v", got, want)
			}
		}
		if code, _, ok := eobReviewDecision(t, eob); !ok || code != "A3" {
			t.Errorf("EOB review decision = %q (present=%v), want A3 beside the reason codes", code, ok)
		}
	})

	t.Run("the payer's own review decision, not ours", func(t *testing.T) {
		eob := projectedEOB(t, deniedClaimResponse(
			reviewActionExt("A2", "Not Certified", shnsdk.X12ReviewDecisionReasonSystem, "0X"), nil))
		code, _, ok := eobReviewDecision(t, eob)
		if !ok || code != "A2" {
			t.Fatalf("EOB review decision = %q (present=%v), want the payer's A2", code, ok)
		}
	})
}

// TestNativeSubmit_DecisionDetailTheEOBCannotCarryIsRefused: decision detail a
// decision EOB cannot state — here a review reason coded in a system the
// resource does not bind — is refused loudly (502) rather than dropped or
// replaced with a code the payer never sent.
func TestNativeSubmit_DecisionDetailTheEOBCannotCarryIsRefused(t *testing.T) {
	res := projectFromDecision(t, deniedClaimResponse(
		reviewActionExt("A3", "Not Certified", "http://example.org/reasons", "9"), nil))
	if res.Status != http.StatusBadGateway {
		t.Fatalf("status = %d (%s), want 502", res.Status, res.Message)
	}
	if len(res.SideEffectFHIR) != 0 {
		t.Fatalf("a refused projection still stored %d EOB(s)", len(res.SideEffectFHIR))
	}
}

// TestNativeSubmit_ApprovedEOBStatesThePayersDecision: an approval carries the
// payer's own notes and its own review decision, and states no denial reason.
func TestNativeSubmit_ApprovedEOBStatesThePayersDecision(t *testing.T) {
	eob := projectedEOB(t, approvedClaimResponse(nil))
	if got := eobProcessNotes(t, eob); len(got) != 1 || got[0] != "Approved for 12 visits." {
		t.Errorf("approved EOB notes = %q, want the payer's own note", got)
	}
	code, _, ok := eobReviewDecision(t, eob)
	if !ok || code != "A1" {
		t.Errorf("approved EOB review decision = %q (present=%v), want the payer's A1", code, ok)
	}
	if reasons := eobDenialReasons(t, eob); len(reasons) != 0 {
		t.Errorf("approved EOB states denial reasons: %v", reasons)
	}
}

// TestNativeSubmit_ApprovalReasonCodesAreRefused: a decision EOB states reason
// codes only on a denial, so an approval whose ClaimResponse carries one states
// detail the resource cannot hold. It is refused loudly (502) — the payer's own
// reason line is never dropped to make the projection fit.
func TestNativeSubmit_ApprovalReasonCodesAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason map[string]any
	}{
		{"remittance advice remark", map[string]any{"system": shnsdk.RARCSystem, "code": "N130"}},
		{"claim adjustment reason", map[string]any{"system": shnsdk.CARCSystem, "code": "197", "display": "Precertification absent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := projectFromDecision(t, approvedClaimResponse([]map[string]any{tc.reason}))
			if res.Status != http.StatusBadGateway {
				t.Fatalf("status = %d (%s), want 502", res.Status, res.Message)
			}
			if len(res.SideEffectFHIR) != 0 {
				t.Fatalf("a refused projection still stored %d EOB(s)", len(res.SideEffectFHIR))
			}
		})
	}
}

// TestNativeSubmit_DenialStatingAnAuthorizationNumberIsRefused: a payer answer
// that reads as a denial yet states an authorization number in its own
// preAuthRef field is contradictory — it may be a partial certification whose
// authorization a denial reading would discard. The gateway cannot tell the two
// apart, so it refuses (502) rather than state a denial over an authorization
// the payer may have issued.
func TestNativeSubmit_DenialStatingAnAuthorizationNumberIsRefused(t *testing.T) {
	for _, reviewAction := range []string{"A2", "A3"} {
		t.Run(reviewAction, func(t *testing.T) {
			denial := deniedClaimResponse(reviewActionExt(reviewAction, "Not Certified", "", ""), nil)
			res := projectFromDecision(t, withPreAuthRef(denial, "PA-PARTIAL"))
			if res.Status != http.StatusBadGateway {
				t.Fatalf("status = %d (%s), want 502", res.Status, res.Message)
			}
			if len(res.SideEffectFHIR) != 0 {
				t.Fatalf("a refused projection still stored %d EOB(s)", len(res.SideEffectFHIR))
			}
		})
	}
}
