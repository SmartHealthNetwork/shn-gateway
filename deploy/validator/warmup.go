package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const stateSchema = 2 // Bump when the row table or interpretation changes.
const maxStateBytes = 16 << 10

type markerState struct {
	Schema  int       `json:"schema"`
	Line    string    `json:"line"`
	Key     string    `json:"key"`
	State   string    `json:"state"`
	Warm    []string  `json:"warm"`
	Row     string    `json:"row,omitempty"`
	FiredAt time.Time `json:"fired_at,omitempty"`
	Failure string    `json:"failure,omitempty"`
}

// A temp file in the same directory makes replacement atomic across observers.
// Failure is terminal to the writer, including close/rename failures.
func writeState(path string, st markerState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".shn-validator-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func readState(path, line, key string) (markerState, error) {
	var st markerState
	if key == "" {
		return st, errors.New("identity unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return st, err
	}
	defer f.Close()
	raw, err := readBounded(f, maxStateBytes)
	if err != nil {
		return st, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&st); err != nil {
		return st, err
	}
	if dec.Decode(new(any)) != io.EOF {
		return st, errors.New("invalid state")
	}
	if !validState(st, line, key) {
		return markerState{}, errors.New("invalid state")
	}
	return st, nil
}
func validState(st markerState, line, key string) bool {
	if key == "" || st.Key != key || st.Schema != stateSchema || st.Line != line {
		return false
	}
	rows := readinessRows(line)
	if len(rows) == 0 {
		return false
	}
	if len(st.Warm) > len(rows) {
		return false
	}
	for i, identity := range st.Warm {
		if identity != rows[i].identity {
			return false
		}
	}
	switch st.State {
	case "waiting-for-metadata":
		return len(st.Warm) == 0 && st.Row == "" && st.FiredAt.IsZero() && st.Failure == ""
	case "warming":
		return len(st.Warm) < len(rows) && st.Row == rows[len(st.Warm)].identity && !st.FiredAt.IsZero() && st.Failure == ""
	case "ready":
		return len(st.Warm) == len(rows) && st.Row == "" && st.FiredAt.IsZero() && st.Failure == ""
	case "failed":
		if !safeFailure(st.Failure) {
			return false
		}
		if st.Row == "" {
			return true
		}
		for _, row := range rows {
			if row.identity == st.Row {
				return true
			}
		}
		return false
	default:
		return false
	}
}
func safeFailure(s string) bool {
	switch s {
	case "metadata unavailable", "request failed", "response incomplete", "response oversized", "redirect refused", "wrong response status", "not an OperationOutcome", "invalid outcome", "unexpected verdict", "configuration invalid", "state publication failed", "identity unavailable", "fixture unavailable", "request invalid", "child exited", "child launch failed", "startup canceled":
		return true
	}
	return false
}

// runWarmup has one lifetime, owned by the supervisor. It never resumes/retries
// a dispatched row, even when an observer deletes state or the HTTP server keeps
// processing after client cancellation. A fresh JVM is the recovery boundary.
func runWarmup(ctx context.Context, base, path string, st markerState) error {
	if st.Key == "" || !validState(st, st.Line, st.Key) || st.State != "waiting-for-metadata" {
		return errors.New("identity unavailable")
	}
	fail := func(reason string) error {
		st.State = "failed"
		st.Failure = reason
		if err := writeState(path, st); err != nil {
			reason = "state publication failed"
		}
		elapsed := time.Duration(0)
		if !st.FiredAt.IsZero() {
			elapsed = time.Since(st.FiredAt).Round(time.Millisecond)
		}
		fmt.Fprintf(os.Stderr, "warmup: line=%s row=%s elapsed=%s outcome=failed reason=%s\n", st.Line, st.Row, elapsed, reason)
		return errors.New(reason)
	}
	client := httpClient()
	for {
		attempt, cancel := context.WithTimeout(ctx, probeBudget)
		err := metadata(attempt, client, base+"/metadata")
		cancel()
		if err == nil {
			break
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fail("metadata unavailable")
		case <-timer.C:
		}
	}
	rows := readinessRows(st.Line)
	for _, row := range rows {
		if ctx.Err() != nil {
			return fail("startup canceled")
		}
		st.State = "warming"
		st.Row = row.identity
		st.FiredAt = time.Now().UTC()
		if err := writeState(path, st); err != nil {
			return fail("state publication failed")
		}
		fmt.Fprintf(os.Stderr, "warmup: line=%s row=%s elapsed=0s outcome=started\n", st.Line, st.Row)
		if err := validate(ctx, client, base, row); err != nil {
			return fail(err.Error())
		}
		elapsed := time.Since(st.FiredAt)
		st.Warm = append(st.Warm, row.identity)
		// Publish completed progress with the next row's in-flight state before
		// dispatch. The final row instead publishes ready immediately after EOF.
		if len(st.Warm) == len(rows) {
			st.State = "ready"
			st.Row = ""
			st.FiredAt = time.Time{}
			if err := writeState(path, st); err != nil {
				return fail("state publication failed")
			}
		}
		fmt.Fprintf(os.Stderr, "warmup: line=%s row=%s elapsed=%s outcome=completed\n", st.Line, row.identity, elapsed.Round(time.Millisecond))
	}
	return nil
}
