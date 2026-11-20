package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
)

// A Hub or Authorization Framework instance that has not yet read the
// network's membership once since it started refuses every call before it
// acts: 503, HubDeliveredHeader "no", and the JSON body {"error":
// hubReasonNotYetLoaded} (PARTICIPANT_PROTOCOL §4.1, §6.1c). It stays in the
// network's in-cluster endpoint while it starts, so about half of the new
// connections made then reach it, and a pooled connection that reached it stays
// on it. The call is safe to send again, the same sealed envelope included:
// "no" says the instance refused before it verified, recorded or forwarded
// anything. So the gateway sends it again, at most notYetLoadedRetries times,
// each on a new connection (freshConnClient), after notYetLoadedWaits, inside
// the call's own deadline. Every other answer, and a failure with none, keeps
// its own handling; a call refused that way every time is carried as its last
// refusal, as before.
const notYetLoadedRetries = 2

// notYetLoadedWaits is the wait before each retry. The retry is not waiting for
// the refusing instance to load (its Retry-After is for whoever sent the
// call): it needs a new connection, which may reach a ready instance at once.
var notYetLoadedWaits = [notYetLoadedRetries]time.Duration{250 * time.Millisecond, 500 * time.Millisecond}

// The services and results of NetworkRetryMetric.
const (
	RetryServiceHub   = "hub"
	RetryServiceAuthz = "authz"
	// RetryAnswered: the retry was answered with anything but the
	// not-yet-loaded refusal (a success, or any other answer).
	RetryAnswered = "answered"
	// RetryRefused: the retry was refused not yet loaded again.
	RetryRefused = "refused"
	// RetryFailed: the retry got no answer of the service's own (a failed
	// connection, a timeout, or a proxy's answer in front of it).
	RetryFailed = "failed"
)

// NetworkRetryServices and NetworkRetryResults are NetworkRetryMetric's closed
// lists.
var (
	NetworkRetryServices = []string{RetryServiceHub, RetryServiceAuthz}
	NetworkRetryResults  = []string{RetryAnswered, RetryRefused, RetryFailed}
)

// notYetLoadedAnswer reports whether an answer is the not-yet-loaded refusal:
// status 503, exactly one HubDeliveredHeader and it "no", and a JSON body whose
// "error" is exactly hubReasonNotYetLoaded. It is decided by those fields
// alone, never by matching the body's text.
func notYetLoadedAnswer(status int, h http.Header, body []byte) bool {
	if status != http.StatusServiceUnavailable {
		return false
	}
	if d := h.Values(HubDeliveredHeader); len(d) != 1 || d[0] != "no" {
		return false
	}
	var e struct {
		Error *string `json:"error"`
	}
	return json.Unmarshal(body, &e) == nil && e.Error != nil && *e.Error == hubReasonNotYetLoaded
}

// freshTransports holds, for each base transport a retry has cloned, its clone
// with keep-alives off.
var freshTransports sync.Map // *http.Transport -> *http.Transport

// freshConnClient is c for a retry that must not reuse a pooled connection: a
// copy of c (its timeout and jar) whose transport is c's with its base
// *http.Transport cloned with DisableKeepAlives, so every request it sends
// dials a new connection and closes it after the answer. net/http picks an
// idle pooled connection for any request, whatever its Close says, unless its
// transport disables keep-alives. The clone keeps everything else of the base,
// its TLS configuration and client certificate among them, and every wrapper
// the gateway puts around a base is kept around it in place: endpointcert's
// recorder and tags, and serviceclient's redirect policy. ok is false when the
// base is not an *http.Transport (a transport an embedding program supplied):
// no new connection can be guaranteed, so no retry is made.
func freshConnClient(c *http.Client) (*http.Client, bool) {
	if c == nil {
		c = http.DefaultClient
	}
	rt, ok := freshRoundTripper(c.Transport)
	if !ok {
		return nil, false
	}
	out := *c
	out.Transport = rt
	return &out, true
}

