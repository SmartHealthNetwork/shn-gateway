// nativepas_relay_test.go — the PAS submit and update legs relay the payer's own
// decision.
//
// Every row here asserts the same two things about a payer answer: the requester
// receives THE PAYER'S BYTES, byte for byte, and the payer gateway's own pend
// ledger follows the answer rather than deciding anything of its own. A pend, a
// re-pend, a denial and an approval are all the payer's message; none of them is
// a failure of this gateway, and none of them is replaced by a later answer to a
// different operation.
//
// The rows that pin what is GONE are as load-bearing as the rows that pin what is
// relayed: nothing polls (`GET /ClaimResponse/{id}` never fires), nothing is
// assembled, and a fresh submit carries no infoChanged stamp minted so that a
// payer gateway would poll.
package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const relayRequester = "holder:requesting-ehr"

// pasInfoChangedExtURL is the Da Vinci PAS Claim-item infoChanged extension. The
// ENGINE no longer reads it: it was the payer-side poll discriminator, and there
// is no poll. It survives here because these rows assert what is NOT stamped and
// what a non-conformant amendment looks like — a test fixture, not a code path.
const pasInfoChangedExtURL = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-infoChanged"

// payerCreated is the date the payer puts on every answer in this file. It is the
// ledger's timestamp-rule input, so it is fixed rather than "now".
var payerCreated = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// recordingPayer is a payer endpoint that answers its queued replies in order and
// records EVERY call it saw, method and path included. The path record is what
// makes "nothing polls" assertable: a `GET /ClaimResponse/{id}` shows up here or
// it did not happen.
type recordingPayer struct {
	mu      sync.Mutex
	calls   []payerCall
	answers []stubAnswer
	t       *testing.T
}

type payerCall struct {
	method string
	path   string
	body   []byte
}

// stubAnswer is one queued payer reply.
type stubAnswer struct {
	status int
	body   string
}

func newRecordingPayer(t *testing.T, answers ...stubAnswer) (*httptest.Server, *recordingPayer) {
	t.Helper()
	p := &recordingPayer{answers: answers, t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0)
		if r.Body != nil {
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(r.Body)
			body = buf.Bytes()
		}
		p.mu.Lock()
		i := len(p.calls)
		p.calls = append(p.calls, payerCall{method: r.Method, path: r.URL.Path, body: body})
		p.mu.Unlock()
		if i >= len(p.answers) {
			t.Errorf("payer call #%d (%s %s) past the queued %d answer(s)", i+1, r.Method, r.URL.Path, len(p.answers))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(p.answers[i].status)
		_, _ = w.Write([]byte(p.answers[i].body))
	}))
	t.Cleanup(srv.Close)
	return srv, p
}

func (p *recordingPayer) seen() []payerCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]payerCall(nil), p.calls...)
}

// refuseAnyPoll fails the test if the leg read a ClaimResponse behind the
// requester's back. The poll is deleted; this is the row that keeps it deleted.
func (p *recordingPayer) refuseAnyPoll(t *testing.T) {
	t.Helper()
	for _, c := range p.seen() {
		if c.method == http.MethodGet {
			t.Fatalf("the leg polled the payer (%s %s): a relayed decision is the payer's own answer to the operation it answered", c.method, c.path)
		}
	}
}

// pendedAnswer is a payer pend carrying the identifiers a later inquiry names the
// authorization by, and the date the ledger's timestamp rule reads.
func relayPendAnswer(t *testing.T, id, trace string) []byte {
	t.Helper()
	cr := `{"resourceType":"ClaimResponse","id":"` + id + `","status":"active","outcome":"queued",` +
		`"created":"` + payerCreated.Format(time.RFC3339) + `",` +
		`"identifier":[{"system":"urn:payer:response","value":"resp-` + id + `"}],` +
		`"item":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber",` +
		`"valueIdentifier":{"system":"urn:payer:trace","value":"` + trace + `"}}],"itemSequence":1}]}`
	return fixturePASResponse(t, []byte(cr), true)
}

// decidedAnswer is a terminal payer decision with the same identifiers.
func relayDecidedAnswer(t *testing.T, id, trace, preAuthRef string) []byte {
	t.Helper()
	cr := `{"resourceType":"ClaimResponse","id":"` + id + `","status":"active","outcome":"complete",` +
		`"created":"` + payerCreated.Format(time.RFC3339) + `","preAuthRef":"` + preAuthRef + `",` +
		`"preAuthPeriod":{"end":"2030-01-01"},` +
		`"identifier":[{"system":"urn:payer:response","value":"resp-` + id + `"}],` +
		`"item":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber",` +
		`"valueIdentifier":{"system":"urn:payer:trace","value":"` + trace + `"}}],"itemSequence":1}]}`
	return fixturePASResponse(t, []byte(cr), true)
}

