//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Every token this gateway verifies states every binding: a receiving
// gateway binds a request's token to its envelope's correlation and sender,
// so an envelope that states no sender, or a correlation that is only
// blanks, binds nothing and is refused on authority, 403 "authz verification
// failed", before the requester is looked up or anything is read, at every
// conformance level. (An exactly empty correlation is refused before, 400
// "missing correlation id".) The control is the same leg as the requester
// sends it, answered.
func TestInbound_AnEnvelopeStatingNoBindingIsRefusedOnAuthority(t *testing.T) {
	t.Parallel()
	request := originatorBuiltConformantBundle(t, "MBR-COVERED")
	rows := map[string]func(m *shnsdk.Metadata, tok *shnsdk.Token){
		// The token is the requester's own: only the envelope states no
		// sender.
		"no sender": func(m *shnsdk.Metadata, _ *shnsdk.Token) { m.Sender = "" },
		// The token states the same blank holder: nothing but the blank
		// itself is wrong.
		"a blank sender":      func(m *shnsdk.Metadata, tok *shnsdk.Token) { m.Sender, tok.Holder = " ", " " },
		"a blank correlation": func(m *shnsdk.Metadata, tok *shnsdk.Token) { m.CorrelationID, tok.CorrelationID = " ", " " },
		"a correlation of tabs and spaces": func(m *shnsdk.Metadata, tok *shnsdk.Token) {
			m.CorrelationID, tok.CorrelationID = "\t ", "\t "
		},
	}
	for _, level := range allLevels {
		for name, restate := range rows {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p, own := guardPayer(t, level)
				var refreshes atomic.Int32
				p.g.cfg.RegistryRefresh = func(context.Context) error { refreshes.Add(1); return nil }
				var recs exchangeRecords
				p.g.cfg.ExchangeObserved = recs.observe
				p.restate = restate
				got := p.send(t, "pas-claim", "", request)
				p.wantRefused(t, got, http.StatusForbidden, `"authz verification failed"`)
				if got.framed || got.header.Get(NotActedHeader) != "" || got.header.Get("Retry-After") != "" {
					t.Fatalf("answer framed %v, %s %q, Retry-After %q: want the bare authority refusal, not the requester-unknown one",
						got.framed, NotActedHeader, got.header.Get(NotActedHeader), got.header.Get("Retry-After"))
				}
				if rec := recs.only(t); rec.Outcome != ExchangeRefused || rec.RefusedBy != RefusedByPayerGateway || rec.Rule != RefusalAuthority {
					t.Fatalf("exchange record %s by %s on %s, want refused by the payer gateway on %s", rec.Outcome, rec.RefusedBy, rec.Rule, RefusalAuthority)
				}
				if n := refreshes.Load(); n != 0 {
					t.Fatalf("the registry was refreshed %d time(s): the requester was looked up before the refusal", n)
				}
				if n := p.partner.calls.Load(); n != 0 {
					t.Fatalf("the payer's system was called %d time(s)", n)
				}
				if n := own.calls.Load(); n != 0 {
					t.Fatalf("the gateway made %d call(s) of its own", n)
				}
			})
		}
		t.Run(level.String()+"/control", func(t *testing.T) {
			p, _ := guardPayer(t, level)
			p.restate = func(*shnsdk.Metadata, *shnsdk.Token) {}
			if got := p.send(t, "pas-claim", "", request); got.status != http.StatusOK || !got.framed {
				t.Fatalf("answer %d %s, want a framed 200", got.status, got.body)
			}
		})
	}
}

