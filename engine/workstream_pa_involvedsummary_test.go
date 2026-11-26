//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"golang.org/x/crypto/nacl/box"
)

// A gateway signs a summary of the involved list it sends on each side of a
// leg, for a Hub that lists both involved and involved-summary, and for no
// other: these rows hold what it sends, and that it sends nothing else.

// hubReads is which of the two hubAccepts fields a row's Hub lists.
type hubReads struct {
	name            string
	involved, summy bool
}

var everyHubReading = []hubReads{
	{"neither", false, false},
	{"involved only", true, false},
	{"involved-summary only", false, true},
	{"both", true, true},
}

func (h hubReads) both() bool { return h.involved && h.summy }

// noOmission is a summary's Omitted when the gateway left no one out.
var noOmission = []shnsdk.InvolvedOmission{}

// decodeSummary is the summary an envelope carries, failing the test when
// there is none or it does not decode.
func decodeSummary(t *testing.T, md shnsdk.Metadata) shnsdk.InvolvedSummary {
	t.Helper()
	sum, err := shnsdk.DecodeInvolvedSummary(md.InvolvedSummary)
	if err != nil || sum == nil {
		t.Fatalf("no summary that decodes: %q (%v)", md.InvolvedSummary, err)
	}
	return *sum
}

// tokenOf is the token an envelope's AuthzToken (or an involved entry) carries.
func tokenOf(t *testing.T, js string) shnsdk.Token {
	t.Helper()
	var tok shnsdk.Token
	if err := json.Unmarshal([]byte(js), &tok); err != nil {
		t.Fatal(err)
	}
	return tok
}

