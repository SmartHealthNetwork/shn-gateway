package engine

import (
	"context"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestPASConsumptionRetainedSourceEcho(t *testing.T) {
	const sent = `{"resourceType":"Bundle","entry":[{"fullUrl":"https://shn.example/fhir/Claim/c1","resource":{"resourceType":"Claim","id":"c1","identifier":[{"system":"urn:claim","value":"c1"}],"patient":{"reference":"Patient/known"}}},{"fullUrl":"https://shn.example/fhir/Patient/known","resource":{"resourceType":"Patient","id":"known","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"known"}]}}]}`
	for _, tc := range []struct {
		name, response      string
		linked, otherLinked bool
		want                int
	}{
		{"exact echo", `{"resourceType":"ClaimResponse","request":{"identifier":{"system":"urn:claim","value":"c1"}},"patient":{"reference":"Patient/known"}}`, true, false, 0},
		{"unresolved other patient", `{"resourceType":"ClaimResponse","request":{"identifier":{"system":"urn:claim","value":"c1"}},"patient":{"reference":"Patient/other"}}`, true, false, 503},
		{"source-proven other patient", `{"resourceType":"ClaimResponse","request":{"identifier":{"system":"urn:claim","value":"c1"}},"patient":{"reference":"Patient/other"}}`, true, true, 502},
		{"unrelated request", `{"resourceType":"ClaimResponse","request":{"identifier":{"system":"urn:claim","value":"other"}},"patient":{"reference":"Patient/known"}}`, true, false, 502},
		{"missing request linkage", `{"resourceType":"ClaimResponse","patient":{"reference":"Patient/known"}}`, true, false, 503},
		{"missing source link", `{"resourceType":"ClaimResponse","request":{"identifier":{"system":"urn:claim","value":"c1"}},"patient":{"reference":"Patient/known"}}`, false, false, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &Gateway{cfg: Config{HolderID: "provider", SubjectReferenceResolver: subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
				if ref.Holder == "provider" && ref.System == shnsdk.MemberSystem && ref.Value == "known" && tc.linked {
					return "pci:issued", true, nil
				}
				if ref.Holder == "payer" && ref.System == "fhir-relative" && ref.Value == "Patient/other" && tc.otherLinked {
					return "pci:other", true, nil
				}
				return "", false, nil
			})}}
			sub := pasSubmission{bundleJSON: []byte(sent), respJSON: []byte(tc.response), leg: "pas-claim"}
			status, _ := g.validatePASConsumption(context.Background(), sub, "pci:issued", "payer")
			if status != tc.want {
				t.Fatalf("status=%d want %d", status, tc.want)
			}
		})
	}
}

func TestPASInquiryConsumptionRetainedSourceEcho(t *testing.T) {
	const sent = `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","id":"inquiry","patient":{"reference":"Patient/known"}}},{"resource":{"resourceType":"Patient","id":"known","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"known"}]}}]}`
	for _, tc := range []struct {
		name, response      string
		linked, otherLinked bool
		want                int
	}{
		{"exact echo", `{"resourceType":"ClaimResponse","request":{"reference":"Claim/original"},"patient":{"reference":"Patient/known"}}`, true, false, 0},
		{"unresolved other patient", `{"resourceType":"ClaimResponse","request":{"reference":"Claim/original"},"patient":{"reference":"Patient/other"}}`, true, false, 503},
		{"source-proven other patient", `{"resourceType":"ClaimResponse","request":{"reference":"Claim/original"},"patient":{"reference":"Patient/other"}}`, true, true, 502},
		{"missing request linkage", `{"resourceType":"ClaimResponse","patient":{"reference":"Patient/known"}}`, true, false, 503},
		{"missing source link", `{"resourceType":"ClaimResponse","request":{"reference":"Claim/original"},"patient":{"reference":"Patient/known"}}`, false, false, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &Gateway{cfg: Config{HolderID: "provider", SubjectReferenceResolver: subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
				if ref.Holder == "provider" && ref.System == shnsdk.MemberSystem && ref.Value == "known" && tc.linked {
					return "pci:issued", true, nil
				}
				if ref.Holder == "payer" && ref.System == "fhir-relative" && ref.Value == "Patient/other" && tc.otherLinked {
					return "pci:other", true, nil
				}
				return "", false, nil
			})}}
			selected := shnsdk.PASInquirySelection{Response: []byte(tc.response), Bundle: []byte(tc.response)}
			status, _ := g.validateInquiryPatient(context.Background(), selected, "pci:issued", "payer", []byte(sent))
			if status != tc.want {
				t.Fatalf("status=%d want %d", status, tc.want)
			}
		})
	}
}

