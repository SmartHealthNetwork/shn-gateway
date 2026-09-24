package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Both dependency modes run the same preservation and graph assertions.
func authoredSubmitUnderTest(line string, in shnsdk.ConformantClaimInputs) ([]byte, error) {
	return buildAuthoredPASSubmit(line, in)
}
func authoredUpdateUnderTest(line string, in shnsdk.ConformantClaimUpdateInputs) ([]byte, error) {
	return buildAuthoredPASUpdate(line, in)
}

const attachmentContextURL = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context"
const attachmentCoverageURL = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage"

func attachmentQRFixture(orderType, line string) []byte {
	coverageURL := attachmentContextURL
	if line == "2.2" {
		coverageURL = attachmentCoverageURL
	}
	return []byte(fmt.Sprintf(`{"resourceType":"QuestionnaireResponse","status":"completed","subject":{"reference":"Patient/MBR-OX","display":"Synthetic patient"},"questionnaire":"https://example.test/Questionnaire/synthetic","opaqueDecimal":9007199254740993.2300,"item":[{"linkId":"1","answer":[{"valueDecimal":9007199254740993.2300},{"valueDecimal":1.2300},{"valueDecimal":1.20e+03},{"valueDecimal":-0.0}]}],"extension":[{"url":%q,"valueReference":{"reference":"Coverage/source-coverage","type":"Coverage","display":"Coverage source","identifier":{"system":"urn:synthetic","value":"coverage"},"extension":[{"url":"https://example.test/precision","valueDecimal":9007199254740993.2300}],"opaque":{"preserve":true}}},{"url":%q,"valueReference":{"reference":%q,"type":%q,"display":"Order source","identifier":{"system":"urn:synthetic","value":"order"},"opaque":{"preserve":true}}},{"url":%q,"valueReference":{"reference":"Coverage/unrelated","display":"Unrelated coverage"}},{"url":%q,"valueReference":{"reference":%q,"display":"Unrelated order"}}],"otherReferences":[{"reference":"#contained"},{"reference":"Observation/outside"},{"reference":"https://elsewhere.test/Patient/MBR-OX"},{"reference":"Organization/org-cms-payer"}],"contained":[{"resourceType":"Coverage","id":"contained-coverage","beneficiary":{"reference":"Patient/MBR-OX","extension":[{"url":"https://example.test/nested","valueReference":{"reference":"Patient/MBR-OX"}}]}},{"resourceType":"ServiceRequest","id":"contained-sr","subject":{"reference":"Patient/MBR-OX"}},{"resourceType":"DeviceRequest","id":"contained-dr","subject":{"reference":"Patient/MBR-OX"}}]}`, coverageURL, attachmentContextURL, orderType+"/source-order", orderType, attachmentContextURL, attachmentContextURL, orderType+"/unrelated"))
}

func attachmentInputs(orderType, line string, absolute bool) shnsdk.ConformantClaimInputs {
	order := pasTailServiceRequest()
	oldID := "sr-x"
	if orderType == "DeviceRequest" {
		order = pasTailDeviceRequest()
		oldID = "dr-ox"
	}
	order = bytes.Replace(order, []byte(oldID), []byte("source-order"), 1)
	return shnsdk.ConformantClaimInputs{Insurer: testPayerOrganization(shnsdk.CMSPayerIdentity), Coverage: testMemberCoverage("MBR-OX"), QR: attachmentQRFixture(orderType, line), SR: order, Provider: testRequestingProvider(), MemberIDSystem: shnsdk.MemberSystem,
		PatientRef: "Patient/MBR-OX", CoverageRef: "Coverage/source-coverage", MemberID: "MBR-OX",
		Corr: "synthetic-submit", Created: pasTailClock(), AbsoluteRefs: absolute, PayerOrgEntry: true, Payer: shnsdk.CMSPayerIdentity}
}

func attachmentUpdateInputs(line string, absolute, diagnostic bool) shnsdk.ConformantClaimUpdateInputs {
	in := attachmentInputs("ServiceRequest", line, absolute)
	out := shnsdk.ConformantClaimUpdateInputs{Insurer: testPayerOrganization(in.Payer), Coverage: testMemberCoverage(in.MemberID), QR: in.QR, SR: in.SR, Provider: in.Provider, MemberIDSystem: in.MemberIDSystem, PatientRef: in.PatientRef, CoverageRef: in.CoverageRef,
		MemberID: in.MemberID, Corr: "synthetic-update", OriginalCorr: in.Corr, Created: in.Created,
		AbsoluteRefs: absolute, PayerOrgEntry: true, Payer: in.Payer,
		Provenance: []byte(`{"resourceType":"Provenance","id":"source-prov","recorded":"2023-11-14T22:13:20Z","target":[{"reference":"QuestionnaireResponse/source-qr"}],"agent":[{"who":{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567890"}}}],"opaque":{"preserve":"attribution"}}`)}
	if diagnostic {
		out.DiagnosticReport = []byte(`{"resourceType":"DiagnosticReport","id":"source-dr","status":"final","code":{"text":"Synthetic report"},"subject":{"reference":"Patient/MBR-OX"},"opaque":{"preserve":"report"}}`)
	}
	return out
}

