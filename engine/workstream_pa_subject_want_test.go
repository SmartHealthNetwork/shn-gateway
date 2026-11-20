//go:build !shn_coreonly

package engine

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const wantPCI = "pci:00000000000000000000000000000001"

// verifyBoundWant holds a token's subject to the expectation it is given:
// the patient this gateway holds, some patient, none, or the answer's patient
// or none. An expectation that states nothing, or a held patient that came out
// empty, is refused before the token is read: never read as "do not check".
func TestVerifyBoundWant_HoldsTheSubjectToItsExpectation(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	tok := func(subject string) shnsdk.Token {
		return signTestToken(shnsdk.Token{Operation: "op", Scope: "s", Subject: subject, Frame: "f", Holder: "h",
			CorrelationID: "c", PayloadHash: "hash", Expiry: now.Add(time.Hour)}, priv)
	}
	for name, row := range map[string]struct {
		subject string
		want    subjectWant
		err     error // nil: accepted
	}{
		"the held patient":                      {wantPCI, subjectIs(wantPCI), nil},
		"another patient than the held one":     {"pci:00000000000000000000000000000002", subjectIs(wantPCI), errors.New("any")},
		"no patient where one is held":          {"", subjectIs(wantPCI), errors.New("any")},
		"a held patient that came out empty":    {"", subjectIs(""), errSubjectUnstated},
		"a held patient that is blank":          {" ", subjectIs(" "), errSubjectUnstated},
		"some patient, named":                   {wantPCI, subjectNamed(), nil},
		"some patient, none named":              {"", subjectNamed(), errSubjectMisfit},
		"some patient, not a pci":               {"member-1", subjectNamed(), errSubjectMisfit},
		"no patient, none named":                {"", subjectNone(), nil},
		"no patient, one named":                 {wantPCI, subjectNone(), errSubjectMisfit},
		"the answer's patient":                  {wantPCI, subjectFromAnswer(), nil},
		"the answer's, none":                    {"", subjectFromAnswer(), nil},
		"the answer's, not a pci":               {"member-1", subjectFromAnswer(), errSubjectMisfit},
		"an expectation never stated":           {wantPCI, subjectWant{}, errSubjectUnstated},
		"never stated, and a token naming none": {"", subjectWant{}, errSubjectUnstated},
	} {
		t.Run(name, func(t *testing.T) {
			err := verifyBoundWant(tok(row.subject), pub, now, "f", "op", "c", "h", row.want, "hash")
			switch {
			case row.err == nil && err != nil:
				t.Fatalf("refused: %v; want accepted", err)
			case row.err != nil && err == nil:
				t.Fatal("accepted; want refused")
			case row.err == errSubjectUnstated || row.err == errSubjectMisfit:
				if !errors.Is(err, row.err) {
					t.Fatalf("err %v; want %v", err, row.err)
				}
			}
		})
	}
}

// Every leg's tokens are held to its taxonomy row, never to an empty expected
// subject: on a leg whose request names its patient, the request token names
// the patient the originating gateway bound (some patient, at the receiving
// gateway, which binds it afterwards) and the answer's names the same patient;
// on a leg that names none, neither names one; on a leg that takes its patient
// from the answer, the request's names none and the answer's names the patient
// it discloses, or none.
func TestSubjectWant_EveryLegOfTheCatalogByItsRow(t *testing.T) {
	legs := 0
	for _, w := range workstreams {
		for tx, s := range w.catalog() {
			legs++
			var req, recv, answer, minted subjectWant
			switch {
			case s.namesNoPatient():
				req, recv, answer, minted = subjectNone(), subjectNone(), subjectNone(), subjectNone()
			case s.fromAnswer():
				req, recv, answer, minted = subjectNone(), subjectNone(), subjectFromAnswer(), subjectIs(wantPCI)
			default:
				req, recv, answer, minted = subjectIs(wantPCI), subjectNamed(), subjectIs(wantPCI), subjectIs(wantPCI)
			}
			if got := s.requestSubjectWant(wantPCI, true); got != req {
				t.Errorf("%s: the originated request's expected patient %+v, want %+v", tx, got, req)
			}
			if got := s.requestSubjectWant("", false); got != recv {
				t.Errorf("%s: the received request's expected patient %+v, want %+v", tx, got, recv)
			}
			if got := s.answerSubjectWant(wantPCI); got != answer {
				t.Errorf("%s: the answer's expected patient %+v, want %+v", tx, got, answer)
			}
			if got := s.mintedSubjectWant(wantPCI, true); got != minted {
				t.Errorf("%s: this gateway's own answer's expected patient %+v, want %+v", tx, got, minted)
			}
			if s.fromAnswer() {
				if got := s.mintedSubjectWant("", true); got != subjectNone() {
					t.Errorf("%s: an answer disclosing no patient's data %+v, want none", tx, got)
				}
			}
			if !s.namesNoPatient() && !s.fromAnswer() {
				if got := s.requestSubjectWant("", true); got != subjectIs("") {
					t.Errorf("%s: an originated request whose patient came out empty %+v, want an unstated expectation", tx, got)
				}
			}
		}
	}
	if legs < 10 {
		t.Fatalf("read %d legs: the rows must see every leg", legs)
	}
}

