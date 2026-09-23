package engine

import (
	"context"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func inquiryDeepInput(body, version string) CheckInput {
	in := deepInput(body)
	in.Direction, in.Status = "response", 200
	in.Exchange.legType = "pas-claim-inquire"
	in.DeclaredVersion, in.Exchange.contractVersion = version, version
	return in
}

const inquiryPatientEntry = `{"fullUrl":"urn:uuid:patient","resource":{"resourceType":"Patient","identifier":[{"system":"urn:source","value":"a"}]}}`
const inquiryAnswerEntry = `{"fullUrl":"urn:uuid:answer","resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:patient"}}}`

func inquiryDeepBundle(entries ...string) string {
	return `{"resourceType":"Bundle","type":"collection","entry":[` + strings.Join(entries, ",") + `]}`
}
func inquiryDeepReturns(bundles ...string) string {
	var params []string
	for _, bundle := range bundles {
		params = append(params, `{"name":"return","resource":`+bundle+`}`)
	}
	return `{"resourceType":"Parameters","parameter":[` + strings.Join(params, ",") + `]}`
}

func TestDeepInquiryGraphCardinalityAndClosure(t *testing.T) {
	single := inquiryDeepBundle(inquiryPatientEntry, inquiryAnswerEntry)
	multiple := inquiryDeepBundle(inquiryPatientEntry, inquiryAnswerEntry, strings.ReplaceAll(inquiryAnswerEntry, "urn:uuid:answer", "urn:uuid:answer-two"))
	for _, tc := range []struct {
		name, version, body string
		want                CheckState
	}{
		{"old zero omitted", "pa.pas@2.0", `{"resourceType":"Bundle","type":"collection"}`, CheckValid},
		{"old zero", "pa.pas@2.0", inquiryDeepBundle(), CheckValid},
		{"old malformed entries", "pa.pas@2.0", `{"resourceType":"Bundle","type":"collection","entry":null}`, CheckInvalid},
		{"middle zero", "pa.pas@2.1", inquiryDeepBundle(), CheckValid},
		{"old multiple", "pa.pas@2.0", multiple, CheckValid},
		{"middle multiple", "pa.pas@2.1", multiple, CheckValid},
		{"modern zero omitted", "pa.pas@2.2", `{"resourceType":"Parameters"}`, CheckValid},
		{"modern zero", "pa.pas@2.2", inquiryDeepReturns(), CheckValid},
		{"modern empty return", "pa.pas@2.2", inquiryDeepReturns(inquiryDeepBundle()), CheckValid},
		{"modern multiple returns", "pa.pas@2.2", inquiryDeepReturns(single, multiple), CheckValid},
		{"old missing closure", "pa.pas@2.0", inquiryDeepBundle(inquiryAnswerEntry), CheckInvalid},
		{"old multiple missing closure", "pa.pas@2.0", strings.Replace(multiple, `"reference":"urn:uuid:patient"`, `"reference":"urn:uuid:missing"`, 1), CheckInvalid},
		{"modern sibling cannot close", "pa.pas@2.2", inquiryDeepReturns(inquiryDeepBundle(inquiryAnswerEntry), single), CheckInvalid},
		{"modern second return missing closure", "pa.pas@2.2", inquiryDeepReturns(single, inquiryDeepBundle(inquiryAnswerEntry)), CheckInvalid},
		{"duplicate same bundle", "pa.pas@2.2", inquiryDeepReturns(inquiryDeepBundle(inquiryPatientEntry, inquiryPatientEntry, inquiryAnswerEntry)), CheckInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &Gateway{}
			in := inquiryDeepInput(tc.body, tc.version)
			rule := deepRule(t, g, "pas.graph")
			if got := rule.Check(context.Background(), in); got.State != tc.want {
				t.Fatalf("graph = %+v, want %s", got, tc.want)
			}
			inquiryDeepModeTwins(t, g, in, rule, tc.want)
		})
	}
	for _, body := range []string{inquiryDeepBundle(), multiple} {
		in := inquiryDeepInput(body, "pa.pas@2.0")
		in.Exchange.legType = "pas-claim"
		if got := checkPASGraph(context.Background(), in); got.State != CheckInvalid {
			t.Fatalf("submit cardinality relaxed: %+v", got)
		}
	}
}

