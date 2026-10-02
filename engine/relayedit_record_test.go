package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The registered edits a gateway applies to the bytes it transmits on a leg
// are named on the exchange record (ExchangeRecord.Edits) and on the captured
// events of the transmit (leg.sealed, native.request): the edit ids only, in
// transmit order, never a value an edit removed or wrote.

// edits renders ids as strings, the record's form.
func editStrings(ids ...relay.EditID) []string {
	var out []string
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

// capturedEvents collects a gateway's diagnostic events.
type capturedEvents struct {
	mu     sync.Mutex
	events []diagnostics.Event
}

func (c *capturedEvents) sink(e diagnostics.Event) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return true
}

// of returns the captured events of kind.
func (c *capturedEvents) of(kind string) []diagnostics.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []diagnostics.Event
	for _, e := range c.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// wantEventEdits asserts the one event of kind names exactly want in its
// Detail, or carries no Detail when want is empty.
func wantEventEdits(t *testing.T, c *capturedEvents, kind string, want []string) {
	t.Helper()
	events := c.of(kind)
	if len(events) != 1 {
		t.Fatalf("want one %s event, got %d", kind, len(events))
	}
	e := events[0]
	if len(want) == 0 {
		if e.Detail != "" {
			t.Fatalf("%s carries detail %q, want none: a transmit with no edit is captured as before", kind, e.Detail)
		}
		return
	}
	var d diagnostics.RelayEditsDetail
	if err := json.Unmarshal([]byte(e.Detail), &d); err != nil || !slices.Equal(d.RelayEdits, want) {
		t.Fatalf("%s detail %q (%v), want relayEdits %v", kind, e.Detail, err, want)
	}
	if !e.BodyComplete {
		t.Fatalf("%s: naming its edits marked its body partial", kind)
	}
}

// ingressCRD posts body through the recorded CRD ingress route.
func ingressCRD(env *inProcessExchange, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteCRD)(rec, crdIngressPost(body))
	return rec
}

// ingressDTR posts body through the recorded DTR ingress route.
func ingressDTR(env *inProcessExchange, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteDTR)(rec, req)
	return rec
}

// recording attaches an exchange recorder and a diagnostic sink to the
// provider gateway of env.
func recording(env *inProcessExchange) (*exchangeRecords, *capturedEvents) {
	var got exchangeRecords
	var events capturedEvents
	env.originator.cfg.ExchangeObserved = got.observe
	env.originator.cfg.Diagnostic = events.sink
	return &got, &events
}

