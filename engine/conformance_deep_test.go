package engine

import (
	"context"
	"errors"
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

func TestDeepProfileOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
		err   error
		want  CheckState
	}{
		{"valid", true, nil, CheckValid},
		{"invalid", false, nil, CheckInvalid},
		{"unavailable", false, errors.New("controlled outage"), CheckUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := syntheticValidatorFunc(func([]byte) (shnsdk.Result, error) { return shnsdk.Result{Valid: tc.valid}, tc.err })
			g := &Gateway{cfg: Config{Validator: v}}
			got := deepRule(t, g, "fhir.profile").Check(context.Background(), deepInput(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`))
			if got.State != tc.want {
				t.Fatalf("got %+v want %s", got, tc.want)
			}
		})
	}
	if got := validationResult(validationCheckEvidence{State: "invented"}, nil); got.State != CheckUnavailable {
		t.Fatal(got)
	}
	if got := validationResult(validationCheckEvidence{State: validationValid}, context.Canceled); got.State != CheckUnavailable {
		t.Fatal(got)
	}
}

func TestDeepNoDeclarationUnavailable(t *testing.T) {
	in := deepInput(`{"resourceType":"Bundle","entry":[]}`)
	in.DeclaredVersion = ""
	in.Exchange.contractVersion = "pa.pas@2.0"
	g := &Gateway{cfg: Config{Validator: syntheticFakeValidator()}}
	if got := deepRule(t, g, "fhir.profile").Check(context.Background(), in); got.State != CheckUnavailable {
		t.Fatal(got)
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
			in.Exchange.legType, in.Direction, in.Status, in.DeclaredVersion = tc.leg, tc.direction, 200, tc.version
			if !r.Applies(in) || r.Check(context.Background(), in).State != CheckValid {
				t.Fatal("valid row failed")
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
		})
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
			in.Direction, in.Status, in.DeclaredVersion = row.direction, 200, "pa.dtr@"+line
			targets, contract, gotLine, ok := deepValidationTargets(in)
			want := "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/" + row.profile + "|" + def.PackageVersion
			if !ok || contract != "pa.dtr" || gotLine != line || len(targets) != 1 || targets[0].profile != want {
				t.Fatalf("%s/%s: %+v", line, row.direction, targets)
			}
		}
	}
}
func TestDeepRuleSetContainsOnlySupportedBaseline(t *testing.T) {
	want := map[string]bool{"fhir.profile": true, "pas.graph": true, "pas.provenance": true, "qr.attestation": true, "crd.resources": true, "cds.response": true, "version.consistency": true}
	for _, r := range (&Gateway{}).DeepRules() {
		if !want[r.ID] {
			t.Fatalf("unexpected rule %s", r.ID)
		}
		delete(want, r.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing rules %+v", want)
	}
}
