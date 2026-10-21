package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// One patient-match rule on every leg: a gateway refuses only when the
// network cannot identify the patient it must check consent and authority
// for. Eligibility identifies the member from the payer's own records and
// checks no consent, so a token naming another patient is carried at every
// level and the payer's binding is recorded. A federated query's consent was
// checked for the token's patient, and a patient-authored DTR item is signed
// as the patient the leg's authority names, so on those legs a difference is
// refused at every level, and a member the recipient does not hold is refused
// with the same bytes, so the refusal does not say which it was.

// anotherHeldMember is a member the test system of record holds other than
// dtrFrameMember (whose identifier is levelPayer.pci).
const anotherHeldMember = "MBR-NOTCOVERED"

// observedRun is one leg's answer with the binding-differs events and the
// involved tokens the payer gateway asked for.
type observedRun struct {
	answer     payerAnswer
	differs    int
	authorized []string
}

// runObserved sends body on leg under a token for subject, with the Hub
// reading the involved list, recording what the gateway observed and asked
// the Authorization Framework for.
func runObserved(t *testing.T, p *levelPayer, leg string, body []byte, subject string) observedRun {
	t.Helper()
	p.g.cfg.HubAcceptsInvolved = true
	var run observedRun
	var mu sync.Mutex
	observe := p.g.cfg.Observer
	p.g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == SubjectBindingDiffersEvent && e.LegType == leg {
			mu.Lock()
			run.differs++
			mu.Unlock()
		}
		observe(e)
	}
	p.g.cfg.Client = &http.Client{Transport: authorizeRecorder{inner: p.g.cfg.Client.Transport, mu: &mu, got: &run.authorized}}
	run.answer = p.sendAs(t, leg, "", body, subject)
	mu.Lock()
	defer mu.Unlock()
	run.authorized = append([]string(nil), run.authorized...)
	return run
}

// A token naming another patient on an eligibility request the payer answers
// from its records is carried at every level: the answer is about the
// request's own member, the difference is observed, and the payer's own
// binding is named on the answer's involved list (payer-held).
func TestEligibility_RecordsTokenSubjectDifferenceIsCarried(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			other, _, ok := p.store.ResolvePatient(anotherHeldMember)
			if !ok {
				t.Fatalf("%s is not in the test system of record", anotherHeldMember)
			}
			run := runObserved(t, p, "coverage-eligibility", eligibilityRequest(t, dtrFrameMember), other)
			if run.answer.status != http.StatusOK || !bytes.Contains(run.answer.body, []byte(`"CoverageEligibilityResponse"`)) {
				t.Fatalf("answer %d %s, want the payer's answer carried", run.answer.status, run.answer.body)
			}
			ref, err := ParseCoverageEligibilityResponsePatient(run.answer.body)
			if err != nil || ref != "Patient/"+dtrFrameMember {
				t.Fatalf("the answer is about %q (%v), want the request's own member", ref, err)
			}
			if run.differs != 1 {
				t.Fatalf("%s raised %d times, want once", SubjectBindingDiffersEvent, run.differs)
			}
			if !containsSuffix(run.authorized, p.pci+"|"+shnsdk.InvolvementPayerHeld) {
				t.Fatalf("authorized %v, want the payer's own binding named payer-held on the involved list", run.authorized)
			}
		})
	}
}

// The forward path names the payer's binding on the involved list too.
func TestEligibility_DeclaredTokenSubjectDifferenceIsRecorded(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
			p.partner.respByPath[eligibilityPath] = answer
			run := runObserved(t, p, "coverage-eligibility", eligibilityRequest(t, dtrFrameMember), "pci-of-another-patient")
			if run.answer.status != http.StatusOK || !bytes.Equal(run.answer.body, answer) {
				t.Fatalf("answer %d %s, want the payer's answer relayed", run.answer.status, run.answer.body)
			}
			if run.differs != 1 || !containsSuffix(run.authorized, p.pci+"|"+shnsdk.InvolvementPayerHeld) {
				t.Fatalf("differs %d, authorized %v, want the payer's binding observed and named", run.differs, run.authorized)
			}
		})
	}
}

