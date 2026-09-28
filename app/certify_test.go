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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	"github.com/SmartHealthNetwork/shn-gateway/internal/testrecord"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestCertificationClientsAreIndependent(t *testing.T) {
	cfg := config{FHIRValidateURL21: "http://configured21/fhir"}
	clients := certificationValidators(env(nil), cfg, "http://canonical/fhir", nil, nil)
	defer engine.CloseCertificationClients(clients)
	// No lane is invented: 2.2 has neither a configured lane nor a default, so it
	// has no client and the collector records it as unavailable.
	if _, invented := clients["2.2"]; invented {
		t.Fatalf("2.2 got a certification client with no lane configured: %#v", clients["2.2"])
	}
	canonical, ok := clients["2.0"].(*shnsdk.OperationValidator)
	if !ok || canonical.BaseURL != "http://canonical/fhir" {
		t.Fatalf("2.0: %#v", clients["2.0"])
	}
	// A 2.1 address certifies only once it has qualified, through its own client.
	gated, ok := clients["2.1"].(*engine.GatedCertificationValidator)
	if !ok || gated.Endpoint() != "http://configured21/fhir" {
		t.Fatalf("2.1: %#v, want the configured lane gated on its own qualification", clients["2.1"])
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
	defer engine.CloseCertificationClients(clients)
	for line, want := range map[string]string{"2.1": "http://certify21/fhir", "2.2": "http://certify22/fhir"} {
		v, ok := clients[line].(*engine.GatedCertificationValidator)
		if !ok || v.Endpoint() != want {
			t.Fatalf("%s certification client = %#v, want %s", line, clients[line], want)
		}
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

// unresolvedComposeLanes answers the default lanes' Compose names in
// http.DefaultTransport, unresolved, for the rest of the test: the 2.2 default
// lane's boot qualifier dials one, and it would otherwise be resolved on the
// live network. Call it once, before any build, so no build's goroutine is
// reading http.DefaultTransport when it is swapped or restored.
func unresolvedComposeLanes(t *testing.T) {
	t.Helper()
	base := http.DefaultTransport
	http.DefaultTransport = lifecycleTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Hostname(), "shn-validator-") {
			return nil, fmt.Errorf("dial %s: no such host (fixture)", r.URL.Host)
		}
		return base.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = base })
}

