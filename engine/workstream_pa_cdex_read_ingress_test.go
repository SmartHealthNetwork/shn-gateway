//go:build !shn_coreonly

package engine

// workstream_pa_cdex_read_ingress_test.go — the payer gateway's side of the
// payer's read of its CDex Task (cdex-task-status, D129): the payer's own
// system reads and vreads its Task at a provider's base, the read is carried
// exactly in its frame's readPath with no body and no patient, the provider's
// answer is delivered as it came, and, after a 2xx answer naming a patient,
// the payer's own binding of that patient is reported to the Hub once
// (requesterbinding.go). Each row at every conformance level.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

// readFailingSoR is a payer's system of record whose member reads fail, or,
// with block set, hang until block closes, whatever the read's context says.
type readFailingSoR struct {
	*prefetchSoR
	block chan struct{}
}

func (s readFailingSoR) ResolvePatientContext(context.Context, string) (string, Demo, bool, error) {
	if s.block != nil {
		<-s.block
	}
	return "", Demo{}, false, &SoRReadError{Kind: SoRUnavailable}
}

// readProviderID is the provider the payer reads its Task at.
const readProviderID = "cdex-provider"

// readHub stands in for the network on the payer's read: the Authorization
// Framework's /authorize (signing the leg's tokens and the binding's), the
// Hub's /route (opening the read as the provider's gateway would, then
// sealing back the configured answer under a response token naming the
// configured patient), and the Hub's /requester-binding, served by a real
// listener so the transport's own outcomes (an answer, none, no connection)
// are the gateway's to read.
type readHub struct {
	authzPriv     ed25519.PrivateKey
	payerEncPub   *[32]byte
	provEncPub    *[32]byte
	provEncPriv   *[32]byte
	clock         func() time.Time
	binding       *httptest.Server
	bindingTarget string // the binding route's base, when not binding's

	mu sync.Mutex
	// The answer /route seals: its frame's status, headers and body, and the
	// patient its token names.
	answerStatus  int
	answerHeaders map[string]string
	answerBody    []byte
	answerSubject string
	// What each /route carried, as the provider's gateway opens it.
	routed []routedRead
	// Each /authorize request, decoded. An involved token's request is
	// answered refuseInvolved with refuseBody instead, when set; failed by the
	// transport when unreachable; held until authorizeHold closes, when set;
	// and minted with involvement swapped for the other requester-side value
	// when swapInvolvement.
	authorized      []authorizeReq
	refuseInvolved  int
	refuseBody      string
	unreachable     bool
	authorizeHold   chan struct{}
	swapInvolvement bool
	// misbindInvolved mints the binding's token for another payload, a
	// token that does not verify for the answer.
	misbindInvolved bool
	// dialHold, when set, holds the record before it is dialed until it
	// closes or the record's context ends; dialing counts the records held.
	dialHold chan struct{}
	dialing  int
	// The binding route's answer, its status; lose drops the connection
	// after reading the request, and partial after reading part of its body;
	// redirect answers 302; hold, when set, is waited on after the request
	// is read.
	bindingStatus int
	lose          bool
	partial       bool
	redirect      bool
	hold          chan struct{}
	posts         []postedBinding
	// bindingAnswers, when set, are the binding route's answers in turn,
	// each with its headers, before it answers bindingStatus.
	bindingAnswers []bindingAnswer
}

// bindingAnswer is one answer of the binding route: its status and headers.
type bindingAnswer struct {
	status int
	header http.Header
}

type routedRead struct {
	headers map[string]string
	body    []byte
	token   shnsdk.Token
	raw     string // the request token as the envelope carried it
	resp    string // the response token sealed back
}

type postedBinding struct {
	body      []byte
	assertion string
}

func newReadHub(t *testing.T, payerEncPub *[32]byte) *readHub {
	t.Helper()
	_, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	h := &readHub{authzPriv: authzPriv, payerEncPub: payerEncPub, provEncPub: provEncPub, provEncPriv: provEncPriv,
		clock: ingressFixedClock(), answerStatus: http.StatusOK, bindingStatus: http.StatusNoContent}
	h.binding = httptest.NewServer(http.HandlerFunc(h.serveBinding))
	t.Cleanup(h.binding.Close)
	return h
}