// The same member under its own token names nobody else: the control.
func TestEligibility_RecordsSameSubjectNamesNoOneElse(t *testing.T) {
	p := eligibilityPayer(t, EnforcementStrict, false)
	run := runObserved(t, p, "coverage-eligibility", eligibilityRequest(t, dtrFrameMember), p.pci)
	if run.answer.status != http.StatusOK || run.differs != 0 || containsSuffix(run.authorized, shnsdk.InvolvementPayerHeld) {
		t.Fatalf("answer %d, differs %d, authorized %v", run.answer.status, run.differs, run.authorized)
	}
}

// A member the payer's records do not hold is the payer's own answer, at
// every level: 400 unknown member, recorded under no network rule (other).
func TestEligibility_RecordsUnknownMemberIsThePayersAnswer(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			ans := p.sendAs(t, "coverage-eligibility", "", eligibilityRequest(t, "MBR-NOT-HELD"), shnsdk.ResolvePCI("MBR-NOT-HELD", "", ""))
			if ans.status != http.StatusBadRequest || strings.TrimSpace(string(ans.body)) != `{"error":"unknown member"}` {
				t.Fatalf("answer %d %s, want the payer's 400 unknown member", ans.status, ans.body)
			}
			if rec := got.only(t); rec.Rule != ExchangeOther {
				t.Fatalf("record names rule %q: the payer's own answer is not a network refusal", rec.Rule)
			}
		})
	}
}

// sendConsented is levelPayer.sendAs for a bare request carrying consentRef
// in its envelope and token, as a federated query does.
func sendConsented(t *testing.T, p *levelPayer, leg string, body []byte, subject, consentRef string) payerAnswer {
	t.Helper()
	p.seq++
	spec, _ := legSpecFor(leg)
	corr := fmt.Sprintf("corr-%s-%d", leg, p.seq)
	clock := p.g.cfg.Clock()
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: p.requester.ID, Recipient: p.g.cfg.HolderID, TransactionType: leg, ConsentRef: consentRef,
		AuthorityFrame: spec.ReqFrame, Timestamp: clock.Format(time.RFC3339), CorrelationID: corr,
	}, body, p.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatal(err)
	}
	tok := signTestToken(shnsdk.Token{
		Operation: spec.Op, Subject: subject, Frame: spec.ReqFrame, Holder: p.requester.ID, ConsentRef: consentRef,
		CorrelationID: corr, Expiry: clock.Add(time.Hour), PayloadHash: sha256hexT(env.Ciphertext),
	}, p.authzPriv)
	tb, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	env.Metadata.AuthzToken = string(tb)
	raw, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	as, err := json.Marshal(shnsdk.IssueAssertion("hub", p.g.cfg.HolderID, p.hubPriv, clock, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", bytes.NewReader(raw))
	r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(as))
	rec := httptest.NewRecorder()
	p.g.inboundRoute()(rec, r.WithContext(context.Background()))
	if rec.Code != http.StatusOK {
		return payerAnswer{status: rec.Code, body: rec.Body.Bytes(), corr: corr}
	}
	hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, p.requester, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response frame: %v", err)
	}
	return payerAnswer{status: hdr.Status, body: answer, framed: true, corr: corr}
}

