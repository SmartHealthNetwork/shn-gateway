//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A requesting provider that is an Organization with the payer Organization's
// id is a record read from the participant's own system the build cannot
// carry as read: the request carries each as an entry of its own. It is
// refused before the SDK's build, on a submission and on an amendment at each
// PAS line, 502 by buildFidelityStatus, where the SDK's own error left it the
// gateway's 500. A provider of another type, or another id, builds as before.
// The amendment is pinned here rather than on a route: every amendment names
// the payer its submission named and a provider selected from the same order
// and requester records (UC-04 and UC-05 reuse the submission's; a resumed
// amendment selects it again from them), and that submission is refused first
// (TestSubmitPASClaim_ProviderThatIsThePayersRecordIsRefused502).
func TestPASBuild_ProviderThatIsThePayersRecordIsRefused502(t *testing.T) {
	const want = "a record read from the system of record cannot be carried as read: the requesting provider is an Organization with the payer Organization's id (Organization/org-cms-payer), and the request carries each as an entry of its own"
	payersRecord := testPayerOrganization(shnsdk.CMSPayerIdentity)
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for name, build := range map[string]func(provider []byte) error{
			"a submission": func(provider []byte) error {
				in := attachmentInputs("ServiceRequest", line, false)
				in.Provider = provider
				_, err := buildAuthoredPASSubmit(line, in)
				return err
			},
			"an amendment": func(provider []byte) error {
				in := attachmentUpdateInputs(line, false, true)
				in.Provider = provider
				_, err := buildAuthoredPASUpdate(line, in)
				return err
			},
		} {
			err := build(payersRecord)
			if !errors.Is(err, errRecordNotCarried) || err.Error() != want {
				t.Fatalf("%s at %s: %v, want %q", name, line, err, want)
			}
			if got := buildFidelityStatus(err); got != http.StatusBadGateway {
				t.Fatalf("%s at %s: status %d, want 502", name, line, got)
			}
			if err := build(testRequestingProvider()); err != nil {
				t.Fatalf("%s at %s with another provider: %v", name, line, err)
			}
		}
	}
	for name, row := range map[string]struct{ provider, insurer string }{
		"a PractitionerRole with the payer's id": {`{"resourceType":"PractitionerRole","id":"org-cms-payer"}`, string(payersRecord)},
		"an Organization with another id":        {`{"resourceType":"Organization","id":"org-other"}`, string(payersRecord)},
		"no provider id":                         {`{"resourceType":"Organization"}`, `{"resourceType":"Organization"}`},
		"a provider that is not JSON":            {`{`, string(payersRecord)},
		"an insurer that is not JSON":            {`{"resourceType":"Organization","id":"org-cms-payer"}`, `{`},
	} {
		if err := providerIsThePayersRecord([]byte(row.provider), []byte(row.insurer)); err != nil {
			t.Errorf("%s: %v, want none", name, err)
		}
	}
}

// The PAS submission a dispatch, UC-03's oxygen order or a provider-data
// request builds, at every level: the dispatched supplier, the Claim's
// provider, read from the system of record as an Organization with the payer
// Organization's id, is refused 502, the fidelity of the build, and no PAS
// claim is sent. Before, the SDK's own error left it the gateway's 500.
func TestSubmitPASClaim_ProviderThatIsThePayersRecordIsRefused502(t *testing.T) {
	const member = "MBR-COVERED"
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			recs, status, msg, _ := env.originator.originCRDRecords(env.ctx, "crd-order-select", member)
			if status != 0 {
				t.Fatalf("originCRDRecords: %d %s", status, msg)
			}
			realCov, found, status, msg := env.originator.memberCoverage(env.ctx, member)
			if status != 0 || !found {
				t.Fatalf("memberCoverage: status=%d found=%v %s", status, found, msg)
			}
			pid, routed, _, _, _ := env.originator.routedPayerIdentifier(env.ctx, realCov)
			if !routed {
				t.Fatal("the member's coverage routes to no payer")
			}
			realPayerOrg, _, status, msg, _ := env.originator.memberPayerOrganization(env.ctx, realCov, pid)
			if status != 0 {
				t.Fatalf("memberPayerOrganization: %d %s", status, msg)
			}
			var payerID string
			if m, err := exactMembers(realPayerOrg); err == nil {
				payerID = exactString(m, "id")
			}
			if payerID == "" {
				t.Fatal("the payer's Organization record has no id")
			}
			order, err := buildDispatchDeviceRequest("dr-tail", "Patient/"+member, "Organization/"+payerID, "E1390", "Stationary oxygen concentrator", "J96.10")
			if err != nil {
				t.Fatal(err)
			}
			supplier := bytes.Replace([]byte(`{"resourceType":"Organization","id":"ID","name":"A supplier"}`), []byte(`"ID"`), []byte(`"`+payerID+`"`), 1)
			x := &exchangeRecorder{}
			ctx := context.WithValue(env.ctx, exchangeRecorderKey{}, x)
			req := claimRequest(testOrderBody(order, "")).WithContext(ctx)
			_, status, msg, err = env.originator.submitPASClaim(ctx, req, pasFollowInputs{pci: "pci-1", orderJSON: order, supplierJSON: supplier, sourcesRead: true, coverage: realCov, insurer: realPayerOrg, patientRef: "Patient/" + member, coverageRef: "Coverage/" + member, member: member, memberSystem: recs.memberSystem, payer: shnsdk.CMSPayerIdentity, recipient: env.payerID})
			if err != nil || status != http.StatusBadGateway || !strings.Contains(msg, "the requesting provider is an Organization with the payer Organization's id (Organization/"+payerID+")") {
				t.Fatalf("submitPASClaim = %d %q %v, want the 502 fidelity refusal", status, msg, err)
			}
			if x.outcome != ExchangeRefused || x.rule != RefusalFidelity || x.refusalStage != vocab.RefusalStageBuild {
				t.Fatalf("recorded %s %s/%s, want refused fidelity/build", x.outcome, x.rule, x.refusalStage)
			}
			env.substrate.mu.Lock()
			sent := append([]string(nil), env.substrate.sentNames...)
			env.substrate.mu.Unlock()
			if len(sent) != 0 {
				t.Fatalf("sent %v: want nothing sent, the PAS claim among it", sent)
			}
		})
	}
}