func (h *readHub) serveBinding(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	partial := h.partial
	h.mu.Unlock()
	if partial {
		// Some of the record arrived, then the Hub went away.
		_, _ = io.ReadFull(r.Body, make([]byte, 8))
		h.mu.Lock()
		h.posts = append(h.posts, postedBinding{assertion: r.Header.Get("X-Holder-Assertion")})
		h.mu.Unlock()
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.posts = append(h.posts, postedBinding{body: body, assertion: r.Header.Get("X-Holder-Assertion")})
	status, lose, hold, redirect := h.bindingStatus, h.lose, h.hold, h.redirect
	var header http.Header
	if len(h.bindingAnswers) > 0 {
		status, header = h.bindingAnswers[0].status, h.bindingAnswers[0].header
		h.bindingAnswers = h.bindingAnswers[1:]
	}
	h.mu.Unlock()
	if redirect {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
		return
	}
	if hold != nil {
		<-hold
	}
	if r.URL.Path != shnsdk.HubRequesterBindingPath || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if lose {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	for k, vs := range header {
		w.Header()[k] = vs
	}
	w.WriteHeader(status)
}

func (h *readHub) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(req.URL.Path, shnsdk.HubRequesterBindingPath):
		h.mu.Lock()
		dialHold := h.dialHold
		if dialHold != nil {
			h.dialing++
		}
		h.mu.Unlock()
		if dialHold != nil {
			select {
			case <-dialHold:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		// The record goes over a real connection, so whether it was written
		// is the transport's to say.
		target := h.binding.URL
		if h.bindingTarget != "" {
			target = h.bindingTarget
		}
		u, _ := url.Parse(target + shnsdk.HubRequesterBindingPath)
		out := req.Clone(req.Context())
		out.URL, out.Host = u, u.Host
		return http.DefaultTransport.RoundTrip(out)
	case strings.HasSuffix(req.URL.Path, "/authorize"):
		return h.authorize(req)
	case strings.HasSuffix(req.URL.Path, "/route"):
		return h.route(req)
	}
	return errResp("unexpected stub call to " + req.URL.Path), nil
}

func (h *readHub) authorize(req *http.Request) (*http.Response, error) {
	var a authorizeReq
	body, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(body, &a)
	h.mu.Lock()
	h.authorized = append(h.authorized, a)
	refuse, refuseBody, unreachable, hold, swap, misbind := h.refuseInvolved, h.refuseBody, h.unreachable, h.authorizeHold, h.swapInvolvement, h.misbindInvolved
	h.mu.Unlock()
	if a.Involvement != "" {
		if hold != nil {
			select {
			case <-hold:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		if unreachable {
			return nil, errors.New("dial tcp: connection refused")
		}
		if refuse != 0 {
			if refuseBody == "" {
				refuseBody = `{"error":"refused"}`
			}
			return &http.Response{StatusCode: refuse, Body: io.NopCloser(strings.NewReader(refuseBody)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
		}
		if misbind {
			a.PayloadHash = strings.Repeat("0", 64)
		}
		if swap {
			if a.Involvement == shnsdk.InvolvementRequesterDerived {
				a.Involvement = shnsdk.InvolvementRequesterHeld
			} else {
				a.Involvement = shnsdk.InvolvementRequesterDerived
			}
		}
	}
	tok := signTestToken(shnsdk.Token{Operation: a.Operation, Scope: "task-status-only", Subject: a.SubjectPCI, Frame: a.Frame, Holder: "payer",
		CorrelationID: a.CorrelationID, Expiry: h.clock().Add(time.Hour), PayloadHash: a.PayloadHash, Involvement: a.Involvement}, h.authzPriv)
	b, _ := json.Marshal(map[string]any{"token": tok})
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
}

func (h *readHub) route(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	env, err := shnsdk.DecodeEnvelope(raw)
	if err != nil {
		return errResp("stub: decode envelope"), nil
	}
	plain, err := shnsdk.Open(env, h.provEncPub, h.provEncPriv)
	if err != nil {
		return errResp("stub: open"), nil
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(plain)
	if err != nil {
		return errResp("stub: the read is not framed"), nil
	}
	var reqTok shnsdk.Token
	_ = json.Unmarshal([]byte(env.Metadata.AuthzToken), &reqTok)
	h.mu.Lock()
	status, headers, answer, subject := h.answerStatus, h.answerHeaders, h.answerBody, h.answerSubject
	h.mu.Unlock()
	payload, err := shnsdk.EncodeHTTPFrameHeaders(status, headers, answer)
	if err != nil {
		return errResp("stub: frame the answer"), nil
	}
	spec, _ := legSpecFor(legCDexTaskStatus)
	meta := shnsdk.Metadata{Sender: readProviderID, Recipient: "payer", TransactionType: env.Metadata.TransactionType,
		AuthorityFrame: spec.RespFrame, Timestamp: h.clock().Format(time.RFC3339), CorrelationID: env.Metadata.CorrelationID}
	out, err := sealForProvider(meta, payload, h.payerEncPub, h.authzPriv, env.Metadata.CorrelationID, spec.RespOp, spec.RespFrame, readProviderID, subject, h.clock())
	if err != nil {
		return errResp("stub: seal"), nil
	}
	respEnv, _ := shnsdk.DecodeEnvelope(out)
	h.mu.Lock()
	h.routed = append(h.routed, routedRead{headers: hdr.Headers, body: body, token: reqTok, raw: env.Metadata.AuthzToken, resp: respEnv.Metadata.AuthzToken})
	h.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(out)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
}

func (h *readHub) snapshot() ([]routedRead, []authorizeReq, []postedBinding) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.routed), slices.Clone(h.authorized), slices.Clone(h.posts)
}

// readPayer is a payer's gateway with its ingress on (the test bypass grants
// the create and read scopes) at level, over readHub, with readProviderID
// registered as a provider that takes the attachment request and the read.
type readPayer struct {
	g     *Gateway
	hub   *readHub
	recs  *exchangeRecords
	sign  ed25519.PublicKey
	level ConformanceEnforcement
}

func newReadPayer(t *testing.T, level ConformanceEnforcement, opts ...func(*Config)) *readPayer {
	t.Helper()
	payerEncPub, payerEncPriv := genKeyPair(t)
	signPub, signPriv := genED25519(t)
	hub := newReadHub(t, payerEncPub)
	authzPub := hub.authzPriv.Public().(ed25519.PublicKey)
	hubPub, _ := genED25519(t)
	reg := shnsdk.NewRegistry()
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", EncPub: payerEncPub, SignPub: signPub, MessageFrames: shnsdk.SupportedMessageFrames()})
	reg.Set(readProviderID, shnsdk.RegistryEntry{ID: readProviderID, Role: "provider", EncPub: hub.provEncPub,
		Legs: []string{legCDexRequest, legCDexTaskStatus}, MessageFrames: shnsdk.SupportedMessageFrames()})
	reg.Set("older-provider", shnsdk.RegistryEntry{ID: "older-provider", Role: "provider", EncPub: hub.provEncPub,
		Legs: []string{legCDexRequest}, MessageFrames: shnsdk.SupportedMessageFrames()})
	sor := newCensusSoR()
	var recs exchangeRecords
	cfg := Config{
		Role: "payer", HolderID: "payer",
		Identity: shnsdk.Identity{HolderID: "payer", SignPriv: signPriv, EncPub: payerEncPub, EncPriv: payerEncPriv},
		AuthzURL: "http://authz.test", AuthzPub: authzPub, HubURL: "http://hub.test", HubTransportPub: hubPub,
		Reg: reg, Validator: shnsdk.NewFakeValidator(), SoR: sor, Store: sor, Clock: hub.clock,
		Client:    &http.Client{Transport: hub},
		Responder: NewNativeResponder(http.DefaultClient, "http://payer.invalid", fixedClock),
		// The payer's own Task create carries its system's message; nothing
		// here needs a module.
		ConformanceEnforcement: level,
		IngressBaseURL:         testIngressBaseURL,
		ExchangeObserved:       recs.observe,
	}
	EnableIngressForTest(&cfg)
	for _, o := range opts {
		o(&cfg)
	}
	return &readPayer{g: mustNew(t, cfg), hub: hub, recs: &recs, sign: signPub, level: level}
}

// get serves the payer system's GET of target (a path and query under the
// ingress base, as sent) through the gateway's routes, and waits for the
// work after the answer and its record.
func (p *readPayer) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := p.getNoWait(t, target)
	p.g.waitAfterAnswers()
	return rec
}

func (p *readPayer) getNoWait(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, testIngressBaseURL+target, nil)
	rec := httptest.NewRecorder()
	p.g.Handler().ServeHTTP(rec, req)
	return rec
}

// readTaskAnswer is the provider's Task as its system answers a read: example
// 19 with the given Task.for.
func readTaskAnswer(t *testing.T, forValue any) []byte {
	t.Helper()
	return cdexRequestMutate(t, func(m map[string]any) {
		m["id"] = "t-1"
		if forValue != nil {
			m["for"] = forValue
		}
	})
}

// heldMemberFor is a Task.for naming a member the payer's records hold.
var heldMemberFor = map[string]any{"identifier": map[string]any{"type": map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}}, "value": "MBR-COVERED"}}

// readAnswerHeaders are the provider's answer headers the network carries.
var readAnswerHeaders = map[string]string{"Content-Type": "application/fhir+json; charset=utf-8", shnsdk.FrameHeaderETag: `W/"3"`, shnsdk.FrameHeaderLastModified: "Wed, 11 Nov 2026 12:00:00 GMT"}

// boundPCIOf is the patient the payer binds member to, from task, by the
// create's rule.
func (p *readPayer) boundPCIOf(t *testing.T, member string, task []byte) (string, bool) {
	t.Helper()
	pci, held, found, err := p.g.resolveSubjectBinding(t.Context(), member, task)
	if err != nil || !found {
		t.Fatalf("bind %s: %v %v", member, found, err)
	}
	return pci, held
}

// The control, at every level: a read and a vread at the provider's base,
// each with a query, are carried to that provider exactly: an empty body, the
// path and query as sent in the frame's readPath and nothing else, under a
// token naming no patient. The provider's Task comes back byte for byte, with
// its status, media type, ETag and Last-Modified. Then the payer's own binding
// of the Task's patient is reported to the Hub once, under a holder assertion
// binding the body: the leg's two tokens as the envelopes carried them, and a
// token the Framework minted for the binding against the answer's own token,
// held for a member the payer's records hold and derived for one they do not.
// The record is accepted (204) and on the exchange record.
func TestCDexReadIngress_CarriesTheReadAndReportsTheBinding(t *testing.T) {
	for _, level := range allLevels {
		for _, row := range []struct {
			name, target, readPath string
			forValue               any
			member                 string
			involvement            string
		}{
			{"read, derived", "/cdex/provider/" + readProviderID + "/Task/t-1?_elements=status,statusReason&x=a|b%2F", "Task/t-1?_elements=status,statusReason&x=a|b%2F", nil, "Member123", shnsdk.InvolvementRequesterDerived},
			{"vread, held", "/cdex/provider/" + readProviderID + "/Task/t-1/_history/3", "Task/t-1/_history/3", heldMemberFor, "MBR-COVERED", shnsdk.InvolvementRequesterHeld},
			{"empty query kept", "/cdex/provider/" + readProviderID + "/Task/t-1?", "Task/t-1?", nil, "Member123", shnsdk.InvolvementRequesterDerived},
		} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := newReadPayer(t, level)
				task := readTaskAnswer(t, row.forValue)
				p.hub.answerBody, p.hub.answerHeaders, p.hub.answerSubject = task, readAnswerHeaders, "pci:00000000000000000000000000000d15"
				rec := p.get(t, row.target)
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), task) {
					t.Fatalf("answer %d %s, want the provider's Task byte for byte", rec.Code, rec.Body)
				}
				for name, want := range readAnswerHeaders {
					if got := rec.Header().Get(name); got != want {
						t.Errorf("answer %s %q, want %q", name, got, want)
					}
				}
				routed, authorized, posts := p.hub.snapshot()
				if len(routed) != 1 {
					t.Fatalf("routed %d reads, want one", len(routed))
				}
				r := routed[0]
				if len(r.body) != 0 || len(r.headers) != 1 || r.headers[shnsdk.FrameHeaderReadPath] != row.readPath {
					t.Fatalf("the provider's side received headers %v and body %q, want readPath %q alone and no body", r.headers, r.body, row.readPath)
				}
				if r.token.Subject != "" || r.token.Operation != "cdex-task-read" {
					t.Fatalf("the read's token names %q for %s, want no patient on cdex-task-read", r.token.Subject, r.token.Operation)
				}
				if len(posts) != 1 {
					t.Fatalf("the Hub got %d records, want one", len(posts))
				}
				post := posts[0]
				var body shnsdk.RequesterBinding
				if err := json.Unmarshal(post.body, &body); err != nil || body.Validate() != nil {
					t.Fatalf("record %s: %v", post.body, err)
				}
				if body.RequestToken != r.raw || body.ResponseToken != r.resp || body.Binding != shnsdk.RequesterBindingBound || body.Involved == nil || body.Involved.Involvement != row.involvement {
					t.Fatalf("record %+v, want the leg's tokens as carried and a %s binding", body, row.involvement)
				}
				var inv shnsdk.Token
				_ = json.Unmarshal([]byte(body.Involved.Token), &inv)
				var resp shnsdk.Token
				_ = json.Unmarshal([]byte(r.resp), &resp)
				pci, held := p.boundPCIOf(t, row.member, task)
				if inv.Subject != pci || held != (row.involvement == shnsdk.InvolvementRequesterHeld) ||
					inv.Operation != "cdex-task-read-response" || inv.Frame != "provider-disclosure" || inv.PayloadHash != resp.PayloadHash || inv.CorrelationID != resp.CorrelationID {
					t.Fatalf("involved token %+v, want the payer's binding %s for the answer", inv, pci)
				}
				asked := authorized[len(authorized)-1]
				if asked.ResponseToken != r.resp || asked.Involvement != row.involvement {
					t.Fatalf("the binding's token was asked for with %+v, want the answer's own token", asked)
				}
				wantBodyBoundAssertion(t, post, p.sign)
				x := p.recs.only(t)
				if x.Outcome != ExchangeAnswered || x.Route != RouteCDexReadBase || x.Exchange != legCDexTaskStatus || x.Recipient != readProviderID {
					t.Fatalf("record %+v, want an answered read at the provider's base", x)
				}
				want := RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: row.involvement, Outcome: RequesterBindingAccepted}
				if x.RequesterBinding == nil || *x.RequesterBinding != want {
					t.Fatalf("requester binding %+v, want %+v", x.RequesterBinding, want)
				}
			})
		}
	}
}

