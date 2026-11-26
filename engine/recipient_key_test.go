package engine

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Rows for the refusal of a leg, or of an answer, whose recipient's registered
// encryption key cannot be sealed to (msgRecipientKeyNotUsable): a network
// rule, authentication, at every conformance level and with the conformance
// module absent. The rows here are the core's: they run on a leg of whichever
// workstream the build compiles in (the toy alone in the core-only profile).
// PA's own routes hold the same refusal in workstream_pa_refusal_reason_test.go
// (its ingress) and workstream_pa_answersize_test.go (its answer).

// recipientKeyNotUsableBody is the whole of the answer a caller is given.
const recipientKeyNotUsableBody = `{"error":"the recipient's registered encryption key is not usable","rule":"authentication"}`

// cryptoDetail is what the answer never carries and the log line and the
// request leg's record do.
const cryptoDetail = "small-order (or absent) X25519 key"

// lowOrderKeys are the X25519 public keys of small order shn-sdk's Seal
// refuses (its own rows list the same encodings): u=0, u=1, u=p-1, the two
// points of order 8, and the non-canonical p and p+1, each with its unused top
// bit clear and set; and no key at all ("absent"), which Seal refuses the same
// way.
func lowOrderKeys(t *testing.T) map[string]*[32]byte {
	t.Helper()
	base := map[string]string{
		"zero":        "0000000000000000000000000000000000000000000000000000000000000000",
		"one":         "0100000000000000000000000000000000000000000000000000000000000000",
		"p-1":         "ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"order 8 (a)": "e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800",
		"order 8 (b)": "5f9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f1157",
		"p":           "edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"p+1":         "eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	}
	out := map[string]*[32]byte{}
	for name, h := range base {
		for _, top := range []bool{false, true} {
			b, err := hex.DecodeString(h)
			if err != nil || len(b) != 32 {
				t.Fatalf("%s: %v", name, err)
			}
			key := name
			if top {
				b[31] |= 0x80
				key += ", top bit set"
			}
			var k [32]byte
			copy(k[:], b)
			if _, err := shnsdk.Seal(shnsdk.Metadata{}, []byte("x"), &k); !errors.Is(err, shnsdk.ErrLowOrderKey) {
				t.Fatalf("%s: shn-sdk's Seal answers %v, not ErrLowOrderKey; the row would prove nothing", key, err)
			}
			out[key] = &k
		}
	}
	if _, err := shnsdk.Seal(shnsdk.Metadata{}, []byte("x"), nil); !errors.Is(err, shnsdk.ErrLowOrderKey) {
		t.Fatalf("absent: shn-sdk's Seal answers %v, not ErrLowOrderKey; the row would prove nothing", err)
	}
	out["absent"] = nil
	return out
}

// sortedKeyNames is the names of keys in order, so the rows run in one order.
func sortedKeyNames(keys map[string]*[32]byte) []string {
	names := make([]string, 0, len(keys))
	for n := range keys {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// registerEncKey replaces holder's registered encryption key in reg.
func registerEncKey(t *testing.T, reg shnsdk.Registry, holder string, key *[32]byte) {
	t.Helper()
	h, ok := reg.Lookup(holder)
	if !ok {
		t.Fatalf("%s is not registered", holder)
	}
	h.EncPub = key
	reg.Set(holder, h)
}

// countTokens counts g's calls to the Authorization Framework.
func countTokens(g *Gateway) *atomic.Int32 {
	var n atomic.Int32
	g.cfg.TokenMetric = func(string, time.Duration) { n.Add(1) }
	return &n
}

type errorString string

func (e errorString) Error() string { return string(e) }

// keyColumn is a conformance choice a row runs at: a level, or the module
// absent.
type keyColumn struct {
	level  ConformanceEnforcement
	absent bool
}

func (c keyColumn) String() string {
	if c.absent {
		return "module-absent"
	}
	return c.level.String()
}

// keyColumns are every level, then the conformance module absent.
var keyColumns = []keyColumn{{level: EnforcementNone}, {level: EnforcementObserve}, {level: EnforcementStructural}, {level: EnforcementStrict}, {level: EnforcementStrict, absent: true}}

// keyLog collects the holder log for the rest of the test.
type keyLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *keyLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *keyLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureKeyLog(t *testing.T) *keyLog {
	t.Helper()
	l := &keyLog{}
	previous := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(previous) })
	return l
}

