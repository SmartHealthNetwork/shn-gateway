// egressadapt_test.go — g.egressAdapt: the pipeline between build
// and validate that applies route's transform chain (if any), builds the
// transform Provenance, and emits leg.transformed. See versionroute_test.go for
// the route-SELECTION tests (arms 1-3) this pipeline consumes.
package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

var fixedEgressClock = func() time.Time { return time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC) }

// TestEgressAdaptNilChainIsPassthrough: arms (1)/(2) carry route.Chain==nil
// — egressAdapt must be a pure pass-through (no Provenance built, no
// observer event emitted). This is the production-mesh canary: the default
// answers-byte-stable behavior depends on this branch never touching bytes.
func TestEgressAdaptNilChainIsPassthrough(t *testing.T) {
	var observed []ObserverEvent
	g := &Gateway{cfg: Config{
		HolderID: "test-holder", Clock: fixedEgressClock,
		Observer: func(e ObserverEvent) { observed = append(observed, e) },
	}}
	route := legRoute{Token: "pa.pas@2.0", BuildLine: "2.0", Chain: nil}
	in := []byte(`{"resourceType":"Bundle"}`)
	out, reports, err := g.egressAdapt(route, in, ExchangeIdentity{CorrelationID: "corr-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(in) {
		t.Fatalf("nil-chain egressAdapt must be byte-identical: got %s, want %s", out, in)
	}
	if reports != nil {
		t.Fatalf("nil-chain must produce no LossReports, got %+v", reports)
	}
	if len(observed) != 0 {
		t.Fatalf("nil-chain must emit NO observer event (no transform ran), got %+v", observed)
	}
}

// TestEgressAdaptValidatesAtTargetLane: a chain output deliberately
// corrupted by a stub step must fail the caller's EXISTING
// validateFHIR(..., "egress", LineOf(route.Token)) at the TARGET lane —
// transformed-output-invalid is a hard failure at that surface, never a
// silent seal (the spec's own rejection row). This test proves the SURFACE
// (egressAdapt's output fails validate); "nothing seals" is proven at every
// one of the nine select-before-build call sites by the ordinary
// `if status, msg := g.validateFHIR(...); status != 0 { writeJSON(...); return
// }` early-return that immediately follows the egressAdapt call there (e.g.
// originate.go's handleUC03/handleUC07HCPCS/handleUC04/handleUC05/handleUC08,
// pas_tail.go's submitClaimAndResolve, originate_resume.go's scenarioToPend/
// completeClinician/completePatient) — the SAME pattern every one of those
// sites already used for its own build errors, unchanged by egress adaptation.
func TestEgressAdaptValidatesAtTargetLane(t *testing.T) {
	const corruptMarker = "CORRUPTED-BY-STUB-STEP"
	corrupt := func(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
		return []byte(`{"resourceType":"Bundle","` + corruptMarker + `":true}`),
			LossReport{Module: "pa.pas 2.0->2.1", Source: "2.0", Target: "2.1"}, nil
	}
	step := CompatStep{Contract: "pa.pas", From: "2.0", To: "2.1", Class: StepFull, Up: corrupt, Down: corrupt}
	route := legRoute{Token: "pa.pas@2.1", BuildLine: "2.0", Chain: []CompatStep{step}}

	g := &Gateway{cfg: Config{
		HolderID: "test-holder", Clock: fixedEgressClock,
		Validator:        shnsdk.NewFakeValidator(),
		ValidatorsByLine: map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator(), "2.1": &shnsdk.FakeValidator{RejectIfContains: corruptMarker}},
	}}

	adapted, _, err := g.egressAdapt(route, []byte(`{"resourceType":"Bundle"}`), ExchangeIdentity{CorrelationID: "corr-2"})
	if err != nil {
		t.Fatalf("egressAdapt itself must not error on a structurally-valid (if semantically corrupted) stub output: %v", err)
	}
	status, msg := g.validateFHIR(context.Background(), adapted, "egress", shnsdk.LineOf(route.Token))
	if status == 0 {
		t.Fatal("corrupted chain output must fail the TARGET-lane egress validate — nothing may seal")
	}
	if msg == "" {
		t.Fatal("want a non-empty validation failure message")
	}
}

