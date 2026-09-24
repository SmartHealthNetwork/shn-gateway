package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The Da Vinci reference payer answers `Claim/$inquire` with a `Parameters` whose
// output parameter is named `responseBundle`, where every published operation
// definition declares `return` (recorded from the pinned image to
// testdata/br-payer/pas-inquire-response.json; the unpatched upstream commit
// answers the same way). The rows here pin both halves of the settled answer to
// that: the Bundles are READ under either name, and the departure is REPORTED.
//
// What makes this worth a file of its own is how it used to fail. Reading only the
// declared name, this gateway relayed the payer's bytes to the requester intact and
// took NOTHING from them — no match attempted, no decision recorded, and no signal
// anywhere that a decision had gone missing. Silent loss reads exactly like
// "the payer had nothing to say", which is why the reporting rows below assert on
// the REPORT and not only on the ledger: a row that watched the ledger alone would
// have passed just as happily before this was fixed, for the wrong reason.

// inquiryAnswerUnder re-labels the synthetic 2.2 answer's output parameters to
// name, leaving every other byte alone. The Bundles are identical across the rows
// that use it, so the only thing under test is the name they arrived under.
func inquiryAnswerUnder(t *testing.T, name string) []byte {
	t.Helper()
	raw := inquiryFixture(t, "pas-inquiry-response-2.2.json")
	var doc struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name     string          `json:"name"`
			Resource json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("read the 2.2 answer: %v", err)
	}
	if doc.ResourceType != "Parameters" || len(doc.Parameter) == 0 {
		t.Fatalf("the 2.2 fixture is not a Parameters carrying output parameters: %s", raw)
	}
	params := make([]map[string]json.RawMessage, 0, len(doc.Parameter))
	for _, p := range doc.Parameter {
		encoded, err := json.Marshal(name)
		if err != nil {
			t.Fatal(err)
		}
		params = append(params, map[string]json.RawMessage{"name": encoded, "resource": p.Resource})
	}
	out, err := json.Marshal(map[string]any{"resourceType": "Parameters", "parameter": params})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPASInquire_ReadsEitherOutputParameterName: the same response Bundles reach
// the same reader, the same subject check and the same ledger effect whether the
// payer names its output parameter the declared `return` or the `responseBundle`
// the reference payer actually sends — and a THIRD name is read by neither, so the
// set that is accepted stays closed rather than becoming "any parameter".
func TestPASInquire_ReadsEitherOutputParameterName(t *testing.T) {
	for _, row := range []struct {
		name   string
		read   bool
		reason string
	}{
		{pasInquiryDeclaredOutput, true, "the name every published operation definition declares"},
		{pasInquiryRecordedOutput, true, "the name the Da Vinci reference payer records against"},
		{"bundle", false, "a name no definition declares and no payer has been recorded sending"},
	} {
		t.Run(row.name, func(t *testing.T) {
			answer := inquiryAnswerUnder(t, row.name)

			// Readable either way: an answer this gateway cannot read at all is the
			// 502 below, and neither of these is that.
			if bad := validatePASInquiryAnswer(answer); bad.Status != 0 {
				t.Fatalf("%s (%s): the shape check refused a Parameters of response Bundles: %+v", row.name, row.reason, bad)
			}

			got := readPASInquiryAnswers(inquiryRequester, answer)
			if row.read && len(got) == 0 {
				t.Fatalf("%s (%s): no ClaimResponse was read; the payer's decision would be taken to the requester and recorded nowhere", row.name, row.reason)
			}
			if !row.read && len(got) != 0 {
				t.Fatalf("%s (%s): %d ClaimResponse(s) read under a name outside the closed set", row.name, row.reason, len(got))
			}

			// The subject scope walks the same Bundles the reader does. A name read
			// for the ledger but skipped by the subject check would be an answer
			// decided on without its patient ever being compared.
			members, ok := pasInquiryAnswerSubjects(answer)
			if !ok {
				t.Fatalf("%s: the answer's subjects could not be read", row.name)
			}
			if row.read && len(members) == 0 {
				t.Errorf("%s: the reader took ClaimResponses the subject check never saw", row.name)
			}
			if !row.read && len(members) != 0 {
				t.Errorf("%s: the subject check walked Bundles the reader does not read", row.name)
			}

			// And the effect that matters: the decision is recorded under either
			// accepted name, and under neither rejected one.
			f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}})
			f.apply(t, inquiryRequester, answer)
			rec := f.state(t)
			if row.read && (rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved) {
				t.Errorf("%s: ledger = %+v, want the payer's decision recorded", row.name, rec)
			}
			if !row.read && rec.State != PendStatePended {
				t.Errorf("%s: ledger = %+v, want no movement", row.name, rec)
			}
		})
	}
}

