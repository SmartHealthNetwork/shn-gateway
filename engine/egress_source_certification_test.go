package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// PCV-15: a real PAS translation requires source proof at every optional
// conformance level, on the precise bytes and version the chain will consume.
func TestEgressAdaptCertifiesPASSourceAtEveryLevel(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			input := pasGolden(t, "2.1/conformant/pas-submit-request.json")
			want := bytes.Clone(input)
			var sourceCalls, targetCalls int
			var sourceProfile string
			source := certificationValidatorFunc(func(_ context.Context, body []byte, profile string) (shnsdk.Result, error) {
				sourceCalls++
				sourceProfile = profile
				if !bytes.Equal(body, want) {
					t.Fatalf("source certifier received changed bytes")
				}
				return shnsdk.Result{Valid: true}, nil
			})
			target := certificationValidatorFunc(func(_ context.Context, _ []byte, _ string) (shnsdk.Result, error) {
				targetCalls++
				return shnsdk.Result{Valid: true}, nil
			})
			g := &Gateway{cfg: Config{HolderID: "source-proof", Clock: fixedEgressClock,
				ConformanceEnforcement: level,
				ValidatorsByLine:       map[string]shnsdk.Validator{"2.1": source, "2.2": target},
			}}
			route := legRoute{Token: "pa.pas@2.2", BuildLine: "2.1", Chain: chainFor("pa.pas", "2.1", "2.2")}
			out, _, err := g.egressAdapt(context.Background(), route, input, ExchangeIdentity{CorrelationID: "source-proof", LegType: "pas-claim", Counterpart: "payer"})
			if err != nil {
				t.Fatalf("egressAdapt: %v", err)
			}
			ctx := withFindingContext(context.Background(), findingContext{LegType: "pas-claim", CorrelationID: "source-proof", Seam: "originate", Whose: "own"})
			if status, msg := g.validateFHIREgressOrBridged(ctx, out, "pa.pas", "2.2", true); status != 0 {
				t.Fatalf("target proof refused: %d %s", status, msg)
			}
			wantProfile, ok := profileFor("PASRequestBundle", "2.1", "pas-claim")
			if !ok || sourceProfile != wantProfile || sourceCalls != 1 || targetCalls != 1 {
				t.Fatalf("proof source=%d profile=%q target=%d; want source profile %q and one call each", sourceCalls, sourceProfile, targetCalls, wantProfile)
			}
			if !bytes.Equal(input, want) {
				t.Fatal("adaptation mutated caller-owned source bytes")
			}
		})
	}
}

func TestEgressAdaptUsesOperationValidatorProfileEvidence(t *testing.T) {
	input := pasGolden(t, "2.1/conformant/pas-submit-request.json")
	wantProfile, ok := profileFor("PASRequestBundle", "2.1", "pas-claim")
	if !ok {
		t.Fatal("missing PAS source profile")
	}
	for _, tc := range []struct {
		name, outcome, code string
		status              int
	}{
		{"profile valid, terminology unproven", `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`, "", 0},
		{"profile invalid", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid"}]}`, "adaptation_failed", http.StatusBadGateway},
		{"profile unsupported", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","details":{"coding":[{"system":"http://hl7.org/fhir/java-core-messageId","code":"Validation_VAL_Profile_Unknown"}]}}]}`, "adaptation_unavailable", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || r.URL.Path != "/Bundle/$validate" || r.URL.Query().Get("profile") != wantProfile || !bytes.Equal(body, input) {
					t.Errorf("source request method=%s path=%s profile=%q bytes=%d err=%v", r.Method, r.URL.Path, r.URL.Query().Get("profile"), len(body), err)
				}
				w.Header().Set("Content-Type", "application/fhir+json")
				fmt.Fprint(w, tc.outcome)
			}))
			defer srv.Close()
			v := shnsdk.NewOperationValidator(srv.URL)
			if tc.status == 0 {
				ev, err := v.ValidateEvidence(context.Background(), bytes.Clone(input), wantProfile)
				if err != nil || !ev.ExecutionAttempted || ev.Profile.State != shnsdk.ValidationValid || ev.Terminology.State != shnsdk.ValidationUnavailable {
					t.Fatalf("actual validator evidence=%+v err=%v", ev, err)
				}
			}
			g := &Gateway{cfg: Config{HolderID: "source-proof", Clock: fixedEgressClock, ConformanceEnforcement: EnforcementNone,
				ValidatorsByLine: map[string]shnsdk.Validator{"2.1": v},
			}}
			route := legRoute{Token: "pa.pas@2.2", BuildLine: "2.1", Chain: chainFor("pa.pas", "2.1", "2.2")}
			out, reports, err := g.egressAdapt(context.Background(), route, input, ExchangeIdentity{CorrelationID: "operation-evidence", LegType: "pas-claim"})
			if tc.status == 0 {
				if err != nil || len(out) == 0 || len(reports) != 1 || calls != 2 {
					t.Fatalf("valid source did not adapt: calls=%d out=%d reports=%d err=%v", calls, len(out), len(reports), err)
				}
			} else {
				var classified *ingressContextError
				if !errors.As(err, &classified) || classified.status != tc.status || classified.code != tc.code || out != nil || reports != nil || calls != 1 {
					t.Fatalf("unproved source adapted: calls=%d out=%d reports=%d err=%v", calls, len(out), len(reports), err)
				}
			}
		})
	}
}

