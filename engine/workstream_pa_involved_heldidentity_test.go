//go:build !shn_coreonly

package engine

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// recordsSoR is the census system of record, which also holds Patient records
// under ids of its own (records, by "Patient/<id>"), as a real system holds a
// patient under a record id that is not the member identifier.
type recordsSoR struct {
	*countingSoR
	records map[string]string
}

func (s recordsSoR) ResolveByReference(ref string) ([]byte, bool) {
	if raw, ok := s.records[ref]; ok {
		return []byte(raw), true
	}
	return s.countingSoR.ResolveByReference(ref)
}

// heldRecordsGateway is involvedGateway whose system of record also holds
// MBR-OX's Patient under the record id rec-ox.
func heldRecordsGateway(t *testing.T) (*Gateway, *[]string) {
	t.Helper()
	g, _, sor, omitted := involvedGateway(t, true)
	g.cfg.SoR = recordsSoR{countingSoR: sor, records: map[string]string{
		"Patient/rec-ox": `{"resourceType":"Patient","id":"rec-ox","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-OX"}],"birthDate":"1958-07-14","name":[{"family":"Okafor"}]}`,
	}}
	return g, omitted
}

// Each leg's request naming one other patient by ref, as that leg carries a
// reference to another patient: a second order's subject on CRD, an order
// input's subject on DTR, and an entry's subject on PAS.
func namingAnother(leg, ref string) []byte {
	switch leg {
	case "crd-order-dispatch":
		return []byte(`{"hook":"order-dispatch","context":{"patientId":"MBR-COVERED","order":"ServiceRequest/sr1"},"prefetch":{` +
			`"patient":{"resourceType":"Patient","id":"MBR-COVERED"},` +
			`"order":{"resourceType":"ServiceRequest","id":"sr2","subject":{"reference":"` + ref + `"}}}}`)
	case "dtr-questionnaire-fetch":
		return []byte(`{"resourceType":"Parameters","parameter":[` +
			`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},` +
			`{"name":"order","resource":{"resourceType":"ServiceRequest","subject":{"reference":"` + ref + `"}}}]}`)
	default:
		return []byte(`{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},` +
			`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"` + ref + `"}}}]}`)
	}
}

var involvedLegs = []string{"crd-order-dispatch", "dtr-questionnaire-fetch", "pas-claim"}

// A patient named by an id alone, which the system does not hold as a member
// and the request does not carry, is unknown on every leg the involved list
// reads, never named under an identifier derived from that id: not when the
// id names no record, an absolute reference (another server's), or a record
// the system holds under that id for a member (that record is not read: one
// identity rule binds every patient, the leg's subject binding's).
func TestRequestNamedPatients_AnIDAloneIsUnknown(t *testing.T) {
	for _, leg := range involvedLegs {
		for name, ref := range map[string]string{
			"no record":                          "Patient/rec-nowhere",
			"an absolute reference":              "https://other.example/fhir/Patient/rec-ox",
			"a record the system holds under it": "Patient/rec-ox",
		} {
			t.Run(leg+"/"+name, func(t *testing.T) {
				g, omitted := heldRecordsGateway(t)
				if got := g.requestNamedPatients(context.Background(), leg, "corr-1", namingAnother(leg, ref), heldPCI(t, "MBR-COVERED")); got != nil {
					t.Fatalf("named %v, want no one", got)
				}
				if !slices.Equal(*omitted, []string{involvedOmitUnknown}) {
					t.Fatalf("omitted %v, want one unknown", *omitted)
				}
			})
		}
	}
}

// On CRD, DTR and the inquiry, as on PAS, another patient the system of record
// does not hold is never named under an identifier derived from the request:
// a Patient the request carries for the member it names, by that id, is
// counted with demographics of its own or without, a contained one included.
func TestRequestNamedPatients_ACarriedPatientByItsIDIsCountedOffPAS(t *testing.T) {
	contained := func(leg string, demographics bool) []byte {
		p := `{"resourceType":"Patient","id":"p9"`
		if demographics {
			p += `,"birthDate":"1990-01-01","name":[{"family":"Contained"}]`
		}
		p += `}`
		switch leg {
		case "crd-order-dispatch":
			return []byte(`{"hook":"order-dispatch","context":{"patientId":"MBR-COVERED"},"prefetch":{` +
				`"patient":{"resourceType":"Patient","id":"MBR-COVERED"},` +
				`"order":{"resourceType":"ServiceRequest","id":"sr2","contained":[` + p + `],"subject":{"reference":"#p9"}}}}`)
		default:
			return []byte(`{"resourceType":"Parameters","parameter":[` +
				`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},` +
				`{"name":"order","resource":{"resourceType":"ServiceRequest","contained":[` + p + `],"subject":{"reference":"#p9"}}}]}`)
		}
	}
	for _, leg := range []string{"crd-order-dispatch", "dtr-questionnaire-fetch"} {
		for _, demographics := range []bool{true, false} {
			t.Run(leg+"/demographics "+strconv.FormatBool(demographics), func(t *testing.T) {
				g, omitted := heldRecordsGateway(t)
				if got := g.requestNamedPatients(context.Background(), leg, "corr-1", contained(leg, demographics), heldPCI(t, "MBR-COVERED")); got != nil || !slices.Equal(*omitted, []string{involvedOmitUnknown}) {
					t.Fatalf("named %v, omitted %v; want no one, one unknown", got, *omitted)
				}
			})
		}
	}
	t.Run("two carried Patients under one id that disagree", func(t *testing.T) {
		// On CRD, which names a contained Patient by its id: two under one id
		// that disagree on demographics identify no one.
		g, omitted := heldRecordsGateway(t)
		order := func(id, family string) string {
			return `{"resourceType":"ServiceRequest","id":"` + id + `","contained":[{"resourceType":"Patient","id":"p3","birthDate":"1990-01-01","name":[{"family":"` + family + `"}]}],"subject":{"reference":"#p3"}}`
		}
		payload := []byte(`{"hook":"order-dispatch","context":{"patientId":"MBR-COVERED"},"prefetch":{` +
			`"patient":{"resourceType":"Patient","id":"MBR-COVERED"},` +
			`"orders":{"resourceType":"Bundle","entry":[{"resource":` + order("sr2", "One") + `},{"resource":` + order("sr3", "Two") + `}]}}}`)
		if got := g.requestNamedPatients(context.Background(), "crd-order-dispatch", "corr-1", payload, heldPCI(t, "MBR-COVERED")); got != nil || !slices.Equal(*omitted, []string{involvedOmitUnknown}) {
			t.Fatalf("named %v, omitted %v; want no one, one unknown", got, *omitted)
		}
	})
}

