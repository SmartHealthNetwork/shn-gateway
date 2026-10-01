package engine

// nosor_test.go — a payer-native gateway that keeps no system of record
// (NoSystemOfRecord). It holds no one: every member a request names is
// bound by the carry path (resolveSubjectBinding: the member id and the Patient
// the request carries), exactly as a store that holds nobody binds it, and the
// payer files its records under that binding, never under the token's subject
// (payer_subject_test.go). Eligibility answered from the payer's own
// records is not offered; the eligibility answer fence reads "not held", never
// a system-of-record failure.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// heldNobodySoR is a system of record that is there and readable but holds no
// one: every read answers not found, without error. A store-less payer answers
// every leg exactly as a gateway over this store does.
type heldNobodySoR struct{}

func (heldNobodySoR) ResolvePatient(string) (string, Demo, bool)       { return "", Demo{}, false }
func (heldNobodySoR) PatientFHIRRef(string) (string, bool)             { return "", false }
func (heldNobodySoR) CoverageInforce(string) (bool, string)            { return false, "" }
func (heldNobodySoR) SupplementalReport(string) ([]byte, bool)         { return nil, false }
func (heldNobodySoR) FacilityRecords(string) (map[string][]byte, bool) { return nil, false }
func (heldNobodySoR) OpenOrder(string) ([]byte, bool)                  { return nil, false }
func (heldNobodySoR) OpenCoverage(string) ([]byte, bool)               { return nil, false }
func (heldNobodySoR) ResolveByReference(string) ([]byte, bool)         { return nil, false }
func (heldNobodySoR) ClinicalContext(string) (shnsdk.ClinicalContext, bool) {
	return shnsdk.ClinicalContext{}, false
}
func (heldNobodySoR) ResolvePatientContext(context.Context, string) (string, Demo, bool, error) {
	return "", Demo{}, false, nil
}
func (heldNobodySoR) PatientFHIRRefContext(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (heldNobodySoR) CoverageInforceContext(context.Context, string) (bool, string, error) {
	return false, "", nil
}
func (heldNobodySoR) ClinicalContextContext(context.Context, string) (shnsdk.ClinicalContext, bool, error) {
	return shnsdk.ClinicalContext{}, false, nil
}
func (heldNobodySoR) SupplementalReportContext(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}
func (heldNobodySoR) FacilityRecordsContext(context.Context, string) (map[string][]byte, bool, error) {
	return nil, false, nil
}
func (heldNobodySoR) OpenOrderContext(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}
func (heldNobodySoR) OpenCoverageContext(context.Context, string) ([][]byte, error) {
	return nil, nil
}
func (heldNobodySoR) ResolveByReferenceContext(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

var (
	_ SystemOfRecord        = heldNobodySoR{}
	_ ContextSystemOfRecord = heldNobodySoR{}
)

// ---- the reads ----

// The two member lookups a payer leg makes answer "not held" without error;
// every other read fails with ErrNoSystemOfRecord, which a caller answers as a
// system-of-record failure, never as "no data". hasSystemOfRecord tells the
// store-less gateway from one whose store holds nobody.
func TestNoSystemOfRecord_ReadsAndPresence(t *testing.T) {
	sor := NoSystemOfRecord()
	reader, ok := sor.(ContextSystemOfRecord)
	if !ok {
		t.Fatalf("%T is not a ContextSystemOfRecord: its reads would go through the legacy adapter", sor)
	}
	if err := checkSystemOfRecordSignatures(sor); err != nil {
		t.Fatalf("New would refuse it: %v", err)
	}
	ctx := context.Background()
	if pci, demo, found, err := reader.ResolvePatientContext(ctx, strangerMember); pci != "" || demo != (Demo{}) || found || err != nil {
		t.Fatalf("ResolvePatientContext = (%q, %+v, %v, %v), want not held without error", pci, demo, found, err)
	}
	if ref, found, err := reader.PatientFHIRRefContext(ctx, strangerMember); ref != "" || found || err != nil {
		t.Fatalf("PatientFHIRRefContext = (%q, %v, %v), want not held without error", ref, found, err)
	}
	for name, read := range map[string]func() error{
		"CoverageInforceContext":    func() error { _, _, err := reader.CoverageInforceContext(ctx, strangerMember); return err },
		"ClinicalContextContext":    func() error { _, _, err := reader.ClinicalContextContext(ctx, strangerMember); return err },
		"SupplementalReportContext": func() error { _, _, err := reader.SupplementalReportContext(ctx, strangerMember); return err },
		"FacilityRecordsContext":    func() error { _, _, err := reader.FacilityRecordsContext(ctx, strangerMember); return err },
		"OpenOrderContext":          func() error { _, _, err := reader.OpenOrderContext(ctx, strangerMember); return err },
		"OpenCoverageContext":       func() error { _, err := reader.OpenCoverageContext(ctx, strangerMember); return err },
		"ResolveByReferenceContext": func() error { _, _, err := reader.ResolveByReferenceContext(ctx, "Organization/payer"); return err },
	} {
		err := read()
		if !errors.Is(err, ErrNoSystemOfRecord) {
			t.Errorf("%s: err = %v, want ErrNoSystemOfRecord", name, err)
			continue
		}
		if status, _ := SoRFailureResponse(err); status < http.StatusInternalServerError {
			t.Errorf("%s: answered %d, want a system-of-record failure (5xx)", name, status)
		}
	}
	// The legacy reads have no error to carry: each answers not found.
	if _, _, found := sor.ResolvePatient(strangerMember); found {
		t.Error("ResolvePatient found a member")
	}
	if _, found := sor.PatientFHIRRef(strangerMember); found {
		t.Error("PatientFHIRRef found a member")
	}
	if inforce, reason := sor.CoverageInforce(strangerMember); inforce || reason != "" {
		t.Errorf("CoverageInforce = (%v, %q)", inforce, reason)
	}
	if _, found := sor.OpenCoverage(strangerMember); found {
		t.Error("OpenCoverage found a record")
	}
	if _, found := sor.ResolveByReference("Organization/payer"); found {
		t.Error("ResolveByReference found a record")
	}

	for name, row := range map[string]struct {
		sor SystemOfRecord
		has bool
	}{
		"no system of record":    {NoSystemOfRecord(), false},
		"a store holding nobody": {heldNobodySoR{}, true},
		"the census fixture":     {newCensusSoR(), true},
	} {
		if got := hasSystemOfRecord(row.sor); got != row.has {
			t.Errorf("%s: hasSystemOfRecord = %v, want %v", name, got, row.has)
		}
	}
}

// ---- the payer's PA legs answer as over a store that holds nobody ----

// nosorTokenSubject is the leg token's subject: a patient other than the one
// the payer binds, so the binding-differs event and the involved list have
// something to say.
const nosorTokenSubject = "pci:token-subject"

// nosorBirth and nosorFamily are the demographics of the Patient a request
// carries for strangerMember.
const (
	nosorBirth  = "1962-03-11"
	nosorFamily = "Nakamura"
)

// legRun is everything a requester and the network see of one payer leg.
type legRun struct {
	status     int
	framed     bool
	body       []byte
	subjects   []string // the subject handed to the payer's system
	events     []string // every observer event, identifier-free fields only
	authorized []string // every token the leg asked for: operation|subject|involvement
	ledger     string   // what the leg left in the payer's records, when the case reads them
}

// authorizeRecorder records each /authorize request the payer gateway makes
// (the response leg's token and each involved patient's) and delegates.
type authorizeRecorder struct {
	inner http.RoundTripper
	mu    *sync.Mutex
	got   *[]string
}

func (a authorizeRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/authorize") && r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var req authorizeReq
		_ = json.Unmarshal(body, &req)
		a.mu.Lock()
		*a.got = append(*a.got, req.Operation+"|"+req.SubjectPCI+"|"+req.Involvement)
		a.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	return a.inner.RoundTrip(r)
}

// payerLegCase is one payer-native leg driven by runPayerLeg.
type payerLegCase struct {
	name, leg, operation string
	body                 []byte
	answers              map[string][]byte // the payer's system's answer by path, over the level payer's defaults
	service              string            // the CDS service the native forward calls; "" for the level payer's order-select one
	setup                func(t *testing.T, p *levelPayer)
	ledger               func(t *testing.T, p *levelPayer, corr string) string // what the leg left in the payer's records
	want                 []byte                                                // the payer's answer the requester must receive
	subject              string                                                // the binding handed to the payer's system
	wantLedger           string
}

// runPayerLeg drives one leg through handleInbound at observe on a payer
// gateway whose system of record is sor, whose Hub reads the involved list,
// and whose native forward answers as c says.
func runPayerLeg(t *testing.T, sor SystemOfRecord, c payerLegCase) legRun {
	t.Helper()
	p := newLevelPayer(t, EnforcementObserve)
	p.g.cfg.SoR = sor
	if c.service != "" {
		n := NewNativeResponder(p.partner.srv.Client(), p.partner.srv.URL, c.service, p.store, fixedClock, WithConformancePolicy(NewConformancePolicy(EnforcementObserve)))
		n.bindFindingEmitter(p.g.emitFinding)
		p.g.cfg.Responder = n
	}
	p.g.cfg.HubAcceptsInvolved = true
	for path, answer := range c.answers {
		p.partner.respByPath[path] = answer
	}
	if c.setup != nil {
		c.setup(t, p)
	}
	var run legRun
	p.g.cfg.Responder = subjectCapture{got: &run.subjects, inner: p.g.cfg.Responder}
	// Certification evidence is delivered to the observer after the answer:
	// the events are read once it has all arrived (waitCertification).
	var mu sync.Mutex
	observe := p.g.cfg.Observer
	p.g.cfg.Observer = func(e ObserverEvent) {
		mu.Lock()
		defer mu.Unlock()
		run.events = append(run.events, fmt.Sprintf("%s/%s/%s/%s/%s/%d/%s", e.Kind, e.LegType, e.Direction, e.CorrelationID, e.Op, e.Status, e.Detail))
		observe(e)
	}
	p.g.cfg.Client = &http.Client{Transport: authorizeRecorder{inner: p.g.cfg.Client.Transport, mu: &mu, got: &run.authorized}}
	got := p.sendAs(t, c.leg, c.operation, c.body, nosorTokenSubject)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.g.waitCertification(ctx); err != nil {
		t.Fatalf("certification evidence did not arrive: %v", err)
	}
	// An observe-level check runs off the request path, so its events are read
	// once the queue is idle, and they may land anywhere among the leg's own:
	// the leg's events keep their order, and the queued checks' events follow
	// them, sorted, so two runs compare equal whatever the interleaving.
	p.g.drainObserveChecks()
	mu.Lock()
	defer mu.Unlock()
	run.status, run.framed, run.body = got.status, got.framed, got.body
	var inline, queued []string
	for _, e := range run.events {
		if strings.HasPrefix(e, CRDEmbeddedValidatedEvent+"/") || strings.HasPrefix(e, ConformanceObservedEvent+"/") {
			queued = append(queued, e)
		} else {
			inline = append(inline, e)
		}
	}
	slices.Sort(queued)
	run.events = append(inline, queued...)
	run.authorized = slices.Clone(run.authorized)
	if c.ledger != nil {
		run.ledger = c.ledger(t, p, got.corr)
	}
	return run
}

// pendLedger reports the pend row for (pci, corr) and the EOBs filed under
// pci, as a leg's ledger inspector.
func pendLedger(pci string, corr func(legCorr string) string) func(t *testing.T, p *levelPayer, legCorr string) string {
	return func(t *testing.T, p *levelPayer, legCorr string) string {
		t.Helper()
		rec, found, err := p.store.PendRecordOf(pci, corr(legCorr))
		if err != nil {
			t.Fatalf("read ledger: %v", err)
		}
		eobs, _ := p.store.EOBsForPatient(pci)
		return fmt.Sprintf("pend found=%v state=%s outcome=%s; %d EOB(s)", found, rec.State, rec.Outcome, len(eobs))
	}
}

// withBundlePatient replaces the resource of each Patient entry in a Bundle
// with patient.
func withBundlePatient(t *testing.T, bundle []byte, patient string) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatal(err)
	}
	replaced := 0
	for _, e := range b["entry"].([]any) {
		entry := e.(map[string]any)
		if r, _ := entry["resource"].(map[string]any); r != nil && r["resourceType"] == "Patient" {
			entry["resource"] = json.RawMessage(patient)
			replaced++
		}
	}
	if replaced != 1 {
		t.Fatalf("fixture: %d Patient entries replaced, want 1", replaced)
	}
	return mustJSON(t, b)
}