// buildForCertification builds a provider gateway with a configured 2.1
// certification address and the 2.2 default lane, plus extra env. Its caller
// runs unresolvedComposeLanes first.
func buildForCertification(t *testing.T, extra map[string]string) built {
	t.Helper()
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
	for k, v := range extra {
		env[k] = v
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
	return b
}

// build wires the lane manager's defaults into the certification clients: a
// line with no address gets the gated default, at the Compose default
// endpoint, from the same lane routing is qualifying. Passing nil at the build
// call site leaves 2.2 with no client and this row red.
func TestBuildWiresDefaultLanesIntoCertification(t *testing.T) {
	unresolvedComposeLanes(t)
	b := buildForCertification(t, nil)
	gated, ok := b.certification["2.2"].(*engine.GatedCertificationValidator)
	if !ok {
		t.Fatalf("2.2 certification client = %#v, want the gated default wired from the lane manager", b.certification["2.2"])
	}
	if gated.Endpoint() != engine.DefaultLaneURL("2.2") {
		t.Fatalf("gated endpoint = %q, want the Compose default %q", gated.Endpoint(), engine.DefaultLaneURL("2.2"))
	}
	if v, ok := b.certification["2.1"].(*engine.GatedCertificationValidator); !ok || v.Endpoint() != "http://127.0.0.1:9/fhir21" {
		t.Fatalf("2.1 certification client = %#v, want the configured lane gated on its own qualification", b.certification["2.1"])
	}
}

// build asks for the boot certification warm-up exactly when certification
// evidence is collected: at every level but none, where no certification
// worker starts and nothing would use a warmed validator.
func TestBuildWarmsCertificationOnlyWhenItRuns(t *testing.T) {
	unresolvedComposeLanes(t)
	for level, want := range map[string]bool{"": true, "observe": true, "structural": true, "strict": true, "none": false} {
		b := buildForCertification(t, map[string]string{"CONFORMANCE_ENFORCEMENT": level})
		if b.warmCertification != want {
			t.Errorf("CONFORMANCE_ENFORCEMENT=%q: warmCertification %v, want %v", level, b.warmCertification, want)
		}
		if len(b.certification) == 0 {
			t.Errorf("CONFORMANCE_ENFORCEMENT=%q: build handed the warm-up no certification clients", level)
		}
	}
}

// A default lane serves certification only once routing has qualified it. Before
// that the client answers the lane-unavailable error naming the env to set, and
// dials nothing; after qualification it certifies through its own client at the
// lane's endpoint, and records the lane's verdict: the lane is a real 2.2 lane
// replayed strictly (../engine/testdata/recordings/lane-2.2-certify-literal.json),
// which finds the literal Claim invalid.
func TestCertificationDefaultLaneGatedOnQualification(t *testing.T) {
	rec := testrecord.Load(t, filepath.Join("..", "engine", "testdata", "recordings", "lane-2.2-certify-literal.json"))
	rec.Subset() // the lane's answer with no profile is not asked; hits bound the other
	replay := rec.Server()
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		replay.Config.Handler.ServeHTTP(w, r)
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
	gated.Start() // what the certification worker does when it starts
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
	if err != nil || res.Valid || len(res.Issues) != 9 || !slices.Contains(res.Issues, "Invalid profile. Failed to retrieve explicitly requested profile with url=profile") {
		t.Fatalf("after qualification: res=%+v err=%v, want the lane's invalid verdict (nine errors, the unknown profile among them)", res, err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("qualified lane dialed %d times, want 1", hits)
	}
}

// A 2.2 address (FHIR_VALIDATE_URL_2_2) certifies only once it has passed the
// qualification a default lane passes, run by the real qualifier on the
// client's own loop. The lane is the real 2.2 lane replayed cold
// (../internal/lanequalify/testdata/recordings/lane-2.2-metadata.json and
// lane-2.2-warm.json), whose first answer to the versioned approved
// ClaimResponse is still warming (three slicing errors). Before the
// qualification passes, the exchange's verdict is "lane not qualified" and the
// lane is not dialed; the qualification's corpus meets the warming answer, and
// the certification after it meets the lane's settled answer: valid.
func TestCertificationAddressGatedOnItsOwnQualification(t *testing.T) {
	warm := testrecord.Load(t, laneRecording(t, "lane-2.2-warm"))
	warm.Subset() // the corpus and one certification are asked, not the verification pass
	metadata := testrecord.Load(t, laneRecording(t, "lane-2.2-metadata"))
	warmReplay, metadataReplay := warm.Server(), metadata.Server()
	var posts int32
	lane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fhir/metadata" {
			metadataReplay.Config.Handler.ServeHTTP(w, r)
			return
		}
		atomic.AddInt32(&posts, 1)
		warmReplay.Config.Handler.ServeHTTP(w, r)
	}))
	defer lane.Close()
	row := warm.Exchanges[4] // the versioned approved ClaimResponse, first asked while warming
	payload, profile := []byte(row.Request.Body), row.Request.Query["profile"][0]
	if !strings.Contains(string(warm.Exchanges[4].Response.Body), "SLICING_CANNOT_BE_EVALUATED") || strings.Contains(string(warm.Exchanges[5].Response.Body), `"severity":"error"`) {
		t.Fatal("the recording no longer holds the warming answer followed by the settled one")
	}

	release := make(chan struct{})
	qualify := func(ctx context.Context, base, line string) error {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return qualifyDefaultLane(ctx, base, line)
	}
	clients := certificationValidators(env(nil), config{FHIRValidateURL22: lane.URL + "/fhir"}, "http://canonical/fhir", nil, qualify)
	defer engine.CloseCertificationClients(clients)
	gated, ok := clients["2.2"].(*engine.GatedCertificationValidator)
	if !ok {
		t.Fatalf("2.2 client = %#v, want the address gated on its own qualification", clients["2.2"])
	}
	gated.Start() // what the certification worker does when it starts
	_, err := gated.Validate(context.Background(), payload, profile)
	var unavailable *engine.CertificationLaneUnavailable
	if want := "certification validator unavailable: lane not qualified; qualification pending"; !errors.As(err, &unavailable) || err.Error() != want {
		t.Fatalf("before qualification: err = %v, want exactly %q", err, want)
	}
	if atomic.LoadInt32(&posts) != 0 {
		t.Fatal("an unqualified address was dialed")
	}

	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := gated.Validate(context.Background(), payload, profile)
		if err == nil {
			if !res.Valid || len(res.Issues) != 0 || len(res.Details) == 0 {
				t.Fatalf("after qualification: %+v, want the lane's settled clean answer", res)
			}
			break
		}
		if !errors.As(err, &unavailable) || time.Now().After(deadline) {
			t.Fatalf("never certified after qualification: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got, want := atomic.LoadInt32(&posts), int32(lanequalify.RowCount("2.2")+1); got != want {
		t.Fatalf("the lane got %d requests, want the %d-row corpus and one certification", got, want-1)
	}
}

// A 2.1 or 2.2 certification address is dialed only where evidence is
// collected. At none the gateway builds the gated client but starts no
// certification worker, so the address is never dialed, not even its
// metadata; at a collecting level the worker starts the client's qualification,
// whose first request is the metadata probe. The address records requests.
func TestCertificationAddressDialedOnlyWhereEvidenceIsCollected(t *testing.T) {
	unresolvedComposeLanes(t)
	for level, collects := range map[string]bool{"none": false, "observe": true} {
		t.Run(level, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			lane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, r.Method+" "+r.URL.Path)
				mu.Unlock()
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer lane.Close()
			seen := func() []string {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(asked)
			}
			b := buildForCertification(t, map[string]string{"CONFORMANCE_ENFORCEMENT": level, "FHIR_CERTIFY_URL_2_2": lane.URL + "/fhir"})
			if _, ok := b.certification["2.2"].(*engine.GatedCertificationValidator); !ok {
				t.Fatalf("2.2 client = %#v, want the address gated on its own qualification", b.certification["2.2"])
			}
			if !collects {
				time.Sleep(300 * time.Millisecond)
				if got := seen(); len(got) != 0 {
					t.Fatalf("at none the address was dialed: %v", got)
				}
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for len(seen()) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("at observe the address's qualification never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if got := seen()[0]; got != "GET /fhir/metadata" {
				t.Fatalf("first request %q, want the qualification's metadata probe", got)
			}
		})
	}
}
