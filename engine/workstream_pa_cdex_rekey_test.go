//go:build !shn_coreonly

package engine

// workstream_pa_cdex_rekey_test.go — the payer's attachment-request Task's
// return-path re-key (E-11): the provider's gateway replaces the Task's one
// payer-url with its own return path for the payer that sent it, even where a
// signature covers it, and carries the Task as sent, recording why, wherever
// it cannot.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// rekeyTaskPayerURL is the payer's own payer-url the Task names.
const rekeyTaskPayerURL = "https://payer.example/fhir"

// rekeyTask is a payer's attachment-request Task whose inputs are inputs, and
// whose root members before "input" are extra (with its trailing comma).
func rekeyTask(extra, inputs string) []byte {
	return []byte(`{"resourceType":"Task","id":"ar1",` + extra + `"status":"requested","intent":"order",` +
		`"code":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"attachment-request-code"}]},` +
		`"for":{"reference":"#patient"},"input":[` + inputs + `]}`)
}

func payerURLInput(value string) string {
	return `{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"payer-url"}]},` + value + `}`
}

const attachmentsNeededInput = `{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"attachments-needed"}]},` +
	`"valueCodeableConcept":{"coding":[{"system":"http://loinc.org","code":"11506-3"}]}}`

// rekeyGateway is a provider's gateway whose registry lists payer as taking
// legs, with ingress base base, recording its exchanges.
func rekeyGateway(base, payer string, legs []string) (*Gateway, *exchangeRecords) {
	reg := shnsdk.NewRegistry()
	reg.Set(payer, shnsdk.RegistryEntry{ID: payer, Role: "payer", Legs: legs})
	var got exchangeRecords
	return &Gateway{cfg: Config{Role: "provider", Reg: reg, IngressBaseURL: base, ExchangeObserved: got.observe}}, &got
}

// rekeyCall runs cdexTaskReturnRekey on task for payer inside a recorded
// exchange, and returns what it carries, its error and the record.
func rekeyCall(t *testing.T, g *Gateway, got *exchangeRecords, task []byte, payer string) (relay.Payload, error, ExchangeRecord) {
	t.Helper()
	var p relay.Payload
	var err error
	g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
		p, err = g.cdexTaskReturnRekey(r.Context(), task, "application/fhir+json", payer)
		w.WriteHeader(http.StatusOK)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	return p, err, got.only(t)
}

// The Task's one payer-url becomes the gateway's return path for the payer
// that sent it, and nothing else in the Task changes. The value is rebuilt
// from the ingress base and that payer, never read from the Task.
func TestCDexTaskRekey_PayerURLBecomesTheReturnPath(t *testing.T) {
	for _, payer := range []string{"payer-1", "payer-2"} {
		t.Run(payer, func(t *testing.T) {
			g, got := rekeyGateway(pendRekeyBase, payer, []string{legCDexSubmit})
			task := rekeyTask("", attachmentsNeededInput+","+payerURLInput(`"valueUrl":"`+rekeyTaskPayerURL+`"`))
			p, err, xr := rekeyCall(t, g, got, task, payer)
			if err != nil {
				t.Fatal(err)
			}
			want, err := cdexroute.ReturnBase(pendRekeyBase, payer)
			if err != nil {
				t.Fatal(err)
			}
			out := relay.BytesForTest(p)
			if !slices.Equal(p.Edits(), []relay.EditID{relay.EditCDexTaskReturnRekey}) || len(xr.EditsNotMade) != 0 {
				t.Fatalf("edits %v, not made %v", p.Edits(), xr.EditsNotMade)
			}
			wantOut := bytes.Replace(task, []byte(`"valueUrl":"`+rekeyTaskPayerURL+`"`), []byte(`"valueUrl":"`+want+`"`), 1)
			if !bytes.Equal(out, wantOut) {
				t.Fatalf("carried\n%s\nwant\n%s", out, wantOut)
			}
		})
	}
}

// Where a signature covers the payer-url the re-key is still made (D127), and
// the forward discloses it with the signature it broke.
func TestCDexTaskRekey_SignedPayerURLIsRekeyedAndDisclosed(t *testing.T) {
	g, got := rekeyGateway(pendRekeyBase, "payer-1", []string{legCDexSubmit})
	sig := `"extension":[{"url":"http://example.org/sig","valueSignature":{"type":[{"code":"1.2.840.10065.1.12.1.1"}],` +
		`"when":"2026-11-02T00:00:00Z","who":{"reference":"Organization/payer"}}}],`
	task := rekeyTask(sig, payerURLInput(`"valueUrl":"`+rekeyTaskPayerURL+`"`))
	p, err, _ := rekeyCall(t, g, got, task, "payer-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.SignedEdited(), []string{"Signature in Task"}) {
		t.Fatalf("signed edited %v", p.SignedEdited())
	}
	h := http.Header{}
	setEditedHeader(h, p)
	if got := h.Values(EditedHeader); !slices.Equal(got, []string{`cdex-task-return-rekey; signed="Signature in Task"`}) {
		t.Fatalf("%s = %q", EditedHeader, got)
	}
}

