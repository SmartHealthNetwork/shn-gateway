//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// subjectBundle is a Bundle carrying each resource as an entry.
func subjectBundle(resources ...string) []byte {
	b := `{"resourceType":"Bundle","type":"collection","entry":[`
	for i, r := range resources {
		if i > 0 {
			b += ","
		}
		b += `{"resource":` + r + `}`
	}
	return []byte(b + `]}`)
}

// A leg whose subject reference resolves to a Patient the request carries (its
// subject resource) binds a member the system of record does not hold from that
// Patient alone: from its birth date and family name, else from the member id
// alone, never from another Patient the request carries under the member's id. A
// subject without both, in a request that carries another person under the member's
// number (a coverage's party), identifies no one patient: refused 400, on authority.
// With no subject resource the binding is the leg's as before.
func TestResolveSubjectPCIFrom_TheSubjectResourceDecides(t *testing.T) {
	const birth, family = "2015-06-01", "Childname"
	subject := []byte(requestPatient(strangerMember, birth, family))
	bare := []byte(`{"resourceType":"Patient","id":"` + strangerMember + `"}`)
	other := requestPatient(strangerMember, "1999-01-01", "Othername")
	// otherByID is a Patient for the member by its member identifier, under another id,
	// that the Coverage names as its beneficiary: the party rule reads it as the member's
	// own (visitCarriedPatients), and earlier releases bound the member from it. The
	// Coverage's subscriber and policyholder are another person under the member's
	// number.
	otherByID := strings.Replace(other, `"id":"`+strangerMember+`"`, `"id":"other"`, 1)
	party := partyCoverage(`"contained":[` + exactParent + `]`)
	beneficiaryParty := strings.Replace(party, `"beneficiary":{"reference":"Patient/`+strangerMember+`"}`, `"beneficiary":{"reference":"Patient/other"}`, 1)
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	ctx := context.Background()
	for name, row := range map[string]struct {
		payload, subject []byte
		want             string
		err              error
	}{
		"a subject with demographics, beside a same-id Patient that disagrees": {
			subjectBundle(string(subject), other), subject, derivedPCI(strangerMember, birth, family), nil},
		"a subject with demographics, beside a party under the member's number": {
			subjectBundle(string(subject), party), subject, derivedPCI(strangerMember, birth, family), nil},
		"a subject without them, beside a same-id Patient with them": {
			subjectBundle(string(bare), other), bare, strangerPCI(), nil},
		"a subject without them, beside a member's Patient with them and a party under the member's number": {
			subjectBundle(string(bare), otherByID, beneficiaryParty), bare, "", errSubjectUnidentifiable},
		"a subject without them, beside a party and no Patient with them (as before)": {
			subjectBundle(string(bare), party), bare, "", errPatientUnidentifiable},
		"no subject resource, two same-id Patients that disagree (as before)": {
			subjectBundle(string(subject), other), nil, strangerPCI(), nil},
		"no subject resource, one same-id Patient (as before)": {
			subjectBundle(string(subject)), nil, derivedPCI(strangerMember, birth, family), nil},
	} {
		t.Run(name, func(t *testing.T) {
			pci, found, err := g.resolveSubjectPCIFrom(ctx, strangerMember, row.payload, row.subject)
			if row.err != nil {
				subjectText := errors.Is(err, errSubjectUnidentifiable)
				if !errors.Is(err, row.err) || !errors.Is(err, errPatientUnidentifiable) || subjectText != (row.err == errSubjectUnidentifiable) || pci != "" || found {
					t.Fatalf("got (%q, found=%v, err=%v), want %v", pci, found, err, row.err)
				}
				return
			}
			if err != nil || !found || pci != row.want {
				t.Fatalf("got (%q, found=%v, err=%v), want %q", pci, found, err, row.want)
			}
		})
	}

	// The payer side binds by the same rule, and answers the unidentifiable subject
	// 400 with its own text.
	if pci, status, msg := g.bindInboundSubjectFrom(ctx, strangerMember, subjectBundle(string(subject), other), subject); status != 0 || pci != derivedPCI(strangerMember, birth, family) {
		t.Fatalf("payer, subject with demographics: got (%q, %d, %q)", pci, status, msg)
	}
	if pci, status, msg := g.bindInboundSubjectFrom(ctx, strangerMember, subjectBundle(string(bare), otherByID, beneficiaryParty), bare); status != http.StatusBadRequest || msg != msgSubjectUnidentifiable || pci != "" {
		t.Fatalf("payer, unidentifiable subject: got (%q, %d, %q), want 400 %q", pci, status, msg, msgSubjectUnidentifiable)
	}
	if status, msg := subjectBindingFailure(ctx, errSubjectUnidentifiable); status != http.StatusBadRequest || msg != msgSubjectUnidentifiable {
		t.Fatalf("subjectBindingFailure: got (%d, %q)", status, msg)
	}

	// Earlier releases bound that request from the member's other Patient: with no
	// subject resource it still does (the control).
	if pci, found, err := g.resolveSubjectPCIFrom(ctx, strangerMember, subjectBundle(string(bare), otherByID, beneficiaryParty), nil); err != nil || !found || pci != derivedPCI(strangerMember, "1999-01-01", "Othername") {
		t.Fatalf("no subject resource beside a party: got (%q, found=%v, err=%v)", pci, found, err)
	}

	// A member the system of record holds binds from it, whatever the subject says.
	held, _, _ := newCensusSoR().ResolvePatient("MBR-COVERED")
	heldSubject := []byte(requestPatient("MBR-COVERED", "1900-01-01", "Other"))
	if pci, found, err := g.resolveSubjectPCIFrom(ctx, "MBR-COVERED", subjectBundle(string(heldSubject)), heldSubject); err != nil || !found || pci != held {
		t.Fatalf("held member: got (%q, found=%v, err=%v), want the record's %q", pci, found, err, held)
	}
}
