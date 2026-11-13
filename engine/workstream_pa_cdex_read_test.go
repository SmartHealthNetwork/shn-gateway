//go:build !shn_coreonly

package engine

// workstream_pa_cdex_read_test.go — the provider gateway's side of the
// payer's read of its CDex Task (cdex-task-status, D129): the read carried to
// the provider's own Task endpoint as a plain FHIR read, the answer put to the
// network's delivery rule (Q2) and bound to its patient (Q1, D121), each row at
// every conformance level.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// readTaskSystem is a provider's own Task endpoint answering reads: it
// records each request as it arrived (method, request line, headers, body)
// and answers each with the next of its answers, the last one repeated.
type readTaskSystem struct {
	srv     *httptest.Server
	mu      sync.Mutex
	got     []readReceived
	answers []readAnswer
	// redirects answers a request for a path with a 302 to another path on
	// the same server, as a front end might.
	redirects map[string]string
}

type readReceived struct {
	method, requestURI string
	header             http.Header
	body               []byte
}

type readAnswer struct {
	status int
	header map[string]string
	body   []byte
}

func newReadTaskSystem(t *testing.T, answers ...readAnswer) *readTaskSystem {
	t.Helper()
	s := &readTaskSystem{answers: answers}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.got = append(s.got, readReceived{method: r.Method, requestURI: r.RequestURI, header: r.Header.Clone(), body: b})
		if to, ok := s.redirects[r.URL.Path]; ok {
			s.mu.Unlock()
			w.Header().Set("Location", to)
			w.WriteHeader(http.StatusFound)
			return
		}
		a := s.answers[min(len(s.got), len(s.answers))-1]
		s.mu.Unlock()
		body := a.body
		// A subsetting read, as a FHIR server answers one: _summary=true
		// leaves out Task.input (not a summary element), and _elements
		// leaves it out unless the list names it; _summary=false and
		// _summary=data keep it.
		if q := r.URL.Query(); a.status/100 == 2 && len(body) > 0 && dropsInput(q) {
			var m map[string]any
			if json.Unmarshal(body, &m) == nil {
				delete(m, "input")
				body, _ = json.Marshal(m)
			}
		}
		for k, v := range a.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(a.status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// dropsInput reports whether a conforming FHIR server leaves Task.input out of
// its answer to a read with query q.
func dropsInput(q url.Values) bool {
	if q.Get("_summary") == "true" {
		return true
	}
	if !q.Has("_elements") {
		return false
	}
	for _, e := range strings.Split(q.Get("_elements"), ",") {
		if strings.TrimSpace(e) == "input" {
			return false
		}
	}
	return true
}

func (s *readTaskSystem) calls() []readReceived {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// taskURL is the provider's own Task endpoint on s.
func (s *readTaskSystem) taskURL() string { return s.srv.URL + "/fhir/Task" }

// readSoR is the provider's system of record in these rows: the census
// fixture, serving its FHIR base, counting the reads a patient binding makes
// (a member's lookup, a reference's read), and failing a reference's read with
// refErr when set.
type readSoR struct {
	*censusSoR
	legacySoRReader
	base   string
	refErr error
	// held are the resources a reference's read answers in place of the
	// census's.
	held map[string][]byte
	// noIdentifier are members the system reports as held with no
	// identifier.
	noIdentifier map[string]bool
	// notHeld are members the system reports it does not hold.
	notHeld        map[string]bool
	patients, refs atomic.Int32
}

func newReadSoR(base string) *readSoR {
	c := newCensusSoR()
	return &readSoR{censusSoR: c, legacySoRReader: legacySoRReader{sor: c}, base: base}
}

func (s *readSoR) FHIRBase() string { return s.base }

func (s *readSoR) ResolvePatientContext(ctx context.Context, member string) (string, Demo, bool, error) {
	s.patients.Add(1)
	if s.noIdentifier[member] {
		return "", Demo{}, true, nil
	}
	if s.notHeld[member] {
		return "", Demo{}, false, nil
	}
	return s.legacySoRReader.ResolvePatientContext(ctx, member)
}

func (s *readSoR) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	s.refs.Add(1)
	if s.refErr != nil {
		return nil, false, s.refErr
	}
	if b, ok := s.held[ref]; ok {
		return b, true, nil
	}
	return s.legacySoRReader.ResolveByReferenceContext(ctx, ref)
}

// lookups is how many reads a patient binding made of s.
func (s *readSoR) lookups() int32 { return s.patients.Load() + s.refs.Load() }

var (
	_ ContextSystemOfRecord  = (*readSoR)(nil)
	_ SystemOfRecordFHIRBase = (*readSoR)(nil)
)

// cdexReadScope is the read scope a provider sets in these rows.
const cdexReadScope = "system/Task.r"

// readProvider is a provider gateway whose participant sets its own Task
// endpoint (sys) and the read scope, over sor, with the ingress base E-11
// re-keys under, and payer-x registered as a payer that takes the
// documentation submission, attested as payerIdentity.
type readProvider struct {
	*cdexRequestProvider
	sys *readTaskSystem
	sor *readSoR
}

func newReadProvider(t *testing.T, level ConformanceEnforcement, sys *readTaskSystem, sor *readSoR, opts ...func(*Config)) *readProvider {
	t.Helper()
	if sor == nil {
		sor = newReadSoR("")
	}
	p := newCDexRequestProvider(t, level, append([]func(*Config){func(c *Config) {
		c.SoR, c.Store = sor, sor.censusSoR
		c.ProviderCDexTaskURL = sys.taskURL()
		c.ProviderCDexTaskClient = sys.srv.Client()
		c.ProviderCDexReadScope = cdexReadScope
		c.ProviderCDexReadClient = sys.srv.Client()
		c.IngressBaseURL = cdexIngressBase
		EnableIngressForTest(c)
		e, _ := c.Reg.Lookup("payer-x")
		e.Legs = []string{legCDexSubmit}
		e.PayerIDs = []shnsdk.PayerIdentifier{payerIdentity}
		c.Reg.Set("payer-x", e)
	}}, opts...)...)
	return &readProvider{cdexRequestProvider: p, sys: sys, sor: sor}
}

// readAnswerOf is what the payer's gateway receives for a read: the framed
// status, headers and body, and the patient the answer's token names.
type readAnswerOf struct {
	status  int
	framed  bool
	headers map[string]string
	body    []byte
	subject string
}

// read delivers a read of readPath (none when hasPath is false) with body as
// the Hub would, on leg (the read's own unless set), under a token naming no
// patient: the flagged leg's request names none (D129, Q1).
func (p *cdexRequestProvider) read(t *testing.T, leg, readPath string, hasPath bool, body []byte) readAnswerOf {
	t.Helper()
	if leg == "" {
		leg = legCDexTaskStatus
	}
	spec, ok := legSpecFor(leg)
	if !ok {
		t.Fatalf("no catalog entry for %s", leg)
	}
	clock := p.g.cfg.Clock()
	corr := "corr-" + leg
	headers := map[string]string{}
	if hasPath {
		headers[shnsdk.FrameHeaderReadPath] = readPath
	}
	payload, err := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, headers, body)
	if err != nil {
		t.Fatal(err)
	}
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: p.payer.ID, Recipient: p.g.cfg.HolderID, TransactionType: spec.WireName,
		AuthorityFrame: spec.ReqFrame, Timestamp: clock.Format(time.RFC3339), CorrelationID: corr,
	}, payload, p.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatal(err)
	}
	subject := ""
	if leg != legCDexTaskStatus {
		subject = "pci:payer-task-subject"
	}
	tok := signTestToken(shnsdk.Token{
		Operation: spec.Op, Subject: subject, Frame: spec.ReqFrame, Holder: p.payer.ID,
		CorrelationID: corr, Expiry: clock.Add(time.Hour), PayloadHash: sha256hexT(env.Ciphertext),
	}, p.authzPriv)
	tokBytes, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	env.Metadata.AuthzToken = string(tokBytes)
	raw, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	as, err := json.Marshal(shnsdk.IssueAssertion("hub", p.g.cfg.HolderID, p.hubPriv, clock, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", bytes.NewReader(raw))
	r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(as))
	rec := httptest.NewRecorder()
	p.g.inboundRoute()(rec, r)
	p.g.drainObserveChecks()
	if rec.Code != http.StatusOK {
		return readAnswerOf{status: rec.Code, body: rec.Body.Bytes()}
	}
	respEnv, err := shnsdk.DecodeEnvelope(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("decode response envelope: %v", err)
	}
	var respTok shnsdk.Token
	if err := json.Unmarshal([]byte(respEnv.Metadata.AuthzToken), &respTok); err != nil {
		t.Fatalf("response token: %v", err)
	}
	hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, p.payer, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response frame: %v", err)
	}
	return readAnswerOf{status: hdr.Status, framed: true, headers: hdr.Headers, body: answer, subject: respTok.Subject}
}