// On PAS, which names a carried Patient by its entry's fullUrl, that fullUrl
// and the Patient's id are one person when the request names both: the
// subject's own entry is neither named as another patient nor counted as one
// left out, whichever of its two names the subject is bound by. A Patient
// carried only under its fullUrl (or a contained #id) is unknown.
func TestRequestNamedPatients_APASEntryUnderTwoNamesIsOnePerson(t *testing.T) {
	const demo = `,"birthDate":"1975-04-02","name":[{"family":"Johansson"}]`
	for _, tc := range []struct {
		name, leg, claimPatient, other string
	}{
		{"the subject bound by its id", "pas-claim", "Patient/MBR-COVERED", ""},
		{"the subject bound by its fullUrl, the id named too", "pas-claim", "urn:uuid:subj", `,{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}}`},
		{"on an update, the subject bound by its id", "pas-claim-update", "Patient/MBR-COVERED", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, omitted := heldRecordsGateway(t)
			payload := []byte(`{"resourceType":"Bundle","entry":[` +
				`{"fullUrl":"urn:uuid:subj","resource":{"resourceType":"Patient","id":"MBR-COVERED"` + demo + `}},` +
				`{"resource":{"resourceType":"Claim","patient":{"reference":"` + tc.claimPatient + `"}}}` + tc.other + `]}`)
			member := tc.claimPatient
			if m, ok := pasMemberFromRefOK(member); ok {
				member = m
			}
			subject, found, err := g.resolveSubjectPCI(context.Background(), member, payload)
			if err != nil || !found {
				t.Fatalf("subject: %v %v", found, err)
			}
			if got := g.requestNamedPatients(context.Background(), tc.leg, "corr-1", payload, subject); got != nil || len(*omitted) != 0 {
				t.Fatalf("named %v, omitted %v; want no one, nothing left out: one Patient, the subject", got, *omitted)
			}
		})
	}
	t.Run("a request a typed read would refuse", func(t *testing.T) {
		// A Practitioner entry's identifier written as an object: the
		// members are read from this request, so its aliases are too.
		g, omitted := heldRecordsGateway(t)
		payload := []byte(`{"resourceType":"Bundle","entry":[` +
			`{"fullUrl":"urn:uuid:subj","resource":{"resourceType":"Patient","id":"MBR-COVERED"` + demo + `}},` +
			`{"fullUrl":"urn:uuid:prac","resource":{"resourceType":"Practitioner","identifier":{"system":"x","value":"y"}}},` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}}]}`)
		if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, heldPCI(t, "MBR-COVERED")); got != nil || len(*omitted) != 0 {
			t.Fatalf("named %v, omitted %v; want no one, nothing left out", got, *omitted)
		}
	})
	t.Run("an entry named by its id and its member identifier", func(t *testing.T) {
		// The subject is bound by its urn:uuid fullUrl; the request also names
		// the same entry by its id and by its member identifier (a held
		// member): one person, the subject, under every name.
		g, omitted := heldRecordsGateway(t)
		payload := []byte(`{"resourceType":"Bundle","entry":[` +
			`{"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"p1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-OX"}]` + demo + `}},` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"urn:uuid:a"}}},` +
			`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/p1"}}},` +
			`{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/MBR-OX"}}}]}`)
		subject, found, err := g.resolveSubjectPCI(context.Background(), "urn:uuid:a", payload)
		if err != nil || !found {
			t.Fatalf("subject: %v %v", found, err)
		}
		if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, subject); got != nil || len(*omitted) != 0 {
			t.Fatalf("named %v, omitted %v; want no one, nothing left out: the subject's own entry under three names", got, *omitted)
		}
	})
	for _, tc := range []struct {
		name, claimPatient, other string
		counted                   int
	}{
		// Claim.patient resolves to the entry by its id: all its names are the
		// subject's.
		{"the subject bound by the entry's id, its member identifier named too", "Patient/p1", "Patient/MBR-OX", 0},
		// Claim.patient names the entry by its member identifier, which no
		// reference resolves to: only that name is the subject's, and the
		// entry's other names are counted unknown (a known false omission).
		{"the subject bound by the entry's member identifier, its id named too", "Patient/MBR-OX", "Patient/p1", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One entry under three names (urn:uuid:a, p1, MBR-OX, a held
			// member).
			g, omitted := heldRecordsGateway(t)
			payload := []byte(`{"resourceType":"Bundle","entry":[` +
				`{"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"p1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-OX"}]` + demo + `}},` +
				`{"resource":{"resourceType":"Claim","patient":{"reference":"` + tc.claimPatient + `"}}},` +
				`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"` + tc.other + `"}}},` +
				`{"resource":{"resourceType":"Observation","subject":{"reference":"urn:uuid:a"}}}]}`)
			member, _ := pasMemberFromRefOK(tc.claimPatient)
			subject, found, err := g.resolveSubjectPCI(context.Background(), member, payload)
			if err != nil || !found {
				t.Fatalf("subject: %v %v", found, err)
			}
			if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, subject); got != nil || len(*omitted) != tc.counted {
				t.Fatalf("named %v, omitted %v; want no one named, %d counted", got, *omitted, tc.counted)
			}
		})
	}
	t.Run("a member who is their own subscriber", func(t *testing.T) {
		// The Da Vinci IG's own Coverage shape: beneficiary and subscriber the
		// subject's entry. The subject is decided by Claim.patient's structure,
		// so the subscriber element (no member) and the entry's other names
		// name no one and count nothing.
		g, omitted := heldRecordsGateway(t)
		payload := []byte(`{"resourceType":"Bundle","entry":[` +
			`{"fullUrl":"urn:uuid:subj","resource":{"resourceType":"Patient","id":"S1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-COVERED"}]` + demo + `}},` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"urn:uuid:subj"}}},` +
			`{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"urn:uuid:subj"},"subscriber":{"reference":"urn:uuid:subj"}}},` +
			`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}},` +
			`{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/S1"}}}]}`)
		subject, found, err := g.resolveSubjectPCI(context.Background(), "urn:uuid:subj", payload)
		if err != nil || !found {
			t.Fatalf("subject: %v %v", found, err)
		}
		if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, subject); got != nil || len(*omitted) != 0 {
			t.Fatalf("named %v, omitted %v; want no one, nothing counted", got, *omitted)
		}
	})
	t.Run("a name the subject's entry shares with another Patient", func(t *testing.T) {
		// The subject's entry has id S1, and a Patient in a nested Bundle has
		// id S1 too: S1 is no name of the subject alone, so it is counted
		// unknown, never absorbed into the subject.
		g, omitted := heldRecordsGateway(t)
		payload := []byte(`{"resourceType":"Bundle","entry":[` +
			`{"fullUrl":"urn:uuid:subj","resource":{"resourceType":"Patient","id":"S1"` + demo + `}},` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"urn:uuid:subj"}}},` +
			`{"resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Patient","id":"S1","birthDate":"1980-02-02","name":[{"family":"Other"}]}}]}},` +
			`{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/S1"}}}]}`)
		subject, found, err := g.resolveSubjectPCI(context.Background(), "urn:uuid:subj", payload)
		if err != nil || !found {
			t.Fatalf("subject: %v %v", found, err)
		}
		if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, subject); got != nil || !slices.Equal(*omitted, []string{involvedOmitUnknown}) {
			t.Fatalf("named %v, omitted %v; want no one, S1 counted unknown", got, *omitted)
		}
	})
	t.Run("Claims naming different patients", func(t *testing.T) {
		// Two Claims whose patients differ decide no subject by structure:
		// the other Claim's patient, a held member, is named, never absorbed
		// into the subject.
		g, omitted := heldRecordsGateway(t)
		payload := []byte(`{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-OX"}}}]}`)
		got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, heldPCI(t, "MBR-COVERED"))
		if want := []involvedPatient{{pci: heldPCI(t, "MBR-OX"), involvement: shnsdk.InvolvementRequestNamed}}; !slices.Equal(got, want) || len(*omitted) != 0 {
			t.Fatalf("named %v, omitted %v; want MBR-OX named", got, *omitted)
		}
	})
	t.Run("an update's prior Claim decides nothing", func(t *testing.T) {
		// Only the operative (first) Claim decides the subject: a prior Claim
		// whose patient reads as the same name but resolves to another entry
		// adds none of that entry's names (M2, a held member, is named).
		g, omitted := heldRecordsGateway(t)
		entry := func(base, member string) string {
			return `{"fullUrl":"` + base + `/Patient/x","resource":{"resourceType":"Patient","id":"x","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + member + `"}]` + demo + `}}`
		}
		payload := []byte(`{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"https://b1.example/fhir/Patient/x"}}},` +
			entry("https://b1.example/fhir", "MBR-COVERED") + `,` + entry("https://b2.example/fhir", "MBR-OX") + `,` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"https://b2.example/fhir/Patient/x"}}},` +
			`{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/MBR-OX"}}}]}`)
		got := g.requestNamedPatients(context.Background(), "pas-claim-update", "corr-1", payload, heldPCI(t, "MBR-COVERED"))
		if want := []involvedPatient{{pci: heldPCI(t, "MBR-OX"), involvement: shnsdk.InvolvementRequestNamed}}; !slices.Equal(got, want) {
			t.Fatalf("named %v, omitted %v; want MBR-OX named, never absorbed into the subject", got, *omitted)
		}
	})
	t.Run("another Patient under Claim.patient's own name", func(t *testing.T) {
		// Claim.patient is Patient/x, and a Patient in a nested Bundle goes by
		// x too: x names no one alone, so it is counted, never silently taken
		// as the subject.
		g, omitted := heldRecordsGateway(t)
		payload := []byte(`{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/x"}}},` +
			`{"resource":{"resourceType":"Patient","id":"x"` + demo + `}},` +
			`{"resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Patient","id":"x","birthDate":"1980-02-02","name":[{"family":"Other"}]}}]}}]}`)
		if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, "pci:subject"); got != nil || !slices.Equal(*omitted, []string{involvedOmitUnknown}) {
			t.Fatalf("named %v, omitted %v; want no one, x counted unknown", got, *omitted)
		}
	})
	t.Run("another contained Patient under Claim.patient's own #id", func(t *testing.T) {
		// Claim.patient is a contained #p1, and another resource contains a
		// different p1 it names #p1 too: #p1 names no one alone, so it is
		// counted, never silently taken as the subject.
		g, omitted := heldRecordsGateway(t)
		payload := []byte(`{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","contained":[{"resourceType":"Patient","id":"p1"` + demo + `}],"patient":{"reference":"#p1"}}},` +
			`{"resource":{"resourceType":"ServiceRequest","contained":[{"resourceType":"Patient","id":"p1","birthDate":"1980-02-02","name":[{"family":"Other"}]}],"subject":{"reference":"#p1"}}}]}`)
		if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, "pci:subject"); got != nil || !slices.Equal(*omitted, []string{involvedOmitUnknown}) {
			t.Fatalf("named %v, omitted %v; want no one, #p1 counted unknown", got, *omitted)
		}
	})
	t.Run("held members named by reference", func(t *testing.T) {
		// Each name that is a member the system holds is named under its held
		// identity, whatever entry also carries it; the entry's fullUrl, held
		// by no one, is counted.
		g, omitted := heldRecordsGateway(t)
		payload := []byte(`{"resourceType":"Bundle","entry":[` +
			`{"fullUrl":"urn:uuid:x","resource":{"resourceType":"Patient","id":"MBR-OX","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-PAYERB"}]` + demo + `}},` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},` +
			`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-OX"}}},` +
			`{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/MBR-PAYERB"}}}]}`)
		got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, heldPCI(t, "MBR-COVERED"))
		want := []involvedPatient{
			{pci: heldPCI(t, "MBR-OX"), involvement: shnsdk.InvolvementRequestNamed},
			{pci: heldPCI(t, "MBR-PAYERB"), involvement: shnsdk.InvolvementRequestNamed},
		}
		slices.SortFunc(got, func(a, b involvedPatient) int { return strings.Compare(a.pci, b.pci) })
		slices.SortFunc(want, func(a, b involvedPatient) int { return strings.Compare(a.pci, b.pci) })
		if !slices.Equal(got, want) || !slices.Equal(*omitted, []string{involvedOmitUnknown}) {
			t.Fatalf("named %v, omitted %v; want both held members, one unknown", got, *omitted)
		}
	})
	t.Run("a fullUrl two Patient entries share", func(t *testing.T) {
		// urn:uuid:a, Claim.patient's own name, names two entries, so it
		// names no one alone: it and both entries' other names are counted
		// unknown, none absorbed into the subject.
		g, omitted := heldRecordsGateway(t)
		patient := func(id, family string) string {
			return `{"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"` + id + `","birthDate":"1990-01-01","name":[{"family":"` + family + `"}]}}`
		}
		payload := []byte(`{"resourceType":"Bundle","entry":[` + patient("p1", "One") + `,` + patient("p2", "Two") + `,` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"urn:uuid:a"}}},` +
			`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/p1"}}},` +
			`{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/p2"}}}]}`)
		subject, found, err := g.resolveSubjectPCI(context.Background(), "urn:uuid:a", payload)
		if err != nil || !found {
			t.Fatalf("subject: %v %v", found, err)
		}
		if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, subject); got != nil || !slices.Equal(*omitted, []string{involvedOmitUnknown, involvedOmitUnknown, involvedOmitUnknown}) {
			t.Fatalf("named %v, omitted %v; want no one, urn:uuid:a and both entries counted unknown", got, *omitted)
		}
	})
	t.Run("two Patient entries under one id are two people", func(t *testing.T) {
		// Neither fullUrl is an alias of p1: the one the subject is bound by
		// is the subject, and the other is counted, never absorbed into it.
		g, omitted := heldRecordsGateway(t)
		patient := func(url, family string) string {
			return `{"fullUrl":"` + url + `","resource":{"resourceType":"Patient","id":"p1","birthDate":"1990-01-01","name":[{"family":"` + family + `"}]}}`
		}
		payload := []byte(`{"resourceType":"Bundle","entry":[` + patient("urn:uuid:a", "One") + `,` + patient("urn:uuid:b", "Two") + `,` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"urn:uuid:a"}}},` +
			`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"urn:uuid:b"}}},` +
			`{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/p1"}}}]}`)
		subject, found, err := g.resolveSubjectPCI(context.Background(), "urn:uuid:a", payload)
		if err != nil || !found {
			t.Fatalf("subject: %v %v", found, err)
		}
		if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, subject); got != nil {
			t.Fatalf("named %v, want no one", got)
		}
		if !slices.Equal(*omitted, []string{involvedOmitUnknown, involvedOmitUnknown}) {
			t.Fatalf("omitted %v, want the other entry and p1 counted unknown", *omitted)
		}
	})
	for _, tc := range []struct{ name, payload string }{
		{"an entry by urn:uuid only", `{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},` +
			`{"fullUrl":"urn:uuid:p2","resource":{"resourceType":"Patient","id":"p2"` + demo + `}},` +
			`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"urn:uuid:p2"}}}]}`},
		{"a contained #id only", `{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},` +
			`{"resource":{"resourceType":"ServiceRequest","contained":[{"resourceType":"Patient","id":"p3"` + demo + `}],"subject":{"reference":"#p3"}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, omitted := heldRecordsGateway(t)
			if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", []byte(tc.payload), heldPCI(t, "MBR-COVERED")); got != nil || !slices.Equal(*omitted, []string{involvedOmitUnknown}) {
				t.Fatalf("named %v, omitted %v; want no one, one unknown", got, *omitted)
			}
		})
	}
}

// pasMemberFromRefOK is the member a PAS reference names when it is a Patient
// reference ("Patient/<id>"); ok=false for any other (a urn:uuid:).
func pasMemberFromRefOK(ref string) (string, bool) {
	if len(ref) > len("Patient/") && ref[:len("Patient/")] == "Patient/" {
		return pasMemberFromRef(ref), true
	}
	return "", false
}

// A Coverage's party, the person its subscriber or policyHolder element names,
// is no member on any leg, even named by a bare reference to a patient the
// system holds and the request does not carry (a dependent's parent, as
// origination sends it on CRD and DTR). Its control, the same reference as an
// order's subject, is named. The elements are FHIR's, by their exact
// (case-sensitive) names: an element "Subscriber" is no Coverage slot, and the
// patient it references is named as any other.
func TestCarriedMembers_ABareCoverageSlotNamesNoOne(t *testing.T) {
	coverage := func(slot string) string {
		return `{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"},"` + slot + `":{"reference":"Patient/MBR-OX"}}`
	}
	payloads := func(slot string) map[string]string {
		return map[string]string{
			"crd-order-dispatch":      `{"hook":"order-dispatch","context":{"patientId":"MBR-COVERED"},"prefetch":{"coverage":{"resourceType":"Bundle","entry":[{"resource":` + coverage(slot) + `}]}}}`,
			"dtr-questionnaire-fetch": `{"resourceType":"Parameters","parameter":[{"name":"coverage","resource":` + coverage(slot) + `}]}`,
			"pas-claim":               `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},{"resource":` + coverage(slot) + `}]}`,
		}
	}
	for _, slot := range []string{"subscriber", "policyHolder"} {
		for leg, payload := range payloads(slot) {
			t.Run(slot+"/"+leg, func(t *testing.T) {
				if got := carriedMembers([]byte(payload), leg); slices.Contains(got, "MBR-OX") {
					t.Fatalf("members %v name the Coverage's party", got)
				}
				g, omitted := heldRecordsGateway(t)
				if got := g.requestNamedPatients(context.Background(), leg, "corr-1", []byte(payload), heldPCI(t, "MBR-COVERED")); got != nil || len(*omitted) != 0 {
					t.Fatalf("named %v, omitted %v; want no one", got, *omitted)
				}
				if got := carriedMembers(namingAnother(leg, "Patient/MBR-OX"), leg); !slices.Contains(got, "MBR-OX") {
					t.Fatalf("control: an order's subject is a member: %v", got)
				}
			})
		}
	}
	for leg, payload := range payloads("Subscriber") {
		t.Run("Subscriber/"+leg, func(t *testing.T) {
			g, _ := heldRecordsGateway(t)
			got := g.requestNamedPatients(context.Background(), leg, "corr-1", []byte(payload), heldPCI(t, "MBR-COVERED"))
			if want := []involvedPatient{{pci: heldPCI(t, "MBR-OX"), involvement: shnsdk.InvolvementRequestNamed}}; !slices.Equal(got, want) {
				t.Fatalf("named %v, want MBR-OX: \"Subscriber\" is no Coverage element", got)
			}
		})
	}
}

// subjectShapes are, for CRD, DTR and the inquiry, a request whose own
// patient is subject (named by subject's own id) and that carries patient
// (a Patient resource) for it, plus other, a resource whose subject is ref.
func subjectShapes(subject, patient, ref string) map[string][]byte {
	return map[string][]byte{
		"crd-order-dispatch": []byte(`{"hook":"order-dispatch","context":{"patientId":"` + subject + `"},"prefetch":{` +
			`"patient":` + patient + `,` +
			`"order":{"resourceType":"ServiceRequest","id":"sr2","subject":{"reference":"` + ref + `"}}}}`),
		"dtr-questionnaire-fetch": []byte(`{"resourceType":"Parameters","parameter":[` +
			`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/` + subject + `"}}},` +
			`{"name":"patient","resource":` + patient + `},` +
			`{"name":"order","resource":{"resourceType":"ServiceRequest","subject":{"reference":"` + ref + `"}}}]}`),
		"pas-claim-inquire": []byte(`{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/` + subject + `"}}},` +
			`{"resource":` + patient + `},` +
			`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"` + ref + `"}}}]}`),
	}
}

// unknowns is n involvedOmitUnknown reasons.
func unknowns(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = involvedOmitUnknown
	}
	return out
}

