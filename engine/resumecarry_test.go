package engine

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A pended authorization's pin addresses its follow-up: the same peer, and the
// pend it references. The pinned IG line is a version check. When nothing
// reaches the pinned line at resume, the follow-up is built at this gateway's
// own line and carried as sent, at every level, and the difference from the
// pin is recorded; only the participant's own REQUIRE_SHARED_LINE refuses it,
// as routing. A pend or peer that cannot be resolved is still refused.

// qualifiedLane is a default lane at line that has passed its qualification.
func qualifiedLane(t *testing.T, line string) *DiscoveredLane {
	t.Helper()
	d := NewDiscoveredLane(line, DefaultLaneURL(line), shnsdk.NewFakeValidator())
	if err := d.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil || !d.Ready() {
		t.Fatalf("qualify the %s default lane: %v", line, err)
	}
	return d
}

// pinTrigger is one way a pend's pinned line loses its reach between the
// submission and a later inquiry: before sets up the gateway the submission
// runs on, after changes it as a restart would.
type pinTrigger struct {
	name          string
	before, after func(t *testing.T, gw *Gateway)
}

var pinTriggers = []pinTrigger{
	{
		// The submission reached the payer's 2.2 by native reach, through a
		// qualified default lane this gateway does not declare. After a restart
		// the default lane qualifies again in the background; an inquiry
		// continued before that finishes has no reach to 2.2.
		name: "a default lane not yet qualified again after a restart",
		before: func(t *testing.T, gw *Gateway) {
			gw.cfg.ValidatorsByContractLine = ContractLineLanes(map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()},
				map[string]*DiscoveredLane{"2.2": qualifiedLane(t, "2.2")}, nil, gw.cfg.DeclaredContractVersions)
		},
		after: func(t *testing.T, gw *Gateway) {
			gw.cfg.ValidatorsByContractLine = ContractLineLanes(map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()},
				map[string]*DiscoveredLane{"2.2": NewDiscoveredLane("2.2", DefaultLaneURL("2.2"), shnsdk.NewFakeValidator())}, nil, gw.cfg.DeclaredContractVersions)
		},
	},
	{
		// The submission ran at 2.2, a line this gateway declared and laned.
		// Across a restart its configuration dropped the line and its lane.
		name: "the pinned line no longer declared or laned after a configuration change",
		before: func(t *testing.T, gw *Gateway) {
			gw.cfg.DeclaredContractVersions = []string{shnsdk.ContractPACRD20, shnsdk.ContractPADTR20, shnsdk.ContractPAPAS20, "pa.pas@2.2"}
			gw.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator(), "2.2": shnsdk.NewFakeValidator()})
		},
		after: func(t *testing.T, gw *Gateway) {
			gw.cfg.DeclaredContractVersions = nil
			gw.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()})
		},
	},
}

// pinnedPendAt22 runs a submission the payer pends, with the payer declaring
// only pa.pas@2.2 and the gateway set up by trigger.before, and returns the
// gateway, the payer stub and the continuation, pinned at 2.2.
func pinnedPendAt22(t *testing.T, trigger pinTrigger) (*Gateway, *pasFollowStub, string) {
	t.Helper()
	gw, stub := pasFollowSystem(t, "pended")
	entry, _ := gw.cfg.Reg.Lookup("payer")
	entry.ContractVersions = []string{"pa.pas@2.2"}
	gw.cfg.Reg.Set("payer", entry)
	trigger.before(t, gw)
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(testOrderBody(pasFollowSubmit(0).orderJSON, "")))
	pend, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
	if status != 0 || err != nil || pend.Continuation == "" {
		t.Fatalf("submit: %d %q %v (%+v)", status, msg, err, pend)
	}
	store, _ := gw.continuations()
	cont, _, _ := store.ReadContinuation("provider", pend.Continuation)
	if cont.Line != "2.2" {
		t.Fatalf("the submission ran at %q; this row needs it pinned at 2.2", cont.Line)
	}
	trigger.after(t, gw)
	if route, ok := gw.resumeReach("pa.pas@2.2", "payer", "pa.pas", "2.2", contractLineSet(gw.declaredContractVersions(), "pa.pas"), ""); ok {
		t.Fatalf("the trigger left a reach to the pinned line: %+v", route)
	}
	return gw, stub, pend.Continuation
}

// inquire asks about continuation once through the gateway's own inquiry
// route, recording the observer events it raised. An origination route writes
// no exchange record; the carry is recorded on the observer stream (and its
// log line), as for any leg a scenario route carries across lines.
func inquire(gw *Gateway, continuation string) (*httptest.ResponseRecorder, []ObserverEvent) {
	var events []ObserverEvent
	gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	gw.cfg.OriginationProfile = "demo" // the origination routes need a lane
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire",
		strings.NewReader(`{"continuation":"`+continuation+`","waitSeconds":0}`)))
	return rec, events
}

