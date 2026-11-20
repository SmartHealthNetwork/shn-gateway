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
// SDK's fulfillment reads (the shn-sdk release a gateway pins may read less
// than the relay): a Signature at a place FHIR R4 types Signature marks the
// Task signed though it lacks a member R4 requires, and the query is refused
// 422 without extending it. Every such Task is answered and recorded alike,
// whichever reading refused it: on the fidelity of the gateway's own edit, at
// the build, so at every level. The relay's reading alone is held by
// TestSealCDexFulfillment_RefusesATaskTheRelayReadsAsSigned.
func TestFederatedQueryInbound_ATaskTheRelayReadsAsSignedIsNotExtended(t *testing.T) {
	const bare = `{"data":"c2ln"}`
	const full = `{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-01-01T00:00:00Z","who":{"reference":"Organization/provider"}}`
	inExtension := func(sig string) func(*testing.T, []byte) []byte {
		return func(t *testing.T, task []byte) []byte {
			return replaceOnce(t, task, `"resourceType":"Task",`, `"resourceType":"Task","extension":[{"url":"http://example.org/sig","valueSignature":`+sig+`}],`)
		}
	}
	rows := []struct {
		name   string
		edit   func(*testing.T, []byte) []byte
		signed bool
	}{
		{"no Signature: extended", func(_ *testing.T, task []byte) []byte { return task }, false},
		{"a Signature in an extension, lacking members R4 requires", inExtension(bare), true},
		{"a Signature in an input's valueSignature, lacking members R4 requires", func(t *testing.T, task []byte) []byte {
			return replaceOnce(t, task, `"input":[`, `"input":[{"type":{"text":"signature"},"valueSignature":`+bare+`},`)
		}, true},
		{"a signed Provenance it contains whose target is named TARGET", func(t *testing.T, task []byte) []byte {
			return replaceOnce(t, task, `"resourceType":"Task",`, `"resourceType":"Task","contained":[{"resourceType":"Provenance","id":"p1",`+
				`"TARGET":[{"reference":"#"}],"recorded":"2026-01-01T00:00:00Z","agent":[{"who":{"reference":"Organization/provider"}}],"signature":[`+bare+`]}],`)
		}, true},
		{"a signed Provenance it contains that targets it by an id named ID", func(t *testing.T, task []byte) []byte {
			return replaceOnce(t, task, `"resourceType":"Task",`, `"resourceType":"Task","ID":"t1","contained":[{"resourceType":"Provenance","id":"p1",`+
				`"target":[{"reference":"Task/t1"}],"recorded":"2026-01-01T00:00:00Z","agent":[{"who":{"reference":"Organization/provider"}}],"signature":[`+bare+`]}],`)
		}, true},
		// A Signature every reading finds, by its members too.
		{"a full Signature in an extension", inExtension(full), true},
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

// TestSealCDexFulfillment_RefusesATaskTheRelayReadsAsSigned: a Task with a
// Signature at a place R4 types Signature, lacking members R4 requires, is
// refused as signed content without echoing the Task, by whichever reading
// finds it; and the relay's reading alone names it, in a fulfillment the SDK
// built.
func TestSealCDexFulfillment_RefusesATaskTheRelayReadsAsSigned(t *testing.T) {
	const sig = `"valueSignature":`
	task := replaceOnce(t, []byte(cdexAnswerTask), `"intent":"order",`, `"intent":"order","extension":[{"url":"http://example.org/sig",`+sig+`{"data":"c2ln"}}],`)
	_, _, err := sealCDexFulfillment(task, []byte(cdexAnswerRecords))
	assertCDexSignedRefusal(t, err)
	f, held := fulfilledUnderAMask(t, task, []byte(cdexAnswerRecords), sig)
	carrier, err := cdexSignedByTheRelay(f, held)
	if err != nil || !strings.Contains(carrier, "Signature in Task") {
		t.Fatalf("the relay's reading: %q, %v; want it to name Signature in Task", carrier, err)
	}
}

// TestSealCDexFulfillment_TheRelayReadsTheRecordsToo: a signed Provenance
// among the facility's records is read by the relay's rule too, its members
// in any case: one that targets the request Task refuses the fulfillment, as
// does one that targets the records Bundle when the fulfillment writes that
// Bundle's id; one that targets the Bundle embedded as it is does not. So
// does a Signature the relay reads in a records Bundle whose id is written.
// Each row holds the fulfillment's outcome, whichever reading refuses, and
// the relay's reading alone, in a fulfillment the SDK built.
func TestSealCDexFulfillment_TheRelayReadsTheRecordsToo(t *testing.T) {
	const signature = `"signature":`
	prov := func(rt, target, reference, ref string) string {
		return `{"resourceType":"Bundle","id":"r1","type":"collection","entry":[{"fullUrl":"urn:shn:fedquery:p","resource":` +
			`{"` + rt + `":"Provenance","id":"prov-1","` + target + `":[{"` + reference + `":"` + ref + `"}],` + signature + `[{"data":"c2ln"}]}}]}`
	}
	task := []byte(cdexAnswerTask)
	// The Task already uses the records' id, so the fulfillment rewrites it.
	taken := replaceOnce(t, task, `"intent":"order",`, `"intent":"order","basedOn":[{"reference":"#r1"}],`)
	for _, row := range []struct {
		name    string
		task    []byte
		records string
		// sig is the member that makes the records signed, masked to build
		// the fulfillment the relay's reading alone is held to.
		sig     string
		refused bool
	}{
		{"records Provenance.TARGET to the Task", task, prov("resourceType", "TARGET", "reference", "Task/cdex-req-1"), signature, true},
		{"records Provenance.target[].Reference to the Task", task, prov("resourceType", "target", "Reference", "Task/cdex-req-1"), signature, true},
		{"records Provenance named by RESOURCETYPE, to the Task", task, prov("RESOURCETYPE", "target", "reference", "Task/cdex-req-1"), signature, true},
		{"records Provenance.TARGET to the records Bundle, its id rewritten", taken, prov("resourceType", "TARGET", "reference", "Bundle/r1"), signature, true},
		{"records Provenance.TARGET to the records Bundle, embedded as it is", task, prov("resourceType", "TARGET", "reference", "Bundle/r1"), signature, false},
		{"records Provenance.TARGET to another resource", task, prov("resourceType", "TARGET", "reference", "Observation/obs-1"), signature, false},
		{"records Bundle with no id and a Signature in its meta lacking R4's members, its id written", task,
			`{"resourceType":"Bundle","meta":{"extension":[{"url":"urn:x","valueSignature":{"data":"c2ln"}}]},"type":"collection"}`, `"valueSignature":`, true},
		// The records become contained in the fulfilled Task, so a "#" in
		// their own contained names the Task through them.
		{"records with a contained Provenance whose target is #, embedded as they are", task,
			`{"resourceType":"Bundle","id":"r1","type":"collection","contained":[{"resourceType":"Provenance","id":"prov-1","target":[{"reference":"#"}],` + signature + `[{"data":"c2ln"}]}]}`, signature, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, _, err := sealCDexFulfillment(row.task, []byte(row.records))
			if !row.refused {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
			} else {
				assertCDexSignedRefusal(t, err)
			}
			f, held := fulfilledUnderAMask(t, row.task, []byte(row.records), row.sig)
			carrier, err := cdexSignedByTheRelay(f, held)
			if err != nil || (carrier != "") != row.refused {
				t.Fatalf("the relay's reading: %q, %v; want a signature named: %v", carrier, err, row.refused)
			}
		})
	}
}

// TestSealCDexFulfillment_TheSDKsReadingOfSignedContent: the facility's
// gateway fulfills each row. Neither reading of signed content finds a
// signature over what the fulfillment changes: the SDK's (shn-sdk v1.15.0,
// signedcontent) and the relay's, which since that release are one reading.
// Earlier readings differed on each: shn-sdk v1.14.0's refused the first
// seven, and shn-sdk v1.13.0's, which earlier gateway releases pinned, the
// last.
func TestSealCDexFulfillment_TheSDKsReadingOfSignedContent(t *testing.T) {
	const (
		task = `{"resourceType":"Task","id":"t1","status":"requested","intent":"order"}`
		// A records Bundle with no id, so the fulfillment writes one.
		noID = `{"resourceType":"Bundle","type":"collection",`
		// A signed Provenance whose one target is "#".
		hashProv = `{"resourceType":"Provenance","id":"p1","target":[{"reference":"#"}],"recorded":"2026-01-01T00:00:00Z","agent":[{"who":{"reference":"Practitioner/1"}}],"signature":[{"data":"c2ln"}]}`
		// A Basic with a Signature's members.
		shaped = `{"resourceType":"Basic","id":"b1","type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-01-01T00:00:00Z","who":{"reference":"Practitioner/1"}}`
	)
	for _, row := range []struct{ name, task, records string }{
		{"a resource the Task contains, with an extension's valueSignature lacking R4's members",
			`{"resourceType":"Task","id":"t1","status":"requested","intent":"order","contained":[{"resourceType":"Basic","id":"b1","code":{"text":"x"},"extension":[{"url":"urn:x","valueSignature":{"data":"c2ln"}}]}]}`,
			`{"resourceType":"Bundle","id":"r1","type":"collection"}`},
		{"a resource the Task contains, with a modifier extension's valueSignature holding only type and when",
			`{"resourceType":"Task","id":"t1","status":"requested","intent":"order","contained":[{"resourceType":"Basic","id":"b1","code":{"text":"x"},"modifierExtension":[{"url":"urn:x","valueSignature":{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-01-01T00:00:00Z"}}]}]}`,
			`{"resourceType":"Bundle","id":"r1","type":"collection"}`},
		{"records: a signed Provenance targeting #, contained in an entry's resource, the Bundle's id written", task,
			noID + `"entry":[{"fullUrl":"https://provider.example/fhir/Basic/b1","resource":{"resourceType":"Basic","id":"b1","code":{"text":"x"},"contained":[` + hashProv + `]}}]}`},
		{"records: a signed Provenance targeting #, an entry's resource, the Bundle's id written", task,
			noID + `"entry":[{"fullUrl":"https://provider.example/fhir/Provenance/p1","resource":` + hashProv + `}]}`},
		{"records: an entry's resource with a Signature's members, the Bundle's id written", task,
			noID + `"entry":[{"fullUrl":"https://provider.example/fhir/Basic/b1","resource":` + shaped + `}]}`},
		{"records: a resource in the Bundle's contained with a Signature's members, the Bundle's id written", task,
			noID + `"contained":[` + shaped + `]}`},
		{"records: an entry's response outcome with a Signature's members, the Bundle's id written", task,
			noID + `"entry":[{"fullUrl":"urn:x:1","response":{"status":"200","outcome":` + shaped + `}}]}`},
		{"records: the Bundle itself with a Signature's when and who beside its type, its id written", task,
			noID + `"when":"2026-01-01T00:00:00Z","who":{"reference":"Practitioner/1"}}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, _, err := sealCDexFulfillment([]byte(row.task), []byte(row.records)); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// assertCDexSignedRefusal: err is a signed-content refusal of the request,
// by the SDK's reading or the relay's.
func assertCDexSignedRefusal(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, shnsdk.ErrCDexSignedContent) {
		assertCDexRefused(t, err, shnsdk.ErrCDexSignedContent)
		return
	}
	assertCDexRefused(t, err, relay.ErrSignedContent)
}

// fulfilledUnderAMask is the fulfillment the SDK builds for task and records
// with the member sig, which makes one of them signed, renamed to a member
// no reading takes for a signature, then named sig again in the fulfilled
// Task; and the records as they are. It holds the relay's reading
// (cdexSignedByTheRelay) to a signature whatever the SDK's reading finds.
func fulfilledUnderAMask(t *testing.T, task, records []byte, sig string) (shnsdk.CDexFulfillment, relay.Body) {
	t.Helper()
	const mask = `"x-masked":`
	if bytes.Count(task, []byte(sig))+bytes.Count(records, []byte(sig)) != 1 {
		t.Fatalf("fixture: %q occurs other than once", sig)
	}
	mt := bytes.Replace(task, []byte(sig), []byte(mask), 1)
	mr := bytes.Replace(records, []byte(sig), []byte(mask), 1)
	f, err := shnsdk.BuildCDexFulfillment(mt, mr)
	if err != nil {
		t.Fatalf("the SDK's fulfillment of the masked request: %v", err)
	}
	f.Task = replaceOnce(t, f.Task, mask, sig)
	return f, relay.NewBody(records, relay.OriginUpstreamResponse)
}

func replaceOnce(t *testing.T, b []byte, old, new string) []byte {
	t.Helper()
	if bytes.Count(b, []byte(old)) != 1 {
		t.Fatalf("fixture: %q occurs %d times", old, bytes.Count(b, []byte(old)))
	}
	return bytes.Replace(b, []byte(old), []byte(new), 1)
}
