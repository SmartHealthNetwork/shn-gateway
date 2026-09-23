// hubtimeout_test.go — the originating gateway names a Hub leg that produced no
// answer within its client's timeout budget, instead of collapsing it into the
// generic routing failure. Hermetic: the stub Hub holds /route open until the
// client gives up.
package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHubLegTimeout_NamesTheBudget: the client's own Timeout ends the wait →
// the caller reads 504 with the fact and the budget, and the leg outcome stays
// "unreachable" (the leg did not complete).
func TestHubLegTimeout_NamesTheBudget(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, uc03Coverage())
	stub.routeDelay = 5 * time.Second
	gw.cfg.Client = &http.Client{Transport: stub, Timeout: 50 * time.Millisecond}
	var got []string
	gw.cfg.LegMetric = func(outcome string) { got = append(got, outcome) }

	rec := callUC03(t, gw)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504; body=%s", rec.Code, rec.Body.String())
	}
	const want = `"error":"no answer on the hub leg within 50ms (hub leg timeout)"`
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("body=%s want to contain %s", rec.Body.String(), want)
	}
	if strings.Contains(rec.Body.String(), "hub routing failed") {
		t.Fatalf("body=%s still carries the generic routing failure", rec.Body.String())
	}
	wantOutcomes := []string{LegOutcomeRouted, LegOutcomeUnreachable}
	if strings.Join(got, ",") != strings.Join(wantOutcomes, ",") {
		t.Fatalf("outcomes=%v want %v", got, wantOutcomes)
	}
}

// TestHubLegTimeout_CallerDeadlineFirst: the client has a 5 s Timeout but the
// caller's own request deadline ends the wait first → 504 "hub leg timed out"
// with no number. The gateway's budget was not what ended the wait, so
// claiming "within 5s" would be false; this row goes red if the caller-
// deadline guard is dropped.
func TestHubLegTimeout_CallerDeadlineFirst(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, uc03Coverage())
	stub.routeDelay = 5 * time.Second
	gw.cfg.Client = &http.Client{Transport: stub, Timeout: 5 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"hub leg timed out"`) {
		t.Fatalf("body=%s want the timeout with no number", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "within") {
		t.Fatalf("body=%s claims a budget that did not end the wait", rec.Body.String())
	}
}

// timeoutShapedFault is a transport error that reports Timeout() — the shape of
// a dial or TLS-handshake timeout — returned instantly, before any Hub was reached.
type timeoutShapedFault struct{}

func (timeoutShapedFault) Error() string   { return "net/http: TLS handshake timeout" }
func (timeoutShapedFault) Timeout() bool   { return true }
func (timeoutShapedFault) Temporary() bool { return false }

// TestHubLegFault_TimeoutShapedFaultStaysRoutingFailed: an instant transport
// error that merely looks like a timeout must not become "within 30s" — the
// gateway's own leg deadline never fired, so the Hub was not waited on. It stays
// the generic 502, with no timeout wording and no number.
func TestHubLegFault_TimeoutShapedFaultStaysRoutingFailed(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, uc03Coverage())
	stub.routeErr = timeoutShapedFault{}
	gw.cfg.Client = &http.Client{Transport: stub, Timeout: 30 * time.Second}
	var got []string
	gw.cfg.LegMetric = func(outcome string) { got = append(got, outcome) }

	start := time.Now()
	rec := callUC03(t, gw)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the instant fault was waited on")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"hub routing failed"`) {
		t.Fatalf("body=%s want the generic routing failure", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "30s") || strings.Contains(rec.Body.String(), "timed out") || strings.Contains(rec.Body.String(), "timeout") {
		t.Fatalf("body=%s claims a timeout for a fault that did not wait on the Hub", rec.Body.String())
	}
	wantOutcomes := []string{LegOutcomeRouted, LegOutcomeUnreachable}
	if strings.Join(got, ",") != strings.Join(wantOutcomes, ",") {
		t.Fatalf("outcomes=%v want %v", got, wantOutcomes)
	}
}

// TestHubLegTimeout_NoClientBudget: the client has no Timeout and the caller's
// own request deadline ends the wait → 504 "hub leg timed out", no number (no
// budget of the gateway's ended the wait, so none is claimed).
func TestHubLegTimeout_NoClientBudget(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, uc03Coverage())
	stub.routeDelay = 5 * time.Second
	gw.cfg.Client = &http.Client{Transport: stub}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504; body=%s", rec.Code, rec.Body.String())
	}
	const want = `"error":"hub leg timed out"`
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("body=%s want to contain %s", rec.Body.String(), want)
	}
}

// TestHubLegFault_StaysRoutingFailed: a transport fault that is not a timeout
// keeps the generic 502 "hub routing failed" and the "unreachable" outcome —
// the timeout branch takes nothing away from the existing contract.
func TestHubLegFault_StaysRoutingFailed(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, uc03Coverage())
	stub.routeErr = errors.New("dial tcp: connection refused")
	gw.cfg.Client = &http.Client{Transport: stub, Timeout: 50 * time.Millisecond}
	var got []string
	gw.cfg.LegMetric = func(outcome string) { got = append(got, outcome) }

	rec := callUC03(t, gw)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"hub routing failed"`) {
		t.Fatalf("body=%s want the generic routing failure", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "timed out") || strings.Contains(rec.Body.String(), "timeout") {
		t.Fatalf("body=%s claims a timeout for a fault that was not one", rec.Body.String())
	}
	wantOutcomes := []string{LegOutcomeRouted, LegOutcomeUnreachable}
	if strings.Join(got, ",") != strings.Join(wantOutcomes, ",") {
		t.Fatalf("outcomes=%v want %v", got, wantOutcomes)
	}
}
