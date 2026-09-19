package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
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

// crdPartnerCoverageCard is a minimal valid CRD answer in the reference payer's
// shape: no card, and the order returned with its coverage information in an
// update system action. The payer gateway relays a valid CDS Hooks answer
// exactly and refuses one that is not, so every payor-edge CRD Handle() test
// answers with this.
var crdPartnerCoverageCard = []byte(`{"cards":[],"systemActions":[{"type":"update","description":"Add coverage information",` +
	`"resource":{"resourceType":"ServiceRequest","id":"sr1","extension":[` +
	`{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information",` +
	`"extension":[{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"no-auth"}]}]}}]}`)

// --- the mapping over a bare Coverage (the questionnaire request's coverage) ---

// mapBareCoverage runs the mapping over a questionnaire request envelope carrying cov,
// returning the mapped Coverage bytes (nil on a refusal) and the refusal.
func mapBareCoverage(t *testing.T, cov []byte, own, backend shnsdk.PayerIdentifier) ([]byte, LegResult) {
	t.Helper()
	req, err := json.Marshal(shnsdk.QuestionnaireFetchRequest{Canonical: "http://x/q", Coverage: cov})
	if err != nil {
		t.Fatalf("marshal fetch request: %v", err)
	}
	n := NewNativeResponder(nil, "", "order-sign", nil, nil, WithPayorEdgeIdentity(own, backend))
	p, lr, err := n.payorEdgeRequest(peerBody(req), payorEdgeDTRFetch, "application/json")
	if err != nil {
		t.Fatalf("payorEdgeRequest: %v", err)
	}
	if lr.Status != 0 {
		return nil, lr
	}
	var out dtrLegRequest
	if err := json.Unmarshal(relay.BytesForTest(p), &out); err != nil {
		t.Fatalf("mapped request: %v", err)
	}
	return out.Coverage, lr
}

func TestPayorEdgeBareCoverage_ContainedShape_Maps(t *testing.T) {
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/p1", "MBR-1", ownIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	out, lr := mapBareCoverage(t, cov, ownIdentity, backendIdentity)
	if lr.Status != 0 {
		t.Fatalf("refused: %d %s", lr.Status, lr.Message)
	}
	newGot, newOK := shnsdk.ParsePayerIdentifier(out, nil)
	if !newOK || newGot != backendIdentity {
		t.Fatalf("mapped coverage payor = %v (ok=%v), want %v", newGot, newOK, backendIdentity)
	}
	// Nothing else touched — the contained Organization's name must survive.
	if !bytes.Contains(out, []byte(`"Centers for Medicare and Medicaid Services"`)) {
		t.Errorf("the mapping touched the payer Organization's name: %s", out)
	}
}

func TestPayorEdgeBareCoverage_InlineShape_Maps(t *testing.T) {
	cov := []byte(`{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"},"payor":[{"identifier":{"system":"` + ownIdentity.System + `","value":"` + ownIdentity.Value + `"}}]}`)
	out, lr := mapBareCoverage(t, cov, ownIdentity, backendIdentity)
	if lr.Status != 0 {
		t.Fatalf("refused: %d %s", lr.Status, lr.Message)
	}
	newGot, newOK := shnsdk.ParsePayerIdentifier(out, nil)
	if !newOK || newGot != backendIdentity {
		t.Fatalf("mapped inline payor = %v (ok=%v), want %v", newGot, newOK, backendIdentity)
	}
}

func TestPayorEdgeBareCoverage_MismatchRefuses(t *testing.T) {
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/p1", "MBR-1", foreignIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	_, lr := mapBareCoverage(t, cov, ownIdentity, backendIdentity)
	if lr.Status != 400 || !strings.Contains(lr.Message, foreignIdentity.Value) {
		t.Fatalf("a foreign payor identity must be refused naming it, got %d %q", lr.Status, lr.Message)
	}
}