// readOf is a read of readPath on the read's own leg, with no body.
func (p *readProvider) readOf(t *testing.T, readPath string) readAnswerOf {
	t.Helper()
	return p.read(t, "", readPath, true, nil)
}

// returnPath is this provider gateway's documentation return path for payer
// (E-11's value).
func returnPath(t *testing.T, base, payer string) string {
	t.Helper()
	ret, err := cdexroute.ReturnBase(base, payer)
	if err != nil {
		t.Fatal(err)
	}
	return ret
}

// readTask is example 19 as the provider's system stores it once the
// network carried it for payer-x: its payer-url re-keyed to this provider
// gateway's return path for payer-x, then edit applied.
func readTask(t *testing.T, edit func(task map[string]any)) []byte {
	t.Helper()
	ret := returnPath(t, cdexIngressBase, "payer-x")
	return cdexRequestMutate(t, func(m map[string]any) {
		m["id"] = "t-1"
		setPayerURL(m, map[string]any{"valueUrl": ret})
		if edit != nil {
			edit(m)
		}
	})
}

// setPayerURL replaces the payer-url input's value members with value.
func setPayerURL(m map[string]any, value map[string]any) {
	for _, in := range m["input"].([]any) {
		in := in.(map[string]any)
		if bytes.Contains(jsonOf(in["type"]), []byte(`"payer-url"`)) {
			for k := range in {
				if strings.HasPrefix(k, "value") {
					delete(in, k)
				}
			}
			for k, v := range value {
				in[k] = v
			}
		}
	}
}

// jsonOf is v as JSON, nil when it cannot be written.
func jsonOf(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// taskAnswer is a provider's 200 answer of body, with an ETag and a
// Last-Modified, and a media type with a parameter, each to be carried.
func taskAnswer(body []byte) readAnswer {
	return readAnswer{status: http.StatusOK, body: body, header: map[string]string{
		"Content-Type": "application/fhir+json; charset=utf-8", "ETag": `W/"2"`, "Last-Modified": "Wed, 11 Nov 2026 07:00:00 GMT",
	}}
}

// boundPCI is the patient the provider gateway binds member to, from task,
// by the attachment request's rule.
func boundPCI(t *testing.T, p *readProvider, member string, task []byte) string {
	t.Helper()
	pci, status, msg := p.g.bindInboundSubjectAs(context.Background(), member, task, http.StatusBadGateway)
	if status != 0 || pci == "" {
		t.Fatalf("bind %s: %d %s", member, status, msg)
	}
	return pci
}

// The control, at every level: a read and a vread reach the provider's own
// Task endpoint as a plain FHIR read, below that endpoint, with no body and
// no media type, asking for FHIR JSON and naming the payer outside the body;
// the provider's Task comes back to the payer byte for byte, with its media
// type, ETag and Last-Modified, under the patient the provider's gateway
// binds from it (its response token names that patient; the request's names
// none).
func TestCDexRead_ReadAndVreadCarriedByteForByte(t *testing.T) {
	task := readTask(t, nil)
	for _, c := range []struct{ path, uri string }{
		{"Task/t-1", "/fhir/Task/t-1"},
		{"Task/t-1/_history/2", "/fhir/Task/t-1/_history/2"},
	} {
		for _, level := range allLevels {
			t.Run(c.path+"/"+level.String(), func(t *testing.T) {
				sys := newReadTaskSystem(t, taskAnswer(task))
				p := newReadProvider(t, level, sys, nil)
				ans := p.readOf(t, c.path)
				if !ans.framed || ans.status != http.StatusOK || !bytes.Equal(ans.body, task) {
					t.Fatalf("answer %d (framed %v) %s, want the provider's Task byte for byte", ans.status, ans.framed, ans.body)
				}
				for k, want := range map[string]string{"Content-Type": "application/fhir+json; charset=utf-8", "ETag": `W/"2"`, "Last-Modified": "Wed, 11 Nov 2026 07:00:00 GMT"} {
					if got := ans.headers[k]; got != want {
						t.Errorf("answer %s %q, want %q carried", k, got, want)
					}
				}
				got := sys.calls()
				if len(got) != 1 {
					t.Fatalf("the provider's Task endpoint was called %d times, want once", len(got))
				}
				g := got[0]
				if g.method != http.MethodGet || g.requestURI != c.uri || len(g.body) != 0 {
					t.Fatalf("the provider's system got %s %s with %d body bytes, want GET %s with none", g.method, g.requestURI, len(g.body), c.uri)
				}
				if g.header.Get("Accept") != "application/fhir+json" || g.header.Get("Content-Type") != "" || g.header.Get(EditedHeader) != "" {
					t.Errorf("headers %v: want Accept application/fhir+json, no Content-Type, no %s", g.header, EditedHeader)
				}
				if v := g.header.Get(RequesterHeader); v != `payer-x; identifiers="http://example.org/cdex/payer/payer-ids|Payer123"` {
					t.Errorf("%s = %q", RequesterHeader, v)
				}
				if want := boundPCI(t, p, "Member123", task); ans.subject != want {
					t.Errorf("the answer's token names %q, want the bound patient %q", ans.subject, want)
				}
				rec := p.got.only(t)
				if rec.Outcome != ExchangeAnswered || rec.Exchange != legCDexTaskStatus || rec.AnswerOwnership != "relayed" || rec.Ownership != "relayed" {
					t.Fatalf("record %s %s (request %s, answer %s), want the leg answered, relayed both ways", rec.Outcome, rec.Exchange, rec.Ownership, rec.AnswerOwnership)
				}
			})
		}
	}
}

// The query is carried exactly as sent, at every level: a raw | and an
// escaped / survive byte for byte, and an empty query keeps its "?". The
// URL is built by appending to the parsed Task endpoint, never parsed again.
func TestCDexRead_QueryCarriedAsSent(t *testing.T) {
	task := readTask(t, nil)
	for _, c := range []struct{ path, uri string }{
		{"Task/t-1?_format=json&code=a|b&x=c%2Fd", "/fhir/Task/t-1?_format=json&code=a|b&x=c%2Fd"},
		{"Task/t-1?", "/fhir/Task/t-1?"},
		{"Task/t-1/_history/2?_pretty=true", "/fhir/Task/t-1/_history/2?_pretty=true"},
		{"Task/...", "/fhir/Task/..."},
	} {
		for _, level := range allLevels {
			t.Run(c.path+"/"+level.String(), func(t *testing.T) {
				sys := newReadTaskSystem(t, taskAnswer(task))
				p := newReadProvider(t, level, sys, nil)
				if ans := p.readOf(t, c.path); ans.status != http.StatusOK {
					t.Fatalf("answer %d %s", ans.status, ans.body)
				}
				if got := sys.calls(); len(got) != 1 || got[0].requestURI != c.uri {
					t.Fatalf("the provider's system got %+v, want one read of %s", got, c.uri)
				}
			})
		}
	}
}

// A read whose path is not a Task read or vread is refused 400 on routing at
// every level, nothing sent: a bad id, a dot segment, a query that cannot be
// carried as one, another resource, anything after the id but a version, or
// no path at all. A network rule.
func TestCDexRead_AMalformedReadPathIsRefusedWithNothingSent(t *testing.T) {
	for name, c := range map[string]struct {
		path    string
		hasPath bool
	}{
		"an id that is not a FHIR id": {"Task/a_b", true},
		"an id of 65 characters":      {"Task/" + strings.Repeat("a", 65), true},
		"the dot segment ..":          {"Task/..", true},
		"the dot segment .":           {"Task/.", true},
		"a version ..":                {"Task/t-1/_history/..", true},
		"a space in the query":        {"Task/t-1?a b", true},
		"a # in the query":            {"Task/t-1?a#b", true},
		"another resource":            {"Patient/t-1", true},
		"a path after the id":         {"Task/t-1/x", true},
		"an absolute address":         {"https://ehr.example/fhir/Task/t-1", true},
		"an empty path":               {"", true},
		"no path":                     {"", false},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sys := newReadTaskSystem(t, taskAnswer(readTask(t, nil)))
				p := newReadProvider(t, level, sys, nil)
				ans := p.read(t, "", c.path, c.hasPath, nil)
				if !ans.framed || ans.status != http.StatusBadRequest {
					t.Fatalf("answer %d (framed %v) %s, want a framed 400", ans.status, ans.framed, ans.body)
				}
				if len(sys.calls()) != 0 {
					t.Fatalf("the provider's system was called %d times", len(sys.calls()))
				}
				wantRefusal(t, p.got.only(t), http.StatusBadRequest, RefusedByProviderGateway, RefusalRouting)
				if ans.subject != "" {
					t.Errorf("the refusal's token names %q, want no patient", ans.subject)
				}
			})
		}
	}
}

