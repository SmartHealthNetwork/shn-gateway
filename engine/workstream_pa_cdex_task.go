//go:build !shn_coreonly

package engine

import (
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextask"
)

// What the payer's gateway (its ingress, handleCDexRequestIngress) and the
// provider's gateway (the leg delivered to it, handleCDexRequestInbound)
// decide alike on one attachment-request Task, so the two ends of the leg
// refuse the same Task the same way and bind it to one patient by one rule.

// The refusals both ends make of a Task.
const (
	errCDexRequestTwoWays = "the Task can be read two ways: it repeats a member name, or names one in another case"
	errCDexRequestNotTask = "not a CDex Task Attachment Request: the body is not a Task this leg carries"
	errCDexRequestCode    = "the Task's code is not one this leg carries: it takes a Task Attachment Request coded " +
		cdextask.PASTempCodes + "#" + cdextask.CodeAttachmentRequest + " or " +
		cdextask.PASTempCodes + "#" + cdextask.CodeAttachmentRequestQuestionnaire
	errCDexRequestNoPatient = "cannot bind the request to a patient: Task.for names no one member number (an identifier typed v2-0203#MB)"
)

// cdexRequestCodeTaken reports whether the Task's code is one the leg carries,
// read by system and code: a request for attachments by code, or (D128) a
// request for a questionnaire to be completed. Any other code, the CDex
// data-request codes among them, is outside the leg's authority. Both ends of
// the leg decide by it.
func cdexRequestCodeTaken(task cdextask.Task) bool {
	return task.Code.Has(cdextask.PASTempCodes, cdextask.CodeAttachmentRequest) ||
		task.Code.Has(cdextask.PASTempCodes, cdextask.CodeAttachmentRequestQuestionnaire)
}

// v20203MB is the identifier type of a member number (HL7 v2 table 0203, MB):
// the slice cdex-patient-demographics requires of the contained Patient.
var v20203MB = cdextask.Coding{System: "http://terminology.hl7.org/CodeSystem/v2-0203", Code: "MB"}

// cdexTaskMember is the member the Task's patient is bound by: the value of
// each member number (an identifier typed v2-0203#MB) the Task names for
// Task.for, on the reference itself (Task.for.identifier) and on the Patient it
// contains as Task.for. ok is false when it names none, two that differ, or an
// identifier on the reference that cannot be read: it then names no one patient
// the network can identify (D121). Whether the
// Patient is contained, or the member number sits on the reference, is the
// profile's concern, never the network's.
func cdexTaskMember(task cdextask.Task) (string, bool) {
	p := task.Patient
	if p.State != cdextask.Present {
		return "", false
	}
	ids := slices.Clone(p.Identifiers)
	switch p.Identifier.State {
	case cdextask.Present:
		ids = append(ids, p.Identifier.Identifier)
	case cdextask.Unreadable:
		// An identifier on the reference that cannot be read could be a
		// member number that differs: unknown, never none.
		return "", false
	}
	member := ""
	for _, id := range ids {
		if !slices.Contains(id.Type, v20203MB) {
			continue
		}
		if member != "" && id.Value != member {
			return "", false
		}
		member = id.Value
	}
	return member, member != ""
}