// TestPASInquire_NonconformantOutputNameReported is the row the fix exists for:
// reading the deviant name must not make it invisible. It asserts the REPORT — the
// observer event and the sentences that ride the answer's certification evidence —
// not the ledger outcome, because the ledger outcome is identical whether or not
// anything was ever said about the deviation.
func TestPASInquire_NonconformantOutputNameReported(t *testing.T) {
	const (
		corr    = "corr-inquiry-leg-77"
		partner = "payer"
	)
	report := func(t *testing.T, answer []byte) ([]string, []ObserverEvent) {
		t.Helper()
		var seen []ObserverEvent
		g := &Gateway{cfg: Config{Clock: fixedClock, Observer: func(e ObserverEvent) { seen = append(seen, e) }}}
		return g.reportInquiryAnswerNonconformance(corr, partner, answer), seen
	}

	t.Run("the reference payer's name is reported, naming both terms", func(t *testing.T) {
		stated, seen := report(t, inquiryAnswerUnder(t, pasInquiryRecordedOutput))
		if len(stated) != 1 {
			t.Fatalf("stated %d departures, want exactly one: %v", len(stated), stated)
		}
		if len(seen) != 1 || seen[0].Kind != "peer.nonconformant" {
			t.Fatalf("observed %+v, want one peer.nonconformant event", seen)
		}
		// Both terms, in both places. Either alone leaves the reader guessing
		// which side of the exchange is wrong.
		for _, where := range []struct{ label, text string }{
			{"the certification sentence", stated[0]},
			{"the observer event", seen[0].Detail},
		} {
			if !strings.Contains(where.text, pasInquiryRecordedOutput) {
				t.Errorf("%s does not name what the payer sent (%s): %q", where.label, pasInquiryRecordedOutput, where.text)
			}
			if !strings.Contains(where.text, pasInquiryDeclaredOutput) {
				t.Errorf("%s does not name what the operation declares (%s): %q", where.label, pasInquiryDeclaredOutput, where.text)
			}
		}
		if seen[0].Detail != stated[0] {
			t.Errorf("the observer and the certification evidence state the departure differently:\n %q\n %q", seen[0].Detail, stated[0])
		}
		// Traceable, like every other event this leg raises.
		if seen[0].CorrelationID != corr || seen[0].Counterpart != partner {
			t.Errorf("event = %+v, want it tied to this exchange and its counterpart", seen[0])
		}
		if seen[0].LegType != "pas-claim-inquire" || seen[0].Op != "pas-inquire-response" || seen[0].Direction != "ingress" {
			t.Errorf("event = %+v, want it named to this leg's answer", seen[0])
		}
	})

	// A name outside the closed read set is reported TOO, and reported as what it is:
	// carried, not read. Before this, such an answer was completely silent — it passed
	// the shape check (no output parameter it knew to look at), was read by nothing,
	// compared by nothing, and relayed. That is the same silent loss this leg was opened
	// to fix, one name over; ruling on the one deviation we happen to know about would
	// have left the next one invisible.
	t.Run("a name this gateway does not read is reported as unread", func(t *testing.T) {
		stated, seen := report(t, inquiryAnswerUnder(t, "bundle"))
		if len(stated) != 1 || len(seen) != 1 {
			t.Fatalf("an unread output parameter was not reported: stated=%v observed=%+v", stated, seen)
		}
		if !strings.Contains(stated[0], "bundle") || !strings.Contains(stated[0], pasInquiryDeclaredOutput) {
			t.Errorf("the report names %q; it must name what the payer sent AND what the operation declares", stated[0])
		}
		if !strings.Contains(stated[0], "does not read") {
			t.Errorf("the report must say the resource went UNREAD, which is what distinguishes it from a name we do read: %q", stated[0])
		}
		// And it really is unread: the read set stays closed.
		if got := readPASInquiryAnswers(inquiryRequester, inquiryAnswerUnder(t, "bundle")); len(got) != 0 {
			t.Errorf("%d ClaimResponse(s) read under a name outside the closed set", len(got))
		}
	})

	t.Run("a parameter carrying no resource is not a departure", func(t *testing.T) {
		// A Parameters may carry ordinary value parameters; only a carried RESOURCE is
		// a response bundle this gateway either read or did not.
		stated, seen := report(t, []byte(`{"resourceType":"Parameters","parameter":[{"name":"note","valueString":"x"}]}`))
		if len(stated) != 0 || len(seen) != 0 {
			t.Fatalf("a value parameter was reported as an unread response bundle: stated=%v observed=%+v", stated, seen)
		}
	})

	t.Run("a conformant answer reports nothing", func(t *testing.T) {
		stated, seen := report(t, inquiryAnswerUnder(t, pasInquiryDeclaredOutput))
		if len(stated) != 0 || len(seen) != 0 {
			t.Fatalf("a conformant answer was reported as nonconformant: stated=%v observed=%+v", stated, seen)
		}
		// The 2.0.1/2.1.0 shape is a bare Bundle with no output parameter at all.
		if stated, seen := report(t, decidedAnswer(t)); len(stated) != 0 || len(seen) != 0 {
			t.Fatalf("a bare response Bundle was reported as nonconformant: stated=%v observed=%+v", stated, seen)
		}
	})

	t.Run("the recorded real answer is what this is about", func(t *testing.T) {
		real, err := os.ReadFile(filepath.Join("testdata", "br-payer", "pas-inquire-response.json"))
		if err != nil {
			t.Fatal(err)
		}
		if got := pasInquiryOutputDeviations(real); len(got) != 1 || got[0] != pasInquiryRecordedOutput {
			t.Fatalf("the recorded reference-payer answer deviates as %v, want [%s] — if this changed, the addendum and the reader's closed set both need revisiting",
				got, pasInquiryRecordedOutput)
		}
		if len(readPASInquiryAnswers(inquiryRequester, real)) == 0 {
			t.Error("the recorded reference-payer answer yields no ClaimResponse: its decision would reach the requester and be recorded nowhere")
		}
	})
}