// By default, at every level whose payload checks consume lanes, an inquiry
// whose pinned line has no lane is carried at this gateway's own line to the
// pend's payer, and the difference is recorded on the observer stream. At
// none, where no payload check consumes a lane, the inquiry's builder is
// certified at the pinned line (certifiedBuilds), so it is built natively
// there instead (TestResumeBuildsACertifiedInquiryAtThePinUnlanedAtNone).
func TestResumeCarriesWhenThePinnedLineHasNoReach(t *testing.T) {
	for _, trigger := range pinTriggers {
		for _, level := range allLevels {
			if !payloadChecksConsumeLanes(level) {
				continue
			}
			t.Run(trigger.name+"/"+level.String(), func(t *testing.T) {
				gw, stub, continuation := pinnedPendAt22(t, trigger)
				gw.cfg.ConformanceEnforcement = level
				rec, events := inquire(gw, continuation)
				if rec.Code != http.StatusOK {
					t.Fatalf("inquiry %d %s, want it carried", rec.Code, rec.Body)
				}
				if !stub.sawInquiry {
					t.Fatal("the inquiry never reached the pend's payer")
				}
				if n := len(stub.sent["pas-claim-inquire"]); n != 1 {
					t.Fatalf("%d inquiries sent, want 1", n)
				}
				if !slices.ContainsFunc(events, func(e ObserverEvent) bool {
					return e.Kind == "leg.originated" && e.LegType == "pas-claim-inquire" && e.Route != nil &&
						e.Route.Token == shnsdk.ContractPAPAS20 && e.Route.Chain == nil && e.Route.Pinned == "pa.pas@2.2"
				}) {
					t.Fatalf("no leg.originated at this gateway's own %s naming the pin: %+v", shnsdk.ContractPAPAS20, events)
				}
				carried := slices.IndexFunc(events, func(e ObserverEvent) bool {
					return e.Kind == LineCarriedEvent && e.LegType == "pas-claim-inquire" &&
						strings.Contains(e.Detail, "carried at pa.pas@2.0") && strings.Contains(e.Detail, "pinned at pa.pas@2.2")
				})
				if carried < 0 {
					t.Fatalf("no %s naming the pin and the carried line: %+v", LineCarriedEvent, events)
				}
			})
		}
	}
}

// At none, where no payload check consumes a lane, an inquiry whose pinned line
// has no lane is built natively at that pinned line: its builder is certified
// there at build time (certifiedBuilds), so no runtime check would read the
// lane. Nothing is carried and nothing is recorded as a carry, with or without
// REQUIRE_SHARED_LINE, since the pinned line is reached.
func TestResumeBuildsACertifiedInquiryAtThePinUnlanedAtNone(t *testing.T) {
	for _, trigger := range pinTriggers {
		for _, requireShared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/requireShared=%v", trigger.name, requireShared), func(t *testing.T) {
				gw, stub, continuation := pinnedPendAt22(t, trigger)
				gw.cfg.ConformanceEnforcement = EnforcementNone
				gw.cfg.RequireSharedLine = requireShared
				rec, events := inquire(gw, continuation)
				if rec.Code != http.StatusOK {
					t.Fatalf("inquiry %d %s, want it built at the pinned line", rec.Code, rec.Body)
				}
				if n := len(stub.sent["pas-claim-inquire"]); n != 1 {
					t.Fatalf("%d inquiries sent, want 1", n)
				}
				if !slices.ContainsFunc(events, func(e ObserverEvent) bool {
					return e.Kind == "leg.originated" && e.LegType == "pas-claim-inquire" && e.Route != nil &&
						e.Route.Token == "pa.pas@2.2" && e.Route.Chain == nil && e.Route.Pinned == ""
				}) {
					t.Fatalf("no leg.originated at the pinned pa.pas@2.2: %+v", events)
				}
				if slices.ContainsFunc(events, func(e ObserverEvent) bool { return e.Kind == LineCarriedEvent }) {
					t.Fatalf("an inquiry built at its pinned line was recorded as carried: %+v", events)
				}
			})
		}
	}
}

