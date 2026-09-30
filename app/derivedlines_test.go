package app

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// nativePayerEnv is a payer gateway that forwards to its payer's own system.
func nativePayerEnv(extra map[string]string) map[string]string {
	m := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_BASE_URL": "https://payer.example/fhir",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

const system22 = "pa.crd@2.2, pa.dtr@2.2, pa.pas@2.2"

var derived22 = []string{shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22, shnsdk.ContractPAPDex21}

// TestDerivedDeclaration: with SHN_CONTRACT_VERSIONS unset, a payer
// forwarding to a system that declares its versions declares that system's
// lines, not the build default; an explicit SHN_CONTRACT_VERSIONS still wins;
// a system with no declared versions keeps the build default.
func TestDerivedDeclaration(t *testing.T) {
	load := func(t *testing.T, env map[string]string) config {
		t.Helper()
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	t.Run("unset derives the system's lines", func(t *testing.T) {
		cfg := load(t, nativePayerEnv(map[string]string{"PAYER_DAVINCI_CONTRACT_VERSIONS": system22}))
		if !slices.Equal(cfg.ContractVersions, derived22) || !cfg.ContractVersionsDerived {
			t.Fatalf("declared %q (derived %v), want %q derived", cfg.ContractVersions, cfg.ContractVersionsDerived, derived22)
		}
	})
	t.Run("blank is unset", func(t *testing.T) {
		cfg := load(t, nativePayerEnv(map[string]string{"PAYER_DAVINCI_CONTRACT_VERSIONS": system22, "SHN_CONTRACT_VERSIONS": " , "}))
		if !slices.Equal(cfg.ContractVersions, derived22) {
			t.Fatalf("declared %q, want %q", cfg.ContractVersions, derived22)
		}
	})
	t.Run("explicit wins", func(t *testing.T) {
		cfg := load(t, nativePayerEnv(map[string]string{"PAYER_DAVINCI_CONTRACT_VERSIONS": system22, "SHN_CONTRACT_VERSIONS": "pa.crd@2.0, pa.pas@2.0"}))
		if !slices.Equal(cfg.ContractVersions, []string{"pa.crd@2.0", "pa.pas@2.0"}) || cfg.ContractVersionsDerived {
			t.Fatalf("declared %q (derived %v), want the explicit set", cfg.ContractVersions, cfg.ContractVersionsDerived)
		}
	})
	t.Run("no system versions keeps the default", func(t *testing.T) {
		cfg := load(t, nativePayerEnv(nil))
		if !slices.Equal(cfg.ContractVersions, shnsdk.SupportedContractVersions()) || cfg.ContractVersionsDerived {
			t.Fatalf("declared %q (derived %v), want the build default", cfg.ContractVersions, cfg.ContractVersionsDerived)
		}
	})
}

// TestDerivedDeclaration_BootRefusals: a derivation that cannot be made, and
// a native-forward payer whose explicit declaration has no CRD, DTR or PAS
// line, refuse to boot.
func TestDerivedDeclaration_BootRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		must []string
	}{
		{"system line this build cannot exchange", map[string]string{"PAYER_DAVINCI_CONTRACT_VERSIONS": "pa.pas@3.0"},
			[]string{"pa.pas@3.0", "SHN_CONTRACT_VERSIONS"}},
		{"system declares no PA line", map[string]string{"PAYER_DAVINCI_CONTRACT_VERSIONS": "pa.pdex@2.1"},
			[]string{"no CRD, DTR or PAS line"}},
		{"explicit declaration with no PA line", map[string]string{"SHN_CONTRACT_VERSIONS": "pa.pdex@2.1"},
			[]string{"SHN_CONTRACT_VERSIONS", "no CRD, DTR or PAS line"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := nativePayerEnv(tc.env)
			_, err := loadConfig(func(k string) string { return env[k] })
			if err == nil {
				t.Fatal("booted")
			}
			for _, m := range tc.must {
				if !strings.Contains(err.Error(), m) {
					t.Fatalf("refusal %q does not name %q", err, m)
				}
			}
		})
	}
	t.Run("a provider's pdex-only declaration is not refused", func(t *testing.T) {
		env := baseEnv(map[string]string{"SHN_CONTRACT_VERSIONS": "pa.pdex@2.1"})
		if _, err := loadConfig(func(k string) string { return env[k] }); err != nil {
			t.Fatalf("provider refused: %v", err)
		}
	})
}