// deviceRequestSubmitBundle is the HomeOxygen DME single-shot submit: a
// DeviceRequest order, the lane whose order type alone used to route it to the poll.
func deviceRequestSubmitBundle(t *testing.T) []byte {
	t.Helper()
	dr := []byte(`{"resourceType":"DeviceRequest","id":"dr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-COVERED"},"codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0424","display":"Stationary compressed gaseous oxygen system"}]}}`)
	b, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		Coverage: testMemberCoverage("MBR-COVERED"), Provider: testRequestingProvider(),
		MemberIDSystem: shnsdk.MemberSystem, SR: dr,
		PatientRef: "Patient/MBR-COVERED", CoverageRef: "Coverage/MBR-COVERED", MemberID: "MBR-COVERED",
		Corr: "corr-dr-submit", Created: fixedClock(), Payer: shnsdk.CMSPayerIdentity,
	})
	if err != nil {
		t.Fatalf("deviceRequestSubmitBundle: %v", err)
	}
	return b
}

// relayResponder wires a native responder at the payer with the requester holder
// the engine verified, so the ledger keys land in that requester's namespace.
func relayResponder(t *testing.T, srv *httptest.Server, store Store) (*nativeResponder, context.Context) {
	t.Helper()
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", store, fixedClock)
	ctx, _ := withPASLeg(context.Background(), relayRequester)
	return n, ctx
}

// relayedExactly asserts the requester received the payer's bytes unchanged and
// that the answer is owned as the payer's message, not as one this gateway wrote.
func relayedExactly(t *testing.T, res LegResult, want []byte) {
	t.Helper()
	if !bytes.Equal(responseBytes(res), want) {
		t.Fatalf("the payer's bytes were not relayed unchanged\n got: %s\nwant: %s", responseBytes(res), want)
	}
	if !res.ResponseRelayed() {
		t.Fatal("the answer is not owned as the payer's own message")
	}
}

// ledgerState reads the ledger row for one authorization.
func ledgerState(t *testing.T, store Store, pci, corr string) PendRecord {
	t.Helper()
	ledger, ok := LedgerOf(store)
	if !ok {
		t.Fatal("the fixture store has no pend ledger")
	}
	rec, found, err := ledger.PendRecordOf(pci, corr)
	if err != nil || !found {
		t.Fatalf("no ledger row for %s/%s (found=%v err=%v)", pci, corr, found, err)
	}
	return rec
}

// ---- submit ----

