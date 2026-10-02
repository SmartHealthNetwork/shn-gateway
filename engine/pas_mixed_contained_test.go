package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// A PAS request whose member's Coverage carries, inside it, a resource naming
// another member names another patient as surely as an order for one does:
// patient.mixed reads every resource the bundle carries, contained
// and nested ones included, and every reference in them. A Coverage's party
// (coverageParty: a dependent's parent, named by its subscriber or
// policyHolder) is another person the Coverage names, so only its own
// identity is not checked; what it names is.

// pasCarried is what a row adds to the request: a resource in the member's
// Coverage, an entry of its own, identifiers on the member's Patient entry, or
// any of them together.
type pasCarried struct {
	resource string // the contained resource, its id "x"
	party    bool   // named by the Coverage's subscriber, not an extension
	entry    string // a Bundle entry added to the request
	// subscriber is the literal reference the member's Coverage names its
	// subscriber by.
	subscriber string
	// memberIdentifiers are identifiers (a JSON list's items) added to the
	// member's Patient entry.
	memberIdentifiers string
}

// pasAnotherPatientCarried are the resources naming another member, each as
// the dry run probed them through the real chain.
var pasAnotherPatientCarried = map[string]pasCarried{
	"a RelatedPerson for another patient":                          {resource: `{"resourceType":"RelatedPerson","id":"x","patient":{"reference":"Patient/MBR-NOTCOVERED"}}`},
	"a Patient carrying another member's identifier":               {resource: `{"resourceType":"Patient","id":"x","identifier":[{"system":"urn:shn:member","value":"MBR-NOTCOVERED"}]}`},
	"a RelatedPerson for another patient, typed":                   {resource: `{"resourceType":"RelatedPerson","id":"x","patient":{"type":"Patient","reference":"Patient/MBR-NOTCOVERED"}}`},
	"a RelatedPerson for another member by identifier":             {resource: `{"resourceType":"RelatedPerson","id":"x","patient":{"identifier":{"system":"urn:shn:member","value":"MBR-NOTCOVERED"}}}`},
	"a RelatedPerson for another member by identifier, typed":      {resource: `{"resourceType":"RelatedPerson","id":"x","patient":{"type":"Patient","identifier":{"system":"urn:shn:member","value":"MBR-NOTCOVERED"}}}`},
	"a RelatedPerson for another patient at an absolute reference": {resource: `{"resourceType":"RelatedPerson","id":"x","patient":{"reference":"http://localhost/fhir/Patient/MBR-NOTCOVERED"}}`},
	"a RelatedPerson for another patient at a foreign base":        {resource: `{"resourceType":"RelatedPerson","id":"x","patient":{"reference":"https://other.example/fhir/Patient/MBR-NOTCOVERED"}}`},
	"an Observation about another patient":                         {resource: `{"resourceType":"Observation","id":"x","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/MBR-NOTCOVERED"}}`},
	"a Patient-typed reference to another member in any element":   {resource: `{"resourceType":"Organization","id":"x","extension":[{"url":"http://example.org/fhir/StructureDefinition/about","valueReference":{"type":"Patient","identifier":{"system":"urn:shn:member","value":"MBR-NOTCOVERED"}}}]}`},
	"a party naming another patient":                               {resource: `{"resourceType":"Patient","id":"x","identifier":[{"system":"urn:shn:member","value":"MBR-PARENT"}],"link":[{"type":"seealso","other":{"reference":"Patient/MBR-NOTCOVERED"}}]}`, party: true},
	"a party naming another member by identifier":                  {resource: `{"resourceType":"Patient","id":"x","identifier":[{"system":"urn:shn:member","value":"MBR-PARENT"}],"generalPractitioner":[{"type":"Patient","identifier":{"system":"urn:shn:member","value":"MBR-NOTCOVERED"}}]}`, party: true},
	// A fullUrl naming the member does not make an entry the member: its id
	// says it is another.
	"another member's Patient entry under the member's fullUrl": {entry: `{"fullUrl":"https://ehr.example/fhir/Patient/MBR-COVERED","resource":{"resourceType":"Patient","id":"MBR-NOTCOVERED"}}`},
	// A Patient carrying another member's member identifier is another
	// patient, whatever else it carries.
	"the member's Patient entry carrying another member's identifier too": {memberIdentifiers: `{"system":"urn:shn:member","value":"MBR-COVERED"},{"system":"urn:shn:member","value":"MBR-NOTCOVERED"}`},
	"a Patient carrying both members' identifiers":                        {resource: `{"resourceType":"Patient","id":"x","identifier":[{"system":"urn:shn:member","value":"MBR-COVERED"},{"system":"urn:shn:member","value":"MBR-NOTCOVERED"}]}`},
	// A party is a contained Patient; the parent carried as a Patient entry
	// of its own, even one the member's Coverage names as its subscriber, is
	// another patient.
	"the parent as a Patient entry of its own, named by the subscriber": {
		entry:      `{"fullUrl":"http://localhost/fhir/Patient/MBR-PARENT","resource":{"resourceType":"Patient","id":"MBR-PARENT","identifier":[{"system":"urn:shn:member","value":"MBR-PARENT"}]}}`,
		subscriber: "Patient/MBR-PARENT",
	},
	// A party is a contained Patient; a subscriber named as a Patient
	// reference is another patient.
	"a subscriber naming the parent as a literal Patient reference": {subscriber: "Patient/MBR-PARENT"},
	// A member identifier names that member in any element, typed or not.
	"an untyped reference to another member by identifier in any element": {resource: `{"resourceType":"Organization","id":"x","extension":[{"url":"http://example.org/fhir/StructureDefinition/about","valueReference":{"identifier":{"system":"urn:shn:member","value":"MBR-NOTCOVERED"}}}]}`},
}

