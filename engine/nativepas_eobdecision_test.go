package engine

import (
	"bytes"
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
			// The payer's own review action, re-expressed value for value in
			// the EOB's own extension.
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
		denial, err := shnsdk.BuildDeniedResponseWithNotesAtLineExact("2.0", "Patient/MBR-COVERED", "corr-decision",
			"Not covered under the member's plan.", nil, testAnswerRequest, fixedClock())
		if err != nil {
			t.Fatal(err)
		}
		eob := projectedEOB(t, denial)
		if reasons := eobDenialReasons(t, eob); len(reasons) != 0 {
			t.Errorf("EOB states reason codes the payer never sent: %v", reasons)
		}
		// The payer's own adjudication is carried byte for byte: its
		// amountless "submitted" entry with its A3. No amount is added.
		code, display, ok := eobReviewDecision(t, eob)
		if !ok || code != "A3" || display != "Not Certified" {
			t.Fatalf("EOB review decision = %q %q (present=%v), want the payer's A3 Not Certified", code, display, ok)
		}
		if bytes.Contains(eob, []byte(`"amount"`)) {
			t.Fatalf("EOB states an amount the payer's denial did not supply: %s", eob)
		}
	})

	t.Run("the payer's own amount carries its review decision", func(t *testing.T) {
		eob := projectedEOB(t, withItemAmount(deniedClaimResponse(reviewActionExt("A3", "Not Certified", "", ""), nil), `{"value":125.5,"currency":"USD"}`))
		code, display, ok := eobReviewDecision(t, eob)
		if !ok || code != "A3" || display != "Not Certified" {
			t.Fatalf("EOB review decision = %q %q (present=%v), want the payer's A3 Not Certified", code, display, ok)
		}
		// The fixture is re-marshalled from a map, so its keys are sorted: the
		// payer's bytes here are {"currency","value"}, carried as they are.
		if !bytes.Contains(eob, []byte(`"amount":{"currency":"USD","value":125.5}`)) {
			t.Fatalf("EOB does not carry the payer's own amount exactly: %s", eob)
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
		eob := projectedEOB(t, withItemAmount(deniedClaimResponse(
			reviewActionExt("A2", "Not Certified", shnsdk.X12ReviewDecisionReasonSystem, "0X"), nil), `{"value":0,"currency":"USD"}`))
		code, _, ok := eobReviewDecision(t, eob)
		if !ok || code != "A2" {
			t.Fatalf("EOB review decision = %q (present=%v), want the payer's A2", code, ok)
		}
	})
}

