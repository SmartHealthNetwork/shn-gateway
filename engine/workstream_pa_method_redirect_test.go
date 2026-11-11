//go:build !shn_coreonly

package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// movedBody is what the payer's system answers with its redirect.
const movedBody = `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"moved"}]}`

// methodRedirectSystem answers each request carrying a body with code to
// location (relative to the system), with movedBody, and answers whatever
// reaches location with followed; it counts both. A CRD leg's service
// listing read is answered as a listing.
type methodRedirectSystem struct {
	*httptest.Server
	posts, followed atomic.Int32
}

func newMethodRedirectSystem(t *testing.T, code int, location string, followed func(w http.ResponseWriter)) *methodRedirectSystem {
	t.Helper()
	s := &methodRedirectSystem{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == location {
			s.followed.Add(1)
			followed(w)
			return
		}
		if answerListing(w, r) {
			return
		}
		s.posts.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Location", s.URL+location)
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, movedBody)
	}))
	t.Cleanup(s.Close)
	return s
}

// Every family of request that carries a body, at every level: a 301, 302 or
// 303 within the payer's own origin, which would turn the request into a GET
// with no body, is not followed. The system received the one request, the
// address it named receives nothing, and the leg answers with the system's
// own 3xx and its body, carried as the participant's answer, with no refusal
// of this gateway's. The operator's log line names the status, never the
// address.
func TestNativeForward_AMethodChangingRedirectIsCarriedNotFollowed(t *testing.T) {
	for _, level := range allLevels {
		for _, f := range bodyFamilies() {
			for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
				t.Run(fmt.Sprintf("%v/%s/%d", level, f.name, code), func(t *testing.T) {
					logs := captureLog(t)
					system := newMethodRedirectSystem(t, code, "/moved/here", func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) })
					n := NewNativeResponder(system.Client(), system.URL, fixedClock, WithCRDService("shn-order-select"), WithConformancePolicy(NewConformancePolicy(level)))
					n.bindFindingEmitter(func(ConformanceFinding) {})
					res, err := n.Handle(f.ctx(context.Background()), f.leg, "corr", "PCI-1", f.body(t))
					if err != nil {
						t.Fatalf("err %v, want the system's %d carried as its answer", err, code)
					}
					if res.Status != code || !res.ResponseRelayed() || string(relay.BytesForTest(res.Response)) != movedBody {
						t.Fatalf("leg answer %d relayed %v %s, want the system's %d and its body", res.Status, res.ResponseRelayed(), relay.BytesForTest(res.Response), code)
					}
					if system.posts.Load() != 1 || system.followed.Load() != 0 {
						t.Fatalf("requests: system %d, redirect target %d; want 1 and none", system.posts.Load(), system.followed.Load())
					}
					line := logs.String()
					if !strings.Contains(line, fmt.Sprintf("your system answered %d to a request that carries a body", code)) || strings.Contains(line, "/moved/here") || strings.Contains(line, system.URL) {
						t.Fatalf("log %q, want the status only", line)
					}
				})
			}
		}
	}
}

// The login wall: a payer's front end that answers a CRD hook POST with a
// 302 to an HTML sign-in page. The page is never requested, and the
// requester gets the system's 302, never a 200 with the page's HTML.
func TestNativeForward_ALoginWallRedirectIsCarriedAsTheSystems302(t *testing.T) {
	const page = "<html><body><form action=\"/login\">Sign in</form></body></html>"
	system := newMethodRedirectSystem(t, http.StatusFound, "/login", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, page)
	})
	n := NewNativeResponder(system.Client(), system.URL, fixedClock, WithCRDService("shn-order-select"), WithConformancePolicy(NewConformancePolicy(EnforcementNone)))
	n.bindFindingEmitter(func(ConformanceFinding) {})
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "PCI-1", cdsRequest("order-sign"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusFound || strings.Contains(string(relay.BytesForTest(res.Response)), "Sign in") {
		t.Fatalf("leg answer %d %s, want the system's 302, never the sign-in page", res.Status, relay.BytesForTest(res.Response))
	}
	if system.followed.Load() != 0 {
		t.Fatalf("the sign-in page was requested %d times, want never", system.followed.Load())
	}
}