// wantBodyBoundAssertion checks the record's X-Holder-Assertion is the
// payer's own, for the audience hub, and that its bh binds the body.
func wantBodyBoundAssertion(t *testing.T, post postedBinding, signPub ed25519.PublicKey) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(post.assertion)
	if err != nil {
		t.Fatalf("assertion: %v", err)
	}
	var a shnsdk.Assertion
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("assertion: %v", err)
	}
	sum := sha256.Sum256(post.body)
	if a.HolderID != "payer" || a.Audience != "hub" || a.BodyHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("assertion %+v, want the payer's for the hub binding the body", a)
	}
	if err := shnsdk.VerifyAssertion(a, "hub", signPub, ingressFixedClock()()); err != nil {
		t.Fatalf("assertion does not verify: %v", err)
	}
}

// What the Hub answers the record is kept apart, at every level, and the
// record is sent once whatever it is: accepted for its 204; refused, with the
// status, for its 400, 403, 409, 502, a 503 that does not state the record
// was never taken (no X-SHN-Delivered: no; the resend's rows are in
// requesterbinding_resend_test.go) and a redirect, which is never followed; unknown once any byte of the record was written and no
// answer came (a lost 204, the Hub dropping the connection partway through
// the record, the client's deadline running out); not sent when the Hub
// cannot be reached before a byte is written. The read's answer is the
// provider's Task in every case.
func TestCDexReadIngress_TheRecordsOutcomesAreKeptApart(t *testing.T) {
	for _, level := range allLevels {
		for _, row := range []struct {
			name         string
			status       int
			lose, closed bool
			want         RequesterBindingRecord
			posts        int
			hub          func(t *testing.T, h *readHub)
			opts         []func(*Config)
		}{
			{name: "a redirect", hub: func(_ *testing.T, h *readHub) { h.redirect = true },
				want: RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: http.StatusFound}, posts: 1},
			{name: "dropped partway through the record", hub: func(_ *testing.T, h *readHub) { h.partial = true },
				want: RequesterBindingRecord{Outcome: RequesterBindingUnknown}, posts: 1},
			{name: "no answer before the deadline", hub: func(t *testing.T, h *readHub) {
				h.hold = make(chan struct{})
				t.Cleanup(func() { close(h.hold) })
			}, opts: []func(*Config){func(c *Config) { c.Client.Timeout = 300 * time.Millisecond }},
				want: RequesterBindingRecord{Outcome: RequesterBindingUnknown}, posts: 1},
			{"204", http.StatusNoContent, false, false, RequesterBindingRecord{Outcome: RequesterBindingAccepted}, 1, nil, nil},
			{"400", http.StatusBadRequest, false, false, RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: 400}, 1, nil, nil},
			{"403", http.StatusForbidden, false, false, RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: 403}, 1, nil, nil},
			{"409", http.StatusConflict, false, false, RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: 409}, 1, nil, nil},
			{"503 not stating the record was never taken, final", http.StatusServiceUnavailable, false, false, RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: 503}, 1, nil, nil},
			{"502", http.StatusBadGateway, false, false, RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByHub, Status: 502}, 1, nil, nil},
			{"a lost 204: the Hub read the record, then dropped the connection", http.StatusNoContent, true, false, RequesterBindingRecord{Outcome: RequesterBindingUnknown}, 1, nil, nil},
			{"the Hub unreachable", 0, false, true, RequesterBindingRecord{Outcome: RequesterBindingNotSent, NotSent: RequesterNotSentHubUnreachable}, 0, nil, nil},
		} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := newReadPayer(t, level, row.opts...)
				task := readTaskAnswer(t, nil)
				p.hub.answerBody, p.hub.answerHeaders, p.hub.answerSubject = task, readAnswerHeaders, "pci:00000000000000000000000000000d15"
				if row.status != 0 {
					p.hub.bindingStatus, p.hub.lose = row.status, row.lose
				}
				if row.hub != nil {
					row.hub(t, p.hub)
				}
				if row.closed {
					l, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					p.hub.bindingTarget = "http://" + l.Addr().String()
					_ = l.Close()
				}
				rec := p.get(t, "/cdex/provider/"+readProviderID+"/Task/t-1")
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), task) {
					t.Fatalf("answer %d %s, want the provider's Task whatever became of the record", rec.Code, rec.Body)
				}
				if _, _, posts := p.hub.snapshot(); len(posts) != row.posts {
					t.Fatalf("the Hub got %d records, want %d: a record the Hub may have taken is never resent", len(posts), row.posts)
				}
				want := row.want
				want.Binding, want.Involvement = shnsdk.RequesterBindingBound, shnsdk.InvolvementRequesterDerived
				if x := p.recs.only(t); x.RequesterBinding == nil || *x.RequesterBinding != want {
					t.Fatalf("requester binding %+v, want %+v", x.RequesterBinding, want)
				}
			})
		}
	}
}