func TestEgressAdaptRejectsUnprovedSourceBeforeTransformation(t *testing.T) {
	for _, tc := range []struct {
		name, leg string
		evidence  *shnsdk.ValidationEvidence
		status    int
		code      string
	}{
		{"missing lane", "pas-claim", nil, http.StatusServiceUnavailable, "adaptation_unavailable"},
		{"invalid profile", "pas-claim", &shnsdk.ValidationEvidence{ExecutionAttempted: true, Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationInvalid}, Terminology: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid}}, http.StatusBadGateway, "adaptation_failed"},
		{"no execution", "pas-claim", &shnsdk.ValidationEvidence{Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid}, Terminology: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid}}, http.StatusServiceUnavailable, "adaptation_unavailable"},
		{"unsupported operation", "unknown", syntheticEvidence(), http.StatusServiceUnavailable, "adaptation_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var transformed bool
			step := CompatStep{Contract: "pa.pas", From: "2.1", To: "2.2", Class: StepFull,
				Up: func(b []byte, _ ExchangeIdentity) ([]byte, LossReport, error) {
					transformed = true
					return b, LossReport{}, nil
				}}
			var events []ObserverEvent
			g := &Gateway{cfg: Config{HolderID: "source-proof", Clock: fixedEgressClock,
				Observer: func(e ObserverEvent) { events = append(events, e) },
			}}
			if tc.evidence != nil {
				g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.1": observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) { return *tc.evidence, nil })}
			}
			route := legRoute{Token: "pa.pas@2.2", BuildLine: "2.1", Chain: []CompatStep{step}}
			out, reports, err := g.egressAdapt(context.Background(), route, []byte(`{"resourceType":"Bundle"}`), ExchangeIdentity{CorrelationID: "source-reject", LegType: tc.leg})
			var typed *ingressContextError
			if !errors.As(err, &typed) || typed.status != tc.status || typed.code != tc.code || out != nil || reports != nil || transformed {
				t.Fatalf("source refusal out=%s reports=%v err=%v transformed=%t; want %d %s before transform", out, reports, err, transformed, tc.status, tc.code)
			}
			observationFlush(t, g)
			if len(events) != 1 || events[0].Kind != "leg.failed" || events[0].Route == nil {
				t.Fatalf("source refusal observer events=%+v", events)
			}
		})
	}
}

func TestEgressAdaptTerminologyEvidenceDoesNotOverrideValidSourceProfile(t *testing.T) {
	for _, state := range []shnsdk.ValidationState{shnsdk.ValidationUnavailable, shnsdk.ValidationInvalid} {
		t.Run(string(state), func(t *testing.T) {
			var transformed bool
			step := CompatStep{Contract: "pa.pas", From: "2.1", To: "2.2", Class: StepFull,
				Up: func(b []byte, _ ExchangeIdentity) ([]byte, LossReport, error) {
					transformed = true
					return b, LossReport{Module: "pa.pas 2.1->2.2", Source: "2.1", Target: "2.2"}, nil
				}}
			g := &Gateway{cfg: Config{HolderID: "source-proof", Clock: fixedEgressClock,
				ValidatorsByLine: map[string]shnsdk.Validator{"2.1": observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
					return shnsdk.ValidationEvidence{ExecutionAttempted: true,
						Profile:     shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid},
						Terminology: shnsdk.ValidationCheckEvidence{State: state}}, nil
				})},
			}}
			route := legRoute{Token: "pa.pas@2.2", BuildLine: "2.1", Chain: []CompatStep{step}}
			out, _, err := g.egressAdapt(context.Background(), route, []byte(`{"resourceType":"Bundle"}`), ExchangeIdentity{CorrelationID: "term-separate", LegType: "pas-claim"})
			if err != nil || !transformed || len(out) == 0 {
				t.Fatalf("profile-valid source refused because terminology=%s: out=%s err=%v", state, out, err)
			}
		})
	}
}

