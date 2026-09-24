// bridgerefuse_test.go — direct unit tests for the kit-bridging-visualization demo's two
// designed-refusal reshape sites (task2 brief A3a, fix-round: the second live run showed
// the refusal can legitimately take EITHER of two shapes depending on infra posture):
//   - selectLegLineOrBridgeRefuse — the SELECTION-time *RouteRefusalError reshape
//     (selectLegLineOrFail's handleUC03Bridge-only sibling).
//   - handleUC03Bridge's own egressAdapt call site — the APPLY-time *SemanticChangeError
//     reshape (bridgeRefusalText/writeBridgeRefusal, shared by both sites).
//
// Both reshape into the SAME 200 structured body scenariodrive's Client.Post/RunCheck can
// actually assert against — Post treats ANY non-2xx as a transport failure and never hands
// the body to a Check's Want matcher, so a non-2xx refusal could never be pinned at all.
// Minimal literal Gateway construction (mirroring versionroute_test.go's
// TestSelectLegToken_RefusalIsLegible) — no HTTP substrate needed for the selection-time
// tests, since selectLegLine only consults g.cfg.Reg/DeclaredContractVersions/Validator*;
// the apply-time test drives the REAL chainFor/applyChain machinery (via g.egressAdapt)
// against the same vendored golden fixture transform_pas_test.go uses, so the
// *SemanticChangeError it reshapes is genuine, not a synthetic stand-in.
package engine

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestSelectLegLineOrBridgeRefuse_Success: own and peer share the pas-claim line — the
// ordinary case (the bridge-demo arm's own PAS declaration matches the shared line) —
// returns the route, writes nothing.
func TestSelectLegLineOrBridgeRefuse_Success(t *testing.T) {
	reg := shnsdk.NewRegistry()
	reg.Set("bridge-demo-gw", shnsdk.RegistryEntry{ID: "bridge-demo-gw", Role: "payer",
		ContractVersions: []string{shnsdk.ContractPAPAS20}})
	g := &Gateway{cfg: Config{Reg: reg, DeclaredContractVersions: []string{shnsdk.ContractPAPAS20}}}

	rec := httptest.NewRecorder()
	route, ok := g.selectLegLineOrBridgeRefuse(rec, "bridge-demo-gw", "pas-claim", "corr-1")
	if !ok {
		t.Fatalf("want ok=true, got false (body=%s)", rec.Body.String())
	}
	if route.Token != shnsdk.ContractPAPAS20 {
		t.Errorf("route.Token = %q, want %q", route.Token, shnsdk.ContractPAPAS20)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %s, want nothing written on success", rec.Body.String())
	}
}

