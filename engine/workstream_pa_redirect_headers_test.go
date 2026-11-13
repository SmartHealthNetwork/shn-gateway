//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
)

// partnerHeadersForTest are a payer's fixed request headers for its own
// system (PAYER_DAVINCI_BACKEND_HEADERS): one routes, one holds a key. Their
// names are ones no list of credential names would know.
func partnerHeadersForTest() http.Header {
	return http.Header{"X-Route-Tenant": {"tenant-7"}, "X-Partner-Key": {"OPAQUE-PARTNER-KEY"}}
}

// recordingOrigin is an https server that answers body and keeps the headers
// of each request it receives.
type recordingOrigin struct {
	*httptest.Server
	mu   sync.Mutex
	seen []http.Header
}

func newRecordingOrigin(t *testing.T, answer func(r *http.Request, w http.ResponseWriter)) *recordingOrigin {
	t.Helper()
	o := &recordingOrigin{}
	o.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		o.mu.Lock()
		o.seen = append(o.seen, r.Header.Clone())
		o.mu.Unlock()
		answer(r, w)
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *recordingOrigin) requests() []http.Header {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.seen)
}

// requireNoPartnerHeader fails when h carries a configured header or
// Authorization.
func requireNoPartnerHeader(t *testing.T, who string, h http.Header) {
	t.Helper()
	for k := range partnerHeadersForTest() {
		if v, ok := h[k]; ok {
			t.Errorf("%s received %s %q, want none", who, k, v)
		}
	}
	if v, ok := h["Authorization"]; ok {
		t.Errorf("%s received Authorization %q, want none", who, v)
	}
}

// requirePartnerHeaders fails when h lacks a configured header.
func requirePartnerHeaders(t *testing.T, who string, h http.Header) {
	t.Helper()
	for k, v := range partnerHeadersForTest() {
		if h.Get(k) != v[0] {
			t.Errorf("%s received %s %q, want %q", who, k, h.Get(k), v[0])
		}
	}
}

// smartClientFor is the payer's SMART client for its system srv: every
// request it sends carries the bearer its token endpoint issued, as a payer
// with a credential block sends it.
func smartClientFor(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok-redirect","token_type":"bearer","expires_in":300}`)
	}))
	t.Cleanup(tok.Close)
	hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tok.URL, ClientID: "c1", ClientSecret: "s1", Scope: "x", Transport: srv.Client().Transport})
	if err != nil {
		t.Fatal(err)
	}
	return hc
}

// requireBearer fails when h does not carry the bearer smartClientFor's
// token endpoint issued.
func requireBearer(t *testing.T, who string, h http.Header) {
	t.Helper()
	if got := h.Get("Authorization"); got != "Bearer tok-redirect" {
		t.Errorf("%s received Authorization %q, want the payer's bearer", who, got)
	}
}

// configuredRedirectExchange delivers one inquiry, at level, to a payer
// gateway configured with partnerHeadersForTest, whose native forward reaches
// system through client.
func configuredRedirectExchange(t *testing.T, level ConformanceEnforcement, client *http.Client, system string) (ExchangeRecord, payerAnswer) {
	t.Helper()
	p := newLevelPayer(t, level)
	n := NewNativeResponder(client, system, fixedClock,
		WithCRDService("shn-order-select"), WithPASStore(p.store), WithConformancePolicy(NewConformancePolicy(level)),
		WithBackendHeaders(partnerHeadersForTest()))
	n.bindFindingEmitter(p.g.emitFinding)
	p.g.cfg.Responder = n
	var records exchangeRecords
	p.g.cfg.ExchangeObserved = records.observe
	ans := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	return records.only(t), ans
}

