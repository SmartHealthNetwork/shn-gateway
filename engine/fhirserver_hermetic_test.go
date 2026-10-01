package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
)

// liveFHIRServer records every read that reached the real network: a test
// that names a fhirServer supplies its own resolver and dial, so the run
// fails if any read got past them to the system's.
var liveFHIRServer struct {
	mu   sync.Mutex
	hits []string
}

func noteLiveFHIRServer(what string) error {
	liveFHIRServer.mu.Lock()
	defer liveFHIRServer.mu.Unlock()
	liveFHIRServer.hits = append(liveFHIRServer.hits, what)
	return errors.New("no live network in tests: " + what)
}

// TestMain replaces the fhirServer read's system resolver and dial for every
// test in the package, and fails the run when any read used them.
func TestMain(m *testing.M) {
	fhirServerSystemResolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		return nil, noteLiveFHIRServer("resolve " + host)
	}
	fhirServerSystemDial = func(_ context.Context, _ *net.Dialer, _, address string) (net.Conn, error) {
		return nil, noteLiveFHIRServer("dial " + address)
	}
	code := m.Run()
	if hits := takeLiveFHIRServer(); len(hits) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: a test read a fhirServer through the real network (give it a resolver and dial): %v\n", hits)
		code = 1
	}
	os.Exit(code)
}

func takeLiveFHIRServer() []string {
	liveFHIRServer.mu.Lock()
	defer liveFHIRServer.mu.Unlock()
	hits := liveFHIRServer.hits
	liveFHIRServer.hits = nil
	return hits
}

// The guard itself: a read given no resolver or dial reaches the system's,
// which the run records. This test removes only its own two records: any a
// test before it left stay for TestMain to fail the run on.
func TestFHIRServerReadNeverReachesTheRealNetworkInTests(t *testing.T) {
	earlier := takeLiveFHIRServer()
	defer func() {
		liveFHIRServer.mu.Lock()
		liveFHIRServer.hits = append(earlier, liveFHIRServer.hits...)
		liveFHIRServer.mu.Unlock()
	}()
	r := fhirServerReader{mode: FHIRServerReadPrivate}
	if _, _, _, status, _ := r.read(context.Background(), fhirServerRead{base: "https://ehr.example/fhir", patient: "p"}); status != fhirServerRefused {
		t.Fatalf("status %d", status)
	}
	if _, _, _, status, _ := r.read(context.Background(), fhirServerRead{base: "https://10.9.8.7:8443/fhir", patient: "p"}); status != fhirServerRefused {
		t.Fatalf("status %d", status)
	}
	if hits := takeLiveFHIRServer(); len(hits) != 2 || hits[0] != "resolve ehr.example" || hits[1] != "dial 10.9.8.7:8443" {
		t.Fatalf("recorded %v, want the resolve and the dial", hits)
	}
}
