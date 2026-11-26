package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The core's own rows for a gateway's summary of the involved list it sends:
// the envelope's size limit, the signature, the closed list of reasons a
// summary is not sent and the record of it, and the course of a binding the
// answer's summary follows. They run in the core-only build; the rows through
// the payer's and the provider's real handlers are the PA workstream's
// (workstream_pa_involvedsummary_test.go,
// workstream_pa_involvedsummary_binding_test.go).

// recordingContext is a context carrying a fresh exchange recorder, as a
// route's handler gives each call, and the recorder.
func recordingContext() (context.Context, *exchangeRecorder) {
	x := &exchangeRecorder{}
	return context.WithValue(context.Background(), exchangeRecorderKey{}, x), x
}

// A summary never takes an envelope over its limit: the envelope is sent
// without it, as it was before summaries were, and one under its limit keeps
// it. The drop is raised and recorded only when it is what brings the
// envelope within its limit.
func TestInvolvedSummary_NeverTakesAnEnvelopeOverItsLimit(t *testing.T) {
	var events []ObserverEvent
	g := &Gateway{cfg: Config{Clock: time.Now, Observer: func(e ObserverEvent) {
		if e.Kind == InvolvedSummaryNotSentEvent {
			events = append(events, e)
		}
	}}}
	env := shnsdk.Envelope{Metadata: shnsdk.Metadata{TransactionType: "leg-x", AuthzToken: "{}", InvolvedSummary: `{"side":"request"}`}, Ciphertext: make([]byte, 64)}
	with, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	bare := env
	bare.Metadata.InvolvedSummary = ""
	without, err := shnsdk.EncodeEnvelope(bare)
	if err != nil {
		t.Fatal(err)
	}
	ctx, x := recordingContext()
	kept := env
	if got, err := g.withoutOverLimitSummary(ctx, &kept, with, len(with)); err != nil || !bytes.Equal(got, with) || kept.Metadata.InvolvedSummary == "" {
		t.Fatalf("an envelope at its limit lost its summary (%v)", err)
	}
	if x.rec.InvolvedSummaryNotSent != "" {
		t.Fatalf("recorded %q for an envelope that kept its summary", x.rec.InvolvedSummaryNotSent)
	}
	over := env
	if got, err := g.withoutOverLimitSummary(ctx, &over, with, len(with)-1); err != nil || !bytes.Equal(got, without) || over.Metadata.InvolvedSummary != "" {
		t.Fatalf("an envelope the summary takes over its limit kept it (%v)", err)
	}
	if x.rec.InvolvedSummaryNotSent != InvolvedSummaryOverLimit {
		t.Fatalf("recorded %q, want %s", x.rec.InvolvedSummaryNotSent, InvolvedSummaryOverLimit)
	}
	if got, err := g.withoutOverLimitSummary(ctx, &bare, without, len(without)-1); err != nil || !bytes.Equal(got, without) {
		t.Fatalf("an envelope with no summary was changed (%v)", err)
	}
	if len(events) != 1 || events[0].Detail != InvolvedSummaryOverLimit {
		t.Fatalf("events %+v, want one %s, for the envelope the drop brought within its limit", events, InvolvedSummaryOverLimit)
	}
	// An envelope still over its limit without the summary is refused as too
	// large by its caller, as before summaries: the drop is not why, so it is
	// neither raised nor recorded.
	still := env
	events = nil
	ctx, x = recordingContext()
	if got, err := g.withoutOverLimitSummary(ctx, &still, with, len(without)-1); err != nil || len(got) <= len(without)-1 {
		t.Fatalf("an envelope over its limit either way came back %d bytes (%v)", len(got), err)
	}
	if len(events) != 0 || x.rec.InvolvedSummaryNotSent != "" {
		t.Fatalf("events %+v, recorded %q; want neither: the summary is not why the envelope is over its limit", events, x.rec.InvolvedSummaryNotSent)
	}
}

// Every reason this gateway leaves a patient out for is one a summary may
// state (shnsdk.InvolvedSummaryOmitReasons), so the Hub records each as given,
// never as unrecognized.
func TestInvolvedSummary_EveryOmitReasonIsOneASummaryStates(t *testing.T) {
	summary := shnsdk.InvolvedSummaryOmitReasons()
	for _, reason := range InvolvedOmitReasons {
		if !slices.Contains(summary, reason) {
			t.Errorf("%q is not in shnsdk.InvolvedSummaryOmitReasons %v", reason, summary)
		}
	}
}

