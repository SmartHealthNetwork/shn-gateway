package engine

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

// exchangeRecords collects the records a gateway hands ExchangeObserved.
type exchangeRecords struct {
	mu   sync.Mutex
	recs []ExchangeRecord
}

func (c *exchangeRecords) observe(r ExchangeRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r)
}

// only returns the one record a call produced.
func (c *exchangeRecords) only(t *testing.T) ExchangeRecord {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.recs) != 1 {
		t.Fatalf("want exactly one exchange record, got %d: %+v", len(c.recs), c.recs)
	}
	rec := c.recs[0]
	c.recs = nil
	wantClosed(t, rec)
	return rec
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// wantClosed asserts every classified field holds a value its closed list
// names — the property a metric dimension relies on.
func wantClosed(t *testing.T, rec ExchangeRecord) {
	t.Helper()
	for name, v := range map[string]struct {
		list []string
		v    string
	}{
		"Direction": {ExchangeDirections, rec.Direction},
		"Route":     {ExchangeRoutes, rec.Route},
		"Exchange":  {ExchangeKinds, rec.Exchange},
		"Outcome":   {ExchangeOutcomes, rec.Outcome},
	} {
		if !slices.Contains(v.list, v.v) {
			t.Errorf("%s = %q, not in its closed list", name, v.v)
		}
	}
	if rec.Outcome == ExchangeRefused {
		if !slices.Contains(RefusalParties, rec.RefusedBy) || !slices.Contains(RefusalRules, rec.Rule) {
			t.Errorf("refusal %q/%q not in the closed lists", rec.RefusedBy, rec.Rule)
		}
	} else if rec.RefusedBy != "" || rec.Rule != "" {
		t.Errorf("RefusedBy/Rule %q/%q set on outcome %q", rec.RefusedBy, rec.Rule, rec.Outcome)
	}
	if b := rec.Backend; b != nil && b.ErrorClass != "" && !slices.Contains(BackendErrorClasses, b.ErrorClass) {
		t.Errorf("Backend.ErrorClass %q not in its closed list", b.ErrorClass)
	}
	for _, k := range rec.Findings.Kinds {
		switch CheckKind(k) {
		case KindFHIRIngress, KindFHIREgress, KindFHIRBridged, KindCDSEnvelope, KindNetwork, KindContent:
		default:
			t.Errorf("Findings.Kinds names %q, not a CheckKind", k)
		}
	}
}

func wantRefusal(t *testing.T, rec ExchangeRecord, status int, by, rule string) {
	t.Helper()
	if rec.Outcome != ExchangeRefused || rec.RefusedBy != by || rec.Rule != rule || rec.Status != status {
		t.Fatalf("record = %s %s/%s status %d, want refused %s/%s status %d", rec.Outcome, rec.RefusedBy, rec.Rule, rec.Status, by, rule, status)
	}
}

// Every closed list ends in "other" and names each value once, and the
// exchange kinds are exactly the PA catalog's legs: a new leg cannot be
// recorded as "other" by omission.
func TestExchangeKindsAreThePACatalog(t *testing.T) {
	for name, list := range map[string][]string{
		"ExchangeDirections": ExchangeDirections, "ExchangeRoutes": ExchangeRoutes, "ExchangeKinds": ExchangeKinds,
		"ExchangeOutcomes": ExchangeOutcomes, "RefusalParties": RefusalParties, "RefusalRules": RefusalRules,
		"BackendErrorClasses": BackendErrorClasses,
	} {
		if list[len(list)-1] != ExchangeOther {
			t.Errorf("%s does not end in %q", name, ExchangeOther)
		}
		seen := map[string]bool{}
		for _, v := range list {
			if seen[v] {
				t.Errorf("%s names %q twice", name, v)
			}
			seen[v] = true
		}
	}
	var legs []string
	for leg := range paCatalog {
		legs = append(legs, leg)
	}
	kinds := slices.Clone(ExchangeKinds[:len(ExchangeKinds)-1])
	slices.Sort(legs)
	slices.Sort(kinds)
	if !slices.Equal(legs, kinds) {
		t.Fatalf("ExchangeKinds %v, want the PA catalog's legs %v", kinds, legs)
	}
	for route := range ingressRoutes {
		if !slices.Contains(ExchangeRoutes, route) {
			t.Errorf("ingress route %q is not in ExchangeRoutes", route)
		}
	}
}