// TestSelectLegLineOrBridgeRefuse_RouteRefusalError: own and peer share NO pas-claim line,
// and this build has no validator lane for the peer's declared line at all — so neither
// arm (2)'s native reach nor arm (3)'s transform chain can even be attempted (both gate on
// g.validatorForLine before considering a target line), and selectLegRoute fails closed
// with a *RouteRefusalError — exactly the shape provider-gw hits against the real deployed
// bridge-demo-refuse-gw (it declares no 2.1/2.2 validator lane of its own; see
// scenariodrive.BridgeChecks's doc comment). Asserts the EXACT 200 structured body task2's
// brief specifies, not just its absence of an error.
func TestSelectLegLineOrBridgeRefuse_RouteRefusalError(t *testing.T) {
	reg := shnsdk.NewRegistry()
	reg.Set("bridge-demo-refuse-gw", shnsdk.RegistryEntry{ID: "bridge-demo-refuse-gw", Role: "payer",
		ContractVersions: []string{shnsdk.ContractPAPAS22}})
	g := &Gateway{cfg: Config{Reg: reg, DeclaredContractVersions: []string{shnsdk.ContractPAPAS20}}}

	rec := httptest.NewRecorder()
	route, ok := g.selectLegLineOrBridgeRefuse(rec, "bridge-demo-refuse-gw", "pas-claim", "corr-2")
	if ok {
		t.Fatalf("want ok=false (a designed refusal), got route=%+v", route)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (the demo lane's own structured-refusal surface, never a non-2xx — "+
			"scenariodrive's Client.Post treats any non-2xx as a transport failure and never hands the "+
			"body to a Check's Want matcher)", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"refused":true`,
		`"refusedAt":"pas-claim"`,
		"no shared contract line for pa.pas (leg pas-claim)",
		"no configured validator lane for line 2.2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s, want it to contain %q", body, want)
		}
	}
}

// TestSelectLegLineOrBridgeRefuse_OnlyRouteRefusalErrorReshapes is the A3
// "must-not-pass-on-any-other-red" boundary asserted at the engine layer (complementing
// scenariodrive's own TestBridgeRefuseWant_RejectsImpostors, which asserts the SAME
// property from the check-matcher side): a leg whose contract this build's catalog does
// not even recognize returns a plain (non-RouteRefusalError) error from legContract, which
// selectLegLine propagates as-is — selectLegLineOrBridgeRefuse must NOT reshape that into
// the 200 refusal shape (it would misrepresent a caller bug as the designed exhibit).
func TestSelectLegLineOrBridgeRefuse_OnlyRouteRefusalErrorReshapes(t *testing.T) {
	g := &Gateway{cfg: Config{Reg: shnsdk.NewRegistry()}}
	rec := httptest.NewRecorder()
	_, ok := g.selectLegLineOrBridgeRefuse(rec, "bridge-demo-refuse-gw", "not-a-real-legtype", "corr-3")
	if ok {
		t.Fatal("want ok=false for an unknown legType")
	}
	if strings.Contains(rec.Body.String(), `"refused":true`) {
		t.Errorf("body = %s, an unknown-legType (caller bug) error must NOT be reshaped into the designed-refusal 200 body", rec.Body.String())
	}
	if rec.Code == 200 {
		t.Errorf("status = %d, an unknown-legType caller bug must not read as the designed refusal (200)", rec.Code)
	}
}

// TestBridgeRefusalText covers bridgeRefusalText's full discrimination contract (the
// fix-round's core new logic): both designed-refusal shapes extract their own Error()
// text (including through an fmt.Errorf %w wrap, the shape a real call chain produces),
// and any other error — the thing that must never be misread as "designed" — extracts
// nothing.
func TestBridgeRefusalText(t *testing.T) {
	rre := &RouteRefusalError{Contract: "pa.pas", LegType: "pas-claim", Recipient: "bridge-demo-refuse-gw",
		Own: []string{"pa.pas@2.0"}, Peer: []string{"pa.pas@2.2"}, BridgeIssue: "no configured validator lane for line 2.2"}
	sce := &SemanticChangeError{Contract: "pa.pas", From: "2.0", To: "2.1", Direction: "up",
		MissingElements: []string{"Claim.item.extension:certificationType"}}

	for _, tc := range []struct {
		name string
		err  error
		want string
		ok   bool
	}{
		{"RouteRefusalError", rre, rre.Error(), true},
		{"SemanticChangeError", sce, sce.Error(), true},
		{"RouteRefusalError wrapped (%w)", fmt.Errorf("egressAdapt: %w", rre), rre.Error(), true},
		{"SemanticChangeError wrapped (%w)", fmt.Errorf("applyChain: %w", sce), sce.Error(), true},
		{"a plain error must not reshape", errors.New("boom"), "", false},
		{"a RelayError must not reshape", &RelayError{Status: 502, Body: []byte("bad gateway")}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := bridgeRefusalText(tc.err)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (err=%v)", ok, tc.ok, tc.err)
			}
			if got != tc.want {
				t.Errorf("text = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWriteBridgeRefusal pins the exact 200 body shape task2's brief specifies.
func TestWriteBridgeRefusal(t *testing.T) {
	rec := httptest.NewRecorder()
	writeBridgeRefusal(rec, "pas-claim", "shn: semantic-change refusal: pa.pas 2.0->2.1 (up direction): no honest byte-level source for Claim.item.extension:certificationType")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"refused":true`,
		`"refusedAt":"pas-claim"`,
		// NOT "2.0->2.1" — writeJSON's json.Encoder HTML-escapes ">" to > on the wire
		// (Go's json package's default SetEscapeHTML(true)), so a matcher (or a test
		// asserting the raw body bytes, as scenariodrive.Has does) that includes the arrow
		// literally can never match. "semantic-change refusal: pa.pas" alone is what
		// BridgeChecks's own refuseWant pins, for exactly this reason.
		"semantic-change refusal: pa.pas",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s, want it to contain %q", body, want)
		}
	}
}

