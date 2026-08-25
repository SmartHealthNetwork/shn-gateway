// gateway/engine/inbound_test.go — coverage-eligibility is the promoted,
// Coverage-derived engine handler. handleEligibilityInbound answers directly
// off the member's own Coverage (SoR.CoverageInforce + SoR.OpenCoverage's payor) and
// NEVER consults the injected Adjudicator (R11) — the poison-pill Adjudicator below
// proves it: Eligibility() panics if called, and the test still passes.
package engine

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// poisonEligibilityAdjudicator is a shnsdk.Adjudicator whose Eligibility method
// panics — the R11 poison pill. The other three methods are unused by this test
// (coverage-eligibility never reaches OrderSelect/Questionnaire/PriorAuth) so they
// return harmless zero values rather than also panicking, keeping the failure signal
// specific to the one method the promoted handler must never call.
type poisonEligibilityAdjudicator struct{}

func (poisonEligibilityAdjudicator) Eligibility(memberID string) (bool, string) {
	panic("R11 violated: handleEligibilityInbound consulted the injected Adjudicator for coverage-eligibility")
}
func (poisonEligibilityAdjudicator) OrderSelect(cpt string) (bool, string) { return false, "" }
func (poisonEligibilityAdjudicator) Questionnaire(canonical string) ([]byte, bool) {
	return nil, false
}
func (poisonEligibilityAdjudicator) PriorAuth(qrJSON []byte, hasDiagnosticReport bool) (shnsdk.PASDecision, error) {
	return shnsdk.PASDecision{}, nil
}

var _ shnsdk.Adjudicator = poisonEligibilityAdjudicator{}

// poisonEligibilityResponder is the R11 pill on the OTHER seam: a content occupant that
// panics the moment the engine routes coverage-eligibility to it. Eligibility is an
// engine-side Coverage read (R11), so no occupant — partner or mirror — may ever see the
// leg. Any other leg answers harmlessly, keeping the failure signal specific.
type poisonEligibilityResponder struct{}

func (poisonEligibilityResponder) Handle(_ context.Context, leg, _, _ string, _ []byte) (LegResult, error) {
	if leg == "coverage-eligibility" {
		panic("R11 violated: the coverage-eligibility leg reached a content occupant")
	}
	return LegResult{}, nil
}

var _ LegResponder = poisonEligibilityResponder{}

// TestEligibilityInbound_CoverageDerivedInsurer drives the coverage-eligibility leg
// for MBR-PAYERB — a stub persona whose OpenCoverage names payor
// {system: shnsdk.CMSPayerIdentity.System, value: "00078"} (holderdata.go's
// censusPayerOverrides, the existing FR-G40 hermetic multi-payer fixture) — through
// handleEligibilityInbound directly (frame-incapable requester ⇒ a bare response body,
// simplest to assert against). It asserts:
//  1. the response's insurer identifies 00078, never the "Organization/payer" literal
//     BuildEligibilityResponse used to hardcode;
//  2. covered=true, sourced from SoR.CoverageInforce; and
//  3. the poisoned Adjudicator.Eligibility was never called (R11) — proven by NOT
//     panicking, since the poison pill panics on any call.
func TestEligibilityInbound_CoverageDerivedInsurer(t *testing.T) {
	g, requester := newInboundTestGateway(t, false) // frame-incapable ⇒ bare response body
	// Poison BOTH payer seams: the raw Adjudicator (what R11 names) and the content
	// occupant behind Responder (the only other place a verdict could come from). The
	// promoted handler must consult neither — it reads the member's own Coverage.
	g.cfg.Adjudicator = poisonEligibilityAdjudicator{}
	g.cfg.Responder = poisonEligibilityResponder{}

	const member = "MBR-PAYERB"
	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		t.Fatalf("fixture bug: %s not resolvable in the stub SoR", member)
	}
	covJSON, hasCov := g.cfg.SoR.OpenCoverage(member)
	if !hasCov {
		t.Fatalf("fixture bug: %s has no OpenCoverage", member)
	}
	payer, ok := shnsdk.ParsePayerIdentifier(covJSON, nil)
	if !ok || payer.Value != "00078" {
		t.Fatalf("fixture bug: %s's OpenCoverage payor = %+v (ok=%v), want value 00078", member, payer, ok)
	}

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	cerJSON, err := shnsdk.BuildEligibilityRequest(member, "1234567890", now)
	if err != nil {
		t.Fatalf("BuildEligibilityRequest: %v", err)
	}
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: requester.ID, Recipient: "payer", TransactionType: "coverage-eligibility",
		AuthorityFrame: "provider-tpo", Timestamp: g.cfg.Clock().Format(time.RFC3339),
		CorrelationID: "corr-elig-1",
	}, cerJSON, g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tok := shnsdk.Token{Operation: "eligibility-inquiry", Subject: pci, CorrelationID: "corr-elig-1"}

	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.handleEligibilityInbound(rec, r, env, tok, cerJSON, "")

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	plain := openResponseLeg(t, requester, rec.Body.Bytes())
	if shnsdk.IsFramed(plain) {
		t.Fatalf("frame-incapable requester must get a bare body, got a v1 frame: %q", plain)
	}

	covered, _, err := shnsdk.ParseEligibilityResponse(plain)
	if err != nil {
		t.Fatalf("ParseEligibilityResponse: %v", err)
	}
	if !covered {
		t.Fatalf("covered = false, want true (MBR-PAYERB is in force)")
	}
	if strings.Contains(string(plain), "Organization/payer") {
		t.Fatalf("response still carries the deleted Organization/payer literal: %s", plain)
	}
	if !strings.Contains(string(plain), "00078") {
		t.Fatalf("response insurer does not name the Coverage-derived payor (00078): %s", plain)
	}
}

