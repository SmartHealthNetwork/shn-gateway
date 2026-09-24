package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// failingValidator always errors: a validator outage, not a conformance
// verdict.
type failingValidator struct{}

func (failingValidator) Validate(context.Context, []byte, string) (shnsdk.Result, error) {
	return shnsdk.Result{}, errors.New("boom")
}

func findingGateway(t *testing.T, v shnsdk.Validator) (*Gateway, *[]ObserverEvent, *bytes.Buffer) {
	t.Helper()
	events := &[]ObserverEvent{}
	g := &Gateway{cfg: Config{
		Clock:                  func() time.Time { return time.Unix(0, 0).UTC() },
		Validator:              v,
		Observer:               func(e ObserverEvent) { *events = append(*events, e) },
		ConformanceEnforcement: EnforcementStrict,
	}}
	logged := &bytes.Buffer{}
	log.SetOutput(logged)
	t.Cleanup(func() { observationFlush(t, g); g.Close(); log.SetOutput(os.Stderr) })
	return g, events, logged
}

// An invalid verdict emits one finding carrying every field the leg context
// supplies, and refuses with the validator's issues in the message.
func TestValidateGovernedInvalidEmitsFindingAndRefuses(t *testing.T) {
	const marker = "REJECTED-MARKER"
	g, events, logged := findingGateway(t, &shnsdk.FakeValidator{RejectIfContains: marker})
	ctx := withFindingContext(context.Background(), findingContext{
		LegType: "crd-order-select", CorrelationID: "corr-7", Seam: "provider-ingress", Whose: "peer",
	})

	status, msg := g.validateFHIR(ctx, []byte(`{"resourceType":"Coverage","id":"`+marker+`"}`), "ingress", "")

	if status != http.StatusUnprocessableEntity {
		t.Fatalf("an invalid verdict must refuse 422 today, got %d %q", status, msg)
	}
	if !strings.Contains(msg, "ingress validation failed") {
		t.Fatalf("the refusal keeps its message, got %q", msg)
	}
	if !strings.Contains(msg, "fhir.profile") || strings.Contains(msg, marker) {
		t.Fatalf("the refusal must name the safe rule without diagnostic text, got %q", msg)
	}
	var finding *ObserverEvent
	observationFlush(t, g)
	for i := range *events {
		if (*events)[i].Kind == ConformanceObservedEvent {
			finding = &(*events)[i]
		}
	}
	if finding == nil {
		t.Fatal("an invalid verdict must emit a conformance finding")
	}
	for _, want := range []string{
		`"kind":"fhir-ingress"`, `"legType":"crd-order-select"`, `"correlationId":"corr-7"`,
		`"seam":"provider-ingress"`, `"whose":"peer"`, `"action":"refused"`,
	} {
		if !strings.Contains(finding.Detail, want) {
			t.Fatalf("finding missing %s: %s", want, finding.Detail)
		}
	}
	if strings.Contains(finding.Detail, marker) || strings.Contains(logged.String(), `"issues":["`+marker) {
		t.Fatalf("a finding must never carry diagnostic text: %s", finding.Detail)
	}
}

// A valid verdict is silent: no finding, no refusal.
func TestValidateGovernedValidEmitsNothing(t *testing.T) {
	g, events, _ := findingGateway(t, syntheticFakeValidator())
	status, _ := g.validateFHIR(context.Background(), []byte(`{"resourceType":"Coverage"}`), "ingress", "")
	if status != 0 {
		t.Fatalf("a valid verdict must not refuse, got %d", status)
	}
	observationFlush(t, g)
	for _, e := range *events {
		if e.Kind == ConformanceObservedEvent {
			t.Fatal("a valid verdict must emit no conformance finding")
		}
	}
}

