package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestCertificationClientsAreIndependent(t *testing.T) {
	clients := map[string]shnsdk.Validator{
		"2.0": engine.NewCertificationOperationValidator("http://canonical/fhir"),
		"2.1": engine.NewCertificationOperationValidator("http://configured21/fhir"),
	}
	want := map[string]string{"2.0": "http://canonical/fhir", "2.1": "http://configured21/fhir"}
	for line, endpoint := range want {
		v, ok := clients[line].(*shnsdk.OperationValidator)
		if !ok || v.BaseURL != endpoint {
			t.Fatalf("%s: %#v", line, clients[line])
		}
		defer v.Client.CloseIdleConnections()
		for other, w := range clients {
			if other != line && w.(*shnsdk.OperationValidator).Client == v.Client {
				t.Fatal("shared client")
			}
		}
	}
}

// Legacy certify-only configuration remains accepted, but never adds a native
// routing lane. Actual registry observations use the native validation lanes.
func TestCertifyURLNeverLanesRouting(t *testing.T) {
	cfg := config{ConformanceEnforcement: engine.EnforcementStrict, FHIRValidateURL: "http://v/fhir", FHIRValidateURL21: "http://lane21/fhir", FHIRCertifyURL21: "http://certify21/fhir", FHIRCertifyURL22: "http://certify22/fhir"}
	canonical := shnsdk.NewFakeValidator()
	lanes, err := validatorLanesForDeclared(env(nil), []string{"pa.pas@2.0"}, canonical, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if lanes["2.2"] != nil {
		t.Fatal("a certify-only FHIR_CERTIFY_URL_2_2 entered the routing lane map — it must never lane a line")
	}
	if lanes["2.1"] == nil {
		t.Fatal("the configured FHIR_VALIDATE_URL_2_1 lane must still enter the routing lane map")
	}

	// The discovery path reads the same routing fields and no others: 2.2 stays a
	// default candidate that has not qualified, never a lane, whatever the
	// certify-only address says.
	routing, manager, err := discoverValidatorLanes(context.Background(), env(nil), []string{"pa.pas@2.0"}, canonical, cfg,
		engine.DefaultLaneURL, func(context.Context, string, string) error { return errors.New("never qualifies") })
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if routing["2.2"] != nil {
		t.Fatal("discoverValidatorLanes laned 2.2 from a certify-only address")
	}
	if d := manager.defaults["2.2"]; d == nil || d.Ready() || d.Base() != engine.DefaultLaneURL("2.2") {
		t.Fatalf("2.2 default lane = %#v, want the unqualified Compose default, untouched by the certify-only address", d)
	}
}

// Legacy certify-only settings retain their existing URL validation contract.
func TestCertifyURLRequiresHostAtBoot(t *testing.T) {
	e := baseEnv(map[string]string{"FHIR_CERTIFY_URL_2_2": "http:///fhir"})
	_, err := loadConfig(func(k string) string { return e[k] })
	if err == nil || !strings.Contains(err.Error(), "FHIR_CERTIFY_URL_2_2") || !strings.Contains(err.Error(), "host") {
		t.Fatalf("want a boot refusal naming FHIR_CERTIFY_URL_2_2 and the missing host, got %v", err)
	}
	e = baseEnv(map[string]string{"FHIR_CERTIFY_URL_2_2": "http://certify22:8080/fhir"})
	if _, err := loadConfig(func(k string) string { return e[k] }); err != nil {
		t.Fatalf("a well-formed certify address must boot: %v", err)
	}
}

// Native observations use the real rule registries, never passive certifier clients.
// In particular none must not briefly start their independent qualification loops.
func TestBuildDoesNotStartLegacyCertificationClients(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	defer keys.Close()
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	defer disc.Close()
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-test-provider-certification")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "provider", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ROLE":                    "provider",
		"CONFORMANCE_ENFORCEMENT": "none",
		"SHN_SECRETS":             dir,
		"SHN_DISCOVERY_URL":       disc.URL,
		// Exercise real startup: native lane discovery remains separate.
		"FHIR_VALIDATE_URL":         "http://127.0.0.1:9/fhir",
		"FHIR_VALIDATE_URL_2_1":     "http://127.0.0.1:9/fhir21", // configured: not a default
		"FHIR_DATA_URL":             "https://sor.example/fhir",
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
	}
	for _, certifyURL := range []string{"", "http://127.0.0.1:9/certify22"} {
		t.Run("certify="+certifyURL, func(t *testing.T) {
			env["FHIR_CERTIFY_URL_2_2"] = certifyURL
			b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			b.lanes.Close() // stop the boot-time qualifier of the 2.2 default at once
			t.Cleanup(func() {
				if b.gateway != nil {
					_ = b.gateway.Close()
				}
			})
			if len(b.certification) != 0 {
				t.Fatalf("native startup allocated %d legacy certification clients (including independent qualification workers), want zero", len(b.certification))
			}
			if got := b.gateway.ConformanceLevelForTest(); got != engine.EnforcementNone {
				t.Fatalf("level = %v, want none", got)
			}
		})
	}
}

// The exported legacy client remains usable by explicit callers. A default lane
// serves certification only once routing has qualified it. Before
// that the client answers the lane-unavailable error naming the env to set, and
// dials nothing; after qualification it certifies through its own client at the
// lane's endpoint.
func TestCertificationDefaultLaneGatedOnQualification(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"All OK"}]}`))
	}))
	defer upstream.Close()
	lane := engine.NewDiscoveredLane("2.2", upstream.URL+"/fhir", shnsdk.NewOperationValidator(upstream.URL+"/fhir"))
	// A qualifier that never succeeds on its own: this row is about routing's
	// qualification gating the legacy client and its existing reason text.
	never := func(context.Context, string, string) error { return errors.New("not yet") }
	gated := engine.NewGatedCertificationValidator(lane, never, "FHIR_CERTIFY_URL_2_2 and FHIR_VALIDATE_URL_2_2 are not configured")
	defer gated.Close()
	if gated.Endpoint() != upstream.URL+"/fhir" {
		t.Fatalf("gated endpoint = %q", gated.Endpoint())
	}

	_, err := gated.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "profile")
	var unavailable *engine.CertificationLaneUnavailable
	if want := "certification validator unavailable: FHIR_CERTIFY_URL_2_2 and FHIR_VALIDATE_URL_2_2 are not configured; default lane qualification pending"; !errors.As(err, &unavailable) || err.Error() != want {
		t.Fatalf("before qualification: err = %q, want exactly %q (legacy compatibility text)", err, want)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("an unqualified default lane was dialed")
	}

	if err := lane.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	res, err := gated.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "profile")
	if err != nil || !res.Valid {
		t.Fatalf("after qualification: res=%+v err=%v", res, err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("qualified lane dialed %d times, want 1", hits)
	}
}