// TestNativeSubmit_DecisionDetailTheEOBCannotCarryIsRelayed: decision
// detail a decision EOB cannot carry as written — here a note typed outside
// the resource's required NoteType binding — never refuses the payer's
// decision: it is relayed exactly and recorded, no EOB is written, and the leg
// names the detail. A review reason in any system is carried as written.
func TestNativeSubmit_DecisionDetailTheEOBCannotCarryIsRelayed(t *testing.T) {
	decision := withNote(deniedClaimResponse(reviewActionExt("A3", "Not Certified", "", ""), nil), "email", "Reply by email.")
	body := fixturePASResponse(t, decision, true)
	srv := stubPartnerSrv(t, http.StatusOK, body)
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
	ctx, leg := withPASLeg(context.Background(), "provider")
	res, err := n.Handle(ctx, "pas-claim", "corr-decision", "PCI-1", originatorBuiltConformantBundle(t, "MBR-COVERED"))
	if err != nil || res.Status != 0 || !bytes.Equal(responseBytes(res), body) {
		t.Fatalf("the payer's decision must be relayed exactly: err=%v status=%d (%s)", err, res.Status, res.Message)
	}
	if len(res.SideEffectFHIR) != 0 || res.Commit == nil {
		t.Fatalf("want the decision recorded with no EOB: %d EOB(s), commit=%v", len(res.SideEffectFHIR), res.Commit != nil)
	}
	want := "decision EOB not written: process note 1 type is not display, print or printoper"
	if len(leg.detailed) != 1 || leg.detailed[0].kind != DecisionEOBNotWrittenEvent || leg.detailed[0].detail != want {
		t.Fatalf("notes = %+v, want %s %q", leg.detailed, DecisionEOBNotWrittenEvent, want)
	}
	eob := projectedEOB(t, deniedClaimResponse(reviewActionExt("A3", "Not Certified", "http://example.org/reasons", "9"), nil))
	if !bytes.Contains(eob, []byte(`{"url":"reasonCode","valueCodeableConcept":{"coding":[{"code":"9","system":"http://example.org/reasons"}]}}`)) {
		t.Fatalf("the payer's review reason is not carried as written: %s", eob)
	}
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

// TestNativeSubmit_ApprovedEOBStatesThePayersDecision: an approval carries the
// payer's own notes and its own review decision, and states no denial reason.
func TestNativeSubmit_ApprovedEOBStatesThePayersDecision(t *testing.T) {
	eob := projectedEOB(t, withItemAmount(approvedClaimResponse(nil), `{"value":980,"currency":"USD"}`))
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

	// An approval whose payer stated no amount carries its amountless entry
	// with its A1, exactly; no amount is added.
	bare := projectedEOB(t, approvedClaimResponse(nil))
	if code, _, ok := eobReviewDecision(t, bare); !ok || code != "A1" {
		t.Fatalf("approved EOB review decision = %q (present=%v), want the payer's A1: %s", code, ok, bare)
	}
	if bytes.Contains(bare, []byte(`"amount"`)) || !bytes.Contains(bare, []byte(`"preAuthRef":["PA-DECISION"]`)) {
		t.Fatalf("approved EOB with no amount anywhere: %s", bare)
	}
}

// withItemAmount states an amount on the ClaimResponse item's first
// adjudication: the payer's own adjudicated figure.
func withItemAmount(claimResponse []byte, amount string) []byte {
	var cr map[string]any
	if err := json.Unmarshal(claimResponse, &cr); err != nil {
		panic(err)
	}
	item := cr["item"].([]any)[0].(map[string]any)
	adj := item["adjudication"].([]any)
	adj[0].(map[string]any)["amount"] = json.RawMessage(amount)
	b, err := json.Marshal(cr)
	if err != nil {
		panic(err)
	}
	return b
}

// TestNativeSubmit_ApprovalReasonCodesAreCarried: an approval whose
// ClaimResponse carries reason codes has them carried exactly as the payer
// wrote them: the EOB claims no profile that would forbid them, and the
// payer's own reason line is never dropped.
func TestNativeSubmit_ApprovalReasonCodesAreCarried(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason map[string]any
		want   string
	}{
		{"remittance advice remark", map[string]any{"system": shnsdk.RARCSystem, "code": "N130"}, `"reason":{"coding":[{"code":"N130","system":"` + shnsdk.RARCSystem + `"}]}`},
		{"claim adjustment reason", map[string]any{"system": shnsdk.CARCSystem, "code": "197", "display": "Precertification absent"}, `"reason":{"coding":[{"code":"197","display":"Precertification absent","system":"` + shnsdk.CARCSystem + `"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eob := projectedEOB(t, approvedClaimResponse([]map[string]any{tc.reason}))
			if !bytes.Contains(eob, []byte(tc.want)) {
				t.Fatalf("the payer's reason is not carried as written (%s): %s", tc.want, eob)
			}
		})
	}
}

// TestNativeSubmit_DenialStatingAnAuthorizationNumberIsRelayed: a
// payer answer that reads as a denial yet states an authorization number in
// its own preAuthRef field may be a partial certification; this gateway's
// reading cannot tell, so it writes nothing from it (no decision, no EOB) —
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
			if len(res.SideEffectFHIR) != 0 || res.Commit != nil {
				t.Fatalf("nothing may be written: %d EOB(s), commit=%v", len(res.SideEffectFHIR), res.Commit != nil)
			}
			if leg.unreadRule() != RuleEOBDecision || len(leg.detailed) != 1 || leg.detailed[0].kind != pendDecisionNotRecordedEvent || leg.detailed[0].detail != decisionReadAsDenialWithAuthNumber {
				t.Fatalf("leg unread=%q notes=%+v, want the decision named as not recorded", leg.unreadRule(), leg.detailed)
			}
		})
	}
}