// REJECTION: under REQUIRE_SHARED_LINE the same inquiry is refused as routing,
// naming the pinned line, and nothing is sent, at every level whose payload
// checks consume lanes. At none the certified inquiry reaches its pinned line,
// so there is nothing to refuse.
func TestResumeRefusesUnderRequireSharedLine(t *testing.T) {
	for _, trigger := range pinTriggers {
		for _, level := range allLevels {
			if !payloadChecksConsumeLanes(level) {
				continue
			}
			t.Run(trigger.name+"/"+level.String(), func(t *testing.T) {
				gw, stub, continuation := pinnedPendAt22(t, trigger)
				gw.cfg.ConformanceEnforcement = level
				gw.cfg.RequireSharedLine = true
				rec, _ := inquire(gw, continuation)
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no bridge to the pinned line 2.2") {
					t.Fatalf("inquiry %d %s, want the 422 routing refusal naming the pinned line", rec.Code, rec.Body)
				}
				if stub.sawInquiry {
					t.Fatal("a refused inquiry was sent")
				}
			})
		}
	}
}

// REJECTION: what the pin addresses is still refused at every level when it
// cannot be resolved: a continuation this gateway does not hold, and a payer
// the registry no longer resolves. Neither is carried.
func TestResumeStillRefusesAnUnresolvablePendOrPeer(t *testing.T) {
	trigger := pinTriggers[0]
	for _, level := range allLevels {
		t.Run("unknown continuation/"+level.String(), func(t *testing.T) {
			gw, stub, _ := pinnedPendAt22(t, trigger)
			gw.cfg.ConformanceEnforcement = level
			rec, _ := inquire(gw, "m0-not-a-continuation")
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unknown continuation") || stub.sawInquiry {
				t.Fatalf("an unknown continuation was answered %d %s (sent=%v), want 404 unknown continuation", rec.Code, rec.Body, stub.sawInquiry)
			}
		})
		t.Run("unresolvable payer/"+level.String(), func(t *testing.T) {
			gw, stub, continuation := pinnedPendAt22(t, trigger)
			gw.cfg.ConformanceEnforcement = level
			reg := shnsdk.NewRegistry()
			provider, _ := gw.cfg.Reg.Lookup("provider")
			reg.Set("provider", provider)
			gw.cfg.Reg = reg
			rec, _ := inquire(gw, continuation)
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `recipient \"payer\" not in registry`) || stub.sawInquiry {
				t.Fatalf("an inquiry to an unresolvable payer was answered %d %s (sent=%v), want 502 naming the payer", rec.Code, rec.Body, stub.sawInquiry)
			}
		})
	}
}

// resumeRegistry is a registry whose payer declares tokens.
func resumeRegistry(tokens ...string) shnsdk.Registry {
	reg := shnsdk.NewRegistry()
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", ContractVersions: tokens})
	return reg
}

// The amendment resumes through the same selection: with no reach to its
// pinned line it is carried at this gateway's own line, Pinned naming the
// pin, or refused as routing under REQUIRE_SHARED_LINE. A gateway that
// declares no line of the contract at all still refuses.
func TestResumeSelectionCarriesTheAmendment(t *testing.T) {
	// The amendment's builder is not certified at any line, so this holds at
	// none too: only a certified builder builds at a line it has no lane for.
	for _, c := range []struct {
		requireShared bool
		level         ConformanceEnforcement
	}{{false, EnforcementStrict}, {true, EnforcementStrict}, {false, EnforcementNone}, {true, EnforcementNone}} {
		requireShared := c.requireShared
		g := &Gateway{cfg: Config{
			DeclaredContractVersions: []string{shnsdk.ContractPAPAS20},
			ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()}),
			RequireSharedLine:        requireShared,
			ConformanceEnforcement:   c.level,
			Reg:                      resumeRegistry("pa.pas@2.2"),
		}}
		route, err := g.selectResumeRoute("pa.pas@2.2", "payer", "pas-claim-update", relay.BuilderSDKPASUpdate)
		if requireShared {
			var rre *RouteRefusalError
			if !errors.As(err, &rre) || rre.LegType != "pas-claim-update" || !strings.Contains(rre.BridgeIssue, "no bridge to the pinned line 2.2") {
				t.Fatalf("REQUIRE_SHARED_LINE: %+v, %v, want the routing refusal", route, err)
			}
			continue
		}
		if err != nil || route.Token != shnsdk.ContractPAPAS20 || route.BuildLine != "2.0" || route.Chain != nil || route.Pinned != "pa.pas@2.2" {
			t.Fatalf("default: %+v, %v, want carried at pa.pas@2.0 with the pin named", route, err)
		}
		if info := routeInfoFor(route); info.Pinned != "pa.pas@2.2" {
			t.Fatalf("the routed story drops the pin: %+v", info)
		}
		if sent := route.asSent(); sent.Pinned != "pa.pas@2.2" {
			t.Fatalf("carrying as sent drops the pin: %+v", sent)
		}
	}
	g := &Gateway{cfg: Config{DeclaredContractVersions: []string{shnsdk.ContractPACRD20}, Reg: resumeRegistry("pa.pas@2.2")}}
	if _, err := g.selectResumeRoute("pa.pas@2.2", "payer", "pas-claim-update", ""); err == nil {
		t.Fatal("a gateway that declares no pa.pas line carried a pa.pas amendment")
	}
}