// The provider side, through the route as mounted: success, the counterpart's
// non-2xx answer, and the ids the record carries.
func TestExchangeRecord_IngressAnsweredAndUpstreamError(t *testing.T) {
	env := newInProcessExchange(t)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	env.originator.cfg.CorrelationGen = func() string { return "leg-corr-0077" }
	h := env.originator.ingressRoute(RouteCRD)

	env.payerReturns(LegResult{})
	req := env.crdIngressRequest(t)
	req.Header.Set(CorrelationHeader, "caller-trace-0077")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	r := got.only(t)
	if r.Outcome != ExchangeAnswered || r.Status != http.StatusOK || r.Direction != DirectionIngress || r.Route != RouteCRD ||
		r.Exchange != "crd-order-select" || r.Operation != "order-select" || r.Sender != "provider" || r.Recipient != "payer" ||
		r.CorrelationID != "leg-corr-0077" || r.Trace != "caller-trace-0077" || r.Backend != nil {
		t.Fatalf("answered record = %+v", r)
	}
	if !sha256Hex.MatchString(r.RequestCiphertextHash) || !sha256Hex.MatchString(r.ResponseCiphertextHash) {
		t.Fatalf("ciphertext hashes %q / %q, want sha256 hex (the Hub's payloadBundleHash join)", r.RequestCiphertextHash, r.ResponseCiphertextHash)
	}
	if r.ContractLine == "" {
		t.Fatalf("the routed leg's contract line is unset: %+v", r)
	}

	oo := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}]}`
	env.payerReturns(LegResult{Status: 502, Response: testResponse([]byte(oo))})
	rec = httptest.NewRecorder()
	h(rec, env.crdIngressRequest(t))
	r = got.only(t)
	if rec.Code != 502 || r.Outcome != ExchangeUpstreamError || r.Status != 502 || r.Trace != "" {
		t.Fatalf("relayed non-2xx: status=%d record=%+v, want upstream-error 502", rec.Code, r)
	}
}

// The earliest refusal a caller can get — no bearer — is recorded, with the
// id it was answered under.
func TestExchangeRecord_IngressAuthenticationRefusal(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{CorrelationGen: func() string { return "minted-401" }, ExchangeObserved: got.observe, HolderID: "provider"}}
	for route := range ingressRoutes {
		t.Run(route, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
			r.SetPathValue("id", "shn-order-sign")
			w := httptest.NewRecorder()
			g.ingressRoute(route)(w, r)
			rec := got.only(t)
			wantRefusal(t, rec, http.StatusUnauthorized, RefusedByProviderGateway, RefusalAuthentication)
			if rec.CorrelationID != "minted-401" || rec.Route != route {
				t.Fatalf("record = %+v", rec)
			}
		})
	}
}

// A payer identifier the network does not register is a routing refusal.
func TestExchangeRecord_IngressRoutingRefusal(t *testing.T) {
	env := newInProcessExchange(t)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("99999", "")))
	env.originator.ingressRoute(RoutePAS)(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
}

// hubAnswers answers /route itself — the Hub's own refusal, or no answer —
// and passes every other call to the stub.
type hubAnswers struct {
	next      http.RoundTripper
	status    int
	reason    string
	delivered string
	fail      bool
}

func (h hubAnswers) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/route") {
		return h.next.RoundTrip(req)
	}
	if h.fail {
		return nil, errors.New("dial tcp: connection refused")
	}
	hdr := http.Header{"Content-Type": []string{"application/json"}}
	if h.delivered != "" {
		hdr.Set(HubDeliveredHeader, h.delivered)
	}
	return &http.Response{StatusCode: h.status, Header: hdr, Body: io.NopCloser(strings.NewReader(`{"error":"` + h.reason + `"}`))}, nil
}

// The Hub's own refusals name the Hub and the network rule; a forward the
// recipient's gateway refused names that gateway; no answer is unreachable.
func TestExchangeRecord_IngressHubOutcomes(t *testing.T) {
	rows := []struct {
		name              string
		hub               hubAnswers
		outcome, by, rule string
	}{
		{"replay", hubAnswers{status: 409, reason: "replay detected", delivered: "no"}, ExchangeRefused, RefusedByHub, RefusalReplay},
		{"authority", hubAnswers{status: 403, reason: "authz token verification failed", delivered: "no"}, ExchangeRefused, RefusedByHub, RefusalAuthority},
		{"authentication", hubAnswers{status: 401, reason: "assertion verification failed", delivered: "no"}, ExchangeRefused, RefusedByHub, RefusalAuthentication},
		{"routing", hubAnswers{status: 502, reason: "unknown recipient", delivered: "no"}, ExchangeRefused, RefusedByHub, RefusalRouting},
		{"unknown leg", hubAnswers{status: 400, reason: "unknown transaction type", delivered: "no"}, ExchangeRefused, RefusedByHub, RefusalRouting},
		{"answer's leg", hubAnswers{status: 502, reason: "unknown transaction type", delivered: "yes"}, ExchangeRefused, RefusedByHub, RefusalIntegrity},
		{"integrity", hubAnswers{status: 502, reason: "payload hash mismatch", delivered: "no"}, ExchangeRefused, RefusedByHub, RefusalIntegrity},
		{"audit", hubAnswers{status: 502, reason: "audit append failed", delivered: "no"}, ExchangeRefused, RefusedByHub, RefusalAudit},
		{"stale timestamp", hubAnswers{status: 400, reason: "stale or future timestamp", delivered: "no"}, ExchangeRefused, RefusedByHub, RefusalReplay},
		{"involved refused", hubAnswers{status: 400, reason: "involved patients refused: another patient", delivered: "no"}, ExchangeRefused, RefusedByHub, RefusalAuthority},
		{"malformed request", hubAnswers{status: 400, reason: "missing authority frame", delivered: "no"}, ExchangeRefused, RefusedByHub, RefusalIntegrity},
		{"answer's token", hubAnswers{status: 502, reason: "response leg authorization failed", delivered: "yes"}, ExchangeRefused, RefusedByHub, RefusalAuthority},
		{"hub fault", hubAnswers{status: 500, reason: "encode envelope failed", delivered: "no"}, ExchangeOther, "", ""},
		{"recipient refused", hubAnswers{status: 502, reason: recipientRefusedPrefix + "(403)", delivered: "no"}, ExchangeRefused, RefusedByPayerGateway, ExchangeOther},
		{"recipient 5xx", hubAnswers{status: 502, reason: "forward to recipient failed: the recipient answered 500", delivered: "unknown"}, ExchangeUpstreamError, "", ""},
		{"answer lost", hubAnswers{status: 502, reason: "forward to recipient failed: the recipient's answer could not be read", delivered: "yes"}, ExchangeOther, "", ""},
		{"recipient not reached", hubAnswers{status: 502, reason: "forward to recipient failed: the recipient could not be reached", delivered: "no"}, ExchangeUnreachable, "", ""},
		// Not the Hub's route handler: a load balancer in front of it.
		{"load balancer 502", hubAnswers{status: 502, reason: "Bad Gateway"}, ExchangeOther, "", ""},
		{"load balancer 403", hubAnswers{status: 403, reason: "Forbidden"}, ExchangeUnreachable, "", ""},
		{"hub not reached", hubAnswers{fail: true}, ExchangeUnreachable, "", ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			env := newInProcessExchange(t)
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			row.hub.next = env.substrate
			env.originator.cfg.Client = &http.Client{Transport: row.hub}
			w := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCRD)(w, env.crdIngressRequest(t))
			rec := got.only(t)
			if rec.Outcome != row.outcome || rec.RefusedBy != row.by || rec.Rule != row.rule || rec.Status != w.Code {
				t.Fatalf("record %s %s/%s status %d (answer %d), want %s %s/%s", rec.Outcome, rec.RefusedBy, rec.Rule, rec.Status, w.Code, row.outcome, row.by, row.rule)
			}
			if !sha256Hex.MatchString(rec.RequestCiphertextHash) || rec.ResponseCiphertextHash != "" {
				t.Fatalf("hashes %q / %q: the sealed request's hash is known, no answer was", rec.RequestCiphertextHash, rec.ResponseCiphertextHash)
			}
		})
	}
}

// A handler that panics is recorded and the panic continues to the server; a
// consumer that panics never reaches the answer.
func TestExchangeRecord_Panics(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{ExchangeObserved: got.observe}}
	h := g.recordExchange(RoutePAS, DirectionIngress, func(http.ResponseWriter, *http.Request) { panic("boom") })
	func() {
		defer func() {
			if v := recover(); v != "boom" {
				t.Fatalf("recovered %v, want the handler's own panic", v)
			}
		}()
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	}()
	if rec := got.only(t); rec.Outcome != ExchangeOther || rec.Status != 0 {
		t.Fatalf("panicked call recorded as %+v", rec)
	}

	g.cfg.ExchangeObserved = func(ExchangeRecord) { panic("consumer") }
	w := httptest.NewRecorder()
	g.recordExchange(RoutePAS, DirectionIngress, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})(w, httptest.NewRequest(http.MethodPost, "/", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d", w.Code)
	}
}

// Unset, the seam is not there: the handler gets the writer it was given.
func TestExchangeRecord_UnsetIsAbsent(t *testing.T) {
	g := &Gateway{}
	w := httptest.NewRecorder()
	g.recordExchange(RoutePAS, DirectionIngress, func(got http.ResponseWriter, r *http.Request) {
		if got != http.ResponseWriter(w) || exchangeOf(r.Context()) != nil {
			t.Fatal("the unset seam wrapped the call")
		}
	})(w, httptest.NewRequest(http.MethodPost, "/", nil))
}

// A payload the ownership table refuses is this gateway's fidelity refusal.
func TestExchangeRecord_OwnershipFaultIsFidelity(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{ExchangeObserved: got.observe}}
	g.recordExchange(RouteDTR, DirectionIngress, func(w http.ResponseWriter, _ *http.Request) {
		w, _ = g.withScope(w, relay.RoleRequester)
		writeOwnershipFault(w)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	wantRefusal(t, got.only(t), http.StatusInternalServerError, RefusedByProviderGateway, RefusalFidelity)
}

// ---- the payer side ----

func newRecordingLevelPayer(t *testing.T, level ConformanceEnforcement) (*levelPayer, *exchangeRecords) {
	t.Helper()
	p := newLevelPayer(t, level)
	var got exchangeRecords
	p.g.cfg.ExchangeObserved = got.observe
	return p, &got
}

// The payer's own system answering: the record carries its status, the
// verified leg's identity and the answer's ciphertext hash.
func TestExchangeRecord_InboundAnswered(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	rec := got.only(t)
	if rec.Outcome != ExchangeAnswered || rec.Status != http.StatusOK || rec.Direction != DirectionInbound || rec.Route != RouteSubstrateInbound ||
		rec.Exchange != "crd-order-select" || rec.CorrelationID != ans.corr || rec.Sender != p.requester.ID || rec.Recipient != p.g.cfg.HolderID {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Backend == nil || rec.Backend.Status != http.StatusOK || rec.Backend.ErrorClass != "" || rec.BackendCalls == 0 {
		t.Fatalf("backend = %+v (%d calls)", rec.Backend, rec.BackendCalls)
	}
	if !sha256Hex.MatchString(rec.RequestCiphertextHash) || !sha256Hex.MatchString(rec.ResponseCiphertextHash) {
		t.Fatalf("hashes %q / %q", rec.RequestCiphertextHash, rec.ResponseCiphertextHash)
	}
}

// The payer's own system answering non-2xx, or not at all, is an upstream
// error with the backend's class: the answer is relayed, or this gateway
// says its system could not be reached.
func TestExchangeRecord_InboundBackendFailures(t *testing.T) {
	t.Run("http-5xx", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		p.partner.status = http.StatusServiceUnavailable
		ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		rec := got.only(t)
		if rec.Outcome != ExchangeUpstreamError || rec.Status != ans.status || rec.Backend == nil ||
			rec.Backend.Status != http.StatusServiceUnavailable || rec.Backend.ErrorClass != BackendHTTP5xx {
			t.Fatalf("answer %d; record = %+v backend %+v", ans.status, rec, rec.Backend)
		}
	})
	t.Run("connect", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		// The listing is read first; closing the system after it leaves the
		// operation call unanswered.
		p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		got.only(t)
		p.partner.srv.Close()
		ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		rec := got.only(t)
		if rec.Outcome != ExchangeUpstreamError || ans.status != http.StatusBadGateway || rec.Status != http.StatusBadGateway ||
			rec.Backend == nil || rec.Backend.Status != 0 || rec.Backend.ErrorClass != BackendConnect {
			t.Fatalf("answer %d; record = %+v backend %+v", ans.status, rec, rec.Backend)
		}
	})
}

// Refusals before the leg is verified record no party the envelope claims.
func TestExchangeRecord_InboundEarlyRefusals(t *testing.T) {
	t.Run("hub assertion", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		w := httptest.NewRecorder()
		p.g.inboundRoute()(w, httptest.NewRequest(http.MethodPost, "/substrate/inbound", strings.NewReader("{}")))
		rec := got.only(t)
		wantRefusal(t, rec, http.StatusForbidden, RefusedByPayerGateway, RefusalAuthentication)
		if rec.Sender != "" || rec.CorrelationID != "" || rec.RequestCiphertextHash != "" || rec.Exchange != ExchangeOther {
			t.Fatalf("an unverified call recorded envelope claims: %+v", rec)
		}
		// Its own side is known: the leg's recipient is this gateway.
		if rec.Recipient != p.g.cfg.HolderID {
			t.Fatalf("recipient %q, want this gateway's holder %q", rec.Recipient, p.g.cfg.HolderID)
		}
	})
	t.Run("authz token", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		other, _ := genED25519(t)
		p.g.cfg.AuthzPub = other
		ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		rec := got.only(t)
		wantRefusal(t, rec, ans.status, RefusedByPayerGateway, RefusalAuthority)
		if rec.Sender != "" || rec.Exchange != "crd-order-select" {
			t.Fatalf("record = %+v", rec)
		}
	})
}

// A check the participant opted into refuses at strict as conformance, with
// its finding counted. At observe the same request is answered and the record
// carries no count at all: the call's findings are captured as their own
// events, some of them off the request path, so a count here would be partial.
func TestExchangeRecord_InboundConformance(t *testing.T) {
	const sr = `{"fullUrl":"urn:uuid:sr1","resource":{"resourceType":"ServiceRequest","id":"sr1","status":"draft","intent":"order","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]}}}`
	body := crdSelectWith(t, sr, ``)
	for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			ans := p.send(t, "crd-order-select", "", body)
			rec := got.only(t)
			if level == EnforcementObserve {
				if !rec.Findings.Deferred || rec.Findings.Count != 0 || rec.Findings.Refused || len(rec.Findings.Kinds) != 0 {
					t.Fatalf("at observe the record defers its findings and counts none, got %+v", rec.Findings)
				}
			} else if rec.Findings.Deferred || rec.Findings.Count == 0 || !slices.Contains(rec.Findings.Kinds, string(KindContent)) {
				t.Fatalf("findings = %+v", rec.Findings)
			}
			if level == EnforcementStrict {
				wantRefusal(t, rec, ans.status, RefusedByPayerGateway, RefusalConformance)
				if !rec.Findings.Refused || rec.Backend != nil {
					t.Fatalf("record = %+v", rec)
				}
				return
			}
			if rec.Outcome != ExchangeAnswered || rec.Findings.Refused {
				t.Fatalf("record = %+v", rec)
			}
		})
	}
}

// ---- classification tables ----

func TestBackendClass(t *testing.T) {
	timeoutErr := &net.OpError{Op: "read", Err: timeoutError{}}
	rows := []struct {
		status int
		err    error
		want   string
	}{
		{200, nil, ""},
		{204, nil, ""},
		{302, nil, BackendHTTP3xx},
		{404, nil, BackendHTTP4xx},
		{401, nil, BackendAuth},
		{403, nil, BackendAuth},
		{503, nil, BackendHTTP5xx},
		{0, &net.OpError{Op: "dial", Err: errors.New("connection refused")}, BackendConnect},
		{0, timeoutErr, BackendTimeout},
		{0, context.DeadlineExceeded, BackendTimeout},
		{0, context.Canceled, BackendCancelled},
		{0, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, BackendTLS},
		{0, tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, BackendTLS},
		{0, tokenAcquisitionFailure(t), BackendAuth},
		{0, errors.New("something else"), ExchangeOther},
	}
	for _, row := range rows {
		if got := backendClass(row.status, row.err); got != row.want {
			t.Errorf("backendClass(%d, %v) = %q, want %q", row.status, row.err, got, row.want)
		}
	}
}

// tokenAcquisitionFailure is the error a bearer client returns when its token
// endpoint refuses: the payer's system was never called.
func tokenAcquisitionFailure(t *testing.T) error {
	t.Helper()
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(token.Close)
	hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: token.URL, ClientID: "client", ClientSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = hc.Get("http://payer-system.invalid/")
	if !smartauth.IsTokenAcquisitionError(err) {
		t.Fatalf("want a token acquisition error, got %v", err)
	}
	return err
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestRefusalRuleFor(t *testing.T) {
	for _, row := range []struct {
		kind CheckKind
		rule string
		want string
	}{
		{KindFHIRBridged, "", RefusalFidelity},
		{KindNetwork, RuleSubjectPCI, RefusalAuthority},
		{KindContent, RuleSubjectToken, RefusalAuthority},
		{KindNetwork, RuleDuplicateKey, RefusalIntegrity},
		{KindContent, RulePatientMixed, RefusalConformance},
		{KindFHIRIngress, "", RefusalConformance},
		{KindCDSEnvelope, "response.json", RefusalConformance},
	} {
		if got := refusalRuleFor(row.kind, row.rule); got != row.want {
			t.Errorf("refusalRuleFor(%s, %q) = %q, want %q", row.kind, row.rule, got, row.want)
		}
	}
}

// An answer from the payer's system this gateway cannot read is that call's
// failure; a refusal of anything else leaves the call's class alone.
func TestExchangeRecorder_MalformedAnswer(t *testing.T) {
	x := &exchangeRecorder{}
	x.backend(200, 0, "")
	x.checked(KindContent, RulePatientMixed, true, true)
	if x.rec.Backend.ErrorClass != "" {
		t.Fatalf("a request refusal marked the backend call %q", x.rec.Backend.ErrorClass)
	}
	x.checked(KindContent, RuleAnswerShape, true, true)
	if x.rec.Backend.ErrorClass != BackendMalformed {
		t.Fatalf("an unreadable answer left the backend call %q", x.rec.Backend.ErrorClass)
	}
	if !bytes.Equal([]byte(x.guardRule), []byte(RefusalConformance)) {
		t.Fatalf("guard rule %q", x.guardRule)
	}
}

// A payer's answer the provider's gateway refuses at strict is that gateway's
// conformance refusal, after the leg was answered; below strict the same
// answer is relayed and its finding counted.
func TestExchangeRecord_IngressRefusesTheAnswer(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			env.payerReturns(LegResult{Response: testResponse([]byte(`{"cards":[{"summary":7}]}`))})
			w := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCRD)(w, env.crdIngressRequest(t))
			rec := got.only(t)
			// At observe the record defers its findings (they are captured as
			// their own events); at strict the refusing finding is counted.
			if level == EnforcementObserve && !rec.Findings.Deferred || level == EnforcementStrict && rec.Findings.Count == 0 {
				t.Fatalf("answer %d %s; findings %+v at %s", w.Code, w.Body.String(), rec.Findings, level)
			}
			if level == EnforcementStrict {
				wantRefusal(t, rec, w.Code, RefusedByProviderGateway, RefusalConformance)
				if !sha256Hex.MatchString(rec.ResponseCiphertextHash) {
					t.Fatalf("the refused answer's hash is unset: %+v", rec)
				}
				return
			}
			if w.Code != http.StatusOK || rec.Outcome != ExchangeAnswered {
				t.Fatalf("answer %d; record %+v", w.Code, rec)
			}
		})
	}
}

