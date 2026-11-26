//go:build !shn_coreonly

package engine

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// An answer's involved list names the payer's own binding of the member, so
// its summary says the list is complete only once that binding is settled:
// it ran and its result is on the list (a differing patient named, or none
// needed). A binding that failed is stated as the member left out, by reason.
// One never attempted, or resolved but refused before its patient entered the
// list, sends no summary, which the Hub reads as unknown, and the call's
// record says why (InvolvedSummaryBindingUnsettled). These rows drive the
// payer gateway's real inbound handlers, at every conformance level.

// The rows run serially: each payer's responder (newLevelPayer) reads the
// package's shared fake clock (fixedClock), and a row runs in parallel only
// when it shares no store, clock or environment with another.

// summaryRig is a payer gateway whose Hub lists involved and involved-summary,
// and the exchange records it hands ExchangeObserved.
type summaryRig struct {
	*levelPayer
	records *exchangeRecords
}

// summaryPayer is a payer gateway at level whose Hub lists involved and
// involved-summary.
func summaryPayer(t *testing.T, level ConformanceEnforcement) summaryRig {
	t.Helper()
	return withSummaries(newLevelPayer(t, level))
}

// withSummaries has p's Hub list involved and involved-summary, and its
// Framework's tokens carry the involvement asked for, as the real one's do,
// and collects its exchange records.
func withSummaries(p *levelPayer) summaryRig {
	p.g.cfg.HubAcceptsInvolved, p.g.cfg.HubAcceptsInvolvedSummary = true, true
	p.g.cfg.Client.Transport.(*inboundAuthzStub).involvement = true
	var got exchangeRecords
	p.g.cfg.ExchangeObserved = got.observe
	return summaryRig{levelPayer: p, records: &got}
}

// wantNotSent checks the call's one exchange record says why no summary was
// sent: want, or "" for a call whose summary was.
func (p summaryRig) wantNotSent(t *testing.T, want string) {
	t.Helper()
	if got := p.records.only(t).InvolvedSummaryNotSent; got != want {
		t.Fatalf("the record's InvolvedSummaryNotSent is %q, want %q", got, want)
	}
}

// wantAnswerSummary checks the answer carries a summary for the answer side
// stating omitted, that its list holds listed entries, and that the call's
// record names no summary not sent.
func wantAnswerSummary(t *testing.T, p summaryRig, ans payerAnswer, listed int, omitted []shnsdk.InvolvedOmission) {
	t.Helper()
	if !ans.framed {
		t.Fatalf("answer %d %s: not a framed answer", ans.status, ans.body)
	}
	if got := len(involvedSubjects(t, ans.meta.Involved)); got != listed {
		t.Fatalf("listed %d, want %d", got, listed)
	}
	assertSummary(t, ans.meta, p.g.cfg.Identity.SignPriv.Public().(ed25519.PublicKey), shnsdk.InvolvedSideAnswer, omitted)
	p.wantNotSent(t, "")
}

// wantNoAnswerSummary checks the answer carries no summary, and that the
// call's record says why: reason.
func wantNoAnswerSummary(t *testing.T, p summaryRig, ans payerAnswer, reason string) {
	t.Helper()
	if !ans.framed {
		t.Fatalf("answer %d %s: not a framed answer", ans.status, ans.body)
	}
	if ans.meta.InvolvedSummary != "" {
		t.Fatalf("a summary was sent: %s", ans.meta.InvolvedSummary)
	}
	p.wantNotSent(t, reason)
}

// (a) A binding that failed is stated as one member left out under the
// reason its failure maps to; (c) one that ran and resolved, to the token's
// own patient or to no member held, states the list complete.
func TestInvolvedSummary_TheAnswerStatesItsBinding(t *testing.T) {
	unavailable := func(p *levelPayer) {
		p.g.cfg.SoR = failingSoR{SystemOfRecord: p.g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(p.g.cfg.SoR), err: &SoRReadError{Kind: SoRUnavailable}}
	}
	for _, row := range []struct {
		name    string
		setup   func(*levelPayer)
		body    []byte
		omitted []shnsdk.InvolvedOmission
	}{
		{"(a) the system of record cannot be read", unavailable, conformantCRD("MBR-COVERED", "72148"),
			[]shnsdk.InvolvedOmission{{Reason: involvedOmitUnreadable, Count: 1}}},
		{"(a) the member cannot be identified", nil, unidentifiableCRD(t),
			[]shnsdk.InvolvedOmission{{Reason: involvedOmitUnknown, Count: 1}}},
		{"(c) the token's own patient", nil, conformantCRD("MBR-COVERED", "72148"), noOmission},
		{"(c) no member held", func(p *levelPayer) { p.g.cfg.RequireKnownMembers = true }, conformantCRD("MBR-NOBODY", "72148"), noOmission},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := summaryPayer(t, level)
				if row.setup != nil {
					row.setup(p.levelPayer)
				}
				ans := p.send(t, "crd-order-select", "", row.body)
				wantAnswerSummary(t, p, ans, 0, row.omitted)
			})
		}
	}
}

