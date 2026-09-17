package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const forwardingLimit = 256

// admission owns public sockets, pending dials and both streaming directions.
// It never parses, reconstructs, retries or buffers a complete HTTP message.
type admission struct {
	mu            sync.Mutex
	ready, closed bool
	listener      net.Listener
	backend       string
	limit         int
	dial          func(context.Context, string, string) (net.Conn, error)
	ctx           context.Context
	cancel        context.CancelFunc
	stop          func() bool
	pairs         map[*forwardPair]struct{}
	wg            sync.WaitGroup
	errors        chan error
}
type forwardPair struct{ client, backend net.Conn }

func newAdmission(parent context.Context, address, backend string, limit int, dial func(context.Context, string, string) (net.Conn, error)) (*admission, error) {
	if parent.Err() != nil || limit < 1 {
		return nil, fmt.Errorf("admission unavailable")
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	if dial == nil {
		dial = (&net.Dialer{Timeout: 4 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	a := &admission{listener: ln, backend: backend, limit: limit, dial: dial, ctx: ctx, cancel: cancel, pairs: map[*forwardPair]struct{}{}, errors: make(chan error, 1)}
	a.wg.Add(1)
	a.stop = context.AfterFunc(parent, a.revoke)
	go a.accept()
	return a, nil
}
func (a *admission) addr() string { return a.listener.Addr().String() }
func (a *admission) count() int   { a.mu.Lock(); defer a.mu.Unlock(); return len(a.pairs) }
func (a *admission) admit() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.ctx.Err() != nil {
		return false
	}
	a.ready = true
	return true
}
func (a *admission) accept() {
	defer a.wg.Done()
	for {
		c, err := a.listener.Accept()
		if err != nil {
			a.mu.Lock()
			closed := a.closed
			a.mu.Unlock()
			if !closed {
				a.revoke()
				a.errors <- err
			}
			return
		}
		p := &forwardPair{client: c}
		a.mu.Lock()
		if a.closed || !a.ready || a.ctx.Err() != nil || len(a.pairs) >= a.limit {
			a.mu.Unlock()
			c.Close()
			continue
		}
		a.pairs[p] = struct{}{}
		a.wg.Add(1)
		a.mu.Unlock()
		go a.forward(p)
	}
}
func (a *admission) forward(p *forwardPair) {
	defer a.wg.Done()
	defer func() {
		a.mu.Lock()
		delete(a.pairs, p)
		a.mu.Unlock()
		p.client.Close()
		if p.backend != nil {
			p.backend.Close()
		}
	}()
	b, err := a.dial(a.ctx, "tcp", a.backend)
	if err != nil {
		return
	}
	a.mu.Lock()
	if a.closed || a.ctx.Err() != nil {
		a.mu.Unlock()
		b.Close()
		return
	}
	p.backend = b
	a.mu.Unlock()
	done := make(chan error, 2)
	copyDirection := func(dst, src net.Conn) {
		// Wrappers keep io.CopyBuffer from selecting TCP's optional fast paths;
		// each direction has a fixed 32KiB user-space buffer.
		_, err := io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, make([]byte, 32<<10))
		if err == nil {
			if tcp, ok := dst.(interface{ CloseWrite() error }); ok {
				err = tcp.CloseWrite()
			} else {
				err = fmt.Errorf("TCP half-close unavailable")
			}
		}
		done <- err
	}
	go copyDirection(b, p.client)
	go copyDirection(p.client, b)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			p.client.Close()
			b.Close()
		}
	}
}

// revoke never waits and never holds the lock while closing or canceling I/O.
// Registration is disabled first, so wait cannot race a later WaitGroup.Add.
func (a *admission) revoke() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.ready = false
	connections := make([]net.Conn, 0, len(a.pairs)*2)
	for p := range a.pairs {
		connections = append(connections, p.client)
		if p.backend != nil {
			connections = append(connections, p.backend)
		}
	}
	a.mu.Unlock()
	a.cancel()
	a.listener.Close()
	for _, c := range connections {
		c.Close()
	}
}
func (a *admission) wait() {
	a.wg.Wait()
	if a.stop != nil {
		a.stop()
	}
}