// ---- inbound refusal rules, the Authorization Framework and lost answers ----

// inboundRequest is levelPayer.sendFramed's sealed request, with mutate
// applied to the envelope before it is encoded; body replaces the encoded
// envelope when non-nil.
func inboundRequest(t *testing.T, p *levelPayer, leg string, mutate func(*levelPayer, *shnsdk.Envelope), body []byte) *http.Request {
	t.Helper()
	return inboundRequestWith(t, p, leg, conformantCRD("MBR-COVERED", "72148"), mutate, body)
}

// inboundRequestWith is inboundRequest sealing inboundPayload.
func inboundRequestWith(t *testing.T, p *levelPayer, leg string, inboundPayload []byte, mutate func(*levelPayer, *shnsdk.Envelope), body []byte) *http.Request {
	t.Helper()
	spec := paCatalog[leg]
	clock := p.g.cfg.Clock()
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: p.requester.ID, Recipient: p.g.cfg.HolderID, TransactionType: leg,
		AuthorityFrame: spec.ReqFrame, Timestamp: clock.Format(time.RFC3339), CorrelationID: "corr-review",
	}, inboundPayload, p.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatal(err)
	}
	tok := signTestToken(shnsdk.Token{
		Operation: spec.Op, Subject: p.pci, Frame: spec.ReqFrame, Holder: p.requester.ID,
		CorrelationID: "corr-review", Expiry: clock.Add(time.Hour), PayloadHash: sha256hexT(env.Ciphertext),
	}, p.authzPriv)
	tb, _ := json.Marshal(tok)
	env.Metadata.AuthzToken = string(tb)
	if mutate != nil {
		mutate(p, &env)
	}
	raw, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		raw = body
	}
	as, _ := json.Marshal(shnsdk.IssueAssertion("hub", p.g.cfg.HolderID, p.hubPriv, clock, time.Minute))
	r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", bytes.NewReader(raw))
	r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(as))
	return r
}

// A payer that requires known members (Config.RequireKnownMembers) refusing a
// member its system does not hold refuses on a check it opted into: the
// record names conformance, at every level.
func TestExchangeRecord_InboundUnknownMemberIsConformance(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.g.cfg.RequireKnownMembers = true
			ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-NOT-HELD", "72148"))
			if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), "unknown member") {
				t.Fatalf("answer %d %s", ans.status, ans.body)
			}
			wantRefusal(t, got.only(t), ans.status, RefusedByPayerGateway, RefusalConformance)
		})
	}
}

// Every refusal before an inbound leg is opened names its network rule.
func TestExchangeRecord_InboundRefusalRules(t *testing.T) {
	rows := []struct {
		name   string
		leg    string
		mutate func(*levelPayer, *shnsdk.Envelope)
		body   []byte
		status int
		rule   string
	}{
		{"undecodable envelope", "crd-order-select", nil, []byte("not an envelope"), http.StatusBadRequest, RefusalIntegrity},
		{"no authority frame", "crd-order-select", func(_ *levelPayer, e *shnsdk.Envelope) { e.Metadata.AuthorityFrame = "" }, nil, http.StatusBadRequest, RefusalIntegrity},
		{"no correlation", "crd-order-select", func(_ *levelPayer, e *shnsdk.Envelope) { e.Metadata.CorrelationID = "" }, nil, http.StatusBadRequest, RefusalIntegrity},
		{"another recipient", "crd-order-select", func(_ *levelPayer, e *shnsdk.Envelope) { e.Metadata.Recipient = "someone-else" }, nil, http.StatusForbidden, RefusalRouting},
		{"unknown transaction", "crd-order-select", func(_ *levelPayer, e *shnsdk.Envelope) { e.Metadata.TransactionType = "no-such-leg" }, nil, http.StatusBadRequest, RefusalRouting},
		{"undecryptable", "crd-order-select", func(p *levelPayer, e *shnsdk.Envelope) {
			e.Ciphertext = e.Ciphertext[:len(e.Ciphertext)-1]
			rehashToken(t, p, e)
		}, nil, http.StatusBadRequest, RefusalIntegrity},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, EnforcementObserve)
			w := httptest.NewRecorder()
			p.g.inboundRoute()(w, inboundRequest(t, p, row.leg, row.mutate, row.body))
			wantRefusal(t, got.only(t), row.status, RefusedByPayerGateway, row.rule)
			if w.Code != row.status {
				t.Fatalf("answer %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// rehashToken re-signs the envelope's token over its (changed) ciphertext, so
// the leg verifies and the change is found where the bytes are opened.
func rehashToken(t *testing.T, p *levelPayer, e *shnsdk.Envelope) {
	t.Helper()
	var tok shnsdk.Token
	if err := json.Unmarshal([]byte(e.Metadata.AuthzToken), &tok); err != nil {
		t.Fatal(err)
	}
	tok.PayloadHash = sha256hexT(e.Ciphertext)
	tb, _ := json.Marshal(signTestToken(tok, p.authzPriv))
	e.Metadata.AuthzToken = string(tb)
}

// authorizeAnswers answers /authorize itself — a denial — and passes every
// other call to the stub.
type authorizeAnswers struct{ next http.RoundTripper }

func (a authorizeAnswers) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/authorize") {
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"denied"}`))}, nil
	}
	return a.next.RoundTrip(req)
}

// The Authorization Framework denying the leg is its refusal, on authority;
// an answer this gateway cannot verify is its own integrity refusal.
func TestExchangeRecord_IngressAuthorizationAndLostAnswer(t *testing.T) {
	t.Run("authorization framework", func(t *testing.T) {
		env := newInProcessExchange(t)
		var got exchangeRecords
		env.originator.cfg.ExchangeObserved = got.observe
		env.originator.cfg.Client = &http.Client{Transport: authorizeAnswers{next: env.substrate}}
		w := httptest.NewRecorder()
		env.originator.ingressRoute(RouteCRD)(w, env.crdIngressRequest(t))
		wantRefusal(t, got.only(t), w.Code, RefusedByAuthorizationFramework, RefusalAuthority)
	})
	t.Run("answer lost", func(t *testing.T) {
		env := newInProcessExchange(t)
		var got exchangeRecords
		env.originator.cfg.ExchangeObserved = got.observe
		env.corruptResponseToken(t)
		w := httptest.NewRecorder()
		env.originator.ingressRoute(RouteCRD)(w, env.crdIngressRequest(t))
		rec := got.only(t)
		wantRefusal(t, rec, w.Code, RefusedByProviderGateway, RefusalIntegrity)
		if !sha256Hex.MatchString(rec.ResponseCiphertextHash) {
			t.Fatalf("the unverifiable answer's hash is unset: %+v", rec)
		}
	})
}

// A leg no shared contract line can carry is a routing refusal.
func TestExchangeRecord_RouteRefusal(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{ExchangeObserved: got.observe}}
	g.recordExchange(RouteDTR, DirectionIngress, func(w http.ResponseWriter, _ *http.Request) {
		g.relayOriginationError(w, &RouteRefusalError{Contract: "pa.dtr", LegType: "dtr-questionnaire-fetch", Recipient: "payer"})
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
}

// An id a message chose is recorded as it is only when it is one bounded
// token; otherwise as its digest, which still joins the leg's records.
func TestBoundedID(t *testing.T) {
	long := strings.Repeat("a", 65)
	for id, want := range map[string]string{
		"":                 "",
		"claim-corr-77":    "claim-corr-77",
		"Jane Doe MRN 123": "sha256:" + sha256hexT([]byte("Jane Doe MRN 123")),
		long:               "sha256:" + sha256hexT([]byte(long)),
	} {
		if got := boundedID(id); got != want {
			t.Errorf("boundedID(%q) = %q, want %q", id, got, want)
		}
	}
}

// A PAS submit whose Claim names its own correlation is sent under it; the
// record keeps the caller's own trace value beside it, and a Claim
// correlation that is not one bounded token is recorded as its digest.
func TestExchangeRecord_PASClaimCorrelation(t *testing.T) {
	for claimCorr, want := range map[string]string{
		"claim-corr-88":            "claim-corr-88",
		"claim corr with spaces 1": "sha256:" + sha256hexT([]byte("claim corr with spaces 1")),
	} {
		t.Run(claimCorr, func(t *testing.T) {
			env := newInProcessExchange(t)
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			claim := `"identifier":[{"system":"urn:shn:correlation","value":"` + claimCorr + `"}],`
			r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", claim)))
			r.Header.Set(CorrelationHeader, "caller-trace-9")
			env.originator.ingressRoute(RoutePAS)(httptest.NewRecorder(), r)
			rec := got.only(t)
			if rec.CorrelationID != want || rec.Trace != "caller-trace-9" || rec.Exchange != "pas-claim" {
				t.Fatalf("record id %q trace %q exchange %q, want %q / caller-trace-9 / pas-claim", rec.CorrelationID, rec.Trace, rec.Exchange, want)
			}
		})
	}
}

// A requester without a message frame cannot be sent the payer's error
// answer; the gateway's own refusal stands in, and the record still says the
// payer's system failed.
func TestExchangeRecord_InboundLegacyRequester(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	entry, _ := p.g.cfg.Reg.Lookup(p.requester.ID)
	entry.MessageFrames = nil
	p.g.cfg.Reg.Set(p.requester.ID, entry)
	p.partner.status = http.StatusServiceUnavailable
	ans := p.sendFramed(t, "crd-order-select", nil, conformantCRD("MBR-COVERED", "72148"), p.pci)
	rec := got.only(t)
	if ans.framed || rec.Outcome != ExchangeUpstreamError || rec.Status != ans.status ||
		rec.Backend == nil || rec.Backend.ErrorClass != BackendHTTP5xx {
		t.Fatalf("answer %d framed=%v; record %+v backend %+v", ans.status, ans.framed, rec, rec.Backend)
	}
}

// A payer's service listing that cannot be read is the payer's system
// failing, whichever way it fails, never the payer gateway's refusal.
func TestExchangeRecord_InboundListingUnavailable(t *testing.T) {
	t.Run("connect", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		p.partner.srv.Close()
		ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		rec := got.only(t)
		if rec.Outcome != ExchangeUpstreamError || rec.Status != ans.status || rec.Backend == nil || rec.Backend.ErrorClass != BackendConnect {
			t.Fatalf("answer %d; record %+v backend %+v", ans.status, rec, rec.Backend)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"services":[{"hook":"order-select"}]}`))
		}))
		t.Cleanup(listing.Close)
		n := NewNativeResponder(listing.Client(), listing.URL, "shn-order-select", p.store, fixedClock)
		n.bindFindingEmitter(p.g.emitFinding)
		p.g.cfg.Responder = n
		ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		rec := got.only(t)
		if rec.Outcome != ExchangeUpstreamError || rec.Status != ans.status || rec.Backend == nil ||
			rec.Backend.Status != http.StatusOK || rec.Backend.ErrorClass != BackendMalformed {
			t.Fatalf("answer %d; record %+v backend %+v", ans.status, rec, rec.Backend)
		}
	})
}

