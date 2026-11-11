//go:build !shn_coreonly

package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// MON-R25: optionalURLs is the one table of endpoint settings, and every row
// names the endpoint kind its clients carry; the kind table has no row the
// settings table lacks.
func TestMONR25EveryOptionalURLHasAnEndpointKind(t *testing.T) {
	seen := map[string]bool{}
	for _, row := range optionalURLs(config{}) {
		seen[row[0]] = true
		tag, ok := settingTags[row[0]]
		if !ok {
			t.Errorf("setting %s has no endpoint kind in settingTags", row[0])
			continue
		}
		if !tag.Valid() {
			t.Errorf("setting %s has an invalid tag %v", row[0], tag)
		}
	}
	for name := range settingTags {
		if !seen[name] {
			t.Errorf("settingTags has %s, which is not an optionalURLs setting", name)
		}
	}
}

// MON-R25: every /internal/checks target a fully configured gateway builds
// gets a client with its endpoint's kind.
func TestMONR25EveryCheckTargetHasAnEndpointKind(t *testing.T) {
	u := func(name string) string { return "https://" + name + ".test" }
	cfg := config{
		AuthzURL: u("authz"), HubURL: u("hub"), ConsentURL: u("consent"),
		FHIRValidateURL: u("v20"), FHIRValidateURL21: u("v21"), FHIRValidateURL22: u("v22"),
		FHIRCertifyURL21: u("c21"), FHIRCertifyURL22: u("c22"),
		FHIRDataURL: u("fhir"), RegistrarURL: u("registrar"), FHIRTokenURL: u("fhir-token"),
		PayerDavinciBaseURL: u("payer"), PayerDavinciDTRBaseURL: u("dtr"), PayerDavinciPASBaseURL: u("pas"),
		PayerDavinciTokenURL: u("payer-token"), ProviderDTRPopulateURL: u("populate"),
		ProviderDTRPopulateTokenURL: u("populate-token"), ProviderDavinciIngressBaseURL: u("ingress"),
		PayerDavinciIngressBaseURL: u("payer-ingress"),
		ProviderCDexTaskURL:        u("cdex-task"), ProviderCDexTokenURL: u("cdex-task-token"),
		DiscoveryURL: u("discovery"), AuthzPubkeyURL: u("authz-key"), HubTransportKeyURL: u("hub-key"),
	}
	for _, row := range optionalURLs(cfg) {
		if row[1] == "" {
			t.Fatalf("the test config leaves %s unset", row[0])
		}
	}
	targets := append(checkTargets(cfg), auditCheckTarget(u("audit")))
	payer := cfg
	payer.Role = "payer"
	cds, ok := payerCDSCheckTarget(payer, shnsdk.NewRegistry(), "")
	if !ok {
		t.Fatal("a forwarding payer has no CDS service listing check")
	}
	targets = append(targets, cds)
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		targets = append(targets, checks.Target{ID: "validator-lane-" + line, Kind: checks.KindValidatorLane})
	}
	// The ruling, pinned here rather than read back from settingTags: one
	// kind per setting that can name a host of its own.
	of := endpointcert.Of
	want := map[string]endpointcert.Tag{
		"AUTHZ_URL": of(endpointcert.Authorization), "HUB_URL": of(endpointcert.Hub), "CONSENT_URL": of(endpointcert.Consent),
		"FHIR_VALIDATE_URL": validatorLane("2.0"), "FHIR_VALIDATE_URL_2_1": validatorLane("2.1"), "FHIR_VALIDATE_URL_2_2": validatorLane("2.2"),
		"FHIR_CERTIFY_URL_2_1": certifyLane("2.1"), "FHIR_CERTIFY_URL_2_2": certifyLane("2.2"),
		"FHIR_DATA_URL": of(endpointcert.FHIR), "REGISTRAR_URL": of(endpointcert.Registrar), "FHIR_TOKEN_URL": of(endpointcert.FHIRToken),
		"PAYER_DAVINCI_BASE_URL": of(endpointcert.PayerDavinci), "PAYER_DAVINCI_DTR_BASE_URL": of(endpointcert.PayerDTR),
		"PAYER_DAVINCI_PAS_BASE_URL": of(endpointcert.PayerPAS), "PAYER_DAVINCI_TOKEN_URL": of(endpointcert.PayerToken),
		"PROVIDER_DTR_POPULATE_URL": of(endpointcert.Populate), "PROVIDER_DTR_POPULATE_TOKEN_URL": of(endpointcert.PopulateToken),
		"PROVIDER_CDEX_TASK_URL": of(endpointcert.CDexTask), "PROVIDER_CDEX_TOKEN_URL": of(endpointcert.CDexTaskToken),
		"PROVIDER_DAVINCI_INGRESS_BASE_URL": of(endpointcert.Ingress), "PAYER_DAVINCI_INGRESS_BASE_URL": of(endpointcert.Ingress),
		"SHN_DISCOVERY_URL": of(endpointcert.Discovery),
		"AUTHZ_PUBKEY_URL":  of(endpointcert.AuthorizationKey), "HUB_TRANSPORT_KEY_URL": of(endpointcert.HubKey),
		"AUDIT_ENDPOINT": of(endpointcert.Audit), "PAYER_DAVINCI_WELL_KNOWN": of(endpointcert.PayerDavinci),
		payerCDSCheckID:      of(endpointcert.CDS),
		"validator-lane-2.0": validatorLane("2.0"), "validator-lane-2.1": validatorLane("2.1"), "validator-lane-2.2": validatorLane("2.2"),
	}
	rec := endpointcert.New()
	for _, tgt := range tagCheckTargets(rec, targets, shnsdk.NewClient()) {
		if tgt.Client == nil {
			t.Errorf("check target %s has no tagged client", tgt.ID)
			continue
		}
		base, tag, ok := endpointcert.Unwrap(tgt.Client.Transport)
		// A network service's or sidecar's probe follows no redirect at
		// all: the policy sits directly beneath the tag.
		if followsNoRedirect(tag.Kind()) {
			inner, policed := serviceclient.Base(base)
			if !policed {
				t.Errorf("check target %s's client is not under the network-service redirect policy", tgt.ID)
			}
			base = inner
		}
		if w, known := want[tgt.ID]; !ok || !known || tag != w || base != nil {
			t.Errorf("check target %s's client is tagged %v (%v) over %T, want %v over the runner's transport", tgt.ID, tag, ok, base, w)
		}
		if followsNoRedirect(tag.Kind()) {
			continue
		}
		// Every participant target's client is under the participant
		// redirect policy: a hop from https to http is refused before it is
		// sent.
		https, _ := url.Parse("https://system.example/metadata")
		plain, _ := url.Parse("http://system.example/metadata")
		if tgt.Client.CheckRedirect == nil || !errors.Is(tgt.Client.CheckRedirect(&http.Request{URL: plain, Header: http.Header{}}, []*http.Request{{URL: https}}), smartauth.ErrCleartextRedirect) {
			t.Errorf("check target %s's client follows a redirect from https to http", tgt.ID)
		}
	}
}