// TestNoSystemOfRecord_PayerLegsAnswerAsAStoreHoldingNobody: on every
// payer-native PA leg (CRD order-select, order-sign and order-dispatch; DTR
// $questionnaire-package and $next-question; PAS $submit, update and
// $inquire), a gateway that keeps no system of record answers exactly as the
// same gateway over a store that holds nobody: the same framed status and
// bytes, the member bound by the carry path from the Patient the request
// carries and handed to the payer's system, the same binding-differs event,
// the same involved list (the payer-derived binding), and the same records
// filed under that binding.
func TestNoSystemOfRecord_PayerLegsAnswerAsAStoreHoldingNobody(t *testing.T) {
	carried := requestPatient(strangerMember, nosorBirth, nosorFamily)
	derived := derivedPCI(strangerMember, nosorBirth, nosorFamily)
	byID := derivedPCI(strangerMember, "", "")
	realCRD := realCRDAnswer(t)
	orderSign := bytes.Replace(conformantCRDWithPatient(strangerMember, "72148", carried), []byte(`"order-select"`), []byte(`"order-sign"`), 1)
	dispatch := []byte(strings.ReplaceAll(crdDispatchRequest, "MBR-COVERED", strangerMember))
	nextAnswer := nextQuestionAnswer(t, "Patient/"+strangerMember, rawItems(t, adaptiveTree(t, "1")))

	// The amendment carries the member's Patient with demographics in place
	// of the golden's: it binds by those, and its pend (the prior
	// authorization it amends) is filed under that binding.
	updateRaw, related := updateBundle(t)
	update := withBundlePatient(t, rebindPASPatient(t, updateRaw, strangerMember), carried)
	updateSubject := derived
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)

	rows := []payerLegCase{
		{name: "CRD order-select", leg: "crd-order-select", body: conformantCRDWithPatient(strangerMember, "72148", carried),
			want: realCRD, subject: derived},
		{name: "CRD order-sign", leg: "crd-order-select", body: orderSign,
			answers: map[string][]byte{"/cds-services/order-sign-crd": realCRD}, service: "order-sign-crd", want: realCRD, subject: derived},
		// The dispatch request carries no Patient: it binds by the member id.
		{name: "CRD order-dispatch", leg: "crd-order-dispatch", body: dispatch, want: realCRD, subject: byID},
		{name: "DTR $questionnaire-package", leg: "dtr-questionnaire-fetch", operation: shnsdk.FrameOperationQuestionnairePackage,
			body: dtrPackage(resourceParam("coverage", dtrCoverage("cov-1", strangerMember)), resourceParam("referenced", carried)),
			want: []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`), subject: derived},
		// The round's QuestionnaireResponse names the member and carries no
		// Patient: it binds by the member id.
		{name: "DTR $next-question", leg: "dtr-questionnaire-fetch", operation: shnsdk.FrameOperationNextQuestion,
			body: []byte(nextQuestionQR(strangerMember)), answers: map[string][]byte{nextPath: nextAnswer}, want: nextAnswer, subject: byID},
		{name: "PAS $submit", leg: "pas-claim", body: pasBundleWithPatient(t, strangerMember, carried, false),
			ledger: pendLedger(derived, func(corr string) string { return corr }),
			want:   []byte(assemblyRealPending), subject: derived, wantLedger: "pend found=true state=" + string(PendStatePended) + " outcome=; 0 EOB(s)"},
		{name: "PAS update", leg: "pas-claim-update", body: update,
			answers: map[string][]byte{pasSubmitPath: approved},
			setup: func(t *testing.T, p *levelPayer) {
				t.Helper()
				if err := p.store.RecordPendedClaim(updateSubject, related); err != nil {
					t.Fatal(err)
				}
			},
			ledger: pendLedger(updateSubject, func(string) string { return related }),
			want:   approved, subject: updateSubject,
			// The update leg decides the pend and builds no EOB
			// (TestNativeUpdate_ApprovalRelayedAndDecided).
			wantLedger: "pend found=true state=" + string(PendStateDecided) + " outcome=" + string(PendOutcomeApproved) + "; 0 EOB(s)"},
		// The inquiry carries a Patient with no demographics: it binds by the
		// member id alone, over either store.
		{name: "PAS $inquire", leg: "pas-claim-inquire", body: inquiryBundle(strangerMember, "", "TRN-1", "72148"),
			want: decidedAnswer(t), subject: byID},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			none := runPayerLeg(t, NoSystemOfRecord(), row)
			nobody := runPayerLeg(t, heldNobodySoR{}, row)

			// The rows are not vacuous: the payer's answer is relayed, about
			// the carry-path binding, which is not the token's subject.
			if nobody.status != http.StatusOK || !nobody.framed || !bytes.Equal(nobody.body, row.want) {
				t.Fatalf("over a store holding nobody: answer %d (framed %v) %s, want the payer's answer relayed", nobody.status, nobody.framed, nobody.body)
			}
			if len(nobody.subjects) != 1 || nobody.subjects[0] != row.subject {
				t.Fatalf("over a store holding nobody the payer was handed %q, want the carry-path binding %q", nobody.subjects, row.subject)
			}
			if countKind(nobody.events, SubjectBindingDiffersEvent) != 1 {
				t.Fatalf("over a store holding nobody: events %v, want one %s", nobody.events, SubjectBindingDiffersEvent)
			}
			if !containsSuffix(nobody.authorized, row.subject+"|"+shnsdk.InvolvementPayerDerived) {
				t.Fatalf("over a store holding nobody: authorized %v, want the payer-derived binding named on the involved list", nobody.authorized)
			}
			if nobody.ledger != row.wantLedger {
				t.Fatalf("over a store holding nobody the payer's records read %q, want %q", nobody.ledger, row.wantLedger)
			}

			if none.status != nobody.status || none.framed != nobody.framed || !bytes.Equal(none.body, nobody.body) {
				t.Fatalf("no system of record answered %d (framed %v) %s\nover a store holding nobody %d (framed %v) %s",
					none.status, none.framed, none.body, nobody.status, nobody.framed, nobody.body)
			}
			if !slices.Equal(none.subjects, nobody.subjects) {
				t.Fatalf("no system of record handed the payer %q, a store holding nobody %q", none.subjects, nobody.subjects)
			}
			if !slices.Equal(none.events, nobody.events) {
				t.Fatalf("events differ:\n none   %v\n nobody %v", none.events, nobody.events)
			}
			if !slices.Equal(none.authorized, nobody.authorized) {
				t.Fatalf("involved lists differ:\n none   %v\n nobody %v", none.authorized, nobody.authorized)
			}
			if none.ledger != nobody.ledger {
				t.Fatalf("records differ: none %q, nobody %q", none.ledger, nobody.ledger)
			}
		})
	}
}

func countKind(events []string, kind string) int {
	n := 0
	for _, e := range events {
		if strings.HasPrefix(e, kind+"/") {
			n++
		}
	}
	return n
}

func containsSuffix(list []string, suffix string) bool {
	for _, s := range list {
		if strings.HasSuffix(s, "|"+suffix) {
			return true
		}
	}
	return false
}

// ---- a submit and a later inquiry that carries less ----

// TestNoSystemOfRecord_InquiryWithoutTheSubmitsPatientIsRelayed documents the
// carry path as it stands, not a fix. A store-less payer binds each leg from
// what that leg carries: the $submit carried the member's Patient with its
// birthDate and family name, so its pend is filed under the identifier derived
// from those (derivedPCI(member, birth, family)); the $inquire carries a
// Patient with no demographics, so it binds by the member id alone
// (derivedPCI(member, "", "")). The two keys differ, so the inquiry's decision
// does not reach the submit's pend: the payer's answer is still relayed
// exactly, the pend stays pended under the submit's key, and no EOB is filed
// under either. The control, an inquiry carrying the same Patient, decides the
// pend, so the row can fail.
func TestNoSystemOfRecord_InquiryWithoutTheSubmitsPatientIsRelayed(t *testing.T) {
	// The inquiry answer decides the authorization the pended $submit answer
	// named: the same ClaimResponse identifier (as in
	// TestLevelPayerInquire_AfterSkippedSubmit).
	const traceSystem = `"system":"http://example.org/PATIENT_EVENT_TRACE_NUMBER","value":"`
	start := strings.Index(assemblyRealPending, traceSystem)
	if start < 0 {
		t.Fatal("fixture: the pended answer carries no trace identifier")
	}
	pendedTrace := assemblyRealPending[start+len(traceSystem):]
	pendedTrace = pendedTrace[:strings.IndexByte(pendedTrace, '"')]
	decided := bytes.Replace(decidedAnswer(t), []byte(`"value": "111099"`), []byte(`"value": "`+pendedTrace+`"`), 1)
	if bytes.Equal(decided, decidedAnswer(t)) {
		t.Fatal("fixture: the inquiry answer's identifier was not replaced")
	}
	carried := requestPatient(strangerMember, nosorBirth, nosorFamily)
	submitKey := derivedPCI(strangerMember, nosorBirth, nosorFamily)
	inquiryKey := derivedPCI(strangerMember, "", "")
	if submitKey == inquiryKey {
		t.Fatal("fixture: the two derived keys must differ")
	}
	withCarried := bytes.Replace(inquiryBundle(strangerMember, "", "TRN-1", "72148"),
		[]byte(`{"resourceType":"Patient","id":"`+strangerMember+`"}`), []byte(carried), 1)

	for _, tc := range []struct {
		name    string
		inquiry []byte
		decides bool
	}{
		{"control: the inquiry carries the submit's Patient", withCarried, true},
		{"the inquiry carries no demographics", inquiryBundle(strangerMember, "", "TRN-1", "72148"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newLevelPayer(t, EnforcementObserve)
			p.g.cfg.SoR = NoSystemOfRecord()
			submit := p.sendAs(t, "pas-claim", "", pasBundleWithPatient(t, strangerMember, carried, false), nosorTokenSubject)
			if submit.status != http.StatusOK || !bytes.Equal(submit.body, []byte(assemblyRealPending)) {
				t.Fatalf("submit answer %d %s", submit.status, submit.body)
			}
			if rec, found, err := p.store.PendRecordOf(submitKey, submit.corr); err != nil || !found || rec.State != PendStatePended {
				t.Fatalf("the submit's pend is filed under the key derived from its Patient: found=%v %+v err=%v", found, rec, err)
			}
			if _, found, _ := p.store.PendRecordOf(nosorTokenSubject, submit.corr); found {
				t.Fatal("a pend was filed under the token's subject")
			}

			p.partner.respByPath[pasInquirePath] = decided
			// Asked by the trace number the payer's pend named for the claim (the
			// submission carries none), so the inquiry is about that claim.
			inquiry := p.sendAs(t, "pas-claim-inquire", "", withTrace(t, tc.inquiry, "http://example.org/ITEM_TRACE_NUMBER", "prior-auth-required-trace"), nosorTokenSubject)
			if inquiry.status != http.StatusOK || !inquiry.framed || !bytes.Equal(inquiry.body, decided) {
				t.Fatalf("the inquiry must relay the payer's answer exactly: %d %s", inquiry.status, inquiry.body)
			}
			rec, found, err := p.store.PendRecordOf(submitKey, submit.corr)
			if err != nil || !found {
				t.Fatalf("the submit's pend is gone: found=%v err=%v", found, err)
			}
			submitEOBs, _ := p.store.EOBsForPatient(submitKey)
			inquiryEOBs, _ := p.store.EOBsForPatient(inquiryKey)
			tokenEOBs, _ := p.store.EOBsForPatient(nosorTokenSubject)
			if len(tokenEOBs) != 0 {
				t.Fatalf("%d EOB(s) filed under the token's subject", len(tokenEOBs))
			}
			if tc.decides {
				if rec.State != PendStateDecided || len(submitEOBs) != 1 || len(inquiryEOBs) != 0 {
					t.Fatalf("control: want the submit's pend decided with its EOB, got %+v, %d EOB(s) under the submit's key, %d under the inquiry's", rec, len(submitEOBs), len(inquiryEOBs))
				}
				return
			}
			// The inquiry is bound under another key than the submit's pend:
			// its decision lands nowhere.
			if rec.State != PendStatePended || len(submitEOBs) != 0 || len(inquiryEOBs) != 0 {
				t.Fatalf("want the submit's pend still pended and no EOB, got %+v, %d EOB(s) under the submit's key, %d under the inquiry's", rec, len(submitEOBs), len(inquiryEOBs))
			}
			if _, found, _ := p.store.PendRecordOf(inquiryKey, submit.corr); found {
				t.Fatal("a pend appeared under the inquiry's key")
			}
		})
	}
}

// ---- eligibility ----

// TestNoSystemOfRecord_EligibilityNotOffered: with no system of record and no
// eligibility endpoint of the payer's own to forward to, eligibility is not
// offered: a framed 501 at every level, nothing sent to the payer's system,
// and no Coverage read. A store that holds nobody still answers from its
// records (the member is unknown there), and the census answers as before.
func TestNoSystemOfRecord_EligibilityNotOffered(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run("no system of record/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			p.g.cfg.SoR = NoSystemOfRecord()
			got := p.sendAs(t, "coverage-eligibility", "", eligibilityRequest(t, strangerMember), nosorTokenSubject)
			// The payer gateway's own refusal, in the shape every inbound
			// refusal takes ({"error": …}), exactly.
			want := `{"error":"coverage eligibility is not offered by this payer"}`
			if !got.framed || got.status != http.StatusNotImplemented || strings.TrimSpace(string(got.body)) != want {
				t.Fatalf("answer %d (framed %v) %s, want a framed 501 %s", got.status, got.framed, got.body, want)
			}
			if p.partner.lastPath != "" {
				t.Fatalf("the payer's system was called at %s", p.partner.lastPath)
			}
		})
	}
	t.Run("a store holding nobody answers from its records", func(t *testing.T) {
		p := eligibilityPayer(t, EnforcementObserve, false)
		p.g.cfg.SoR = heldNobodySoR{}
		got := p.sendAs(t, "coverage-eligibility", "", eligibilityRequest(t, strangerMember), nosorTokenSubject)
		if got.status != http.StatusBadRequest || !strings.Contains(string(got.body), refusalUnknownMember) {
			t.Fatalf("answer %d %s, want the store's own 400 %q", got.status, got.body, refusalUnknownMember)
		}
	})
	t.Run("the census answers as before", func(t *testing.T) {
		p := eligibilityPayer(t, EnforcementObserve, false)
		got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
		if got.status != http.StatusOK || !bytes.Contains(got.body, []byte(`"CoverageEligibilityResponse"`)) {
			t.Fatalf("answer %d %s", got.status, got.body)
		}
	})
}