func TestPayorEdgeBareCoverage_NoResolvablePayorRefuses(t *testing.T) {
	cov := []byte(`{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"}}`)
	_, lr := mapBareCoverage(t, cov, ownIdentity, backendIdentity)
	if lr.Status != 400 || !strings.Contains(lr.Message, "no resolvable payor identifier") {
		t.Fatalf("a Coverage with no payor must be refused, got %d %q", lr.Status, lr.Message)
	}
}

// --- the mapping over a PAS $submit Bundle ---

// conformantSubmitBundle builds a $submit Bundle via the SAME SDK builder the
// originator uses, in either the SDK's default shape (contained payor Org, a Claim.insurer
// that resolves to nothing in the Bundle) or the conformant PayerOrgEntry shape (a
// resolvable Organization bundle entry shared by Coverage.payor AND Claim.insurer).
func conformantSubmitBundle(t *testing.T, payer shnsdk.PayerIdentifier, payerOrgEntry bool) []byte {
	t.Helper()
	sr := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-1"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine w/o contrast"}]}}`)
	b, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{Coverage: testMemberCoverage("MBR-1"),
		Provider:       testRequestingProvider(),
		MemberIDSystem: shnsdk.MemberSystem,
		SR:             sr, PatientRef: "Patient/MBR-1", CoverageRef: "Coverage/MBR-1", MemberID: "MBR-1",
		Corr: "corr-payoredge", Created: time.Unix(1700000000, 0).UTC(),
		ContainedInsurer: payerOrgEntry, AbsoluteRefs: payerOrgEntry, PayerOrgEntry: payerOrgEntry,
		Insurer: testPayerOrganization(payer),
		Payer:   payer,
	})
	if err != nil {
		t.Fatalf("conformantSubmitBundle: %v", err)
	}
	return b
}

// bundleResources returns the Bundle's entry resources and a resolver for a reference
// to one of them (by fullUrl or relative Type/id).
func bundleResources(t *testing.T, bundleJSON []byte) ([]json.RawMessage, func(ref string) ([]byte, bool)) {
	t.Helper()
	var b struct {
		Entry []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		t.Fatalf("parse bundle: %v", err)
	}
	var out []json.RawMessage
	for _, e := range b.Entry {
		out = append(out, e.Resource)
	}
	resolve := func(ref string) ([]byte, bool) {
		for _, e := range b.Entry {
			rt, id := resourceHead(e.Resource)
			if (e.FullURL != "" && ref == e.FullURL) || (rt != "" && id != "" && ref == rt+"/"+id) {
				return e.Resource, true
			}
		}
		return nil, false
	}
	return out, resolve
}

func resourceHead(raw json.RawMessage) (string, string) {
	var p struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.ResourceType, p.ID
}

func bundleResource(t *testing.T, bundleJSON []byte, resourceType string) json.RawMessage {
	t.Helper()
	resources, _ := bundleResources(t, bundleJSON)
	for _, r := range resources {
		if rt, _ := resourceHead(r); rt == resourceType {
			return r
		}
	}
	t.Fatalf("bundle has no %s entry: %s", resourceType, bundleJSON)
	return nil
}

func bundleCoveragePayor(t *testing.T, bundleJSON []byte) (shnsdk.PayerIdentifier, bool) {
	t.Helper()
	_, resolve := bundleResources(t, bundleJSON)
	return shnsdk.ParsePayerIdentifier(bundleResource(t, bundleJSON, "Coverage"), resolve)
}

func bundleClaimInsurerOrg(t *testing.T, bundleJSON []byte) (shnsdk.PayerIdentifier, bool) {
	t.Helper()
	_, resolve := bundleResources(t, bundleJSON)
	var claim struct {
		Insurer struct {
			Reference string `json:"reference"`
		} `json:"insurer"`
	}
	if err := json.Unmarshal(bundleResource(t, bundleJSON, "Claim"), &claim); err != nil {
		t.Fatalf("parse claim: %v", err)
	}
	if orgJSON, ok := resolve(claim.Insurer.Reference); ok {
		return shnsdk.ParseOrganizationIdentifier(orgJSON)
	}
	return shnsdk.PayerIdentifier{}, false
}

// mapPASBundle runs the mapping over a $submit Bundle.
func mapPASBundle(t *testing.T, bundle []byte, own, backend shnsdk.PayerIdentifier) ([]byte, LegResult) {
	t.Helper()
	n := NewNativeResponder(nil, "", "order-sign", nil, nil, WithPayorEdgeIdentity(own, backend))
	p, lr, err := n.payorEdgeRequest(peerBody(bundle), payorEdgePASBundle, "application/fhir+json")
	if err != nil {
		t.Fatalf("payorEdgeRequest: %v", err)
	}
	if lr.Status != 0 {
		return nil, lr
	}
	return relay.BytesForTest(p), lr
}

// The SDK's default shape: the Claim.insurer reference ("Organization/payer") resolves to
// nothing in the Bundle, so the request is refused naming it rather than mapped around it.
func TestPayorEdgePASBundle_UnresolvedInsurerRefused(t *testing.T) {
	bundle := conformantSubmitBundle(t, ownIdentity, false)
	_, lr := mapPASBundle(t, bundle, ownIdentity, backendIdentity)
	if lr.Status != 422 || !strings.Contains(lr.Message, `"Organization/payer"`) {
		t.Fatalf("an unresolved insurer reference must be refused naming it, got %d %q", lr.Status, lr.Message)
	}
}

// The contained shape: the Coverage and the Claim each contain the payer Organization, so
// both contained identifiers are mapped.
func TestPayorEdgePASBundle_ContainedShape_Maps(t *testing.T) {
	sr := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-1"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine w/o contrast"}]}}`)
	bundle, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{Coverage: testMemberCoverage("MBR-1"),
		Provider:       testRequestingProvider(),
		MemberIDSystem: shnsdk.MemberSystem,
		SR:             sr, PatientRef: "Patient/MBR-1", CoverageRef: "Coverage/MBR-1", MemberID: "MBR-1",
		Corr: "corr-payoredge", Created: time.Unix(1700000000, 0).UTC(),
		ContainedInsurer: true, Payer: ownIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, lr := mapPASBundle(t, bundle, ownIdentity, backendIdentity)
	if lr.Status != 0 {
		t.Fatalf("refused: %d %s", lr.Status, lr.Message)
	}
	newGot, newOK := bundleCoveragePayor(t, out)
	if !newOK || newGot != backendIdentity {
		t.Fatalf("mapped Coverage.payor = %v (ok=%v), want %v", newGot, newOK, backendIdentity)
	}
	if bytes.Contains(out, []byte(ownIdentity.Value)) {
		t.Fatalf("an identifier naming this payer was left unmapped: %s", out)
	}
}

