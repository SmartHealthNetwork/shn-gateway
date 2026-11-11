//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
)

// plainOrigin is an http server that counts what reaches it.
func plainOrigin(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = io.WriteString(w, `{"resourceType":"Bundle","type":"collection","entry":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// redirectingSystem is a participant's https system that answers every
// request with a redirect (code) to target + the request's path.
func redirectingSystem(t *testing.T, code int, target func() string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		http.Redirect(w, r, target()+r.URL.Path, code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// redirectExchange delivers one inquiry, at level, to a payer gateway whose
// native forward reaches system through client, and returns the record and
// the answer the Hub received.
func redirectExchange(t *testing.T, level ConformanceEnforcement, client *http.Client, system string) (ExchangeRecord, payerAnswer) {
	t.Helper()
	p := newLevelPayer(t, level)
	n := NewNativeResponder(client, system, fixedClock,
		WithCRDService("shn-order-select"), WithPASStore(p.store), WithConformancePolicy(NewConformancePolicy(level)))
	n.bindFindingEmitter(p.g.emitFinding)
	p.g.cfg.Responder = n
	var records exchangeRecords
	p.g.cfg.ExchangeObserved = records.observe
	ans := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	return records.only(t), ans
}

// A payer's system that redirects the forward from https to http is refused,
// at every level, before the hop is sent: a 307 or 308 replays the body, which
// would travel in cleartext. The gateway's client carries no bearer here (a
// participant with no credential block), so the engine's own redirect policy
// is the one that holds. Nothing reaches the http origin; the payer gateway
// attests the refusal delivered (its system answered, and the request was
// sent to it) on message integrity, with the reason; the record names the
// system's 3xx as the backend, http-3xx, and the refused hop.
func TestNativeForward_AnHTTPSToHTTPRedirectIsRefusedAtEveryLevel(t *testing.T) {
	for _, level := range allLevels {
		for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
			t.Run(fmt.Sprint(level)+"/"+http.StatusText(code), func(t *testing.T) {
				plain, hits := plainOrigin(t)
				system := redirectingSystem(t, code, func() string { return plain.URL })
				rec, ans := redirectExchange(t, level, system.Client(), system.URL)
				if hits.Load() != 0 {
					t.Fatalf("the http origin received %d requests, want none", hits.Load())
				}
				if ans.status != http.StatusBadGateway || ans.header.Get(HubDeliveredHeader) != "yes" {
					t.Fatalf("answer %d (delivered %q) %s, want 502 attested delivered", ans.status, ans.header.Get(HubDeliveredHeader), ans.body)
				}
				var got map[string]string
				if err := json.Unmarshal(ans.body, &got); err != nil {
					t.Fatalf("answer %s: %v", ans.body, err)
				}
				if want := map[string]string{"error": errCleartextRedirect, "rule": RefusalIntegrity}; got["error"] != want["error"] || got["rule"] != want["rule"] || len(got) != 2 {
					t.Fatalf("answer %q, want %q", got, want)
				}
				if rec.Outcome != ExchangeRefused || rec.Rule != RefusalIntegrity {
					t.Fatalf("record outcome %q rule %q, want refused on integrity", rec.Outcome, rec.Rule)
				}
				if rec.Backend == nil || rec.Backend.Status != code || rec.Backend.ErrorClass != BackendHTTP3xx {
					t.Fatalf("backend %+v, want the system's %d, %s", rec.Backend, code, BackendHTTP3xx)
				}
				if want := []string{smartauth.RedirectCleartextRefused}; !slices.Equal(rec.BackendRedirects, want) {
					t.Fatalf("redirects %q, want %q", rec.BackendRedirects, want)
				}
				// The forward was sent to the system: the record names whose
				// message it carried.
				if rec.Ownership == "" {
					t.Fatal("the record names no ownership for the sent forward")
				}
			})
		}
	}
}

// A redirect the policy follows is recorded by its origin class, and the
// answer at its end is the answer: here a redirect within the system's own
// origin (a forward carries a body, so it follows no other), whose answer is
// relayed.
func TestNativeForward_AFollowedRedirectIsRecordedByItsOriginClass(t *testing.T) {
	answer := decidedAnswer(t)
	var system *httptest.Server
	system = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if r.URL.Query().Get("moved") == "" {
			http.Redirect(w, r, system.URL+r.URL.Path+"?moved=1", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write(answer)
	}))
	t.Cleanup(system.Close)
	rec, ans := redirectExchange(t, EnforcementNone, system.Client(), system.URL)
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s, want the system's answer", ans.status, ans.body)
	}
	if want := []string{smartauth.RedirectSameOrigin}; !slices.Equal(rec.BackendRedirects, want) {
		t.Fatalf("redirects %q, want %q", rec.BackendRedirects, want)
	}
	if rec.Backend == nil || rec.Backend.Status != http.StatusOK || rec.Backend.ErrorClass != "" {
		t.Fatalf("backend %+v, want the answer's 200", rec.Backend)
	}
	// With no redirect, the record names none.
	plain := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write(answer)
	}))
	t.Cleanup(plain.Close)
	if rec, _ := redirectExchange(t, EnforcementNone, plain.Client(), plain.URL); rec.BackendRedirects != nil {
		t.Fatalf("redirects %q with none asked for", rec.BackendRedirects)
	}
}

// The engine applies its redirect policy to a copy of the client it is given:
// the caller's own client is not changed.
func TestNativeResponder_LeavesTheCallersClientUnchanged(t *testing.T) {
	c := &http.Client{}
	_ = NewNativeResponder(c, "https://payer.example", fixedClock)
	_ = NewNativePopulator(c, "https://populate.example")
	if c.CheckRedirect != nil {
		t.Fatal("the caller's client gained a redirect policy")
	}
}

// A read of the participant's system that is redirected from https to http
// fails: nothing reaches the http origin, the read is recorded with class
// http-3xx and the refused hop, and the leg answers as it does for any failed
// read. The forward is the one refusal surface.
func TestBackendReads_AnHTTPSToHTTPRedirectIsNeverFollowed(t *testing.T) {
	t.Run("the CDS service listing", func(t *testing.T) {
		plain, hits := plainOrigin(t)
		system := redirectingSystem(t, http.StatusFound, func() string { return plain.URL })
		x := &exchangeRecorder{}
		ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
		_, err := DiscoverCDSServices(ctx, system.Client(), system.URL, nil)
		if err == nil || !strings.Contains(err.Error(), "cds-services") {
			t.Fatalf("err %v, want the listing read to fail", err)
		}
		if hits.Load() != 0 {
			t.Fatalf("the http origin received %d requests, want none", hits.Load())
		}
		if x.rec.Backend == nil || x.rec.Backend.Status != http.StatusFound || x.rec.Backend.ErrorClass != BackendHTTP3xx {
			t.Fatalf("backend %+v, want the system's 302, %s", x.rec.Backend, BackendHTTP3xx)
		}
		if want := []string{smartauth.RedirectCleartextRefused}; !slices.Equal(x.rec.BackendRedirects, want) {
			t.Fatalf("redirects %q, want %q", x.rec.BackendRedirects, want)
		}
	})
	t.Run("a read of the system of record", func(t *testing.T) {
		plain, hits := plainOrigin(t)
		system := redirectingSystem(t, http.StatusFound, func() string { return plain.URL })
		x := &exchangeRecorder{rec: ExchangeRecord{Direction: DirectionInbound}}
		ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
		readCtx, note := noteRead(ctx)
		if note == nil {
			t.Fatal("the read is not noted")
		}
		req, err := http.NewRequestWithContext(readCtx, http.MethodGet, system.URL+"/Patient?identifier=x", nil)
		if err != nil {
			t.Fatal(err)
		}
		hc := system.Client()
		hc.CheckRedirect = smartauth.CheckRedirect
		resp, err := hc.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, smartauth.ErrCleartextRedirect) {
			t.Fatalf("err %v, want ErrCleartextRedirect", err)
		}
		note(sorClass(SoRCleartextRedirect()))
		if hits.Load() != 0 {
			t.Fatalf("the http origin received %d requests, want none", hits.Load())
		}
		if x.rec.Backend == nil || x.rec.Backend.ErrorClass != BackendHTTP3xx {
			t.Fatalf("backend %+v, want %s", x.rec.Backend, BackendHTTP3xx)
		}
		if want := []string{smartauth.RedirectCleartextRefused}; !slices.Equal(x.rec.BackendRedirects, want) {
			t.Fatalf("redirects %q, want %q", x.rec.BackendRedirects, want)
		}
	})
	t.Run("the leg's answer is a failed read's", func(t *testing.T) {
		gotStatus, gotMsg := SoRFailureResponse(SoRCleartextRedirect())
		wantStatus, wantMsg := SoRFailureResponse(&SoRReadError{Kind: SoRUnavailable})
		if gotStatus != wantStatus || gotMsg != wantMsg {
			t.Fatalf("answer %d %q, want an unavailable read's %d %q", gotStatus, gotMsg, wantStatus, wantMsg)
		}
		if !errors.Is(SoRCleartextRedirect(), smartauth.ErrCleartextRedirect) {
			t.Fatal("the read error does not say it was a refused downgrade")
		}
	})
}

// The provider's $populate engine is a participant's own system too: a
// redirect of the populate request from https to http is never followed, and
// the populate fails at its transport, as any populate the engine does not
// answer does.
func TestNativePopulate_AnHTTPSToHTTPRedirectIsNeverFollowed(t *testing.T) {
	plain, hits := plainOrigin(t)
	system := redirectingSystem(t, http.StatusTemporaryRedirect, func() string { return plain.URL })
	n := NewNativePopulator(system.Client(), system.URL+"/Questionnaire/$populate")
	_, _, failure := n.post(context.Background(), []byte(`{"resourceType":"Parameters"}`))
	want := PopulateFailure{Stage: populateStageTransport, Reason: populateReasonCleartextRedirect, Status: http.StatusTemporaryRedirect}
	if failure == nil || *failure != want {
		t.Fatalf("failure %+v, want %+v", failure, want)
	}
	if hits.Load() != 0 {
		t.Fatalf("the http origin received %d requests, want none", hits.Load())
	}
}

// The record keeps every call's redirects, in order: a read's, then the
// forward's.
func TestExchangeRecorder_RedirectsAccumulateOverCalls(t *testing.T) {
	x := &exchangeRecorder{}
	x.redirected([]string{smartauth.RedirectSameOrigin})
	x.redirected(nil)
	x.redirected([]string{smartauth.RedirectCrossOrigin, smartauth.RedirectCleartextRefused})
	want := []string{smartauth.RedirectSameOrigin, smartauth.RedirectCrossOrigin, smartauth.RedirectCleartextRefused}
	if !slices.Equal(x.rec.BackendRedirects, want) {
		t.Fatalf("redirects %q, want %q", x.rec.BackendRedirects, want)
	}
	var none *exchangeRecorder
	none.redirected(want) // a call with no recorder records nothing
}

// downgradingTokenClient is a SMART client whose token endpoint (an https
// server) redirects the token request to an http origin: the token request
// is never sent there, so no token is obtained and nothing is sent to the
// system either.
func downgradingTokenClient(t *testing.T) (*http.Client, *atomic.Int32) {
	t.Helper()
	plain, hits := plainOrigin(t)
	tokenSrv := redirectingSystem(t, http.StatusTemporaryRedirect, func() string { return plain.URL })
	hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tokenSrv.URL + "/token", ClientID: "gw", ClientSecret: "OPAQUE-VALUE-3686", Scope: "system/*.read", Clock: fixedClock, HTTPClient: tokenSrv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return hc, hits
}

// A token endpoint that redirects the token request from https to http is the
// participant's own authorization server, and the hop is never sent; but the
// operation itself was never sent either, so it is answered as any token
// failure is: not delivered (the system could not be reached), never the
// integrity refusal attested delivered, which says the system answered. The
// record names the refused hop and the token failure's class.
func TestNativeForward_ATokenEndpointDowngradeIsATokenFailure(t *testing.T) {
	for _, level := range allLevels {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			var systemHits atomic.Int32
			system := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				systemHits.Add(1)
				http.Error(w, "unexpected", http.StatusTeapot)
			}))
			t.Cleanup(system.Close)
			hc, tokenHits := downgradingTokenClient(t)
			rec, ans := redirectExchange(t, level, hc, system.URL)
			if systemHits.Load() != 0 || tokenHits.Load() != 0 {
				t.Fatalf("the system received %d requests and the http origin %d; want none", systemHits.Load(), tokenHits.Load())
			}
			if ans.status != http.StatusBadGateway || ans.header.Get(HubDeliveredHeader) == "yes" {
				t.Fatalf("answer %d (delivered %q) %s, want 502 not attested delivered", ans.status, ans.header.Get(HubDeliveredHeader), ans.body)
			}
			if strings.Contains(string(ans.body), errCleartextRedirect) || strings.Contains(string(ans.body), RefusalIntegrity) {
				t.Fatalf("answer %s names the system's own redirect", ans.body)
			}
			if rec.Rule == RefusalIntegrity || rec.Backend == nil || rec.Backend.ErrorClass != BackendAuth {
				t.Fatalf("record rule %q backend %+v, want no integrity refusal and the token failure's %s", rec.Rule, rec.Backend, BackendAuth)
			}
			if want := []string{smartauth.RedirectCleartextRefused}; !slices.Equal(rec.BackendRedirects, want) {
				t.Fatalf("redirects %q, want %q", rec.BackendRedirects, want)
			}
		})
	}
}

// The same token failure on a read is classed as the token failure it is,
// never as the system's own redirect (http-3xx); on $populate it is a token
// acquisition failure, as before.
func TestBackendReads_ATokenEndpointDowngradeIsATokenFailure(t *testing.T) {
	hc, _ := downgradingTokenClient(t)
	system, _ := plainOrigin(t)
	req, _ := http.NewRequest(http.MethodGet, system.URL+"/Patient", nil)
	resp, err := hc.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, smartauth.ErrCleartextRedirect) || !smartauth.IsTokenAcquisitionError(err) {
		t.Fatalf("err %v, want a token acquisition failure caused by the downgrade", err)
	}
	if got := sorClass(err); got == BackendHTTP3xx {
		t.Fatalf("a token endpoint's downgrade classed %q, the system's own redirect", got)
	}
	t.Run("the CDS service listing", func(t *testing.T) {
		x := &exchangeRecorder{}
		ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
		if _, err := DiscoverCDSServices(ctx, hc, system.URL, nil); err == nil {
			t.Fatal("the listing read did not fail")
		}
		if x.rec.Backend == nil || x.rec.Backend.ErrorClass != BackendAuth {
			t.Fatalf("backend %+v, want %s", x.rec.Backend, BackendAuth)
		}
	})
	t.Run("$populate", func(t *testing.T) {
		n := NewNativePopulator(hc, system.URL+"/Questionnaire/$populate")
		_, _, failure := n.post(context.Background(), []byte(`{"resourceType":"Parameters"}`))
		want := PopulateFailure{Stage: populateStageTokenAcquisition, Reason: populateReasonOther}
		if failure == nil || *failure != want {
			t.Fatalf("failure %+v, want %+v", failure, want)
		}
	})
}

// tokenClientFor is a SMART client whose token endpoint is tokenURL, trusting
// the test servers' certificate.
func tokenClientFor(t *testing.T, tokenURL string, trust *httptest.Server) *http.Client {
	t.Helper()
	hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tokenURL, ClientID: "gw", ClientSecret: "OPAQUE-SECRET", Scope: "system/*.read", Clock: fixedClock, HTTPClient: trust.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return hc
}

// A token endpoint that answers the token request with a redirect, to another
// origin or within its own, is never followed: the origin it names receives
// nothing, the operation is never sent, and the leg answers exactly as it does
// when the token endpoint does not answer at all, at every level: not
// delivered, the token failure's class, never the integrity refusal. The
// record names the refused hop.
func TestNativeForward_ATokenEndpointRedirectIsAnsweredAsAnUnreachableTokenEndpoint(t *testing.T) {
	for _, level := range allLevels {
		for _, sameOrigin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/same-origin=%v", level, sameOrigin), func(t *testing.T) {
				var systemHits, otherHits atomic.Int32
				system := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					systemHits.Add(1)
					http.Error(w, "unexpected", http.StatusTeapot)
				}))
				t.Cleanup(system.Close)
				other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					otherHits.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"access_token":"AT-LEAKED","token_type":"bearer","expires_in":300}`)
				}))
				t.Cleanup(other.Close)
				var tokenSrv *httptest.Server
				tokenSrv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if sameOrigin {
						// The redirect's target is this endpoint's /token2:
						// a request to it is a hop that was followed.
						if r.URL.Path == "/token2" {
							otherHits.Add(1)
						}
						http.Redirect(w, r, tokenSrv.URL+"/token2", http.StatusTemporaryRedirect)
						return
					}
					http.Redirect(w, r, other.URL+"/token", http.StatusPermanentRedirect)
				}))
				t.Cleanup(tokenSrv.Close)
				rec, ans := redirectExchange(t, level, tokenClientFor(t, tokenSrv.URL+"/token", tokenSrv), system.URL)
				if systemHits.Load() != 0 || otherHits.Load() != 0 {
					t.Fatalf("the system received %d requests and the redirect's target %d; want none", systemHits.Load(), otherHits.Load())
				}
				// The same leg with a token endpoint that does not answer.
				dead := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				deadURL := dead.URL
				dead.Close()
				deadRec, deadAns := redirectExchange(t, level, tokenClientFor(t, deadURL+"/token", tokenSrv), system.URL)
				if ans.status != deadAns.status || string(ans.body) != string(deadAns.body) || ans.header.Get(HubDeliveredHeader) != deadAns.header.Get(HubDeliveredHeader) {
					t.Fatalf("answer %d %s (delivered %q), want the unreachable token endpoint's %d %s (delivered %q)",
						ans.status, ans.body, ans.header.Get(HubDeliveredHeader), deadAns.status, deadAns.body, deadAns.header.Get(HubDeliveredHeader))
				}
				if rec.Rule == RefusalIntegrity || rec.Backend == nil || rec.Backend.ErrorClass != BackendAuth || deadRec.Backend == nil || rec.Backend.ErrorClass != deadRec.Backend.ErrorClass {
					t.Fatalf("record rule %q backend %+v, want the unreachable token endpoint's %+v", rec.Rule, rec.Backend, deadRec.Backend)
				}
				if want := []string{smartauth.RedirectTokenRefused}; !slices.Equal(rec.BackendRedirects, want) {
					t.Fatalf("redirects %q, want %q", rec.BackendRedirects, want)
				}
			})
		}
	}
}
