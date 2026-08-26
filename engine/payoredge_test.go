package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// bridgeIdentity/backendIdentity are the two identities the payer-edge identity mapping
// seam's tests translate between — the shape of the live 2026-08-26 defect (a bridge
// persona's Coverage payor vs. the real reference payer's CMS identifier), but with
// invented values so these tests never depend on shnsdk.CMSPayerIdentity's literal
// contents beyond what BuildCoverageWithPayer itself needs.
var (
	ownIdentity     = shnsdk.PayerIdentifier{System: "urn:shn:demo-payer", Value: "SHN-BRIDGE-DEMO"}
	backendIdentity = shnsdk.CMSPayerIdentity
	foreignIdentity = shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "99999"}
)

// crdPartnerCoverageCard is a minimal, mappable CRD partner response (a real
// coverage-information split-shape extension, same fixture shape as
// TestNativeResponder_CRDNativeForwardsVerbatim) — normalizeCRDResponse 502s on an
// unmappable card, so every payor-edge CRD Handle() test needs this, not an empty
// `{"cards":[]}`.
var crdPartnerCoverageCard = []byte(`{"cards":[{"suggestions":[{"actions":[{"resource":{"extension":[` +
	`{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information",` +
	`"extension":[{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"no-auth"}]}]}}]}]}]}`)

// --- restampBareCoveragePayor (CRD/DTR bare-Coverage shape) ---

func TestRestampBareCoveragePayor_ContainedShape_Restamps(t *testing.T) {
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/p1", "MBR-1", ownIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	out, got, gotOK, matched, err := restampBareCoveragePayor(cov, ownIdentity, backendIdentity)
	if err != nil {
		t.Fatalf("restampBareCoveragePayor: %v", err)
	}
	if !matched || !gotOK || got != ownIdentity {
		t.Fatalf("want matched=true got=%v gotOK=true, got matched=%v got=%v gotOK=%v", ownIdentity, matched, got, gotOK)
	}
	newGot, newOK := shnsdk.ParsePayerIdentifier(out, nil)
	if !newOK || newGot != backendIdentity {
		t.Fatalf("restamped coverage payor = %v (ok=%v), want %v", newGot, newOK, backendIdentity)
	}
	// A2: nothing else touched — the contained Organization's name must survive.
	if !bytes.Contains(out, []byte(`"Centers for Medicare and Medicaid Services"`)) {
		t.Errorf("restamp touched the payer Organization's name (A2 violation): %s", out)
	}
}

func TestRestampBareCoveragePayor_InlineShape_Restamps(t *testing.T) {
	cov := []byte(`{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"},"payor":[{"identifier":{"system":"` + ownIdentity.System + `","value":"` + ownIdentity.Value + `"}}]}`)
	out, got, gotOK, matched, err := restampBareCoveragePayor(cov, ownIdentity, backendIdentity)
	if err != nil {
		t.Fatalf("restampBareCoveragePayor: %v", err)
	}
	if !matched || got != ownIdentity || !gotOK {
		t.Fatalf("want a match on the inline identity, got matched=%v got=%v gotOK=%v", matched, got, gotOK)
	}
	newGot, newOK := shnsdk.ParsePayerIdentifier(out, nil)
	if !newOK || newGot != backendIdentity {
		t.Fatalf("restamped inline payor = %v (ok=%v), want %v", newGot, newOK, backendIdentity)
	}
}

func TestRestampBareCoveragePayor_MismatchRefuses(t *testing.T) {
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/p1", "MBR-1", foreignIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	out, got, gotOK, matched, err := restampBareCoveragePayor(cov, ownIdentity, backendIdentity)
	if err != nil {
		t.Fatalf("restampBareCoveragePayor: %v", err)
	}
	if matched {
		t.Fatalf("a foreign payor identity must NOT match own")
	}
	if !gotOK || got != foreignIdentity {
		t.Fatalf("got=%v gotOK=%v, want the resolved foreign identity %v", got, gotOK, foreignIdentity)
	}
	if !bytes.Equal(out, cov) {
		t.Errorf("a refused restamp must return the ORIGINAL bytes unchanged")
	}
}