// pasSamePatientCarried are resources the member's Coverage may carry at
// every level: each names only the member, or is the Coverage's party.
var pasSamePatientCarried = map[string]pasCarried{
	"a RelatedPerson for the member":                 {resource: `{"resourceType":"RelatedPerson","id":"x","patient":{"reference":"Patient/MBR-COVERED"}}`},
	"a RelatedPerson for the member by identifier":   {resource: `{"resourceType":"RelatedPerson","id":"x","patient":{"identifier":{"system":"urn:shn:member","value":"MBR-COVERED"}}}`},
	"the member's own Patient":                       {resource: `{"resourceType":"Patient","id":"x","identifier":[{"system":"urn:shn:member","value":"MBR-COVERED"}]}`},
	"a dependent's parent, the Coverage's party":     {resource: `{"resourceType":"Patient","id":"x","identifier":[{"system":"urn:shn:member","value":"MBR-PARENT"}]}`, party: true},
	"an untyped reference by identifier in any slot": {resource: `{"resourceType":"Organization","id":"x","extension":[{"url":"http://example.org/fhir/StructureDefinition/about","valueReference":{"identifier":{"system":"http://example.org/npi","value":"1999999999"}}}]}`},
	"a versioned reference to the member":            {resource: `{"resourceType":"RelatedPerson","id":"x","patient":{"reference":"Patient/MBR-COVERED/_history/2"}}`},
	"an absolute versioned reference to the member":  {resource: `{"resourceType":"RelatedPerson","id":"x","patient":{"reference":"http://localhost/fhir/Patient/MBR-COVERED/_history/2"}}`},
	"a reference by an identifier the member's Patient entry carries": {
		resource:          `{"resourceType":"RelatedPerson","id":"x","patient":{"identifier":{"system":"http://example.org/mrn","value":"MRN-1"}}}`,
		memberIdentifiers: `{"system":"http://example.org/mrn","value":"MRN-1"}`,
	},
}

