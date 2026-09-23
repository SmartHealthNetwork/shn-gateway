package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestNativeCapabilityNoValidator(t *testing.T) {
	reg := shnsdk.NewRegistry()
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", BaseURL: "https://payer.example", ContractVersions: []string{"pa.pas@2.2"}})
	g := &Gateway{cfg: Config{Reg: reg}}
	for _, row := range []struct {
		recipient, leg, version string
		want                    bool
	}{
		{"payer", "pas-claim", "pa.pas@2.2", true}, {"payer", "pas-claim", "", true},
		{"missing", "pas-claim", "pa.pas@2.2", false}, {"payer", "typo", "pa.pas@2.2", false},
		{"payer", "pas-claim", "pa.pas@2.0", false}, {"payer", "pas-claim", "pa.crd@2.2", false},
		{"payer", "crd-order-select", "", false},
	} {
		if got := g.canCarryNative(row.recipient, row.leg, row.version); got != row.want {
			t.Errorf("%+v got=%v", row, got)
		}
	}
	reg.Set("bare", shnsdk.RegistryEntry{ID: "bare", Role: "payer", BaseURL: "https://bare.example"})
	if !g.canCarryNative("bare", "pas-claim", "") {
		t.Fatal("registered legacy endpoint lost")
	}
	reg.Set("no-endpoint", shnsdk.RegistryEntry{ID: "no-endpoint", Role: "payer", ContractVersions: []string{"pa.pas@2.2"}})
	if g.canCarryNative("no-endpoint", "pas-claim", "pa.pas@2.2") {
		t.Fatal("missing endpoint accepted")
	}
}

func TestNativeCapabilityUnlanedVersionDeclaration(t *testing.T) {
	g := &Gateway{cfg: Config{ValidatorsByLine: map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()}}}
	body := []byte("opaque participant bytes")
	got, version, status, msg := g.unframeRequest("pas-claim", framedRequest(t, "pa.pas@2.2", body))
	if status != 0 || version != "pa.pas@2.2" || !bytes.Equal(got, body) {
		t.Fatalf("status=%d version=%s body=%s msg=%s", status, version, got, msg)
	}
}

func TestRequestVersionDeclarationDoesNotBorrowProfile(t *testing.T) {
	if got := requestVersion(Content{ProfileID: "pa.pas@2.2"}); got != "" {
		t.Fatalf("fabricated declaration %q", got)
	}
}

func TestNativeVersionDeclarationPair(t *testing.T) {
	for _, version := range []string{"", "pa.pas@2.2"} {
		t.Run(version, func(t *testing.T) {
			pair := newInProcessExchange(t)
			g := pair.originator
			g.cfg.ConformanceEnforcement = EnforcementNone
			g.cfg.Validator = nil
			g.cfg.ValidatorsByLine = nil
			proofCalls := 0
			g.cfg.AdaptationValidator = func(string, string) shnsdk.Validator {
				proofCalls++
				return certificationValidatorFunc(func(context.Context, []byte, string) (shnsdk.Result, error) {
					return shnsdk.Result{}, errors.New("transform checker down")
				})
			}
			entry, _ := g.cfg.Reg.Lookup("payer")
			entry.ContractVersions = []string{"pa.pas@2.0"}
			entry.RequestFrames = []string{shnsdk.RequestFrameV1}
			g.cfg.Reg.Set("payer", entry)
			body := []byte("actual producer bytes")
			framed, err := shnsdk.EncodeHTTPFrameHeaders(200, map[string]string{"Content-Type": "application/opaque", shnsdk.FrameHeaderContractVersion: version}, body)
			if err != nil {
				t.Fatal(err)
			}
			pair.payerReturns(LegResult{Response: relay.Exact(relay.NewBody(framed, relay.OriginPeerFrame), "application/opaque")})
			reply, err := g.OriginateLegMessage(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "payer", "pas-claim", "pci", "corr", "", Content{WorkstreamType: workstreamPA, Carried: true, ProfileID: "pa.pas@2.0", DeclaredVersion: "pa.pas@2.0", Payload: testRequest([]byte("request bytes"))})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := reply.bytes("pas-claim")
			if err != nil || !bytes.Equal(raw, body) || reply.DeclaredVersion != version || pair.routeHitCount() != 1 || proofCalls != 0 {
				t.Fatalf("reply=%+v raw=%s routes=%d err=%v", reply, raw, pair.routeHitCount(), err)
			}
			ctx := withFindingContext(context.Background(), findingContext{LegType: "pas-claim", CorrelationID: "explicit-transform", Seam: "originate", Whose: "own"})
			if status, _ := g.validateFHIREgressOrBridged(ctx, []byte(`{"resourceType":"Bundle"}`), "pa.pas", "2.2", true); status != http.StatusServiceUnavailable || proofCalls != 1 {
				t.Fatal("explicit transform passed without its required checker")
			}
			if !g.canCarryNative("payer", "pas-claim", "pa.pas@2.0") {
				t.Fatal("down transform disabled independent native endpoint")
			}
		})
	}
}

