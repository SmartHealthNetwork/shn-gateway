package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A request module 1 built and module 2 walked to the payer's IG line is the
// builder's message carried by the registered transform (E-09): sealed so,
// never under the builder alone, and only once the compat manifest's own steps
// make the same bytes again. Each refusal row is that walk with one thing
// wrong, and sends nothing.

// pasWalk is a real pa.pas walk: a 2.0 ClaimResponse golden carried to 2.2
// along the registered chain.
func pasWalk(t *testing.T) (legRoute, []byte) {
	t.Helper()
	steps := chainFor("pa.pas", "2.0", "2.2")
	if len(steps) != 2 {
		t.Fatalf("want a 2-hop pa.pas 2.0->2.2 chain, got %d", len(steps))
	}
	return legRoute{Token: "pa.pas@2.2", BuildLine: "2.0", Chain: steps}, pasGolden(t, "claimresponse-approved.json")
}

func walkGateway(lane22 shnsdk.Validator) *Gateway {
	return &Gateway{cfg: Config{
		TranslateIGLines: true,
		HolderID:         "test-holder", Clock: fixedEgressClock,
		Validator:                shnsdk.NewFakeValidator(),
		ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator(), "2.2": lane22}),
	}}
}

func recordingCtx() (context.Context, *exchangeRecorder) {
	x := &exchangeRecorder{}
	return context.WithValue(context.Background(), exchangeRecorderKey{}, x), x
}

func TestEgressBuild_AWalkedRequestIsItsBuildersThenTheTransform(t *testing.T) {
	route, in := pasWalk(t)
	ctx, _ := recordingCtx()
	built, status, msg, err := walkGateway(shnsdk.NewFakeValidator()).egressBuild(ctx, route, ExchangeIdentity{CorrelationID: "corr-e09", LegType: "pas-claim"}, moduleBuild{
		Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: in, Contract: "pa.pas",
	})
	if err != nil || status != 0 {
		t.Fatalf("egressBuild: %d %s %v", status, msg, err)
	}
	if bytes.Equal(built.Bytes, in) {
		t.Fatal("fixture: the walk changed nothing")
	}
	p := built.Payload
	if p.Ownership() != relay.OwnershipEdited || !slices.Equal(p.Edits(), []relay.EditID{relay.EditRegisteredTransform}) || p.Builder() != relay.BuilderSDKPASSubmit {
		t.Fatalf("sealed %v, want the submission's builder then E-09", p)
	}
	sent, err := relay.Transmit(p, relay.Check(requestKey("pas-claim", false)))
	if err != nil || !bytes.Equal(sent, built.Bytes) {
		t.Fatalf("the originated pas-claim transmit: %v", err)
	}
	// The record keeps both halves: whose message it is and the edit.
	x := &exchangeRecorder{}
	x.sent(p)
	if x.rec.Ownership != "edited" || x.rec.Builder != string(relay.BuilderSDKPASSubmit) || !slices.Equal(x.rec.Edits, []string{"E-09"}) ||
		!slices.Equal(x.rec.RequestEdits, []string{"E-09"}) || x.rec.AnswerOwnership != "" || len(x.rec.AnswerEdits) != 0 {
		t.Fatalf("recorded ownership %q builder %q edits %v, want edited by sdk-pas-submit with E-09", x.rec.Ownership, x.rec.Builder, x.rec.Edits)
	}
	// The same message as an update is not a submission: its own row refuses it.
	if _, err := relay.Transmit(p, relay.Check(requestKey("pas-claim-update", false))); err == nil {
		t.Fatal("a transformed submission was admitted as an update")
	}
}

// A walk that changes no byte (the envelope legs, the identity CRD chain) is
// the builder's message as built: no edit, no verification.
func TestEgressBuild_AnUnchangedWalkIsSealedAsBuilt(t *testing.T) {
	steps := chainFor("pa.crd", "2.0", "2.2")
	if len(steps) == 0 {
		t.Fatal("fixture: no pa.crd chain")
	}
	body := []byte(`{"hook":"order-select","hookInstance":"x"}`)
	built, status, _, err := walkGateway(shnsdk.NewFakeValidator()).egressBuild(context.Background(), legRoute{Token: "pa.crd@2.2", BuildLine: "2.0", Chain: steps},
		ExchangeIdentity{CorrelationID: "corr-crd", LegType: "crd-order-select"}, moduleBuild{Builder: relay.BuilderSDKCRDRequest, ContentType: "application/json", Body: body})
	if err != nil || status != 0 {
		t.Fatalf("egressBuild: %d %v", status, err)
	}
	if p := built.Payload; p.Ownership() != relay.OwnershipAuthored || p.Edits() != nil || p.Builder() != relay.BuilderSDKCRDRequest {
		t.Fatalf("sealed %v, want the CDS Hooks request as built", p)
	}
}