// A read carries no body: one that does is refused 400 on message integrity
// at every level, nothing sent.
func TestCDexRead_ABodyIsRefused(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newReadTaskSystem(t, taskAnswer(readTask(t, nil)))
			p := newReadProvider(t, level, sys, nil)
			ans := p.read(t, "", "Task/t-1", true, []byte(`{"resourceType":"Task"}`))
			if !ans.framed || ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), refusalCDexReadBody) {
				t.Fatalf("answer %d (framed %v) %s, want a framed 400 %q", ans.status, ans.framed, ans.body, refusalCDexReadBody)
			}
			if len(sys.calls()) != 0 {
				t.Fatalf("the provider's system was called %d times", len(sys.calls()))
			}
			wantRefusal(t, p.got.only(t), http.StatusBadRequest, RefusedByProviderGateway, RefusalIntegrity)
		})
	}
}

// readPath is defined on the read's leg alone: a frame naming it on the
// attachment request is refused 400 on message integrity, as a frame naming
// an operation on a leg that defines none is, before the leg's handler runs.
func TestCDexRead_AReadPathOnAnotherLegIsRefused(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newReadTaskSystem(t, taskAnswer(readTask(t, nil)))
			p := newReadProvider(t, level, sys, nil)
			ans := p.read(t, legCDexRequest, "Task/t-1", true, cdexRequestTask(t))
			if !ans.framed || ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), "readPath header is not defined for this transaction type") {
				t.Fatalf("answer %d (framed %v) %s, want the framed 400", ans.status, ans.framed, ans.body)
			}
			if len(sys.calls()) != 0 {
				t.Fatalf("the provider's system was called %d times", len(sys.calls()))
			}
			wantRefusal(t, p.got.only(t), http.StatusBadRequest, RefusedByProviderGateway, RefusalIntegrity)
		})
	}
	// The control: the same Task on the attachment request with no readPath
	// is carried.
	sys := newReadTaskSystem(t, readAnswer{status: http.StatusCreated, body: []byte(`{"resourceType":"Task"}`)})
	p := newReadProvider(t, EnforcementObserve, sys, nil)
	if ans := p.read(t, legCDexRequest, "", false, cdexRequestTask(t)); ans.status != http.StatusCreated {
		t.Fatalf("control: answer %d %s, want the Task carried", ans.status, ans.body)
	}
}

// A provider whose participant sets no read scope takes no read: at every
// level the read is refused 422 on routing before anything is read, both by a
// provider that takes the attachment request and by one that takes nothing.
func TestCDexRead_NotTakenIsRefusedAsRouting(t *testing.T) {
	for name, opt := range map[string]func(*Config){
		"a Task endpoint and no read scope": func(c *Config) { c.ProviderCDexReadScope, c.ProviderCDexReadClient = "", nil },
		"a blank read scope":                func(c *Config) { c.ProviderCDexReadScope, c.ProviderCDexReadClient = "  ", nil },
		"no Task endpoint": func(c *Config) {
			c.ProviderCDexReadScope, c.ProviderCDexReadClient, c.ProviderCDexTaskURL, c.ProviderCDexTaskClient = "", nil, "", nil
		},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sys := newReadTaskSystem(t, taskAnswer(readTask(t, nil)))
				p := newReadProvider(t, level, sys, nil, opt)
				ans := p.readOf(t, "Task/t-1")
				if !ans.framed || ans.status != http.StatusUnprocessableEntity || !strings.Contains(string(ans.body), refusalCDexReadNotTaken) {
					t.Fatalf("answer %d (framed %v) %s, want a framed 422 %q", ans.status, ans.framed, ans.body, refusalCDexReadNotTaken)
				}
				if len(sys.calls()) != 0 {
					t.Fatalf("the provider's system was called %d times", len(sys.calls()))
				}
				wantRefusal(t, p.got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
				if slices.Contains(p.g.cfg.TakenLegs(), legCDexTaskStatus) {
					t.Fatalf("the gateway declares %s", legCDexTaskStatus)
				}
			})
		}
	}
}