func inquiryDeepModeTwins(t *testing.T, g *Gateway, in CheckInput, rule ConformanceRule, want CheckState) {
	t.Helper()
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		in.Exchange.policy = NewConformancePolicy(level)
		calls := 0
		counted := rule
		counted.Check = func(ctx context.Context, input CheckInput) CheckResult { calls++; return rule.Check(ctx, input) }
		err := g.enforceRules(context.Background(), in, []ConformanceRule{counted})
		wantCalls := 0
		if level == EnforcementStrict {
			wantCalls = 1
		}
		if calls != wantCalls {
			t.Fatalf("%s calls=%d want %d", level, calls, wantCalls)
		}
		if level == EnforcementStrict && want != CheckValid {
			status := 502
			if want == CheckUnavailable {
				status = 503
			}
			wantStructuralError(t, err, status, rule.ID)
		} else if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
	}
}

func TestDeepInquiryCombinedRulesAndEmptyApplicability(t *testing.T) {
	single := inquiryDeepBundle(inquiryPatientEntry, inquiryAnswerEntry)
	for _, tc := range []struct {
		name, version, body string
		profile, patient    bool
	}{
		{"old empty", "pa.pas@2.0", inquiryDeepBundle(), true, false},
		{"middle omitted entries", "pa.pas@2.1", `{"resourceType":"Bundle","type":"collection"}`, true, false},
		{"modern omitted returns", "pa.pas@2.2", `{"resourceType":"Parameters"}`, false, false},
		{"modern empty returns", "pa.pas@2.2", inquiryDeepReturns(), false, false},
		{"modern empty bundles", "pa.pas@2.2", inquiryDeepReturns(inquiryDeepBundle(), inquiryDeepBundle()), true, false},
		{"modern independent bundles", "pa.pas@2.2", inquiryDeepReturns(single, single), true, true},
		{"old multiple answers", "pa.pas@2.0", inquiryDeepBundle(inquiryPatientEntry, inquiryAnswerEntry, strings.ReplaceAll(inquiryAnswerEntry, "urn:uuid:answer", "urn:uuid:second")), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, validations := 0, 0
			validator := syntheticEvidenceValidatorFunc(func([]byte) (shnsdk.Result, error) { validations++; return shnsdk.Result{Valid: true}, nil })
			g := &Gateway{cfg: Config{Validator: validator, SubjectReferenceResolver: subjectResolverFunc(func(context.Context, PatientReference) (string, bool, error) { calls++; return "pci-a", true, nil })}}
			in := inquiryDeepInput(tc.body, tc.version)
			for _, id := range []string{"fhir.profile", "fhir.terminology", "patient.consistency", "pas.graph", "version.consistency"} {
				want := true
				if id == "fhir.profile" || id == "fhir.terminology" {
					want = tc.profile
				}
				if id == "patient.consistency" {
					want = tc.patient
				}
				if got := deepRule(t, g, id).Applies(in); got != want {
					t.Fatalf("%s applies=%v want %v", id, got, want)
				}
			}
			for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
				calls = 0
				in.Exchange.policy = NewConformancePolicy(level)
				if err := g.enforceContent(context.Background(), in); err != nil {
					t.Fatalf("%s combined rules: %v", level, err)
				}
				if (level != EnforcementStrict || !tc.patient) && calls != 0 {
					t.Fatalf("unnecessary identity calls=%d", calls)
				}
				if (level != EnforcementStrict || !tc.profile) && validations != 0 {
					t.Fatalf("unnecessary validator calls=%d", validations)
				}
				if level == EnforcementStrict && tc.profile && validations == 0 {
					t.Fatal("profile check omitted")
				}
			}
		})
	}
	for _, tc := range []struct{ name, body, version, leg, direction string }{
		{"null returns", `{"resourceType":"Parameters","parameter":null}`, "pa.pas@2.2", "pas-claim-inquire", "response"},
		{"object returns", `{"resourceType":"Parameters","parameter":{}}`, "pa.pas@2.2", "pas-claim-inquire", "response"},
		{"wrong return name", `{"resourceType":"Parameters","parameter":[{"name":"other","resource":{"resourceType":"Bundle","type":"collection"}}]}`, "pa.pas@2.2", "pas-claim-inquire", "response"},
		{"old wrapper", inquiryDeepReturns(), "pa.pas@2.0", "pas-claim-inquire", "response"},
		{"undeclared wrapper", inquiryDeepReturns(), "", "pas-claim-inquire", "response"},
		{"unknown declaration", inquiryDeepReturns(), "pa.pas@9.9", "pas-claim-inquire", "response"},
		{"submit wrapper", inquiryDeepReturns(), "pa.pas@2.2", "pas-claim", "response"},
		{"inquiry request", inquiryDeepReturns(), "pa.pas@2.2", "pas-claim-inquire", "request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := inquiryDeepInput(tc.body, tc.version)
			in.Exchange.legType, in.Direction = tc.leg, tc.direction
			for _, id := range []string{"fhir.profile", "fhir.terminology", "patient.consistency"} {
				if !deepRule(t, &Gateway{}, id).Applies(in) {
					t.Fatalf("%s incorrectly waived", id)
				}
			}
		})
	}
}