// A Task whose payer-url cannot be re-keyed is carried exactly as the payer
// sent it, and the exchange records why: never a guess at which address, and
// never a refusal.
func TestCDexTaskRekey_NotMadeIsCarriedAndRecorded(t *testing.T) {
	url := `"valueUrl":"` + rekeyTaskPayerURL + `"`
	for name, row := range map[string]struct {
		task  []byte
		legs  []string
		whyNo []string
	}{
		"no payer-url": {rekeyTask("", attachmentsNeededInput), []string{legCDexSubmit}, nil},
		"a payer-url that is a valueString": {rekeyTask("", payerURLInput(`"valueString":"`+rekeyTaskPayerURL+`"`)),
			[]string{legCDexSubmit}, []string{EditNotMadeTaskNoStructuralPayerURL}},
		"an empty valueUrl": {rekeyTask("", payerURLInput(`"valueUrl":""`)),
			[]string{legCDexSubmit}, []string{EditNotMadeTaskNoStructuralPayerURL}},
		"two payer-url inputs": {rekeyTask("", payerURLInput(url)+","+payerURLInput(`"valueUrl":"https://elsewhere.example/fhir"`)),
			[]string{legCDexSubmit}, []string{EditNotMadeTaskUnreadablePayerURL}},
		"an input that cannot be read": {rekeyTask("", payerURLInput(url)+`,"not an input"`),
			[]string{legCDexSubmit}, []string{EditNotMadeTaskUnreadablePayerURL}},
		"not a Task": {[]byte(`{"resourceType":"Parameters","parameter":[]}`), []string{legCDexSubmit}, []string{EditNotMadeTaskUnreadable}},
		"not JSON":   {[]byte(`not a Task`), []string{legCDexSubmit}, []string{EditNotMadeTaskUnreadable}},
		"a payer that does not take the submission": {rekeyTask("", payerURLInput(url)), nil, []string{EditNotMadeTaskPayerDoesNotTake}},
	} {
		t.Run(name, func(t *testing.T) {
			g, got := rekeyGateway(pendRekeyBase, "payer-1", row.legs)
			p, err, xr := rekeyCall(t, g, got, row.task, "payer-1")
			if err != nil {
				t.Fatal(err)
			}
			if out := relay.BytesForTest(p); !bytes.Equal(out, row.task) || len(p.Edits()) != 0 {
				t.Fatalf("carried %s (edits %v), want the Task exactly", out, p.Edits())
			}
			if !slices.Equal(xr.EditsNotMade, row.whyNo) {
				t.Fatalf("not made %v, want %v", xr.EditsNotMade, row.whyNo)
			}
		})
	}
}

// A payer this gateway's registry cannot name is unknown: its payer-url is
// carried and recorded, never re-keyed on a guess.
func TestCDexTaskRekey_PayerNotInTheRegistryIsCarried(t *testing.T) {
	g, got := rekeyGateway(pendRekeyBase, "payer-1", []string{legCDexSubmit})
	task := rekeyTask("", payerURLInput(`"valueUrl":"`+rekeyTaskPayerURL+`"`))
	p, err, xr := rekeyCall(t, g, got, task, "payer-unknown")
	if err != nil || !bytes.Equal(relay.BytesForTest(p), task) || !slices.Equal(xr.EditsNotMade, []string{EditNotMadeTaskPayerDoesNotTake}) {
		t.Fatalf("err %v, carried %s, not made %v", err, relay.BytesForTest(p), xr.EditsNotMade)
	}
}

// A gateway with no return path to write cannot make the re-key: a fidelity
// refusal of its own edit, and only for a Task it would edit.
func TestCDexTaskRekey_NoReturnPathIsAFidelityRefusal(t *testing.T) {
	g, got := rekeyGateway("", "payer-1", []string{legCDexSubmit})
	_, err, _ := rekeyCall(t, g, got, rekeyTask("", payerURLInput(`"valueUrl":"`+rekeyTaskPayerURL+`"`)), "payer-1")
	if !errors.Is(err, errNoReturnPath) {
		t.Fatalf("err %v, want errNoReturnPath", err)
	}
	g, got = rekeyGateway("", "payer-1", []string{legCDexSubmit})
	task := rekeyTask("", attachmentsNeededInput)
	p, err, _ := rekeyCall(t, g, got, task, "payer-1")
	if err != nil || !bytes.Equal(relay.BytesForTest(p), task) {
		t.Fatalf("a Task with no payer-url: err %v, carried %s", err, relay.BytesForTest(p))
	}
}

// The re-key's value is a JSON string even where the return path holds
// characters HTML escaping would change.
func TestCDexTaskRekey_ValueIsTheExactReturnPath(t *testing.T) {
	g, got := rekeyGateway(pendRekeyBase, "payer&1", []string{legCDexSubmit})
	p, err, _ := rekeyCall(t, g, got, rekeyTask("", payerURLInput(`"valueUrl":"`+rekeyTaskPayerURL+`"`)), "payer&1")
	if err != nil {
		t.Fatalf("no return path for this id: %v", err)
	}
	var task struct {
		Input []struct {
			ValueURL string `json:"valueUrl"`
		} `json:"input"`
	}
	if err := json.Unmarshal(relay.BytesForTest(p), &task); err != nil || len(task.Input) != 1 {
		t.Fatalf("carried %s: %v", relay.BytesForTest(p), err)
	}
	want, _ := cdexroute.ReturnBase(pendRekeyBase, "payer&1")
	if task.Input[0].ValueURL != want || strings.Contains(string(relay.BytesForTest(p)), `\`+"u0026") {
		t.Fatalf("payer-url %q, want %q unescaped", task.Input[0].ValueURL, want)
	}
}