// The network's 404 (D129, Q2), at every level: a provider's 2xx that is not
// one JSON Task carrying exactly one structural payer-url equal to this
// gateway's return path for the reading payer is answered the network's 404,
// the same bytes in every case, recorded as the leg's authority and as this
// gateway's own refusal, under no patient. Nothing of the Task is delivered,
// and no patient lookup is made. The control, the payer's own Task, is
// delivered.
func TestCDexRead_ATaskNotTheReadersIsTheNetworks404(t *testing.T) {
	other := returnPath(t, cdexIngressBase, "payer-y")
	elsewhere := returnPath(t, "https://another-provider-gw.test", "payer-x")
	payerURLType := map[string]any{"coding": []any{map[string]any{"system": "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes", "code": "payer-url"}}}
	rows := map[string]readAnswer{
		"another payer's return path": taskAnswer(readTask(t, func(m map[string]any) { setPayerURL(m, map[string]any{"valueUrl": other}) })),
		"the reader's path at another provider's gateway": taskAnswer(readTask(t, func(m map[string]any) {
			setPayerURL(m, map[string]any{"valueUrl": elsewhere})
		})),
		"an address outside the network": taskAnswer(readTask(t, func(m map[string]any) {
			setPayerURL(m, map[string]any{"valueUrl": "http://example.org/cdex/payer/$submit-attachment"})
		})),
		"a path that differs by a slash": taskAnswer(readTask(t, func(m map[string]any) {
			setPayerURL(m, map[string]any{"valueUrl": returnPath(t, cdexIngressBase, "payer-x") + "/"})
		})),
		"no payer-url": taskAnswer(readTask(t, func(m map[string]any) {
			var keep []any
			for _, in := range m["input"].([]any) {
				if !bytes.Contains(jsonOf(in.(map[string]any)["type"]), []byte(`"payer-url"`)) {
					keep = append(keep, in)
				}
			}
			m["input"] = keep
		})),
		"two payer-urls": taskAnswer(readTask(t, func(m map[string]any) {
			m["input"] = append(m["input"].([]any), map[string]any{"type": payerURLType, "valueUrl": returnPath(t, cdexIngressBase, "payer-x")})
		})),
		"a valueString": taskAnswer(readTask(t, func(m map[string]any) {
			setPayerURL(m, map[string]any{"valueString": returnPath(t, cdexIngressBase, "payer-x")})
		})),
		"an unreadable input": taskAnswer(readTask(t, func(m map[string]any) {
			m["input"] = append(m["input"].([]any), map[string]any{"valueString": "x"})
		})),
		"a body that reads two ways": {status: http.StatusOK, header: map[string]string{"Content-Type": "application/fhir+json"},
			body: append(bytes.TrimSuffix(readTask(t, nil), []byte("}")), []byte(`,"resourceType":"Task"}`)...)},
		"not JSON (XML)": {status: http.StatusOK, header: map[string]string{"Content-Type": "application/fhir+xml"},
			body: []byte(`<Task xmlns="http://hl7.org/fhir"><id value="t-1"/></Task>`)},
		"not a Task": {status: http.StatusOK, header: map[string]string{"Content-Type": "application/fhir+json"},
			body: []byte(`{"resourceType":"Patient","id":"t-1"}`)},
		"an empty 200": {status: http.StatusOK},
		"another payer's Task that also cannot be bound": taskAnswer(readTask(t, func(m map[string]any) {
			setPayerURL(m, map[string]any{"valueUrl": other})
			delete(m, "for")
		})),
	}
	var first []byte
	for name, a := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sys := newReadTaskSystem(t, a)
				p := newReadProvider(t, level, sys, nil)
				ans := p.readOf(t, "Task/t-1")
				if !ans.framed || ans.status != http.StatusNotFound || !bytes.Equal(ans.body, cdexReadNotFound) {
					t.Fatalf("answer %d (framed %v) %s, want the network's 404 %s", ans.status, ans.framed, ans.body, cdexReadNotFound)
				}
				if first == nil {
					first = ans.body
				} else if !bytes.Equal(first, ans.body) {
					t.Fatalf("404 bytes differ between cases:\n%s\n%s", first, ans.body)
				}
				if ans.headers["Content-Type"] != "application/fhir+json" || ans.headers["ETag"] != "" {
					t.Errorf("404 headers %v, want only its own media type", ans.headers)
				}
				if ans.subject != "" {
					t.Errorf("the 404's token names %q, want no patient", ans.subject)
				}
				if n := p.sor.lookups(); n != 0 {
					t.Fatalf("%d patient lookups made, want none", n)
				}
				rec := p.got.only(t)
				wantRefusal(t, rec, http.StatusNotFound, RefusedByProviderGateway, RefusalAuthority)
				if rec.AnswerOwnership != "authored" || rec.AnswerBuilder != string(relay.BuilderGatewayRefusal) {
					t.Errorf("answer %s by %s, want authored by the gateway's refusal", rec.AnswerOwnership, rec.AnswerBuilder)
				}
			})
		}
	}
	// The control: the reader's own Task is delivered.
	sys := newReadTaskSystem(t, taskAnswer(readTask(t, nil)))
	p := newReadProvider(t, EnforcementStrict, sys, nil)
	if ans := p.readOf(t, "Task/t-1"); ans.status != http.StatusOK {
		t.Fatalf("control: answer %d %s, want the reader's Task delivered", ans.status, ans.body)
	}
}

// The 404's body is a constant the network writes: it names nothing of a
// Task, and its code is FHIR's not-found.
func TestCDexRead_The404IsAConstantOperationOutcome(t *testing.T) {
	var oo struct {
		ResourceType string `json:"resourceType"`
		Issue        []struct {
			Severity, Code, Diagnostics string
		} `json:"issue"`
	}
	if err := json.Unmarshal(cdexReadNotFound, &oo); err != nil || oo.ResourceType != "OperationOutcome" || len(oo.Issue) != 1 ||
		oo.Issue[0].Severity != "error" || oo.Issue[0].Code != "not-found" || oo.Issue[0].Diagnostics != cdexReadNotFoundMessage {
		t.Fatalf("404 body %s: %+v %v", cdexReadNotFound, oo, err)
	}
}

