package engine

import (
	"bytes"
	"context"
	"encoding/json"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"strings"
	"testing"
	"time"
)

const sourceTree = `{"resourceType":"Questionnaire","url":"https://example.test/Questionnaire/synthetic","status":"active","item":[{"linkId":"1","type":"string"},{"linkId":"6.1","type":"string"}]}`

func TestDTRRawSourcePairedBuildIsolation(t *testing.T) {
	raw, tree := []byte(contextQR), []byte(sourceTree)
	before := append([]byte(nil), raw...)
	qc := contextInputs()
	qc.Authored = time.Unix(1700000000, 0).UTC()
	source := newRawDTRBuildSource(raw, tree, qc)
	item, err := buildOxygenNecessityItem("1234567890", "Synthetic service", "J44.9", "2026-06-03")
	if err != nil {
		t.Fatal(err)
	}
	event := append([]byte(nil), item...)
	amended := source.withAmendment(item, "amend qr with item 6.1").withQRID("amended-synthetic")
	for i := range raw {
		raw[i] = 'x'
	}
	for i := range tree {
		tree[i] = 'x'
	}
	for i := range item {
		item[i] = 'x'
	}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		out, err := amended.buildAtLine(line)
		if err != nil {
			t.Fatal(err)
		}
		again, err := amended.buildAtLine(line)
		if err != nil || !bytes.Equal(out, again) {
			t.Fatal("build is not immutable/deterministic")
		}
		if !bytes.Contains(out, []byte(`9007199254740993.2300`)) || !bytes.Contains(out, []byte(`amended-synthetic`)) {
			t.Fatalf("lost source content: %s", out)
		}
		var q struct{ Item []json.RawMessage }
		_ = json.Unmarshal(out, &q)
		found := false
		for _, got := range q.Item {
			var p struct {
				LinkID string `json:"linkId"`
			}
			_ = json.Unmarshal(got, &p)
			if p.LinkID == "6.1" {
				var compact bytes.Buffer
				_ = json.Compact(&compact, event)
				if !bytes.Equal(compact.Bytes(), got) {
					t.Fatalf("captured event changed: %s", got)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("missing captured amendment")
		}
		original, err := source.buildAtLine(line)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(original, []byte(`"6.1"`)) {
			t.Fatal("amendment mutated source shared by another token")
		}
	}
	if !bytes.Equal(source.raw, before) {
		t.Fatal("source bytes mutated")
	}
	conflict := newRawDTRBuildSource([]byte(strings.Replace(contextQR, `"resourceType"`, `"meta":{"profile":["`+dtrQRCanonical+`|2.0.1"]},"resourceType"`, 1)), []byte(sourceTree), qc)
	if _, err := conflict.buildAtLine("2.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := conflict.buildAtLine("2.2"); err == nil {
		t.Fatal("translated a supplied profile assertion")
	}
	if _, err := source.buildAtLine("9.9"); err == nil {
		t.Fatal("accepted unknown line")
	}
}

func TestDTRAutomaticSourceCopiesAnswerPointers(t *testing.T) {
	truth, number, text := true, 7, "Synthetic answer"
	coding := shnsdk.AnswerCoding{System: "https://example.test/synthetic", Code: "synthetic", Display: "Synthetic"}
	answers := map[string]shnsdk.Answer{"b": {Boolean: &truth}, "n": {Integer: &number}, "s": {String: &text}, "c": {Coding: &coding}}
	tree := []byte(`{"resourceType":"Questionnaire","status":"active","url":"https://example.test/Questionnaire/automatic","item":[{"linkId":"b","type":"boolean","required":true},{"linkId":"n","type":"integer","required":true},{"linkId":"s","type":"string","required":true},{"linkId":"c","type":"choice","required":true}]}`)
	qc := contextInputs()
	qc.Authored = time.Unix(1700000000, 0).UTC()
	source := newAutomaticDTRBuildSource(tree, answers, qc)
	before, err := source.buildAtLine("2.2")
	if err != nil {
		t.Fatal(err)
	}
	truth = false
	number = 999
	text = "changed"
	coding.Code = "changed"
	delete(answers, "b")
	tree[0] = 'x'
	qc.Authored = time.Time{}
	after, err := source.buildAtLine("2.2")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("caller mutation changed captured automatic event")
	}
	other, err := source.buildAtLine("2.0")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(other, []byte(`auto-client`)) || !bytes.Contains(other, []byte(`"auto"`)) {
		t.Fatalf("did not use owned line-aware automatic builder: %s", other)
	}
	if !bytes.Contains(before, []byte(`auto-client`)) {
		t.Fatal("missing native 2.2 automatic attribution")
	}
}