// The originating gateway holds the token the Framework minted for its
// request to the leg's row and to the patient it bound: a patient leg's token
// for no patient, or for another, is not used, and neither is one minted for a
// leg whose request names no patient that names one. An operation of no leg
// this gateway compiles is refused. A token that fits is used.
func TestVerifyAuthorized_TheMintedTokenFitsTheLegsRow(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	g := &Gateway{cfg: Config{HolderID: "provider", AuthzPub: pub, Clock: func() time.Time { return now }}}
	mint := func(op, subject string) shnsdk.Token {
		return signTestToken(shnsdk.Token{Operation: op, Scope: "s", Subject: subject, Frame: "f", Holder: "provider",
			CorrelationID: "c", PayloadHash: "hash", Expiry: now.Add(time.Hour)}, priv)
	}
	req := func(op, subject string) authorizeReq {
		return authorizeReq{Frame: "f", Operation: op, SubjectPCI: subject, CorrelationID: "c", PayloadHash: "hash"}
	}
	for name, row := range map[string]struct {
		tok  shnsdk.Token
		req  authorizeReq
		used bool
	}{
		"a patient leg, its patient":                            {mint("eligibility-inquiry", wantPCI), req("eligibility-inquiry", wantPCI), true},
		"a patient leg, a token for no patient":                 {mint("eligibility-inquiry", ""), req("eligibility-inquiry", wantPCI), false},
		"a patient leg, a token for another patient":            {mint("eligibility-inquiry", "pci:00000000000000000000000000000002"), req("eligibility-inquiry", wantPCI), false},
		"a patient leg, a request whose patient is empty":       {mint("eligibility-inquiry", ""), req("eligibility-inquiry", ""), false},
		"a patient leg's answer, its patient":                   {mint("eligibility-response", wantPCI), req("eligibility-response", wantPCI), true},
		"a patient leg's answer, empty":                         {mint("eligibility-response", ""), req("eligibility-response", ""), false},
		"a no-patient leg, a token for none":                    {mint("pas-signal", ""), req("pas-signal", ""), true},
		"a no-patient leg, a token naming a patient":            {mint("pas-signal", wantPCI), req("pas-signal", wantPCI), false},
		"a from-answer leg's request, a token for none":         {mint("cdex-task-read", ""), req("cdex-task-read", ""), true},
		"a from-answer leg's request, naming a patient":         {mint("cdex-task-read", wantPCI), req("cdex-task-read", wantPCI), false},
		"a from-answer leg's answer, the disclosed patient":     {mint("cdex-task-read-response", wantPCI), req("cdex-task-read-response", wantPCI), true},
		"a from-answer leg's answer, disclosing none":           {mint("cdex-task-read-response", ""), req("cdex-task-read-response", ""), true},
		"a from-answer answer disclosing none, a patient named": {mint("cdex-task-read-response", wantPCI), req("cdex-task-read-response", ""), false},
		"an operation of no leg":                                {mint("no-such-operation", wantPCI), req("no-such-operation", wantPCI), false},
	} {
		t.Run(name, func(t *testing.T) {
			err := g.verifyAuthorized(row.tok, row.req)
			if row.used && err != nil {
				t.Fatalf("not used: %v; want used", err)
			}
			if !row.used && err == nil {
				t.Fatal("used; want refused")
			}
		})
	}
}

// The receiving gateway refuses, at every level, a request whose token does
// not fit the leg's row before it reads the request: a token naming no patient,
// or one that is no patient identifier, on a leg whose request names its
// patient. A token naming the patient is answered as before.
func TestInbound_ARequestTokenThatDoesNotFitTheLegIsRefused(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		for name, subject := range map[string]string{"no patient": "", "no patient identifier": "member-1", "a bare prefix": "pci:"} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := eligibilityPayer(t, level, false)
				ans := p.sendAs(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember), subject)
				if ans.status != http.StatusForbidden || !bytes.Contains(ans.body, []byte("authz verification failed")) || p.partner.calls.Load() != 0 {
					t.Fatalf("answer %d %s; want 403 authz verification failed, nothing read", ans.status, ans.body)
				}
			})
		}
		t.Run(level.String()+"/control", func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			if ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember)); ans.status != http.StatusOK {
				t.Fatalf("answer %d %s; want answered", ans.status, ans.body)
			}
		})
	}
}