// A Task that passes the delivery rule but that the provider's gateway cannot
// bind to a patient is refused 502 on the leg's authority at every level
// (D129, Q1): the network cannot identify the patient whose record it would
// disclose. Nothing of the Task is delivered.
func TestCDexRead_AnUnbindableTaskIs502(t *testing.T) {
	mb := map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}}
	for name, c := range map[string]struct {
		edit  func(m map[string]any)
		sor   *readSoR
		cause string
	}{
		"no Task.for": {func(m map[string]any) { delete(m, "for") }, nil, cdexReadUnboundNoMember},
		"a contained Patient with no member number": {func(m map[string]any) {
			p := m["contained"].([]any)[0].(map[string]any)
			p["identifier"] = []any{map[string]any{"system": "http://example.org/other", "value": "x"}}
		}, nil, cdexReadUnboundNoMember},
		"two member numbers that differ": {func(m map[string]any) {
			p := m["contained"].([]any)[0].(map[string]any)
			p["identifier"] = append(p["identifier"].([]any), map[string]any{"type": mb, "system": "http://example.org/x", "value": "Member999"})
		}, nil, cdexReadUnboundNoMember},
		"an absolute reference": {func(m map[string]any) {
			m["for"] = map[string]any{"reference": "https://ehr.example/fhir/Patient/MBR-COVERED"}
		}, nil, cdexReadUnboundNoMember},
		"a reference to another type": {func(m map[string]any) { m["for"] = map[string]any{"reference": "Group/g-1"} }, nil, cdexReadUnboundNoMember},
		"a reference with a version": {func(m map[string]any) {
			m["for"] = map[string]any{"reference": "Patient/MBR-COVERED/_history/1"}
		}, nil, cdexReadUnboundNoMember},
		"a reference to a dot segment": {func(m map[string]any) { m["for"] = map[string]any{"reference": "Patient/.."} }, nil, cdexReadUnboundNoMember},
		"a contained reference that names none": {func(m map[string]any) {
			m["for"] = map[string]any{"reference": "#nobody"}
		}, nil, cdexReadUnboundNoMember},
		"a bare reference when the bases differ": {func(m map[string]any) {
			m["for"] = map[string]any{"reference": "Patient/MBR-COVERED"}
		}, newReadSoR("https://sor.example/fhir"), cdexReadUnboundBases},
		"a bare reference with no system-of-record base": {func(m map[string]any) {
			m["for"] = map[string]any{"reference": "Patient/MBR-COVERED"}
		}, newReadSoR(""), cdexReadUnboundBases},
		"a bare reference the system of record does not hold": {func(m map[string]any) {
			m["for"] = map[string]any{"reference": "Patient/MBR-NOT-HELD"}
		}, nil, cdexReadUnboundNotHeld},
		"a bare reference to a Patient with no member identifier": {func(m map[string]any) {
			m["for"] = map[string]any{"reference": "Patient/p-1"}
		}, nil, cdexReadUnboundPatient},
		"a bare reference to a Patient with two member identifiers": {func(m map[string]any) {
			m["for"] = map[string]any{"reference": "Patient/p-2"}
		}, nil, cdexReadUnboundPatient},
		"a bare reference answered with another resource": {func(m map[string]any) {
			m["for"] = map[string]any{"reference": "Patient/p-3"}
		}, nil, cdexReadUnboundPatient},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				task := readTask(t, c.edit)
				sys := newReadTaskSystem(t, taskAnswer(task))
				sor := c.sor
				if sor == nil {
					sor = newReadSoR(sys.srv.URL + "/fhir")
					sor.held = map[string][]byte{
						"Patient/p-1": []byte(`{"resourceType":"Patient","id":"p-1","identifier":[{"system":"http://example.org/mrn","value":"MRN-1"}]}`),
						"Patient/p-2": []byte(`{"resourceType":"Patient","id":"p-2","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"A"},{"system":"` + shnsdk.MemberSystem + `","value":"B"}]}`),
						"Patient/p-3": []byte(`{"resourceType":"Group","id":"p-3","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-COVERED"}]}`),
					}
				}
				p := newReadProvider(t, level, sys, sor)
				ans := p.readOf(t, "Task/t-1")
				want := refusalCDexReadUnbound + c.cause
				if !ans.framed || ans.status != http.StatusBadGateway || !strings.Contains(string(ans.body), want) {
					t.Fatalf("answer %d (framed %v) %s, want a framed 502 %q", ans.status, ans.framed, ans.body, want)
				}
				if bytes.Contains(ans.body, []byte("Member123")) || bytes.Contains(ans.body, []byte(`"Task"`)) {
					t.Fatalf("the 502 carries the Task: %s", ans.body)
				}
				if ans.subject != "" {
					t.Errorf("the 502's token names %q, want no patient", ans.subject)
				}
				wantRefusal(t, p.got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalAuthority)
			})
		}
	}
}

// A read of the system of record that fails is that system's failure, as on
// every leg (subjectBindingFailure): answered with its status, nothing of the
// Task delivered.
func TestCDexRead_ASystemOfRecordFailureIsItsOwnClass(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			task := readTask(t, func(m map[string]any) { m["for"] = map[string]any{"reference": "Patient/MBR-COVERED"} })
			sys := newReadTaskSystem(t, taskAnswer(task))
			sor := newReadSoR(sys.srv.URL + "/fhir")
			sor.refErr = &SoRReadError{Kind: SoRInvalidResponse}
			p := newReadProvider(t, level, sys, sor)
			ans := p.readOf(t, "Task/t-1")
			wantStatus, wantMsg := SoRFailureResponse(sor.refErr)
			if !ans.framed || ans.status != wantStatus || !strings.Contains(string(ans.body), wantMsg) {
				t.Fatalf("answer %d (framed %v) %s, want %d %q", ans.status, ans.framed, ans.body, wantStatus, wantMsg)
			}
			if bytes.Contains(ans.body, []byte(`"Task"`)) {
				t.Fatalf("the failure carries the Task: %s", ans.body)
			}
		})
	}
}

// Lead ruling (b), at every level: a Task.for that is only Patient/<id> is
// read from the system of record when the Task endpoint is served under that
// system's FHIR base, compared by structure, and bound by the member the
// system names the Patient by. The Task is delivered under that patient,
// the system's held binding. A base that differs refuses it 502, a host
// named another way included: hosts are compared as written, never resolved
// (TestSameReadBase holds the rest of the rule case by case).
func TestCDexRead_ABareReferenceIsReadWhenTheBasesAreOne(t *testing.T) {
	task := readTask(t, func(m map[string]any) { m["for"] = map[string]any{"reference": "Patient/MBR-COVERED"} })
	held, _, _ := newCensusSoR().ResolvePatient("MBR-COVERED")
	for name, base := range map[string]func(sys *readTaskSystem) string{
		"the same base":              func(s *readTaskSystem) string { return s.srv.URL + "/fhir" },
		"a trailing slash":           func(s *readTaskSystem) string { return s.srv.URL + "/fhir/" },
		"the scheme in another case": func(s *readTaskSystem) string { return "HTTP" + strings.TrimPrefix(s.srv.URL, "http") + "/fhir" },
		"localhost for the address (not)": func(s *readTaskSystem) string {
			return strings.Replace(s.srv.URL, "127.0.0.1", "localhost", 1) + "/fhir"
		},
		"the path in another case (not)": func(s *readTaskSystem) string { return s.srv.URL + "/FHIR" },
		// Formatted, not concatenated: tools/gateroute reads a ".." step
		// appended to a value as a climb toward the repository's root.
		"a dot segment (not)": func(s *readTaskSystem) string { return fmt.Sprintf("%s/x/../fhir", s.srv.URL) },
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sys := newReadTaskSystem(t, taskAnswer(task))
				sor := newReadSoR(base(sys))
				p := newReadProvider(t, level, sys, sor)
				ans := p.readOf(t, "Task/t-1")
				if strings.HasSuffix(name, "(not)") {
					if ans.status != http.StatusBadGateway || !strings.Contains(string(ans.body), cdexReadUnboundBases) {
						t.Fatalf("answer %d %s, want the 502 naming the bases", ans.status, ans.body)
					}
					if sor.refs.Load() != 0 {
						t.Fatalf("the Patient was read from a system whose base is not the Task endpoint's")
					}
					return
				}
				if ans.status != http.StatusOK || !bytes.Equal(ans.body, task) {
					t.Fatalf("answer %d %s, want the Task delivered", ans.status, ans.body)
				}
				if ans.subject != held {
					t.Fatalf("the answer's token names %q, want the system of record's held binding %q", ans.subject, held)
				}
				if sor.refs.Load() != 1 {
					t.Fatalf("%d reference reads, want the Patient read once", sor.refs.Load())
				}
			})
		}
	}
}