// noCoverageSoR wraps a SystemOfRecord and forces OpenCoverage to answer false for ONE
// named member (simulating the MBR-UC04/MBR-UC08-class fixture gap: the member resolves
// via ResolvePatient, but the payer partition has no Coverage row for them at all) —
// while ResolveByReference("Organization/payer") still resolves the payer's own
// well-known Organization, so the self-read insurer path has something to read.
type noCoverageSoR struct {
	SystemOfRecord
	member string
	orgRef string
	orgRaw []byte
}

func (s noCoverageSoR) OpenCoverage(memberID string) ([]byte, bool) {
	if memberID == s.member {
		return nil, false
	}
	return s.SystemOfRecord.OpenCoverage(memberID)
}

// CoverageInforce is wrapped too: a real fhirsor-backed SoR derives BOTH facts from the
// SAME absent Coverage row (fhirsor.SoR.CoverageInforce/OpenCoverage both search
// beneficiary=Patient/<id> and both come back empty) — the stub's CoverageInforce and
// OpenCoverage are independent reads (persona.inforce vs. a synthesized Coverage), so
// this wrapper couples them for the one member under test to reproduce that live shape.
func (s noCoverageSoR) CoverageInforce(memberID string) (bool, string) {
	if memberID == s.member {
		return false, ""
	}
	return s.SystemOfRecord.CoverageInforce(memberID)
}

func (s noCoverageSoR) ResolveByReference(ref string) ([]byte, bool) {
	if ref == s.orgRef {
		return s.orgRaw, true
	}
	return s.SystemOfRecord.ResolveByReference(ref)
}

var _ SystemOfRecord = noCoverageSoR{}

// TestEligibilityInbound_NoCoverageRow_AnswersNotCoveredWithSelfInsurer is the
// controller-ruled fix for a member with NO Coverage
// record at all (the member itself resolves — ResolvePatient succeeds — so this is a
// Coverage absence, not an unknown member) must answer a VALID not-covered
// CoverageEligibilityResponse, never a 422. There is no member Coverage to derive an
// insurer from, so the response's insurer instead names the payer's OWN well-known
// Organization (a self-read via SoR.ResolveByReference("Organization/payer") +
// shnsdk.ParseOrganizationIdentifier) — never the deleted "Organization/payer" literal,
// and never a hollow (insurer-less) response.
func TestEligibilityInbound_NoCoverageRow_AnswersNotCoveredWithSelfInsurer(t *testing.T) {
	g, requester := newInboundTestGateway(t, false)
	orgJSON := []byte(`{"resourceType":"Organization","id":"payer","name":"Test Payer","identifier":[{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"` + shnsdk.CMSPayerIdentity.Value + `"}]}`)
	g.cfg.SoR = noCoverageSoR{SystemOfRecord: g.cfg.SoR, member: "MBR-COVERED", orgRef: "Organization/payer", orgRaw: orgJSON}

	const member = "MBR-COVERED"
	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		t.Fatalf("fixture bug: %s not resolvable", member)
	}
	if _, hasCov := g.cfg.SoR.OpenCoverage(member); hasCov {
		t.Fatalf("fixture bug: %s must have no Coverage row for this test to be non-vacuous", member)
	}

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	cerJSON, err := shnsdk.BuildEligibilityRequest(member, "1234567890", now)
	if err != nil {
		t.Fatalf("BuildEligibilityRequest: %v", err)
	}
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: requester.ID, Recipient: "payer", TransactionType: "coverage-eligibility",
		AuthorityFrame: "provider-tpo", Timestamp: g.cfg.Clock().Format(time.RFC3339),
		CorrelationID: "corr-elig-2",
	}, cerJSON, g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tok := shnsdk.Token{Operation: "eligibility-inquiry", Subject: pci, CorrelationID: "corr-elig-2"}

	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.handleEligibilityInbound(rec, r, env, tok, cerJSON, "")

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (no-Coverage-row is a valid not-covered answer, not a 422); body=%s", rec.Code, rec.Body.String())
	}
	plain := openResponseLeg(t, requester, rec.Body.Bytes())
	covered, reason, err := shnsdk.ParseEligibilityResponse(plain)
	if err != nil {
		t.Fatalf("ParseEligibilityResponse: %v", err)
	}
	if covered {
		t.Fatalf("covered = true, want false (no Coverage row on file)")
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty (CoverageInforce's no-row answer, no disposition text)", reason)
	}
	if !strings.Contains(string(plain), shnsdk.CMSPayerIdentity.Value) {
		t.Fatalf("response insurer does not name the payer's own self-read identity (%s): %s", shnsdk.CMSPayerIdentity.Value, plain)
	}
}
