//go:build !shn_coreonly

package engine

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// providerTaskSystem is a provider's own Task endpoint: it records each
// request it is sent and answers status with body.
type providerTaskSystem struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	header []http.Header
	status int
	answer []byte
}

func newProviderTaskSystem(t *testing.T, status int, answer []byte) *providerTaskSystem {
	t.Helper()
	s := &providerTaskSystem{status: status, answer: answer}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, b)
		s.header = append(s.header, r.Header.Clone())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(s.status)
		_, _ = w.Write(s.answer)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *providerTaskSystem) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// cdexIngressBase is the provider gateway's ingress base in these rows.
const cdexIngressBase = "https://provider-gw.test"

// payerIdentity is the payer identity the network's registry attests for
// payer-x in these rows: example 19's requester.
var payerIdentity = shnsdk.PayerIdentifier{System: "http://example.org/cdex/payer/payer-ids", Value: "Payer123"}

// configuredProvider is newCDexRequestProvider whose participant sets its own
// Task endpoint (sys), with the ingress base E-11 re-keys under, and payer-x
// registered as a payer that takes the documentation submission, attested as
// payerIdentity.
func configuredProvider(t *testing.T, level ConformanceEnforcement, sys *providerTaskSystem, opts ...func(*Config)) *cdexRequestProvider {
	t.Helper()
	return newCDexRequestProvider(t, level, append([]func(*Config){func(c *Config) {
		c.ProviderCDexTaskURL = sys.srv.URL + "/fhir/Task"
		c.ProviderCDexTaskClient = sys.srv.Client()
		c.IngressBaseURL = cdexIngressBase
		EnableIngressForTest(c)
		e, _ := c.Reg.Lookup("payer-x")
		e.Legs = []string{legCDexSubmit}
		e.PayerIDs = []shnsdk.PayerIdentifier{payerIdentity}
		c.Reg.Set("payer-x", e)
	}}, opts...)...)
}

// example19PayerURL is example 19's payer-url, as the payer's system wrote it.
const example19PayerURL = `"http://example.org/cdex/payer/$submit-attachment"`

// The control, at every level: a provider with its own Task endpoint gets the
// payer's Task exactly as sent but for its payer-url, re-keyed to this
// gateway's return path for the sending payer (E-11) and disclosed by
// X-SHN-Edited, with the payer named outside the body (X-SHN-Requester); the
// provider's 201 comes back to the payer exactly as its system wrote it.
func TestCDexRequest_CarriedToTheProvidersTaskEndpoint(t *testing.T) {
	answer := []byte(`{"resourceType":"Task","id":"t-1","status":"requested","intent":"order"}`)
	ret, err := cdexroute.ReturnBase(cdexIngressBase, "payer-x")
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newProviderTaskSystem(t, http.StatusCreated, answer)
			p := configuredProvider(t, level, sys)
			sent := cdexRequestTask(t)
			ans := p.send(t, sent)
			if !ans.framed || ans.status != http.StatusCreated || !bytes.Equal(ans.body, answer) {
				t.Fatalf("answer %d (framed %v) %s, want the provider's 201 as its system wrote it", ans.status, ans.framed, ans.body)
			}
			if sys.calls() != 1 {
				t.Fatalf("the provider's Task endpoint was called %d times, want once", sys.calls())
			}
			got, h := sys.bodies[0], sys.header[0]
			if !bytes.Contains(got, []byte(`"`+ret+`"`)) {
				t.Fatalf("the forwarded Task names no return path %s:\n%s", ret, got)
			}
			if back := bytes.Replace(got, []byte(`"`+ret+`"`), []byte(example19PayerURL), 1); !bytes.Equal(back, sent) {
				t.Fatalf("the forwarded Task differs from the one sent beyond its payer-url")
			}
			if v := h.Values(EditedHeader); len(v) != 1 || v[0] != "cdex-task-return-rekey" {
				t.Errorf("%s = %q, want the re-key disclosed", EditedHeader, v)
			}
			if v := h.Get(RequesterHeader); v != `payer-x; identifiers="http://example.org/cdex/payer/payer-ids|Payer123"` {
				t.Errorf("%s = %q", RequesterHeader, v)
			}
			if ct := h.Get("Content-Type"); ct != "application/fhir+json" {
				t.Errorf("Content-Type %q, want application/fhir+json", ct)
			}
			if rec := p.got.only(t); rec.Outcome != ExchangeAnswered || rec.Exchange != legCDexRequest {
				t.Fatalf("record %s %s, want the leg answered", rec.Outcome, rec.Exchange)
			}
		})
	}
}