func TestPayorEdgePASBundle_PayerOrgEntryShape_MapsCoverageAndInsurer(t *testing.T) {
	bundle := conformantSubmitBundle(t, ownIdentity, true)
	out, lr := mapPASBundle(t, bundle, ownIdentity, backendIdentity)
	if lr.Status != 0 {
		t.Fatalf("refused: %d %s", lr.Status, lr.Message)
	}
	newCov, ok := bundleCoveragePayor(t, out)
	if !ok || newCov != backendIdentity {
		t.Fatalf("mapped Coverage.payor = %v (ok=%v), want %v", newCov, ok, backendIdentity)
	}
	newInsurer, ok := bundleClaimInsurerOrg(t, out)
	if !ok || newInsurer != backendIdentity {
		t.Fatalf("mapped Claim.insurer org = %v (ok=%v), want %v (the PayerOrgEntry shape shares ONE entry between Coverage.payor and Claim.insurer)", newInsurer, ok, backendIdentity)
	}
	// The payer Organization's name must survive the mapping untouched.
	if !bytes.Contains(out, []byte(`"Centers for Medicare and Medicaid Services"`)) {
		t.Errorf("the mapping touched the payer Organization's name: %s", out)
	}
}

func TestPayorEdgePASBundle_MismatchRefuses(t *testing.T) {
	bundle := conformantSubmitBundle(t, foreignIdentity, true)
	_, lr := mapPASBundle(t, bundle, ownIdentity, backendIdentity)
	if lr.Status != 400 || !strings.Contains(lr.Message, foreignIdentity.Value) {
		t.Fatalf("a foreign bundle payor must be refused naming it, got %d %q", lr.Status, lr.Message)
	}
}