func TestNativeEndpointVersionDeclaration(t *testing.T) {
	for _, declared := range [][]string{nil, {"pa.pas@2.2"}, {"pa.pas@2.0", "pa.pas@2.2"}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("actual backend")) }))
		n := NewNativeResponder(srv.Client(), srv.URL, "", nil, nil, WithDeclaredContractVersions(declared))
		result, err := n.Handle(withAnswerLine(context.Background(), "pa.pas@2.0"), "pas-claim", "corr", "pci", []byte("opaque"))
		srv.Close()
		if err != nil || result.Status != 0 || result.ResponseContractVersion != "" || result.ResponseVersionSource != "" {
			t.Fatalf("declaration=%v result=%+v err=%v", declared, result, err)
		}
	}
}

// PCV-15: receive capability and HRex's per-version endpoint describe where
// to send a request. They do not attest what that HTTP backend returns.
func TestNativeReceiveCapabilityCannotStampIndependentReply(t *testing.T) {
	const answer = "exact producer PAS 2.2 answer"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Claim/$submit" || r.Method != http.MethodPost {
			t.Errorf("backend route %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/participant-answer")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(answer))
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, binding, want, source string
	}{
		{"receive only", "", "", ""},
		{"independent output binding", responseBinding("pas-submit", server.URL+"/Claim/$submit", "pa.pas@2.2"), "pa.pas@2.2", "configured-endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var declarations NativeResponseDeclarations
			if tc.binding != "" {
				var err error
				declarations, err = ParseNativeResponseDeclarations("[" + tc.binding + "]")
				if err != nil {
					t.Fatal(err)
				}
			}
			n := NewNativeResponder(server.Client(), server.URL, "", nil, nil,
				WithDeclaredContractVersions([]string{"pa.pas@2.0"}), WithNativeResponseDeclarations(declarations))
			n.SetEndpointEvidence(map[string]string{"pa.pas@2.0": server.URL + "/Claim/$submit"})
			ctx := withAnswerLine(context.WithValue(context.Background(), nativeExchangeKey{}, ExchangeContext{contractVersion: "pa.pas@2.0"}), "pa.pas@2.0")
			result, err := n.Handle(ctx, "pas-claim", "corr", "pci", []byte("exact request"))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := relay.Transmit(result.Response, relay.Check(answerKey("pas-claim", relay.OutcomeAnswered)))
			if err != nil || result.ApplicationStatus != http.StatusAccepted || !bytes.Equal(raw, []byte(answer)) || result.Response.ContentType() != "application/participant-answer" || result.ResponseContractVersion != tc.want || result.ResponseVersionSource != tc.source {
				t.Fatalf("status=%d body=%q media=%q version=%q source=%q err=%v", result.ApplicationStatus, raw, result.Response.ContentType(), result.ResponseContractVersion, result.ResponseVersionSource, err)
			}
		})
	}
}

func TestNativeMissingOutputDeclarationHasNoOptionalCertificate(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			validator := syntheticLineValidator("2.0")
			g := &Gateway{cfg: Config{HolderID: "provider", ConformanceEnforcement: level,
				ValidatorsByLine: map[string]shnsdk.Validator{"2.0": validator}}}
			in := CheckInput{Exchange: ExchangeContext{legType: "pas-claim-inquire", contractVersion: "pa.pas@2.0", policy: NewConformancePolicy(level)},
				Direction: "response", Status: http.StatusOK, Body: []byte(`{"resourceType":"Parameters"}`)}
			err := g.enforceContent(context.Background(), in)
			if level == EnforcementNone {
				if err != nil || len(validator.Calls()) != 0 {
					t.Fatalf("none should carry exact answer without optional certification: err=%v calls=%v", err, validator.Calls())
				}
				return
			}
			var ce *conformanceError
			if !errors.As(err, &ce) || ce.Rule != "fhir.profile" || ce.status != http.StatusServiceUnavailable || len(validator.Calls()) != 0 {
				t.Fatalf("strict missing producer declaration=%v; calls=%v", err, validator.Calls())
			}
		})
	}
}