// TestNativeSubmit_PendRelayedForDeviceRequest: the DME single-shot lane. The
// payer pended; the requester receives that pend, and the authorization is
// recorded under the keys the payer's own answer states.
func TestNativeSubmit_PendRelayedForDeviceRequest(t *testing.T) {
	pend := relayPendAnswer(t, "cr-dme", "trace-dme")
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(pend)})
	store := newCensusSoR()
	n, ctx := relayResponder(t, srv, store)
	res, err := n.Handle(ctx, "pas-claim", "corr-dme", "PCI-1", deviceRequestSubmitBundle(t))
	if err != nil || res.Status != 0 {
		t.Fatalf("submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, pend)
	if pended, _, perr := shnsdk.ParsePendedResponse(responseBytes(res)); perr != nil || !pended {
		t.Fatalf("a DeviceRequest single-shot must surface the payer's PEND (pended=%v err=%v)", pended, perr)
	}
	payer.refuseAnyPoll(t)
	if res.Commit == nil {
		t.Fatal("a pend must be recorded so a follow-up can resolve it")
	}
	if err := res.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := ledgerState(t, store, "PCI-1", "corr-dme").State; got != PendStatePended {
		t.Fatalf("ledger state = %q, want pended", got)
	}
	ledger, _ := LedgerOf(store)
	_, corr, found, ambiguous, lerr := ledger.LookupPended(relayRequester, PendKeys{
		RequesterHolder: relayRequester, PreAuthRef: "", ItemTraceNumbers: []string{"urn:payer:trace|trace-dme"}})
	if lerr != nil || ambiguous || !found || corr != "corr-dme" {
		t.Fatalf("the pend is not findable by the trace number the payer echoed (found=%v ambiguous=%v corr=%q err=%v)", found, ambiguous, corr, lerr)
	}
}

// TestNativeSubmit_PendRelayedForInfoChanged: the other lane that used to poll —
// a ServiceRequest submit whose Claim item carries the PAS infoChanged extension.
// A requester may still send one; it is the payer's input, and it is no longer a
// signal to this gateway to go and fetch a different answer.
func TestNativeSubmit_PendRelayedForInfoChanged(t *testing.T) {
	pend := relayPendAnswer(t, "cr-ic", "trace-ic")
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(pend)})
	store := newCensusSoR()
	n, ctx := relayResponder(t, srv, store)
	res, err := n.Handle(ctx, "pas-claim", "corr-ic", "PCI-1", serviceRequestSubmitBundle(t, true))
	if err != nil || res.Status != 0 {
		t.Fatalf("submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, pend)
	payer.refuseAnyPoll(t)
	if pended, _, perr := shnsdk.ParsePendedResponse(responseBytes(res)); perr != nil || !pended {
		t.Fatalf("an infoChanged submit must surface the payer's PEND (pended=%v err=%v)", pended, perr)
	}
	if res.Commit == nil {
		t.Fatal("a pend must be recorded so a follow-up can resolve it")
	}
	if err := res.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := ledgerState(t, store, "PCI-1", "corr-ic").State; got != PendStatePended {
		t.Fatalf("ledger state = %q, want pended", got)
	}
}

// TestNativeSubmit_PendWithUnusableTransmissionIdentifiersRelayed drives the case
// that the reference payer stopped REACHING rather than stopped having: an answer
// carrying `extension-TransmissionIdentifiers` whose `applicationSenderCode` has
// no value, which violates FHIR `ext-1`.
//
// While this leg assembled its own answer from those bytes and certified the
// assembly as SHN's own, that shape was refused with `422 PAS assembly validation
// failed` and the requester saw `502 hub routing failed`. A relayed decision is
// the payer's message and is never certified as ours, so it reaches the requester
// exactly as the payer wrote it. The row exists so this does not depend on a
// particular payer build's incidental behaviour.
func TestNativeSubmit_PendWithUnusableTransmissionIdentifiersRelayed(t *testing.T) {
	const emptySender = `{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers","extension":[{"url":"applicationSenderCode"},{"url":"applicationReceiverCode","valueString":"8189991234"}]}`
	cr := `{"resourceType":"ClaimResponse","id":"cr-ext1","status":"active","outcome":"queued",` +
		`"created":"` + payerCreated.Format(time.RFC3339) + `","extension":[` + emptySender + `],` +
		`"identifier":[{"system":"urn:payer:response","value":"resp-cr-ext1"}]}`
	answer := fixturePASResponse(t, []byte(cr), true)
	if !bytes.Contains(answer, []byte("applicationSenderCode")) {
		t.Fatal("the fixture lost the extension this row is about")
	}
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(answer)})
	store := newCensusSoR()
	n, ctx := relayResponder(t, srv, store)
	res, err := n.Handle(ctx, "pas-claim", "corr-ext1", "PCI-1", deviceRequestSubmitBundle(t))
	if err != nil || res.Status != 0 {
		t.Fatalf("a payer answer this gateway did not write is not validated as its own: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, answer)
	payer.refuseAnyPoll(t)
}

// TestNativeSubmit_DenialRelayed: the payer denied at submit. The denial reaches
// the requester unchanged, the ledger records the decision, and the decision EOB
// is written in the same write.
func TestNativeSubmit_DenialRelayed(t *testing.T) {
	denied := fixturePASResponse(t, loadDeniedClaimResponseBytes(t), true)
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(denied)})
	store := newCensusSoR()
	n, ctx := relayResponder(t, srv, store)
	res, err := n.Handle(ctx, "pas-claim", "corr-deny", "PCI-1", serviceRequestSubmitBundle(t, false))
	if err != nil || res.Status != 0 {
		t.Fatalf("submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, denied)
	payer.refuseAnyPoll(t)
	if len(res.SideEffectFHIR) != 1 {
		t.Fatalf("a denial states the payer's own decision on one EOB; got %d side-effects", len(res.SideEffectFHIR))
	}
	if res.Commit == nil {
		t.Fatal("a decision must be recorded")
	}
	if err := res.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rec := ledgerState(t, store, "PCI-1", "corr-deny")
	if rec.State != PendStateDecided || rec.Outcome != PendOutcomeDenied {
		t.Fatalf("ledger = %q/%q, want decided/denied", rec.State, rec.Outcome)
	}
	if _, ok := store.EOBByID("eob-corr-deny"); !ok {
		t.Fatal("the decision EOB was not written with the decision")
	}
}

// TestNativeSubmit_ApprovalRelayed: an approval at submit is relayed exactly and
// decides the authorization.
func TestNativeSubmit_ApprovalRelayed(t *testing.T) {
	approved := relayDecidedAnswer(t, "cr-ok", "trace-ok", "AUTH-OK-1")
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(approved)})
	store := newCensusSoR()
	n, ctx := relayResponder(t, srv, store)
	res, err := n.Handle(ctx, "pas-claim", "corr-ok", "PCI-1", serviceRequestSubmitBundle(t, false))
	if err != nil || res.Status != 0 {
		t.Fatalf("submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, approved)
	payer.refuseAnyPoll(t)
	if res.Commit == nil {
		t.Fatal("a decision must be recorded")
	}
	if err := res.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rec := ledgerState(t, store, "PCI-1", "corr-ok")
	if rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved {
		t.Fatalf("ledger = %q/%q, want decided/approved", rec.State, rec.Outcome)
	}
	if !rec.DecidedAt.Equal(payerCreated) {
		t.Fatalf("the ledger dated the decision %v, want the payer's own %v", rec.DecidedAt, payerCreated)
	}
}

// TestNoInfoChangedInjectedOnFreshSubmit: the originator's single-shot tail no
// longer stamps the Da Vinci PAS infoChanged extension on a FRESH submit. That
// stamp existed only to make a payer gateway poll; a submit that changes nothing
// must not claim information changed.
func TestNoInfoChangedInjectedOnFreshSubmit(t *testing.T) {
	sr := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]}}`)
	dr := []byte(`{"resourceType":"DeviceRequest","id":"dr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-COVERED"},"codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0424"}]}}`)
	for _, row := range []struct {
		name  string
		order []byte
	}{{"ServiceRequest single-shot", sr}, {"DeviceRequest single-shot", dr}} {
		for _, brPayer := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s brPayer=%v", row.name, brPayer), func(t *testing.T) {
				built, err := buildPASSubmitBundle("2.0", brPayer, row.order, nil,
					testRequestingProvider(), testMemberCoverage("MBR-COVERED"), testPayerOrganization(shnsdk.CMSPayerIdentity),
					shnsdk.MemberSystem, "Patient/MBR-COVERED", "Coverage/MBR-COVERED", "MBR-COVERED",
					"corr-fresh", fixedClock(), shnsdk.CMSPayerIdentity)
				if err != nil {
					t.Fatalf("build: %v", err)
				}
				if bytes.Contains(built, []byte(pasInfoChangedExtURL)) {
					t.Fatalf("a fresh submit carries an infoChanged stamp it has no information change to state:\n%s", built)
				}
			})
		}
	}
}

// ---- update ----

// updateRelayLeg seeds a pended authorization for the conformant update golden's
// prior claim and returns the leg under test.
func updateRelayLeg(t *testing.T, srv *httptest.Server) (*nativeResponder, context.Context, *censusSoR, []byte, string, string) {
	t.Helper()
	bundle := originatorBuiltConformantUpdateBundleProfile(t, true)
	const origCorr = "convergence-pas-submit-0001"
	const pci = "PCI-CONF-UPD"
	store := newCensusSoR()
	if _, err := store.RecordPendedKeyed(pci, origCorr, payerCreated.Add(-time.Hour), PendKeys{
		RequesterHolder: relayRequester, RequestIDs: []string{"urn:shn:corr|" + origCorr}}); err != nil {
		t.Fatalf("seed the pend: %v", err)
	}
	n, ctx := relayResponder(t, srv, store)
	return n, ctx, store, bundle, pci, origCorr
}

// TestNativeUpdate_RePendRelayedAndReleased: the payer re-pends an amendment that
// asked for re-evaluation. That re-pend is the payer's answer — it is relayed,
// and the authorization returns to pended so a later amendment can still bind.
func TestNativeUpdate_RePendRelayedAndReleased(t *testing.T) {
	repend := relayPendAnswer(t, "cr-rp", "trace-rp")
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(repend)})
	n, ctx, store, bundle, pci, origCorr := updateRelayLeg(t, srv)
	res, err := n.Handle(ctx, "pas-claim-update", "corr-upd", pci, bundle)
	if err != nil || res.Status != 0 {
		t.Fatalf("a payer re-pend is an answer, not a refusal: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, repend)
	payer.refuseAnyPoll(t)
	if res.Commit == nil {
		t.Fatal("a re-pend must release the claim and refresh its keys")
	}
	if err := res.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := ledgerState(t, store, pci, origCorr).State; got != PendStatePended {
		t.Fatalf("ledger state after a re-pend = %q, want pended", got)
	}
	ledger, _ := LedgerOf(store)
	_, corr, found, _, lerr := ledger.LookupPended(relayRequester, PendKeys{
		RequesterHolder: relayRequester, ItemTraceNumbers: []string{"urn:payer:trace|trace-rp"}})
	if lerr != nil || !found || corr != origCorr {
		t.Fatalf("the re-pend's own identifiers were not added to the authorization (found=%v corr=%q err=%v)", found, corr, lerr)
	}
}

// TestNativeUpdate_CarryForwardRePendRelayed: an amendment that asks for no
// re-evaluation is re-pended by the payer too, and this gateway used to replace
// that answer with its own 422 "amendment still insufficient". The payer's word
// is the answer.
func TestNativeUpdate_CarryForwardRePendRelayed(t *testing.T) {
	repend := relayPendAnswer(t, "cr-cf", "trace-cf")
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(repend)})
	n, ctx, store, bundle, pci, origCorr := updateRelayLeg(t, srv)
	res, err := n.Handle(ctx, "pas-claim-update", "corr-cf", pci, stripInfoChangedExtension(t, bundle))
	if err != nil || res.Status != 0 {
		t.Fatalf("a carry-forward re-pend is the payer's answer: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, repend)
	payer.refuseAnyPoll(t)
	if res.Commit != nil {
		if err := res.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	if got := ledgerState(t, store, pci, origCorr).State; got != PendStatePended {
		t.Fatalf("ledger state after a carry-forward re-pend = %q, want pended", got)
	}
}

// TestNativeUpdate_DenialRelayedAndDecided: a terminal denial on the update leg
// reaches the requester and decides the authorization, so it is never re-pended.
func TestNativeUpdate_DenialRelayedAndDecided(t *testing.T) {
	denied := fixturePASResponse(t, loadDeniedClaimResponseBytes(t), true)
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(denied)})
	n, ctx, store, bundle, pci, origCorr := updateRelayLeg(t, srv)
	res, err := n.Handle(ctx, "pas-claim-update", "corr-upd-deny", pci, bundle)
	if err != nil || res.Status != 0 {
		t.Fatalf("a payer denial is an answer, not this gateway's 422: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, denied)
	payer.refuseAnyPoll(t)
	if res.Commit == nil {
		t.Fatal("a decision must be recorded")
	}
	if err := res.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rec := ledgerState(t, store, pci, origCorr)
	if rec.State != PendStateDecided || rec.Outcome != PendOutcomeDenied {
		t.Fatalf("ledger = %q/%q, want decided/denied", rec.State, rec.Outcome)
	}
	if len(res.SideEffectFHIR) != 0 {
		t.Fatalf("the update leg builds no EOB; got %d side-effects", len(res.SideEffectFHIR))
	}
}

// TestNativeUpdate_ApprovalRelayedAndDecided: an approval is relayed exactly and
// decides the authorization.
func TestNativeUpdate_ApprovalRelayedAndDecided(t *testing.T) {
	approved := relayDecidedAnswer(t, "cr-upd-ok", "trace-upd-ok", "AUTH-UPD-1")
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(approved)})
	n, ctx, store, bundle, pci, origCorr := updateRelayLeg(t, srv)
	res, err := n.Handle(ctx, "pas-claim-update", "corr-upd-ok", pci, bundle)
	if err != nil || res.Status != 0 {
		t.Fatalf("update: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, approved)
	payer.refuseAnyPoll(t)
	if res.Commit == nil || res.Rollback == nil {
		t.Fatal("a decided update commits the decision and keeps Rollback armed")
	}
	if err := res.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rec := ledgerState(t, store, pci, origCorr)
	if rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved {
		t.Fatalf("ledger = %q/%q, want decided/approved", rec.State, rec.Outcome)
	}
}

// payerVersionConflict is the reference payer's answer when an amendment's $submit
// lands while its own pend-resolution timer is writing the same ClaimResponse
// (observed live 2026-09-11): HAPI's ResourceVersionConflictException relayed as
// an OperationOutcome.
const payerVersionConflict = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"HAPI-0550: HAPI-0989: Trying to update ClaimResponse/5039/_history/2 but this is not the current version"}]}`