// The provider's own refusal of the Task reaches the payer as its system
// wrote it, at every level: the network carries the answer, it never judges
// it.
func TestCDexRequest_TheProvidersErrorIsRelayedAsSent(t *testing.T) {
	answer := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"not today"}]}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newProviderTaskSystem(t, http.StatusUnprocessableEntity, answer)
			p := configuredProvider(t, level, sys)
			ans := p.send(t, cdexRequestTask(t))
			if !ans.framed || ans.status != http.StatusUnprocessableEntity || !bytes.Equal(ans.body, answer) {
				t.Fatalf("answer %d (framed %v) %s, want the provider's 422 as its system wrote it", ans.status, ans.framed, ans.body)
			}
		})
	}
}

// The provider's gateway refuses a Task only on the network's own primitives,
// as the payer's ingress refuses it, at every level, before its system is
// called: a body that reads two ways (integrity), one that is not a Task or
// carries another code (the leg's authority), and a Task naming no one member
// number (the leg's authority).
func TestCDexRequest_RefusesOnlyOnTheNetworksPrimitives(t *testing.T) {
	for name, c := range map[string]struct {
		body   func(t *testing.T) []byte
		status int
		msg    string
		rule   string
	}{
		"a repeated member": {func(t *testing.T) []byte {
			return cdexRequestEdit(t, `"status" : "requested",`, `"status" : "requested", "status" : "requested",`)
		}, http.StatusBadRequest, errCDexRequestTwoWays, RefusalIntegrity},
		"not JSON":   {func(*testing.T) []byte { return []byte("not json") }, http.StatusForbidden, errCDexRequestNotTask, RefusalAuthority},
		"not a Task": {func(*testing.T) []byte { return []byte(`{"resourceType":"Parameters"}`) }, http.StatusForbidden, errCDexRequestNotTask, RefusalAuthority},
		"no code":    {func(t *testing.T) []byte { return cdexRequestEdit(t, `"code" : {`, `"codeX" : {`) }, http.StatusForbidden, errCDexRequestCode, RefusalAuthority},
		"another code": {func(t *testing.T) []byte {
			return cdexRequestEdit(t, `"code" : "attachment-request-code"`, `"code" : "attachment-request-questionnaire"`)
		}, http.StatusForbidden, errCDexRequestCode, RefusalAuthority},
		"no member number": {func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) { m["for"] = map[string]any{"display": "someone"} })
		}, http.StatusUnprocessableEntity, errCDexRequestNoPatient, RefusalAuthority},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{}`))
				p := configuredProvider(t, level, sys)
				ans := p.send(t, c.body(t))
				if ans.status != c.status || !strings.Contains(string(ans.body), c.msg) {
					t.Fatalf("answer %d %s, want %d %q", ans.status, ans.body, c.status, c.msg)
				}
				if sys.calls() != 0 {
					t.Fatalf("the provider's system was called %d times for a refused Task", sys.calls())
				}
				wantRefusal(t, p.got.only(t), c.status, RefusedByProviderGateway, c.rule)
			})
		}
	}
}

// The provider's gateway binds the Task by its member number exactly as the
// payer's ingress does (TestCDexRequestIngress_APayerSystemMemberNumberDerivesFromTheNumberAlone):
// a member its system does not hold derives from the number alone, so the
// Task and its documentation submission name one patient. Bound so, under a
// token for that patient, the binding differs from nothing; under a token for
// another, the provider's binding is recorded as differing, and the Task is
// carried either way.
func TestCDexRequest_APayerSystemMemberNumberDerivesFromTheNumberAlone(t *testing.T) {
	want := derivedPCI("Member123", "", "")
	for _, level := range allLevels {
		for subject, differs := range map[string]bool{want: false, "pci:another": true} {
			t.Run(level.String()+"/"+subject, func(t *testing.T) {
				sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{"resourceType":"Task"}`))
				var events []ObserverEvent
				var mu sync.Mutex
				p := configuredProvider(t, level, sys, func(c *Config) {
					c.Observer = func(e ObserverEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }
				})
				ans := p.sendWith(t, cdexRequestTask(t), func(_ *shnsdk.Metadata, tok *shnsdk.Token) { tok.Subject = subject })
				if ans.status != http.StatusCreated || sys.calls() != 1 {
					t.Fatalf("answer %d, %d calls: want the Task carried", ans.status, sys.calls())
				}
				mu.Lock()
				defer mu.Unlock()
				got := false
				for _, e := range events {
					got = got || e.Kind == SubjectBindingDiffersEvent
				}
				if got != differs {
					t.Fatalf("binding differs event %v under a token for %s, want %v: the binding is not %s", got, subject, differs, want)
				}
			})
		}
	}
}

