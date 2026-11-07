//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The facility's report and the Provenance that attributes it are a peer's
// bytes. The claim update module 1 builds carries them as the facility sent
// them: their own ids, their meta (profile included), their members in the
// facility's own order and spelling. The one change is registered edit E-14,
// the report's subject.reference value, where it is made.

// facilityRecordsAsWritten gives the facility's report and Provenance their own
// ids, a declared profile and a narrative holding markup, with the report
// naming subject, and writes the facility's answer the way its own system
// might: indented, with < > & as themselves.
func facilityRecordsAsWritten(t *testing.T, subject map[string]any) func(int, []byte) []byte {
	return func(n int, p []byte) []byte {
		if n != 1 {
			return p
		}
		p = withFacilityRecords(t, p, func(entries []any) []any {
			dr := facilityEntry(t, entries, "DiagnosticReport")
			dr["id"] = "fac-dr-9"
			dr["subject"] = subject
			dr["meta"] = map[string]any{"versionId": "2", "profile": []any{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-diagnosticreport-note"}}
			dr["text"] = map[string]any{"status": "generated", "div": `<div xmlns="http://www.w3.org/1999/xhtml">Operative note &amp; findings &lt;as written&gt;</div>`}
			prov := facilityEntry(t, entries, "Provenance")
			prov["id"] = "fac-prov-4"
			prov["meta"] = map[string]any{"profile": []any{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-provenance"}}
			prov["target"] = []any{map[string]any{"reference": "DiagnosticReport/fac-dr-9"}}
			if subject["reference"] == "Patient/fac-77" {
				entries = append(entries, map[string]any{"fullUrl": "urn:shn:fedquery:fac-77", "resource": map[string]any{
					"resourceType": "Patient", "id": "fac-77",
					"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "MBR-UC05"}},
				}})
			}
			return entries
		})
		var v any
		if err := json.Unmarshal(p, &v); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
		return bytes.TrimRight(b.Bytes(), "\n")
	}
}

// facilityRecordBytes returns the exact bytes of the facility's resource of
// type rt in a federated answer (a Task whose contained Bundle holds the
// records).
func facilityRecordBytes(t *testing.T, answer []byte, rt string) []byte {
	t.Helper()
	var task struct {
		Contained []json.RawMessage `json:"contained"`
	}
	if err := json.Unmarshal(answer, &task); err != nil {
		t.Fatal(err)
	}
	for _, c := range task.Contained {
		var b struct {
			ResourceType string `json:"resourceType"`
			Entry        []struct {
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		}
		if json.Unmarshal(c, &b) != nil || b.ResourceType != "Bundle" {
			continue
		}
		for _, e := range b.Entry {
			var head struct {
				ResourceType string `json:"resourceType"`
			}
			if json.Unmarshal(e.Resource, &head) == nil && head.ResourceType == rt {
				return e.Resource
			}
		}
	}
	t.Fatalf("the facility's answer holds no %s", rt)
	return nil
}

// sentRecordBytes returns the exact bytes of the resource of type rt in the
// claim update the payer received.
func sentRecordBytes(t *testing.T, stub *pendResumeSubstrate, rt string) []byte {
	t.Helper()
	sent := stub.requests["pas-claim-update"]
	if len(sent) != 1 {
		t.Fatalf("%d claim updates sent, want 1", len(sent))
	}
	var b struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(sent[0], &b); err != nil {
		t.Fatal(err)
	}
	for _, e := range b.Entry {
		var head struct {
			ResourceType string `json:"resourceType"`
		}
		if json.Unmarshal(e.Resource, &head) == nil && head.ResourceType == rt {
			return e.Resource
		}
	}
	t.Fatalf("the claim update carries no %s", rt)
	return nil
}

// TestHandleUC05_CarriesFacilityRecordsAsSent: the payer receives the
// facility's report and Provenance byte for byte as the facility sent them,
// apart from E-14's value where E-14 is made. Every level.
func TestHandleUC05_CarriesFacilityRecordsAsSent(t *testing.T) {
	const route = "POST /scenario/uc05"
	for _, row := range []struct {
		name    string
		subject map[string]any
		// rekeyed: E-14 replaces the report's subject.reference value with
		// the Claim's patient.
		rekeyed bool
	}{
		{"no edit: the report already names the Claim's patient", map[string]any{"reference": "Patient/MBR-UC05"}, false},
		{"E-14 made: the facility's own Patient", map[string]any{"reference": "Patient/fac-77", "display": "L. Johansson"}, true},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				var answer []byte
				write := facilityRecordsAsWritten(t, row.subject)
				gw, stub := uc05Fixture(t, level, func(n int, p []byte) []byte {
					out := write(n, p)
					if n == 1 {
						answer = out
					}
					return out
				})
				rec, _ := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc05", ""))
				if rec.Code != http.StatusOK {
					t.Fatalf("uc05 = %d %s legs %v, want 200 with the claim update sent", rec.Code, rec.Body, stub.legTypes)
				}
				wantReport := facilityRecordBytes(t, answer, "DiagnosticReport")
				if row.rekeyed {
					wantReport = bytes.Replace(wantReport, []byte(`"Patient/fac-77"`), []byte(`"Patient/MBR-UC05"`), 1)
				}
				if got := sentRecordBytes(t, stub, "DiagnosticReport"); !bytes.Equal(got, wantReport) {
					t.Errorf("the payer did not receive the facility's report as sent\n got: %s\nwant: %s", got, wantReport)
				}
				wantProv := facilityRecordBytes(t, answer, "Provenance")
				if got := sentRecordBytes(t, stub, "Provenance"); !bytes.Equal(got, wantProv) {
					t.Errorf("the payer did not receive the facility's Provenance as sent\n got: %s\nwant: %s", got, wantProv)
				}
			})
		}
	}
}