// A summary the payer's gateway cannot sign is not sent, and its answer is
// carried as before, the call's record saying why (InvolvedSummaryUnsigned),
// at every conformance level.
func TestInvolvedSummary_AnUnsignedAnswerIsRecorded(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := summaryPayer(t, level)
			p.g.signInvolvedSummary = func(shnsdk.InvolvedSummary, ed25519.PrivateKey, shnsdk.Token) (string, error) {
				return "", errors.New("signing failed")
			}
			ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
			if ans.status != http.StatusOK {
				t.Fatalf("answered %d %s, want the answer carried", ans.status, ans.body)
			}
			wantNoAnswerSummary(t, p, ans, InvolvedSummaryUnsigned)
		})
	}
}

// partyOnlyCoverage is a Coverage carrying, as its subscriber, a parent
// Patient bearing member's number: the only Patient the request carries under
// that number is a party, so a payer that does not hold member cannot
// identify it (errPatientUnidentifiable).
func partyOnlyCoverage(member string) string {
	parent := `{"resourceType":"Patient","id":"parent","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + member + `"}]}`
	return `{"resourceType":"Coverage","id":"c1","contained":[` + parent + `],"subscriber":{"reference":"#parent"},"beneficiary":{"reference":"Patient/` + member + `"}}`
}

// unidentifiableCRD is an order-select request for a member the payer does
// not hold, carrying only a party under its number.
func unidentifiableCRD(t *testing.T) []byte {
	t.Helper()
	body := string(conformantCRD("eKID", "72148"))
	plain := `"coverage":{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"Patient/eKID"}}`
	if !strings.Contains(body, plain) {
		t.Fatal("fixture: no coverage to replace")
	}
	return []byte(strings.Replace(body, plain, `"coverage":`+partyOnlyCoverage("eKID"), 1))
}

// unidentifiableEligibility is a coverage-eligibility request for a member the
// payer does not hold, carrying only a party under its number.
func unidentifiableEligibility(t *testing.T) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(eligibilityRequest(t, "eKID"), &doc); err != nil {
		t.Fatal(err)
	}
	var cov map[string]any
	if err := json.Unmarshal([]byte(partyOnlyCoverage("eKID")), &cov); err != nil {
		t.Fatal(err)
	}
	doc["contained"] = []any{cov}
	return mustJSON(t, doc)
}

