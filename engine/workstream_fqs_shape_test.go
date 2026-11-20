//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
)

// The facility's gateway answers a federated query by extending the
// requester's own Task, so a request it cannot extend is refused 422 at every
// level, with no fulfillment sent, under the network rule it rests on: a body
// that repeats a member name (exactly, or equal under case folding) can be
// read two ways, so message integrity, as can one that is not one JSON
// document; a Task nested past the relay's limits on a body the gateway
// transmits (past the gateway's own bound), a root that is not a Task (its
// resourceType named exactly), a contained or output that is not an array, or
// a contained or output named only in another case, which the fulfillment's
// own member would repeat, is the fidelity of the gateway's own edit, at the
// build. Each shape reaches the refusal past the query's own reading
// (cdexTaskQuery), which reads names in any case and ignores both members.
func TestFederatedQueryInbound_ARequestTheFulfillmentCannotExtendIsRefused(t *testing.T) {
	type want struct {
		reason, rule, stage string
	}
	integrity := want{refusalCDexNotOneTask, RefusalIntegrity, ""}
	notATask := want{refusalCDexNotOneTask, RefusalFidelity, vocab.RefusalStageBuild}
	folded := want{refusalCDexFoldedMember, RefusalFidelity, vocab.RefusalStageBuild}
	pastLimits := want{refusalCDexPastLimits, RefusalFidelity, vocab.RefusalStageBuild}
	// An extension nested past the relay's depth limit on a body the gateway
	// transmits, which the query's own reading takes.
	deep := `"resourceType":"Task","extension":[{"url":"http://example.org/deep","valueX":` + strings.Repeat(`{"a":`, 62) + `1` + strings.Repeat(`}`, 62) + `}]`
	rows := []struct {
		name     string
		from, to string
		refused  *want
	}{
		{"no contained or output, and none named in another case: extended", "", "", nil},
		{"contained and output named exactly, as arrays: extended", `"resourceType":"Task"`, `"resourceType":"Task","contained":[],"output":[]`, nil},
		{"a repeated member", `{"authoredOn":`, `{"status":"requested","authoredOn":`, &integrity},
		{"a member repeated under case folding", `{"authoredOn":`, `{"STATUS":"requested","authoredOn":`, &integrity},
		{"a repeated member inside an input", `"input":[`, `"input":[{"type":{"text":"a","text":"b"}},`, &integrity},
		{"a string the query's reading takes that is not JSON text (a lone surrogate)", `"resourceType":"Task"`, `"resourceType":"Task","note":[{"text":"\ud800"}]`, &integrity},
		{"nested past the gateway's own limits", `"resourceType":"Task"`, deep, &pastLimits},
		{"resourceType named only in another case", `"resourceType":"Task"`, `"RESOURCETYPE":"Task"`, &notATask},
		{"contained not an array", `"resourceType":"Task"`, `"resourceType":"Task","contained":{}`, &notATask},
		{"output not an array", `"resourceType":"Task"`, `"resourceType":"Task","output":"x"`, &notATask},
		{"output named Output", `"resourceType":"Task"`, `"resourceType":"Task","Output":[]`, &folded},
		{"output named OUTPUT, not an array", `"resourceType":"Task"`, `"resourceType":"Task","OUTPUT":{}`, &folded},
		{"contained named Contained", `"resourceType":"Task"`, `"resourceType":"Task","Contained":[]`, &folded},
	}
	// The network's refusals of the request itself come before the
	// facility's decline: each fault, in a Task whose status does not allow
	// it to be fulfilled, is refused under the same rule, never declined.
	type tcase struct {
		name, from, to string
		refused        *want
		closed         bool
	}
	var cases []tcase
	for _, r := range rows {
		cases = append(cases, tcase{r.name, r.from, r.to, r.refused, false})
	}
	for _, r := range rows {
		if r.refused != nil {
			cases = append(cases, tcase{r.name + ", in a closed Task", r.from, r.to, r.refused, true})
		}
	}
	for _, level := range bindLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.answering(t, "federated-query")
			p.g.cfg.ConsentURL = "http://consent.test"
			p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
			// A facility holding a record of the member's, so a Task it can
			// extend is extended with it.
			sor := &facilitySoR{censusSoR: newCensusSoR(), patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("", facDR1)}}}
			sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
			p.g.cfg.SoR = sor
			pci, _, ok := sor.ResolvePatient("MBR-UC05")
			if !ok {
				t.Fatal("the facility resolves no patient for MBR-UC05")
			}
			for _, row := range cases {
				task := cdexTaskFor(t, "MBR-UC05")
				if row.closed {
					task = replaceOnce(t, task, `"status":"requested"`, `"status":"cancelled"`)
				}
				if row.from != "" {
					task = replaceOnce(t, task, row.from, row.to)
				}
				if _, _, err := cdexTaskQuery(task); err != nil {
					t.Fatalf("%s: the query's own reading refuses it first: %v", row.name, err)
				}
				got.reset()
				ans := sendConsented(t, p, "federated-query", task, pci, "Consent/c-1")
				rec := got.only(t)
				if row.refused == nil {
					if ans.status != http.StatusOK || rec.Outcome != ExchangeAnswered || !bytes.Contains(ans.body, []byte(`"output"`)) {
						t.Fatalf("%s: answered %d (%s), want the Task extended: %s", row.name, ans.status, rec.Outcome, ans.body)
					}
					continue
				}
				w := row.refused
				if ans.status != http.StatusUnprocessableEntity || !bytes.Contains(ans.body, []byte(w.reason)) || bytes.Contains(ans.body, []byte(`"output"`)) {
					t.Fatalf("%s: answered %d %s, want 422 %q", row.name, ans.status, ans.body, w.reason)
				}
				if rec.Outcome != ExchangeRefused || rec.Status != http.StatusUnprocessableEntity || rec.Rule != w.rule || rec.RefusalStage != w.stage {
					t.Fatalf("%s: record %s %s/%s status %d, want refused 422 %s/%s", row.name, rec.Outcome, rec.Rule, rec.RefusalStage, rec.Status, w.rule, w.stage)
				}
			}
		})
	}
}

