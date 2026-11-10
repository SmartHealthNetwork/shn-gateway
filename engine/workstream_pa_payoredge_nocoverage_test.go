//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// mappingRecordingPayer is a payer gateway at level, with the payer identity mapping
// on when mapping, recording each exchange.
func mappingRecordingPayer(t *testing.T, level ConformanceEnforcement, mapping bool) (*levelPayer, *exchangeRecords) {
	t.Helper()
	var opts []NativeOption
	if mapping {
		opts = append(opts, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
	}
	p := newLevelPayer(t, level, opts...)
	var got exchangeRecords
	p.g.cfg.ExchangeObserved = got.observe
	return p, &got
}

// wantNoCoverageEditNotMade: with the mapping on, the record names the payer
// identity mapping (E-03) as not made because the request carries no Coverage;
// with it off, there is no edit to make and none is named.
func wantNoCoverageEditNotMade(t *testing.T, rec ExchangeRecord, mapping bool) {
	t.Helper()
	var want []string
	if mapping {
		want = []string{EditNotMadePayorEdgeNoCoverage}
	}
	if !slices.Equal(rec.EditsNotMade, want) {
		t.Fatalf("editsNotMade = %q, want %q", rec.EditsNotMade, want)
	}
}

// An inquiry that reaches a payer's gateway with no Coverage has no payor
// identifier for the payer identity mapping to re-key: it is carried to the
// payer's own system exactly as sent, at every level, whether or not the
// mapping is on, and the payer's answer is relayed. The network routed it here
// already; whether a Coverage is missing is the payer's own system's to answer.
func TestPayorEdge_InquiryWithNoCoverageIsCarriedAtEveryLevel(t *testing.T) {
	base := inquiryBundle("MBR-COVERED", "", "TRN-1", "72148")
	for _, dropInsurance := range []bool{false, true} {
		body := withoutCoverage(t, base, dropInsurance)
		for _, mapping := range []bool{false, true} {
			for _, level := range allLevels {
				name := level.String()
				if mapping {
					name += "/mapping on"
				}
				if dropInsurance {
					name += "/no insurance"
				}
				t.Run(name, func(t *testing.T) {
					p, got := mappingRecordingPayer(t, level, mapping)
					ans := p.send(t, "pas-claim-inquire", "", body)
					if ans.status != http.StatusOK {
						t.Fatalf("answer %d %s, want the payer's own answer", ans.status, ans.body)
					}
					if p.partner.lastPath != pasInquirePath || !bytes.Equal(p.partner.lastBody, body) {
						t.Fatalf("the payer's system received %q %s, want the inquiry exactly as sent", p.partner.lastPath, p.partner.lastBody)
					}
					wantNoCoverageEditNotMade(t, got.only(t), mapping)
				})
			}
		}
	}
}

// A Coverage anywhere in an inquiry is still read for the mapping: one carried
// only inside a contained resource, where the mapping does not look, is not
// "no Coverage", so the mapping refuses it rather than forward a Coverage it
// did not re-key.
func TestPayorEdge_InquiryWithOnlyAContainedCoverageIsRefused(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), &doc); err != nil {
		t.Fatal(err)
	}
	var coverage map[string]any
	var kept []any
	for _, e := range doc["entry"].([]any) {
		r := e.(map[string]any)["resource"].(map[string]any)
		if r["resourceType"] == "Coverage" {
			coverage = r
			continue
		}
		kept = append(kept, e)
	}
	if coverage == nil {
		t.Fatal("fixture: no Coverage")
	}
	for _, e := range kept {
		if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "Claim" {
			r["contained"] = []any{coverage}
		}
	}
	doc["entry"] = kept
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := mappingRecordingPayer(t, level, true)
			ans := p.send(t, "pas-claim-inquire", "", body)
			if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), "inbound Coverage carries no resolvable payor identifier") {
				t.Fatalf("answer %d %s, want the mapping's 400", ans.status, ans.body)
			}
			if p.partner.lastBody != nil {
				t.Fatalf("the payer's system received %s, want nothing", p.partner.lastBody)
			}
			if rec := got.only(t); len(rec.EditsNotMade) != 0 {
				t.Fatalf("editsNotMade = %q, want none: the request was refused, not carried", rec.EditsNotMade)
			}
		})
	}
}

