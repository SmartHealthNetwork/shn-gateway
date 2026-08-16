package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// adjTestClock is the fixed clock used by sandbox-adjudicator tests.
var adjTestClock = time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

// adjTestCPT is the CPT code in the UC-03/approved fixture.
const adjTestCPT = "72148"

// newSandboxResponderForTest builds a sandboxResponder backed by a fresh
// StubHolderData (satisfies both SystemOfRecord and Store) and a fixed clock.
func newSandboxResponderForTest(t *testing.T) LegResponder {
	t.Helper()
	data := NewStubHolderData()
	clock := func() time.Time { return adjTestClock }
	adj := NewSandboxAdjudicator(data, clock)
	return NewSandboxResponder(adj, data, data, clock)
}

// TestSandbox_PASClaimUpdateNative_Finalizes is CANARY #1 of the four-cell relocation: the
// CONFORMANT sandbox update case (case "pas-claim-update") completes the in-process
// pend→approve resolution (FinalizeClaimUpdate) — UC-04's distinctive capability now on the
// conformant leg. It mirrors the minimized case "pas-claim-update" (adjudicator.go:375-425) but
// reads the prior-claim key via parseConformantPASUpdateFacts and the QR/member/hasDR via
// parseConformantPASSubjects (not the strict ParseClaimBundle). No EOB on the update leg.
func TestSandbox_PASClaimUpdateNative_Finalizes(t *testing.T) {
	bundle := originatorBuiltConformantUpdateBundle(t) // related[prior]=convergence-pas-submit-0001, hasDR=true
	const origCorr = "convergence-pas-submit-0001"
	const pci = "pci:conf-update"

	newSeeded := func(t *testing.T) (LegResponder, *StubHolderData) {
		t.Helper()
		data := NewStubHolderData()
		clock := func() time.Time { return adjTestClock }
		r := NewSandboxResponder(NewSandboxAdjudicator(data, clock), data, data, clock)
		return r, data
	}

	t.Run("prior pend -> approved -> Commit FinalizeClaimUpdate, Rollback armed, no EOB", func(t *testing.T) {
		r, data := newSeeded(t)
		_ = data.RecordPendedClaim(pci, origCorr)
		res, err := r.Handle(context.Background(), "pas-claim-update", "corr-upd", pci, bundle)
		if err != nil || res.Status != 0 {
			t.Fatalf("conformant sandbox update: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if res.Commit == nil || res.Rollback == nil {
			t.Fatalf("approved update must carry FinalizeClaimUpdate Commit + armed Rollback")
		}
		if len(res.SideEffectFHIR) != 0 {
			t.Fatalf("update leg must emit NO EOB; got %d", len(res.SideEffectFHIR))
		}
		// The decision is approved (a parseable ClaimResponse with a preAuthRef).
		parsed, perr := shnsdk.ParseClaimResponse(res.ResponseFHIR)
		if perr != nil || parsed.Outcome != "approved" || parsed.PreAuthRef == "" {
			t.Fatalf("want approved ClaimResponse + preAuthRef, got %+v err=%v", parsed, perr)
		}
		// Commit completes the pend→approve transition: a replayed update finds nothing (409).
		if err := res.Commit(); err != nil {
			t.Fatalf("Commit (FinalizeClaimUpdate): %v", err)
		}
		replay, _ := r.Handle(context.Background(), "pas-claim-update", "corr-upd2", pci, bundle)
		if replay.Status != http.StatusConflict {
			t.Fatalf("after Finalize, a replayed update must 409 (claim gone), got %d", replay.Status)
		}
	})

	t.Run("no prior pend -> 409 (derived-ledger fail-safe)", func(t *testing.T) {
		r, _ := newSeeded(t) // NOT seeded
		res, _ := r.Handle(context.Background(), "pas-claim-update", "corr-upd", pci, bundle)
		if res.Status != http.StatusConflict {
			t.Fatalf("no prior pend must be 409, got %d", res.Status)
		}
	})
}

// dtrFetchTestCoverage builds a Coverage resource via the SAME builder originate.go
// uses (shnsdk.BuildCoverageWithPayer) — the honest, SoR-derived shape the sandbox
// responder's 2.2 QR shell reads (dtrPackageCoverageSubject), never a hand-fabricated one.
func dtrFetchTestCoverage(t *testing.T, member string) json.RawMessage {
	t.Helper()
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/"+member, member, shnsdk.CMSPayerIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	return cov
}

// TestAdjudicatorAnswers22PackageWithQRShell (verified live):
// the sandbox responder's dtr-questionnaire-fetch answer at DTR line 2.2 carries an
// in-progress, ZERO-answer QuestionnaireResponse entry (DTR-QPackageBundle's
// Bundle.entry:questionnaireResponse min=1), with the qr-coverage extension present and
// derived from the requester's OWN Coverage — never fabricated. At 2.0/2.1 the entry
// stays absent, byte-fencing the earlier shape (buildQuestionnairePackageAtLine's
// qr-optional/unconstrained lines never gained a QR from this responder before, and
// still don't).
func TestAdjudicatorAnswers22PackageWithQRShell(t *testing.T) {
	r := newSandboxResponderForTest(t)
	const member = "MBR-UC03"
	coverage := dtrFetchTestCoverage(t, member)
	fetch := shnsdk.QuestionnaireFetchRequest{Canonical: shnsdk.QuestionnaireCanonicalLumbarMRI, Coverage: coverage}
	reqBytes, err := json.Marshal(fetch)
	if err != nil {
		t.Fatalf("marshal fetch: %v", err)
	}

	t.Run("2.2: QR shell present, in-progress, zero answers, qr-coverage present", func(t *testing.T) {
		ctx := withAnswerLine(context.Background(), shnsdk.ContractPADTR22)
		res, err := r.Handle(ctx, "dtr-questionnaire-fetch", "corr-qr22", "pci-1", reqBytes)
		if err != nil || res.Status != 0 {
			t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		var bundle struct {
			Entry []struct {
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		}
		if err := json.Unmarshal(res.ResponseFHIR, &bundle); err != nil {
			t.Fatalf("unmarshal package bundle: %v", err)
		}
		type shellQR struct {
			ResourceType string `json:"resourceType"`
			Status       string `json:"status"`
			Item         []any  `json:"item"`
			Extension    []struct {
				URL            string `json:"url"`
				ValueReference *struct {
					Reference string `json:"reference"`
				} `json:"valueReference"`
			} `json:"extension"`
		}
		var qr *shellQR
		for _, e := range bundle.Entry {
			var probe struct {
				ResourceType string `json:"resourceType"`
			}
			if err := json.Unmarshal(e.Resource, &probe); err != nil {
				continue
			}
			if probe.ResourceType == "QuestionnaireResponse" {
				qr = new(shellQR)
				if err := json.Unmarshal(e.Resource, qr); err != nil {
					t.Fatalf("unmarshal QR entry: %v", err)
				}
			}
		}
		if qr == nil {
			t.Fatalf("no QuestionnaireResponse entry in the 2.2 package: %s", res.ResponseFHIR)
		}
		if qr.Status != "in-progress" {
			t.Errorf("QR status = %q, want in-progress", qr.Status)
		}
		if len(qr.Item) != 0 {
			t.Errorf("QR carries %d items, want ZERO answers (an honest shell)", len(qr.Item))
		}
		var coverageRef string
		for _, ext := range qr.Extension {
			if ext.URL == dtrQRCoverageExtURL && ext.ValueReference != nil {
				coverageRef = ext.ValueReference.Reference
			}
		}
		if coverageRef == "" {
			t.Fatal("qr-coverage extension missing or empty — the shell must NOT fabricate this")
		}
		// Derived from the requester's own Coverage.id, not invented and not
		// read off a private identifier: BuildCoverageWithPayer stamps the conformant
		// coverage id "c1", so the shell references Coverage/c1 — the SAME resource the
		// requester sent, whatever its member number happens to be.
		const wantCoverageRef = "Coverage/c1"
		if coverageRef != wantCoverageRef {
			t.Errorf("qr-coverage reference = %q, want %q (derived from the requester's own Coverage.id)", coverageRef, wantCoverageRef)
		}
	})

	t.Run("2.2: no coverage in the fetch request -> 400, never a fabricated shell", func(t *testing.T) {
		noCovFetch, _ := json.Marshal(shnsdk.QuestionnaireFetchRequest{Canonical: shnsdk.QuestionnaireCanonicalLumbarMRI})
		ctx := withAnswerLine(context.Background(), shnsdk.ContractPADTR22)
		res, err := r.Handle(ctx, "dtr-questionnaire-fetch", "corr-qr22b", "pci-1", noCovFetch)
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if res.Status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (a legible refusal, not a fabricated shell)", res.Status)
		}
	})

	for _, tok := range []string{shnsdk.ContractPADTR20, shnsdk.ContractPADTR21} {
		t.Run(tok+": no QuestionnaireResponse entry (byte fence)", func(t *testing.T) {
			ctx := withAnswerLine(context.Background(), tok)
			res, err := r.Handle(ctx, "dtr-questionnaire-fetch", "corr-fence", "pci-1", reqBytes)
			if err != nil || res.Status != 0 {
				t.Fatalf("Handle: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			if strings.Contains(string(res.ResponseFHIR), "QuestionnaireResponse") {
				t.Fatalf("%s package must carry NO QuestionnaireResponse entry: %s", tok, res.ResponseFHIR)
			}
			// Same shape this responder has always emitted at these lines: a bare
			// one-entry Bundle wrapping the Questionnaire only.
			want, werr := buildQuestionnairePackageAtLine(shnsdk.LineOf(tok), shnsdk.SandboxLumbarQuestionnaire(), nil)
			if werr != nil {
				t.Fatalf("reference build: %v", werr)
			}
			if string(res.ResponseFHIR) != string(want) {
				t.Fatalf("%s package = %s, want byte-identical to the QR-less wrap %s", tok, res.ResponseFHIR, want)
			}
		})
	}
}
