package engine

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// producingResponder answers with the participant's bytes as this gateway's
// own (a produced answer, not a relay), keeping everything else the native
// responder derived: its decision, its decision EOB and its Commit.
type producingResponder struct{ inner LegResponder }

func (p producingResponder) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	res, err := p.inner.Handle(ctx, leg, corrID, subjectPCI, requestFHIR)
	if err == nil && res.Status == 0 {
		res.Response = relay.ForTest(relay.BytesForTest(res.Response), res.Response.ContentType())
	}
	return res, err
}

func eobFindings(p *levelPayer) []ConformanceFinding {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ConformanceFinding
	for _, f := range p.findings {
		if f.Kind == string(KindFHIREgress) {
			out = append(out, f)
		}
	}
	return out
}

// TestDecisionEOBGate_InvalidNeverWritten: at every level that checks it, a
// decision EOB that fails its own validation is never written, whether the
// payer's answer was relayed or produced by this gateway; the decision is
// recorded without it, the payer's answer reaches the requester, and the
// operator is told why. At none nothing is checked.
func TestDecisionEOBGate_InvalidNeverWritten(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	for _, answer := range []string{"relayed", "produced"} {
		for _, level := range allLevels {
			t.Run(answer+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				if answer == "produced" {
					p.g.cfg.Responder = producingResponder{inner: p.g.cfg.Responder}
				}
				p.g.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: `"ExplanationOfBenefit"`,
					RejectIssue: &shnsdk.Issue{Severity: "error", Code: "structure", MessageID: "Extension_EXTP_Context_Wrong"}}
				p.partner.respByPath[pasSubmitPath] = approved
				got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
				if got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
					t.Fatalf("the payer's decision must reach the requester exactly: %d %s", got.status, got.body)
				}
				rec, found := p.pendOf(t, got.corr)
				if !found || rec.State != PendStateDecided {
					t.Fatalf("the decision must be recorded: found=%v %+v", found, rec)
				}
				if level == EnforcementNone {
					if p.eobCount() != 1 {
						t.Fatalf("at none nothing is checked and the EOB is written: %d", p.eobCount())
					}
					return
				}
				if p.eobCount() != 0 {
					t.Fatalf("an EOB that failed its own validation was written (%s answer)", answer)
				}
				if f := eobFindings(p); len(f) != 1 || f[0].Decision != "relayed" {
					t.Fatalf("want one relayed EOB finding, got %+v", f)
				}
				if len(p.skipped) != 0 {
					t.Fatalf("the decision is read and recorded, not skipped: %+v", p.skipped)
				}
				p.wantSideRecord(t, "pas-claim", DecisionEOBNotWrittenEvent,
					"decision EOB not written: it fails its own validation (structure Extension_EXTP_Context_Wrong)")
			})
		}
	}
}

// TestDecisionEOBGate_OutageAtStrict: the validator is down for everything at
// strict. A relayed decision is relayed and recorded, its EOB is not written,
// and the check that could not run is recorded as a finding as well as an
// observer event. In the same row, strict's check of the participant's own
// produced payload behaves as it always has: with no validator it refuses.
func TestDecisionEOBGate_OutageAtStrict(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	down := validatorFunc(func([]byte) (shnsdk.Result, error) { return shnsdk.Result{}, context.DeadlineExceeded })

	p := newLevelPayer(t, EnforcementStrict)
	p.g.cfg.Validator = down
	p.partner.respByPath[pasSubmitPath] = approved
	got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
	if got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
		t.Fatalf("the payer's decision must be relayed exactly: %d %s", got.status, got.body)
	}
	if rec, found := p.pendOf(t, got.corr); !found || rec.State != PendStateDecided || p.eobCount() != 0 {
		t.Fatalf("the decision is recorded with no EOB: found=%v %+v, %d EOB(s)", found, rec, p.eobCount())
	}
	p.wantSideRecord(t, "pas-claim", DecisionEOBNotWrittenEvent, "decision EOB not written: its validation could not run (validator unavailable)")
	if f := eobFindings(p); len(f) != 1 || f[0].Verdict != "unavailable" || f[0].Decision != "relayed" || f[0].Level != "strict" {
		t.Fatalf("want one unavailable EOB finding at strict, got %+v", f)
	}

	// The participant's own payload, produced here, at strict during the
	// same outage: refused exactly as before this change.
	q := newLevelPayer(t, EnforcementStrict)
	q.g.cfg.Responder = producingResponder{inner: q.g.cfg.Responder}
	q.g.cfg.Validator = down
	q.partner.respByPath[pasSubmitPath] = approved
	got = q.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
	if got.status != http.StatusInternalServerError || !bytes.Contains(got.body, []byte("validator unavailable")) {
		t.Fatalf("strict's payload check during an outage must refuse as before: %d %s", got.status, got.body)
	}
	if _, found := q.pendOf(t, got.corr); found || q.eobCount() != 0 {
		t.Fatalf("a refused answer writes nothing: found=%v %d EOB(s)", found, q.eobCount())
	}
}

