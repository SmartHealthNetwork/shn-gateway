//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A dependent's Coverage may name the parent, its subscriber or policyHolder,
// as a Patient entry of the request Bundle rather than a Patient contained in
// the Coverage: the PAS IG carries its subscriber profile that way. That entry
// is the Coverage's party (shnsdk.CoverageEntryParties) when only the
// subscriber or policyHolder of Coverage entries name it, by its exact
// fullUrl or by a relative reference that resolves to it against the Coverage
// entry's base, never by id alone. Like a contained party it is another
// person: its own identity is not read as another patient, and what it names
// is. These rows hold the request side (patient.mixed, the inquiry's members
// bind, the involved list); answer_party_test.go holds the answer side.

// entryParentURN is the parent's fullUrl in a request whose Coverage entry has
// no RESTful base.
const entryParentURN = "urn:uuid:5e1d7c2a-9b4f-4e8a-8f3c-2d6b1a0c9e77"

// entryParent is a dependent's request whose parent rides as a Patient entry
// of its own.
type entryParent struct {
	// base is the base the parent's entry is under: "" for the base of the
	// member's Coverage entry (a urn:uuid when that has none).
	base string
	// ref is the reference the member's Coverage's subscriber names the
	// parent by, given the parent's fullUrl, or nil for no subscriber.
	ref func(parentURL string) string
	// restful marks a request only a Coverage entry with a RESTful base can
	// carry (a relative reference resolves against that base alone).
	restful bool
	// edit is a further change to the request.
	edit func(t *testing.T, b map[string]any, parentURL string)
}

// byFullURL names the parent by its exact fullUrl.
func byFullURL(parentURL string) string { return parentURL }

// byRelativeReference names the parent by "Patient/<id>".
func byRelativeReference(string) string { return "Patient/MBR-PARENT" }

// entryParentAccepts are the dependent's requests each level carries, strict
// included: the parent is the Coverage's party.
var entryParentAccepts = map[string]entryParent{
	"the subscriber names the parent's entry by its fullUrl":                        {ref: byFullURL},
	"the subscriber names the parent's entry by a relative reference that resolves": {ref: byRelativeReference, restful: true},
	"both slots name the parent's entry, typed Patient, beside the parent's own member identifier": {ref: byFullURL,
		edit: func(t *testing.T, b map[string]any, parentURL string) {
			cov := requestCoverage(t, b)
			slot := func() map[string]any {
				return map[string]any{"reference": parentURL, "type": "Patient", "identifier": map[string]any{"system": "urn:shn:member", "value": "MBR-PARENT"}}
			}
			cov["subscriber"], cov["policyHolder"] = slot(), slot()
		}},
}