// The requester's request: each registered edit the provider's gateway
// applies to the request it seals and sends is named on its record and its
// leg.sealed event, in transmit order; a request carried exactly names none.
func TestExchangeRecord_RequesterEdits(t *testing.T) {
	t.Run("a CRD request with a callback: E-01", func(t *testing.T) {
		env := newInProcessExchange(t)
		got, events := recording(env)
		if rec := ingressCRD(env, conformantCRDRequest("MBR-COVERED")); rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		want := editStrings(relay.EditCDSCallbackStrip)
		if r := got.only(t); !slices.Equal(r.Edits, want) {
			t.Fatalf("edits %v, want %v", r.Edits, want)
		}
		wantEventEdits(t, events, "leg.sealed", want)
	})

	t.Run("a CRD request with no callback is carried exactly: no edits", func(t *testing.T) {
		env := newInProcessExchange(t)
		got, events := recording(env)
		body := conformantCRDRequest("MBR-COVERED")
		body = bytes.Replace(body, []byte(`"fhirServer":"https://provider.example/fhir",`), nil, 1)
		body = bytes.Replace(body, []byte(`"fhirAuthorization":{"token_type":"Bearer","access_token":"tok"},`), nil, 1)
		if bytes.Contains(body, []byte("fhirServer")) || bytes.Contains(body, []byte("fhirAuthorization")) {
			t.Fatal("fixture: the callback is still in the request")
		}
		if rec := ingressCRD(env, body); rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		if !bytes.Equal(sentRequest(t, env), body) {
			t.Fatal("the request was not carried exactly; the row proves nothing")
		}
		if r := got.only(t); r.Edits != nil {
			t.Fatalf("edits %v on a request carried exactly", r.Edits)
		}
		wantEventEdits(t, events, "leg.sealed", nil)
	})

	t.Run("a PAS submit is carried exactly: no edits", func(t *testing.T) {
		env := newInProcessExchange(t)
		got, events := recording(env)
		r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", "")))
		rec := httptest.NewRecorder()
		env.originator.ingressRoute(RoutePAS)(rec, r)
		// The harness's payer answers with CDS cards, which a PAS ingress
		// refuses; the request was sent, and that is what this row reads.
		if env.routeHitCount() != 1 || !bytes.Equal(sentRequest(t, env), []byte(pasIngressBundle("00001", ""))) {
			t.Fatalf("the submit was not sent exactly: answer %d %s", rec.Code, rec.Body.String())
		}
		if r := got.only(t); r.Exchange != "pas-claim" || r.Edits != nil {
			t.Fatalf("record %q edits %v, want pas-claim with none", r.Exchange, r.Edits)
		}
		wantEventEdits(t, events, "leg.sealed", nil)
	})

	t.Run("a fhirServer-only CRD request: E-01 then E-07", func(t *testing.T) {
		e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")))
		env, _ := fhirServerEnv(t, e, "")
		got, events := recording(env)
		if rec := ingressCRD(env, fhirServerRequest(e.base)); rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		want := editStrings(relay.EditCDSCallbackStrip, relay.EditCDSCoverageCarry)
		r := got.only(t)
		if !slices.Equal(r.Edits, want) {
			t.Fatalf("edits %v, want %v", r.Edits, want)
		}
		wantEventEdits(t, events, "leg.sealed", want)
		// Ids only: no value an edit removed or wrote.
		line, _ := json.Marshal(r)
		for _, v := range []string{"ehr-secret-token", e.base, "example.com"} {
			if bytes.Contains(line, []byte(v)) {
				t.Fatalf("the record carries %q: %s", v, line)
			}
		}
	})

	t.Run("an EHR-sent coverage is never carried over: no E-07", func(t *testing.T) {
		e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")))
		env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
		got, _ := recording(env)
		sentCoverage := `{"resourceType":"Coverage","id":"ehr-c","beneficiary":{"reference":"Patient/` + strangerMember + `"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}`
		body := bytes.Replace(fhirServerRequest(e.base), []byte(`"id":"`+strangerMember+`"}`),
			[]byte(`"id":"`+strangerMember+`"},"coverage":`+sentCoverage), 1)
		if rec := ingressCRD(env, body); rec.Code != http.StatusOK || e.calls.Load() != 0 {
			t.Fatalf("answer %d %s; %d reads", rec.Code, rec.Body.String(), e.calls.Load())
		}
		if r := got.only(t); !slices.Equal(r.Edits, editStrings(relay.EditCDSCallbackStrip)) {
			t.Fatalf("edits %v, want only E-01", r.Edits)
		}
	})

	// A request refused before it is sent names no edit, though the edits
	// were prepared: the record names what was transmitted.
	for name, row := range map[string]struct {
		mode     string
		noServer bool
	}{
		"the read off: refused, no E-07":  {FHIRServerReadOff, false},
		"no fhirServer: refused, no edit": {FHIRServerReadPrivate, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")))
			env, _ := fhirServerEnv(t, e, row.mode)
			got, events := recording(env)
			body := fhirServerRequest(e.base)
			if row.noServer {
				body = bytes.Replace(body, []byte(`"fhirServer" : "`+e.base+`",`), nil, 1)
			}
			if rec := ingressCRD(env, body); rec.Code != http.StatusPreconditionFailed {
				t.Fatalf("answer %d %s, want 412", rec.Code, rec.Body.String())
			}
			r := got.only(t)
			wantRefusal(t, r, http.StatusPreconditionFailed, RefusedByProviderGateway, RefusalRouting)
			if r.Edits != nil {
				t.Fatalf("a refused request names edits %v", r.Edits)
			}
			if n := len(events.of("leg.sealed")); n != 0 {
				t.Fatalf("%d legs sealed for a refused request", n)
			}
		})
	}

	// A leg the Authorization Framework denies was sealed, never sent: its
	// sealed event names the edits it was sealed with, its record none.
	t.Run("a leg authorization denies names no edit on its record", func(t *testing.T) {
		env := newInProcessExchange(t)
		got, events := recording(env)
		env.originator.cfg.Client = &http.Client{Transport: authorizeAnswers{next: env.substrate}}
		w := ingressCRD(env, conformantCRDRequest("MBR-COVERED"))
		r := got.only(t)
		wantRefusal(t, r, w.Code, RefusedByAuthorizationFramework, RefusalAuthority)
		if r.Edits != nil || env.routeHitCount() != 0 {
			t.Fatalf("edits %v (%d legs routed) on a leg never sent", r.Edits, env.routeHitCount())
		}
		wantEventEdits(t, events, "leg.sealed", editStrings(relay.EditCDSCallbackStrip))
	})

	t.Run("CRD enrichment opt-in: E-02; off: none", func(t *testing.T) {
		for _, enrich := range []bool{true, false} {
			s := newPrefetchSoR()
			env := newInProcessExchange(t)
			env.originator.cfg.SoR = s.sor()
			env.originator.cfg.EnrichNativeRequests = enrich
			got, events := recording(env)
			if rec := ingressCRD(env, ehrRequest(`"coverage":`+ehrCoverage)); rec.Code != http.StatusOK {
				t.Fatalf("enrich %v: answer %d %s", enrich, rec.Code, rec.Body.String())
			}
			want := editStrings(relay.EditCDSCallbackStrip)
			if enrich {
				want = editStrings(relay.EditCDSCallbackStrip, relay.EditCDSPrefetchObtain)
			}
			if r := got.only(t); !slices.Equal(r.Edits, want) {
				t.Fatalf("enrich %v: edits %v, want %v", enrich, r.Edits, want)
			}
			wantEventEdits(t, events, "leg.sealed", want)
		}
	})

	t.Run("DTR enrichment opt-in: E-04 and E-05; off: none", func(t *testing.T) {
		ownPatient := `{"name":"referenced","resource":{"resourceType":"Patient","id":"example"}}`
		rows := map[string]struct {
			body  []byte
			sor   func(*prefetchSoR) SystemOfRecord
			setup func(t *testing.T, s *prefetchSoR)
			want  []relay.EditID
		}{
			"no coverage: E-04": {
				body: ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire, `{"name":"context","valueString":"ctx-1"}`, ownPatient),
				sor:  func(s *prefetchSoR) SystemOfRecord { return s.sor() },
				setup: func(t *testing.T, s *prefetchSoR) {
					s.answer(t, "Coverage", page("", "", sorEntry(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value))))
				},
				want: []relay.EditID{relay.EditDTRCoverageObtain},
			},
			"no Patient: E-05": {
				body:  ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire),
				sor:   func(s *prefetchSoR) SystemOfRecord { return recordSoR{searchingPrefetchSoR{s}} },
				setup: func(*testing.T, *prefetchSoR) {},
				want:  []relay.EditID{relay.EditDTRPatientObtain},
			},
		}
		for name, row := range rows {
			for _, enrich := range []bool{true, false} {
				s := newPrefetchSoR()
				row.setup(t, s)
				env := newInProcessExchange(t)
				env.originator.cfg.SoR = row.sor(s)
				env.originator.cfg.EnrichNativeRequests = enrich
				declareFramedDTR(t, env, true)
				env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
				got, events := recording(env)
				if rec := ingressDTR(env, row.body); rec.Code != http.StatusOK {
					t.Fatalf("%s, enrich %v: answer %d %s", name, enrich, rec.Code, rec.Body.String())
				}
				var want []string
				if enrich {
					want = editStrings(row.want...)
				}
				if r := got.only(t); !slices.Equal(r.Edits, want) {
					t.Fatalf("%s, enrich %v: edits %v, want %v", name, enrich, r.Edits, want)
				}
				wantEventEdits(t, events, "leg.sealed", want)
			}
		}
	})
}