// keyLeg is a leg this build compiles in: the first by name whose request its
// requester carries from its participant and whose answer its recipient
// sends, each a transmit a test payload passes. The toy's one leg in the
// core-only profile.
func keyLeg(t *testing.T) (workstream, tx string, spec legSpec) {
	t.Helper()
	type leg struct {
		ws, tx string
		spec   legSpec
	}
	var legs []leg
	for _, w := range workstreams {
		for tx, s := range w.catalog() {
			legs = append(legs, leg{w.id(), tx, s})
		}
	}
	sort.Slice(legs, func(i, j int) bool { return legs[i].tx < legs[j].tx })
	for _, l := range legs {
		if l.spec.Op == "" || l.spec.RespOp == "" {
			continue
		}
		if _, err := relay.Transmit(testRequest([]byte(`{}`)), relay.Check(requestKey(l.tx, true))); err != nil {
			continue
		}
		if _, err := relay.Transmit(testResponse([]byte(`{}`)), relay.Check(answerKey(l.tx, relay.OutcomeAnswered))); err != nil {
			continue
		}
		return l.ws, l.tx, l.spec
	}
	t.Fatal("the build compiles in no leg a requester carries and a recipient answers")
	return "", "", legSpec{}
}

// keyNet stands in for the Authorization Framework and the Hub: it mints a
// token for each leg it is asked for, signed with authzPriv and naming holder,
// and answers each envelope routed to it as the recipient (the envelope's
// own) with an echo sealed to answerTo under its own answer token. It counts
// both.
type keyNet struct {
	authzPriv               ed25519.PrivateKey
	clock                   func() time.Time
	holder                  string
	answerTo                *[32]byte
	spec                    legSpec
	authorizations, answers atomic.Int32
}

// keyEcho is what keyNet's recipient answers.
var keyEcho = []byte(`{"echo":true}`)

func (n *keyNet) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	answer := func(b []byte) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
	}
	switch {
	case strings.HasSuffix(req.URL.Path, "/authorize"):
		n.authorizations.Add(1)
		var a authorizeReq
		_ = json.Unmarshal(body, &a)
		tok := signTestToken(shnsdk.Token{Operation: a.Operation, Subject: a.SubjectPCI, Frame: a.Frame, Holder: n.holder,
			CorrelationID: a.CorrelationID, Expiry: n.clock().Add(time.Hour), PayloadHash: a.PayloadHash}, n.authzPriv)
		b, _ := json.Marshal(authorizeResp{Token: &tok})
		return answer(b)
	case strings.HasSuffix(req.URL.Path, "/route"):
		n.answers.Add(1)
		env, err := shnsdk.DecodeEnvelope(body)
		if err != nil {
			return nil, err
		}
		var reqTok shnsdk.Token
		_ = json.Unmarshal([]byte(env.Metadata.AuthzToken), &reqTok)
		resp, err := shnsdk.Seal(shnsdk.Metadata{Sender: env.Metadata.Recipient, Recipient: env.Metadata.Sender, TransactionType: env.Metadata.TransactionType,
			AuthorityFrame: n.spec.RespFrame, Timestamp: n.clock().Format(time.RFC3339), CorrelationID: env.Metadata.CorrelationID}, keyEcho, n.answerTo)
		if err != nil {
			return nil, err
		}
		tok := signTestToken(shnsdk.Token{Operation: n.spec.RespOp, Subject: reqTok.Subject, Frame: n.spec.RespFrame, Holder: env.Metadata.Recipient,
			CorrelationID: env.Metadata.CorrelationID, Expiry: n.clock().Add(time.Hour), PayloadHash: sha256hex(resp.Ciphertext)}, n.authzPriv)
		tb, _ := json.Marshal(tok)
		resp.Metadata.AuthzToken = string(tb)
		b, err := shnsdk.EncodeEnvelope(resp)
		if err != nil {
			return nil, err
		}
		return answer(b)
	}
	return nil, fmt.Errorf("keyNet: unexpected call to %s", req.URL.Path)
}

// keyRig is a gateway of role "self" whose one peer, "peer", it sends a leg
// to or answers one from, on keyLeg's leg, at col, through keyNet. Its
// records are the calls it answered.
type keyRig struct {
	g           *Gateway
	net         *keyNet
	self, peer  shnsdk.Identity
	ws, tx, pci string
	spec        legSpec
	tokens      *atomic.Int32
	mu          sync.Mutex
	records     []ExchangeRecord
}

