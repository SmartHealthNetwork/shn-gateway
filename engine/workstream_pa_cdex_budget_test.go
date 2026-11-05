//go:build !shn_coreonly

package engine

// workstream_pa_cdex_budget_test.go — the budget for large bodies at each edge.
// A documentation attachment holds it from before it is read until it is
// answered, at the provider's ingress and, once its envelope is over the
// network's limit for other legs, at the payer's inbound route. A request that
// does not fit is refused 503 with Retry-After as routing, at every level, with
// nothing read past the line, nothing sent and the payer's system not called.

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// holdingAllBut holds all of g's budget but room bytes, for the test.
func holdingAllBut(t *testing.T, g *Gateway, room int64) {
	t.Helper()
	release, ok := g.bodies.acquire(g.bodies.limit - room)
	if !ok {
		t.Fatalf("the budget has %d of %d held", g.bodies.held, g.bodies.limit)
	}
	t.Cleanup(release)
}

func TestCDexIngress_TheBudgetRefusesASubmissionThatDoesNotFit(t *testing.T) {
	t.Parallel()
	body := cdexSubmissionBody("MBR-COVERED", cdexPayerCMS)
	size := int64(len(body))
	unstated := func(r *http.Request) { r.ContentLength = -1 }
	for name, row := range map[string]struct {
		room    int64
		request func(*http.Request)
		carried bool
	}{
		"under":                               {room: size + 1, carried: true},
		"at":                                  {room: size, carried: true},
		"one byte over":                       {room: size - 1},
		"no stated length, at":                {room: shnsdk.MaxAttachmentBytes, request: unstated, carried: true},
		"no stated length, under the ceiling": {room: shnsdk.MaxAttachmentBytes - 1, request: unstated},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				holdingAllBut(t, env.originator, row.room)
				held := env.originator.bodies.held
				r := cdexPost(body, "")
				if row.request != nil {
					row.request(r)
				}
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(cdexRoute(""))(rec, r)
				if env.originator.bodies.held != held {
					t.Fatalf("%d bytes still held after the answer", env.originator.bodies.held-held)
				}
				if row.carried {
					if rec.Code != http.StatusOK || !bytes.Equal(env.lastRequestPayload(), body) {
						t.Fatalf("answer %d %.300s, want the submission carried", rec.Code, rec.Body)
					}
					return
				}
				if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), errAttachmentBudget) {
					t.Fatalf("answer %d %.300s, want 503 %s", rec.Code, rec.Body, errAttachmentBudget)
				}
				if ra := rec.Header().Get("Retry-After"); ra != strconv.Itoa(bodyBudgetRetryAfter) {
					t.Fatalf("Retry-After %q, want %d", ra, bodyBudgetRetryAfter)
				}
				if env.lastRequestPayload() != nil {
					t.Fatal("a refused submission was sent")
				}
				xr := got.only(t)
				wantRefusal(t, xr, http.StatusServiceUnavailable, RefusedByProviderGateway, RefusalRouting)
				wantReasonInAnswer(t, xr, rec.Body.String())
			})
		}
	}
}

// stalledSubmission is a connection that has stated length and sent one byte,
// and stays open, to a server of env's routes whose read deadline is short.
func stalledSubmission(t *testing.T, env *inProcessExchange, length int) net.Conn {
	t.Helper()
	previous := attachmentReadTimeout
	attachmentReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { attachmentReadTimeout = previous })
	env.originator.cfg.IngressEnabled = true
	srv := httptest.NewServer(env.originator.routes())
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	fmt.Fprintf(conn, "POST /$submit-attachment HTTP/1.1\r\nHost: x\r\nContent-Type: application/fhir+json\r\nContent-Length: %d\r\n\r\n{", length)
	return conn
}

// A submission that stalls is answered: a read that failed keeps its deadline,
// or net/http's discard of what is left would hold the answer for the stall.
func TestCDexIngress_AStalledSubmissionIsAnswered(t *testing.T) {
	conn := stalledSubmission(t, newInProcessExchange(t), 1000)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to the stalled submission: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("answer %d, want 400", resp.StatusCode)
	}
}