// assertSummary checks that md carries a summary signed with pub for side and
// md's own token, and for neither the other side nor any other token (each
// involved entry's), listing exactly the entries md's list holds and omitting
// exactly omitted.
func assertSummary(t *testing.T, md shnsdk.Metadata, pub ed25519.PublicKey, side string, omitted []shnsdk.InvolvedOmission) {
	t.Helper()
	sum := decodeSummary(t, md)
	legTok := tokenOf(t, md.AuthzToken)
	if err := shnsdk.VerifyInvolvedSummary(sum, pub, side, legTok); err != nil {
		t.Fatalf("the summary does not verify for its side and its own token: %v (%+v)", err, sum)
	}
	other := shnsdk.InvolvedSideAnswer
	if side == other {
		other = shnsdk.InvolvedSideRequest
	}
	if shnsdk.VerifyInvolvedSummary(sum, pub, other, legTok) == nil {
		t.Fatalf("the summary verifies for the %s side too", other)
	}
	var entries []shnsdk.InvolvedToken
	if md.Involved != "" {
		var err error
		if entries, err = shnsdk.DecodeInvolved(md.Involved); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range entries {
		if shnsdk.VerifyInvolvedSummary(sum, pub, side, tokenOf(t, e.Token)) == nil {
			t.Fatal("the summary verifies for an involved entry's token: it must be bound to the side's own")
		}
	}
	if sum.Listed != len(entries) {
		t.Fatalf("listed %d, want the %d entries the list holds", sum.Listed, len(entries))
	}
	if !slices.Equal(sum.Omitted, omitted) {
		t.Fatalf("omitted %+v, want %+v", sum.Omitted, omitted)
	}
}

// assertNoSummary checks that raw, an envelope as sent, carries no summary:
// no involvedSummary member at all, so it is byte for byte the envelope the
// gateway sent before summaries were.
func assertNoSummary(t *testing.T, raw []byte) {
	t.Helper()
	env, err := shnsdk.DecodeEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if env.Metadata.InvolvedSummary != "" || bytes.Contains(raw, []byte(`"involvedSummary"`)) {
		t.Fatalf("a summary was sent: %q", env.Metadata.InvolvedSummary)
	}
	again, err := shnsdk.EncodeEnvelope(env)
	if err != nil || !bytes.Equal(again, raw) {
		t.Fatalf("the envelope is not what its metadata encodes to without a summary (%v)", err)
	}
}

// sentRequests is each request envelope the fake Hub received, as sent.
func (e *inProcessExchange) sentRequests() [][]byte {
	e.substrate.mu.Lock()
	defer e.substrate.mu.Unlock()
	return slices.Clone(e.substrate.sent)
}

// The request side, through the provider's own PAS ingress and the gateway's
// real seal and send: only a Hub listing both fields gets a summary, which
// verifies with the provider's holder signing key for the request side and the
// request's own token, lists the entries the list holds and states each
// patient left out; every other Hub gets the envelope it got before, the list
// sent exactly when the Hub lists involved. The request is carried in every
// row, with its summary or without, at every conformance level, except that
// at strict the participant's patient.mixed check refuses a request carrying
// another patient before it is sent, so nothing goes.
func TestInvolvedSummary_RequestOnlyForAHubThatReadsOne(t *testing.T) {
	t.Parallel()
	party := string(withCoverageCarrying(t, []byte(pasIngressBundle("00001", "")), pasAnotherPatientCarried["a party naming another patient"]))
	other := heldPCI(t, "MBR-NOTCOVERED")
	for _, row := range []struct {
		name       string
		body       string
		refuse     bool
		wantListed int
		omitted    []shnsdk.InvolvedOmission
	}{
		{"another patient named", party, false, 1, noOmission},
		{"another patient left out", party, true, 0, []shnsdk.InvolvedOmission{{Reason: involvedOmitRefused, Count: 1}}},
		{"no other patient", pasIngressBundle("00001", ""), false, 0, noOmission},
	} {
		for _, hub := range everyHubReading {
			for _, level := range allLevels {
				t.Run(row.name+"/"+hub.name+"/"+level.String(), func(t *testing.T) {
					env := newInProcessExchange(t)
					env.originator.cfg.ConformanceEnforcement = level
					env.originator.cfg.HubAcceptsInvolved = hub.involved
					env.originator.cfg.HubAcceptsInvolvedSummary = hub.summy
					if row.refuse {
						env.substrate.refuseInvolved = map[string]bool{other: true}
					}
					env.payerReturns(LegResult{Response: testResponse(levelPASPayerAnswer)})
					rec := httptest.NewRecorder()
					env.originator.handlePASIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(row.body)))
					sent := env.sentRequests()
					if row.body == party && refusesAt(level, RulePatientMixed) {
						if rec.Code != http.StatusForbidden || len(sent) != 0 {
							t.Fatalf("status %d, %d envelopes sent; want the request refused unsent", rec.Code, len(sent))
						}
						return
					}
					if rec.Code >= 300 || len(sent) != 1 {
						t.Fatalf("status %d, %d envelopes sent; want the request carried once", rec.Code, len(sent))
					}
					md, err := shnsdk.DecodeEnvelope(sent[0])
					if err != nil {
						t.Fatal(err)
					}
					if listed := len(involvedSubjects(t, md.Metadata.Involved)); hub.involved && listed != row.wantListed || !hub.involved && md.Metadata.Involved != "" {
						t.Fatalf("list %q, want %d entries exactly when the Hub lists involved", md.Metadata.Involved, row.wantListed)
					}
					if !hub.both() {
						assertNoSummary(t, sent[0])
						return
					}
					assertSummary(t, md.Metadata, env.originator.cfg.Identity.SignPriv.Public().(ed25519.PublicKey), shnsdk.InvolvedSideRequest, row.omitted)
				})
			}
		}
	}
}