// namedOffPAS runs requestNamedPatients on leg for payload, whose own patient
// is subject, and fails unless no one is named and omitted is want.
func namedOffPAS(t *testing.T, leg string, payload []byte, subject string, want []string) {
	t.Helper()
	g, omitted := heldRecordsGateway(t)
	pci, found, err := g.resolveSubjectPCI(context.Background(), subject, payload)
	if err != nil || !found {
		t.Fatalf("subject: %v %v", found, err)
	}
	if got := g.requestNamedPatients(context.Background(), leg, "corr-1", payload, pci); got != nil || !slices.Equal(*omitted, want) {
		t.Fatalf("named %v, omitted %v; want no one named, %v", got, *omitted, want)
	}
}

// Off PAS, every other name of a Patient the request carries under the leg's
// own patient's name (its member identifiers, or its id when the request
// names the subject by a member identifier) may be the subject's or another
// person's: it is counted, never named under an identity of its own and never
// taken as the subject's, whether it is held (as the same person or another)
// or not. Its control: a member identifier equal to the subject's own name is
// no other name.
func TestRequestNamedPatients_AnotherNameOfTheSubjectsPatientIsCounted(t *testing.T) {
	patient := func(member, birth, family string) string {
		return `{"resourceType":"Patient","id":"p1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + member + `"}],"birthDate":"` + birth + `","name":[{"family":"` + family + `"}]}`
	}
	for _, tc := range []struct {
		name, patient, ref string
		counted            int
	}{
		{"an unheld member number", patient("MBR-NEW1", "1990-01-01", "Subject"), "Patient/MBR-NEW1", 1},
		{"a member number held as the same person", patient("MBR-OX", "1958-07-14", "Okafor-Oxygen"), "Patient/MBR-OX", 1},
		{"a member number held as another person", patient("MBR-OX", "1990-01-01", "Subject"), "Patient/MBR-OX", 1},
		{"control: a member number equal to the subject's own name", patient("p1", "1990-01-01", "Subject"), "Patient/p1", 0},
	} {
		for leg, payload := range subjectShapes("p1", tc.patient, tc.ref) {
			t.Run(tc.name+"/"+leg, func(t *testing.T) {
				namedOffPAS(t, leg, payload, "p1", unknowns(tc.counted))
			})
		}
	}
}