// A submission that stalls gives its bytes back when its read deadline passes,
// over a real connection the sender keeps open, so it cannot keep the budget.
func TestCDexIngress_AStalledSubmissionGivesTheBudgetBack(t *testing.T) {
	held := func(b *bodyBudget) int64 {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.held
	}
	env := newInProcessExchange(t)
	stalledSubmission(t, env, shnsdk.MaxAttachmentBytes)
	wait := func(what string, done func() bool) {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if done() {
				return
			}
		}
		t.Fatalf("%s: %d bytes held", what, held(env.originator.bodies))
	}
	wait("the submission never held the budget", func() bool { return held(env.originator.bodies) == shnsdk.MaxAttachmentBytes })
	wait("the stalled submission kept the budget", func() bool { return held(env.originator.bodies) == 0 })
}

// With no credential and no room, the answer is about the credential: a caller
// that does not authenticate never reaches the budget.
func TestCDexIngress_NoCredentialNeverReachesTheBudget(t *testing.T) {
	_, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	holdingAllBut(t, g, 0)
	r := httptest.NewRequest(http.MethodPost, "/$submit-attachment", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/fhir+json")
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("answer %d (Retry-After %q) %.200s, want 401", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
}

// Every way a submission ends gives its bytes back: refused for its content or
// its size, and failed at the Hub.
func TestCDexIngress_EveryPathReleasesTheBudget(t *testing.T) {
	t.Parallel()
	for name, body := range map[string][]byte{
		"not a submission": []byte("{"),
		"over the ceiling": cdexSubmissionOf(t, shnsdk.MaxAttachmentBytes+1),
		"no payer named":   cdexSubmissionBody("MBR-COVERED", nil),
	} {
		t.Run(name, func(t *testing.T) {
			env := newInProcessExchange(t)
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(cdexRoute(""))(rec, cdexPost(body, ""))
			if rec.Code < 400 {
				t.Fatalf("answer %d, want a refusal", rec.Code)
			}
			if env.originator.bodies.held != 0 {
				t.Fatalf("%d bytes held after the refusal", env.originator.bodies.held)
			}
		})
	}
	t.Run("the Hub's refusal", func(t *testing.T) {
		env := newInProcessExchange(t)
		base := env.originator.cfg.Client.Transport
		env.originator.cfg.Client.Transport = hubAnswers{next: base, status: 503, reason: "large envelope budget exhausted", delivered: "no"}
		rec := httptest.NewRecorder()
		env.originator.ingressRoute(cdexRoute(""))(rec, cdexPost(cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), ""))
		if rec.Code != http.StatusServiceUnavailable || env.originator.bodies.held != 0 {
			t.Fatalf("answer %d, %d bytes held", rec.Code, env.originator.bodies.held)
		}
	})
}

// With no valid Hub assertion and no room, the answer is about the assertion: a
// caller the gateway cannot authenticate never reaches the budget.
func TestInbound_NoAssertionNeverReachesTheBudget(t *testing.T) {
	p, _ := newRecordingLevelPayer(t, EnforcementObserve)
	holdingAllBut(t, p.g, 0)
	r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", strings.NewReader(strings.Repeat(" ", shnsdk.MaxRequestBytes+1)))
	r.Header.Set("X-Hub-Assertion", "not-an-assertion")
	rec := httptest.NewRecorder()
	p.g.inboundRoute().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || rec.Header().Get(NotActedHeader) != "" {
		t.Fatalf("answer %d (%s %q), want 403", rec.Code, NotActedHeader, rec.Header().Get(NotActedHeader))
	}
}

// The line is exact: an envelope of exactly the network's limit for other legs
// is never counted, and one byte more is, with a stated length or without one.
func TestInbound_TheBudgetsLineIsExact(t *testing.T) {
	t.Parallel()
	padTo := func(n int) func([]byte) []byte {
		return func(raw []byte) []byte { return append(raw, bytes.Repeat([]byte(" "), n-len(raw))...) }
	}
	for name, row := range map[string]struct {
		size    int
		coded   bool
		carried bool
	}{
		"at the line":              {size: shnsdk.MaxRequestBytes, carried: true},
		"one byte over":            {size: shnsdk.MaxRequestBytes + 1},
		"at the line, no length":   {size: shnsdk.MaxRequestBytes, coded: true, carried: true},
		"one byte over, no length": {size: shnsdk.MaxRequestBytes + 1, coded: true},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := newRecordingLevelPayer(t, EnforcementObserve)
			p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
			p.envelope = padTo(row.size)
			if row.coded {
				p.coding = "gzip"
				pad := p.envelope
				p.envelope = func(raw []byte) []byte { return gzipT(t, pad(raw)) }
			}
			holdingAllBut(t, p.g, 0)
			ans := p.send(t, legCDexSubmit, "", cdexSubmissionBody("MBR-COVERED", cdexPayerCMS))
			if row.carried && ans.status != http.StatusOK {
				t.Fatalf("answer %d %.300s, want it carried, never counted", ans.status, ans.body)
			}
			if !row.carried && ans.status != http.StatusServiceUnavailable {
				t.Fatalf("answer %d %.300s, want 503", ans.status, ans.body)
			}
		})
	}
}

