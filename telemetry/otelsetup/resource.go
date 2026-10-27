package otelsetup

import (
	"crypto/rand"
	"crypto/sha1"
	"fmt"
	"sort"
)

// Identity is what the gateway knows about itself for the resource (MON-R5).
type Identity struct {
	// Version is the version /health reports.
	Version string
	// HolderID is the gateway's own holder id, the only holder id any
	// export carries (MON-R19).
	HolderID string
	Role     string
	// MetricsService and MetricsEnv are METRICS_SERVICE and METRICS_ENV as
	// set (empty when unset).
	MetricsService string
	MetricsEnv     string
	// Hostname returns the host name for service.instance.id; nil or an
	// error selects a random id per boot.
	Hostname func() (string, error)
}

// instanceNamespace is the UUID namespace of service.instance.id's UUIDv5 of
// the host name (MON-R46). Fixed: a host keeps its id across restarts.
var instanceNamespace = [16]byte{0x7d, 0x3a, 0x51, 0x0e, 0x2c, 0x4f, 0x4b, 0x8a, 0x9e, 0x61, 0x0d, 0x5b, 0x83, 0x2f, 0xc4, 0x17}

// InstanceID is service.instance.id: a UUIDv5 of the host name under the
// gateway's namespace, stable for a host, else a random UUIDv4 per boot
// (MON-R46).
func InstanceID(hostname func() (string, error)) string {
	if hostname != nil {
		if h, err := hostname(); err == nil && h != "" {
			return uuidV5(instanceNamespace, h)
		}
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return formatUUID(b)
}

func uuidV5(ns [16]byte, name string) string {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	var b [16]byte
	copy(b[:], h.Sum(nil))
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// resourceAttributes are the resource's attributes, sorted by key (MON-R5):
// service.name from OTEL_SERVICE_NAME, else OTEL_RESOURCE_ATTRIBUTES'
// service.name, else METRICS_SERVICE, else shn-gateway; service.version;
// service.instance.id; deployment.environment.name from METRICS_ENV when set;
// shn.holder_id; shn.role; then the rest of OTEL_RESOURCE_ATTRIBUTES, which
// can set neither service.version nor any shn.* key (Load drops those).
func resourceAttributes(c Config, id Identity, instanceID string) [][2]string {
	attrs := map[string]string{}
	for k, v := range c.ResourceAttrs {
		attrs[k] = v
	}
	name := c.ServiceName
	if name == "" {
		name = attrs["service.name"]
	}
	if name == "" {
		name = id.MetricsService
	}
	if name == "" {
		name = "shn-gateway"
	}
	attrs["service.name"] = name
	attrs["service.version"] = id.Version
	if _, set := attrs["service.instance.id"]; !set {
		attrs["service.instance.id"] = instanceID
	}
	if _, set := attrs["deployment.environment.name"]; !set && id.MetricsEnv != "" {
		attrs["deployment.environment.name"] = id.MetricsEnv
	}
	attrs["shn.holder_id"] = id.HolderID
	attrs["shn.role"] = id.Role
	out := make([][2]string, 0, len(attrs))
	for k, v := range attrs {
		out = append(out, [2]string{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