// A FHIR validation that refuses counts its finding under its kind and names
// conformance as the refusal's rule.
func TestExchangeRecord_ValidationFinding(t *testing.T) {
	const marker = "REJECTED-MARKER"
	g, _, _ := findingGateway(t, &shnsdk.FakeValidator{RejectIfContains: marker})
	x := &exchangeRecorder{self: RefusedByProviderGateway}
	ctx := context.WithValue(withFindingContext(context.Background(), findingContext{LegType: "crd-order-select", Seam: "provider-ingress", Whose: "peer"}), exchangeRecorderKey{}, x)
	if status, _ := g.validateFHIR(ctx, []byte(`{"resourceType":"Coverage","id":"`+marker+`"}`), "ingress", ""); status == 0 {
		t.Fatal("the invalid resource was not refused")
	}
	if x.rec.Findings.Count != 1 || !x.rec.Findings.Refused || !slices.Equal(x.rec.Findings.Kinds, []string{string(KindFHIRIngress)}) || x.guardRule != RefusalConformance {
		t.Fatalf("findings %+v rule %q", x.rec.Findings, x.guardRule)
	}
}

// Recording never changes an answer: the same calls with the hook set and
// unset get the same status, headers and body.
func TestExchangeRecord_ConformanceNeutral(t *testing.T) {
	type answer struct {
		status int
		header http.Header
		body   string
	}
	ingress := func(observe bool, relay LegResult, auth bool) answer {
		env := newInProcessExchange(t)
		env.originator.cfg.CorrelationGen = func() string { return "leg-neutral" }
		if observe {
			env.originator.cfg.ExchangeObserved = func(ExchangeRecord) {}
		}
		env.payerReturns(relay)
		req := env.crdIngressRequest(t)
		if auth {
			env.originator.cfg.ingressAuthBypass = false
		}
		w := httptest.NewRecorder()
		env.originator.ingressRoute(RouteCRD)(w, req)
		return answer{w.Code, w.Header(), w.Body.String()}
	}
	inbound := func(observe bool, backend int) payerAnswer {
		p := newLevelPayer(t, EnforcementObserve)
		if observe {
			p.g.cfg.ExchangeObserved = func(ExchangeRecord) {}
		}
		p.partner.status = backend
		a := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		a.corr = ""
		return a
	}
	oo := testResponse([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}]}`))
	for name, row := range map[string]struct {
		relay LegResult
		auth  bool
	}{"answered": {LegResult{}, false}, "relayed error": {LegResult{Status: 502, Response: oo}, false}, "no bearer": {LegResult{}, true}} {
		off, on := ingress(false, row.relay, row.auth), ingress(true, row.relay, row.auth)
		if off.status != on.status || off.body != on.body || fmt.Sprint(off.header) != fmt.Sprint(on.header) {
			t.Errorf("ingress %s: hook changed the answer\n off %+v\n  on %+v", name, off, on)
		}
	}
	for _, backend := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		off, on := inbound(false, backend), inbound(true, backend)
		if off.status != on.status || off.framed != on.framed || !bytes.Equal(off.body, on.body) {
			t.Errorf("inbound backend %d: hook changed the answer\n off %d %s\n  on %d %s", backend, off.status, off.body, on.status, on.body)
		}
	}
}

// ---- refusal rules, the operation, store outages and the door call id ----

// failingSoR is a system of record whose patient reads fail with err.
type failingSoR struct {
	SystemOfRecord
	ContextSystemOfRecord
	err error
}

func (f failingSoR) ResolvePatientContext(context.Context, string) (string, Demo, bool, error) {
	return "", Demo{}, false, f.err
}

// A payer whose own system of record cannot answer is its system failing, an
// upstream error with the read's class, never the payer gateway's refusal.
func TestExchangeRecord_InboundSystemOfRecordFailure(t *testing.T) {
	for kind, class := range map[SoRFailureKind]string{SoRUnavailable: ExchangeOther, SoRAuthenticationFailed: BackendAuth, SoRInvalidResponse: BackendMalformed} {
		t.Run(string(kind), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, EnforcementObserve)
			req := eligibilityInboundRequest(t, p.g, "sor")
			p.g.cfg.SoR = failingSoR{SystemOfRecord: p.g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(p.g.cfg.SoR), err: &SoRReadError{Kind: kind}}
			w := httptest.NewRecorder()
			p.g.inboundRoute()(w, req)
			rec := got.only(t)
			if rec.Outcome != ExchangeUpstreamError || rec.Backend == nil || rec.Backend.ErrorClass != class || rec.Exchange != "coverage-eligibility" {
				t.Fatalf("record %+v backend %+v", rec, rec.Backend)
			}
		})
	}
}

// A token for another patient than the request names is an authority refusal.
func TestExchangeRecord_InboundSubjectBinding(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	cer := []byte(`{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],` +
		`"patient":{"reference":"Patient/MBR-UC04"},"created":"2026-01-01T00:00:00Z",` +
		`"insurer":{"reference":"Organization/payer"},"provider":{"reference":"Practitioner/1234567890"}}`)
	w := httptest.NewRecorder()
	p.g.inboundRoute()(w, buildEligibilityInboundRequest(t, p.g, cer, "corr-subject"))
	rec := got.only(t)
	if rec.Outcome != ExchangeRefused || rec.RefusedBy != RefusedByPayerGateway || rec.Rule != RefusalAuthority {
		t.Fatalf("record %s %s/%s status %d", rec.Outcome, rec.RefusedBy, rec.Rule, rec.Status)
	}
}

// A request frame naming a line the payer does not serve is routing; one it
// cannot read is message integrity.
func TestExchangeRecord_InboundFrameRefusals(t *testing.T) {
	t.Run("unserved line", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		ans := p.sendFramed(t, "crd-order-select", map[string]string{shnsdk.FrameHeaderContractVersion: "pa.crd@9.9.9"}, conformantCRD("MBR-COVERED", "72148"), p.pci)
		wantRefusal(t, got.only(t), ans.status, RefusedByPayerGateway, RefusalRouting)
	})
	t.Run("unreadable frame", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		w := httptest.NewRecorder()
		p.g.inboundRoute()(w, inboundRequestWith(t, p, "crd-order-select", []byte{0x00, 0x01, 0xff, 0xff}, nil, nil))
		rec := got.only(t)
		if rec.Outcome != ExchangeRefused || rec.Rule != RefusalIntegrity {
			t.Fatalf("record %s %s/%s status %d", rec.Outcome, rec.RefusedBy, rec.Rule, rec.Status)
		}
	})
}

// The DTR operation is recorded as one of the operations a frame can name;
// anything else a peer writes there is never copied into the record.
func TestExchangeRecord_InboundOperationAndLine(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	p.send(t, "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, dtrFetchReq)
	rec := got.only(t)
	if rec.Operation != shnsdk.FrameOperationQuestionnairePackage || rec.ContractLine == "" {
		t.Fatalf("operation %q line %q", rec.Operation, rec.ContractLine)
	}
	p.send(t, "dtr-questionnaire-fetch", "Jane Doe DOB 1970-01-01\nforged", dtrFetchReq)
	if rec := got.only(t); rec.Operation != "" && rec.Operation != ExchangeOther {
		t.Fatalf("the peer's operation text reached the record: %q", rec.Operation)
	}
}

// An answer that repeats a member name refuses on message integrity, on the
// requester's side and on the payer's (where it is the payer system's answer).
func TestExchangeRecord_RepeatedMemberName(t *testing.T) {
	dup := []byte(`{"resourceType":"Bundle","resourceType":"Bundle","type":"collection","entry":[]}`)
	t.Run("provider", func(t *testing.T) {
		env := newInProcessExchange(t)
		var got exchangeRecords
		env.originator.cfg.ExchangeObserved = got.observe
		env.payerReturns(LegResult{Response: testResponse(dup)})
		w := httptest.NewRecorder()
		env.originator.ingressRoute(RoutePAS)(w, httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", ""))))
		wantRefusal(t, got.only(t), w.Code, RefusedByProviderGateway, RefusalIntegrity)
	})
	t.Run("payer", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		p.partner.respByPath[pasSubmitPath] = dup
		ans := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
		rec := got.only(t)
		wantRefusal(t, rec, ans.status, RefusedByPayerGateway, RefusalIntegrity)
		if rec.Backend == nil || rec.Backend.ErrorClass != BackendMalformed {
			t.Fatalf("backend %+v", rec.Backend)
		}
	})
}

// A payload the ownership table refuses on an inbound leg is this gateway's
// fidelity refusal.
func TestExchangeRecord_InboundOwnershipFault(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	req := inboundRequest(t, p, "crd-order-select", nil, nil)
	raw, _ := io.ReadAll(req.Body)
	env, err := shnsdk.DecodeEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	var tok shnsdk.Token
	if err := json.Unmarshal([]byte(env.Metadata.AuthzToken), &tok); err != nil {
		t.Fatal(err)
	}
	p.g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
		w, _ = p.g.withScope(w, relay.RoleRecipient)
		p.g.refuseInbound(w, r, inboundLegs["crd-order-select"], env, tok, "", http.StatusInternalServerError, errOwnershipFault, nil)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil))
	rec := got.only(t)
	if rec.Outcome != ExchangeRefused || rec.Rule != RefusalFidelity {
		t.Fatalf("record %s %s/%s", rec.Outcome, rec.RefusedBy, rec.Rule)
	}
}

// replayDown is a one-time-use record that cannot be consulted.
type replayDown struct{}

func (replayDown) CheckAndRecord(string, string, string, time.Time, time.Time) (bool, error) {
	return false, errors.New("store down")
}

// A store outage is neither a refusal nor the participant's failure.
func TestExchangeRecord_StoreUnavailable(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	p.g.replay = replayDown{}
	w := httptest.NewRecorder()
	p.g.inboundRoute()(w, inboundRequest(t, p, "crd-order-select", nil, nil))
	if rec := got.only(t); w.Code != http.StatusServiceUnavailable || rec.Outcome != ExchangeOther || rec.Status != http.StatusServiceUnavailable {
		t.Fatalf("answer %d record %+v", w.Code, rec)
	}
}

// An inquiry naming no registered payer is a routing refusal too.
func TestExchangeRecord_InquireRoutingRefusal(t *testing.T) {
	env := newInProcessExchange(t)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	w := httptest.NewRecorder()
	body := strings.Replace(levelInquiry(t, "MBR-COVERED", ""), `"value":"00001"`, `"value":"99999"`, 1)
	env.originator.ingressRoute(RoutePASInquire)(w, httptest.NewRequest(http.MethodPost, "/Claim/$inquire", strings.NewReader(body)))
	wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
}

// A call the pa-test door proved is recorded with the door's call id.
func TestExchangeRecord_DoorCallID(t *testing.T) {
	env := newInProcessExchange(t)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	key := []byte("synthetic-door-trace-key")
	env.originator.cfg.Diagnostic = func(diagnostics.Event) bool { return true }
	env.originator.cfg.DiagnosticTraceKey = key
	req := env.crdIngressRequest(t)
	req.Header.Set("X-SHN-Test-Trace", diagnostics.TraceProof(key, "call-0077", req.Method, req.URL.RequestURI(), env.originator.cfg.Clock()))
	env.originator.ingressRoute(RouteCRD)(httptest.NewRecorder(), req)
	if rec := got.only(t); rec.CallID != "call-0077" {
		t.Fatalf("call id %q", rec.CallID)
	}
}

// The ingress key store being down is an outage, not a refusal of the caller.
func TestExchangeRecord_IngressKeyStoreUnavailable(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()
	kid, err := newKID()
	if err != nil {
		t.Fatal(err)
	}
	bearer := mintAssertionKID(t, priv, kid, jwt.MapClaims{"client_id": "br-provider", "aud": testIngressBaseURL, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
	eph, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	g := gatewayWithAuthStores(t, "br-provider", pub, &unavailableKeyStore{IngressKeyStore: eph}, nil)
	var got exchangeRecords
	g.cfg.ExchangeObserved = got.observe
	req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+"/Claim/$submit", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, req)
	if rec := got.only(t); w.Code != http.StatusServiceUnavailable || rec.Outcome != ExchangeOther || rec.Status != http.StatusServiceUnavailable {
		t.Fatalf("answer %d record %+v", w.Code, rec)
	}
}

// ---- transport, consent, frames, ownership faults and the backend call ----

// A transport failure is never the Hub refusing on a network rule: a
// connection that failed after the request was sent, an answer from in front
// of the Hub, and a Hub answer that could not be read.
func TestHubRefusalOutcome_Transport(t *testing.T) {
	for name, row := range map[string]struct {
		e       *hubRefusalError
		outcome string
	}{
		"connection failed after sending": {&hubRefusalError{status: 502, reason: "the connection to the Hub failed after the request was sent", delivered: "unknown"}, ExchangeOther},
		"load balancer, not forwarded":    {&hubRefusalError{status: 403, reason: "Forbidden"}, ExchangeUnreachable},
		"load balancer 5xx":               {hubRefusal(502, []byte("<html>Bad Gateway</html>"), ""), ExchangeOther},
		"hub, marked":                     {hubRefusal(502, []byte(`{"error":"payload hash mismatch"}`), "no"), ExchangeRefused},
	} {
		if got, _, _ := hubRefusalOutcome(row.e); got != row.outcome {
			t.Errorf("%s: outcome %q, want %q", name, got, row.outcome)
		}
	}
	x := &exchangeRecorder{self: RefusedByProviderGateway}
	x.legOutcome(&answerLostError{cause: "the Hub's answer could not be read"})
	if x.outcome != ExchangeOther {
		t.Errorf("an unreadable Hub answer settled %q, want other", x.outcome)
	}
}

// A consent reference missing on a federated query is a consent refusal.
func TestExchangeRecord_InboundConsent(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	ans := p.sendAs(t, "federated-query", "", []byte(`{"resourceType":"Parameters"}`), p.pci)
	wantRefusal(t, got.only(t), ans.status, RefusedByPayerGateway, RefusalConsent)
}

// A frame that names an operation on a leg that takes none is a frame this
// gateway cannot read; a token that is not JSON is an authority refusal.
func TestExchangeRecord_InboundFrameOperationAndToken(t *testing.T) {
	t.Run("operation on a leg without one", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		ans := p.send(t, "crd-order-select", shnsdk.FrameOperationQuestionnairePackage, conformantCRD("MBR-COVERED", "72148"))
		wantRefusal(t, got.only(t), ans.status, RefusedByPayerGateway, RefusalIntegrity)
	})
	t.Run("token not JSON", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		w := httptest.NewRecorder()
		p.g.inboundRoute()(w, inboundRequest(t, p, "crd-order-select", func(_ *levelPayer, e *shnsdk.Envelope) { e.Metadata.AuthzToken = "not json" }, nil))
		wantRefusal(t, got.only(t), http.StatusForbidden, RefusedByPayerGateway, RefusalAuthority)
	})
}

// A payer's CDS Hooks answer that repeats a member name is its system's
// malformed answer.
func TestExchangeRecord_InboundCRDRepeatedMember(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	p.partner.respByPath[crdSelectPath] = []byte(`{"cards":[],"cards":[]}`)
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	rec := got.only(t)
	wantRefusal(t, rec, ans.status, RefusedByPayerGateway, RefusalIntegrity)
	if rec.Backend == nil || rec.Backend.ErrorClass != BackendMalformed {
		t.Fatalf("backend %+v", rec.Backend)
	}
}

// The ingress's own payer routing on the CRD and DTR routes, and the DTR leg
// named only once the caller is authenticated.
func TestExchangeRecord_IngressRoutingCRDAndDTR(t *testing.T) {
	t.Run("crd", func(t *testing.T) {
		env := newInProcessExchange(t)
		var got exchangeRecords
		env.originator.cfg.ExchangeObserved = got.observe
		body := bytes.Replace(conformantCRDRequest("MBR-COVERED"), []byte(`"value":"00001"`), []byte(`"value":"99999"`), 1)
		w := httptest.NewRecorder()
		env.originator.ingressRoute(RouteCRD)(w, crdIngressPost(body))
		wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
	})
	t.Run("dtr", func(t *testing.T) {
		env := newInProcessExchange(t)
		env.originator.cfg.SoR = newPrefetchSoR().sor()
		var got exchangeRecords
		env.originator.cfg.ExchangeObserved = got.observe
		fixture := dtrFixture(t, "dtr-package-params-2.0.json")
		body := bytes.Replace(fixture, []byte(`"value": "`+shnsdk.CMSPayerIdentity.Value+`"`), []byte(`"value": "99999"`), 1)
		if bytes.Equal(body, fixture) {
			t.Fatal("fixture: the payer identifier was not replaced")
		}
		req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/fhir+json")
		w := httptest.NewRecorder()
		env.originator.ingressRoute(RouteDTR)(w, req)
		wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
	})
	t.Run("dtr unauthenticated", func(t *testing.T) {
		var got exchangeRecords
		g := &Gateway{cfg: Config{ExchangeObserved: got.observe}}
		g.ingressRoute(RouteDTR)(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
		if rec := got.only(t); rec.Exchange != ExchangeOther || rec.Rule != RefusalAuthentication {
			t.Fatalf("record exchange %q rule %q", rec.Exchange, rec.Rule)
		}
	})
}

// An ownership fault building the answer, and one a responder reports, are
// this gateway's fidelity refusals.
func TestExchangeRecord_InboundOwnershipFaults(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	spec := paCatalog["crd-order-select"]
	req := inboundRequest(t, p, "crd-order-select", nil, nil)
	raw, _ := io.ReadAll(req.Body)
	env, _ := shnsdk.DecodeEnvelope(raw)
	var tok shnsdk.Token
	_ = json.Unmarshal([]byte(env.Metadata.AuthzToken), &tok)
	run := func(h http.HandlerFunc) ExchangeRecord {
		p.g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
			w, _ = p.g.withScope(w, relay.RoleRecipient)
			h(w, r)
		})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil))
		return got.only(t)
	}
	rec := run(func(w http.ResponseWriter, r *http.Request) {
		p.g.respondLeg(w, r, spec.RespFrame, spec.RespOp, "crd-order-select", env.Metadata.CorrelationID, LegResult{}.Response, tok.Subject, p.requester.ID, "", "")
	})
	if rec.Outcome != ExchangeRefused || rec.Rule != RefusalFidelity {
		t.Fatalf("answer build: %s %s/%s", rec.Outcome, rec.RefusedBy, rec.Rule)
	}
	_, fault := relay.Transmit(LegResult{}.Response, relay.Check(answerKey("crd-order-select", relay.OutcomeAnswered)))
	if !isOwnershipFault(fault) {
		t.Fatalf("fixture: %v is not an ownership fault", fault)
	}
	rec = run(func(w http.ResponseWriter, r *http.Request) {
		p.g.responderFailed(w, r, inboundLegs["crd-order-select"], env, tok, "", fault)
	})
	if rec.Outcome != ExchangeRefused || rec.Rule != RefusalFidelity {
		t.Fatalf("responder fault: %s %s/%s", rec.Outcome, rec.RefusedBy, rec.Rule)
	}
}

// The payer's system answering with a body that cannot be read, and a
// service listing that is not one, are that system's failures.
func TestExchangeRecord_BackendReadAndListing(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/cds-services" {
				_ = json.NewEncoder(w).Encode(map[string]any{"services": stubCDSServices})
				return
			}
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte(`{"cards":`))
		}))
		t.Cleanup(broken.Close)
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		n := NewNativeResponder(broken.Client(), broken.URL, "shn-order-select", p.store, fixedClock)
		n.bindFindingEmitter(p.g.emitFinding)
		p.g.cfg.Responder = n
		p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		if rec := got.only(t); rec.Outcome != ExchangeUpstreamError || rec.Backend == nil || rec.Backend.ErrorClass != BackendRead {
			t.Fatalf("record %+v backend %+v", rec, rec.Backend)
		}
	})
	for name, listing := range map[string]string{"not JSON": `nope`, "no services array": `{}`} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(listing)) }))
			t.Cleanup(srv.Close)
			p, got := newRecordingLevelPayer(t, EnforcementObserve)
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", p.store, fixedClock)
			n.bindFindingEmitter(p.g.emitFinding)
			p.g.cfg.Responder = n
			p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
			if rec := got.only(t); rec.Outcome != ExchangeUpstreamError || rec.Backend == nil || rec.Backend.ErrorClass != BackendMalformed {
				t.Fatalf("record %+v backend %+v", rec, rec.Backend)
			}
		})
	}
}

// The system-of-record read classes, as the answer names them, and the
// outcome a refusal they cause settles. The error alone never says the
// request ended (a connector can cancel its own read): that is read from the
// request's context (TestExchangeRecord_ReadEndedWithTheRequest).
func TestSoRFailureClass(t *testing.T) {
	for name, row := range map[string]struct {
		err     error
		class   string
		outcome string
	}{
		"deadline":         {context.DeadlineExceeded, BackendTimeout, ExchangeUpstreamError},
		"cancelled":        {context.Canceled, ExchangeOther, ExchangeUpstreamError},
		"authentication":   {&SoRReadError{Kind: SoRAuthenticationFailed}, BackendAuth, ExchangeUpstreamError},
		"unavailable":      {&SoRReadError{Kind: SoRUnavailable}, ExchangeOther, ExchangeUpstreamError},
		"invalid response": {&SoRReadError{Kind: SoRInvalidResponse}, BackendMalformed, ExchangeUpstreamError},
		"untyped":          {errors.New("read failed"), BackendMalformed, ExchangeUpstreamError},
	} {
		if got := sorClass(row.err); got != row.class {
			t.Errorf("%s: class %q, want %q", name, got, row.class)
		}
		x := &exchangeRecorder{}
		x.sorFailure()
		if x.outcome != row.outcome {
			t.Errorf("%s: outcome %q, want %q", name, x.outcome, row.outcome)
		}
	}
	if sorClass(nil) != "" {
		t.Error("a read that succeeded has a class")
	}
}

// The settling rules no single route exercises: a leg answered but an error
// written, a handler that writes nothing, and a caller trace equal to the leg
// id.
func TestExchangeRecord_SettlingRules(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{ExchangeObserved: got.observe}}
	g.recordExchange(RoutePAS, DirectionIngress, func(w http.ResponseWriter, r *http.Request) {
		exchangeOf(r.Context()).decided(ExchangeAnswered, "", "")
		w.WriteHeader(http.StatusBadGateway)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	wantRefusal(t, got.only(t), http.StatusBadGateway, RefusedByProviderGateway, ExchangeOther)

	g.recordExchange(RoutePAS, DirectionIngress, func(http.ResponseWriter, *http.Request) {})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	if rec := got.only(t); rec.Status != http.StatusOK || rec.Outcome != ExchangeAnswered {
		t.Fatalf("silent handler: %+v", rec)
	}

	g.recordExchange(RoutePAS, DirectionIngress, func(w http.ResponseWriter, r *http.Request) {
		exchangeOf(r.Context()).trace("same-1")
		stampIngressIDs(w, "same-1", "same-1")
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	if rec := got.only(t); rec.CorrelationID != "same-1" || rec.Trace != "" {
		t.Fatalf("trace equal to the leg id: id %q trace %q", rec.CorrelationID, rec.Trace)
	}
}

// ---- the Hub reason table, timeouts, the DTR leg and 1xx ----

// Every Hub reason the record names a rule for, with that rule. The strings
// are pinned against the Hub's own by test/invariants (TestHubRulesMatchTheHub).
func TestHubRules(t *testing.T) {
	for reason, rule := range hubRules {
		delivered := "no"
		if strings.HasPrefix(reason, "response ") || reason == "decode recipient response failed" {
			delivered = "yes"
		}
		outcome, by, got := hubRefusalOutcome(hubRefusal(502, []byte(`{"error":"`+reason+`"}`), delivered))
		if outcome != ExchangeRefused || by != RefusedByHub || got != rule {
			t.Errorf("%q: %s %s/%s, want refused hub/%s", reason, outcome, by, got, rule)
		}
	}
	want := map[string]string{
		"unknown sender": RefusalAuthentication, "response leg sender mismatch": RefusalIntegrity,
		"response leg authorization failed": RefusalAuthority, "replay detected": RefusalReplay,
		"unknown recipient": RefusalRouting, "response audit append failed": RefusalAudit,
	}
	for reason, rule := range want {
		if hubRules[reason] != rule {
			t.Errorf("hubRules[%q] = %q, want %q", reason, hubRules[reason], rule)
		}
	}
}

// A Hub leg with no answer in time is unreachable; a responder error that is
// neither the participant's system nor an ownership fault is other.
func TestExchangeRecorder_TimeoutAndResponderFault(t *testing.T) {
	x := &exchangeRecorder{self: RefusedByProviderGateway}
	x.legOutcome(&hubTimeoutError{})
	if x.outcome != ExchangeUnreachable {
		t.Fatalf("hub timeout settled %q", x.outcome)
	}
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	req := inboundRequest(t, p, "crd-order-select", nil, nil)
	raw, _ := io.ReadAll(req.Body)
	env, _ := shnsdk.DecodeEnvelope(raw)
	var tok shnsdk.Token
	_ = json.Unmarshal([]byte(env.Metadata.AuthzToken), &tok)
	p.g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
		w, _ = p.g.withScope(w, relay.RoleRecipient)
		p.g.responderFailed(w, r, inboundLegs["crd-order-select"], env, tok, "", errors.New("responder bug"))
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil))
	if rec := got.only(t); rec.Outcome != ExchangeOther {
		t.Fatalf("responder fault recorded %s %s/%s", rec.Outcome, rec.RefusedBy, rec.Rule)
	}
}

// A DTR call that reaches its payer names its leg.
func TestExchangeRecord_IngressDTRNamesItsLeg(t *testing.T) {
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = newPrefetchSoR().sor()
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	declareFramedDTR(t, env, true)
	env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
	req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(dtrFixture(t, "dtr-package-params-2.0.json")))
	req.Header.Set("Content-Type", "application/fhir+json")
	env.originator.ingressRoute(RouteDTR)(httptest.NewRecorder(), req)
	if rec := got.only(t); rec.Exchange != "dtr-questionnaire-fetch" || rec.Outcome != ExchangeAnswered {
		t.Fatalf("record exchange %q outcome %q", rec.Exchange, rec.Outcome)
	}
}

// A 1xx is not the answer.
func TestExchangeWriter_Ignores1xx(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{ExchangeObserved: got.observe}}
	g.recordExchange(RoutePAS, DirectionIngress, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusContinue)
		w.WriteHeader(http.StatusAccepted)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	if rec := got.only(t); rec.Status != http.StatusAccepted {
		t.Fatalf("status %d", rec.Status)
	}
}

// ---- the payer system's reads and the backend classes ----

// Every read of the payer's own system of record on a leg it answers is a
// backend call: a failed one on a CRD leg makes the exchange an upstream
// error with the read's class, and a successful one is counted.
func TestExchangeRecord_InboundSystemOfRecordReads(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		p.g.cfg.SoR = failingSoR{SystemOfRecord: p.g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(p.g.cfg.SoR), err: &SoRReadError{Kind: SoRInvalidResponse}}
		p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		rec := got.only(t)
		if rec.Outcome != ExchangeUpstreamError || rec.Backend == nil || rec.Backend.ErrorClass != BackendMalformed {
			t.Fatalf("record %+v backend %+v", rec, rec.Backend)
		}
	})
	t.Run("counted", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		// At least one read of the system of record precedes the operation
		// call, and each is counted.
		if ans := got.only(t); ans.BackendCalls < 2 {
			t.Fatalf("backend calls %d: the system-of-record read was not counted beside the operation call", ans.BackendCalls)
		}
	})
}

// A call cut short because the request ended is never the participant's
// failure: cut short before its answer's headers or while its body was read,
// it is cancelled, and the refusal it caused settles as other.
func TestExchangeRecord_CancelledBackendCall(t *testing.T) {
	for name, midBody := range map[string]bool{"before headers": false, "mid-body": true} {
		t.Run(name, func(t *testing.T) {
			reached, release := make(chan struct{}), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if midBody {
					w.Header().Set("Content-Length", "1000")
					_, _ = w.Write([]byte(`{"cards":`))
					w.(http.Flusher).Flush()
				}
				close(reached)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(func() { close(release); srv.Close() })
			var cancel context.CancelFunc
			client := srv.Client()
			if midBody {
				// The request ends once the answer's headers are in, while
				// its body is read.
				client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					resp, err := srv.Client().Transport.RoundTrip(r)
					cancel()
					return resp, err
				})}
			}
			n := NewNativeResponder(client, srv.URL, "shn-order-select", nil, fixedClock)
			var got exchangeRecords
			g := &Gateway{cfg: Config{ExchangeObserved: got.observe, Role: "payer"}}
			g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
				var ctx context.Context
				ctx, cancel = context.WithCancel(r.Context())
				if !midBody {
					go func() { <-reached; cancel() }()
				}
				_, _, err := n.post(ctx, srv.URL, "/cds-services/shn-order-select", relay.Exact(relay.NewBody([]byte(`{}`), relay.OriginPeerFrame), "application/json"), "crd-order-select", "crd")
				if err == nil {
					t.Error("a cancelled call succeeded")
				}
				exchangeOf(r.Context()).decided(ExchangeRefused, "", "")
				w.WriteHeader(http.StatusBadGateway)
			})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
			rec := got.only(t)
			if rec.Outcome != ExchangeOther || rec.Backend == nil || rec.Backend.ErrorClass != BackendCancelled {
				t.Fatalf("record %+v backend %+v", rec, rec.Backend)
			}
			if midBody && rec.Backend.Status != http.StatusOK {
				t.Fatalf("fixture: the call was cut short before its headers (status %d)", rec.Backend.Status)
			}
		})
	}
}

// A call to the payer's system that fails as cancelled while the request it
// serves is still live (a client abandoning its own attempt, or its body
// read) was not cut short by that request: its system did not answer, and
// the refusal it caused is upstream. So for the forwarded operation, its
// body, and the CDS service listing.
func TestExchangeRecord_CancelledWhileTheRequestIsLive(t *testing.T) {
	abandoned := fmt.Errorf("attempt abandoned: %w", context.Canceled)
	failing := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, abandoned })
	cutBody := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(iotest.ErrReader(abandoned)), Request: r}, nil
	})
	post := func(client *http.Client) func(context.Context) {
		return func(ctx context.Context) {
			n := NewNativeResponder(client, "http://payer.test", "shn-order-select", nil, fixedClock)
			if _, _, err := n.post(ctx, "http://payer.test", "/cds-services/shn-order-select", relay.Exact(relay.NewBody([]byte(`{}`), relay.OriginPeerFrame), "application/json"), "crd-order-select", "crd"); err == nil {
				t.Error("a failed call succeeded")
			}
		}
	}
	listing := func(ctx context.Context) {
		if _, err := DiscoverCDSServices(ctx, &http.Client{Transport: failing}, "http://payer.test", nil); err == nil {
			t.Error("a failed listing read succeeded")
		}
	}
	calls := map[string]struct {
		call   func(context.Context)
		status int // the call's status: the body row's answer had begun
	}{
		"the operation":     {post(&http.Client{Transport: failing}), 0},
		"its answer's body": {post(&http.Client{Transport: cutBody}), http.StatusOK},
		"the listing":       {listing, 0},
	}
	// While the request is live the call is its system not answering; once
	// the request's deadline has passed, it is a timeout.
	for _, deadline := range []bool{false, true} {
		want := ExchangeOther
		if deadline {
			want = BackendTimeout
		}
		for name, c := range calls {
			t.Run(fmt.Sprintf("%s/deadline=%v", name, deadline), func(t *testing.T) {
				var got exchangeRecords
				g := &Gateway{cfg: Config{ExchangeObserved: got.observe, Role: "payer"}}
				g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
					ctx := r.Context()
					if deadline {
						var cancel context.CancelFunc
						ctx, cancel = context.WithDeadline(ctx, time.Unix(0, 0))
						defer cancel()
					}
					c.call(ctx)
					exchangeOf(r.Context()).decided(ExchangeUpstreamError, "", "")
					w.WriteHeader(http.StatusBadGateway)
				})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
				rec := got.only(t)
				if rec.Outcome != ExchangeUpstreamError || rec.Backend == nil || rec.Backend.ErrorClass != want || rec.Backend.Status != c.status {
					t.Fatalf("record %+v backend %+v, want class %q status %d", rec, rec.Backend, want, c.status)
				}
			})
		}
	}
}

// ReadSystemOfRecord returns a store-less gateway's reader as it was given,
// as STABILITY.md says.
func TestReadSystemOfRecord_NoSystemOfRecordAsGiven(t *testing.T) {
	if got, ok := ReadSystemOfRecord(NoSystemOfRecord()).(noSystemOfRecord); !ok || got != (noSystemOfRecord{}) {
		t.Fatalf("got %T, want the store-less reader itself", got)
	}
}

// A readable answer a check found fault with is the payer's answer, not its
// system failing: recorded at observe and relayed, its call has no class.
func TestExchangeRecord_FindingOnReadableAnswerIsNotMalformed(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	var mu sync.Mutex
	findings := 0
	p.g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == ConformanceObservedEvent {
			mu.Lock()
			findings++
			mu.Unlock()
		}
	}
	p.partner.respByPath[crdSelectPath] = []byte(`{"cards":[{"summary":"` + strings.Repeat("s", 200) + `","indicator":"info"}]}`)
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	rec := got.only(t)
	p.g.drainObserveChecks()
	mu.Lock()
	n := findings
	mu.Unlock()
	if ans.status != http.StatusOK || n == 0 || !rec.Findings.Deferred {
		t.Fatalf("fixture: answer %d, %d finding(s), record findings %+v; want a relayed answer with findings, deferred at observe", ans.status, n, rec.Findings)
	}
	if rec.Outcome != ExchangeAnswered || rec.Backend == nil || rec.Backend.ErrorClass != "" {
		t.Fatalf("record %+v backend %+v", rec, rec.Backend)
	}
}

// patientRefFailingSoR fails only reads of the payer's own Patient id.
type patientRefFailingSoR struct {
	SystemOfRecord
	ContextSystemOfRecord
	err error
}

func (f patientRefFailingSoR) PatientFHIRRefContext(context.Context, string) (string, bool, error) {
	return "", false, f.err
}

// The eligibility answer's patient check reads the payer's own system after
// the operation answered: the read is counted, and the operation call stays
// the exchange's Backend, whether the read succeeded or not.
func TestExchangeRecord_ReadAfterTheForwardKeepsTheForward(t *testing.T) {
	for name, fail := range map[string]bool{"read": false, "read timed out": true} {
		t.Run(name, func(t *testing.T) {
			p := eligibilityPayer(t, EnforcementObserve, true)
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			if fail {
				p.g.cfg.SoR = patientRefFailingSoR{SystemOfRecord: p.g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(p.g.cfg.SoR), err: context.DeadlineExceeded}
			}
			spy := &sorSpy{inner: p.g.cfg.SoR}
			p.g.cfg.SoR = spy
			// The payer's answer names its own id for the patient, so the
			// check reads that id.
			p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/payer-own-id")
			ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if !slices.Contains(spy.methods, "PatientFHIRRef") {
				t.Fatalf("fixture: the answer's patient was not read (%v)", spy.methods)
			}
			reads := len(spy.methods)
			rec := got.only(t)
			if ans.status != http.StatusOK || rec.Backend == nil || rec.Backend.Status != http.StatusOK || rec.Backend.ErrorClass != "" {
				t.Fatalf("answer %d; record %+v backend %+v", ans.status, rec, rec.Backend)
			}
			if rec.BackendCalls != reads+1 {
				t.Fatalf("backend calls %d, want %d: %d read(s) and the operation", rec.BackendCalls, reads+1, reads)
			}
		})
	}
}

// sorSpy sees every read of the system of record, and whether it came
// through the exchange record's recording reader.
type sorSpy struct {
	inner   SystemOfRecord
	mu      sync.Mutex
	methods []string
	bypass  []string
}

func (s *sorSpy) saw(ctx context.Context, method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods = append(s.methods, method)
	if ctx == nil || ctx.Value(sorReadNotedKey{}) == nil {
		s.bypass = append(s.bypass, method)
	}
}

func (s *sorSpy) reader() ContextSystemOfRecord {
	return ReadSystemOfRecord(s.inner).(recordingSoR).inner
}

func (s *sorSpy) ResolvePatient(m string) (string, Demo, bool) {
	s.saw(nil, "ResolvePatient")
	return s.inner.ResolvePatient(m)
}
func (s *sorSpy) PatientFHIRRef(m string) (string, bool) {
	s.saw(nil, "PatientFHIRRef")
	return s.inner.PatientFHIRRef(m)
}
func (s *sorSpy) CoverageInforce(m string) (bool, string) {
	s.saw(nil, "CoverageInforce")
	return s.inner.CoverageInforce(m)
}
func (s *sorSpy) ClinicalContext(m string) (shnsdk.ClinicalContext, bool) {
	s.saw(nil, "ClinicalContext")
	return s.inner.ClinicalContext(m)
}
func (s *sorSpy) SupplementalReport(m string) ([]byte, bool) {
	s.saw(nil, "SupplementalReport")
	return s.inner.SupplementalReport(m)
}
func (s *sorSpy) FacilityRecords(m string) (map[string][]byte, bool) {
	s.saw(nil, "FacilityRecords")
	return s.inner.FacilityRecords(m)
}
func (s *sorSpy) OpenOrder(m string) ([]byte, bool) {
	s.saw(nil, "OpenOrder")
	return s.inner.OpenOrder(m)
}
func (s *sorSpy) OpenCoverage(m string) ([]byte, bool) {
	s.saw(nil, "OpenCoverage")
	return s.inner.OpenCoverage(m)
}
func (s *sorSpy) ResolveByReference(r string) ([]byte, bool) {
	s.saw(nil, "ResolveByReference")
	return s.inner.ResolveByReference(r)
}
func (s *sorSpy) ResolvePatientContext(ctx context.Context, m string) (string, Demo, bool, error) {
	s.saw(ctx, "ResolvePatient")
	return s.reader().ResolvePatientContext(ctx, m)
}
func (s *sorSpy) PatientFHIRRefContext(ctx context.Context, m string) (string, bool, error) {
	s.saw(ctx, "PatientFHIRRef")
	return s.reader().PatientFHIRRefContext(ctx, m)
}
func (s *sorSpy) CoverageInforceContext(ctx context.Context, m string) (bool, string, error) {
	s.saw(ctx, "CoverageInforce")
	return s.reader().CoverageInforceContext(ctx, m)
}
func (s *sorSpy) ClinicalContextContext(ctx context.Context, m string) (shnsdk.ClinicalContext, bool, error) {
	s.saw(ctx, "ClinicalContext")
	return s.reader().ClinicalContextContext(ctx, m)
}
func (s *sorSpy) SupplementalReportContext(ctx context.Context, m string) ([]byte, bool, error) {
	s.saw(ctx, "SupplementalReport")
	return s.reader().SupplementalReportContext(ctx, m)
}
func (s *sorSpy) FacilityRecordsContext(ctx context.Context, m string) (map[string][]byte, bool, error) {
	s.saw(ctx, "FacilityRecords")
	return s.reader().FacilityRecordsContext(ctx, m)
}
func (s *sorSpy) OpenOrderContext(ctx context.Context, m string) ([]byte, bool, error) {
	s.saw(ctx, "OpenOrder")
	return s.reader().OpenOrderContext(ctx, m)
}
func (s *sorSpy) OpenCoverageContext(ctx context.Context, m string) ([][]byte, error) {
	s.saw(ctx, "OpenCoverage")
	return s.reader().OpenCoverageContext(ctx, m)
}
func (s *sorSpy) ResolveByReferenceContext(ctx context.Context, r string) ([]byte, bool, error) {
	s.saw(ctx, "ResolveByReference")
	return s.reader().ResolveByReferenceContext(ctx, r)
}

// Every read of the payer's own system of record on each leg it answers goes
// through the recording reader, and each is counted: a read that bypassed it
// would be a backend call the record never saw.
func TestExchangeRecord_EveryInboundReadIsNoted(t *testing.T) {
	type row struct {
		leg, operation string
		body           func(t *testing.T, p *levelPayer) []byte
		declared       bool
	}
	rows := []row{
		{leg: "crd-order-select", body: func(*testing.T, *levelPayer) []byte { return conformantCRD("MBR-COVERED", "72148") }},
		{leg: "coverage-eligibility", body: func(t *testing.T, _ *levelPayer) []byte { return eligibilityRequest(t, dtrFrameMember) }},
		{leg: "coverage-eligibility", declared: true, body: func(t *testing.T, p *levelPayer) []byte {
			// An answer naming the payer's own id for the patient is checked
			// against a read after the operation.
			p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/payer-own-id")
			return eligibilityRequest(t, dtrFrameMember)
		}},
		{leg: "pas-claim", body: func(t *testing.T, _ *levelPayer) []byte { return originatorBuiltConformantBundle(t, "MBR-COVERED") }},
		{leg: "pas-claim-update", body: func(t *testing.T, p *levelPayer) []byte {
			request, related := updateBundle(t)
			p.seedPend(t, related)
			return request
		}},
		{leg: "pas-claim-inquire", body: func(*testing.T, *levelPayer) []byte { return inquiryBundle("MBR-COVERED", "", "TRN-1", "72148") }},
		{leg: "dtr-questionnaire-fetch", operation: shnsdk.FrameOperationQuestionnairePackage, body: func(*testing.T, *levelPayer) []byte { return dtrFetchReq }},
		{leg: "crd-order-dispatch", body: func(*testing.T, *levelPayer) []byte { return []byte(crdDispatchRequest) }},
	}
	for _, level := range allLevels {
		for _, r := range rows {
			name := r.leg + "/" + level.String()
			if r.declared {
				name += "/declared"
			}
			t.Run(name, func(t *testing.T) {
				p := eligibilityPayer(t, level, r.declared)
				var got exchangeRecords
				p.g.cfg.ExchangeObserved = got.observe
				body := r.body(t, p)
				spy := &sorSpy{inner: p.g.cfg.SoR}
				p.g.cfg.SoR = spy
				before := p.partner.calls.Load()
				p.send(t, r.leg, r.operation, body)
				rec := got.only(t)
				if len(spy.bypass) != 0 {
					t.Fatalf("reads outside the recording reader: %v", spy.bypass)
				}
				// Every call to the payer's system is counted once: each read
				// of its system of record and each request its services saw.
				calls := int(p.partner.calls.Load() - before)
				if rec.BackendCalls != len(spy.methods)+calls {
					t.Fatalf("backend calls %d, want %d reads of the system of record (%v) and %d requests", rec.BackendCalls, len(spy.methods), spy.methods, calls)
				}
			})
		}
	}
}

// A payer answer this gateway cannot read is that call's malformed answer
// even below strict, where it is relayed as sent.
func TestExchangeRecord_UnreadableAnswerRelayedIsMalformed(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	p.partner.respByPath[crdSelectPath] = []byte(`[1]`)
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	rec := got.only(t)
	if ans.status != http.StatusOK || rec.Outcome != ExchangeAnswered || rec.Backend == nil || rec.Backend.ErrorClass != BackendMalformed {
		t.Fatalf("answer %d; record %+v backend %+v", ans.status, rec, rec.Backend)
	}
}

// A search of the facility's system of record on a federated query it
// answers is a backend call like any read, and a search that cannot answer
// is the facility's system failing.
func TestExchangeRecord_FacilitySearchesAreNoted(t *testing.T) {
	record := func(sor *facilitySoR) ExchangeRecord {
		t.Helper()
		sor.censusSoR = newCensusSoR()
		sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
		g := newFacilityGateway(sor)
		var got exchangeRecords
		g.cfg.ExchangeObserved = got.observe
		g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
			_, _, status, _ := g.facilityRecordsBundle(r.Context(), "MBR-UC05", facilityQueries("DiagnosticReport", "DocumentReference"), "Consent/c-1")
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
		})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil))
		return got.only(t)
	}
	ok := &facilitySoR{patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("", facDR1)}, "DocumentReference": {facPage("", facDOC)}}}
	// The member's Patient id and Patient, then one search per type.
	if rec := record(ok); rec.Outcome != ExchangeAnswered || rec.BackendCalls != 2+len(ok.searched) || len(ok.searched) != 2 {
		t.Fatalf("answered: %s, %d backend calls for %d searches", rec.Outcome, rec.BackendCalls, len(ok.searched))
	}
	for name, row := range map[string]struct {
		err   error
		class string
	}{
		"unavailable":         {&SearchError{Outcome: SearchUnavailable, Reason: "system of record unavailable"}, ExchangeOther},
		"out of its own time": {&SearchError{Outcome: SearchUnavailable, Reason: "time bound"}, BackendTimeout},
		"timed out":           {context.DeadlineExceeded, BackendTimeout},
	} {
		rec := record(&facilitySoR{patient: facPatient, searchErr: row.err})
		if rec.Outcome != ExchangeUpstreamError || rec.Backend == nil || rec.Backend.ErrorClass != row.class || rec.BackendCalls != 3 {
			t.Errorf("%s: %s backend %+v calls %d", name, rec.Outcome, rec.Backend, rec.BackendCalls)
		}
	}
	rec := record(&facilitySoR{patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {[]byte(`{"resourceType":"Bundle","type":"searchset","type":"x"}`)}}})
	if rec.Outcome != ExchangeUpstreamError || rec.Backend == nil || rec.Backend.ErrorClass != BackendMalformed {
		t.Errorf("malformed page: %s backend %+v", rec.Outcome, rec.Backend)
	}
}

// A read after the operation that the request's end cut short makes a
// refusal it caused "other", as a cut-short operation call does.
func TestExchangeRecord_CutShortReadAfterTheForward(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{ExchangeObserved: got.observe, Role: "payer"}}
	g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
		x := exchangeOf(r.Context())
		x.backend(http.StatusOK, time.Millisecond, "")
		x.read(0, time.Millisecond, BackendCancelled)
		x.refusing(RefusalConformance)
		w.WriteHeader(http.StatusServiceUnavailable)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	if rec := got.only(t); rec.Outcome != ExchangeOther || rec.Backend == nil || rec.Backend.Status != http.StatusOK || rec.BackendCalls != 2 {
		t.Fatalf("record %+v backend %+v", rec, rec.Backend)
	}
}

// A body that stopped arriving in time is a timeout, not a read failure.
func TestBodyReadClass(t *testing.T) {
	for name, row := range map[string]struct {
		err  error
		want string
	}{
		"request ended": {fmt.Errorf("read: %w", context.Canceled), BackendCancelled},
		"deadline":      {fmt.Errorf("read: %w", context.DeadlineExceeded), BackendTimeout},
		"net timeout":   {&net.OpError{Op: "read", Err: timeoutErr{}}, BackendTimeout},
		"reset":         {errors.New("connection reset by peer"), BackendRead},
	} {
		if got := bodyReadClass(row.err); got != row.want {
			t.Errorf("%s: %q, want %q", name, got, row.want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// When the payer offers no service for the hook, the listing it read is the
// exchange's backend call: the only call made.
func TestExchangeRecord_ListingIsTheBackendWhenNothingIsForwarded(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"services":[]}`))
	}))
	t.Cleanup(srv.Close)
	n := NewNativeResponder(srv.Client(), srv.URL, "", p.store, fixedClock)
	n.bindFindingEmitter(p.g.emitFinding)
	p.g.cfg.Responder = n
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	rec := got.only(t)
	if ans.status == http.StatusOK || rec.Backend == nil || rec.Backend.Status != http.StatusOK || rec.Backend.ErrorClass != "" {
		t.Fatalf("answer %d; record %+v backend %+v", ans.status, rec, rec.Backend)
	}
}