// A payer's system that redirects the forward, a request that carries a body,
// to another origin: the hop is not sent, whatever the redirect's code, at
// every level. The other origin receives nothing at all, the payer's own
// system received the request with its configured headers and bearer, and
// the forward is refused 502 on routing, attested delivered (the system
// answered, with its 3xx, and may have acted), its reason naming the origin
// only so the recipient's operator can point its registered endpoint at the
// final address. The record names the system's 3xx, http-3xx and the refused
// hop.
func TestNativeForward_ACrossOriginRedirectIsRefusedAsRoutingAtEveryLevel(t *testing.T) {
	answer := decidedAnswer(t)
	for _, level := range allLevels {
		for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
			t.Run(fmt.Sprintf("%v/%d", level, code), func(t *testing.T) {
				other := newRecordingOrigin(t, func(_ *http.Request, w http.ResponseWriter) {
					w.Header().Set("Content-Type", "application/fhir+json")
					_, _ = w.Write(answer)
				})
				system := newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
					http.Redirect(w, r, other.URL+r.URL.Path+"?patient=x", code)
				})
				rec, ans := configuredRedirectExchange(t, level, smartClientFor(t, system.Server), system.URL)
				sent, away := system.requests(), other.requests()
				if len(sent) != 1 || len(away) != 0 {
					t.Fatalf("requests: system %d, other origin %d; want 1 and none", len(sent), len(away))
				}
				requirePartnerHeaders(t, "the payer's own system", sent[0])
				requireBearer(t, "the payer's own system", sent[0])
				if ans.status != http.StatusBadGateway || ans.header.Get(HubDeliveredHeader) != "yes" {
					t.Fatalf("answer %d (delivered %q) %s, want 502 attested delivered", ans.status, ans.header.Get(HubDeliveredHeader), ans.body)
				}
				var got map[string]string
				if err := json.Unmarshal(ans.body, &got); err != nil {
					t.Fatalf("answer %s: %v", ans.body, err)
				}
				want := map[string]string{"error": fmt.Sprintf(errCrossOriginRedirectFor, other.URL), "rule": RefusalRouting}
				if got["error"] != want["error"] || got["rule"] != want["rule"] || len(got) != 2 {
					t.Fatalf("answer %q, want %q", got, want)
				}
				if strings.Contains(string(ans.body), "patient=") {
					t.Fatalf("answer %s carries the redirect's query", ans.body)
				}
				if rec.Outcome != ExchangeRefused || rec.Rule != RefusalRouting {
					t.Fatalf("record outcome %q rule %q, want refused on routing", rec.Outcome, rec.Rule)
				}
				if rec.Backend == nil || rec.Backend.Status != code || rec.Backend.ErrorClass != BackendHTTP3xx {
					t.Fatalf("backend %+v, want the system's %d, %s", rec.Backend, code, BackendHTTP3xx)
				}
				if want := []string{smartauth.RedirectCrossOriginRefused}; !slices.Equal(rec.BackendRedirects, want) {
					t.Fatalf("redirects %q, want %q", rec.BackendRedirects, want)
				}
			})
		}
	}
}

// A redirect within the payer's own origin (A to A) keeps every configured
// header: they were configured for that origin.
func TestNativeForward_ASameOriginRedirectKeepsTheConfiguredHeaders(t *testing.T) {
	answer := decidedAnswer(t)
	var system *recordingOrigin
	system = newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
		if r.URL.Query().Get("moved") == "" {
			http.Redirect(w, r, system.URL+r.URL.Path+"?moved=1", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write(answer)
	})
	rec, ans := configuredRedirectExchange(t, EnforcementNone, smartClientFor(t, system.Server), system.URL)
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s, want 200", ans.status, ans.body)
	}
	got := system.requests()
	if len(got) != 2 {
		t.Fatalf("the payer's system received %d requests, want 2", len(got))
	}
	for i, h := range got {
		requirePartnerHeaders(t, fmt.Sprintf("hop %d to the payer's own origin", i), h)
		requireBearer(t, fmt.Sprintf("hop %d to the payer's own origin", i), h)
	}
	if want := []string{smartauth.RedirectSameOrigin}; !slices.Equal(rec.BackendRedirects, want) {
		t.Fatalf("redirects %q, want %q", rec.BackendRedirects, want)
	}
}

