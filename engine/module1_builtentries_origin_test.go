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

// The built request's spelling checks (builtEntriesNamed) reach only records
// the gateway read from its own participant's system, so each refusal is 502,
// the fidelity of the gateway's own build, on a submission and on an
// amendment alike.
func TestBuiltEntriesNamed_RefusesASystemOfRecordRecord502(t *testing.T) {
	spellTwice := func(record []byte) []byte {
		out := bytes.Replace(record, []byte(`"id":`), []byte(`"ID":"x","id":`), 1)
		if bytes.Equal(out, record) {
			t.Fatal("the record holds no id to spell twice")
		}
		return out
	}
	const twice = "a record read from the system of record cannot be carried as read: the build's own id for it cannot be set beside the id it spells another way"
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for name, build := range map[string]func() error{
			"a submission's order": func() error {
				in := attachmentInputs("ServiceRequest", line, false)
				in.SR = spellTwice(in.SR)
				_, err := buildAuthoredPASSubmit(line, in)
				return err
			},
			"an amendment's order": func() error {
				in := attachmentUpdateInputs(line, false, true)
				in.SR = spellTwice(in.SR)
				_, err := buildAuthoredPASUpdate(line, in)
				return err
			},
		} {
			err := build()
			if !errors.Is(err, errRecordNotCarried) || err.Error() != twice {
				t.Fatalf("%s at %s: %v, want %q", name, line, err, twice)
			}
			if got := buildFidelityStatus(err); got != http.StatusBadGateway {
				t.Fatalf("%s at %s: status %d, want 502", name, line, got)
			}
		}
	}
}

// The PAS submission a dispatch, UC-03's oxygen order or a provider-data
// request builds, at every level: a record read from the system of record that
// spells its resourceType or id only another way, or an order (which the build
// re-ids) spelling its id twice, is what the gateway read from its own
// participant's system: 502, the fidelity of the build, with no Retry-After.
// (On UC-05's route an order spelled so is refused earlier, at the order-sign
// build, which reads it by its exact members.)
func TestSubmitPASClaim_SpelledSystemOfRecordRecordIsRefused502(t *testing.T) {
	const (
		member   = "MBR-COVERED"
		idOnly   = "it states its resourceType or id only in another spelling"
		reIDOnly = "the build's own id for it cannot be set beside the id it spells another way"
	)
	spell := func(b []byte, with string) []byte {
		out := bytes.Replace(b, []byte(`"id":`), []byte(with), 1)
		if bytes.Equal(out, b) {
			t.Fatal("the record holds no id to spell")
		}
		return out
	}
	type inputs struct{ order, supplier, coverage, insurer []byte }
	for name, row := range map[string]struct {
		edit func(*inputs)
		want string
	}{
		"the order spells its id twice": {func(in *inputs) { in.order = spell(in.order, `"ID":"x","id":`) }, reIDOnly},
		"the order spells its id as ID": {func(in *inputs) { in.order = spell(in.order, `"ID":`) }, reIDOnly},
		"the Coverage spells its id":    {func(in *inputs) { in.coverage = spell(in.coverage, `"ID":`) }, idOnly},
		"the payer Organization spells": {func(in *inputs) { in.insurer = spell(in.insurer, `"ID":`) }, idOnly},
		"the supplier spells its id":    {func(in *inputs) { in.supplier = spell(in.supplier, `"ID":`) }, idOnly},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
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
				order, err := buildDispatchDeviceRequest("dr-tail", "Patient/"+member, "Organization/supplier-tail", "E1390", "Stationary oxygen concentrator", "J96.10")
				if err != nil {
					t.Fatal(err)
				}
				in := inputs{order: order, supplier: []byte(`{"resourceType":"Organization","id":"supplier-tail","name":"Tail supplier"}`), coverage: realCov, insurer: realPayerOrg}
				row.edit(&in)
				x := &exchangeRecorder{}
				ctx := context.WithValue(env.ctx, exchangeRecorderKey{}, x)
				req := claimRequest(testOrderBody(in.order, "")).WithContext(ctx)
				_, status, msg, err = env.originator.submitPASClaim(ctx, req, pasFollowInputs{pci: "pci-1", orderJSON: in.order, supplierJSON: in.supplier, sourcesRead: true, coverage: in.coverage, insurer: in.insurer, patientRef: "Patient/" + member, coverageRef: "Coverage/" + member, member: member, memberSystem: recs.memberSystem, payer: shnsdk.CMSPayerIdentity, recipient: env.payerID})
				if err != nil || status != http.StatusBadGateway || !strings.Contains(msg, errRecordNotCarried.Error()+": "+row.want) {
					t.Fatalf("submitPASClaim = %d %q %v, want the 502 fidelity refusal %q", status, msg, err, row.want)
				}
				if x.outcome != ExchangeRefused || x.rule != RefusalFidelity || x.refusalStage != vocab.RefusalStageBuild {
					t.Fatalf("recorded %s %s/%s, want refused fidelity/build", x.outcome, x.rule, x.refusalStage)
				}
			})
		}
	}
}

// The facility's report and the Provenance attributing it are UC-05's only
// entries that are a peer's message, and neither reaches builtEntriesNamed.
// A Provenance that states its id only in another spelling is refused first
// by the SDK's Carried build: 422, the fidelity of SHN's own build, at every
// level, and no claim update is sent. (The route fixture's validator is the
// fake, so this row does not pin what a real $validate does at structural or
// strict; ingress validation is the participant's opt-in conformance, and at
// none and observe it refuses nothing.)
func TestHandleUC05_FacilityProvenanceWithNoExactIDIsRefusedByTheSDKBuild(t *testing.T) {
	const route = "POST /scenario/uc05"
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			gw, stub := uc05Fixture(t, level, func(n int, p []byte) []byte {
				if n != 1 {
					return p
				}
				return withFacilityRecords(t, p, func(entries []any) []any {
					prov := facilityEntry(t, entries, "Provenance")
					prov["ID"] = prov["id"]
					delete(prov, "id")
					return entries
				})
			})
			rec, recs := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc05", ""))
			body := rec.Body.String()
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "the Provenance attributing the report has no id") ||
				!strings.Contains(body, "(fidelity of SHN's own build)") || strings.Contains(body, errRecordNotCarried.Error()) {
				t.Fatalf("status %d: %s; want the SDK build's 422", rec.Code, body)
			}
			if legAttempted(stub.legTypes, "pas-claim-update") {
				t.Fatalf("a claim update was sent (legs: %v)", stub.legTypes)
			}
			r := onlyRecord(t, recs)
			wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
			if r.RefusalStage != vocab.RefusalStageBuild {
				t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
			}
		})
	}
}