type attachmentTestEntry struct {
	FullURL  string          `json:"fullUrl"`
	Resource json.RawMessage `json:"resource"`
}

func attachmentEntries(t *testing.T, body []byte) []attachmentTestEntry {
	t.Helper()
	var b struct {
		Entry []attachmentTestEntry `json:"entry"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	return b.Entry
}
func attachmentObject(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func attachmentString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}
func attachmentResource(t *testing.T, entries []attachmentTestEntry, typ string) attachmentTestEntry {
	t.Helper()
	var matches []attachmentTestEntry
	for _, e := range entries {
		if attachmentString(t, attachmentObject(t, e.Resource)["resourceType"]) == typ {
			matches = append(matches, e)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("%s entries=%d", typ, len(matches))
	}
	return matches[0]
}

// attachmentResourceByID selects one entry by type AND id. A prior-authorization
// request now carries TWO Organizations — the payer it is addressed to and the
// requesting provider it comes from — so "the Organization entry" no longer
// names one resource.
func attachmentResourceByID(t *testing.T, entries []attachmentTestEntry, typ, id string) attachmentTestEntry {
	t.Helper()
	for _, e := range entries {
		m := attachmentObject(t, e.Resource)
		if attachmentString(t, m["resourceType"]) == typ && attachmentString(t, m["id"]) == id {
			return e
		}
	}
	t.Fatalf("no %s/%s entry", typ, id)
	return attachmentTestEntry{}
}
func attachmentRef(t *testing.T, e attachmentTestEntry, absolute bool) string {
	t.Helper()
	if absolute {
		return e.FullURL
	}
	m := attachmentObject(t, e.Resource)
	return attachmentString(t, m["resourceType"]) + "/" + attachmentString(t, m["id"])
}
func attachmentCompact(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func assertAttachmentPreservation(t *testing.T, source, body, sdkBody []byte, orderType string, absolute bool) {
	t.Helper()
	entries := attachmentEntries(t, body)
	qr := attachmentResource(t, entries, "QuestionnaireResponse")
	original := attachmentObject(t, source)
	got := attachmentObject(t, qr.Resource)
	for _, field := range []string{"opaqueDecimal", "item", "contained", "questionnaire", "status"} {
		if !bytes.Equal(attachmentCompact(t, original[field]), attachmentCompact(t, got[field])) {
			t.Errorf("preserved QR field %s changed: %s", field, got[field])
		}
	}
	if n := bytes.Count(qr.Resource, []byte("9007199254740993.2300")); n != 3 {
		t.Errorf("exact large decimals=%d, want3: %s", n, qr.Resource)
	}
	for _, literal := range []string{"1.2300", "1.20e+03", "-0.0"} {
		if !bytes.Contains(qr.Resource, []byte(literal)) {
			t.Errorf("lost numeric lexeme %s", literal)
		}
	}
	var oldExt, newExt []map[string]json.RawMessage
	if err := json.Unmarshal(original["extension"], &oldExt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got["extension"], &newExt); err != nil {
		t.Fatal(err)
	}
	if len(newExt) != len(oldExt) {
		t.Fatalf("extension count=%d", len(newExt))
	}
	for i := range oldExt {
		oldRef := attachmentObject(t, oldExt[i]["valueReference"])
		newRef := attachmentObject(t, newExt[i]["valueReference"])
		want := attachmentString(t, oldRef["reference"])
		if i == 0 {
			want = attachmentRef(t, attachmentResource(t, entries, "Coverage"), absolute)
		}
		if i == 1 {
			want = attachmentRef(t, attachmentResource(t, entries, orderType), absolute)
		}
		if ref := attachmentString(t, newRef["reference"]); ref != want {
			t.Errorf("context[%d]=%s want %s", i, ref, want)
		}
		delete(oldRef, "reference")
		delete(newRef, "reference")
		oldRaw, _ := json.Marshal(oldRef)
		newRaw, _ := json.Marshal(newRef)
		if !bytes.Equal(oldRaw, newRaw) {
			t.Errorf("context[%d] Reference members changed: %s", i, newRaw)
		}
	}
	// Non-QR envelope bytes remain exactly those owned by the SDK, including
	// every Claim, Coverage, order, DiagnosticReport and Provenance resource.
	sdkQR := attachmentResource(t, attachmentEntries(t, sdkBody), "QuestionnaireResponse")
	if !bytes.Equal(bytes.Replace(body, qr.Resource, []byte("null"), 1), bytes.Replace(sdkBody, sdkQR.Resource, []byte("null"), 1)) {
		t.Error("changed bytes outside the QR resource span")
	}
	subject := attachmentObject(t, got["subject"])
	if ref := attachmentString(t, subject["reference"]); ref != attachmentRef(t, attachmentResource(t, entries, "Patient"), absolute) {
		t.Errorf("subject=%s", ref)
	}
	var refs []map[string]string
	if err := json.Unmarshal(got["otherReferences"], &refs); err != nil {
		t.Fatal(err)
	}
	wants := []string{"#contained", "Observation/outside", "https://elsewhere.test/Patient/MBR-OX", attachmentRef(t, attachmentResourceByID(t, entries, "Organization", "org-cms-payer"), absolute)}
	for i, want := range wants {
		if refs[i]["reference"] != want {
			t.Errorf("other ref[%d]=%s want %s", i, refs[i]["reference"], want)
		}
	}
	// Independently resolve requestedService on the actual current Claim.
	requestedServices := 0
	for _, e := range entries {
		m := attachmentObject(t, e.Resource)
		if attachmentString(t, m["resourceType"]) != "Claim" {
			continue
		}
		if len(m["item"]) == 0 {
			continue
		} // Minimal prior Claim has no requested item.
		var items []struct {
			Extension []struct {
				URL            string `json:"url"`
				ValueReference struct {
					Reference string `json:"reference"`
				} `json:"valueReference"`
			} `json:"extension"`
		}
		if err := json.Unmarshal(m["item"], &items); err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			for _, ext := range item.Extension {
				if strings.HasSuffix(ext.URL, "extension-requestedService") {
					requestedServices++
					if ext.ValueReference.Reference != attachmentRef(t, attachmentResource(t, entries, orderType), absolute) {
						t.Errorf("requestedService does not resolve: %s", ext.ValueReference.Reference)
					}
				}
			}
		}
	}
	if requestedServices != 1 {
		t.Errorf("requestedService targets=%d, want 1", requestedServices)
	}
}

func TestAuthoredPASSubmitPreservesAttachment(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, typ := range []string{"ServiceRequest", "DeviceRequest"} {
			for _, absolute := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/absolute=%t", line, typ, absolute), func(t *testing.T) {
					in := attachmentInputs(typ, line, absolute)
					qrBefore, orderBefore := bytes.Clone(in.QR), bytes.Clone(in.SR)
					sdkBody, err := shnsdk.BuildConformantClaimBundleAtLine(line, in)
					if err != nil {
						t.Fatal(err)
					}
					got, err := authoredSubmitUnderTest(line, in)
					if err != nil {
						t.Fatal(err)
					}
					assertAttachmentPreservation(t, in.QR, got, sdkBody, typ, absolute)
					if !bytes.Equal(in.QR, qrBefore) || !bytes.Equal(in.SR, orderBefore) {
						t.Fatal("mutated source inputs")
					}
				})
			}
		}
	}
}

func TestAuthoredPASUpdatePreservesAttachment(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, absolute := range []bool{false, true} {
			for _, diagnostic := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/absolute=%t/diagnostic=%t", line, absolute, diagnostic), func(t *testing.T) {
					in := attachmentUpdateInputs(line, absolute, diagnostic)
					before, _ := json.Marshal(in)
					sdkBody, err := shnsdk.BuildConformantClaimUpdateBundleAtLine(line, in)
					if err != nil {
						t.Fatal(err)
					}
					got, err := authoredUpdateUnderTest(line, in)
					if err != nil {
						t.Fatal(err)
					}
					assertAttachmentPreservation(t, in.QR, got, sdkBody, "ServiceRequest", absolute)
					after, _ := json.Marshal(in)
					if !bytes.Equal(before, after) {
						t.Fatal("mutated update inputs")
					}
					entries := attachmentEntries(t, got)
					claims := 0
					for _, e := range entries {
						if attachmentString(t, attachmentObject(t, e.Resource)["resourceType"]) == "Claim" {
							claims++
						}
					}
					if claims != 2 {
						t.Errorf("prior and current Claims=%d", claims)
					}
					prov := attachmentObject(t, attachmentResource(t, entries, "Provenance").Resource)
					var targets []map[string]string
					if err := json.Unmarshal(prov["target"], &targets); err != nil {
						t.Fatal(err)
					}
					targetType := "QuestionnaireResponse"
					if diagnostic {
						targetType = "DiagnosticReport"
					}
					if len(targets) != 1 || targets[0]["reference"] != attachmentRef(t, attachmentResource(t, entries, targetType), absolute) {
						t.Errorf("Provenance targets=%v", targets)
					}
					if !bytes.Contains(got, []byte(in.OriginalCorr)) {
						t.Error("original correlation missing")
					}
				})
			}
		}
	}
}

func TestAuthoredPASOptionalAttachmentParity(t *testing.T) {
	for _, absolute := range []bool{false, true} {
		for _, qr := range [][]byte{nil, []byte(`{"resourceType":"QuestionnaireResponse","status":"completed","subject":{"reference":"Patient/MBR-OX"}}`)} {
			in := attachmentInputs("ServiceRequest", "2.0", absolute)
			in.QR = qr
			want, err := shnsdk.BuildConformantClaimBundleAtLine("2.0", in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := authoredSubmitUnderTest("2.0", in)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("optional/context-free attachment changed SDK bytes")
			}
		}
	}
}

func TestAuthoredPASUnsupportedLinePreservesSDKError(t *testing.T) {
	in := attachmentInputs("DeviceRequest", "2.0", true)
	_, want := shnsdk.BuildConformantClaimBundleAtLine("9.9", in)
	_, got := authoredSubmitUnderTest("9.9", in)
	if want == nil || got == nil || got.Error() != want.Error() {
		t.Fatalf("submit error=%v want %v", got, want)
	}
	update := attachmentUpdateInputs("2.0", true, false)
	_, want = shnsdk.BuildConformantClaimUpdateBundleAtLine("9.9", update)
	_, got = authoredUpdateUnderTest("9.9", update)
	if want == nil || got == nil || got.Error() != want.Error() {
		t.Fatalf("update error=%v want %v", got, want)
	}
}

// The numeric specimen traverses population, DTR composition, supplier/evidence
// completion, adaptation, final validation and the actual holder-to-Hub send.
type attachmentPrecisionPopulator struct {
	optionalType string
	canonical    string
	returned     []byte
}

func (p *attachmentPrecisionPopulator) Populate(ctx context.Context, request []byte, pc PopulateContext) ([]byte, []FilledItem, error) {
	raw, filled, err := (fakePopulator{canonical: p.canonical}).Populate(ctx, request, pc)
	if err != nil {
		return nil, nil, err
	}
	raw = bytes.Replace(raw, []byte(`"valueDecimal":86`), []byte(`"valueDecimal":86.0000`), 1)
	raw = append([]byte(`{"opaqueDecimal":9007199254740993.2300,"extension":[{"url":"https://example.test/precision","valueDecimal":1.20e+03}],`), raw[1:]...)
	if p.optionalType != "" {
		raw = appendAuthoredTestContext(raw, `{"reference":"`+p.optionalType+`/optional-order","type":"`+p.optionalType+`","display":"Preserve optional order"}`)
	}
	p.returned = bytes.Clone(raw)
	return raw, filled, nil
}

func TestAuthoredPASFinalOutgoingPreservesPrecision(t *testing.T) {
	testAuthoredPASFinalOutgoing(t, "")
}
func TestAuthoredPASProviderDataOptionalOrder(t *testing.T) {
	for _, typ := range []string{"ServiceRequest", "DeviceRequest"} {
		t.Run(typ, func(t *testing.T) { testAuthoredPASFinalOutgoing(t, typ) })
	}
}
func testAuthoredPASFinalOutgoing(t *testing.T, optionalType string) {
	var events []ObserverEvent
	populator := attachmentPrecisionPopulator{canonical: "http://smarthealth.network/fhir/Questionnaire/home-oxygen", optionalType: optionalType}

	authzPub, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	_, provSignPriv := genED25519(t)
	payerEncPub, _ := genKeyPair(t)
	payerSignPub, _ := genED25519(t)

	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	base := newCensusSoR()
	// Resolve the same synthetic subject identity used by the holder data source.
	pci := shnsdk.ResolvePCI("MBR-OX", "1958-07-14", "Okafor-Oxygen")

	// Build the seeded order + supplier the way fhirseed does: an E0431
	// DeviceRequest whose performer references the DME supplier Organization.
	const performerRef = "Organization/org-dme-ox"
	orderJSON, err := buildHomeOxygenDeviceRequest("dr-ox", "Patient/MBR-OX", performerRef)
	if err != nil {
		t.Fatalf("build DeviceRequest: %v", err)
	}
	supplierJSON, err := buildHomeOxygenSupplier("org-dme-ox")
	if err != nil {
		t.Fatalf("build supplier: %v", err)
	}
	sor := &homeOxygenSoR{
		censusSoR:    base,
		member:       "MBR-OX",
		demo:         Demo{BirthDate: "1958-07-14", FamilyName: "Okafor-Oxygen"},
		pci:          pci,
		orderJSON:    orderJSON,
		performerRef: performerRef,
		supplierJSON: supplierJSON,
	}

	const canonical = "http://smarthealth.network/fhir/Questionnaire/home-oxygen"
	stub := &homeOxygenSubstrate{
		authzPriv:      authzPriv,
		providerEncPub: provEncPub,
		clock:          clock,
		pci:            pci,
		canonical:      canonical,
	}

	reg := shnsdk.NewRegistry()
	reg.Set("provider", shnsdk.RegistryEntry{ID: "provider", Role: "provider", EncPub: provEncPub, SignPub: authzPub})
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", EncPub: payerEncPub, SignPub: payerSignPub, RequestFrames: dtrOperationFrames})

	const fakeBase = "http://stub.test"
	gw := mustNew(t, Config{
		Role:        "provider",
		HolderID:    "provider",
		PayerRouter: payerRouterFor(t, "payer"),
		Identity: shnsdk.Identity{
			HolderID: "provider",
			SignPriv: provSignPriv,
			EncPub:   provEncPub,
			EncPriv:  provEncPriv,
		},
		AuthzURL:           fakeBase,
		AuthzPub:           authzPub,
		HubTransportPub:    authzPub,
		HubURL:             fakeBase,
		Reg:                reg,
		Validator:          shnsdk.NewFakeValidator(),
		SoR:                sor,
		Store:              base,
		Clock:              clock,
		NPI:                "1234567890",
		OriginationProfile: "provider-data",
		Populator:          &populator,
		Observer:           func(e ObserverEvent) { e.Payload = bytes.Clone(e.Payload); events = append(events, e) },
		Client:             &http.Client{Transport: stub},
	})

	req := httptest.NewRequest(http.MethodPost, "/scenario/homeoxygen", nil)
	rec := httptest.NewRecorder()
	gw.handleHomeOxygen(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("full outgoing path status=%d: %s", rec.Code, rec.Body.String())
	}
	if err := gw.WaitObserverCompletion(context.Background()); err != nil {
		t.Fatal(err)
	}
	var outgoing, finalQR []byte
	for _, e := range events {
		if e.Kind == "leg.originated" && e.LegType == "pas-claim" {
			outgoing = e.Payload
			finalQR = attachmentResource(t, attachmentEntries(t, outgoing), "QuestionnaireResponse").Resource
		}
	}
	if len(outgoing) == 0 {
		t.Fatal("no actual PAS leg")
	}
	for _, literal := range []string{"9007199254740993.2300", "86.0000", "1.20e+03"} {
		if !bytes.Contains(finalQR, []byte(literal)) {
			t.Errorf("final outgoing QR lost %s: %s", literal, finalQR)
		}
		if !bytes.Contains(populator.returned, []byte(literal)) {
			t.Errorf("source specimen lost %s", literal)
		}
	}
	if len(sor.resolveByRefCall) == 0 {
		t.Fatal("supplier/evidence path was not reached")
	}
}

func TestAuthoredPASRejectsInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*shnsdk.ConformantClaimInputs)
	}{
		{"malformed QR", func(in *shnsdk.ConformantClaimInputs) { in.QR = []byte(`{`) }},
		{"wrong QR resource type", func(in *shnsdk.ConformantClaimInputs) {
			in.QR = bytes.Replace(in.QR, []byte(`"resourceType":"QuestionnaireResponse"`), []byte(`"resourceType":"Observation"`), 1)
		}},
		{"missing source order identity", func(in *shnsdk.ConformantClaimInputs) {
			in.SR = bytes.Replace(in.SR, []byte(`"id":"source-order",`), nil, 1)
		}},
		{"malformed source order", func(in *shnsdk.ConformantClaimInputs) { in.SR = []byte(`[]`) }},
		{"missing coverage identity", func(in *shnsdk.ConformantClaimInputs) { in.CoverageRef = "" }},
		{"wrong coverage identity type", func(in *shnsdk.ConformantClaimInputs) { in.CoverageRef = "Patient/source-coverage" }},
		{"malformed administrative Reference", func(in *shnsdk.ConformantClaimInputs) {
			in.QR = []byte(`{"resourceType":"QuestionnaireResponse","status":"completed","extension":[{"url":"` + attachmentContextURL + `","valueReference":null}]}`)
		}},
		{"non-string administrative reference", func(in *shnsdk.ConformantClaimInputs) {
			in.QR = []byte(`{"resourceType":"QuestionnaireResponse","status":"completed","extension":[{"url":"` + attachmentContextURL + `","valueReference":{"reference":7}}]}`)
		}},
		{"missing administrative reference", func(in *shnsdk.ConformantClaimInputs) {
			in.QR = []byte(`{"resourceType":"QuestionnaireResponse","status":"completed","extension":[{"url":"` + attachmentContextURL + `","valueReference":{"display":"Missing identity"}}]}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := attachmentInputs("ServiceRequest", "2.0", true)
			tc.mutate(&in)
			if _, err := authoredSubmitUnderTest("2.0", in); err == nil {
				t.Fatal("accepted invalid authored attachment inputs")
			}
		})
	}
	t.Run("update requires QR", func(t *testing.T) {
		in := attachmentUpdateInputs("2.0", true, false)
		in.QR = nil
		if _, err := authoredUpdateUnderTest("2.0", in); err == nil {
			t.Fatal("accepted update without QR")
		}
	})
	t.Run("update refuses DeviceRequest", func(t *testing.T) {
		in := attachmentUpdateInputs("2.0", true, false)
		device := attachmentInputs("DeviceRequest", "2.0", true)
		in.SR = device.SR
		in.QR = device.QR
		if _, err := authoredUpdateUnderTest("2.0", in); err == nil {
			t.Fatal("accepted unsupported DeviceRequest update")
		}
	})
}