// The carry prefers a line both sides declare, as fresh selection does, over
// this gateway's own highest line: the peer then receives a line it declares.
func TestResumeCarryPrefersASharedLine(t *testing.T) {
	fake := shnsdk.NewFakeValidator()
	g := &Gateway{cfg: Config{
		DeclaredContractVersions: []string{shnsdk.ContractPAPAS20, "pa.pas@2.2"},
		ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.2": fake}),
		Reg:                      resumeRegistry(shnsdk.ContractPAPAS20, "pa.pas@2.1"),
	}}
	route, err := g.selectResumeRoute("pa.pas@2.1", "payer", "pas-claim-inquire", relay.BuilderSDKPASInquiry)
	if err != nil || route.Token != shnsdk.ContractPAPAS20 || route.Chain != nil || route.Pinned != "pa.pas@2.1" {
		t.Fatalf("%+v, %v, want carried at the shared pa.pas@2.0, naming the pin", route, err)
	}
	// With nothing shared, this gateway's own highest line.
	g.cfg.Reg = resumeRegistry("pa.pas@2.1")
	if route, err := g.selectResumeRoute("pa.pas@2.1", "payer", "pas-claim-inquire", relay.BuilderSDKPASInquiry); err != nil || route.Token != "pa.pas@2.2" || route.Pinned != "pa.pas@2.1" {
		t.Fatalf("%+v, %v, want carried at this gateway's own pa.pas@2.2", route, err)
	}
}

// REJECTION: a carry at the pinned line itself is no carry. A declared line
// that is not served (no lane, at a level that checks: a state the gateway
// does not boot in) and is the only candidate refuses, as before, rather than
// claim a carry and then fail its build or check at that line.
func TestResumeNeverCarriesToThePinItself(t *testing.T) {
	g := &Gateway{cfg: Config{
		DeclaredContractVersions: []string{"pa.pas@2.2"},
		ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()}),
		Reg:                      resumeRegistry("pa.pas@2.2"),
	}}
	route, err := g.selectResumeRoute("pa.pas@2.2", "payer", "pas-claim-update", "")
	var rre *RouteRefusalError
	if !errors.As(err, &rre) || !strings.Contains(rre.BridgeIssue, "no bridge to the pinned line 2.2") {
		t.Fatalf("%+v, %v, want the pinned-line refusal", route, err)
	}
}

// A carried amendment's QuestionnaireResponse attachment is built and checked
// at the DTR line paired with the line the amendment is built at, never the
// pin's, so the bundle never mixes lines; an amendment at its pin keeps the
// pend's.
func TestAmendmentDTRLineFollowsTheCarry(t *testing.T) {
	st := pendState{pasDTRLine: "2.2"}
	if got, err := amendmentDTRLine(st, legRoute{Token: "pa.pas@2.2", BuildLine: "2.2"}); err != nil || got != "2.2" {
		t.Fatalf("at the pin: %q, %v, want the pend's 2.2", got, err)
	}
	want, err := pairedDTRLine("2.0")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := amendmentDTRLine(st, legRoute{Token: shnsdk.ContractPAPAS20, BuildLine: "2.0", Pinned: "pa.pas@2.2"}); err != nil || got != want {
		t.Fatalf("carried at 2.0: %q, %v, want %q", got, err, want)
	}
}

// TestResumeCarryServesOnlyFollowUpsTheGatewayBuilds pins who reaches the
// resume selection, and so the carry: only the follow-ups this gateway builds
// itself on its participant's behalf, the scenario amendment
// (resumeAmendmentOrFail) and the continuation's inquiry
// (inquireContinuation). A native participant's own amendment (POST
// /Claim/$submit naming its prior Claim, handlePASIngress) and inquiry (POST
// /Claim/$inquire, handlePASInquireIngress) are its own bytes, carried exactly
// (relay.Exact, Content.Carried) through OriginateLeg's selection with no pin,
// so they never reach this selection and are never rebuilt at another line. A
// new caller here must show it carries no participant's bytes.
func TestResumeCarryServesOnlyFollowUpsTheGatewayBuilds(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var callers []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "selectResumeRoute" {
						callers = append(callers, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	slices.Sort(callers)
	if want := []string{"inquireContinuation", "resumeAmendmentOrFail"}; !slices.Equal(callers, want) {
		t.Fatalf("selectResumeRoute is called from %v, want only %v", callers, want)
	}
}
