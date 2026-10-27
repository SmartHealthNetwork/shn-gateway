package otelsetup

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-([45])[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// MON-R46: service.instance.id is a UUIDv5 of the host name, stable for a
// host and distinct across hosts, else a random UUIDv4 per boot.
func TestInstanceID_MON_R46(t *testing.T) {
	host := func(h string) func() (string, error) { return func() (string, error) { return h, nil } }
	a, a2, b := InstanceID(host("host-a")), InstanceID(host("host-a")), InstanceID(host("host-b"))
	if a != a2 || a == b {
		t.Fatalf("host-a %s %s, host-b %s: want stable per host and distinct across hosts", a, a2, b)
	}
	if m := uuidShape.FindStringSubmatch(a); m == nil || m[1] != "5" {
		t.Fatalf("%s is not a UUIDv5", a)
	}
	// Pinned: the namespace never changes, so a host keeps its id across
	// releases.
	if a != "4b8af6f6-d16e-5265-8251-307409f9c820" {
		t.Fatalf("host-a id %s changed: every replica's series would restart under a new id", a)
	}
	failing := func() (string, error) { return "", errors.New("no host name") }
	r1, r2 := InstanceID(failing), InstanceID(nil)
	if r1 == r2 {
		t.Fatal("random ids repeat")
	}
	for _, r := range []string{r1, r2, InstanceID(host(""))} {
		if m := uuidShape.FindStringSubmatch(r); m == nil || m[1] != "4" {
			t.Fatalf("%s is not a UUIDv4", r)
		}
	}
}

// MON-R5: the resource's precedence.
func TestResourceAttributes_MON_R5(t *testing.T) {
	get := func(attrs [][2]string, k string) (string, bool) {
		for _, kv := range attrs {
			if kv[0] == k {
				return kv[1], true
			}
		}
		return "", false
	}
	id := Identity{Version: "v1", HolderID: "h-own", Role: "provider"}
	for _, c := range []struct {
		name    string
		cfg     Config
		id      Identity
		service string
	}{
		{"default", Config{}, id, "shn-gateway"},
		{"metrics service", Config{}, Identity{Version: "v1", HolderID: "h-own", Role: "provider", MetricsService: "ms"}, "ms"},
		{"resource attribute over metrics service", Config{ResourceAttrs: map[string]string{"service.name": "ra"}}, Identity{MetricsService: "ms"}, "ra"},
		{"OTEL_SERVICE_NAME over all", Config{ServiceName: "osn", ResourceAttrs: map[string]string{"service.name": "ra"}}, Identity{MetricsService: "ms"}, "osn"},
	} {
		attrs := resourceAttributes(c.cfg, c.id, "inst-1")
		if v, _ := get(attrs, "service.name"); v != c.service {
			t.Errorf("%s: service.name %q, want %q", c.name, v, c.service)
		}
	}
	attrs := resourceAttributes(Config{}, id, "inst-1")
	if _, set := get(attrs, "deployment.environment.name"); set {
		t.Error("deployment.environment.name set without METRICS_ENV")
	}
	for k, want := range map[string]string{"service.version": "v1", "service.instance.id": "inst-1", "shn.holder_id": "h-own", "shn.role": "provider"} {
		if v, _ := get(attrs, k); v != want {
			t.Errorf("%s=%q, want %q", k, v, want)
		}
	}
	attrs = resourceAttributes(Config{ResourceAttrs: map[string]string{"service.instance.id": "pod-7", "deployment.environment.name": "ra-env"}},
		Identity{MetricsEnv: "metrics-env"}, "inst-1")
	if v, _ := get(attrs, "service.instance.id"); v != "pod-7" {
		t.Errorf("an operator's service.instance.id is kept: %q", v)
	}
	if v, _ := get(attrs, "deployment.environment.name"); v != "ra-env" {
		t.Errorf("an operator's deployment.environment.name is kept: %q", v)
	}
}

// MON-R46: the replica /internal/stats names is the service.instance.id the
// exported resource carries, an operator's OTEL_RESOURCE_ATTRIBUTES value
// included, and stays the same with OTLP export off.
func TestServiceInstanceIDIsTheResources_MON_R46(t *testing.T) {
	host := InstanceID(testIdentity.Hostname)
	for _, c := range []struct {
		name, attrs, want string
	}{
		{"the host's", "", host},
		{"an operator's", "service.instance.id=pod-7", "pod-7"},
	} {
		tel, _, _ := build(t, "http/protobuf", "http://127.0.0.1:1/v1/metrics", map[string]string{"OTEL_RESOURCE_ATTRIBUTES": c.attrs})
		var rm metricdata.ResourceMetrics
		if err := tel.reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		exported, _ := rm.Resource.Set().Value("service.instance.id")
		if got := tel.ServiceInstanceID(); got != c.want || exported.AsString() != c.want {
			t.Errorf("%s: ServiceInstanceID %q, exported resource %q, want %q", c.name, got, exported.AsString(), c.want)
		}
		tel.Discard()
		off := Build(Load(env(map[string]string{"OTEL_RESOURCE_ATTRIBUTES": c.attrs}), nil), testIdentity, Options{Logf: func(string, ...any) {}})
		if got := off.ServiceInstanceID(); got != c.want {
			t.Errorf("%s, OTLP off: ServiceInstanceID %q, want %q", c.name, got, c.want)
		}
	}
}