func newKeyRig(t *testing.T, col keyColumn, role string) *keyRig {
	t.Helper()
	self, err := shnsdk.GenerateIdentity("self")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := shnsdk.GenerateIdentity("peer")
	if err != nil {
		t.Fatal(err)
	}
	authzPriv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	ws, tx, spec := keyLeg(t)
	pci := "pci:recipient-key"
	if spec.namesNoPatient() || spec.fromAnswer() {
		pci = ""
	}
	reg := shnsdk.NewRegistry()
	reg.Set("peer", shnsdk.RegistryEntry{ID: "peer", Role: "peer", EncPub: peer.EncPub, SignPub: peer.SignPub})
	net := &keyNet{authzPriv: authzPriv, clock: clock, holder: "self", answerTo: self.EncPub, spec: spec}
	rig := &keyRig{self: self, peer: peer, ws: ws, tx: tx, pci: pci, spec: spec, net: net}
	rig.g = &Gateway{cfg: Config{
		Role: role, HolderID: "self", Identity: self, Reg: reg, Clock: clock,
		AuthzURL: "https://authz.test", AuthzPub: authzPriv.Public().(ed25519.PublicKey), HubURL: "https://hub.test",
		Client: &http.Client{Transport: net}, ConformanceEnforcement: col.level, conformanceModuleAbsent: col.absent,
		ExchangeObserved: func(r ExchangeRecord) {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.records = append(rig.records, r)
		},
	}}
	rig.tokens = countTokens(rig.g)
	return rig
}

// only is the one record the rig's call produced.
func (rig *keyRig) only(t *testing.T) ExchangeRecord {
	t.Helper()
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.records) != 1 {
		t.Fatalf("%d exchange records, want one: %+v", len(rig.records), rig.records)
	}
	return rig.records[0]
}

// send is the rig's participant's call that sends the leg to peer, carried as
// its system sent it, answered as the origination routes answer a leg's
// error (relayOriginationError).
func (rig *keyRig) send(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	rig.g.recordExchange("recipient-key", DirectionIngress, func(w http.ResponseWriter, r *http.Request) {
		answer, err := rig.g.roundTrip(r.Context(), r, "peer", rig.spec.ReqFrame, rig.spec.RespFrame, rig.spec.Op, rig.spec.RespOp, rig.tx, rig.spec.Scope, rig.pci, "corr-recipient-key", "",
			Content{WorkstreamType: rig.ws, Carried: true, Payload: testRequest([]byte(`{"ask":true}`))})
		if err == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(answer)
			return
		}
		if !rig.g.relayOriginationError(w, err) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
	})(w, httptest.NewRequest(http.MethodPost, "/", nil))
	return w
}

// answer is the rig's answer to a leg from peer, built for peer as an inbound
// route builds one; systemAnswered says its participant's system answered
// first.
func (rig *keyRig) answer(t *testing.T, systemAnswered bool) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	rig.g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
		if systemAnswered {
			exchangeOf(r.Context()).backend(http.StatusOK, 0, "")
		}
		out, status, msg := rig.g.buildResponseLeg(r, rig.spec.RespFrame, rig.spec.RespOp, rig.tx, "corr-recipient-key", testResponse(keyEcho),
			answerKey(rig.tx, relay.OutcomeAnswered), nil, rig.pci, "peer", "")
		if status != 0 {
			writeResponseLegFailure(w, status, msg)
			return
		}
		writeLeg(w, out)
	})(w, httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil))
	return w
}

