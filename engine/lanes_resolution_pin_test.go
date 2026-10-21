package engine

import (
	"crypto/ed25519"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

var updateLaneResolution = flag.Bool("update-lane-resolution", false, "rewrite testdata/lane_resolution.golden")

// laneGateway is a gateway with cfg's lanes, as New would resolve them: the
// fixtures set the deprecated per-line lanes, so the grid also proves that New
// derives today's resolution from them (deriveContractLanes).
func laneGateway(cfg Config) *Gateway {
	deriveContractLanes(&cfg)
	return &Gateway{cfg: cfg}
}

// laneResolutionFixtures are the lane configurations a gateway runs with, each
// with named validators so the grid can say which one a resolution reached.
func laneResolutionFixtures() (configs []struct {
	name string
	cfg  func() Config
}, names map[shnsdk.Validator]string) {
	canonical, x21, x22 := shnsdk.NewFakeValidator(), shnsdk.NewFakeValidator(), shnsdk.NewFakeValidator()
	names = map[shnsdk.Validator]string{canonical: "canonical", x21: "explicit-2.1", x22: "explicit-2.2"}
	lane := func(line string, ready bool) *DiscoveredLane {
		d := NewDiscoveredLane(line, "http://validator-"+line+".invalid/fhir", shnsdk.NewFakeValidator())
		d.ready.Store(ready)
		state := "warming"
		if ready {
			state = "ready"
		}
		names[d] = "default-" + line + "-" + state
		return d
	}
	byLine := func(m map[string]shnsdk.Validator) map[string]shnsdk.Validator { return m }
	configs = []struct {
		name string
		cfg  func() Config
	}{
		{"unlaned", func() Config { return Config{Validator: canonical} }},
		{"explicit lanes", func() Config {
			return Config{Validator: canonical, ValidatorsByLine: byLine(map[string]shnsdk.Validator{"2.0": canonical, "2.1": x21, "2.2": x22})}
		}},
		{"explicit 2.1 only", func() Config {
			return Config{Validator: canonical, ValidatorsByLine: byLine(map[string]shnsdk.Validator{"2.0": canonical, "2.1": x21})}
		}},
		{"default lanes ready", func() Config {
			return Config{Validator: canonical, ValidatorsByLine: byLine(map[string]shnsdk.Validator{"2.0": canonical}),
				DefaultValidatorsByLine: map[string]*DiscoveredLane{"2.1": lane("2.1", true), "2.2": lane("2.2", true)}}
		}},
		{"default lanes warming", func() Config {
			return Config{Validator: canonical, ValidatorsByLine: byLine(map[string]shnsdk.Validator{"2.0": canonical}),
				DefaultValidatorsByLine: map[string]*DiscoveredLane{"2.1": lane("2.1", false), "2.2": lane("2.2", false)}}
		}},
		{"single-line alias, default ready", func() Config {
			return Config{Validator: canonical, ValidatorsByLine: byLine(map[string]shnsdk.Validator{"2.0": canonical, "2.1": canonical}),
				CanonicalFallbackLines: map[string]bool{"2.1": true}, DefaultValidatorsByLine: map[string]*DiscoveredLane{"2.1": lane("2.1", true)}}
		}},
		{"single-line alias, no default", func() Config {
			return Config{Validator: canonical, ValidatorsByLine: byLine(map[string]shnsdk.Validator{"2.0": canonical, "2.1": canonical}),
				CanonicalFallbackLines: map[string]bool{"2.1": true}}
		}},
		{"canonical only, at none", func() Config {
			return Config{Validator: canonical, ValidatorsByLine: byLine(map[string]shnsdk.Validator{"2.0": canonical}),
				ConformanceEnforcement: EnforcementNone}
		}},
		{"no validator, at none", func() Config {
			return Config{ConformanceEnforcement: EnforcementNone}
		}},
	}
	return configs, names
}

// laneResolutionGrid is, for every lane configuration, every contract (each
// native one, pa.pdex, which no gateway builds, and an unknown one) and every
// line (none, each native line and an unknown one), the validator
// validatorForContractLine resolves, whether servesLine serves the line, and
// the validator validatorForLine resolves: one line each, sorted.
func laneResolutionGrid() string {
	configs, names := laneResolutionFixtures()
	label := func(v shnsdk.Validator) string {
		if v == nil {
			return "-"
		}
		if n, ok := names[v]; ok {
			return n
		}
		if o, ok := v.(observingValidator); ok {
			if n, ok := names[o.inner]; ok {
				return "observed:" + n
			}
		}
		return "unnamed"
	}
	contracts := map[string]bool{"pa.pdex": true, "x.unknown": true}
	for _, tok := range paNativeContractVersions() {
		c, _, _ := strings.Cut(tok, "@")
		contracts[c] = true
	}
	var rows []string
	for _, c := range configs {
		for _, contract := range slices.Sorted(func(yield func(string) bool) {
			for k := range contracts {
				if !yield(k) {
					return
				}
			}
		}) {
			for _, line := range []string{"", "2.0", "2.1", "2.2", "9.9"} {
				g := laneGateway(c.cfg())
				rows = append(rows, fmt.Sprintf("%s | %s@%s | contract-line %s | serves %v | line %s",
					c.name, contract, line, label(g.validatorForContractLine(contract, line)), g.servesLine(contract, line), label(g.validatorForLine(line))))
			}
		}
	}
	return strings.Join(rows, "\n") + "\n"
}

// TestLaneResolution_Pinned pins lane resolution over every lane configuration,
// contract and line (testdata/lane_resolution.golden). Re-keying the lanes
// changes none of it.
func TestLaneResolution_Pinned(t *testing.T) {
	got := laneResolutionGrid()
	const golden = "testdata/lane_resolution.golden"
	if *updateLaneResolution {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (record it with -update-lane-resolution)", err)
	}
	if got != string(want) {
		t.Fatalf("lane resolution changed:\n%s", diffLines(string(want), got))
	}
	// The multi-line refusal: a multi-line contract never borrows the
	// single-line alias, and resolves no validator at the aliased line while its
	// own default lane is absent or warming.
	for _, row := range []string{
		"single-line alias, no default | pa.dtr@2.1 | contract-line - |",
		"single-line alias, no default | pa.pas@2.1 | contract-line - |",
		"default lanes warming | pa.dtr@2.1 | contract-line - |",
		// pa.pdex is no contract this gateway builds: it resolves no lane where
		// lanes are configured, whatever the linked SDK lists.
		"explicit lanes | pa.pdex@2.1 | contract-line - | serves false |",
		"single-line alias, default ready | pa.pdex@2.1 | contract-line - | serves false |",
	} {
		if !strings.Contains(got, row) {
			t.Errorf("the grid lost the row %q", row)
		}
	}
	// Every pa.pdex row resolves exactly as the unknown contract's does.
	for _, row := range strings.Split(got, "\n") {
		if strings.Contains(row, "| pa.pdex@") && !strings.Contains(got, strings.Replace(row, "| pa.pdex@", "| x.unknown@", 1)) {
			t.Errorf("%q resolves unlike the unknown contract", row)
		}
	}
}

// diffLines lists the lines that differ between want and got.
func diffLines(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var out []string
	for i := 0; i < max(len(w), len(g)); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			out = append(out, "- "+a, "+ "+b)
		}
	}
	return strings.Join(out, "\n")
}

