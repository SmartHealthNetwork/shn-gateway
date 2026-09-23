// contractlines_test.go — contract-line boot gates: the declared-set
// env (D1a) and the per-line validator lanes (F7). Declared defaults qualify at
// boot; explicit endpoints preserve URL-only startup. Configured-map tests
// separately retain the legacy pure helper contract.
package app

import (
	"context"
	"errors"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"slices"
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
func TestConfiguredValidatorLaneFailClosed(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()

	t.Run("declared 2.2 with no lane stays unavailable", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(string) string { return "" }, []string{"pa.pas@2.2"}, canonical, config{FHIRValidateURL: "http://v/fhir"})
		if err != nil || lanes["2.2"] != nil {
			t.Fatalf("missing lane borrowed or boot blocked: lanes=%v err=%v", lanes, err)
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
			assertLineFake(t, lanes[line], line)
			for _, other := range []string{"2.0", "2.1", "2.2"} {
				if line != other && lanes[line] == lanes[other] {
					t.Fatalf("lines %s and %s share a validator", line, other)
				}
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
func TestConfiguredValidatorLaneSingleLineDeclaration(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()

	t.Run("declared pa.crd@2.2 alone, no lane, stays unavailable", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(string) string { return "" }, []string{"pa.crd@2.2"}, canonical, config{FHIRValidateURL: "http://v/fhir"})
		if err != nil || lanes["2.2"] != nil {
			t.Fatalf("missing lane borrowed or boot blocked: lanes=%v err=%v", lanes, err)
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
		assertLineFake(t, lanes["2.2"], "2.2")
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
func TestConfiguredLaneMapIncludesConfiguredUndeclaredLine(t *testing.T) {
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

	t.Run("declared-without-lane stays unavailable", func(t *testing.T) {
		lanes, err := validatorLanesForDeclared(func(string) string { return "" }, []string{"pa.pas@2.0", "pa.pas@2.2"}, canonical, config{})
		if err != nil || lanes["2.2"] != nil {
			t.Fatal("missing lane borrowed or boot blocked")
		}
	})
}

// assertLineFake verifies both the selected line and its structural discrimination.
func assertLineFake(t *testing.T, validator shnsdk.Validator, line string) {
	t.Helper()
	fake, ok := validator.(*engine.LineFakeValidator)
	if !ok || fake.Line != line {
		t.Fatalf("validator = %T (%v), want structural fake for %s", validator, validator, line)
	}
	result, err := fake.Validate(context.Background(), []byte(`{"resourceType":"Claim","type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"professional"}]},"item":[{"sequence":1}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid != (line == "2.0") {
		t.Fatalf("line %s accepted a Claim without later-line item details: valid=%v issues=%v", line, result.Valid, result.Issues)
	}
}

func TestSelectValidatorCanonicalLineFake(t *testing.T) {
	validator, err := selectValidator(func(k string) string {
		if k == "SHN_FAKE_VALIDATOR" {
			return "1"
		}
		return ""
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	assertLineFake(t, validator, "2.0")
}

// The named declaration gates exercise discovery and retain its cleanup owner;
// TestConfigured* above independently preserves configured-map compatibility.
func TestValidatorLaneFailClosed(t *testing.T) {
	for _, token := range []string{"pa.crd@2.2", "pa.dtr@2.1", "pa.pas@2.1"} {
		for _, fail := range []bool{false, true} {
			line := shnsdk.LineOf(token)
			lanes, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{token}, shnsdk.NewFakeValidator(), config{ConformanceEnforcement: engine.EnforcementStrict}, engine.DefaultLaneURL, func(context.Context, string, string) error {
				if fail {
					return errors.New("finite qualifier refused")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			m.workers.Wait()
			if lanes[line] != nil || m.defaults[line].Ready() == fail {
				t.Fatal("qualification coverage misreported")
			}
			m.Close()
		}
	}
}

func TestLaneMapIncludesConfiguredUndeclaredLine(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()
	lanes, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.pdex@2.1"}, canonical, config{ConformanceEnforcement: engine.EnforcementStrict, FHIRValidateURL22: "http://configured.test/fhir"}, engine.DefaultLaneURL, func(ctx context.Context, _, _ string) error { <-ctx.Done(); return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if lanes["2.2"] == nil || m.defaults["2.2"] != nil {
		t.Fatal("explicit undeclared lane lost precedence")
	}
	if lanes["2.1"] != canonical || !m.fallbacks["2.1"] || m.defaults["2.1"].Ready() {
		t.Fatal("PDex fallback and unavailable default were conflated")
	}
}

// Native backend configuration does not expand this gateway's authored builders
// or its ordinary publication declaration. Future receive publication is separate.
func TestNativeBackendFutureConfigurationIsNotBuilderCapability(t *testing.T) {
	e := baseEnv(map[string]string{"ROLE": "payer", "SHN_CONTRACT_VERSIONS": "pa.pas@2.0", "PAYER_DAVINCI_BASE_URL": "https://backend.example", "PAYER_DAVINCI_CONTRACT_VERSIONS": "pa.pas@9.9"})
	cfg, err := loadConfig(func(k string) string { return e[k] })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.PayerDavinciContractVersions, ",") != "pa.pas@9.9" || strings.Join(cfg.ContractVersions, ",") != "pa.pas@2.0" {
		t.Fatalf("backend=%v builders=%v", cfg.PayerDavinciContractVersions, cfg.ContractVersions)
	}
	if strings.Join(cfg.NativeReceiveVersions, ",") != "pa.pas@9.9" {
		t.Fatalf("native receive declaration = %v", cfg.NativeReceiveVersions)
	}
	delete(e, "PAYER_DAVINCI_BASE_URL")
	if _, err := loadConfig(func(k string) string { return e[k] }); err == nil || !strings.Contains(err.Error(), "PAYER_DAVINCI_CONTRACT_VERSIONS") {
		t.Fatalf("future backend without base: %v", err)
	}
	e["PAYER_DAVINCI_BASE_URL"] = "https://backend.example"
	e["SHN_CONTRACT_VERSIONS"] = "pa.pas@9.9"
	if _, err := loadConfig(func(k string) string { return e[k] }); err == nil || !strings.Contains(err.Error(), "pa.pas@9.9") {
		t.Fatalf("future authored builder admitted: %v", err)
	}
}

func TestNativeReceivePublicationMatchesActualBackend(t *testing.T) {
	e := baseEnv(map[string]string{"ROLE": "payer", "PAYER_DAVINCI_BASE_URL": "https://backend.example/fhir", "PAYER_DAVINCI_CONTRACT_VERSIONS": "pa.pas@9.9"})
	cfg, err := loadConfig(func(k string) string { return e[k] })
	if err != nil {
		t.Fatal(err)
	}
	reg := shnsdk.NewRegistry()
	entry := shnsdk.RegistryEntry{ID: "payer", Role: "payer", BaseURL: "https://gateway.example", ContractVersions: cfg.PublishedContractVersions}
	reg.Set("payer", entry)
	if err := checkNativeReceivePublication(cfg, "payer", reg); err != nil {
		t.Fatal(err)
	}
	entry.ContractVersions = []string{shnsdk.ContractPAPDex21}
	reg.Set("payer", entry)
	if err := checkNativeReceivePublication(cfg, "payer", reg); err != nil {
		t.Fatalf("underpublication must permit boot until safe post-deploy rotation: %v", err)
	}
	entry.ContractVersions = shnsdk.SupportedContractVersions()
	reg.Set("payer", entry)
	if err := checkNativeReceivePublication(cfg, "payer", reg); err == nil {
		t.Fatal("known PAS line absent from backend advertised")
	}
	delete(e, "PAYER_DAVINCI_CONTRACT_VERSIONS")
	cfg, err = loadConfig(func(k string) string { return e[k] })
	if err != nil {
		t.Fatal(err)
	}
	entry.ContractVersions = append(shnsdk.SupportedContractVersions(), "pa.pas@9.9")
	reg.Set("payer", entry)
	if err := checkNativeReceivePublication(cfg, "payer", reg); err == nil {
		t.Fatal("contradictory registry declaration accepted")
	}
}

func TestKnownBackendReceiveLineDoesNotChangeBuilder(t *testing.T) {
	e := baseEnv(map[string]string{"ROLE": "payer", "PAYER_DAVINCI_BASE_URL": "https://backend.example/fhir", "PAYER_DAVINCI_CONTRACT_VERSIONS": "pa.pas@2.2"})
	cfg, err := loadConfig(func(k string) string { return e[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.PublishedContractVersions, "pa.pas@2.2") || slices.Contains(cfg.PublishedContractVersions, "pa.pas@2.0") {
		t.Fatalf("peer-visible backend receipt = %v", cfg.PublishedContractVersions)
	}
	if !slices.Contains(cfg.ContractVersions, "pa.pas@2.0") || slices.Contains(cfg.ContractVersions, "pa.pas@2.2") {
		t.Fatalf("authored builder set changed = %v", cfg.ContractVersions)
	}
	reg := shnsdk.NewRegistry()
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", BaseURL: "https://gateway.example", ContractVersions: cfg.PublishedContractVersions})
	if err := checkNativeReceivePublication(cfg, "payer", reg); err != nil {
		t.Fatalf("app refused backend-supported feed line: %v", err)
	}
}

func TestNativeReceiveBootGuardPreservesUnconfiguredKnownLines(t *testing.T) {
	for _, role := range []string{"provider", "payer"} {
		e := baseEnv(map[string]string{"ROLE": role})
		cfg, err := loadConfig(func(k string) string { return e[k] })
		if err != nil {
			t.Fatal(err)
		}
		reg := shnsdk.NewRegistry()
		entry := shnsdk.RegistryEntry{ID: role, Role: role, BaseURL: "https://gateway.example", ContractVersions: []string{"pa.pas@2.2"}}
		reg.Set(role, entry)
		if err := checkNativeReceivePublication(cfg, role, reg); err != nil {
			t.Fatalf("%s rejected SDK-known native receipt without backend assertion: %v", role, err)
		}
		entry.ContractVersions = []string{"pa.pas@9.9"}
		reg.Set(role, entry)
		if err := checkNativeReceivePublication(cfg, role, reg); err == nil {
			t.Fatalf("%s accepted unsupported future receipt", role)
		}
	}
}
