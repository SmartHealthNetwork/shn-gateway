package shngateway_test

import (
	"os"
	"regexp"
	"testing"
)

// retractedVersions are the published versions withdrawn from version selection.
// Adding one here without adding it to go.mod fails this test, and vice versa.
// Every release that was DELETED for cause belongs here: deleting a tag removes
// discoverability, it does not withdraw the module. See go.mod for the
// measurement.
var retractedVersions = []string{"v0.34.0", "v0.36.0", "v0.36.1"}

// TestGoModRetainsRetractions: `retract` directives are only honored from the
// HIGHEST released version's go.mod. Drop them in a later cut and both
// retractions silently stop reaching anyone — no error, no warning, just a
// withdrawn version quietly back in selection.
//
// That is precisely the per-cut manual obligation this module has been removing
// everywhere else, so it gets the same treatment: an executable check rather
// than a line in a checklist someone has to remember mid-release.
//
// Why the versions were withdrawn is in go.mod beside the directives. What
// matters here is only that they are still THERE. Deleting a tag does not
// withdraw a Go module — the proxy is an immutable cache and keeps serving it —
// so these directives are the only thing that reaches a consumer.
func TestGoModRetainsRetractions(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, v := range retractedVersions {
		// Match a retract line for this version in either form: `retract vX`, or
		// a bare `vX` line INSIDE the retract block. The block is required
		// separately below, and no other go.mod block lists bare versions on
		// their own line, so this cannot be satisfied by an unrelated stanza.
		re := regexp.MustCompile(`(?m)^(\s*retract\s+` + regexp.QuoteMeta(v) + `|\t` + regexp.QuoteMeta(v) + `)\s*(//.*)?$`)
		if !re.Match(b) {
			t.Errorf("gateway/go.mod no longer retracts %s — a cut from this tree would put a withdrawn version back into version selection, and `go get` would stop warning. Retractions are honored only from the highest released version's go.mod, so they must be carried forward at EVERY cut.", v)
		}
	}
	if !regexp.MustCompile(`(?m)^retract\s*\(`).Match(b) {
		t.Error("gateway/go.mod has no retract block at all")
	}
}