// subjectShapesWith is subjectShapes with the extra resource in place of the
// order.
func subjectShapesWith(subject, patient, extra string) map[string][]byte {
	return map[string][]byte{
		"crd-order-dispatch": []byte(`{"hook":"order-dispatch","context":{"patientId":"` + subject + `"},"prefetch":{` +
			`"patient":` + patient + `,"order":` + extra + `}}`),
		"dtr-questionnaire-fetch": []byte(`{"resourceType":"Parameters","parameter":[` +
			`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/` + subject + `"}}},` +
			`{"name":"patient","resource":` + patient + `},{"name":"order","resource":` + extra + `}]}`),
		"pas-claim-inquire": []byte(`{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/` + subject + `"}}},` +
			`{"resource":` + patient + `},{"resource":` + extra + `}]}`),
	}
}

// Off PAS, each distinct Patient the request carries under the leg's own
// patient's name beyond the first is counted, never folded silently into the
// subject: two Patients may be two people. No demographics are compared, so
// two that agree are counted too (an accepted omission). Its control: the same
// Patient carried twice is one.
func TestRequestNamedPatients_ASecondPatientUnderTheSubjectsNameIsCounted(t *testing.T) {
	contained := func(id, fields string) string {
		return `{"resourceType":"ServiceRequest","id":"srx","contained":[{"resourceType":"Patient","id":"` + id + `"` + fields + `}],"subject":{"reference":"#` + id + `"}}`
	}
	unheld := `{"resourceType":"Patient","id":"p1","birthDate":"1990-01-01","name":[{"family":"Subject"}]}`
	for _, tc := range []struct {
		name, subject, patient, extra string
		counted                       int
	}{
		{"a held subject and another person", "MBR-COVERED", `{"resourceType":"Patient","id":"MBR-COVERED"}`, contained("MBR-COVERED", `,"birthDate":"2001-01-01","name":[{"family":"Other"}]`), 1},
		{"an unheld subject and another person", "p1", unheld, contained("p1", `,"birthDate":"2001-01-01","name":[{"family":"Other"}]`), 1},
		{"a Patient stating only another birth date", "p1", unheld, contained("p1", `,"birthDate":"2001-01-01"`), 1},
		{"a Patient that agrees with the held record", "MBR-COVERED", `{"resourceType":"Patient","id":"MBR-COVERED"}`, contained("MBR-COVERED", `,"birthDate":"1975-04-02","name":[{"family":"Johansson"}]`), 1},
		{"control: the same Patient carried twice", "p1", unheld, contained("p1", `,"birthDate":"1990-01-01","name":[{"family":"Subject"}]`), 0},
	} {
		for leg, payload := range subjectShapesWith(tc.subject, tc.patient, tc.extra) {
			t.Run(tc.name+"/"+leg, func(t *testing.T) {
				namedOffPAS(t, leg, payload, tc.subject, unknowns(tc.counted))
			})
		}
	}
	// Three Patients under p1, in the request's order on DTR and the
	// inquiry: one stating only the family name, the subject's, and one
	// stating only another birth date. Two are counted.
	z := `{"resourceType":"Patient","id":"p1","name":[{"family":"Subject"}]}`
	y := contained("p1", `,"birthDate":"2001-01-01"`)
	for leg, payload := range map[string][]byte{
		"dtr-questionnaire-fetch": []byte(`{"resourceType":"Parameters","parameter":[` +
			`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/p1"}}},` +
			`{"name":"z","resource":` + z + `},{"name":"patient","resource":` + unheld + `},{"name":"order","resource":` + y + `}]}`),
		"pas-claim-inquire": []byte(`{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/p1"}}},` +
			`{"resource":` + z + `},{"resource":` + unheld + `},{"resource":` + y + `}]}`),
	} {
		t.Run("three Patients/"+leg, func(t *testing.T) {
			g, omitted := heldRecordsGateway(t)
			// The subject as the leg binds it, whichever of the three that is.
			pci, found, err := g.resolveSubjectPCI(context.Background(), "p1", payload)
			if err != nil || !found {
				t.Fatalf("subject: %v %v", found, err)
			}
			if got := g.requestNamedPatients(context.Background(), leg, "corr-1", payload, pci); got != nil || !slices.Equal(*omitted, unknowns(2)) {
				t.Fatalf("named %v, omitted %v; want no one named, two unknown", got, *omitted)
			}
		})
	}
}