// A configured X-Correlation-Id is never sent: on a request to the payer's
// system the header is only ever the leg's id, or absent.
func TestBackendCorrelation_ConfiguredValueIsNeverSent(t *testing.T) {
	p := newLevelPayer(t, EnforcementObserve)
	WithBackendHeaders(http.Header{"X-Correlation-Id": {"configured"}, "X-Route": {"r1"}})(p.g.cfg.Responder.(*nativeResponder))
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	for name, h := range map[string]http.Header{"operation": p.partner.lastHeader, "listing": p.partner.listingHeader} {
		if got := h.Values(CorrelationHeader); name == "operation" && (len(got) != 1 || got[0] != ans.corr) || name == "listing" && len(got) != 0 {
			t.Errorf("%s: X-Correlation-Id %v", name, got)
		}
		if h.Get("X-Route") != "r1" {
			t.Errorf("%s: the configured routing header was dropped", name)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "http://payer.test/Claim/$submit", nil)
	req.Header.Set(CorrelationHeader, "configured")
	setBackendCorrelation(withResponderCorrelation(context.Background(), "two words"), req)
	if got := req.Header.Get(CorrelationHeader); got != "" {
		t.Errorf("an unbounded leg id left %q in place", got)
	}
}

// recordFacility runs a federated query's records read against sor on an
// answering leg, under ctx, and returns its exchange record.
func recordFacility(t *testing.T, ctx context.Context, sor SystemOfRecord) ExchangeRecord {
	t.Helper()
	g := newFacilityGateway(nil)
	g.cfg.SoR = sor
	var got exchangeRecords
	g.cfg.ExchangeObserved = got.observe
	g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
		_, _, status, _ := g.facilityRecordsBundle(r.Context(), "MBR-UC05", facilityQueries("DiagnosticReport", "DocumentReference"), "Consent/c-1")
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil).WithContext(ctx))
	return got.only(t)
}

