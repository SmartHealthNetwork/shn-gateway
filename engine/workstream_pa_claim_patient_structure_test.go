//go:build !shn_coreonly

package engine

// A PAS submission's, update's and inquiry's Claim.patient that names its
// Patient by a "#<id>" (one the Claim contains) or a URN (one Bundle entry) is
// read by structure (pasPatientRefMember): it names that Patient's member, as
// Patient/<id> would, and is bound to it on the provider's gateway and the
// payer's alike, at every level, whether the participant's system of record
// holds the member or not. Never to the reference's text, which every request
// naming its patient so would share. A referenced Patient that holds no member
// key names no one: refused 400 on authority at every level.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const (
	cpsBirthDate = "1970-01-01"
	cpsFamily    = "Structure"
)

// cpsContained names the first Claim's patient "#p1", a Patient the Claim
// contains carrying the member identifiers ids (with demographics when demo),
// and leaves every other subject as it is.
func cpsContained(t *testing.T, body []byte, demo bool, ids ...string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	claim := firstClaim(doc)
	if claim == nil {
		t.Fatal("fixture: no Claim")
	}
	p := map[string]any{"resourceType": "Patient", "id": "p1"}
	var identifiers []any
	for _, v := range ids {
		identifiers = append(identifiers, map[string]any{"system": "urn:shn:member", "value": v})
	}
	if identifiers != nil {
		p["identifier"] = identifiers
	}
	if demo {
		p["birthDate"] = cpsBirthDate
		p["name"] = []any{map[string]any{"family": cpsFamily}}
	}
	contained, _ := claim["contained"].([]any)
	claim["contained"] = append(contained, p)
	claim["patient"] = map[string]any{"reference": "#p1"}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// cpsURN gives member's Patient entry the fullUrl urn:uuid:cps (with
// demographics) and names it so from every subject that named Patient/member.
// bare removes the entry's id and its identifiers: it then holds no member key.
func cpsURN(t *testing.T, body []byte, member string, bare bool) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range doc["entry"].([]any) {
		em := e.(map[string]any)
		r, _ := em["resource"].(map[string]any)
		if r["resourceType"] == "Patient" && r["id"] == member {
			em["fullUrl"] = "urn:uuid:cps"
			r["birthDate"] = cpsBirthDate
			r["name"] = []any{map[string]any{"family": cpsFamily}}
			if bare {
				delete(r, "id")
				delete(r, "identifier")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("fixture: no Patient entry %s", member)
	}
	// Every reference that names the member's Patient, in any form, names it
	// by the urn now, Claim.patient first.
	renamed := 0
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if ref, ok := v["reference"].(string); ok {
				if id, _, ok := readPatientReference(ref); ok && id == member {
					v["reference"] = "urn:uuid:cps"
					renamed++
				}
			}
			for _, c := range v {
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(doc)
	if claim := firstClaim(doc); claim == nil || renamed == 0 {
		t.Fatal("fixture: nothing named the member's Patient")
	} else if slot, _ := claim["patient"].(map[string]any); slot["reference"] != "urn:uuid:cps" {
		t.Fatalf("fixture: Claim.patient is %v, not the urn", slot["reference"])
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// cpsStranger renames the fixture's member to one no system of record holds.
func cpsStranger(b []byte, to string) []byte {
	return []byte(strings.ReplaceAll(string(b), "MBR-COVERED", to))
}

// cpsForm is one way of naming the member's Patient by structure.
type cpsForm struct {
	name string
	demo bool // the referenced Patient carries a birth date and family name
	make func(t *testing.T, body []byte, member string) []byte
	// textPCI is the identifier the reference's own text would derive, which
	// earlier releases bound such a request to.
	textPCI string
}

var cpsForms = []cpsForm{
	{"a contained Patient with demographics", true, func(t *testing.T, b []byte, m string) []byte { return cpsContained(t, b, true, m) },
		derivedPCI("#p1", cpsBirthDate, cpsFamily)},
	{"a contained Patient without demographics", false, func(t *testing.T, b []byte, m string) []byte { return cpsContained(t, b, false, m) },
		derivedPCI("#p1", "", "")},
	{"a urn:uuid Patient entry", true, func(t *testing.T, b []byte, m string) []byte { return cpsURN(t, b, m, false) },
		derivedPCI("urn:uuid:cps", cpsBirthDate, cpsFamily)},
}

// cpsWant is the identifier member binds to: its held one, else the one derived
// from the referenced Patient.
func cpsWant(held string, member string, demo bool) string {
	if held != "" {
		return held
	}
	if demo {
		return derivedPCI(member, cpsBirthDate, cpsFamily)
	}
	return derivedPCI(member, "", "")
}

// cpsProviderSend posts body to the provider's PAS ingress at level and returns
// the answer and the token subjects the gateway asked the Hub for.
func cpsProviderSend(t *testing.T, level ConformanceEnforcement, body []byte, records *exchangeRecords) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	if records != nil {
		env.originator.cfg.ExchangeObserved = records.observe
	}
	env.payerReturns(LegResult{Response: testResponse(levelPASPayerAnswer)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/fhir+json")
	env.originator.ingressRoute(RoutePAS)(rec, req)
	return rec, slices.Clone(env.substrate.authorizedSubjects)
}

// cpsProviderBase is the provider ingress fixture for leg.
func cpsProviderBase(t *testing.T, leg string) []byte {
	if leg == "pas-claim" {
		return []byte(pasIngressBundle("00001", ""))
	}
	return readConformantGolden(t, "pas-update-request.json")
}

func cpsHeldPCI(t *testing.T, member string) string {
	t.Helper()
	pci, _, ok := newCensusSoR().ResolvePatient(member)
	if !ok {
		t.Fatalf("%s is not held", member)
	}
	return pci
}

func TestPASClaimPatientByStructure_TheProviderBindsItsMember(t *testing.T) {
	t.Parallel()
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		for _, held := range []bool{true, false} {
			for _, form := range cpsForms {
				for _, level := range allLevels {
					t.Run(strings.Join([]string{leg, map[bool]string{true: "held", false: "unheld"}[held], form.name, level.String()}, "/"), func(t *testing.T) {
						base, member, heldPCI := cpsProviderBase(t, leg), "MBR-COVERED", ""
						if held {
							heldPCI = cpsHeldPCI(t, member)
						} else {
							base, member = cpsStranger(base, "MBR-STRUCTURE"), "MBR-STRUCTURE"
						}
						rec, subjects := cpsProviderSend(t, level, form.make(t, base, member), nil)
						want := cpsWant(heldPCI, member, form.demo)
						if rec.Code != http.StatusOK || !slices.Equal(subjects, []string{want}) {
							t.Fatalf("answer %d %s, bound %v; want carried and bound to %s", rec.Code, rec.Body, subjects, want)
						}
					})
				}
			}
		}
	}
}

// cpsPayerSend sends leg's body to the payer's gateway at level under a token
// naming subject, and returns the answer and whether its own binding differed
// from the token's.
func cpsPayerSend(t *testing.T, level ConformanceEnforcement, leg string, member string, transform func([]byte) []byte, subject string) (payerAnswer, bool, *levelPayer) {
	t.Helper()
	p := newLevelPayer(t, level)
	differs := false
	prev := p.g.cfg.Observer
	p.g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == SubjectBindingDiffersEvent {
			differs = true
		}
		prev(e)
	}
	var base []byte
	if leg == "pas-claim" {
		base = originatorBuiltConformantBundle(t, "MBR-COVERED")
	} else {
		req, related := updateBundle(t)
		p.seedPend(t, related)
		base = req
	}
	if member != "MBR-COVERED" {
		base = cpsStranger(base, member)
	}
	return p.sendAs(t, leg, "", transform(base), subject), differs, p
}

func TestPASClaimPatientByStructure_ThePayerBindsItsMember(t *testing.T) {
	t.Parallel()
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		for _, held := range []bool{true, false} {
			for _, form := range cpsForms {
				for _, level := range allLevels {
					t.Run(strings.Join([]string{leg, map[bool]string{true: "held", false: "unheld"}[held], form.name, level.String()}, "/"), func(t *testing.T) {
						member, heldPCI := "MBR-COVERED", ""
						if held {
							heldPCI = cpsHeldPCI(t, member)
						} else {
							member = "MBR-STRUCTURE"
						}
						want := cpsWant(heldPCI, member, form.demo)
						ans, differs, p := cpsPayerSend(t, level, leg, member, func(b []byte) []byte { return form.make(t, b, member) }, want)
						if ans.status != http.StatusOK || p.partner.lastPath == "" || differs {
							t.Fatalf("answer %d %s (reached %q, binding differs %v); want carried, bound to the token's %s", ans.status, ans.body, p.partner.lastPath, differs, want)
						}
						// The reference's own text binds no one: a token naming the
						// identifier it would derive is not this gateway's binding.
						_, differs, _ = cpsPayerSend(t, level, leg, member, func(b []byte) []byte { return form.make(t, b, member) }, form.textPCI)
						if !differs {
							t.Fatal("a token naming the identifier derived from the reference's text matched the binding")
						}
					})
				}
			}
		}
	}
}

// Two members whose requests each name a contained Patient "#p1" with no
// demographics are two patients: each is bound to its own identifier, on both
// gateways, and the payer records each pended request under its own member.
// Before, both were bound to derivedPCI("#p1", "", ""), one identifier.
func TestPASClaimPatientByStructure_TwoMembersNeverShareAnIdentifier(t *testing.T) {
	t.Parallel()
	members := []string{"MBR-STRUCTURE-A", "MBR-STRUCTURE-B"}
	for _, level := range allLevels {
		t.Run("provider/"+level.String(), func(t *testing.T) {
			var bound []string
			for _, m := range members {
				body := cpsContained(t, cpsStranger([]byte(pasIngressBundle("00001", "")), m), false, m)
				rec, subjects := cpsProviderSend(t, level, body, nil)
				if rec.Code != http.StatusOK || len(subjects) != 1 {
					t.Fatalf("%s: answer %d %s, bound %v", m, rec.Code, rec.Body, subjects)
				}
				bound = append(bound, subjects[0])
			}
			if want := []string{derivedPCI(members[0], "", ""), derivedPCI(members[1], "", "")}; !slices.Equal(bound, want) {
				t.Fatalf("bound %v, want %v", bound, want)
			}
		})
		t.Run("payer pend ledger/"+level.String(), func(t *testing.T) {
			// One payer gateway, one ledger: each member's pended submission is
			// recorded under that member's own identifier.
			p := newLevelPayer(t, level)
			for _, m := range members {
				body := cpsContained(t, cpsStranger(originatorBuiltConformantBundle(t, "MBR-COVERED"), m), false, m)
				if ans := p.sendAs(t, "pas-claim", "", body, derivedPCI(m, "", "")); ans.status != http.StatusOK {
					t.Fatalf("%s: answer %d %s", m, ans.status, ans.body)
				}
			}
			ledger, ok := testLedger(p.store).(*memPendLedger)
			if !ok {
				t.Fatalf("the test store's ledger is a %T", testLedger(p.store))
			}
			ledger.mu.Lock()
			var subjects []string
			for _, row := range ledger.pendedClaims {
				subjects = append(subjects, row.subjectPCI)
			}
			ledger.mu.Unlock()
			slices.Sort(subjects)
			want := []string{derivedPCI(members[0], "", ""), derivedPCI(members[1], "", "")}
			slices.Sort(want)
			if !slices.Equal(subjects, want) {
				t.Fatalf("pended under %v, want each member's own %v", subjects, want)
			}
		})
	}
}

// The referenced Patient holds no member key: no member identifier, two
// different ones, or (an entry) neither an id nor a member identifier. It names
// no one, so it is refused 400 on authority at every level, on the provider's
// gateway and the payer's, on a submission, an update and an inquiry, before
// anything is bound or reaches the participant's system.
func TestPASClaimPatientByStructure_APatientWithNoMemberKeyNamesNoOne(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		make func(t *testing.T, b []byte, member string) []byte
	}{
		{"a contained Patient with no member identifier", func(t *testing.T, b []byte, _ string) []byte { return cpsContained(t, b, true) }},
		{"a contained Patient with two member identifiers", func(t *testing.T, b []byte, m string) []byte { return cpsContained(t, b, true, m, "MBR-OTHER") }},
		{"a urn:uuid Patient entry with no id and no member identifier", func(t *testing.T, b []byte, m string) []byte { return cpsURN(t, b, m, true) }},
	}
	for _, row := range rows {
		for _, level := range allLevels {
			name := row.name + "/" + level.String()
			t.Run("provider submission/"+name, func(t *testing.T) {
				var got exchangeRecords
				rec, subjects := cpsProviderSend(t, level, row.make(t, []byte(pasIngressBundle("00001", "")), "MBR-COVERED"), &got)
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "holds no member identifier") || len(subjects) != 0 {
					t.Fatalf("answer %d %s, bound %v; want 400 %q, nothing bound", rec.Code, rec.Body, subjects, msgPatientCarriesNoMember)
				}
				if x := got.only(t); x.Outcome != ExchangeRefused || x.RefusalReason != msgPatientCarriesNoMember {
					t.Fatalf("recorded %s %q, want refused for %q", x.Outcome, x.RefusalReason, msgPatientCarriesNoMember)
				}
			})
			for _, leg := range []string{"pas-claim", "pas-claim-update"} {
				t.Run("payer "+leg+"/"+name, func(t *testing.T) {
					ans, _, p := cpsPayerSend(t, level, leg, "MBR-COVERED", func(b []byte) []byte { return row.make(t, b, "MBR-COVERED") }, cpsHeldPCI(t, "MBR-COVERED"))
					p.wantRefused(t, ans, http.StatusBadRequest, "holds no member identifier")
				})
			}
		}
	}
	// The inquiry reads Claim.patient by the same reader, on the provider's
	// ingress and the payer's gateway alike.
	for _, level := range allLevels {
		t.Run("provider inquiry/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			body := cpsContained(t, []byte(levelInquiry(t, "MBR-COVERED", "MBR-COVERED")), true)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/Claim/$inquire", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/fhir+json")
			env.originator.ingressRoute(RoutePASInquire)(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "holds no member identifier") || len(env.substrate.authorizedSubjects) != 0 {
				t.Fatalf("answer %d %s, bound %v; want 400 %q, nothing bound", rec.Code, rec.Body, env.substrate.authorizedSubjects, msgPatientCarriesNoMember)
			}
			if x := got.only(t); x.Outcome != ExchangeRefused || x.RefusalReason != msgPatientCarriesNoMember {
				t.Fatalf("recorded %s %q, want refused for %q", x.Outcome, x.RefusalReason, msgPatientCarriesNoMember)
			}
		})
	}
	for _, level := range allLevels {
		t.Run("payer inquiry/"+level.String(), func(t *testing.T) {
			const m = dtrFrameMember
			contained := `"contained":[{"resourceType":"Patient","id":"p1"}],`
			p, got := newRecordingLevelPayer(t, level)
			ans := p.sendAs(t, "pas-claim-inquire", "", pasRefBody(t, "pas-claim-inquire", m, "#p1", contained), p.pci)
			p.wantRefused(t, ans, http.StatusBadRequest, "holds no member identifier")
			if x := got.only(t); x.Outcome != ExchangeRefused {
				t.Fatalf("recorded %s, want refused", x.Outcome)
			}
		})
	}
}