func TestAuthoredPASAssembledIdentityRefusals(t *testing.T) {
	in := attachmentInputs("DeviceRequest", "2.2", true)
	body, err := shnsdk.BuildConformantClaimBundleAtLine("2.2", in)
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"QuestionnaireResponse", "Coverage", "DeviceRequest"} {
		for _, duplicate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/duplicate=%t", typ, duplicate), func(t *testing.T) {
				entries := attachmentEntries(t, body)
				for i, e := range entries {
					if attachmentString(t, attachmentObject(t, e.Resource)["resourceType"]) == typ {
						if duplicate {
							entries = append(entries, e)
						} else {
							entries = append(entries[:i], entries[i+1:]...)
						}
						break
					}
				}
				root := attachmentObject(t, body)
				root["entry"], _ = json.Marshal(entries)
				changed, _ := json.Marshal(root)
				if _, err := completeAuthoredPASAttachment(changed, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs); err == nil {
					t.Fatal("accepted missing or duplicate required identity")
				}
			})
		}
	}

	for _, typ := range []string{"QuestionnaireResponse", "Coverage", "DeviceRequest"} {
		t.Run("distinct second required entry/"+typ, func(t *testing.T) {
			entries := attachmentEntries(t, body)
			second := attachmentResource(t, entries, typ)
			resource := attachmentObject(t, second.Resource)
			id := attachmentString(t, resource["id"])
			resource["id"], _ = json.Marshal(id + "-second")
			second.Resource, _ = json.Marshal(resource)
			second.FullURL += "-second"
			entries = append(entries, second)
			root := attachmentObject(t, body)
			root["entry"], _ = json.Marshal(entries)
			changed, _ := json.Marshal(root)
			if _, err := completeAuthoredPASAttachment(changed, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs); err == nil {
				t.Fatal("accepted distinct second required resource")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func([]attachmentTestEntry) []attachmentTestEntry
	}{
		{"malformed resource", func(e []attachmentTestEntry) []attachmentTestEntry { e[0].Resource = []byte(`[]`); return e }},
		{"missing resource", func(e []attachmentTestEntry) []attachmentTestEntry { e[0].Resource = []byte(`null`); return e }},
		{"wrong order type", func(e []attachmentTestEntry) []attachmentTestEntry {
			for i := range e {
				e[i].Resource = bytes.Replace(e[i].Resource, []byte(`"resourceType":"DeviceRequest"`), []byte(`"resourceType":"ServiceRequest"`), 1)
			}
			return e
		}},
		{"invalid URL", func(e []attachmentTestEntry) []attachmentTestEntry { e[0].FullURL = "%broken"; return e }},
		{"relative URL", func(e []attachmentTestEntry) []attachmentTestEntry { e[0].FullURL = "Claim/local"; return e }},
		{"missing URL", func(e []attachmentTestEntry) []attachmentTestEntry { e[0].FullURL = ""; return e }},
		{"URL resource disagreement", func(e []attachmentTestEntry) []attachmentTestEntry { e[0].FullURL += "-different"; return e }},
		{"URL with query", func(e []attachmentTestEntry) []attachmentTestEntry { e[0].FullURL += "?x=1"; return e }},
		{"URL with fragment", func(e []attachmentTestEntry) []attachmentTestEntry { e[0].FullURL += "#x"; return e }},
		{"duplicate full URL", func(e []attachmentTestEntry) []attachmentTestEntry { e[1].FullURL = e[0].FullURL; return e }},
		{"ambiguous relative identity", func(e []attachmentTestEntry) []attachmentTestEntry {
			copy := e[1]
			copy.FullURL = strings.Replace(copy.FullURL, "https://shn.example", "https://another.test", 1)
			return append(e, copy)
		}},
		{"duplicate resource field", func(e []attachmentTestEntry) []attachmentTestEntry {
			e[0].Resource = append([]byte(`{"resourceType":"Claim",`), e[0].Resource[1:]...)
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := tc.mutate(attachmentEntries(t, body))
			root := attachmentObject(t, body)
			root["entry"], _ = json.Marshal(entries)
			changed, _ := json.Marshal(root)
			if _, err := completeAuthoredPASAttachment(changed, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs); err == nil {
				t.Fatal("accepted ambiguous or malformed assembled identity")
			}
		})
	}

	for _, typ := range []string{"claim", "Claim_Bad", "Claim/Bad", "Cläim", ""} {
		t.Run("malformed resource type/"+typ, func(t *testing.T) {
			entries := attachmentEntries(t, body)
			resource := attachmentObject(t, entries[0].Resource)
			resource["resourceType"], _ = json.Marshal(typ)
			entries[0].Resource, _ = json.Marshal(resource)
			entries[0].FullURL = strings.Replace(entries[0].FullURL, "/Claim/", "/"+typ+"/", 1)
			root := attachmentObject(t, body)
			root["entry"], _ = json.Marshal(entries)
			changed, _ := json.Marshal(root)
			if _, err := completeAuthoredPASAttachment(changed, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs); err == nil {
				t.Fatal("accepted malformed resource type")
			}
		})
	}
	for _, bad := range [][]byte{[]byte(`{`), []byte(`null`), []byte(`[]`), []byte(`{"resourceType":"Patient","entry":[]}`), []byte(`{"resourceType":"Bundle","entry":null}`)} {
		if _, err := completeAuthoredPASAttachment(bad, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs); err == nil {
			t.Errorf("accepted malformed bundle %s", bad)
		}
	}
}

func TestAuthoredPASUsesActualAssemblyIdentities(t *testing.T) {
	in := attachmentInputs("DeviceRequest", "2.2", true)
	body, err := shnsdk.BuildConformantClaimBundleAtLine("2.2", in)
	if err != nil {
		t.Fatal(err)
	}
	// All references and fullUrls change together; no production identity is
	// assumed by the completion boundary.
	body = bytes.ReplaceAll(body, []byte("https://shn.example/fhir/"), []byte("https://assembly.example.test/base/"))
	body = bytes.ReplaceAll(body, []byte("convergence-"), []byte("assembled-"))
	got, err := completeAuthoredPASAttachment(body, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs)
	if err != nil {
		t.Fatal(err)
	}
	assertAttachmentPreservation(t, in.QR, got, body, "DeviceRequest", true)
	// Repeated QR-looking text outside entry.resource must not receive the splice.
	qr := attachmentResource(t, attachmentEntries(t, body), "QuestionnaireResponse").Resource
	annotated := append([]byte(`{"opaque":`), qr...)
	annotated = append(annotated, ',')
	annotated = append(annotated, body[1:]...)
	got, err = completeAuthoredPASAttachment(annotated, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs)
	if err != nil {
		t.Fatal(err)
	}
	original := attachmentObject(t, annotated)["opaque"]
	retained := attachmentObject(t, got)["opaque"]
	if !bytes.Equal(original, retained) {
		t.Fatal("replaced QR-looking content outside the actual entry")
	}
}

func TestAuthoredPASPairedSourceIsolation(t *testing.T) {
	in := attachmentInputs("DeviceRequest", "2.0", true)
	qc := contextInputs()
	qc.PatientRef = in.PatientRef
	qc.CoverageRef = in.CoverageRef
	qc.OrderRef = "DeviceRequest/source-order"
	qc.Authored = pasTailClock()
	raw := bytes.ReplaceAll([]byte(contextQR), []byte("Patient/synthetic"), []byte(in.PatientRef))
	retained := bytes.Clone(raw)
	source := newRawDTRBuildSource(raw, []byte(sourceTree), qc)
	original, err := source.buildAtLine("2.0")
	if err != nil {
		t.Fatal(err)
	}
	saved := bytes.Clone(original)
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		in.QR, err = source.buildAtLine(line)
		if err != nil {
			t.Fatal(err)
		}
		inputBefore := bytes.Clone(in.QR)
		body, err := authoredSubmitUnderTest(line, in)
		if err != nil {
			t.Fatal(err)
		}
		qr := attachmentResource(t, attachmentEntries(t, body), "QuestionnaireResponse").Resource
		profile, err := dtrQRProfile(line)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(qr, []byte(profile)) || !bytes.Contains(qr, []byte("9007199254740993.2300")) {
			t.Fatalf("paired attachment lost profile or source: %s", qr)
		}
		if !bytes.Equal(in.QR, inputBefore) || !bytes.Equal(original, saved) || !bytes.Equal(raw, retained) {
			t.Fatal("assembly mutated retained source or original DTR artifact")
		}
	}
	again, err := source.buildAtLine("2.0")
	if err != nil || !bytes.Equal(again, saved) {
		t.Fatal("paired assembly mutated immutable source")
	}
}

func TestAuthoredPASConstructionBoundary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	completions := map[string]int{}
	sdkCalls := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := call.Fun.(*ast.Ident); ok && (name.Name == "buildAuthoredPASSubmit" || name.Name == "buildAuthoredPASUpdate") {
				calls[file]++
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if selector.Sel.Name == "completeAuthoredPASRequest" {
				completions[file]++
			}
			owner, ok := selector.X.(*ast.Ident)
			if !ok || owner.Name != "shnsdk" || !strings.HasPrefix(selector.Sel.Name, "BuildConformantClaim") {
				return true
			}
			sdkCalls++
			if file != "pas_attachment_assembly.go" {
				t.Errorf("unwrapped SDK conformant call in %s", file)
			}
			if len(call.Args) != 2 {
				t.Errorf("unexpected SDK arguments in %s", file)
				return true
			}
			line, ok := call.Args[0].(*ast.Ident)
			if !ok || line.Name != "line" {
				t.Errorf("SDK line is not forwarded unchanged in %s", file)
			}
			return true
		})
	}
	if sdkCalls != 2 {
		t.Errorf("SDK conformant construction calls=%d", sdkCalls)
	}
	for file, want := range map[string]int{"pas_tail.go": 1, "originate_resume.go": 3, "originate.go": 7} {
		if calls[file] != want {
			t.Errorf("%s wrapper calls=%d want%d", file, calls[file], want)
		}
		if completions[file] != want {
			t.Errorf("%s source-bound completion calls=%d want%d", file, completions[file], want)
		}
		delete(completions, file)
		delete(calls, file)
	}
	if len(completions) != 0 {
		t.Errorf("unexpected source-bound completion callers: %v", completions)
	}
	if len(calls) != 0 {
		t.Errorf("unexpected construction callers: %v", calls)
	}
}

