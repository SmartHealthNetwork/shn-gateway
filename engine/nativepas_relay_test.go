// nativepas_relay_test.go — the PAS submit and update legs relay the payer's own
// reply, whether it is a decision, a pend, or an application refusal.
//
// Every row here asserts that the requester receives THE PAYER'S BYTES, status,
// and media type. Native delivery neither reads nor advances a clinical ledger;
// explicit ledger actions have independent tests below.
//
// The rows that pin what is GONE are as load-bearing as the rows that pin what is
// relayed: nothing polls (`GET /ClaimResponse/{id}` never fires), nothing is
// assembled, and a fresh submit carries no infoChanged stamp minted so that a
// payer gateway would poll.
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// payerCreated is the synthetic date the payer puts on every answer in this file.
var payerCreated = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// recordingPayer is a payer endpoint that answers its queued replies in order and
// records EVERY call it saw, method and path included. The path record is what
// makes "nothing polls" assertable: a `GET /ClaimResponse/{id}` shows up here or
// it did not happen.
type recordingPayer struct {
	mu      sync.Mutex
	calls   []payerCall
	answers []conflictAnswer
	t       *testing.T
}

type payerCall struct {
	method string
	path   string
	body   []byte
}

func newRecordingPayer(t *testing.T, answers ...conflictAnswer) (*httptest.Server, *recordingPayer) {
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

// This synthetic complete answer has no explicit review action. It remains an
// ambiguous local-consumption fixture even though native relay carries it intact.
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
	b, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{ItemFacts: syntheticPASItemFacts(),
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
// payer pended; the requester receives those exact bytes without a local
// ledger write or a follow-up poll.
func TestNativeSubmit_PendRelayedForDeviceRequest(t *testing.T) {
	pend := relayPendAnswer(t, "cr-dme", "trace-dme")
	srv, payer := newRecordingPayer(t, conflictAnswer{http.StatusOK, string(pend)})
	store := nativeClinicalStoreForbidden{}
	n, ctx := relayResponder(t, srv, store)
	request := deviceRequestSubmitBundle(t)
	res, err := n.Handle(ctx, "pas-claim", "corr-dme", "PCI-1", request)
	if err != nil || res.Status != 0 {
		t.Fatalf("submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, pend)
	if pended, _, perr := shnsdk.ParsePendedResponse(responseBytes(res)); perr != nil || !pended {
		t.Fatalf("a DeviceRequest single-shot must surface the payer's PEND (pended=%v err=%v)", pended, perr)
	}
	payer.refuseAnyPoll(t)
	assertNativeRelayWithoutClinicalEffects(t, res, pend, http.StatusOK, "application/fhir+json")
	calls := payer.seen()
	if len(calls) != 1 || !bytes.Equal(calls[0].body, request) {
		t.Fatalf("native requests=%+v", calls)
	}
}

// TestNativeSubmit_PendRelayedForInfoChanged: the other lane that used to poll —
// a ServiceRequest submit whose Claim item carries the PAS infoChanged extension.
// A requester may still send one; it is the payer's input, and it is no longer a
// signal to this gateway to go and fetch a different answer.
func TestNativeSubmit_PendRelayedForInfoChanged(t *testing.T) {
	pend := relayPendAnswer(t, "cr-ic", "trace-ic")
	srv, payer := newRecordingPayer(t, conflictAnswer{http.StatusOK, string(pend)})
	store := nativeClinicalStoreForbidden{}
	n, ctx := relayResponder(t, srv, store)
	request := serviceRequestSubmitBundle(t, true)
	res, err := n.Handle(ctx, "pas-claim", "corr-ic", "PCI-1", request)
	if err != nil || res.Status != 0 {
		t.Fatalf("submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, pend)
	payer.refuseAnyPoll(t)
	if pended, _, perr := shnsdk.ParsePendedResponse(responseBytes(res)); perr != nil || !pended {
		t.Fatalf("an infoChanged submit must surface the payer's PEND (pended=%v err=%v)", pended, perr)
	}
	assertNativeRelayWithoutClinicalEffects(t, res, pend, http.StatusOK, "application/fhir+json")
	calls := payer.seen()
	if len(calls) != 1 || !bytes.Equal(calls[0].body, request) {
		t.Fatalf("native requests=%+v", calls)
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
	srv, payer := newRecordingPayer(t, conflictAnswer{http.StatusOK, string(answer)})
	store := newCensusSoR()
	n, ctx := relayResponder(t, srv, store)
	res, err := n.Handle(ctx, "pas-claim", "corr-ext1", "PCI-1", deviceRequestSubmitBundle(t))
	if err != nil || res.Status != 0 {
		t.Fatalf("a payer answer this gateway did not write is not validated as its own: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, answer)
	payer.refuseAnyPoll(t)
}

// TestNativeSubmit_DenialRelayed: the payer response reaches the requester
// unchanged without implicit clinical writes.
func TestNativeSubmit_DenialRelayed(t *testing.T) {
	denied := fixturePASResponse(t, loadDeniedClaimResponseBytes(t), true)
	srv, payer := newRecordingPayer(t, conflictAnswer{http.StatusOK, string(denied)})
	store := nativeClinicalStoreForbidden{}
	n, ctx := relayResponder(t, srv, store)
	request := serviceRequestSubmitBundle(t, false)
	res, err := n.Handle(ctx, "pas-claim", "corr-deny", "PCI-1", request)
	if err != nil || res.Status != 0 {
		t.Fatalf("submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, denied)
	payer.refuseAnyPoll(t)
	assertNativeRelayWithoutClinicalEffects(t, res, denied, http.StatusOK, "application/fhir+json")
	calls := payer.seen()
	if len(calls) != 1 || !bytes.Equal(calls[0].body, request) {
		t.Fatalf("native requests=%+v", calls)
	}
}

// TestNativeSubmit_ApprovalRelayed: the response is relayed exactly without
// interpreting or recording a local clinical decision.
func TestNativeSubmit_ApprovalRelayed(t *testing.T) {
	approved := relayDecidedAnswer(t, "cr-ok", "trace-ok", "AUTH-OK-1")
	srv, payer := newRecordingPayer(t, conflictAnswer{http.StatusOK, string(approved)})
	store := nativeClinicalStoreForbidden{}
	n, ctx := relayResponder(t, srv, store)
	request := serviceRequestSubmitBundle(t, false)
	res, err := n.Handle(ctx, "pas-claim", "corr-ok", "PCI-1", request)
	if err != nil || res.Status != 0 {
		t.Fatalf("submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
	}
	relayedExactly(t, res, approved)
	payer.refuseAnyPoll(t)
	assertNativeRelayWithoutClinicalEffects(t, res, approved, http.StatusOK, "application/fhir+json")
	calls := payer.seen()
	if len(calls) != 1 || !bytes.Equal(calls[0].body, request) {
		t.Fatalf("native requests=%+v", calls)
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

// ---- update: PCV-07/08/09 native delivery and explicit local state ----

const updatePriorCorr = "convergence-pas-submit-0001"
const updatePCI = "PCI-CONF-UPD"

// A seeded authorization is deliberately present to detect an implicit mutation.
// The separate poison-store row proves that a native update does not read one.
func updateRelayLeg(t *testing.T, srv *httptest.Server) (*nativeResponder, context.Context, *censusSoR, []byte, string, string) {
	t.Helper()
	bundle := originatorBuiltConformantUpdateBundleProfile(t, true)
	store := newCensusSoR()
	if _, err := store.RecordPendedKeyed(updatePCI, updatePriorCorr, payerCreated.Add(-time.Hour), PendKeys{
		RequesterHolder: relayRequester, RequestIDs: []string{"urn:shn:corr|" + updatePriorCorr}}); err != nil {
		t.Fatal(err)
	}
	n, ctx := relayResponder(t, srv, store)
	return n, ctx, store, bundle, updatePCI, updatePriorCorr
}

func assertUpdateRelay(t *testing.T, n *nativeResponder, ctx context.Context, store *censusSoR,
	payer *recordingPayer, corr, pci, prior string, request, answer []byte, status int) {
	t.Helper()
	t.Logf("fixture sha256 request=%x answer=%x", sha256.Sum256(request), sha256.Sum256(answer))
	before := ledgerState(t, store, pci, prior)
	res, err := n.Handle(ctx, "pas-claim-update", corr, pci, request)
	if err != nil {
		t.Fatalf("native update: %v", err)
	}
	if res.ApplicationStatus != status || res.Response.ContentType() != "application/fhir+json" || !bytes.Equal(responseBytes(res), answer) {
		t.Fatalf("peer reply changed: status=%d application=%d media=%q body=%q", res.Status, res.ApplicationStatus, res.Response.ContentType(), responseBytes(res))
	}
	if res.Status != 0 && res.Status != status {
		t.Fatalf("unexpected local status %d for application status %d", res.Status, status)
	}
	if res.Commit != nil || res.Rollback != nil || len(res.SideEffectFHIR) != 0 {
		t.Fatal("native relay returned clinical callbacks or side effects")
	}
	if !res.ResponseRelayed() {
		t.Fatal("peer reply ownership lost")
	}
	calls := payer.seen()
	if len(calls) != 1 || calls[0].method != http.MethodPost || calls[0].path != "/Claim/$submit" || !bytes.Equal(calls[0].body, request) {
		t.Fatalf("native update dispatched other than exact one POST: %+v", calls)
	}
	payer.refuseAnyPoll(t)
	if after := ledgerState(t, store, pci, prior); !reflect.DeepEqual(after, before) {
		t.Fatalf("native delivery changed ledger row: before=%+v after=%+v", before, after)
	}
}

func TestNativeUpdate_RePendRelayedWithoutLocalTransition(t *testing.T) {
	repend := relayPendAnswer(t, "cr-rp", "trace-rp")
	srv, payer := newRecordingPayer(t, conflictAnswer{http.StatusOK, string(repend)})
	n, ctx, store, bundle, pci, prior := updateRelayLeg(t, srv)
	assertUpdateRelay(t, n, ctx, store, payer, "corr-upd", pci, prior, bundle, repend, http.StatusOK)
	assertTraceAbsent(t, store, "trace-rp")
}

func TestNativeUpdate_CarryForwardRePendRelayed(t *testing.T) {
	repend := relayPendAnswer(t, "cr-cf", "trace-cf")
	srv, payer := newRecordingPayer(t, conflictAnswer{http.StatusOK, string(repend)})
	n, ctx, store, bundle, pci, prior := updateRelayLeg(t, srv)
	carryForward := stripInfoChangedExtension(t, bundle)
	if bytes.Equal(carryForward, bundle) {
		t.Fatal("carry-forward fixture retained infoChanged")
	}
	assertUpdateRelay(t, n, ctx, store, payer, "corr-cf", pci, prior, carryForward, repend, http.StatusOK)
	assertTraceAbsent(t, store, "trace-cf")
}

func TestNativeUpdate_DenialRelayedWithoutLocalDecision(t *testing.T) {
	denied := fixturePASResponse(t, loadDeniedClaimResponseBytes(t), true)
	srv, payer := newRecordingPayer(t, conflictAnswer{http.StatusOK, string(denied)})
	n, ctx, store, bundle, pci, prior := updateRelayLeg(t, srv)
	assertUpdateRelay(t, n, ctx, store, payer, "corr-upd-deny", pci, prior, bundle, denied, http.StatusOK)
}

func TestNativeUpdate_AmbiguousCompleteRelayedWithoutLocalDecision(t *testing.T) {
	// This exact fixture has no explicit A1. Its clinical meaning remains ambiguous.
	approved := relayDecidedAnswer(t, "cr-upd-ok", "trace-upd-ok", "AUTH-UPD-1")
	srv, payer := newRecordingPayer(t, conflictAnswer{http.StatusOK, string(approved)})
	n, ctx, store, bundle, pci, prior := updateRelayLeg(t, srv)
	assertUpdateRelay(t, n, ctx, store, payer, "corr-upd-ok", pci, prior, bundle, approved, http.StatusOK)
	if parsed, err := shnsdk.ParseClaimResponse(approved); err == nil || parsed.Outcome == "approved" {
		t.Fatalf("ambiguous original fixture was silently consumed as approval: parsed=%+v err=%v", parsed, err)
	}
}

func TestNativeUpdate_VersionConflictReturnsFirst409(t *testing.T) {
	approved := relayDecidedAnswer(t, "cr-retry", "trace-retry", "AUTH-RETRY-1")
	srv, payer := newRecordingPayer(t,
		conflictAnswer{http.StatusConflict, hapiVersionConflict},
		conflictAnswer{http.StatusOK, string(approved)})
	n, ctx, store, bundle, pci, prior := updateRelayLeg(t, srv)
	assertUpdateRelay(t, n, ctx, store, payer, "corr-retry", pci, prior, bundle, []byte(hapiVersionConflict), http.StatusConflict)
	// The queued approval is never delivered by this invocation.
}

func TestNativeUpdate_VersionConflictDoesNotDispatchQueuedRePend(t *testing.T) {
	repend := relayPendAnswer(t, "cr-retry-rp", "trace-retry-rp")
	srv, payer := newRecordingPayer(t,
		conflictAnswer{http.StatusConflict, hapiVersionConflict},
		conflictAnswer{http.StatusOK, string(repend)})
	n, ctx, store, bundle, pci, prior := updateRelayLeg(t, srv)
	assertUpdateRelay(t, n, ctx, store, payer, "corr-retry-rp", pci, prior, bundle, []byte(hapiVersionConflict), http.StatusConflict)
	assertTraceAbsent(t, store, "trace-retry-rp")
}

func TestNativeUpdate_DecidedClaim409(t *testing.T) {
	bundle := originatorBuiltConformantUpdateBundleProfile(t, true)
	for _, row := range []struct {
		name   string
		status int
		answer []byte
	}{
		{"peer409", http.StatusConflict, []byte(`{"resourceType":"OperationOutcome","issue":[{"code":"processing","diagnostics":"claim already decided"}]}`)},
		{"peer-success", http.StatusOK, relayDecidedAnswer(t, "cr-decided", "trace-decided", "AUTH-DECIDED")},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv, payer := newRecordingPayer(t, conflictAnswer{row.status, string(row.answer)})
			n, ctx, store, _, pci, prior := updateRelayLeg(t, srv)
			if _, err := store.RecordDecision(pci, prior, PendOutcomeApproved, payerCreated, nil); err != nil {
				t.Fatal(err)
			}
			assertUpdateRelay(t, n, ctx, store, payer, "corr-decided", pci, prior, bundle, row.answer, row.status)
		})
	}
}

func TestNativeUpdate_MalformedSuccessRelayedAtAdapter(t *testing.T) {
	malformed := []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	srv, payer := newRecordingPayer(t, conflictAnswer{http.StatusOK, string(malformed)})
	n, ctx, store, bundle, pci, prior := updateRelayLeg(t, srv)
	assertUpdateRelay(t, n, ctx, store, payer, "corr-bad", pci, prior, bundle, malformed, http.StatusOK)
}

func TestNativeUpdate_TransportFaultHasNoReplyOrLocalWork(t *testing.T) {
	bundle := originatorBuiltConformantUpdateBundleProfile(t, true)
	n := NewNativeResponder(&http.Client{}, "http://127.0.0.1:1", "shn-order-select", nativeClinicalStoreForbidden{}, fixedClock)
	res, err := n.Handle(context.Background(), "pas-claim-update", "corr-dead", updatePCI, bundle)
	if err == nil {
		t.Fatal("no-response transport failure must return an error")
	}
	if res.Status != 0 || res.ApplicationStatus != 0 || res.Commit != nil || res.Rollback != nil || len(res.SideEffectFHIR) != 0 || len(responseBytes(res)) != 0 {
		t.Fatalf("transport failure had clinical effect or fabricated application reply: %+v", res)
	}
}

func assertTraceAbsent(t *testing.T, store *censusSoR, trace string) {
	t.Helper()
	_, _, found, _, err := store.LookupPended(relayRequester, PendKeys{ItemTraceNumbers: []string{"urn:payer:trace|" + trace}})
	if err != nil || found {
		t.Fatalf("native relay indexed response trace %q: found=%v err=%v", trace, found, err)
	}
}
