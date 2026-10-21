package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// batchBoundValidator is the gateway's validator, counting its calls.
type batchBoundValidator struct {
	inner shnsdk.Validator
	calls atomic.Int32
}

func (v *batchBoundValidator) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	v.calls.Add(1)
	return v.inner.Validate(ctx, body, profile)
}

// batchBoundIngress sends body through the provider's eligibility route at
// level, as runEligibilityIngress does, counting its system-of-record reads.
func batchBoundIngress(t *testing.T, level ConformanceEnforcement, body, answer []byte) (*inProcessExchange, *httptest.ResponseRecorder, ExchangeRecord, *countingSoR) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	sor := &countingSoR{censusSoR: newCensusSoR()}
	env.originator.cfg.SoR = sor
	env.payerReturns(LegResult{Response: testResponse(answer)})
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(body))
	return env, rec, got.only(t), sor
}

// batchBoundPayer is a payer gateway at level counting its system-of-record
// reads and its validator's calls.
func batchBoundPayer(t *testing.T, level ConformanceEnforcement, declared bool) (*levelPayer, *countingSoR, *batchBoundValidator) {
	t.Helper()
	p := eligibilityPayer(t, level, declared)
	sor := &countingSoR{censusSoR: p.store}
	p.g.cfg.SoR = sor
	v := &batchBoundValidator{inner: p.g.cfg.Validator}
	p.g.cfg.Validator = v
	return p, sor, v
}

// eligBatchOf is a batch of n requests for member, each one alone.
func eligBatchOf(t *testing.T, n int, member string) []byte {
	t.Helper()
	requests := make([][]byte, n)
	for i := range requests {
		requests[i] = eligibilityRequest(t, member)
	}
	return eligBatch(t, requests...)
}

// A batch of maxEligibilityBatch requests is read as before; one more is
// refused 413, naming the bound and how to send the requests, before
// anything is read from it, recorded as a limit, at every level: at the
// provider's ingress before it is routed, and at the payer's gateway on the
// records and forward paths before its system is called.
func TestEligibilityBatch_OneMoreThanTheBoundIsRefused(t *testing.T) {
	over := eligibilityBatchTooLarge(maxEligibilityBatch + 1)
	if want := "the batch carries " + strconv.Itoa(maxEligibilityBatch+1) + " coverage eligibility requests; a gateway reads at most " +
		strconv.Itoa(maxEligibilityBatch) + " in one batch, so send them in batches of at most " + strconv.Itoa(maxEligibilityBatch); over != want {
		t.Fatalf("refusal %q, want %q", over, want)
	}
	atBound := eligBatchOf(t, maxEligibilityBatch, dtrFrameMember)
	pastBound := eligBatchOf(t, maxEligibilityBatch+1, dtrFrameMember)
	answer := eligResponses(t, "Patient/"+dtrFrameMember)
	for _, level := range allLevels {
		t.Run("provider/at the bound/"+level.String(), func(t *testing.T) {
			env, rec, _, sor := batchBoundIngress(t, level, atBound, answer)
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 || sor.count() < maxEligibilityBatch {
				t.Fatalf("answer %d %s, routed %d after %d reads; want the batch carried, each request bound", rec.Code, rec.Body.String(), env.routeHitCount(), sor.count())
			}
		})
		t.Run("provider/past the bound/"+level.String(), func(t *testing.T) {
			env, rec, x, sor := batchBoundIngress(t, level, pastBound, answer)
			refusedBeforeTheNetwork(t, env, rec, http.StatusRequestEntityTooLarge, over)
			wantRefusal(t, x, http.StatusRequestEntityTooLarge, RefusedByProviderGateway, RefusalLimit)
			if n := sor.count(); n != 0 {
				t.Fatalf("%d system-of-record reads before the refusal", n)
			}
		})
		for _, declared := range []bool{false, true} {
			path := map[bool]string{false: "records", true: "forward"}[declared]
			t.Run("payer/"+path+"/at the bound/"+level.String(), func(t *testing.T) {
				p, sor, v := batchBoundPayer(t, level, declared)
				p.partner.respByPath[eligibilityPath] = answer
				ans := p.send(t, "coverage-eligibility", "", atBound)
				// Answering from its records, the payer answers one request at
				// a time (its own 501); forwarding, it binds and carries the
				// batch, validating each request above none.
				if !declared {
					if ans.status != http.StatusNotImplemented {
						t.Fatalf("answer %d %s, want its 501 as before", ans.status, ans.body)
					}
					return
				}
				validated := int(v.calls.Load())
				if ans.status != http.StatusOK || sor.count() < 1 || (level != EnforcementNone && validated < maxEligibilityBatch) {
					t.Fatalf("answer %d %s after %d reads and %d validations; want the batch carried, each request validated", ans.status, ans.body, sor.count(), validated)
				}
			})
			t.Run("payer/"+path+"/past the bound/"+level.String(), func(t *testing.T) {
				p, sor, v := batchBoundPayer(t, level, declared)
				var got exchangeRecords
				p.g.cfg.ExchangeObserved = got.observe
				p.partner.respByPath[eligibilityPath] = answer
				ans := p.send(t, "coverage-eligibility", "", pastBound)
				if ans.status != http.StatusRequestEntityTooLarge || !strings.Contains(string(ans.body), over) || p.partner.calls.Load() != 0 {
					t.Fatalf("answer %d %s (payer system calls %d), want 413 %s", ans.status, ans.body, p.partner.calls.Load(), over)
				}
				if sor.count() != 0 || v.calls.Load() != 0 {
					t.Fatalf("%d system-of-record reads and %d validations before the refusal", sor.count(), v.calls.Load())
				}
				wantRefusal(t, got.only(t), http.StatusRequestEntityTooLarge, RefusedByPayerGateway, RefusalLimit)
			})
		}
	}
}

// The bound counts requests, whatever form carries them: a batch of
// maxEligibilityBatch requests in a Parameters, or with one of them in a FAIS
// Bundle of its own, is read; one more in either is refused.
func TestEligibilityBatch_TheBoundCountsRequestsInEveryForm(t *testing.T) {
	requests := func(n int) [][]byte {
		out := make([][]byte, n)
		for i := range out {
			out[i] = eligibilityRequest(t, dtrFrameMember)
		}
		out[0] = eligFAIS(t, dtrFrameMember, "00001")
		return out
	}
	for _, n := range []int{maxEligibilityBatch, maxEligibilityBatch + 1} {
		for name, body := range map[string][]byte{
			"in a Parameters":            eligibilityAsParameters(t, eligBatchOf(t, n, dtrFrameMember)),
			"with a request in a Bundle": eligBatch(t, requests(n)...),
		} {
			t.Run(name+"/"+strconv.Itoa(n), func(t *testing.T) {
				env, rec, _ := runEligibilityIngress(t, EnforcementObserve, body, eligResponses(t, "Patient/"+dtrFrameMember))
				if n == maxEligibilityBatch {
					if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
						t.Fatalf("answer %d %s, want the batch carried", rec.Code, rec.Body.String())
					}
					return
				}
				refusedBeforeTheNetwork(t, env, rec, http.StatusRequestEntityTooLarge, eligibilityBatchTooLarge(n))
			})
		}
	}
}

// The gateway's published documents state the bound the code enforces.
func TestEligibilityBatch_TheDocsStateTheBound(t *testing.T) {
	want := "reads at most " + strconv.Itoa(maxEligibilityBatch) + " in one batch"
	for _, doc := range []string{"../docs/CONFIGURATION.md", "../STABILITY.md"} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s does not state %q", doc, want)
		}
	}
}