// Validator outage and an unlaned contract line are outages, not findings:
// their errors are unchanged and no finding is emitted.
func TestValidateGovernedOutageIsUnavailableFinding(t *testing.T) {
	g, events, _ := findingGateway(t, nil)
	status, msg := g.validateFHIR(context.Background(), []byte(`{}`), "egress", "2.0")
	if status != 503 || !strings.Contains(msg, "fhir.profile") {
		t.Fatalf("outage = %d %q", status, msg)
	}
	found := false
	observationFlush(t, g)
	for _, e := range *events {
		if e.Kind == ConformanceObservedEvent {
			var f ConformanceFinding
			_ = json.Unmarshal([]byte(e.Detail), &f)
			if f.Rule != "fhir.profile" || f.State != CheckUnavailable || f.Action != "refused" {
				t.Fatalf("finding %+v", f)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing unavailable finding")
	}
}

// A validate made outside any handler still produces a finding, labelled
// unknown: an absent context never suppresses.
func TestValidateGovernedWithoutContextStillEmits(t *testing.T) {
	const marker = "REJECTED-MARKER"
	g, events, _ := findingGateway(t, &shnsdk.FakeValidator{RejectIfContains: marker})
	g.validateFHIR(context.Background(), []byte(`{"id":"`+marker+`"}`), "egress", "")
	var found bool
	observationFlush(t, g)
	for _, e := range *events {
		if e.Kind == ConformanceObservedEvent && strings.Contains(e.Detail, `"legType":"unknown"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("a validate with no finding context must still emit, labelled unknown")
	}
}

// refusal() appends the bounded issues to the message with "; " between them,
// so a refused sender learns WHAT was wrong.
func TestGovResultRefusalJoinsBoundedIssues(t *testing.T) {
	r := govResult{Status: http.StatusUnprocessableEntity, Msg: "ingress validation failed", Issues: []string{"issue one", "issue two"}}
	status, msg := r.refusal()
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status must pass through unchanged, got %d", status)
	}
	if msg != "ingress validation failed: issue one; issue two" {
		t.Fatalf("refusal must join bounded issues onto the message, got %q", msg)
	}
}

// A result carrying no issues (an outage, or a status-0 pass-through) leaves
// the message untouched.
func TestGovResultRefusalWithoutIssuesLeavesMsgUnchanged(t *testing.T) {
	r := govResult{Status: http.StatusInternalServerError, Msg: "validator unavailable"}
	status, msg := r.refusal()
	if status != http.StatusInternalServerError || msg != "validator unavailable" {
		t.Fatalf("a result with no issues must pass through unchanged, got %d %q", status, msg)
	}
}

// ---- The five sites that used to call a validator directly, bypassing the choke point ----

// fakeValidatorIssue is the exact diagnostic shnsdk.FakeValidator{RejectIfContains: marker}
// reports (sdk/fhirvalidate.go: `"fake: contains " + f.RejectIfContains`) — the one issue
// every migrated site below sees, pinned here once rather than re-typed at each case.
func fakeValidatorIssue(_ string) string { return "fhir.profile" }

// wantJSONErrorBody asserts a migrated HTTP site's response body decodes to EXACTLY
// {"error": wantError} plus, when wantIssues is non-nil, an "issues" array equal to it.
// Two of the five sites (the eligibility response-ingress 502 and the payer egress 500)
// no longer write a literal at all after this task's migration — they write
// govResult.Msg, composed as `dir + " validation failed"` inside validateGoverned
// (gateway.go). That string matches each site's own pre-migration literal today, but
// nothing coupled the two before this test: renaming validateGoverned's message would
// silently rewrite these bodies with every other gate in the repo green. This is the
// coupling a status-only test misses.
func wantJSONErrorBody(wantError string, wantIssues []string) func(t *testing.T, body string) {
	return func(t *testing.T, body string) {
		t.Helper()
		var got struct {
			Error  string   `json:"error"`
			Issues []string `json:"issues,omitempty"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("body did not decode as JSON: %v (body=%q)", err, body)
		}
		if got.Error != wantError {
			t.Errorf("body error = %q, want %q", got.Error, wantError)
		}
		if wantIssues == nil {
			if len(got.Issues) != 0 {
				t.Errorf("body issues = %v, want none (this site never echoed them)", got.Issues)
			}
			return
		}
		if !slices.Equal(got.Issues, wantIssues) {
			t.Errorf("body issues = %v, want %v", got.Issues, wantIssues)
		}
	}
}

func wantSafeConformanceRefusal(category, rule, direction string) func(t *testing.T, body string) {
	return func(t *testing.T, body string) {
		t.Helper()
		var got map[string]string
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("body did not decode as JSON: %v (body=%q)", err, body)
		}
		want := map[string]string{
			"category":  category,
			"rule":      rule,
			"gateway":   "provider",
			"level":     "strict",
			"direction": direction,
		}
		if !maps.Equal(got, want) {
			t.Fatalf("safe refusal = %#v, want %#v", got, want)
		}
	}
}

// wantRawMessage is DELETED with its only caller, the "pas assembly" row: it
// asserted validatePASResult's bare (status, msg) pair, which no migrated site
// returns any more (see drivePASAssembly's deletion note below).

// Each migrated site keeps the exact status AND body it wrote before — an eligibility
// ingress refusal is 422 at the payer's inbound leg but 502 at the requester's
// response read — and each now emits a finding whose
// identity (Kind, Direction, LegType) is the choke point's, not just "some event fired".
func TestMigratedBypassSitesKeepTheirStatusAndEmit(t *testing.T) {
	const marker = "REJECTED-MARKER"
	issue := fakeValidatorIssue(marker)
	for _, tc := range []struct {
		name        string
		wantStatus  int
		wantLegType string // findingContextFrom(ctx) at the call site under test
		drive       func(t *testing.T, marker string) (int, string, []ObserverEvent)
		assertBody  func(t *testing.T, body string)
	}{
		{
			name: "uc01 eligibility egress", wantStatus: http.StatusUnprocessableEntity,
			wantLegType: "coverage-eligibility", drive: driveUC01EligibilityEgress,
			assertBody: wantJSONErrorBody("egress validation failed", []string{issue}),
		},
		{
			name: "eligibility response ingress", wantStatus: http.StatusBadGateway,
			wantLegType: "coverage-eligibility", drive: driveEligibilityResponseIngress,
			assertBody: wantSafeConformanceRefusal("conformance_invalid", "fhir.profile", "response"),
		},
		{
			name: "payer eligibility ingress", wantStatus: http.StatusUnprocessableEntity,
			wantLegType: "coverage-eligibility", drive: drivePayerEligibilityIngress,
			assertBody: wantJSONErrorBody("ingress validation failed", []string{issue}),
		},
		{
			name: "payer eligibility egress", wantStatus: http.StatusInternalServerError,
			wantLegType: "coverage-eligibility", drive: drivePayerEligibilityEgress,
			assertBody: wantJSONErrorBody("egress validation failed", nil),
		},
		// The fifth row, "pas assembly", is gone: the inquiry-leg change deleted the
		// terminal-response assembly and with it that migrated site's
		// 422/"PAS assembly validation failed" pair (see drivePASAssembly's
		// deletion note below).
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, events := tc.drive(t, marker)
			if status != tc.wantStatus {
				t.Fatalf("migrating the site must not change its status: got %d want %d", status, tc.wantStatus)
			}
			tc.assertBody(t, body)

			var found *ObserverEvent
			for i := range events {
				if events[i].Kind == ConformanceObservedEvent {
					found = &events[i]
				}
			}
			if found == nil {
				t.Fatal("a migrated site must emit its finding through the choke point")
			}
			if found.Direction != "validate" {
				t.Errorf("finding Direction = %q, want %q", found.Direction, "validate")
			}
			if found.LegType != tc.wantLegType {
				t.Errorf("finding LegType = %q, want %q", found.LegType, tc.wantLegType)
			}
			var f ConformanceFinding
			if err := json.Unmarshal([]byte(found.Detail), &f); err != nil {
				t.Fatal(err)
			}
			if f.Rule != "fhir.profile" || f.State != CheckInvalid || f.Action != "refused" ||
				f.Level != "strict" || f.PayloadSHA256 == "" {
				t.Errorf("migrated invalidity lost its strict refusal metadata: %+v", f)
			}
		})
	}
}