func TestPASRetainedEchoRefusesAmbiguousSentClaim(t *testing.T) {
	const sent = `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","id":"c1","patient":{"reference":"Patient/known"}}},{"resource":{"resourceType":"Claim","id":"c2","patient":{"reference":"Patient/other"}}},{"resource":{"resourceType":"Patient","id":"known","identifier":[{"system":"urn:shn:pci","value":"pci:issued"}]}}]}`
	const answer = `{"resourceType":"ClaimResponse","request":{"reference":"Claim/c1"},"patient":{"reference":"Patient/known"}}`
	g := &Gateway{cfg: Config{HolderID: "provider", SubjectReferenceResolver: subjectResolverFunc(func(context.Context, PatientReference) (string, bool, error) {
		return "pci:issued", true, nil
	})}}
	if status, _ := g.validateRetainedPatientEcho(context.Background(), []byte(sent), []byte(answer), "pci:issued"); status != 503 {
		t.Fatalf("ambiguous sent Claim status=%d, want unavailable", status)
	}
}

func TestPASConsumptionRetainedAmendmentClaimEcho(t *testing.T) {
	const patient = `{"resourceType":"Patient","id":"known","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"known"}]}`
	const operative = `{"resourceType":"Claim","id":"update","patient":{"reference":"Patient/known"},"related":[{"claim":{"reference":"Claim/prior"}}]}`
	const prior = `{"resourceType":"Claim","id":"prior","patient":{"reference":"Patient/known"}}`
	const answer = `{"resourceType":"ClaimResponse","request":{"reference":"Claim/prior"},"patient":{"reference":"Patient/known"}}`
	for _, tc := range []struct {
		name, operative, prior string
		reverse                bool
		want                   int
	}{
		{"related original Claim", operative, prior, false, 0},
		{"unrelated second Claim", `{"resourceType":"Claim","id":"update","patient":{"reference":"Patient/known"}}`, prior, false, 502},
		{"prior Claim different patient", operative, `{"resourceType":"Claim","id":"prior","patient":{"reference":"Patient/other"}}`, false, 503},
		{"prior Claim first", operative, prior, true, 503},
		{"duplicate prior relation", `{"resourceType":"Claim","id":"update","patient":{"reference":"Patient/known"},"related":[{"claim":{"reference":"Claim/prior"}},{"claim":{"reference":"Claim/prior"}}]}`, prior, false, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := tc.operative, tc.prior
			if tc.reverse {
				first, second = second, first
			}
			sent := []byte(`{"resourceType":"Bundle","entry":[{"resource":` + first + `},{"resource":` + patient + `},{"resource":` + second + `}]}`)
			g := &Gateway{cfg: Config{HolderID: "provider", SubjectReferenceResolver: subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
				if ref.Holder == "provider" && ref.System == shnsdk.MemberSystem && ref.Value == "known" {
					return "pci:issued", true, nil
				}
				return "", false, nil
			})}}
			sub := pasSubmission{bundleJSON: sent, respJSON: []byte(answer), leg: "pas-claim-update"}
			status, _ := g.validatePASConsumption(context.Background(), sub, "pci:issued", "payer")
			if status != tc.want {
				t.Fatalf("status=%d want %d", status, tc.want)
			}
		})
	}
}
