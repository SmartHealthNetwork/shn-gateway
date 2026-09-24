package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestEnforceRulesFindingClassAndClosedReason(t *testing.T) {
	for _, row := range []struct {
		name       string
		rule       string
		class      CheckClass
		result     CheckResult
		wantReason string
	}{
		{"structural invalid", "content.contract", CheckStructural, CheckResult{State: CheckInvalid, Code: "content.contract", Severity: "error"}, "content.contract"},
		{"deep invalid", "fhir.profile", CheckDeep, CheckResult{State: CheckInvalid, Code: "profile-unsupported", Severity: "error"}, "profile-unsupported"},
		{"missing declaration", "fhir.profile", CheckDeep, deepUnavailable("version_unavailable"), "version_unavailable"},
		{"checker unavailable", "fhir.profile", CheckDeep, deepUnavailable("validator_unavailable"), "validator_unavailable"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var findings []ConformanceFinding
			g := &Gateway{cfg: Config{HolderID: "payer", Observer: func(e ObserverEvent) {
				if e.Kind == ConformanceObservedEvent {
					var f ConformanceFinding
					if err := json.Unmarshal([]byte(e.Detail), &f); err != nil {
						t.Error(err)
						return
					}
					findings = append(findings, f)
				}
			}}}
			in := CheckInput{Exchange: ExchangeContext{policy: NewConformancePolicy(EnforcementStrict), legType: "pas-claim"}, Direction: "request", Body: []byte(`{"resourceType":"Bundle"}`), DeclaredVersion: "pa.pas@2.0"}
			err := g.enforceRules(context.Background(), in, []ConformanceRule{{ID: row.rule, Class: row.class, Applies: func(CheckInput) bool { return true }, Check: func(context.Context, CheckInput) CheckResult { return row.result }}})
			if err == nil {
				t.Fatal("expected refusal")
			}
			observationFlush(t, g)
			if len(findings) != 1 || findings[0].CheckClass != row.class || findings[0].ResultSeverity != "error" || findings[0].ClosedReason != row.wantReason || findings[0].Action != "refused" {
				t.Fatalf("strict finding: %+v", findings)
			}
		})
	}
}