func TestSubjectConsistencyIndependentScopes(t *testing.T) {
	single := inquiryDeepBundle(inquiryPatientEntry, inquiryAnswerEntry)
	contained := `{"fullUrl":"urn:uuid:answer","resource":{"resourceType":"ClaimResponse","patient":{"reference":"#patient"},"contained":[{"resourceType":"Patient","id":"patient","identifier":[{"system":"urn:source","value":"a"}]},{"resourceType":"Coverage","id":"coverage","beneficiary":{"reference":"#patient"}}]}}`
	for _, tc := range []struct {
		name, body string
		want       CheckState
		values     string
	}{
		{"independent same URLs", inquiryDeepReturns(single, single), CheckValid, "a,a"},
		{"same URLs different patients", inquiryDeepReturns(single, strings.Replace(single, `"value":"a"`, `"value":"b"`, 1)), CheckInvalid, "a,b"},
		{"sibling only target", inquiryDeepReturns(inquiryDeepBundle(inquiryAnswerEntry), single), CheckUnavailable, "a"},
		{"duplicate URL within bundle", inquiryDeepReturns(inquiryDeepBundle(inquiryPatientEntry, inquiryPatientEntry, inquiryAnswerEntry)), CheckUnavailable, ""},
		{"contained owner and sibling", inquiryDeepReturns(inquiryDeepBundle(contained)), CheckValid, "a,a"},
		{"contained independent owners", inquiryDeepReturns(inquiryDeepBundle(contained, strings.ReplaceAll(contained, "urn:uuid:answer", "urn:uuid:second"))), CheckValid, "a,a,a,a"},
		{"contained independent differing patients", inquiryDeepReturns(inquiryDeepBundle(contained, strings.Replace(strings.ReplaceAll(contained, "urn:uuid:answer", "urn:uuid:second"), `"value":"a"`, `"value":"b"`, 1))), CheckInvalid, "a,a,b,b"},
		{"nested Bundle scope", inquiryDeepReturns(inquiryDeepBundle(inquiryPatientEntry, inquiryAnswerEntry, `{"fullUrl":"urn:uuid:nested","resource":`+single+`}`)), CheckValid, "a,a"},
		{"nested Bundle cannot use parent target", inquiryDeepReturns(inquiryDeepBundle(inquiryPatientEntry, inquiryAnswerEntry, `{"fullUrl":"urn:uuid:nested","resource":`+inquiryDeepBundle(inquiryAnswerEntry)+`}`)), CheckUnavailable, "a"},
		{"contained sibling owner forbidden", inquiryDeepReturns(inquiryDeepBundle(contained, `{"fullUrl":"urn:uuid:second","resource":{"resourceType":"ClaimResponse","patient":{"reference":"#patient"}}}`)), CheckUnavailable, "a,a"},
		{"duplicate contained ID", inquiryDeepReturns(inquiryDeepBundle(strings.Replace(contained, `"id":"coverage"`, `"id":"patient"`, 1))), CheckUnavailable, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var values []string
			g := &Gateway{cfg: Config{SubjectReferenceResolver: subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
				if ref.Holder != "payer" || ref.System != "urn:source" {
					t.Fatalf("wrong scope: %+v", ref)
				}
				values = append(values, ref.Value)
				return "pci-" + ref.Value, true, nil
			})}}
			in := inquiryDeepInput(tc.body, "pa.pas@2.2")
			rule := deepRule(t, g, "patient.consistency")
			if got := rule.Check(context.Background(), in); got.State != tc.want {
				t.Fatalf("identity=%+v want %s; refs=%v", got, tc.want, values)
			}
			if got := strings.Join(values, ","); got != tc.values {
				t.Fatalf("authoritative evidence=%q want %q", got, tc.values)
			}
			inquiryDeepModeTwins(t, g, in, rule, tc.want)
		})
	}
}
