//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Module 2's bridged check reads the bytes it changed against the target
// line's own profile for their species and leg (profileFor, the map
// certification uses), not base FHIR alone, through the reading the line's
// lane needs (validateAt: a PAS request Bundle at 2.1 and 2.2 per entry).
// Bytes SHN translated are SHN's own edit, so a translation that is valid
// FHIR but not valid at the target line's profile is refused at every level,
// as a fidelity refusal at the verify stage. A code the lane could not check
// (the unchecked reading) is no defect of the translation, and is recorded,
// not refused. Every row here runs against a stub lane; the real-lane rows
// are the platform's conformance suite's, under make validate.

// bridgedLane is a target line's validator lane that answers res to every
// call; it records the resource type of each body and the profile it was
// asked for.
type bridgedLane struct {
	mu     sync.Mutex
	asked  []string
	bodies [][]byte
	res    shnsdk.Result
}

func (l *bridgedLane) Validate(_ context.Context, body []byte, profile string) (shnsdk.Result, error) {
	l.mu.Lock()
	l.asked = append(l.asked, resourceTypeOf(body)+" at "+profile)
	l.bodies = append(l.bodies, append([]byte(nil), body...))
	l.mu.Unlock()
	return l.res, nil
}

func (l *bridgedLane) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.asked...)
}

// bridgedRequestWalk is a 2.1 PAS request Bundle, with edit applied, walked
// to 2.2 along the registered chain (the 2.0->2.1 up-step refuses every
// request, so a walked request is a 2.1 one).
func bridgedRequestWalk(t *testing.T, edit func(b map[string]any)) (legRoute, []byte) {
	t.Helper()
	steps := chainFor("pa.pas", "2.1", "2.2")
	if len(steps) == 0 {
		t.Fatal("fixture: no pa.pas 2.1->2.2 chain")
	}
	return legRoute{Token: "pa.pas@2.2", BuildLine: "2.1", Chain: steps}, requestBundle(t, "2.1", edit)
}

func bridgedBuild(ctx context.Context, t *testing.T, g *Gateway, leg string, edit func(b map[string]any)) (int, string) {
	t.Helper()
	route, in := bridgedRequestWalk(t, edit)
	_, status, msg, err := g.egressBuild(ctx, route, ExchangeIdentity{CorrelationID: "corr-1135", LegType: leg},
		moduleBuild{Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: in, Contract: "pa.pas"})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return status, msg
}

// asUpdate makes the request an update: its Claim names the Claim it
// changes, which it carries beside itself.
func asUpdate(t *testing.T) func(b map[string]any) {
	return func(b map[string]any) {
		prior := anotherClaim(t, b, "prior-claim")
		resourceAt(b, 0)["related"] = []any{map[string]any{"claim": map[string]any{"reference": "Claim/prior-claim"}}}
		b["entry"] = append(entries(b), prior)
	}
}

// The bridged check reads the walked request at the target line's profile for
// the leg. A submission or update at 2.2 is read per entry: one call with no
// profile asked, the Bundle without the request Bundle profile and each Claim
// declaring the candidate it is built to. An inquiry is read at its request
// Bundle profile.
func TestBridgedCheck_ReadsTheTargetLinesProfile(t *testing.T) {
	submit, update := claimProfile(t, "2.2", "pas-claim"), claimProfile(t, "2.2", "pas-claim-update")
	inquiry, _ := profileFor("PASRequestBundle", "2.2", "pas-claim-inquire")
	for name, c := range map[string]struct {
		leg    string
		edit   func(b map[string]any)
		want   []string
		claims [][]any
	}{
		"a submission": {"pas-claim", nil, []string{"Bundle at "}, [][]any{{submit}}},
		"an update":    {"pas-claim-update", asUpdate(t), []string{"Bundle at "}, [][]any{{update}, {submit}}},
		"an inquiry":   {"pas-claim-inquire", nil, []string{"Bundle at " + inquiry}, [][]any{nil}},
	} {
		t.Run(name, func(t *testing.T) {
			lane := &bridgedLane{res: shnsdk.Result{Valid: true}}
			ctx, _ := recordingCtx()
			if status, msg := bridgedBuild(ctx, t, walkGateway(lane), c.leg, c.edit); status != 0 {
				t.Fatalf("a valid translation refused: %d %q", status, msg)
			}
			if got := lane.calls(); !slices.Equal(got, c.want) {
				t.Fatalf("the target lane was asked %q, want %q", got, c.want)
			}
			if got := claimProfilesIn(t, lane.bodies[0]); fmt.Sprint(got) != fmt.Sprint(c.claims) {
				t.Fatalf("the Claims read declare %v, want %v", got, c.claims)
			}
		})
	}
}