// driveUC01EligibilityEgress drives handleScenario (UC-01) so its OWN
// egress $validate of the built CoverageEligibilityRequest fails — before any
// Hub round trip is even attempted. marker rides the provider's NPI
// (Config.NPI), which eligibilityRequest stamps into the request's
// Practitioner reference verbatim; a real NPI is never validator-visible
// text, so this is the one field this site's egress bytes carry that a test
// can steer without touching the member the census fixture must resolve.
func driveUC01EligibilityEgress(t *testing.T, marker string) (int, string, []ObserverEvent) {
	t.Helper()
	var events []ObserverEvent
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = EnforcementStrict
	env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	env.originator.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: marker}
	env.originator.cfg.NPI = marker
	rec := httptest.NewRecorder()
	env.originator.handleScenario(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc01", strings.NewReader(`{"branch":"covered"}`)))
	observationFlush(t, env.originator)
	return rec.Code, rec.Body.String(), events
}

// driveEligibilityResponseIngress drives handleScenario (UC-01) all the way
// through a real Hub round trip (newInProcessExchange's relaySubstrate, which
// seals back whatever LegResult a test configures for ANY leg type — including
// the version-neutral coverage-eligibility leg pendResumeSubstrate does not
// model) so the SECOND UC-01 site — ingress-validating the payer's decrypted
// answer — is what fails. The request's own bytes stay marker-free (egress
// above must pass first); the canned payer response the substrate seals back
// carries the marker instead, so only the response-ingress check can reject.
func driveEligibilityResponseIngress(t *testing.T, marker string) (int, string, []ObserverEvent) {
	t.Helper()
	var events []ObserverEvent
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = EnforcementStrict
	env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	env.originator.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: marker}
	crrJSON := []byte(`{"resourceType":"CoverageEligibilityResponse","marker":"` + marker + `"}`)
	env.payerReturns(LegResult{Response: testResponse(crrJSON)})
	rec := httptest.NewRecorder()
	env.originator.handleScenario(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc01", strings.NewReader(`{"branch":"covered"}`)))
	observationFlush(t, env.originator)
	return rec.Code, rec.Body.String(), events
}

