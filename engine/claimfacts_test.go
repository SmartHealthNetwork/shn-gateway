package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The origination claim facts: the gateway builds a requester's Claim from the
// facts the request to its origination route states, carries each as stated,
// and refuses — before any PAS leg — a fact the Claim requires at the line that
// the request does not state. These rows hold each element at each line.

// claimBodyWithout is testClaimBody with one claimFacts member (or the
// certificationType) removed.
func claimBodyWithout(t *testing.T, member string) string {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(testOrderBody(pasFollowSubmit(0).orderJSON, "")), &body); err != nil {
		t.Fatal(err)
	}
	if member == "certificationType" {
		delete(body, member)
	} else {
		var facts map[string]json.RawMessage
		if err := json.Unmarshal(body["claimFacts"], &facts); err != nil {
			t.Fatal(err)
		}
		if _, ok := facts[member]; !ok {
			t.Fatalf("fixture: no claimFacts member %q", member)
		}
		delete(facts, member)
		raw, _ := json.Marshal(facts)
		body["claimFacts"] = raw
	}
	out, _ := json.Marshal(body)
	return string(out)
}

func claimRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(body))
}

// sentClaim is the first Claim in a sent PAS Bundle.
func sentClaim(t *testing.T, bundle []byte) map[string]json.RawMessage {
	t.Helper()
	var b struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatal(err)
	}
	for _, e := range b.Entry {
		var m map[string]json.RawMessage
		if json.Unmarshal(e.Resource, &m) == nil && string(m["resourceType"]) == `"Claim"` {
			return m
		}
	}
	t.Fatal("no Claim in the sent bundle")
	return nil
}

func claimItem0(t *testing.T, claim map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(claim["item"], &items); err != nil || len(items) == 0 {
		t.Fatalf("claim items: %v", err)
	}
	return items[0]
}

func itemExtensionValue(t *testing.T, item map[string]json.RawMessage, url string) json.RawMessage {
	t.Helper()
	var exts []struct {
		URL   string          `json:"url"`
		Value json.RawMessage `json:"valueCodeableConcept"`
	}
	_ = json.Unmarshal(item["extension"], &exts)
	for _, e := range exts {
		if e.URL == url {
			return e.Value
		}
	}
	return nil
}