// No token for the binding means nothing is sent to the Hub, at every level,
// and the answer is delivered. A refusal the Framework states is recorded as
// the binding refused, by the Framework, with its status: its 403 denial, its
// 502 naming the rule it could not record the decision on, and an earlier
// deploy's 400. Anything else is not-sent for authorization-failed: a bare
// 502 (a proxy's), a 429, a 500, a 401, 404 or 409 (refused without a stated
// refusal), the Framework unreachable, and a token that does not carry the
// involvement asked for.
func TestCDexReadIngress_TheFrameworksRefusalSendsNothing(t *testing.T) {
	refused := func(status int) RequesterBindingRecord {
		return RequesterBindingRecord{Outcome: RequesterBindingRefused, RefusedBy: RefusedByAuthorizationFramework, Status: status}
	}
	notSent := RequesterBindingRecord{Outcome: RequesterBindingNotSent, NotSent: RequesterNotSentAuthorization}
	for _, level := range allLevels {
		for _, row := range []struct {
			name string
			hub  func(h *readHub)
			want RequesterBindingRecord
		}{
			{"403", func(h *readHub) { h.refuseInvolved = http.StatusForbidden }, refused(http.StatusForbidden)},
			{"502 naming its rule", func(h *readHub) {
				h.refuseInvolved, h.refuseBody = http.StatusBadGateway, `{"error":"decision not recorded","rule":"audit"}`
			}, refused(http.StatusBadGateway)},
			{"an earlier deploy's 400", func(h *readHub) { h.refuseInvolved = http.StatusBadRequest }, refused(http.StatusBadRequest)},
			{"a bare 502", func(h *readHub) { h.refuseInvolved = http.StatusBadGateway }, notSent},
			{"429", func(h *readHub) { h.refuseInvolved = http.StatusTooManyRequests }, notSent},
			{"500", func(h *readHub) { h.refuseInvolved = http.StatusInternalServerError }, notSent},
			{"401", func(h *readHub) { h.refuseInvolved = http.StatusUnauthorized }, notSent},
			{"404", func(h *readHub) { h.refuseInvolved = http.StatusNotFound }, notSent},
			{"409", func(h *readHub) { h.refuseInvolved = http.StatusConflict }, notSent},
			{"unreachable", func(h *readHub) { h.unreachable = true }, notSent},
			{"another involvement", func(h *readHub) { h.swapInvolvement = true }, notSent},
			{"a token that does not verify for the answer", func(h *readHub) { h.misbindInvolved = true }, notSent},
		} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := newReadPayer(t, level)
				task := readTaskAnswer(t, nil)
				p.hub.answerBody, p.hub.answerSubject = task, "pci:00000000000000000000000000000d15"
				row.hub(p.hub)
				if rec := p.get(t, "/cdex/provider/"+readProviderID+"/Task/t-1"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), task) {
					t.Fatalf("answer %d %s, want the provider's Task", rec.Code, rec.Body)
				}
				if _, _, posts := p.hub.snapshot(); len(posts) != 0 {
					t.Fatalf("the Hub got %d records with no token for the binding", len(posts))
				}
				want := row.want
				want.Binding, want.Involvement = shnsdk.RequesterBindingBound, shnsdk.InvolvementRequesterDerived
				if x := p.recs.only(t); x.RequesterBinding == nil || *x.RequesterBinding != want {
					t.Fatalf("requester binding %+v, want %+v", x.RequesterBinding, want)
				}
			})
		}
	}
}

// A Task the payer's gateway cannot bind is reported unavailable, with no
// token and no request for one, and why stays on its own record, at every
// level; the answer is still delivered: a Task.for that is only a reference
// into the provider's own server (§15.5's 2026-11-11 line), one naming two
// member numbers that differ, an answer that is not a Task, a member the
// payer's records do not hold when it requires known members, a member
// number shared with a Coverage's subscriber and no Patient of the member's
// own, and a system of record that cannot be read.
func TestCDexReadIngress_AnUnboundTaskIsReportedUnavailable(t *testing.T) {
	mb := map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}}
	for _, level := range allLevels {
		for _, row := range []struct {
			name    string
			answer  func(t *testing.T) []byte
			unbound string
			opts    []func(*Config)
		}{
			{"a member not held, known members required", func(t *testing.T) []byte { return readTaskAnswer(t, nil) }, RequesterUnboundUnknownMember,
				[]func(*Config){func(c *Config) { c.RequireKnownMembers = true }}},
			{"a member number shared with a subscriber", func(t *testing.T) []byte {
				return cdexRequestMutate(t, func(m map[string]any) {
					m["id"] = "t-1"
					m["for"] = map[string]any{"identifier": map[string]any{"type": mb, "value": "Member123"}}
					var keep []any
					for _, c := range m["contained"].([]any) {
						if c.(map[string]any)["resourceType"] != "Patient" {
							keep = append(keep, c)
						}
					}
					m["contained"] = append(keep, map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
						"subscriber": map[string]any{"reference": "#sub"}, "beneficiary": map[string]any{"reference": "Patient/other"},
						"contained": []any{map[string]any{"resourceType": "Patient", "id": "sub",
							"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "Member123"}}}}})
				})
			}, RequesterUnboundUnidentifiable, nil},
			{"a system of record that cannot be read", func(t *testing.T) []byte { return readTaskAnswer(t, nil) }, RequesterUnboundSystemOfRecord,
				[]func(*Config){func(c *Config) { c.SoR = readFailingSoR{newPrefetchSoR(), nil} }}},
			{"a bare Patient reference", func(t *testing.T) []byte { return readTaskAnswer(t, map[string]any{"reference": "Patient/123"}) }, RequesterUnboundNoMember, nil},
			{"two member numbers", func(t *testing.T) []byte {
				return readTaskAnswer(t, map[string]any{"reference": "#patient", "identifier": map[string]any{"type": mb, "value": "Other456"}})
			}, RequesterUnboundNoMember, nil},
			{"not a Task", func(t *testing.T) []byte { return []byte(`{"resourceType":"Basic","id":"t-1"}`) }, RequesterUnboundUnreadable, nil},
		} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := newReadPayer(t, level, row.opts...)
				answer := row.answer(t)
				p.hub.answerBody, p.hub.answerSubject = answer, "pci:00000000000000000000000000000d15"
				if rec := p.get(t, "/cdex/provider/"+readProviderID+"/Task/t-1"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want the provider's answer delivered", rec.Code, rec.Body)
				}
				_, authorized, posts := p.hub.snapshot()
				if len(posts) != 1 {
					t.Fatalf("the Hub got %d records, want one", len(posts))
				}
				var body shnsdk.RequesterBinding
				if err := json.Unmarshal(posts[0].body, &body); err != nil || body.Binding != shnsdk.RequesterBindingUnavailable || body.Involved != nil {
					t.Fatalf("record %s, want unavailable with no token", posts[0].body)
				}
				for _, a := range authorized {
					if a.Involvement != "" {
						t.Fatalf("a token was asked for an unbound binding: %+v", a)
					}
				}
				want := RequesterBindingRecord{Binding: shnsdk.RequesterBindingUnavailable, Unbound: row.unbound, Outcome: RequesterBindingAccepted}
				if x := p.recs.only(t); x.RequesterBinding == nil || *x.RequesterBinding != want {
					t.Fatalf("requester binding %+v, want %+v", x.RequesterBinding, want)
				}
			})
		}
	}
}