// refusedAtVerify asserts a bridged build refused with 422 naming says, and
// recorded as a fidelity refusal at the verify stage.
func refusedAtVerify(t *testing.T, rec *exchangeRecorder, status int, msg, says string) {
	t.Helper()
	if status != http.StatusUnprocessableEntity || !strings.Contains(msg, says) {
		t.Fatalf("got %d %q, want 422 naming %q", status, msg, says)
	}
	if rec.guardRule != RefusalFidelity || rec.guardStage != vocab.RefusalStageVerify {
		t.Fatalf("recorded %q at %q, want %s at %s", rec.guardRule, rec.guardStage, RefusalFidelity, vocab.RefusalStageVerify)
	}
}

// A translation that is valid FHIR but not valid at the target line's profile
// is refused at every level, and recorded as a fidelity refusal at the verify
// stage: the message the gateway built failed its own check. Before the
// target profile was read, the same bytes passed as base-FHIR valid.
func TestBridgedCheck_AProfileInvalidTranslationIsRefusedAtEveryLevel(t *testing.T) {
	cardinality := shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "Validation_VAL_Profile_Minimum",
		Expression:  []string{"Bundle.entry[0].resource/*Claim/convergence-claim*/"},
		Diagnostics: "Claim.supportingInfo: minimum required = 1, but only found 0"}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			g := walkGateway(&bridgedLane{res: invalidWith(cardinality)})
			g.cfg.ConformanceEnforcement = level
			ctx, rec := recordingCtx()
			status, msg := bridgedBuild(ctx, t, g, "pas-claim", nil)
			refusedAtVerify(t, rec, status, msg, "minimum required")
		})
	}
}

// The request Bundle profile's own constraints govern the bridged check too,
// whatever the lane says: with a lane that finds every part valid, a walked
// request breaking one of them is refused at every level, as a fidelity
// refusal at the verify stage. The walk carries what the 2.1 request had;
// the target line's profile decides.
func TestBridgedCheck_TheRequestBundlesOwnRulesRefuseAtEveryLevel(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(b map[string]any)
		says string
	}{
		"no identifier":            {func(b map[string]any) { delete(b, "identifier") }, "Bundle.identifier: minimum required = 1"},
		"an entry with no fullUrl": {func(b map[string]any) { delete(entryAt(b, 2), "fullUrl") }, "Bundle.entry.fullUrl: minimum required = 1"},
		"the Claim not first": {func(b map[string]any) {
			e := entries(b)
			b["entry"] = append(append([]any{}, e[1:]...), e[0])
		}, "ClaimFirst"},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				g := walkGateway(&bridgedLane{res: shnsdk.Result{Valid: true}})
				g.cfg.ConformanceEnforcement = level
				ctx, rec := recordingCtx()
				status, msg := bridgedBuild(ctx, t, g, "pas-claim", c.edit)
				refusedAtVerify(t, rec, status, msg, c.says)
			})
		}
	}
}

// A translation whose only finding at the target profile is a code the lane
// could not check is sent at every level: the unchecked reading says nothing
// about the bytes. The same gap beside a defect the lane decided is refused.
func TestBridgedCheck_AnUncheckedCodeIsSentAndADefectBesideItIsRefused(t *testing.T) {
	gap := reasonCodeGap("Bundle.entry[0].resource/*Claim/convergence-claim*/.item[0].productOrService")
	invariant := shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "http://hl7.org/fhir/StructureDefinition/Period#per-1",
		Expression:  []string{"Bundle.entry[0].resource/*Claim/convergence-claim*/.billablePeriod"},
		Diagnostics: "Constraint failed: per-1: 'If present, start SHALL have a lower or equal value than end'"}
	for _, level := range allLevels {
		t.Run("only a gap/"+level.String(), func(t *testing.T) {
			g := walkGateway(&bridgedLane{res: invalidWith(gap...)})
			g.cfg.ConformanceEnforcement = level
			ctx, _ := recordingCtx()
			if status, msg := bridgedBuild(ctx, t, g, "pas-claim", nil); status != 0 {
				t.Fatalf("a translation whose only finding is an unchecked code was refused: %d %q", status, msg)
			}
		})
		t.Run("a gap beside a defect/"+level.String(), func(t *testing.T) {
			g := walkGateway(&bridgedLane{res: invalidWith(append(append([]shnsdk.Issue(nil), gap...), invariant)...)})
			g.cfg.ConformanceEnforcement = level
			ctx, rec := recordingCtx()
			status, msg := bridgedBuild(ctx, t, g, "pas-claim", nil)
			refusedAtVerify(t, rec, status, msg, "per-1")
		})
	}
}