// Off PAS, a name the leg's own patient's carried Patient goes by is counted,
// never named, even when the person another Patient carried under the
// subject's name is held under it, or when a Coverage's party goes by it too:
//   - a held member identifier on a different person under the subject's id;
//   - the subject's own agreeing Patient's member identifier, beside a
//     different person under the subject's id (both counted);
//   - a member number the subject's Patient and a Coverage's contained
//     subscriber both go by.
func TestRequestNamedPatients_TheSubjectsPatientsOtherNamesAreNeverNamed(t *testing.T) {
	mid := func(v string) string {
		return `"identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + v + `"}]`
	}
	other := `{"resourceType":"ServiceRequest","id":"srx","contained":[{"resourceType":"Patient","id":"MBR-COVERED",` + mid("MBR-OX") + `,"birthDate":"2001-01-01","name":[{"family":"Other"}]}],"subject":{"reference":"#MBR-COVERED"}}`
	stranger := `{"resourceType":"ServiceRequest","id":"srx","contained":[{"resourceType":"Patient","id":"MBR-COVERED","birthDate":"2001-01-01","name":[{"family":"Other"}]}],"subject":{"reference":"#MBR-COVERED"}}`
	own := `{"resourceType":"Patient","id":"MBR-COVERED",` + mid("MBR-ALIAS") + `,"birthDate":"1975-04-02","name":[{"family":"Johansson"}]}`
	sub := `{"resourceType":"Coverage","contained":[{"resourceType":"Patient","id":"sub",` + mid("MBR-FAM") + `,"birthDate":"1960-01-01","name":[{"family":"Parent"}]}],"beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"#sub"}}`
	fam := `{"resourceType":"Patient","id":"p1",` + mid("MBR-FAM") + `,"birthDate":"1990-01-01","name":[{"family":"Subject"}]}`
	obs := func(ref string) string {
		return `{"resourceType":"Observation","subject":{"reference":"` + ref + `"}}`
	}
	for _, tc := range []struct {
		name, subject string
		resources     []string
		counted       int
	}{
		{"a held member identifier on another person", "MBR-COVERED", []string{other, obs("Patient/MBR-OX")}, 1},
		{"the subject's own member identifier beside another person", "MBR-COVERED", []string{own, stranger, obs("Patient/MBR-ALIAS")}, 2},
		// MBR-FAM, and the subscriber's Patient as a second one under it.
		{"a member number a Coverage's subscriber goes by too", "p1", []string{fam, sub, obs("Patient/MBR-FAM")}, 2},
	} {
		var params, entries, prefetch []string
		for i, r := range tc.resources {
			params = append(params, `{"name":"r`+string(rune('a'+i))+`","resource":`+r+`}`)
			entries = append(entries, `{"resource":`+r+`}`)
			prefetch = append(prefetch, `"r`+string(rune('a'+i))+`":`+r)
		}
		for leg, payload := range map[string][]byte{
			"crd-order-dispatch": []byte(`{"hook":"order-dispatch","context":{"patientId":"` + tc.subject + `"},"prefetch":{` + strings.Join(prefetch, ",") + `}}`),
			"dtr-questionnaire-fetch": []byte(`{"resourceType":"Parameters","parameter":[` +
				`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/` + tc.subject + `"}}},` + strings.Join(params, ",") + `]}`),
			"pas-claim-inquire": []byte(`{"resourceType":"Bundle","entry":[` +
				`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/` + tc.subject + `"}}},` + strings.Join(entries, ",") + `]}`),
		} {
			t.Run(tc.name+"/"+leg, func(t *testing.T) {
				namedOffPAS(t, leg, payload, tc.subject, unknowns(tc.counted))
			})
		}
	}
}

// On CRD the leg's own patient is context.patientId by the request's
// structure: a Patient the request carries under it (by member identifier)
// has its other names counted, never named, though no resource references
// context.patientId itself, even its held id. Its control: under a
// context.patientId the Patient does not go by, the held member is named under
// its held identity.
func TestRequestNamedPatients_CRDsContextPatientIDIsTheSubjectsName(t *testing.T) {
	mid := func(v string) string { return `{"system":"` + shnsdk.MemberSystem + `","value":"` + v + `"}` }
	pt := `{"resourceType":"Patient","id":"MBR-OX","identifier":[` + mid("MBR-NEW9") + `],"birthDate":"1990-01-01","name":[{"family":"Subject"}]}`
	for _, leg := range []string{"crd-order-dispatch", "crd-order-select"} {
		payload := func(patientID string) []byte {
			return []byte(`{"hook":"` + strings.TrimPrefix(leg, "crd-") + `","context":{"patientId":"` + patientID + `"},"prefetch":{"patient":` + pt + `,` +
				`"order":{"resourceType":"ServiceRequest","id":"sr2","subject":{"reference":"Patient/MBR-OX"}}}}`)
		}
		t.Run(leg, func(t *testing.T) {
			namedOffPAS(t, leg, payload("MBR-NEW9"), "MBR-NEW9", unknowns(1))
		})
		t.Run("control/"+leg, func(t *testing.T) {
			g, omitted := heldRecordsGateway(t)
			subject, found, err := g.resolveSubjectPCI(context.Background(), "MBR-NEW8", payload("MBR-NEW8"))
			if err != nil || !found {
				t.Fatalf("subject: %v %v", found, err)
			}
			got := g.requestNamedPatients(context.Background(), leg, "corr-1", payload("MBR-NEW8"), subject)
			if want := []involvedPatient{{pci: heldPCI(t, "MBR-OX"), involvement: shnsdk.InvolvementRequestNamed}}; !slices.Equal(got, want) || len(*omitted) != 0 {
				t.Fatalf("named %v, omitted %v; want MBR-OX named under its held identity", got, *omitted)
			}
		})
	}
}

// failingReadsSoR is heldRecordsGateway's system of record whose member reads
// of fail fail; cancel, when set, is called at that read instead, the
// request's budget running out.
type failingReadsSoR struct {
	recordsSoR
	fail   string
	cancel context.CancelFunc

	mu    sync.Mutex
	reads int
}

func (s *failingReadsSoR) ResolvePatientContext(ctx context.Context, member string) (string, Demo, bool, error) {
	if member == s.fail {
		s.mu.Lock()
		s.reads++
		s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
			return "", Demo{}, false, ctx.Err()
		}
		return "", Demo{}, false, &SoRReadError{Kind: SoRUnavailable}
	}
	return legacySoRReader{sor: s.recordsSoR}.ResolvePatientContext(ctx, member)
}

func (s *failingReadsSoR) PatientFHIRRefContext(ctx context.Context, m string) (string, bool, error) {
	return legacySoRReader{sor: s.recordsSoR}.PatientFHIRRefContext(ctx, m)
}

func (s *failingReadsSoR) CoverageInforceContext(ctx context.Context, m string) (bool, string, error) {
	return legacySoRReader{sor: s.recordsSoR}.CoverageInforceContext(ctx, m)
}

func (s *failingReadsSoR) ClinicalContextContext(ctx context.Context, m string) (shnsdk.ClinicalContext, bool, error) {
	return legacySoRReader{sor: s.recordsSoR}.ClinicalContextContext(ctx, m)
}

func (s *failingReadsSoR) SupplementalReportContext(ctx context.Context, m string) ([]byte, bool, error) {
	return legacySoRReader{sor: s.recordsSoR}.SupplementalReportContext(ctx, m)
}

func (s *failingReadsSoR) FacilityRecordsContext(ctx context.Context, m string) (map[string][]byte, bool, error) {
	return legacySoRReader{sor: s.recordsSoR}.FacilityRecordsContext(ctx, m)
}

func (s *failingReadsSoR) OpenOrderContext(ctx context.Context, m string) ([]byte, bool, error) {
	return legacySoRReader{sor: s.recordsSoR}.OpenOrderContext(ctx, m)
}

func (s *failingReadsSoR) OpenCoverageContext(ctx context.Context, m string) ([][]byte, error) {
	return legacySoRReader{sor: s.recordsSoR}.OpenCoverageContext(ctx, m)
}

func (s *failingReadsSoR) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	return legacySoRReader{sor: s.recordsSoR}.ResolveByReferenceContext(ctx, ref)
}

var _ ContextSystemOfRecord = (*failingReadsSoR)(nil)

