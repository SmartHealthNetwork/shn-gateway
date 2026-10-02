package engine

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// fhirServerTestAddress is the address WithFHIRServerTrustForTest resolves its
// one name to: a private address, which the default mode
// (FHIRServerReadPrivate) reads and the public mode refuses, as it would a
// server inside the participant's own network.
var fhirServerTestAddress = netip.MustParseAddr("10.255.0.1")

// fhirServerTestBinary reports whether the process is a test binary; the
// fhirServer test seam is available only there.
var fhirServerTestBinary = testing.Testing

// WithFHIRServerTrustForTest points the fhirServer read of the gateway cfg
// configures (CDS_FHIR_SERVER_READ, fhirserver_read.go) at one TLS test server
// standing in for an EHR's FHIR server, for a test outside this package (the
// root module's two-gateway rows):
//
//   - host, the name a request's fhirServer gives, resolves to a private
//     address (fhirServerTestAddress); every other name does not resolve, so
//     nothing else is read;
//   - TLS is verified, against roots alone (the test server's certificate):
//     never skipped, and never the system roots;
//   - dial makes the connection, to the test server, in place of the checked
//     address, after every address check has run.
//
// It changes nothing else the read does: the mode, its address and port checks,
// the redirect, proxy, size and time bounds, and what the answer must be.
// It panics outside a test binary, and when any argument is missing.
func WithFHIRServerTrustForTest(cfg *Config, host string, roots *x509.CertPool, dial func(ctx context.Context, network, address string) (net.Conn, error)) {
	if !fhirServerTestBinary() {
		panic("engine: WithFHIRServerTrustForTest is available only in tests")
	}
	if cfg == nil || host == "" || roots == nil || dial == nil {
		panic("engine: WithFHIRServerTrustForTest needs a config, a host, the server's roots and a dial")
	}
	cfg.fhirServerRoots = roots
	cfg.fhirServerResolve = func(_ context.Context, name string) ([]netip.Addr, error) {
		if !strings.EqualFold(strings.TrimSuffix(name, "."), host) {
			return nil, errors.New("engine: not the test fhirServer")
		}
		return []netip.Addr{fhirServerTestAddress}, nil
	}
	cfg.fhirServerDial = dial
}
