//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer's gateway carries a request the network routed to it to the
// payer's own system exactly as sent: whatever payer identifiers its
// Coverages (and a Claim's insurer) name, the payer's system receives them as
// the requester wrote them, on every PA leg. These rows drive
// nativeResponder.Handle and compare what the payer's system receives with
// the request byte for byte.

var (
	// ownIdentity is a payer identifier the reference payer's own data does
	// not name (a bridge persona's), and foreignIdentity one naming another
	// payer: the payer's gateway carries either exactly.
	ownIdentity     = shnsdk.PayerIdentifier{System: "urn:shn:demo-payer", Value: "SHN-BRIDGE-DEMO"}
	foreignIdentity = shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "99999"}
)

// crdPartnerCoverageCard is a minimal valid CRD answer in the reference payer's
// shape: no card, and the order returned with its coverage information in an
// update system action. The payer gateway relays a valid CDS Hooks answer
// exactly and refuses one that is not, so every CRD Handle() test here answers
// with this.
var crdPartnerCoverageCard = []byte(`{"cards":[],"systemActions":[{"type":"update","description":"Add coverage information",` +
	`"resource":{"resourceType":"ServiceRequest","id":"sr1","extension":[` +
	`{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information",` +
	`"extension":[{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"no-auth"}]}]}}]}`)

// conformantSubmitBundle builds a $submit Bundle via the SAME SDK builder the
// originator uses, in either the SDK's default shape (contained payor Org, a Claim.insurer
// that resolves to nothing in the Bundle) or the conformant PayerOrgEntry shape (a
// resolvable Organization bundle entry shared by Coverage.payor AND Claim.insurer).
func conformantSubmitBundle(t *testing.T, payer shnsdk.PayerIdentifier, absolute bool) []byte {
	t.Helper()
	sr := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-1"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine w/o contrast"}]}}`)
	b, err := shnsdk.BuildConformantClaimBundleCarried(shnsdk.ConformantClaimInputs{PASClaimFacts: testClaimFacts(), Coverage: testMemberCoverage("MBR-1"),
		Provider:       testRequestingProvider(),
		MemberIDSystem: shnsdk.MemberSystem,
		SR:             sr, PatientRef: "Patient/MBR-1", CoverageRef: "Coverage/MBR-1", MemberID: "MBR-1",
		Corr: "corr-payer-forward", Created: time.Unix(1700000000, 0).UTC(),
		AbsoluteRefs: absolute,
		// The payer as a resolvable entry: required under this module's shn-sdk pin
		// before v0.59.0, and what every request carries from v0.59.0 on.
		PayerOrgEntry: true,
		Insurer:       testPayerOrganization(payer),
		Payer:         payer,
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

// dtrPackageWithCoverage is a $questionnaire-package input carrying cov as its
// coverage parameter (none when cov is nil) and a questionnaire canonical.
func dtrPackageWithCoverage(cov []byte) []byte {
	params := `{"name":"questionnaire","valueCanonical":"http://x/q"}`
	if cov != nil {
		params = `{"name":"coverage","resource":` + string(cov) + `},` + params
	}
	return []byte(`{"resourceType":"Parameters","parameter":[` + params + `]}`)
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

// inquiryBundleWithPayor builds a prior-authorization inquiry Bundle whose Coverage
// names payer inline. The inquiry rides the same carrier as a submission (its
// Bundle carries the Coverage entries and, where present, the Claim insurer).
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

// conformantUpdateBundle builds a $submit-amendment (pas-claim-update) Bundle via the SAME
// SDK builder the originator uses, with a caller-chosen payer identity — the
// pas-claim-update sibling of conformantSubmitBundle. Reuses the committed conformant
// update golden for the QR/DiagnosticReport/Provenance entries — their content is
// irrelevant to the payer identifiers; only Coverage.payor/Claim.insurer (and
// Claim.related[prior], which BeginClaimUpdate keys on) matter here.
func conformantUpdateBundle(t *testing.T, payer shnsdk.PayerIdentifier, absolute bool, corr, originalCorr string) []byte {
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
	in := shnsdk.ConformantClaimUpdateInputs{PASClaimFacts: testClaimFacts(), CertificationType: testCertificationType(), Coverage: testMemberCoverage(member),
		Provider:       testRequestingProvider(),
		MemberIDSystem: shnsdk.MemberSystem,
		QR:             qrJSON, SR: sr, PatientRef: ref, CoverageRef: "Coverage/" + member, MemberID: member,
		Provenance: provJSON, DiagnosticReport: drJSON,
		Corr: corr, OriginalCorr: originalCorr,
		Created:      created,
		AbsoluteRefs: absolute,
		// The payer as a resolvable entry: required under this module's shn-sdk pin
		// before v0.59.0, and what every request carries from v0.59.0 on.
		PayerOrgEntry: true,
		Insurer:       testPayerOrganization(payer),
		Payer:         payer,
	}
	in.PriorClaim = testSubmissionFor("2.0", in)
	b, err := shnsdk.BuildConformantClaimUpdateBundleCarried(in)
	if err != nil {
		t.Fatalf("conformantUpdateBundle: %v", err)
	}
	return b
}

func payorFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "relayfidelity", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pasSubmitResponder(t *testing.T, opts ...NativeOption) (*nativeResponder, *stubPartner) {
	t.Helper()
	p := newStubPartner(t)
	p.respByPath["/Claim/$submit"] = fixturePASResponse(t,
		[]byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1"}`), true)
	return NewNativeResponder(p.srv.Client(), p.srv.URL, fixedClock,
		append([]NativeOption{WithCRDService("order-sign"), WithPASStore(newCensusSoR())}, opts...)...), p
}