// An answer that discloses no patient sends no record, at every level: a
// 2xx whose token names none, and the provider's non-2xx answers (its own
// 404 and 410, the network's 404 for a Task the payer may not be delivered,
// the 502 for one the provider's gateway cannot bind), each delivered as
// sent with its status and media type.
func TestCDexReadIngress_AnAnswerNamingNoPatientSendsNoRecord(t *testing.T) {
	for _, level := range allLevels {
		for _, row := range []struct {
			name   string
			status int
			body   []byte
		}{
			{"a 2xx naming no patient", http.StatusOK, []byte(`{"resourceType":"Task","id":"t-1"}`)},
			{"the provider's 404", http.StatusNotFound, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"not-found"}]}`)},
			{"the provider's 410", http.StatusGone, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"deleted"}]}`)},
			{"the network's 404", http.StatusNotFound, cdexReadNotFound},
			{"the unbound 502", http.StatusBadGateway, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"the provider's gateway cannot bind the Task to a patient, so it is not delivered: x"}]}`)},
		} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := newReadPayer(t, level)
				p.hub.answerStatus, p.hub.answerBody = row.status, row.body
				p.hub.answerHeaders = map[string]string{"Content-Type": "application/fhir+json"}
				rec := p.get(t, "/cdex/provider/"+readProviderID+"/Task/t-1")
				if rec.Code != row.status || !bytes.Equal(rec.Body.Bytes(), row.body) || rec.Header().Get("Content-Type") != "application/fhir+json" {
					t.Fatalf("answer %d %q %s, want %d as sent", rec.Code, rec.Header().Get("Content-Type"), rec.Body, row.status)
				}
				if _, _, posts := p.hub.snapshot(); len(posts) != 0 {
					t.Fatalf("the Hub got %d records for an answer naming no patient", len(posts))
				}
				if x := p.recs.only(t); x.RequesterBinding != nil {
					t.Fatalf("requester binding %+v, want none", x.RequesterBinding)
				}
			})
		}
	}
}

// The record is never awaited by the payer's client: the read's answer is
// written in full while the Hub has not yet answered the record, and the
// call's exchange record is written once it has.
func TestCDexReadIngress_TheRecordNeverHoldsUpTheAnswer(t *testing.T) {
	p := newReadPayer(t, EnforcementNone)
	task := readTaskAnswer(t, nil)
	p.hub.answerBody, p.hub.answerSubject = task, "pci:00000000000000000000000000000d15"
	p.hub.hold = make(chan struct{})
	rec := p.getNoWait(t, "/cdex/provider/"+readProviderID+"/Task/t-1")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), task) {
		t.Fatalf("answer %d %s, want the provider's Task while the record is outstanding", rec.Code, rec.Body)
	}
	p.recs.mu.Lock()
	early := len(p.recs.recs)
	p.recs.mu.Unlock()
	close(p.hub.hold)
	p.g.waitAfterAnswers()
	if early != 0 {
		t.Fatalf("%d exchange records before the record was answered, want the record to wait for it", early)
	}
	if x := p.recs.only(t); x.RequesterBinding == nil || x.RequesterBinding.Outcome != RequesterBindingAccepted {
		t.Fatalf("requester binding %+v, want accepted", x.RequesterBinding)
	}
}

// The payer's gateway refuses a read on the network's own primitives, at
// every level, each recorded as its own refusal with nothing sent: a read or
// vread at the plain base (404, routing, its OperationOutcome naming where a
// Task is read); a path whose id or version is not a FHIR id, or a query
// that cannot be carried as one (400, routing); a base that names no
// registered provider (404, routing); and a read with a body (400, message
// integrity). The control is carried.
func TestCDexReadIngress_RefusesOnTheNetworksPrimitives(t *testing.T) {
	long := strings.Repeat("a", 65)
	for _, level := range allLevels {
		for _, row := range []struct {
			name   string
			target string
			query  string // a raw query set on the URL as is
			body   string
			status int
			rule   string
			msg    string
		}{
			{name: "control", target: "/cdex/provider/" + readProviderID + "/Task/t-1", status: http.StatusOK},
			{name: "a read at the plain base", target: "/Task/t-1", status: http.StatusNotFound, rule: RefusalRouting, msg: errCDexReadPlainBase},
			{name: "a vread at the plain base", target: "/Task/t-1/_history/1", status: http.StatusNotFound, rule: RefusalRouting, msg: errCDexReadPlainBase},
			{name: "an escaped slash in the id", target: "/cdex/provider/" + readProviderID + "/Task/a%2Fb", status: http.StatusBadRequest, rule: RefusalRouting, msg: errCDexReadPath},
			{name: "an escaped dot segment", target: "/cdex/provider/" + readProviderID + "/Task/%2E%2E", status: http.StatusBadRequest, rule: RefusalRouting, msg: errCDexReadPath},
			{name: "an escaped letter", target: "/cdex/provider/" + readProviderID + "/Task/abc%41", status: http.StatusBadRequest, rule: RefusalRouting, msg: errCDexReadPath},
			{name: "an id that is not a FHIR id", target: "/cdex/provider/" + readProviderID + "/Task/t_1", status: http.StatusBadRequest, rule: RefusalRouting, msg: errCDexReadPath},
			{name: "an id over 64", target: "/cdex/provider/" + readProviderID + "/Task/" + long, status: http.StatusBadRequest, rule: RefusalRouting, msg: errCDexReadPath},
			{name: "a version that is not a FHIR id", target: "/cdex/provider/" + readProviderID + "/Task/t-1/_history/v_1", status: http.StatusBadRequest, rule: RefusalRouting, msg: errCDexReadPath},
			{name: "a query with a space", target: "/cdex/provider/" + readProviderID + "/Task/t-1", query: "a b", status: http.StatusBadRequest, rule: RefusalRouting, msg: errCDexReadPath},
			{name: "an unregistered provider's base", target: "/cdex/provider/nobody/Task/t-1", status: http.StatusNotFound, rule: RefusalRouting, msg: errCDexRequestNoProvider},
			{name: "a payer's base", target: "/cdex/provider/payer/Task/t-1", status: http.StatusNotFound, rule: RefusalRouting, msg: errCDexRequestNoProvider},
			{name: "a body", target: "/cdex/provider/" + readProviderID + "/Task/t-1", body: `{"resourceType":"Task"}`, status: http.StatusBadRequest, rule: RefusalIntegrity, msg: errCDexReadBody},
		} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := newReadPayer(t, level)
				p.hub.answerBody, p.hub.answerSubject = readTaskAnswer(t, nil), "pci:00000000000000000000000000000d15"
				req := httptest.NewRequest(http.MethodGet, testIngressBaseURL+row.target, strings.NewReader(row.body))
				if row.query != "" {
					req.URL.RawQuery = row.query
				}
				rec := httptest.NewRecorder()
				p.g.Handler().ServeHTTP(rec, req)
				p.g.waitAfterAnswers()
				routed, _, _ := p.hub.snapshot()
				x := p.recs.only(t)
				if row.status == http.StatusOK {
					if rec.Code != http.StatusOK || len(routed) != 1 {
						t.Fatalf("control: %d %s, routed %d", rec.Code, rec.Body, len(routed))
					}
					return
				}
				if rec.Code != row.status || rec.Header().Get("Content-Type") != "application/fhir+json" || !strings.Contains(rec.Body.String(), row.msg) {
					t.Fatalf("answer %d %q %s, want %d naming %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body, row.status, row.msg)
				}
				if row.status == http.StatusNotFound && !strings.Contains(rec.Body.String(), `"code":"not-found"`) {
					t.Fatalf("answer %s, want an OperationOutcome of code not-found", rec.Body)
				}
				if len(routed) != 0 {
					t.Fatalf("%d reads routed, want nothing sent", len(routed))
				}
				if x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByPayerGateway || x.Rule != row.rule || x.Status != row.status || x.Exchange != legCDexTaskStatus {
					t.Fatalf("record %+v, want refused payer-gateway/%s %d on %s", x, row.rule, row.status, legCDexTaskStatus)
				}
				if !strings.Contains(row.msg, x.RefusalReason) || x.RefusalReason == "" {
					t.Fatalf("refusal reason %q, want the fixed sentence of %q", x.RefusalReason, row.msg)
				}
			})
		}
	}
}

// The read routes take a bearer whose scopes permit reading a Task, at every
// level, at the provider's base and the plain base alike: one granted only the
// create scope is refused 403 insufficient_scope naming the read scope,
// recorded as the payer gateway's authority refusal, with nothing sent; no
// bearer is 401; one granted the v1 read, a v2 r, both scopes in one token, or
// v1 *, is admitted. A granular read permits nothing.
func TestCDexReadIngress_TakesAScopeThatPermitsRead(t *testing.T) {
	priv, pub := newTestClientKey(t)
	scopes := []string{TaskWriteScope, TaskReadScope, "system/Task.r", "system/Task.cr", "system/Task.*", "system/Task.r?status=requested"}
	for _, level := range allLevels {
		for _, row := range []struct {
			scope  string
			status int
		}{
			{"", http.StatusUnauthorized},
			{TaskWriteScope, http.StatusForbidden},
			{"system/Task.r?status=requested", http.StatusForbidden},
			{TaskReadScope, http.StatusOK},
			{"system/Task.r", http.StatusOK},
			{"system/Task.cr", http.StatusOK},
			{TaskWriteScope + " " + TaskReadScope, http.StatusOK},
			{"system/Task.*", http.StatusOK},
		} {
			for _, base := range []string{"/cdex/provider/" + readProviderID, "/cdex/provider/" + readProviderID + "/Task/t-1/_history/2", ""} {
				t.Run(fmt.Sprintf("%q at %q/%s", row.scope, base, level), func(t *testing.T) {
					p := newReadPayer(t, level, func(c *Config) {
						c.IngressClients = map[string]IngressClientRegistration{"payer-system": {Alg: "ES384", PublicKeyPEM: pub, Scopes: scopes}}
						c.ingressAuthBypass = false
					})
					p.hub.answerBody = readTaskAnswer(t, nil)
					target := base + "/Task/t-1"
					if strings.Contains(base, "/_history/") {
						target = base // a vread
					}
					req := httptest.NewRequest(http.MethodGet, testIngressBaseURL+target, nil)
					if row.status != http.StatusUnauthorized {
						req.Header.Set("Authorization", "Bearer "+scopedBearer(t, p.g, priv, "payer-system", row.scope))
					}
					rec := httptest.NewRecorder()
					p.g.Handler().ServeHTTP(rec, req)
					p.g.waitAfterAnswers()
					want := row.status
					if want == http.StatusOK && base == "" {
						want = http.StatusNotFound // admitted, then the plain base's own answer
					}
					if rec.Code != want {
						t.Fatalf("answer %d %s, want %d", rec.Code, rec.Body, want)
					}
					routed, _, _ := p.hub.snapshot()
					if (want == http.StatusOK) != (len(routed) == 1) {
						t.Fatalf("routed %d reads at %d", len(routed), want)
					}
					x := p.recs.only(t)
					switch want {
					case http.StatusForbidden:
						if ch := rec.Header().Get("WWW-Authenticate"); !strings.Contains(ch, `insufficient_scope`) || !strings.Contains(ch, `scope="`+TaskReadScope+`"`) {
							t.Fatalf("challenge %q, want insufficient_scope naming %s", ch, TaskReadScope)
						}
						if x.Rule != RefusalAuthority || x.RefusedBy != RefusedByPayerGateway || x.RefusalReason != string(reasonScopeTaskRead) {
							t.Fatalf("record %+v, want the payer gateway's authority refusal", x)
						}
					case http.StatusUnauthorized:
						if x.Rule != RefusalAuthentication {
							t.Fatalf("record %+v, want authentication", x)
						}
					}
				})
			}
		}
	}
}

// The read's discovery at every base: /cdex/providers marks the bases that
// serve reads; the plain base's CapabilityStatement declares Task create
// only, a reading provider's base create, read and vread, and an older
// provider's base create only; a base naming no listed provider has none. A
// provider's base serves the SMART configuration, which advertises each read
// scope a native client is registered for exactly, since only such a string is
// granted, and none a module client is: a client registered only
// system/Task.* sees system/Task.*, never system/Task.read, which its token
// request is refused. Each at every level.
func TestCDexReadIngress_Discovery(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) { checkCDexReadDiscovery(t, level) })
	}
}

func checkCDexReadDiscovery(t *testing.T, level ConformanceEnforcement) {
	_, pub := newTestClientKey(t)
	interactions := func(t *testing.T, body []byte) []string {
		t.Helper()
		var cs struct {
			ResourceType string `json:"resourceType"`
			Rest         []struct {
				Resource []struct {
					Type        string `json:"type"`
					Interaction []struct {
						Code string `json:"code"`
					} `json:"interaction"`
				} `json:"resource"`
			} `json:"rest"`
		}
		if err := json.Unmarshal(body, &cs); err != nil || cs.ResourceType != "CapabilityStatement" || len(cs.Rest) != 1 || len(cs.Rest[0].Resource) != 1 {
			t.Fatalf("not one CapabilityStatement of one resource: %s", body)
		}
		var out []string
		for _, i := range cs.Rest[0].Resource[0].Interaction {
			out = append(out, i.Code)
		}
		return out
	}
	p := newReadPayer(t, level)
	rec := p.get(t, CDexProvidersPath)
	var listing struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	reads := map[string]any{}
	for _, e := range listing.Providers {
		reads[e["participant"].(string)] = e["reads"]
	}
	if reads[readProviderID] != true || reads["older-provider"] != false || len(reads) != 2 {
		t.Fatalf("listing %s, want reads true for %s and false for older-provider", rec.Body, readProviderID)
	}
	for path, want := range map[string][]string{
		"/metadata": {"create"},
		"/cdex/provider/" + readProviderID + "/metadata": {"create", "read", "vread"},
		"/cdex/provider/older-provider/metadata":         {"create"},
	} {
		rec := p.get(t, path)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/fhir+json" {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		if got := interactions(t, rec.Body.Bytes()); !slices.Equal(got, want) {
			t.Errorf("%s declares %v, want %v", path, got, want)
		}
	}
	if rec := p.get(t, "/cdex/provider/nobody/metadata"); rec.Code != http.StatusNotFound {
		t.Fatalf("an unlisted base's metadata: %d %s, want 404", rec.Code, rec.Body)
	}

	for _, row := range []struct {
		scopes, other []string
		module        []string
		want          []string
	}{
		{scopes: []string{TaskWriteScope}, want: []string{TaskWriteScope}},
		{scopes: []string{TaskWriteScope, "system/Task.r?status=requested"}, want: []string{TaskWriteScope}},
		{scopes: []string{TaskWriteScope, TaskReadScope}, want: []string{TaskWriteScope, TaskReadScope}},
		{scopes: []string{TaskWriteScope, "system/Task.r"}, want: []string{TaskWriteScope, "system/Task.r"}},
		{scopes: []string{"system/Task.*"}, want: []string{TaskWriteScope, "system/Task.*"}},
		{scopes: []string{"system/Task.cruds"}, want: []string{TaskWriteScope, "system/Task.cruds"}},
		{scopes: []string{TaskWriteScope, TaskReadScope}, other: []string{"system/Task.cr", TaskReadScope},
			want: []string{TaskWriteScope, "system/Task.cr", TaskReadScope}},
		{scopes: []string{TaskWriteScope}, module: []string{TaskWriteScope, TaskReadScope}, want: []string{TaskWriteScope}},
	} {
		p := newReadPayer(t, level, func(c *Config) {
			c.IngressClients = map[string]IngressClientRegistration{"payer-system": {Alg: "ES384", PublicKeyPEM: pub, Scopes: row.scopes}}
			if row.other != nil {
				c.IngressClients["payer-system-2"] = IngressClientRegistration{Alg: "ES384", PublicKeyPEM: pub, Scopes: row.other}
			}
			if row.module != nil {
				c.IngressClients["module-client"] = IngressClientRegistration{Alg: "ES384", PublicKeyPEM: pub, Scopes: row.module, Module: ModuleIDs()[0]}
			}
			c.ingressAuthBypass = false
		})
		for _, path := range []string{"/.well-known/smart-configuration", "/cdex/provider/" + readProviderID + "/.well-known/smart-configuration"} {
			rec := p.get(t, path)
			var conf struct {
				TokenEndpoint string   `json:"token_endpoint"`
				Scopes        []string `json:"scopes_supported"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &conf); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
			}
			if conf.TokenEndpoint != testIngressBaseURL+ingressTokenPath || !slices.Equal(conf.Scopes, row.want) {
				t.Errorf("%v at %s: %+v, want scopes %v", row.scopes, path, conf, row.want)
			}
		}
		if rec := p.get(t, "/cdex/provider/nobody/.well-known/smart-configuration"); rec.Code != http.StatusNotFound {
			t.Fatalf("an unlisted base's SMART configuration: %d, want 404", rec.Code)
		}
	}
}