// The recipient's forward to its own system: the payer identity mapping (E-03)
// is named on the payer's own inbound record and its native.request event;
// with the mapping off the request is forwarded exactly and names none.
func TestExchangeRecord_RecipientEdits(t *testing.T) {
	for _, mapped := range []bool{true, false} {
		var events capturedEvents
		opts := []NativeOption{WithNativeDiagnostic(events.sink)}
		if mapped {
			opts = append(opts, withIdentityMapping()...)
		}
		p := newLevelPayer(t, EnforcementNone, opts...)
		var got exchangeRecords
		p.g.cfg.ExchangeObserved = got.observe
		body := []byte(pasIngressBundle("00001", ""))
		if ans := p.send(t, "pas-claim", "", body); ans.status != http.StatusOK {
			t.Fatalf("mapped %v: answer %d %s", mapped, ans.status, ans.body)
		}
		var want []string
		if mapped {
			want = editStrings(relay.EditPayorEdgeRestamp)
			if !bytes.Equal(p.partner.lastBody, mappedPayor(t, body)) {
				t.Fatal("the mapping did not apply; the row proves nothing")
			}
		}
		r := got.only(t)
		if r.Direction != DirectionInbound || !slices.Equal(r.Edits, want) {
			t.Fatalf("mapped %v: %s record edits %v, want %v", mapped, r.Direction, r.Edits, want)
		}
		wantEventEdits(t, &events, "native.request", want)
	}
}