// buildEligibilityInboundRequest builds a real, fully-signed inbound
// coverage-eligibility envelope for MBR-COVERED addressed to gw, past
// verifyHubAssertion and VerifyBound — the shape a real Hub forward to
// handleInbound arrives in. newPendResumeFixture does not expose its own
// authz/hub key material (it is a local var inside the constructor), so this
// generates and OVERRIDES gw's AuthzPub/HubTransportPub with fresh
// test-controlled keys, the same direct-field-override pattern the worked
// example above uses for Observer/Validator.
func buildEligibilityInboundRequest(t *testing.T, gw *Gateway, cerJSON []byte, correlationID string) *http.Request {
	t.Helper()
	hubPub, hubPriv := genED25519(t)
	authzPub, authzPriv := genED25519(t)
	gw.cfg.HubTransportPub = hubPub
	gw.cfg.AuthzPub = authzPub

	const member = "MBR-COVERED"
	pci, _, found := gw.cfg.SoR.ResolvePatient(member)
	if !found {
		t.Fatalf("fixture bug: %s not resolvable in the stub SoR", member)
	}

	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: "requester", Recipient: gw.cfg.HolderID, TransactionType: "coverage-eligibility",
		AuthorityFrame: "provider-tpo", Timestamp: gw.cfg.Clock().Format(time.RFC3339),
		CorrelationID: correlationID,
	}, cerJSON, gw.cfg.Identity.EncPub)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tok := signTestToken(shnsdk.Token{
		Operation: "eligibility-inquiry", Subject: pci, Frame: "provider-tpo",
		Holder: "requester", CorrelationID: correlationID, Expiry: gw.cfg.Clock().Add(time.Hour),
		PayloadHash: sha256hexT(env.Ciphertext),
	}, authzPriv)
	tokBytes, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	env.Metadata.AuthzToken = string(tokBytes)
	body, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}

	as := shnsdk.IssueAssertion("hub", gw.cfg.HolderID, hubPriv, gw.cfg.Clock(), time.Minute)
	raw, err := json.Marshal(as)
	if err != nil {
		t.Fatalf("marshal assertion: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", bytes.NewReader(body))
	r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(raw))
	return r
}

// eligibilityInboundRequest is a valid inbound coverage-eligibility envelope
// for MBR-COVERED whose REQUEST bytes carry marker — driving the payer's
// INGRESS $validate (of the peer's own request) to reject.
func eligibilityInboundRequest(t *testing.T, gw *Gateway, marker string) *http.Request {
	t.Helper()
	cerJSON := []byte(`{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],` +
		`"patient":{"reference":"Patient/MBR-COVERED"},"created":"2026-01-01T00:00:00Z",` +
		`"insurer":{"reference":"Organization/payer"},"provider":{"reference":"Practitioner/1234567890"},` +
		`"marker":"` + marker + `"}`)
	return buildEligibilityInboundRequest(t, gw, cerJSON, "corr-elig-ingress-1")
}

