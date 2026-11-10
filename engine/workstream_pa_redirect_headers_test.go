//go:build !shn_coreonly

package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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

// A payer's system that redirects the forward to another origin: the hop is
// followed, and the other origin receives none of the headers the payer
// configured for its own system, nor Authorization, at every level. The
// payer's own system received them all; the hop is recorded as cross-origin,
// and the other origin's answer is the answer. A configured header is the
// payer's credential or routing for its own origin: carrying it to another
// would hand SHN's carry of it to a party it was never configured for
// (message integrity, and the authentication of the gateway's own carry).
func TestNativeForward_ACrossOriginRedirectCarriesNoConfiguredHeaderAtEveryLevel(t *testing.T) {
	answer := decidedAnswer(t)
	for _, level := range allLevels {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			other := newRecordingOrigin(t, func(_ *http.Request, w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/fhir+json")
				_, _ = w.Write(answer)
			})
			system := newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
				http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
			})
			rec, ans := configuredRedirectExchange(t, level, smartClientFor(t, system.Server), system.URL)
			if ans.status != http.StatusOK {
				t.Fatalf("answer %d %s, want the other origin's answer", ans.status, ans.body)
			}
			sent, away := system.requests(), other.requests()
			if len(sent) != 1 || len(away) != 1 {
				t.Fatalf("requests: system %d, other origin %d; want one each", len(sent), len(away))
			}
			requirePartnerHeaders(t, "the payer's own system", sent[0])
			requireBearer(t, "the payer's own system", sent[0])
			requireNoPartnerHeader(t, "the other origin", away[0])
			if want := []string{smartauth.RedirectCrossOrigin}; !slices.Equal(rec.BackendRedirects, want) {
				t.Fatalf("redirects %q, want %q", rec.BackendRedirects, want)
			}
		})
	}
}

// A forward the payer's system redirects to another origin that redirects it
// back (A to B to A) arrives back at the payer's system with no configured
// header and no Authorization, at every level: once the chain has left the
// origin, another origin chose where it goes, so nothing configured for the
// origin is sent for the rest of the chain. The payer's system answers that
// request as it chooses; here it answers, and its answer is the answer.
func TestNativeForward_AChainBackToTheOriginCarriesNoConfiguredHeaderAtEveryLevel(t *testing.T) {
	answer := decidedAnswer(t)
	for _, level := range allLevels {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			var system *recordingOrigin
			other := newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
				http.Redirect(w, r, system.URL+r.URL.Path+"?back=1", http.StatusTemporaryRedirect)
			})
			system = newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
				if r.URL.Query().Get("back") == "" {
					http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "application/fhir+json")
				_, _ = w.Write(answer)
			})
			rec, ans := configuredRedirectExchange(t, level, smartClientFor(t, system.Server), system.URL)
			if ans.status != http.StatusOK {
				t.Fatalf("answer %d %s, want the payer's system's answer", ans.status, ans.body)
			}
			got, away := system.requests(), other.requests()
			if len(got) != 2 || len(away) != 1 {
				t.Fatalf("requests: system %d, other origin %d; want 2 and 1", len(got), len(away))
			}
			requirePartnerHeaders(t, "the payer's system, first", got[0])
			requireBearer(t, "the payer's system, first", got[0])
			requireNoPartnerHeader(t, "the other origin", away[0])
			requireNoPartnerHeader(t, "the payer's system, on the hop back", got[1])
			if want := []string{smartauth.RedirectCrossOrigin, smartauth.RedirectSameOrigin}; !slices.Equal(rec.BackendRedirects, want) {
				t.Fatalf("redirects %q, want %q", rec.BackendRedirects, want)
			}
		})
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
// them to another origin it is redirected to.
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
}