// The originating gateway takes an answer only from the holder its leg was
// routed to: an answer whose envelope names another sender is refused
// "response sender mismatch" whoever holds its token, the routed recipient
// included, since the sender is checked before the token; an answer from the
// recipient whose token another holder holds is refused "response leg
// authorization failed", the token bound to the routed recipient, never to
// the envelope's own claim. Each at every conformance level; the control is
// the same answer, its token signed again unchanged, which is taken.
func TestOrigination_AnAnswerIsTheRoutedRecipientsSenderFirst(t *testing.T) {
	t.Parallel()
	// rows[0] is also read on every route below.
	rows := []struct {
		name    string
		restate func(e *shnsdk.Envelope, tok *shnsdk.Token)
		cause   string // "" taken
	}{
		{"another sender, the recipient's token", func(e *shnsdk.Envelope, _ *shnsdk.Token) { e.Metadata.Sender = "payer-b" }, "response sender mismatch"},
		{"another sender, its own token", func(e *shnsdk.Envelope, tok *shnsdk.Token) { e.Metadata.Sender, tok.Holder = "payer-b", "payer-b" }, "response sender mismatch"},
		{"no sender", func(e *shnsdk.Envelope, _ *shnsdk.Token) { e.Metadata.Sender = "" }, "response sender mismatch"},
		{"the recipient, another holder's token", func(_ *shnsdk.Envelope, tok *shnsdk.Token) { tok.Holder = "payer-b" }, "response leg authorization failed"},
		{"the recipient, a token stating no holder", func(_ *shnsdk.Envelope, tok *shnsdk.Token) { tok.Holder = "" }, "response leg authorization failed"},
		{"control: the recipient, its own token", func(*shnsdk.Envelope, *shnsdk.Token) {}, ""},
	}
	for _, level := range allLevels {
		for _, row := range rows {
			t.Run(level.String()+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				hubFault{restate: row.restate}.install(t, env)
				_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Payload: testRequest(env.crdReq)})
				if row.cause == "" {
					if err != nil {
						t.Fatalf("refused: %v; want taken", err)
					}
					return
				}
				var lost *answerLostError
				if !errors.As(err, &lost) || lost.cause != row.cause || lost.verified {
					t.Fatalf("err %v; want the unverified %q", err, row.cause)
				}
			})
		}
		// What the requester's system sees for an answer from another sender
		// whose token is the recipient's own, on each route, as the
		// delivered-answer rows read it: the 502 naming the reason, marked
		// delivered unknown.
		for _, e := range deliveredEnvelopes {
			t.Run(level.String()+"/another sender, the recipient's token, answered on "+e.name, func(t *testing.T) {
				t.Parallel()
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				hubFault{restate: rows[0].restate}.install(t, env)
				rec := e.call(t, env)
				const text = "the recipient may have received this request (response sender mismatch): check its outcome before resending"
				if e.fhir() {
					wantDeliveredOutcome(t, rec, http.StatusBadGateway, deliveredUnknown, text)
				} else {
					wantDeliveredProblem(t, rec, http.StatusBadGateway, deliveredUnknown, e.mediaType, text)
				}
			})
		}
	}
}

// The outbound path never states an empty binding: for every leg the
// gateway compiles, from each leg taxonomy row (pa.json, fqs.json) as the
// engine reads it, the request's frame and operation and the answer's frame
// and operation are stated, and each operation finds its leg's row
// (legSpecForOp), so verifyAuthorized, verifyAnswerToken and
// verifyInboundToken pass shnsdk.VerifyBoundWant a value for each, and never
// meet shnsdk.ErrBindingUnstated on an exchange as the network carries it.
// The holder each binds is this gateway's own (Config.HolderID) or a
// registered peer's; the gateway's is its identity bundle's id, which
// shnsdk.LoadBundle requires at boot (the app reads the bundle before it
// builds the engine), so a gateway with none does not start.
func TestOutboundBindings_EveryCompiledLegStatesEachBinding(t *testing.T) {
	compiled := map[string]bool{}
	for _, w := range workstreams {
		for tx, s := range w.catalog() {
			compiled[tx] = true
			for what, v := range map[string]string{"request frame": s.ReqFrame, "request operation": s.Op, "answer frame": s.RespFrame, "answer operation": s.RespOp} {
				if strings.TrimSpace(v) == "" {
					t.Errorf("%s: the %s is empty", tx, what)
				}
			}
			if got, answer, ok := legSpecForOp(s.Op); !ok || answer || got.Op != s.Op || got.RespOp != s.RespOp {
				t.Errorf("%s: its request operation %q does not find its row as a request", tx, s.Op)
			}
			if got, answer, ok := legSpecForOp(s.RespOp); !ok || !answer || got.Op != s.Op || got.RespOp != s.RespOp {
				t.Errorf("%s: its answer operation %q does not find its row as an answer", tx, s.RespOp)
			}
		}
	}
	rows := 0
	for _, tax := range []legtaxonomy.Taxonomy{legtaxonomy.PA(), legtaxonomy.FQS()} {
		for _, l := range tax.Legs {
			if !l.Routable() {
				continue
			}
			rows++
			if !compiled[l.TransactionType] {
				t.Errorf("%s/%s: a routable row the gateway does not compile", tax.Workstream, l.TransactionType)
			}
		}
	}
	if rows < 9 || len(compiled) < rows {
		t.Fatalf("read %d routable rows and %d compiled legs: the matrix must see every leg", rows, len(compiled))
	}

	// The holder at boot: an identity bundle without an id does not load.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"role":"provider"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := shnsdk.LoadBundle(dir); err == nil || !strings.Contains(err.Error(), `missing "id"`) {
		t.Fatalf("a bundle with no holder id loaded (%v); want it refused at boot", err)
	}
}