// The Claim's insurer is mapped only when it names this payer: an insurer naming
// another identity is not rewritten to this payer's backend identity.
func TestPayorEdgePASBundle_InsurerNamingAnotherIdentityLeftAsSent(t *testing.T) {
	bundle := conformantSubmitBundle(t, ownIdentity, true)
	// Split the shared entry: the insurer points at a second Organization entry that
	// names a different payer.
	var b map[string]any
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatal(err)
	}
	entries := b["entry"].([]any)
	var claim map[string]any
	for _, e := range entries {
		r := e.(map[string]any)["resource"].(map[string]any)
		if r["resourceType"] == "Claim" {
			claim = r
		}
	}
	claim["insurer"] = map[string]any{"reference": "Organization/other-insurer"}
	b["entry"] = append(entries, map[string]any{
		"fullUrl": "urn:uuid:5b0f2a54-1b8e-4d8e-9d0a-3c1f0b7c1a11",
		"resource": map[string]any{"resourceType": "Organization", "id": "other-insurer",
			"identifier": []any{map[string]any{"system": foreignIdentity.System, "value": foreignIdentity.Value}}},
	})
	split, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	out, lr := mapPASBundle(t, split, ownIdentity, backendIdentity)
	if lr.Status != 0 {
		t.Fatalf("refused: %d %s", lr.Status, lr.Message)
	}
	if got, ok := bundleClaimInsurerOrg(t, out); !ok || got != foreignIdentity {
		t.Fatalf("insurer org = %v (ok=%v), want it left as %v", got, ok, foreignIdentity)
	}
	if got, ok := bundleCoveragePayor(t, out); !ok || got != backendIdentity {
		t.Fatalf("Coverage.payor = %v (ok=%v), want %v", got, ok, backendIdentity)
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
	approvedBody = fixturePASResponse(t, approvedBody, true)

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

// inquiryBundleWithPayor builds a prior-authorization inquiry Bundle whose Coverage
// names payer inline — the shape the payer-identity seam maps on this leg. The
// inquiry rides the SAME carrier as a submission (its Bundle carries the Coverage
// entries and, where present, the Claim insurer), so this exercises the registered
// edit on the inquiry leg rather than re-deriving it.
func inquiryBundleWithPayor(payer shnsdk.PayerIdentifier) []byte {
	return []byte(`{"resourceType":"Bundle","type":"collection",
		"identifier":{"system":"http://provider.example/inq","value":"INQ-PE-1"},
		"timestamp":"2026-09-18T00:00:00Z",
		"entry":[
		  {"fullUrl":"urn:uuid:claim","resource":{"resourceType":"Claim","use":"preauthorization",
		    "identifier":[{"system":"http://provider.example/inq","value":"INQUIRY-TRN"}],
		    "patient":{"reference":"Patient/MBR-1"},
		    "item":[{"sequence":1,
		      "productOrService":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine"}]},
		      "extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber",
		        "valueIdentifier":{"system":"http://provider.example/trn","value":"TRN-PE-1"}}]}]}},
		  {"fullUrl":"urn:uuid:patient","resource":{"resourceType":"Patient","id":"MBR-1"}},
		  {"fullUrl":"urn:uuid:coverage","resource":{"resourceType":"Coverage",
		    "beneficiary":{"reference":"Patient/MBR-1"},
		    "payor":[{"identifier":{"system":"` + payer.System + `","value":"` + payer.Value + `"}}]}}]}`)
}

// TestNativeInquire_PayorEdge_SeamOffAndRestamp is the inquiry leg's own
// payer-identity-seam proof, affirmative on all three arms: seam off forwards the
// payor verbatim, this payer's own identity is restamped to its backend's, and a
// Coverage naming ANOTHER payer refuses before a byte is forwarded. The behaviour
// is inherited — the inquiry rides the same registered edit and the same carrier
// as a submission — but inheritance is not evidence, and the other legs each have
// these rows.
func TestNativeInquire_PayorEdge_SeamOffAndRestamp(t *testing.T) {
	answer := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"ClaimResponse","outcome":"complete","patient":{"reference":"Patient/SubscriberExample"}}}]}`)

	t.Run("seam off forwards verbatim", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Claim/$inquire"] = answer
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim-inquire", "corr", "MBR-1", inquiryBundleWithPayor(ownIdentity))
		if err != nil || res.Status != 0 {
			t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if p.lastPath != "/Claim/$inquire" {
			t.Fatalf("posted to %q, want /Claim/$inquire", p.lastPath)
		}
		got, ok := bundleCoveragePayor(t, p.lastBody)
		if !ok || got != ownIdentity {
			t.Fatalf("seam off must forward the payor VERBATIM; sent=%v ok=%v, want %v", got, ok, ownIdentity)
		}
	})

	t.Run("own identity restamps to backend", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Claim/$inquire"] = answer
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", newCensusSoR(), fixedClock, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
		res, err := n.Handle(context.Background(), "pas-claim-inquire", "corr", "MBR-1", inquiryBundleWithPayor(ownIdentity))
		if err != nil || res.Status != 0 {
			t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		got, ok := bundleCoveragePayor(t, p.lastBody)
		if !ok || got != backendIdentity {
			t.Fatalf("the bytes the payer's system received must carry the BACKEND identity; sent=%v ok=%v, want %v", got, ok, backendIdentity)
		}
		if bytes.Contains(p.lastBody, []byte(ownIdentity.Value)) {
			t.Fatalf("an identifier naming this payer was left unmapped: %s", p.lastBody)
		}
	})

	t.Run("mismatch refuses before forwarding", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Claim/$inquire"] = answer
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", newCensusSoR(), fixedClock, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
		res, err := n.Handle(context.Background(), "pas-claim-inquire", "corr", "MBR-1", inquiryBundleWithPayor(foreignIdentity))
		if err != nil {
			t.Fatalf("Handle must return a LegResult refusal, not a bare error: %v", err)
		}
		if res.Status != 400 {
			t.Fatalf("a Coverage naming another payer must refuse with a 400-class LegResult, got status=%d", res.Status)
		}
		if p.lastBody != nil {
			t.Fatalf("it must refuse BEFORE forwarding any bytes; the payer's system received: %s", p.lastBody)
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
	b, err := shnsdk.BuildConformantClaimUpdateBundle(shnsdk.ConformantClaimUpdateInputs{Coverage: testMemberCoverage(member),
		Provider:       testRequestingProvider(),
		MemberIDSystem: shnsdk.MemberSystem,
		QR:             qrJSON, SR: sr, PatientRef: ref, CoverageRef: "Coverage/" + member, MemberID: member,
		Provenance: provJSON, DiagnosticReport: drJSON,
		Corr: corr, OriginalCorr: originalCorr,
		Created:          created,
		ContainedInsurer: payerOrgEntry, AbsoluteRefs: payerOrgEntry, PayerOrgEntry: payerOrgEntry,
		Insurer: testPayerOrganization(payer),
		Payer:   payer,
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
	approvedBody = fixturePASResponse(t, approvedBody, true)
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
