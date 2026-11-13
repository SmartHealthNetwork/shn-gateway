//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// hubLegBudget is the wait the timed-out rows give the Hub leg: the Hub's
// client alone has it, so the calls to the Authorization Framework and the
// consent service are not cut short.
const hubLegBudget = 2 * time.Second

// hubAnswer is the Hub's answer to the provider's POST /route: status, body
// and HubDeliveredHeader ("" sends none).
type hubAnswer struct {
	status          int
	body, delivered string
}

// hubFault is how a row's POST /route ends: with the Hub's answer, the
// connection lost after the request was written, no answer within the Hub
// leg's budget (after the request was written, or before), or the harness's
// own answer, its envelope rewritten (mutate) or the payer's answer set
// (answer).
type hubFault struct {
	answer  *hubAnswer
	lost    bool
	timeout bool
	unsent  bool
	mutate  func([]byte) []byte
	payer   *LegResult
	// forgedToken signs the answer's token with a key the Framework does
	// not hold: it reads, and fails VerifyBound.
	forgedToken bool
	// restate rewrites the answer envelope's metadata and its token, which
	// is then signed again with the Framework's own key, so the token still
	// verifies against what the envelope states.
	restate func(env *shnsdk.Envelope, tok *shnsdk.Token)
	// requireSharedLine is the provider's own setting refusing an answer
	// stamped with another line than the leg's.
	requireSharedLine bool
}

// install sets f on env's provider gateway, leaving every other call as the
// harness answers it.
func (f hubFault) install(t *testing.T, env *inProcessExchange) {
	base := env.originator.cfg.Client.Transport
	env.substrate.mutateResp = f.mutate
	if f.forgedToken {
		env.corruptResponseToken(t)
	}
	if f.restate != nil {
		env.substrate.mutateResp = func(out []byte) []byte {
			e, err := shnsdk.DecodeEnvelope(out)
			if err != nil {
				t.Fatalf("restate: decode envelope: %v", err)
			}
			var tok shnsdk.Token
			if err := json.Unmarshal([]byte(e.Metadata.AuthzToken), &tok); err != nil {
				t.Fatalf("restate: unmarshal token: %v", err)
			}
			f.restate(&e, &tok)
			b, err := json.Marshal(signTestToken(tok, env.substrate.authzPriv))
			if err != nil {
				t.Fatal(err)
			}
			e.Metadata.AuthzToken = string(b)
			mutated, err := shnsdk.EncodeEnvelope(e)
			if err != nil {
				t.Fatalf("restate: encode envelope: %v", err)
			}
			return mutated
		}
	}
	if f.payer != nil {
		env.payerReturns(*f.payer)
	}
	env.originator.cfg.RequireSharedLine = f.requireSharedLine
	rt := diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/route") {
			return base.RoundTrip(r)
		}
		if !f.unsent && (f.lost || f.timeout) {
			if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteRequest != nil {
				trace.WroteRequest(httptrace.WroteRequestInfo{})
			}
		}
		switch {
		case f.timeout:
			<-r.Context().Done()
			return nil, r.Context().Err()
		case f.lost:
			return nil, io.ErrUnexpectedEOF
		case f.answer != nil:
			h := http.Header{"Content-Type": {"application/json"}}
			if f.answer.delivered != "" {
				h.Set(HubDeliveredHeader, f.answer.delivered)
			}
			return &http.Response{StatusCode: f.answer.status, Body: io.NopCloser(strings.NewReader(f.answer.body)), Header: h}, nil
		}
		return base.RoundTrip(r)
	})
	if f.timeout {
		// Only the Hub's client waits the leg's budget: the Framework's and
		// the consent service's calls keep a client of their own.
		env.originator.cfg.AuthzClient = &http.Client{Transport: base}
		env.originator.cfg.ConsentClient = &http.Client{Transport: base}
		env.originator.cfg.Client = &http.Client{Transport: rt, Timeout: hubLegBudget}
		return
	}
	env.originator.cfg.Client = &http.Client{Transport: rt, Timeout: env.originator.cfg.Client.Timeout}
}

// deliveredEnvelope is a route a requester's system calls, the media type of
// a delivered answer there, and how to call it.
type deliveredEnvelope struct {
	name string
	// mediaType is a delivered answer's media type: application/json on the
	// CDS Hooks route, whose error media type is a full item of the promise;
	// application/problem+json on the /scenario routes, test tooling;
	// application/fhir+json, an OperationOutcome, on a FHIR operation route.
	mediaType string
	call      func(t *testing.T, env *inProcessExchange) *httptest.ResponseRecorder
}