// eligibilityInboundRequestClean is a valid inbound coverage-eligibility
// envelope for MBR-COVERED whose REQUEST bytes carry no marker (so the
// ingress check passes) — the correlationID itself carries marker instead,
// and BuildEligibilityResponse always stamps it verbatim into the built
// response's own Request reference ("CoverageEligibilityRequest/req-<corr>"),
// so only the payer's EGRESS check (its own built response) can reject.
func eligibilityInboundRequestClean(t *testing.T, gw *Gateway, marker string) *http.Request {
	t.Helper()
	cerJSON := []byte(`{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["benefits"],` +
		`"patient":{"reference":"Patient/MBR-COVERED"},"created":"2026-01-01T00:00:00Z",` +
		`"insurer":{"reference":"Organization/payer"},"provider":{"reference":"Practitioner/1234567890"}}`)
	return buildEligibilityInboundRequest(t, gw, cerJSON, "corr-"+marker)
}

// drivePayerEligibilityIngress drives the payer's real handleInbound
// dispatch (Hub-assertion verification, envelope decode, token binding, the
// findingContext tag handleInbound itself sets) into handleEligibilityInbound,
// with the inbound REQUEST bytes carrying marker.
func drivePayerEligibilityIngress(t *testing.T, marker string) (int, string, []ObserverEvent) {
	t.Helper()
	var events []ObserverEvent
	gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-COVERED"})
	gw.cfg.ConformanceEnforcement = EnforcementStrict
	gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	gw.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: marker}
	rec := httptest.NewRecorder()
	gw.handleInbound(rec, eligibilityInboundRequest(t, gw, marker))
	observationFlush(t, gw)
	return rec.Code, rec.Body.String(), events
}

// drivePayerEligibilityEgress is drivePayerEligibilityIngress's twin for the
// SECOND validate in handleEligibilityInbound — this payer's own built
// CoverageEligibilityResponse — via eligibilityInboundRequestClean, whose
// request bytes stay marker-free so only the egress check can reject.
func drivePayerEligibilityEgress(t *testing.T, marker string) (int, string, []ObserverEvent) {
	t.Helper()
	var events []ObserverEvent
	gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-COVERED"})
	gw.cfg.ConformanceEnforcement = EnforcementStrict
	gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	gw.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: marker}
	rec := httptest.NewRecorder()
	gw.handleInbound(rec, eligibilityInboundRequestClean(t, gw, marker))
	observationFlush(t, gw)
	return rec.Code, rec.Body.String(), events
}

// drivePASAssembly is DELETED. It drove validatePASResult's local-assembly
// arm — the `422 PAS assembly validation failed` site whose bytes were the
// terminal-response assembly's output. That change removed the assembly itself
// (assembleTerminalPASBundle, LegResult.ResponseAssembled, and the separate
// validate call that certified SHN's copy of a payer's decision), so the
// migrated bypass site this helper and its "pas assembly" table row pinned no
// longer exists. validatePASResult's one remaining arm is the ordinary
// validateFHIRForContract wrapper, covered by TestPASResultCertification
// (pasretention_test.go) and by TestPinnedFindingContext_InboundEgress /
// _InboundUpdateEgress, which pin its production finding tag.

// At none an invalid verdict relays: no refusal, and the finding says so. This
// is the none twin of TestValidateGovernedInvalidEmitsFindingAndRefuses.
func TestValidateGovernedAtNoneSkips(t *testing.T) {
	g, events, _ := findingGateway(t, failIfCalledValidator{})
	g.cfg.ConformanceEnforcement = EnforcementNone
	status, msg := g.validateFHIR(context.Background(), []byte(`{"id":"REJECTED-MARKER"}`), "ingress", "")
	observationFlush(t, g)
	if status != 0 || msg != "" || len(*events) != 0 {
		t.Fatalf("none: %d %q %+v", status, msg, *events)
	}
}