// Off PAS, when the read of a member that may be the leg's own patient fails,
// or the budget runs out at it, every other name of a Patient carried under
// it is left out for that reason, never named under an identity of its own:
// on DTR and the inquiry, the subject's own name, read as any member there.
// (On CRD the subject's context.patientId is the subject by the request's
// structure, with no read.)
func TestRequestNamedPatients_ANameOfAnUnreadMemberIsLeftOut(t *testing.T) {
	patient := `{"resourceType":"Patient","id":"p1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-NEW1"}],"birthDate":"1990-01-01","name":[{"family":"Subject"}]}`
	for _, tc := range []struct {
		name   string
		budget bool
		want   []string
	}{
		{"the read fails", false, []string{involvedOmitUnreadable, involvedOmitUnreadable}},
		// MBR-NEW1, not held, is unknown in its own right.
		{"the budget runs out", true, []string{involvedOmitUnknown, involvedOmitBudget}},
	} {
		for leg, payload := range subjectShapes("p1", patient, "Patient/MBR-NEW1") {
			if leg == "crd-order-dispatch" {
				continue
			}
			t.Run(tc.name+"/"+leg, func(t *testing.T) {
				g, omitted := heldRecordsGateway(t)
				subject, found, err := g.resolveSubjectPCI(context.Background(), "p1", payload)
				if err != nil || !found {
					t.Fatalf("subject: %v %v", found, err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				sor := &failingReadsSoR{recordsSoR: g.cfg.SoR.(recordsSoR), fail: "p1"}
				if tc.budget {
					sor.cancel = cancel
				}
				g.cfg.SoR = sor
				got := g.requestNamedPatients(ctx, leg, "corr-1", payload, subject)
				if got != nil || !slices.Equal(*omitted, tc.want) {
					t.Fatalf("named %v, omitted %v; want no one named, %v", got, *omitted, tc.want)
				}
				if sor.reads == 0 {
					t.Fatal("p1 was never read: the row proves nothing")
				}
			})
		}
	}
}

// Off PAS, the names counted under the leg's own patient's name close over
// the Patients the request carries under them: a second Patient that goes by
// the subject's Patient's member number under an id of its own is counted
// (and its id with it), never named under that id, held subject or not; one
// with no id at all is counted too.
func TestRequestNamedPatients_APatientUnderAnotherNameOfTheSubjectIsCounted(t *testing.T) {
	mid := func(v string) string {
		return `"identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + v + `"}]`
	}
	contained := func(id, fields string) string {
		return `{"resourceType":"ServiceRequest","id":"srx","contained":[{"resourceType":"Patient","id":"` + id + `",` + fields + `}],"subject":{"reference":"#` + id + `"}}`
	}
	unheld := `{"resourceType":"Patient","id":"p1",` + mid("MBR-NEW1") + `,"birthDate":"1990-01-01","name":[{"family":"Subject"}]}`
	for _, tc := range []struct {
		name, subject, patient, extra string
		counted                       int
	}{
		// The second Patient's id, and the second Patient itself.
		{"an unheld subject", "p1", unheld,
			contained("p2", mid("MBR-NEW1")+`,"birthDate":"1990-01-01","name":[{"family":"Subject"}]`), 2},
		{"a held subject", "MBR-COVERED",
			`{"resourceType":"Patient","id":"MBR-COVERED",` + mid("MBR-ALIAS") + `,"birthDate":"1975-04-02","name":[{"family":"Johansson"}]}`,
			contained("pt-9", mid("MBR-ALIAS")+`,"birthDate":"1975-04-02","name":[{"family":"Johansson"}]`), 2},
		// The second Patient, which has no id to count.
		{"a second Patient with no id", "p1", unheld,
			`{"resourceType":"Patient",` + mid("MBR-NEW1") + `,"birthDate":"1990-01-01","name":[{"family":"Subject"}]}`, 1},
	} {
		for leg, payload := range subjectShapesWith(tc.subject, tc.patient, tc.extra) {
			t.Run(tc.name+"/"+leg, func(t *testing.T) {
				namedOffPAS(t, leg, payload, tc.subject, unknowns(tc.counted))
			})
		}
	}
}

// On DTR and the inquiry, whose own patient's name is decided by its read, a
// member past maxInvolvedMembers may be the leg's own patient, never read:
// every other name of a Patient carried under it is counted overflow, never
// named. (On CRD context.patientId is the subject first, by structure.)
func TestRequestNamedPatients_ANameOfACutMemberIsLeftOut(t *testing.T) {
	var refs []string
	for i := range maxInvolvedMembers - 1 {
		refs = append(refs, `{"resourceType":"Observation","subject":{"reference":"Patient/a`+string(rune('a'+i/26))+string(rune('a'+i%26))+`"}}`)
	}
	refs = append(refs, `{"resourceType":"Observation","subject":{"reference":"Patient/MBR-A"}}`)
	patient := `{"resourceType":"Patient","id":"zz-subject","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-A"}],"birthDate":"1990-01-01","name":[{"family":"Subject"}]}`
	var params, entries []string
	for i, r := range refs {
		params = append(params, `{"name":"o`+string(rune('a'+i/26))+string(rune('a'+i%26))+`","resource":`+r+`}`)
		entries = append(entries, `{"resource":`+r+`}`)
	}
	for leg, payload := range map[string][]byte{
		"dtr-questionnaire-fetch": []byte(`{"resourceType":"Parameters","parameter":[` +
			`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/zz-subject"}}},` +
			`{"name":"patient","resource":` + patient + `},` + strings.Join(params, ",") + `]}`),
		"pas-claim-inquire": []byte(`{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/zz-subject"}}},` +
			`{"resource":` + patient + `},` + strings.Join(entries, ",") + `]}`),
	} {
		t.Run(leg, func(t *testing.T) {
			if got := namedMembers(payload, leg); len(got) != maxInvolvedMembers+1 || got[len(got)-1].member != "zz-subject" {
				t.Fatalf("members %v: the row needs the subject cut last", got)
			}
			g, omitted := heldRecordsGateway(t)
			pci, found, err := g.resolveSubjectPCI(context.Background(), "zz-subject", payload)
			if err != nil || !found {
				t.Fatalf("subject: %v %v", found, err)
			}
			got := g.requestNamedPatients(context.Background(), leg, "corr-1", payload, pci)
			overflow := 0
			for _, o := range *omitted {
				if o == involvedOmitOverflow {
					overflow++
				}
			}
			// zz-subject cut, and MBR-A with it; the 31 others are ids alone.
			if got != nil || overflow != 2 || len(*omitted) != maxInvolvedMembers+1 {
				t.Fatalf("named %v, omitted %v; want no one named, the subject and MBR-A overflow", got, *omitted)
			}
		})
	}
}

// On CRD, context.patientId names the leg's own patient as the leg's subject
// binding reads it, a "Patient/" prefix taken off: a Patient the request
// carries under it by member identifier has its other names counted, never
// named, its held id included, held subject or not.
func TestRequestNamedPatients_CRDsPrefixedContextPatientIDIsTheSubjectsName(t *testing.T) {
	for _, tc := range []struct{ name, patientID, member, birth, family string }{
		{"held", "Patient/MBR-COVERED", "MBR-COVERED", "1975-04-02", "Johansson"},
		{"unheld", "Patient/MBR-NEW9", "MBR-NEW9", "1990-01-01", "Subject"},
		{"unheld, no prefix", "MBR-NEW9", "MBR-NEW9", "1990-01-01", "Subject"},
	} {
		// A Patient whose own id, MBR-OX, is held.
		pt := `{"resourceType":"Patient","id":"MBR-OX","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + tc.member + `"}],"birthDate":"` + tc.birth + `","name":[{"family":"` + tc.family + `"}]}`
		for _, leg := range []string{"crd-order-dispatch", "crd-order-select"} {
			payload := []byte(`{"hook":"` + strings.TrimPrefix(leg, "crd-") + `","context":{"patientId":"` + tc.patientID + `"},"prefetch":{"patient":` + pt + `}}`)
			t.Run(tc.name+"/"+leg, func(t *testing.T) {
				namedOffPAS(t, leg, payload, tc.member, unknowns(1))
			})
		}
	}
}

// Off PAS, the leg's own patient's name may be read by its member read as no
// one's where the leg's binding reads it as the subject's: a Patient under the
// subject's id that a Coverage names as its party is the person looked up to
// that read (identifyNamed), and another person to the binding. Every other
// name of a Patient carried under it is still counted, never named: on CRD as
// context.patientId's, and on DTR and the inquiry as a member read as no one's,
// the party's own member number among them, though the system holds it:
//   - on CRD, MBR-NEW1 itself, p1, MBR-OX, and the party's Patient as a
//     second Patient under the subject's name;
//   - on DTR and the inquiry, MBR-NEW1 itself, p1 and MBR-OX.
func TestRequestNamedPatients_TheSubjectsNameReadAsNoOnesIsStillCounted(t *testing.T) {
	own := `{"resourceType":"Patient","id":"p1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-NEW1"}],"birthDate":"1990-01-01","name":[{"family":"Subject"}]}`
	parent := `{"resourceType":"Patient","id":"MBR-NEW1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-OX"}],"birthDate":"1960-01-01","name":[{"family":"Parent"}]}`
	held := `{"resourceType":"Observation","subject":{"reference":"Patient/MBR-OX"}}`
	cov := `{"resourceType":"Coverage","id":"cov2","beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"Patient/MBR-NEW1"}}`
	order := `{"resourceType":"ServiceRequest","id":"sr2","subject":{"reference":"Patient/p1"}}`
	for leg, payload := range map[string][]byte{
		"crd-order-dispatch": []byte(`{"hook":"order-dispatch","context":{"patientId":"MBR-NEW1"},"prefetch":{"patient":` + own + `,"parent":` + parent + `,"coverage":` + cov + `,"order":` + order +
			`,"obs":{"resourceType":"Observation","subject":{"reference":"Patient/MBR-NEW1"}},"held":` + held + `}}`),
		"dtr-questionnaire-fetch": []byte(`{"resourceType":"Parameters","parameter":[` +
			`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-NEW1"}}},` +
			`{"name":"patient","resource":` + own + `},{"name":"parent","resource":` + parent + `},{"name":"cov2","resource":` + cov + `},{"name":"order","resource":` + order + `},{"name":"held","resource":` + held + `}]}`),
		"pas-claim-inquire": []byte(`{"resourceType":"Bundle","entry":[` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-NEW1"}}},` +
			`{"resource":` + own + `},{"resource":` + parent + `},{"resource":` + cov + `},{"resource":` + order + `},{"resource":` + held + `}]}`),
	} {
		t.Run(leg, func(t *testing.T) {
			counted := 3
			if leg == "crd-order-dispatch" {
				counted = 4
			}
			g, omitted := heldRecordsGateway(t)
			pci, found, err := g.resolveSubjectPCI(context.Background(), "MBR-NEW1", payload)
			if err != nil || !found || pci != derivedPCI("MBR-NEW1", "1990-01-01", "Subject") {
				t.Fatalf("subject %q: %v %v; the row needs the binding to read p1 as the subject's", pci, found, err)
			}
			if _, outcome, err := g.identifyNamed(context.Background(), namedMember{member: "MBR-NEW1", scope: payload}, pci); outcome != namedUnknown || err != nil {
				t.Fatalf("MBR-NEW1 read as %d, %v; the row needs its member read to identify no one", outcome, err)
			}
			got := g.requestNamedPatients(context.Background(), leg, "corr-1", payload, pci)
			if got != nil || !slices.Equal(*omitted, unknowns(counted)) {
				t.Fatalf("named %v, omitted %v; want no one named, %d counted unknown", got, *omitted, counted)
			}
		})
	}
}

// Off PAS, a member whose own read identifies no one may be the leg's own
// patient read another way, so a name a Patient carried under it goes by is
// counted, never named, even one the system holds (an accepted omission). Its
// control: the held name on no such Patient is named.
func TestRequestNamedPatients_ANameUnderAnUnknownMemberIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name, patient string
		named         bool
	}{
		{"a held name on an unknown member's Patient", `{"resourceType":"Patient","id":"x9","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-OX"}]}`, false},
		{"control: the held name on no such Patient", `{"resourceType":"Patient","id":"x9"}`, true},
	} {
		for leg, payload := range subjectShapesWith("MBR-COVERED", `{"resourceType":"Patient","id":"MBR-COVERED"}`,
			`{"resourceType":"List","entry":[{"item":{"reference":"Patient/x9"}},{"item":{"reference":"Patient/MBR-OX"}}],"contained":[`+tc.patient+`]}`) {
			t.Run(tc.name+"/"+leg, func(t *testing.T) {
				g, omitted := heldRecordsGateway(t)
				got := g.requestNamedPatients(context.Background(), leg, "corr-1", payload, heldPCI(t, "MBR-COVERED"))
				var want []involvedPatient
				omits := unknowns(2)
				if tc.named {
					want, omits = []involvedPatient{{pci: heldPCI(t, "MBR-OX"), involvement: shnsdk.InvolvementRequestNamed}}, unknowns(1)
				}
				if !slices.Equal(got, want) || !slices.Equal(*omitted, omits) {
					t.Fatalf("named %v, omitted %v; want %v named, %v", got, *omitted, want, omits)
				}
			})
		}
	}
}