// A CDS Hooks request with no Coverage is carried through the mapping as it
// always was, and from v1.9.0 the record says why the mapping made no
// edit, as it does for an inquiry. The rows run at none and observe: those are
// the levels that carry an order-select request with no Coverage to the
// mapping (structural and strict check its Coverage first, a participant's
// opt-in). An order-dispatch request is carried at every level.
func TestPayorEdge_CRDWithNoCoverageRecordsTheEditNotMade(t *testing.T) {
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
	for _, mapping := range []bool{false, true} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
			name := level.String()
			if mapping {
				name += "/mapping on"
			}
			t.Run(name, func(t *testing.T) {
				p, got := mappingRecordingPayer(t, level, mapping)
				ans := p.send(t, "crd-order-select", "", body)
				if ans.status != http.StatusOK {
					t.Fatalf("answer %d %s", ans.status, ans.body)
				}
				if !bytes.Equal(p.partner.lastBody, body) {
					t.Fatalf("the payer's system received %s, want the request exactly as sent", p.partner.lastBody)
				}
				wantNoCoverageEditNotMade(t, got.only(t), mapping)
			})
		}
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

// An order-dispatch request with no Coverage is carried at every level, and
// with the mapping on its record names the edit not made.
func TestPayorEdge_CRDDispatchWithNoCoverageRecordsTheEditNotMade(t *testing.T) {
	body := crdDispatchWith(t, levelCRDDispatchCoverage, ``)
	for _, mapping := range []bool{false, true} {
		for _, level := range allLevels {
			name := level.String()
			if mapping {
				name += "/mapping on"
			}
			t.Run(name, func(t *testing.T) {
				p, got := mappingRecordingPayer(t, level, mapping)
				ans := p.send(t, "crd-order-dispatch", "", body)
				if ans.status != http.StatusOK {
					t.Fatalf("answer %d %s", ans.status, ans.body)
				}
				if !bytes.Equal(p.partner.lastBody, body) {
					t.Fatalf("the payer's system received %s, want the request exactly as sent", p.partner.lastBody)
				}
				wantNoCoverageEditNotMade(t, got.only(t), mapping)
			})
		}
	}
}

// A DTR $questionnaire-package request is read for its "coverage" parameters.
// With no Coverage anywhere, the mapping reports nothing to map
// (errPayorEdgeNoCoverage), which the record names; one whose only Coverage is
// under another parameter name is carried as it always was, but is not reported
// as carrying no Coverage, because it carries one. Read at the mapping itself;
// TestPayorEdge_DTRWithNoCoverageRecordsTheEditNotMade is the inbound row.
func TestPayorEdge_DTRNoCoverageIsReportedOnlyWhenThereIsNone(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(dtrFetchReq, &doc); err != nil {
		t.Fatal(err)
	}
	var others, renamed []any
	found := false
	for _, prm := range doc["parameter"].([]any) {
		m := prm.(map[string]any)
		if m["name"] == "coverage" {
			found = true
			renamed = append(renamed, map[string]any{"name": "insurance", "resource": m["resource"]})
			continue
		}
		others = append(others, prm)
		renamed = append(renamed, prm)
	}
	if !found {
		t.Fatal("fixture: no coverage parameter")
	}
	encode := func(p []any) []byte {
		doc["parameter"] = p
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	own := []shnsdk.PayerIdentifier{ownIdentity}
	ops, err := locatePayorEdge(payorDoc(t, encode(others)), payorEdgeParametersCoverage, own, backendIdentity, nil)
	if ops != nil || !errors.Is(err, errPayorEdgeNoCoverage) {
		t.Fatalf("no Coverage: %v, %v; want errPayorEdgeNoCoverage", ops, err)
	}
	ops, err = locatePayorEdge(payorDoc(t, encode(renamed)), payorEdgeParametersCoverage, own, backendIdentity, nil)
	if ops != nil || err != nil {
		t.Fatalf("a Coverage under another parameter name: %v, %v; want carried with no report", ops, err)
	}
}

// With no Coverage, nothing names this payer to the mapping, so a Claim
// insurer that names this payer's own identity is left as sent: the mapping
// re-stamps an insurer only once a Coverage has named this payer.
func TestPayorEdge_InquiryWithNoCoverageLeavesTheInsurerAsSent(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(withoutCoverage(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), true), &doc); err != nil {
		t.Fatal(err)
	}
	set := false
	for _, e := range doc["entry"].([]any) {
		if r := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "Claim" {
			r["insurer"] = map[string]any{"identifier": map[string]any{"system": ownIdentity.System, "value": ownIdentity.Value}}
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
			p, got := mappingRecordingPayer(t, level, true)
			ans := p.send(t, "pas-claim-inquire", "", body)
			if ans.status != http.StatusOK {
				t.Fatalf("answer %d %s", ans.status, ans.body)
			}
			if !bytes.Equal(p.partner.lastBody, body) {
				t.Fatalf("the payer's system received %s, want the insurer and every other byte as sent", p.partner.lastBody)
			}
			wantNoCoverageEditNotMade(t, got.only(t), true)
		})
	}
}

