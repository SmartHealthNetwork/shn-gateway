package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type legacyEvidenceProbe struct{ calls atomic.Int32 }

func (v *legacyEvidenceProbe) Validate(context.Context, []byte, string) (shnsdk.Result, error) {
	v.calls.Add(1)
	return shnsdk.Result{Valid: true}, nil
}

func TestValidatorEvidenceGatewayConstruction(t *testing.T) {
	for _, placement := range []string{"default", "per-line", "discovered-default"} {
		for _, kind := range []string{"operation", "wrapper", "qualified", "unqualified", "gated-ready", "gated-own-ready", "gated-pending", "legacy", "qualified-legacy"} {
			// The default-discovery map specifically stores discovered lanes.
			if placement == "discovered-default" && kind != "qualified" && kind != "unqualified" && kind != "qualified-legacy" {
				continue
			}
			t.Run(placement+"/"+kind, func(t *testing.T) {
				var first shnsdk.ValidationEvidence
				for _, observe := range []bool{false, true} {
					var hits atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						hits.Add(1)
						if kind == "wrapper" {
							fmt.Fprint(w, `{"outcomes":[{"issues":[{"level":"WARNING","type":"INVARIANT","message":"PRIVATE-SENTINEL"}]}]}`)
						} else {
							fmt.Fprint(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"invariant","diagnostics":"PRIVATE-SENTINEL"}]}`)
						}
					}))
					defer srv.Close()
					var inner shnsdk.Validator = shnsdk.NewOperationValidator(srv.URL)
					legacy := &legacyEvidenceProbe{}
					if kind == "wrapper" {
						inner = shnsdk.NewHTTPValidator(srv.URL)
					}
					if kind == "legacy" || kind == "qualified-legacy" {
						inner = legacy
					}
					lane := NewDiscoveredLane("2.2", srv.URL, inner)
					ready := kind == "qualified" || kind == "gated-ready" || kind == "qualified-legacy"
					if ready {
						if err := lane.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
							t.Fatal(err)
						}
					}
					switch kind {
					case "qualified", "unqualified", "qualified-legacy":
						inner = lane
					case "gated-ready", "gated-own-ready", "gated-pending":
						v := NewGatedCertificationValidator(lane, nil, "test configuration")
						if kind == "gated-own-ready" {
							v.ready = true
						}
						defer v.Close()
						inner = v
					}
					sor := newCensusSoR()
					_, priv := genED25519(t)
					cfg := Config{Role: "provider", HolderID: "provider", Identity: shnsdk.Identity{SignPriv: priv}, SoR: sor, Store: sor, ConformanceEnforcement: EnforcementStrict}
					var events []ObserverEvent
					if observe {
						cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
					}
					switch placement {
					case "default":
						cfg.Validator = inner
					case "per-line":
						cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.2": inner}
					case "discovered-default":
						cfg.DefaultValidatorsByLine = map[string]*DiscoveredLane{"2.2": lane}
					}
					g := mustNew(t, cfg)
					selected := g.validatorForLine("2.2")
					var ev shnsdk.ValidationEvidence
					var err error
					if selected == nil {
						ev = unavailableValidatorEvidence()
						err = errors.New("unqualified")
					} else if evidence, ok := selected.(shnsdk.EvidenceValidator); ok {
						ev, err = evidence.ValidateEvidence(context.Background(), []byte(`{"resourceType":"Patient"}`), "http://example.org/profile")
					} else {
						ev = unavailableValidatorEvidence()
						err = errors.New("legacy")
					}
					unavailable := kind == "unqualified" || kind == "gated-pending" || kind == "legacy" || kind == "qualified-legacy"
					if unavailable {
						if ev.Profile.State != shnsdk.ValidationUnavailable || hits.Load() != 0 || legacy.calls.Load() != 0 {
							t.Fatalf("unavailable gate called inner or certified: %+v hits=%d legacy=%d", ev, hits.Load(), legacy.calls.Load())
						}
					} else {
						if err != nil || ev.Profile.State != shnsdk.ValidationValid || hits.Load() != 1 || len(ev.Profile.Issues) != 1 || ev.Profile.Issues[0] != (shnsdk.ValidationIssue{Severity: "warning", Code: "invariant"}) {
							t.Fatalf("lost single-call warning evidence: %+v err=%v hits=%d", ev, err, hits.Load())
						}
					}
					if ev.Terminology.State != shnsdk.ValidationUnavailable {
						t.Fatalf("invented terminology support: %+v", ev)
					}
					if !observe {
						first = ev
					} else if !reflect.DeepEqual(first, ev) {
						t.Fatalf("inspection changed evidence: off=%+v on=%+v", first, ev)
					}
					wantEvents := 0
					if observe && selected != nil {
						wantEvents = 1
					}
					observationFlush(t, g)
					if len(events) != wantEvents {
						t.Fatalf("events=%+v want %d", events, wantEvents)
					}
					if len(events) > 0 {
						// The profile can be valid while terminology support remains
						// unavailable; the overall observation cannot certify valid.
						want := "validator unavailable"
						if events[0].Detail != want || events[0].Kind != "validate.result" {
							t.Fatalf("unsafe/changed event: %+v", events)
						}
					}
				}
			})
		}
	}
}

func TestLineFakeValidatorEvidenceExplicit(t *testing.T) {
	v := NewLineFakeValidator("2.2")
	ev, err := v.ValidateEvidence(context.Background(), []byte(`{"resourceType":"Patient"}`), "")
	if err != nil || ev.Profile.State != shnsdk.ValidationUnavailable {
		t.Fatalf("implicit fake support: %+v %v", ev, err)
	}
	v.Evidence = &shnsdk.ValidationEvidence{Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid, Code: "synthetic"}, Terminology: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid, Code: "synthetic"}}
	ev, err = v.ValidateEvidence(context.Background(), []byte(`{"resourceType":"Claim"}`), "")
	if err != nil || ev.Profile.State != shnsdk.ValidationInvalid || ev.Terminology.State != shnsdk.ValidationValid || len(v.Calls()) != 2 {
		t.Fatalf("fake scoped rejection: %+v %v calls=%d", ev, err, len(v.Calls()))
	}
}

func TestValidatorEvidenceSyntheticFixtureSemantics(t *testing.T) {
	ctx := context.Background()
	body := []byte(`{"resourceType":"Patient"}`)
	recorder := &recordingValidator{valid: false}
	ev, err := recorder.ValidateEvidence(ctx, body, "profile")
	if err != nil || ev.Profile.State != shnsdk.ValidationInvalid || len(recorder.calls) != 1 || ev.Terminology.State != shnsdk.ValidationValid {
		t.Fatal(ev, err, recorder.calls)
	}
	assembly := &pasAssemblyValidator{valid: false}
	ev, err = assembly.ValidateEvidence(ctx, body, "profile")
	if err != nil || ev.Profile.State != shnsdk.ValidationInvalid || assembly.profile != "profile" {
		t.Fatal(ev, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	ev, err = assembly.ValidateEvidence(canceled, body, "profile")
	if !errors.Is(err, context.Canceled) || ev.Profile.State != shnsdk.ValidationUnavailable {
		t.Fatal(ev, err)
	}
	assembly.err = errors.New("outage")
	ev, err = assembly.ValidateEvidence(ctx, body, "profile")
	if err == nil || ev.Profile.State != shnsdk.ValidationUnavailable {
		t.Fatal(ev, err)
	}
	spy := &findingSpyValidator{}
	ev, err = spy.ValidateEvidence(ctx, body, "profile")
	if err != nil || ev.Profile.State != shnsdk.ValidationValid || len(spy.calls) != 1 {
		t.Fatal(ev, err, spy.calls)
	}
	for _, mode := range []string{"QR rejection", "Provenance rejection", "validator outage", "valid"} {
		base := syntheticFakeValidator()
		v := &dtrRecordingValidator{base: base, mode: mode}
		resource := body
		if mode == "QR rejection" {
			resource = []byte(`{"resourceType":"QuestionnaireResponse"}`)
		}
		if mode == "Provenance rejection" {
			resource = []byte(`{"resourceType":"Provenance"}`)
		}
		ev, err = v.ValidateEvidence(ctx, resource, "profile")
		want := shnsdk.ValidationInvalid
		if mode == "valid" {
			want = shnsdk.ValidationValid
		}
		if mode == "validator outage" {
			want = shnsdk.ValidationUnavailable
		}
		if ev.Profile.State != want || (err != nil) != (mode == "validator outage") || len(v.calls) != 1 {
			t.Fatal(mode, ev, err, v.calls)
		}
	}
}