// TestLaneResolution_UnknownContractHasNoLane: a contract no workstream
// declares resolves no lane under every laned configuration, and no gateway
// serves it, at every level, none included: re-keying the lanes by contract
// makes it fail closed, where per-line lanes lent it any contract's lane at a
// laned line. An unlaned gateway (no lanes at all) validates every contract
// line against its one Validator, as it always has.
func TestLaneResolution_UnknownContractHasNoLane(t *testing.T) {
	configs, _ := laneResolutionFixtures()
	for _, c := range configs {
		for _, level := range allLevels {
			cfg := c.cfg()
			cfg.ConformanceEnforcement = level
			g := laneGateway(cfg)
			for _, line := range []string{"2.0", "2.1", "2.2", "9.9"} {
				v := g.validatorForContractLine("x.unknown", line)
				laned := len(g.cfg.ValidatorsByContractLine) > 0
				if laned && v != nil {
					t.Errorf("%s at %v: x.unknown@%s resolved a lane", c.name, level, line)
				}
				if !laned && v != g.cfg.Validator {
					t.Errorf("%s at %v: an unlaned gateway did not use its one Validator for x.unknown@%s", c.name, level, line)
				}
				if laned && g.servesLine("x.unknown", line) {
					t.Errorf("%s at %v: x.unknown@%s is served", c.name, level, line)
				}
			}
		}
	}
}