// withCoverageCarrying is bundle with what c carries: its resource contained
// in the first Coverage (named from an extension, or as the Coverage's
// subscriber for a party), its entry added, and its identifiers added to the
// member's Patient entry.
func withCoverageCarrying(t *testing.T, bundle []byte, c pasCarried) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatal(err)
	}
	entries := b["entry"].([]any)
	if c.resource != "" {
		var res map[string]any
		if err := json.Unmarshal([]byte(c.resource), &res); err != nil {
			t.Fatalf("fixture %s: %v", c.resource, err)
		}
		found := false
		for _, e := range entries {
			cov := e.(map[string]any)["resource"].(map[string]any)
			if cov["resourceType"] != "Coverage" {
				continue
			}
			cov["contained"] = []any{res}
			if c.party {
				cov["subscriber"] = map[string]any{"reference": "#x"}
			} else {
				cov["extension"] = []any{map[string]any{"url": "http://example.org/fhir/StructureDefinition/carried", "valueReference": map[string]any{"reference": "#x"}}}
			}
			found = true
			break
		}
		if !found {
			t.Fatal("fixture: no Coverage entry")
		}
	}
	if c.subscriber != "" {
		found := false
		for _, e := range entries {
			cov := e.(map[string]any)["resource"].(map[string]any)
			if cov["resourceType"] == "Coverage" {
				cov["subscriber"] = map[string]any{"reference": c.subscriber}
				found = true
				break
			}
		}
		if !found {
			t.Fatal("fixture: no Coverage entry")
		}
	}
	if c.memberIdentifiers != "" {
		var ids []any
		if err := json.Unmarshal([]byte("["+c.memberIdentifiers+"]"), &ids); err != nil {
			t.Fatalf("fixture %s: %v", c.memberIdentifiers, err)
		}
		found := false
		for _, e := range entries {
			p := e.(map[string]any)["resource"].(map[string]any)
			if p["resourceType"] != "Patient" || p["id"] != "MBR-COVERED" {
				continue
			}
			have, _ := p["identifier"].([]any)
			p["identifier"] = append(have, ids...)
			found = true
		}
		if !found {
			t.Fatal("fixture: no Patient entry for the member")
		}
	}
	if c.entry != "" {
		var entry map[string]any
		if err := json.Unmarshal([]byte(c.entry), &entry); err != nil {
			t.Fatalf("fixture %s: %v", c.entry, err)
		}
		b["entry"] = append(entries, entry)
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// noPatientMixed asserts no finding names patient.mixed.
func noPatientMixed(t *testing.T, findings []ConformanceFinding) {
	t.Helper()
	for _, f := range findings {
		if f.Rule == RulePatientMixed {
			t.Fatalf("a patient.mixed finding for a bundle naming only the member: %+v", f)
		}
	}
}

// The provider's PAS ingress, POST /Claim/$submit.
func TestLevelPASIngress_AnotherPatientCarried(t *testing.T) {
	for name, c := range pasAnotherPatientCarried {
		body := string(withCoverageCarrying(t, []byte(pasIngressBundle("00001", "")), c))
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
				wantLegLevelOutcome(t, "pas-claim", level, env, rec, ev.findings, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS bundle")
				if !refusesAt(level, RulePatientMixed) {
					wantCarriedExactly(t, env, body)
				}
			})
		}
	}
}

func TestLevelPASIngress_SamePatientCarried(t *testing.T) {
	for name, c := range pasSamePatientCarried {
		body := string(withCoverageCarrying(t, []byte(pasIngressBundle("00001", "")), c))
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s, network hits %d: want carried", rec.Code, rec.Body.String(), env.routeHitCount())
				}
				wantCarriedExactly(t, env, body)
				noPatientMixed(t, ev.findings)
			})
		}
	}
}

// The payer's gateway, binding a $submit and an amendment from the network.
func TestLevelPayerPAS_AnotherPatientCarried(t *testing.T) {
	for _, leg := range []struct{ name, path string }{{"pas-claim", pasSubmitPath}, {"pas-claim-update", pasSubmitPath}} {
		for name, c := range pasAnotherPatientCarried {
			for _, level := range allLevels {
				t.Run(leg.name+"/"+name+"/"+level.String(), func(t *testing.T) {
					p := newLevelPayer(t, level)
					body := pasPayerBody(t, p, leg.name, c)
					got := p.send(t, leg.name, "", body)
					p.wantRequestRow(t, leg.name, got, body, leg.path, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS bundle")
				})
			}
		}
	}
}

func TestLevelPayerPAS_SamePatientCarried(t *testing.T) {
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		for name, c := range pasSamePatientCarried {
			for _, level := range allLevels {
				t.Run(leg+"/"+name+"/"+level.String(), func(t *testing.T) {
					p := newLevelPayer(t, level)
					body := pasPayerBody(t, p, leg, c)
					got := p.send(t, leg, "", body)
					if got.status != http.StatusOK || p.partner.lastPath != pasSubmitPath {
						t.Fatalf("answer %d %s at %q: want forwarded", got.status, got.body, p.partner.lastPath)
					}
					noPatientMixed(t, p.findings)
				})
			}
		}
	}
}

// pasPayerBody is the request leg carries to a payer, its Coverage carrying c:
// a $submit, or an amendment of a pend this gateway holds.
func pasPayerBody(t *testing.T, p *levelPayer, leg string, c pasCarried) []byte {
	t.Helper()
	if leg == "pas-claim" {
		return withCoverageCarrying(t, []byte(pasIngressBundle("00001", "")), c)
	}
	request, related := updateBundle(t)
	p.seedPend(t, related)
	return withCoverageCarrying(t, request, c)
}