// TestHandleUC03BridgeEgressAdapt_SemanticChangeErrorReshapesTo200 is the fix-round's
// apply-time hermetic proof, driven against the REAL chainFor/applyChain machinery (the
// same vendored golden fixture and up-direction gated step transform_pas_test.go's
// TestPasStep2021Up_RequestDirectionGated already proves refuses) rather than a synthetic
// error — g.egressAdapt only needs route.Chain/payload/x; g.observe is nil-safe on a
// zero-value Config, so no HTTP substrate is needed. This is exactly the shape the SECOND
// live run's fix (SHN_DEMO_EGRESS_NATIVE_LINES) is meant to force handleUC03Bridge's own
// pas-claim leg into: a chain IS selected (2.0->2.2 spans the gated 2.0->2.1 up-step), and
// it refuses at APPLY time, never at selection.
func TestHandleUC03BridgeEgressAdapt_SemanticChangeErrorReshapesTo200(t *testing.T) {
	steps := chainFor("pa.pas", "2.0", "2.2")
	if len(steps) == 0 {
		t.Fatal("chainFor(pa.pas, 2.0, 2.2) returned no steps — the manifest changed under this test")
	}
	route := legRoute{Token: "pa.pas@2.2", BuildLine: "2.0", Chain: steps}
	// A request Bundle containing a Claim entry — pasStep2021Up's up-direction gate has no
	// honest 2.1-mandatory source for it (transform_pas_test.go's own control fixture).
	payload := pasGolden(t, "conformant/pas-submit-request.json")

	g := &Gateway{}
	_, _, err := g.egressAdapt(route, payload, ExchangeIdentity{CorrelationID: "corr-bridge-refuse", LegType: "pas-claim", Counterpart: "bridge-demo-refuse-gw"})
	if err == nil {
		t.Fatal("want a semantic-change refusal from the real 2.0->2.2 chain, got success")
	}
	var sce *SemanticChangeError
	if !errors.As(err, &sce) {
		t.Fatalf("want *SemanticChangeError (errors.As), got %T: %v", err, err)
	}
	if sce.Contract != "pa.pas" || sce.From != "2.0" || sce.To != "2.1" || sce.Direction != "up" {
		t.Fatalf("unexpected error fields: %+v", sce)
	}

	text, ok := bridgeRefusalText(err)
	if !ok {
		t.Fatalf("bridgeRefusalText did not reshape a genuine *SemanticChangeError: %v", err)
	}
	if text != sce.Error() {
		t.Errorf("text = %q, want the error's own text %q", text, sce.Error())
	}

	rec := httptest.NewRecorder()
	writeBridgeRefusal(rec, "pas-claim", text)
	body := rec.Body.String()
	for _, want := range []string{
		`"refused":true`,
		`"refusedAt":"pas-claim"`,
		// NOT "2.0->2.1" — writeJSON's json.Encoder HTML-escapes ">" to > on the wire
		// (Go's json package's default SetEscapeHTML(true)), so a matcher (or a test
		// asserting the raw body bytes, as scenariodrive.Has does) that includes the arrow
		// literally can never match. "semantic-change refusal: pa.pas" alone is what
		// BridgeChecks's own refuseWant pins, for exactly this reason.
		"semantic-change refusal: pa.pas",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s, want it to contain %q", body, want)
		}
	}
}