func TestEgressAdaptSourceCheckerCannotMutateTransformInput(t *testing.T) {
	in := []byte(`{"resourceType":"Bundle","marker":"source"}`)
	want := bytes.Clone(in)
	var transformed []byte
	step := CompatStep{Contract: "pa.pas", From: "2.1", To: "2.2", Class: StepFull,
		Up: func(b []byte, _ ExchangeIdentity) ([]byte, LossReport, error) {
			transformed = bytes.Clone(b)
			return b, LossReport{}, nil
		}}
	g := &Gateway{cfg: Config{HolderID: "source-proof", Clock: fixedEgressClock,
		ValidatorsByLine: map[string]shnsdk.Validator{"2.1": observationValidator(func(_ context.Context, body []byte, _ string) (shnsdk.ValidationEvidence, error) {
			body[0] = 'x'
			return *syntheticEvidence(), nil
		})},
	}}
	route := legRoute{Token: "pa.pas@2.2", BuildLine: "2.1", Chain: []CompatStep{step}}
	if _, _, err := g.egressAdapt(context.Background(), route, in, ExchangeIdentity{CorrelationID: "copy", LegType: "pas-claim"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transformed, want) || !bytes.Equal(in, want) {
		t.Fatalf("validator changed transform input=%q caller source=%q", transformed, in)
	}
}

func TestEgressAdaptCanceledSourceNeverTransforms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var called, transformed bool
	step := CompatStep{Contract: "pa.pas", From: "2.1", To: "2.2", Class: StepFull,
		Up: func(b []byte, _ ExchangeIdentity) ([]byte, LossReport, error) {
			transformed = true
			return b, LossReport{}, nil
		}}
	g := &Gateway{cfg: Config{HolderID: "source-proof", Clock: fixedEgressClock,
		ValidatorsByLine: map[string]shnsdk.Validator{"2.1": observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
			called = true
			return *syntheticEvidence(), nil
		})},
	}}
	route := legRoute{Token: "pa.pas@2.2", BuildLine: "2.1", Chain: []CompatStep{step}}
	_, _, err := g.egressAdapt(ctx, route, []byte(`{"resourceType":"Bundle"}`), ExchangeIdentity{CorrelationID: "canceled", LegType: "pas-claim"})
	var typed *ingressContextError
	if !errors.As(err, &typed) || typed.code != "adaptation_unavailable" || called || transformed {
		t.Fatalf("canceled source err=%v called=%t transformed=%t", err, called, transformed)
	}
}

func TestEgressAdaptCancellationDuringSourceProofNeverTransforms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var transformed bool
	step := CompatStep{Contract: "pa.pas", From: "2.1", To: "2.2", Class: StepFull,
		Up: func(b []byte, _ ExchangeIdentity) ([]byte, LossReport, error) {
			transformed = true
			return b, LossReport{}, nil
		}}
	g := &Gateway{cfg: Config{HolderID: "source-proof", Clock: fixedEgressClock,
		ValidatorsByLine: map[string]shnsdk.Validator{"2.1": observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
			cancel()
			return *syntheticEvidence(), nil // a checker that ignores cancellation cannot authorize the edit
		})},
	}}
	route := legRoute{Token: "pa.pas@2.2", BuildLine: "2.1", Chain: []CompatStep{step}}
	_, _, err := g.egressAdapt(ctx, route, []byte(`{"resourceType":"Bundle"}`), ExchangeIdentity{CorrelationID: "mid-cancel", LegType: "pas-claim"})
	var typed *ingressContextError
	if !errors.As(err, &typed) || typed.code != "adaptation_unavailable" || transformed {
		t.Fatalf("in-flight cancellation err=%v transformed=%t", err, transformed)
	}
}