// TestNoSystemOfRecord_ObservedGatewayKeepsNone: a gateway with an observer
// (OBSERVER_ADDR) has its system of record decorated by New (observingSoR). A
// store-less gateway so decorated still keeps no system of record: it does not
// offer eligibility, exactly as without the observer.
func TestNoSystemOfRecord_ObservedGatewayKeepsNone(t *testing.T) {
	base, _ := newInboundTestGateway(t, true)
	cfg := base.cfg
	cfg.SoR = NoSystemOfRecord()
	cfg.Populator = nil
	cfg.Observer = func(ObserverEvent) {}
	g := mustNew(t, cfg)
	if hasSystemOfRecord(g.cfg.SoR) {
		t.Errorf("New with an observer turned no system of record (%T) into one", g.cfg.SoR)
	}

	p := eligibilityPayer(t, EnforcementObserve, false)
	p.g.cfg.SoR = observingSoR{inner: NoSystemOfRecord(), observer: p.g.cfg.Observer, clock: p.g.cfg.Clock}
	got := p.sendAs(t, "coverage-eligibility", "", eligibilityRequest(t, strangerMember), nosorTokenSubject)
	if !got.framed || got.status != http.StatusNotImplemented || !strings.Contains(string(got.body), refusalEligibilityNotOffered) {
		t.Fatalf("observed: answer %d (framed %v) %s, want a framed 501 %q", got.status, got.framed, got.body, refusalEligibilityNotOffered)
	}
}