// A summary is signed only with a signing key and bound only to a token that
// carries a signature: each way that fails makes no summary.
func TestInvolvedSummary_SigningNeedsAKeyAndASignedToken(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signed := shnsdk.Token{Signature: []byte("signature")}
	for name, c := range map[string]struct {
		priv ed25519.PrivateKey
		tok  shnsdk.Token
	}{
		"no signing key":            {nil, signed},
		"a key of the wrong size":   {priv[:16], signed},
		"a token with no signature": {priv, shnsdk.Token{}},
	} {
		if got, err := signInvolvedSummary(shnsdk.InvolvedSummary{Side: shnsdk.InvolvedSideRequest}, c.priv, c.tok); err == nil || got != "" {
			t.Errorf("%s: signed %q (%v), want no summary", name, got, err)
		}
	}
	if got, err := signInvolvedSummary(shnsdk.InvolvedSummary{Side: shnsdk.InvolvedSideRequest}, priv, signed); err != nil || got == "" {
		t.Fatalf("control: not signed: %v", err)
	}
}

// A summary that cannot be made is recorded as unsigned, for a Hub that reads
// summaries, and nothing is recorded for one that does not.
func TestInvolvedSummary_AnUnmadeSummaryIsRecorded(t *testing.T) {
	for _, reads := range []bool{true, false} {
		g := &Gateway{cfg: Config{Clock: time.Now, HubAcceptsInvolved: true, HubAcceptsInvolvedSummary: reads}}
		ctx, x := recordingContext()
		if got := g.involvedSummary(ctx, shnsdk.InvolvedSideRequest, "leg-x", "corr-1", shnsdk.Token{}, "", nil); got != "" {
			t.Fatalf("a summary with no key was made: %q", got)
		}
		want := ""
		if reads {
			want = InvolvedSummaryUnsigned
		}
		if x.rec.InvolvedSummaryNotSent != want {
			t.Fatalf("Hub reads summaries %v: recorded %q, want %q", reads, x.rec.InvolvedSummaryNotSent, want)
		}
	}
}

// The record's reason is one of the closed list, each value once, and the
// first a call gives is kept; a value outside the list is recorded as other.
func TestInvolvedSummary_TheNotSentReasonIsClosed(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range InvolvedSummaryNotSentReasons {
		if seen[v] || v == "" || v == ExchangeOther {
			t.Fatalf("InvolvedSummaryNotSentReasons %v names %q twice, empty or as other", InvolvedSummaryNotSentReasons, v)
		}
		seen[v] = true
	}
	for _, reason := range InvolvedSummaryNotSentReasons {
		_, x := recordingContext()
		x.involvedSummaryNotSent(reason)
		for _, later := range InvolvedSummaryNotSentReasons {
			x.involvedSummaryNotSent(later)
		}
		if x.rec.InvolvedSummaryNotSent != reason {
			t.Fatalf("recorded %q, want the first reason %q", x.rec.InvolvedSummaryNotSent, reason)
		}
	}
	_, x := recordingContext()
	x.involvedSummaryNotSent("a caller's string")
	if x.rec.InvolvedSummaryNotSent != ExchangeOther {
		t.Fatalf("recorded %q, want %s", x.rec.InvolvedSummaryNotSent, ExchangeOther)
	}
	var none *exchangeRecorder
	none.involvedSummaryNotSent(InvolvedSummaryUnsigned)
}

// collectorCourse is a collector's binding outcome after course ran on it.
func collectorCourse(course func(*involvedCollector)) (map[string]int, bool) {
	c := &involvedCollector{held: map[string]bool{}}
	course(c)
	return c.bindingOutcome()
}