// MON-R25: the store's TLS configs (the DSN's and each fallback's) carry the
// store kind's hook after their own verification; a plaintext DSN has none.
func TestMONR25StoreTLSConfigsCarryTheStoreHook(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@db.test:5432/x?sslmode=verify-full",
		"postgres://u:p@db.test:5432/x?sslmode=prefer",
	} {
		cc, err := pgconn.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		rec := endpointcert.New()
		tagStoreTLS(rec, cc)
		configs := []*tls.Config{cc.TLSConfig}
		for _, fb := range cc.Fallbacks {
			configs = append(configs, fb.TLSConfig)
		}
		hooked := 0
		for _, c := range configs {
			if c == nil {
				continue
			}
			if c.VerifyConnection == nil {
				t.Fatalf("%s: a TLS config has no hook", dsn)
			}
			if err := c.VerifyConnection(tls.ConnectionState{ServerName: "db.test", PeerCertificates: testLeaf()}); err != nil {
				t.Fatalf("%s: hook = %v", dsn, err)
			}
			hooked++
		}
		if hooked == 0 {
			t.Fatalf("%s: no TLS config hooked", dsn)
		}
		if _, ok := rec.Soonest(endpointcert.Of(endpointcert.Store), endpointcert.Server); !ok {
			t.Fatalf("%s: the store's certificate was not recorded", dsn)
		}
	}
	cc, err := pgconn.ParseConfig("postgres://u:p@db.test:5432/x?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	tagStoreTLS(endpointcert.New(), cc)
	if cc.TLSConfig != nil {
		t.Fatal("a plaintext DSN gained a TLS config")
	}
	tagStoreTLS(nil, cc) // must not panic
}