// TestNoSystemOfRecord_EligibilityAnswerFenceSeesNotHeld: a store-less payer
// that forwards eligibility to its own endpoint fences the payer's answer
// about another patient as a patient it does not hold: at observe the answer
// is relayed exactly and a content finding recorded (a verdict, not a check
// that could not run); at strict it is refused 403. Neither answers a
// system-of-record failure. An answer about the request's own member passes
// at strict.
func TestNoSystemOfRecord_EligibilityAnswerFenceSeesNotHeld(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementStrict} {
		t.Run("another patient/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			p.g.cfg.SoR = NoSystemOfRecord()
			answer := payersEligibilityAnswer("Patient/someone-else")
			p.partner.respByPath[eligibilityPath] = answer
			got := p.sendAs(t, "coverage-eligibility", "", eligibilityRequest(t, strangerMember), nosorTokenSubject)
			if got.status >= http.StatusInternalServerError {
				t.Fatalf("answer %d %s: a system-of-record failure", got.status, got.body)
			}
			var findings []ConformanceFinding
			for _, f := range p.content() {
				if f.Rule == RulePatientAnswer {
					findings = append(findings, f)
				}
			}
			if len(findings) != 1 || findings[0].Verdict == "unavailable" || findings[0].LegType != "coverage-eligibility" {
				t.Fatalf("want one %s verdict on coverage-eligibility, got %+v", RulePatientAnswer, findings)
			}
			if level == EnforcementStrict {
				if !got.framed || got.status != http.StatusForbidden || !strings.Contains(string(got.body), "response patient does not match request patient") {
					t.Fatalf("strict: answer %d (framed %v) %s, want the framed 403", got.status, got.framed, got.body)
				}
				if findings[0].Decision != "refused" {
					t.Fatalf("strict: finding %+v, want refused", findings[0])
				}
				return
			}
			if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
				t.Fatalf("observe: answer %d %s, want the payer's bytes relayed", got.status, got.body)
			}
			if findings[0].Decision != "relayed" {
				t.Fatalf("observe: finding %+v, want relayed", findings[0])
			}
		})
	}
	t.Run("the request's own member/strict", func(t *testing.T) {
		p := eligibilityPayer(t, EnforcementStrict, true)
		p.g.cfg.SoR = NoSystemOfRecord()
		answer := payersEligibilityAnswer("Patient/" + strangerMember)
		p.partner.respByPath[eligibilityPath] = answer
		got := p.sendAs(t, "coverage-eligibility", "", eligibilityRequest(t, strangerMember), nosorTokenSubject)
		if got.status != http.StatusOK || !bytes.Equal(got.body, answer) || len(p.content()) != 0 {
			t.Fatalf("answer %d %s, findings %v: want the payer's bytes relayed and nothing recorded", got.status, got.body, findingsText(p.content()))
		}
	})
}