// A request whose patients the leg's strict readers cannot read at all is
// sent with no list and a summary saying why: one patient's worth left out,
// request-unreadable, never a list that left no one out. Its readable control
// lists the other patient and omits no one.
func TestInvolvedSummary_AnUnreadableRequestStatesItsOmission(t *testing.T) {
	base := []byte(`{"hook":"order-select","context":{"patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-OX"}}}]}}}`)
	unreadable := strictOnlyUnreadable(base)
	if len(unreadable) == 0 {
		t.Fatal("fixture: no unreadable request")
	}
	for name, body := range unreadable {
		t.Run(name, func(t *testing.T) {
			g, _, _, _ := involvedGateway(t, true)
			g.cfg.HubAcceptsInvolvedSummary = true
			md := requestSide(t, g, "crd-order-select", body)
			if md.Involved != "" {
				t.Fatalf("list %q, want none", md.Involved)
			}
			assertSummary(t, md, g.cfg.Identity.SignPriv.Public().(ed25519.PublicKey), shnsdk.InvolvedSideRequest,
				[]shnsdk.InvolvedOmission{{Reason: involvedOmitRequestUnreadable, Count: 1}})
		})
	}
	g, _, _, _ := involvedGateway(t, true)
	g.cfg.HubAcceptsInvolvedSummary = true
	md := requestSide(t, g, "crd-order-select", base)
	assertSummary(t, md, g.cfg.Identity.SignPriv.Public().(ed25519.PublicKey), shnsdk.InvolvedSideRequest, noOmission)
	if md.Involved == "" {
		t.Fatal("control: the readable request names no one")
	}
}

// requestSide runs the request side's involved pass of leg over body, as the
// gateway does once the request's token is minted, and returns the metadata
// it sets: the token, the list and the summary.
func requestSide(t *testing.T, g *Gateway, leg string, body []byte) shnsdk.Metadata {
	t.Helper()
	return requestSideIn(t, g, context.Background(), leg, body)
}

func requestSideIn(t *testing.T, g *Gateway, ctx context.Context, leg string, body []byte) shnsdk.Metadata {
	t.Helper()
	subject := heldPCI(t, "MBR-COVERED")
	tok := signTestToken(shnsdk.Token{Frame: "provider-tpo", Operation: "pas-submit", Subject: subject, CorrelationID: "corr-1", PayloadHash: "hash-1",
		Holder: "provider", Expiry: fakeAuthzNow.Add(time.Hour)}, fakeAuthzKey)
	js, err := tokenJSON(tok)
	if err != nil {
		t.Fatal(err)
	}
	md := shnsdk.Metadata{AuthzToken: js}
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	md.Involved, md.InvolvedSummary = g.involvedRequestSide(ctx, r, leg, "provider-tpo", "pas-submit", "corr-1", "hash-1", body, subject, tok)
	return md
}

// A summary states its own side's omissions alone: patients already counted
// on the call's record, or on another side's tally the context carries, are
// another side's, never this one's. The call's record still counts this side's
// too.
func TestInvolvedSummary_EachSideCountsItsOwnOmissions(t *testing.T) {
	base := []byte(`{"hook":"order-select","context":{"patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-OX"}}}]}}}`)
	// The other side's omissions, on the record and on its own tally.
	outer := func() (context.Context, *exchangeRecorder) {
		x := &exchangeRecorder{}
		x.involvedOmitted(involvedOmitBudget)
		x.involvedOmitted(involvedOmitBudget)
		ctx, tally := withInvolvedTally(context.WithValue(context.Background(), exchangeRecorderKey{}, x))
		tally.add(involvedOmitBudget)
		tally.add(involvedOmitBudget)
		return ctx, x
	}
	t.Run("request", func(t *testing.T) {
		g, authz, _, _ := involvedGateway(t, true)
		g.cfg.HubAcceptsInvolvedSummary = true
		authz.refuse[heldPCI(t, "MBR-OX")] = true
		ctx, x := outer()
		md := requestSideIn(t, g, ctx, "crd-order-select", base)
		assertSummary(t, md, g.cfg.Identity.SignPriv.Public().(ed25519.PublicKey), shnsdk.InvolvedSideRequest,
			[]shnsdk.InvolvedOmission{{Reason: involvedOmitRefused, Count: 1}})
		if x.rec.InvolvedOmitted[involvedOmitRefused] != 1 || x.rec.InvolvedOmitted[involvedOmitBudget] != 2 {
			t.Fatalf("record %v, want both sides' omissions on it", x.rec.InvolvedOmitted)
		}
	})
	t.Run("answer", func(t *testing.T) {
		g, authz, _, _ := involvedGateway(t, true)
		g.cfg.HubAcceptsInvolvedSummary = true
		authz.refuse["pci:held"] = true
		ctx, x := outer()
		md := answerSide(t, g, ctx, "pci:held")
		assertSummary(t, md, g.cfg.Identity.SignPriv.Public().(ed25519.PublicKey), shnsdk.InvolvedSideAnswer,
			[]shnsdk.InvolvedOmission{{Reason: involvedOmitRefused, Count: 1}})
		if x.rec.InvolvedOmitted[involvedOmitRefused] != 1 || x.rec.InvolvedOmitted[involvedOmitBudget] != 2 {
			t.Fatalf("record %v, want both sides' omissions on it", x.rec.InvolvedOmitted)
		}
	})
}