// MON-R25: build() tags every outbound kind it is configured for, and each
// consumer's client keeps the very transport it used before this change, only
// Wrapped: the shared client's consumers keep http.DefaultTransport (a nil
// transport, read at each request), so no connection or pool changes; a
// certification client keeps its own bounded transport.
func TestMONR25BuildTagsEachConsumerOverItsOwnTransport(t *testing.T) {
	b := buildForKinds(t, "payer", payerKindsEnv)
	got := b.certs.Made()
	want := []endpointcert.Tag{
		endpointcert.Of(endpointcert.FHIR), endpointcert.Of(endpointcert.FHIRToken),
		endpointcert.Of(endpointcert.PayerDavinci), endpointcert.Of(endpointcert.PayerDTR), endpointcert.Of(endpointcert.PayerPAS),
		endpointcert.Of(endpointcert.PayerEligibility), endpointcert.Of(endpointcert.PayerToken), endpointcert.Of(endpointcert.CDS),
		endpointcert.Of(endpointcert.Discovery), endpointcert.Of(endpointcert.Hub), endpointcert.Of(endpointcert.HubKey),
		endpointcert.Of(endpointcert.Authorization), endpointcert.Of(endpointcert.AuthorizationKey),
		endpointcert.Of(endpointcert.Registrar), endpointcert.Of(endpointcert.Consent), endpointcert.Of(endpointcert.Audit),
		validatorLane("2.0"), validatorLane("2.1"), validatorLane("2.2"),
		certifyLane("2.0"), certifyLane("2.1"), certifyLane("2.2"),
		endpointcert.Of(endpointcert.Diagnostic),
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("build tagged no %v client; tagged %v", w, got)
		}
	}

	// The consumers' own clients, each with its kind over the transport they used before
	// transport (nil: http.DefaultTransport) and the shared timeout.
	hub, authz, consent := b.gateway.OutboundClientsForTest()
	for _, c := range []struct {
		name   string
		client *http.Client
		want   endpointcert.Tag
	}{
		{"engine Client (Hub)", hub, endpointcert.Of(endpointcert.Hub)},
		{"engine AuthzClient", authz, endpointcert.Of(endpointcert.Authorization)},
		{"engine ConsentClient", consent, endpointcert.Of(endpointcert.Consent)},
		{"audit appender", b.auditAppender.ClientForTest(), endpointcert.Of(endpointcert.Audit)},
		{"registrar poller", b.client, endpointcert.Of(endpointcert.Registrar)},
		{"diagnostic publisher", b.diagnostic.publisher.Client, endpointcert.Of(endpointcert.Diagnostic)},
	} {
		base, tag, ok := endpointcert.Unwrap(c.client.Transport)
		if !ok || tag != c.want {
			t.Errorf("%s: tagged %v (%v), want %v", c.name, tag, ok, c.want)
		}
		// A network service's client follows no redirect: the policy sits
		// directly beneath the tag, over the transport it used before.
		if c.name != "diagnostic publisher" {
			inner, policed := serviceclient.Base(base)
			if !policed {
				t.Errorf("%s: transport %T is not under the network-service redirect policy", c.name, base)
			}
			base = inner
		}
		if base != nil {
			t.Errorf("%s: transport %T, want the transport it used before: http.DefaultTransport (nil)", c.name, base)
		}
		// A copy of the client used before: its Timeout and CheckRedirect as they
		// were (the shared client's: 30 s, none; the sink's: none, its own).
		if c.name == "diagnostic publisher" {
			if c.client.Timeout != 0 || c.client.CheckRedirect == nil {
				t.Errorf("%s: timeout %v, redirect policy set %v; want the sink's own (0, its own policy)", c.name, c.client.Timeout, c.client.CheckRedirect != nil)
			}
		} else if c.client.Timeout != shnsdk.NewClient().Timeout || c.client.CheckRedirect != nil {
			t.Errorf("%s: timeout %v, redirect policy set %v; want the shared client's (30 s, none)", c.name, c.client.Timeout, c.client.CheckRedirect != nil)
		}
	}
	for line, v := range b.certification {
		ov, ok := v.(*shnsdk.OperationValidator)
		if g, gated := v.(interface {
			CertificationClient() *shnsdk.OperationValidator
		}); gated {
			ov, ok = g.CertificationClient(), true
		}
		if !ok {
			t.Errorf("certification %s: %T", line, v)
			continue
		}
		base, tag, ok := endpointcert.Unwrap(certificationInner(t, ov))
		tr, bounded := base.(*http.Transport)
		if !ok || tag != certifyLane(line) || !bounded || tr.MaxConnsPerHost != 1 {
			t.Errorf("certification %s: inner %T tagged %v (%v); want its own bounded transport as certify %s", line, base, tag, ok, line)
		}
	}
}