func TestRestampBareCoveragePayor_NoResolvablePayorRefuses(t *testing.T) {
	cov := []byte(`{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"}}`)
	_, got, gotOK, matched, err := restampBareCoveragePayor(cov, ownIdentity, backendIdentity)
	if err != nil {
		t.Fatalf("restampBareCoveragePayor: %v", err)
	}
	if matched || gotOK {
		t.Fatalf("a Coverage with no payor at all must resolve to gotOK=false, matched=false; got gotOK=%v matched=%v got=%v", gotOK, matched, got)
	}
}

// --- restampPASBundlePayor (PAS $submit Bundle shape) ---

// conformantSubmitBundle builds a $submit Bundle via the SAME SDK builder the
// originator uses, in either the SHN-native default shape (contained payor Org, generic
// unresolvable Claim.insurer) or the demo/kit conformant PayerOrgEntry shape (a
// resolvable Organization bundle entry shared by Coverage.payor AND Claim.insurer).
func conformantSubmitBundle(t *testing.T, payer shnsdk.PayerIdentifier, payerOrgEntry bool) []byte {
	t.Helper()
	sr := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-1"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine w/o contrast"}]}}`)
	b, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		SR: sr, PatientRef: "Patient/MBR-1", CoverageRef: "Coverage/MBR-1", MemberID: "MBR-1",
		Corr: "corr-payoredge", Created: time.Unix(1700000000, 0).UTC(),
		ContainedInsurer: payerOrgEntry, AbsoluteRefs: payerOrgEntry, PayerOrgEntry: payerOrgEntry,
		Payer: payer,
	})
	if err != nil {
		t.Fatalf("conformantSubmitBundle: %v", err)
	}
	return b
}

func bundleCoveragePayor(t *testing.T, bundleJSON []byte) (shnsdk.PayerIdentifier, bool) {
	t.Helper()
	b, err := parsePayorEdgeBundle(bundleJSON)
	if err != nil {
		t.Fatalf("parsePayorEdgeBundle: %v", err)
	}
	idx := b.findIndex("Coverage")
	if idx < 0 {
		t.Fatalf("bundle has no Coverage entry: %s", bundleJSON)
	}
	return shnsdk.ParsePayerIdentifier(b.entries[idx].Resource, b.resolveRef)
}

func bundleClaimInsurerOrg(t *testing.T, bundleJSON []byte) (shnsdk.PayerIdentifier, bool) {
	t.Helper()
	b, err := parsePayorEdgeBundle(bundleJSON)
	if err != nil {
		t.Fatalf("parsePayorEdgeBundle: %v", err)
	}
	idx := b.findIndex("Claim")
	if idx < 0 {
		t.Fatalf("bundle has no Claim entry: %s", bundleJSON)
	}
	var claim struct {
		Insurer struct {
			Reference string `json:"reference"`
		} `json:"insurer"`
	}
	if err := json.Unmarshal(b.entries[idx].Resource, &claim); err != nil {
		t.Fatalf("parse claim: %v", err)
	}
	if orgJSON, ok := b.resolveRef(claim.Insurer.Reference); ok {
		return shnsdk.ParseOrganizationIdentifier(orgJSON)
	}
	return shnsdk.PayerIdentifier{}, false
}

func TestRestampPASBundlePayor_ContainedShape_Restamps(t *testing.T) {
	bundle := conformantSubmitBundle(t, ownIdentity, false)
	out, got, gotOK, matched, err := restampPASBundlePayor(bundle, ownIdentity, backendIdentity)
	if err != nil {
		t.Fatalf("restampPASBundlePayor: %v", err)
	}
	if !matched || !gotOK || got != ownIdentity {
		t.Fatalf("want a match on own identity, got matched=%v got=%v gotOK=%v", matched, got, gotOK)
	}
	newGot, newOK := bundleCoveragePayor(t, out)
	if !newOK || newGot != backendIdentity {
		t.Fatalf("restamped Coverage.payor = %v (ok=%v), want %v", newGot, newOK, backendIdentity)
	}
}

func TestRestampPASBundlePayor_PayerOrgEntryShape_RestampsCoverageAndInsurer(t *testing.T) {
	bundle := conformantSubmitBundle(t, ownIdentity, true)
	out, got, gotOK, matched, err := restampPASBundlePayor(bundle, ownIdentity, backendIdentity)
	if err != nil {
		t.Fatalf("restampPASBundlePayor: %v", err)
	}
	if !matched || !gotOK || got != ownIdentity {
		t.Fatalf("want a match on own identity, got matched=%v got=%v gotOK=%v", matched, got, gotOK)
	}
	newCov, ok := bundleCoveragePayor(t, out)
	if !ok || newCov != backendIdentity {
		t.Fatalf("restamped Coverage.payor = %v (ok=%v), want %v", newCov, ok, backendIdentity)
	}
	newInsurer, ok := bundleClaimInsurerOrg(t, out)
	if !ok || newInsurer != backendIdentity {
		t.Fatalf("restamped Claim.insurer org = %v (ok=%v), want %v (the PayerOrgEntry shape shares ONE entry between Coverage.payor and Claim.insurer)", newInsurer, ok, backendIdentity)
	}
	// A2: the payer Organization's name must survive the restamp untouched.
	if !bytes.Contains(out, []byte(`"Centers for Medicare and Medicaid Services"`)) {
		t.Errorf("restamp touched the payer Organization's name (A2 violation): %s", out)
	}
}

func TestRestampPASBundlePayor_MismatchRefuses(t *testing.T) {
	bundle := conformantSubmitBundle(t, foreignIdentity, true)
	out, got, gotOK, matched, err := restampPASBundlePayor(bundle, ownIdentity, backendIdentity)
	if err != nil {
		t.Fatalf("restampPASBundlePayor: %v", err)
	}
	if matched {
		t.Fatalf("a foreign bundle payor must NOT match own")
	}
	if !gotOK || got != foreignIdentity {
		t.Fatalf("got=%v gotOK=%v, want the resolved foreign identity %v", got, gotOK, foreignIdentity)
	}
	if !bytes.Equal(out, bundle) {
		t.Errorf("a refused restamp must return the ORIGINAL bundle bytes unchanged")
	}
}

// --- Handle()-level rows: (b) A1 guard, (c) seam-off, (d) affirmative re-stamp ---

// TestNativeResponder_PayorEdge_CRD_SeamOff proves row (c)'s premise for the CRD leg in
// isolation: with the seam unconfigured, a bridge-identity Coverage forwards VERBATIM
// (the byte-identical fence every existing deployment relies on).
func TestNativeResponder_PayorEdge_CRD_SeamOff(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/cds-services/order-sign"] = crdPartnerCoverageCard
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "order-sign", nil, nil)
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/p1", "MBR-1", ownIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	req := []byte(`{"hook":"order-sign","context":{"draftOrders":{"resourceType":"Bundle","entry":[]}},"prefetch":{"coverage":` + string(cov) + `}}`)
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("seam off must forward, got refusal status=%d msg=%s", res.Status, res.Message)
	}
	gotSent, ok := shnsdk.ParsePayerIdentifier(mustExtractPrefetchCoverage(t, p.lastBody), nil)
	if !ok || gotSent != ownIdentity {
		t.Fatalf("seam off must forward the payor VERBATIM; sent=%v ok=%v, want %v", gotSent, ok, ownIdentity)
	}
}

// TestNativeResponder_PayorEdge_CRD_MismatchRefuses proves row (b) for the CRD leg: the
// seam configured, a mismatched inbound payor refuses BEFORE any bytes reach the
// partner (refuse-before-forward, mirroring the foreign-peer version filter's own
// posture in this file).
func TestNativeResponder_PayorEdge_CRD_MismatchRefuses(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/cds-services/order-sign"] = []byte(`{"cards":[]}`)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "order-sign", nil, nil, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/p1", "MBR-1", foreignIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	req := []byte(`{"hook":"order-sign","context":{"draftOrders":{"resourceType":"Bundle","entry":[]}},"prefetch":{"coverage":` + string(cov) + `}}`)
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", req)
	if err != nil {
		t.Fatalf("Handle must return a LegResult refusal, not a bare error: %v", err)
	}
	if res.Status != 400 {
		t.Fatalf("mismatched payor must refuse with a 400-class LegResult, got status=%d", res.Status)
	}
	if p.lastBody != nil {
		t.Fatalf("mismatched payor must refuse BEFORE forwarding any bytes; partner received: %s", p.lastBody)
	}
}

// TestNativeResponder_PayorEdge_CRD_AbsentPayorRefuses proves the "no resolvable payor
// identifier at all" arm of A1.
func TestNativeResponder_PayorEdge_CRD_AbsentPayorRefuses(t *testing.T) {
	p := newStubPartner(t)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "order-sign", nil, nil, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
	req := []byte(`{"hook":"order-sign","context":{"draftOrders":{"resourceType":"Bundle","entry":[]}},"prefetch":{"coverage":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/p1"}}}}`)
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", req)
	if err != nil {
		t.Fatalf("Handle must return a LegResult refusal, not a bare error: %v", err)
	}
	if res.Status != 400 {
		t.Fatalf("absent payor must refuse with a 400-class LegResult, got status=%d", res.Status)
	}
	if p.lastBody != nil {
		t.Fatalf("absent payor must refuse BEFORE forwarding any bytes; partner received: %s", p.lastBody)
	}
}

// TestNativeResponder_PayorEdge_CRD_OwnIdentityRestamps proves row (d): the seam
// configured, an own-identity exchange re-stamps the wire bytes the partner receives to
// the BACKEND identity — asserted AFFIRMATIVELY on the received bytes, not by absence.
func TestNativeResponder_PayorEdge_CRD_OwnIdentityRestamps(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/cds-services/order-sign"] = crdPartnerCoverageCard
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "order-sign", nil, nil, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/p1", "MBR-1", ownIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	req := []byte(`{"hook":"order-sign","context":{"draftOrders":{"resourceType":"Bundle","entry":[]}},"prefetch":{"coverage":` + string(cov) + `}}`)
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("own-identity exchange must complete, got refusal status=%d msg=%s", res.Status, res.Message)
	}
	gotSent, ok := shnsdk.ParsePayerIdentifier(mustExtractPrefetchCoverage(t, p.lastBody), nil)
	if !ok || gotSent != backendIdentity {
		t.Fatalf("the bytes the partner received must carry the BACKEND identity; sent=%v ok=%v, want %v", gotSent, ok, backendIdentity)
	}
}