// With configured headers, a redirect of the forward from https to http is
// still refused before the hop is sent, at every level, on message integrity.
func TestNativeForward_WithConfiguredHeadersAnHTTPSToHTTPRedirectIsStillRefused(t *testing.T) {
	for _, level := range allLevels {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			plain, hits := plainOrigin(t)
			system := redirectingSystem(t, http.StatusTemporaryRedirect, func() string { return plain.URL })
			rec, ans := configuredRedirectExchange(t, level, smartClientFor(t, system), system.URL)
			if hits.Load() != 0 {
				t.Fatalf("the http origin received %d requests, want none", hits.Load())
			}
			if ans.status != http.StatusBadGateway || rec.Rule != RefusalIntegrity {
				t.Fatalf("answer %d, rule %q; want 502 on integrity", ans.status, rec.Rule)
			}
			if want := []string{smartauth.RedirectCleartextRefused}; !slices.Equal(rec.BackendRedirects, want) {
				t.Fatalf("redirects %q, want %q", rec.BackendRedirects, want)
			}
		})
	}
}

// The CDS service listing read, through the payer's SMART client, carries the
// configured headers and the bearer to the payer's own system, and none of
// them to another origin it is redirected to, which receives only what a
// fresh read of its address carries.
func TestCDSServiceListing_ACrossOriginRedirectCarriesNoConfiguredHeader(t *testing.T) {
	other := newRecordingOrigin(t, func(_ *http.Request, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"services":[{"id":"shn-order-select","hook":"order-select"}]}`)
	})
	system := newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	})
	services, err := DiscoverCDSServices(context.Background(), smartClientFor(t, system.Server), system.URL, partnerHeadersForTest())
	if err != nil || len(services) != 1 {
		t.Fatalf("services %v, err %v; want the other origin's listing", services, err)
	}
	sent, away := system.requests(), other.requests()
	if len(sent) != 1 || len(away) != 1 {
		t.Fatalf("requests: system %d, other origin %d; want one each", len(sent), len(away))
	}
	requirePartnerHeaders(t, "the payer's own system", sent[0])
	requireBearer(t, "the payer's own system", sent[0])
	requireNoPartnerHeader(t, "the other origin", away[0])
	requireOnlyFreshReadHeaders(t, "the other origin", "application/json", away[0])
}

// requireOnlyFreshReadHeaders fails unless h, as another origin received it,
// holds only what a fresh read of that address carries: Accept as the
// request set it, and the User-Agent and Accept-Encoding the transport adds.
func requireOnlyFreshReadHeaders(t *testing.T, who, accept string, h http.Header) {
	t.Helper()
	if got := h.Values("Accept"); len(got) != 1 || got[0] != accept {
		t.Errorf("%s received Accept %q, want %q", who, got, accept)
	}
	for k, v := range h {
		switch k {
		case "Accept", "User-Agent", "Accept-Encoding":
		default:
			t.Errorf("%s received %s %q, want only Accept and the transport's User-Agent and Accept-Encoding", who, k, v)
		}
	}
}

// The client a payer gateway's native responder reaches its system with, used
// for a read (a GET with no body) carrying every header a forward sets (the
// configured headers, the bearer, the requester, the leg's correlation id and
// an edit's disclosure): a redirect to another origin is sent there without
// any of them, and so is the hop back to the payer's own system.
func TestNativeResponder_ACrossOriginReadCarriesNoHeaderSetForTheSystem(t *testing.T) {
	var system *recordingOrigin
	other := newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
		http.Redirect(w, r, system.URL+"/Task/back", http.StatusFound)
	})
	system = newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
		if r.URL.Path == "/Task/1" {
			http.Redirect(w, r, other.URL+"/Task/1", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = io.WriteString(w, `{"resourceType":"Task","id":"1"}`)
	})
	n := NewNativeResponder(smartClientFor(t, system.Server), system.URL, fixedClock, WithBackendHeaders(partnerHeadersForTest()))
	ctx := withResponderCorrelation(context.Background(), "leg-0123")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, system.URL+"/Task/1", nil)
	req.Header.Set("Accept", "application/fhir+json")
	n.applyBackendHeaders(req)
	req.Header.Set(RequesterHeader, "payer-net-id")
	req.Header.Set(EditedHeader, "identity-mapping")
	setBackendCorrelation(ctx, req)
	resp, err := n.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want the system's answer at the end of the chain", resp.StatusCode)
	}
	sent, away := system.requests(), other.requests()
	if len(sent) != 2 || len(away) != 1 {
		t.Fatalf("requests: system %d, other origin %d; want 2 and 1", len(sent), len(away))
	}
	requirePartnerHeaders(t, "the payer's own system", sent[0])
	requireBearer(t, "the payer's own system", sent[0])
	for _, k := range []string{RequesterHeader, EditedHeader, CorrelationHeader} {
		if sent[0].Get(k) == "" {
			t.Errorf("the payer's own system received no %s", k)
		}
	}
	requireOnlyFreshReadHeaders(t, "the other origin", "application/fhir+json", away[0])
	requireOnlyFreshReadHeaders(t, "the hop back to the payer's own system", "application/fhir+json", sent[1])
}

// A provider's gateway reading a payer's CDex Task (cdex-task-status) from
// the provider's own Task endpoint, which redirects the read to another
// origin that redirects it back, at every level: the provider's system
// receives the read with the requester, the leg's correlation id and the
// bearer; the other origin, and the hop back to the provider's system, each
// receive only what a fresh read of its address carries. The Task at the end
// of the chain names its member by number, so it is delivered as the read's
// rule defines it, under the patient that number binds.
func TestCDexTaskStatus_ACrossOriginRedirectCarriesOnlyTheKeptHeaders(t *testing.T) {
	task := readTask(t, nil)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			var system *recordingOrigin
			other := newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
				http.Redirect(w, r, system.URL+"/fhir/Task/back", http.StatusFound)
			})
			system = newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
				if r.URL.Path == "/fhir/Task/t-1" {
					http.Redirect(w, r, other.URL+"/fhir/Task/t-1", http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "application/fhir+json")
				_, _ = w.Write(task)
			})
			client := smartClientFor(t, system.Server)
			p := newReadProvider(t, level, newReadTaskSystem(t), nil, func(c *Config) {
				c.ProviderCDexTaskURL = system.URL + "/fhir/Task"
				c.ProviderCDexTaskClient = client
				c.ProviderCDexReadClient = client
			})
			ans := p.readOf(t, "Task/t-1")
			if !ans.framed || ans.status != http.StatusOK || !bytes.Equal(ans.body, task) {
				t.Fatalf("answer %d (framed %v) %s, want the Task at the end of the chain", ans.status, ans.framed, ans.body)
			}
			if want := boundPCI(t, p, "Member123", task); ans.subject != want {
				t.Errorf("the answer's token names %q, want the bound patient %q", ans.subject, want)
			}
			sent, away := system.requests(), other.requests()
			if len(sent) != 2 || len(away) != 1 {
				t.Fatalf("requests: provider's system %d, other origin %d; want 2 and 1", len(sent), len(away))
			}
			requireBearer(t, "the provider's own system", sent[0])
			for _, k := range []string{RequesterHeader, CorrelationHeader} {
				if sent[0].Get(k) == "" {
					t.Errorf("the provider's own system received no %s", k)
				}
			}
			requireOnlyFreshReadHeaders(t, "the other origin", "application/fhir+json", away[0])
			requireOnlyFreshReadHeaders(t, "the hop back to the provider's own system", "application/fhir+json", sent[1])
		})
	}
}
