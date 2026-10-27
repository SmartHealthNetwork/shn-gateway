package engine

import (
	"errors"
	"testing"
)

// stateWithoutClaims is a workstream's in-memory state that keeps no claims.
type stateWithoutClaims struct{}

func (stateWithoutClaims) reset() {}

// stateWithClaims is one that keeps claims (answered here by noClaimKeeper's
// methods: the guard reads only whether a state keeps them).
type stateWithClaims struct{ noClaimKeeper }

func (stateWithClaims) reset()                          {}
func (s stateWithClaims) claimUpdates() memClaimUpdates { return s }

// TestMemStore_NoClaimKeeperBuildsAndClaimsNothing: an in-memory Store over
// workstreams none of which keeps claims (a core-only build) still builds, and
// every claim-update method refuses with errNoClaimKeeper: nothing is claimed.
// Two workstreams that keep claims still refuse to build.
func TestMemStore_NoClaimKeeperBuildsAndClaimsNothing(t *testing.T) {
	d := newMemStoreFrom(map[string]memState{"a": stateWithoutClaims{}})
	if err := d.RecordPendedClaim("pci:s", "c"); !errors.Is(err, errNoClaimKeeper) {
		t.Fatalf("RecordPendedClaim: %v, want errNoClaimKeeper", err)
	}
	if begun, err := d.BeginClaimUpdate("pci:s", "c"); begun || !errors.Is(err, errNoClaimKeeper) {
		t.Fatalf("BeginClaimUpdate: %v %v, want not begun and errNoClaimKeeper", begun, err)
	}
	if err := d.ReleaseClaimUpdate("pci:s", "c"); !errors.Is(err, errNoClaimKeeper) {
		t.Fatalf("ReleaseClaimUpdate: %v, want errNoClaimKeeper", err)
	}
	if err := d.FinalizeClaimUpdate("pci:s", "c"); !errors.Is(err, errNoClaimKeeper) {
		t.Fatalf("FinalizeClaimUpdate: %v, want errNoClaimKeeper", err)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("two workstreams keeping claims built an in-memory Store")
		}
	}()
	newMemStoreFrom(map[string]memState{"a": stateWithClaims{}, "b": stateWithClaims{}})
}
