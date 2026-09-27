//go:build linux

package engine

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
)

// downEndpoint is the base URL of an endpoint that never answers: a socket
// bound to a loopback port and never listening, so a connection to it is
// refused, and held for the whole test, so no other listener can be given
// its port (a closed server's freed port can be, and then answers). Linux
// only: macOS drops a connection to such a socket instead of refusing it.
func downEndpoint(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", sa.(*syscall.SockaddrInet4).Port)
}

// The endpoint refuses a connection, as a closed server does, and its port
// cannot be taken by a new listener while the test holds it.
func TestDownEndpointRefusesAndKeepsItsPort(t *testing.T) {
	addr := strings.TrimPrefix(downEndpoint(t), "http://")
	c, err := net.Dial("tcp", addr)
	if err == nil {
		_ = c.Close()
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dialing the down endpoint: %v, want connection refused", err)
	}
	if ln, err := net.Listen("tcp", addr); err == nil {
		_ = ln.Close()
		t.Fatal("another listener was given the down endpoint's port")
	}
}
