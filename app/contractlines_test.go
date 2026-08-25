// contractlines_test.go — contract-line boot gates: the declared-set
// env (D1a) and the per-line validator lanes (F7). Both are FAIL-CLOSED at boot:
// a deployment must not advertise a contract line it cannot build or validate.
package app

import (
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func baseEnv(extra map[string]string) map[string]string {
	m := map[string]string{
		"ROLE":              "provider",
		"SHN_SECRETS":       "/etc/shn/bundles/provider",
		"SHN_DISCOVERY_URL": "http://accounts:8088/discovery",
		// ROLE=provider with an unset ORIGINATION_PROFILE now normalizes to "demo" at
		// load, which requires the operated $populate endpoint.
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// TestDeclaredSetEnv: SHN_CONTRACT_VERSIONS is boot-validated — junk and
// non-native tokens are startup refusals, a valid subset is carried verbatim, and
// an unset env keeps this build's default declaration.
func TestDeclaredSetEnv(t *testing.T) {
	t.Run("unset keeps the build default", func(t *testing.T) {
		cfg, err := loadConfig(func(k string) string { return baseEnv(nil)[k] })
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(cfg.ContractVersions, ",") != strings.Join(shnsdk.SupportedContractVersions(), ",") {
			t.Fatalf("declared = %v, want the build default %v", cfg.ContractVersions, shnsdk.SupportedContractVersions())
		}
	})
	t.Run("junk token refuses boot", func(t *testing.T) {
		e := baseEnv(map[string]string{"SHN_CONTRACT_VERSIONS": "pa.pas@2.0,not-a-token"})
		_, err := loadConfig(func(k string) string { return e[k] })
		if err == nil || !strings.Contains(err.Error(), "not-a-token") {
			t.Fatalf("want a boot refusal naming the junk token, got %v", err)
		}
	})
	t.Run("non-native token refuses boot", func(t *testing.T) {
		e := baseEnv(map[string]string{"SHN_CONTRACT_VERSIONS": "pa.pas@9.9"})
		_, err := loadConfig(func(k string) string { return e[k] })
		if err == nil || !strings.Contains(err.Error(), "pa.pas@9.9") {
			t.Fatalf("want a boot refusal naming the unbuildable token, got %v", err)
		}
	})
	t.Run("valid subset is carried", func(t *testing.T) {
		e := baseEnv(map[string]string{"SHN_CONTRACT_VERSIONS": "pa.crd@2.2, pa.dtr@2.2 ,pa.pas@2.2"})
		cfg, err := loadConfig(func(k string) string { return e[k] })
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(cfg.ContractVersions, ",") != "pa.crd@2.2,pa.dtr@2.2,pa.pas@2.2" {
			t.Fatalf("declared = %v", cfg.ContractVersions)
		}
	})
}

// TestValidatorLaneFailClosed: declaring a non-canonical multi-line contract line
// without its FHIR_VALIDATE_URL_* lane (and with the fake validator off) must
// refuse to boot — validating 2.2 bytes against a 2.0 IG is not a degraded mode,
// it is a wrong answer (FR-36/FR-G29).
func TestValidatorLaneFailClosed(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()

	t.Run("declared 2.2 with no lane refuses", func(t *testing.T) {
		_, err := validatorLanesForDeclared(func(string) string { return "" },
			[]string{"pa.pas@2.2"}, canonical, config{FHIRValidateURL: "http://v/fhir"})
		if err == nil {
			t.Fatal("want a fail-closed boot error")
		}
		for _, must := range []string{"pa.pas@2.2", "FHIR_VALIDATE_URL_2_2", "FR-36"} {
			if !strings.Contains(err.Error(), must) {
				t.Fatalf("refusal %q missing %q", err, must)
			}
		}
	})

	t.Run("declared 2.2 WITH its lane boots", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(string) string { return "" },
			[]string{"pa.pas@2.0", "pa.pas@2.2"}, canonical,
			config{FHIRValidateURL: "http://v/fhir", FHIRValidateURL22: "http://v22/fhir"})
		if err != nil {
			t.Fatal(err)
		}
		if lanes["2.0"] == nil || lanes["2.2"] == nil {
			t.Fatalf("lanes = %v, want both 2.0 and 2.2 resolved", lanes)
		}
		if lanes["2.0"] == lanes["2.2"] {
			t.Fatal("2.2 must NOT be served by the canonical lane — one HAPI hosts one IG version")
		}
	})

	t.Run("fake validator serves every line", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(k string) string {
			if k == "SHN_FAKE_VALIDATOR" {
				return "1"
			}
			return ""
		}, shnsdk.NativeContractVersions(), canonical, config{})
		if err != nil {
			t.Fatalf("SHN_FAKE_VALIDATOR must keep every line laned (harness/e2e): %v", err)
		}
		for _, line := range []string{"2.0", "2.1", "2.2"} {
			if lanes[line] == nil {
				t.Fatalf("fake validator lane missing for %s", line)
			}
		}
	})

	t.Run("the DEFAULT declaration needs no new env", func(t *testing.T) {
		// The pre-multi-line deployment ran ONE validator. pa.pdex@2.1 is a SINGLE-line
		// contract, so it rides the canonical lane rather than demanding a 2.1 HAPI —
		// otherwise every existing deployment would refuse to start.
		lanes, err := validatorLanesForDeclared(func(string) string { return "" },
			shnsdk.SupportedContractVersions(), canonical, config{FHIRValidateURL: "http://v/fhir"})
		if err != nil {
			t.Fatalf("the default declaration must boot with only FHIR_VALIDATE_URL: %v", err)
		}
		if lanes["2.1"] != canonical {
			t.Fatalf("pa.pdex@2.1 must ride the canonical lane, got %v", lanes["2.1"])
		}
	})
}

