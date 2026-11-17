//go:build !shn_coreonly

package engine

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The facility's gateway answers a federated query by extending the
// requester's own Task, so it judges whether a signature covers that Task by
// the relay's rule, the one every edit it makes is held to, whatever the
// SDK's fulfillment reads: a Signature at a place FHIR R4 types Signature
// marks the Task signed though it lacks a member R4 requires, and the query is
// refused 422 without extending it, answered and recorded as a Task the SDK
// reads as signed is: on the fidelity of the gateway's own edit, at the
// build, so at every level.
func TestFederatedQueryInbound_ATaskTheRelayReadsAsSignedIsNotExtended(t *testing.T) {
	const bare = `{"data":"c2ln"}`
	const full = `{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-01-01T00:00:00Z","who":{"reference":"Organization/provider"}}`
	inExtension := func(sig string) func(*testing.T, []byte) []byte {
		return func(t *testing.T, task []byte) []byte {
			return replaceOnce(t, task, `"resourceType":"Task",`, `"resourceType":"Task","extension":[{"url":"http://example.org/sig","valueSignature":`+sig+`}],`)
		}
	}
	rows := []struct {
		name string
		edit func(*testing.T, []byte) []byte
		// sdkBuilds: the SDK's fulfillment reads no signature here, so a
		// refusal is the relay's reading alone.
		signed, sdkBuilds bool
	}{
		{"no Signature: extended", func(_ *testing.T, task []byte) []byte { return task }, false, true},
		{"a Signature in an extension, lacking members R4 requires", inExtension(bare), true, true},
		{"a Signature in an input's valueSignature, lacking members R4 requires", func(t *testing.T, task []byte) []byte {
			return replaceOnce(t, task, `"input":[`, `"input":[{"type":{"text":"signature"},"valueSignature":`+bare+`},`)
		}, true, true},
		{"a signed Provenance it contains whose target is named TARGET", func(t *testing.T, task []byte) []byte {
			return replaceOnce(t, task, `"resourceType":"Task",`, `"resourceType":"Task","contained":[{"resourceType":"Provenance","id":"p1",`+
				`"TARGET":[{"reference":"#"}],"recorded":"2026-01-01T00:00:00Z","agent":[{"who":{"reference":"Organization/provider"}}],"signature":[`+bare+`]}],`)
		}, true, true},
		{"a signed Provenance it contains that targets it by an id named ID", func(t *testing.T, task []byte) []byte {
			return replaceOnce(t, task, `"resourceType":"Task",`, `"resourceType":"Task","ID":"t1","contained":[{"resourceType":"Provenance","id":"p1",`+
				`"target":[{"reference":"Task/t1"}],"recorded":"2026-01-01T00:00:00Z","agent":[{"who":{"reference":"Organization/provider"}}],"signature":[`+bare+`]}],`)
		}, true, true},
		// The refusal the SDK's reading already made: the rows above are
		// answered and recorded as it is.
		{"a full Signature in an extension", inExtension(full), true, false},
	}
	for _, level := range bindLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.answering(t, "federated-query")
			p.g.cfg.ConsentURL = "http://consent.test"
			p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
			// A facility holding a record of the member's, so a Task no
			// signature covers is extended with it.
			sor := &facilitySoR{censusSoR: newCensusSoR(), patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("", facDR1)}}}
			sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
			p.g.cfg.SoR = sor
			pci, _, ok := sor.ResolvePatient("MBR-UC05")
			if !ok {
				t.Fatal("the facility resolves no patient for MBR-UC05")
			}
			var signedAnswer *payerAnswer
			var signedRecord ExchangeRecord
			for _, row := range rows {
				task := row.edit(t, cdexTaskFor(t, "MBR-UC05"))
				_, err := shnsdk.BuildCDexFulfillment(task, []byte(`{"resourceType":"Bundle","id":"r1","type":"collection"}`))
				if (err == nil) != row.sdkBuilds {
					t.Fatalf("%s: the SDK's fulfillment: %v, want it to build: %v", row.name, err, row.sdkBuilds)
				}
				got.reset()
				ans := sendConsented(t, p, "federated-query", task, pci, "Consent/c-1")
				rec := got.only(t)
				if !row.signed {
					if ans.status != http.StatusOK || rec.Outcome != ExchangeAnswered || !bytes.Contains(ans.body, []byte(`"output"`)) {
						t.Fatalf("%s: answered %d (%s), want the Task extended: %s", row.name, ans.status, rec.Outcome, ans.body)
					}
					continue
				}
				if ans.status != http.StatusUnprocessableEntity || !bytes.Contains(ans.body, []byte(refusalCDexSignedTask)) || bytes.Contains(ans.body, []byte(`"output"`)) {
					t.Fatalf("%s: answered %d %s, want 422 %q", row.name, ans.status, ans.body, refusalCDexSignedTask)
				}
				if rec.Outcome != ExchangeRefused || rec.Status != http.StatusUnprocessableEntity || rec.Rule != RefusalFidelity || rec.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("%s: record %s %s/%s status %d, want refused 422 on the fidelity of the gateway's own edit at the build", row.name, rec.Outcome, rec.Rule, rec.RefusalStage, rec.Status)
				}
				if signedAnswer == nil {
					signedAnswer, signedRecord = &ans, rec
					continue
				}
				if !bytes.Equal(ans.body, signedAnswer.body) || ans.framed != signedAnswer.framed || rec.RefusedBy != signedRecord.RefusedBy || rec.Rule != signedRecord.Rule {
					t.Fatalf("%s: answered %s, recorded %s/%s; another signed Task %s, %s/%s", row.name, ans.body, rec.RefusedBy, rec.Rule, signedAnswer.body, signedRecord.RefusedBy, signedRecord.Rule)
				}
			}
		})
	}
}