// The answer's summary follows its binding's course, as each binding site
// reports it (bindingRead): (a) a binding read that failed is a member left
// out under its reason, so the list is incomplete; (b) one never attempted, a
// member that names no one, or one resolved and not yet on the list, sends no
// summary; (c) one resolved and settled, or one that found no member held, is
// complete.
func TestInvolvedSummary_TheBindingsCourse(t *testing.T) {
	unavailable := &SoRReadError{Kind: SoRUnavailable}
	for _, row := range []struct {
		name   string
		course func(*involvedCollector)
		ok     bool
		failed map[string]int
	}{
		{"(b) never attempted", func(*involvedCollector) {}, false, nil},
		{"(b) a member that names no one", func(c *involvedCollector) { c.bindingRead("", false, false, errMemberNamesNoOne) }, false, nil},
		{"(b) resolved, not yet on the list", func(c *involvedCollector) { c.bindingRead("pci:held", true, true, nil) }, false, nil},
		{"(a) the system of record did not answer", func(c *involvedCollector) { c.bindingRead("", false, false, unavailable) }, true,
			map[string]int{involvedOmitUnreadable: 1}},
		{"(a) the request's patients cannot be read", func(c *involvedCollector) { c.bindingRead("", false, false, errRequestPatientsUnreadable) }, true,
			map[string]int{involvedOmitRequestUnreadable: 1}},
		{"(a) a patient the request cannot identify", func(c *involvedCollector) { c.bindingRead("", false, false, errPatientUnidentifiable) }, true,
			map[string]int{involvedOmitUnknown: 1}},
		{"(a) a member held with no identifier", func(c *involvedCollector) { c.bindingRead("", true, true, nil) }, true,
			map[string]int{involvedOmitUnknown: 1}},
		{"(c) no member held", func(c *involvedCollector) { c.bindingRead("", false, false, nil) }, true, nil},
		{"(c) resolved and settled", func(c *involvedCollector) { c.bindingRead("pci:held", true, true, nil); c.settled() }, true, nil},
		{"(b) a later member left unbound", func(c *involvedCollector) {
			c.bindingRead("pci:held", true, true, nil)
			c.settled()
			c.bindingRead("", false, false, errPatientUnidentifiable)
			c.leftUnbound()
			c.settled()
		}, false, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			failed, ok := collectorCourse(row.course)
			if ok != row.ok || !maps.Equal(failed, row.failed) {
				t.Fatalf("outcome %v %v, want %v %v", failed, ok, row.failed, row.ok)
			}
		})
	}
	var none *involvedCollector
	none.bindingRead("pci:held", true, true, nil)
	none.leftUnbound()
	if _, ok := none.bindingOutcome(); ok {
		t.Fatal("no collector reads as settled")
	}
}

// A leg naming several members binds them in order (bindEachMember): the
// first is the leg's patient, each later one not the token's is named on the
// list, and a refusal stops the batch. Stopped before its last member, the
// list is open, whatever the stopping member's own course, and no summary is
// sent; stopped at its last, that member's course is the summary's.
func TestInvolvedSummary_EachMemberOfALegIsBound(t *testing.T) {
	const token = "pci:token"
	type member struct {
		pci string
		err error // the binding's failure; nil for a binding that resolved
	}
	ok := func(pci string) member { return member{pci: pci} }
	fails := func(err error) member { return member{err: err} }
	for _, row := range []struct {
		name    string
		members []member
		status  int
		settled bool
		failed  map[string]int
		listed  []string
	}{
		{"every member bound", []member{ok(token), ok("pci:b"), ok(token)}, 0, true, nil, []string{"pci:b"}},
		{"the first member fails", []member{fails(errPatientUnidentifiable), ok("pci:b"), ok("pci:c")}, http.StatusBadRequest, false, nil, nil},
		{"a middle member fails", []member{ok(token), fails(errPatientUnidentifiable), ok("pci:c")}, http.StatusBadRequest, false, nil, nil},
		{"the last member fails", []member{ok(token), ok("pci:b"), fails(errPatientUnidentifiable)}, http.StatusBadRequest, true,
			map[string]int{involvedOmitUnknown: 1}, []string{"pci:b"}},
		{"a lone member fails", []member{fails(errPatientUnidentifiable)}, http.StatusBadRequest, true, map[string]int{involvedOmitUnknown: 1}, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			g := &Gateway{cfg: Config{Clock: time.Now}}
			c := &involvedCollector{held: map[string]bool{}}
			ctx := context.WithValue(context.Background(), involvedCollectorKey{}, c)
			var bound []int
			status, _ := g.bindEachMember(ctx, "leg-x", "corr-1", token, len(row.members), func(i int) (string, int, string) {
				bound = append(bound, i)
				m := row.members[i]
				c.bindingRead(m.pci, true, m.err == nil, m.err)
				if m.err != nil {
					return "", http.StatusBadRequest, "refused"
				}
				return m.pci, 0, ""
			})
			if status != row.status {
				t.Fatalf("status %d, want %d", status, row.status)
			}
			if last := bound[len(bound)-1]; status != 0 && row.members[last].err == nil {
				t.Fatalf("stopped at member %d, which bound", last)
			}
			failed, settled := c.bindingOutcome()
			if settled != row.settled || !maps.Equal(failed, row.failed) {
				t.Fatalf("outcome %v %v, want %v %v", failed, settled, row.failed, row.settled)
			}
			var listed []string
			for _, e := range c.list() {
				listed = append(listed, e.pci)
			}
			if !slices.Equal(listed, row.listed) {
				t.Fatalf("listed %v, want %v", listed, row.listed)
			}
		})
	}
}