// Every read scope the SMART configuration advertises is one the token
// endpoint grants a registered client, and a read scope it does not advertise
// is not: a client registered only system/Task.* is granted system/Task.*,
// which the read routes take, and refused system/Task.read 400 invalid_scope,
// which is why it is not advertised.
func TestCDexReadIngress_AnAdvertisedReadScopeIsGranted(t *testing.T) {
	priv, pub := newTestClientKey(t)
	for _, scopes := range [][]string{{"system/Task.*"}, {TaskWriteScope, TaskReadScope}, {TaskWriteScope, "system/Task.rs"}} {
		p := newReadPayer(t, EnforcementNone, func(c *Config) {
			c.IngressClients = map[string]IngressClientRegistration{"payer-system": {Alg: "ES384", PublicKeyPEM: pub, Scopes: scopes}}
			c.ingressAuthBypass = false
		})
		rec := p.get(t, "/.well-known/smart-configuration")
		var conf struct {
			Scopes []string `json:"scopes_supported"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &conf); err != nil {
			t.Fatal(err)
		}
		advertised := 0
		for _, scope := range conf.Scopes {
			if !scopesPermit([]string{scope}, taskRead) {
				continue
			}
			advertised++
			p.hub.answerBody = readTaskAnswer(t, nil)
			req := httptest.NewRequest(http.MethodGet, testIngressBaseURL+"/cdex/provider/"+readProviderID+"/Task/t-1", nil)
			req.Header.Set("Authorization", "Bearer "+scopedBearer(t, p.g, priv, "payer-system", scope))
			got := httptest.NewRecorder()
			p.g.Handler().ServeHTTP(got, req)
			p.g.waitAfterAnswers()
			if got.Code != http.StatusOK {
				t.Fatalf("%v: a read under the advertised %s: %d %s", scopes, scope, got.Code, got.Body)
			}
		}
		if advertised != 1 {
			t.Fatalf("%v: advertised %v, want the one read scope registered", scopes, conf.Scopes)
		}
		if slices.Contains(scopes, TaskReadScope) {
			continue
		}
		if slices.Contains(conf.Scopes, TaskReadScope) {
			t.Fatalf("%v: advertised %s, which no client registered", scopes, TaskReadScope)
		}
		claims := validClaims("payer-system", p.g.ingressAuth.tokenURL(), ingressFixedClock()())
		claims["jti"] = "jti-unadvertised-" + strings.Join(scopes, "-")
		if rec := postTokenScope(t, p.g.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, claims), TaskReadScope); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_scope") {
			t.Fatalf("%v: a token for the unadvertised %s: %d %s, want 400 invalid_scope", scopes, TaskReadScope, rec.Code, rec.Body)
		}
	}
}

// A record that cannot be built here is not-sent for record-unbuildable,
// with nothing dialed: one over the record's limit (a token too long to
// carry), and one whose holder assertion cannot be made (no signing key).
func TestCDexReadIngress_AnUnbuildableRecordIsNotSent(t *testing.T) {
	t.Run("over the limit", func(t *testing.T) {
		p := newReadPayer(t, EnforcementNone)
		got := p.g.reportRequesterBinding(t.Context(), legCDexTaskStatus,
			legTokens{request: strings.Repeat("x", shnsdk.MaxRequesterBindingBytes), response: "{}"}, requesterBound{unbound: RequesterUnboundNoMember})
		want := RequesterBindingRecord{Binding: shnsdk.RequesterBindingUnavailable, Unbound: RequesterUnboundNoMember, Outcome: RequesterBindingNotSent, NotSent: RequesterNotSentUnbuildable}
		if got != want {
			t.Fatalf("%+v, want %+v", got, want)
		}
		if _, _, posts := p.hub.snapshot(); len(posts) != 0 {
			t.Fatalf("the Hub got %d records", len(posts))
		}
	})
	// The holder assertion cannot be made: nothing is dialed. (A gateway
	// with no signing key sends no leg either, so the report is driven alone.)
	t.Run("no signing key", func(t *testing.T) {
		p := newReadPayer(t, EnforcementNone)
		p.g.cfg.Identity.SignPriv = nil
		got := p.g.reportRequesterBinding(t.Context(), legCDexTaskStatus,
			legTokens{request: "{}", response: "{}"}, requesterBound{unbound: RequesterUnboundNoMember})
		want := RequesterBindingRecord{Binding: shnsdk.RequesterBindingUnavailable, Unbound: RequesterUnboundNoMember, Outcome: RequesterBindingNotSent, NotSent: RequesterNotSentUnbuildable}
		if got != want {
			t.Fatalf("%+v, want %+v", got, want)
		}
		if _, _, posts := p.hub.snapshot(); len(posts) != 0 {
			t.Fatalf("the Hub got %d records", len(posts))
		}
	})
}

// A stop waits a bounded time for the reports in flight, then cuts them, and
// every read's exchange record is written, at every level: a record whose
// bytes were written to the Hub is unknown; one cut before any byte was
// written is other, the binding as far as it was known (the cut ended it, not
// a stated failure): cut while its token was asked for, or after its token
// but before it was dialed; one whose binding hangs past the cut is written
// all the same, binding unavailable and other. The stop's step is bounded by
// the 1 s DEPLOYMENT.md states ("Stopping the gateway").
func TestCDexReadIngress_AStopSettlesTheRecordsInFlight(t *testing.T) {
	for _, level := range allLevels {
		for _, row := range []struct {
			name string
			set  func(t *testing.T, p *readPayer)
			opts []func(*Config)
			want RequesterBindingRecord
		}{
			{"written, no answer", func(t *testing.T, p *readPayer) {
				p.hub.hold = make(chan struct{})
				t.Cleanup(func() { close(p.hub.hold) })
			}, nil, RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: shnsdk.InvolvementRequesterDerived, Outcome: RequesterBindingUnknown}},
			{"asking for the token", func(t *testing.T, p *readPayer) {
				p.hub.authorizeHold = make(chan struct{})
				t.Cleanup(func() { close(p.hub.authorizeHold) })
			}, nil, RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: shnsdk.InvolvementRequesterDerived, Outcome: ExchangeOther}},
			{"after the token, before the dial", func(t *testing.T, p *readPayer) {
				p.hub.dialHold = make(chan struct{})
				t.Cleanup(func() { close(p.hub.dialHold) })
			}, nil, RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: shnsdk.InvolvementRequesterDerived, Outcome: ExchangeOther}},
			{"binding hangs past the cut", nil, []func(*Config){func(c *Config) {
				block := make(chan struct{})
				c.SoR = readFailingSoR{newPrefetchSoR(), block}
				t.Cleanup(func() { close(block) })
			}}, RequesterBindingRecord{Binding: shnsdk.RequesterBindingUnavailable, Outcome: ExchangeOther}},
		} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := newReadPayer(t, level, row.opts...)
				task := readTaskAnswer(t, nil)
				p.hub.answerBody, p.hub.answerSubject = task, "pci:00000000000000000000000000000d15"
				if row.set != nil {
					row.set(t, p)
				}
				if rec := p.getNoWait(t, "/cdex/provider/"+readProviderID+"/Task/t-1"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), task) {
					t.Fatalf("answer %d %s, want the provider's Task", rec.Code, rec.Body)
				}
				// Let the report reach the step the row holds it at.
				deadline := time.Now().Add(5 * time.Second)
				for {
					_, authorized, posts := p.hub.snapshot()
					p.hub.mu.Lock()
					dialing := p.hub.dialing
					p.hub.mu.Unlock()
					reached := len(posts) > 0 || dialing > 0 || (p.hub.dialHold == nil && slices.ContainsFunc(authorized, func(a authorizeReq) bool { return a.Involvement != "" }))
					if row.set == nil {
						reached = true
						time.Sleep(50 * time.Millisecond)
					}
					if reached || time.Now().After(deadline) {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				start := time.Now()
				p.g.stopAfterAnswers()
				// The documented step, two waits, and a little for the records
				// to be written.
				if bound := 2*afterAnswerStopWait + 250*time.Millisecond; time.Since(start) > bound {
					t.Fatalf("the stop took %v, over the documented 1 s step (two waits of %v) and its slack", time.Since(start), afterAnswerStopWait)
				}
				if x := p.recs.only(t); x.RequesterBinding == nil || *x.RequesterBinding != row.want {
					t.Fatalf("requester binding %+v, want %+v", x.RequesterBinding, row.want)
				}
			})
		}
	}
}

// A HEAD on a read route is served as the GET it is, without the body, at
// every level (RFC 9110 §9.3.2), at a provider's base (read and vread) and at
// the plain base (read and vread): the same status and headers as the GET,
// no body, the read carried through the network as for the GET, the record
// and its access line saying HEAD, and, for a 2xx naming a patient, the
// binding record sent once and accepted, as a HEAD still learns the Task's
// existence, status and validators. Both ask for gzip and read the answer as
// sent: the HEAD's Content-Encoding and Content-Length are the GET's, and its
// record counts no answer bytes, since none were sent.
func TestCDexReadIngress_AHeadIsServedAsTheGet(t *testing.T) {
	for _, level := range allLevels {
		for _, row := range []struct {
			target string
			status int
			record bool
		}{
			{"/cdex/provider/" + readProviderID + "/Task/t-1", http.StatusOK, true},
			{"/cdex/provider/" + readProviderID + "/Task/t-1/_history/2", http.StatusOK, true},
			{"/Task/t-1", http.StatusNotFound, false},
			{"/Task/t-1/_history/2", http.StatusNotFound, false},
		} {
			t.Run(row.target+"/"+level.String(), func(t *testing.T) {
				do := func(method string) (*http.Response, []byte, ExchangeRecord, int) {
					p := newReadPayer(t, level)
					p.hub.answerBody, p.hub.answerHeaders, p.hub.answerSubject = readTaskAnswer(t, nil), readAnswerHeaders, "pci:00000000000000000000000000000d15"
					srv := httptest.NewServer(p.g.Handler())
					defer srv.Close()
					req, err := http.NewRequest(method, srv.URL+row.target, nil)
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Accept-Encoding", "gzip")
					client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					p.g.waitAfterAnswers()
					routed, _, posts := p.hub.snapshot()
					want := 0
					if row.status == http.StatusOK {
						want = 1
					}
					if len(routed) != want {
						t.Fatalf("%s: %d reads routed, want %d", method, len(routed), want)
					}
					return resp, body, p.recs.only(t), len(posts)
				}
				get, getBody, getRec, getPosts := do(http.MethodGet)
				head, headBody, headRec, headPosts := do(http.MethodHead)
				if head.StatusCode != row.status || get.StatusCode != row.status || len(headBody) != 0 || len(getBody) == 0 {
					t.Fatalf("HEAD %d with %d bytes, GET %d with %d, want %d, HEAD with no body", head.StatusCode, len(headBody), get.StatusCode, len(getBody), row.status)
				}
				for _, name := range []string{"Content-Type", "ETag", "Last-Modified", "Content-Encoding", "Content-Length", "Vary"} {
					if head.Header.Get(name) != get.Header.Get(name) {
						t.Errorf("HEAD %s %q, GET %q", name, head.Header.Get(name), get.Header.Get(name))
					}
				}
				if row.status == http.StatusOK && get.Header.Get("Content-Encoding") != "gzip" {
					t.Fatalf("control: the GET's Task was not coded (%d bytes)", len(getBody))
				}
				if headRec.ResponseBytes == nil || *headRec.ResponseBytes != 0 || getRec.ResponseBytes == nil || *getRec.ResponseBytes == 0 {
					t.Fatalf("response bytes: HEAD %v, GET %v; want 0 for the HEAD", headRec.ResponseBytes, getRec.ResponseBytes)
				}
				if headRec.Method != http.MethodHead || getRec.Method != "" || headRec.Outcome != getRec.Outcome || headRec.Status != getRec.Status {
					t.Fatalf("records: HEAD method %q %s %d, GET method %q %s %d", headRec.Method, headRec.Outcome, headRec.Status, getRec.Method, getRec.Outcome, getRec.Status)
				}
				if !row.record {
					if headPosts != 0 || headRec.RequesterBinding != nil {
						t.Fatalf("the plain base's HEAD sent %d records", headPosts)
					}
					return
				}
				want := RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: shnsdk.InvolvementRequesterDerived, Outcome: RequesterBindingAccepted}
				if headPosts != 1 || getPosts != 1 || headRec.RequesterBinding == nil || *headRec.RequesterBinding != want {
					t.Fatalf("HEAD sent %d records (%+v), GET %d, want one each, accepted", headPosts, headRec.RequesterBinding, getPosts)
				}
			})
		}
	}
}

// A module ingress client builds no read: its read is refused before
// anything is sent, at every level, whatever builder it names, and a native
// client naming one is refused too (ingressBuilder).
func TestCDexReadIngress_AModuleClientsReadIsRefused(t *testing.T) {
	priv, pub := newTestClientKey(t)
	for _, level := range allLevels {
		for _, row := range []struct {
			name, module, builder string
		}{
			{"a module client naming a builder", ModuleOnBehalf, string(relay.BuilderSDKEligibility)},
			{"a module client naming none", ModuleOnBehalf, ""},
			{"a native client naming a builder", "", string(relay.BuilderSDKEligibility)},
		} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := newReadPayer(t, level, func(c *Config) {
					c.IngressClients = map[string]IngressClientRegistration{"client": {Alg: "ES384", PublicKeyPEM: pub, Scopes: []string{TaskWriteScope, TaskReadScope}, Module: row.module}}
					c.ingressAuthBypass = false
				})
				p.hub.answerBody = readTaskAnswer(t, nil)
				req := httptest.NewRequest(http.MethodGet, testIngressBaseURL+"/cdex/provider/"+readProviderID+"/Task/t-1", nil)
				req.Header.Set("Authorization", "Bearer "+scopedBearer(t, p.g, priv, "client", TaskReadScope))
				if row.builder != "" {
					req.Header.Set(BuilderHeader, row.builder)
				}
				rec := httptest.NewRecorder()
				p.g.Handler().ServeHTTP(rec, req)
				p.g.waitAfterAnswers()
				if rec.Code/100 != 4 {
					t.Fatalf("answer %d %s, want the builder refusal", rec.Code, rec.Body)
				}
				if routed, _, _ := p.hub.snapshot(); len(routed) != 0 {
					t.Fatalf("%d reads routed, want none", len(routed))
				}
			})
		}
	}
}

// A record is due only for an answer delivered to the payer's system as a
// 2xx whose token names a patient: one whose delivery was refused here (its
// transmit check wrote this gateway's fault instead) disclosed nothing to the
// payer, and has none.
func TestCDexReadRecordDue(t *testing.T) {
	toks := legTokens{request: "{}", response: "{}", responseToken: shnsdk.Token{Subject: "pci:00000000000000000000000000000d15"}}
	ok := statedAnswer{status: http.StatusOK}
	for _, row := range []struct {
		name      string
		delivered bool
		stated    statedAnswer
		toks      legTokens
		want      bool
	}{
		{"delivered 2xx naming a patient", true, ok, toks, true},
		{"not delivered", false, ok, toks, false},
		{"a 404", true, statedAnswer{status: http.StatusNotFound}, toks, false},
		{"naming no patient", true, ok, legTokens{request: "{}", response: "{}"}, false},
		{"no answer token", true, ok, legTokens{request: "{}", responseToken: toks.responseToken}, false},
	} {
		if got := requesterBindingDue(row.delivered, row.stated, row.toks); got != row.want {
			t.Errorf("%s: %v, want %v", row.name, got, row.want)
		}
	}
}

// A report that ends abnormally is recorded other with the binding as far as
// it was known, and unavailable when the patient was not yet bound; never an
// empty binding.
func TestRequesterBindingAborted(t *testing.T) {
	for _, row := range []struct {
		name    string
		pending *RequesterBindingRecord
		want    RequesterBindingRecord
	}{
		{"before the binding", nil, RequesterBindingRecord{Binding: shnsdk.RequesterBindingUnavailable, Outcome: ExchangeOther}},
		{"bound", &RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: shnsdk.InvolvementRequesterHeld},
			RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: shnsdk.InvolvementRequesterHeld, Outcome: ExchangeOther}},
		{"unbound", &RequesterBindingRecord{Binding: shnsdk.RequesterBindingUnavailable, Unbound: RequesterUnboundNoMember},
			RequesterBindingRecord{Binding: shnsdk.RequesterBindingUnavailable, Unbound: RequesterUnboundNoMember, Outcome: ExchangeOther}},
	} {
		x := &exchangeRecorder{}
		if row.pending != nil {
			x.requesterBindingPending(*row.pending)
			x.requesterBindingMaySend()
		}
		x.requesterBindingAborted()
		if got := x.settledRequesterBinding(); got == nil || *got != row.want {
			t.Errorf("%s: %+v, want %+v", row.name, got, row.want)
		}
	}
}

// A stop's step for the binding records under way is the 1 s DEPLOYMENT.md
// states ("Stopping the gateway"): two waits of afterAnswerStopWait.
func TestStopAfterAnswersWaitsTheDocumentedBound(t *testing.T) {
	if 2*afterAnswerStopWait != time.Second {
		t.Fatalf("the stop waits up to %v for binding records, DEPLOYMENT.md states 1 s", 2*afterAnswerStopWait)
	}
}

// waitAfterAnswers waits until every call's work after its answer, and every
// exchange record waiting for it, is done.
func (g *Gateway) waitAfterAnswers() {
	g.afterAnswers.Wait()
	g.afterStop().records.Wait()
}

// A report whose budget runs out before any byte of its record is written,
// here on a system of record that neither answers nor fails within it, is
// other, binding unavailable, not a failure recorded by its cause: a hung
// Framework, Hub or system of record lands here alike. At
// every level; nothing reaches the Hub.
func TestCDexReadIngress_ABudgetRunOutBeforeTheRecordIsOther(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newReadPayer(t, level, func(c *Config) { c.SoR = slowSoR{newPrefetchSoR()} })
			p.g.bindingBudgetForTest = 200 * time.Millisecond
			task := readTaskAnswer(t, nil)
			p.hub.answerBody, p.hub.answerSubject = task, "pci:00000000000000000000000000000d15"
			if rec := p.get(t, "/cdex/provider/"+readProviderID+"/Task/t-1"); rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body)
			}
			if _, _, posts := p.hub.snapshot(); len(posts) != 0 {
				t.Fatalf("the Hub got %d records", len(posts))
			}
			want := RequesterBindingRecord{Binding: shnsdk.RequesterBindingUnavailable, Outcome: ExchangeOther}
			if x := p.recs.only(t); x.RequesterBinding == nil || *x.RequesterBinding != want {
				t.Fatalf("requester binding %+v, want %+v", x.RequesterBinding, want)
			}
		})
	}
}

// slowSoR is a payer's system of record whose member reads last until their
// context ends.
type slowSoR struct{ *prefetchSoR }

func (slowSoR) ResolvePatientContext(ctx context.Context, _ string) (string, Demo, bool, error) {
	<-ctx.Done()
	return "", Demo{}, false, &SoRReadError{Kind: SoRUnavailable}
}