// A Task whose requester identifier is not one the network attests for the
// payer that sent it is carried, at every conformance level, and the mismatch
// is an observer event, never a refusal: the Task's own content is the
// payer's, not a network primitive. One it attests, and one naming its
// requester by a reference alone, raise none.
func TestCDexRequest_ARequesterNotTheSendersIsCarriedAndRecorded(t *testing.T) {
	for name, c := range map[string]struct {
		body    func(t *testing.T) []byte
		differs bool
	}{
		"the sender's own":   {cdexRequestTask, false},
		"another identifier": {func(t *testing.T) []byte { return cdexRequestEdit(t, `"value" : "Payer123"`, `"value" : "Payer999"`) }, true},
		"another system": {func(t *testing.T) []byte {
			return cdexRequestEdit(t, `"system" : "http://example.org/cdex/payer/payer-ids"`, `"system" : "http://example.org/other"`)
		}, true},
		"a reference alone": {func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) { m["requester"] = map[string]any{"reference": "Organization/o"} })
		}, false},
		"an unreadable one": {func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) { m["requester"] = map[string]any{"identifier": "x"} })
		}, true},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{"resourceType":"Task"}`))
				var events []ObserverEvent
				var mu sync.Mutex
				p := configuredProvider(t, level, sys, func(c *Config) {
					c.Observer = func(e ObserverEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }
				})
				ans := p.send(t, c.body(t))
				if ans.status != http.StatusCreated || sys.calls() != 1 {
					t.Fatalf("answer %d %s, %d calls: want the Task carried", ans.status, ans.body, sys.calls())
				}
				mu.Lock()
				defer mu.Unlock()
				got := false
				for _, e := range events {
					if e.Kind == CDexRequesterDiffersEvent {
						got = true
						if e.LegType != legCDexRequest || e.Detail != "" {
							t.Errorf("event %+v names more than the leg", e)
						}
					}
				}
				if got != c.differs {
					t.Fatalf("%s raised %v, want %v", CDexRequesterDiffersEvent, got, c.differs)
				}
			})
		}
	}
}

// A re-key the gateway cannot make, as for a payer whose participant id no
// return path can be built for, is refused on the fidelity of SHN's own edit
// at its build, at every level, and the Task never reaches the provider's
// system with its return address off the network.
func TestCDexRequest_ARekeyThatCannotBeMadeIsRefusedOnFidelity(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{}`))
			p := configuredProvider(t, level, sys, func(c *Config) { c.IngressBaseURL = "not a base" })
			ans := p.send(t, cdexRequestTask(t))
			if ans.status != http.StatusUnprocessableEntity || !strings.Contains(string(ans.body), refusalCDexTaskRekey) {
				t.Fatalf("answer %d %s, want the 422 fidelity refusal", ans.status, ans.body)
			}
			if sys.calls() != 0 {
				t.Fatalf("the provider's system was called %d times", sys.calls())
			}
			rec := p.got.only(t)
			wantRefusal(t, rec, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
			if rec.RefusalStage != "build" {
				t.Errorf("refusal stage %q, want build", rec.RefusalStage)
			}
		})
	}
}