// answerSide builds a pas-claim answer through buildResponseLeg, as the payer
// gateway does, its binding settled with bound (the payer's own binding of the
// member, when it differs; "" for none), and returns the answer envelope's
// metadata.
func answerSide(t *testing.T, g *Gateway, ctx context.Context, bound string) shnsdk.Metadata {
	t.Helper()
	return answerSideAfter(t, g, ctx, func(c *involvedCollector) {
		if bound != "" {
			c.bindingRead(bound, true, true, nil)
			c.differs(bound)
		}
		c.settled()
	})
}

// answerSideAfter is answerSide with the binding's course set by bind, on the
// leg's collector, in place of a settled binding.
func answerSideAfter(t *testing.T, g *Gateway, ctx context.Context, bind func(*involvedCollector)) shnsdk.Metadata {
	t.Helper()
	pub, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	reg := shnsdk.NewRegistry()
	reg.Set("provider-gw", shnsdk.RegistryEntry{ID: "provider-gw", Role: "provider", EncPub: pub, MessageFrames: []string{shnsdk.MessageFrameV1}})
	g.cfg.Reg = reg
	ctx = g.withInvolvedCollector(ctx, "pas-claim")
	if c := involvedCollectorFrom(ctx); c != nil {
		bind(c)
	}
	r := httptest.NewRequest(http.MethodPost, "/inbound", nil).WithContext(ctx)
	frame := g.successFrame(ctx, "provider-gw", statedAnswer{status: http.StatusOK}, "application/fhir+json", "")
	out, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-response", "pas-claim", "corr-1", relayedResponse([]byte(`{"resourceType":"Bundle"}`)),
		answerKey("pas-claim", relay.OutcomeAnswered), frame, heldPCI(t, "MBR-COVERED"), "provider-gw", "")
	if status != 0 || len(out) == 0 {
		t.Fatalf("the answer was not built: %d %q", status, msg)
	}
	env, err := shnsdk.DecodeEnvelope(out)
	if err != nil {
		t.Fatal(err)
	}
	if !g.cfg.HubAcceptsInvolvedSummary || !g.cfg.HubAcceptsInvolved {
		assertNoSummary(t, out)
	}
	return env.Metadata
}

// The answer side, through buildResponseLeg: only a Hub listing both fields
// gets a summary, which verifies with this gateway's holder signing key for
// the answer side and the answer's own token, lists the entries the answer's
// list holds and states each patient left out; every other Hub gets the
// answer it got before. An answer naming no one states listed 0 and no
// omission.
func TestInvolvedSummary_AnswerOnlyForAHubThatReadsOne(t *testing.T) {
	for _, row := range []struct {
		name       string
		bound      string
		refuse     bool
		wantListed int
		omitted    []shnsdk.InvolvedOmission
	}{
		{"its own binding named", "pci:held", false, 1, noOmission},
		{"its own binding left out", "pci:held", true, 0, []shnsdk.InvolvedOmission{{Reason: involvedOmitRefused, Count: 1}}},
		{"no binding of its own", "", false, 0, noOmission},
	} {
		for _, hub := range everyHubReading {
			t.Run(row.name+"/"+hub.name, func(t *testing.T) {
				g, authz, _, _ := involvedGateway(t, hub.involved)
				g.cfg.HubAcceptsInvolvedSummary = hub.summy
				if row.refuse {
					authz.refuse[row.bound] = true
				}
				md := answerSide(t, g, context.Background(), row.bound)
				if listed := len(involvedSubjects(t, md.Involved)); hub.involved && listed != row.wantListed || !hub.involved && md.Involved != "" {
					t.Fatalf("list %q, want %d entries exactly when the Hub lists involved", md.Involved, row.wantListed)
				}
				if hub.both() {
					assertSummary(t, md, g.cfg.Identity.SignPriv.Public().(ed25519.PublicKey), shnsdk.InvolvedSideAnswer, row.omitted)
				}
			})
		}
	}
}

