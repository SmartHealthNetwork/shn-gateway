//go:build !shn_coreonly

package engine

import (
	"bytes"
	"go/ast"
	"net/http"
	"slices"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A PAS request the gateway builds on the provider-data lane can carry a
// record from the participant's own system that registered edit E-06
// re-pointed before the build. The request is still the builder's own message
// (authored, its builder named), and its exchange record and access line name
// E-06 in relayEdits and requestEdits. With no record re-pointed it names
// nothing. Each leg the gateway builds a PAS request for has its row.

// pasBuilds are the PAS requests module 1 builds: the submission and the
// update (which also carries the supplemental report).
var pasBuilds = []struct {
	leg     string
	builder relay.BuilderID
}{
	{"pas-claim", relay.BuilderSDKPASSubmit},
	{"pas-claim-update", relay.BuilderSDKPASUpdate},
}

// atBuildLine is a route with no chain: the request is sent at its build
// line, as built.
var atBuildLine = legRoute{Token: "pa.pas@2.0", BuildLine: "2.0"}

func TestEgressBuild_ARequestCarryingARecordE06ChangedNamesIt(t *testing.T) {
	body := pasGolden(t, "claimresponse-approved.json")
	for _, b := range pasBuilds {
		for _, row := range []struct {
			name   string
			inputs []relay.EditID
			want   []string
		}{
			{"a record re-pointed", []relay.EditID{relay.EditEvidenceSubjectRekey}, []string{"E-06"}},
			{"control: no record re-pointed", nil, nil},
		} {
			t.Run(b.leg+"/"+row.name, func(t *testing.T) {
				ctx, _ := recordingCtx()
				built, status, msg, err := walkGateway(shnsdk.NewFakeValidator()).egressBuild(ctx, atBuildLine, ExchangeIdentity{CorrelationID: "corr-e06", LegType: b.leg}, moduleBuild{
					Builder: b.builder, ContentType: "application/fhir+json", Body: body, InputEdits: row.inputs, Contract: "pa.pas",
				})
				if err != nil || status != 0 {
					t.Fatalf("egressBuild: %d %s %v", status, msg, err)
				}
				p := built.Payload
				if p.Ownership() != relay.OwnershipAuthored || p.Builder() != b.builder || p.Edits() != nil || !slices.Equal(p.InputEdits(), row.inputs) {
					t.Fatalf("sealed %v inputs %v, want authored by %s carrying %v", p, p.InputEdits(), b.builder, row.inputs)
				}
				sent, err := relay.Transmit(p, relay.Check(requestKey(b.leg, false)))
				if err != nil || !bytes.Equal(sent, body) {
					t.Fatalf("the %s transmit: %v; want the request as built", b.leg, err)
				}
				x := &exchangeRecorder{}
				x.sent(p)
				if x.rec.Ownership != "authored" || x.rec.Builder != string(b.builder) ||
					!slices.Equal(x.rec.RequestEdits, row.want) || !slices.Equal(x.rec.Edits, row.want) {
					t.Fatalf("recorded ownership %q builder %q request edits %v edits %v, want authored by %s naming %v",
						x.rec.Ownership, x.rec.Builder, x.rec.RequestEdits, x.rec.Edits, b.builder, row.want)
				}
			})
		}
	}
}

// E-06 and the registered transform together: a provider-data request whose
// route module 2 walks to the payer's line (TRANSLATE_IG_LINES) is the
// builder's message carrying a record E-06 changed, then E-09. It is recorded
// edited, its builder kept, naming both in the order made.
func TestEgressBuild_E06ThenTheRegisteredTransform(t *testing.T) {
	route, in := pasWalk(t)
	for _, b := range pasBuilds {
		t.Run(b.leg, func(t *testing.T) {
			ctx, _ := recordingCtx()
			built, status, msg, err := walkGateway(shnsdk.NewFakeValidator()).egressBuild(ctx, route, ExchangeIdentity{CorrelationID: "corr-e06-e09", LegType: b.leg}, moduleBuild{
				Builder: b.builder, ContentType: "application/fhir+json", Body: in, InputEdits: []relay.EditID{relay.EditEvidenceSubjectRekey}, Contract: "pa.pas",
			})
			if err != nil || status != 0 {
				t.Fatalf("egressBuild: %d %s %v", status, msg, err)
			}
			p := built.Payload
			if p.Ownership() != relay.OwnershipEdited || p.Builder() != b.builder ||
				!slices.Equal(p.Edits(), []relay.EditID{relay.EditRegisteredTransform}) || !slices.Equal(p.InputEdits(), []relay.EditID{relay.EditEvidenceSubjectRekey}) {
				t.Fatalf("sealed %v edits %v inputs %v, want %s's message carrying E-06, then E-09", p, p.Edits(), p.InputEdits(), b.builder)
			}
			if _, err := relay.Transmit(p, relay.Check(requestKey(b.leg, false))); err != nil {
				t.Fatalf("the %s transmit: %v", b.leg, err)
			}
			x := &exchangeRecorder{}
			x.sent(p)
			if x.rec.Ownership != "edited" || x.rec.Builder != string(b.builder) || !slices.Equal(x.rec.RequestEdits, []string{"E-06", "E-09"}) {
				t.Fatalf("recorded ownership %q builder %q request edits %v, want edited by %s naming E-06 then E-09", x.rec.Ownership, x.rec.Builder, x.rec.RequestEdits, b.builder)
			}
		})
	}
}

// REJECTION: a build can name only an input edit (relay.InputEditIDs). An
// edit made to a request's own bytes, or an unregistered id, named as one
// cannot be named on the request, so nothing is sent: the fidelity of SHN's
// own edit, at verify, at every level.
func TestEgressBuild_AnEditThatIsNotAnInputEditSendsNothing(t *testing.T) {
	body := pasGolden(t, "claimresponse-approved.json")
	for _, id := range []relay.EditID{relay.EditCDSCallbackStrip, relay.EditRegisteredTransform, "E-99"} {
		for _, level := range allLevels {
			t.Run(string(id)+"/"+level.String(), func(t *testing.T) {
				ctx, x := recordingCtx()
				g := walkGateway(shnsdk.NewFakeValidator())
				g.cfg.ConformanceEnforcement = level
				built, status, _, err := g.egressBuild(ctx, atBuildLine, ExchangeIdentity{CorrelationID: "corr-bad", LegType: "pas-claim"}, moduleBuild{
					Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: body, InputEdits: []relay.EditID{id}, Contract: "pa.pas",
				})
				if err != nil || status != http.StatusInternalServerError || built.Payload.Ownership() != 0 || built.Bytes != nil {
					t.Fatalf("egressBuild = %d %v %v, want 500 with nothing built", status, err, built.Payload)
				}
				if x.outcome != ExchangeRefused || x.rule != RefusalFidelity || x.refusalStage != vocab.RefusalStageVerify {
					t.Fatalf("recorded %q %q at %q, want refused on fidelity at verify", x.outcome, x.rule, x.refusalStage)
				}
			})
		}
	}
}

// Every PAS request module 1 builds names the input edits its reads returned:
// each moduleBuild with a PAS builder sets InputEdits, and the edits each
// completion (completeAuthoredPASRequest) and each supplemental report read
// (supplementalReport) return reach a moduleBuild's InputEdits in the same
// function, so no site can carry an E-06'd record without naming it.
func TestEveryPASBuildNamesItsInputEdits(t *testing.T) {
	sites := moduleBuildLiterals(t)
	var pas int
	for _, s := range sites {
		if s.builder != "BuilderSDKPASSubmit" && s.builder != "BuilderSDKPASUpdate" {
			continue
		}
		pas++
		if !s.hasInputEdits {
			t.Errorf("%s: a %s build names no InputEdits (the edits its completion returned)", s.pos, s.builder)
		}
	}
	if pas < 11 {
		t.Fatalf("found %d PAS builds, want at least the 11 provider-data completion sites", pas)
	}
	fset, files := engineFiles(t)
	reads := 0
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// The edits each read returns, by variable.
			returned := map[string]string{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Rhs) != 1 || len(as.Lhs) < 2 {
					return true
				}
				call, ok := as.Rhs[0].(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "completeAuthoredPASRequest" && sel.Sel.Name != "supplementalReport") {
					return true
				}
				reads++
				id, ok := as.Lhs[1].(*ast.Ident)
				if !ok || id.Name == "_" {
					t.Errorf("%s: %s drops the input edits %s returned", fset.Position(as.Pos()), fn.Name.Name, sel.Sel.Name)
					return true
				}
				returned[id.Name] = fset.Position(as.Pos()).String()
				return true
			})
			// The variables a moduleBuild's InputEdits reads.
			named := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "moduleBuild" {
					return true
				}
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "InputEdits" {
							ast.Inspect(kv.Value, func(m ast.Node) bool {
								if id, ok := m.(*ast.Ident); ok {
									named[id.Name] = true
								}
								return true
							})
						}
					}
				}
				return true
			})
			for v, pos := range returned {
				if !named[v] {
					t.Errorf("%s: %s reads input edits into %s, which no moduleBuild's InputEdits names", pos, fn.Name.Name, v)
				}
			}
		}
	}
	if reads < 12 {
		t.Fatalf("found %d completion and supplemental report reads, want at least 12", reads)
	}
}

