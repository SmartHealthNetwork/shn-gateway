//go:build shn_testdouble

package otelsetup

import (
	"net"
	"sync"
	"testing"
)

// handed holds the listeners tests bound themselves, by address, for
// ServePull to serve in place of binding that address.
var (
	handedMu sync.Mutex
	handed   = map[string]net.Listener{}
)

func init() {
	bind := listen
	listen = func(network, addr string) (net.Listener, error) {
		handedMu.Lock()
		ln, ok := handed[addr]
		delete(handed, addr)
		handedMu.Unlock()
		if ok {
			return ln, nil
		}
		return bind(network, addr)
	}
}

// handTestBinary says whether this is a test binary; a test replaces it to
// prove HandListenerForTest refuses outside one.
var handTestBinary = testing.Testing

// HandListenerForTest has the next ServePull whose address is ln's serve ln
// instead of binding that address: the test binds the port and holds it from
// the start, so no other process can take it between a probe and the
// gateway's bind. The test names ln.Addr() as the pull address. An unused
// listener is withdrawn and closed when the test ends. It panics outside a
// test binary.
func HandListenerForTest(t testing.TB, ln net.Listener) {
	if !handTestBinary() {
		panic("otelsetup: HandListenerForTest is available only in tests")
	}
	addr := ln.Addr().String()
	handedMu.Lock()
	handed[addr] = ln
	handedMu.Unlock()
	t.Cleanup(func() {
		handedMu.Lock()
		unused, ok := handed[addr]
		delete(handed, addr)
		handedMu.Unlock()
		if ok {
			_ = unused.Close()
		}
	})
}