// TestValidateGovernedFindingPinsFullFieldSet pins EVERY field validateGoverned's
// emitFinding call fills, at its own ONE construction site (gateway.go). A
// reviewer mutating that literal one field at a time — Level hardwired to
// "none", the `if bridged { whose = "network" }` override deleted, Direction
// deleted, Line/Profile deleted, PayloadSHA256 deleted — previously left the
// whole engine/adversarial/invariant/harness/nativeforward suite green: no
// test anywhere compared the finding's fields against what the call site
// actually had in hand. Level matters most: the cloud console's
// configured-versus-observed disagreement badge, and the compensating control
// for hosted tenants the invariant cannot see, both rest on finding.Level
// being the RUNNING gateway's real level, not a constant. Whose is second: the
// operator console's Whose column must never label SHN's own bridged edit
// (prime directive 3) as the peer's data.
//
// Case 1 pins an ordinary (unbridged) strict-level finding's full field set,
// including Level == the gateway's actual configured level ("strict") — this
// alone catches the hardwired-"none" mutation regardless of which level a
// test happens to run at. Case 2 pins a bridged finding's full field set at
// EnforcementNone specifically: it proves (a) Level still reads the true
// running level ("none", not hardwired) and (b) Whose reads "network"
// because kindForDirection/the bridged override fired, not because the
// level forced a refusal — a bridged check refuses at every level, so this
// case is never able to piggyback strict's own refusal behaviour to look
// right by accident.
func TestValidateGovernedFindingPinsFullFieldSet(t *testing.T) {
	const marker = "REJECTED-MARKER"
	resourceJSON := []byte(`{"resourceType":"QuestionnaireResponse","id":"` + marker + `"}`)
	wantSHA := sha256hex(resourceJSON)

	for _, tc := range []struct {
		name      string
		level     ConformanceEnforcement
		bridged   bool
		dir       string
		line      string
		profile   string
		wantKind  string
		wantWhose string
	}{
		{
			name: "strict, unbridged peer check", level: EnforcementStrict, bridged: false,
			dir: "ingress", line: "2.0", profile: baseQRProfile,
			wantKind: "fhir-ingress", wantWhose: "peer",
		},
		{
			name: "none, bridged check", level: EnforcementNone, bridged: true,
			// dir/line/profile mirror validateFHIREgressOrBridged's own call shape
			// (gateway.go:1445-1448): every real bridged check is "egress" with no
			// profile.
			dir: "egress", line: "2.1", profile: "",
			wantKind: "fhir-bridged", wantWhose: "network",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, events, _ := findingGateway(t, nil)
			g.cfg.ConformanceEnforcement = tc.level
			ctx := withFindingContext(context.Background(), findingContext{
				LegType: "crd-order-select", CorrelationID: "corr-full-1",
				Seam: "provider-ingress", Whose: "peer",
			})

			gr := g.validateGoverned(ctx, findingContextFrom(ctx), &shnsdk.FakeValidator{RejectIfContains: marker},
				resourceJSON, tc.dir, tc.line, tc.profile, tc.bridged)
			if gr.Status == 0 {
				t.Fatal("fixture bug: expected a refusal (a bridged check always refuses; the strict case must too)")
			}

			var finding *ObserverEvent
			observationFlush(t, g)
			for i := range *events {
				if (*events)[i].Kind == ConformanceObservedEvent {
					finding = &(*events)[i]
				}
			}
			if finding == nil {
				t.Fatal("no finding emitted")
			}
			var got ConformanceFinding
			if err := json.Unmarshal([]byte(finding.Detail), &got); err != nil {
				t.Fatalf("finding.Detail did not decode: %v (%s)", err, finding.Detail)
			}
			// Issues' redaction shape is TestValidateGovernedInvalidEmitsFindingAndRefuses's
			// concern, not this test's — every OTHER field is compared explicitly (not
			// via a struct-literal ==, which []string makes uncompilable anyway).
			for _, row := range [][3]string{
				{"Kind", got.Kind, tc.wantKind},
				{"Direction", got.Direction, tc.dir},
				{"LegType", got.LegType, "crd-order-select"},
				{"CorrelationID", got.CorrelationID, "corr-full-1"},
				{"Seam", got.Seam, "provider-ingress"},
				{"Whose", got.Whose, tc.wantWhose},
				{"Line", got.Line, tc.line},
				{"Profile", got.Profile, tc.profile},
				{"Level", got.Level, tc.level.String()},
				{"Action", got.Action, "refused"}, {"Decision", got.Decision, ""}, {"RuleSet", got.RuleSet, ConformanceRuleSet}, {"State", string(got.State), string(CheckInvalid)},
				{"PayloadSHA256", got.PayloadSHA256, wantSHA},
			} {
				field, gotVal, wantVal := row[0], row[1], row[2]
				if gotVal != wantVal {
					t.Errorf("finding.%s = %q, want %q", field, gotVal, wantVal)
				}
			}
		})
	}
}

