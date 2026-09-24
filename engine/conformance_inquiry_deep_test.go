package engine

import (
	"context"
	"strings"
	"testing"
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