func (e deliveredEnvelope) fhir() bool { return e.mediaType == "application/fhir+json" }

var deliveredEnvelopes = []deliveredEnvelope{
	{"CDS Hooks", "application/json", func(t *testing.T, env *inProcessExchange) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		env.originator.withIngressCorrelation(env.originator.handleCRDIngress)(rec, env.crdIngressRequest(t))
		return rec
	}},
	// The route as the gateway mounts it (routes()), so the mount's writer is
	// the one under test.
	{"scenario", "application/problem+json", func(t *testing.T, env *inProcessExchange) *httptest.ResponseRecorder {
		declareOriginationProfile(env.originator, "demo")
		rec := httptest.NewRecorder()
		env.originator.routes().ServeHTTP(rec, post(env.originator, "/scenario/uc01", `"branch":"covered"`))
		return rec
	}},
	{"PAS $submit", "application/fhir+json", func(t *testing.T, env *inProcessExchange) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		env.originator.withIngressCorrelation(env.originator.handlePASIngress)(rec,
			httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", ""))))
		return rec
	}},
}

// Through the provider's real routes, every way its request can fail after
// the recipient may have received it reaches the provider's system marked so
// by structure, in each envelope: HubDeliveredHeader yes or unknown, with the
// status and text it has always had (a Hub answer that could not be read or
// verified reads that the recipient may have received the request), and the
// problem details members after the {"error"} member (application/json on the
// CDS Hooks route, application/problem+json on the /scenario routes), or on a
// FHIR operation route the OperationOutcome whose issue codes the value in its
// details. An answer the Hub marks not delivered, a wait that ended before the
// request was sent and a carried 2xx answer are as they were.
func TestOrigination_AnAnswerAfterTheRecipientMayHaveReceivedItIsMarkedDelivered(t *testing.T) {
	t.Parallel()
	const check = "check its outcome before resending"
	rows := []struct {
		name      string
		fault     hubFault
		status    int
		delivered deliveredMaybe // "" states no delivery
		text      string
		// textPrefix, when set, is how the text begins, for a text that
		// names the line the route chose.
		textPrefix bool
		// lined says the row needs a leg routed at a contract line, which the
		// /scenario/uc01 eligibility leg is not.
		lined bool
	}{
		{name: "Hub marked yes, answer lost", fault: hubFault{answer: &hubAnswer{502, `{"error":"response audit append failed"}`, "yes"}}, status: 502, delivered: deliveredYes,
			text: "the recipient received this request and answered, but its answer was lost on the way back (response audit append failed); it may have acted on the request: " + check},
		{name: "Hub marked yes, answer too large", fault: hubFault{answer: &hubAnswer{502, `{"error":"` + hubReasonAnswerTooLarge + `"}`, "yes"}}, status: 502, delivered: deliveredYes,
			text: errResponseTooLarge},
		{name: "Hub marked yes, recipient's gateway refused the answer", fault: hubFault{answer: &hubAnswer{502, `{"error":"` + hubReasonRecipientRefusedAnswer + `","rule":"authority"}`, "yes"}}, status: 502, delivered: deliveredYes,
			text: errAnswerRefusedDelivered},
		{name: "Hub marked unknown", fault: hubFault{answer: &hubAnswer{502, `{"error":"forward to recipient failed: the recipient answered 503"}`, "unknown"}}, status: 502, delivered: deliveredUnknown,
			text: "the recipient may have received this request (forward to recipient failed: the recipient answered 503): " + check},
		{name: "unmarked 5xx in front of the Hub", fault: hubFault{answer: &hubAnswer{502, `<html>Bad Gateway</html>`, ""}}, status: 502, delivered: deliveredUnknown,
			text: "the recipient may have received this request (Bad Gateway): " + check},
		{name: "connection lost after the request was written", fault: hubFault{lost: true}, status: 502, delivered: deliveredUnknown,
			text: "the recipient may have received this request (the connection to the Hub failed after the request was sent): " + check},
		{name: "no answer in time after the request was sent", fault: hubFault{timeout: true}, status: 504, delivered: deliveredUnknown,
			text: "no answer on the hub leg within 2s (hub leg timeout); the recipient may have received this request: " + check},
		// A proxy's 200 in front of the Hub is no envelope: nothing says the
		// recipient answered.
		{name: "proxy's HTML 200 in front of the Hub", fault: hubFault{answer: &hubAnswer{200, `<html>ok</html>`, ""}}, status: 502, delivered: deliveredUnknown,
			text: "the recipient may have received this request (the Hub's answer is not an envelope): " + check},
		{name: "answer whose token is signed by another key", fault: hubFault{forgedToken: true}, status: 502, delivered: deliveredUnknown,
			text: "the recipient may have received this request (response leg authorization failed): " + check},
		// A token that verifies against the envelope's own stated sender,
		// another holder than the recipient: nothing says the recipient
		// answered.
		{name: "answer from another sender", fault: hubFault{restate: func(e *shnsdk.Envelope, tok *shnsdk.Token) {
			e.Metadata.Sender, tok.Holder = "payer-b", "payer-b"
		}}, status: 502, delivered: deliveredUnknown,
			text: "the recipient may have received this request (response sender mismatch): " + check},
		// The envelope names another correlation than the leg's, its token
		// still bound to the leg's.
		{name: "answer on another correlation", fault: hubFault{restate: func(e *shnsdk.Envelope, _ *shnsdk.Token) {
			e.Metadata.CorrelationID = "another-correlation"
		}}, status: 502, delivered: deliveredUnknown,
			text: "the recipient may have received this request (response correlation mismatch): " + check},
		{name: "answer whose token cannot be read", fault: hubFault{mutate: func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"authzToken":"`), []byte(`"authzToken":"x`), 1)
		}}, status: 502, delivered: deliveredUnknown,
			text: "the recipient may have received this request (response leg authorization failed): " + check},
		// A verified answer, refused on the provider's own setting for the
		// line its frame states: the recipient answered.
		{name: "verified answer stamped with another line", fault: hubFault{requireSharedLine: true,
			payer: &LegResult{AnswerStatus: http.StatusOK, Response: testResponse([]byte(`{}`)), AddressHeaders: map[string]string{shnsdk.FrameHeaderContractVersion: "pa.other@9.9"}}},
			status: 502, delivered: deliveredYes, textPrefix: true, lined: true,
			text: "the recipient received this request and answered, but its answer could not be accepted (response contract version mismatch: frame declares pa.other@9.9, leg routed "},
		// A verified answer from the recipient that does not decrypt: its
		// ciphertext changed, its token bound again to the changed bytes.
		{name: "verified answer that does not decrypt", fault: hubFault{restate: func(e *shnsdk.Envelope, tok *shnsdk.Token) {
			e.Ciphertext = append([]byte(nil), e.Ciphertext...)
			e.Ciphertext[len(e.Ciphertext)-1] ^= 0xff
			tok.PayloadHash = sha256hex(e.Ciphertext)
		}}, status: 502, delivered: deliveredYes,
			text: "the recipient received this request and answered, but its answer could not be accepted (response decryption failed); it may have acted on the request: " + check},
		// A verified answer whose frame does not decode: the frame's first
		// byte, then nothing a frame holds.
		{name: "verified answer whose frame does not decode", fault: hubFault{
			payer: &LegResult{Response: testResponse([]byte{0x00, 0xff})}},
			status: 502, delivered: deliveredYes,
			text: "the recipient received this request and answered, but its answer could not be accepted (response frame decode failed); it may have acted on the request: " + check},
		// A verified answer whose frame states 204 and carries a body.
		{name: "verified answer whose frame says two things", fault: hubFault{
			payer: &LegResult{AnswerStatus: http.StatusNoContent, Response: testResponse([]byte(`{}`))}},
			status: 502, delivered: deliveredYes,
			text: "the recipient received this request and answered, but its answer could not be accepted (response frame states 204 No Content and carries a body); it may have acted on the request: " + check},
		{name: "Hub marked no", fault: hubFault{answer: &hubAnswer{502, `{"error":"unknown recipient"}`, "no"}}, status: 502,
			text: "hub refused the exchange: unknown recipient"},
		{name: "no answer in time before the request was sent", fault: hubFault{timeout: true, unsent: true}, status: 504,
			text: "no answer on the hub leg within 2s (hub leg timeout)"},
	}
	for _, e := range deliveredEnvelopes {
		for _, row := range rows {
			if row.lined && e.name == "scenario" {
				continue
			}
			t.Run(e.name+"/"+row.name, func(t *testing.T) {
				t.Parallel()
				env := newInProcessExchange(t)
				row.fault.install(t, env)
				rec := e.call(t, env)
				text := row.text
				if row.textPrefix {
					text = deliveredText(t, rec, e.fhir())
					if !strings.HasPrefix(text, row.text) || !strings.HasSuffix(text, "); it may have acted on the request: "+check) {
						t.Fatalf("answer %d %s, want a text beginning %q", rec.Code, rec.Body.String(), row.text)
					}
				}
				switch {
				case row.delivered == "":
					wantNotDelivered(t, rec, e.fhir(), row.status, text)
				case e.fhir():
					wantDeliveredOutcome(t, rec, row.status, row.delivered, text)
				default:
					wantDeliveredProblem(t, rec, row.status, row.delivered, e.mediaType, text)
				}
			})
		}
	}
	// A carried answer is the recipient's, as it was: no delivered mark.
	for _, e := range []deliveredEnvelope{deliveredEnvelopes[0], deliveredEnvelopes[2]} {
		t.Run(e.name+"/carried 2xx", func(t *testing.T) {
			t.Parallel()
			env := newInProcessExchange(t)
			if e.fhir() {
				env.payerReturns(LegResult{Response: testResponse(levelPASPayerAnswer)})
			}
			rec := e.call(t, env)
			if set := rec.Header().Values(HubDeliveredHeader); len(set) > 0 || rec.Code != http.StatusOK || strings.Contains(rec.Header().Get("Content-Type"), "problem") {
				t.Fatalf("carried answer %d delivered=%q %q %.300s, want the 200 unmarked", rec.Code, rec.Header().Get(HubDeliveredHeader), rec.Header().Get("Content-Type"), rec.Body.String())
			}
		})
	}
}

// deliveredText is the text an answer states: its error member, or its
// OperationOutcome issue's diagnostics.
func deliveredText(t *testing.T, rec *httptest.ResponseRecorder, fhir bool) string {
	t.Helper()
	if fhir {
		return readOutcome(t, rec).Issue[0].Diagnostics
	}
	var p struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("answer %s: %v", rec.Body.String(), err)
	}
	return p.Error
}

// stabilityItemClasses reads the class of every item in STABILITY.md's
// inventory tables, in the format test/gatewayfence holds the file to: a
// table under the header "| Item | Class | Owner |" and its separator, one
// item per row, in backticks. An item listed twice keeps every class it is
// given.
func stabilityItemClasses(src string) map[string][]string {
	out := map[string][]string{}
	lines := strings.Split(src, "\n")
	in := false
	for i, l := range lines {
		if !strings.HasPrefix(l, "|") {
			in = false
			continue
		}
		if l == "| Item | Class | Owner |" && i+1 < len(lines) && lines[i+1] == "|---|---|---|" {
			in = true
			continue
		}
		if !in || l == "|---|---|---|" {
			continue
		}
		cells := strings.Split(strings.Trim(l, "|"), "|")
		if len(cells) != 3 {
			continue
		}
		item := strings.TrimSpace(cells[0])
		if len(item) < 3 || item[0] != '`' || item[len(item)-1] != '`' {
			continue
		}
		item = strings.Trim(item, "`")
		out[item] = append(out[item], strings.TrimSpace(cells[1]))
	}
	return out
}

// operatorRouteTierProblems names every operator route (one routes() mounts
// behind withProblemDetails) that STABILITY.md does not class as test tooling
// alone: a delivered answer there is application/problem+json, which only a
// route outside the full promise may take.
func operatorRouteTierProblems(patterns []string, stability string) []string {
	classes := stabilityItemClasses(stability)
	var out []string
	for _, p := range patterns {
		if c := classes[p]; len(c) != 1 || c[0] != "test tooling" {
			out = append(out, fmt.Sprintf("%s is classed %v in STABILITY.md, not test tooling", p, c))
		}
	}
	return out
}

// operatorRoutePatterns are the patterns of every operator route g mounts.
func operatorRoutePatterns(g *Gateway) []string {
	var out []string
	role := g.cfg.Role
	for _, declared := range [][]mountedRoute{g.coreRoutes(role), g.workstreamRoutes(role), g.moduleRoutes(role)} {
		for _, rt := range declared {
			if rt.operator {
				out = append(out, rt.pattern)
			}
		}
	}
	return out
}

// Every operator route, whose delivered answers routes() makes
// application/problem+json, is test tooling in STABILITY.md: an operator
// route that the promise covers would otherwise change its media type
// unrecorded.
func TestOperatorRoutes_AreTestToolingInTheStabilityPromise(t *testing.T) {
	t.Parallel()
	env := newInProcessExchange(t)
	declareOriginationProfile(env.originator, "demo")
	patterns := operatorRoutePatterns(env.originator)
	if len(patterns) < 10 {
		t.Fatalf("%d operator routes mounted, want module 1's /scenario routes: %v", len(patterns), patterns)
	}
	src, err := os.ReadFile("../STABILITY.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range operatorRouteTierProblems(patterns, string(src)) {
		t.Error(p)
	}
}

// The check names an operator route the promise classes otherwise, lists
// twice, or does not list.
func TestOperatorRoutes_TierCheckFires(t *testing.T) {
	t.Parallel()
	const table = "| Item | Class | Owner |\n|---|---|---|\n"
	for name, row := range map[string]struct{ stability, problem string }{
		"classed in the promise": {table + "| `POST /scenario/x` | PA workstream | PA workstream |\n", "POST /scenario/x is classed [PA workstream]"},
		"listed twice":           {table + "| `POST /scenario/x` | test tooling | test tooling |\n| `POST /scenario/x` | core | core |\n", "POST /scenario/x is classed [test tooling core]"},
		"not listed":             {table + "| `POST /scenario/y` | test tooling | test tooling |\n", "POST /scenario/x is classed []"},
		"outside a table":        {"| `POST /scenario/x` | test tooling | test tooling |\n", "POST /scenario/x is classed []"},
	} {
		got := strings.Join(operatorRouteTierProblems([]string{"POST /scenario/x"}, row.stability), "\n")
		if !strings.Contains(got, row.problem) {
			t.Errorf("%s: problems %q, want %q", name, got, row.problem)
		}
	}
	if got := operatorRouteTierProblems([]string{"POST /scenario/x"}, table+"| `POST /scenario/x` | test tooling | test tooling |\n"); len(got) != 0 {
		t.Errorf("a route classed test tooling: problems %v, want none", got)
	}
}

// requestRequiredValidator judges a PAS answer as a PAS 2.1.0 or 2.2.1 lane
// judges it at the line's response Bundle profile, for the one element it
// tests: a ClaimResponse without request is a structural defect there (the
// profile requires it; base FHIR does not), and every other call is valid.
// test/conformance's TestUndeclaredPASAnswerReadsAtItsLineProfile proves that
// verdict on the real lanes.
type requestRequiredValidator struct{}

func (requestRequiredValidator) Validate(_ context.Context, body []byte, profile string) (shnsdk.Result, error) {
	if profile != pasResponseBundleProfile {
		return shnsdk.Result{Valid: true}, nil
	}
	var b struct {
		Entry []struct {
			Resource map[string]any `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return shnsdk.Result{}, err
	}
	for _, e := range b.Entry {
		if _, has := e.Resource["request"]; e.Resource["resourceType"] == "ClaimResponse" && !has {
			return invalid(errIssue("Validation_VAL_Profile_Minimum", []string{"Bundle.entry[0].resource.request"}, "ClaimResponse.request: minimum required = 1")), nil
		}
	}
	return shnsdk.Result{Valid: true}, nil
}

// withoutClaimResponseRequest is a PAS response Bundle with request removed
// from each ClaimResponse in it.
func withoutClaimResponseRequest(t *testing.T, body []byte) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	removed := false
	entries, _ := doc["entry"].([]any)
	for _, e := range entries {
		if r, ok := e.(map[string]any)["resource"].(map[string]any); ok && r["resourceType"] == "ClaimResponse" {
			if _, had := r["request"]; had {
				delete(r, "request")
				removed = true
			}
		}
	}
	if !removed {
		t.Fatal("fixture: the answer has no ClaimResponse.request to remove")
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A payer's PAS answer that declares no profile and lacks ClaimResponse.request,
// which its line's profile requires, is refused at the provider's ingress at
// structural and strict, judged at its line's profile. The payer answered, with
// an answer verified as its own, so the refusal states delivered yes: the 502
// and its text as before, marked by structure. At none and observe it is carried
// exactly, stating no delivery, and so is the same answer with request at strict.
func TestPASIngress_AnUndeclaredAnswerRefusedForItsLineProfileIsDeliveredYes(t *testing.T) {
	t.Parallel()
	leg := carriedAnswerLegs()[0]
	if leg.leg != "pas-claim" {
		t.Fatalf("fixture: the first carried leg is %s, want pas-claim", leg.leg)
	}
	withRequest := leg
	withRequest.answer = func(t *testing.T) []byte { return withoutDeclaredProfiles(t, levelPASPayerAnswer) }
	withoutRequest := leg
	withoutRequest.answer = func(t *testing.T) []byte {
		return withoutDeclaredProfiles(t, withoutClaimResponseRequest(t, levelPASPayerAnswer))
	}
	for _, row := range []struct {
		name    string
		leg     carriedAnswerLeg
		level   ConformanceEnforcement
		refused bool
	}{
		{"no request/none", withoutRequest, EnforcementNone, false},
		{"no request/observe", withoutRequest, EnforcementObserve, false},
		{"no request/structural", withoutRequest, EnforcementStructural, true},
		{"no request/strict", withoutRequest, EnforcementStrict, true},
		{"control: request/strict", withRequest, EnforcementStrict, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, rec, _, xr, answer := carriedAnswerRun(t, row.leg, row.level, "", requestRequiredValidator{})
			if row.refused {
				wantDeliveredOutcome(t, rec, http.StatusBadGateway, deliveredYes, "ingress validation failed")
				wantRefusal(t, xr, http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
				return
			}
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), relayedPASAnswer(t, answer)) || len(rec.Header().Values(HubDeliveredHeader)) > 0 {
				t.Fatalf("answer %d delivered=%q %s, want the payer's answer carried exactly, stating no delivery", rec.Code,
					rec.Header().Get(HubDeliveredHeader), rec.Body)
			}
		})
	}
}

// A payer's questionnaire package answer that this gateway carries across DTR
// lines (E-08, module 2 on) and whose QuestionnaireResponse the registered
// step refuses (two Coverage contexts) is SHN's own edit refusing, after the
// payer answered: delivered yes, at every level, its status and text kept.
func TestDTRIngress_AnAnswerTheLineCarryRefusesIsDeliveredYes(t *testing.T) {
	t.Parallel()
	answer := e08Answer("PackageBundle", []string{"Questionnaire", "QuestionnaireResponse"}, false, "operationOutcome")
	two := bytes.Replace(answer, []byte(`"reference" : "Coverage/c1" } }, `),
		[]byte(`"reference" : "Coverage/c1" } }, { "url" : "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context", "valueReference" : { "reference" : "Coverage/c2" } }, `), 1)
	if bytes.Equal(two, answer) {
		t.Fatal("fixture: no second Coverage context added")
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			r := e08RowTuned(t, level, declares22, two, nil, nil, nil, nil)
			if r.rec.Code < http.StatusBadRequest || !strings.Contains(r.rec.Body.String(), "cannot be carried into this gateway's DTR 2.2") {
				t.Fatalf("answer %d %s, want the line carry's refusal", r.rec.Code, r.rec.Body)
			}
			wantAnsweredRefusalYes(t, r.rec)
		})
	}
}

// A recipient's own error answer that this gateway carries back is the
// recipient's bytes, never edited: the delivery marks go only into bodies the
// gateway builds itself (writeRefusal). After an earlier leg of the call was
// answered, the carried answer gains only the header, delivered yes (the
// mark's floor): the CRD leg's recipient answered and may have acted. Before
// any leg was answered, the same answer is carried with no header at all.
func TestOrigination_ARecipientsOwnErrorIsCarriedExactly(t *testing.T) {
	t.Parallel()
	own := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"not-found","diagnostics":"no such questionnaire <q>"}]}`)
	for name, row := range map[string]struct {
		failing string
		marked  bool
	}{
		"a later leg, after an answered one": {"dtr-questionnaire-fetch", true},
		"the first leg, before any answer":   {"crd-order-select", false},
	} {
		t.Run(name, func(t *testing.T) {
			sor, _ := originSystem(t, prefetchMember)
			g, _ := originGateway(t, "provider-data", sor, nil, func(tx string, _ []byte) (int, []byte) {
				if tx == row.failing {
					return http.StatusNotFound, own
				}
				return 0, payerAnswerFor(t, sor.order, shnsdk.CoveredCovered, shnsdk.PANeededAuthNeeded, "http://example.org/Questionnaire/q")
			})
			rec, _ := originRun(t, g)
			if rec.Code != http.StatusNotFound || !bytes.Equal(rec.Body.Bytes(), own) {
				t.Fatalf("answer %d %s, want the recipient's own 404 exactly\n%s", rec.Code, rec.Body, own)
			}
			if row.marked {
				if got := rec.Header().Values(HubDeliveredHeader); len(got) != 1 || got[0] != "yes" {
					t.Fatalf("delivered=%q, want yes after the answered CRD leg", got)
				}
				return
			}
			wantNoDeliveryStated(t, rec)
		})
	}
}