// TestDerivedDeclaration_NeedsItsLane: a derived non-canonical line needs its
// validator lane exactly as a declared one does. On a network without the
// default validator services (FHIR_DEFAULT_VALIDATOR_LANES=none), a 2.2
// system with no FHIR_VALIDATE_URL_2_2 refuses to boot, naming the key and
// where the declaration came from; with the key set it boots with the lane.
func TestDerivedDeclaration_NeedsItsLane(t *testing.T) {
	boot := func(env map[string]string) (map[string]shnsdk.Validator, error) {
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if err != nil {
			return nil, err
		}
		lanes, _, err := discoverValidatorLanes(context.Background(), func(k string) string { return env[k] }, cfg.ContractVersions,
			shnsdk.NewFakeValidator(), cfg, engine.DefaultLaneURL, qualifyDefaultLane)
		return lanes, err
	}
	env := nativePayerEnv(map[string]string{"PAYER_DAVINCI_CONTRACT_VERSIONS": system22, "FHIR_DEFAULT_VALIDATOR_LANES": "none"})
	_, err := boot(env)
	if err == nil {
		t.Fatal("booted a derived 2.2 line with no lane")
	}
	for _, m := range []string{"FHIR_VALIDATE_URL_2_2", "derived from PAYER_DAVINCI_CONTRACT_VERSIONS"} {
		if !strings.Contains(err.Error(), m) {
			t.Fatalf("refusal %q does not name %q", err, m)
		}
	}
	env["FHIR_VALIDATE_URL_2_2"] = "http://validator-2-2.example:8080/fhir"
	lanes, err := boot(env)
	if err != nil {
		t.Fatalf("with its lane: %v", err)
	}
	if lanes["2.2"] == nil {
		t.Fatalf("no 2.2 lane: %v", lanes)
	}
}

// TestDerivedDeclaration_MatchesTheSharedRule: a gateway that derives its
// declaration (SHN_CONTRACT_VERSIONS unset) declares exactly the shared rule's
// set, and a gateway given that set rendered as SHN_CONTRACT_VERSIONS — what
// the hosted control plane renders for a hosted payer — declares the same set,
// so the gateway and the registry entry published for it cannot disagree.
func TestDerivedDeclaration_MatchesTheSharedRule(t *testing.T) {
	for _, system := range []string{system22, "pa.pas@2.2, pa.crd@2.0", "pa.crd@2.2, pa.dtr@2.0, pa.pas@2.0", " pa.pas@2.1 ,pa.pas@2.1, pa.pdex@2.1"} {
		t.Run(system, func(t *testing.T) {
			rule, err := engine.DeriveDeclaredContractVersions(splitTrimmed(system))
			if err != nil {
				t.Fatal(err)
			}
			env := nativePayerEnv(map[string]string{"PAYER_DAVINCI_CONTRACT_VERSIONS": system})
			derived, err := loadConfig(func(k string) string { return env[k] })
			if err != nil {
				t.Fatal(err)
			}
			env["SHN_CONTRACT_VERSIONS"] = strings.Join(rule, ", ")
			rendered, err := loadConfig(func(k string) string { return env[k] })
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(derived.ContractVersions, rule) || !slices.Equal(rendered.ContractVersions, rule) {
				t.Fatalf("derived %q, rendered %q, shared rule %q", derived.ContractVersions, rendered.ContractVersions, rule)
			}
		})
	}
}