// moduleBuildSite is one moduleBuild literal in the engine's own code: where
// it is, its builder's relay constant, and whether it sets InputEdits to
// something other than nil.
type moduleBuildSite struct {
	pos           string
	builder       string
	hasInputEdits bool
}

func moduleBuildLiterals(t *testing.T) []moduleBuildSite {
	t.Helper()
	fset, files := engineFiles(t)
	var out []moduleBuildSite
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "moduleBuild" {
				return true
			}
			site := moduleBuildSite{pos: fset.Position(lit.Pos()).String()}
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, _ := kv.Key.(*ast.Ident)
				switch {
				case key == nil:
				case key.Name == "Builder":
					if sel, ok := kv.Value.(*ast.SelectorExpr); ok {
						site.builder = sel.Sel.Name
					}
				case key.Name == "InputEdits":
					if id, ok := kv.Value.(*ast.Ident); !ok || id.Name != "nil" {
						site.hasInputEdits = true
					}
				}
			}
			out = append(out, site)
			return true
		})
	}
	return out
}

// End to end, the UC-04 amendment: the payer pends the submission and the
// same request sends the claim update carrying the operative report from the
// participant's system. A system that holds the member under its own id has
// the report re-pointed by E-06, and the update's sealed leg names it; its
// control, a system holding the member under the member id, names nothing.
func TestUC04Amendment_ASupplementalReportE06ChangedIsNamed(t *testing.T) {
	const member = "MBR-UC04"
	for _, row := range []struct {
		name, sorID string
		want        []string
	}{
		{"the system holds the member under its own id", "pat-7", []string{"E-06"}},
		{"control: the system holds the member under the member id", member, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			gw, stub := uc04RouteFixture(t, "2.0")
			inner, ok := gw.cfg.SoR.(recordIDInner)
			if !ok {
				t.Fatalf("fixture SoR is %T", gw.cfg.SoR)
			}
			gw.cfg.SoR = &reportUnderRecordID{
				recordIDSoR: &recordIDSoR{recordIDInner: inner, member: member, recordID: row.sorID, demo: Demo{BirthDate: "1982-11-03", FamilyName: "Chen"}},
				report:      []byte(supplementalReportAsHeld("Patient/" + row.sorID)),
			}
			var events capturedEvents
			gw.cfg.Diagnostic = events.sink
			rec := runUC04(gw, testRouteBody(gw, "/scenario/uc04", ""))
			if rec.Code != http.StatusOK || !legAttempted(stub.legTypes, "pas-claim-update") {
				t.Fatalf("uc04 = %d %s legs %v, want 200 with the amendment sent", rec.Code, rec.Body, stub.legTypes)
			}
			if sent := stub.requests["pas-claim-update"]; len(sent) != 1 || !bytes.Contains(sent[0], []byte(`"Patient/`+member+`"`)) || bytes.Contains(sent[0], []byte(`"Patient/pat-7"`)) {
				t.Fatalf("the amendment does not carry the report naming the member's patient alone")
			}
			var update capturedEvents
			for _, e := range events.of("leg.sealed") {
				if e.LegType == "pas-claim-update" {
					update.events = append(update.events, e)
				}
			}
			wantEventEdits(t, &update, "leg.sealed", row.want)
		})
	}
}

// reportUnderRecordID is recordIDSoR holding report as the member's
// supplemental report, about the Patient it holds the member under.
type reportUnderRecordID struct {
	*recordIDSoR
	report []byte
}

func (s *reportUnderRecordID) SupplementalReport(m string) ([]byte, bool) {
	if m == s.member {
		return s.report, true
	}
	return s.recordIDSoR.SupplementalReport(m)
}