// REJECTION (network rule, authentication): a gateway refuses a leg whose
// recipient's registered encryption key is of small order, in every encoding,
// or absent, at every level and with the conformance module absent, before a
// token is asked for or anything is sent: its caller is answered 502 with the
// rule and no cryptographic detail, and the record names this gateway's
// refusal on the rule and, in its reason, the cause. The log names the cause.
// The same leg to a recipient with a usable key is sealed, sent and answered.
func TestRoundTrip_ARecipientKeyNotUsableIsRefusedOnAuthentication(t *testing.T) {
	if ws, tx, _ := keyLeg(t); testing.Verbose() {
		t.Logf("on %s's leg %s", ws, tx)
	}
	logged := captureKeyLog(t)
	keys := lowOrderKeys(t)
	for _, col := range keyColumns {
		for _, name := range sortedKeyNames(keys) {
			t.Run(col.String()+"/"+name, func(t *testing.T) {
				rig := newKeyRig(t, col, "provider")
				registerEncKey(t, rig.g.cfg.Reg, "peer", keys[name])
				w := rig.send(t)
				if w.Code != http.StatusBadGateway || string(bytes.TrimSpace(w.Body.Bytes())) != recipientKeyNotUsableBody {
					t.Fatalf("answer %d %s, want 502 %s", w.Code, w.Body.String(), recipientKeyNotUsableBody)
				}
				if strings.Contains(w.Body.String(), "X25519") || strings.Contains(w.Body.String(), "order") {
					t.Fatalf("the answer carries the cryptographic detail: %s", w.Body.String())
				}
				if a, h, n := rig.net.authorizations.Load(), rig.net.answers.Load(), rig.tokens.Load(); a != 0 || h != 0 || n != 0 {
					t.Fatalf("the refused leg was authorized (%d), crossed the Hub (%d) or asked for a token (%d)", a, h, n)
				}
				r := rig.only(t)
				if r.Outcome != ExchangeRefused || r.RefusedBy != RefusedByProviderGateway || r.Rule != RefusalAuthentication || r.Status != http.StatusBadGateway {
					t.Fatalf("record %s %s/%s status %d, want refused by the provider gateway on authentication, 502", r.Outcome, r.RefusedBy, r.Rule, r.Status)
				}
				if r.RefusalReason != string(reasonRecipientKeyNotUsable) || !strings.Contains(r.RefusalReason, cryptoDetail) {
					t.Fatalf("record reason %q, want %q, naming the %s", r.RefusalReason, reasonRecipientKeyNotUsable, cryptoDetail)
				}
				if !strings.Contains(logged.String(), rig.tx+" not sealed to peer: "+string(reasonRecipientKeyNotUsable)) {
					t.Fatalf("the log does not name the cause:\n%s", logged.String())
				}
			})
		}
		t.Run(col.String()+"/control: a usable key", func(t *testing.T) {
			rig := newKeyRig(t, col, "provider")
			w := rig.send(t)
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), keyEcho) || rig.net.answers.Load() != 1 || rig.tokens.Load() == 0 {
				t.Fatalf("answer %d %s after %d legs and %d tokens; want the leg sealed, sent and answered", w.Code, w.Body.String(), rig.net.answers.Load(), rig.tokens.Load())
			}
			if r := rig.only(t); r.Outcome != ExchangeAnswered {
				t.Fatalf("record %+v, want answered", r)
			}
		})
	}
}

