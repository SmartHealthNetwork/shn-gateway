package engine

import (
	"os"
	"path/filepath"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// payerRouterFor builds the one-entry test payer router (CMSPayerIdentity → holderID), matching the
// single mapping the devstack/harness/compose provider directories carry (FR-G40; no default).
func payerRouterFor(t *testing.T, holderID string) PayerRouter {
	t.Helper()
	r, err := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: shnsdk.CMSPayerIdentity.System, Value: shnsdk.CMSPayerIdentity.Value, HolderID: holderID},
	})
	if err != nil {
		t.Fatalf("payerRouterFor: %v", err)
	}
	return r
}

func TestConfigPayerRouter(t *testing.T) {
	r, err := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: "s", Value: "00001", HolderID: "conformance-payer"},
		{System: "s", Value: "00078", HolderID: "acme-health"},
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if h, ok := r.Resolve(shnsdk.PayerIdentifier{System: "s", Value: "00078"}); !ok || h != "acme-health" {
		t.Fatalf("resolve hit: got (%q,%v)", h, ok)
	}
	if _, ok := r.Resolve(shnsdk.PayerIdentifier{System: "s", Value: "99999"}); ok {
		t.Fatalf("resolve miss should be ok=false")
	}
}

func TestConfigPayerRouterDuplicateIsError(t *testing.T) {
	_, err := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: "s", Value: "00001", HolderID: "a"},
		{System: "s", Value: "00001", HolderID: "b"},
	})
	if err == nil {
		t.Fatal("duplicate (system,value) must be a hard error")
	}
}

func TestLoadPayerDirectory(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "payers.json")
		const body = `[
			{"system": "s", "value": "00001", "holderId": "conformance-payer"},
			{"system": "s", "value": "00078", "holderId": "acme-health"}
		]`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		entries, err := LoadPayerDirectory(path)
		if err != nil {
			t.Fatalf("LoadPayerDirectory: %v", err)
		}
		want := []PayerDirectoryEntry{
			{System: "s", Value: "00001", HolderID: "conformance-payer"},
			{System: "s", Value: "00078", HolderID: "acme-health"},
		}
		if len(entries) != len(want) {
			t.Fatalf("got %d entries, want %d: %+v", len(entries), len(want), entries)
		}
		for i := range want {
			if entries[i] != want[i] {
				t.Fatalf("entry %d: got %+v, want %+v", i, entries[i], want[i])
			}
		}
	})

	t.Run("read error", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "does-not-exist.json")
		if _, err := LoadPayerDirectory(missing); err == nil {
			t.Fatal("nonexistent path must be a hard error")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte(`"{not json`), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		if _, err := LoadPayerDirectory(path); err == nil {
			t.Fatal("malformed JSON must be a hard error")
		}
	})
}

func TestFeedPayerRouterResolvesAndFailsClosed(t *testing.T) {
	reg := shnsdk.NewRegistry()
	reg.Set("payer-a", shnsdk.RegistryEntry{ID: "payer-a", Role: "payer",
		PayerIDs: []shnsdk.PayerIdentifier{{System: "s", Value: "00001"}}})
	reg.Set("payer-b", shnsdk.RegistryEntry{ID: "payer-b", Role: "payer",
		PayerIDs: []shnsdk.PayerIdentifier{{System: "s", Value: "00078"}}})
	r := NewFeedPayerRouter(reg)

	if h, ok := r.Resolve(shnsdk.PayerIdentifier{System: "s", Value: "00078"}); !ok || h != "payer-b" {
		t.Fatalf("resolve hit: got (%q,%v)", h, ok)
	}
	if _, ok := r.Resolve(shnsdk.PayerIdentifier{System: "s", Value: "99999"}); ok {
		t.Fatalf("miss must be ok=false")
	}
	// A non-payer entry carrying a payer-id (e.g. a manifest-seeded holder that loads OUTSIDE the
	// registrar role-gate) must NEVER resolve — the router self-fails-closed on role (defense in
	// depth; does not trust the upstream 400-gate as the sole guarantee).
	reg.Set("facility-x", shnsdk.RegistryEntry{ID: "facility-x", Role: "facility",
		PayerIDs: []shnsdk.PayerIdentifier{{System: "s", Value: "12345"}}})
	if _, ok := r.Resolve(shnsdk.PayerIdentifier{System: "s", Value: "12345"}); ok {
		t.Fatalf("a non-payer entry must not resolve (role filter)")
	}
	// Collision: a second holder claims the SAME (system,value) → ambiguous → fail closed (AI-G12).
	reg.Set("payer-c", shnsdk.RegistryEntry{ID: "payer-c", Role: "payer",
		PayerIDs: []shnsdk.PayerIdentifier{{System: "s", Value: "00078"}}})
	if _, ok := r.Resolve(shnsdk.PayerIdentifier{System: "s", Value: "00078"}); ok {
		t.Fatalf("feed collision must fail closed (ok=false)")
	}
	// Same holder duplicated in own PayerIDs: not a cross-holder collision, should resolve to that holder.
	// This guards against regression if Resolve is refactored to use a match-counter or seen set (AI-G12).
	reg.Set("payer-dup", shnsdk.RegistryEntry{ID: "payer-dup", Role: "payer",
		PayerIDs: []shnsdk.PayerIdentifier{
			{System: "s", Value: "dup01"},
			{System: "s", Value: "dup01"},
		}})
	if h, ok := r.Resolve(shnsdk.PayerIdentifier{System: "s", Value: "dup01"}); !ok || h != "payer-dup" {
		t.Fatalf("same-holder duplicate payer-id: got (%q,%v), want (payer-dup,true)", h, ok)
	}
}