// freshRoundTripper is rt with its base *http.Transport replaced by its clone
// with keep-alives off (one per base, kept in freshTransports), the wrappers
// above it rebuilt around the clone in the same order.
func freshRoundTripper(rt http.RoundTripper) (http.RoundTripper, bool) {
	if base, _, ok := endpointcert.Unwrap(rt); ok {
		inner, ok := freshRoundTripper(base)
		if !ok {
			return nil, false
		}
		return endpointcert.Rewrap(rt, inner), true
	}
	if base, ok := serviceclient.Base(rt); ok {
		inner, ok := freshRoundTripper(base)
		if !ok {
			return nil, false
		}
		return serviceclient.Transport(inner), true
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	t, ok := rt.(*http.Transport)
	if !ok {
		return nil, false
	}
	ft, ok := freshTransports.Load(t)
	if !ok {
		clone := t.Clone()
		clone.DisableKeepAlives = true
		ft, _ = freshTransports.LoadOrStore(t, clone)
	}
	return ft.(*http.Transport), true
}

// retryWait waits d before a retry, false when ctx ends first.
func (g *Gateway) retryWait(ctx context.Context, d time.Duration) bool {
	if g.waitForRetry != nil {
		return g.waitForRetry(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// retryingNotYetLoaded logs the retry about to be made: the service, which
// retry it is and the leg's correlation id, nothing of the call's content.
func retryingNotYetLoaded(service string, n int, correlationID string) {
	log.Printf("gateway: %s instance refused %q before acting; sending again on a new connection (retry %d of %d, correlation %s)",
		service, hubReasonNotYetLoaded, n, notYetLoadedRetries, correlationID)
}

// networkRetried counts one retry by its result (Config.NetworkRetryMetric).
func (g *Gateway) networkRetried(service, result string) {
	if g.cfg.NetworkRetryMetric != nil {
		g.cfg.NetworkRetryMetric(service, result)
	}
}

// hubRetryResult is the result of a retried POST to the Hub.
func hubRetryResult(err error) string {
	var refused *hubRefusalError
	switch {
	case err == nil:
		return RetryAnswered
	case errors.As(err, &refused):
		if refused.notYetLoaded {
			return RetryRefused
		}
		if !refused.fromHub {
			return RetryFailed
		}
		return RetryAnswered
	case isAnswerLost(err):
		return RetryAnswered
	default:
		return RetryFailed
	}
}

// hubBytesLeft reports whether a POST to the Hub that ended in err sent the
// request's bytes: the Hub answered (any answer, a refusal or one that could
// not be read included, or a redirect), or the connection failed after the
// request was written.
func hubBytesLeft(err error) bool {
	var refused *hubRefusalError
	var lost *answerLostError
	var sent *hubSentError
	var redirected *hubRedirectError
	return err == nil || errors.As(err, &refused) || errors.As(err, &lost) || errors.As(err, &sent) || errors.As(err, &redirected)
}

// isAnswerLost reports whether err is an answer the Hub gave that this gateway
// could not accept: the Hub answered.
func isAnswerLost(err error) bool {
	var lost *answerLostError
	return errors.As(err, &lost)
}

// authzNotYetLoaded is an authorize call refused by a Framework instance that
// has not yet loaded the network's membership. It is errAuthzUnreachable, as
// before (recorded, counted and answered as one); the type only says the call
// may be sent again.
type authzNotYetLoaded struct{ err error }

func (e *authzNotYetLoaded) Error() string { return e.err.Error() }
func (e *authzNotYetLoaded) Unwrap() error { return e.err }

// authzRetryResult is the result of a retried authorize call.
func authzRetryResult(err error) string {
	var nyl *authzNotYetLoaded
	switch {
	case err == nil:
		return RetryAnswered
	case errors.As(err, &nyl):
		return RetryRefused
	case errors.Is(err, errAuthzUnreachable):
		var limited *authzLimited
		if errors.As(err, &limited) {
			return RetryAnswered // the Framework's own 429
		}
		return RetryFailed
	default:
		return RetryAnswered
	}
}
