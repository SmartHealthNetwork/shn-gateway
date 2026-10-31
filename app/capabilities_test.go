//go:build !shn_coreonly

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	health "github.com/SmartHealthNetwork/shn-sdk/health"
)

// TestConvergeRegistry_CarriesDeclaredCapabilities: the feed's list and limit
// reach the registry, and a list absent from the feed stays absent, not empty.
func TestConvergeRegistry_CarriesDeclaredCapabilities(t *testing.T) {
	limit := int64(4 << 20)
	var enc [32]byte
	enc[0] = 7
	holder := func(id string) shnsdk.Holder {
		return shnsdk.Holder{ID: id, Role: "payer", EncPub: base64.StdEncoding.EncodeToString(enc[:]), BaseURL: "https://" + id + ".example"}
	}
	declared, none, unset := holder("declared"), holder("none"), holder("unset")
	declared.Legs, declared.AttachmentMaxBytes = []string{"cdex-attachment-submit"}, &limit
	none.Legs = []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]shnsdk.Holder{declared, none, unset})
	}))
	defer srv.Close()

	reg := shnsdk.NewRegistry()
	if _, err := convergeRegistry(context.Background(), http.DefaultClient, srv.URL, reg); err != nil {
		t.Fatalf("convergeRegistry: %v", err)
	}
	e, _ := reg.Lookup("declared")
	if !slices.Equal(e.Legs, declared.Legs) || e.AttachmentMaxBytes == nil || *e.AttachmentMaxBytes != limit {
		t.Fatalf("declared entry = %q limit %v", e.Legs, e.AttachmentMaxBytes)
	}
	if e, _ := reg.Lookup("none"); e.Legs == nil || len(e.Legs) != 0 || e.AttachmentMaxBytes != nil {
		t.Fatalf("an empty list read as %#v", e.Legs)
	}
	if e, _ := reg.Lookup("unset"); e.Legs != nil {
		t.Fatalf("an absent list read as %#v", e.Legs)
	}
}

// capabilitiesRegistrar answers PUT /register/{id}/capabilities with the next
// status in statuses, the last one repeating, and counts the requests.
func capabilitiesRegistrar(t *testing.T, statuses ...int) (url string, puts *atomic.Int32, last func() shnsdk.CapabilityDeclaration) {
	t.Helper()
	puts = &atomic.Int32{}
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/register/payer-a/capabilities" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		body.Store(raw)
		i := min(int(puts.Add(1))-1, len(statuses)-1)
		w.WriteHeader(statuses[i])
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, puts, func() shnsdk.CapabilityDeclaration {
		var d shnsdk.CapabilityDeclaration
		_ = json.Unmarshal(body.Load().([]byte), &d)
		return d
	}
}

// registrarTestClient has a transport of its own, as a gateway's registrar
// client always does.
func registrarTestClient() *http.Client {
	return &http.Client{Transport: http.DefaultTransport}
}