// entryParentRefusals are the requests that name another patient, each one
// mutation from an accepted one: patient.mixed refuses them at strict and
// records them below it.
var entryParentRefusals = map[string]entryParent{
	"no Coverage names the parent's entry":                                          {},
	"the subscriber names the parent by a relative reference that does not resolve": {base: "https://elsewhere.example/fhir", ref: byRelativeReference},
	"the parent's entry is also named from an extension": {ref: byFullURL,
		edit: func(t *testing.T, b map[string]any, parentURL string) {
			requestCoverage(t, b)["extension"] = []any{map[string]any{"url": "http://example.org/fhir/StructureDefinition/carried", "valueReference": map[string]any{"reference": parentURL}}}
		}},
	"another member's Coverage names the parent's entry": {
		edit: func(t *testing.T, b map[string]any, parentURL string) {
			base := strings.TrimSuffix(parentURL, "/Patient/MBR-PARENT")
			other, cov := "urn:uuid:7a0e4c1b-3d2f-4b6a-9c8e-1f5d2b7a6c33", "urn:uuid:0c9b8a7d-6e5f-4a3b-8c2d-1e0f9a8b7c66"
			if base != parentURL {
				other, cov = base+"/Patient/MBR-NOTCOVERED", base+"/Coverage/cov-other"
			}
			b["entry"] = append(b["entry"].([]any),
				map[string]any{"fullUrl": other, "resource": map[string]any{"resourceType": "Patient", "id": "MBR-NOTCOVERED",
					"identifier": []any{map[string]any{"system": "urn:shn:member", "value": "MBR-NOTCOVERED"}}}},
				map[string]any{"fullUrl": cov, "resource": map[string]any{"resourceType": "Coverage", "id": "cov-other", "status": "active",
					"beneficiary": map[string]any{"reference": other}, "subscriber": map[string]any{"reference": parentURL},
					"payor": []any{map[string]any{"display": "Payer"}}}})
		}},
	// Only the slot naming the party is exempt: the other slot names another
	// patient by a literal reference, relative or absolute.
	"the subscriber names the parent's entry, the policyHolder another patient by a relative reference": {ref: byFullURL,
		edit: func(t *testing.T, b map[string]any, _ string) {
			requestCoverage(t, b)["policyHolder"] = map[string]any{"reference": "Patient/MBR-NOTCOVERED"}
		}},
	"the subscriber names the parent's entry, the policyHolder another patient by an absolute reference": {ref: byFullURL,
		edit: func(t *testing.T, b map[string]any, _ string) {
			requestCoverage(t, b)["policyHolder"] = map[string]any{"reference": "https://elsewhere.example/fhir/Patient/MBR-NOTCOVERED"}
		}},
	"the parent's entry names another patient": {ref: byFullURL,
		edit: func(t *testing.T, b map[string]any, parentURL string) {
			for _, e := range b["entry"].([]any) {
				if em := e.(map[string]any); em["fullUrl"] == parentURL {
					em["resource"].(map[string]any)["link"] = []any{map[string]any{"type": "seealso", "other": map[string]any{"reference": "Patient/MBR-NOTCOVERED"}}}
				}
			}
		}},
}

// requestCoverage is the member's Coverage entry's resource in b.
func requestCoverage(t *testing.T, b map[string]any) map[string]any {
	t.Helper()
	for _, e := range b["entry"].([]any) {
		if res, _ := e.(map[string]any)["resource"].(map[string]any); res["resourceType"] == "Coverage" {
			return res
		}
	}
	t.Fatal("fixture: no Coverage entry")
	return nil
}

// withEntryParent is bundle as a dependent's request carrying the parent as
// a Patient entry of its own, as p says, or false when bundle cannot carry p.
// Every entry whose resource has an id and no fullUrl gets a RESTful one under
// http://localhost/fhir; a Coverage entry with no RESTful base names the
// member's Patient entry by its fullUrl, since a relative reference resolves
// against no other base.
func withEntryParent(t *testing.T, bundle []byte, p entryParent) ([]byte, bool) {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatal(err)
	}
	var member, cov map[string]any
	for _, e := range b["entry"].([]any) {
		em := e.(map[string]any)
		res := em["resource"].(map[string]any)
		typ, _ := res["resourceType"].(string)
		id, _ := res["id"].(string)
		if _, has := em["fullUrl"]; !has && id != "" {
			em["fullUrl"] = "http://localhost/fhir/" + typ + "/" + id
		}
		if typ == "Patient" && id == "MBR-COVERED" {
			member = em
		}
		if typ == "Coverage" && cov == nil {
			cov = em
		}
	}
	if member == nil || cov == nil {
		t.Fatal("fixture: no member Patient entry or Coverage entry")
	}
	covURL, _ := cov["fullUrl"].(string)
	covRes := cov["resource"].(map[string]any)
	id, _ := covRes["id"].(string)
	base, restful := strings.CutSuffix(covURL, "/Coverage/"+id)
	restful = restful && id != "" && strings.HasPrefix(covURL, "http")
	if p.restful && !restful {
		return nil, false
	}
	if !restful {
		covRes["beneficiary"] = map[string]any{"reference": member["fullUrl"]}
	}
	parentURL := entryParentURN
	switch {
	case p.base != "":
		parentURL = p.base + "/Patient/MBR-PARENT"
	case restful:
		parentURL = base + "/Patient/MBR-PARENT"
	}
	b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": parentURL, "resource": map[string]any{
		"resourceType": "Patient", "id": "MBR-PARENT",
		"identifier": []any{map[string]any{"system": "urn:shn:member", "value": "MBR-PARENT"}},
		"name":       []any{map[string]any{"family": "Parent"}}}})
	if p.ref != nil {
		covRes["subscriber"] = map[string]any{"reference": p.ref(parentURL)}
		covRes["relationship"] = map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/subscriber-relationship", "code": "child"}}}
	}
	if p.edit != nil {
		p.edit(t, b, parentURL)
	}
	return mustJSON(t, b), true
}

