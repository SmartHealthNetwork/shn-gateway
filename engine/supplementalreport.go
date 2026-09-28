package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// Refusals of the supplemental report a claim update would carry. Each names
// the reason, never a patient.
const (
	msgSupplementalUnreadable   = "supplemental report is not a resource"
	msgSupplementalNoSubject    = "supplemental report names no subject.reference"
	msgSupplementalOtherSubject = "supplemental report's subject is not the member's patient in the system of record"
)

// supplementalReport reads the supplemental report the provider's own system
// holds for member, for a claim update this gateway builds, and names the
// member's network patient in it: the report's subject.reference, when it
// names Patient/<sorID> (the Patient this exchange's one reading of the
// system found the member under), is re-pointed to Patient/<member>
// (registered edit E-06). Nothing else in the report changes; when the system
// names the patient by the member id the report is returned exactly as held.
// A non-zero status refuses the update:
//   - 500 when the system holds no supplemental report for the member;
//   - 422 when the report names no subject.reference, names any other
//     subject, or carries a signature over the subject;
//   - 502 when the report cannot be read as a resource;
//   - the system-of-record failure when the read fails.
func (g *Gateway) supplementalReport(ctx context.Context, member, sorID string) ([]byte, int, string) {
	report, found, err := ReadSystemOfRecord(g.cfg.SoR).SupplementalReportContext(ctx, member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, status, msg
	}
	if !found {
		return nil, http.StatusInternalServerError, "no supplemental report"
	}
	return rekeySupplementalSubject(report, "Patient/"+sorID, member)
}

// rekeySupplementalSubject applies E-06 to report: its subject.reference,
// which must be sorRef, becomes Patient/<member>. See supplementalReport.
func rekeySupplementalSubject(report []byte, sorRef, member string) ([]byte, int, string) {
	body := relay.NewBody(report, relay.OriginUpstreamResponse)
	doc, err := relay.Doc(body)
	if err != nil || doc.Kind(doc.Root()) != relay.KindObject {
		return nil, http.StatusBadGateway, msgSupplementalUnreadable
	}
	subject, ok := doc.Member(doc.Root(), "subject")
	if !ok || doc.Kind(subject) != relay.KindObject {
		return nil, http.StatusUnprocessableEntity, msgSupplementalNoSubject
	}
	refNode, ok := doc.Member(subject, "reference")
	if !ok || doc.Kind(refNode) != relay.KindString {
		return nil, http.StatusUnprocessableEntity, msgSupplementalNoSubject
	}
	if v, err := doc.StringValue(refNode); err != nil || v != sorRef {
		return nil, http.StatusUnprocessableEntity, msgSupplementalOtherSubject
	}
	// A report the system already names by the member id is left exactly as
	// held: the replace changes nothing, so Apply returns the body relayed.
	p, err := relay.Apply(body, "application/fhir+json", relay.EditEvidenceSubjectRekey,
		doc.Replace(refNode, jsonString("Patient/"+member)))
	var signed *relay.SignedContentError
	switch {
	case errors.As(err, &signed):
		return nil, http.StatusUnprocessableEntity, signed.Error()
	case err != nil:
		return nil, http.StatusInternalServerError, "prepare supplemental report failed"
	}
	out, err := relay.Transmit(p, supplementalReportCheck)
	if err != nil {
		return nil, http.StatusInternalServerError, "prepare supplemental report failed"
	}
	return out, 0, ""
}

// supplementalReportCheck admits the report exactly as held, or with E-06
// alone.
func supplementalReportCheck(p relay.Payload) error {
	switch {
	case p.Ownership() == relay.OwnershipRelayed && len(p.Edits()) == 0:
		return nil
	case p.Ownership() == relay.OwnershipEdited && slices.Equal(p.Edits(), []relay.EditID{relay.EditEvidenceSubjectRekey}):
		return nil
	}
	return fmt.Errorf("supplemental report %s %v: only E-06 may change it", p.Ownership(), p.Edits())
}