// TestPASInquire_NonconformanceRidesCertificationEvidence: the sentence reaches the
// certification record for the ANSWER, and only for the answer — the request this
// gateway sent is its own and has no departure to report. Without this, the
// evidence an operator reads back would say the exchange was ordinary.
func TestPASInquire_NonconformanceRidesCertificationEvidence(t *testing.T) {
	var seen []ObserverEvent
	g := certificationGateway(t, certificationValidatorFunc(func(context.Context, []byte, string) (shnsdk.Result, error) {
		return shnsdk.Result{Valid: true}, nil
	}), func(e ObserverEvent) { seen = append(seen, e) })

	const sentence = "prior-authorization inquiry answer carries its response bundle under output parameter responseBundle; the operation declares return"
	g.certificationPair("pas-claim-inquire", "payer-native", "corr-inquiry-cert", "2.0",
		[]byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`),
		[]byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`),
		sentence)
	certificationFlush(t, g)

	byDirection := map[string]CertificationEvidence{}
	for _, e := range seen {
		if e.Kind != "leg.certified" {
			continue
		}
		var ev CertificationEvidence
		if err := json.Unmarshal([]byte(e.Detail), &ev); err != nil {
			t.Fatalf("certification evidence is not readable: %v", err)
		}
		byDirection[ev.Direction] = ev
	}
	answer, ok := byDirection["response"]
	if !ok {
		t.Fatalf("no certification evidence for the answer; saw %d events", len(seen))
	}
	if len(answer.Nonconformance) != 1 || answer.Nonconformance[0] != sentence {
		t.Errorf("the answer's evidence states %v, want the departure %q", answer.Nonconformance, sentence)
	}
	request, ok := byDirection["request"]
	if !ok {
		t.Fatal("no certification evidence for the request")
	}
	if len(request.Nonconformance) != 0 {
		t.Errorf("the request's evidence carries %v; the departure belongs to the answer that made it", request.Nonconformance)
	}
	// A conformant exchange says nothing at all — the field is absent, not empty.
	raw, err := json.Marshal(CertificationEvidence{LegType: "pas-claim-inquire", Direction: "response"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("nonconformance")) {
		t.Errorf("a conformant record carries a nonconformance member: %s", raw)
	}
}