func TestEgressBuild_TheTransformRefuses(t *testing.T) {
	route, in := pasWalk(t)
	x := ExchangeIdentity{CorrelationID: "corr-e09", LegType: "pas-claim"}
	// An unregistered step: the stand-in rewrites the Bundle, but the compat
	// manifest registers no such step. Its contract, lines and class are the
	// manifest's own 2.0-2.1 row's, so only its step functions differ.
	manifest := chainFor("pa.pas", "2.0", "2.1")
	if len(manifest) != 1 {
		t.Fatalf("fixture: %d pa.pas 2.0-2.1 rows", len(manifest))
	}
	stand := CompatStep{Contract: "pa.pas", From: "2.0", To: "2.1", Class: manifest[0].Class,
		Up: func(b []byte, _ ExchangeIdentity) ([]byte, LossReport, error) {
			return []byte(`{"resourceType":"Bundle","rewritten":true}`), LossReport{Module: "pa.pas 2.0->2.1", Source: "2.0", Target: "2.1"}, nil
		}}
	stand.Down = stand.Up
	unregistered := legRoute{Token: "pa.pas@2.1", BuildLine: "2.0", Chain: []CompatStep{stand}}
	// A step function where the manifest's identity row has none.
	crdIdentity := chainFor("pa.crd", "2.0", "2.1")
	if len(crdIdentity) != 1 || crdIdentity[0].Up != nil {
		t.Fatalf("fixture: the pa.crd 2.0-2.1 row is not the identity: %+v", crdIdentity)
	}
	notIdentity := crdIdentity[0]
	notIdentity.Up = func(b []byte, _ ExchangeIdentity) ([]byte, LossReport, error) {
		return append(bytes.Clone(b[:len(b)-1]), []byte(`,"x":1}`)...), LossReport{}, nil
	}
	notIdentity.Down = notIdentity.Up
	crdBody := []byte(`{"hook":"order-select","hookInstance":"x"}`)
	// A step that writes into its input and returns it: had the walk read the
	// message as built rather than a copy, the change would pass for that
	// message.
	inPlace := stand
	inPlace.Up = func(b []byte, _ ExchangeIdentity) ([]byte, LossReport, error) {
		b[len(b)-2] = 'X'
		return b, LossReport{}, nil
	}
	inPlace.Down = inPlace.Up
	for _, r := range []struct {
		name  string
		run   func(ctx context.Context, g *Gateway) (int, string)
		says  string
		level ConformanceEnforcement
	}{
		{"an unregistered step", func(ctx context.Context, g *Gateway) (int, string) {
			_, status, msg, _ := g.egressBuild(ctx, unregistered, x, moduleBuild{Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: in, Contract: "pa.pas"})
			return status, msg
		}, "does not register", EnforcementNone},
		{"a step function the manifest's identity row does not have", func(ctx context.Context, g *Gateway) (int, string) {
			_, status, msg, _ := g.egressBuild(ctx, legRoute{Token: "pa.crd@2.1", BuildLine: "2.0", Chain: []CompatStep{notIdentity}},
				ExchangeIdentity{CorrelationID: "corr-crd", LegType: "crd-order-select"}, moduleBuild{Builder: relay.BuilderSDKCRDRequest, ContentType: "application/json", Body: crdBody})
			return status, msg
		}, "does not register", EnforcementNone},
		{"a step that writes into the message as built", func(ctx context.Context, g *Gateway) (int, string) {
			body := bytes.Clone(in)
			_, status, msg, _ := g.egressBuild(ctx, legRoute{Token: "pa.pas@2.1", BuildLine: "2.0", Chain: []CompatStep{inPlace}}, x,
				moduleBuild{Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: body, Contract: "pa.pas"})
			if !bytes.Equal(body, in) {
				t.Fatal("the walk wrote into the message as built")
			}
			return status, msg
		}, "does not register", EnforcementNone},
		{"output the registered steps did not make", func(ctx context.Context, g *Gateway) (int, string) {
			out, _, err := g.egressAdapt(route, in, x)
			if err != nil {
				t.Fatal(err)
			}
			tampered := bytes.Replace(out, []byte(`"ClaimResponse"`), []byte(`"Claim"`), 1)
			if bytes.Equal(tampered, out) {
				t.Fatal("fixture: nothing to tamper")
			}
			_, status, msg := g.sealTransformed(ctx, route, x, sealRequest(relay.BuilderSDKPASSubmit, in, "application/fhir+json"), tampered)
			return status, msg
		}, "not what the registered steps make", EnforcementNone},
		{"a message that is not the gateway's own", func(ctx context.Context, g *Gateway) (int, string) {
			out, _, err := g.egressAdapt(route, in, x)
			if err != nil {
				t.Fatal(err)
			}
			_, status, msg := g.sealTransformed(ctx, route, x, relay.Exact(relay.NewBody(in, relay.OriginPeerFrame), "application/fhir+json"), out)
			return status, msg
		}, "only the gateway's own message", EnforcementNone},
	} {
		t.Run(r.name, func(t *testing.T) {
			ctx, rec := recordingCtx()
			g := walkGateway(shnsdk.NewFakeValidator())
			g.cfg.ConformanceEnforcement = r.level
			status, msg := r.run(ctx, g)
			if status != http.StatusInternalServerError || !strings.Contains(msg, r.says) || !strings.Contains(msg, "nothing was sent") {
				t.Fatalf("got %d %q, want 500 saying %q", status, msg, r.says)
			}
			if rec.rule != RefusalFidelity {
				t.Fatalf("recorded refusal %q, want %s", rec.rule, RefusalFidelity)
			}
		})
	}
}

