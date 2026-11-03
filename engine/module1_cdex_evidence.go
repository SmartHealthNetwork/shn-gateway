//go:build !shn_coreonly

// module1_cdex_evidence.go — the supplemental evidence module 1's attachment
// fetch (UC-05) takes from a facility's records answer, for the
// prior-authorization update it builds.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
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
// the later entry wins), and the Provenance that attributes it (targets it by
// Type/id). Both are the bytes the Bundle holds. It also names the entries
// they are: what the requester builds into its own message, which the fence
// reads apart from the rest of the answer (patientFence.checkEntries).
func cdexEvidenceEntries(bundle []byte) (cdexEvidenceFound, error) {
	var b struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := decodeMessage(bundle, &b); err != nil {
		return cdexEvidenceFound{}, fmt.Errorf("records Bundle is not readable: %w", err)
	}
	type head struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		Target       []struct {
			Reference string `json:"reference"`
		} `json:"target"`
	}
	heads := make([]head, len(b.Entry))
	var report, provenance []byte
	reportPos, provenancePos := -1, -1
	reportRef := ""
	var reportAt time.Time
	reportDated := false
	for i, e := range b.Entry {
		if json.Unmarshal(e.Resource, &heads[i]) != nil {
			return cdexEvidenceFound{}, errors.New("records Bundle entry is not readable")
		}
		if heads[i].ResourceType == "DiagnosticReport" && heads[i].ID != "" {
			at, dated := fhirInstant(recordClinicalDate(e.Resource))
			if report == nil || (dated && (!reportDated || !at.Before(reportAt))) || (!dated && !reportDated) {
				report, reportRef, reportAt, reportDated, reportPos = e.Resource, "DiagnosticReport/"+heads[i].ID, at, dated, i
			}
		}
	}
	if report == nil {
		return cdexEvidenceFound{}, errors.New("records Bundle holds no report")
	}
	for i, e := range b.Entry {
		if heads[i].ResourceType != "Provenance" {
			continue
		}
		for _, t := range heads[i].Target {
			if t.Reference == reportRef {
				provenance, provenancePos = e.Resource, i
			}
		}
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

// repointEvidenceSubject returns a copy of a facility's report whose subject
// names claimPatient.
//
// This is a change the requester makes to its own message, disclosed to
// partners: a prior-authorization update is the requester's own message, and
// its supplemental report must be about the Claim's patient, as the payer
// reads it. The facility's report is checked to be about that patient first
// (requesterRecordsFence). Only the value of the report's subject is
// replaced, by {"reference":<claimPatient>}; every other byte of the
// facility's report is kept, and the result is checked to be exactly that.
// The copy is never presented as the facility's bytes, which reach the
// requester unchanged in the facility's answer.
func repointEvidenceSubject(report []byte, claimPatient string) ([]byte, error) {
	doc, err := relay.Doc(relay.NewBody(report, relay.OriginPeerFrame))
	if err != nil || doc.Kind(doc.Root()) != relay.KindObject {
		return nil, errors.New("supplemental report is not a resource")
	}
	subject, ok := doc.Member(doc.Root(), "subject")
	if !ok || doc.Kind(subject) != relay.KindObject {
		return nil, errors.New("supplemental report has no subject")
	}
	value, err := json.Marshal(struct {
		Reference string `json:"reference"`
	}{claimPatient})
	if err != nil {
		return nil, err
	}
	start, end := doc.Span(subject)
	out := make([]byte, 0, len(report)-(end-start)+len(value))
	out = append(append(append(out, report[:start]...), value...), report[end:]...)
	// The copy is the report with that one value replaced.
	check, err := relay.Doc(relay.NewBody(out, relay.OriginPeerFrame))
	if err != nil {
		return nil, fmt.Errorf("repointed report: %w", err)
	}
	got, ok := check.Member(check.Root(), "subject")
	if !ok {
		return nil, errors.New("repointed report has no subject")
	}
	gs, ge := check.Span(got)
	if gs != start || !bytes.Equal(out[gs:ge], value) || !bytes.Equal(out[:gs], report[:start]) || !bytes.Equal(out[ge:], report[end:]) {
		return nil, errors.New("repointed report differs from the facility's beyond its subject")
	}
	return out, nil
}
