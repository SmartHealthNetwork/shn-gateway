package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// This package's tests never resolve a name on the live network. Their fixtures
// intercept http.DefaultTransport, and a client that dials on its own would
// otherwise resolve the fixture's host for real: live network in a hermetic
// gate, and inside a synctest bubble a lookup Go's resolver shares with one
// started outside it. TestMain installs a resolver that answers every question
// with an error before any test runs, and fails the package if a test asked.
func TestMain(m *testing.M) {
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: guardDNS}
	// Go's resolver creates its shared state on the first lookup. Make that
	// lookup here, outside any synctest bubble: state created inside a bubble
	// would crash a later lookup from outside it before this guard could name
	// the host.
	_, _ = net.DefaultResolver.LookupHost(context.Background(), "dns-guard-prime.invalid")
	dnsGuard.asked = nil
	code := m.Run()
	if asked := dnsQuestions(); len(asked) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: this package's tests resolved names on the live network: %s\n", strings.Join(asked, ", "))
		code = 1
	}
	os.Exit(code)
}

var dnsGuard struct {
	mu    sync.Mutex
	asked []string
}

// dnsQuestions are the names asked so far, sorted and deduplicated.
func dnsQuestions() []string {
	dnsGuard.mu.Lock()
	defer dnsGuard.mu.Unlock()
	out := slices.Clone(dnsGuard.asked)
	slices.Sort(out)
	return slices.Compact(out)
}

func guardDNS(context.Context, string, string) (net.Conn, error) { return dnsGuardConn{}, nil }

var errLiveDNS = errors.New("live DNS lookup refused in tests")

// dnsGuardConn records the question of each query written to it and answers
// every read with an error. It is not a net.PacketConn, so the resolver
// frames every query as on a stream: a two-byte length prefix first.
type dnsGuardConn struct{}

func (dnsGuardConn) Write(b []byte) (int, error) {
	msg := b
	if len(msg) >= 2 {
		msg = msg[2:]
	}
	name := "<unparsed>"
	if len(msg) > 12 {
		var labels []string
		for i := 12; i < len(msg) && msg[i] != 0; {
			n := int(msg[i])
			if i+1+n > len(msg) {
				break
			}
			labels = append(labels, string(msg[i+1:i+1+n]))
			i += 1 + n
		}
		if len(labels) > 0 {
			name = strings.Join(labels, ".")
		}
	}
	dnsGuard.mu.Lock()
	dnsGuard.asked = append(dnsGuard.asked, name)
	dnsGuard.mu.Unlock()
	return len(b), nil
}

func (dnsGuardConn) Read([]byte) (int, error)         { return 0, errLiveDNS }
func (dnsGuardConn) Close() error                     { return nil }
func (dnsGuardConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (dnsGuardConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (dnsGuardConn) SetDeadline(time.Time) error      { return nil }
func (dnsGuardConn) SetReadDeadline(time.Time) error  { return nil }
func (dnsGuardConn) SetWriteDeadline(time.Time) error { return nil }

// The guard sees a lookup, fails it, and would fail the package: a name
// resolved through the default resolver, inside a synctest bubble or outside
// one, is recorded and answered with an error, and neither crashes the other.
// The probes' own records are removed so they do not fail the run.
func TestTheDNSGuardRefusesAndRecordsALookup(t *testing.T) {
	const inside, outside = "dns-guard-bubble.invalid", "dns-guard-probe.invalid"
	synctest.Test(t, func(t *testing.T) {
		if _, err := net.DefaultResolver.LookupHost(context.Background(), inside); err == nil {
			t.Error("a lookup through the guard succeeded inside a bubble")
		}
	})
	if _, err := net.DefaultResolver.LookupHost(context.Background(), outside); err == nil {
		t.Error("a lookup through the guard succeeded")
	}
	asked := dnsQuestions()
	for _, probe := range []string{inside, outside} {
		if !slices.Contains(asked, probe) {
			t.Errorf("the guard did not record %s: %v", probe, asked)
		}
	}
	dnsGuard.mu.Lock()
	dnsGuard.asked = slices.DeleteFunc(dnsGuard.asked, func(n string) bool {
		for _, probe := range []string{inside, outside} {
			if n == probe || strings.HasPrefix(n, probe+".") {
				return true
			}
		}
		return false
	})
	dnsGuard.mu.Unlock()
}
