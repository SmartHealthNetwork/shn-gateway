package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type subjectResolverFunc func(context.Context, PatientReference) (string, bool, error)

func (f subjectResolverFunc) ResolveSubject(ctx context.Context, ref PatientReference) (string, bool, error) {
	return f(ctx, ref)
}

func deepRule(t *testing.T, g *Gateway, id string) ConformanceRule {
	t.Helper()
	for _, r := range g.DeepRules() {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("missing deep rule %s", id)
	return ConformanceRule{}
}
func deepInput(body string) CheckInput {
	return CheckInput{Body: []byte(body), Direction: "request", DeclaredVersion: "pa.pas@2.0", Exchange: ExchangeContext{holder: "provider", recipient: "payer", legType: "pas-claim", subjectPCI: "pci-a", policy: NewConformancePolicy(EnforcementStrict)}}
}

// Dropping either evidence channel must not let a required check pass.
func TestDeepEvidenceOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state shnsdk.ValidationState
		err   error
		want  CheckState
	}{
		{"valid", shnsdk.ValidationValid, nil, CheckValid}, {"invalid", shnsdk.ValidationInvalid, nil, CheckInvalid},
		{"unsupported", shnsdk.ValidationUnavailable, nil, CheckUnavailable}, {"not applicable cannot waive required", shnsdk.ValidationNotApplicable, nil, CheckUnavailable},
		{"malformed state", "invented", nil, CheckUnavailable}, {"canceled", shnsdk.ValidationValid, context.Canceled, CheckUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := syntheticEvidence()
			ev.Profile.State = tc.state
			ev.Profile.Code = "value"
			g := &Gateway{cfg: Config{Validator: &shnsdk.FakeValidator{Evidence: ev, Err: tc.err}}}
			in := deepInput(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`)
			got := deepRule(t, g, "fhir.profile").Check(context.Background(), in)
			if got.State != tc.want {
				t.Fatalf("got %+v want %s", got, tc.want)
			}
		})
	}
}
func TestDeepRealClientDoesNotInventTerminology(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"informational"}]}`))
	}))
	defer srv.Close()
	g := &Gateway{cfg: Config{Validator: shnsdk.NewOperationValidator(srv.URL)}}
	in := deepInput(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`)
	profile := deepRule(t, g, "fhir.profile").Check(context.Background(), in)
	if profile.State != CheckValid || len(profile.Issues) != 1 || profile.Issues[0].Severity != "warning" {
		t.Fatalf("warning lost: %+v", profile)
	}
	term := deepRule(t, g, "fhir.terminology").Check(context.Background(), in)
	if term.State != CheckUnavailable || calls != 2 {
		t.Fatalf("coverage invented: %+v calls=%d", term, calls)
	}
}
func TestDeepNoDeclarationAndLegacyEvidenceUnavailable(t *testing.T) {
	in := deepInput(`{"resourceType":"Bundle","entry":[]}`)
	g := &Gateway{cfg: Config{Validator: syntheticFakeValidator()}}
	in.DeclaredVersion = ""
	in.Exchange.contractVersion = "pa.pas@2.0"
	if got := deepRule(t, g, "fhir.profile").Check(context.Background(), in); got.State != CheckUnavailable {
		t.Fatalf("borrowed request stamp: %+v", got)
	}
	in.DeclaredVersion = "pa.pas@2.0"
	g.cfg.Validator = validatorFunc(func([]byte) (shnsdk.Result, error) { t.Fatal("called legacy validator"); return shnsdk.Result{}, nil })
	if got := deepRule(t, g, "fhir.profile").Check(context.Background(), in); got.State != CheckUnavailable {
		t.Fatalf("legacy pass: %+v", got)
	}
}
func TestSubjectConsistencyNamespacesAndDependents(t *testing.T) {
	// Subscriber is a different person; only beneficiary is the coverage patient.
	body := `{"resourceType":"Bundle","entry":[{"fullUrl":"https://source.example/fhir/Patient/local","resource":{"resourceType":"Patient","id":"local","identifier":[{"system":"urn:source","value":"external-a"}]}},{"resource":{"resourceType":"Claim","patient":{"reference":"https://source.example/fhir/Patient/local"}}},{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/other-alias"},"subscriber":{"reference":"Patient/parent"}}}]}`
	for _, tc := range []struct {
		name    string
		resolve subjectResolverFunc
		want    CheckState
	}{
		{"different aliases same person", func(_ context.Context, r PatientReference) (string, bool, error) {
			if r.Holder != "provider" {
				t.Fatalf("wrong owner %+v", r)
			}
			if strings.Contains(r.Value, "parent") {
				t.Fatal("subscriber compared")
			}
			return "pci-a", true, nil
		}, CheckValid},
		{"proven mismatch", func(_ context.Context, r PatientReference) (string, bool, error) { return "pci-b", true, nil }, CheckInvalid},
		{"unknown linkage", func(context.Context, PatientReference) (string, bool, error) { return "", false, nil }, CheckUnavailable},
		{"failed linkage", func(context.Context, PatientReference) (string, bool, error) {
			return "", false, errors.New("private-patient")
		}, CheckUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &Gateway{cfg: Config{SubjectReferenceResolver: tc.resolve}}
			got := deepRule(t, g, "patient.consistency").Check(context.Background(), deepInput(body))
			if got.State != tc.want || strings.Contains(got.Code, "private") {
				t.Fatalf("got %+v want %s", got, tc.want)
			}
		})
	}
}
func TestSubjectConsistencyCannotResolveForeignByLocalID(t *testing.T) {
	var refs []PatientReference
	g := &Gateway{cfg: Config{HolderID: "payer", SubjectReferenceResolver: subjectResolverFunc(func(_ context.Context, r PatientReference) (string, bool, error) {
		refs = append(refs, r)
		if r.Holder == "provider" && r.System == "https://foreign.example/fhir" && r.Value == "Patient/123" {
			return "pci-a", true, nil
		}
		return "", false, nil
	})}}
	in := deepInput(`{"resourceType":"Claim","patient":{"reference":"https://foreign.example/fhir/Patient/123"}}`)
	r := deepRule(t, g, "patient.consistency")
	if got := r.Check(context.Background(), in); got.State != CheckValid {
		t.Fatalf("explicit foreign linkage: %+v refs=%+v", got, refs)
	}
	in.Body = []byte(`{"resourceType":"Claim","patient":{"reference":"Patient/123"}}`)
	if got := r.Check(context.Background(), in); got.State != CheckUnavailable {
		t.Fatalf("foreign/local lexical equality: %+v", got)
	}
	in.Direction = "response"
	in.Status = 200
	if got := r.Check(context.Background(), in); got.State != CheckUnavailable || refs[len(refs)-1].Holder != "payer" {
		t.Fatalf("response owner: %+v %+v", got, refs)
	}
}
func TestDeepModesDoNotCallOptionalDependencies(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			calls := 0
			ev := syntheticEvidence()
			ev.Terminology.State = shnsdk.ValidationUnavailable
			g := &Gateway{cfg: Config{HolderID: "gw", Validator: &shnsdk.FakeValidator{Evidence: ev}, SubjectReferenceResolver: subjectResolverFunc(func(context.Context, PatientReference) (string, bool, error) { calls++; return "pci-a", true, nil })}}
			in := deepInput(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/123"}}}]}`)
			in.Exchange.policy = NewConformancePolicy(level)
			err := g.enforceContent(context.Background(), in)
			if level == EnforcementStrict {
				wantStructuralError(t, err, 503, "fhir.terminology")
			} else if err != nil || calls != 0 {
				t.Fatalf("optional rule enforced: %v calls=%d", err, calls)
			}
		})
	}
}
func TestDeepRegistryMutationAndModeTwins(t *testing.T) {
	for _, tc := range []struct{ id, leg, direction, version, good, bad string }{
		{"pas.graph", "pas-claim", "response", "pa.pas@2.0", assemblyRealPending, `{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://payer.example/ClaimResponse/x","resource":{"resourceType":"ClaimResponse","id":"x","insurer":{"reference":"Organization/missing"}}}]}`},
		{"cds.response", "crd-order-select", "response", "pa.crd@2.0", `{"cards":[]}`, `{"cards":[{"summary":"hello","indicator":"invalid","source":{"label":"payer","topic":{"system":"urn:synthetic","code":"x"}}}]}`},
		{"crd.resources", "crd-order-select", "request", "pa.crd@2.0", `{"hook":"order-select","context":{"draftOrders":{"resourceType":"Bundle","entry":[]}},"prefetch":{}}`, `{"hook":"order-select","context":{},"prefetch":{"x":{"resourceType":"Binary","data":"c2VjcmV0"}}}`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			g := &Gateway{}
			r := deepRule(t, g, tc.id)
			in := deepInput(tc.good)
			in.Exchange.legType = tc.leg
			in.Direction = tc.direction
			in.Status = 200
			in.DeclaredVersion = tc.version
			if !r.Applies(in) {
				t.Fatal("not applicable")
			}
			if got := r.Check(context.Background(), in); got.State != CheckValid {
				t.Fatalf("valid: %+v", got)
			}
			in.Body = []byte(tc.bad)
			if got := r.Check(context.Background(), in); got.State != CheckInvalid {
				t.Fatalf("mutation: %+v", got)
			}
			for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
				in.Exchange.policy = NewConformancePolicy(level)
				err := g.enforceRules(context.Background(), in, []ConformanceRule{r})
				if (err != nil) != (level == EnforcementStrict) {
					t.Fatalf("mode %v: %v", level, err)
				}
			}
			in.Body = []byte("{bad")
			if got := r.Check(context.Background(), in); got.State != CheckUnavailable {
				t.Fatalf("unreadable check: %+v", got)
			}
		})
	}
}