// The bases rule, case by case (lead ruling b): scheme and host compared
// without regard to case, the scheme's default port elided, one ending slash
// ignored, the path compared exactly, and a dot segment never equal.
func TestSameReadBase(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://ehr.example/fhir", "https://ehr.example/fhir", true},
		{"https://ehr.example/fhir", "HTTPS://ehr.example/fhir", true},
		{"https://ehr.example/fhir", "https://EHR.Example/fhir", true},
		{"https://ehr.example/fhir", "https://ehr.example:443/fhir", true},
		{"http://ehr.example/fhir", "http://ehr.example:80/fhir", true},
		{"https://ehr.example:8443/fhir", "https://ehr.example:8443/fhir", true},
		{"https://ehr.example:8443/fhir", "https://ehr.example/fhir", false},
		{"https://ehr.example/fhir", "https://ehr.example:80/fhir", false},
		{"https://ehr.example/fhir", "https://ehr.example/fhir/", true},
		{"https://ehr.example/fhir", "https://ehr.example/fhir//", false},
		{"https://ehr.example/fhir", "https://ehr.example/FHIR", false},
		{"https://ehr.example/fhir", "http://ehr.example/fhir", false},
		{"https://ehr.example/fhir", "https://ehr.example/x/../fhir", false},
		{"https://ehr.example/./fhir", "https://ehr.example/./fhir", false},
		{"https://ehr.example/%2E%2E/fhir", "https://ehr.example/%2E%2E/fhir", false},
		{"https://ehr.example/fhir", "https://ehr.example/fhir?x=1", false},
		{"https://ehr.example/fhir", "https://u@ehr.example/fhir", false},
		{"https://ehr.example/fhir", "", false},
		{"", "", false},
		{"https://ehr.example/fhir", "https://ehr.exämple/fhir", false},
		{"https://ehr.example", "https://ehr.example/", true},
	} {
		if got := sameReadBase(c.a, c.b); got != c.want {
			t.Errorf("sameReadBase(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := sameReadBase(c.b, c.a); got != c.want {
			t.Errorf("sameReadBase(%q, %q) = %v, want %v", c.b, c.a, got, c.want)
		}
	}
}

// The provider's own 404 and 410, and its 401 once the gateway has sent the
// read once more with a fresh token (and recorded the resend), are carried as
// sent at every level, under no patient: they disclose none.
func TestCDexRead_TheProvidersOwnAnswersAreCarried(t *testing.T) {
	oo := func(code string) []byte {
		return []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"` + code + `","diagnostics":"the provider's own"}]}`)
	}
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		for _, level := range allLevels {
			t.Run(fmt.Sprintf("%d/%s", status, level), func(t *testing.T) {
				body := oo("not-found")
				sys := newReadTaskSystem(t, readAnswer{status: status, body: body, header: map[string]string{"Content-Type": "application/fhir+json"}})
				p := newReadProvider(t, level, sys, nil)
				ans := p.readOf(t, "Task/t-1")
				if !ans.framed || ans.status != status || !bytes.Equal(ans.body, body) {
					t.Fatalf("answer %d (framed %v) %s, want the provider's %d as sent", ans.status, ans.framed, ans.body, status)
				}
				if ans.subject != "" || p.sor.lookups() != 0 {
					t.Fatalf("token names %q after %d lookups, want no patient and none", ans.subject, p.sor.lookups())
				}
				if rec := p.got.only(t); rec.Outcome != ExchangeUpstreamError {
					t.Fatalf("record %s, want the provider's error carried", rec.Outcome)
				}
			})
		}
	}
	for _, level := range allLevels {
		t.Run("401/"+level.String(), func(t *testing.T) {
			var tokens atomic.Int32
			var scopes []string
			var mu sync.Mutex
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				mu.Lock()
				scopes = append(scopes, r.PostForm.Get("scope"))
				mu.Unlock()
				n := tokens.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"access_token":"AT-%d","token_type":"bearer","expires_in":3600}`, n)
			}))
			t.Cleanup(tokenSrv.Close)
			body := oo("login")
			sys := newReadTaskSystem(t, readAnswer{status: http.StatusUnauthorized, body: body, header: map[string]string{"Content-Type": "application/fhir+json", "WWW-Authenticate": `Bearer error="invalid_token"`}})
			hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tokenSrv.URL, ClientID: "provider-gw", Scope: cdexReadScope, ClientSecret: "s", HTTPClient: tokenSrv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			p := newReadProvider(t, level, sys, nil, func(c *Config) { c.ProviderCDexReadClient = hc })
			ans := p.readOf(t, "Task/t-1")
			if !ans.framed || ans.status != http.StatusUnauthorized || !bytes.Equal(ans.body, body) {
				t.Fatalf("answer %d (framed %v) %s, want the provider's 401 as sent", ans.status, ans.framed, ans.body)
			}
			got := sys.calls()
			if len(got) != 2 || got[0].header.Get("Authorization") != "Bearer AT-1" || got[1].header.Get("Authorization") != "Bearer AT-2" {
				t.Fatalf("the provider's system got %d reads, want the read and its one resend with a fresh token", len(got))
			}
			mu.Lock()
			defer mu.Unlock()
			for _, s := range scopes {
				if s != cdexReadScope {
					t.Fatalf("token asked for %q, want the read scope %q", s, cdexReadScope)
				}
			}
			rec := p.got.only(t)
			if want := []BackendResend{{Reason: BackendResendCredentialRefreshed, Challenge: smartauth.ChallengeInvalidToken}}; !slices.Equal(rec.BackendResends, want) {
				t.Fatalf("resends %+v, want %+v", rec.BackendResends, want)
			}
		})
	}
}

// X-SHN-Requester is information only: it is set on every forward, and a
// Task whose requester differs from what the network attests for the reader
// is still delivered at every level, with the difference observed, as on the
// attachment request.
func TestCDexRead_TheRequesterHeaderChangesNoOutcome(t *testing.T) {
	for name, c := range map[string]struct {
		edit    func(m map[string]any)
		differs bool
	}{
		"the attested requester": {nil, false},
		"another requester": {func(m map[string]any) {
			m["requester"] = map[string]any{"identifier": map[string]any{"system": "http://example.org/other", "value": "Payer999"}}
		}, true},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				task := readTask(t, c.edit)
				sys := newReadTaskSystem(t, taskAnswer(task))
				var events []ObserverEvent
				var mu sync.Mutex
				p := newReadProvider(t, level, sys, nil, func(cfg *Config) {
					cfg.Observer = func(e ObserverEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }
				})
				ans := p.readOf(t, "Task/t-1")
				if ans.status != http.StatusOK || !bytes.Equal(ans.body, task) {
					t.Fatalf("answer %d %s, want the Task delivered", ans.status, ans.body)
				}
				if v := sys.calls()[0].header.Get(RequesterHeader); !strings.HasPrefix(v, "payer-x") {
					t.Fatalf("%s = %q, want the reader named", RequesterHeader, v)
				}
				mu.Lock()
				defer mu.Unlock()
				got := slices.ContainsFunc(events, func(e ObserverEvent) bool {
					return e.Kind == CDexRequesterDiffersEvent && e.LegType == legCDexTaskStatus
				})
				if got != c.differs {
					t.Fatalf("%s raised %v, want %v", CDexRequesterDiffersEvent, got, c.differs)
				}
			})
		}
	}
}

// The read's forward is the read's own: its target is built from the parsed
// Task endpoint, its escaped path kept, never by joining strings.
func TestCDexRead_ReadTargetKeepsTheEndpointsEscapedPath(t *testing.T) {
	u, err := cdexroute.TaskReadURL("https://ehr.example/a%2Fb/fhir/Task")
	if err != nil {
		t.Fatal(err)
	}
	got, err := cdexroute.ReadTarget(u, "t-1", "x=a|b", true)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestURI() != "/a%2Fb/fhir/Task/t-1?x=a|b" {
		t.Fatalf("RequestURI %q", got.RequestURI())
	}
	if _, err := url.Parse(got.String()); err != nil {
		t.Fatal(err)
	}
}