// stepsCtx is a context whose Err calls are counted: context.Canceled from the
// limit-th on (never, for limit 0).
type stepsCtx struct {
	context.Context
	calls, limit int
}

func (c *stepsCtx) Err() error {
	c.calls++
	if c.limit > 0 && c.calls >= c.limit {
		return context.Canceled
	}
	return nil
}

// subjectCarriedNames walks a request in time linear in it: each name it
// reaches, and each Patient carried under one, is walked at most once for each
// decision, however many members reach it (a step each, as its budget checks
// count them, and one more at its end):
//   - n Patients chained by name (pk going by p(k+1)) under the subject's
//     name, every name a member cut past maxInvolvedMembers: every name
//     counted, each Patient past the first an extra, in two walks (the
//     subject's, and the cut members' as one);
//   - one Patient going by k names under the subject's name: one walk.
func TestSubjectCarriedNames_TheWalkIsLinearInTheRequest(t *testing.T) {
	mid := func(v string) string { return `{"system":"` + shnsdk.MemberSystem + `","value":"` + v + `"}` }
	const n = 400
	var chain []string
	var cut []namedMember
	for k := range n {
		chain = append(chain, `{"name":"p","resource":{"resourceType":"Patient","id":"p`+strconv.Itoa(k)+`","identifier":[`+mid("p"+strconv.Itoa(k+1))+`]}}`)
		cut = append(cut, namedMember{member: "p" + strconv.Itoa(k+1)})
	}
	var wide []string
	for k := 1; k <= n; k++ {
		wide = append(wide, mid("p"+strconv.Itoa(k)))
	}
	for _, tc := range []struct {
		name          string
		patients      []string
		cut           []namedMember
		extras, steps int
		exact         bool
	}{
		{"a chain", chain, cut, n - 1, 2*(n+1+n) + 1, false},
		// Every name, the one Patient once, and the end.
		{"one Patient", []string{`{"name":"p","resource":{"resourceType":"Patient","id":"p0","identifier":[` + strings.Join(wide, ",") + `]}}`}, nil, 0, n + 1 + 1 + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"resourceType":"Parameters","parameter":[` + strings.Join(tc.patients, ",") + `]}`)
			for i := range tc.cut {
				tc.cut[i].scope = payload
			}
			ctx := &stepsCtx{Context: context.Background()}
			decided, extras, complete := subjectCarriedNames(ctx, payload, []namedMember{{member: "p0", scope: payload}}, []namedResult{{outcome: namedSubject}}, tc.cut)
			if !complete || len(decided) != n || extras != tc.extras {
				t.Fatalf("complete %v, %d decided, %d extras; want complete, %d decided, %d extras", complete, len(decided), extras, n, tc.extras)
			}
			for k := 1; k <= n; k++ {
				if d := decided["p"+strconv.Itoa(k)]; d != decidedUnknown {
					t.Fatalf("p%d decided %d; want unknown", k, d)
				}
			}
			if ctx.calls > tc.steps || tc.exact && ctx.calls != tc.steps {
				t.Fatalf("%d steps; want %d (exact %v)", ctx.calls, tc.steps, tc.exact)
			}
		})
	}
}

// When the budget runs out before subjectCarriedNames has walked every name it
// would decide, a member that is not decided may still be another name of the
// leg's own patient: requestNamedPatients names no one, counting each member it
// would have named for the budget. Its control: with the budget left, the held
// member is named.
func TestRequestNamedPatients_NoOneIsNamedWhenTheWalkRunsOutOfBudget(t *testing.T) {
	payload := subjectShapes("p1", `{"resourceType":"Patient","id":"p1","birthDate":"1990-01-01","name":[{"family":"Subject"}]}`, "Patient/MBR-OX")["dtr-questionnaire-fetch"]
	g, omitted := heldRecordsGateway(t)
	subject, found, err := g.resolveSubjectPCI(context.Background(), "p1", payload)
	if err != nil || !found {
		t.Fatalf("subject: %v %v", found, err)
	}
	members := namedMembers(payload, "dtr-questionnaire-fetch")
	results := []namedResult{}
	for _, m := range members {
		pci, outcome, err := g.identifyNamed(context.Background(), m, subject)
		results = append(results, namedResult{pci: pci, outcome: outcome, err: err})
	}
	if _, _, complete := subjectCarriedNames(&stepsCtx{Context: context.Background(), limit: 1}, payload, members, results, nil); complete {
		t.Fatal("the walk completed with no budget left")
	}
	if got := g.requestNamedPatients(context.Background(), "dtr-questionnaire-fetch", "corr-1", payload, subject); len(got) != 1 || len(*omitted) != 0 {
		t.Fatalf("control: named %v, omitted %v; want MBR-OX named", got, *omitted)
	}
	// The same request, its steps counted: the walk's one step (p1, under which
	// only p1's own Patient is carried) is the last, so a budget ending there
	// leaves the first pass whole and the walk short.
	probe := &stepsCtx{Context: context.Background()}
	if got := g.requestNamedPatients(probe, "dtr-questionnaire-fetch", "corr-1", payload, subject); len(got) != 1 {
		t.Fatalf("probe: named %v; want MBR-OX named", got)
	}
	*omitted = nil
	ctx := &stepsCtx{Context: context.Background(), limit: probe.calls}
	if got := g.requestNamedPatients(ctx, "dtr-questionnaire-fetch", "corr-1", payload, subject); got != nil || !slices.Equal(*omitted, []string{involvedOmitBudget}) {
		t.Fatalf("named %v, omitted %v; want no one named, MBR-OX counted for the budget", got, *omitted)
	}
}

// On CRD, DTR and the inquiry, a Patient the request carries under the leg's
// own patient's name has its other names counted however it is carried: where
// a Coverage may name it as its party (a dependent's Coverage, naming a parent
// the request does not carry, or one spelling its party slot in another
// case), as the resource of an entry with no id of its own but a RESTful
// fullUrl naming the subject's id, or by a name in another case or another
// identifier system. Its control: the held member, on no such Patient, is
// named under its held identity.
func TestRequestNamedPatients_AnyPatientUnderTheSubjectsNameIsCounted(t *testing.T) {
	mid := func(v string) string { return `{"system":"` + shnsdk.MemberSystem + `","value":"` + v + `"}` }
	alias := `{"resourceType":"Patient","id":"pt-1","identifier":[` + mid("MBR-COVERED") + `,` + mid("MBR-OX") + `],"birthDate":"1975-04-02","name":[{"family":"Johansson"}]}`
	dependent := `{"resourceType":"Coverage","id":"cov","beneficiary":{"reference":"Patient/MBR-COVERED"},"subscriber":{"reference":"Patient/parent"}}`
	unclear := `{"resourceType":"Coverage","id":"cov","contained":[{"resourceType":"Patient","id":"b","identifier":[` + mid("MBR-COVERED") + `,` + mid("MBR-OX") + `]}],"beneficiary":{"reference":"#b"},"Subscriber":{"reference":"#b"}}`
	own := `{"resourceType":"Patient","id":"MBR-COVERED"}`
	held := `{"resourceType":"Observation","subject":{"reference":"Patient/MBR-OX"}}`
	for _, tc := range []struct {
		name      string
		resources []string
		named     bool
	}{
		{"a dependent's Coverage beside the subject's Patient's other name", []string{own, alias, dependent, held}, false},
		{"a Coverage spelling its party slot in another case", []string{own, unclear, held}, false},
		{"another case", []string{`{"resourceType":"Patient","id":"MBR-COVERED","Identifier":[` + mid("MBR-OX") + `]}`, held}, false},
		{"another identifier system", []string{`{"resourceType":"Patient","id":"MBR-COVERED","identifier":[{"system":"urn:example:mrn","value":"MBR-OX"}]}`, held}, false},
		{"a resourceType in another case", []string{own, `{"resourceType":"patient","id":"pt-1","identifier":[` + mid("MBR-COVERED") + `,` + mid("MBR-OX") + `]}`, held}, false},
		{"control: on no such Patient", []string{own, dependent, held}, true},
	} {
		var params, entries, prefetch []string
		for i, r := range tc.resources {
			params = append(params, `{"name":"r`+strconv.Itoa(i)+`","resource":`+r+`}`)
			entries = append(entries, `{"resource":`+r+`}`)
			prefetch = append(prefetch, `"r`+strconv.Itoa(i)+`":`+r)
		}
		for leg, payload := range map[string][]byte{
			"crd-order-dispatch": []byte(`{"hook":"order-dispatch","context":{"patientId":"MBR-COVERED"},"prefetch":{` + strings.Join(prefetch, ",") + `}}`),
			"dtr-questionnaire-fetch": []byte(`{"resourceType":"Parameters","parameter":[` +
				`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},` + strings.Join(params, ",") + `]}`),
			"pas-claim-inquire": []byte(`{"resourceType":"Bundle","entry":[` +
				`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},` + strings.Join(entries, ",") + `]}`),
		} {
			t.Run(tc.name+"/"+leg, func(t *testing.T) {
				g, omitted := heldRecordsGateway(t)
				got := g.requestNamedPatients(context.Background(), leg, "corr-1", payload, heldPCI(t, "MBR-COVERED"))
				named := slices.Contains(got, involvedPatient{pci: heldPCI(t, "MBR-OX"), involvement: shnsdk.InvolvementRequestNamed})
				if named != tc.named || len(got) > 1 {
					t.Fatalf("named %v, omitted %v; want MBR-OX named %v, no one else", got, *omitted, tc.named)
				}
			})
		}
	}
	t.Run("an entry with no id under a fullUrl naming the subject/pas-claim-inquire", func(t *testing.T) {
		for _, tc := range []struct {
			name, patient string
		}{
			{"no id", `{"fullUrl":"http://ehr.example/fhir/Patient/MBR-COVERED","resource":{"resourceType":"Patient","identifier":[` + mid("MBR-OX") + `],"birthDate":"1975-04-02","name":[{"family":"Johansson"}]}}`},
			{"its id", `{"fullUrl":"http://ehr.example/fhir/Patient/MBR-COVERED","resource":{"resourceType":"Patient","id":"MBR-COVERED","identifier":[` + mid("MBR-OX") + `],"birthDate":"1975-04-02","name":[{"family":"Johansson"}]}}`},
		} {
			payload := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},` + tc.patient + `,{"resource":` + held + `}]}`)
			g, omitted := heldRecordsGateway(t)
			if got := g.requestNamedPatients(context.Background(), "pas-claim-inquire", "corr-1", payload, heldPCI(t, "MBR-COVERED")); got != nil || !slices.Equal(*omitted, unknowns(1)) {
				t.Fatalf("%s: named %v, omitted %v; want no one named, MBR-OX counted", tc.name, got, *omitted)
			}
		}
	})
}