func cdexTaskFor(t *testing.T, member string) []byte {
	t.Helper()
	b, err := shnsdk.BuildCDexTaskDataRequest("Patient/"+member, "DiagnosticReport", "2024-01-01", "2026-12-31",
		shnsdk.CDexTaskMeta{AuthoredOn: fixedClock(), Requester: "provider", Owner: "payer"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func patientDTRFor(t *testing.T, member string) []byte {
	t.Helper()
	return mustJSON(t, map[string]string{"linkId": "functional-status-oswestry", "answer": "42", "patientRef": "Patient/" + member})
}

var bindLevels = []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict}

// A federated query for one patient under a token for another is refused at
// every level, recorded as consent: the consent the network checked is not
// the queried patient's. A queried member the facility does not hold is
// refused with exactly the same answer, so the refusal does not tell the
// requester whether the facility holds the patient.
func TestFederatedQuery_PatientNotTheConsentedOneIsRefused(t *testing.T) {
	for _, level := range bindLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			// A consent service that permits, and a count of the reads that
			// fetch the member's records for disclosure.
			p.g.cfg.ConsentURL = "http://consent.test"
			p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
			reads := &recordReadCounter{SystemOfRecord: p.g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(p.g.cfg.SoR)}
			p.g.cfg.SoR = reads
			// The control: the token's own patient passes the bind and the
			// consent check, and the facility reads the member's records.
			control := sendConsented(t, p, "federated-query", cdexTaskFor(t, dtrFrameMember), p.pci, "Consent/c-1")
			if bytes.Contains(control.body, []byte(refusalPatientNotConsented)) || reads.n.Load() == 0 {
				t.Fatalf("control answer %d %s after %d record reads, want the bind passed and the records read", control.status, control.body, reads.n.Load())
			}
			reads.n.Store(0)
			got.reset()
			other, _, _ := p.store.ResolvePatient(anotherHeldMember)
			held := sendConsented(t, p, "federated-query", cdexTaskFor(t, dtrFrameMember), other, "Consent/c-1")
			wantRefusal(t, got.only(t), held.status, RefusedByPayerGateway, RefusalConsent)
			if held.status != http.StatusForbidden || !bytes.Contains(held.body, []byte(refusalPatientNotConsented)) {
				t.Fatalf("held answer %d %s", held.status, held.body)
			}
			got.reset()
			notHeld := sendConsented(t, p, "federated-query", cdexTaskFor(t, "MBR-NOT-HELD"), other, "Consent/c-1")
			wantRefusal(t, got.only(t), notHeld.status, RefusedByPayerGateway, RefusalConsent)
			if notHeld.status != held.status || notHeld.framed != held.framed || !bytes.Equal(notHeld.body, held.body) {
				t.Fatalf("not held answered %d %s, held %d %s: the refusal tells them apart", notHeld.status, notHeld.body, held.status, held.body)
			}
			if n := reads.n.Load(); n != 0 {
				t.Fatalf("the facility read records %d times for a refused query", n)
			}
		})
	}
}

// consentPermits answers the consent service's check with a permit and
// delegates every other call.
type consentPermits struct{ inner http.RoundTripper }

func (c consentPermits) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "consent.test" {
		return c.inner.RoundTrip(r)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Request: r,
		Body: io.NopCloser(strings.NewReader(`{"permit":true,"consentRef":"Consent/c-1"}`))}, nil
}

// recordReadCounter counts the reads that fetch a member's records for
// disclosure: the member's Patient reference, a record by reference, and a
// connector's records.
type recordReadCounter struct {
	SystemOfRecord
	ContextSystemOfRecord
	n atomic.Int32
}

func (c *recordReadCounter) PatientFHIRRefContext(ctx context.Context, m string) (string, bool, error) {
	c.n.Add(1)
	return c.ContextSystemOfRecord.PatientFHIRRefContext(ctx, m)
}

func (c *recordReadCounter) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	c.n.Add(1)
	return c.ContextSystemOfRecord.ResolveByReferenceContext(ctx, ref)
}

func (c *recordReadCounter) FacilityRecordsContext(ctx context.Context, m string) (map[string][]byte, bool, error) {
	c.n.Add(1)
	return c.ContextSystemOfRecord.FacilityRecordsContext(ctx, m)
}

// A patient-authored DTR item for one patient under a token for another is
// refused at every level, recorded as authority, and nothing is signed. A
// patient the PHG does not hold is refused with exactly the same answer.
func TestPatientDTR_PatientNotTheAuthorizedOneIsRefused(t *testing.T) {
	for _, level := range bindLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			control := p.sendAs(t, "patient-dtr", "", patientDTRFor(t, dtrFrameMember), p.pci)
			if control.status != http.StatusOK || !bytes.Contains(control.body, []byte("attestedItem")) {
				t.Fatalf("control answer %d %s, want the attested item", control.status, control.body)
			}
			got.reset()
			other, _, _ := p.store.ResolvePatient(anotherHeldMember)
			held := p.sendAs(t, "patient-dtr", "", patientDTRFor(t, dtrFrameMember), other)
			wantRefusal(t, got.only(t), held.status, RefusedByPayerGateway, RefusalAuthority)
			if held.status != http.StatusForbidden || !bytes.Contains(held.body, []byte(refusalPatientNotAuthorized)) || bytes.Contains(held.body, []byte("attestedItem")) {
				t.Fatalf("held answer %d %s", held.status, held.body)
			}
			got.reset()
			notHeld := p.sendAs(t, "patient-dtr", "", patientDTRFor(t, "MBR-NOT-HELD"), other)
			wantRefusal(t, got.only(t), notHeld.status, RefusedByPayerGateway, RefusalAuthority)
			if notHeld.status != held.status || notHeld.framed != held.framed || !bytes.Equal(notHeld.body, held.body) {
				t.Fatalf("not held answered %d %s, held %d %s: the refusal tells them apart", notHeld.status, notHeld.body, held.status, held.body)
			}
		})
	}
}

