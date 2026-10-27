// compat.go — the compatibility manifest: a Go table
// consulted at runtime inside a deployed binary, NEVER JSON. This is
// deliberately NOT tools/contracts/manifest.json — that file is IG package-pin
// text consumed by contractsgen's KV/curl-site machinery (a repo-file read
// cannot serve a routing filter inside a running process, and the parser must
// never see unrelated content — the "hapi.fhir.implementationguides.<key>.
// <field>=" block-indexing gotcha, hit twice in practice).
//
// compatManifest carries one row per ADJACENT line pair per contract (the
// Stripe model: N-1 modules bridge N lines). chainFor is the pure path-walk
// over that table; ranking chains for selection (full beats carry beats
// gated, fewer steps beat more, ties broken by highest source line) lives in
// selectChain — chainFor only answers "is there a path, and what is
// it", never "which path is best".
package engine

import (
	"sort"
	"strings"
)

// compatManifest is the compatibility table: every compiled-in workstream's
// rows (workstream.lineBridge), in registry order, set at package init
// (workstreams.go). The rows and their derivation are the workstream's (the PA
// workstream's: paCompatSteps); the table's structure and its walk (chainFor)
// are the core's.
var compatManifest []CompatStep

// nativeContracts returns the distinct contract names (the part before "@")
// across NativeContractVersions(), sorted.
func nativeContracts() []string {
	seen := map[string]bool{}
	var out []string
	for _, tok := range NativeContractVersions() {
		contract, _, ok := strings.Cut(tok, "@")
		if !ok || contract == "" || seen[contract] {
			continue
		}
		seen[contract] = true
		out = append(out, contract)
	}
	sort.Strings(out)
	return out
}

// nativeLinesForContract returns one contract's native lines, in NUMERIC
// ascending order (compareLines, not sort.Strings — an explicit ordered-lines
// helper rather than relying on the "2.0" < "2.1" < "2.2" lexical accident).
func nativeLinesForContract(contract string) []string {
	var lines []string
	for _, tok := range NativeContractVersions() {
		c, line, ok := strings.Cut(tok, "@")
		if !ok || c != contract || line == "" {
			continue
		}
		lines = append(lines, line)
	}
	sort.Slice(lines, func(i, j int) bool { return compareLines(lines[i], lines[j]) < 0 })
	return lines
}

// manifestLinesForContract returns the distinct lines the manifest table
// actually has rows for, one contract, NUMERIC ascending order. This is the
// universe chainFor walks — deliberately independent of nativeLinesForContract
// so an unknown line (never appearing in any manifest row) is a clean nil,
// not an accidental match against the native set.
func manifestLinesForContract(contract string) []string {
	seen := map[string]bool{}
	for _, s := range compatManifest {
		if s.Contract != contract {
			continue
		}
		seen[s.From] = true
		seen[s.To] = true
	}
	lines := make([]string, 0, len(seen))
	for l := range seen {
		lines = append(lines, l)
	}
	sort.Slice(lines, func(i, j int) bool { return compareLines(lines[i], lines[j]) < 0 })
	return lines
}

// manifestStep looks up the single row bridging the adjacent pair (from, to)
// — from and to must be given low, high (From, To order); it does not search
// direction-reversed.
func manifestStep(contract, from, to string) (CompatStep, bool) {
	for _, s := range compatManifest {
		if s.Contract == contract && s.From == from && s.To == to {
			return s, true
		}
	}
	return CompatStep{}, false
}

// indexOfLine returns lines' index of line, or (-1, false) if absent.
func indexOfLine(lines []string, line string) (int, bool) {
	for i, l := range lines {
		if l == line {
			return i, true
		}
	}
	return -1, false
}

// chainFor returns the ordered steps bridging from→to for contract (either
// direction), nil if the manifest has a hole. Chain ranking lives in
// selectChain; chainFor is the pure path-walk.
func chainFor(contract, from, to string) []CompatStep {
	lines := manifestLinesForContract(contract)
	fi, fok := indexOfLine(lines, from)
	ti, tok := indexOfLine(lines, to)
	if !fok || !tok {
		return nil
	}
	if fi == ti {
		return []CompatStep{}
	}

	steps := make([]CompatStep, 0, abs(ti-fi))
	if fi < ti {
		for i := fi; i < ti; i++ {
			step, ok := manifestStep(contract, lines[i], lines[i+1])
			if !ok {
				return nil
			}
			steps = append(steps, step)
		}
	} else {
		for i := fi; i > ti; i-- {
			step, ok := manifestStep(contract, lines[i-1], lines[i])
			if !ok {
				return nil
			}
			steps = append(steps, step)
		}
	}
	return steps
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