// The authoritative capability survives optional SoR observation decoration;
// legacy demographics never become an implicit linkage source.
type authoritativeSoR struct {
	SystemOfRecord
	calls int
}

func (s *authoritativeSoR) ResolveSubject(_ context.Context, r PatientReference) (string, bool, error) {
	s.calls++
	if r.Holder == "provider" && r.System == "urn:source" && r.Value == "id-a" {
		return "pci-a", true, nil
	}
	return "", false, nil
}
func (s *authoritativeSoR) ResolvePatient(string) (string, Demo, bool) {
	panic("legacy identity must not be used")
}
func TestSubjectConsistencyDefaultCapabilityAndObservation(t *testing.T) {
	e := newInProcessExchange(t)
	for _, observed := range []bool{false, true} {
		source := &authoritativeSoR{SystemOfRecord: e.originator.cfg.SoR}
		cfg := e.originator.cfg
		cfg.SoR = source
		cfg.SubjectReferenceResolver = nil
		cfg.Observer = nil
		if observed {
			cfg.Observer = func(ObserverEvent) {}
		}
		g, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		in := deepInput(`{"resourceType":"Claim","patient":{"identifier":{"system":"urn:source","value":"id-a"}}}`)
		got := deepRule(t, g, "patient.consistency").Check(context.Background(), in)
		if got.State != CheckValid || source.calls != 1 {
			t.Fatalf("observed=%v got=%+v calls=%d", observed, got, source.calls)
		}
		in.Body = []byte(`{"resourceType":"Claim","patient":{"reference":"Patient/id-a"}}`)
		if got := deepRule(t, g, "patient.consistency").Check(context.Background(), in); got.State != CheckUnavailable {
			t.Fatalf("unsupported local namespace: %+v", got)
		}
	}
	g := &Gateway{cfg: Config{SoR: &authoritativeSoR{SystemOfRecord: e.originator.cfg.SoR}}}
	in := deepInput(`{"resourceType":"Claim","patient":{"reference":"Patient/id-a"}}`)
	if got := g.checkSubjectConsistency(context.Background(), in); got.State != CheckUnavailable {
		t.Fatalf("nil resolver: %+v", got)
	}
}
func TestDeepGovernedModesAndAdaptation(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		calls := 0
		v := validatorFunc(func([]byte) (shnsdk.Result, error) { calls++; return shnsdk.Result{}, errors.New("private") })
		g := &Gateway{cfg: Config{ConformanceEnforcement: level}}
		got := g.validateGoverned(context.Background(), findingContext{}, v, []byte("{bad"), "ingress", "2.0", "", false)
		if level == EnforcementStrict {
			if got.Status != 503 {
				t.Fatalf("strict legacy unavailable=%+v", got)
			}
		} else if got.Status != 0 {
			t.Fatalf("optional check refused at %s: %+v", level, got)
		}
		if calls != 0 {
			t.Fatalf("legacy or optional validation invoked at %s", level)
		}
		got = g.validateGoverned(context.Background(), findingContext{}, v, []byte("{bad"), "egress", "2.0", "", true)
		if got.Status == 0 {
			t.Fatalf("adaptation waived at %s", level)
		}
	}
}
func TestDeepRequiredCheckNotApplicableAndWarnings(t *testing.T) {
	g := &Gateway{}
	for _, state := range []CheckState{CheckNotApplicable, CheckUnavailable} {
		r := ConformanceRule{ID: "required", Class: CheckDeep, Applies: func(CheckInput) bool { return true }, Check: func(context.Context, CheckInput) CheckResult { return CheckResult{State: state} }}
		err := g.enforceRules(context.Background(), deepInput(`{}`), []ConformanceRule{r})
		wantStructuralError(t, err, 503, "required")
	}
	r := ConformanceRule{ID: "warning", Class: CheckDeep, Applies: func(CheckInput) bool { return true }, Check: func(context.Context, CheckInput) CheckResult {
		return CheckResult{State: CheckInvalid, Severity: "warning"}
	}}
	if err := g.enforceRules(context.Background(), deepInput(`{}`), []ConformanceRule{r}); err != nil {
		t.Fatal(err)
	}
}

