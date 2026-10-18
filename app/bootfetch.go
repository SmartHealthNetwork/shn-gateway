package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"syscall"
	"time"
)

// bootFetchWaits are the waits between a boot fetch's tries, the last
// repeated, and bootFetchBudget the time into a fetch after which no new try
// starts: a gateway that boots while the Authorization Framework or the Hub
// rolls to, or is replaced by, a new task waits for it instead of failing
// its boot.
var bootFetchWaits = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

const bootFetchBudget = 60 * time.Second

// bootClient is c for the boot fetches of discovery and the trust anchors: a
// GET that gets no answer (the connection refused, reset or closed) or is
// answered 502, 503 or 504 is sent again after each of waits in turn, the
// last repeated, while the budget lasts. Every other answer is the answer at once: a 4xx, or a
// document or key that cannot be read, fails the boot as before, and a key
// is never fetched again into acceptance. Each try is bounded by c's own
// timeout; the client is not, so the tries share the budget instead.
func bootClient(c *http.Client, waits []time.Duration, budget time.Duration) *http.Client {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out := *c
	out.Timeout = 0
	out.Transport = &bootRetry{base: base, perTry: c.Timeout, waits: waits, budget: budget}
	return &out
}

type bootRetry struct {
	base   http.RoundTripper
	perTry time.Duration
	waits  []time.Duration
	budget time.Duration
}

func (b *bootRetry) RoundTrip(req *http.Request) (*http.Response, error) {
	stop := time.Now().Add(b.budget)
	for try := 0; ; try++ {
		ctx, cancel := req.Context(), context.CancelFunc(func() {})
		if b.perTry > 0 {
			ctx, cancel = context.WithTimeout(ctx, b.perTry)
		}
		resp, err := b.base.RoundTrip(req.Clone(ctx))
		wait := b.waits[min(try, len(b.waits)-1)]
		if time.Now().Add(wait).After(stop) || !bootTransient(resp, err) {
			if resp == nil {
				cancel()
				return nil, err
			}
			resp.Body = &cancelOnClose{resp.Body, cancel}
			return resp, err
		}
		if resp != nil {
			resp.Body.Close()
		}
		cancel()
		pause := time.NewTimer(wait)
		select {
		case <-req.Context().Done():
			pause.Stop()
			return nil, req.Context().Err()
		case <-pause.C:
		}
	}
}

// bootTransient reports a try that may be made again: no answer, or a
// proxy's 502, 503 or 504 with no task behind it yet. Windows reports a
// refused or reset connection by its own codes (10061, 10054).
func bootTransient(resp *http.Response, err error) bool {
	if err != nil {
		return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
			errors.Is(err, syscall.Errno(10061)) || errors.Is(err, syscall.Errno(10054)) ||
			errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// cancelOnClose ends a try's own bound once its answer is read.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