// The recorder keeps the closed set: only registered ids, each once, in the
// order first transmitted; a nil recorder records nothing.
func TestExchangeRecorder_EditsClosedSet(t *testing.T) {
	x := &exchangeRecorder{}
	x.edits([]relay.EditID{relay.EditCDSCallbackStrip, "E-99", relay.EditCDSCoverageCarry})
	x.edits([]relay.EditID{relay.EditCDSCoverageCarry, "", "e-01", relay.EditCDSCallbackStrip, relay.EditPayorEdgeRestamp})
	want := editStrings(relay.EditCDSCallbackStrip, relay.EditCDSCoverageCarry, relay.EditPayorEdgeRestamp)
	if !slices.Equal(x.rec.Edits, want) {
		t.Fatalf("edits %v, want %v", x.rec.Edits, want)
	}
	x.edits(nil)
	if !slices.Equal(x.rec.Edits, want) {
		t.Fatalf("no edits changed the record: %v", x.rec.Edits)
	}
	var none *exchangeRecorder
	none.edits([]relay.EditID{relay.EditCDSCallbackStrip}) // must not panic
}

// The record handed to the observer owns its edits: a later note never
// changes a record already emitted.
func TestExchangeRecord_EditsAreCloned(t *testing.T) {
	g := &Gateway{cfg: Config{ExchangeObserved: func(ExchangeRecord) {}}}
	var emitted ExchangeRecord
	g.cfg.ExchangeObserved = func(r ExchangeRecord) { emitted = r }
	x := &exchangeRecorder{}
	x.edits([]relay.EditID{relay.EditCDSCallbackStrip})
	g.emitExchange(x, &exchangeWriter{ResponseWriter: httptest.NewRecorder(), x: x}, false)
	x.rec.Edits[0] = string(relay.EditPayorEdgeRestamp)
	if !slices.Equal(emitted.Edits, editStrings(relay.EditCDSCallbackStrip)) {
		t.Fatalf("the emitted record's edits changed after it was emitted: %v", emitted.Edits)
	}
}

// Each registered edit has a name, and the party whose received bytes it
// changes: an edit to what a gateway sends into the network reaches the
// leg's other party; an edit to what it sends its own participant's system
// reaches no one else. Pinned per id, so a new edit decides its row.
func TestRelayEditReceivedByTable(t *testing.T) {
	want := map[relay.EditID]struct {
		name string
		by   relay.Role
	}{
		relay.EditCDSCallbackStrip:     {"cds-callback-strip", relay.RoleRecipient},
		relay.EditCDSPrefetchObtain:    {"cds-prefetch-obtain", relay.RoleRecipient},
		relay.EditPayorEdgeRestamp:     {"payor-edge-restamp", 0},
		relay.EditDTRCoverageObtain:    {"dtr-coverage-obtain", relay.RoleRecipient},
		relay.EditDTRPatientObtain:     {"dtr-patient-obtain", relay.RoleRecipient},
		relay.EditEvidenceSubjectRekey: {"evidence-subject-rekey", relay.RoleRecipient},
		relay.EditCDSCoverageCarry:     {"cds-callback-coverage-carry", relay.RoleRecipient},
	}
	ids := relay.EditIDs()
	if len(ids) != len(want) {
		t.Fatalf("%d registered edits, %d pinned here: pin the new edit's row", len(ids), len(want))
	}
	for _, id := range ids {
		w, ok := want[id]
		if !ok {
			t.Fatalf("%s is not pinned here", id)
		}
		name, ok := RelayEditName(id)
		if !ok || name != w.name {
			t.Errorf("RelayEditName(%s) = %q %v, want %q", id, name, ok, w.name)
		}
		by, ok := RelayEditReceivedBy(id)
		if !ok || by != w.by {
			t.Errorf("RelayEditReceivedBy(%s) = %v %v, want %v", id, by, ok, w.by)
		}
	}
	for _, id := range []relay.EditID{"", "E-99", "e-01"} {
		if _, ok := RelayEditName(id); ok {
			t.Errorf("RelayEditName(%q) names an unregistered edit", id)
		}
		if _, ok := RelayEditReceivedBy(id); ok {
			t.Errorf("RelayEditReceivedBy(%q) places an unregistered edit", id)
		}
	}
}

// A transmit's captured Detail names the same closed set the record does:
// registered ids only, each once, in order; none at all for a transmit with
// no edit.
func TestRelayEditsDetail(t *testing.T) {
	got := relayEditsDetail([]relay.EditID{relay.EditCDSCallbackStrip, "E-99", relay.EditCDSCoverageCarry, relay.EditCDSCallbackStrip, ""})
	if want := `{"relayEdits":["E-01","E-07"]}`; got != want {
		t.Fatalf("detail %s, want %s", got, want)
	}
	for _, none := range [][]relay.EditID{nil, {}, {"E-99"}} {
		if got := relayEditsDetail(none); got != "" {
			t.Fatalf("detail %q for %v, want none", got, none)
		}
	}
}

