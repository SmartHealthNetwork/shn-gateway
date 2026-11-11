//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// bodyFamily is one family of request that carries a body to a participant's
// own system through the native forward: the leg, the context its handler
// needs, and a body it forwards.
type bodyFamily struct {
	name string
	leg  string
	ctx  func(context.Context) context.Context
	body func(t *testing.T) []byte
}

// answerListing answers a CRD leg's CDS service listing read, a bodiless GET
// the forward makes before it, and reports whether r was that read.
func answerListing(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"services":[{"id":"shn-order-select","hook":"order-sign"},{"id":"order-sign-crd","hook":"order-sign"}]}`)
	return true
}

func bodyFamilies() []bodyFamily {
	plain := func(ctx context.Context) context.Context { return ctx }
	return []bodyFamily{
		{"PAS submission", "pas-claim", plain, func(t *testing.T) []byte { return originatorBuiltConformantBundle(t, "MBR-COVERED") }},
		{"CRD hook", "crd-order-select", plain, func(*testing.T) []byte { return cdsRequest("order-sign") }},
		{"DTR questionnaire package", "dtr-questionnaire-fetch", dtrPkgCtx, func(*testing.T) []byte { return dtrFetchReq }},
	}
}

// Every family of request that carries a body follows no redirect to another
// origin, at every level: the other origin receives nothing, and the forward
// fails as the cross-origin refusal naming that origin, with neither the
// failure nor this gateway's log line carrying the redirect's path or query;
// the log line names the status and origin for this gateway's operator. A
// redirect within the system's own origin is followed, the body replayed.
// Every family reaches the system through the one native call (n.post),
// eligibility and the CDex requests among them, and fails through it.
func TestNativeForward_EveryBodyFamilyFollowsNoCrossOriginRedirect(t *testing.T) {
	for _, level := range allLevels {
		for _, f := range bodyFamilies() {
			t.Run(fmt.Sprintf("%v/%s/cross-origin", level, f.name), func(t *testing.T) {
				logs := captureLog(t)
				var otherHits atomic.Int32
				other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					otherHits.Add(1)
					_, _ = io.ReadAll(r.Body)
				}))
				t.Cleanup(other.Close)
				var systemHits atomic.Int32
				system := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if answerListing(w, r) {
						return
					}
					systemHits.Add(1)
					_, _ = io.ReadAll(r.Body)
					http.Redirect(w, r, other.URL+r.URL.Path+"?patient=x", http.StatusPermanentRedirect)
				}))
				t.Cleanup(system.Close)
				n := NewNativeResponder(system.Client(), system.URL, fixedClock, WithCRDService("shn-order-select"), WithConformancePolicy(NewConformancePolicy(level)))
				n.bindFindingEmitter(func(ConformanceFinding) {})
				_, err := n.Handle(f.ctx(context.Background()), f.leg, "corr", "PCI-1", f.body(t))
				var uf *upstreamFailure
				if !errors.As(err, &uf) || uf.crossOriginRedirect != other.URL || !uf.sent {
					t.Fatalf("err %v, want the cross-origin refusal naming %s, sent", err, other.URL)
				}
				if strings.Contains(err.Error(), "patient=") {
					t.Fatalf("failure %q carries the redirect's query", err)
				}
				line := logs.String()
				if !strings.Contains(line, "a redirect to another origin ("+other.URL+")") || !strings.Contains(line, "answered 308") || strings.Contains(line, "patient=") {
					t.Fatalf("log %q, want the status and origin only", line)
				}
				if otherHits.Load() != 0 || systemHits.Load() != 1 {
					t.Fatalf("requests: other origin %d, system %d; want none and 1", otherHits.Load(), systemHits.Load())
				}
			})
			t.Run(fmt.Sprintf("%v/%s/same-origin", level, f.name), func(t *testing.T) {
				var bodies atomic.Int32
				var system *httptest.Server
				system = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if answerListing(w, r) {
						return
					}
					if b, _ := io.ReadAll(r.Body); len(b) > 0 {
						bodies.Add(1)
					}
					if r.URL.Query().Get("moved") == "" {
						http.Redirect(w, r, system.URL+r.URL.Path+"?moved=1", http.StatusTemporaryRedirect)
						return
					}
					w.WriteHeader(http.StatusTeapot)
				}))
				t.Cleanup(system.Close)
				n := NewNativeResponder(system.Client(), system.URL, fixedClock, WithCRDService("shn-order-select"), WithConformancePolicy(NewConformancePolicy(level)))
				n.bindFindingEmitter(func(ConformanceFinding) {})
				_, err := n.Handle(f.ctx(context.Background()), f.leg, "corr", "PCI-1", f.body(t))
				var uf *upstreamFailure
				if errors.As(err, &uf) && uf.crossOriginRedirect != "" {
					t.Fatalf("a same-origin redirect was refused: %v", err)
				}
				if bodies.Load() != 2 {
					t.Fatalf("the system received the body %d times, want 2 (the redirect followed with its body)", bodies.Load())
				}
			})
		}
	}
}

// The provider's $populate request carries a body too: its engine's redirect
// to another origin is not followed, the other origin receives nothing, and
// the populate fails at its transport with the reason cross_origin_refused
// and the redirect's status, for the provider's operator to read.
func TestNativePopulate_ACrossOriginRedirectIsNotFollowed(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		_, _ = io.ReadAll(r.Body)
	}))
	t.Cleanup(other.Close)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(engine.Close)
	n := NewNativePopulator(engine.Client(), engine.URL+"/Questionnaire/$populate")
	_, _, failure := n.post(context.Background(), []byte(`{"resourceType":"Parameters"}`))
	want := PopulateFailure{Stage: populateStageTransport, Reason: populateReasonCrossOriginRedirect, Status: http.StatusTemporaryRedirect}
	if failure == nil || *failure != want {
		t.Fatalf("failure %+v, want %+v", failure, want)
	}
	if otherHits.Load() != 0 {
		t.Fatalf("the other origin received %d requests, want none", otherHits.Load())
	}
}

// A PAS update (an amendment, bound to the pend it follows up) carries a body
// too, and follows no redirect to another origin: the other origin receives
// nothing, and the forward fails as the cross-origin refusal.
func TestNativeForward_APASUpdateFollowsNoCrossOriginRedirect(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		_, _ = io.ReadAll(r.Body)
	}))
	t.Cleanup(other.Close)
	system := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(system.Close)
	n, ctx, _, bundle, pci, _ := updateRelayLeg(t, system)
	_, err := n.Handle(ctx, "pas-claim-update", "corr-upd", pci, bundle)
	var uf *upstreamFailure
	if !errors.As(err, &uf) || uf.crossOriginRedirect != other.URL || !uf.sent {
		t.Fatalf("err %v, want the cross-origin refusal naming %s, sent", err, other.URL)
	}
	if otherHits.Load() != 0 {
		t.Fatalf("the other origin received %d requests, want none", otherHits.Load())
	}
}