// A Coverage the mapping's sites do not list is still a Coverage: one whose
// resourceType is spelled in another case, or one nested in another entry's
// Bundle, keeps the inquiry from reading as "no Coverage", so the mapping
// refuses it rather than forward a Coverage it did not re-key.
func TestPayorEdge_InquiryWithACoverageTheMappingDoesNotListIsRefused(t *testing.T) {
	base := inquiryBundle("MBR-COVERED", "", "TRN-1", "72148")
	lower := bytes.Replace(base, []byte(`"resourceType":"Coverage"`), []byte(`"resourceType":"coverage"`), 1)
	if bytes.Equal(lower, base) {
		t.Fatal("fixture: no Coverage resourceType to respell")
	}
	var doc map[string]any
	if err := json.Unmarshal(base, &doc); err != nil {
		t.Fatal(err)
	}
	var kept []any
	var coverageEntry any
	for _, e := range doc["entry"].([]any) {
		if e.(map[string]any)["resource"].(map[string]any)["resourceType"] == "Coverage" {
			coverageEntry = e
			continue
		}
		kept = append(kept, e)
	}
	kept = append(kept, map[string]any{"fullUrl": "urn:uuid:nested", "resource": map[string]any{
		"resourceType": "Bundle", "type": "collection", "entry": []any{coverageEntry}}})
	doc["entry"] = kept
	nested, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"resourceType in another case": lower, "nested in another entry's Bundle": nested} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p, _ := mappingRecordingPayer(t, level, true)
				ans := p.send(t, "pas-claim-inquire", "", body)
				if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), "inbound Coverage carries no resolvable payor identifier") {
					t.Fatalf("answer %d %s, want the mapping's 400", ans.status, ans.body)
				}
				if p.partner.lastBody != nil {
					t.Fatalf("the payer's system received %s, want nothing", p.partner.lastBody)
				}
			})
		}
	}
}

// A DTR $questionnaire-package request with no Coverage that still names its
// patient (on its order) is forwarded at none and observe, and so reaches the
// mapping: it is carried as sent, and with the mapping on its record names the
// edit not made. At structural and strict a request with no coverage is refused
// first (RuleRequestShape).
func TestPayorEdge_DTRWithNoCoverageRecordsTheEditNotMade(t *testing.T) {
	body := []byte(`{"resourceType":"Parameters","parameter":[` +
		`{"name":"order","resource":{"resourceType":"ServiceRequest","id":"sr-1","status":"active","intent":"order","subject":{"reference":"Patient/` + dtrFrameMember + `"}}},` +
		`{"name":"questionnaire","valueCanonical":"http://x/q"}]}`)
	for _, mapping := range []bool{false, true} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
			name := level.String()
			if mapping {
				name += "/mapping on"
			}
			t.Run(name, func(t *testing.T) {
				p, got := mappingRecordingPayer(t, level, mapping)
				ans := p.send(t, "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, body)
				if ans.status != http.StatusOK {
					t.Fatalf("answer %d %s", ans.status, ans.body)
				}
				if !bytes.Equal(p.partner.lastBody, body) {
					t.Fatalf("the payer's system received %s, want the request exactly as sent", p.partner.lastBody)
				}
				wantNoCoverageEditNotMade(t, got.only(t), mapping)
			})
		}
	}
}