func facility(sor *facilitySoR) *facilitySoR {
	sor.censusSoR = newCensusSoR()
	sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
	return sor
}

// A connector reports a read or search the request's end cut short as its
// system being unavailable, without saying why: the record still reads it
// from the request, cancelled or timed out, never the facility's failure.
func TestExchangeRecord_ReadEndedWithTheRequest(t *testing.T) {
	unavailable := &SoRReadError{Kind: SoRUnavailable}
	for name, row := range map[string]struct {
		end     func(context.Context) (context.Context, context.CancelFunc)
		class   string
		outcome string
	}{
		"request ended": {context.WithCancel, BackendCancelled, ExchangeOther},
		"deadline": {func(c context.Context) (context.Context, context.CancelFunc) {
			return context.WithDeadline(c, time.Unix(0, 0))
		}, BackendTimeout, ExchangeUpstreamError},
	} {
		ctx, cancel := row.end(context.Background())
		cancel()
		for what, sor := range map[string]*facilitySoR{
			"read":   {patient: facPatient, readErr: unavailable},
			"search": {patient: facPatient, searchErr: &SearchError{Outcome: SearchUnavailable, Reason: "system of record unavailable"}},
		} {
			rec := recordFacility(t, ctx, facility(sor))
			if rec.Backend == nil || rec.Backend.ErrorClass != row.class || rec.Outcome != row.outcome {
				t.Errorf("%s, %s: %s backend %+v", name, what, rec.Outcome, rec.Backend)
			}
		}
	}
}