// entryParentFixtures are the request fixtures the guard rows run over.
func entryParentFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	update, _ := updateBundle(t)
	return map[string][]byte{
		"submit":           serviceRequestSubmitBundle(t, false),
		"update":           update,
		"provider ingress": []byte(pasIngressBundle("00001", "")),
		"inquiry":          inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"),
	}
}

// The walk itself (pasCarriesAnotherPatient) and the inquiry's members bind
// (readPASInquiryFacts), on every fixture each row fits.
func TestEntryParent_TheRequestGuards(t *testing.T) {
	t.Parallel()
	strict := func(string) bool { return true }
	for fixture, base := range entryParentFixtures(t) {
		for name, p := range entryParentAccepts {
			body, ok := withEntryParent(t, base, p)
			if !ok {
				continue
			}
			t.Run(fixture+"/accepts/"+name, func(t *testing.T) {
				if pasCarriesAnotherPatient(body, "MBR-COVERED") {
					t.Fatal("the walk read the Coverage's party as another patient")
				}
				if fixture == "inquiry" {
					if _, status, msg := readPASInquiryFacts(body, strict); status != 0 {
						t.Fatalf("the inquiry bind refused: %d %s", status, msg)
					}
				}
			})
		}
		for name, p := range entryParentRefusals {
			body, ok := withEntryParent(t, base, p)
			if !ok {
				continue
			}
			t.Run(fixture+"/refuses/"+name, func(t *testing.T) {
				if !pasCarriesAnotherPatient(body, "MBR-COVERED") {
					t.Fatal("the walk read no other patient")
				}
				if fixture == "inquiry" {
					if _, status, _ := readPASInquiryFacts(body, strict); status != http.StatusForbidden {
						t.Fatalf("the inquiry bind answered %d, want 403", status)
					}
				}
			})
		}
	}
}

// The inquiry's members bind counts a Patient entry that is no party: an
// entry no Coverage names is a second member, whatever the walk after it
// reads.
func TestEntryParent_TheInquiryMembersBind(t *testing.T) {
	strict := func(string) bool { return true }
	body, _ := withEntryParent(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), entryParent{})
	if _, status, msg := readPASInquiryFacts(body, strict); status != http.StatusForbidden || msg != "inconsistent patient in PAS inquiry" {
		t.Fatalf("an unnamed Patient entry: %d %s", status, msg)
	}
	named, _ := withEntryParent(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), entryParent{ref: byFullURL})
	if _, status, msg := readPASInquiryFacts(named, strict); status != 0 {
		t.Fatalf("the Coverage's party: %d %s", status, msg)
	}
}