func TestEnforceRulesActualStructuralAndDeepFindings(t *testing.T) {
	for _, row := range []struct {
		name   string
		body   []byte
		class  CheckClass
		rule   string
		reason string
	}{
		{"malformed JSON", []byte(`{"resourceType":`), CheckStructural, "json.syntax", "json.syntax"},
		{"unavailable profile checker", []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`), CheckDeep, "fhir.profile", "validator_unavailable"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var got []ConformanceFinding
			g := &Gateway{cfg: Config{HolderID: "payer", Observer: func(e ObserverEvent) {
				if e.Kind != ConformanceObservedEvent {
					return
				}
				var f ConformanceFinding
				if err := json.Unmarshal([]byte(e.Detail), &f); err != nil {
					t.Error(err)
					return
				}
				got = append(got, f)
			}}}
			in := CheckInput{Exchange: ExchangeContext{policy: NewConformancePolicy(EnforcementStrict), legType: "pas-claim"}, Direction: "request", Body: row.body, DeclaredVersion: "pa.pas@2.0"}
			rules := StructuralRules()
			if row.class == CheckDeep {
				rules = g.DeepRules()
			}
			if err := g.enforceRules(context.Background(), in, rules); err == nil {
				t.Fatal("expected refusal")
			}
			observationFlush(t, g)
			if len(got) != 1 || got[0].Rule != row.rule || got[0].CheckClass != row.class || got[0].ClosedReason != row.reason || got[0].ResultSeverity != "error" {
				t.Fatalf("actual rule finding: %+v", got)
			}
		})
	}
}

func TestRuleFindingIgnoresUntrustedPayloadProfile(t *testing.T) {
	known, _ := profileFor("PASRequestBundle", "2.0", "pas-claim")
	in := CheckInput{Exchange: ExchangeContext{policy: NewConformancePolicy(EnforcementObserve), legType: "pas-claim"}, Direction: "request", Body: []byte(`{"resourceType":"Bundle","meta":{"profile":["` + known + `"]}}`), DeclaredVersion: "pa.pas@2.0", evidence: &contentEvidence{}}
	f := safeFinding(ruleFinding(in, ConformanceRule{ID: "fhir.profile", Class: CheckDeep}, deepUnavailable("validator_unavailable"), "not_enforced"))
	if f.Profile != "" || len(f.Profiles) != 0 {
		t.Fatalf("payload meta.profile became a selected target: %+v", f)
	}
}

func TestStrictAuthoredDTRQRFindingPreservesSelectedBaseProfile(t *testing.T) {
	var validatedProfile string
	g, events, logged := findingGateway(t, observationValidator(func(_ context.Context, _ []byte, profile string) (shnsdk.Result, error) {
		validatedProfile = profile
		return shnsdk.Result{Valid: false, Issues: []string{"synthetic rejection"}}, nil
	}))
	ctx := withFindingContext(context.Background(), findingContext{LegType: "dtr-questionnaire-fetch", CorrelationID: "corr-qr-profile", Seam: "originate", Whose: "own"})
	status, _ := g.validateFHIRForContract(ctx, []byte(`{"resourceType":"QuestionnaireResponse","status":"completed"}`), "egress", "pa.dtr", "2.0", baseQRProfile)
	if status != 422 || validatedProfile != baseQRProfile {
		t.Fatalf("strict checker status=%d selected profile=%q", status, validatedProfile)
	}
	observationFlush(t, g)
	if len(*events) != 1 {
		t.Fatalf("want one strict finding, got %+v", *events)
	}
	var f ConformanceFinding
	if err := json.Unmarshal([]byte((*events)[0].Detail), &f); err != nil {
		t.Fatal(err)
	}
	if f.Rule != "fhir.profile" || f.Profile != baseQRProfile || f.LegType != "dtr-questionnaire-fetch" || f.Whose != "own" || f.Action != "refused" || !strings.Contains(logged.String(), baseQRProfile) {
		t.Fatalf("selected strict QR profile lost: %+v log=%s", f, logged)
	}
	g.emitFinding(ConformanceFinding{Kind: ConformanceObservedEvent, LegType: "dtr-questionnaire-fetch", Level: "strict", Profile: "http://evil.example/StructureDefinition/FOREIGN-PATIENT-SECRET"})
	observationFlush(t, g)
	if len(*events) != 2 {
		t.Fatalf("want selected and filtered findings, got %+v", *events)
	}
	var rejected ConformanceFinding
	if err := json.Unmarshal([]byte((*events)[1].Detail), &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.Profile != "" || strings.Contains((*events)[1].Detail, "FOREIGN-PATIENT-SECRET") || strings.Contains(logged.String(), "FOREIGN-PATIENT-SECRET") {
		t.Fatalf("untrusted profile crossed finding carrier: %+v", rejected)
	}
	profiles := []string{baseQRProfile}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, species := range []string{"Claim", "ClaimResponse", "PASRequestBundle", "PASResponseBundle"} {
			p, ok := profileFor(species, line, "pas-claim")
			if !ok {
				t.Fatalf("missing %s profile at %s", species, line)
			}
			profiles = append(profiles, p)
		}
	}
	bounded := safeFinding(ConformanceFinding{Profiles: profiles})
	if len(bounded.Profiles) != 8 || bounded.Profiles[0] != baseQRProfile {
		t.Fatalf("known profile target bound changed: %+v", bounded.Profiles)
	}
}

// A handler that sets the context hands the choke point the leg metadata a
// finding needs; a validate call made outside any handler still produces a
// finding, labelled unknown, and never suppresses one.
func TestFindingContextRoundTrips(t *testing.T) {
	fc := findingContext{LegType: "crd-order-select", CorrelationID: "corr-1", Seam: "provider-ingress", Whose: "peer"}
	got := findingContextFrom(withFindingContext(context.Background(), fc))
	if got != fc {
		t.Fatalf("finding context must round-trip: got %+v want %+v", got, fc)
	}
}

func TestFindingContextAbsentIsUnknownNeverSuppressing(t *testing.T) {
	got := findingContextFrom(context.Background())
	if got.LegType != "unknown" {
		t.Fatalf("an absent finding context must read as unknown, got %q", got.LegType)
	}
	if got.Whose != "" || got.CorrelationID != "" || got.Seam != "" {
		t.Fatalf("an absent finding context must invent nothing else: %+v", got)
	}
}

func TestFindingContextEmptyLegTypeIsUnknown(t *testing.T) {
	got := findingContextFrom(withFindingContext(context.Background(), findingContext{Whose: "own"}))
	if got.LegType != "unknown" {
		t.Fatalf("an empty legType must read as unknown, got %q", got.LegType)
	}
}

// The finding is emitted on both carriers with the same JSON, and that JSON is
// metadata only: a validator diagnostic string never appears on either one
// (§5 — diagnostics can contain whole foreign resources, and hosted-tenant
// logs are tailed across the participant boundary into SHN's console).
func TestEmitFindingBothCarriersMetadataOnly(t *testing.T) {
	const diagnostic = "Coverage.status: minimum required = 1, but only found 0 (from FOREIGN-RESOURCE-BODY)"
	var events []ObserverEvent
	g := &Gateway{cfg: Config{
		Clock:    func() time.Time { return time.Unix(0, 0).UTC() },
		Observer: func(e ObserverEvent) { events = append(events, e) },
	}}

	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	g.emitFinding(ConformanceFinding{
		Kind: "fhir-ingress", Direction: "ingress", LegType: "crd-order-select",
		CorrelationID: "corr-1", Seam: "provider-ingress", Whose: "peer",
		Line: "2.0", Level: "none", Decision: "relayed",
		PayloadSHA256: "abcd", Issues: []string{diagnostic}, // RAW — emitFinding must redact this itself.
	})

	observationFlush(t, g)
	if len(events) != 1 {
		t.Fatalf("want exactly one observer event, got %d", len(events))
	}
	if events[0].Kind != ConformanceObservedEvent {
		t.Fatalf("observer kind = %q, want %q", events[0].Kind, ConformanceObservedEvent)
	}
	if events[0].Direction != "validate" {
		t.Fatalf("observer direction = %q, want validate", events[0].Direction)
	}
	if events[0].CorrelationID != "corr-1" || events[0].LegType != "crd-order-select" {
		t.Fatalf("observer event must carry the leg: %+v", events[0])
	}
	if !strings.Contains(logged.String(), "conformance: ") {
		t.Fatalf("log line must be the conformance: structured line, got %q", logged.String())
	}
	wantRedacted := certificationIssueMetadata([]string{diagnostic})[0]
	for name, carrier := range map[string]string{"log": logged.String(), "observer": events[0].Detail} {
		if strings.Contains(carrier, diagnostic) || strings.Contains(carrier, "FOREIGN-RESOURCE-BODY") {
			t.Fatalf("%s carrier leaked a validator diagnostic: %s", name, carrier)
		}
		if !strings.Contains(carrier, `"action":"not_enforced"`) {
			t.Fatalf("%s carrier must state the decision: %s", name, carrier)
		}
		if !strings.Contains(carrier, wantRedacted) {
			t.Fatalf("%s carrier must carry the safe issue count, got: %s", name, carrier)
		}
	}
}

// A gateway with no observer configured still writes the log line: the
// observer stream exists only where OBSERVER_ADDR is set, so in a hosted
// tenant the log is the only carrier.
func TestEmitFindingWithoutObserverStillLogs(t *testing.T) {
	g := &Gateway{cfg: Config{Clock: func() time.Time { return time.Unix(0, 0).UTC() }}}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	g.emitFinding(ConformanceFinding{Kind: "fhir-egress", Decision: "refused", Level: "strict"})
	if !strings.Contains(logged.String(), `"kind":"fhir-egress"`) {
		t.Fatalf("log carrier must carry the finding without an observer: %q", logged.String())
	}
}

// The strict refusal body is the one place issue text may appear, and it is
// bounded exactly like the CDS certifier's list. The boundary at exactly
// findingIssuesShown (5) is the one an off-by-one guard (< vs <=) misbehaves
// on: it would wrongly append "and 0 more" to a five-item list.
func TestBoundIssues(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"zero", nil, []string{}},
		{"under the bound", []string{"a", "b"}, []string{"a", "b"}},
		{"exactly at the bound", []string{"a", "b", "c", "d", "e"}, []string{"a", "b", "c", "d", "e"}},
		{"over the bound", []string{"a", "b", "c", "d", "e", "f", "g"}, []string{"a", "b", "c", "d", "e", "and 2 more"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := boundIssues(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("boundIssues(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("boundIssues(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}
