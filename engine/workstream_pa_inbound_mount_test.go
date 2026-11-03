//go:build !shn_coreonly

package engine

import (
	"crypto/ed25519"
	"net/http"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const inboundPattern = "POST /substrate/inbound"

// mountConfig is a minimal gateway of role with no Hub transport key.
func mountConfig(t *testing.T, role string) Config {
	t.Helper()
	_, signPriv := genED25519(t)
	stub := newCensusSoR()
	cfg := Config{Role: role, HolderID: role, Identity: shnsdk.Identity{HolderID: role, SignPriv: signPriv}, SoR: stub, Store: stub}
	if role == "payer" {
		// A payer's gateway needs its content occupant.
		cfg.Responder = NewNativeResponder(http.DefaultClient, "http://payer.test", fixedClock)
	}
	return cfg
}

// A provider's gateway takes no leg the network delivers until its
// participant configures where the payer's attachment request goes: it
// declares none, mounts no inbound route and needs no Hub transport key.
func TestInboundMount_AProviderThatTakesNoLegMountsNone(t *testing.T) {
	if legs := AnsweredLegs("provider"); len(legs) != 0 {
		t.Fatalf("a provider declares %v, want none", legs)
	}
	g := mustNew(t, mountConfig(t, "provider"))
	if slices.Contains(g.RoutePatternsForTest(), inboundPattern) {
		t.Fatalf("a provider that takes no leg mounts %s", inboundPattern)
	}
}

// A role that takes a leg mounts the inbound route and needs the Hub's
// transport key, decided by the legs it declares (the legs' rows), never by
// the role's name: a provider whose catalog takes a leg mounts it too.
func TestInboundMount_ARoleThatTakesALegMountsItAndNeedsTheHubKey(t *testing.T) {
	check := func(t *testing.T, role string) {
		t.Helper()
		if len(AnsweredLegs(role)) == 0 {
			t.Fatalf("%s declares no leg", role)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("a %s gateway without the Hub transport key was built", role)
				}
			}()
			_, _ = New(mountConfig(t, role))
		}()
		cfg := mountConfig(t, role)
		pub, _ := genED25519(t)
		cfg.HubTransportPub = ed25519.PublicKey(pub)
		if !slices.Contains(mustNew(t, cfg).RoutePatternsForTest(), inboundPattern) {
			t.Errorf("a %s gateway that takes a leg does not mount %s", role, inboundPattern)
		}
	}
	for _, role := range []string{"payer", "facility", "phg"} {
		t.Run(role, func(t *testing.T) { check(t, role) })
	}
	t.Run("provider taking a leg", func(t *testing.T) {
		saved := workstreams
		t.Cleanup(func() { workstreams = saved })
		workstreams = append(slices.Clone(saved), toyWorkstream{tag: "provider-takes", legs: map[string]legSpec{"provider-takes-echo": {
			ReqFrame: "payer-payment", RespFrame: "provider-disclosure", Op: "provider-takes-ask", RespOp: "provider-takes-answer", Scope: "provider-takes-only", Physics: paReadOnly,
			RecipientRoles: []string{"provider"},
		}}})
		check(t, "provider")
	})
}

// A provider whose participant sets its own Task endpoint takes the payer's
// attachment request: it declares it, mounts the inbound route and needs the
// Hub's transport key. Unset, or blank, it takes none of that. Decided by the
// leg's setting (TakenWhenSet), never by the role's name.
func TestInboundMount_AProviderWithItsTaskEndpointTakesTheAttachmentRequest(t *testing.T) {
	configured := func(t *testing.T) Config {
		cfg := mountConfig(t, "provider")
		cfg.ProviderCDexTaskURL = "https://provider.test/fhir/Task"
		cfg.ProviderCDexTaskClient = http.DefaultClient
		cfg.IngressBaseURL = "https://provider-gw.test"
		EnableIngressForTest(&cfg)
		return cfg
	}
	if got := configured(t).TakenLegs(); !slices.Equal(got, []string{legCDexRequest}) {
		t.Fatalf("a provider with its Task endpoint takes %v, want [%s]", got, legCDexRequest)
	}
	for _, blank := range []string{"", "  "} {
		cfg := mountConfig(t, "provider")
		cfg.ProviderCDexTaskURL = blank
		if got := cfg.TakenLegs(); len(got) != 0 {
			t.Errorf("a provider with Task endpoint %q takes %v, want none", blank, got)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a provider taking the attachment request was built without the Hub transport key")
			}
		}()
		_, _ = New(configured(t))
	}()
	cfg := configured(t)
	pub, _ := genED25519(t)
	cfg.HubTransportPub = ed25519.PublicKey(pub)
	if !slices.Contains(mustNew(t, cfg).RoutePatternsForTest(), inboundPattern) {
		t.Errorf("a provider taking the attachment request does not mount %s", inboundPattern)
	}
	// A payer setting it takes nothing more: the leg is not its role's.
	payer := mountConfig(t, "payer")
	payer.ProviderCDexTaskURL = "https://provider.test/fhir/Task"
	if got := payer.TakenLegs(); slices.Contains(got, legCDexRequest) {
		t.Errorf("a payer with a Task endpoint takes %v", got)
	}
}

// The Task endpoint is a provider's own setting and its forward is
// authorized: set on another role, or without its client, the gateway does
// not boot.
func TestNew_AProviderTaskEndpointIsAProvidersAndAuthorized(t *testing.T) {
	for name, c := range map[string]struct {
		role   string
		client *http.Client
		want   string
	}{
		"on a payer":                 {"payer", http.DefaultClient, "is a provider's own Task endpoint; this gateway's role is \"payer\""},
		"on a facility":              {"facility", http.DefaultClient, "is a provider's own Task endpoint; this gateway's role is \"facility\""},
		"without a client":           {"provider", nil, "requires ProviderCDexTaskClient"},
		"without a return path base": {"provider", http.DefaultClient, "requires the Da Vinci ingress"},
		// The base set but the ingress that serves its return path not on:
		// every re-keyed address would name a route this gateway lacks.
		"a return path base without the ingress": {"provider", http.DefaultClient, "requires the Da Vinci ingress"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := mountConfig(t, c.role)
			cfg.ProviderCDexTaskURL = "https://provider.test/fhir/Task"
			cfg.ProviderCDexTaskClient = c.client
			switch name {
			case "without a return path base":
			case "a return path base without the ingress":
				cfg.IngressBaseURL = "https://provider-gw.test"
			default:
				cfg.IngressBaseURL = "https://provider-gw.test"
				EnableIngressForTest(&cfg)
			}
			pub, _ := genED25519(t)
			cfg.HubTransportPub = ed25519.PublicKey(pub)
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("New: %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// A setting this engine does not hold reads as unset: no leg is taken on a
// name it cannot read.
func TestSettingSet_AnUnknownSettingIsUnset(t *testing.T) {
	cfg := Config{Role: "provider", ProviderCDexTaskURL: "https://provider.test/fhir/Task"}
	if !cfg.settingSet(settingProviderCDexTaskURL) || cfg.settingSet("PROVIDER_SOMETHING_ELSE") || cfg.settingSet("") {
		t.Fatal("settingSet reads only the settings this engine holds")
	}
	for leg, setting := range LegsTakenWhenSet() {
		probe := Config{Role: "provider", ProviderCDexTaskURL: "x"}
		if !probe.settingSet(setting) {
			t.Errorf("%s's setting %q is one settingSet cannot read: the leg could never be taken", leg, setting)
		}
	}
}
