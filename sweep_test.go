package shngateway_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
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
// The last arm is the internal-design-document class: pointers into private
// design notes, either by section (`spec §4`) or by the note's date
// (`spec 2026-08-10 §4`, or a bare `spec 2026-08-10`). It was added AFTER
// v0.36.0 shipped 16 of these across 7 files and had to be DELETED, tag and
// Release both. That cut's scrub was a hand-run grep spelled `spec §N` and
// `spec <date> F7`, which does not match `spec <date> §N` or a bare
// `spec <date>` — so it returned empty while the references went out.
// The lesson is not "widen the grep": A PATTERN CANNOT FIND WHAT ITS OWN
// PATTERN DOES NOT DESCRIBE. This arm is a backstop. The net is diffing a new
// snapshot against the PREVIOUS PUBLISHED TAG and accounting for every
// differing file, which is what actually caught this class and stays in the
// publish runbook. The fix for a match is the same as every class above:
// state what the design note DECIDED, since a partner cannot read it.
//
// The final arm is the PARTNER NAME. It is here so the publish runbook can stop
// carrying `grep -riI cambia` as a per-cut manual step — the last one it had.
// A hand-run name grep is what let this module's seed fixture ship that payer's
// NAIC-registry id and UAT subscriber id while returning zero hits: it matched
// the NAME, and the fixture disclosed the IDENTITY. The identity half is closed
// in the generator (internal/fhirseed's publication guard, which fails the bake
// rather than the review); this arm closes the name half, over test files
// included — they ship in the snapshot, and all three occurrences that used to
// live in them were reworded in the same change that added this.
//
// ⚠️ KNOWN TRADE-OFF, stated rather than glossed: this file ships too, and it
// must state its own pattern literally (that is why it is in sweepSkipFiles), so
// the partner name appears HERE — in a list of tokens the project scrubs. The
// same is already true of `\bBo\b`, a maintainer's first name. So
// `grep -riI cambia gateway/` returns this file, and nothing else. Splitting the
// literal to dodge the grep would be the exact evasion this whole sweep exists
// to catch, so it is not done. If the judgement is that ZERO partner-name bytes
// may ship, the answer is to drop this arm and rely on the identity guard in
// internal/fhirseed plus the previous-published-tag diff — NOT to obfuscate the
// token here. That is an owner call about partner naming, not a code decision.
//
// Published spec ids are DELIBERATELY not in this pattern: FR-G*, AI-G*,
// OWD-G* and UC-0X are partner-facing vocabulary (they appear in the published
// participant protocol and conformance docs) and must keep appearing here.
const internalTokenPattern = `S5b|Task[ -][0-9]|(?i:\btask-[0-9])|per the plan|Material-|infra/|goldengen|shn-platform|\bE[0-9][a-z][0-9]?\b|\bD[0-9]\b` +
	`|\bK1\b|PR #[0-9]+|#[0-9]{2,}\b|docs/superpowers|(?i:\bslice[ -][0-9]\b)|\bBo\b|review-fixes|\bround-[0-9]\b` +
	`|ledger[ -][0-9]|(?i:ledger[ -]item[ -][0-9])|option[ -][A-Z] ruling|A′|\bA'[ .,)]|\bT-[0-9]\b|\b[SM]F[0-9]+\b` +
	`|(?i:spec §|spec[ (]*[0-9]{4}-[0-9]{2}-[0-9]{2})` +
	`|(?i:cambia)`

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