// A patient-authored DTR request naming no patient is refused as unreadable.
func TestPatientDTR_NoPatientIsRefused(t *testing.T) {
	p := newLevelPayer(t, EnforcementNone)
	ans := p.sendAs(t, "patient-dtr", "", []byte(`{"linkId":"functional-status-oswestry","answer":"42"}`), p.pci)
	if ans.status != http.StatusBadRequest || !bytes.Contains(ans.body, []byte(refusalPatientDTRNoPatient)) {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
}

// reset drops the records collected so far.
func (c *exchangeRecords) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = nil
}

// ambiguousMemberSoR answers a read of one member as a system of record whose
// records for that member are out of shape (two Patients for the member, or
// one without the facts the binding reads) would, and fails every read with
// down when it is set.
type ambiguousMemberSoR struct {
	SystemOfRecord
	ContextSystemOfRecord
	member string
	down   bool
}

func (a ambiguousMemberSoR) ResolvePatientContext(ctx context.Context, m string) (string, Demo, bool, error) {
	if a.down {
		return "", Demo{}, false, &SoRReadError{Kind: SoRUnavailable}
	}
	if m == a.member {
		return "", Demo{}, false, &SoRReadError{Kind: SoRInvalidResponse}
	}
	return a.ContextSystemOfRecord.ResolvePatientContext(ctx, m)
}

// A member whose records the recipient's system answers out of shape is
// answered exactly as a member it does not hold, so the answer does not say
// the recipient holds records for that patient; the recipient records its
// own system's failure (an upstream error, malformed). A system that is
// down for every member answers so, as before: that says nothing about any
// one patient.
func TestPatientMatch_UnreadableMemberRecordsAnswerAsNotHeld(t *testing.T) {
	const ambiguous = "MBR-AMBIGUOUS"
	for _, leg := range []struct {
		name string
		body func(*testing.T, string) []byte
		send func(*testing.T, *levelPayer, []byte, string) payerAnswer
	}{
		{"federated-query", cdexTaskFor, func(t *testing.T, p *levelPayer, b []byte, s string) payerAnswer {
			return sendConsented(t, p, "federated-query", b, s, "Consent/c-1")
		}},
		{"patient-dtr", patientDTRFor, func(t *testing.T, p *levelPayer, b []byte, s string) payerAnswer {
			return p.sendAs(t, "patient-dtr", "", b, s)
		}},
	} {
		for _, level := range bindLevels {
			t.Run(leg.name+"/"+level.String(), func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				p.g.cfg.SoR = ambiguousMemberSoR{SystemOfRecord: p.g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(p.g.cfg.SoR), member: ambiguous}
				notHeld := leg.send(t, p, leg.body(t, "MBR-NOT-HELD"), p.pci)
				got.reset()
				unreadable := leg.send(t, p, leg.body(t, ambiguous), p.pci)
				if unreadable.status != notHeld.status || unreadable.framed != notHeld.framed || !bytes.Equal(unreadable.body, notHeld.body) {
					t.Fatalf("unreadable records answered %d %s, a member not held %d %s: the answer tells them apart", unreadable.status, unreadable.body, notHeld.status, notHeld.body)
				}
				rec := got.only(t)
				if rec.Outcome != ExchangeUpstreamError || rec.Backend == nil || rec.Backend.ErrorClass != BackendMalformed {
					t.Fatalf("record %s backend %+v, want the system's malformed answer recorded", rec.Outcome, rec.Backend)
				}

				p.g.cfg.SoR = ambiguousMemberSoR{SystemOfRecord: p.g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(p.g.cfg.SoR), down: true}
				down := leg.send(t, p, leg.body(t, dtrFrameMember), p.pci)
				if down.status != http.StatusServiceUnavailable {
					t.Fatalf("a system down for every member answered %d %s, want 503", down.status, down.body)
				}
			})
		}
	}
}
