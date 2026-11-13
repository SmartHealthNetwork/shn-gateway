//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// withoutCoverage is body (a PAS inquiry Bundle) with its Coverage entries
// removed, and, when dropInsurance, each Claim's insurance too, so no Coverage
// is named either.
func withoutCoverage(t *testing.T, body []byte, dropInsurance bool) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var kept []any
	removed := false
	for _, e := range doc["entry"].([]any) {
		r := e.(map[string]any)["resource"].(map[string]any)
		if r["resourceType"] == "Coverage" {
			removed = true
			continue
		}
		if r["resourceType"] == "Claim" && dropInsurance {
			delete(r, "insurance")
		}
		kept = append(kept, e)
	}
	if !removed {
		t.Fatal("fixture: the inquiry carried no Coverage to remove")
	}
	doc["entry"] = kept
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// recordingLevelPayer is a payer gateway at level, recording each exchange.
func recordingLevelPayer(t *testing.T, level ConformanceEnforcement) (*levelPayer, *exchangeRecords) {
	t.Helper()
	p := newLevelPayer(t, level)
	var got exchangeRecords
	p.g.cfg.ExchangeObserved = got.observe
	return p, &got
}

// wantPayerCarriedExactly: the payer's system received body exactly at path, the
// payer's answer was relayed, and the record names no edit not made: a payer's
// gateway makes no edit to a request it carries to its own system, so there
// is none to report.
func wantPayerCarriedExactly(t *testing.T, p *levelPayer, got *exchangeRecords, ans payerAnswer, path string, body []byte) {
	t.Helper()
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s, want the payer's own answer", ans.status, ans.body)
	}
	if p.partner.lastPath != path || !bytes.Equal(p.partner.lastBody, body) {
		t.Fatalf("the payer's system received %q %s, want the request exactly as sent at %s", p.partner.lastPath, p.partner.lastBody, path)
	}
	if rec := got.only(t); len(rec.EditsNotMade) != 0 {
		t.Fatalf("editsNotMade = %q, want none", rec.EditsNotMade)
	}
}

// An inquiry that reaches a payer's gateway with no Coverage is carried to the
// payer's own system exactly as sent, at every level, and the payer's answer
// is relayed. The network routed it here already; whether a Coverage is
// missing is the payer's own system's to answer.
func TestPayerInquiryWithNoCoverageIsCarriedAtEveryLevel(t *testing.T) {
	base := inquiryBundle("MBR-COVERED", "", "TRN-1", "72148")
	for _, dropInsurance := range []bool{false, true} {
		body := withoutCoverage(t, base, dropInsurance)
		for _, level := range allLevels {
			name := level.String()
			if dropInsurance {
				name += "/no insurance"
			}
			t.Run(name, func(t *testing.T) {
				p, got := recordingLevelPayer(t, level)
				wantPayerCarriedExactly(t, p, got, p.send(t, "pas-claim-inquire", "", body), pasInquirePath, body)
			})
		}
	}
}

// An inquiry whose only Coverage the payer's gateway would not find at an
// entry (one contained in its Claim, one whose resourceType is spelled in
// another case, one nested in another entry's Bundle) is carried exactly as
// sent at every level too: the payer's gateway reads no payer identifier from
// it.
func TestPayerInquiryWithACoverageElsewhereIsCarriedAtEveryLevel(t *testing.T) {
	base := inquiryBundle("MBR-COVERED", "", "TRN-1", "72148")
	lower := bytes.Replace(base, []byte(`"resourceType":"Coverage"`), []byte(`"resourceType":"coverage"`), 1)
	if bytes.Equal(lower, base) {
		t.Fatal("fixture: no Coverage resourceType to respell")
	}
	reshape := func(move func(kept []any, coverageEntry map[string]any) []any) []byte {
		var doc map[string]any
		if err := json.Unmarshal(base, &doc); err != nil {
			t.Fatal(err)
		}
		var kept []any
		var coverageEntry map[string]any
		for _, e := range doc["entry"].([]any) {
			if e.(map[string]any)["resource"].(map[string]any)["resourceType"] == "Coverage" {
				coverageEntry = e.(map[string]any)
				continue
			}
			kept = append(kept, e)
		}
		if coverageEntry == nil {
			t.Fatal("fixture: no Coverage")
		}
		doc["entry"] = move(kept, coverageEntry)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	contained := reshape(func(kept []any, cov map[string]any) []any {
		for _, e := range kept {
			if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "Claim" {
				r["contained"] = []any{cov["resource"]}
			}
		}
		return kept
	})
	nested := reshape(func(kept []any, cov map[string]any) []any {
		return append(kept, map[string]any{"fullUrl": "urn:uuid:nested", "resource": map[string]any{
			"resourceType": "Bundle", "type": "collection", "entry": []any{cov}}})
	})
	for name, body := range map[string][]byte{"contained in the Claim": contained, "resourceType in another case": lower, "nested in another entry's Bundle": nested} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p, got := recordingLevelPayer(t, level)
				wantPayerCarriedExactly(t, p, got, p.send(t, "pas-claim-inquire", "", body), pasInquirePath, body)
			})
		}
	}
}