// A summary that cannot be signed is not sent, which the Hub reads as not
// knowing whether the list was complete, and the leg is carried with its list
// as before: through the provider's PAS ingress route as mounted and the
// gateway's real send, at every conformance level, the call's record saying
// why (InvolvedSummaryUnsigned). The answer side's rows through the payer's
// real handlers are TestInvolvedSummary_AnUnsignedAnswerIsRecorded.
func TestInvolvedSummary_ASigningFailureSendsNoSummary(t *testing.T) {
	for _, level := range allLevels {
		t.Run("the request is carried/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.HubAcceptsInvolved, env.originator.cfg.HubAcceptsInvolvedSummary = true, true
			env.originator.signInvolvedSummary = func(shnsdk.InvolvedSummary, ed25519.PrivateKey, shnsdk.Token) (string, error) {
				return "", errors.New("signing failed")
			}
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			env.payerReturns(LegResult{Response: testResponse(levelPASPayerAnswer)})
			// At strict, patient.mixed refuses a request carrying another
			// patient before it is sent: the request there names only its
			// own, and its summary is due all the same.
			body := withCoverageCarrying(t, []byte(pasIngressBundle("00001", "")), pasAnotherPatientCarried["a party naming another patient"])
			want := []string{heldPCI(t, "MBR-NOTCOVERED")}
			if refusesAt(level, RulePatientMixed) {
				body, want = []byte(pasIngressBundle("00001", "")), nil
			}
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RoutePAS)(rec, httptest.NewRequest(http.MethodPost, "/Claim/$submit", bytes.NewReader(body)))
			sent := env.sentRequests()
			if rec.Code >= 300 || len(sent) != 1 {
				t.Fatalf("status %d, %d envelopes sent; want the request carried once", rec.Code, len(sent))
			}
			assertNoSummary(t, sent[0])
			md, _ := shnsdk.DecodeEnvelope(sent[0])
			if got := involvedSubjects(t, md.Metadata.Involved); !slices.Equal(got, want) {
				t.Fatalf("listed %v, want the list sent as before (%v)", got, want)
			}
			if r := got.only(t); r.InvolvedSummaryNotSent != InvolvedSummaryUnsigned {
				t.Fatalf("recorded %q, want %s", r.InvolvedSummaryNotSent, InvolvedSummaryUnsigned)
			}
		})
	}
	t.Run("the answer is built", func(t *testing.T) {
		g, _, _, _ := involvedGateway(t, true)
		g.cfg.HubAcceptsInvolvedSummary = true
		g.signInvolvedSummary = func(shnsdk.InvolvedSummary, ed25519.PrivateKey, shnsdk.Token) (string, error) {
			return "", errors.New("signing failed")
		}
		var events []ObserverEvent
		g.cfg.Observer = func(e ObserverEvent) {
			if e.Kind == InvolvedSummaryNotSentEvent {
				events = append(events, e)
			}
		}
		md := answerSide(t, g, context.Background(), "pci:held")
		if md.InvolvedSummary != "" || len(involvedSubjects(t, md.Involved)) != 1 {
			t.Fatalf("summary %q, list %q; want no summary and the list as before", md.InvolvedSummary, md.Involved)
		}
		if len(events) != 1 || events[0].Detail != InvolvedSummaryUnsigned {
			t.Fatalf("events %+v, want one %s", events, InvolvedSummaryUnsigned)
		}
	})
}