// A Task for a payer that does not take the documentation submission is
// carried exactly, no re-key made and none disclosed: its documentation could
// not come back through the network anyway.
func TestCDexRequest_APayerThatTakesNoSubmissionGetsItsTaskCarriedExactly(t *testing.T) {
	sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{"resourceType":"Task"}`))
	p := configuredProvider(t, EnforcementStrict, sys, func(c *Config) {
		e, _ := c.Reg.Lookup("payer-x")
		e.Legs = []string{}
		c.Reg.Set("payer-x", e)
	})
	sent := cdexRequestTask(t)
	if ans := p.send(t, sent); ans.status != http.StatusCreated {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	if !bytes.Equal(sys.bodies[0], sent) || len(sys.header[0].Values(EditedHeader)) != 0 {
		t.Fatalf("the Task was not carried exactly (edited %q)", sys.header[0].Values(EditedHeader))
	}
}

// RequesterHeader's value: the payer's id alone when the registry attests no
// identity for it, and each identity as system|value, space-separated, in a
// quoted-string otherwise, a quote or backslash escaped.
func TestRequesterHeaderValue(t *testing.T) {
	for want, ids := range map[string][]shnsdk.PayerIdentifier{
		"payer-x":                            nil,
		`payer-x; identifiers="s1|v1"`:       {{System: "s1", Value: "v1"}},
		`payer-x; identifiers="s1|v1 s2|v2"`: {{System: "s1", Value: "v1"}, {System: "s2", Value: "v2"}},
		`payer-x; identifiers="s\"q|v\\b"`:   {{System: `s"q`, Value: `v\b`}},
	} {
		if got := requesterHeaderValue("payer-x", ids); got != want {
			t.Errorf("requesterHeaderValue(%v) = %q, want %q", ids, got, want)
		}
	}
}

// A Task whose payer-url the gateway cannot tell how to re-key is carried
// exactly, the reason recorded: a payer-url that is not a valueUrl, and two
// payer-url inputs. The record's editsNotMade names it, and nothing is
// disclosed.
func TestCDexRequest_APayerURLItCannotRekeyIsCarriedExactly(t *testing.T) {
	for name, c := range map[string]struct {
		body func(t *testing.T) []byte
		why  string
	}{
		"a valueString": {func(t *testing.T) []byte {
			return cdexRequestEdit(t, `"valueUrl" : "http://example.org/cdex/payer/$submit-attachment"`, `"valueString" : "http://example.org/cdex/payer/$submit-attachment"`)
		}, EditNotMadeTaskNoStructuralPayerURL},
		"two payer-url inputs": {func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) {
				in := m["input"].([]any)
				for _, x := range in {
					for _, c := range x.(map[string]any)["type"].(map[string]any)["coding"].([]any) {
						if c.(map[string]any)["code"] == "payer-url" {
							m["input"] = append(in, x)
							return
						}
					}
				}
				t.Fatal("fixture: no payer-url input")
			})
		}, EditNotMadeTaskUnreadablePayerURL},
	} {
		t.Run(name, func(t *testing.T) {
			sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{"resourceType":"Task"}`))
			p := configuredProvider(t, EnforcementStrict, sys)
			sent := c.body(t)
			if ans := p.send(t, sent); ans.status != http.StatusCreated {
				t.Fatalf("answer %d %s", ans.status, ans.body)
			}
			if !bytes.Equal(sys.bodies[0], sent) || len(sys.header[0].Values(EditedHeader)) != 0 {
				t.Fatalf("the Task was not carried exactly (edited %q)", sys.header[0].Values(EditedHeader))
			}
			if got := p.got.only(t).EditsNotMade; len(got) != 1 || got[0] != c.why {
				t.Fatalf("editsNotMade %v, want [%s]", got, c.why)
			}
		})
	}
}

// A provider's system that cannot be reached is named as the provider's, at
// every level: the answer every forward to a participant's own system gives,
// for the system the leg's row names as its recipient.
func TestCDexRequest_AProvidersSystemNotReachedIsNamedAsTheProviders(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{}`))
			sys.srv.Close()
			p := configuredProvider(t, level, sys)
			ans := p.send(t, cdexRequestTask(t))
			if ans.status != http.StatusBadGateway || !strings.Contains(string(ans.body), "the provider's system could not be reached") {
				t.Fatalf("answer %d %s, want 502 naming the provider's system", ans.status, ans.body)
			}
			if strings.Contains(string(ans.body), "payer's system") {
				t.Fatalf("the answer names the payer's system: %s", ans.body)
			}
		})
	}
}

