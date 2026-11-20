package engine

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// subjectWant is what this gateway expects a token's subject to be, stated
// structurally: from the leg's taxonomy row (whether its request, or its
// answer, names a patient) and from what this gateway holds. It never rides
// in an empty string: shnsdk.VerifyBound reads an empty expected subject as
// "do not check", which reads the same whether the leg names no patient, the
// gateway does not hold the patient, or the value came out empty by accident,
// and unknown is not empty. The zero value states nothing and is refused.
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
// (errSubjectUnstated), never read as "do not check".
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

// errSubjectUnstated is a verify whose expected subject states nothing: the
// zero subjectWant, or subjectIs with no patient.
var errSubjectUnstated = errors.New("no expected subject")

// errSubjectMisfit is a token whose subject does not fit the leg's row or the
// patient this gateway holds.
var errSubjectMisfit = errors.New("token subject does not fit the leg")

// namesPatient reports whether subject names a patient: a network patient
// identifier ("pci:" and a value).
func namesPatient(subject string) bool {
	return strings.HasPrefix(subject, "pci:") && len(subject) > len("pci:")
}

// fits reports whether subject fits w; ok is false when w states nothing.
func (w subjectWant) fits(subject string) (fits, ok bool) {
	switch w.kind {
	case subjectWantIs:
		if strings.TrimSpace(w.pci) == "" {
			return false, false
		}
		return subject == w.pci, true
	case subjectWantNamed:
		return namesPatient(subject), true
	case subjectWantNone:
		return subject == "", true
	case subjectWantFromAnswer:
		return subject == "" || namesPatient(subject), true
	}
	return false, false
}

// verifyBoundWant is shnsdk.VerifyBound with the subject stated structurally
// (subjectWant): an unstated expectation is refused before the token is read,
// every other binding is VerifyBound's own, and the subject is then held to
// the expectation. The SDK's VerifyBound is called with the held patient for
// subjectIs, and with no subject otherwise, which this check then decides.
func verifyBoundWant(t shnsdk.Token, pub ed25519.PublicKey, now time.Time, wantFrame, wantOp, wantCorr, wantHolder string, subject subjectWant, wantPayloadHash string) error {
	if _, ok := subject.fits(""); !ok {
		return errSubjectUnstated
	}
	held := ""
	if subject.kind == subjectWantIs {
		held = subject.pci
	}
	if err := shnsdk.VerifyBound(t, pub, now, wantFrame, wantOp, wantCorr, wantHolder, held, wantPayloadHash); err != nil {
		return err
	}
	if fits, _ := subject.fits(t.Subject); !fits {
		return errSubjectMisfit
	}
	return nil
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
// responder, and naming the patient the leg's row says the answer names
// (answerSubjectWant) for pci, the patient this gateway's request named. A
// transaction type of no leg this gateway compiles is refused.
func (g *Gateway) verifyAnswerToken(tok shnsdk.Token, txType, respFrame, respOp, corr, responder, pci, payloadHash string) error {
	spec, ok := legSpecFor(txType)
	if !ok {
		return errSubjectUnstated
	}
	return verifyBoundWant(tok, g.cfg.AuthzPub, g.cfg.Clock(), respFrame, respOp, corr, responder, spec.answerSubjectWant(pci), payloadHash)
}

// verifyInboundToken verifies the token a request on leg spec arrived with:
// bound to the leg's request, the envelope's correlation and its sender, and
// naming the patient the leg's row says the request names: some patient on a
// leg whose request names one, which this gateway binds afterwards, and none on
// a leg that names none or takes its patient from the answer.
func (g *Gateway) verifyInboundToken(tok shnsdk.Token, spec legSpec, corr, sender, payloadHash string) error {
	return verifyBoundWant(tok, g.cfg.AuthzPub, g.cfg.Clock(), spec.ReqFrame, spec.Op, corr, sender, spec.requestSubjectWant("", false), payloadHash)
}