// The native responder reads the bundle again before it posts it to the
// payer's system: what strict refuses there never reaches it, and below
// strict the bundle is posted as it came.
func TestNativeResponder_AnotherPatientCarried(t *testing.T) {
	for name, c := range pasAnotherPatientCarried {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(relayDecidedAnswer(t, "cr-mixed", "trace-mixed", "AUTH-MIXED"))})
				n, ctx := relayResponder(t, srv, newCensusSoR())
				n.conformance = NewConformancePolicy(level)
				body := withCoverageCarrying(t, serviceRequestSubmitBundle(t, false), c)
				res, err := n.Handle(ctx, "pas-claim", "corr-mixed", "PCI-1", body)
				payer.mu.Lock()
				defer payer.mu.Unlock()
				if refusesAt(level, RulePatientMixed) {
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

// What names only the member reaches the payer's system at every level,
// strict included. The responder's own read decides only and records no
// finding; whether a finding is recorded is the payer's inbound bind's, on the
// same payer-native seam (TestLevelPayerPAS_AnotherPatientCarried,
// wantRequestRow).
func TestNativeResponder_SamePatientCarried(t *testing.T) {
	for name, c := range pasSamePatientCarried {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(relayDecidedAnswer(t, "cr-same", "trace-same", "AUTH-SAME"))})
				n, ctx := relayResponder(t, srv, newCensusSoR())
				n.conformance = NewConformancePolicy(level)
				body := withCoverageCarrying(t, serviceRequestSubmitBundle(t, false), c)
				res, err := n.Handle(ctx, "pas-claim", "corr-same", "PCI-1", body)
				payer.mu.Lock()
				defer payer.mu.Unlock()
				if err != nil || res.Status != 0 || len(payer.calls) != 1 || !bytes.Equal(payer.calls[0].body, body) {
					t.Fatalf("err=%v status=%d %q, %d payer calls: want the bundle posted as it came", err, res.Status, res.Message, len(payer.calls))
				}
			})
		}
	}
}

// Another patient's Patient carried as an entry of its own is another
// patient in the request too, the request's own Patient entry beside it.
func TestLevelPASIngress_AnotherPatientEntry(t *testing.T) {
	body := levelPASBundleWithEntry(`{"fullUrl":"http://localhost/fhir/Patient/MBR-NOTCOVERED","resource":{"resourceType":"Patient","id":"MBR-NOTCOVERED"}}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
			wantLegLevelOutcome(t, "pas-claim", level, env, rec, ev.findings, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS bundle")
			if !refusesAt(level, RulePatientMixed) {
				wantCarriedExactly(t, env, body)
			}
		})
	}
}

// The inquiry, Claim/$inquire, is read the same way: at the provider's
// ingress and at the payer's gateway.
func TestLevelInquire_AnotherPatientCarried(t *testing.T) {
	for name, c := range pasAnotherPatientCarried {
		ingress := string(withCoverageCarrying(t, []byte(levelInquiry(t, "MBR-COVERED", "")), c))
		payer := withCoverageCarrying(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), c)
		for _, level := range allLevels {
			t.Run("ingress/"+name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelInquireRow(t, level, ingress, levelInquiryPayerAnswer(t))
				wantLegLevelOutcome(t, "pas-claim-inquire", level, env, rec, ev.findings, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS inquiry")
				if !refusesAt(level, RulePatientMixed) {
					wantCarriedExactly(t, env, ingress)
				}
			})
			t.Run("payer/"+name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "pas-claim-inquire", "", payer)
				p.wantRequestRow(t, "pas-claim-inquire", got, payer, pasInquirePath, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS inquiry")
			})
		}
	}
}

func TestLevelInquire_SamePatientCarried(t *testing.T) {
	for name, c := range pasSamePatientCarried {
		ingress := string(withCoverageCarrying(t, []byte(levelInquiry(t, "MBR-COVERED", "")), c))
		payer := withCoverageCarrying(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), c)
		for _, level := range allLevels {
			t.Run("ingress/"+name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelInquireRow(t, level, ingress, levelInquiryPayerAnswer(t))
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s, network hits %d: want carried", rec.Code, rec.Body.String(), env.routeHitCount())
				}
				wantCarriedExactly(t, env, ingress)
				noPatientMixed(t, ev.findings)
			})
			t.Run("payer/"+name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "pas-claim-inquire", "", payer)
				if got.status != http.StatusOK || p.partner.lastPath != pasInquirePath {
					t.Fatalf("answer %d %s at %q: want forwarded", got.status, got.body, p.partner.lastPath)
				}
				noPatientMixed(t, p.findings)
			})
		}
	}
}