// certificationInner is a certification client's inner transport.
func certificationInner(t *testing.T, v *shnsdk.OperationValidator) http.RoundTripper {
	t.Helper()
	inner, ok := engine.CertificationInnerForTest(v)
	if !ok || inner == nil {
		t.Fatalf("certification transport %T has no inner transport", v.Client.Transport)
	}
	return inner
}

func testLeaf() []*x509.Certificate {
	return []*x509.Certificate{{NotAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), DNSNames: []string{"db.test"}}}
}

// MON-R25: a provider that populates through its own $populate endpoint
// tags that client and its token client with their settings' kinds.
func TestMONR25ProviderBuildTagsItsPopulateClients(t *testing.T) {
	b := buildForKinds(t, "provider", func(u string) map[string]string {
		return map[string]string{
			"FHIR_DATA_URL":                       u + "/fhir",
			"PROVIDER_DTR_NATIVE":                 "true",
			"PROVIDER_DTR_POPULATE_URL":           u + "/populate",
			"PROVIDER_DTR_POPULATE_TOKEN_URL":     u + "/populate-token",
			"PROVIDER_DTR_POPULATE_CLIENT_ID":     "populate-client",
			"PROVIDER_DTR_POPULATE_CLIENT_SECRET": "populate-secret",
		}
	})
	got := b.certs.Made()
	for _, w := range []endpointcert.Tag{endpointcert.Of(endpointcert.Populate), endpointcert.Of(endpointcert.PopulateToken)} {
		if !slices.Contains(got, w) {
			t.Errorf("build made no %v transport; made %v", w, got)
		}
	}
}

// payerKindsEnv configures a payer for every outbound kind a payer build
// makes, each setting naming the local fixture u.
func payerKindsEnv(u string) map[string]string {
	return map[string]string{
		"AUTHZ_URL":                    u + "/authz",
		"HUB_URL":                      u + "/hub",
		"CONSENT_URL":                  u + "/consent",
		"FHIR_DATA_URL":                u + "/fhir",
		"FHIR_TOKEN_URL":               u + "/fhir-token",
		"FHIR_CLIENT_ID":               "fhir-client",
		"FHIR_CLIENT_SECRET":           "fhir-secret",
		"PAYER_DAVINCI_BASE_URL":       u + "/payer",
		"PAYER_DAVINCI_CDS_BASE_URL":   u + "/cds",
		"PAYER_DAVINCI_DTR_BASE_URL":   u + "/dtr",
		"PAYER_DAVINCI_PAS_BASE_URL":   u + "/pas",
		"PAYER_DAVINCI_TOKEN_URL":      u + "/payer-token",
		"PAYER_DAVINCI_CLIENT_ID":      "payer-client",
		"PAYER_DAVINCI_CLIENT_SECRET":  "payer-secret",
		"PAYER_DAVINCI_CRD_SERVICE_ID": "svc",
		"PAYER_ELIGIBILITY_URL":        u + "/eligibility",
		"FHIR_VALIDATE_URL":            u + "/v20",
		"FHIR_VALIDATE_URL_2_1":        u + "/v21",
		"FHIR_VALIDATE_URL_2_2":        u + "/v22",
		"FHIR_CERTIFY_URL_2_1":         u + "/c21",
		"FHIR_CERTIFY_URL_2_2":         u + "/c22",
	}
}

