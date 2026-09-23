package engine

import (
	"context"
	"encoding/json"
	"errors"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// These helpers explicitly opt a synthetic scenario into support. They certify
// only the controlled fixture, never a real endpoint, IG or terminology service.
func syntheticEvidence() *shnsdk.ValidationEvidence {
	return &shnsdk.ValidationEvidence{ExecutionAttempted: true, Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid, Code: "synthetic"}, Terminology: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid, Code: "synthetic"}}
}
func syntheticFakeValidator() *shnsdk.FakeValidator {
	return &shnsdk.FakeValidator{Evidence: syntheticEvidence()}
}
func syntheticLineValidator(line string) *LineFakeValidator {
	v := NewLineFakeValidator(line)
	v.Evidence = syntheticEvidence()
	return v
}

func (s *findingSpyValidator) ValidateEvidence(ctx context.Context, body []byte, _ string) (shnsdk.ValidationEvidence, error) {
	var probe struct{ ResourceType string }
	_ = json.Unmarshal(body, &probe)
	s.mu.Lock()
	s.calls = append(s.calls, findingSpyCall{resourceType: probe.ResourceType, fc: findingContextFrom(ctx)})
	s.mu.Unlock()
	return *syntheticEvidence(), nil
}
func (failingValidator) ValidateEvidence(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
	return unavailableValidatorEvidence(), errors.New("boom")
}
func (v failIfCalledValidator) ValidateEvidence(ctx context.Context, body []byte, profile string) (shnsdk.ValidationEvidence, error) {
	_, err := v.Validate(ctx, body, profile)
	return unavailableValidatorEvidence(), err
}

func (v *recordingValidator) ValidateEvidence(_ context.Context, body []byte, _ string) (shnsdk.ValidationEvidence, error) {
	v.calls = append(v.calls, body)
	ev := *syntheticEvidence()
	if !v.valid {
		ev.Profile.State = shnsdk.ValidationInvalid
		ev.Profile.Code = "synthetic-rejection"
	}
	return ev, nil
}
func (v *pasAssemblyValidator) ValidateEvidence(ctx context.Context, _ []byte, profile string) (shnsdk.ValidationEvidence, error) {
	v.profile = profile
	v.calls++
	if err := ctx.Err(); err != nil {
		return unavailableValidatorEvidence(), err
	}
	if v.err != nil {
		return unavailableValidatorEvidence(), v.err
	}
	ev := *syntheticEvidence()
	if !v.valid {
		ev.Profile.State = shnsdk.ValidationInvalid
		ev.Profile.Code = "synthetic-rejection"
	}
	return ev, nil
}
func (v *dtrRecordingValidator) ValidateEvidence(ctx context.Context, body []byte, profile string) (shnsdk.ValidationEvidence, error) {
	ev, err := delegateValidatorEvidence(ctx, v.base, body, profile)
	var probe struct{ ResourceType string }
	_ = json.Unmarshal(body, &probe)
	if v.mode == "QR rejection" && probe.ResourceType == "QuestionnaireResponse" || v.mode == "Provenance rejection" && probe.ResourceType == "Provenance" {
		ev.Profile = shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationInvalid, Code: "synthetic-rejection"}
	}
	if v.mode == "validator outage" {
		ev = unavailableValidatorEvidence()
		err = errors.New("controlled unavailable validator")
	}
	v.calls = append(v.calls, dtrRecordedValidation{append([]byte(nil), body...), profile, ev.Profile.State == shnsdk.ValidationValid && err == nil})
	return ev, err
}

// syntheticEvidenceValidatorFunc is an explicitly supported test checker. It
// preserves the chosen legacy verdict for worker tests; it is not a real adapter.
type syntheticEvidenceValidatorFunc func([]byte) (shnsdk.Result, error)

func (f syntheticEvidenceValidatorFunc) Validate(_ context.Context, b []byte, _ string) (shnsdk.Result, error) {
	return f(b)
}
func (f syntheticEvidenceValidatorFunc) ValidateEvidence(ctx context.Context, b []byte, p string) (shnsdk.ValidationEvidence, error) {
	r, err := f.Validate(ctx, b, p)
	e := *syntheticEvidence()
	if err != nil {
		return shnsdk.ValidationEvidence{}, err
	}
	if !r.Valid {
		e.Profile.State = shnsdk.ValidationInvalid
		e.Profile.Code = "synthetic-rejection"
	}
	return e, nil
}
