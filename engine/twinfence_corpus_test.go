package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The twin-fence corpus (testdata/twinfence/) is the shared conformance
// vector set for the two request fences this engine carries as deliberate
// twins of the published SDK Responder:
//
//   - the FR-16/FR-27 attestation conformance fence (fenceAttestedItems,
//     attestfence.go), and
//   - the FR-32 supplemental-data / subject-bind fence on the conformant
//     pas-claim-update leg (conformantPASUpdateBind, pas_native.go).
//
// The vectors are minted by the substrate's vector generator and committed
// byte-identically here and in the SDK module (sdk/testdata/twinfence); each
// side's own test drives ITS fence over the SAME inputs and asserts the SAME
// accept/reject verdict, so a tolerance added to one side fails the other
// until the twin learns it too. The root-module lockstep test pins the two
// committed copies fresh and identical.
//
// The corpus covers the COMMON decision surface. The engine's registry arm
// (Claim.patient resolved to a PCI and compared to the token subject) is
// engine-only — the standalone SDK Responder has no patient registry — and
// stays pinned by this package's own conformantPASUpdateBind rejection set.

// twinFenceVector mirrors the committed vector schema (a data contract, kept
// local so the published module's copy of this test stays standalone).
type twinFenceVector struct {
	Name           string          `json:"name"`
	Family         string          `json:"family"`
	Description    string          `json:"description"`
	Expect         string          `json:"expect"`
	RejectContains string          `json:"rejectContains"`
	RejectStatus   int             `json:"rejectStatus"`
	TokenMember    string          `json:"tokenMember"`
	OriginalCorr   string          `json:"originalCorr"`
	Bundle         json.RawMessage `json:"bundle"`
}

func readTwinFenceCorpus(t *testing.T) []twinFenceVector {
	t.Helper()
	dir := filepath.Join("testdata", "twinfence")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("twin-fence corpus dir %s not found (the committed vectors ship with the module): %v", dir, err)
	}
	var vecs []twinFenceVector
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read vector %s: %v", e.Name(), err)
		}
		var v twinFenceVector
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("parse vector %s: %v", e.Name(), err)
		}
		if v.Name+".json" != e.Name() {
			t.Fatalf("vector file %s carries mismatched name %q", e.Name(), v.Name)
		}
		vecs = append(vecs, v)
	}
	if len(vecs) == 0 {
		t.Fatal("twin-fence corpus is empty")
	}
	return vecs
}

// TestTwinFenceCorpus drives every corpus vector into this engine's own copy
// of each fence and asserts the authored verdict. An unknown family fails
// loud: a vector added for a new fence family must not silently no-op here
// while the SDK enforces it.
func TestTwinFenceCorpus(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range readTwinFenceCorpus(t) {
		v := v
		seen[v.Family+"/"+v.Expect] = true
		t.Run(v.Name, func(t *testing.T) {
			switch v.Family {
			case "attestation":
				reason, ok := fenceAttestedItems(v.Bundle)
				switch v.Expect {
				case "accept":
					if !ok {
						t.Fatalf("attestation fence rejected an accept vector: %s", reason)
					}
				case "reject":
					if ok {
						t.Fatal("attestation fence accepted a reject vector")
					}
					if v.RejectContains != "" && !strings.Contains(reason, v.RejectContains) {
						t.Fatalf("rejection reason %q does not contain %q", reason, v.RejectContains)
					}
				default:
					t.Fatalf("unknown verdict %q", v.Expect)
				}
			case "update-bind":
				if v.TokenMember == "" {
					t.Fatal("update-bind vector carries no tokenMember")
				}
				g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
				pci, _, found := g.cfg.SoR.ResolvePatient(v.TokenMember)
				if !found {
					t.Fatalf("tokenMember %q not resolvable in the census fixture", v.TokenMember)
				}
				_, status, msg := g.conformantPASUpdateBind(v.Bundle, pci)
				switch v.Expect {
				case "accept":
					if status != 0 {
						t.Fatalf("update fence rejected an accept vector: %d %s", status, msg)
					}
				case "reject":
					if status == 0 {
						t.Fatal("update fence accepted a reject vector")
					}
					if v.RejectStatus != 0 && status != v.RejectStatus {
						t.Fatalf("rejection status = %d, want %d (%s)", status, v.RejectStatus, msg)
					}
					if v.RejectContains != "" && !strings.Contains(msg, v.RejectContains) {
						t.Fatalf("rejection message %q does not contain %q", msg, v.RejectContains)
					}
				default:
					t.Fatalf("unknown verdict %q", v.Expect)
				}
			default:
				t.Fatalf("unknown vector family %q — teach this driver the new family before committing its vectors", v.Family)
			}
		})
	}
	// Non-vacuity: both fences must have been driven in both directions.
	for _, want := range []string{"attestation/accept", "attestation/reject", "update-bind/accept", "update-bind/reject"} {
		if !seen[want] {
			t.Fatalf("corpus carries no %s vector — the fence pair is no longer exercised in both directions", want)
		}
	}
}