// TestSealCDexFulfillment_RefusesATaskTheRelayReadsAsSigned: the refusal
// wraps the relay's signed-content error, naming the signature, and does
// not echo the Task.
func TestSealCDexFulfillment_RefusesATaskTheRelayReadsAsSigned(t *testing.T) {
	task := replaceOnce(t, []byte(cdexAnswerTask), `"intent":"order",`, `"intent":"order","extension":[{"url":"http://example.org/sig","valueSignature":{"data":"c2ln"}}],`)
	_, _, err := sealCDexFulfillment(task, []byte(cdexAnswerRecords))
	assertCDexRefused(t, err, relay.ErrSignedContent)
	if errors.Is(err, shnsdk.ErrCDexSignedContent) || !strings.Contains(err.(*cdexRequestRefused).err.Error(), "Signature in Task") {
		t.Fatalf("want the relay's refusal naming Signature in Task, got %v", errors.Unwrap(err))
	}
}

// TestSealCDexFulfillment_TheRelayReadsTheRecordsToo: a signed Provenance
// among the facility's records is read by the relay's rule too, its members
// in any case: one that targets the request Task refuses the fulfillment, as
// does one that targets the records Bundle when the fulfillment writes that
// Bundle's id; one that targets the Bundle embedded as it is does not. So
// does a Signature the relay reads in a records Bundle whose id is written.
func TestSealCDexFulfillment_TheRelayReadsTheRecordsToo(t *testing.T) {
	prov := func(rt, target, reference, ref string) string {
		return `{"resourceType":"Bundle","id":"r1","type":"collection","entry":[{"fullUrl":"urn:shn:fedquery:p","resource":` +
			`{"` + rt + `":"Provenance","id":"prov-1","` + target + `":[{"` + reference + `":"` + ref + `"}],"signature":[{"data":"c2ln"}]}}]}`
	}
	task := []byte(cdexAnswerTask)
	// The Task already uses the records' id, so the fulfillment rewrites it.
	taken := replaceOnce(t, task, `"intent":"order",`, `"intent":"order","basedOn":[{"reference":"#r1"}],`)
	for _, row := range []struct {
		name    string
		task    []byte
		records string
		refused bool
	}{
		{"records Provenance.TARGET to the Task", task, prov("resourceType", "TARGET", "reference", "Task/cdex-req-1"), true},
		{"records Provenance.target[].Reference to the Task", task, prov("resourceType", "target", "Reference", "Task/cdex-req-1"), true},
		{"records Provenance named by RESOURCETYPE, to the Task", task, prov("RESOURCETYPE", "target", "reference", "Task/cdex-req-1"), true},
		{"records Provenance.TARGET to the records Bundle, its id rewritten", taken, prov("resourceType", "TARGET", "reference", "Bundle/r1"), true},
		{"records Provenance.TARGET to the records Bundle, embedded as it is", task, prov("resourceType", "TARGET", "reference", "Bundle/r1"), false},
		{"records Provenance.TARGET to another resource", task, prov("resourceType", "TARGET", "reference", "Observation/obs-1"), false},
		{"records Bundle with no id and a Signature in its meta lacking R4's members, its id written", task,
			`{"resourceType":"Bundle","meta":{"extension":[{"url":"urn:x","valueSignature":{"data":"c2ln"}}]},"type":"collection"}`, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := shnsdk.BuildCDexFulfillment(row.task, []byte(row.records)); err != nil {
				t.Fatalf("the SDK refuses this itself (%v): the row does not hold the gateway to the relay", err)
			}
			_, _, err := sealCDexFulfillment(row.task, []byte(row.records))
			if !row.refused {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			assertCDexRefused(t, err, relay.ErrSignedContent)
		})
	}
}

func replaceOnce(t *testing.T, b []byte, old, new string) []byte {
	t.Helper()
	if bytes.Count(b, []byte(old)) != 1 {
		t.Fatalf("fixture: %q occurs %d times", old, bytes.Count(b, []byte(old)))
	}
	return bytes.Replace(b, []byte(old), []byte(new), 1)
}