// sendFails answers the requests whose path ends in path itself and passes
// every other to next: with status set, that answer (the bytes were
// received); else err, after reporting the request written when wrote is set
// (a connection that failed after the request left), before it otherwise.
type sendFails struct {
	next   http.RoundTripper
	path   string
	wrote  bool
	status int
	err    error
}

func (f sendFails) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, f.path) {
		return f.next.RoundTrip(req)
	}
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	if f.status != 0 {
		body := `{"error":"replay detected"}`
		if f.status/100 == 2 {
			body = "not an envelope"
		}
		return &http.Response{StatusCode: f.status, Header: http.Header{"Content-Type": {"application/json"}, HubDeliveredHeader: {"no"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}
	if f.wrote {
		if tr := httptrace.ContextClientTrace(req.Context()); tr != nil && tr.WroteRequest != nil {
			tr.WroteRequest(httptrace.WroteRequestInfo{})
		}
	}
	return nil, f.err
}

// The record names the edits only once the bytes have left the gateway: a
// request the Hub never received names none; one it received names them,
// whatever it answered.
func TestExchangeRecord_RequesterEditsOnlyOnceSent(t *testing.T) {
	for name, row := range map[string]struct {
		send sendFails
		want []string
	}{
		"a Hub dial error: never sent, no edits":              {sendFails{path: "/route", err: dialRefused()}, nil},
		"the Hub refused it after receiving it: edits":        {sendFails{path: "/route", status: http.StatusConflict}, editStrings(relay.EditCDSCallbackStrip)},
		"the connection failed after the request was written": {sendFails{path: "/route", wrote: true, err: connReset()}, editStrings(relay.EditCDSCallbackStrip)},
		"the Hub's answer is not an envelope: edits":          {sendFails{path: "/route", status: http.StatusOK}, editStrings(relay.EditCDSCallbackStrip)},
	} {
		t.Run(name, func(t *testing.T) {
			env := newInProcessExchange(t)
			got, events := recording(env)
			row.send.next = env.substrate
			env.originator.cfg.Client = &http.Client{Transport: row.send}
			if w := ingressCRD(env, conformantCRDRequest("MBR-COVERED")); w.Code/100 == 2 {
				t.Fatalf("answer %d: the send did not fail; the row proves nothing", w.Code)
			}
			if r := got.only(t); !slices.Equal(r.Edits, row.want) {
				t.Fatalf("record %s, edits %v, want %v", r.Outcome, r.Edits, row.want)
			}
			// The sealed leg's event names what it was sealed with either way.
			wantEventEdits(t, events, "leg.sealed", editStrings(relay.EditCDSCallbackStrip))
		})
	}
}

// The payer's forward names its edits on the record only once they are
// sent: a payer system that could not be dialled, or a bearer that could not
// be obtained, names none; a connection that failed after the request was
// written names them.
func TestExchangeRecord_RecipientEditsOnlyOnceSent(t *testing.T) {
	for name, row := range map[string]struct {
		send sendFails
		want []string
	}{
		"a payer system dial error: never sent, no edits":           {sendFails{path: pasSubmitPath, err: dialRefused()}, nil},
		"a bearer that could not be obtained: never sent, no edits": {sendFails{path: pasSubmitPath, wrote: true, err: tokenAcquisitionFailure(t)}, nil},
		"the connection failed after the request was written":       {sendFails{path: pasSubmitPath, wrote: true, err: connReset()}, editStrings(relay.EditPayorEdgeRestamp)},
	} {
		t.Run(name, func(t *testing.T) {
			var events capturedEvents
			p := newLevelPayer(t, EnforcementNone, append(withIdentityMapping(), WithNativeDiagnostic(events.sink))...)
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			n := p.g.cfg.Responder.(*nativeResponder)
			row.send.next = n.client.Transport
			n.client = &http.Client{Transport: row.send}
			if ans := p.send(t, "pas-claim", "", []byte(pasIngressBundle("00001", ""))); ans.status/100 == 2 {
				t.Fatalf("answer %d: the forward did not fail; the row proves nothing", ans.status)
			}
			if r := got.only(t); r.Outcome != ExchangeUpstreamError || !slices.Equal(r.Edits, row.want) {
				t.Fatalf("record %s, edits %v, want upstream-error with %v", r.Outcome, r.Edits, row.want)
			}
			wantEventEdits(t, &events, "native.request", editStrings(relay.EditPayorEdgeRestamp))
		})
	}
}