func capabilitiesIdentity(t *testing.T) shnsdk.Identity {
	t.Helper()
	id, err := shnsdk.GenerateIdentity("payer-a")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A gateway declares what it compiles for its role, once when the registrar
// takes it. An empty list is sent as one: a provider declares that it takes none.
func TestDeclareCapabilities_SendsTheRolesLegsOnce(t *testing.T) {
	for _, role := range []string{"payer", "provider"} {
		t.Run(role, func(t *testing.T) {
			url, puts, last := capabilitiesRegistrar(t, http.StatusOK)
			d := capabilityDeclaration(config{Role: role})
			if d.AttachmentMaxBytes != nil {
				t.Fatalf("no limit set declares %d, want none: unknown, never a value", *d.AttachmentMaxBytes)
			}
			declareCapabilities(t.Context(), registrarTestClient(), url, capabilitiesIdentity(t), d, time.Millisecond, &declarationCell{})
			if puts.Load() != 1 {
				t.Fatalf("%d declarations, want one", puts.Load())
			}
			got := last()
			if got.Legs == nil || !slices.Equal(got.Legs, d.Legs) {
				t.Fatalf("declared %#v, want %#v", got.Legs, d.Legs)
			}
			if role == "payer" && !slices.Contains(got.Legs, "cdex-attachment-submit") {
				t.Fatalf("a payer declared %q", got.Legs)
			}
			if role == "provider" && len(got.Legs) != 0 {
				t.Fatalf("a provider declared %q", got.Legs)
			}
		})
	}
}

// A test profile's leg is not the network's: the SDK refuses it, and one such
// name would keep the gateway's real legs from being declared at all.
func TestNetworkLegs(t *testing.T) {
	if got := networkLegs([]string{"crd-order-select", "toy-echo"}); !slices.Equal(got, []string{"crd-order-select"}) {
		t.Fatalf("networkLegs = %q", got)
	}
	if got := networkLegs([]string{"toy-echo"}); got == nil || len(got) != 0 {
		t.Fatalf("networkLegs of no network leg = %#v, want an empty list", got)
	}
}

// The wait doubles to its cap and stays there.
func TestNextDeclareRetry(t *testing.T) {
	for retry, want := range map[time.Duration]time.Duration{
		declareCapabilitiesRetry:    2 * declareCapabilitiesRetry,
		6 * time.Minute:             declareCapabilitiesMaxRetry,
		declareCapabilitiesMaxRetry: declareCapabilitiesMaxRetry,
	} {
		if got := nextDeclareRetry(retry, false, time.Hour); got != want {
			t.Errorf("nextDeclareRetry(%s) = %s, want %s", retry, got, want)
		}
		if got := nextDeclareRetry(retry, true, time.Hour); got != time.Hour {
			t.Errorf("once slowed, nextDeclareRetry(%s) = %s, want the hour", retry, got)
		}
	}
}

// A registrar that fails is asked again until it takes the declaration, and
// each wait doubles the last. The gateway's client is left as it was: the feed
// poller is using it.
func TestDeclareCapabilities_RetriesAFailure(t *testing.T) {
	const retry = 20 * time.Millisecond
	url, puts, _ := capabilitiesRegistrar(t, http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusOK)
	client := registrarTestClient()
	start := time.Now()
	declareCapabilities(t.Context(), client, url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, retry, &declarationCell{})
	if puts.Load() != 4 {
		t.Fatalf("%d declarations, want four", puts.Load())
	}
	if elapsed := time.Since(start); elapsed < (1+2+4)*retry {
		t.Fatalf("three retries took %s, want at least %s: the wait did not double", elapsed, (1+2+4)*retry)
	}
	if client.Transport != http.DefaultTransport {
		t.Fatalf("the gateway's client was changed: %T", client.Transport)
	}
}

// A 409 is final, by its status: a resend cannot succeed. Any other refusal is
// sent again.
func TestDeclareCapabilities_AConflictIsFinal(t *testing.T) {
	url, puts, _ := capabilitiesRegistrar(t, http.StatusConflict)
	declareCapabilities(t.Context(), registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Hour, &declarationCell{})
	if puts.Load() != 1 {
		t.Fatalf("%d declarations, want one", puts.Load())
	}

	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusForbidden, http.StatusInternalServerError} {
		url, puts, _ := capabilitiesRegistrar(t, status, http.StatusOK)
		declareCapabilities(t.Context(), registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Millisecond, &declarationCell{})
		if puts.Load() != 2 {
			t.Fatalf("a %d: %d declarations, want two", status, puts.Load())
		}
	}
}

// A registrar older than the declaration has no such route. The gateway goes on
// asking, slowly: one updated later must get the declaration without a restart, or
// the Hub refuses this gateway by name. A holder a moment from registered is
// declared at the short waits.
func TestDeclareCapabilities_AnOlderRegistrarIsAskedUntilItIsUpdated(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		statuses := slices.Repeat([]int{status}, declareCapabilitiesAbsentTries+2)
		url, puts, _ := capabilitiesRegistrar(t, append(statuses, http.StatusOK)...)
		const retry = time.Millisecond
		start := time.Now()
		declareCapabilities(t.Context(), registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, retry, &declarationCell{})
		if got := int(puts.Load()); got != len(statuses)+1 {
			t.Fatalf("a %d: %d declarations, want %d: the registrar updated and took it", status, got, len(statuses)+1)
		}
		if elapsed := time.Since(start); elapsed < 2*declareCapabilitiesSlowFactor*retry {
			t.Fatalf("a %d: the tries past the fourth took %s, want the slow wait", status, elapsed)
		}
	}
	url, puts, _ := capabilitiesRegistrar(t, http.StatusNotFound, http.StatusNotFound, http.StatusOK)
	declareCapabilities(t.Context(), registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Millisecond, &declarationCell{})
	if puts.Load() != 3 {
		t.Fatalf("%d declarations, want three: a holder registered in time is declared", puts.Load())
	}
}

