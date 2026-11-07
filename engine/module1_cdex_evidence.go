//go:build !shn_coreonly

// module1_cdex_evidence.go — the supplemental evidence module 1's attachment
// fetch (UC-05) takes from a facility's records answer, for the
// prior-authorization update it builds.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdexEvidenceFound is the evidence cdexEvidenceEntries picks, with the
// positions of the Bundle's entries that hold it.
type cdexEvidenceFound struct {
	report, provenance           []byte
	reportEntry, provenanceEntry int
}

// cdexEvidenceEntries picks the requester's supplemental evidence from a
// facility's records Bundle: the DiagnosticReport with the latest clinical
// date (recordClinicalDate, compared as instants by fhirInstant; a report
// without a readable date comes before any dated one, and of equal instants
// the later entry wins), and the Provenance that attributes it: one whose
// target names the report by the rule a payer reads it by
// (shnsdk.ProvenanceTargetNames: "DiagnosticReport/<id>" or the report entry's
// own fullUrl in this Bundle, version specific only at the report's own
// meta.versionId). Both are the bytes the Bundle holds. The amendment carries
// the target as written and places the report under its own id at the
// amendment's base, so a target written as the facility's own fullUrl (its
// server's, or the federated query's urn), which names the report here, names
// no entry of the amendment: the payer's content check reads it so. It also names the entries
// they are: what the requester builds into its own message, which the fence
// reads apart from the rest of the answer (patientFence.checkEntries).
func cdexEvidenceEntries(bundle []byte) (cdexEvidenceFound, error) {
	var b struct {
		Entry []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := decodeMessage(bundle, &b); err != nil {
		return cdexEvidenceFound{}, fmt.Errorf("records Bundle is not readable: %w", err)
	}
	// Each record's type, id and Provenance targets are read by their exact
	// member names (exactMembers): a "Target" is not a target, as the payer
	// reads it, so it can never make the pick below.
	type head struct {
		ResourceType, ID string
		Target           []string
	}
	headOf := func(raw []byte) (head, error) {
		m, err := exactMembers(raw)
		if err != nil {
			return head{}, err
		}
		h := head{ResourceType: exactString(m, "resourceType"), ID: exactString(m, "id")}
		var targets []json.RawMessage
		if t, ok := m["target"]; ok {
			if err := json.Unmarshal(t, &targets); err != nil {
				return head{}, err
			}
		}
		for _, t := range targets {
			if ref, err := exactMembers(t); err == nil {
				h.Target = append(h.Target, exactString(ref, "reference"))
			}
		}
		return h, nil
	}
	heads := make([]head, len(b.Entry))
	var report, provenance []byte
	reportPos, provenancePos := -1, -1
	var reportID, reportURL, reportVersion string
	var reportAt time.Time
	reportDated := false
	for i, e := range b.Entry {
		h, err := headOf(e.Resource)
		if err != nil {
			return cdexEvidenceFound{}, errors.New("records Bundle entry is not readable")
		}
		heads[i] = h
		if heads[i].ResourceType == "DiagnosticReport" && heads[i].ID != "" {
			at, dated := fhirInstant(recordClinicalDate(e.Resource))
			if report == nil || (dated && (!reportDated || !at.Before(reportAt))) || (!dated && !reportDated) {
				report, reportAt, reportDated, reportPos = e.Resource, at, dated, i
				reportID, reportURL, reportVersion = heads[i].ID, e.FullURL, metaVersionIDOf(e.Resource)
			}
		}
	}
	if report == nil {
		return cdexEvidenceFound{}, errors.New("records Bundle holds no report")
	}
	// Where several Provenances attribute the report, the one whose target also
	// names it in the amendment is taken: the amendment carries the report under
	// its own id at its own base, so the relative form (version specific only at
	// the report's own meta.versionId) names it there, and a target written as
	// the facility's own fullUrl names no entry of it. Within each kind, the last
	// in the facility's Bundle is taken, so the pick does not depend on anything
	// but the Bundle.
	var resolving []byte
	resolvingPos := -1
	for i, e := range b.Entry {
		if heads[i].ResourceType != "Provenance" {
			continue
		}
		for _, ref := range heads[i].Target {
			if !shnsdk.ProvenanceTargetNames(ref, reportURL, "DiagnosticReport", reportID, reportVersion) {
				continue
			}
			provenance, provenancePos = e.Resource, i
			if shnsdk.ProvenanceTargetNames(ref, "", "DiagnosticReport", reportID, reportVersion) {
				resolving, resolvingPos = e.Resource, i
			}
		}
	}
	if resolving != nil {
		provenance, provenancePos = resolving, resolvingPos
	}
	if provenance == nil {
		return cdexEvidenceFound{}, errors.New("records Bundle has no Provenance for its report")
	}
	return cdexEvidenceFound{report: report, provenance: provenance, reportEntry: reportPos, provenanceEntry: provenancePos}, nil
}

// fhirInstant reads a FHIR date or dateTime as an instant: a year, a
// year-month or a date is the start of that period in UTC; a dateTime
// carries its own offset. ok is false for anything else.
func fhirInstant(s string) (time.Time, bool) {
	for _, layout := range []string{"2006", "2006-01", "2006-01-02", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// rekeyFacilityReportSubject applies registered edit E-14 to report, the
// facility's report read on the federated-query leg that the requester
// attaches to the claim update it builds (its own message), so the report
// names the Claim's patient as the payer reads it. The report, and provenance
// (the facility's Provenance that attributes it), must already be shown to be
// about that patient (requesterRecordsFence). Only the value of the report's
// subject.reference is replaced, by claimPatient; every other byte of the
// facility's report, the rest of its subject included, is kept, and the result
// is checked to be exactly that. edits names E-14 when it changed the report,
// and is nil when the report is carried as sent: a subject.reference already
// claimPatient, or a subject naming the member by identifier alone (the fence
// read that identifier as the member's). A signature covering the report, in
// it or in provenance, refuses the edit with a *relay.SignedContentError. The
// copy is never presented as the facility's bytes, which reach the requester
// unchanged in the facility's answer.
func rekeyFacilityReportSubject(report, provenance []byte, claimPatient string) (out []byte, edits []relay.EditID, err error) {
	body := relay.NewBody(report, relay.OriginPeerFrame)
	doc, err := relay.Doc(body)
	if err != nil || doc.Kind(doc.Root()) != relay.KindObject {
		return nil, nil, errors.New("supplemental report is not a resource")
	}
	subject, ok := doc.Member(doc.Root(), "subject")
	if !ok || doc.Kind(subject) != relay.KindObject {
		return nil, nil, errors.New("supplemental report has no subject")
	}
	refNode, ok := doc.Member(subject, "reference")
	if !ok {
		// The member by identifier alone: carried as sent.
		return report, nil, nil
	}
	if doc.Kind(refNode) != relay.KindString {
		return nil, nil, errors.New("supplemental report's subject.reference is not a string")
	}
	p, err := relay.Apply(body, "application/fhir+json", relay.EditFacilityReportSubjectRekey,
		doc.Replace(refNode, jsonString(claimPatient)))
	var signed *relay.SignedContentError
	switch {
	case errors.As(err, &signed):
		return nil, nil, signed
	case err != nil:
		return nil, nil, fmt.Errorf("re-point supplemental report: %w", err)
	}
	if p.Ownership() == relay.OwnershipEdited {
		signed, err := provenanceSigned(provenance)
		if err != nil {
			return nil, nil, err
		}
		if signed {
			return nil, nil, &relay.SignedContentError{Edit: relay.EditFacilityReportSubjectRekey, Carrier: "Provenance.signature"}
		}
	}
	out, err = relay.Transmit(p, facilityReportCheck)
	if err != nil {
		return nil, nil, fmt.Errorf("re-point supplemental report: %w", err)
	}
	if p.Ownership() == relay.OwnershipEdited {
		edits = []relay.EditID{relay.EditFacilityReportSubjectRekey}
	}
	return out, edits, nil
}

// provenanceSigned reports whether the facility's Provenance that attributes
// the report carries a signature, which covers the report it targets. A
// Provenance it cannot read is an error: whether a signature covers the
// report is unknown, so the edit is not made.
func provenanceSigned(provenance []byte) (bool, error) {
	doc, err := relay.Doc(relay.NewBody(provenance, relay.OriginPeerFrame))
	if err != nil || doc.Kind(doc.Root()) != relay.KindObject {
		return false, errors.New("the Provenance attributing the supplemental report is not a resource")
	}
	_, ok := doc.Member(doc.Root(), "signature")
	return ok, nil
}

// facilityReportCheck admits the facility's report exactly as answered, or
// with E-14 alone.
func facilityReportCheck(p relay.Payload) error {
	switch {
	case p.Ownership() == relay.OwnershipRelayed && len(p.Edits()) == 0:
		return nil
	case p.Ownership() == relay.OwnershipEdited && slices.Equal(p.Edits(), []relay.EditID{relay.EditFacilityReportSubjectRekey}):
		return nil
	}
	return fmt.Errorf("facility report %s %v: only E-14 may change it", p.Ownership(), p.Edits())
}
