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
	cfg := config{FHIRValidateURL21: "http://configured21/fhir"}
	clients := certificationValidators(env(nil), cfg, "http://canonical/fhir", nil, nil)
	// No lane is invented: 2.2 has neither a configured lane nor a default, so it
	// has no client and the collector records it as unavailable.
	if _, invented := clients["2.2"]; invented {
		t.Fatalf("2.2 got a certification client with no lane configured: %#v", clients["2.2"])
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
	fake := certificationValidators(env(map[string]string{"SHN_FAKE_VALIDATOR": "1"}), cfg, "", nil, nil)
	for _, v := range fake {
		if _, ok := v.(*engine.LineFakeValidator); !ok {
			t.Fatal("fake mode used network")
		}
	}
}

// The evidence address for a line is FHIR_CERTIFY_URL_<line> first, then the
// routing lane FHIR_VALIDATE_URL_<line>; and a certify-only address is never a
// lane: the routing lane maps built from the same config do not gain the line,
// so an inbound frame at that line stays refused and origination cannot target
// it (the engine rows in versionroute_test pin that side).
func TestCertifyURLPrecedesLaneAndNeverLanesRouting(t *testing.T) {
	cfg := config{FHIRValidateURL: "http://v/fhir", FHIRValidateURL21: "http://lane21/fhir", FHIRCertifyURL21: "http://certify21/fhir", FHIRCertifyURL22: "http://certify22/fhir"}
	clients := certificationValidators(env(nil), cfg, "http://v/fhir", nil, nil)
	for line, want := range map[string]string{"2.1": "http://certify21/fhir", "2.2": "http://certify22/fhir"} {
		v, ok := clients[line].(*shnsdk.OperationValidator)
		if !ok || v.BaseURL != want {
			t.Fatalf("%s certification client = %#v, want %s", line, clients[line], want)
		}
		v.Client.CloseIdleConnections()
	}

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

// A certify-only address is dialed like a lane, so a value with no host is a
// boot refusal naming the key — never a per-exchange failure behind a hash.
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

// build wires the lane manager's defaults into the certification clients: a
// line with no address gets the gated default, at the Compose default
// endpoint, from the same lane routing is qualifying. Passing nil at the build
// call site leaves 2.2 with no client and this row red.
func TestBuildWiresDefaultLanesIntoCertification(t *testing.T) {
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
		"ROLE":              "provider",
		"SHN_SECRETS":       dir,
		"SHN_DISCOVERY_URL": disc.URL,
		// A real (unreachable) canonical validator, not the fake: fake mode
		// replaces every certification client and would hide the wiring.
		"FHIR_VALIDATE_URL":         "http://127.0.0.1:9/fhir",
		"FHIR_VALIDATE_URL_2_1":     "http://127.0.0.1:9/fhir21", // configured: not a default
		"FHIR_DATA_URL":             "https://sor.example/fhir",
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
	}
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
	gated, ok := b.certification["2.2"].(*engine.GatedCertificationValidator)
	if !ok {
		t.Fatalf("2.2 certification client = %#v, want the gated default wired from the lane manager", b.certification["2.2"])
	}
	if gated.Endpoint() != engine.DefaultLaneURL("2.2") {
		t.Fatalf("gated endpoint = %q, want the Compose default %q", gated.Endpoint(), engine.DefaultLaneURL("2.2"))
	}
	if v, ok := b.certification["2.1"].(*shnsdk.OperationValidator); !ok || v.BaseURL != "http://127.0.0.1:9/fhir21" {
		t.Fatalf("2.1 certification client = %#v, want the configured lane's own client", b.certification["2.1"])
	}
}

// A default lane serves certification only once routing has qualified it. Before
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
	// qualification gating the client, and the exact reason text the
	// configuration reference promises.
	never := func(context.Context, string, string) error { return errors.New("not yet") }
	clients := certificationValidators(env(nil), config{}, "http://canonical/fhir", map[string]*engine.DiscoveredLane{"2.2": lane}, never)
	gated, ok := clients["2.2"].(*engine.GatedCertificationValidator)
	if !ok {
		t.Fatalf("2.2 client = %#v, want the gated default", clients["2.2"])
	}
	defer gated.Close()
	if gated.Endpoint() != upstream.URL+"/fhir" {
		t.Fatalf("gated endpoint = %q", gated.Endpoint())
	}

	_, err := gated.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "profile")
	var unavailable *engine.CertificationLaneUnavailable
	if want := "certification validator unavailable: FHIR_CERTIFY_URL_2_2 and FHIR_VALIDATE_URL_2_2 are not configured; default lane qualification pending"; !errors.As(err, &unavailable) || err.Error() != want {
		t.Fatalf("before qualification: err = %q, want exactly %q (the configuration reference promises this text)", err, want)
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