func TestNativeVersionDeclarationPolicy(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		g := &Gateway{cfg: Config{HolderID: "provider", ConformanceEnforcement: level}}
		in := CheckInput{Exchange: ExchangeContext{legType: "pas-claim-inquire", contractVersion: "pa.pas@2.0", policy: NewConformancePolicy(level)}, Direction: "response", Status: 200, DeclaredVersion: "pa.pas@2.2", Body: []byte(`{"resourceType":"Parameters"}`)}
		err := g.enforceContent(context.Background(), in)
		if level == EnforcementStrict {
			var ce *conformanceError
			if !errors.As(err, &ce) || ce.Rule != "version.consistency" || ce.status != 502 {
				t.Fatalf("strict mismatch=%v", err)
			}
			in.Exchange.contractVersion = "pa.pas@2.2"
			if err := g.enforceContent(context.Background(), in); err != nil {
				t.Fatalf("independent checker-free inquiry refused: %v", err)
			}
			in.Exchange.legType = "pas-claim"
			in.Body = []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`)
			if err := g.enforceContent(context.Background(), in); err == nil {
				t.Fatal("strict missing checker passed")
			}
		} else if err != nil {
			t.Fatalf("%s metadata mismatch refused: %v", level, err)
		}
	}
}

func TestNativeVersionDeclarationOldPeerNoRetry(t *testing.T) {
	for _, framed := range []bool{false, true} {
		pair := newTransportExchange(t)
		entry, _ := pair.originator.cfg.Reg.Lookup("payer")
		entry.RequestFrames = nil
		if framed {
			entry.RequestFrames = []string{shnsdk.RequestFrameV1}
		}
		pair.originator.cfg.Reg.Set("payer", entry)
		answer := []byte("old peer refuses an unlaned representation")
		if framed {
			pair.payerReturns(LegResult{Status: 422, Response: relay.Exact(relay.NewBody(answer, relay.OriginUpstreamResponse), "application/fhir+json")})
		} else {
			sealBare(pair, answer)
		}
		reply, err := pair.originator.OriginateLegMessage(pair.ctx, pair.req, "payer", "pas-claim", "pci", "old-peer", "", Content{WorkstreamType: workstreamPA, Carried: true, DeclaredVersion: "pa.pas@2.2", Payload: testRequest([]byte("native request"))})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := reply.bytes("pas-claim")
		if err != nil || !bytes.Equal(raw, answer) || pair.routeHitCount() != 1 || shnsdk.IsFramed(pair.lastRequestPayload()) != framed {
			t.Fatalf("old peer result=%+v err=%v routes=%d", reply, err, pair.routeHitCount())
		}
		if framed && reply.Status != 422 {
			t.Fatal("old peer refusal replaced")
		}
	}
}

// Evidence refresh is synchronized with the backend, not a sleep or a data race:
// neither an old nor a new receive endpoint may assert an output declaration.
func TestNativeEndpointVersionDeclarationSnapshot(t *testing.T) {
	for _, row := range []struct {
		name               string
		initial, refreshed []string
		want               string
	}{
		{"replace unique", []string{"pa.pas@2.0"}, []string{"pa.pas@2.2"}, ""},
		{"clear unique", []string{"pa.pas@2.0"}, nil, ""},
		{"make unique ambiguous", []string{"pa.pas@2.0"}, []string{"pa.pas@2.0", "pa.pas@2.2"}, ""},
		{"absent stays absent", nil, []string{"pa.pas@2.2"}, ""},
		{"ambiguous stays absent", []string{"pa.pas@2.0", "pa.pas@2.2"}, []string{"pa.pas@2.2"}, ""},
	} {
		for _, status := range []int{http.StatusAccepted, http.StatusUnprocessableEntity} {
			t.Run(fmt.Sprintf("%s/%d", row.name, status), func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				request, answer := []byte("exact participant request"), []byte("exact backend answer")
				path := "/Claim/$submit"
				if len(row.initial) > 0 {
					path = "/line-endpoint"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if r.URL.Path != path || !bytes.Equal(body, request) {
						t.Errorf("dispatched path=%s body=%s", r.URL.Path, body)
					}
					close(started)
					<-release
					w.Header().Set("Content-Type", "application/participant-answer")
					w.WriteHeader(status)
					_, _ = w.Write(answer)
				}))
				defer server.Close()
				defer unblock()
				n := NewNativeResponder(server.Client(), server.URL, "", nil, nil)
				evidence := func(tokens []string) map[string]string {
					m := map[string]string{}
					for _, token := range tokens {
						m[token] = server.URL + path
					}
					return m
				}
				n.SetEndpointEvidence(evidence(row.initial))
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				type outcome struct {
					result LegResult
					err    error
				}
				finished := make(chan outcome, 1)
				go func() {
					res, err := n.Handle(withAnswerLine(ctx, "pa.pas@2.0"), "pas-claim", "corr", "pci", request)
					finished <- outcome{res, err}
				}()
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("backend not reached", ctx.Err())
				}
				refreshed := make(chan struct{})
				go func() { n.SetEndpointEvidence(evidence(row.refreshed)); close(refreshed) }()
				select {
				case <-refreshed:
				case <-ctx.Done():
					t.Fatal("evidence mutex held over backend I/O", ctx.Err())
				}
				unblock()
				var got outcome
				select {
				case got = <-finished:
				case <-ctx.Done():
					t.Fatal("request did not complete", ctx.Err())
				}
				if got.err != nil {
					t.Fatal(got.err)
				}
				result := got.result
				actualStatus := result.ApplicationStatus
				if result.Status != 0 {
					actualStatus = result.Status
				}
				outcomeKey := relay.OutcomeAnswered
				if status/100 != 2 {
					outcomeKey = relay.OutcomeUpstreamError
				}
				raw, err := relay.Transmit(result.Response, relay.Check(answerKey("pas-claim", outcomeKey)))
				source := ""
				if row.want != "" {
					source = "endpoint"
				}
				if err != nil || actualStatus != status || !bytes.Equal(raw, answer) || result.Response.ContentType() != "application/participant-answer" || result.ResponseContractVersion != row.want || result.ResponseVersionSource != source {
					t.Fatalf("status=%d raw=%s declaration=%q source=%q want=%q err=%v", actualStatus, raw, result.ResponseContractVersion, result.ResponseVersionSource, row.want, err)
				}
			})
		}
	}
}

// Every forwarding family enters the same snapshot boundary. CRD's published
// evidence must not replace its independently selected CDS catalog route.
func TestNativeEndpointDispatchRoutes(t *testing.T) {
	for _, row := range []struct{ leg, contract, operation string }{
		{"pas-claim", "pa.pas", ""}, {"pas-claim-update", "pa.pas", ""}, {"pas-claim-inquire", "pa.pas", ""},
		{"dtr-questionnaire-fetch", "pa.dtr", shnsdk.FrameOperationQuestionnairePackage},
		{"dtr-questionnaire-fetch", "pa.dtr", shnsdk.FrameOperationNextQuestion},
		{"crd-order-select", "pa.crd", ""},
	} {
		t.Run(row.leg+"/"+row.operation, func(t *testing.T) {
			request := []byte("participant bytes")
			path, wantVersion := "/line-endpoint", ""
			if row.leg == "pas-claim-inquire" {
				path, wantVersion = "/Claim/$inquire", ""
			}
			if row.operation == shnsdk.FrameOperationNextQuestion {
				path, wantVersion = "/Questionnaire/$next-question", ""
			}
			if row.contract == "pa.crd" {
				request = cdsRequest("order-select")
				path = "/cds-services/svc"
				wantVersion = ""
			}
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(map[string]any{"services": stubCDSServices})
					return
				}
				posts++
				body, _ := io.ReadAll(r.Body)
				if r.URL.Path != path || !bytes.Equal(body, request) {
					t.Errorf("path=%s body=%s", r.URL.Path, body)
				}
				w.Header().Set("Content-Type", "application/participant-answer")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte("exact answer"))
			}))
			defer server.Close()
			n := NewNativeResponder(server.Client(), server.URL, "svc", nil, nil)
			n.SetEndpointEvidence(map[string]string{row.contract + "@2.0": server.URL + "/line-endpoint"})
			ctx := withAnswerLine(context.Background(), row.contract+"@2.0")
			if row.operation != "" {
				ctx = withRequestFrameOperation(ctx, row.operation)
			}
			res, err := n.Handle(ctx, row.leg, "corr", "pci", request)
			if err != nil || res.Status != 0 || res.ApplicationStatus != 202 || res.ResponseContractVersion != wantVersion || posts != 1 {
				t.Fatalf("result=%+v posts=%d err=%v", res, posts, err)
			}
		})
	}
}

// PCV-15: a native builder for the peer's declared line does not need a
// validator lane. Strict enforcement is a later content boundary.
func TestFreshNativeReachWithoutOptionalValidators(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		for _, family := range []struct{ contract, leg string }{{"pa.pas", "pas-claim"}, {"pa.dtr", "dtr-questionnaire-fetch"}, {"pa.crd", "crd-order-select"}} {
			reg := shnsdk.NewRegistry()
			reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", ContractVersions: []string{family.contract + "@2.1", family.contract + "@2.2"}})
			g := &Gateway{cfg: Config{Reg: reg, DeclaredContractVersions: []string{family.contract + "@2.0"}, ConformanceEnforcement: level}}
			route, err := g.selectLegRoute("payer", family.leg)
			if err != nil || route.Token != family.contract+"@2.2" || route.BuildLine != "2.2" || len(route.Chain) != 0 {
				t.Fatalf("%v/%s native route %+v %v", level, family.contract, route, err)
			}
			g.cfg.EgressNativeLines = []string{"2.0"}
			if route, err := g.selectLegRoute("payer", family.leg); err == nil {
				t.Fatalf("missing native builder and transformation evidence produced route %+v", route)
			}
		}
	}
}