func crdResponder(t *testing.T, opts ...NativeOption) (*nativeResponder, *stubPartner) {
	t.Helper()
	p := newStubPartner(t)
	p.respByPath["/cds-services/order-sign"] = crdPartnerCoverageCard
	return NewNativeResponder(p.srv.Client(), p.srv.URL, nil, append([]NativeOption{WithCRDService("order-sign")}, opts...)...), p
}

func handlePAS(t *testing.T, n *nativeResponder, body []byte) LegResult {
	t.Helper()
	res, err := n.Handle(context.Background(), "pas-claim", "corr-payer-forward", "MBR-COVERED", body)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return res
}

func handleCRD(t *testing.T, n *nativeResponder, body []byte) LegResult {
	t.Helper()
	res, err := n.Handle(context.Background(), "crd-order-select", "corr-payer-forward", "pci", body)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return res
}

func assertSent(t *testing.T, p *stubPartner, want []byte) {
	t.Helper()
	if p.lastBody == nil {
		t.Fatal("nothing reached the payer's system")
	}
	if !bytes.Equal(p.lastBody, want) {
		t.Fatalf("the payer's system received bytes other than the expected ones:\n got %d bytes\nwant %d bytes\nfirst difference at %d",
			len(p.lastBody), len(want), firstDifference(p.lastBody, want))
	}
}

func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
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

// carriedIdentities are the payer identifiers each row's request names.
var carriedIdentities = map[string]shnsdk.PayerIdentifier{"a bridge identity": ownIdentity, "another payer's": foreignIdentity, "the reference payer's": shnsdk.CMSPayerIdentity}

// The network's own fixture messages, a signed PAS submit and a CRD request
// whose prefetch coverage Bundle is signed, reach the payer's system byte for
// byte.
func TestPayerForward_FixturesSentExactly(t *testing.T) {
	t.Run("signed PAS submit", func(t *testing.T) {
		signed := payorFixture(t, "valid/pas-submit-2.0.json")
		n, p := pasSubmitResponder(t)
		if res := handlePAS(t, n, signed); res.Status != 0 {
			t.Fatalf("status %d: %s", res.Status, res.Message)
		}
		assertSent(t, p, signed)
	})
	t.Run("CRD request with a signed coverage prefetch", func(t *testing.T) {
		crd := payorFixture(t, "valid/crd-order-sign-request.json")
		n, p := crdResponder(t)
		if res := handleCRD(t, n, crd); res.Status != 0 {
			t.Fatalf("status %d: %s", res.Status, res.Message)
		}
		assertSent(t, p, crd)
	})
}

// A CRD request's prefetch coverage reaches the payer's system exactly, whatever
// payer it names.
func TestPayerForward_CRDCoveragePayorSentAsIs(t *testing.T) {
	for name, payer := range carriedIdentities {
		t.Run(name, func(t *testing.T) {
			p := newStubPartner(t)
			p.respByPath["/cds-services/order-sign"] = crdPartnerCoverageCard
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, nil, WithCRDService("order-sign"))
			cov, err := containedPayorCoverage("Patient/p1", "MBR-1", payer)
			if err != nil {
				t.Fatalf("containedPayorCoverage: %v", err)
			}
			req := []byte(`{"hook":"order-sign","context":{"draftOrders":{"resourceType":"Bundle","entry":[]}},"prefetch":{"coverage":` + string(cov) + `}}`)
			res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", req)
			if err != nil || res.Status != 0 {
				t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			if !bytes.Equal(p.lastBody, req) {
				t.Fatalf("the payer's system received\n%s\nwant exactly\n%s", p.lastBody, req)
			}
			if got, ok := shnsdk.ParsePayerIdentifier(mustExtractPrefetchCoverage(t, p.lastBody), nil); !ok || got != payer {
				t.Fatalf("sent payor %v (ok=%v), want %v", got, ok, payer)
			}
		})
	}
}