// A connector that cancels a read itself, with the request still live (one
// abandoning its sibling reads, say), reports its system not answering, as an
// unavailable read does: the exchange was not cut short.
func TestExchangeRecord_ConnectorsOwnCancellationIsNotTheRequestEnding(t *testing.T) {
	own := fmt.Errorf("sibling read failed: %w", context.Canceled)
	for what, row := range map[string]struct{ own, unavailable *facilitySoR }{
		"read": {
			&facilitySoR{patient: facPatient, readErr: own},
			&facilitySoR{patient: facPatient, readErr: &SoRReadError{Kind: SoRUnavailable}},
		},
		"search": {
			&facilitySoR{patient: facPatient, searchErr: own},
			&facilitySoR{patient: facPatient, searchErr: &SearchError{Outcome: SearchUnavailable, Reason: "system of record unavailable"}},
		},
	} {
		got := recordFacility(t, context.Background(), facility(row.own))
		want := recordFacility(t, context.Background(), facility(row.unavailable))
		if got.Backend == nil || got.Backend.ErrorClass != ExchangeOther || want.Backend == nil ||
			got.Outcome != want.Outcome || got.Status != want.Status || got.RefusedBy != want.RefusedBy || got.Rule != want.Rule {
			t.Errorf("%s: %s %d %q/%q backend %+v, want as unavailable: %s %d %q/%q", what, got.Outcome, got.Status, got.RefusedBy, got.Rule, got.Backend, want.Outcome, want.Status, want.RefusedBy, want.Rule)
		}
	}
}