// TestNativeUpdate_PayerVersionConflictRelayed: the payer's own 409 — its store
// refusing the amendment's write — is the payer's answer. It is relayed on the
// first answer, and the amendment is not sent again: whether and when to resend is
// the requester's decision. The claim is released so that resend can bind.
func TestNativeUpdate_PayerVersionConflictRelayed(t *testing.T) {
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusConflict, payerVersionConflict})
	n, ctx, store, bundle, pci, origCorr := updateRelayLeg(t, srv)
	res, err := n.Handle(ctx, "pas-claim-update", "corr-conflict", pci, bundle)
	if err != nil {
		t.Fatalf("the payer's conflict is an answer, not an error: %v", err)
	}
	if res.Status != http.StatusConflict || string(responseBytes(res)) != payerVersionConflict {
		t.Fatalf("the payer's 409 must be relayed exactly: status=%d body=%s", res.Status, responseBytes(res))
	}
	if len(payer.seen()) != 1 {
		t.Fatalf("posts = %d, want 1: the gateway does not resend on the requester's behalf", len(payer.seen()))
	}
	if res.Rollback == nil {
		t.Fatal("a relayed non-2xx after Begin must release the claim")
	}
	res.Rollback()
	if got := ledgerState(t, store, pci, origCorr).State; got != PendStatePended {
		t.Fatalf("ledger state after the relayed conflict = %q, want pended", got)
	}
}