// A CDS Hooks order-select request with no Coverage is carried exactly as
// sent at none and observe: those are the levels that carry an order-select
// request with no Coverage (structural and strict check its Coverage first, a
// participant's opt-in).
func TestPayerCRDWithNoCoverageIsCarriedAsSent(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(conformantCRD("MBR-COVERED", "72148"), &doc); err != nil {
		t.Fatal(err)
	}
	pf, ok := doc["prefetch"].(map[string]any)
	if !ok || pf["coverage"] == nil {
		t.Fatal("fixture: no prefetch.coverage")
	}
	delete(pf, "coverage")
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
		t.Run(level.String(), func(t *testing.T) {
			p, got := recordingLevelPayer(t, level)
			wantPayerCarriedExactly(t, p, got, p.send(t, "crd-order-select", "", body), crdSelectPath, body)
		})
	}
}

// The provider side routes an inquiry by its own Coverage: one with no
// Coverage names no payer, so it is refused as routing (a network rule) at
// every level, before any leg.
func TestLevelInquireIngress_NoCoverageIsARoutingRefusalAtEveryLevel(t *testing.T) {
	base := []byte(levelInquiry(t, "MBR-COVERED", ""))
	for _, dropInsurance := range []bool{false, true} {
		body := withoutCoverage(t, base, dropInsurance)
		for _, level := range allLevels {
			t.Run(level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RoutePASInquire)(rec, httptest.NewRequest(http.MethodPost, "/Claim/$inquire", bytes.NewReader(body)))
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "no payer identifier on member coverage: the Bundle carries no Coverage")
				wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
			})
		}
	}
}

// An order-dispatch request with no Coverage is carried exactly as sent at
// every level.
func TestPayerCRDDispatchWithNoCoverageIsCarriedAtEveryLevel(t *testing.T) {
	body := crdDispatchWith(t, levelCRDDispatchCoverage, ``)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := recordingLevelPayer(t, level)
			wantPayerCarriedExactly(t, p, got, p.send(t, "crd-order-dispatch", "", body), crdDispatchPath, body)
		})
	}
}

// An inquiry with no Coverage whose Claim insurer names this payer's own
// identifier is carried with the insurer, and every other byte, as sent.
func TestPayerInquiryWithNoCoverageLeavesTheInsurerAsSent(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(withoutCoverage(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), true), &doc); err != nil {
		t.Fatal(err)
	}
	set := false
	for _, e := range doc["entry"].([]any) {
		if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "Claim" {
			r["insurer"] = map[string]any{"identifier": map[string]any{"system": shnsdk.CMSPayerIdentity.System, "value": shnsdk.CMSPayerIdentity.Value}}
			set = true
		}
	}
	if !set {
		t.Fatal("fixture: no Claim")
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := recordingLevelPayer(t, level)
			wantPayerCarriedExactly(t, p, got, p.send(t, "pas-claim-inquire", "", body), pasInquirePath, body)
		})
	}
}

// A DTR $questionnaire-package request with no Coverage that still names its
// patient (on its order) is carried exactly as sent at none and observe. At
// structural and strict a request with no coverage is refused first
// (RuleRequestShape).
func TestPayerDTRWithNoCoverageIsCarriedAsSent(t *testing.T) {
	body := []byte(`{"resourceType":"Parameters","parameter":[` +
		`{"name":"order","resource":{"resourceType":"ServiceRequest","id":"sr-1","status":"active","intent":"order","subject":{"reference":"Patient/` + dtrFrameMember + `"}}},` +
		`{"name":"questionnaire","valueCanonical":"http://x/q"}]}`)
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
		t.Run(level.String(), func(t *testing.T) {
			p, got := recordingLevelPayer(t, level)
			wantPayerCarriedExactly(t, p, got, p.send(t, "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, body), "/Questionnaire/$questionnaire-package", body)
		})
	}
}