// An envelope over the network's limit for other legs holds the payer's budget
// until it is answered, and one that does not fit is refused at once: 503 with
// Retry-After and the attested NotActedHeader, recorded as the
// payer gateway's routing refusal, the payer's system not called. An envelope
// at or under that line is never counted.
func TestInbound_TheBudgetRefusesALargeEnvelopeThatDoesNotFit(t *testing.T) {
	t.Parallel()
	// A large envelope here is a small one padded past the line: the budget
	// reads only its length, and sealing a large one at every level is dear.
	const large = 9 << 20
	padded := func(p *levelPayer) {
		p.envelope = func(raw []byte) []byte { return append(raw, bytes.Repeat([]byte(" "), large-len(raw))...) }
	}
	coded := func(p *levelPayer) {
		p.coding = "gzip"
		p.envelope = func(raw []byte) []byte { return storedGzip(t, raw) }
	}
	codedAndPadded := func(p *levelPayer) {
		padded(p)
		pad := p.envelope
		p.coding = "gzip"
		p.envelope = func(raw []byte) []byte { return storedGzip(t, pad(raw)) }
	}
	for name, row := range map[string]struct {
		room    int64
		prepare func(*levelPayer)
		carried bool
	}{
		"large, room":                 {room: int64(maxEnvelopeBytes), prepare: padded, carried: true},
		"large, exactly its length":   {room: large, prepare: padded, carried: true},
		"large, one byte short":       {room: large - 1, prepare: padded},
		"large, no room":              {room: 0, prepare: padded},
		"ordinary, no room":           {room: 0, carried: true},
		"coded and ordinary, no room": {room: 0, prepare: coded, carried: true},
		"coded and large, room":       {room: int64(maxEnvelopeBytes), prepare: codedAndPadded, carried: true},
		"coded and large, no room":    {room: int64(maxEnvelopeBytes) - 1, prepare: codedAndPadded},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				p.g.bodies.limit = 64 << 20
				p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
				if row.prepare != nil {
					row.prepare(p)
				}
				holdingAllBut(t, p.g, row.room)
				held := p.g.bodies.held
				body := cdexSubmissionBody("MBR-COVERED", cdexPayerCMS)
				ans := p.send(t, legCDexSubmit, "", body)
				if p.g.bodies.held != held {
					t.Fatalf("%d bytes still held after the answer", p.g.bodies.held-held)
				}
				if row.carried {
					if ans.status != http.StatusOK || !bytes.Equal(p.partner.lastBody, body) {
						t.Fatalf("answer %d %.300s, want the submission carried whole", ans.status, ans.body)
					}
					return
				}
				if ans.status != http.StatusServiceUnavailable || p.partner.calls.Load() != 0 {
					t.Fatalf("answer %d %.300s (payer system calls %d), want 503 and no call", ans.status, ans.body, p.partner.calls.Load())
				}
				if v := ans.header.Get(NotActedHeader); v != NotActedAtCapacity {
					t.Fatalf("%s = %q, want %q", NotActedHeader, v, NotActedAtCapacity)
				}
				if ra := ans.header.Get("Retry-After"); ra != strconv.Itoa(bodyBudgetRetryAfter) {
					t.Fatalf("Retry-After %q, want %d", ra, bodyBudgetRetryAfter)
				}
				wantRefusal(t, got.only(t), http.StatusServiceUnavailable, RefusedByPayerGateway, RefusalRouting)
			})
		}
	}
}