// TestNativeUpdate_DecidedClaimReachesThePayer: an amendment of an authorization
// this gateway's ledger has as decided still reaches the payer, which decides
// what an amendment of its own decision means. Its answer is relayed exactly.
func TestNativeUpdate_DecidedClaimReachesThePayer(t *testing.T) {
	answer := relayDecidedAnswer(t, "cr-decided", "trace-decided", "AUTH-DECIDED-2")
	srv, payer := newRecordingPayer(t, stubAnswer{http.StatusOK, string(answer)})
	n, ctx, store, bundle, pci, origCorr := updateRelayLeg(t, srv)
	if _, err := store.RecordDecision(pci, origCorr, PendOutcomeApproved, payerCreated, nil); err != nil {
		t.Fatalf("decide the claim: %v", err)
	}
	res, err := n.Handle(ctx, "pas-claim-update", "corr-decided", pci, bundle)
	if err != nil || res.Status != 0 {
		t.Fatalf("an amendment of a decided claim reaches the payer: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, answer)
	if len(payer.seen()) != 1 {
		t.Fatalf("posts = %d, want 1", len(payer.seen()))
	}
}

// TestNativeUpdate_MalformedUpstream502: a 2xx this gateway cannot read as a PAS
// answer is an upstream problem, refused with 502, and the claim is released.
func TestNativeUpdate_MalformedUpstream502(t *testing.T) {
	srv, _ := newRecordingPayer(t, stubAnswer{http.StatusOK, `{"resourceType":"Bundle","type":"collection","entry":[]}`})
	n, ctx, _, bundle, pci, _ := updateRelayLeg(t, srv)
	res, err := n.Handle(ctx, "pas-claim-update", "corr-bad", pci, bundle)
	if err != nil {
		t.Fatalf("an unreadable 2xx is a refusal, not an error: %v", err)
	}
	if res.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.Status)
	}
	if res.Rollback == nil {
		t.Fatal("a post-Begin refusal must release the claim")
	}
}

// TestNativeUpdate_Transport500: a payer that cannot be reached at all is a fault,
// and the claim is still released.
func TestNativeUpdate_Transport500(t *testing.T) {
	srv, _ := newRecordingPayer(t)
	n, ctx, _, bundle, pci, _ := updateRelayLeg(t, srv)
	n = NewNativeResponder(&http.Client{}, "http://127.0.0.1:1", "shn-order-select", n.store, fixedClock)
	res, err := n.Handle(ctx, "pas-claim-update", "corr-dead", pci, bundle)
	if err == nil {
		t.Fatal("a no-response fault must surface as an error return")
	}
	if res.Rollback == nil {
		t.Fatal("a post-Begin fault must release the claim")
	}
}
