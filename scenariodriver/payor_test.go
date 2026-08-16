// payor_test.go — the member→payor fence.
//
// The conformant lane routes payload-FIRST (AI-G13): the gateway derives the
// payer holder from the Coverage carried in the INBOUND request. So a
// driver-built Coverage that named the wrong payer would not fail — it would
// silently route the run to the wrong payer holder. These rows are the
// rejection tests for that: the mapping is asserted per member on the real
// built bodies, and the duplicated demo identities are pinned against the
// engine's exported ones.
package scenariodriver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// TestPayorOrgs_MatchEngineIdentities fences payorOrgs' duplicated literals
// against gateway/engine's exported demo identities. The duplication is
// deliberate (payorOrgs' own doc: this package must not pull the engine into a
// partner's binary) — the import here is TEST-only, so the values can never
// drift apart unnoticed.
func TestPayorOrgs_MatchEngineIdentities(t *testing.T) {
	for member, want := range map[string]shnsdk.PayerIdentifier{
		"MBR-BRIDGE-DEMO":   engine.BridgeDemoPayerID,
		"MBR-BRIDGE-REFUSE": engine.BridgeRefusePayerID,
	} {
		got := payorOrgFor(member).id
		if got != want {
			t.Errorf("payorOrgFor(%q).id = %+v, want %+v (engine's exported identity)", member, got, want)
		}
	}
	if got := payorOrgFor("MBR-COVERED").id; got != shnsdk.CMSPayerIdentity {
		t.Errorf("payorOrgFor(%q).id = %+v, want the CMS default %+v", "MBR-COVERED", got, shnsdk.CMSPayerIdentity)
	}
}

// TestBuildCRDRequest_PayorFollowsMember: the built prefetch Coverage resolves
// (through the SAME shnsdk.ParsePayerIdentifier the ingress routes with) to the
// member's own payer — CMS for an ordinary member, the demo identity for a demo
// member. The mutation row is the third one: a demo member that still resolved
// to CMS is exactly the silent misroute this fence exists for.
func TestBuildCRDRequest_PayorFollowsMember(t *testing.T) {
	for member, want := range map[string]shnsdk.PayerIdentifier{
		"MBR-COVERED":       shnsdk.CMSPayerIdentity,
		"MBR-BRIDGE-DEMO":   engine.BridgeDemoPayerID,
		"MBR-BRIDGE-REFUSE": engine.BridgeRefusePayerID,
	} {
		body, err := BuildCRDRequest(member, SystemHCPCS, "L8000", "Breast prosthesis, mastectomy bra")
		if err != nil {
			t.Fatalf("BuildCRDRequest(%q): %v", member, err)
		}
		var req struct {
			Prefetch struct {
				Coverage json.RawMessage `json:"coverage"`
			} `json:"prefetch"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("BuildCRDRequest(%q): decode: %v", member, err)
		}
		got, ok := shnsdk.ParsePayerIdentifier(req.Prefetch.Coverage, nil)
		if !ok {
			t.Fatalf("BuildCRDRequest(%q): prefetch coverage carries no resolvable payer identifier", member)
		}
		if got != want {
			t.Errorf("BuildCRDRequest(%q): payer = %+v, want %+v", member, got, want)
		}
	}
}

// TestBuildQuestionnairePackageRequest_PayorFollowsMember is the same fence for
// the DTR $questionnaire-package Parameters (its contained payor is what the
// DTR ingress routes off).
func TestBuildQuestionnairePackageRequest_PayorFollowsMember(t *testing.T) {
	for member, want := range map[string]shnsdk.PayerIdentifier{
		"MBR-COVERED":       shnsdk.CMSPayerIdentity,
		"MBR-BRIDGE-DEMO":   engine.BridgeDemoPayerID,
		"MBR-BRIDGE-REFUSE": engine.BridgeRefusePayerID,
	} {
		body, err := BuildQuestionnairePackageRequest("http://example.org/Questionnaire/q1", member)
		if err != nil {
			t.Fatalf("BuildQuestionnairePackageRequest(%q): %v", member, err)
		}
		var params struct {
			Parameter []struct {
				Name     string          `json:"name"`
				Resource json.RawMessage `json:"resource"`
			} `json:"parameter"`
		}
		if err := json.Unmarshal(body, &params); err != nil {
			t.Fatalf("BuildQuestionnairePackageRequest(%q): decode: %v", member, err)
		}
		var cov json.RawMessage
		for _, p := range params.Parameter {
			if p.Name == "coverage" {
				cov = p.Resource
			}
		}
		if len(cov) == 0 {
			t.Fatalf("BuildQuestionnairePackageRequest(%q): no coverage parameter", member)
		}
		got, ok := shnsdk.ParsePayerIdentifier(cov, nil)
		if !ok {
			t.Fatalf("BuildQuestionnairePackageRequest(%q): coverage carries no resolvable payer identifier", member)
		}
		if got != want {
			t.Errorf("BuildQuestionnairePackageRequest(%q): payer = %+v, want %+v", member, got, want)
		}
	}
}

// TestBuildPASBundle_MemberPayorFence is the rejection test for the silent-misroute defect:
// BuildPASBundle used to route EVERY member's Claim $submit through AddRoutablePayor's hardcoded
// CMS payor, regardless of what payorOrgFor(member) actually resolves to — so a bridge-demo
// member's PAS submission would silently be stamped with the CMS payer identifier and routed to
// the wrong payer holder. The fix fails closed: BuildPASBundle now checks payorOrgFor(member)
// itself and REJECTS any member it doesn't resolve to cmsPayorOrg, loudly, instead of silently
// mis-stamping. The control row proves the ordinary CMS-member path is byte-for-byte unchanged by
// the fence (comparing against the un-fenced RebindPASPatient+AddRoutablePayor composition
// directly, which is exactly what BuildPASBundle did before this defect was fixed).
func TestBuildPASBundle_MemberPayorFence(t *testing.T) {
	t.Run("CMS member unaffected (control row)", func(t *testing.T) {
		rebound, err := RebindPASPatient(PASApproveGolden(), "MBR-COVERED")
		if err != nil {
			t.Fatalf("RebindPASPatient: %v", err)
		}
		want, err := AddRoutablePayor(rebound)
		if err != nil {
			t.Fatalf("AddRoutablePayor: %v", err)
		}
		got, err := BuildPASBundle(PASApproveGolden(), "MBR-COVERED")
		if err != nil {
			t.Fatalf("BuildPASBundle(MBR-COVERED): %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("fence altered the CMS-member path — control row must be byte-unchanged:\ngot:  %s\nwant: %s", got, want)
		}
	})

	for _, member := range []string{"MBR-BRIDGE-DEMO", "MBR-BRIDGE-REFUSE"} {
		t.Run(member+" rejected (fail-closed fence)", func(t *testing.T) {
			out, err := BuildPASBundle(PASApproveGolden(), member)
			if err == nil {
				t.Fatalf("BuildPASBundle(%q): want error (non-CMS member silently CMS-stamped), got nil bundle: %s", member, out)
			}
			wantMsg := fmt.Sprintf(
				"scenariodriver: BuildPASBundle routes via AddRoutablePayor's CMS payor; member %q resolves to %q — non-CMS PAS routing is not implemented (fail-closed fence)",
				member, payorOrgFor(member).name,
			)
			if err.Error() != wantMsg {
				t.Fatalf("error = %q, want %q", err.Error(), wantMsg)
			}
		})
	}
}