// TestDecisionEOBGate_InquiryEachDecisionOnItsOwn: on an inquiry leg that
// records two decisions, one decision's EOB failing its own validation
// withholds that EOB only: both decisions are recorded, the other keeps its
// EOB, and the one withheld is named by its authorization.
func TestDecisionEOBGate_InquiryEachDecisionOnItsOwn(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}})
	f.pend(t, corrClaimC, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimC}})
	f.g.cfg.ConformanceEnforcement = EnforcementStrict
	f.g.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: `"AUTH-B"`}
	var events []ObserverEvent
	f.g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	eobB := &EOBRecord{SubjectPCI: f.subject, EOBID: decisionEOBID(corrClaimB), JSON: []byte(`{"resourceType":"ExplanationOfBenefit","preAuthRef":["AUTH-B"]}`)}
	eobC := &EOBRecord{SubjectPCI: f.subject, EOBID: decisionEOBID(corrClaimC), JSON: []byte(`{"resourceType":"ExplanationOfBenefit","preAuthRef":["AUTH-C"]}`)}
	result := LegResult{SideEffectFHIR: [][]byte{eobB.JSON, eobC.JSON}, decisionEOBs: []*EOBRecord{eobB, eobC}}
	read := &payerAnswerRead{g: f.g, ctx: context.Background(), relayed: true}
	f.g.gateDecisionEOBs(context.Background(), "pas-claim-inquire", "corr-inquiry-leg", &result, read, true)
	if !read.read() {
		t.Fatal("one EOB that may not be written must not keep the answer's decisions from being recorded")
	}
	keys := PendKeys{RequesterHolder: inquiryRequester}
	for corr, eob := range map[string]*EOBRecord{corrClaimB: eobB, corrClaimC: eobC} {
		if err := recordPASDecision(context.Background(), f.store, f.subject, corr, PendOutcomeApproved, fixedClock(), keys, eob)(); err != nil {
			t.Fatal(err)
		}
	}
	wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeApproved)
	wantRecordedDecision(t, f.stateOf(t, corrClaimC), PendOutcomeApproved)
	if _, ok := f.store.EOBByID(decisionEOBID(corrClaimB)); ok {
		t.Fatal("claim B's invalid EOB was written")
	}
	if _, ok := f.store.EOBByID(decisionEOBID(corrClaimC)); !ok {
		t.Fatal("claim C lost its EOB to claim B's")
	}
	var named bool
	for _, e := range events {
		named = named || (e.Kind == DecisionEOBNotWrittenEvent && strings.HasPrefix(e.Detail, "authorization "+corrClaimB+": decision EOB not written: it fails its own validation"))
	}
	if !named {
		t.Fatalf("want claim B's EOB named as not written: %+v", events)
	}
}

// TestDecisionEOBWritesGoThroughTheGate is the static guard on the gate: in
// this package's product code, every decision write passes its EOB through
// eobToWrite (which yields only an EOB gateDecisionEOBs admitted), and a
// decision EOB is written on its own only by recordAdmittedEOB. A new write
// path that hands a Store an EOB directly fails here.
func TestDecisionEOBWritesGoThroughTheGate(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "RecordDecision":
					seen++
					last, ok := call.Args[len(call.Args)-1].(*ast.CallExpr)
					if id, isID := func() (*ast.Ident, bool) {
						if !ok {
							return nil, false
						}
						i, k := last.Fun.(*ast.Ident)
						return i, k
					}(); !isID || id.Name != "eobToWrite" {
						t.Errorf("%s: %s calls RecordDecision with an EOB not passed through eobToWrite", fset.Position(call.Pos()), fn.Name.Name)
					}
				case "RecordEOB":
					seen++
					if fn.Name.Name != "recordAdmittedEOB" {
						t.Errorf("%s: %s writes a decision EOB with RecordEOB; only recordAdmittedEOB may", fset.Position(call.Pos()), fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if seen < 3 {
		t.Fatalf("the guard saw %d decision writes; it no longer reads the write paths", seen)
	}
}

// TestPASInquire_DenialWithAuthNumberRecordsNothing: as on the submit leg, an
// inquiry answer whose decision reads as a denial yet states preAuthRef in its
// ClaimResponse's own field is relayed and records nothing, and the operator
// is told why: these bytes may be a partial certification whose authorization
// a denial reading would discard.
func TestPASInquire_DenialWithAuthNumberRecordsNothing(t *testing.T) {
	for _, code := range []string{"A2", "A3"} {
		t.Run(code, func(t *testing.T) {
			f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{inquiryTraceKey}})
			answer := inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-P", traces: []string{inquiryTraceKey}, code: code})
			if parsed, err := shnsdk.ParseClaimResponse(firstClaimResponse(answer)); err != nil || parsed.Outcome != "denied" {
				t.Fatalf("fixture: want a decision read as a denial, got %+v %v", parsed, err)
			}
			events := f.applyEvents(t, inquiryRequester, answer)
			if got := f.state(t).State; got != PendStatePended {
				t.Fatalf("a denial stating preAuthRef was recorded: %q", got)
			}
			if _, ok := f.store.EOBByID(decisionEOBID(f.corr)); ok {
				t.Fatal("an EOB was written")
			}
			var named bool
			for _, e := range events {
				named = named || (e.Kind == pendDecisionNotRecordedEvent && e.Detail == "authorization "+f.corr+": "+decisionReadAsDenialWithAuthNumber)
			}
			if !named {
				t.Fatalf("want %s naming the authorization: %+v", pendDecisionNotRecordedEvent, events)
			}
		})
	}
}