// TestOriginationClaimInput_ReadAtTheRouteEntry: the route entry reads the
// claim input before any handler decodes the body for its own keys, and puts
// the body back whole.
func TestOriginationClaimInput_ReadAtTheRouteEntry(t *testing.T) {
	r := withOriginationClaimInput(claimRequest(testClaimBody(`"resumeToken":"tok-1"`)))
	var own struct {
		ResumeToken string `json:"resumeToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&own); err != nil || own.ResumeToken != "tok-1" {
		t.Fatalf("the handler's own key after the entry read: %+v %v", own, err)
	}
	in, err := claimInputOf(r)
	if err != nil || in.ClaimFacts == nil {
		t.Fatalf("claim input: %+v %v", in, err)
	}
	if !bytes.Equal(in.ClaimFacts.ClaimType, []byte(testClaimTypeCoding)) || !bytes.Equal(in.CertificationType, []byte(testCertificationCoding)) {
		t.Fatalf("stated codings not kept as written: %s %s", in.ClaimFacts.ClaimType, in.CertificationType)
	}
	// An empty body states nothing (the builder then refuses what is required).
	in, err = claimInputOf(withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)))
	if err != nil || in.ClaimFacts != nil || in.facts().ClaimType != nil {
		t.Fatalf("empty body: %+v %v", in, err)
	}
	// A body that is not JSON is refused 400 at the claim build.
	rec := httptest.NewRecorder()
	if _, ok := claimInputOrFail(rec, withOriginationClaimInput(claimRequest("not json"))); ok || rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: ok=%v %d", ok, rec.Code)
	}
}

// TestOriginationRoute_RefusesUnstatedRequiredFacts: through the origination
// PAS tail every route runs (submitClaimAndFollow → buildPASSubmitBundle), a
// request that does not state a fact the Claim requires is answered 422 naming
// the element, and no PAS leg is sent.
func TestOriginationRoute_RefusesUnstatedRequiredFacts(t *testing.T) {
	for _, tc := range []struct{ member, element string }{
		{"claimType", "Claim.type"},
		{"priority", "Claim.priority"},
		{"serviceCategory", "Claim.item.category"},
	} {
		t.Run(tc.member, func(t *testing.T) {
			gw, stub := pasFollowSystem(t, "approved")
			req := claimRequest(claimBodyWithout(t, tc.member))
			_, status, msg, _ := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
			if status != http.StatusUnprocessableEntity || !strings.Contains(msg, tc.element) || !strings.Contains(msg, "fidelity of SHN's own build") {
				t.Fatalf("status %d %q, want a 422 refusal naming %s", status, msg, tc.element)
			}
			if n := len(stub.sent["pas-claim"]); n != 0 {
				t.Fatalf("%d submissions reached the payer after the refusal", n)
			}
		})
	}
	// The control: the full statement reaches the payer, carried as stated.
	gw, stub := pasFollowSystem(t, "approved")
	req := claimRequest(testOrderBody(pasFollowSubmit(0).orderJSON, ""))
	if _, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0)); status != 0 || err != nil {
		t.Fatalf("%d %q %v", status, msg, err)
	}
	if len(stub.sent["pas-claim"]) != 1 {
		t.Fatalf("%d submissions reached the payer", len(stub.sent["pas-claim"]))
	}
	claim := sentClaim(t, stub.sent["pas-claim"][0])
	item := claimItem0(t, claim)
	for _, c := range []struct{ name, got, want string }{
		{"Claim.type", string(claim["type"]), `{"coding":[` + testClaimTypeCoding + `]}`},
		{"Claim.priority", string(claim["priority"]), `{"coding":[` + testPriorityCoding + `]}`},
		{"Claim.item.category", string(item["category"]), `{"coding":[` + testCategoryCoding + `]}`},
		{"Claim.item.locationCodeableConcept", string(item["locationCodeableConcept"]), `{"coding":[` + testPlaceCoding + `]}`},
	} {
		if canonicalJSON(t, []byte(c.got)) != canonicalJSON(t, []byte(c.want)) {
			t.Errorf("%s = %s, want the request's own %s", c.name, c.got, c.want)
		}
	}
}

// TestOriginationRoute_EachRequiredFactAtEachLine: the facts a route's request
// states (claimInputOf) feed the same submit builder the route calls
// (buildPASSubmitBundle) at every line; each fact the PAS Claim snapshot
// requires at the line and the request does not state is refused, and the
// route answers it 422 (pasBuildFailure). Optional at 2.0 and unstated, the
// place of service and request type are omitted.
func TestOriginationRoute_EachRequiredFactAtEachLine(t *testing.T) {
	member := "MBR-COVERED"
	required := map[string][]struct{ member, element string }{
		"2.0": {{"claimType", "Claim.type"}, {"priority", "Claim.priority"}, {"serviceCategory", "Claim.item.category"}},
	}
	required["2.1"] = append(append([]struct{ member, element string }{}, required["2.0"]...),
		struct{ member, element string }{"placeOfService", "Claim.item.location[x]"},
		struct{ member, element string }{"requestType", "Claim.item requestType"})
	required["2.2"] = required["2.1"]
	build := func(line, body string) ([]byte, error) {
		in, err := claimInputOf(claimRequest(body))
		if err != nil {
			t.Fatal(err)
		}
		return buildPASSubmitBundle(line, false, pasTailServiceRequest(), nil, testRequestingProvider(), testMemberCoverage(member),
			testPayerOrganization(shnsdk.CMSPayerIdentity), shnsdk.MemberSystem, "Patient/"+member, "Coverage/"+member, member,
			"corr-facts", pasTailClock(), shnsdk.CMSPayerIdentity, in.facts())
	}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, r := range required[line] {
			out, err := build(line, claimBodyWithout(t, r.member))
			if err == nil || out != nil || !errors.Is(err, shnsdk.ErrPASClaimFactAbsent) || !strings.Contains(err.Error(), r.element) {
				t.Errorf("%s without %s: err=%v, want the refusal naming %s", line, r.member, err, r.element)
				continue
			}
			rec := httptest.NewRecorder()
			pasBuildFailure(rec, err, "build bundle failed")
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), r.element) {
				t.Errorf("%s without %s: the route answers %d %s, want 422 naming it", line, r.member, rec.Code, rec.Body.String())
			}
		}
		got, err := build(line, testClaimBody(""))
		if err != nil {
			t.Fatalf("%s full statement: %v", line, err)
		}
		item := claimItem0(t, sentClaim(t, got))
		if canonicalJSON(t, item["locationCodeableConcept"]) != canonicalJSON(t, []byte(`{"coding":[`+testPlaceCoding+`]}`)) {
			t.Errorf("%s: place of service %s, want the request's own", line, item["locationCodeableConcept"])
		}
		reqType := itemExtensionValue(t, item, "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceItemRequestType")
		if line != "2.0" && canonicalJSON(t, reqType) != canonicalJSON(t, []byte(`{"coding":[`+testRequestTypeCoding+`]}`)) {
			t.Errorf("%s: requestType %s, want the request's own", line, reqType)
		}
	}
	// 2.0: unstated place of service and request type are omitted, not refused.
	body := claimBodyWithout(t, "placeOfService")
	var m map[string]json.RawMessage
	_ = json.Unmarshal([]byte(body), &m)
	var f map[string]json.RawMessage
	_ = json.Unmarshal(m["claimFacts"], &f)
	delete(f, "requestType")
	m["claimFacts"], _ = json.Marshal(f)
	raw, _ := json.Marshal(m)
	got, err := build("2.0", string(raw))
	if err != nil {
		t.Fatalf("2.0 without place of service and request type: %v", err)
	}
	item := claimItem0(t, sentClaim(t, got))
	if _, ok := item["locationCodeableConcept"]; ok {
		t.Fatal("2.0 wrote a place of service the request does not state")
	}
}

// TestOriginationAmendment_CertificationTypeIsTheRequests: an amendment states
// the certificationType the request that sends it states; required from 2.1,
// the build refuses (422) when the request states none, and it is never read
// off the submission.
func TestOriginationAmendment_CertificationTypeIsTheRequests(t *testing.T) {
	for _, line := range []string{"2.1", "2.2"} {
		for _, stated := range []bool{true, false} {
			body := testClaimBody("")
			if !stated {
				body = claimBodyWithout(t, "certificationType")
			}
			in, err := claimInputOf(claimRequest(body))
			if err != nil {
				t.Fatal(err)
			}
			upd := attachmentUpdateInputs(line, false, false)
			upd.PASClaimFacts, upd.CertificationType = in.facts(), in.CertificationType
			out, err := buildAuthoredPASUpdate(line, upd)
			if stated {
				if err != nil {
					t.Fatalf("%s: %v", line, err)
				}
				cert := itemExtensionValue(t, claimItem0(t, sentClaim(t, out)), "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-certificationType")
				if canonicalJSON(t, cert) != canonicalJSON(t, []byte(`{"coding":[`+testCertificationCoding+`]}`)) {
					t.Fatalf("%s: certificationType %s, want the request's own", line, cert)
				}
				continue
			}
			rec := httptest.NewRecorder()
			pasBuildFailure(rec, err, "build update bundle failed")
			if out != nil || !errors.Is(err, shnsdk.ErrPASClaimFactAbsent) || rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "certificationType") {
				t.Fatalf("%s without certificationType: %v → %d %s, want a 422 naming it", line, err, rec.Code, rec.Body.String())
			}
		}
	}
	// Every amendment the gateway sends states the certificationType of the
	// request that sends it, and nothing else.
	for _, f := range []string{"originate.go", "originate_resume.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "CertificationType:") && !strings.Contains(line, "claimIn.CertificationType") && !strings.Contains(line, "amend.certificationType") {
				t.Errorf("%s: an amendment's certificationType from another source: %s", f, strings.TrimSpace(line))
			}
		}
	}
}

// TestRecordPASSubmission_AmendmentRestatesTheRecordedSubmission: an amendment
// restates the submission it continues from the bytes that submission built —
// its own created, not the amendment's.
func TestRecordPASSubmission_AmendmentRestatesTheRecordedSubmission(t *testing.T) {
	member := "MBR-COVERED"
	order := pasTailServiceRequest()
	submittedAt := time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC)
	for _, line := range []string{"2.0", "2.2"} {
		sub, err := buildPASSubmitBundle(line, false, order, nil, testRequestingProvider(), testMemberCoverage(member),
			testPayerOrganization(shnsdk.CMSPayerIdentity), shnsdk.MemberSystem, "Patient/"+member, "Coverage/"+member, member,
			"corr-sub", submittedAt, shnsdk.CMSPayerIdentity, testClaimFacts())
		if err != nil {
			t.Fatal(err)
		}
		rec, err := recordPASSubmission(sub)
		if err != nil {
			t.Fatal(err)
		}
		if rec.prior.Created != "2026-05-01T09:30:00Z" || !bytes.Contains(rec.prior.ClaimType, []byte(`"professional"`)) ||
			!bytes.Contains(rec.prior.Priority, []byte(`"normal"`)) || len(rec.prior.Items) != 1 {
			t.Fatalf("%s: recorded %+v, want the submitted Claim's own created, type, priority and item", line, rec.prior)
		}
		in := attachmentUpdateInputs(line, false, false)
		in.PriorClaim = rec.prior
		in.SR = order
		in.OriginalCorr = "corr-sub"
		upd, err := shnsdk.BuildConformantClaimUpdateBundleAtLineExact(line, in)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if !bytes.Contains(upd, []byte(`"created":"2026-05-01T09:30:00Z"`)) {
			t.Fatalf("%s: the prior-Claim entry does not restate the submission's own created", line)
		}
		// The amendment re-sends the submitted item exactly as it was: no
		// infoChanged (it is derived from the recorded submission, never
		// stated by the gateway).
		if code := updateItemInfoChanged(t, upd); code != "" {
			t.Fatalf("%s: an item sent before exactly as now carries infoChanged %q", line, code)
		}
		// A resume stating another service category changes the item: "changed".
		in.PASClaimFacts.ServiceCategory = json.RawMessage(`{"system":"https://codesystem.x12.org/005010/1365","code":"42"}`)
		upd, err = shnsdk.BuildConformantClaimUpdateBundleAtLineExact(line, in)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if code := updateItemInfoChanged(t, upd); code != "changed" {
			t.Fatalf("%s: an item changed since the submission carries infoChanged %q, want changed", line, code)
		}
	}
}

// updateItemInfoChanged is the infoChanged code an amendment's first Claim
// item carries, or "".
func updateItemInfoChanged(t *testing.T, bundle []byte) string {
	t.Helper()
	var b struct {
		Entry []struct {
			Resource struct {
				Item []struct {
					Extension []struct {
						URL       string `json:"url"`
						ValueCode string `json:"valueCode"`
					} `json:"extension"`
				} `json:"item"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil || len(b.Entry) == 0 || len(b.Entry[0].Resource.Item) == 0 {
		t.Fatalf("amendment items: %v", err)
	}
	for _, e := range b.Entry[0].Resource.Item[0].Extension {
		if e.URL == pasInfoChangedExtURL {
			return e.ValueCode
		}
	}
	return ""
}

// TestContinuation_RoundTripsCreated: the continuation keeps the submitted
// Claim's created through the gateway's own record and back. (An amendment's
// record of the submission is the pend's own, read off the bytes the
// submission built — recordPASSubmission — never the continuation.)
func TestContinuation_RoundTripsCreated(t *testing.T) {
	back := ContinuationFacts(Continuation{}, shnsdk.PriorAuthContinuation{Created: "2026-05-01T09:30:00Z"})
	if back.ClaimCreated != "2026-05-01T09:30:00Z" || back.SDKContinuation().Created != back.ClaimCreated {
		t.Fatalf("continuation round trip lost created: %+v", back)
	}
}

// TestPASBuildFailure_NamesTheFidelityRefusal: a build refused for an unstated
// fact is answered 422 with the element named; any other failure stays the
// opaque 500.
func TestPASBuildFailure_NamesTheFidelityRefusal(t *testing.T) {
	rec := httptest.NewRecorder()
	pasBuildFailure(rec, errors.New("boom"), "build bundle failed")
	body, _ := io.ReadAll(rec.Result().Body)
	if rec.Code != http.StatusInternalServerError || bytes.Contains(body, []byte("boom")) {
		t.Fatalf("an unrelated build failure must stay the opaque 500: %d %s", rec.Code, body)
	}
}

// TestRecordPASSubmission_RestatesTheSubmittedCodingsAsSent: the prior-Claim
// entry an amendment carries restates the submitted Claim's type and priority
// exactly as the submission sent them — every member of the coding (here its
// version and userSelected), never rebuilt from system and code.
func TestRecordPASSubmission_RestatesTheSubmittedCodingsAsSent(t *testing.T) {
	const statedType = `{"system":"http://terminology.hl7.org/CodeSystem/claim-type","version":"4.0.1","code":"professional","userSelected":true}`
	member := "MBR-COVERED"
	order := pasTailServiceRequest()
	facts := testClaimFacts()
	facts.ClaimType = json.RawMessage(statedType)
	sub, err := buildPASSubmitBundle("2.0", false, order, nil, testRequestingProvider(), testMemberCoverage(member),
		testPayerOrganization(shnsdk.CMSPayerIdentity), shnsdk.MemberSystem, "Patient/"+member, "Coverage/"+member, member,
		"corr-sub", time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC), shnsdk.CMSPayerIdentity, facts)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := recordPASSubmission(sub)
	if err != nil {
		t.Fatal(err)
	}
	in := attachmentUpdateInputs("2.0", false, false)
	in.PriorClaim, in.SR, in.OriginalCorr = rec.prior, order, "corr-sub"
	upd, err := shnsdk.BuildConformantClaimUpdateBundleAtLineExact("2.0", in)
	if err != nil {
		t.Fatal(err)
	}
	var b struct {
		Entry []struct {
			Resource struct {
				ResourceType string          `json:"resourceType"`
				ID           string          `json:"id"`
				Type         json.RawMessage `json:"type"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(upd, &b); err != nil {
		t.Fatal(err)
	}
	for _, e := range b.Entry[1:] {
		if e.Resource.ResourceType != "Claim" {
			continue
		}
		if canonicalJSON(t, e.Resource.Type) != canonicalJSON(t, []byte(`{"coding":[`+statedType+`]}`)) {
			t.Fatalf("prior-Claim type %s, want the submitted coding with every member", e.Resource.Type)
		}
		return
	}
	t.Fatal("the amendment carries no prior-Claim entry")
}

// amendmentLinkPend is a payer's pended answer at line whose Task asks item
// seq for the attachment coded code.
func amendmentLinkPend(t *testing.T, line string, seq int, code string) []byte {
	t.Helper()
	payer := shnsdk.PASIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: shnsdk.CMSPayerIdentity.Value}
	b, err := shnsdk.BuildPendedClaimResponseAtLineExact(line, shnsdk.PendedResponseInputs{
		Correlation: "synthetic-submit",
		Created:     time.Date(2026, 5, 1, 9, 31, 0, 0, time.UTC),
		Task: shnsdk.PendedTaskInputs{
			Identifier: shnsdk.PASIdentifier{System: "urn:test:payer:pa-request", Value: "req-1"},
			Status:     "requested", Patient: "Patient/MBR-OX", Claim: "https://provider.test/fhir/Claim/claim-1",
			Requester: payer, Owner: payer, PayerURL: "https://payer.test/fhir",
			AuthoredOn: time.Date(2026, 5, 1, 9, 31, 0, 0, time.UTC),
			Items:      []shnsdk.PendedItem{{Sequence: seq, AttachmentCodes: []shnsdk.PASCoding{{System: "http://loinc.org", Code: code}}}},
		},
		Request: testAnswerRequest,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// amendmentLinks reads the operative Claim's supportingInfo references, each
// item's informationSequence and each item's infoChanged code.
func amendmentLinks(t *testing.T, bundle []byte) (refs []string, infoSeq map[int][]int, marks map[int]string) {
	t.Helper()
	var b struct {
		Entry []struct {
			Resource struct {
				SupportingInfo []struct {
					ValueReference struct {
						Reference string `json:"reference"`
					} `json:"valueReference"`
				} `json:"supportingInfo"`
				Item []struct {
					Sequence            int   `json:"sequence"`
					InformationSequence []int `json:"informationSequence"`
					Extension           []struct {
						URL       string `json:"url"`
						ValueCode string `json:"valueCode"`
					} `json:"extension"`
				} `json:"item"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil || len(b.Entry) == 0 {
		t.Fatalf("amendment: %v", err)
	}
	infoSeq, marks = map[int][]int{}, map[int]string{}
	for _, si := range b.Entry[0].Resource.SupportingInfo {
		refs = append(refs, si.ValueReference.Reference)
	}
	for _, it := range b.Entry[0].Resource.Item {
		if it.InformationSequence != nil {
			infoSeq[it.Sequence] = it.InformationSequence
		}
		for _, e := range it.Extension {
			if e.URL == pasInfoChangedExtURL {
				marks[it.Sequence] = e.ValueCode
			}
		}
	}
	return refs, infoSeq, marks
}

// TestAmendment_LinksTheAnswerThePayerAskedFor: the gateway's amendment
// builder links the attached report to the item the payer's own pend (kept
// on the submission record) asks it for and marks that item "added". An answer the pend does not ask for,
// and an amendment bridged up to PAS 2.2 (whose supportingInfo slice requires
// a report type no source states), link and mark nothing.
func TestAmendment_LinksTheAnswerThePayerAskedFor(t *testing.T) {
	const note = "11504-8"
	for _, line := range []string{"2.0", "2.1"} {
		for name, tc := range map[string]struct {
			code, target string
			link         bool
		}{
			"asked":                {note, line, true},
			"another attachment":   {"28570-0", line, false},
			"bridged up to PAS 22": {note, "2.2", false},
		} {
			in := attachmentUpdateInputs(line, true, true)
			in.DiagnosticReport = []byte(`{"resourceType":"DiagnosticReport","id":"source-dr","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"` + note + `"}]},"subject":{"reference":"Patient/MBR-OX"}}`)
			sub, err := buildAuthoredPASSubmit(line, attachmentInputs("ServiceRequest", line, true))
			if err != nil {
				t.Fatal(err)
			}
			rec, err := recordPASSubmission(sub)
			if err != nil {
				t.Fatal(err)
			}
			rec.pend = amendmentLinkPend(t, line, 1, tc.code)
			in.PriorClaim = rec.prior
			in.Pend = rec.pendFor(tc.target)
			body, err := buildAuthoredPASUpdate(line, in)
			if err != nil {
				t.Fatalf("%s %s: %v", line, name, err)
			}
			refs, infoSeq, marks := amendmentLinks(t, body)
			if !tc.link {
				if len(refs) != 0 || len(infoSeq) != 0 || len(marks) != 0 {
					t.Errorf("%s %s: linked %v %v, marked %v; want nothing", line, name, refs, infoSeq, marks)
				}
				continue
			}
			if len(refs) != 1 || !strings.HasSuffix(refs[0], "DiagnosticReport/convergence-dr-operative") {
				t.Errorf("%s %s: supportingInfo %v, want the attached report", line, name, refs)
			}
			if len(infoSeq) != 1 || len(infoSeq[1]) != 1 || infoSeq[1][0] != 1 || marks[1] != "added" || len(marks) != 1 {
				t.Errorf("%s %s: informationSequence %v infoChanged %v, want item 1 → [1], added", line, name, infoSeq, marks)
			}
		}
	}
}
