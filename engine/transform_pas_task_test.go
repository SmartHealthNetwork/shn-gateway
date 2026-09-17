package engine

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// pendedTaskDoc is a pended response with one PAS Task at line (profile
// version, line-number extension form) and, optionally, a CDex Task.
func pendedTaskDoc(version, lineExt string, withCDex bool) []byte {
	cdex := ""
	if withCDex {
		cdex = `,{"resource":{"resourceType":"Task","meta":{"profile":["http://hl7.org/fhir/us/davinci-cdex/StructureDefinition/cdex-task-data-request|2.1.0"]},` +
			`"code":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-cdex/CodeSystem/cdex-temp","code":"data-request-query"}]},` +
			`"input":[{"extension":[{"url":"` + pasExtPALineNumber + `","valueInteger":9}],"valueString":"x"}]}}`
	}
	return []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"ClaimResponse","outcome":"queued"}},` +
		`{"resource":{"resourceType":"Task","meta":{"profile":["` + pasTaskProfile + version + `"]},` +
		`"code":{"coding":[{"system":"` + pasTempCodes + `","code":"attachment-request-code"}]},` +
		`"input":[{"type":{"coding":[{"system":"` + pasTempCodes + `","code":"payer-url"}]},"valueUrl":"https://payer.example/fhir"},` +
		`{"extension":[` + lineExt + `],"type":{"coding":[{"system":"` + pasTempCodes + `","code":"attachments-needed"}]},` +
		`"valueCodeableConcept":{"coding":[{"system":"http://loinc.org","code":"28570-0"}]}}]}}` + cdex + `]}`)
}

const (
	paLine3      = `{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-paLineNumber","valueInteger":3}`
	serviceLine3 = `{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceLineNumber","valuePositiveInt":3}`
)

// TestPASTransform_PendedTaskRestatedForTargetLine: the 2.1<->2.2 steps
// re-state a pended Task's profile version and line-number extension for the
// target line; the 2.0<->2.1 steps keep the source Task; needs and any other
// contract's Task stay unchanged; a line number with no 2.2.1 form is
// refused.
func TestPASTransform_PendedTaskRestatedForTargetLine(t *testing.T) {
	x := ExchangeIdentity{CorrelationID: "corr-task"}
	for _, row := range []struct {
		name            string
		step            TransformFunc
		in              []byte
		wantVersion     string
		wantLine        string
		wantCDexLineExt bool
	}{
		{"2.0->2.1 keeps the source Task", pasStep2021Up, pendedTaskDoc("|2.0.1", paLine3, true), "|2.0.1", `"valueInteger":3`, true},
		{"2.1->2.0 keeps the source Task", pasStep2021Down, pendedTaskDoc("|2.1.0", paLine3, true), "|2.1.0", `"valueInteger":3`, true},
		{"2.1->2.2", pasStep2122Up, pendedTaskDoc("|2.1.0", paLine3, true), "|2.2.1", `"valuePositiveInt":3`, true},
		{"2.2->2.1", pasStep2122Down, pendedTaskDoc("|2.2.1", serviceLine3, true), "|2.1.0", `"valueInteger":3`, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			out, _, err := row.step(row.in, x)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out, []byte(pasTaskProfile+row.wantVersion)) || bytes.Count(out, []byte(pasTaskProfile+"|")) != 1 {
				t.Errorf("profile not re-stated to %s: %s", row.wantVersion, out)
			}
			if !bytes.Contains(out, []byte(row.wantLine)) {
				t.Errorf("line number not in the target form %s: %s", row.wantLine, out)
			}
			if !bytes.Contains(out, []byte(`"code":"28570-0"`)) || !bytes.Contains(out, []byte(`"valueUrl":"https://payer.example/fhir"`)) {
				t.Errorf("needs or payer URL changed: %s", out)
			}
			if !bytes.Contains(out, []byte(`cdex-task-data-request|2.1.0`)) || !bytes.Contains(out, []byte(`"valueInteger":9`)) {
				t.Errorf("the CDex Task was changed: %s", out)
			}
		})
	}
	t.Run("unversioned profile is left alone", func(t *testing.T) {
		out, _, err := pasStep2122Up(pendedTaskDoc("", paLine3, false), x)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out, []byte(`"`+pasTaskProfile+`"`)) || !bytes.Contains(out, []byte(`"valuePositiveInt":3`)) {
			t.Errorf("out = %s", out)
		}
	})
	t.Run("a Task declaring another line is left alone", func(t *testing.T) {
		in := pendedTaskDoc("|2.0.1", paLine3, false)
		out, _, err := pasStep2122Up(in, x)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out, []byte(pasTaskProfile+"|2.0.1")) || !bytes.Contains(out, []byte(`"valueInteger":3`)) {
			t.Errorf("out = %s", out)
		}
	})
	t.Run("a line number below 1 has no 2.2.1 form", func(t *testing.T) {
		in := pendedTaskDoc("|2.1.0", strings.Replace(paLine3, `:3}`, `:0}`, 1), false)
		_, _, err := pasStep2122Up(in, x)
		var sc *SemanticChangeError
		if !errors.As(err, &sc) || !strings.Contains(strings.Join(sc.MissingElements, " "), "serviceLineNumber") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("2.1->2.0 stays byte-identical", func(t *testing.T) {
		in := pendedTaskDoc("|2.1.0", paLine3, true)
		out, _, err := pasStep2021Down(in, x)
		if err != nil || !bytes.Equal(out, in) {
			t.Fatalf("out=%s err=%v", out, err)
		}
	})
}