// Whatever follows, the slowing is said once, an outage after it included.
func TestDeclareCapabilities_SaysItSlowedOnce(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(prev)
	nf, down := http.StatusNotFound, http.StatusServiceUnavailable
	url, _, _ := capabilitiesRegistrar(t, nf, nf, nf, nf, down, down, down, http.StatusOK)
	declareCapabilities(t.Context(), registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Microsecond, &declarationCell{})
	if n := strings.Count(logged.String(), "trying hourly"); n != 1 {
		t.Fatalf("said %d times, want once:\n%s", n, logged.String())
	}
	if n := strings.Count(logged.String(), "gateway: capability declaration"); n != 7 {
		t.Fatalf("%d log lines, want three retries, the one that slowed, and the outage's three:\n%s", n, logged.String())
	}

	logged.Reset()
	url, _, _ = capabilitiesRegistrar(t, nf, nf, nf, nf, nf, nf, http.StatusOK)
	declareCapabilities(t.Context(), registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Microsecond, &declarationCell{})
	if n := strings.Count(logged.String(), "gateway: capability declaration"); n != 4 {
		t.Fatalf("%d log lines past the slowing, want it silent:\n%s", n, logged.String())
	}
}

// An outage is not an absent route: its waits stay on the doubling backoff, and
// the declaration lands when it ends.
func TestDeclareCapabilities_AnOutageIsNotAnOlderRegistrar(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(prev)
	statuses := slices.Repeat([]int{http.StatusServiceUnavailable}, declareCapabilitiesAbsentTries+2)
	url, puts, _ := capabilitiesRegistrar(t, append(statuses, http.StatusOK)...)
	declareCapabilities(t.Context(), registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Microsecond, &declarationCell{})
	if got := int(puts.Load()); got != len(statuses)+1 {
		t.Fatalf("%d declarations, want %d", got, len(statuses)+1)
	}
	if strings.Contains(logged.String(), "trying hourly") {
		t.Fatalf("an outage reads as an absent route:\n%s", logged.String())
	}
}

// failTransport fails every request.
type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

// A failed connection clears the last status: a 404 before it must not count again.
func TestAnswerStatus_AFailedConnectionHasNoStatus(t *testing.T) {
	a := &answerStatus{next: failTransport{}, status: http.StatusNotFound}
	if _, err := a.RoundTrip(httptest.NewRequest(http.MethodPut, "http://registrar.invalid/", nil)); err == nil {
		t.Fatal("the failure was lost")
	}
	if a.status != 0 {
		t.Fatalf("status %d after a failed connection", a.status)
	}
}

// An operator reads what the declaration came to from /health, and a declaration
// that did not land never reads as an unhealthy gateway.
func TestDeclarationCell_StatesWhatTheDeclarationCameTo(t *testing.T) {
	for name, row := range map[string]struct {
		statuses  []int
		lastError string
	}{
		"stored":               {[]int{http.StatusOK}, ""},
		"a conflict":           {[]int{http.StatusConflict}, declarationNotApplied},
		"an outage, then none": {[]int{http.StatusBadGateway, http.StatusOK}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			url, _, _ := capabilitiesRegistrar(t, row.statuses...)
			cell := &declarationCell{}
			declareCapabilities(t.Context(), registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Millisecond, cell)
			got := cell.Check(t.Context())
			if got.Status != health.StatusOK || got.LastError != row.lastError || (row.lastError == "") != (got.LastSuccess != "") {
				t.Fatalf("check %+v, want lastError %q", got, row.lastError)
			}
		})
	}
	if got := (&declarationCell{}).Check(t.Context()); got.LastError != "" || got.LastSuccess != "" || got.Name != "capability-declaration" {
		t.Fatalf("before any declaration: %+v", got)
	}
}