func TestDeepDTRPackageProfileMap(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		def, _ := shnsdk.DTRLineDef(line)
		for _, row := range []struct{ direction, body, profile string }{
			{"request", `{"resourceType":"Parameters","parameter":[]}`, "dtr-qpackage-input-parameters"},
			{"response", `{"resourceType":"Bundle","type":"collection","entry":[]}`, "DTR-QPackageBundle"},
			{"response", `{"resourceType":"Parameters","parameter":[{"name":"packagebundle","resource":{"resourceType":"Bundle","type":"collection","entry":[]}}]}`, "dtr-qpackage-output-parameters"},
		} {
			in := deepInput(row.body)
			in.Exchange.legType = "dtr-questionnaire-fetch"
			in.Exchange.operation = shnsdk.FrameOperationQuestionnairePackage
			in.Direction = row.direction
			in.Status = 200
			in.DeclaredVersion = "pa.dtr@" + line
			targets, contract, gotLine, ok := deepValidationTargets(in)
			want := "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/" + row.profile + "|" + def.PackageVersion
			if !ok || contract != "pa.dtr" || gotLine != line || len(targets) != 1 || targets[0].profile != want {
				t.Fatalf("%s/%s: %+v %s %s %v want %s", line, row.direction, targets, contract, gotLine, ok, want)
			}
		}
	}
}

