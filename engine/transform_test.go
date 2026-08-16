package engine

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// TestApplyChainDeterministic pins the determinism invariant: the
// same steps + same inputs run twice produce byte-identical outputs AND
// byte-identical (structurally equal) LossReports. A stub step exercises both
// Carried and Synthesized entries so the whole LossReport shape is covered, not
// just Module/Source/Target.
func TestApplyChainDeterministic(t *testing.T) {
	stubUp := func(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
		out := append([]byte{}, payload...)
		out = append(out, []byte("|corr="+x.CorrelationID)...)
		return out, LossReport{
			Module: "test.stub 1.0->1.1",
			Source: "1.0",
			Target: "1.1",
			Carried: []LossEntry{
				{Path: "Test.legacyField", Detail: "carried; source line 1.0"},
			},
			Synthesized: []LossEntry{
				{Path: "Test.identifier", Detail: "synthesized from correlation id"},
			},
		}, nil
	}
	steps := []CompatStep{
		{Contract: "test.stub", From: "1.0", To: "1.1", Class: StepFull, Up: stubUp},
	}
	x := ExchangeIdentity{CorrelationID: "corr-123"}
	in := []byte(`{"a":1}`)

	out1, reports1, err1 := applyChain(steps, "1.0", in, x)
	if err1 != nil {
		t.Fatalf("run 1: unexpected error: %v", err1)
	}
	out2, reports2, err2 := applyChain(steps, "1.0", in, x)
	if err2 != nil {
		t.Fatalf("run 2: unexpected error: %v", err2)
	}

	if !bytes.Equal(out1, out2) {
		t.Fatalf("non-deterministic output: run1=%q run2=%q", out1, out2)
	}
	if !reflect.DeepEqual(reports1, reports2) {
		t.Fatalf("non-deterministic reports: run1=%+v run2=%+v", reports1, reports2)
	}
	if len(reports1) != 1 || reports1[0].Module != "test.stub 1.0->1.1" {
		t.Fatalf("unexpected report shape: %+v", reports1)
	}
}

// TestApplyChainAbortsOnStepError proves the loss policy's "refuse only on
// semantic change" rule at the executor level: a chain whose second step
// refuses aborts the WHOLE chain — no partial output from the first
// (successful) step escapes to the caller.
func TestApplyChainAbortsOnStepError(t *testing.T) {
	refuseErr := errors.New("semantic change: no honest byte-level source")
	firstOK := func(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
		return append([]byte{}, payload...), LossReport{Module: "test.stub 1.0->1.1", Source: "1.0", Target: "1.1"}, nil
	}
	secondRefuses := func(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
		return nil, LossReport{}, refuseErr
	}
	steps := []CompatStep{
		{Contract: "test.stub", From: "1.0", To: "1.1", Class: StepFull, Up: firstOK},
		{Contract: "test.stub", From: "1.1", To: "1.2", Class: StepGated, Up: secondRefuses},
	}
	x := ExchangeIdentity{CorrelationID: "corr-abort"}
	in := []byte(`{"a":1}`)

	out, reports, err := applyChain(steps, "1.0", in, x)
	if err == nil {
		t.Fatalf("expected an error from the refusing step, got nil (out=%q reports=%+v)", out, reports)
	}
	if !errors.Is(err, refuseErr) {
		t.Fatalf("expected the step's own error to propagate (errors.Is), got: %v", err)
	}
	if out != nil {
		t.Fatalf("expected no partial output to escape an aborted chain, got %q", out)
	}
	if reports != nil {
		t.Fatalf("expected no partial reports to escape an aborted chain, got %+v", reports)
	}
}

// TestIdentityStepPassesThrough pins nil-Up/nil-Down semantics for a Class ==
// "full" identity step: the payload passes through byte-identical and the
// step's LossReport carries no Carried/Synthesized entries (nothing was lost
// or minted — only Module/Source/Target trace which step ran).
func TestIdentityStepPassesThrough(t *testing.T) {
	steps := []CompatStep{
		{Contract: "pa.crd", From: "2.0", To: "2.1", Class: StepFull}, // Up/Down both nil
	}
	x := ExchangeIdentity{CorrelationID: "corr-identity"}
	in := []byte(`{"resourceType":"Bundle"}`)

	out, reports, err := applyChain(steps, "2.0", in, x)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("identity step must pass the payload through byte-identical: got %q want %q", out, in)
	}
	if len(reports) != 1 {
		t.Fatalf("expected exactly one LossReport (one step), got %d: %+v", len(reports), reports)
	}
	r := reports[0]
	if len(r.Carried) != 0 || len(r.Synthesized) != 0 {
		t.Fatalf("identity step's LossReport must carry no Carried/Synthesized entries, got %+v", r)
	}
}

// TestApplyChainMultiStepConnectsInOrder proves the walking-direction logic:
// a two-step chain low->high calls Up on both steps in order, threading the
// intermediate line correctly (2.0->2.1->2.2).
func TestApplyChainMultiStepConnectsInOrder(t *testing.T) {
	var calledFrom []string
	mk := func(from, to string) TransformFunc {
		return func(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
			calledFrom = append(calledFrom, from+"->"+to)
			return payload, LossReport{Module: "test.stub " + from + "->" + to, Source: from, Target: to}, nil
		}
	}
	steps := []CompatStep{
		{Contract: "test.stub", From: "2.0", To: "2.1", Class: StepFull, Up: mk("2.0", "2.1")},
		{Contract: "test.stub", From: "2.1", To: "2.2", Class: StepFull, Up: mk("2.1", "2.2")},
	}
	in := []byte(`{}`)
	_, reports, err := applyChain(steps, "2.0", in, ExchangeIdentity{CorrelationID: "c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"2.0->2.1", "2.1->2.2"}
	if !reflect.DeepEqual(calledFrom, want) {
		t.Fatalf("steps ran out of order: got %v want %v", calledFrom, want)
	}
	if len(reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(reports))
	}
}
