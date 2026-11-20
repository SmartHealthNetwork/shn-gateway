//go:build !shn_coreonly

package engine

import (
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// differentialCase is one body the notification's involved-list reading
// (pasNotifiedMembers) and the request legs' shared reading (carriedMembers)
// both read: a notification, or a request payload carried in a
// SubscriptionStatus parameter beside a decision naming the leg's patient
// zz-leg (asEntry; the shared reader reads the payload alone).
type differentialCase struct {
	name    string
	body    string
	asEntry bool
	// unreadable is a body whose patients neither reading can read: the
	// notification's reading is errRequestPatientsUnreadable, and so is the
	// request legs' (requestMembers), which names no member of it.
	unreadable bool
	// divergence names the ruling that explains why the two readings name
	// different members ("" when they must name the same), and s3Only and
	// sharedOnly are exactly the members each names that the other does not.
	divergence         string
	s3Only, sharedOnly []string
}

// The enumerated divergences between the two readings, each tagged with the
// ruling that explains it.
const (
	divOnePerson    = "one Patient one person: a reference under a carried Patient's member identifier names that Patient, not another member"
	divIDless       = "an id-less Patient is named by its fullUrl, or its place in the notification"
	divURNScope     = "a urn resolves in its own Bundle and outward, never in a sibling Bundle (FHIR R4)"
	divPayorParty   = "a Coverage's payor names its party, as the shared party slots (partySlots) read it; carriedMembers reads only subscriber and policyHolder"
	divPartyReading = "a Coverage's party as the shared binding rule reads it (visitCarriedPatients): a Patient a subscriber names, though it carries a name another Patient shares, which carriedMembers' entry-party reading (CoverageEntryParties) keeps as a member"
)

// differentialMembers are the members each reading names of c's body,
// beside the leg's own patient. A readable body's notification reading names
// the leg's own patient as its subject (zz-leg, for a body carried as an
// entry), so a reading that refuses every body, or names no one, fails here
// rather than agree with a shared reader that names no one either.
func differentialMembers(t *testing.T, c differentialCase) (s3, shared []string) {
	t.Helper()
	body := []byte(c.body)
	if c.asEntry {
		leg := decision("collection", withRequest(decisionCR("cr1", "Patient/zz-leg", pasProviderNPI)))
		body = withStatusParameter(contentEvent(leg), `{"name":"x","resource":`+c.body+`}`)
	}
	members, err := pasNotifiedMembers(body, "https://stated.example/fhir")
	if c.unreadable {
		if members != nil || !errors.Is(err, errRequestPatientsUnreadable) {
			t.Fatalf("the notification's reading names %d members (%v), want none and that its patients cannot be read", len(members), err)
		}
		if got, err := (paWorkstream{}).requestMembers("pas-claim", []byte(c.body)); got != nil || !errors.Is(err, errRequestPatientsUnreadable) {
			t.Fatalf("the request legs' reading names %d members (%v), want none and that its patients cannot be read", len(got), err)
		}
	} else if err != nil {
		t.Fatalf("the notification's reading: %v, want its members", err)
	}
	set := map[string]bool{}
	ownSubject := false
	for _, m := range members {
		if m.subject && (!c.asEntry || m.member == "zz-leg") {
			ownSubject = true
		}
		if !(c.asEntry && m.member == "zz-leg") {
			set[m.member] = true
		}
	}
	if !c.unreadable && !ownSubject {
		t.Fatalf("the notification's reading names %v, none of them the leg's own patient as its subject", members)
	}
	for m := range set {
		s3 = append(s3, m)
	}
	sort.Strings(s3)
	if c.asEntry {
		shared = carriedMembers([]byte(c.body), legPASSubscriptionNotify)
	} else {
		shared = carriedMembers(body, legPASSubscriptionNotify)
	}
	return s3, shared
}

// The notification's involved-list reading and the request legs' shared
// reading (carriedMembers, for this leg) name the same members of every body
// in the corpus, at the member level carriedMembers can express (each member
// string, once), but for the enumerated divergences above, each pinned to
// exactly the members each side names alone. The corpus is the shared
// reader's own test rows (involvedRequest, the repeated-name rows and their
// controls), carried as an entry beside a decision, and the notification
// path's own probe and row fixtures. A new divergence, or one that closes,
// fails here: the readings must not drift apart unremarked.
func TestPASNotify_TheInvolvedListAgreesWithTheSharedReader(t *testing.T) {
	cr := decisionCR("cr1", "Patient/p1", pasProviderNPI)
	one := string(contentEvent(decision("collection", cr, decisionPatient("p1"))))
	param := func(body, resource string) string {
		return string(withStatusParameter([]byte(body), `{"name":"x","resource":`+resource+`}`))
	}
	task := func(ref string) string {
		return `{"fullUrl":"` + decisionBase + `/Task/t9","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"` + ref + `"}}}`
	}
	p2 := func(identifier string) string {
		return `{"fullUrl":"` + decisionBase + `/Patient/p2","resource":{"resourceType":"Patient","id":"p2",` + identifier + `"birthDate":"1990-02-02","name":[{"family":"Roe"}]}}`
	}
	mbr77 := `"identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-77"}],`
	const z = "urn:uuid:11111111-1111-4111-8111-111111111111"
	patientZ := `{"fullUrl":"` + z + `","resource":{"resourceType":"Patient","id":"z","birthDate":"1960-01-01","name":[{"family":"Parent"}]}}`
	taskZ := `{"fullUrl":"urn:uuid:33333333-3333-4333-8333-333333333333","resource":{"resourceType":"Task","status":"requested","intent":"order","for":{"reference":"` + z + `"}}}`
	coverageZ := `{"fullUrl":"urn:uuid:22222222-2222-4222-8222-222222222222","resource":{"resourceType":"Coverage","status":"active","subscriber":{"reference":"` + z + `"}}}`
	bundle := func(entries ...string) string {
		return `{"resourceType":"Bundle","type":"collection","entry":[` + strings.Join(entries, ",") + `]}`
	}
	nested := func(urn, b string) string { return `{"fullUrl":"` + urn + `","resource":` + b + `}` }
	cases := []differentialCase{
		// The shared reader's own rows.
		{name: "involvedRequest", body: string(involvedRequest()), asEntry: true},
		{name: "a reference repeated", body: `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/MBR-OX","reference":"Patient/MBR-PAYERB"}}}]}`, asEntry: true, unreadable: true},
		{name: "a repeated reference's control", body: `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/MBR-PAYERB"}}}]}`, asEntry: true},
		{name: "a Patient id beside one in another case", body: `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"MBR-PAYERB","ID":"MBR-OX"}}]}`, asEntry: true, unreadable: true},
		{name: "a Patient id's control", body: `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"MBR-PAYERB"}}]}`, asEntry: true},
		// The notification path's fixtures.
		{name: "one decision", body: one},
		{name: "a Task for another patient", body: string(contentEvent(decision("collection", cr, decisionPatient("p1"), decisionPatient("p2"), task("Patient/p2"))))},
		{name: "a status parameter's Task", body: param(one, `{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/p2"}}`)},
		{name: "the member's id at another base", body: string(contentEvent(decision("collection", cr, decisionPatient("p1"), otherBasePatient)))},
		{name: "a member identifier naming its Patient", body: string(contentEvent(decision("collection", cr, decisionPatient("p1"), p2(mbr77), task("Patient/MBR-77")))),
			divergence: divOnePerson, sharedOnly: []string{"MBR-77"}},
		{name: "a member identifier no Patient holds", body: string(contentEvent(decision("collection", cr, decisionPatient("p1"), p2(""), task("Patient/MBR-77"))))},
		{name: "a folded reference", body: param(one, `{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"Reference":"Patient/p2"}}`)},
		{name: "a folded resourceType", body: param(one, strings.Replace(roePatient, `"Patient"`, `"patient"`, 1))},
		{name: "a folded resourceType, another id", body: param(one, strings.Replace(strings.Replace(roePatient, `"Patient"`, `"patient"`, 1), `"p1"`, `"p3"`, 1))},
		{name: "a huge decimal", body: string(contentEvent(decision("collection", cr, decisionPatient("p1"),
			strings.Replace(p2(""), `"birthDate"`, `"extension":[{"url":"http://example.org/x","valueDecimal":1e400}],"birthDate"`, 1), task("Patient/p2"))))},
		{name: "an id-less Patient", body: string(contentEvent(decision("collection", cr, decisionPatient("p1"),
			`{"fullUrl":"urn:uuid:a0000000-0000-4000-8000-0000000000dd","resource":{"resourceType":"Patient","birthDate":"1990-05-06","name":[{"family":"Roe"}]}}`))),
			divergence: divIDless, s3Only: []string{"urn:uuid:a0000000-0000-4000-8000-0000000000dd"}},
		{name: "an identifier-only reference", body: param(one, `{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"identifier":{"system":"`+shnsdk.MemberSystem+`","value":"p2"}}}`)},
		// z a Coverage's party: only the urn reference names it.
		{name: "a urn in its own Bundle", body: param(one, bundle(patientZ, coverageZ, taskZ))},
		{name: "a urn in an enclosing Bundle", body: param(one, bundle(patientZ, coverageZ, nested("urn:uuid:44444444-4444-4444-8444-444444444444", bundle(taskZ))))},
		{name: "a urn only a sibling Bundle carries", body: param(one, bundle(nested("urn:uuid:55555555-5555-4555-8555-555555555555", bundle(taskZ)),
			nested("urn:uuid:66666666-6666-4666-8666-666666666666", bundle(patientZ, `{"fullUrl":"urn:uuid:22222222-2222-4222-8222-222222222222","resource":{"resourceType":"Coverage","status":"active","subscriber":{"reference":"`+z+`"}}}`)))),
			divergence: divURNScope, sharedOnly: []string{"z"}},
		{name: "a payor Patient", body: string(contentEvent(decision("collection", cr, decisionPatient("p1"), strings.Replace(decisionPatient("p0"), "Doe", "Parent", 1),
			`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"},"payor":[{"reference":"Patient/p0"}]}}`))),
			divergence: divPayorParty, sharedOnly: []string{"p0"}},
		{name: "a subscriber party named by a Task", body: string(contentEvent(decision("collection", cr, decisionPatient("p1"), strings.Replace(p2(""), "Roe", "Parent", 1),
			`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"Patient/p2"}}}`, task("Patient/p2"))))},
		{name: "a contained subscriber carrying the member's number", body: string(sharedNumberDecision(`{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}]}`))},
		{name: "the stated base and a bare ClaimResponse", body: string(contentEvents(decisionEntry(decision("collection", withRequest(cr), decisionPatient("p1"))), decisionEntry(bareCR("cr2", "Patient/p1", pasProviderNPI))))},
		{name: "a urn decision", body: string(contentEvent(urnDecision(urnPatient)))},
		{name: "an exact identity over a name", body: string(contentEvent(decision("collection", decisionCR("cr1", "Patient/a", pasProviderNPI),
			`{"fullUrl":"`+decisionBase+`/Patient/a","resource":{"resourceType":"Patient","id":"a",`+strings.Replace(mbr77, "MBR-77", "MBRX", 1)+`"birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`,
			`{"fullUrl":"`+decisionBase+`/Patient/MBRX","resource":{"resourceType":"Patient","id":"MBRX","birthDate":"1960-01-01","name":[{"family":"Parent"}]}}`,
			`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/a"},"subscriber":{"reference":"Patient/MBRX"}}}`, task("Patient/MBRX"))))},
		{name: "a name two Patients share", body: string(contentEvent(decision("collection", decisionCR("cr1", "Patient/a", pasProviderNPI),
			`{"fullUrl":"`+decisionBase+`/Patient/a","resource":{"resourceType":"Patient","id":"a",`+strings.Replace(mbr77, "MBR-77", "MBRY", 1)+`"birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`,
			`{"fullUrl":"`+decisionBase+`/Patient/b","resource":{"resourceType":"Patient","id":"b",`+strings.Replace(mbr77, "MBR-77", "MBRY", 1)+`"birthDate":"1960-01-01","name":[{"family":"Parent"}]}}`,
			`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/a"},"subscriber":{"reference":"Patient/b"}}}`, task("Patient/MBRY")))),
			divergence: divPartyReading, sharedOnly: []string{"b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s3, shared := differentialMembers(t, c)
			var s3Only, sharedOnly []string
			for _, m := range s3 {
				if !slices.Contains(shared, m) {
					s3Only = append(s3Only, m)
				}
			}
			for _, m := range shared {
				if !slices.Contains(s3, m) {
					sharedOnly = append(sharedOnly, m)
				}
			}
			if !slices.Equal(s3Only, c.s3Only) || !slices.Equal(sharedOnly, c.sharedOnly) {
				t.Fatalf("the notification's reading names %v, the shared reader %v: %v only in the first, %v only in the second; want %v and %v (%s)",
					s3, shared, s3Only, sharedOnly, c.s3Only, c.sharedOnly, orNone(c.divergence))
			}
			if (c.divergence == "") != (len(c.s3Only) == 0 && len(c.sharedOnly) == 0) {
				t.Fatalf("a divergence is tagged exactly when the readings differ: %q", c.divergence)
			}
		})
	}
}

func orNone(divergence string) string {
	if divergence == "" {
		return "no divergence is enumerated for this case"
	}
	return divergence
}
