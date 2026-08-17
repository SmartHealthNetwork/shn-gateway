package shngateway_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// internalTokenPattern is the publish-runbook's internal-vocabulary sweep. It
// catches the shorthand a private planning process leaves behind in comments
// and strings — plan/slice task numbers, internal design-decision ids, private
// repository paths, internal tooling names, review-round and PR references,
// and teammates' names — none of which mean anything to a partner reading this
// module's source after it is published as a standalone snapshot.
//
// The first eight alternatives are the runbook's original sweep, verbatim. The
// next group was added after the first scrub left 122 residual lines the
// original pattern could not see (plan-slice ids `K1 T<n>`, `slice <n>`,
// internal doc paths under docs/superpowers, `PR #<n>`/`#<nnn>` issue
// references, `review-fixes`/`round-<n>` review shorthand, and a maintainer's
// first name).
//
// The last group is the decision-shorthand class: an internal decision ledger's
// entry numbers (`ledger 2b`), the "option <X> ruling" form those entries are
// named by, the primed-letter ruling id (`A′`, and its ASCII-apostrophe twin
// `A'` before a space or closing punctuation), and internal task ids (`T-4`).
// A comment that cites one of these tells a partner nothing — the fix is always
// to state the decision's CONTENT instead ("the urn:shn:coverage identifier is
// a member number, not a reference"), which is what a reader actually needs.
// Two deliberate tightenings over the runbook's grep, both in the safe
// direction (this test fails where the runbook's grep would pass, never the
// reverse): `option[ -][A-Z] ruling` also catches the hyphenated "option-C
// ruling" spelling, and the ASCII arm requires a word boundary so an ordinary
// possessive ("HIPAA's") cannot trip it.
//
// `\b[SM]F[0-9]+\b` catches review-finding shorthand (`SF5`, `SM4`) that an
// internal review pass leaves in comments — like the decision-shorthand
// class above, the fix is always to say what the finding established, not to
// cite its id.
//
// Published spec ids are DELIBERATELY not in this pattern: FR-G*, AI-G*,
// OWD-G* and UC-0X are partner-facing vocabulary (they appear in the published
// participant protocol and conformance docs) and must keep appearing here.
const internalTokenPattern = `S5b|Task[ -][0-9]|per the plan|Material-|infra/|goldengen|shn-platform|\bE[0-9][a-z][0-9]?\b|\bD[0-9]\b` +
	`|\bK1\b|PR #[0-9]+|#[0-9]{3}\b|docs/superpowers|(?i:\bslice[ -][0-9]\b)|\bBo\b|review-fixes|\bround-[0-9]\b` +
	`|ledger[ -][0-9]|option[ -][A-Z] ruling|A′|\bA'[ .,)]|\bT-[0-9]\b|\b[SM]F[0-9]+\b`

// sweepSkipFiles are the two module-root test files excluded from the sweep.
//
//   - sweep_test.go: this file. It must state the pattern (and the allowlist
//     substrings) literally, so it necessarily matches itself. Excluding it is
//     safe precisely because it holds no prose about the module's behavior —
//     only the pattern and the allowlist table below, both of which are the
//     subject of review whenever they change.
//   - boundary_test.go: its assertion is *about* the substrate module path
//     ("this module must not import SmartHealthNetwork/shn-platform/internal/…"),
//     so the token is the load-bearing subject of the check, not leaked prose.
//     Rewording it would break the boundary test it exists to run.
var sweepSkipFiles = map[string]bool{
	"sweep_test.go":    true,
	"boundary_test.go": true,
}

// sweepAllowlist pins the individual lines that legitimately match the broad
// pattern above. Key = module-relative slash path; value = exact substrings of
// the offending line. An entry means "this match is a false positive or is
// genuinely partner-facing" — every entry carries a WHY comment. Adding one is
// a review decision, not a way to silence the sweep: the substring must be
// specific enough that unrelated prose on the same line cannot hide behind it.
//
// Empty is the goal state and is a stronger claim than any entry: it says the
// shipped module contains no line that even LOOKS like internal vocabulary. The
// table is kept (rather than deleted with the last entry) because the broad
// token classes — a bare `D<digit>` or `E<digit><letter>` — will eventually
// collide with real partner-facing vocabulary (X12/EDI qualifiers, for one),
// and the reviewable place to record that judgement is here.
var sweepAllowlist = map[string][]string{}