// responderParty names the recipient role the leg's row names, and the payer
// for a leg this gateway does not know.
func TestResponderParty(t *testing.T) {
	for leg, want := range map[string]string{legCDexRequest: "provider", "pas-claim": "payer", "no-such-leg": "payer"} {
		if got := responderParty(leg); got != want {
			t.Errorf("responderParty(%q) = %q, want %q", leg, got, want)
		}
	}
}

// The forward carries the leg's id, as every forward to a participant's own
// system does, so the provider's operator can tie the call to its leg.
func TestCDexRequest_TheForwardCarriesTheLegsID(t *testing.T) {
	sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{"resourceType":"Task"}`))
	p := configuredProvider(t, EnforcementStrict, sys)
	ans := p.send(t, cdexRequestTask(t))
	if ans.status != http.StatusCreated {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	if got := sys.header[0].Values(CorrelationHeader); len(got) != 1 || got[0] != ans.corr {
		t.Fatalf("%s = %q, want the leg's id %q", CorrelationHeader, got, ans.corr)
	}
}

// withCDexTaskDeadline shortens the Task forward's deadline for one test.
func withCDexTaskDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	saved := cdexTaskDeadline
	cdexTaskDeadline = d
	t.Cleanup(func() { cdexTaskDeadline = saved })
}

// hungServer is a system that never answers: it holds each request until
// its caller gives up or the test ends.
func hungServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: Close waits on held requests
	return srv
}

// The Task forward is bounded as a payer's forwards are by default: the
// requester's leg budget less five seconds, so the provider gateway's own
// answer reaches the payer inside the payer's budget.
func TestCDexTaskDeadline_InsideTheRequestersBudget(t *testing.T) {
	budget := shnsdk.NewClient().Timeout
	if cdexTaskDeadline != budget-5*time.Second || cdexTaskDeadline <= 0 {
		t.Fatalf("the Task forward's deadline is %s, want the requester's %s budget less 5s", cdexTaskDeadline, budget)
	}
}

// A provider's Task endpoint that never answers is cut off at the forward's
// deadline, and the payer is answered the provider gateway's framed 504
// naming the provider's system, at every level, long before its own budget
// ends.
func TestCDexRequest_AHungTaskEndpointIsAnsweredInsideTheBudget(t *testing.T) {
	withCDexTaskDeadline(t, 300*time.Millisecond)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := &providerTaskSystem{srv: hungServer(t)}
			p := configuredProvider(t, level, sys)
			started := time.Now()
			ans := p.send(t, cdexRequestTask(t))
			if took := time.Since(started); took > 5*time.Second {
				t.Fatalf("answered after %s, want inside the deadline's bound", took)
			}
			if !ans.framed || ans.status != http.StatusGatewayTimeout || !strings.Contains(string(ans.body), "the provider's system received this request but did not answer in time") {
				t.Fatalf("answer %d (framed %v) %s, want the framed 504 naming the provider's system", ans.status, ans.framed, ans.body)
			}
		})
	}
}

// A token endpoint that never answers is inside the same deadline: the
// forward's token request runs under the forward's own context, so the payer
// is answered the provider gateway's framed 504, the Task never sent.
func TestCDexRequest_AHungTokenEndpointIsAnsweredInsideTheBudget(t *testing.T) {
	withCDexTaskDeadline(t, 300*time.Millisecond)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			tokenSrv := hungServer(t)
			sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{"resourceType":"Task"}`))
			hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tokenSrv.URL, ClientID: "provider-gw", Scope: "system/Task.c", ClientSecret: "s", HTTPClient: tokenSrv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			p := configuredProvider(t, level, sys, func(c *Config) { c.ProviderCDexTaskClient = hc })
			started := time.Now()
			ans := p.send(t, cdexRequestTask(t))
			if took := time.Since(started); took > 5*time.Second {
				t.Fatalf("answered after %s, want inside the deadline's bound", took)
			}
			if !ans.framed || ans.status != http.StatusGatewayTimeout || !strings.Contains(string(ans.body), "the provider's system could not be reached in time") {
				t.Fatalf("answer %d (framed %v) %s, want the framed 504 naming the provider's system", ans.status, ans.framed, ans.body)
			}
			if sys.calls() != 0 {
				t.Fatalf("the Task was sent %d times without a token", sys.calls())
			}
		})
	}
}