func mustExtractPrefetchCoverage(t *testing.T, reqJSON []byte) json.RawMessage {
	t.Helper()
	var m struct {
		Prefetch struct {
			Coverage json.RawMessage `json:"coverage"`
		} `json:"prefetch"`
	}
	if err := json.Unmarshal(reqJSON, &m); err != nil {
		t.Fatalf("parse forwarded CRD request: %v (%s)", err, reqJSON)
	}
	return m.Prefetch.Coverage
}

// TestNativeResponder_PayorEdge_DTR_SeamOffAndRestamp covers the DTR leg's (c) seam-off
// and (d) own-identity-restamp rows in one table, mirroring the CRD coverage above.
func TestNativeResponder_PayorEdge_DTR_SeamOffAndRestamp(t *testing.T) {
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/p1", "MBR-1", ownIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	reqFHIR, err := json.Marshal(shnsdk.QuestionnaireFetchRequest{Canonical: "http://x/q", Coverage: cov})
	if err != nil {
		t.Fatalf("marshal fetch request: %v", err)
	}

	t.Run("seam off forwards verbatim", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)
		if _, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", reqFHIR); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		got, ok := shnsdk.ParsePayerIdentifier(mustExtractDTRCoverageParam(t, p.lastBody), nil)
		if !ok || got != ownIdentity {
			t.Fatalf("seam off must forward the payor VERBATIM; sent=%v ok=%v, want %v", got, ok, ownIdentity)
		}
	})

	t.Run("own identity restamps to backend", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
		res, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", reqFHIR)
		if err != nil || res.Status != 0 {
			t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		got, ok := shnsdk.ParsePayerIdentifier(mustExtractDTRCoverageParam(t, p.lastBody), nil)
		if !ok || got != backendIdentity {
			t.Fatalf("the bytes the partner received must carry the BACKEND identity; sent=%v ok=%v, want %v", got, ok, backendIdentity)
		}
	})

	t.Run("mismatch refuses before forwarding", func(t *testing.T) {
		p := newStubPartner(t)
		foreignCov, ferr := shnsdk.BuildCoverageWithPayer("Patient/p1", "MBR-1", foreignIdentity)
		if ferr != nil {
			t.Fatalf("BuildCoverageWithPayer: %v", ferr)
		}
		foreignReq, merr := json.Marshal(shnsdk.QuestionnaireFetchRequest{Canonical: "http://x/q", Coverage: foreignCov})
		if merr != nil {
			t.Fatalf("marshal: %v", merr)
		}
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
		res, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", foreignReq)
		if err != nil {
			t.Fatalf("Handle must return a LegResult refusal, not a bare error: %v", err)
		}
		if res.Status != 400 {
			t.Fatalf("mismatched payor must refuse with a 400-class LegResult, got status=%d", res.Status)
		}
		if p.lastBody != nil {
			t.Fatalf("mismatched payor must refuse BEFORE forwarding any bytes; partner received: %s", p.lastBody)
		}
	})

	t.Run("no coverage carried stays a benign pass-through (seam on)", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
		noCovReq, merr := json.Marshal(shnsdk.QuestionnaireFetchRequest{Canonical: "http://x/q"})
		if merr != nil {
			t.Fatalf("marshal: %v", merr)
		}
		res, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", noCovReq)
		if err != nil || res.Status != 0 {
			t.Fatalf("a DTR fetch legitimately carrying no coverage must still forward: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
	})
}