// The largest size, in [0, hi], for which fits holds, fits holding at 0 and
// failing past the edge.
func largestFitting(t *testing.T, hi int, fits func(n int) bool) int {
	t.Helper()
	if !fits(0) {
		t.Fatal("fixture: nothing fits")
	}
	lo := 0
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// A request at the edge of its leg's limit, which fits without a summary, is
// sent without one rather than refused as too large, through the gateway's
// real send; one with room carries its summary.
//
// The drop is on the call's record (InvolvedSummaryOverLimit), at every
// conformance level, one level per top-level test (levelSplitTests), and a
// request with room records none.
func TestInvolvedSummary_ARequestAtItsLimitIsSentWithoutOneAtNone(t *testing.T) {
	requestAtItsLimit(t, EnforcementNone)
}

func TestInvolvedSummary_ARequestAtItsLimitIsSentWithoutOneAtObserve(t *testing.T) {
	requestAtItsLimit(t, EnforcementObserve)
}

func TestInvolvedSummary_ARequestAtItsLimitIsSentWithoutOneAtStructural(t *testing.T) {
	requestAtItsLimit(t, EnforcementStructural)
}

func TestInvolvedSummary_ARequestAtItsLimitIsSentWithoutOneAtStrict(t *testing.T) {
	requestAtItsLimit(t, EnforcementStrict)
}

// requestAtItsLimit is the request's rows at one level. Each send builds its
// own exchange (newInProcessExchange: its own keys, clock, registry, stores
// and Hub stub), and the row's events and record are its own, so the levels
// run in parallel.
func requestAtItsLimit(t *testing.T, level ConformanceEnforcement) {
	t.Parallel()
	const leg = "crd-order-select"
	var events []ObserverEvent
	var x *exchangeRecorder
	send := func(n int, summary bool) ([]byte, error) {
		events = nil
		env := newInProcessExchange(t)
		env.originator.cfg.Observer = func(e ObserverEvent) {
			if e.Kind == InvolvedSummaryNotSentEvent {
				events = append(events, e)
			}
		}
		env.originator.cfg.ConformanceEnforcement = level
		env.originator.cfg.HubAcceptsInvolved, env.originator.cfg.HubAcceptsInvolvedSummary = true, summary
		body := []byte(`{"resourceType":"Parameters","pad":"` + strings.Repeat("a", n) + `"}`)
		x = &exchangeRecorder{}
		ctx := context.WithValue(env.ctx, exchangeRecorderKey{}, x)
		_, err := env.originator.OriginateLeg(ctx, env.req.WithContext(ctx), env.payerID, leg, "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Payload: testRequest(body)})
		sent := env.sentRequests()
		if len(sent) == 0 {
			return nil, err
		}
		return sent[len(sent)-1], nil
	}
	tooLarge := func(err error) bool {
		var e *envelopeTooLargeError
		return errors.As(err, &e)
	}
	edge := largestFitting(t, shnsdk.MaxRequestBytesFor(leg), func(n int) bool {
		_, err := send(n, false)
		return !tooLarge(err)
	})
	raw, err := send(edge, true)
	if raw == nil {
		t.Fatalf("a request that fits without its summary was not sent: %v", err)
	}
	assertNoSummary(t, raw)
	assertOverLimitEvent(t, events, "corr-1")
	if x.rec.InvolvedSummaryNotSent != InvolvedSummaryOverLimit {
		t.Fatalf("recorded %q, want %s", x.rec.InvolvedSummaryNotSent, InvolvedSummaryOverLimit)
	}
	raw, err = send(edge-4<<10, true)
	if raw == nil {
		t.Fatalf("not sent: %v", err)
	}
	if md, _ := shnsdk.DecodeEnvelope(raw); md.Metadata.InvolvedSummary == "" || len(events) != 0 || x.rec.InvolvedSummaryNotSent != "" {
		t.Fatalf("a request with room sent no summary (events %+v, recorded %q)", events, x.rec.InvolvedSummaryNotSent)
	}
}

// An answer at the edge of the limit the Hub reads, which fits without a
// summary, is sent without one rather than refused; one with room carries it.
// The drop is on the call's record (InvolvedSummaryOverLimit), at every
// conformance level: the answer is sealed after its checks, through
// buildResponseLeg. One level per top-level test (levelSplitTests).
func TestInvolvedSummary_AnAnswerAtTheLimitIsSentWithoutOneAtNone(t *testing.T) {
	answerAtTheLimit(t, EnforcementNone)
}

func TestInvolvedSummary_AnAnswerAtTheLimitIsSentWithoutOneAtObserve(t *testing.T) {
	answerAtTheLimit(t, EnforcementObserve)
}

func TestInvolvedSummary_AnAnswerAtTheLimitIsSentWithoutOneAtStructural(t *testing.T) {
	answerAtTheLimit(t, EnforcementStructural)
}

func TestInvolvedSummary_AnAnswerAtTheLimitIsSentWithoutOneAtStrict(t *testing.T) {
	answerAtTheLimit(t, EnforcementStrict)
}

// answerAtTheLimit is the answer's rows at one level. It stays serial: its
// gateway (involvedGateway) reads the package's shared fake clock and
// Framework key (fakeAuthzNow, fakeAuthzKey).
func answerAtTheLimit(t *testing.T, level ConformanceEnforcement) {
	var events []ObserverEvent
	var x *exchangeRecorder
	build := func(n int, summary bool) ([]byte, int) {
		events = nil
		g, _, _, _ := involvedGateway(t, true)
		g.cfg.ConformanceEnforcement = level
		g.cfg.HubAcceptsInvolvedSummary = summary
		g.cfg.Observer = func(e ObserverEvent) {
			if e.Kind == InvolvedSummaryNotSentEvent {
				events = append(events, e)
			}
		}
		pub, _, err := box.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		reg := shnsdk.NewRegistry()
		reg.Set("provider-gw", shnsdk.RegistryEntry{ID: "provider-gw", Role: "provider", EncPub: pub, MessageFrames: []string{shnsdk.MessageFrameV1}})
		g.cfg.Reg = reg
		x = &exchangeRecorder{}
		ctx := g.withInvolvedCollector(context.WithValue(context.Background(), exchangeRecorderKey{}, x), "pas-claim")
		involvedCollectorFrom(ctx).settled()
		r := httptest.NewRequest(http.MethodPost, "/inbound", nil).WithContext(ctx)
		frame := g.successFrame(ctx, "provider-gw", statedAnswer{status: http.StatusOK}, "application/fhir+json", "")
		out, status, _ := g.buildResponseLeg(r, "payer-coverage", "pas-response", "pas-claim", "corr-1", relayedResponse(make([]byte, n)),
			answerKey("pas-claim", relay.OutcomeAnswered), frame, heldPCI(t, "MBR-COVERED"), "provider-gw", "")
		return out, status
	}
	edge := largestFitting(t, shnsdk.MaxResponseBytes, func(n int) bool {
		_, status := build(n, false)
		return status == 0
	})
	out, status := build(edge, true)
	if status != 0 {
		t.Fatalf("an answer that fits without its summary was refused: %d", status)
	}
	assertNoSummary(t, out)
	assertOverLimitEvent(t, events, "corr-1")
	if x.rec.InvolvedSummaryNotSent != InvolvedSummaryOverLimit {
		t.Fatalf("recorded %q, want %s", x.rec.InvolvedSummaryNotSent, InvolvedSummaryOverLimit)
	}
	out, status = build(edge-4<<10, true)
	if status != 0 {
		t.Fatalf("refused: %d", status)
	}
	if md, _ := shnsdk.DecodeEnvelope(out); md.Metadata.InvolvedSummary == "" || len(events) != 0 || x.rec.InvolvedSummaryNotSent != "" {
		t.Fatalf("an answer with room sent no summary (events %+v, recorded %q)", events, x.rec.InvolvedSummaryNotSent)
	}
}

// assertOverLimitEvent checks that the summary's drop was raised once, under
// its fixed reason and the leg's correlation, and nothing else.
func assertOverLimitEvent(t *testing.T, events []ObserverEvent, corr string) {
	t.Helper()
	if len(events) != 1 || events[0].Detail != InvolvedSummaryOverLimit || events[0].CorrelationID != corr {
		t.Fatalf("events %+v, want one %s %s", events, InvolvedSummaryNotSentEvent, InvolvedSummaryOverLimit)
	}
}