// buildForKinds builds a gateway of role with extra's settings, the
// diagnostic sink and no default validator lanes: each setting names a
// local fixture, so nothing leaves the test. A live database and a mounted
// ingress are left out (tagStoreTLS and the checks rows cover them).
func buildForKinds(t *testing.T, role string, extra func(u string) map[string]string) built {
	t.Helper()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ok.Close)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	t.Cleanup(keys.Close)
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{`+acceptingWire+`"endpoints":{"audit":%q},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, ok.URL+"/audit", keys.URL, keys.URL)
	}))
	t.Cleanup(disc.Close)

	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-test-" + role + "-kinds")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, role, "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "diag.key")
	if err := os.WriteFile(keyFile, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ROLE":                         role,
		"SHN_SECRETS":                  dir,
		"SHN_DISCOVERY_URL":            disc.URL,
		"FHIR_DEFAULT_VALIDATOR_LANES": "none",
		"DIAGNOSTIC_SINK_URL":          ok.URL + "/diag/events",
		"DIAGNOSTIC_SOURCE":            role + "-kinds",
		"DIAGNOSTIC_KEY_FILE":          keyFile,
	}
	for k, v := range extra(ok.URL) {
		env[k] = v
	}
	b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() {
		if b.gateway != nil {
			_ = b.gateway.Close()
		}
		b.lanes.Close()
	})
	return b
}

// MON-R25: with diagnostic capture on, the SMART token client keeps the
// token kind tagSmart gave it beneath the capture: the token endpoint's
// certificate is still recorded as fhir-token.
func TestMONR25DiagnosticCaptureKeepsTheTaggedTokenClient(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"t","token_type":"bearer","expires_in":300}`)
	}))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })

	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-observation-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"DIAGNOSTIC_SINK_URL": "http://collector.test/events", "DIAGNOSTIC_SOURCE": "test-provider", "DIAGNOSTIC_KEY_FILE": key}
	d := newDiagnosticSource(func(k string) string { return env[k] }, "provider", io.Discard, time.Now)
	if d == nil {
		t.Fatal("no diagnostic source")
	}
	rec := endpointcert.New()
	sc := smartauth.Config{TokenURL: srv.URL + "/token", ClientID: "c", ClientSecret: "s"}
	tagSmart(rec, &sc, rec.Wrap(endpointcert.Of(endpointcert.FHIR), nil), endpointcert.FHIRToken)
	d.smart(&sc)
	if _, err := (&smartauth.TokenSource{Config: sc}).Token(t.Context()); err != nil {
		t.Fatalf("token: %v", err)
	}
	if _, ok := rec.Soonest(endpointcert.Of(endpointcert.FHIRToken), endpointcert.Server); !ok {
		t.Fatal("the token endpoint's certificate was not recorded as fhir-token under diagnostic capture")
	}
}

// MON-R25: the one qualifier build() hands routing and certification reads
// the lane's metadata through a client tagged for the side its context names
// (the engine marks each attempt), and records nothing when it names none.
func TestMONR25LaneQualifierRecordsTheSideItsAttemptIsFor(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"resourceType":"Bundle"}`) // not a CapabilityStatement: the qualification stops after the read
	}))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })

	rec := endpointcert.New()
	q := laneQualifier(rec)
	for _, c := range []struct {
		ctx  context.Context
		line string
		want endpointcert.Tag
	}{
		{endpointcert.WithTag(t.Context(), certifyLane("2.2")), "2.2", certifyLane("2.2")},
		{endpointcert.WithTag(t.Context(), validatorLane("2.1")), "2.1", validatorLane("2.1")},
	} {
		if err := q(c.ctx, srv.URL, c.line); err == nil {
			t.Fatalf("%v: a lane answering a Bundle qualified", c.want)
		}
		if na, ok := rec.Soonest(c.want, endpointcert.Server); !ok || !na.Equal(srv.Certificate().NotAfter) {
			t.Fatalf("%v = %v, %v; the qualification's read was not recorded under its side", c.want, na, ok)
		}
	}
	// An attempt that names no lane, or a kind no lane has, is recorded under
	// none: not that kind, and not folded into a validator lane.
	for _, ctx := range []context.Context{t.Context(), endpointcert.WithTag(t.Context(), endpointcert.Of(endpointcert.Hub))} {
		stray := endpointcert.New()
		if err := laneQualifier(stray)(ctx, srv.URL, "2.2"); err == nil {
			t.Fatal("a lane answering a Bundle qualified")
		}
		if got := stray.Readings(); len(got) != 0 {
			t.Fatalf("an attempt with no lane mark was recorded: %v", got)
		}
	}
	if reflect.ValueOf(laneQualifier(nil)).Pointer() != reflect.ValueOf(engine.LaneQualifier(qualifyDefaultLane)).Pointer() {
		t.Fatal("with no recorder the qualifier is not qualifyDefaultLane")
	}
}