func TestAuthoredPASResourceTypeLexemes(t *testing.T) {
	for _, typ := range []string{"Patient", "QuestionnaireResponse", "X", "X1a"} {
		fields := map[string]authoredPASValue{"resourceType": {raw: dtrRaw(typ)}, "id": {raw: dtrRaw("synthetic-id")}}
		got, id, err := authoredPASResourceIdentity(fields)
		if err != nil || got != typ || id != "synthetic-id" {
			t.Errorf("refused ASCII resource type %q: %v", typ, err)
		}
	}
	for _, typ := range []string{"", "patient", "1Patient", "Patient_One", "Patient-One", "Patiént", "Patient\n", "Patient/One"} {
		fields := map[string]authoredPASValue{"resourceType": {raw: dtrRaw(typ)}, "id": {raw: dtrRaw("synthetic-id")}}
		if _, _, err := authoredPASResourceIdentity(fields); err == nil {
			t.Errorf("accepted invalid ASCII resource type %q", typ)
		}
	}
}

func appendAuthoredTestContext(raw []byte, reference string) []byte {
	var qr map[string]json.RawMessage
	if err := json.Unmarshal(raw, &qr); err != nil {
		panic(err)
	}
	var ext []json.RawMessage
	if qr["extension"] != nil {
		if err := json.Unmarshal(qr["extension"], &ext); err != nil {
			panic(err)
		}
	}
	ext = append(ext, json.RawMessage(`{"url":"`+attachmentContextURL+`","valueReference":`+reference+`}`))
	qr["extension"], _ = json.Marshal(ext)
	out, _ := json.Marshal(qr)
	return out
}
func TestAuthoredPASLogicalContext(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, typ := range []string{"ServiceRequest", "DeviceRequest"} {
			for _, absolute := range []bool{false, true} {
				for _, logicalType := range []string{"Encounter", "http://hl7.org/fhir/StructureDefinition/Encounter", ""} {
					t.Run(fmt.Sprintf("%s/%s/%t/%s", line, typ, absolute, logicalType), func(t *testing.T) {
						logical := `{"identifier":{"system":"urn:synthetic:encounter","value":"optional-context"},"display":"Optional encounter","extension":[{"url":"https://example.test/precise","valueDecimal":9007199254740993.2300}]`
						if logicalType != "" {
							logical += `,"type":` + string(dtrRaw(logicalType))
						}
						logical += `}`
						in := attachmentInputs(typ, line, absolute)
						qc := contextInputs()
						qc.PatientRef = in.PatientRef
						qc.CoverageRef = in.CoverageRef
						qc.OrderRef = typ + "/source-order"
						raw := appendAuthoredTestContext(bytes.ReplaceAll([]byte(contextQR), []byte("Patient/synthetic"), []byte(in.PatientRef)), logical)
						before := bytes.Clone(raw)
						composed, err := composeDTRContextAtLine(raw, line, qc)
						if err != nil {
							t.Fatal("composition", err)
						}
						in.QR = composed
						saved := bytes.Clone(composed)
						check := func(body []byte, err error) {
							t.Helper()
							if err != nil {
								t.Fatal("assembly", err)
							}
							qr := attachmentResource(t, attachmentEntries(t, body), "QuestionnaireResponse").Resource
							if !bytes.Contains(qr, []byte(logical)) {
								t.Fatalf("logical context changed: %s", qr)
							}
							if !bytes.Equal(raw, before) || !bytes.Equal(in.QR, saved) {
								t.Fatal("source mutated")
							}
						}
						check(buildAuthoredPASSubmit(line, in))
						if typ == "ServiceRequest" {
							for _, diagnostic := range []bool{false, true} {
								update := attachmentUpdateInputs(line, absolute, diagnostic)
								update.QR = composed
								check(buildAuthoredPASUpdate(line, update))
							}
						}
					})
				}
			}
		}
	}
}

func TestAuthoredPASLogicalContextRefusals(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, typ := range []string{"ServiceRequest", "DeviceRequest"} {
			for _, value := range malformedDTRLogicalReferences() {
				t.Run(line+"/"+typ+"/"+value, func(t *testing.T) {
					in := attachmentInputs(typ, line, false)
					in.QR = appendAuthoredTestContext(in.QR, value)
					if _, err := buildAuthoredPASSubmit(line, in); err == nil {
						t.Fatal("assembly accepted malformed logical Reference")
					}
					if typ == "ServiceRequest" {
						for _, diagnostic := range []bool{false, true} {
							update := attachmentUpdateInputs(line, false, diagnostic)
							update.QR = in.QR
							if _, err := buildAuthoredPASUpdate(line, update); err == nil {
								t.Fatal("update accepted malformed logical Reference")
							}
						}
					}
				})
			}
		}
	}
}