// TestEgressAdaptStampsTargetToken: the frame/stamp carries pa.pas@2.2
// (route.Token) while BuildLine was 2.0 — the wire-truth line a caller
// stamps onto Content.ProfileID is ALWAYS route.Token, never route.BuildLine
// (the frame semantics are unchanged by construction). Drives the REAL pa.pas
// 2.0->2.2 chain (two wired steps) over a real golden fixture.
func TestEgressAdaptStampsTargetToken(t *testing.T) {
	steps := chainFor("pa.pas", "2.0", "2.2")
	if len(steps) != 2 {
		t.Fatalf("want a 2-hop pa.pas 2.0->2.2 chain, got %d steps: %+v", len(steps), steps)
	}
	route := legRoute{Token: "pa.pas@2.2", BuildLine: "2.0", Chain: steps}
	if route.BuildLine == shnsdk.LineOf(route.Token) {
		t.Fatal("fixture invalid: BuildLine must differ from the target line to prove the stamp/build split")
	}

	in := pasGolden(t, "claimresponse-approved.json") // real 2.0 golden, bare ClaimResponse
	var observed []ObserverEvent
	g := &Gateway{cfg: Config{
		HolderID: "test-holder", Clock: fixedEgressClock,
		Observer: func(e ObserverEvent) { observed = append(observed, e) },
	}}

	adapted, reports, err := g.egressAdapt(route, in, ExchangeIdentity{CorrelationID: "corr-3"})
	if err != nil {
		t.Fatalf("egressAdapt: unexpected error: %v", err)
	}
	if len(adapted) == 0 {
		t.Fatal("adapted bytes must be non-empty")
	}
	if len(reports) != 2 {
		t.Fatalf("want one LossReport per chained step, got %d: %+v", len(reports), reports)
	}
	if reports[0].Module != "pa.pas 2.0->2.1" || reports[1].Module != "pa.pas 2.1->2.2" {
		t.Fatalf("report module trace = %+v, want the two-hop chain in order", reports)
	}
	// The wire-truth check itself: this is what every call site stamps onto
	// Content.ProfileID — it is route.Token, computed once at selection, and
	// egressAdapt never mutates it.
	if route.Token != "pa.pas@2.2" {
		t.Fatalf("route.Token = %q, want pa.pas@2.2 (unchanged by egressAdapt)", route.Token)
	}

	// leg.transformed observed, Hub-blind: Detail names the chain,
	// CorrelationID threads through, and since no contract has in-payload
	// tolerance evidence yet (the safe default), the Provenance rides the
	// observer Payload — never the wire.
	if len(observed) != 1 || observed[0].Kind != legTransformedKind {
		t.Fatalf("observed = %+v, want exactly one leg.transformed event", observed)
	}
	ev := observed[0]
	if ev.CorrelationID != "corr-3" {
		t.Fatalf("ObserverEvent.CorrelationID = %q, want corr-3", ev.CorrelationID)
	}
	if ev.Detail == "" {
		t.Fatal("ObserverEvent.Detail must name the module chain")
	}
	if len(ev.Payload) == 0 {
		t.Fatal("observer-only Provenance carriage: ev.Payload must carry the built Provenance")
	}
}

// TestEgressAdaptFillsPromisedFields: egressAdapt fills the three
// leg.transformed fields transformedObserverEvent leaves for its caller
// (transform.go's promise) — LegType/Counterpart copied straight from x, and
// AuthorityFrame looked up from the SAME paCatalog OriginateLeg reads (the
// dtr-questionnaire-fetch row's ReqFrame, "provider-tpo"). NOTE (a later
// review finding): dtr-questionnaire-fetch is in envelopeEgressLegs, so since
// the envelope carve-out this exercises the ENVELOPE PASS-THROUGH branch
// of egressAdapt (envelopeChainReports), not applyChain — the field-filling
// promise is branch-independent (both branches feed transformedObserverEvent),
// and the chain-invoking branch's leg.transformed fields are pinned live by
// the kitlive bridged-run assertions (test/kitlive). The fields land on the
// SAME event TestEgressAdaptStampsTargetToken already pins
// CorrelationID/Detail/Payload on.
func TestEgressAdaptFillsPromisedFields(t *testing.T) {
	steps := chainFor("pa.dtr", "2.1", "2.2")
	if len(steps) != 1 {
		t.Fatalf("want a 1-hop pa.dtr 2.1->2.2 chain, got %d steps: %+v", len(steps), steps)
	}
	route := legRoute{Token: "pa.dtr@2.2", BuildLine: "2.1", Chain: steps}
	in := pasGolden(t, "2.1/questionnaireresponse-autofill.json")

	var observed []ObserverEvent
	g := &Gateway{cfg: Config{
		HolderID: "test-holder", Clock: fixedEgressClock,
		Observer: func(e ObserverEvent) { observed = append(observed, e) },
	}}

	x := ExchangeIdentity{CorrelationID: "corr-1", LegType: "dtr-questionnaire-fetch", Counterpart: "payer"}
	if _, _, err := g.egressAdapt(route, in, x); err != nil {
		t.Fatalf("egressAdapt: unexpected error: %v", err)
	}

	if len(observed) != 1 || observed[0].Kind != legTransformedKind {
		t.Fatalf("observed = %+v, want exactly one leg.transformed event", observed)
	}
	ev := observed[0]
	if ev.LegType != x.LegType {
		t.Fatalf("ev.LegType = %q, want %q", ev.LegType, x.LegType)
	}
	if ev.Counterpart != x.Counterpart {
		t.Fatalf("ev.Counterpart = %q, want %q", ev.Counterpart, x.Counterpart)
	}
	wantFrame := paCatalog[x.LegType].ReqFrame
	if wantFrame == "" {
		t.Fatal("fixture invalid: paCatalog[dtr-questionnaire-fetch].ReqFrame must be non-empty")
	}
	if ev.AuthorityFrame != wantFrame {
		t.Fatalf("ev.AuthorityFrame = %q, want the catalog's ReqFrame %q", ev.AuthorityFrame, wantFrame)
	}
}