// An absent route reads as that while the gateway keeps asking.
func TestDeclarationCell_NamesAnAbsentRoute(t *testing.T) {
	url, _, _ := capabilitiesRegistrar(t, http.StatusNotFound)
	cell := &declarationCell{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		declareCapabilities(ctx, registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Millisecond, cell)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for cell.Check(ctx).LastError != declarationNoRoute {
		if time.Now().After(deadline) {
			t.Fatalf("check %+v, want %q", cell.Check(ctx), declarationNoRoute)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

// Once the route is there, another failure is that failure: a holder refused by a
// registrar that now has the route does not read as a registrar without one.
func TestDeclarationCell_ACauseThatChangedReadsAsTheNewOne(t *testing.T) {
	nf := http.StatusNotFound
	statuses := []int{nf, nf, nf, nf, nf, http.StatusForbidden}
	url, puts, _ := capabilitiesRegistrar(t, statuses...)
	cell := &declarationCell{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		declareCapabilities(ctx, registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Microsecond, cell)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for int(puts.Load()) <= len(statuses) {
		if time.Now().After(deadline) {
			t.Fatalf("%d declarations", puts.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if got := cell.Check(t.Context()); got.LastError != declarationFailing {
		t.Fatalf("check %+v, want %q", got, declarationFailing)
	}
}

// With no registrar there is nothing to declare to, and /health names no check.
func TestHealthNamesNoDeclarationWithoutARegistrar(t *testing.T) {
	f := newShutdownFlush(t)
	delete(f.env, "REGISTRAR_URL")
	b, err := build(t.Context(), f.getenv, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.gateway.Close() })
	if b.registrarURL != "" {
		t.Fatalf("fixture: the gateway has a registrar, %s", b.registrarURL)
	}
	rec := httptest.NewRecorder()
	b.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if strings.Contains(rec.Body.String(), "capability-declaration") {
		t.Fatalf("/health: %s", rec.Body)
	}
}

// A gateway that serves declares itself: what build gave it reaches the
// registrar, signed by the gateway's own key.
func TestBuiltGatewayDeclaresItsLegs(t *testing.T) {
	f := newShutdownFlush(t)
	feed, err := url.Parse(f.env["REGISTRAR_URL"])
	if err != nil {
		t.Fatal(err)
	}
	type declared struct {
		body      []byte
		assertion string
	}
	got := make(chan declared, 1)
	proxy := httputil.NewSingleHostReverseProxy(feed)
	registrar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			proxy.ServeHTTP(w, r)
			return
		}
		if want := "/register/" + f.payer.HolderID + "/capabilities"; r.URL.Path != want {
			t.Errorf("declared at %s, want %s", r.URL.Path, want)
		}
		raw, _ := io.ReadAll(r.Body)
		select {
		case got <- declared{raw, r.Header.Get("X-Holder-Assertion")}:
		default:
		}
		_, _ = w.Write(raw)
	}))
	t.Cleanup(registrar.Close)
	f.env["REGISTRAR_URL"] = registrar.URL
	f.env["PAYER_ATTACHMENT_MAX_BYTES"] = "1048576"

	b, err := build(t.Context(), f.getenv, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.gateway.Close() })
	stop := b.startWorkers(t.Context())
	defer stop()

	select {
	case d := <-got:
		var body shnsdk.CapabilityDeclaration
		if err := json.Unmarshal(d.body, &body); err != nil {
			t.Fatal(err)
		}
		if body.AttachmentMaxBytes == nil || *body.AttachmentMaxBytes != 1<<20 {
			t.Errorf("declared limit %v, want the payer's 1048576", body.AttachmentMaxBytes)
		}
		if !slices.Contains(body.Legs, "cdex-attachment-submit") {
			t.Fatalf("the gateway declared %q", body.Legs)
		}
		raw, err := base64.StdEncoding.DecodeString(d.assertion)
		if err != nil {
			t.Fatal(err)
		}
		var a shnsdk.Assertion
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		if err := shnsdk.VerifyAssertion(a, "registrar", f.payer.SignPub, time.Now()); err != nil {
			t.Fatalf("the declaration's assertion: %v", err)
		}
		// The check /health serves is the one the worker writes.
		deadline := time.Now().Add(10 * time.Second)
		for {
			rec := httptest.NewRecorder()
			b.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
			if strings.Contains(rec.Body.String(), `"name":"capability-declaration","status":"ok","lastSuccess"`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("/health: %s", rec.Body)
			}
			time.Sleep(5 * time.Millisecond)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway declared nothing")
	}
}

// A gateway shutting down stops asking.
func TestDeclareCapabilities_StopsWithItsContext(t *testing.T) {
	url, _, _ := capabilitiesRegistrar(t, http.StatusServiceUnavailable)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		declareCapabilities(ctx, registrarTestClient(), url, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Hour, &declarationCell{})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("declareCapabilities did not stop")
	}
}