// No byte a transform walk made is sealed under a builder alone. In the
// package's non-test files, in every function that walks a request
// (egressAdapt, egressWalk), no sealRequest or relay.Authored call seals a
// variable the walk assigned; egressBuild seals the request as built (m.Body)
// and the walked bytes only through sealTransformed.
func TestNoWalkedByteIsSealedUnderABuilderAlone(t *testing.T) {
	fset, files := engineFiles(t)
	walks := 0
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			walked := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
					return true
				}
				call, ok := as.Rhs[0].(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "egressAdapt" || sel.Sel.Name == "egressWalk") {
					if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
						walked[id.Name] = true
						walks++
					}
				}
				return true
			})
			if len(walked) == 0 {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var bytesArg ast.Expr
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if fun.Name == "sealRequest" && len(call.Args) >= 2 {
						bytesArg = call.Args[1]
					}
				case *ast.SelectorExpr:
					if x, ok := fun.X.(*ast.Ident); ok && x.Name == "relay" && fun.Sel.Name == "Authored" && len(call.Args) >= 2 {
						bytesArg = call.Args[1]
					}
				}
				if id, ok := bytesArg.(*ast.Ident); ok && walked[id.Name] {
					t.Errorf("%s: %s seals %s, bytes a transform walk made, under a builder alone", fset.Position(call.Pos()), fn.Name.Name, id.Name)
				}
				return true
			})
		}
	}
	if walks < 5 {
		t.Fatalf("found %d walks, want egressBuild's and the four byte-equality sites'", walks)
	}
	src, err := os.ReadFile("modulebuild.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"payload := sealRequest(m.Builder, m.Body, m.ContentType)", "g.sealTransformed(ctx, route, x, payload, out)"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("egressBuild no longer contains %q: it seals the request as built, and the walked bytes only as that request carried by the registered transform", want)
		}
	}
}