// The provider's PAS ingress, POST /Claim/$submit: a dependent whose parent
// rides as an entry is carried at every level, strict included; each refusal
// row is refused at strict, and recorded and carried below it.
func TestLevelPASIngress_EntryParent(t *testing.T) {
	t.Parallel()
	for name, p := range entryParentAccepts {
		b, _ := withEntryParent(t, []byte(pasIngressBundle("00001", "")), p)
		body := string(b)
		for _, level := range allLevels {
			t.Run("accepts/"+name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s, network hits %d: want carried", rec.Code, rec.Body.String(), env.routeHitCount())
				}
				wantCarriedExactly(t, env, body)
				noPatientMixed(t, ev.findings)
			})
		}
	}
	for name, p := range entryParentRefusals {
		b, _ := withEntryParent(t, []byte(pasIngressBundle("00001", "")), p)
		body := string(b)
		for _, level := range allLevels {
			t.Run("refuses/"+name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
				wantLegLevelOutcome(t, "pas-claim", level, env, rec, ev.findings, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS bundle")
				if !refusesAt(level, RulePatientMixed) {
					wantCarriedExactly(t, env, body)
				}
			})
		}
	}
}

// The payer's gateway, binding a $submit and an amendment from the network.
func TestLevelPayerPAS_EntryParent(t *testing.T) {
	t.Parallel()
	body := func(t *testing.T, p *levelPayer, leg string, e entryParent) []byte {
		base := []byte(pasIngressBundle("00001", ""))
		if leg == "pas-claim-update" {
			request, related := updateBundle(t)
			p.seedPend(t, related)
			base = request
		}
		out, _ := withEntryParent(t, base, e)
		return out
	}
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		for name, e := range entryParentAccepts {
			for _, level := range allLevels {
				t.Run(leg+"/accepts/"+name+"/"+level.String(), func(t *testing.T) {
					p := newLevelPayer(t, level)
					req := body(t, p, leg, e)
					got := p.send(t, leg, "", req)
					if got.status != http.StatusOK || p.partner.lastPath != pasSubmitPath || !bytes.Equal(p.partner.lastBody, req) {
						t.Fatalf("answer %d %s at %q: want forwarded as sent", got.status, got.body, p.partner.lastPath)
					}
					noPatientMixed(t, p.findings)
				})
			}
		}
		for name, e := range entryParentRefusals {
			for _, level := range allLevels {
				t.Run(leg+"/refuses/"+name+"/"+level.String(), func(t *testing.T) {
					p := newLevelPayer(t, level)
					req := body(t, p, leg, e)
					got := p.send(t, leg, "", req)
					p.wantRequestRow(t, leg, got, req, pasSubmitPath, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS bundle")
				})
			}
		}
	}
}

// The inquiry, Claim/$inquire, at the provider's ingress and at the payer's
// gateway. Its Coverage entry is under a urn:uuid, so the parent is named by
// its fullUrl (the relative-reference rows are the twin-fence corpus's).
func TestLevelInquire_EntryParent(t *testing.T) {
	t.Parallel()
	for _, rows := range []struct {
		accept bool
		set    map[string]entryParent
	}{{true, entryParentAccepts}, {false, entryParentRefusals}} {
		for name, e := range rows.set {
			ingressBody, ok := withEntryParent(t, []byte(levelInquiry(t, "MBR-COVERED", "")), e)
			if !ok {
				continue
			}
			ingress := string(ingressBody)
			payer, _ := withEntryParent(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), e)
			for _, level := range allLevels {
				t.Run("ingress/"+name+"/"+level.String(), func(t *testing.T) {
					env, rec, ev := levelInquireRow(t, level, ingress, levelInquiryPayerAnswer(t))
					if rows.accept {
						if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
							t.Fatalf("answer %d %s, network hits %d: want carried", rec.Code, rec.Body.String(), env.routeHitCount())
						}
						wantCarriedExactly(t, env, ingress)
						noPatientMixed(t, ev.findings)
						return
					}
					wantLegLevelOutcome(t, "pas-claim-inquire", level, env, rec, ev.findings, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS inquiry")
					if !refusesAt(level, RulePatientMixed) {
						wantCarriedExactly(t, env, ingress)
					}
				})
				t.Run("payer/"+name+"/"+level.String(), func(t *testing.T) {
					p := newLevelPayer(t, level)
					got := p.send(t, "pas-claim-inquire", "", payer)
					if rows.accept {
						if got.status != http.StatusOK || p.partner.lastPath != pasInquirePath || !bytes.Equal(p.partner.lastBody, payer) {
							t.Fatalf("answer %d %s at %q: want forwarded as sent", got.status, got.body, p.partner.lastPath)
						}
						noPatientMixed(t, p.findings)
						return
					}
					p.wantRequestRow(t, "pas-claim-inquire", got, payer, pasInquirePath, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS inquiry")
				})
			}
		}
	}
}