// A DTR $questionnaire-package input reaches the payer's system exactly, whatever
// payer its coverage names, and so does one with no coverage.
func TestPayerForward_DTRCoveragePayorSentAsIs(t *testing.T) {
	reqs := map[string][]byte{"no coverage": dtrPackageWithCoverage(nil)}
	for name, payer := range carriedIdentities {
		cov, err := containedPayorCoverage("Patient/p1", "MBR-1", payer)
		if err != nil {
			t.Fatalf("containedPayorCoverage: %v", err)
		}
		reqs[name] = dtrPackageWithCoverage(cov)
	}
	for name, req := range reqs {
		t.Run(name, func(t *testing.T) {
			p := newStubPartner(t)
			p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, nil, WithCRDService("shn-order-select"))
			res, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", req)
			if err != nil || res.Status != 0 {
				t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			if !bytes.Equal(p.lastBody, req) {
				t.Fatalf("the payer's system received\n%s\nwant exactly\n%s", p.lastBody, req)
			}
		})
	}
}

// A PAS submission, built by the same SDK builder the originator uses, reaches
// the payer's system exactly, whatever payer its Coverage and Claim insurer name.
func TestPayerForward_SubmitPayorSentAsIs(t *testing.T) {
	approvedBody := fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1"}`), true)
	for name, payer := range carriedIdentities {
		t.Run(name, func(t *testing.T) {
			p := newStubPartner(t)
			p.respByPath["/Claim/$submit"] = approvedBody
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, fixedClock, WithCRDService("shn-order-select"), WithPASStore(newCensusSoR()))
			bundle := conformantSubmitBundle(t, payer, true)
			res, err := n.Handle(context.Background(), "pas-claim", "corr", "MBR-1", bundle)
			if err != nil || res.Status != 0 {
				t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			assertSent(t, p, bundle)
			if got, ok := bundleCoveragePayor(t, p.lastBody); !ok || got != payer {
				t.Fatalf("sent payor %v (ok=%v), want %v", got, ok, payer)
			}
		})
	}
}

// An inquiry reaches the payer's own $inquire exactly, whatever payer its
// Coverage names.
func TestPayerForward_InquiryPayorSentAsIs(t *testing.T) {
	answer := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"ClaimResponse","outcome":"complete","patient":{"reference":"Patient/SubscriberExample"}}}]}`)
	for name, payer := range carriedIdentities {
		t.Run(name, func(t *testing.T) {
			p := newStubPartner(t)
			p.respByPath["/Claim/$inquire"] = answer
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, fixedClock, WithCRDService("shn-order-select"), WithPASStore(newCensusSoR()))
			req := inquiryBundleWithPayor(payer)
			res, err := n.Handle(context.Background(), "pas-claim-inquire", "corr", "MBR-1", req)
			if err != nil || res.Status != 0 {
				t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			if p.lastPath != "/Claim/$inquire" {
				t.Fatalf("posted to %q, want /Claim/$inquire", p.lastPath)
			}
			assertSent(t, p, req)
		})
	}
}

// An amendment reaches the payer's system exactly, whatever payer its Coverage
// names, and the pend it binds is consumed by the answer as usual.
func TestPayerForward_UpdatePayorSentAsIs(t *testing.T) {
	t.Parallel()
	approvedBody := fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`), true)
	const pci = "PCI-PAYER-FORWARD-UPDATE"
	const origCorr = "corr-payer-forward-submit"
	for name, payer := range carriedIdentities {
		t.Run(name, func(t *testing.T) {
			s := newCensusSoR()
			if err := s.RecordPendedClaim(pci, origCorr); err != nil {
				t.Fatalf("RecordPendedClaim: %v", err)
			}
			p := newStubPartner(t)
			p.respByPath["/Claim/$submit"] = approvedBody
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, fixedClock, WithCRDService("shn-order-select"), WithPASStore(s))
			bundle := conformantUpdateBundle(t, payer, true, "corr-payer-forward-update", origCorr)
			res, err := n.Handle(context.Background(), "pas-claim-update", "corr", pci, bundle)
			if err != nil || res.Status != 0 {
				t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			assertSent(t, p, bundle)
		})
	}
}
