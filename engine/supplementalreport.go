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

// supplementalRefusal is why supplementalReport refused the report: the
// answer's status and reason (which names no patient), whether it is the
// fidelity of the gateway's own build, and the read's error when the read
// itself failed.
type supplementalRefusal struct {
	status   int
	msg      string
	fidelity bool
	err      error
}

// supplementalReport reads the supplemental report the provider's own system
// holds for member, for a claim update this gateway builds, and names the
// member's network patient in it: the report's subject.reference, when it
// names Patient/<sorID> (the Patient this exchange's one reading of the
// system found the member under), is re-pointed to Patient/<member>
// (registered edit E-06). Nothing else in the report changes; when the system
// names the patient by the member id the report is returned exactly as held.
// A non-nil refusal refuses the update:
//   - 502, the fidelity of the gateway's own build, when the report names no
//     subject.reference, names any other subject, or carries a signature over
//     the subject E-06 would re-point: what cannot be carried is what the
//     gateway read from its own participant's system (gateway/STABILITY.md,
//     "What the gateway cannot carry or edit");
//   - 502 when the report cannot be read as a resource, recorded as the
//     system-of-record read failure it is (SoRInvalidAnswer);
//   - 502, the fidelity of the build, when the system holds no supplemental
//     report for the member;
//   - the system-of-record failure when the read fails, with its error.
//
// edits are the registered input edits made to the report ([E-06] when its
// subject was re-pointed, nil when it was returned exactly as held), for the
// seal of the claim update that carries it (moduleBuild.InputEdits).
func (g *Gateway) supplementalReport(ctx context.Context, member, sorID string) ([]byte, []relay.EditID, *supplementalRefusal) {
	report, found, err := ReadSystemOfRecord(g.cfg.SoR).SupplementalReportContext(ctx, member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, nil, &supplementalRefusal{status: status, msg: msg, err: err}
	}
	if !found {
		// The system holds none: unknown, never empty, and nothing the build can
		// carry. The fidelity of the build, as a record read from the
		// participant's own system (502).
		return nil, nil, &supplementalRefusal{status: http.StatusBadGateway, msg: "no supplemental report", fidelity: true}
	}
	out, edited, refusal := rekeySupplementalSubject(report, "Patient/"+sorID, member)
	if refusal != nil {
		return nil, nil, refusal
	}
	var edits []relay.EditID
	if edited {
		edits = []relay.EditID{relay.EditEvidenceSubjectRekey}
	}
	return out, edits, nil
}

// rekeySupplementalSubject applies E-06 to report: its subject.reference,
// which must be sorRef, becomes Patient/<member>; edited reports whether that
// changed it. See supplementalReport.
func rekeySupplementalSubject(report []byte, sorRef, member string) (out []byte, edited bool, refusal *supplementalRefusal) {
	out, edited, err := rekeyEvidenceSubject(report, sorRef, member)
	var signed *relay.SignedContentError
	switch {
	case err == nil:
		return out, edited, nil
	case errors.Is(err, errEvidenceUnreadable):
		// A read of the system the gateway cannot use: the system-of-record
		// failure it is (integrity when the report repeats a member name).
		return nil, false, &supplementalRefusal{status: http.StatusBadGateway, msg: msgSupplementalUnreadable, err: SoRInvalidAnswer(report)}
	case errors.Is(err, errEvidenceNoSubject):
		return nil, false, &supplementalRefusal{status: http.StatusBadGateway, msg: msgSupplementalNoSubject, fidelity: true}
	case errors.Is(err, errEvidenceOtherSubject):
		return nil, false, &supplementalRefusal{status: http.StatusBadGateway, msg: msgSupplementalOtherSubject, fidelity: true}
	case errors.As(err, &signed):
		// E-06, the only edit made here, under a signature: 502, as
		// buildFidelityStatus answers it.
		return nil, false, &supplementalRefusal{status: http.StatusBadGateway, msg: signed.Error(), fidelity: true}
	}
	return nil, false, &supplementalRefusal{status: http.StatusInternalServerError, msg: "prepare supplemental report failed"}
}

// Why rekeyEvidenceSubject did not re-point a record. A signature over the
// subject is a *relay.SignedContentError instead.
var (
	errEvidenceUnreadable   = errors.New("evidence record is not a resource")
	errEvidenceNoSubject    = errors.New("evidence record names no subject.reference")
	errEvidenceOtherSubject = errors.New("evidence record's subject is not the member's patient in the system of record")
	errEvidencePrepare      = errors.New("prepare evidence record failed")
)

// rekeyEvidenceSubject applies registered edit E-06 to record, a clinical
// record the provider's gateway read from its participant's own system for a
// request it builds: its root subject.reference, which must be exactly sorRef
// (Patient/<the id the system holds the member under>), is re-pointed to
// Patient/<member>, and nothing else changes. A record with no
// subject.reference, or one naming anything else (another patient, the member
// id itself under another record id, a versioned or absolute reference), is
// refused, as is one a signature covers (*relay.SignedContentError). A record
// the system already names by the member id is returned exactly as held: the
// replace changes nothing, so Apply returns the body relayed, and edited is
// false: edited is true only when E-06 changed the record.
func rekeyEvidenceSubject(record []byte, sorRef, member string) (out []byte, edited bool, err error) {
	body := relay.NewBody(record, relay.OriginUpstreamResponse)
	doc, err := relay.Doc(body)
	if err != nil || doc.Kind(doc.Root()) != relay.KindObject {
		return nil, false, errEvidenceUnreadable
	}
	subject, ok := doc.Member(doc.Root(), "subject")
	if !ok || doc.Kind(subject) != relay.KindObject {
		return nil, false, errEvidenceNoSubject
	}
	refNode, ok := doc.Member(subject, "reference")
	if !ok || doc.Kind(refNode) != relay.KindString {
		return nil, false, errEvidenceNoSubject
	}
	if v, err := doc.StringValue(refNode); err != nil || v != sorRef {
		return nil, false, errEvidenceOtherSubject
	}
	p, err := relay.Apply(body, "application/fhir+json", relay.EditEvidenceSubjectRekey,
		doc.Replace(refNode, jsonString("Patient/"+member)))
	var signed *relay.SignedContentError
	switch {
	case errors.As(err, &signed):
		return nil, false, signed
	case err != nil:
		return nil, false, errEvidencePrepare
	}
	out, err = relay.Transmit(p, supplementalReportCheck)
	if err != nil {
		return nil, false, errEvidencePrepare
	}
	return out, p.Ownership() == relay.OwnershipEdited, nil
}

// supplementalReportCheck admits an evidence record (the supplemental report,
// or a record the PAS completion reads) exactly as held, or with E-06 alone.
func supplementalReportCheck(p relay.Payload) error {
	switch {
	case p.Ownership() == relay.OwnershipRelayed && len(p.Edits()) == 0:
		return nil
	case p.Ownership() == relay.OwnershipEdited && slices.Equal(p.Edits(), []relay.EditID{relay.EditEvidenceSubjectRekey}):
		return nil
	}
	return fmt.Errorf("evidence record %s %v: only E-06 may change it", p.Ownership(), p.Edits())
}