// (a) A request whose patients the binding cannot read at all is stated as
// its member left out, request-unreadable. Every leg's own readers refuse
// such a request before its binding reads it
// (TestBinding_AnUndecodableBodyIsRefusedFirstOnEveryLeg), so the binding is
// driven directly, as the answer is then built.
func TestInvolvedSummary_AnUnreadableRequestsBindingIsStated(t *testing.T) {
	g, _, _, _ := involvedGateway(t, true)
	g.cfg.HubAcceptsInvolvedSummary = true
	unreadable := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"eNEW","id":"eNEW"}}]}`)
	md := answerSideAfter(t, g, context.Background(), func(c *involvedCollector) {
		ctx := context.WithValue(context.Background(), involvedCollectorKey{}, c)
		if _, status, _ := g.bindInboundSubject(ctx, "eNEW", unreadable); status == 0 {
			t.Fatal("fixture: the binding read the request")
		}
	})
	assertSummary(t, md, g.cfg.Identity.SignPriv.Public().(ed25519.PublicKey), shnsdk.InvolvedSideAnswer,
		[]shnsdk.InvolvedOmission{{Reason: involvedOmitRequestUnreadable, Count: 1}})
	// A member that names no one is refused before anything is read: no
	// reason, so no summary.
	md = answerSideAfter(t, g, context.Background(), func(c *involvedCollector) {
		ctx := context.WithValue(context.Background(), involvedCollectorKey{}, c)
		if _, status, _ := g.bindInboundSubject(ctx, "Patient/", nil); status == 0 {
			t.Fatal("fixture: a member naming no one was bound")
		}
	})
	if md.InvolvedSummary != "" {
		t.Fatalf("a member naming no one sent a summary: %s", md.InvolvedSummary)
	}
}

// (b) A leg refused before its binding ran sends no summary, its record
// saying why; the same leg carried sends one, the control. At every
// conformance level.
func TestInvolvedSummary_ARefusalBeforeBindingSendsNone(t *testing.T) {
	for _, level := range allLevels {
		for name, body := range map[string][]byte{
			"no context.patientId":      crdDispatchWith(t, `"patientId":"MBR-COVERED",`, ``),
			"subject of the wrong type": crdDispatchWith(t, `"patientId":"MBR-COVERED"`, `"patientId":7`),
		} {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := summaryPayer(t, level)
				ans := p.send(t, "crd-order-dispatch", "", body)
				if ans.status == http.StatusOK {
					t.Fatalf("fixture: answered %d", ans.status)
				}
				wantNoAnswerSummary(t, p, ans, InvolvedSummaryBindingUnsettled)
			})
		}
		t.Run("the control/"+level.String(), func(t *testing.T) {
			p := summaryPayer(t, level)
			wantAnswerSummary(t, p, p.send(t, "crd-order-dispatch", "", []byte(crdDispatchRequest)), 0, noOmission)
		})
	}
}

// A leg whose binding resolved but that refused before its patient entered
// the list is not settled: no summary, at strict, where the refusal bites,
// its record saying why. Below strict (none, observe, structural) the same
// request is carried and states its list complete.
func TestInvolvedSummary_ARefusalAfterBindingBeforeTheListSendsNone(t *testing.T) {
	missing, related := updateBundle(t, "Provenance")
	another := crdDispatchWith(t, `"subject":{"reference":"Patient/MBR-COVERED"}`, `"subject":{"reference":"Patient/MBR-UC04"}`)
	for _, row := range []struct {
		name, leg string
		body      []byte
		seed      string
	}{
		{"pas-claim-update missing its Provenance", "pas-claim-update", missing, related},
		{"crd-order-dispatch for another patient", "crd-order-dispatch", another, ""},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := summaryPayer(t, level)
				if row.seed != "" {
					p.seedPend(t, row.seed)
				}
				ans := p.send(t, row.leg, "", row.body)
				if level == EnforcementStrict {
					if ans.status == http.StatusOK {
						t.Fatalf("fixture: strict answered %d %s", ans.status, ans.body)
					}
					wantNoAnswerSummary(t, p, ans, InvolvedSummaryBindingUnsettled)
					return
				}
				if ans.status != http.StatusOK {
					t.Fatalf("fixture: %s refused %d %s", level, ans.status, ans.body)
				}
				wantAnswerSummary(t, p, ans, 0, noOmission)
			})
		}
	}
}

// A forwarded eligibility batch whose later request's member cannot be bound
// states that member left out: the earlier requests' own bindings stay on
// the list, and the summary is never complete.
func TestInvolvedSummary_AnEligibilityBatchWithALaterFailingMember(t *testing.T) {
	for _, row := range []struct {
		name    string
		members []string
		listed  int
	}{
		{"the second request's member", []string{dtrFrameMember, ""}, 0},
		{"the third, after another patient", []string{dtrFrameMember, anotherHeldMember, ""}, 1},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := withSummaries(eligibilityPayer(t, level, true))
				var requests [][]byte
				for _, m := range row.members {
					if m == "" {
						requests = append(requests, unidentifiableEligibility(t))
						continue
					}
					requests = append(requests, eligibilityRequest(t, m))
				}
				ans := p.send(t, "coverage-eligibility", "", eligBatch(t, requests...))
				if ans.status == http.StatusOK || !strings.Contains(string(ans.body), "request") {
					t.Fatalf("fixture: answered %d %s", ans.status, ans.body)
				}
				wantAnswerSummary(t, p, ans, row.listed, []shnsdk.InvolvedOmission{{Reason: involvedOmitUnknown, Count: 1}})
			})
		}
	}
}

// memberFailingSoR is a system of record whose read of member fails with err,
// and that answers every other member as the one it wraps.
type memberFailingSoR struct {
	SystemOfRecord
	ContextSystemOfRecord
	member string
	err    error
}

func (s memberFailingSoR) ResolvePatientContext(ctx context.Context, member string) (string, Demo, bool, error) {
	if member == s.member {
		return "", Demo{}, false, s.err
	}
	return s.ContextSystemOfRecord.ResolvePatientContext(ctx, member)
}

// A forwarded eligibility batch refused at one request's member, with
// requests after it, never bound those requests' members: their patients may
// be missing from the answer's list, so it is open, no summary is sent, and
// the Hub reads the list as unknown, never as complete, whatever the failing
// member's own course (rulings a to c apply to the members bound). The record
// says why (InvolvedSummaryBindingUnsettled). For each way a member's binding
// fails, at the batch's start and in its middle, at every conformance level;
// at its end, the control, the failing member's own course is the summary's:
// a failed read stated under its reason beside the earlier members' bindings,
// a member not held stated complete.
func TestInvolvedSummary_AnEligibilityBatchStoppedBeforeItsLastMemberSendsNone(t *testing.T) {
	failingRead := func(member string, kind SoRFailureKind) func(*levelPayer) {
		return func(p *levelPayer) {
			withSoR(p, func(sor SystemOfRecord, ctx ContextSystemOfRecord) SystemOfRecord {
				return memberFailingSoR{SystemOfRecord: sor, ContextSystemOfRecord: ctx, member: member, err: &SoRReadError{Kind: kind}}
			})
		}
	}
	for _, kind := range []struct {
		name    string
		setup   func(*levelPayer)
		request func(*testing.T) []byte
		omitted []shnsdk.InvolvedOmission
	}{
		{"unknown-member: a patient the request cannot identify", nil, unidentifiableEligibility,
			[]shnsdk.InvolvedOmission{{Reason: involvedOmitUnknown, Count: 1}}},
		{"unknown-member: a member held with no identifier", func(p *levelPayer) {
			withSoR(p, func(sor SystemOfRecord, ctx ContextSystemOfRecord) SystemOfRecord {
				return heldWithoutIdentifierSoR{SystemOfRecord: sor, ContextSystemOfRecord: ctx, member: "MBR-NOID"}
			})
		}, func(t *testing.T) []byte { return eligibilityRequest(t, "MBR-NOID") },
			[]shnsdk.InvolvedOmission{{Reason: involvedOmitUnknown, Count: 1}}},
		{"not held, where known members are required", func(p *levelPayer) { p.g.cfg.RequireKnownMembers = true },
			func(t *testing.T) []byte { return eligibilityRequest(t, "MBR-NOT-HELD") }, noOmission},
		{"the system of record does not answer", failingRead("MBR-SOR-DOWN", SoRUnavailable),
			func(t *testing.T) []byte { return eligibilityRequest(t, "MBR-SOR-DOWN") },
			[]shnsdk.InvolvedOmission{{Reason: involvedOmitUnreadable, Count: 1}}},
		{"the system of record's answer cannot be read", failingRead("MBR-SOR-BAD", SoRInvalidResponse),
			func(t *testing.T) []byte { return eligibilityRequest(t, "MBR-SOR-BAD") },
			[]shnsdk.InvolvedOmission{{Reason: involvedOmitUnreadable, Count: 1}}},
	} {
		for _, at := range []struct {
			name string
			// where is the failing request's place among the batch's three.
			where int
		}{{"at the start", 0}, {"in the middle", 1}, {"at the end", 2}} {
			for _, level := range allLevels {
				t.Run(kind.name+"/"+at.name+"/"+level.String(), func(t *testing.T) {
					p := withSummaries(eligibilityPayer(t, level, true))
					if kind.setup != nil {
						kind.setup(p.levelPayer)
					}
					requests := [][]byte{eligibilityRequest(t, dtrFrameMember), eligibilityRequest(t, anotherHeldMember)}
					requests = slices.Insert(requests, at.where, kind.request(t))
					ans := p.send(t, "coverage-eligibility", "", eligBatch(t, requests...))
					if ans.status == http.StatusOK {
						t.Fatalf("fixture: answered %d %s", ans.status, ans.body)
					}
					if at.where < len(requests)-1 {
						wantNoAnswerSummary(t, p, ans, InvolvedSummaryBindingUnsettled)
						return
					}
					// The end: the leg's patient and another patient bound, the
					// other named on the list.
					wantAnswerSummary(t, p, ans, 1, kind.omitted)
				})
			}
		}
	}
}

// heldWithoutIdentifierSoR is a system of record that holds member but
// reports it with no identifier.
type heldWithoutIdentifierSoR struct {
	SystemOfRecord
	ContextSystemOfRecord
	member string
}

func (s heldWithoutIdentifierSoR) ResolvePatientContext(ctx context.Context, member string) (string, Demo, bool, error) {
	if member == s.member {
		return "", Demo{}, true, nil
	}
	return s.ContextSystemOfRecord.ResolvePatientContext(ctx, member)
}

// withSoR replaces p's system of record with wrap of it.
func withSoR(p *levelPayer, wrap func(SystemOfRecord, ContextSystemOfRecord) SystemOfRecord) {
	p.g.cfg.SoR = wrap(p.g.cfg.SoR, ReadSystemOfRecord(p.g.cfg.SoR))
}

// A member the system of record holds with no identifier is one the payer's
// gateway cannot identify: stated left out, unknown-member, once, as the
// request side's identifyNamed counts it, never complete. On order-select
// the binding refuses it; on coverage eligibility answered from the payer's
// records it is answered as before.
func TestInvolvedSummary_AMemberHeldWithoutAnIdentifierIsUnknown(t *testing.T) {
	held := func(sor SystemOfRecord, ctx ContextSystemOfRecord) SystemOfRecord {
		return heldWithoutIdentifierSoR{SystemOfRecord: sor, ContextSystemOfRecord: ctx, member: dtrFrameMember}
	}
	for _, row := range []struct {
		name, leg string
		payer     func(*testing.T, ConformanceEnforcement) summaryRig
		body      func(*testing.T) []byte
	}{
		{"order-select", "crd-order-select", summaryPayer,
			func(*testing.T) []byte { return conformantCRD(dtrFrameMember, "72148") }},
		{"coverage eligibility from the records", "coverage-eligibility", func(t *testing.T, level ConformanceEnforcement) summaryRig {
			return withSummaries(eligibilityPayer(t, level, false))
		}, func(t *testing.T) []byte { return eligibilityRequest(t, dtrFrameMember) }},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := row.payer(t, level)
				withSoR(p.levelPayer, held)
				ans := p.send(t, row.leg, "", row.body(t))
				wantAnswerSummary(t, p, ans, 0, []shnsdk.InvolvedOmission{{Reason: involvedOmitUnknown, Count: 1}})
			})
		}
	}
}

// Coverage eligibility answered from the payer's own records binds the
// member with one read of its system of record: a read that does not answer
// is stated as that member left out (system-of-record-unavailable, once); a
// member the system does not hold is refused, with no patient to name, so
// the list is stated complete; the member it holds, the token's own patient,
// is stated complete too.
func TestInvolvedSummary_EligibilityFromTheRecordsStatesItsBinding(t *testing.T) {
	for _, row := range []struct {
		name    string
		member  string
		sor     func(SystemOfRecord, ContextSystemOfRecord) SystemOfRecord
		ok      bool
		omitted []shnsdk.InvolvedOmission
	}{
		{"the system of record does not answer", dtrFrameMember, func(sor SystemOfRecord, ctx ContextSystemOfRecord) SystemOfRecord {
			return failingSoR{SystemOfRecord: sor, ContextSystemOfRecord: ctx, err: &SoRReadError{Kind: SoRUnavailable}}
		}, false, []shnsdk.InvolvedOmission{{Reason: involvedOmitUnreadable, Count: 1}}},
		{"a member not held", "MBR-NOBODY", nil, false, noOmission},
		{"the token's own patient", dtrFrameMember, nil, true, noOmission},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := withSummaries(eligibilityPayer(t, level, false))
				if row.sor != nil {
					withSoR(p.levelPayer, row.sor)
				}
				ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, row.member))
				if (ans.status == http.StatusOK) != row.ok {
					t.Fatalf("fixture: answered %d %s", ans.status, ans.body)
				}
				wantAnswerSummary(t, p, ans, 0, row.omitted)
			})
		}
	}
}