func mustExtractDTRCoverageParam(t *testing.T, reqJSON []byte) json.RawMessage {
	t.Helper()
	var got struct {
		Parameter []struct {
			Name     string          `json:"name"`
			Resource json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if err := json.Unmarshal(reqJSON, &got); err != nil {
		t.Fatalf("forwarded body not Parameters: %v (%s)", err, reqJSON)
	}
	for _, pr := range got.Parameter {
		if pr.Name == "coverage" {
			return pr.Resource
		}
	}
	t.Fatalf("forwarded $questionnaire-package missing coverage parameter: %s", reqJSON)
	return nil
}

// TestNativeSubmit_PayorEdge_SeamOffAndRestamp covers the PAS submit leg's (c)/(b)/(d)
// rows, using the SAME conformantSubmitBundle builder as the unit tests above so the
// Handle()-level proof exercises the real posted wire bytes.
func TestNativeSubmit_PayorEdge_SeamOffAndRestamp(t *testing.T) {
	approvedBody := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1"}`)

	t.Run("seam off forwards verbatim", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Claim/$submit"] = approvedBody
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
		bundle := conformantSubmitBundle(t, ownIdentity, true)
		res, err := n.Handle(context.Background(), "pas-claim", "corr", "MBR-1", bundle)
		if err != nil || res.Status != 0 {
			t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		got, ok := bundleCoveragePayor(t, p.lastBody)
		if !ok || got != ownIdentity {
			t.Fatalf("seam off must forward the payor VERBATIM; sent=%v ok=%v, want %v", got, ok, ownIdentity)
		}
	})

	t.Run("own identity restamps to backend", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Claim/$submit"] = approvedBody
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", newCensusSoR(), fixedClock, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
		bundle := conformantSubmitBundle(t, ownIdentity, true)
		res, err := n.Handle(context.Background(), "pas-claim", "corr", "MBR-1", bundle)
		if err != nil || res.Status != 0 {
			t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		got, ok := bundleCoveragePayor(t, p.lastBody)
		if !ok || got != backendIdentity {
			t.Fatalf("the bytes the partner received must carry the BACKEND identity; sent=%v ok=%v, want %v", got, ok, backendIdentity)
		}
	})

	t.Run("mismatch refuses before forwarding", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Claim/$submit"] = approvedBody
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", newCensusSoR(), fixedClock, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
		bundle := conformantSubmitBundle(t, foreignIdentity, true)
		res, err := n.Handle(context.Background(), "pas-claim", "corr", "MBR-1", bundle)
		if err != nil {
			t.Fatalf("Handle must return a LegResult refusal, not a bare error: %v", err)
		}
		if res.Status != 400 {
			t.Fatalf("mismatched payor must refuse with a 400-class LegResult, got status=%d", res.Status)
		}
		if p.lastBody != nil {
			t.Fatalf("mismatched payor must refuse BEFORE forwarding any bytes; partner received: %s", p.lastBody)
		}
	})
}

// conformantUpdateBundle builds a $submit-amendment (pas-claim-update) Bundle via the SAME
// SDK builder the originator uses, with a caller-chosen payer identity — the
// pas-claim-update sibling of conformantSubmitBundle (Finding 1, task1-review.md: the
// update leg's payor-edge seam had no dedicated test). Reuses the committed conformant
// update golden for the QR/DiagnosticReport/Provenance entries — their content is
// irrelevant to the payor-edge seam; only Coverage.payor/Claim.insurer (and
// Claim.related[prior], which BeginClaimUpdate keys on) matter here.
func conformantUpdateBundle(t *testing.T, payer shnsdk.PayerIdentifier, payerOrgEntry bool, corr, originalCorr string) []byte {
	t.Helper()
	const member = "MBR-1"
	created := time.Unix(1700000000, 0).UTC()
	goldenBytes := readConformantGolden(t, "pas-update-request.json")
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(goldenBytes, &bundle); err != nil {
		t.Fatalf("parse conformant update golden: %v", err)
	}
	var qrJSON, drJSON, provJSON []byte
	for _, e := range bundle.Entry {
		var rt struct {
			ResourceType string `json:"resourceType"`
		}
		if json.Unmarshal(e.Resource, &rt) != nil {
			continue
		}
		switch rt.ResourceType {
		case "QuestionnaireResponse":
			qrJSON = e.Resource
		case "DiagnosticReport":
			drJSON = e.Resource
		case "Provenance":
			provJSON = e.Resource
		}
	}
	if qrJSON == nil || drJSON == nil || provJSON == nil {
		t.Fatal("conformant update golden missing QR/DR/Provenance entry")
	}
	ref := "Patient/" + member
	sr := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active","intent":"order","subject":{"reference":"` + ref + `"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine w/o contrast"}]}}`)
	b, err := shnsdk.BuildConformantClaimUpdateBundle(shnsdk.ConformantClaimUpdateInputs{
		QR: qrJSON, SR: sr, PatientRef: ref, CoverageRef: "Coverage/" + member, MemberID: member,
		Provenance: provJSON, DiagnosticReport: drJSON,
		Corr: corr, OriginalCorr: originalCorr,
		Created:          created,
		ContainedInsurer: payerOrgEntry, AbsoluteRefs: payerOrgEntry, PayerOrgEntry: payerOrgEntry,
		Payer: payer,
	})
	if err != nil {
		t.Fatalf("conformantUpdateBundle: %v", err)
	}
	return b
}

// TestNativeUpdate_PayorEdge_SeamOffAndRestamp is the pas-claim-update analog of
// TestNativeSubmit_PayorEdge_SeamOffAndRestamp (task1-review.md Finding 1): drives
// Handle(ctx, "pas-claim-update", ...) through the seam and asserts AFFIRMATIVELY on the
// bytes the backend receives, plus the A1 refusal on the same leg — and, uniquely to the
// update leg, that a refusal never consumes the pended claim (the guard runs BEFORE
// BeginClaimUpdate, per nativepas.go's ordering comment).
func TestNativeUpdate_PayorEdge_SeamOffAndRestamp(t *testing.T) {
	approvedBody := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`)
	const pci = "PCI-PAYOREDGE-UPDATE"
	const origCorr = "corr-payoredge-submit"

	seedPended := func() *censusSoR {
		s := newCensusSoR()
		if err := s.RecordPendedClaim(pci, origCorr); err != nil {
			t.Fatalf("RecordPendedClaim: %v", err)
		}
		return s
	}

	t.Run("seam off forwards verbatim", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Claim/$submit"] = approvedBody
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", seedPended(), fixedClock)
		bundle := conformantUpdateBundle(t, ownIdentity, true, "corr-payoredge-update", origCorr)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr", pci, bundle)
		if err != nil || res.Status != 0 {
			t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		got, ok := bundleCoveragePayor(t, p.lastBody)
		if !ok || got != ownIdentity {
			t.Fatalf("seam off must forward the payor VERBATIM; sent=%v ok=%v, want %v", got, ok, ownIdentity)
		}
	})

	t.Run("own identity restamps to backend", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Claim/$submit"] = approvedBody
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", seedPended(), fixedClock, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
		bundle := conformantUpdateBundle(t, ownIdentity, true, "corr-payoredge-update", origCorr)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr", pci, bundle)
		if err != nil || res.Status != 0 {
			t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		got, ok := bundleCoveragePayor(t, p.lastBody)
		if !ok || got != backendIdentity {
			t.Fatalf("the bytes the backend received must carry the BACKEND identity; sent=%v ok=%v, want %v", got, ok, backendIdentity)
		}
	})

	t.Run("mismatch refuses before forwarding, claim not stranded", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Claim/$submit"] = approvedBody
		s := seedPended()
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", s, fixedClock, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
		bundle := conformantUpdateBundle(t, foreignIdentity, true, "corr-payoredge-update", origCorr)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr", pci, bundle)
		if err != nil {
			t.Fatalf("Handle must return a LegResult refusal, not a bare error: %v", err)
		}
		if res.Status != 400 {
			t.Fatalf("mismatched payor must refuse with a 400-class LegResult, got status=%d", res.Status)
		}
		if p.lastBody != nil {
			t.Fatalf("mismatched payor must refuse BEFORE forwarding any bytes; partner received: %s", p.lastBody)
		}
		// The A1 guard must run BEFORE BeginClaimUpdate (nativepas.go's ordering — a
		// regression that moved it after Begin would strand or double-consume the pend):
		// the pended claim must still be claimable after the refusal.
		claimed, err := s.BeginClaimUpdate(pci, origCorr)
		if err != nil {
			t.Fatalf("BeginClaimUpdate after refusal: %v", err)
		}
		if !claimed {
			t.Fatalf("the pended claim must still be claimable after an A1 refusal — the guard must run BEFORE BeginClaimUpdate")
		}
	})
}