// REJECTION (network rule, authentication): a gateway refuses to send an
// answer to a requester whose registered encryption key is of small order, in
// every encoding, or absent, at every level and with the conformance module
// absent, before the answer's token is asked for: 502, the status of every
// refusal of an answer, with the rule and no cryptographic detail. Once its
// system has answered, the refusal is attested delivered, as every refusal of
// a system's answer is; before, it is not. The record names this gateway's
// refusal on the rule; the log names the cause. The same answer to a
// requester with a usable key is sealed to it and sent.
func TestBuildResponseLeg_ARequesterKeyNotUsableIsRefusedOnAuthentication(t *testing.T) {
	if ws, tx, _ := keyLeg(t); testing.Verbose() {
		t.Logf("on %s's leg %s", ws, tx)
	}
	logged := captureKeyLog(t)
	keys := lowOrderKeys(t)
	for _, col := range keyColumns {
		for _, systemAnswered := range []bool{true, false} {
			attested := map[bool]string{true: "attested", false: "unattested"}[systemAnswered]
			for _, name := range sortedKeyNames(keys) {
				t.Run(col.String()+"/"+attested+"/"+name, func(t *testing.T) {
					rig := newKeyRig(t, col, "payer")
					registerEncKey(t, rig.g.cfg.Reg, "peer", keys[name])
					w := rig.answer(t, systemAnswered)
					if w.Code != http.StatusBadGateway || string(bytes.TrimSpace(w.Body.Bytes())) != recipientKeyNotUsableBody {
						t.Fatalf("answer %d %s, want 502 %s", w.Code, w.Body.String(), recipientKeyNotUsableBody)
					}
					if strings.Contains(w.Body.String(), "X25519") || strings.Contains(w.Body.String(), "order") {
						t.Fatalf("the answer carries the cryptographic detail: %s", w.Body.String())
					}
					wantDelivered := map[bool]string{true: "yes", false: ""}[systemAnswered]
					if d := w.Header().Get(HubDeliveredHeader); d != wantDelivered {
						t.Fatalf("%s %q, want %q", HubDeliveredHeader, d, wantDelivered)
					}
					if a, n := rig.net.authorizations.Load(), rig.tokens.Load(); a != 0 || n != 0 {
						t.Fatalf("the refused answer was authorized (%d) or asked for a token (%d)", a, n)
					}
					r := rig.only(t)
					if r.Outcome != ExchangeRefused || r.RefusedBy != RefusedByPayerGateway || r.Rule != RefusalAuthentication || r.Status != http.StatusBadGateway {
						t.Fatalf("record %s %s/%s status %d, want refused by the payer gateway on authentication, 502", r.Outcome, r.RefusedBy, r.Rule, r.Status)
					}
					if !strings.Contains(logged.String(), rig.tx+" answer not sealed to peer: "+string(reasonRecipientKeyNotUsable)) {
						t.Fatalf("the log does not name the cause:\n%s", logged.String())
					}
				})
			}
			t.Run(col.String()+"/"+attested+"/control: a usable key", func(t *testing.T) {
				rig := newKeyRig(t, col, "payer")
				w := rig.answer(t, systemAnswered)
				if w.Code != http.StatusOK || rig.tokens.Load() == 0 {
					t.Fatalf("answer %d %s after %d tokens, want the answer sealed and sent", w.Code, w.Body.String(), rig.tokens.Load())
				}
				env, err := shnsdk.DecodeEnvelope(w.Body.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				if got, err := shnsdk.Open(env, rig.peer.EncPub, rig.peer.EncPriv); err != nil || !bytes.Equal(got, keyEcho) {
					t.Fatalf("the requester opens %q (%v), want the answer sealed to it", got, err)
				}
			})
		}
	}
}

// The origination mapper answers the refusal 502 with its rule and no
// cryptographic detail, recorded as this gateway's refusal on authentication
// with its reason; any other failure to seal keeps its answer: only shn-sdk's
// refusal of the key is this refusal (errors.Is).
func TestOriginate_OnlyTheKeysRefusalIsAnsweredAsIt(t *testing.T) {
	g := &Gateway{}
	rec := httptest.NewRecorder()
	if !g.relayOriginationError(rec, fmt.Errorf("leg: %w", errRecipientKeyNotUsable)) || rec.Code != http.StatusBadGateway ||
		string(bytes.TrimSpace(rec.Body.Bytes())) != recipientKeyNotUsableBody {
		t.Fatalf("the refusal answered %d %s, want 502 %s", rec.Code, rec.Body.String(), recipientKeyNotUsableBody)
	}
	rec = httptest.NewRecorder()
	if g.relayOriginationError(rec, errorString("seal failed")) {
		t.Fatalf("another seal failure was answered as a refusal: %d %s", rec.Code, rec.Body.String())
	}
	x := &exchangeRecorder{self: RefusedByProviderGateway}
	x.legOutcome(errorString("seal failed"))
	if x.outcome != ExchangeOther || x.rule != "" {
		t.Fatalf("another seal failure recorded %s on %q, want other", x.outcome, x.rule)
	}
}

// The leg's outcome names the refusal of a recipient whose registered key is
// not usable by structure (errors.Is), wrapped or not: refused by this gateway
// on authentication, with the registered reason that names the cause.
func TestLegOutcome_ARecipientKeyNotUsableIsRefusedOnAuthentication(t *testing.T) {
	for name, err := range map[string]error{
		"as returned": errRecipientKeyNotUsable,
		"wrapped":     fmt.Errorf("leg: %w", errRecipientKeyNotUsable),
	} {
		x := &exchangeRecorder{self: RefusedByProviderGateway}
		x.legOutcome(err)
		if x.outcome != ExchangeRefused || x.party != RefusedByProviderGateway || x.rule != RefusalAuthentication || x.reason != reasonRecipientKeyNotUsable {
			t.Fatalf("%s: recorded %s by %q on %q with reason %q, want refused by this gateway on authentication with %q",
				name, x.outcome, x.party, x.rule, x.reason, reasonRecipientKeyNotUsable)
		}
	}
}