// Through the payer's gateway, at every level: the requester's answer is the
// system's own 3xx and its body, the record names the system's status,
// http-3xx and the hop not followed, and nothing else was sent.
func TestNativeForward_AMethodChangingRedirectReachesTheRequesterAtEveryLevel(t *testing.T) {
	for _, level := range allLevels {
		for _, code := range []int{http.StatusFound, http.StatusSeeOther} {
			t.Run(fmt.Sprintf("%v/%d", level, code), func(t *testing.T) {
				var system *recordingOrigin
				system = newRecordingOrigin(t, func(r *http.Request, w http.ResponseWriter) {
					if r.URL.Path == "/moved" {
						w.WriteHeader(http.StatusOK)
						return
					}
					w.Header().Set("Location", system.URL+"/moved")
					w.Header().Set("Content-Type", "application/fhir+json")
					w.WriteHeader(code)
					_, _ = io.WriteString(w, movedBody)
				})
				rec, ans := configuredRedirectExchange(t, level, smartClientFor(t, system.Server), system.URL)
				if got := system.requests(); len(got) != 1 {
					t.Fatalf("the payer's system received %d requests, want 1", len(got))
				}
				// Framed: the Hub sees the payer gateway's 200 frame, and the
				// requester reads the system's 3xx inside it. A bare 3xx would
				// reach the Hub as a failure, never as the system's answer.
				if !ans.framed || ans.status != code || !strings.Contains(string(ans.body), "moved") {
					t.Fatalf("answer framed %v %d %s, want the system's %d and its body, framed", ans.framed, ans.status, ans.body, code)
				}
				if rec.Backend == nil || rec.Backend.Status != code || rec.Backend.ErrorClass != BackendHTTP3xx {
					t.Fatalf("backend %+v, want the system's %d, %s", rec.Backend, code, BackendHTTP3xx)
				}
				if want := []string{smartauth.RedirectMethodChangeRefused}; !slices.Equal(rec.BackendRedirects, want) {
					t.Fatalf("redirects %q, want %q", rec.BackendRedirects, want)
				}
				// Carried, never refused: no rule of this gateway's decided it.
				if rec.Outcome == ExchangeRefused || rec.Rule != "" {
					t.Fatalf("record outcome %q rule %q, want the system's answer carried, no refusal", rec.Outcome, rec.Rule)
				}
			})
		}
	}
}

// A 307 within the origin keeps the request, and is followed with its body.
func TestNativeForward_A307WithinTheOriginIsStillFollowed(t *testing.T) {
	var bodies atomic.Int32
	var system *httptest.Server
	system = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerListing(w, r) {
			return
		}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 && r.Method == http.MethodPost {
			bodies.Add(1)
		}
		if r.URL.Query().Get("moved") == "" {
			http.Redirect(w, r, system.URL+r.URL.Path+"?moved=1", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(system.Close)
	n := NewNativeResponder(system.Client(), system.URL, fixedClock, WithCRDService("shn-order-select"), WithConformancePolicy(NewConformancePolicy(EnforcementNone)))
	n.bindFindingEmitter(func(ConformanceFinding) {})
	res, err := n.Handle(context.Background(), "crd-order-select", "corr", "PCI-1", cdsRequest("order-sign"))
	if err != nil {
		t.Fatal(err)
	}
	if bodies.Load() != 2 || res.Status != http.StatusTeapot {
		t.Fatalf("bodies %d, answer %d; want the POST twice and the final answer", bodies.Load(), res.Status)
	}
}

// The provider's $populate request: its engine's 303 within the origin is not
// followed, and the populate fails as any answer that is not a 2xx does, with
// the redirect's status.
func TestNativePopulate_AMethodChangingRedirectIsNotFollowed(t *testing.T) {
	engine := newMethodRedirectSystem(t, http.StatusSeeOther, "/result", func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) })
	n := NewNativePopulator(engine.Client(), engine.URL+"/Questionnaire/$populate")
	_, _, failure := n.post(context.Background(), []byte(`{"resourceType":"Parameters"}`))
	want := PopulateFailure{Stage: populateStageHTTPStatus, Reason: populateReasonNon2xx, Status: http.StatusSeeOther}
	if failure == nil || *failure != want {
		t.Fatalf("failure %+v, want %+v", failure, want)
	}
	if engine.followed.Load() != 0 || engine.posts.Load() != 1 {
		t.Fatalf("requests: engine %d, redirect target %d; want 1 and none", engine.posts.Load(), engine.followed.Load())
	}
}