// TestEgressAdaptFillsPromisedFieldsChainInvoking is
// TestEgressAdaptFillsPromisedFields's companion for the OTHER branch:
// gateway.go:1226's AuthorityFrame fill (`if spec, ok :=
// paCatalog[x.LegType]; ok { ev.AuthorityFrame = spec.ReqFrame }`) sits
// AFTER the envelope/applyChain if-else — it is shared post-branch code,
// not branch-local — so the gap this test closes is unit COVERAGE of the
// chain-invoking (applyChain) path, not a missing fill. Drives crd-order-select
// (pa.crd), which is NOT in envelopeEgressLegs (TestCRDLegsNeverJoinEnvelopeCarveOut
// pins that; do not add it there), over the real 2.0->2.2 identity chain
// (compat.go's nil-Up/Down pa.crd rows) so applyChain — not
// envelopeChainReports — is the function that actually runs.
func TestEgressAdaptFillsPromisedFieldsChainInvoking(t *testing.T) {
	steps := chainFor("pa.crd", "2.0", "2.2")
	if len(steps) != 2 {
		t.Fatalf("want a 2-hop pa.crd 2.0->2.2 chain, got %d steps: %+v", len(steps), steps)
	}
	route := legRoute{Token: "pa.crd@2.2", BuildLine: "2.0", Chain: steps}
	in := []byte(`{"hook":"order-select","hookInstance":"hi-1","context":{"patientId":"MBR-COVERED"}}`)

	var observed []ObserverEvent
	g := &Gateway{cfg: Config{
		HolderID: "test-holder", Clock: fixedEgressClock,
		Observer: func(e ObserverEvent) { observed = append(observed, e) },
	}}

	x := ExchangeIdentity{CorrelationID: "corr-5", LegType: "crd-order-select", Counterpart: "payer-crd22"}
	if _, _, err := g.egressAdapt(route, in, x); err != nil {
		t.Fatalf("egressAdapt: unexpected error: %v", err)
	}

	if len(observed) != 1 || observed[0].Kind != legTransformedKind {
		t.Fatalf("observed = %+v, want exactly one leg.transformed event", observed)
	}
	ev := observed[0]
	wantFrame := paCatalog[x.LegType].ReqFrame
	if wantFrame == "" {
		t.Fatal("fixture invalid: paCatalog[crd-order-select].ReqFrame must be non-empty")
	}
	if ev.AuthorityFrame != wantFrame {
		t.Fatalf("ev.AuthorityFrame = %q, want the catalog's ReqFrame %q", ev.AuthorityFrame, wantFrame)
	}
}