// A request Task whose status does not allow it to be fulfilled is the
// facility's own decision, not the network's: its gateway builds the
// fulfillment as the facility, so it answers as the facility would answer a
// change its Task state machine does not allow, the same at every level (it
// is no conformance check). The answer is 422 with an OperationOutcome whose
// one issue is a business rule, in the facility's own words, built as
// cdex-decline; the record is the facility's error answer (upstream-error,
// the answer's builder cdex-decline), with no refusal and no Retry-After.
func TestFederatedQueryInbound_AClosedTaskIsDeclinedAsTheFacility(t *testing.T) {
	for _, level := range bindLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.answering(t, "federated-query")
			p.g.cfg.ConsentURL = "http://consent.test"
			p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
			sor := &facilitySoR{censusSoR: newCensusSoR(), patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("", facDR1)}}}
			sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
			p.g.cfg.SoR = sor
			pci, _, ok := sor.ResolvePatient("MBR-UC05")
			if !ok {
				t.Fatal("the facility resolves no patient for MBR-UC05")
			}
			for _, row := range []struct {
				status string
				signed bool
			}{{"cancelled", false}, {"completed", false}, {"cancelled", true}} {
				status := row.status
				task := replaceOnce(t, cdexTaskFor(t, "MBR-UC05"), `"status":"requested"`, `"status":"`+status+`"`)
				if row.signed {
					// Signed content bears only on an edit, and a decline makes
					// none: a signed Task that is closed is declined, not refused
					// on the fidelity of the gateway's own edit.
					task = replaceOnce(t, task, `"resourceType":"Task"`, `"resourceType":"Task","extension":[{"url":"http://example.org/sig","valueSignature":`+fqsTestSignature+`}]`)
					status += ", signed"
				}
				if _, _, err := cdexTaskQuery(task); err != nil {
					t.Fatalf("%s: the query's own reading refuses it first: %v", status, err)
				}
				got.reset()
				ans := sendConsented(t, p, "federated-query", task, pci, "Consent/c-1")
				if !ans.framed || ans.status != http.StatusUnprocessableEntity || ans.contentType != "application/fhir+json" {
					t.Fatalf("%s: answered %d %q (framed %v): %s", status, ans.status, ans.contentType, ans.framed, ans.body)
				}
				for name := range ans.frame {
					if strings.EqualFold(name, "Retry-After") {
						t.Fatalf("%s: the decline names a Retry-After: %v", status, ans.frame)
					}
				}
				var oo struct {
					ResourceType string `json:"resourceType"`
					Issue        []struct {
						Severity, Code, Diagnostics string
					} `json:"issue"`
				}
				if err := json.Unmarshal(ans.body, &oo); err != nil {
					t.Fatalf("%s: %v: %s", status, err, ans.body)
				}
				want := "the Task's status (" + row.status + ") does not allow it to be fulfilled"
				if oo.ResourceType != "OperationOutcome" || len(oo.Issue) != 1 || oo.Issue[0].Severity != "error" || oo.Issue[0].Code != "business-rule" || oo.Issue[0].Diagnostics != want {
					t.Fatalf("%s: answer %s, want one business-rule issue %q", status, ans.body, want)
				}
				rec := got.only(t)
				if rec.Outcome != ExchangeUpstreamError || rec.Status != http.StatusUnprocessableEntity ||
					rec.AnswerOwnership != "authored" || rec.AnswerBuilder != string(relay.BuilderCDexDecline) {
					t.Fatalf("%s: record %s %d answer %s/%s, want the facility's error answer: upstream-error 422 authored/cdex-decline", status, rec.Outcome, rec.Status, rec.AnswerOwnership, rec.AnswerBuilder)
				}
				if rec.RefusedBy != "" || rec.Rule != "" || rec.RefusalStage != "" || rec.RefusalReason != "" {
					t.Fatalf("%s: the decline is recorded as a refusal: by %q rule %q stage %q reason %q", status, rec.RefusedBy, rec.Rule, rec.RefusalStage, rec.RefusalReason)
				}
				if rec.Backend != nil && rec.Backend.ErrorClass != "" {
					t.Fatalf("%s: the facility's system is recorded as failing (%s)", status, rec.Backend.ErrorClass)
				}
			}
		})
	}
}

// fqsTestSignature is a complete R4 Signature, which every reading takes as
// one.
const fqsTestSignature = `{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-06-04T00:00:00Z","who":{"reference":"Organization/payer"}}`
