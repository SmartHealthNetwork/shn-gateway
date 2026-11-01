//go:build shn_testdouble

package otelsetup

import (
	"errors"
	"net"
	"testing"
	"time"
)

// closed reports whether ln has been closed, read from the listener itself.
func closed(ln net.Listener) bool {
	err := ln.(*net.TCPListener).SetDeadline(time.Now().Add(time.Millisecond))
	if err == nil {
		_, err = ln.Accept()
	}
	return errors.Is(err, net.ErrClosed)
}

func handedCount(addr string) int {
	handedMu.Lock()
	defer handedMu.Unlock()
	if _, ok := handed[addr]; ok {
		return 1
	}
	return 0
}

// HandListenerForTest refuses outside a test binary and hands nothing.
func TestHandListenerForTest_RefusesOutsideATestBinary(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	prev := handTestBinary
	handTestBinary = func() bool { return false }
	defer func() { handTestBinary = prev }()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("HandListenerForTest did not panic outside a test binary")
			}
		}()
		HandListenerForTest(t, ln)
	}()
	if handedCount(ln.Addr().String()) != 0 {
		t.Fatal("a refused hand-over left the listener handed")
	}
}

// The bind for a handed address returns the handed listener, once; the next
// bind of that address, and a bind of an address no test handed, bind for real.
func TestHandListenerForTest_ServedOnceThenBindsForReal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	HandListenerForTest(t, ln)
	addr := ln.Addr().String()
	got, err := listen("tcp", addr)
	if err != nil || got != ln {
		t.Fatalf("the bind of the handed address got %v, %v; want the handed listener", got, err)
	}
	if again, err := listen("tcp", addr); err == nil {
		again.Close()
		t.Fatal("a second bind of the handed address got a listener while the test still holds the port: it was handed twice")
	}
	other, err := listen("tcp", "127.0.0.1:0")
	if err != nil || other == ln {
		t.Fatalf("a bind of an address no test handed got %v, %v; want its own listener", other, err)
	}
	other.Close()
}

// A handed listener no bind used is withdrawn and closed when its test ends.
func TestHandListenerForTest_UnusedIsClosedAtCleanup(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	t.Run("hand", func(t *testing.T) { HandListenerForTest(t, ln) })
	if handedCount(addr) != 0 || !closed(ln) {
		_ = ln.Close()
		t.Fatal("an unused handed listener outlived its test")
	}
}