// TestCanonicalContractVersion_ComesFromTheWorkstream: the canonical
// validator's contract version is the one a compiled-in workstream names (the
// PA adapter's, pa.pas@2.0); a build that compiles in no workstream naming one
// has none, and asking does not panic.
func TestCanonicalContractVersion_ComesFromTheWorkstream(t *testing.T) {
	if got := CanonicalContractVersion(); got != shnsdk.ContractPAPAS20 {
		t.Fatalf("CanonicalContractVersion() = %q, want the PA adapter's %q", got, shnsdk.ContractPAPAS20)
	}
	saved := workstreams
	defer func() { workstreams = saved }()
	for name, ws := range map[string][]workstream{
		"no workstream":                nil,
		"a workstream that names none": {toyWorkstream{tag: "toy", legs: map[string]legSpec{}}},
	} {
		workstreams = ws
		if got := CanonicalContractVersion(); got != "" {
			t.Errorf("%s: CanonicalContractVersion() = %q, want none", name, got)
		}
	}
}

// TestNew_DerivesContractLanesFromDeprecatedFields: a published consumer that
// still sets the deprecated per-line lanes, and not ValidatorsByContractLine,
// gets the contract-line lanes they name from New; one that sets
// ValidatorsByContractLine keeps it, the deprecated fields ignored.
func TestNew_DerivesContractLanesFromDeprecatedFields(t *testing.T) {
	_, signPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	base := func() Config {
		stub := newCensusSoR()
		return Config{Role: "provider", HolderID: "provider", Identity: shnsdk.Identity{HolderID: "provider", SignPriv: signPriv}, SoR: stub, Store: stub}
	}
	canonical, x21, other := shnsdk.NewFakeValidator(), shnsdk.NewFakeValidator(), shnsdk.NewFakeValidator()
	legacy := base()
	legacy.Validator = canonical
	legacy.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": canonical, "2.1": x21}
	g := mustNew(t, legacy)
	if g.validatorForContractLine("pa.pas", "2.1") != x21 || g.validatorForContractLine("pa.dtr", "2.0") != canonical || g.validatorForContractLine("pa.pas", "2.2") != nil {
		t.Fatal("New did not derive the contract-line lanes the deprecated per-line lanes name")
	}
	// An alias with no lane behind it makes the gateway laned and serves
	// nothing there: it never lends the canonical validator.
	aliasOnly := base()
	aliasOnly.Validator = canonical
	aliasOnly.CanonicalFallbackLines = map[string]bool{"2.1": true}
	g = mustNew(t, aliasOnly)
	if v := g.validatorForContractLine("pa.pas", "2.1"); v != nil {
		t.Fatalf("an alias line with no lane resolved %T, want none", v)
	}
	current := base()
	current.Validator = canonical
	current.ValidatorsByContractLine = map[string]shnsdk.Validator{"pa.pas@2.1": x21}
	current.ValidatorsByLine = map[string]shnsdk.Validator{"2.1": other}
	g = mustNew(t, current)
	if g.validatorForContractLine("pa.pas", "2.1") != x21 || g.validatorForContractLine("pa.dtr", "2.1") != nil {
		t.Fatal("New let the deprecated per-line lanes override ValidatorsByContractLine")
	}
}
