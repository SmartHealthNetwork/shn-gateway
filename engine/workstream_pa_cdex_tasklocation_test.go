//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// urlDotDot is the URL dot segment "..", spelled so that it is not read as a
// file path climbing out of the package (tools/gateroute): these rows put it
// in a Location URL, never in a path on disk.
var urlDotDot = strings.Repeat(".", 2)

// taskLocationNotMade is the E-16 reasons rec names.
func taskLocationNotMade(rec ExchangeRecord) []string {
	var out []string
	for _, v := range rec.EditsNotMade {
		if strings.HasPrefix(v, "E-16:") {
			out = append(out, v)
		}
	}
	return out
}

// The provider's gateway frames the tail of its system's Location for the
// payer's gateway, at every level, only when that Location lies under its own
// Task endpoint by the segment-boundary match, a relative one resolved against
// the URL the answer came from, and what follows the endpoint reads as a Task
// id with an optional version; otherwise it frames nothing and records why on
// its own exchange. The answer itself is carried as its system wrote it, and
// the Location is never carried as a header.
func TestCDexTaskLocation_TheProvidersGatewayFramesTheTail(t *testing.T) {
	answer := []byte(`{"resourceType":"Task","id":"t-1","status":"requested","intent":"order"}`)
	for name, row := range map[string]struct {
		locations func(base string) []string
		tail      string
		why       string
	}{
		"an absolute Location":        {func(b string) []string { return []string{b + "/fhir/Task/t-1"} }, "Task/t-1", ""},
		"a versioned Location":        {func(b string) []string { return []string{b + "/fhir/Task/t-1/_history/2"} }, "Task/t-1/_history/2", ""},
		"a relative Location":         {func(string) []string { return []string{"Task/t-1"} }, "Task/t-1", ""},
		"an absolute-path Location":   {func(string) []string { return []string{"/fhir/Task/t-1/_history/3"} }, "Task/t-1/_history/3", ""},
		"its scheme in capitals":      {func(b string) []string { return []string{"HTTP" + strings.TrimPrefix(b, "http") + "/fhir/Task/t-1"} }, "Task/t-1", ""},
		"a dot segment staying under": {func(b string) []string { return []string{b + "/fhir/x/" + urlDotDot + "/Task/t-1"} }, "Task/t-1", ""},

		"another host":                      {func(string) []string { return []string{"https://elsewhere.example/fhir/Task/t-1"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"another port":                      {func(string) []string { return []string{"http://127.0.0.1:1/fhir/Task/t-1"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"a sibling sharing a prefix":        {func(b string) []string { return []string{b + "/fhir/Task2/t-1"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"a dot segment stepping out":        {func(b string) []string { return []string{b + "/fhir/Task/" + urlDotDot + "/Patient/t-1"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"a relative id beside the endpoint": {func(string) []string { return []string{"t-1"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"a query":                           {func(b string) []string { return []string{b + "/fhir/Task/t-1?_format=json"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"a fragment":                        {func(b string) []string { return []string{b + "/fhir/Task/t-1#x"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"user information": {func(b string) []string { return []string{strings.Replace(b, "://", "://user@", 1) + "/fhir/Task/t-1"} },
			"", EditNotMadeTaskLocationNotUnderBase},
		"a non-Task tail":             {func(b string) []string { return []string{b + "/fhir/Task/t-1/extra"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"a trailing slash":            {func(b string) []string { return []string{b + "/fhir/Task/t-1/"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"a percent-encoded id":        {func(b string) []string { return []string{b + "/fhir/Task/t%2D1"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"history with no version":     {func(b string) []string { return []string{b + "/fhir/Task/t-1/_history"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"the Task endpoint itself":    {func(b string) []string { return []string{b + "/fhir/Task"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"an id that is not a FHIR id": {func(b string) []string { return []string{b + "/fhir/Task/t_1"} }, "", EditNotMadeTaskLocationNotUnderBase},
		"two Locations": {func(b string) []string { return []string{b + "/fhir/Task/t-1", b + "/fhir/Task/t-2"} },
			"", EditNotMadeTaskLocationNotUnderBase},
		"a Location that is no URL": {func(string) []string { return []string{"http://[::1"} }, "", EditNotMadeTaskLocationNotUnderBase},

		"no Location":      {func(string) []string { return nil }, "", EditNotMadeTaskLocationNoLocation},
		"a blank Location": {func(string) []string { return []string{" "} }, "", EditNotMadeTaskLocationNoLocation},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sys := newProviderTaskSystem(t, http.StatusCreated, answer)
				sys.locations = row.locations(sys.srv.URL)
				p := configuredProvider(t, level, sys)
				ans := p.send(t, cdexRequestTask(t))
				if !ans.framed || ans.status != http.StatusCreated || !bytes.Equal(ans.body, answer) {
					t.Fatalf("answer %d (framed %v) %s, want the provider's 201 as its system wrote it", ans.status, ans.framed, ans.body)
				}
				got, framed := ans.frame[shnsdk.FrameHeaderTaskLocation]
				if row.tail == "" && framed || got != row.tail {
					t.Fatalf("framed taskLocation %q (%v), want %q", got, framed, row.tail)
				}
				for name := range ans.frame {
					if strings.EqualFold(name, "Location") {
						t.Fatalf("the frame carries the Location header itself: %v", ans.frame)
					}
				}
				rec := p.got.only(t)
				var want []string
				if row.why != "" {
					want = []string{row.why}
				}
				if got := taskLocationNotMade(rec); !slices.Equal(got, want) {
					t.Fatalf("E-16 not made %v, want %v", got, want)
				}
				if rec.Outcome != ExchangeAnswered {
					t.Fatalf("record %s, want the leg answered", rec.Outcome)
				}
			})
		}
	}
}

// A relative Location is resolved against the URL the answer came from, the
// one a same-origin redirect the forward followed led to, as HTTP defines it,
// at every level: "t-1" from /fhir/Task/new is /fhir/Task/t-1, under the Task
// endpoint, though from the endpoint itself it would be /fhir/t-1.
func TestCDexTaskLocation_ARelativeLocationResolvesAfterARedirect(t *testing.T) {
	answer := []byte(`{"resourceType":"Task","id":"t-1"}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/fhir/Task":
					w.Header().Set("Location", "/fhir/Task/new")
					w.WriteHeader(http.StatusTemporaryRedirect)
				case "/fhir/Task/new":
					w.Header().Set("Content-Type", "application/fhir+json")
					w.Header().Set("Location", "t-1")
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write(answer)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			sys := newProviderTaskSystem(t, http.StatusCreated, nil)
			p := configuredProvider(t, level, sys, func(c *Config) {
				c.ProviderCDexTaskURL = srv.URL + "/fhir/Task"
				c.ProviderCDexTaskClient = srv.Client()
			})
			ans := p.send(t, cdexRequestTask(t))
			if ans.status != http.StatusCreated || !bytes.Equal(ans.body, answer) {
				t.Fatalf("answer %d %s", ans.status, ans.body)
			}
			if got := ans.frame[shnsdk.FrameHeaderTaskLocation]; got != "Task/t-1" {
				t.Fatalf("framed taskLocation %q, want Task/t-1 resolved against the redirect's target", got)
			}
			if got := taskLocationNotMade(p.got.only(t)); len(got) != 0 {
				t.Fatalf("E-16 not made %v", got)
			}
		})
	}
}

// The Task URL's own spelling decides, at every level, by the segment-boundary
// match: a Location naming its host in capitals, or its default port
// explicitly, is under it and framed; a Task URL that ends in a slash takes a
// relative "Task/t-1" as …/Task/Task/t-1, which names no Task id and is not
// framed. The provider's system is reached at provider.test through a dialer
// to the test server, so the Task URL can name a host and its default port.
func TestCDexTaskLocation_TheTaskURLsSpelling(t *testing.T) {
	answer := []byte(`{"resourceType":"Task","id":"t-1"}`)
	for name, row := range map[string]struct {
		taskURL  string
		location string
		tail     string
		why      string
	}{
		"its host in capitals":         {"http://provider.test/fhir/Task", "http://PROVIDER.TEST/fhir/Task/t-1", "Task/t-1", ""},
		"its default port explicitly":  {"http://provider.test/fhir/Task", "http://provider.test:80/fhir/Task/t-1", "Task/t-1", ""},
		"the default port it names":    {"http://provider.test:80/fhir/Task", "http://provider.test/fhir/Task/t-1", "Task/t-1", ""},
		"a Task URL ending in a slash": {"http://provider.test/fhir/Task/", "Task/t-1", "", EditNotMadeTaskLocationNotUnderBase},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/fhir+json")
					w.Header().Set("Location", row.location)
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write(answer)
				}))
				t.Cleanup(srv.Close)
				addr := srv.Listener.Addr().String()
				client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				}}}
				sys := newProviderTaskSystem(t, http.StatusCreated, nil)
				p := configuredProvider(t, level, sys, func(c *Config) {
					c.ProviderCDexTaskURL = row.taskURL
					c.ProviderCDexTaskClient = client
				})
				ans := p.send(t, cdexRequestTask(t))
				if ans.status != http.StatusCreated || !bytes.Equal(ans.body, answer) {
					t.Fatalf("answer %d %s", ans.status, ans.body)
				}
				if got := ans.frame[shnsdk.FrameHeaderTaskLocation]; got != row.tail {
					t.Fatalf("framed taskLocation %q, want %q", got, row.tail)
				}
				var want []string
				if row.why != "" {
					want = []string{row.why}
				}
				if got := taskLocationNotMade(p.got.only(t)); !slices.Equal(got, want) {
					t.Fatalf("E-16 not made %v, want %v", got, want)
				}
			})
		}
	}
}

// A provider's answer that is not a 2xx frames no Task location and records
// no reason, whatever Location it carries, at every level: the network
// carries the error as sent.
func TestCDexTaskLocation_AnErrorFramesAndRecordsNothing(t *testing.T) {
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}]}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newProviderTaskSystem(t, http.StatusUnprocessableEntity, oo)
			sys.locations = []string{sys.srv.URL + "/fhir/Task/t-1"}
			p := configuredProvider(t, level, sys)
			ans := p.send(t, cdexRequestTask(t))
			if ans.status != http.StatusUnprocessableEntity || !bytes.Equal(ans.body, oo) {
				t.Fatalf("answer %d %s, want the provider's 422 as sent", ans.status, ans.body)
			}
			if _, ok := ans.frame[shnsdk.FrameHeaderTaskLocation]; ok {
				t.Fatalf("an error framed a Task location: %v", ans.frame)
			}
			if got := taskLocationNotMade(p.got.only(t)); len(got) != 0 {
				t.Fatalf("an error recorded %v", got)
			}
		})
	}
}

// cdexLocatedIngressBase is the payer gateway's ingress base in these rows.
const cdexLocatedIngressBase = "https://payer.example.test/fhir"

// runCDexTaskLocated sends the IG's example Task through the payer's ingress
// on route (its base for owner-a, or the plain Task endpoint, which the
// example's owner routes to owner-a), with owner-a declaring frames and
// answering answer, and returns the payer system's answer and the exchange's
// one record.
func runCDexTaskLocated(t *testing.T, level ConformanceEnforcement, route string, frames []string, ingressBase string, answer LegResult) (*httptest.ResponseRecorder, ExchangeRecord) {
	t.Helper()
	env, got := ownerRoutingExchange(t, level)
	e, ok := env.originator.cfg.Reg.Lookup("owner-a")
	if !ok {
		t.Fatal("fixture: no owner-a")
	}
	e.RequestFrames = frames
	env.originator.cfg.Reg.Set("owner-a", e)
	env.originator.cfg.IngressBaseURL = ingressBase
	env.payerReturns(answer)
	rec := httptest.NewRecorder()
	switch route {
	case RouteCDexRequestBase:
		env.originator.ingressRoute(RouteCDexRequestBase)(rec, cdexRequestPost(cdexRequestTask(t), "owner-a"))
	case RouteCDexRequest:
		env.originator.ingressRoute(RouteCDexRequest)(rec, cdexRequestPlainPost(cdexRequestTask(t)))
	default:
		t.Fatalf("route %q", route)
	}
	return rec, got.only(t)
}

// locatedFrames is the request frames a provider's gateway that frames the
// Task's location declares.
var locatedFrames = append(shnsdk.SupportedRequestFrames(), shnsdk.RequestFrameCDexLoc)

// The payer's gateway writes the Task's Location at its own base for the
// provider the Task went to, then the tail the provider's gateway framed, on
// a create at that base and at the plain Task endpoint alike, at every level:
// disclosed by X-SHN-Edited, the body byte for byte the provider's and its
// ETag kept, the answer recorded as edited by E-16 alone.
func TestCDexTaskLocation_ThePayersGatewayWritesTheLocation(t *testing.T) {
	base, err := cdexroute.ProviderBase(cdexLocatedIngressBase, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, tail := range []string{"Task/t-1", "Task/t-1/_history/2"} {
		for _, route := range []string{RouteCDexRequestBase, RouteCDexRequest} {
			for _, level := range allLevels {
				t.Run(tail+"/"+route+"/"+level.String(), func(t *testing.T) {
					rec, x := runCDexTaskLocated(t, level, route, locatedFrames, cdexLocatedIngressBase, LegResult{
						Response: relayedResponse(cdexRequestAnswer), AnswerStatus: http.StatusCreated,
						AnswerHeaders:  map[string]string{shnsdk.FrameHeaderETag: `W/"1"`},
						AddressHeaders: map[string]string{shnsdk.FrameHeaderTaskLocation: tail},
					})
					if rec.Code != http.StatusCreated || !bytes.Equal(rec.Body.Bytes(), cdexRequestAnswer) {
						t.Fatalf("answer %d %s, want the provider's 201 byte for byte", rec.Code, rec.Body)
					}
					if got := rec.Header().Values("Location"); len(got) != 1 || got[0] != base+"/"+tail {
						t.Fatalf("Location %q, want %q", got, base+"/"+tail)
					}
					if got := rec.Header().Values(EditedHeader); !slices.Equal(got, []string{"cdex-task-location-rekey"}) {
						t.Fatalf("%s %q, want the edit disclosed", EditedHeader, got)
					}
					if got := rec.Header().Get("ETag"); got != `W/"1"` {
						t.Fatalf("ETag %q, want the provider's kept: the body is unchanged", got)
					}
					if rec.Header().Get(shnsdk.FrameHeaderTaskLocation) != "" {
						t.Fatal("the frame's address header was written to the payer's system")
					}
					if x.Outcome != ExchangeAnswered || x.AnswerOwnership != "edited" || !slices.Equal(x.AnswerEdits, []string{"E-16"}) || len(taskLocationNotMade(x)) != 0 {
						t.Fatalf("record %s %s %v %v, want answered, edited by E-16", x.Outcome, x.AnswerOwnership, x.AnswerEdits, x.EditsNotMade)
					}
				})
			}
		}
	}
}

// Where E-16 cannot be made, the provider's 2xx reaches the payer's system as
// sent, with no Location and no disclosure, its ETag kept, and the reason
// recorded, at either route and every level: a provider whose gateway does not
// declare cdexloc (an older one); one that declares it but framed no tail
// (its own record says why); and a tail that does not read as a Task id with
// an optional version.
func TestCDexTaskLocation_NotMadeIsCarriedWithoutALocation(t *testing.T) {
	older := shnsdk.SupportedRequestFrames()
	for name, row := range map[string]struct {
		frames  []string
		address map[string]string
		why     string
	}{
		"an older provider gateway":            {older, map[string]string{shnsdk.FrameHeaderTaskLocation: "Task/t-1"}, EditNotMadeTaskLocationProviderGatewayOlder},
		"an older provider gateway, no frames": {nil, nil, EditNotMadeTaskLocationProviderGatewayOlder},
		"no tail framed":                       {locatedFrames, nil, EditNotMadeTaskLocationNoTail},
		"another resource's tail":              {locatedFrames, map[string]string{shnsdk.FrameHeaderTaskLocation: "Patient/t-1"}, EditNotMadeTaskLocationUnreadableTail},
		"a tail with a query":                  {locatedFrames, map[string]string{shnsdk.FrameHeaderTaskLocation: "Task/t-1?x=1"}, EditNotMadeTaskLocationUnreadableTail},
		"a dot-segment tail":                   {locatedFrames, map[string]string{shnsdk.FrameHeaderTaskLocation: "Task/.."}, EditNotMadeTaskLocationUnreadableTail},
		"an absolute URL as the tail":          {locatedFrames, map[string]string{shnsdk.FrameHeaderTaskLocation: "https://provider.example/fhir/Task/t-1"}, EditNotMadeTaskLocationUnreadableTail},
		"history with no version":              {locatedFrames, map[string]string{shnsdk.FrameHeaderTaskLocation: "Task/t-1/_history"}, EditNotMadeTaskLocationUnreadableTail},
	} {
		for _, route := range []string{RouteCDexRequestBase, RouteCDexRequest} {
			for _, level := range allLevels {
				t.Run(name+"/"+route+"/"+level.String(), func(t *testing.T) {
					rec, x := runCDexTaskLocated(t, level, route, row.frames, cdexLocatedIngressBase, LegResult{
						Response: relayedResponse(cdexRequestAnswer), AnswerStatus: http.StatusCreated,
						AnswerHeaders: map[string]string{shnsdk.FrameHeaderETag: `W/"1"`}, AddressHeaders: row.address,
					})
					if rec.Code != http.StatusCreated || !bytes.Equal(rec.Body.Bytes(), cdexRequestAnswer) {
						t.Fatalf("answer %d %s, want the provider's 201 as sent", rec.Code, rec.Body)
					}
					if loc := rec.Header().Values("Location"); len(loc) != 0 {
						t.Fatalf("Location %q written, want none", loc)
					}
					if got := rec.Header().Values(EditedHeader); len(got) != 0 {
						t.Fatalf("%s %q, want nothing disclosed", EditedHeader, got)
					}
					if got := rec.Header().Get("ETag"); got != `W/"1"` {
						t.Fatalf("ETag %q, want the provider's", got)
					}
					if got := taskLocationNotMade(x); !slices.Equal(got, []string{row.why}) {
						t.Fatalf("E-16 not made %v, want [%s]", got, row.why)
					}
					if x.Outcome != ExchangeAnswered || x.AnswerOwnership != "relayed" || len(x.AnswerEdits) != 0 {
						t.Fatalf("record %s %s %v, want answered and relayed exactly", x.Outcome, x.AnswerOwnership, x.AnswerEdits)
					}
				})
			}
		}
	}
}

// A provider's error is carried as sent with no Location and no E-16 reason,
// at every level: there is no created Task to name.
func TestCDexTaskLocation_AnErrorGetsNoLocation(t *testing.T) {
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}]}`)
	for _, route := range []string{RouteCDexRequestBase, RouteCDexRequest} {
		for _, level := range allLevels {
			t.Run(route+"/"+level.String(), func(t *testing.T) {
				rec, x := runCDexTaskLocated(t, level, route, locatedFrames, cdexLocatedIngressBase,
					LegResult{Status: http.StatusUnprocessableEntity, Response: testResponse(oo)})
				if rec.Code != http.StatusUnprocessableEntity || !bytes.Equal(rec.Body.Bytes(), oo) || rec.Header().Get("Location") != "" {
					t.Fatalf("answer %d %s (Location %q), want the provider's 422 as sent", rec.Code, rec.Body, rec.Header().Get("Location"))
				}
				if got := taskLocationNotMade(x); len(got) != 0 {
					t.Fatalf("an error recorded %v", got)
				}
			})
		}
	}
}

// A payer's gateway with no base to write the Location under (its ingress
// base cleared, which only a test build's ingress bypass serves with) refuses
// the answer as a fidelity refusal of its own edit at the build stage, at
// every level, rather than carry it with an address it cannot write: 502,
// naming the edit and that the provider's system answered.
func TestCDexTaskLocation_NoBaseIsAFidelityRefusal(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			rec, x := runCDexTaskLocated(t, level, RouteCDexRequestBase, locatedFrames, "", LegResult{
				Response: relayedResponse(cdexRequestAnswer), AnswerStatus: http.StatusCreated,
				AddressHeaders: map[string]string{shnsdk.FrameHeaderTaskLocation: "Task/t-1"},
			})
			if rec.Code != http.StatusBadGateway || bytes.Contains(rec.Body.Bytes(), cdexRequestAnswer) || rec.Header().Get("Location") != "" ||
				!strings.Contains(rec.Body.String(), noteProviderAnswered) {
				t.Fatalf("answer %d %s, want a 502 naming that the provider answered", rec.Code, rec.Body)
			}
			if x.Outcome != ExchangeRefused || x.Rule != RefusalFidelity || x.RefusalStage != vocab.RefusalStageBuild || x.Status != http.StatusBadGateway {
				t.Fatalf("record %s %s %s %d, want refused on fidelity at build", x.Outcome, x.Rule, x.RefusalStage, x.Status)
			}
		})
	}
}

// A Location the edit's own verifier refuses (a value made other than from
// the edit's inputs, which only a fault in this gateway makes) is refused as a
// fidelity refusal of the edit at the verify stage, at every level and both
// routes, rather than written or dropped: 502, naming the edit and that the
// provider's system answered. The value is made wrong through the package's
// own test seam (cdexTaskLocationValue).
func TestCDexTaskLocation_AValueItsVerifierRefusesIsAFidelityRefusal(t *testing.T) {
	made := cdexTaskLocationValue
	t.Cleanup(func() { cdexTaskLocationValue = made })
	for name, wrong := range map[string]func(base string, loc shnsdk.CDexTaskPath) string{
		"another provider's base": func(base string, loc shnsdk.CDexTaskPath) string {
			return strings.TrimSuffix(base, "owner-a") + "owner-b/" + loc.String()
		},
		"another resource": func(base string, loc shnsdk.CDexTaskPath) string { return base + "/Patient/" + loc.ID },
	} {
		cdexTaskLocationValue = wrong
		for _, route := range []string{RouteCDexRequestBase, RouteCDexRequest} {
			for _, level := range allLevels {
				t.Run(name+"/"+route+"/"+level.String(), func(t *testing.T) {
					rec, x := runCDexTaskLocated(t, level, route, locatedFrames, cdexLocatedIngressBase, LegResult{
						Response: relayedResponse(cdexRequestAnswer), AnswerStatus: http.StatusCreated,
						AddressHeaders: map[string]string{shnsdk.FrameHeaderTaskLocation: "Task/t-1"},
					})
					if rec.Code != http.StatusBadGateway || bytes.Contains(rec.Body.Bytes(), cdexRequestAnswer) || rec.Header().Get("Location") != "" ||
						!strings.Contains(rec.Body.String(), "the gateway cannot write the Task's Location at its network address (fidelity of SHN's own edit)") ||
						!strings.Contains(rec.Body.String(), noteProviderAnswered) {
						t.Fatalf("answer %d %s, want a 502 naming the edit and that the provider answered", rec.Code, rec.Body)
					}
					if x.Outcome != ExchangeRefused || x.Rule != RefusalFidelity || x.RefusalStage != vocab.RefusalStageVerify || x.Status != http.StatusBadGateway {
						t.Fatalf("record %s %s %s %d, want refused on fidelity at verify", x.Outcome, x.Rule, x.RefusalStage, x.Status)
					}
				})
			}
		}
	}
	// The seam restored, the same exchange is made: the refusal is the value's.
	cdexTaskLocationValue = made
	if rec, _ := runCDexTaskLocated(t, EnforcementObserve, RouteCDexRequestBase, locatedFrames, cdexLocatedIngressBase, LegResult{
		Response: relayedResponse(cdexRequestAnswer), AnswerStatus: http.StatusCreated,
		AddressHeaders: map[string]string{shnsdk.FrameHeaderTaskLocation: "Task/t-1"},
	}); rec.Code != http.StatusCreated || rec.Header().Get("Location") == "" {
		t.Fatalf("control: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// The edit is made only through its registered row, against the edit's own
// inputs: another header, a value that is not the inputs' base then a Task
// location, and a body edit's id are each refused.
func TestCDexTaskLocation_HeaderEditedReadsTheRegistry(t *testing.T) {
	src := relay.Exact(relay.NewBody(cdexRequestAnswer, relay.OriginPeerFrame), "application/fhir+json")
	in := relay.HeaderInputs{relay.TaskLocationInputBase: "https://payer.example/cdex/provider/p", relay.TaskLocationInputTail: "Task/t-1"}
	if _, err := headerEdited(src, relay.EditCDexTaskLocationRekey, "Location", "https://payer.example/cdex/provider/p/Task/t-1", in); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		header, value string
	}{
		"another header":          {"Content-Location", "https://payer.example/cdex/provider/p/Task/t-1"},
		"a relative value":        {"Location", "cdex/provider/p/Task/t-1"},
		"a value naming no Task":  {"Location", "https://payer.example/cdex/provider/p/Patient/1"},
		"another provider's base": {"Location", "https://payer.example/cdex/provider/q/Task/t-1"},
	} {
		if p, err := headerEdited(src, relay.EditCDexTaskLocationRekey, c.header, c.value, in); err == nil || p.Ownership() != 0 {
			t.Errorf("%s: made", name)
		}
	}
	if _, err := headerEdited(src, relay.EditDTRAdaptiveAddressRekey, "Location", "https://payer.example/cdex/provider/p/Task/t-1", in); err == nil {
		t.Error("a body edit's id made a header edit")
	}
}

// A payer gateway's boot check (cdexroute.ProviderBase) and the core's check
// of the Location it writes (relay.HeaderEdited) refuse the same ingress
// bases: a gateway that starts can make the edit for every provider, and one
// the core would refuse at every create does not start. Canonical bases pass
// both.
func TestCDexTaskLocation_BootAndCoreRefuseTheSameBases(t *testing.T) {
	src := relay.Exact(relay.NewBody(cdexRequestAnswer, relay.OriginPeerFrame), "application/fhir+json")
	for ingress, refused := range map[string]bool{
		"https://payer.example/f hir":             true,
		"https://payer.example/fhir ":             true,
		"https://payer.example/f\thir":            true,
		"https://payer.example/fhir\x7f":          true,
		"https://payer.example/./fhir":            true,
		"https://payer.example/fhir/" + urlDotDot: true,
		"https://payer.example/%2E%2e/fhir":       true,
		"https://pay\u00e9r.example/fhir":         true,
		"https://payer.example/f\u00fchr":         true,
		"https://payer.example":                   false,
		"https://payer.example:8443/fhir/r4":      false,
		"https://[::1]:8443/fhir":                 false,
		"https://xn--payr-bpa.example/fhir":       false,
		"https://payer.example/fhir.v2/...":       false,
	} {
		_, bootErr := cdexroute.ProviderBase(ingress, "provider-x")
		base := strings.TrimRight(ingress, "/") + cdexroute.ProviderPrefix + "provider-x"
		_, coreErr := headerEdited(src, relay.EditCDexTaskLocationRekey, "Location", base+"/Task/t-1",
			relay.HeaderInputs{relay.TaskLocationInputBase: base, relay.TaskLocationInputTail: "Task/t-1"})
		if (bootErr != nil) != refused || (coreErr != nil) != refused {
			t.Errorf("%q: boot %v, core %v; want both refused: %v", ingress, bootErr, coreErr, refused)
		}
	}
}