// TestInternalTokenPattern_DesignDocRefForms is the rejection test for the
// design-document-reference arm, and it exists because TestNoInternalTokens
// ALONE CAN PASS FOR THE WRONG REASON: it asserts only that the tree holds no
// match, so a pattern that describes nothing is indistinguishable from a tree
// that is clean. That is not hypothetical — it is exactly how v0.36.0 shipped
// 16 references under a green sweep and had to be deleted.
//
// So the pattern itself is the thing under test here. Every "must match" row is
// a form that actually leaked (or the near-twin the deleted cut's narrower grep
// missed); every "must not match" row pins the boundary so a later widening
// cannot quietly swallow ordinary prose about a published specification, which
// is legitimate partner-facing vocabulary.
func TestInternalTokenPattern_DesignDocRefForms(t *testing.T) {
	re := regexp.MustCompile(internalTokenPattern)

	mustMatch := []string{
		// Section-only — the one form the deleted cut's grep did describe.
		`// (spec §1 invariant: ok:false ⇒ failure present)`,
		// Date + section: the form that slipped past it and forced the delete.
		`// machine classification (spec 2026-08-09 §1/§2): present, exact code`,
		`// derived from them (spec 2026-08-10 §3 path 2 "probe retention")`,
		`// routing (spec 2026-08-10 §4 "foreign endpoints route by the filter")`,
		// Bare date, no section — the other form it could not see.
		`// the drift rule this encodes was settled in spec 2026-08-10`,
		// Sentence-initial capital. This one SURVIVED the #448 back-port in
		// both the branch and published v0.36.1: the arm was case-sensitive,
		// and `Spec <date>, F7` is the near-twin of `spec <date> F7` — the
		// exact spelling v0.36.0's hand-grep was written for. It got past on
		// capitalization alone.
		`// Spec 2026-08-11, F7 — a HAPI instance hosts exactly ONE version`,
		// The SAME case blind spot on a pre-existing arm: `Task[ -][0-9]` was
		// case-sensitive, so three lowercase `task-N review` pointers sat in
		// the tree — and in published v0.36.1 — while the sweep read green.
		// Found by the de-wrapped, case-insensitive scan that also exposed the
		// rows above, which is why that scan, not this pattern, is the net.
		`// fix-round finding 1 (task-3 review) — routeInfoFor must render`,
		`// pins the fix for IMPORTANT-1 (task-18 review): the credential check`,
		// Punctuation between the word and the date defeats a `spec ` + digits
		// spelling. This one also sat in v0.36.1. Same lesson a third time:
		// each near-miss looked like the whole class until the next one.
		`// Adversarial row for the opaque-payload message-frame spec (2026-07-17):`,
		// Added while the tree held ZERO instances of either — the cheapest
		// moment an arm ever gets, since there is nothing to reword and no
		// false-positive triage against live hits. The alternative is adding
		// the pattern AND fixing hits during a release, which is backwards.
		// Both forms shipped in v0.36.1 and were removed by the back-port's
		// copy, so without an arm the next one would leak again unseen.
		`// the per-service #16 alarm watches a single stream`,
		`// resolved per ledger item 5 — the member fence stays`,
		// The partner name, in the casings it actually appeared in before this
		// change reworded all three test-file sites.
		`// the Cambia lane, whose order-sign prefetch is a SEARCH template`,
		`t.Fatalf("ingress status = %d, want 502 (cambia's real status)", rec.Code)`,
	}
	for _, line := range mustMatch {
		if m := re.FindString(line); m == "" {
			t.Errorf("pattern does not describe a form that has ALREADY leaked — a green sweep would be meaningless against it:\n\t%s", line)
		}
	}

	// A citation that wraps across a comment break holds no single line with
	// the token, so it is unreachable by the pattern no matter how the pattern
	// is spelled. sweepUnits is what closes that, and these rows are its
	// rejection test — without them the suite still cannot fail for the two
	// wrapped references that survived into the published tag.
	mustMatchWrapped := []string{
		"\t// frame carrying the app status and relays it 200-to-Hub (verbatim; spec\n\t// 2026-07-17) — the error-branch sibling of respondLeg.",
		"// Also writes a *RouteRefusalError (the version-routing legible refusal, spec\n// 2026-08-10 §4) as its 422 — one chokepoint covers every origination site.",
	}
	for _, block := range mustMatchWrapped {
		var joined string
		for _, u := range sweepUnits(block) {
			if u.joined {
				joined = u.text
			}
		}
		if joined == "" {
			t.Errorf("sweepUnits produced no joined unit for a wrapped comment — wrapped citations would stay unreachable:\n\t%q", block)
			continue
		}
		if m := re.FindString(joined); m == "" {
			t.Errorf("pattern does not describe a WRAPPED form that has already leaked (joined: %q):\n\t%s", joined, block)
		}
	}

	// Each widening below shipped with a must-match row above; these are the
	// matching boundary rows, one per widening. Without them a widening is a
	// one-way ratchet — nothing states where it is supposed to STOP, and the
	// next person to broaden the pattern cannot tell prose from jargon.
	mustNotMatch := []string{
		// Prose about a PUBLISHED spec is partner-facing and must survive.
		`// the FHIR specification requires a searchset Bundle here`,
		`// see the spec for the full profile list`,
		`// US Core §3.1.1 pins the identifier slice`,
		// A bare date is not a reference to a private design note.
		`// last reviewed 2026-08-10 against the published IG`,
		// Boundary for `spec[ (]*<date>`: a comma is ordinary prose about a
		// dated edition of a PUBLISHED spec, not a pointer into a design note.
		// This is why the arm allows only space/paren between word and date.
		`// built against the FHIR spec, 2026-08-10 edition, per the IG`,
		// Boundary for the case-insensitive task arm: lowercase `task <n>` with
		// a SPACE is ordinary English ("task 1 before task 2"). Only the
		// hyphenated `task-<n>` id form is jargon, so the arm is
		// `Task[ -][0-9]` (as before) plus `(?i:\btask-[0-9])` — NOT a blanket
		// case-insensitive widening, which would have swallowed this row.
		`// the operator runs task 1 before task 2 during a cut`,
	}
	for _, line := range mustNotMatch {
		if m := re.FindString(line); m != "" {
			t.Errorf("pattern is too broad — it flags ordinary prose about published vocabulary as internal (matched %q):\n\t%s", m, line)
		}
	}

	// Boundary rows for the JOINING widening. Joining makes wrapped citations
	// reachable, but it can also MANUFACTURE a reference that exists in neither
	// line — so these rows pin that it does not. The markdown case is the one
	// that actually bit: `*` was briefly treated as a comment marker, and two
	// adjacent bullets joined into a `spec <date>` that nobody wrote.
	mustNotMatchWrapped := []string{
		"* Validated against the published FHIR spec\n* 2026-08-10 was the cut date for this line",
		"// The FHIR specification lists the profile set; the cut\n// date 2026-08-10 is recorded in the published release notes.",
	}
	for _, block := range mustNotMatchWrapped {
		for _, u := range sweepUnits(block) {
			if !u.joined {
				continue
			}
			if m := re.FindString(u.text); m != "" {
				t.Errorf("joining MANUFACTURED an internal reference that is in neither source line (matched %q, joined: %q):\n\t%s", m, u.text, block)
			}
		}
	}
}

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
		// Keyed by line AND token: an operator scrubbing a cut has to see
		// EVERY offense, so one token must never suppress the report of a
		// different token that happens to share its comment run.
		reported := map[reportKey]bool{}
		for _, u := range sweepUnits(string(b)) {
			if sweepLineAllowed(u.text, allowed) {
				continue
			}
			for _, m := range dedupe(re.FindAllString(u.text, -1)) {
				// A joined run re-reports what its own lines already named.
				// Suppress only THAT token, not the whole unit.
				if u.joined && anyReported(reported, u.start, u.end, m) {
					continue
				}
				reported[reportKey{u.start, m}] = true
				t.Errorf("%s:%d: internal-vocabulary token %q leaks into the published module — reword to public vocabulary (published spec ids FR-G*/AI-G*/OWD-G*/UC-0X are fine), or add the line to sweepAllowlist with a WHY comment.\n\t%s",
					rel, u.start, m, sweepExcerpt(u.text, m))
			}
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

// sweepCommentPrefix matches the comment marker that opens a continuation line
// in the file types this sweep walks: Go/JS `//` and shell/Dockerfile/YAML `#`.
//
// `*` (a block-comment body) is DELIBERATELY absent. This module has no `/* */`
// comments at all, so including it bought nothing — while markdown uses `*` for
// bullets, and joining two adjacent bullets manufactures a reference that is in
// neither one:
//
//   - Validated against the published FHIR spec
//   - 2026-08-10 was the cut date for this line
//
// `gateway/` ships real markdown, so that false positive would have landed on
// whoever next wrote release notes with adjacent bullets.
var sweepCommentPrefix = regexp.MustCompile(`^\s*(?://+|#+)[ \t]?`)

// sweepUnit is one chunk of a file the sweep matches against: either a raw
// line, or a run of consecutive comment lines joined into one logical line.
type sweepUnit struct {
	start, end int // 1-indexed, inclusive; start == end for a raw line
	text       string
	joined     bool
}

// sweepUnits returns every raw line, PLUS each run of consecutive comment lines
// joined into a single logical line reported at the run's first line.
//
// The joined form exists because a citation that WRAPS across a comment break is
// otherwise structurally unreachable by EVERY arm of the pattern: the sweep
// splits on "\n", so `(verbatim; spec` / `// 2026-07-17)` contains no single
// line holding `spec <date>`. That is not hypothetical — two of the three
// references that survived the #448 back-port were exactly this shape, and they
// survived in the PUBLISHED v0.36.1 tag too, which is why that tag read as a
// clean reference when it was not. A parenthetical citation at the end of a
// sentence is precisely what wraps, so this class is the one most exposed to it.
func sweepUnits(content string) []sweepUnit {
	lines := strings.Split(content, "\n")
	units := make([]sweepUnit, 0, len(lines)+8)
	for i, line := range lines {
		units = append(units, sweepUnit{start: i + 1, end: i + 1, text: line})
	}
	for i := 0; i < len(lines); {
		prefix := sweepCommentPrefix.FindString(lines[i])
		if prefix == "" {
			i++
			continue
		}
		j, parts := i, []string(nil)
		for j < len(lines) {
			p := sweepCommentPrefix.FindString(lines[j])
			if p == "" {
				break
			}
			parts = append(parts, strings.TrimSpace(lines[j][len(p):]))
			j++
		}
		if len(parts) > 1 {
			units = append(units, sweepUnit{
				start: i + 1, end: j, text: strings.Join(parts, " "), joined: true,
			})
		}
		i = j
	}
	return units
}

// sweepExcerpt trims the reported text to a window around the match. A joined
// comment run can be an entire doc comment, and dumping all of it buries the
// token the operator has to go and reword.
func sweepExcerpt(text, match string) string {
	text = strings.TrimSpace(text)
	i := strings.Index(text, match)
	if i < 0 {
		return text
	}
	start, end := i-60, i+len(match)+60
	prefix, suffix := "…", "…"
	if start < 0 {
		start, prefix = 0, ""
	}
	if end > len(text) {
		end, suffix = len(text), ""
	}
	// These comments carry §, — and ⇒, so a byte offset can land mid-rune.
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	return prefix + strings.TrimSpace(text[start:end]) + suffix
}

// reportKey identifies one reported offense: the line it was named at, and the
// token that was named. Both, because a comment run can hold several distinct
// tokens and each has to be reported on its own.
type reportKey struct {
	line  int
	token string
}

// anyReported reports whether THIS token was already named on any line in
// [start,end].
func anyReported(reported map[reportKey]bool, start, end int, token string) bool {
	for i := start; i <= end; i++ {
		if reported[reportKey{i, token}] {
			return true
		}
	}
	return false
}

// dedupe removes repeated matches, preserving order: one line naming the same
// token twice is one offense to reword.
func dedupe(matches []string) []string {
	seen := make(map[string]bool, len(matches))
	out := matches[:0:0]
	for _, m := range matches {
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

func sweepLineAllowed(line string, allowed []string) bool {
	for _, sub := range allowed {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}