// An answer's token is held to the leg's row for the patient this gateway's
// request named: on a leg whose request names its patient, that patient; on a
// leg that names none, none; on a leg that takes its patient from the answer,
// the patient it discloses, or none. A transaction type of no leg is refused.
func TestVerifyAnswerToken_TheAnswerFitsTheLegsRow(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	g := &Gateway{cfg: Config{AuthzPub: pub, Clock: func() time.Time { return now }}}
	for name, row := range map[string]struct {
		tx, respOp, subject, requestPatient string
		used                                bool
	}{
		"a patient leg, the request's patient":            {"coverage-eligibility", "eligibility-response", wantPCI, wantPCI, true},
		"a patient leg, another patient":                  {"coverage-eligibility", "eligibility-response", "pci:00000000000000000000000000000002", wantPCI, false},
		"a patient leg, no patient":                       {"coverage-eligibility", "eligibility-response", "", wantPCI, false},
		"a patient leg, a request whose patient is empty": {"coverage-eligibility", "eligibility-response", "", "", false},
		"a no-patient leg, none":                          {"pas-subscription-signal", "pas-signal-response", "", "", true},
		"a no-patient leg, a patient":                     {"pas-subscription-signal", "pas-signal-response", wantPCI, "", false},
		"a from-answer leg, the disclosed patient":        {"cdex-task-status", "cdex-task-read-response", wantPCI, "", true},
		"a from-answer leg, none":                         {"cdex-task-status", "cdex-task-read-response", "", "", true},
		"a from-answer leg, no patient identifier":        {"cdex-task-status", "cdex-task-read-response", "member-1", "", false},
		"a transaction type of no leg":                    {"no-such-leg", "x-response", wantPCI, wantPCI, false},
	} {
		t.Run(name, func(t *testing.T) {
			tok := signTestToken(shnsdk.Token{Operation: row.respOp, Scope: "s", Subject: row.subject, Frame: "rf", Holder: "payer",
				CorrelationID: "c", PayloadHash: "hash", Expiry: now.Add(time.Hour)}, priv)
			err := g.verifyAnswerToken(tok, row.tx, "rf", row.respOp, "c", "payer", row.requestPatient, "hash")
			if row.used && err != nil {
				t.Fatalf("refused: %v; want used", err)
			}
			if !row.used && err == nil {
				t.Fatal("used; want refused")
			}
		})
	}
}

// A request's token is held to the leg's row: some patient on a leg whose
// request names one, which this gateway binds afterwards, and none on a leg
// that names none or takes its patient from the answer.
func TestVerifyInboundToken_TheRequestFitsTheLegsRow(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	g := &Gateway{cfg: Config{AuthzPub: pub, Clock: func() time.Time { return now }}}
	for name, row := range map[string]struct {
		tx, subject string
		used        bool
	}{
		"a patient leg, a patient":             {"coverage-eligibility", wantPCI, true},
		"a patient leg, none":                  {"coverage-eligibility", "", false},
		"a patient leg, no patient identifier": {"coverage-eligibility", "member-1", false},
		"a no-patient leg, none":               {"pas-subscription-signal", "", true},
		"a no-patient leg, a patient":          {"pas-subscription-signal", wantPCI, false},
		"a from-answer leg, none":              {"cdex-task-status", "", true},
		"a from-answer leg, a patient":         {"cdex-task-status", wantPCI, false},
	} {
		t.Run(name, func(t *testing.T) {
			spec, ok := legSpecFor(row.tx)
			if !ok {
				t.Fatalf("no leg %s", row.tx)
			}
			tok := signTestToken(shnsdk.Token{Operation: spec.Op, Scope: "s", Subject: row.subject, Frame: spec.ReqFrame, Holder: "sender",
				CorrelationID: "c", PayloadHash: "hash", Expiry: now.Add(time.Hour)}, priv)
			err := g.verifyInboundToken(tok, spec, "c", "sender", "hash")
			if row.used && err != nil {
				t.Fatalf("refused: %v; want used", err)
			}
			if !row.used && err == nil {
				t.Fatal("used; want refused")
			}
		})
	}
}