// A provider takes the payer's read exactly when its participant sets the
// read scope (and, as New requires, the Task endpoint): it then declares the
// leg beside the attachment request, and mounts the inbound route. A blank
// scope is unset; a payer setting one takes nothing more.
func TestInboundMount_AProviderWithTheReadScopeTakesTheRead(t *testing.T) {
	configured := func(t *testing.T, scope string) Config {
		cfg := mountConfig(t, "provider")
		cfg.ProviderCDexTaskURL = "https://provider.test/fhir/Task"
		cfg.ProviderCDexTaskClient = http.DefaultClient
		cfg.ProviderCDexReadScope = scope
		cfg.ProviderCDexReadClient = http.DefaultClient
		cfg.IngressBaseURL = "https://provider-gw.test"
		EnableIngressForTest(&cfg)
		pub, _ := genED25519(t)
		cfg.HubTransportPub = ed25519.PublicKey(pub)
		return cfg
	}
	for scope, want := range map[string][]string{
		cdexReadScope: {legCDexRequest, legCDexTaskStatus},
		"":            {legCDexRequest},
		"  ":          {legCDexRequest},
	} {
		cfg := configured(t, scope)
		if got := cfg.TakenLegs(); !slices.Equal(got, want) {
			t.Errorf("read scope %q: takes %v, want %v", scope, got, want)
		}
		g := mustNew(t, cfg)
		if (g.paState().cdexRead != nil) != (scope == cdexReadScope) {
			t.Errorf("read scope %q: read state %v", scope, g.paState().cdexRead)
		}
		if !slices.Contains(g.RoutePatternsForTest(), inboundPattern) {
			t.Errorf("read scope %q: no inbound route", scope)
		}
	}
	payer := mountConfig(t, "payer")
	payer.ProviderCDexReadScope = cdexReadScope
	if slices.Contains(payer.TakenLegs(), legCDexTaskStatus) {
		t.Errorf("a payer with a read scope takes %v", payer.TakenLegs())
	}
}

// The read scope is a provider's, and the read it takes must be one the
// gateway can deliver: set on another role, without the Task endpoint,
// without the client it reads with, or with a Task endpoint no read can be
// made below, the gateway does not boot. Without the read scope, the
// attachment request takes any Task endpoint, as before.
func TestNew_TheReadScopeNeedsATaskEndpointAReadIsMadeBelow(t *testing.T) {
	for name, c := range map[string]struct {
		role, taskURL string
		client        *http.Client
		want          string
	}{
		"on a payer":              {"payer", "", http.DefaultClient, `is the scope a provider's gateway reads its own Tasks with; this gateway's role is "payer"`},
		"without a Task endpoint": {"provider", "", http.DefaultClient, "requires ProviderCDexTaskURL (PROVIDER_CDEX_TASK_URL)"},
		"without a client":        {"provider", "https://provider.test/fhir/Task", nil, "requires ProviderCDexReadClient"},
		"a query":                 {"provider", "https://provider.test/fhir/Task?x=1", http.DefaultClient, cdexroute.ErrTaskURLQueryOrFragment.Error()},
		"an empty query":          {"provider", "https://provider.test/fhir/Task?", http.DefaultClient, cdexroute.ErrTaskURLQueryOrFragment.Error()},
		"a fragment":              {"provider", "https://provider.test/fhir/Task#x", http.DefaultClient, cdexroute.ErrTaskURLQueryOrFragment.Error()},
		"no Task segment":         {"provider", "https://provider.test/fhir/Tasks", http.DefaultClient, cdexroute.ErrTaskURLNotTask.Error()},
		"a trailing slash":        {"provider", "https://provider.test/fhir/Task/", http.DefaultClient, cdexroute.ErrTaskURLNotTask.Error()},
		"Task in another case":    {"provider", "https://provider.test/fhir/task", http.DefaultClient, cdexroute.ErrTaskURLNotTask.Error()},
		"a dot segment":           {"provider", "https://provider.test/x/../Task", http.DefaultClient, cdexroute.ErrTaskURLDotSegment.Error()},
		"an escaped dot segment":  {"provider", "https://provider.test/%2E%2E/Task", http.DefaultClient, cdexroute.ErrTaskURLDotSegment.Error()},
		"no host":                 {"provider", "https:///fhir/Task", http.DefaultClient, cdexroute.ErrTaskURLNotAbsolute.Error()},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := mountConfig(t, c.role)
			cfg.ProviderCDexReadScope = cdexReadScope
			cfg.ProviderCDexReadClient = c.client
			if c.taskURL != "" {
				cfg.ProviderCDexTaskURL = c.taskURL
				cfg.ProviderCDexTaskClient = http.DefaultClient
				cfg.IngressBaseURL = "https://provider-gw.test"
				EnableIngressForTest(&cfg)
			}
			pub, _ := genED25519(t)
			cfg.HubTransportPub = ed25519.PublicKey(pub)
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("New: %v, want an error containing %q", err, c.want)
			}
		})
	}
	// Without the read scope, a Task endpoint of any of those shapes still
	// takes the attachment request.
	for _, taskURL := range []string{"https://provider.test/fhir/Task?x=1", "https://provider.test/fhir/Task/", "https://provider.test/x/../Task", "https://provider.test/submit"} {
		cfg := mountConfig(t, "provider")
		cfg.ProviderCDexTaskURL = taskURL
		cfg.ProviderCDexTaskClient = http.DefaultClient
		cfg.IngressBaseURL = "https://provider-gw.test"
		EnableIngressForTest(&cfg)
		pub, _ := genED25519(t)
		cfg.HubTransportPub = ed25519.PublicKey(pub)
		g := mustNew(t, cfg)
		if !slices.Equal(g.cfg.TakenLegs(), []string{legCDexRequest}) {
			t.Errorf("Task endpoint %q without a read scope: takes %v, want the attachment request alone", taskURL, g.cfg.TakenLegs())
		}
	}
}

// A subsetting read of the payer's own Task that leaves out Task.input
// (_summary=true, or _elements not naming input) is answered by the
// provider's system with a Task without its payer-url: the delivery rule
// cannot see whose Task it is, so the payer gets the network's 404, at every
// level. A query that keeps Task.input (_elements naming it, _summary=false or
// data), or subsets nothing (_pretty), is delivered byte for byte.
func TestCDexRead_ASubsettingReadGetsThe404(t *testing.T) {
	task := readTask(t, nil)
	for _, c := range []struct {
		path   string
		status int
	}{
		{"Task/t-1?_summary=true", http.StatusNotFound},
		{"Task/t-1?_elements=status,statusReason", http.StatusNotFound},
		{"Task/t-1?_pretty=true", http.StatusOK},
		// A subsetting query that keeps Task.input keeps the payer-url,
		// so the payer's own Task is delivered.
		{"Task/t-1?_elements=status,input", http.StatusOK},
		{"Task/t-1?_summary=false", http.StatusOK},
		{"Task/t-1?_summary=data", http.StatusOK},
	} {
		for _, level := range allLevels {
			t.Run(c.path+"/"+level.String(), func(t *testing.T) {
				sys := newReadTaskSystem(t, taskAnswer(task))
				p := newReadProvider(t, level, sys, nil)
				ans := p.readOf(t, c.path)
				if ans.status != c.status {
					t.Fatalf("answer %d %s, want %d", ans.status, ans.body, c.status)
				}
				if c.status == http.StatusNotFound && !bytes.Equal(ans.body, cdexReadNotFound) {
					t.Fatalf("answer %s, want the network's 404", ans.body)
				}
				if c.status == http.StatusOK && !bytes.Equal(ans.body, task) {
					t.Fatalf("answer %s, want the Task as sent", ans.body)
				}
			})
		}
	}
}