// bridgedProfile is the certification map's answer for the walked bytes: a
// PAS species at a PAS leg reads its line's profile, and a resource no
// compiled-in workstream certifies, or a leg the map names none for, reads ""
// (the line's base, as before).
func TestBridgedProfile_IsTheCertificationMapsAnswer(t *testing.T) {
	bundle := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Claim"}}]}`)
	want := requestBundleProfile(t, "2.2")
	for name, c := range map[string]struct {
		body []byte
		leg  string
		want string
	}{
		"a PAS request Bundle on a submit":  {bundle, "pas-claim", want},
		"an uncertified resource":           {[]byte(`{"resourceType":"Patient"}`), "pas-claim", ""},
		"a leg the map names no profile on": {bundle, "crd-order-sign", ""},
		"bytes that are not a resource":     {[]byte(`not json`), "pas-claim", ""},
	} {
		if got := bridgedProfile(c.body, "2.2", c.leg); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// The walked fixtures are what the rows above claim: a 2.1 request that the
// walk carries with its Bundle identifier, every fullUrl and the Claim first.
func TestBridgedCheck_TheWalkedFixtureIsAWholeRequest(t *testing.T) {
	route, in := bridgedRequestWalk(t, nil)
	out, _, err := (&Gateway{cfg: Config{TranslateIGLines: true, HolderID: "test-holder", Clock: fixedEgressClock}}).egressAdapt(route, in, ExchangeIdentity{CorrelationID: "corr-1135", LegType: "pas-claim"})
	if err != nil {
		t.Fatal(err)
	}
	var b pasRequestBundleShape
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatal(err)
	}
	if !pasElementPresent(b.Identifier) || !pasElementPresent(b.Timestamp) || len(b.Entry) < 2 || resourceTypeOf(b.Entry[0].Resource) != "Claim" {
		t.Fatalf("the walked request is not whole: %s", out)
	}
	for i, e := range b.Entry {
		if !pasElementPresent(e.FullURL) {
			t.Fatalf("entry %d has no fullUrl", i)
		}
	}
}

// What the gateway sends is the walk's own bytes: the reading's copy, which
// drops the request Bundle profile and declares each Claim's candidate, is
// the lane's alone. The message built for the peer is byte-identical to what
// the registered steps make of the request, and declares no Claim candidate.
func TestBridgedCheck_SendsTheWalksOwnBytes(t *testing.T) {
	lane := &bridgedLane{res: shnsdk.Result{Valid: true}}
	route, in := bridgedRequestWalk(t, nil)
	x := ExchangeIdentity{CorrelationID: "corr-1135", LegType: "pas-claim"}
	g := walkGateway(lane)
	walked, _, err := g.egressAdapt(route, append([]byte(nil), in...), x)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := recordingCtx()
	built, status, msg, err := g.egressBuild(ctx, route, x, moduleBuild{Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: in, Contract: "pa.pas"})
	if err != nil || status != 0 {
		t.Fatalf("egressBuild: %d %q %v", status, msg, err)
	}
	if string(built.Bytes) != string(walked) {
		t.Fatalf("the gateway sends\n%s\nwant the walk's bytes\n%s", built.Bytes, walked)
	}
	if strings.Contains(string(built.Bytes), pasClaimCanonical) {
		t.Fatal("the sent message declares the Claim candidate the reading's copy declares")
	}
	if len(lane.bodies) != 1 || !strings.Contains(string(lane.bodies[0]), pasClaimCanonical) {
		t.Fatal("fixture: the lane's copy declares no Claim candidate, so the row proves nothing")
	}
}