// TestEnvelopeLegChainIsByteIdenticalPassThrough (the envelope-carve-out
// obligation discharge): dtr-questionnaire-fetch is a transport ENVELOPE, not FHIR
// content the pa.dtr compat-manifest rows model — running it through
// applyChain would hand its bytes to dtrStep2122Up, which
// json.Unmarshal-then-json.Marshal's the payload (transform_dtr.go's
// dtrStep2122Up/dtrStep2122Down have no return-input path) and therefore
// re-sorts its JSON object keys. This probe payload — a REAL golden that
// step WOULD rewrite — is the enforcement fence: byte-equality between out
// and in is only possible if the step funcs never ran (a runtime
// !bytes.Equal guard after `out = payload` would be
// structurally unreachable, a detector nothing can trip, so there is none;
// this test IS the fence, and doubles as the adversarial row: any refactor
// that routes envelope legs back through the modules re-marshals the
// fixture's keys and fails this loudly).
func TestEnvelopeLegChainIsByteIdenticalPassThrough(t *testing.T) {
	steps := chainFor("pa.dtr", "2.0", "2.2")
	if len(steps) != 2 {
		t.Fatalf("want a 2-hop pa.dtr 2.0->2.2 chain, got %d steps: %+v", len(steps), steps)
	}
	route := legRoute{Token: "pa.dtr@2.2", BuildLine: "2.0", Chain: steps}
	// A real QR golden dtrStep2122Up would rewrite (key-reorder via its
	// unmarshal/marshal round trip) if the chain actually ran.
	in := pasGolden(t, "2.1/questionnaireresponse-autofill.json")

	var observed []ObserverEvent
	g := &Gateway{cfg: Config{
		HolderID: "test-holder", Clock: fixedEgressClock,
		Observer: func(e ObserverEvent) { observed = append(observed, e) },
	}}

	x := ExchangeIdentity{CorrelationID: "corr-envelope", LegType: "dtr-questionnaire-fetch", Counterpart: "payer"}
	out, reports, err := g.egressAdapt(route, in, x)
	if err != nil {
		t.Fatalf("egressAdapt: unexpected error: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("envelope leg must be byte-IDENTICAL pass-through (proves the step funcs never ran): got %s, want %s", out, in)
	}

	if len(reports) != 2 {
		t.Fatalf("want one synthesized LossReport per chained step, got %d: %+v", len(reports), reports)
	}
	wantReports := []LossReport{
		{Module: "pa.dtr 2.0->2.1", Source: "2.0", Target: "2.1"},
		{Module: "pa.dtr 2.1->2.2", Source: "2.1", Target: "2.2"},
	}
	for i, want := range wantReports {
		if reports[i].Module != want.Module || reports[i].Source != want.Source || reports[i].Target != want.Target {
			t.Fatalf("reports[%d] = %+v, want %+v", i, reports[i], want)
		}
		if len(reports[i].Carried) != 0 || len(reports[i].Synthesized) != 0 {
			t.Fatalf("reports[%d] must be empty-content (no Carried/Synthesized — bytes untouched), got %+v", i, reports[i])
		}
	}

	// leg.transformed still fires — the live machinery story stays honest
	// (spec §4b) even though the bytes never moved.
	if len(observed) != 1 || observed[0].Kind != legTransformedKind {
		t.Fatalf("observed = %+v, want exactly one leg.transformed event", observed)
	}
	if observed[0].CorrelationID != x.CorrelationID || observed[0].LegType != x.LegType || observed[0].Counterpart != x.Counterpart {
		t.Fatalf("observed[0] identity fields = %+v, want CorrelationID/LegType/Counterpart from x (%+v)", observed[0], x)
	}
}

// TestCRDLegsNeverJoinEnvelopeCarveOut: the
// envelope carve-out is DTR-fetch only. A CRD leg type must never land in
// envelopeEgressLegs — CRD's arm-3 byte-identity rests on the identity
// chain genuinely RUNNING (TestD7CRDArm3IdentityChainIsBytePreserving), and
// membership here would misname CDS Hooks payloads as envelopes and
// silently bypass a future real CRD module.
func TestCRDLegsNeverJoinEnvelopeCarveOut(t *testing.T) {
	for _, crdLeg := range []string{"crd-order-select", "crd-order-dispatch"} {
		if envelopeEgressLegs[crdLeg] {
			t.Fatalf("envelopeEgressLegs[%q] = true, want false — CRD legs must never join the envelope carve-out", crdLeg)
		}
	}
	if !envelopeEgressLegs["dtr-questionnaire-fetch"] {
		t.Fatal(`envelopeEgressLegs["dtr-questionnaire-fetch"] = false, want true`)
	}
	if len(envelopeEgressLegs) != 1 {
		t.Fatalf("envelopeEgressLegs has %d entries, want exactly 1 (dtr-questionnaire-fetch only): %+v", len(envelopeEgressLegs), envelopeEgressLegs)
	}
}

// TestEgressAdaptRefusalEmitsLegFailed: today an egressAdapt refusal
// is observer-SILENT (all 14 call sites precede roundTrip, the only
// leg.failed choke point) — spec §1a names this a §6-grade honesty gap. This
// emission is NEW, tested in the same change. Drives a REAL gated chain
// (chainFor("pa.pas","2.0","2.2")) on a Claim-shaped payload — pasStep2021Up's
// unconditional refusal (transform_pas.go) — and asserts exactly one
// leg.failed observed, with Route carrying the attempted 2-step chain
// (classes gated,carry, walk-direction {2.0->2.1, 2.1->2.2} per
// routeInfoFor) + Detail containing "semantic-change refusal" + LegType/
// Counterpart from x — and NO leg.transformed observed (applyChain aborts
// before egressAdapt's transformedObserverEvent branch ever runs).
func TestEgressAdaptRefusalEmitsLegFailed(t *testing.T) {
	steps := chainFor("pa.pas", "2.0", "2.2")
	if len(steps) != 2 {
		t.Fatalf("want a 2-hop pa.pas 2.0->2.2 chain, got %d steps: %+v", len(steps), steps)
	}
	route := legRoute{Token: "pa.pas@2.2", BuildLine: "2.0", Chain: steps}
	in := []byte(`{"resourceType":"Claim","id":"c1"}`) // Claim-shaped ⇒ pasStep2021Up refuses unconditionally

	var observed []ObserverEvent
	g := &Gateway{cfg: Config{
		HolderID: "test-holder", Clock: fixedEgressClock,
		Observer: func(e ObserverEvent) { observed = append(observed, e) },
	}}

	x := ExchangeIdentity{CorrelationID: "corr-refuse", LegType: "pas-claim", Counterpart: "payer-x"}
	out, reports, err := g.egressAdapt(route, in, x)
	if err == nil {
		t.Fatal("want an error (gated chain must refuse), got nil")
	}
	var sce *SemanticChangeError
	if !errors.As(err, &sce) {
		t.Fatalf("want a *SemanticChangeError, got %T: %v", err, err)
	}
	if out != nil || reports != nil {
		t.Fatalf("aborted chain must return nil bytes/reports, got out=%q reports=%+v", out, reports)
	}

	var failed, transformed int
	var ev ObserverEvent
	for _, e := range observed {
		switch e.Kind {
		case "leg.failed":
			failed++
			ev = e
		case legTransformedKind:
			transformed++
		}
	}
	if failed != 1 {
		t.Fatalf("want exactly one leg.failed event, got %d (observed: %+v)", failed, observed)
	}
	if transformed != 0 {
		t.Fatalf("want NO leg.transformed event on a refused chain, got %d", transformed)
	}
	if ev.Direction != "originate" {
		t.Fatalf("ev.Direction = %q, want originate", ev.Direction)
	}
	if ev.LegType != x.LegType {
		t.Fatalf("ev.LegType = %q, want %q", ev.LegType, x.LegType)
	}
	if ev.CorrelationID != x.CorrelationID {
		t.Fatalf("ev.CorrelationID = %q, want %q", ev.CorrelationID, x.CorrelationID)
	}
	if ev.Counterpart != x.Counterpart {
		t.Fatalf("ev.Counterpart = %q, want %q", ev.Counterpart, x.Counterpart)
	}
	if !strings.Contains(ev.Detail, "semantic-change refusal") {
		t.Fatalf("ev.Detail = %q, want it to contain %q", ev.Detail, "semantic-change refusal")
	}
	if ev.Route == nil {
		t.Fatal("ev.Route is nil, want the attempted chain's RouteInfo")
	}
	if ev.Route.Token != route.Token || ev.Route.BuildLine != route.BuildLine {
		t.Fatalf("ev.Route Token/BuildLine = %q/%q, want %q/%q", ev.Route.Token, ev.Route.BuildLine, route.Token, route.BuildLine)
	}
	if len(ev.Route.Chain) != 2 {
		t.Fatalf("ev.Route.Chain has %d steps, want 2 (the attempted chain, up to its refusal point)", len(ev.Route.Chain))
	}
	wantSteps := []ChainStep{
		{Module: "pa.pas 2.0->2.1", From: "2.0", To: "2.1", Class: "gated"},
		{Module: "pa.pas 2.1->2.2", From: "2.1", To: "2.2", Class: "carry"},
	}
	for i, want := range wantSteps {
		if ev.Route.Chain[i] != want {
			t.Fatalf("ev.Route.Chain[%d] = %+v, want %+v", i, ev.Route.Chain[i], want)
		}
	}
}