// The production shape of a hung token endpoint: the deployed token client
// has a timeout of its own shorter than the forward's deadline (10s against
// 25s, gateway/app), so it is cut there first and the payer is answered the
// framed 502 naming the provider's system, inside the budget, not a 504.
func TestCDexRequest_ATokenClientTimeoutShorterThanTheDeadlineIsA502(t *testing.T) {
	withCDexTaskDeadline(t, 2*time.Second)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			tokenSrv := hungServer(t)
			sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{"resourceType":"Task"}`))
			tokenClient := &http.Client{Timeout: 100 * time.Millisecond, Transport: tokenSrv.Client().Transport}
			hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tokenSrv.URL, ClientID: "provider-gw", Scope: "system/Task.c", ClientSecret: "s", HTTPClient: tokenClient})
			if err != nil {
				t.Fatal(err)
			}
			p := configuredProvider(t, level, sys, func(c *Config) { c.ProviderCDexTaskClient = hc })
			started := time.Now()
			ans := p.send(t, cdexRequestTask(t))
			if took := time.Since(started); took > time.Second {
				t.Fatalf("answered after %s, want at the token client's own timeout", took)
			}
			if !ans.framed || ans.status != http.StatusBadGateway || !strings.Contains(string(ans.body), "the provider's system could not be reached") {
				t.Fatalf("answer %d (framed %v) %s, want the framed 502 naming the provider's system", ans.status, ans.framed, ans.body)
			}
			if sys.calls() != 0 {
				t.Fatalf("the Task was sent %d times without a token", sys.calls())
			}
		})
	}
}

// unidentifiableTask is example 19 naming its member by number alone, with
// no Patient of the member's own, and carrying a Coverage whose subscriber is
// another person under that member's id: the number is shared, so it names
// no one patient.
func unidentifiableTask(t *testing.T) []byte {
	t.Helper()
	mb := map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}}
	return cdexRequestMutate(t, func(m map[string]any) {
		m["for"] = map[string]any{"identifier": map[string]any{"type": mb, "system": "http://example.org/cdex/payer/member-ids", "value": "Member123"}}
		var keep []any
		for _, c := range m["contained"].([]any) {
			if c.(map[string]any)["resourceType"] != "Patient" {
				keep = append(keep, c)
			}
		}
		m["contained"] = append(keep, map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
			"subscriber": map[string]any{"reference": "#Member123"}, "beneficiary": map[string]any{"reference": "Patient/other"},
			"contained": []any{map[string]any{"resourceType": "Patient", "id": "Member123",
				"birthDate": "1960-01-01", "name": []any{map[string]any{"family": "Other", "given": []any{"Sub"}}}}}})
	})
}

// A Task whose patient the provider's system cannot identify, its member
// number shared with another person the Task carries, is refused 422 on the
// leg's authority, as the payer's ingress refuses the same Task, at every
// level, before the provider's system is called.
func TestCDexRequest_AnUnidentifiablePatientIsRefused422AsAtThePayersIngress(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{}`))
			p := configuredProvider(t, level, sys)
			ans := p.send(t, unidentifiableTask(t))
			if ans.status != http.StatusUnprocessableEntity || !strings.Contains(string(ans.body), msgPatientUnidentifiable) {
				t.Fatalf("answer %d %s, want 422 %q", ans.status, ans.body, msgPatientUnidentifiable)
			}
			if sys.calls() != 0 {
				t.Fatalf("the provider's system was called %d times", sys.calls())
			}
			wantRefusal(t, p.got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalAuthority)
		})
	}
}