// A payer gateway that keeps no system of record reads nothing: its record
// counts only the calls its payer's system saw, and a leg refused before any
// forward records no backend call.
func TestExchangeRecord_NoSystemOfRecordNotesNoRead(t *testing.T) {
	stores := map[string]func() SystemOfRecord{
		"no store": NoSystemOfRecord,
		// Behind the observer's decorator, as a gateway with the observer
		// stream on reads it.
		"observed": func() SystemOfRecord {
			return observingSoR{inner: NoSystemOfRecord(), clock: time.Now, observer: func(ObserverEvent) {}}
		},
	}
	for _, level := range allLevels {
		for name, store := range stores {
			t.Run("forwarded/"+name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.g.cfg.SoR = store()
				var got exchangeRecords
				p.g.cfg.ExchangeObserved = got.observe
				before := p.partner.calls.Load()
				p.sendAs(t, "crd-order-select", "", conformantCRD(strangerMember, "72148"), nosorTokenSubject)
				rec := got.only(t)
				if saw := int(p.partner.calls.Load() - before); saw == 0 || rec.BackendCalls != saw {
					t.Fatalf("%d backend calls recorded, the payer's system saw %d", rec.BackendCalls, saw)
				}
				if rec.Backend == nil || rec.Backend.Status == 0 {
					t.Fatalf("backend %+v, want the forwarded call", rec.Backend)
				}
			})
		}
		// Refused after the member is bound and before anything is
		// forwarded: no contract line is shared.
		t.Run("refused before forwarding/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level, WithDeclaredContractVersions([]string{"pa.pas@2.0"}))
			p.g.cfg.SoR = NoSystemOfRecord()
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			ans := p.sendAs(t, "crd-order-select", "", conformantCRD(strangerMember, "72148"), nosorTokenSubject)
			if ans.status != http.StatusUnprocessableEntity {
				t.Fatalf("answer %d %s, want the version route's 422", ans.status, ans.body)
			}
			if rec := got.only(t); rec.Backend != nil || rec.BackendCalls != 0 {
				t.Fatalf("backend %+v in %d calls, want none", rec.Backend, rec.BackendCalls)
			}
		})
	}
}

// A connector that cannot search is asked for no search, even through the
// observer's decorator: only its reads are counted.
func TestExchangeRecord_NoSearchIsNoCall(t *testing.T) {
	sor := facility(&facilitySoR{patient: facPatient, legacy: map[string][]byte{"DiagnosticReport": []byte(facDR1)}})
	plain := struct {
		SystemOfRecord
		ContextSystemOfRecord
	}{sor, sor}
	for name, s := range map[string]SystemOfRecord{
		"plain":    plain,
		"observed": observingSoR{inner: plain, clock: time.Now, observer: func(ObserverEvent) {}},
	} {
		// The member's Patient id and Patient, and one read of its records.
		if rec := recordFacility(t, context.Background(), s); rec.BackendCalls != 3 {
			t.Errorf("%s: %d backend calls, want 3", name, rec.BackendCalls)
		}
	}
}

// What the facility's system returns out of shape is its malformed answer.
func TestExchangeRecord_FacilityAnswerOutOfShape(t *testing.T) {
	other := bytes.Replace(facPatient, []byte("MBR-UC05"), []byte("MBR-UC99"), 1)
	for name, sor := range map[string]*facilitySoR{
		"another patient's Patient": {patient: other},
		"no Patient record":         {pages: map[string][][]byte{"DiagnosticReport": {facPage("", facDR1)}}},
		"same record twice":         {patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("n", facDR1), facPage("", facDR1)}}},
	} {
		rec := recordFacility(t, context.Background(), facility(sor))
		if rec.Outcome != ExchangeUpstreamError || rec.Backend == nil || rec.Backend.ErrorClass != BackendMalformed {
			t.Errorf("%s: %s backend %+v", name, rec.Outcome, rec.Backend)
		}
	}
}

// A lowercase X-Correlation-Id configured through the Go API is dropped too.
func TestBackendCorrelation_ConfiguredLowercaseIsNeverSent(t *testing.T) {
	p := newLevelPayer(t, EnforcementObserve)
	WithBackendHeaders(http.Header{"x-correlation-id": {"configured"}})(p.g.cfg.Responder.(*nativeResponder))
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	if got := p.partner.lastHeader.Values(CorrelationHeader); len(got) != 1 || got[0] != ans.corr {
		t.Errorf("operation: X-Correlation-Id %v", got)
	}
	if got := p.partner.listingHeader.Values(CorrelationHeader); len(got) != 0 {
		t.Errorf("listing: X-Correlation-Id %v", got)
	}
}

// A repeated member name in the payer's eligibility answer is recorded as on
// every other leg: refused on message integrity, the answer malformed.
func TestExchangeRecord_EligibilityAnswerRepeatingAMember(t *testing.T) {
	for _, level := range allLevels {
		p := eligibilityPayer(t, level, true)
		var got exchangeRecords
		p.g.cfg.ExchangeObserved = got.observe
		p.partner.respByPath[eligibilityPath] = []byte(`{"resourceType":"CoverageEligibilityResponse","resourceType":"CoverageEligibilityResponse"}`)
		p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
		if rec := got.only(t); rec.Outcome != ExchangeRefused || rec.Rule != RefusalIntegrity || rec.Backend == nil || rec.Backend.ErrorClass != BackendMalformed {
			t.Errorf("%s: %s %s/%s backend %+v", level, rec.Outcome, rec.RefusedBy, rec.Rule, rec.Backend)
		}
	}
}