// A read the provider's system answers after a redirect came from another
// address than the Task endpoint, so the endpoint's base says nothing of
// which server a bare Patient/{id} names: at every level that Task is refused
// 502, with no system-of-record read, though the bases match. A member number
// on Task.for still binds, and that Task is delivered. Read with a client that
// observes no hops, and with the SMART client's redirect rule, alike.
func TestCDexRead_ARedirectedReadBindsNoBareReference(t *testing.T) {
	bare := readTask(t, func(m map[string]any) { m["for"] = map[string]any{"reference": "Patient/MBR-COVERED"} })
	member := readTask(t, nil)
	for name, c := range map[string]struct {
		task   []byte
		status int
	}{
		"a bare reference": {bare, http.StatusBadGateway},
		"a member number":  {member, http.StatusOK},
	} {
		for _, observing := range []bool{true, false} {
			for _, level := range allLevels {
				t.Run(fmt.Sprintf("%s/observing=%v/%s", name, observing, level), func(t *testing.T) {
					sys := newReadTaskSystem(t, taskAnswer(c.task))
					sys.redirects = map[string]string{"/fhir/Task/t-1": "/elsewhere/Task/t-1"}
					sor := newReadSoR(sys.srv.URL + "/fhir")
					client := sys.srv.Client()
					if observing {
						client = &http.Client{Transport: client.Transport, CheckRedirect: smartauth.CheckRedirect}
					}
					p := newReadProvider(t, level, sys, sor, func(cfg *Config) { cfg.ProviderCDexReadClient = client })
					ans := p.readOf(t, "Task/t-1")
					if got := sys.calls(); len(got) != 2 || got[1].requestURI != "/elsewhere/Task/t-1" {
						t.Fatalf("the provider's system got %+v, want the read and its redirected hop", got)
					}
					if ans.status != c.status {
						t.Fatalf("answer %d %s, want %d", ans.status, ans.body, c.status)
					}
					if c.status == http.StatusBadGateway {
						if !strings.Contains(string(ans.body), refusalCDexReadUnbound+cdexReadUnboundRedirected) {
							t.Fatalf("answer %s, want the redirect named", ans.body)
						}
						if sor.refs.Load() != 0 {
							t.Fatalf("%d reference reads, want none", sor.refs.Load())
						}
						wantRefusal(t, p.got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalAuthority)
						return
					}
					if !bytes.Equal(ans.body, c.task) {
						t.Fatalf("answer %s, want the Task as sent", ans.body)
					}
				})
			}
		}
	}
	// The control: unredirected, the same bare reference binds.
	sys := newReadTaskSystem(t, taskAnswer(bare))
	p := newReadProvider(t, EnforcementObserve, sys, newReadSoR(sys.srv.URL+"/fhir"))
	if ans := p.readOf(t, "Task/t-1"); ans.status != http.StatusOK {
		t.Fatalf("control: answer %d %s", ans.status, ans.body)
	}
}

// A system of record that reports the member a read's Task names as held but
// with no identifier names no one patient: the read is refused 502 on the
// leg's authority, a rule recorded, whether or not the participant requires
// known members, at every level; nothing of the Task is delivered.
func TestCDexRead_AMemberWithNoIdentifierIs502WithARule(t *testing.T) {
	for _, requireKnown := range []bool{false, true} {
		for _, level := range allLevels {
			t.Run(fmt.Sprintf("requireKnown=%v/%s", requireKnown, level), func(t *testing.T) {
				sys := newReadTaskSystem(t, taskAnswer(readTask(t, nil)))
				sor := newReadSoR("")
				sor.noIdentifier = map[string]bool{"Member123": true}
				p := newReadProvider(t, level, sys, sor, func(c *Config) { c.RequireKnownMembers = requireKnown })
				ans := p.readOf(t, "Task/t-1")
				if ans.status != http.StatusBadGateway || !strings.Contains(string(ans.body), refusalCDexReadUnbound+"unknown member") {
					t.Fatalf("answer %d %s, want the 502", ans.status, ans.body)
				}
				wantRefusal(t, p.got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalAuthority)
			})
		}
	}
}

// A member the system of record does not hold, for a provider that requires
// known members (REQUIRE_KNOWN_MEMBERS), cannot be bound: the read answers
// 502, recorded as conformance (the check the provider opted into), at every
// level. Without the setting the gateway derives the member's subject and the
// Task is delivered byte for byte: the check is the provider's opt-in.
func TestCDexRead_AMemberNotHeldIs502OnConformanceOnlyWhenKnownMembersAreRequired(t *testing.T) {
	task := readTask(t, nil)
	for _, requireKnown := range []bool{false, true} {
		for _, level := range allLevels {
			t.Run(fmt.Sprintf("requireKnown=%v/%s", requireKnown, level), func(t *testing.T) {
				sys := newReadTaskSystem(t, taskAnswer(task))
				sor := newReadSoR("")
				sor.notHeld = map[string]bool{"Member123": true}
				p := newReadProvider(t, level, sys, sor, func(c *Config) { c.RequireKnownMembers = requireKnown })
				ans := p.readOf(t, "Task/t-1")
				if sor.patients.Load() == 0 {
					t.Fatal("the system of record was not asked for the member")
				}
				if !requireKnown {
					if ans.status != http.StatusOK || !bytes.Equal(ans.body, task) {
						t.Fatalf("answer %d %s, want the provider's Task byte for byte", ans.status, ans.body)
					}
					if want := boundPCI(t, p, "Member123", task); ans.subject != want {
						t.Errorf("the answer's token names %q, want the derived patient %q", ans.subject, want)
					}
					if rec := p.got.only(t); rec.Outcome != ExchangeAnswered {
						t.Fatalf("record %s %s/%s, want answered", rec.Outcome, rec.RefusedBy, rec.Rule)
					}
					return
				}
				if ans.status != http.StatusBadGateway || !strings.Contains(string(ans.body), refusalCDexReadUnbound+"unknown member") {
					t.Fatalf("answer %d %s, want the 502", ans.status, ans.body)
				}
				wantRefusal(t, p.got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
			})
		}
	}
}

// The attachment request's side of the same case: a Task whose member the
// system of record reports with no identifier is refused 400 on the leg's
// authority, a rule recorded, at every level, before the provider's system is
// called.
func TestCDexRequest_AMemberWithNoIdentifierIsRefusedWithARule(t *testing.T) {
	for _, requireKnown := range []bool{false, true} {
		for _, level := range allLevels {
			t.Run(fmt.Sprintf("requireKnown=%v/%s", requireKnown, level), func(t *testing.T) {
				sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{"resourceType":"Task"}`))
				sor := newReadSoR("")
				sor.noIdentifier = map[string]bool{"Member123": true}
				p := configuredProvider(t, level, sys, func(c *Config) { c.SoR, c.RequireKnownMembers = sor, requireKnown })
				ans := p.send(t, cdexRequestTask(t))
				if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), "unknown member") {
					t.Fatalf("answer %d %s, want 400 unknown member", ans.status, ans.body)
				}
				if sys.calls() != 0 {
					t.Fatalf("the provider's system was called %d times", sys.calls())
				}
				wantRefusal(t, p.got.only(t), http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
			})
		}
	}
}