// MON-R25: the routing validators each declared line resolves to carry the
// validator kind for that line.
func TestMONR25RoutingValidatorsCarryTheirLine(t *testing.T) {
	cfg := config{certs: endpointcert.New(), FHIRValidateURL21: "https://v21.test/fhir", FHIRValidateURL22: "https://v22.test/fhir", ConformanceEnforcement: engine.EnforcementObserve}
	declared := []string{"pa.pas@2.1", "pa.pas@2.2"}
	lanes, err := validatorLanesForDeclared(func(string) string { return "" }, declared, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"2.1", "2.2"} {
		v, ok := lanes[line].(*shnsdk.OperationValidator)
		if !ok {
			t.Fatalf("line %s: %T", line, lanes[line])
		}
		base, tag, ok := endpointcert.Unwrap(v.Client.Transport)
		inner, policed := serviceclient.Base(base)
		if !ok || tag != validatorLane(line) || !policed || inner != nil {
			t.Fatalf("line %s validator tagged %v (%v) over %T (under the redirect policy %v)", line, tag, ok, base, policed)
		}
	}
}

// MON-R25: a payer forwarding unauthenticated sends through one client over
// the transport it used before (http.DefaultTransport, one pool), recording under
// payer-davinci and each per-operation setting's kind the engine marks.
func TestMONR25UnauthenticatedPayerForwardsOverOneTransport(t *testing.T) {
	b := buildForKinds(t, "payer", func(u string) map[string]string {
		return map[string]string{
			"FHIR_DATA_URL":                u + "/fhir",
			"PAYER_DAVINCI_BASE_URL":       u + "/payer",
			"PAYER_DAVINCI_CRD_SERVICE_ID": "svc",
			"DIAGNOSTIC_SINK_URL":          "", "DIAGNOSTIC_SOURCE": "", "DIAGNOSTIC_KEY_FILE": "",
		}
	})
	n, ok := b.nativeResponder.(interface{ ClientForTest() *http.Client })
	if !ok {
		t.Fatalf("native responder %T", b.nativeResponder)
	}
	base, tag, ok := endpointcert.Unwrap(n.ClientForTest().Transport)
	if !ok || tag != endpointcert.Of(endpointcert.PayerDavinci) || base != nil {
		t.Fatalf("payer client tagged %v (%v) over %T; want payer-davinci over http.DefaultTransport", tag, ok, base)
	}
	of := endpointcert.Of
	for _, w := range []endpointcert.Tag{of(endpointcert.PayerDTR), of(endpointcert.PayerPAS), of(endpointcert.PayerEligibility), of(endpointcert.CDS)} {
		if !slices.Contains(b.certs.Made(), w) {
			t.Errorf("the payer client does not record %v", w)
		}
	}
	if n.ClientForTest().Timeout != shnsdk.NewClient().Timeout {
		t.Fatalf("payer client timeout %v", n.ClientForTest().Timeout)
	}
}

