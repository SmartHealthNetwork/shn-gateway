package engine

import (
	"crypto/ed25519"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// subjectWant is what this gateway expects a token's subject to be, stated
// structurally: from the leg's taxonomy row (whether its request, or its
// answer, names a patient) and from what this gateway holds. It never rides
// in an empty string, since unknown is not empty, and it maps one to one onto
// the SDK's own statement of it (shnsdk.SubjectWant, sdk below), which
// verifies it. The zero value states nothing and is refused.
type subjectWant struct {
	kind subjectWantKind
	pci  string
}

type subjectWantKind int

const (
	subjectWantUnstated subjectWantKind = iota
	subjectWantIs
	subjectWantNamed
	subjectWantNone
	subjectWantFromAnswer
)

// subjectIs expects the patient this gateway holds: the token's subject must
// be pci. A pci that is empty or blank states no patient and is refused
// (shnsdk.ErrSubjectUnstated), never read as "do not check".
func subjectIs(pci string) subjectWant { return subjectWant{kind: subjectWantIs, pci: pci} }

// subjectNamed expects some patient, on a leg that names one, where this
// gateway does not yet hold it (a receiving gateway binds the patient the
// request names afterwards): the token must name a patient identifier.
func subjectNamed() subjectWant { return subjectWant{kind: subjectWantNamed} }

// subjectNone expects no patient, on a leg whose row says this side names
// none.
func subjectNone() subjectWant { return subjectWant{kind: subjectWantNone} }

// subjectFromAnswer expects the patient an answer discloses, or none, on a
// leg that takes its patient from the answer.
func subjectFromAnswer() subjectWant { return subjectWant{kind: subjectWantFromAnswer} }

// sdk is w as the SDK states it, one to one. The zero value maps to the SDK's
// zero value, which states nothing and is refused (shnsdk.ErrSubjectUnstated);
// the SDK's SubjectChecked, a caller that checks the subject itself, has no
// counterpart here: this gateway always states what it expects.
func (w subjectWant) sdk() shnsdk.SubjectWant {
	switch w.kind {
	case subjectWantIs:
		return shnsdk.SubjectIs(w.pci)
	case subjectWantNamed:
		return shnsdk.SubjectNamed()
	case subjectWantNone:
		return shnsdk.SubjectNone()
	case subjectWantFromAnswer:
		return shnsdk.SubjectFromAnswer()
	}
	return shnsdk.SubjectWant{}
}

// verifyBoundWant is shnsdk.VerifyBoundWant with the subject stated as this
// gateway states it (subjectWant): every binding is required, an empty or
// blank one refused (shnsdk.ErrBindingUnstated) and an unstated subject
// refused (shnsdk.ErrSubjectUnstated), both before the token is read; a
// subject that does not fit is shnsdk.ErrSubjectMismatch. Every token this
// gateway verifies as a leg's own goes through it, and no engine file calls
// the SDK's deprecated forms (TestEveryVerifyStatesItsSubject).
func verifyBoundWant(t shnsdk.Token, pub ed25519.PublicKey, now time.Time, wantFrame, wantOp, wantCorr, wantHolder string, subject subjectWant, wantPayloadHash string) error {
	return shnsdk.VerifyBoundWant(t, pub, now, wantFrame, wantOp, wantCorr, wantHolder, subject.sdk(), wantPayloadHash)
}

// requestSubjectWant is what a leg's request token must name, by the leg's
// row: on a leg whose request names its patient, held (the patient the
// originating gateway bound) or, where held is not this gateway's to know,
// some patient; on a leg that names none, or takes its patient from the
// answer, none.
func (s legSpec) requestSubjectWant(held string, holds bool) subjectWant {
	if s.namesNoPatient() || s.fromAnswer() {
		return subjectNone()
	}
	if !holds {
		return subjectNamed()
	}
	return subjectIs(held)
}

// answerSubjectWant is what a leg's answer token must name, by the leg's row:
// the request's patient on a leg whose request names one, none on a leg that
// names none, and the patient the answer discloses, or none, on a leg that
// takes its patient from the answer.
func (s legSpec) answerSubjectWant(requestPatient string) subjectWant {
	switch {
	case s.namesNoPatient():
		return subjectNone()
	case s.fromAnswer():
		return subjectFromAnswer()
	}
	return subjectIs(requestPatient)
}

// mintedSubjectWant is what a token this gateway asked the Framework for must
// name, for the patient it asked for (held): on its request, the leg's
// request rule for the patient it holds; on its answer, the leg's answer
// rule, where an answer on a leg that takes its patient from the answer names
// the patient this gateway asked for, or, for an answer that discloses no
// patient's data, none.
func (s legSpec) mintedSubjectWant(held string, answer bool) subjectWant {
	if !answer {
		return s.requestSubjectWant(held, true)
	}
	if s.fromAnswer() {
		if held == "" {
			return subjectNone()
		}
		return subjectIs(held)
	}
	return s.answerSubjectWant(held)
}

// verifyAnswerToken verifies the token an answer on leg txType arrived with:
// bound to the leg's answer, to the original request's correlation and to the
// responder, the holder the leg was routed to (never the answer envelope's own
// claim of its sender), and naming the patient the leg's row says the answer names
// (answerSubjectWant) for pci, the patient this gateway's request named. A
// transaction type of no leg this gateway compiles is refused.
func (g *Gateway) verifyAnswerToken(tok shnsdk.Token, txType, respFrame, respOp, corr, responder, pci, payloadHash string) error {
	spec, ok := legSpecFor(txType)
	if !ok {
		return shnsdk.ErrSubjectUnstated
	}
	return verifyBoundWant(tok, g.cfg.AuthzPub, g.cfg.Clock(), respFrame, respOp, corr, responder, spec.answerSubjectWant(pci), payloadHash)
}

// verifyInboundToken verifies the token a request on leg spec arrived with:
// bound to the leg's request, the envelope's correlation and its sender, and
// naming the patient the leg's row says the request names: some patient on a
// leg whose request names one, which this gateway binds afterwards, and none on
// a leg that names none or takes its patient from the answer. An envelope
// that states no sender, or a blank correlation, binds nothing and is refused
// (shnsdk.ErrBindingUnstated) before the token is read.
func (g *Gateway) verifyInboundToken(tok shnsdk.Token, spec legSpec, corr, sender, payloadHash string) error {
	return verifyBoundWant(tok, g.cfg.AuthzPub, g.cfg.Clock(), spec.ReqFrame, spec.Op, corr, sender, spec.requestSubjectWant("", false), payloadHash)
}