// The native responder reads the bundle again before it posts it to the
// payer's system: a dependent's request reaches it at every level, and what
// strict refuses never does.
func TestNativeResponder_EntryParent(t *testing.T) {
	t.Parallel()
	for _, rows := range []struct {
		accept bool
		set    map[string]entryParent
	}{{true, entryParentAccepts}, {false, entryParentRefusals}} {
		for name, e := range rows.set {
			body, _ := withEntryParent(t, serviceRequestSubmitBundle(t, false), e)
			for _, level := range allLevels {
				t.Run(name+"/"+level.String(), func(t *testing.T) {
					srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(relayDecidedAnswer(t, "cr-entry", "trace-entry", "AUTH-ENTRY"))})
					n, ctx := relayResponder(t, srv, newCensusSoR())
					n.conformance = NewConformancePolicy(level)
					res, err := n.Handle(ctx, "pas-claim", "corr-entry", "PCI-1", body)
					payer.mu.Lock()
					defer payer.mu.Unlock()
					if !rows.accept && refusesAt(level, RulePatientMixed) {
						if err != nil || res.Status != http.StatusForbidden || res.Message != "inconsistent patient in PAS bundle" || len(payer.calls) != 0 {
							t.Fatalf("err=%v status=%d %q, %d payer calls: want refused before the payer's system", err, res.Status, res.Message, len(payer.calls))
						}
						return
					}
					if err != nil || res.Status != 0 || len(payer.calls) != 1 || !bytes.Equal(payer.calls[0].body, body) {
						t.Fatalf("err=%v status=%d %q, %d payer calls: want the bundle posted as it came", err, res.Status, res.Message, len(payer.calls))
					}
				})
			}
		}
	}
}

// The provider's PAS ingress records a patient that a parent carried as a
// Patient entry of its own names as involved, as it does one a contained
// parent names: at none, observe and structural the request is carried once
// and its envelope's involved list names that member, not the parent; when
// that member's token is refused the leg is still sent, the member left out
// and the refusal counted. At strict patient.mixed refuses the request before
// anything is sent.
func TestLevelPASIngress_APatientAnEntryPartyNamesIsInvolved(t *testing.T) {
	t.Parallel()
	b, _ := withEntryParent(t, []byte(pasIngressBundle("00001", "")), entryParentRefusals["the parent's entry names another patient"])
	body := string(b)
	other := heldPCI(t, "MBR-NOTCOVERED")
	for _, refused := range []bool{false, true} {
		for _, level := range allLevels {
			name := level.String()
			if refused {
				name += "/refused"
			}
			t.Run(name, func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				env.originator.cfg.HubAcceptsInvolved = true
				var omitted []string
				var mu sync.Mutex
				env.originator.cfg.InvolvedMetric = func(reason string) {
					mu.Lock()
					defer mu.Unlock()
					omitted = append(omitted, reason)
				}
				if refused {
					env.substrate.refuseInvolved = map[string]bool{other: true}
				}
				env.payerReturns(LegResult{Response: testResponse(levelPASPayerAnswer)})
				rec := httptest.NewRecorder()
				env.originator.handlePASIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(body)))
				env.substrate.mu.Lock()
				sent := append([]string(nil), env.substrate.involved...)
				env.substrate.mu.Unlock()
				if level == EnforcementStrict {
					if rec.Code != http.StatusForbidden || len(sent) != 0 {
						t.Fatalf("strict: status %d, %d envelopes sent; want 403 and none", rec.Code, len(sent))
					}
					return
				}
				if rec.Code >= 300 || len(sent) != 1 {
					t.Fatalf("status %d, %d envelopes sent; want the request carried once", rec.Code, len(sent))
				}
				got := involvedSubjects(t, sent[0])
				mu.Lock()
				defer mu.Unlock()
				if refused {
					if len(got) != 0 || !slices.Equal(omitted, []string{involvedOmitRefused}) {
						t.Fatalf("listed %v, omitted %v; want the refused patient left out and the refusal counted", got, omitted)
					}
					return
				}
				if !slices.Equal(got, []string{other}) || len(omitted) != 0 {
					t.Fatalf("listed %v, omitted %v; want only the patient the parent's entry names (%s)", got, omitted, other)
				}
			})
		}
	}
}

