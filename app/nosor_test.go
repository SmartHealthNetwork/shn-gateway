package app

// nosor_test.go — a payer whose own system answers every exchange behind
// PAYER_DAVINCI_BASE_URL may run without FHIR_DATA_URL: it boots with
// no system of record (engine.NoSystemOfRecord) and says so. Every other
// gateway still refuses to boot without one, and a store-less payer cannot opt
// in to REQUIRE_KNOWN_MEMBERS: with no store there are no members to know.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// noSystemOfRecordBootLine is the line build prints for a store-less payer.
const noSystemOfRecordBootLine = "gateway: no system of record (FHIR_DATA_URL unset)"

// The payer-native eligibility boot lines with PAYER_ELIGIBILITY_URL unset.
const (
	noSORNoEligibilityLine = `gateway: PAYER_ELIGIBILITY_URL unset — with no system of record, a coverage-eligibility request is answered 501 "coverage eligibility is not offered by this payer"`
	recordsEligibilityLine = "gateway: PAYER_ELIGIBILITY_URL unset — coverage eligibility is answered from the payer's records"
)

// nosorBuildEnv is the env for build() of a gateway registered as role, with
// hermetic discovery, anchor keys and a payer Da Vinci endpoint, and no
// FHIR_DATA_URL; extra is laid over it.
func nosorBuildEnv(t *testing.T, role string, extra map[string]string) map[string]string {
	t.Helper()
	payer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(payer.Close)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	t.Cleanup(keys.Close)
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	t.Cleanup(disc.Close)
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-test-nosor-" + role)
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, role, "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ROLE": role, "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": disc.URL, "SHN_FAKE_VALIDATOR": "1",
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
	}
	if role == "payer" {
		env["PAYER_DAVINCI_BASE_URL"] = payer.URL
		env["PAYER_DAVINCI_CRD_SERVICE_ID"] = "svc"
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// builtSystemOfRecord is the dynamic type of the system of record build handed
// engine.New. engine.Gateway exposes no accessor for it, so the test reads the
// unexported field; it never calls through it.
func builtSystemOfRecord(t *testing.T, b built) reflect.Type {
	t.Helper()
	if b.gateway == nil {
		t.Fatal("build returned no gateway")
	}
	sor := reflect.ValueOf(b.gateway).Elem().FieldByName("cfg").FieldByName("SoR")
	if !sor.IsValid() || sor.IsNil() {
		t.Fatal("engine.Gateway has no cfg.SoR: the test's reach into the engine is stale")
	}
	return sor.Elem().Type()
}

func buildForTest(t *testing.T, env map[string]string) (built, string, error) {
	t.Helper()
	var out bytes.Buffer
	b, err := build(context.Background(), func(k string) string { return env[k] }, &out, nil)
	t.Cleanup(func() {
		if b.gateway != nil {
			_ = b.gateway.Close()
		}
	})
	return b, out.String(), err
}

// TestBuild_PayerNativeBootsWithoutSystemOfRecord: ROLE=payer with
// PAYER_DAVINCI_BASE_URL and no FHIR_DATA_URL boots with no system of record
// and prints the boot line. The control, the same payer with FHIR_DATA_URL,
// reads its own FHIR system of record and prints no such line.
func TestBuild_PayerNativeBootsWithoutSystemOfRecord(t *testing.T) {
	none := reflect.TypeOf(engine.NoSystemOfRecord())

	b, out, err := buildForTest(t, nosorBuildEnv(t, "payer", nil))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := builtSystemOfRecord(t, b); got != none {
		t.Fatalf("system of record = %v, want %v", got, none)
	}
	if b.nativeResponder == nil {
		t.Fatal("no native responder: the payer's own system must answer every exchange")
	}
	if !strings.Contains(out, noSystemOfRecordBootLine) || !strings.Contains(out, "PAYER_ELIGIBILITY_URL") {
		t.Fatalf("boot output missing %q naming PAYER_ELIGIBILITY_URL:\n%s", noSystemOfRecordBootLine, out)
	}
	// Its eligibility line says what it does without an eligibility endpoint,
	// never that it answers from records it does not keep.
	if !strings.Contains(out, noSORNoEligibilityLine) || strings.Contains(out, recordsEligibilityLine) {
		t.Fatalf("store-less eligibility boot line: want %q, not %q:\n%s", noSORNoEligibilityLine, recordsEligibilityLine, out)
	}

	b, out, err = buildForTest(t, nosorBuildEnv(t, "payer", map[string]string{"FHIR_DATA_URL": "https://sor.example/fhir"}))
	if err != nil {
		t.Fatalf("control build: %v", err)
	}
	if got := builtSystemOfRecord(t, b); got == none {
		t.Fatal("control: a payer with FHIR_DATA_URL was given no system of record")
	}
	if strings.Contains(out, noSystemOfRecordBootLine) {
		t.Fatalf("control: a payer with FHIR_DATA_URL printed %q", noSystemOfRecordBootLine)
	}
	if !strings.Contains(out, recordsEligibilityLine) || strings.Contains(out, noSORNoEligibilityLine) {
		t.Fatalf("control: eligibility boot line: want %q:\n%s", recordsEligibilityLine, out)
	}
}

// TestBuild_NoSystemOfRecordRefusedOffPayerNative: every gateway but a
// payer-native one still refuses to boot without FHIR_DATA_URL, naming it. The
// provider, facility and phg rows carry a stray PAYER_DAVINCI_BASE_URL, so
// only the role keeps them off the store-less path; the payer row has no
// PAYER_DAVINCI_BASE_URL, so only the forward keeps it off.
func TestBuild_NoSystemOfRecordRefusedOffPayerNative(t *testing.T) {
	// A closed local port: nothing may dial it before the refusal.
	stray := map[string]string{"PAYER_DAVINCI_BASE_URL": "http://127.0.0.1:1", "PAYER_DAVINCI_CRD_SERVICE_ID": "svc"}
	for name, row := range map[string]struct {
		env       map[string]string
		wantStray bool
	}{
		"provider with a stray PAYER_DAVINCI_BASE_URL": {nosorBuildEnv(t, "provider", stray), true},
		"facility with a stray PAYER_DAVINCI_BASE_URL": {nosorBuildEnv(t, "facility", stray), true},
		"phg with a stray PAYER_DAVINCI_BASE_URL":      {nosorBuildEnv(t, "phg", stray), true},
		"payer without PAYER_DAVINCI_BASE_URL": {nosorBuildEnv(t, "payer", map[string]string{
			"PAYER_DAVINCI_BASE_URL": "", "PAYER_DAVINCI_CRD_SERVICE_ID": "",
		}), false},
	} {
		t.Run(name, func(t *testing.T) {
			// The row reaches build's check with the configuration it names:
			// loadConfig keeps the stray forward, so only the role refuses.
			cfg, err := loadConfig(func(k string) string { return row.env[k] })
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got := cfg.PayerDavinciBaseURL != ""; got != row.wantStray {
				t.Fatalf("loaded PAYER_DAVINCI_BASE_URL = %q, want set=%v", cfg.PayerDavinciBaseURL, row.wantStray)
			}
			_, out, err := buildForTest(t, row.env)
			if !errors.Is(err, errNoSystemOfRecord) || !strings.Contains(err.Error(), "FHIR_DATA_URL is required") {
				t.Fatalf("build err = %v, want the FHIR_DATA_URL-required refusal", err)
			}
			if strings.Contains(out, noSystemOfRecordBootLine) {
				t.Fatalf("a refused gateway printed %q", noSystemOfRecordBootLine)
			}
		})
	}
}

// TestLoadConfig_KnownMembersNeedASystemOfRecord: REQUIRE_KNOWN_MEMBERS=true
// is refused for a store-less payer-native gateway and accepted beside
// FHIR_DATA_URL; a store-less payer that does not opt in loads.
func TestLoadConfig_KnownMembersNeedASystemOfRecord(t *testing.T) {
	base := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_BASE_URL": "https://payer.example",
	}
	for _, tc := range []struct {
		name    string
		extra   map[string]string
		wantErr string
		want    bool
	}{
		{name: "store-less, opted in", extra: map[string]string{"REQUIRE_KNOWN_MEMBERS": "true"},
			wantErr: "REQUIRE_KNOWN_MEMBERS=true needs FHIR_DATA_URL"},
		{name: "with FHIR_DATA_URL, opted in", extra: map[string]string{"REQUIRE_KNOWN_MEMBERS": "true", "FHIR_DATA_URL": "https://sor.example/fhir"}, want: true},
		{name: "store-less, not opted in", extra: map[string]string{"REQUIRE_KNOWN_MEMBERS": "false"}},
		{name: "store-less, unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for k, v := range tc.extra {
				env[k] = v
			}
			cfg, err := loadConfig(func(k string) string { return env[k] })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("loadConfig err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.RequireKnownMembers != tc.want {
				t.Fatalf("RequireKnownMembers = %v, want %v", cfg.RequireKnownMembers, tc.want)
			}
		})
	}
}