// TestValidatorLaneSingleLineDeclaration: a contract declared at
// exactly ONE line must fail-close identically to the 2+-line case when that
// line is non-canonical, undeclared elsewhere, and has no configured lane —
// the demo/refuse holders (the bridging-demo topology) declare each
// of pa.crd/pa.dtr/pa.pas at a single line each, so the guard must not treat
// "declared once" as "nothing to validate". This is the rejection-test half
// of the guard: linesPerContract is keyed by NativeContractVersions() (a
// package-level constant), not by what's declared, so pa.crd/pa.dtr/pa.pas —
// each natively tri-line — were never actually skippable by the len(...)<2
// check; only a GENUINELY single-native-line contract (pa.pdex, one native
// token) is. These cases lock that guarantee down with an explicit test.
func TestValidatorLaneSingleLineDeclaration(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()

	t.Run("declared pa.crd@2.2 alone, no lane, refuses", func(t *testing.T) {
		_, err := validatorLanesForDeclared(func(string) string { return "" },
			[]string{"pa.crd@2.2"}, canonical, config{FHIRValidateURL: "http://v/fhir"})
		if err == nil {
			t.Fatal("want a fail-closed boot error for a single-line non-canonical declaration")
		}
		for _, must := range []string{"pa.crd@2.2", "FHIR_VALIDATE_URL_2_2", "FR-36"} {
			if !strings.Contains(err.Error(), must) {
				t.Fatalf("refusal %q missing %q", err, must)
			}
		}
	})

	t.Run("declared pa.crd@2.2 alone, fake validator, boots", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(k string) string {
			if k == "SHN_FAKE_VALIDATOR" {
				return "1"
			}
			return ""
		}, []string{"pa.crd@2.2"}, canonical, config{FHIRValidateURL: "http://v/fhir"})
		if err != nil {
			t.Fatalf("SHN_FAKE_VALIDATOR must still serve a single-line declaration: %v", err)
		}
		if lanes["2.2"] != canonical {
			t.Fatalf("lanes = %v, want 2.2 served by the fake", lanes)
		}
	})

	t.Run("declared pa.crd@2.2 alone, with its lane, boots", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(string) string { return "" },
			[]string{"pa.crd@2.2"}, canonical,
			config{FHIRValidateURL: "http://v/fhir", FHIRValidateURL22: "http://v22/fhir"})
		if err != nil {
			t.Fatalf("a configured lane must let a single-line declaration boot: %v", err)
		}
		if lanes["2.2"] == nil || lanes["2.2"] == canonical {
			t.Fatalf("lanes = %v, want 2.2 served by the configured 2.2 lane (not canonical)", lanes)
		}
	})

	t.Run("declared pa.pas@2.0 alone, the canonical line, boots without any lane env", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(string) string { return "" },
			[]string{"pa.pas@2.0"}, canonical, config{FHIRValidateURL: "http://v/fhir"})
		if err != nil {
			t.Fatalf("a single canonical-line declaration must boot with only FHIR_VALIDATE_URL: %v", err)
		}
		if lanes["2.0"] != canonical {
			t.Fatalf("lanes = %v, want 2.0 served by the canonical lane", lanes)
		}
	})

	t.Run("declared pa.pdex@2.1 alone, genuinely single-native-line, still rides canonical unaffected", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(string) string { return "" },
			[]string{"pa.pdex@2.1"}, canonical, config{FHIRValidateURL: "http://v/fhir"})
		if err != nil {
			t.Fatalf("pa.pdex@2.1 (no other native pdex line exists) must still boot on the canonical lane: %v", err)
		}
		if lanes["2.1"] != canonical {
			t.Fatalf("lanes = %v, want pa.pdex@2.1 riding the canonical lane unchanged", lanes)
		}
	})
}