// The request's named patients (the Audit Plane's involved list) read the
// subject as the binding does: a request naming its Patient by "#<id>" or URN
// names its member as the subject, never as another patient beside a subject
// named by the reference's text.
func TestPASClaimPatientByStructure_TheSubjectIsNoOtherPatient(t *testing.T) {
	t.Parallel()
	base := []byte(strings.Replace(pasIngressBundle("00001", ""), `{"resource":{"resourceType":"Patient","id":"MBR-COVERED"}},`, "", 1))
	for name, body := range map[string][]byte{
		"a contained Patient":      cpsContained(t, base, true, "MBR-COVERED"),
		"a urn:uuid Patient entry": cpsURN(t, []byte(pasIngressBundle("00001", "")), "MBR-COVERED", false),
	} {
		t.Run(name, func(t *testing.T) {
			ms, err := paWorkstream{}.requestMembers("pas-claim", body)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range ms {
				if !m.subject || m.shared || m.unreadable {
					t.Fatalf("named %+v: every name is the subject's", ms)
				}
			}
		})
	}
	// A contained Patient's id is local to the Claim: a reference elsewhere
	// naming Patient/<that id> names another patient, never the subject.
	t.Run("Patient/<the contained id> elsewhere", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal(base, &doc); err != nil {
			t.Fatal(err)
		}
		doc["entry"] = append(doc["entry"].([]any), map[string]any{"resource": map[string]any{"resourceType": "Observation",
			"status": "final", "code": map[string]any{"text": "x"}, "subject": map[string]any{"reference": "Patient/p1"}}})
		withOther, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		ms, err := paWorkstream{}.requestMembers("pas-claim", cpsContained(t, withOther, true, "MBR-COVERED"))
		if err != nil {
			t.Fatal(err)
		}
		seen := false
		for _, m := range ms {
			if m.member == "p1" {
				seen = true
				if m.subject {
					t.Fatalf("named %+v: Patient/p1 is another patient, not the subject", ms)
				}
			}
		}
		if !seen {
			t.Fatalf("named %+v: Patient/p1 is not named at all", ms)
		}
	})
}