// The registered steps never trigger the transform's refusal on their own
// walk: every PAS submission and update module 1 builds (the authored builders,
// at every line: service and device orders, relative and absolute references,
// an update with and without a supplemental report), walked along every
// registered chain to every other line, is verified and sealed as its builder's
// message then E-09. A chain that refuses the request (the gated up-step) is not
// a walk the gateway sends, and is counted apart. CRD and questionnaire-package
// walks change no byte (identity steps, an envelope leg), so they never reach
// the verifier.
func TestEgressBuild_TheRegisteredStepsAlwaysVerify(t *testing.T) {
	lines := []string{"2.0", "2.1", "2.2"}
	type build struct {
		name    string
		builder relay.BuilderID
		body    func(line string) ([]byte, error)
	}
	var builds []build
	for _, order := range []string{"ServiceRequest", "DeviceRequest"} {
		for _, absolute := range []bool{false, true} {
			builds = append(builds, build{fmt.Sprintf("submit %s absolute=%v", order, absolute), relay.BuilderSDKPASSubmit,
				func(line string) ([]byte, error) {
					return buildAuthoredPASSubmit(line, attachmentInputs(order, line, absolute))
				}})
		}
	}
	for _, diagnostic := range []bool{false, true} {
		builds = append(builds, build{fmt.Sprintf("update report=%v", diagnostic), relay.BuilderSDKPASUpdate,
			func(line string) ([]byte, error) {
				return buildAuthoredPASUpdate(line, attachmentUpdateInputs(line, false, diagnostic))
			}})
	}
	g := walkGateway(shnsdk.NewFakeValidator())
	verified, refused := 0, 0
	for _, from := range lines {
		for _, b := range builds {
			body, err := b.body(from)
			if err != nil {
				t.Fatalf("%s at %s: %v", b.name, from, err)
			}
			for _, to := range lines {
				steps := chainFor("pa.pas", from, to)
				if to == from || len(steps) == 0 {
					continue
				}
				route := legRoute{Token: "pa.pas@" + to, BuildLine: from, Chain: steps}
				ctx, rec := recordingCtx()
				built, status, msg, err := g.egressBuild(ctx, route, ExchangeIdentity{CorrelationID: "corr-baseline", LegType: "pas-claim"}, moduleBuild{
					Builder: b.builder, ContentType: "application/fhir+json", Body: body,
				})
				var sce *SemanticChangeError
				switch {
				case errors.As(err, &sce) && from == "2.0":
					// Only the gated 2.0->2.1 up-step refuses these builds.
					refused++
					continue
				case err != nil || status != 0 || rec.rule != "":
					t.Fatalf("%s at %s walked to %s: %d %q %v (refusal %q)", b.name, from, to, status, msg, err, rec.rule)
				}
				if bytes.Equal(built.Bytes, body) {
					continue
				}
				if p := built.Payload; p.Ownership() != relay.OwnershipEdited || p.Builder() != b.builder || !slices.Equal(p.Edits(), []relay.EditID{relay.EditRegisteredTransform}) {
					t.Fatalf("%s at %s walked to %s sealed %v", b.name, from, to, p)
				}
				verified++
			}
		}
	}
	if verified == 0 {
		t.Fatal("no walk reached the verifier: the baseline proves nothing")
	}
	t.Logf("%d walks verified, %d refused by a gated step", verified, refused)
}

// The transform's verifier is the compat manifest's own steps, never the
// chain object the walk was handed: registeredRederive matches each step of the
// route to the manifest's row and runs that row's functions. A route whose
// chain is changed after the match (its step function swapped for a stand-in)
// is still verified against the manifest's own step, and the stand-in never
// runs.
func TestRegisteredRederive_RunsTheManifestsOwnSteps(t *testing.T) {
	route, in := pasWalk(t)
	x := ExchangeIdentity{CorrelationID: "corr-e09"}
	rederive, err := registeredRederive(route, x)
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	stand := func(b []byte, _ ExchangeIdentity) ([]byte, LossReport, error) {
		ran = true
		return []byte(`{"resourceType":"Bundle","stand-in":true}`), LossReport{}, nil
	}
	for i := range route.Chain {
		route.Chain[i].Up, route.Chain[i].Down = stand, stand
	}
	got, err := rederive(bytes.Clone(in))
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := applyChain(chainFor("pa.pas", "2.0", "2.2"), "2.0", bytes.Clone(in), x)
	if err != nil {
		t.Fatal(err)
	}
	if ran || !bytes.Equal(got, want) {
		t.Fatalf("the verifier ran the route's chain object (stand-in ran=%v), not the manifest's own steps", ran)
	}
}