func TestDeepWarningEvidenceAndSharedExecution(t *testing.T) {
	calls := 0
	v := syntheticEvidenceValidatorFunc(func([]byte) (shnsdk.Result, error) { calls++; return shnsdk.Result{Valid: true}, nil })
	g := &Gateway{cfg: Config{Validator: v}}
	in := deepInput(`{"resourceType":"Bundle","entry":[]}`)
	in.evidence = &contentEvidence{}
	for _, id := range []string{"fhir.profile", "fhir.terminology"} {
		if got := deepRule(t, g, id).Check(context.Background(), in); got.State != CheckValid {
			t.Fatal(got)
		}
	}
	if calls != 1 {
		t.Fatalf("separate rules duplicated execution: %d", calls)
	}
	result := validationResult(shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid, Issues: []shnsdk.ValidationIssue{{Severity: "warning", Code: "informational"}}}, nil)
	if result.State != CheckValid || len(result.Issues) != 1 || result.Issues[0].Severity != "warning" {
		t.Fatalf("warning lost: %+v", result)
	}
}

func TestSubjectConsistencyRelativeGraphNamespace(t *testing.T) {
	body := `{"resourceType":"Bundle","entry":[{"fullUrl":"https://foreign.example/fhir/Claim/c","resource":{"resourceType":"Claim","id":"c","patient":{"reference":"Patient/123"}}},{"fullUrl":"https://foreign.example/fhir/Patient/123","resource":{"resourceType":"Patient","id":"123","identifier":[{"system":"urn:external-linkage","value":"a"}]}}]}`
	calls := 0
	g := &Gateway{cfg: Config{SubjectReferenceResolver: subjectResolverFunc(func(_ context.Context, r PatientReference) (string, bool, error) {
		calls++
		if r.System != "urn:external-linkage" || r.Value != "a" {
			t.Fatalf("resolved relative foreign id as local: %+v", r)
		}
		return "pci-a", true, nil
	})}}
	if got := g.checkSubjectConsistency(context.Background(), deepInput(body)); got.State != CheckValid || calls != 1 {
		t.Fatalf("graph %+v calls=%d", got, calls)
	}
}