// TestLaneMapIncludesConfiguredUndeclaredLine: a configured
// FHIR_VALIDATE_URL_<line> for a NATIVE line enters the lane map even when
// that line is UNDECLARED — the exact widening the recorded route-selection
// deviation names (arm (2) native-reach needs the lane map to cover more
// than the declared set). Paired with the rejection row: a DECLARED line
// with no configured lane still refuses boot — the widening only ADDS lanes,
// it never rescues a declared-but-unlaned line.
func TestLaneMapIncludesConfiguredUndeclaredLine(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()

	t.Run("undeclared line with a configured lane enters the map", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(string) string { return "" },
			[]string{"pa.pas@2.0"}, canonical, // 2.2 NOT declared
			config{FHIRValidateURL: "http://v/fhir", FHIRValidateURL22: "http://v22/fhir"})
		if err != nil {
			t.Fatal(err)
		}
		if lanes["2.2"] == nil {
			t.Fatal("a configured FHIR_VALIDATE_URL_2_2 must enter the lane map even though 2.2 is undeclared")
		}
	})

	t.Run("undeclared line with NO configured lane stays absent (opt-in, not automatic)", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(string) string { return "" },
			[]string{"pa.pas@2.0"}, canonical, config{FHIRValidateURL: "http://v/fhir"}) // no 2.1/2.2 URL at all
		if err != nil {
			t.Fatal(err)
		}
		if lanes["2.2"] != nil {
			t.Fatal("an undeclared line with NO configured URL must stay unlaned — D1a is an opt-in, not automatic")
		}
	})

	t.Run("rejection pair: declared-without-lane still fails closed", func(t *testing.T) {
		_, err := validatorLanesForDeclared(func(string) string { return "" },
			[]string{"pa.pas@2.0", "pa.pas@2.2"}, canonical,
			config{FHIRValidateURL: "http://v/fhir"}) // no FHIRValidateURL22
		if err == nil {
			t.Fatal("declared 2.2 with no configured lane must still refuse boot — D1a only ADDS undeclared lanes, never rescues a declared-but-unlaned line")
		}
	})
}