// MON-R25: the ingress certificate TLS_CERT_FILE names is recorded as the
// gateway's own when it is loaded.
func TestMONR25IngressCertificateIsRecordedAsSelf(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.StartTLS()
	defer srv.Close()
	cert := srv.TLS.Certificates[0]
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	b := buildForKinds(t, "provider", func(u string) map[string]string {
		return map[string]string{"FHIR_DATA_URL": u + "/fhir", "TLS_CERT_FILE": certFile, "TLS_KEY_FILE": keyFile}
	})
	na, ok := b.certs.Soonest(endpointcert.Of(endpointcert.Ingress), endpointcert.Self)
	if !ok || !na.Equal(srv.Certificate().NotAfter) {
		t.Fatalf("ingress self = %v, %v; want TLS_CERT_FILE's NotAfter", na, ok)
	}
	var none *endpointcert.Recorder
	none.RecordOwn(endpointcert.Ingress, "TLS_CERT_FILE", &cert) // must not panic
	b.certs.RecordOwn(endpointcert.Ingress, "TLS_CERT_FILE", nil)
}

// MON-R25: certification takes routing's qualification pass on a default
// lane only from the same qualifier value (engine's qualifierID). build()
// makes one laneQualifier and hands that one variable to routing and to
// certification: two calls of laneQualifier would be two functions once
// inlined, and every default lane would be qualified twice.
func TestMONR25BuildHandsOneQualifierToRoutingAndCertification(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "app.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var build *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "build" && fd.Recv == nil {
			build = fd
		}
	}
	if build == nil {
		t.Fatal("no build func in app.go")
	}
	lastArg := map[string][]string{}
	made := 0
	ast.Inspect(build, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "discoverValidatorLanes", "certificationValidators":
			arg := "a non-variable"
			if a, ok := call.Args[len(call.Args)-1].(*ast.Ident); ok {
				arg = a.Name
			}
			lastArg[id.Name] = append(lastArg[id.Name], arg)
		case "laneQualifier":
			made++
		}
		return true
	})
	routing, certification := lastArg["discoverValidatorLanes"], lastArg["certificationValidators"]
	if made != 1 || len(routing) != 1 || len(certification) != 1 || routing[0] != certification[0] || routing[0] == "a non-variable" {
		t.Fatalf("build makes %d laneQualifier(s); routing gets %v, certification %v: want one value, the same variable to both", made, routing, certification)
	}
}

// MON-R25: each SMART client build() makes records its system's kind for
// the operations and its token kind for the token requests, the token
// fetched under the operation's mark included; the payer's records each
// marked operation under that operation's kind.
func TestMONR25SMARTClientsRecordTheirOperationAndTokenKinds(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"t","token_type":"bearer","expires_in":300}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })

	of := endpointcert.Of
	for _, c := range []struct {
		name       string
		cfg        func(config) config
		build      func(config) (*http.Client, error)
		mark       endpointcert.Tag
		ops, token endpointcert.Tag
	}{
		{"fhir", func(c config) config {
			c.FHIRTokenURL, c.FHIRClientID, c.FHIRClientSecret = srv.URL+"/token", "c", "s"
			return c
		}, fhirHTTPClient, endpointcert.Tag{}, of(endpointcert.FHIR), of(endpointcert.FHIRToken)},
		{"payer, a PAS forward", func(c config) config {
			c.PayerDavinciTokenURL, c.PayerDavinciClientID, c.PayerDavinciClientSecret = srv.URL+"/token", "c", "s"
			return c
		}, payerDavinciHTTPClient, of(endpointcert.PayerPAS), of(endpointcert.PayerPAS), of(endpointcert.PayerToken)},
		{"populate", func(c config) config {
			c.ProviderDTRPopulateTokenURL, c.ProviderDTRPopulateClientID, c.ProviderDTRPopulateClientSecret = srv.URL+"/token", "c", "s"
			return c
		}, providerDTRPopulateHTTPClient, endpointcert.Tag{}, of(endpointcert.Populate), of(endpointcert.PopulateToken)},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg(config{certs: endpointcert.New()})
			hc, err := c.build(cfg)
			if err != nil || hc == nil {
				t.Fatalf("build: %v", err)
			}
			ctx := t.Context()
			if c.mark.Valid() {
				ctx = endpointcert.WithTag(ctx, c.mark)
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/op", nil)
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			got := cfg.certs.Readings()
			if len(got) != 2 {
				t.Fatalf("readings %v; want exactly %v and %v", got, c.ops, c.token)
			}
			for _, want := range []endpointcert.Tag{c.ops, c.token} {
				if _, ok := cfg.certs.Soonest(want, endpointcert.Server); !ok {
					t.Errorf("%v not recorded; readings %v", want, got)
				}
			}
		})
	}
}