// Every registered rule has a real invalid row and a canceled/unavailable row,
// including none/observe/basic counterparts that execute no synchronous checks.
func TestDeepRegistryAllRules(t *testing.T) {
	item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", shnsdk.Attestation{NPI: "1999999999", Text: "I attest these findings.", When: "2026-06-04"})
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ id, leg, dir, version, good, bad string }
	rows := []row{
		{"cds.request.context", "crd-order-select", "request", "pa.crd@2.0", `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p","selections":[],"draftOrders":{"resourceType":"Bundle"}}}`, `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","selections":[],"draftOrders":{"resourceType":"Bundle"}}}`},
		{"fhir.profile", "pas-claim", "request", "pa.pas@2.0", `{"resourceType":"Bundle","entry":[]}`, `{"resourceType":"Bundle","entry":[]}`},
		{"fhir.terminology", "pas-claim", "request", "pa.pas@2.0", `{"resourceType":"Bundle","entry":[]}`, `{"resourceType":"Bundle","entry":[]}`},
		{"patient.consistency", "pas-claim", "request", "pa.pas@2.0", `{"resourceType":"Claim","patient":{"reference":"Patient/a"}}`, `{"resourceType":"Claim","patient":{"reference":"Patient/b"}}`},
		{"pas.graph", "pas-claim", "response", "pa.pas@2.0", assemblyRealPending, `{"resourceType":"Bundle","entry":[]}`},
		{"pas.provenance", "pas-claim-update", "request", "pa.pas@2.2", pasProvenanceRuleValid, strings.Replace(pasProvenanceRuleValid, `"agent":[{"who":{"reference":"Practitioner/author"}}]`, `"agent":[]`, 1)},
		{"qr.attestation", "pas-claim", "request", "pa.pas@2.0", string(wrapQRItemBundle(t, item)), string(wrapQRItemBundle(t, stripItemExtension(t, item)))},
		{"crd.resources", "crd-order-select", "request", "pa.crd@2.0", `{"context":{},"prefetch":{}}`, `{"context":{},"prefetch":{"x":{"resourceType":"Binary","data":"x"}}}`},
		{"cds.response", "crd-order-select", "response", "pa.crd@2.0", `{"cards":[]}`, `{"cards":[{"summary":"hello","indicator":"bad","source":{"label":"payer"}}]}`},
		{"version.consistency", "pas-claim", "response", "pa.pas@2.0", `{}`, `{}`},
	}
	covered := map[string]bool{}
	for _, tc := range rows {
		t.Run(tc.id, func(t *testing.T) {
			covered[tc.id] = true
			ev := syntheticEvidence()
			g := &Gateway{cfg: Config{Validator: &shnsdk.FakeValidator{Evidence: ev}, SubjectReferenceResolver: subjectResolverFunc(func(_ context.Context, r PatientReference) (string, bool, error) {
				if r.Value == "Patient/b" {
					return "pci-b", true, nil
				}
				return "pci-a", true, nil
			})}}
			rule := deepRule(t, g, tc.id)
			in := deepInput(tc.good)
			in.Exchange.legType = tc.leg
			in.Direction = tc.dir
			in.Status = 200
			in.DeclaredVersion = tc.version
			in.Exchange.contractVersion = tc.version
			if !rule.Applies(in) {
				t.Fatal("case does not exercise applicability")
			}
			if got := rule.Check(context.Background(), in); got.State != CheckValid {
				t.Fatalf("valid %+v", got)
			}
			in.Body = []byte(tc.bad)
			if tc.id == "fhir.profile" {
				ev.Profile.State = shnsdk.ValidationInvalid
				ev.Profile.Code = "value"
			}
			if tc.id == "fhir.terminology" {
				ev.Terminology.State = shnsdk.ValidationInvalid
				ev.Terminology.Code = "code-invalid"
			}
			if tc.id == "version.consistency" {
				in.DeclaredVersion = "pa.pas@2.2"
			}
			if got := rule.Check(context.Background(), in); got.State != CheckInvalid {
				t.Fatalf("invalid %+v", got)
			}
			for _, unavailable := range []bool{false, true} {
				ctx := context.Background()
				if unavailable {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
					if got := rule.Check(ctx, in); got.State != CheckUnavailable {
						t.Fatalf("unavailable %+v", got)
					}
				}
				for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
					calls := 0
					wrapped := rule
					wrapped.Check = func(ctx context.Context, in CheckInput) CheckResult { calls++; return rule.Check(ctx, in) }
					in.Exchange.policy = NewConformancePolicy(level)
					err := g.enforceRules(ctx, in, []ConformanceRule{wrapped})
					if level != EnforcementStrict {
						if err != nil || calls != 0 {
							t.Fatalf("%s: err=%v calls=%d", level, err, calls)
						}
						continue
					}
					want := 422
					if tc.dir == "response" {
						want = 502
					}
					if unavailable {
						want = 503
					}
					wantStructuralError(t, err, want, tc.id)
					if calls != 1 {
						t.Fatalf("strict calls %d", calls)
					}
				}
			}
		})
	}
	for _, r := range (&Gateway{}).DeepRules() {
		if !covered[r.ID] {
			t.Errorf("registered rule lacks outcome/mode rows: %s", r.ID)
		}
	}
}

func TestSubjectConsistencyExternalEmptyRosterAllModes(t *testing.T) {
	calls := 0
	g := &Gateway{cfg: Config{Validator: syntheticFakeValidator(), SubjectReferenceResolver: subjectResolverFunc(func(_ context.Context, r PatientReference) (string, bool, error) {
		calls++
		if r.Holder != "provider" || r.System != "https://external.example/fhir" || r.Value != "Patient/a" {
			t.Fatalf("namespace %+v", r)
		}
		return "pci-a", true, nil
	})}}
	in := deepInput(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"https://external.example/fhir/Patient/a"}}}]}`)
	// No SoR/roster exists. Only the explicit external authority establishes linkage.
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		in.Exchange.policy = NewConformancePolicy(level)
		if err := g.enforceContent(context.Background(), in); err != nil {
			t.Fatalf("%s: %v", level, err)
		}
	}
	if calls != 1 {
		t.Fatalf("optional identity checks ran synchronously: %d", calls)
	}
}