// On CRD, context.patientId is never named as another patient, even when its
// own read binds it to another identity than the leg's subject (the request
// the leg bound may differ from the one the involved list reads).
func TestRequestNamedPatients_CRDsContextPatientIDIsNeverNamed(t *testing.T) {
	for _, leg := range []string{"crd-order-dispatch", "crd-order-select"} {
		payload := []byte(`{"hook":"` + strings.TrimPrefix(leg, "crd-") + `","context":{"patientId":"MBR-OX"},"prefetch":{` +
			`"order":{"resourceType":"ServiceRequest","id":"sr2","subject":{"reference":"Patient/MBR-OX"}}}}`)
		t.Run(leg, func(t *testing.T) {
			g, omitted := heldRecordsGateway(t)
			if got := g.requestNamedPatients(context.Background(), leg, "corr-1", payload, "pci:bound-from-another-request"); got != nil || !slices.Equal(*omitted, unknowns(1)) {
				t.Fatalf("named %v, omitted %v; want no one named, MBR-OX counted", got, *omitted)
			}
		})
	}
}

// A request that repeats a member name, which readers may read two ways, names
// no member off PAS (its members are read as the strict reader reads it), so no
// one is named. Its control: the same request without the repeat names the
// held member.
func TestRequestNamedPatients_ARequestThatRepeatsANameNamesNoOne(t *testing.T) {
	for _, tc := range []struct {
		name, obs string
		named     bool
	}{
		{"a repeat", `{"resourceType":"Observation","status":"final","status":"final","subject":{"reference":"Patient/MBR-OX"}}`, false},
		{"control", `{"resourceType":"Observation","status":"final","subject":{"reference":"Patient/MBR-OX"}}`, true},
	} {
		payload := []byte(`{"resourceType":"Parameters","parameter":[` +
			`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},{"name":"obs","resource":` + tc.obs + `}]}`)
		t.Run(tc.name, func(t *testing.T) {
			g, omitted := heldRecordsGateway(t)
			got := g.requestNamedPatients(context.Background(), "dtr-questionnaire-fetch", "corr-1", payload, heldPCI(t, "MBR-COVERED"))
			var want []involvedPatient
			var omits []string
			if tc.named {
				want, omits = []involvedPatient{{pci: heldPCI(t, "MBR-OX"), involvement: shnsdk.InvolvementRequestNamed}}, nil
			}
			if !slices.Equal(got, want) || !slices.Equal(*omitted, omits) {
				t.Fatalf("named %v, omitted %v; want %v named, %v", got, *omitted, want, omits)
			}
		})
	}
}

// On DTR and the inquiry, whose own patient's name is read as any member's,
// when the budget runs out at the subject's own read no member is named, even
// one read before it and seeding no walk: a held MBR-OX the subject's Patient
// goes by is counted for the budget.
func TestRequestNamedPatients_NoOneIsNamedWhenTheBudgetEndsAtTheSubjectsRead(t *testing.T) {
	mid := func(v string) string { return `{"system":"` + shnsdk.MemberSystem + `","value":"` + v + `"}` }
	patient := `{"resourceType":"Patient","id":"p1","identifier":[` + mid("MBR-OX") + `],"birthDate":"1990-01-01","name":[{"family":"Subject"}]}`
	for leg, payload := range subjectShapes("p1", patient, "Patient/MBR-OX") {
		if leg == "crd-order-dispatch" {
			continue
		}
		t.Run(leg, func(t *testing.T) {
			if got := carriedMembers(payload, leg); !slices.Equal(got, []string{"MBR-OX", "p1"}) {
				t.Fatalf("members %v: the row needs MBR-OX read before p1", got)
			}
			g, omitted := heldRecordsGateway(t)
			subject, found, err := g.resolveSubjectPCI(context.Background(), "p1", payload)
			if err != nil || !found {
				t.Fatalf("subject: %v %v", found, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g.cfg.SoR = &failingReadsSoR{recordsSoR: g.cfg.SoR.(recordsSoR), fail: "p1", cancel: cancel}
			if got := g.requestNamedPatients(ctx, leg, "corr-1", payload, subject); got != nil || !slices.Equal(*omitted, []string{involvedOmitBudget, involvedOmitBudget}) {
				t.Fatalf("named %v, omitted %v; want no one named, both counted for the budget", got, *omitted)
			}
		})
	}
}