func TestPASAttachmentValidationChecksExactFinalBytes(t *testing.T) {
	source := newRawDTRBuildSource([]byte(contextQR), []byte(sourceTree), contextInputs())
	qr, err := source.buildAtLine("2.2")
	if err != nil {
		t.Fatal(err)
	}
	recorder := &dtrRecordingValidator{base: syntheticLineValidator("2.2")}
	g := &Gateway{cfg: Config{ValidatorsByLine: map[string]shnsdk.Validator{"2.2": recorder}, ConformanceEnforcement: EnforcementStrict}}
	bundle := []byte(`{"resourceType":"Bundle","entry":[{"resource":` + string(qr) + `}]}`)
	if status, msg := g.validatePASAttachments(context.Background(), bundle, "2.2", true); status != 0 {
		t.Fatalf("%d %s", status, msg)
	}
	if len(recorder.calls) != 1 || !bytes.Equal(recorder.calls[0].payload, qr) || recorder.calls[0].profile != dtrQRCanonical+"|2.2.0" {
		t.Fatal("did not validate exact embedded QR at paired profile")
	}
	missing := []byte(`{"resourceType":"Bundle","entry":[]}`)
	if status, _ := g.validatePASAttachments(context.Background(), missing, "2.2", true); status == 0 {
		t.Fatal("missing expected QR accepted")
	}
	if status, _ := g.validatePASAttachments(context.Background(), missing, "2.2", false); status != 0 {
		t.Fatal("true no-documentation flow refused")
	}
	extra := []byte(`{"resourceType":"Bundle","entry":[{"resource":` + string(qr) + `},{"resource":{"resourceType":"QuestionnaireResponse","meta":{"profile":["` + dtrQRCanonical + `|2.2.0"]}}}]}`)
	if status, _ := g.validatePASAttachments(context.Background(), extra, "2.2", true); status != 422 {
		t.Fatalf("unvalidated additional QR accepted: %d", status)
	}
}

func TestDTRSourceAmendmentRefusalPreservesSource(t *testing.T) {
	source := newRawDTRBuildSource([]byte(contextQR), []byte(sourceTree), contextInputs())
	before, err := source.buildAtLine("2.2")
	if err != nil {
		t.Fatal(err)
	}
	item, err := buildOxygenNecessityItem("1234567890", "Synthetic service", "J44.9", "2026-06-03")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"linkId":"6.1"`, `The ordering provider attests that Synthetic service (diagnosis J44.9) is medically necessary.`, `I attest this order is medically necessary.`, `"valueString":"1234567890"`, `"valueDate":"2026-06-03"`, `Practitioner/1234567890`} {
		if !bytes.Contains(item, []byte(want)) {
			t.Fatalf("oxygen event changed: %s", want)
		}
	}
	bad := source.withAmendment([]byte(`{"linkId":`), "amend qr with item 6.1")
	if _, err := bad.buildAtLine("2.2"); err == nil || !strings.HasPrefix(err.Error(), "amend qr with item 6.1: ") {
		t.Fatalf("lost amendment error prefix: %v", err)
	}
	after, err := source.buildAtLine("2.2")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed amendment mutated source")
	}
	if _, err := buildPASAttachment(source, ""); err == nil {
		t.Fatal("missing PAS line accepted")
	}
	if raw, err := buildPASAttachment(nil, "2.2"); err != nil || len(raw) != 0 {
		t.Fatal("true no-documentation source refused")
	}
}

func TestPASAttachmentFinalMutationRefusals(t *testing.T) {
	source := newRawDTRBuildSource([]byte(contextQR), []byte(sourceTree), contextInputs())
	qr, err := source.buildAtLine("2.2")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"coverage deleted", "wrong profile", "malformed resource", "wrong container", "missing lane", "outage"} {
		t.Run(mode, func(t *testing.T) {
			var resource map[string]json.RawMessage
			_ = json.Unmarshal(qr, &resource)
			switch mode {
			case "coverage deleted":
				var extensions []map[string]json.RawMessage
				_ = json.Unmarshal(resource["extension"], &extensions)
				var kept []map[string]json.RawMessage
				for _, ext := range extensions {
					if !bytes.Contains(ext["url"], []byte("qr-coverage")) {
						kept = append(kept, ext)
					}
				}
				resource["extension"], _ = json.Marshal(kept)
			case "wrong profile":
				resource["meta"] = json.RawMessage(`{"profile":["` + dtrQRCanonical + `|2.0.1"]}`)
			}
			final, _ := json.Marshal(resource)
			bundle := []byte(`{"resourceType":"Bundle","entry":[{"resource":` + string(final) + `}]}`)
			recorder := &dtrRecordingValidator{base: syntheticLineValidator("2.2")}
			g := &Gateway{cfg: Config{ValidatorsByLine: map[string]shnsdk.Validator{"2.2": recorder}, ConformanceEnforcement: EnforcementStrict}}
			want := 422
			switch mode {
			case "malformed resource":
				bundle = []byte(`{"resourceType":"Bundle","entry":[{"resource":17}]}`)
				want = 502
			case "wrong container":
				bundle = []byte(`{"resourceType":"Patient"}`)
				want = 502
			case "missing lane":
				delete(g.cfg.ValidatorsByLine, "2.2")
				want = 503
			case "outage":
				recorder.mode = "validator outage"
				want = 503
			}
			if status, _ := g.validatePASAttachments(context.Background(), bundle, "2.2", true); status != want {
				t.Fatalf("status=%d want=%d", status, want)
			}
		})
	}
}
