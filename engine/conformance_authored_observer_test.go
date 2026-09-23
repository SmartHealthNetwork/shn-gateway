package engine

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// PCV-04: an explicit authored target retains its selected profile and
// exact immutable bytes. Optional checks share one evidence call and do not
// run on a delivery goroutine; strict keeps its synchronous refusal semantics.
func TestAuthoredTargetPolicyAndExactObservation(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			var calls atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			const profile = baseQRProfile
			body := []byte(" \n" + `{"resourceType":"QuestionnaireResponse","note":"\u0061"}` + "\t")
			original := bytes.Clone(body)
			v := observationValidator(func(ctx context.Context, got []byte, selected string) (shnsdk.ValidationEvidence, error) {
				calls.Add(1)
				if selected != profile || !bytes.Equal(got, original) {
					t.Errorf("selected target changed: profile=%q body=%s", selected, got)
				}
				if level == EnforcementObserve || level == EnforcementBasic {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return shnsdk.ValidationEvidence{}, ctx.Err()
					}
				}
				return *syntheticEvidence(), nil
			})
			g := newObservationGateway(t, level, v)
			fc := findingContext{LegType: "dtr-questionnaire-fetch", CorrelationID: "authored-corr", Seam: "provider-native", Whose: "own"}
			if got := g.validateGoverned(context.Background(), fc, v, body, "egress", "2.2", profile, false); got.Status != 0 {
				t.Fatalf("valid target refused: %+v", got)
			}
			if level == EnforcementObserve || level == EnforcementBasic {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("authored target was not observed")
				}
				copy(body, bytes.Repeat([]byte("x"), len(body)))
				close(release)
				observationFlush(t, g)
				rows, dropped := g.ConformanceObservationsForTest()
				if dropped != 0 || len(rows) != 2 {
					t.Fatalf("authored findings=%d dropped=%d", len(rows), dropped)
				}
				for i, id := range []string{"fhir.profile", "fhir.terminology"} {
					if rows[i].Rule != id || rows[i].State != CheckValid || rows[i].Action != "not_enforced" || rows[i].Profile != profile || rows[i].PayloadSHA256 != sha256hex(original) {
						t.Fatalf("authored finding %d: %+v", i, rows[i])
					}
				}
				if g.observationMemory.bytes != 0 {
					t.Fatal("authored snapshot reservation leaked")
				}
			} else {
				observationFlush(t, g)
				if g.certification != nil {
					t.Fatal("off/strict started optional worker")
				}
			}
			want := int32(1)
			if level == EnforcementNone {
				want = 0
			}
			if calls.Load() != want {
				t.Fatalf("validator calls=%d want %d", calls.Load(), want)
			}
		})
	}
}

func TestAuthoredObservationFailureAndPanicRemainUnavailable(t *testing.T) {
	for _, row := range []struct {
		name      string
		validator observationValidator
	}{
		{"outage", func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
			return shnsdk.ValidationEvidence{}, errors.New("private resource text")
		}},
		{"panic", func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
			panic("private resource text")
		}},
		{"terminology", func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
			ev := *syntheticEvidence()
			ev.Terminology = shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationUnavailable}
			return ev, nil
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			g := newObservationGateway(t, EnforcementObserve, row.validator)
			body := []byte(`{"resourceType":"QuestionnaireResponse"}`)
			if got := g.validateGoverned(context.Background(), findingContext{LegType: "dtr-questionnaire-fetch", Whose: "own"}, row.validator, body, "egress", "2.1", "selected", false); got.Status != 0 {
				t.Fatalf("observe refused: %+v", got)
			}
			observationFlush(t, g)
			rows, _ := g.ConformanceObservationsForTest()
			if len(rows) != 2 || rows[1].State != CheckUnavailable || (row.name != "terminology" && rows[0].State != CheckUnavailable) {
				t.Fatalf("unavailable authored evidence: %+v", rows)
			}
		})
	}
}

func TestAuthoredObservationBlockedValidatorCancels(t *testing.T) {
	entered := make(chan struct{})
	v := observationValidator(func(ctx context.Context, _ []byte, _ string) (shnsdk.ValidationEvidence, error) {
		close(entered)
		<-ctx.Done()
		return shnsdk.ValidationEvidence{}, ctx.Err()
	})
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Validator: v}}
	g.startCertification()
	body := []byte(`{"resourceType":"QuestionnaireResponse"}`)
	if got := g.validateGoverned(context.Background(), findingContext{LegType: "dtr-questionnaire-fetch"}, v, body, "egress", "2.2", "selected", false); got.Status != 0 {
		t.Fatalf("blocked observation affected delivery: %+v", got)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("validator did not begin")
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if g.observationMemory.bytes != 0 {
		t.Fatal("canceled authored target leaked memory")
	}
}

func TestValidateResultRequiresProfileAndTerminologyEvidence(t *testing.T) {
	for _, row := range []struct {
		name, want string
		state      shnsdk.ValidationState
	}{
		{"valid", "valid", shnsdk.ValidationValid},
		{"invalid", "invalid", shnsdk.ValidationInvalid},
		{"unavailable", "validator unavailable", shnsdk.ValidationUnavailable},
		{"not-applicable", "validator unavailable", shnsdk.ValidationNotApplicable},
	} {
		t.Run(row.name, func(t *testing.T) {
			var events []ObserverEvent
			g := &Gateway{cfg: Config{Observer: func(e ObserverEvent) { events = append(events, e) }}}
			inner := observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
				ev := *syntheticEvidence()
				ev.Terminology.State = row.state
				return ev, nil
			})
			v := observingValidator{g: g, inner: inner}
			if _, err := v.ValidateEvidence(context.Background(), []byte(`{"resourceType":"Bundle"}`), ""); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := g.WaitObserverCompletion(ctx); err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].Kind != "validate.result" || events[0].Detail != row.want {
				t.Fatalf("capture=%+v want %q", events, row.want)
			}
		})
	}
}