// An outage is an outage at both levels: identical error, no finding.
func TestValidateGovernedOutageModes(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		g, events, _ := findingGateway(t, &failingValidator{})
		g.cfg.ConformanceEnforcement = level
		status, msg := g.validateFHIR(context.Background(), []byte(`{}`), "ingress", "")
		if level != EnforcementStrict {
			observationFlush(t, g)
			if status != 0 || len(*events) != 0 {
				t.Fatalf("%s: %d %q %+v", level, status, msg, *events)
			}
			continue
		}
		observationFlush(t, g)
		if status != 503 || !strings.Contains(msg, "fhir.profile") || len(*events) != 1 {
			t.Fatalf("strict: %d %q %+v", status, msg, *events)
		}
		var f ConformanceFinding
		_ = json.Unmarshal([]byte((*events)[0].Detail), &f)
		if f.Rule != "fhir.profile" || f.State != CheckUnavailable || f.Action != "refused" {
			t.Fatalf("finding %+v", f)
		}
	}
}

func TestEligibilityUnavailableStatusAllCallers(t *testing.T) {
	for _, side := range []string{"originator", "payer"} {
		for _, direction := range []string{"request", "response"} {
			for _, missing := range []bool{false, true} {
				if missing && direction == "response" {
					continue
				} // no checker can pass the preceding request
				t.Run(side+"/"+direction+fmt.Sprint(missing), func(t *testing.T) {
					var events []ObserverEvent
					var validator shnsdk.Validator = syntheticValidatorFunc(func(body []byte) (shnsdk.Result, error) {
						if direction == "request" || bytes.Contains(body, []byte(`"CoverageEligibilityResponse"`)) {
							return shnsdk.Result{}, errors.New("synthetic unavailable")
						}
						return shnsdk.Result{Valid: true}, nil
					})
					if missing {
						validator = nil
					}
					rec := httptest.NewRecorder()
					if side == "originator" {
						e := newInProcessExchange(t)
						e.originator.cfg.ConformanceEnforcement = EnforcementStrict
						e.originator.cfg.Validator = validator
						e.originator.cfg.Observer = func(ev ObserverEvent) { events = append(events, ev) }
						e.payerReturns(LegResult{Response: testResponse([]byte(`{"resourceType":"CoverageEligibilityResponse"}`))})
						e.originator.handleScenario(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc01", strings.NewReader(`{"branch":"covered"}`)))
						observationFlush(t, e.originator)
					} else {
						g, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-COVERED"})
						g.cfg.ConformanceEnforcement = EnforcementStrict
						g.cfg.Validator = validator
						g.cfg.Observer = func(ev ObserverEvent) { events = append(events, ev) }
						g.handleInbound(rec, eligibilityInboundRequestClean(t, g, "unavailable"))
						observationFlush(t, g)
					}
					if rec.Code != 503 {
						t.Fatalf("unavailable flattened: %d %s", rec.Code, rec.Body.String())
					}
					if side == "originator" && direction == "response" {
						wantSafeConformanceRefusal("conformance_unavailable", "fhir.profile", "response")(t, rec.Body.String())
					} else if !strings.Contains(rec.Body.String(), "conformance_unavailable: fhir.profile") {
						t.Fatalf("unavailable flattened: %d %s", rec.Code, rec.Body.String())
					}
					found := false
					for _, ev := range events {
						if ev.Kind != ConformanceObservedEvent {
							continue
						}
						var f ConformanceFinding
						_ = json.Unmarshal([]byte(ev.Detail), &f)
						if f.Rule == "fhir.profile" && f.State == CheckUnavailable && f.Action == "refused" {
							found = true
						}
					}
					if !found {
						t.Fatal("missing unavailable rule/action finding")
					}
				})
			}
		}
	}
}