// MON-R25: boot reads discovery and the two trust-anchor keys each through
// a client of its own setting's kind, so a key served from a host of its
// own is recorded as that key.
func TestMONR25BootRecordsDiscoveryAndEachKeySeparately(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	roots := x509.NewCertPool()
	// Each server has a certificate of its own, with its own NotAfter, so a
	// key read through another setting's client is told apart.
	// Far enough ahead that the real clock TLS verifies against never passes it.
	base := time.Date(2090, 3, 1, 0, 0, 0, 0, time.UTC)
	serve := func(notAfter time.Time, h http.HandlerFunc) *httptest.Server {
		t.Helper()
		srv := httptest.NewUnstartedServer(h)
		cert, leaf := selfSigned(t, notAfter)
		roots.AddCert(leaf)
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		srv.StartTLS()
		t.Cleanup(srv.Close)
		return srv
	}
	authzKey := serve(base.Add(24*time.Hour), func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, keyBody) })
	hubKey := serve(base.Add(48*time.Hour), func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, keyBody) })
	disc := serve(base, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{`+acceptingWire+`"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, authzKey.URL, hubKey.URL)
	})
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: roots}
	old := http.DefaultTransport
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = old })

	cfg := config{certs: endpointcert.New(), DiscoveryURL: disc.URL}
	if _, _, err := resolveDiscovery(t.Context(), shnsdk.NewClient(), cfg); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[endpointcert.Kind]time.Time{endpointcert.Discovery: base, endpointcert.AuthorizationKey: base.Add(24 * time.Hour), endpointcert.HubKey: base.Add(48 * time.Hour)} {
		na, ok := cfg.certs.Soonest(endpointcert.Of(kind), endpointcert.Server)
		if !ok || !na.Equal(want) {
			t.Errorf("%v = %v, %v; want its own server's certificate (%v)", kind, na, ok, want)
		}
	}
	if n := len(cfg.certs.Readings()); n != 3 {
		t.Fatalf("%d readings, want discovery and the two keys", n)
	}
}

// selfSigned is a P-256 certificate for the loopback addresses, valid from
// 2020 until notAfter.
func selfSigned(t *testing.T, notAfter time.Time) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(notAfter.Unix()),
		NotBefore:    time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: notAfter,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

// MON-R25: build hands the exporters the gateway's recorder, so the pull
// listener's own certificate is recorded under metrics-pull when it loads.
func TestMONR25BuildRecordsTheMetricsListenersCertificate(t *testing.T) {
	cert, _ := selfSigned(t, time.Date(2090, 3, 1, 0, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "pull.pem"), filepath.Join(dir, "pull-key.pem")
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := buildMonitored(t, monitoringGateway(t, map[string]string{"OTEL_METRICS_EXPORTER": "prometheus",
		"METRICS_PULL_TLS_CERT_FILE": certFile, "METRICS_PULL_TLS_KEY_FILE": keyFile}))
	na, ok := b.certs.Soonest(endpointcert.Of(endpointcert.MetricsPull), endpointcert.Self)
	if !ok || !na.Equal(time.Date(2090, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("metrics-pull self = %v, %v; want the listener's certificate", na, ok)
	}
}