// TestNoInternalTokens keeps the published gateway snapshot free of internal
// planning vocabulary BY CONSTRUCTION, rather than by a manual grep at publish
// time. It walks the whole module tree — not just Go files, since the runbook
// sweep covers Dockerfiles, shell scripts and docs too — and fails naming
// file:line for every non-allowlisted match.
func TestNoInternalTokens(t *testing.T) {
	re := regexp.MustCompile(internalTokenPattern)

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata-cache":
				return fs.SkipDir
			}
			return nil
		}
		rel := filepath.ToSlash(path)
		if sweepSkipFiles[rel] {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if info.Size() > 4<<20 {
			return nil // outsized blob: not source prose
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil // binary
		}
		allowed := sweepAllowlist[rel]
		for i, line := range strings.Split(string(b), "\n") {
			m := re.FindString(line)
			if m == "" {
				continue
			}
			if sweepLineAllowed(line, allowed) {
				continue
			}
			t.Errorf("%s:%d: internal-vocabulary token %q leaks into the published module — reword to public vocabulary (published spec ids FR-G*/AI-G*/OWD-G*/UC-0X are fine), or add the line to sweepAllowlist with a WHY comment.\n\t%s",
				rel, i+1, m, strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module tree: %v", err)
	}
}

// repoRootReachPattern catches the ONE failure class that has now twice made
// the published module's own `go test ./...` red: a file inside this module
// reaching UP past the module root, into the monorepo that surrounds it in
// development but does not exist around the published snapshot.
//
// `../../testdata` from a first-level package (engine/, app/) and `../../..`
// from a second-level directory (deploy/validator/) both land on the monorepo
// root. In the monorepo they resolve to real files, so every in-repo gate —
// `make check`, `make e2e`, `make smoke` — passes while the published artifact
// is broken. Only a standalone clone sees it, which is exactly why this has to
// be a BY-CONSTRUCTION check rather than a publish-time grep.
//
// The fix is never to widen the reach: vendor the file into the module (see
// gateway/app/testdata/golden/README.md and gateway/engine/testdata/golden/
// README.md, both pinned against their repo-root originals by the root
// module's test/conformance/gateway_vendored_golden_drift_test.go), or drop
// the dependency.
//
// BOTH spellings are covered: the literal path form (`"../../testdata/..."`,
// `${DIR}/../../..`) and Go's split-argument form
// (`filepath.Join("..", "..", "testdata", ...)`). The split form is the one
// that actually shipped broken — a pattern that only saw literal paths read as
// clean while `gateway/engine`'s transform suite reached the monorepo root.
const repoRootReachPattern = `(?:\.\./)+(?:\.\.)?|(?:"\.\."\s*,\s*)*"\.\."`

// TestNoRepoRootReach walks the module tree and fails on any monorepo-root
// reach. There is no allowlist: a reach that resolves only in the monorepo is
// never correct in a module that ships on its own.
func TestNoRepoRootReach(t *testing.T) {
	re := regexp.MustCompile(repoRootReachPattern)

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata-cache", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		rel := filepath.ToSlash(path)
		if sweepSkipFiles[rel] {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if info.Size() > 4<<20 {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil // binary
		}
		// A directory nested N levels deep may legitimately use N `..`
		// segments to reach the module root (deploy/eval/payer/ builds from
		// `../../..`, the gateway module root). Only a reach that lands ABOVE
		// the module root is a bug, so the depth of this file's directory sets
		// the budget.
		depth := 0
		if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." {
			depth = strings.Count(dir, "/") + 1
		}
		for i, line := range strings.Split(string(b), "\n") {
			m := worstReachOnLine(re, line)
			if m == "" {
				continue
			}
			if escapeSegments(m) <= depth {
				continue
			}
			t.Errorf("%s:%d: %q reaches above the gateway module root — it resolves only inside the monorepo, so the PUBLISHED module (and the shipped deploy/ bundle) breaks on it. Vendor the file into the module with a root-side drift pin, or drop the dependency.\n\t%s",
				rel, i+1, m, strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module tree: %v", err)
	}
}

// worstReachOnLine returns the deepest `../…` run on a line (a line may hold
// more than one, and only the deepest decides).
func worstReachOnLine(re *regexp.Regexp, line string) string {
	worst := ""
	for _, m := range re.FindAllString(line, -1) {
		if escapeSegments(m) > escapeSegments(worst) {
			worst = m
		}
	}
	return worst
}

// escapeSegments counts the `..` path segments in a matched reach: `../../` is
// two levels up, `../../..` is three.
func escapeSegments(match string) int {
	return strings.Count(match, "..")
}

func sweepLineAllowed(line string, allowed []string) bool {
	for _, sub := range allowed {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}