// The involved list the Hub records names the member, never the parent a
// dependent's Coverage names as a Patient entry of its own, on every leg that
// lists: the list passes over only the parent's own identity, as it does a
// contained party's, and walks the parent's elements as any resource's, so a
// patient the parent's entry names is listed. A Patient entry that is no party
// (shnsdk.CoverageEntryParties) is listed as any other patient is.
func TestCarriedMembersSkipACoveragesEntryParty(t *testing.T) {
	legs := []string{"crd-order-select", "crd-order-dispatch", "dtr-questionnaire-fetch", "pas-claim", "pas-claim-update", "pas-claim-inquire"}
	rows := map[string]entryParent{}
	for name, p := range entryParentAccepts {
		rows[name] = p
	}
	for name, p := range entryParentRefusals {
		rows[name] = p
	}
	parties := 0
	for name, p := range rows {
		body, _ := withEntryParent(t, serviceRequestSubmitBundle(t, false), p)
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		parentURL, names := "", ""
		for _, e := range doc["entry"].([]any) {
			em := e.(map[string]any)
			res := em["resource"].(map[string]any)
			if res["id"] == "MBR-PARENT" {
				parentURL, _ = em["fullUrl"].(string)
				if links, ok := res["link"].([]any); ok && len(links) > 0 {
					names = "MBR-NOTCOVERED"
				}
			}
		}
		party := shnsdk.CoverageEntryParties(doc)[parentURL]
		if party {
			parties++
		}
		for _, leg := range legs {
			got := carriedMembers(body, leg)
			if !slices.Contains(got, "MBR-COVERED") {
				t.Fatalf("%s/%s: members %v, want the member", name, leg, got)
			}
			if listed := slices.Contains(got, "MBR-PARENT") || slices.Contains(got, parentURL); listed == party {
				t.Fatalf("%s/%s: members %v: the parent listed %v, the sdk names it a party %v", name, leg, got, listed, party)
			}
			if names != "" && !slices.Contains(got, names) {
				t.Fatalf("%s/%s: members %v, want the patient the parent's entry names (%s)", name, leg, got, names)
			}
		}
	}
	if parties == 0 || parties == len(rows) {
		t.Fatalf("%d of %d rows carry the parent as a party: both kinds must be reached", parties, len(rows))
	}
	// The row whose parent's entry names another patient: that patient and
	// the member are listed, the parent is not, on every leg.
	named, _ := withEntryParent(t, serviceRequestSubmitBundle(t, false), entryParentRefusals["the parent's entry names another patient"])
	for _, leg := range legs {
		got := carriedMembers(named, leg)
		if !slices.Equal(got, []string{"MBR-COVERED", "MBR-NOTCOVERED"}) {
			t.Fatalf("%s: members %v, want the member and the patient the parent's entry names, not the parent", leg, got)
		}
	}
	// A parent that names no one adds no one.
	for name, p := range entryParentAccepts {
		body, _ := withEntryParent(t, serviceRequestSubmitBundle(t, false), p)
		for _, leg := range legs {
			if got := carriedMembers(body, leg); !slices.Equal(got, []string{"MBR-COVERED"}) {
				t.Fatalf("%s/%s: members %v, want only the member", name, leg, got)
			}
		}
	}
}
