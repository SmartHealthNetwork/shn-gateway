package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLineFakeQRValidationScope(t *testing.T) {
	const bare = `{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/synthetic"},"item":[{"linkId":"1"}]}`
	const dtr = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaireresponse"
	const base = "http://hl7.org/fhir/StructureDefinition/QuestionnaireResponse|4.0.1"
	for _, tc := range []struct {
		name, profile, claim string
		valid                bool
	}{
		{"unprofiled intermediate", "", "", true}, {"explicit base", base, "", true},
		{"explicit DTR", dtr + "|2.2.0", "", false}, {"in-band DTR", "", dtr + "|2.2.0", false},
		{"base cannot suppress DTR", base, dtr + "|2.2.0", false},
		{"wrong selected version", dtr + "|2.1.0", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := bare
			if tc.claim != "" {
				raw = strings.Replace(raw, `"status"`, `"meta":{"profile":["`+tc.claim+`"]},"status"`, 1)
			}
			result, err := syntheticLineValidator("2.2").Validate(context.Background(), []byte(raw), tc.profile)
			if err != nil || result.Valid != tc.valid {
				t.Fatalf("valid=%v issues=%v err=%v", result.Valid, result.Issues, err)
			}
		})
	}
}

func TestNativePopulationPreservesSubjectMembers(t *testing.T) {
	raw := []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/scoped","type":"Patient","display":"Synthetic","identifier":{"system":"https://example.test/member","value":"synthetic"},"extension":[{"url":"https://example.test/number","valueDecimal":9007199254740993.2300}]},"questionnaire":"https://example.test/Questionnaire/synthetic","item":[{"linkId":"1"}],"extension":[{"url":"https://example.test/opaque","valueDecimal":9007199254740993.2300}]}`)
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified subject", true: "foreign subject"}[foreign], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(raw) }))
			defer srv.Close()
			expected := "Patient/scoped"
			if foreign {
				expected = "Patient/other"
			}
			pkg := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Questionnaire","url":"https://example.test/Questionnaire/synthetic"}}]}`)
			got, _, err := NewNativePopulator(srv.Client(), srv.URL).Populate(context.Background(), pkg, PopulateContext{PatientRef: "Patient/logical", SubjectFHIRRef: expected})
			if foreign {
				if err == nil {
					t.Fatal("accepted foreign subject")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var before, after map[string]json.RawMessage
			json.Unmarshal(raw, &before)
			json.Unmarshal(got, &after)
			var oldSubject, newSubject map[string]json.RawMessage
			json.Unmarshal(before["subject"], &oldSubject)
			json.Unmarshal(after["subject"], &newSubject)
			for key, value := range oldSubject {
				if key != "reference" && !bytes.Equal(value, newSubject[key]) {
					t.Fatalf("subject.%s changed: %s -> %s", key, value, newSubject[key])
				}
			}
			if string(newSubject["reference"]) != `"Patient/logical"` {
				t.Fatalf("reference=%s", newSubject["reference"])
			}
			if !bytes.Equal(before["extension"], after["extension"]) {
				t.Fatal("changed opaque extension/number")
			}
			completed, err := composeDTRContextAtLine(got, "2.2", shnsdk.QRContext{PatientRef: "Patient/logical", CoverageRef: "Coverage/synthetic", OrderRef: "DeviceRequest/synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			var final map[string]json.RawMessage
			_ = json.Unmarshal(completed, &final)
			for _, key := range []string{"subject", "item", "questionnaire"} {
				if !bytes.Equal(after[key], final[key]) {
					t.Fatalf("completion changed native %s", key)
				}
			}
			if !bytes.Contains(completed, []byte(`9007199254740993.2300`)) {
				t.Fatal("completion changed native numeric precision")
			}
		})
	}
}

type dtrRecordedValidation struct {
	payload []byte
	profile string
	valid   bool
}
type dtrRecordingValidator struct {
	base  shnsdk.Validator
	mode  string
	calls []dtrRecordedValidation
}

// A controlled payer approval for the strict resume rows. The older stub's
// bare ClaimResponse is a valid local decision specimen but not a PAS success
// envelope. This graph has one decision and closes only synthetic references.
func dtrResumeApprovalBundle(t *testing.T, response []byte, member string) []byte {
	t.Helper()
	var cr map[string]any
	if err := json.Unmarshal(response, &cr); err != nil {
		t.Fatal(err)
	}
	cr["id"] = "resume-decision"
	patient := "Patient/" + member
	base := "https://payer.example/fhir/"
	b := map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
		map[string]any{"fullUrl": base + "ClaimResponse/resume-decision", "resource": cr},
		map[string]any{"fullUrl": base + patient, "resource": map[string]any{"resourceType": "Patient", "id": member}},
		map[string]any{"fullUrl": base + "Organization/payer", "resource": map[string]any{"resourceType": "Organization", "id": "payer"}},
	}}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePASBundleGraph(out); err != nil {
		t.Fatalf("synthetic payer response graph: %v", err)
	}
	return out
}

func (v *dtrRecordingValidator) Validate(ctx context.Context, raw []byte, profile string) (shnsdk.Result, error) {
	var resource struct{ ResourceType string }
	_ = json.Unmarshal(raw, &resource)
	result, err := v.base.Validate(ctx, raw, profile)
	if (v.mode == "QR rejection" && resource.ResourceType == "QuestionnaireResponse") || (v.mode == "Provenance rejection" && resource.ResourceType == "Provenance") {
		result = shnsdk.Result{Valid: false, Issues: []string{"controlled rejection"}}
	}
	if v.mode == "validator outage" {
		err = fmt.Errorf("controlled unavailable validator")
		result.Valid = false
	}
	v.calls = append(v.calls, dtrRecordedValidation{append([]byte(nil), raw...), profile, result.Valid && err == nil})
	return result, err
}

func TestDTRResumeValidationAndRefusals(t *testing.T) {
	for _, scenario := range []string{"uc06", "uc07"} {
		for _, mode := range []string{"approval", "QR rejection", "Provenance rejection", "validator outage", "missing lane"} {
			t.Run(scenario+"/"+mode, func(t *testing.T) {
				opts := pendFixtureOpts{member: "MBR-UC06", birthDate: "1969-07-21", familyName: "Reyes", pendedItem: "functional-status", declared: []string{shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22}}
				if scenario == "uc07" {
					opts.member = "MBR-UC07"
					opts.birthDate = "1990-08-25"
					opts.familyName = "Haddad"
					opts.pendedItem = "patient-reported-functional-status"
					opts.extraRoles = map[string]string{"phg": "phg"}
				}
				gw, stub := newPendResumeFixture(t, opts)
				// This existing transport fixture returns canonical PAS stubs. Inject the
				// validator outcomes explicitly to isolate resume validation and refusal
				// ordering; full line conformance runs in the native exchange harness.
				recorder := &dtrRecordingValidator{base: syntheticFakeValidator()}
				gw.cfg.Validator = syntheticLineValidator("2.0")
				gw.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": syntheticLineValidator("2.0"), "2.2": recorder}
				before := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/scenario/"+scenario, nil)
				st, ok := gw.scenarioToPend(before, request, scenario, opts.member)
				if !ok {
					t.Fatalf("pend: %d %s", before.Code, before.Body.String())
				}
				if st.dtrLine != "2.2" {
					t.Fatalf("pending DTR line=%q", st.dtrLine)
				}
				// The pending state is fixture setup. The row under test is the
				// independently enforced amendment, after the pending answer arrived.
				gw.cfg.ConformanceEnforcement = EnforcementStrict
				stub.overrideResponse = func(leg string, payload []byte) []byte {
					if leg == "pas-claim-update" {
						return dtrResumeApprovalBundle(t, payload, opts.member)
					}
					return payload
				}
				stub.responseDeclarations = map[string]string{"pas-claim-update": shnsdk.ContractPAPAS22}
				// The synthetic payer declares its own published output line;
				// the request's pin is not evidence of the producer's version.
				recorder.calls = nil
				recorder.mode = mode
				if mode == "missing lane" {
					delete(gw.cfg.ValidatorsByLine, "2.2")
				}
				after := httptest.NewRecorder()
				if scenario == "uc06" {
					ok = gw.completeClinician(after, request, st, "", "")
				} else {
					ok = gw.completePatient(after, request, st, "")
				}
				want := 200
				if strings.Contains(mode, "rejection") {
					want = 422
				}
				if mode == "validator outage" || mode == "missing lane" {
					want = 503
				}
				if mode == "target missing" {
					want = 422
				} // The existing pinned-route guard refuses before validation.
				if after.Code != want || ok != (mode == "approval") {
					t.Fatalf("completion ok=%v status=%d want=%d body=%s", ok, after.Code, want, after.Body.String())
				}
				if mode != "approval" {
					if legAttempted(stub.legTypes, "pas-claim-update") {
						t.Fatal("sent rejected amendment")
					}
					return
				}
				qr, provenance, finalBundle := false, false, false
				for _, call := range recorder.calls {
					var r map[string]json.RawMessage
					_ = json.Unmarshal(call.payload, &r)
					if string(r["resourceType"]) == `"QuestionnaireResponse"` {
						if call.profile != dtrQRCanonical+"|2.2.0" || !call.valid {
							t.Fatalf("amendment validation %+v", call)
						}
						qr = true
					}
					if string(r["resourceType"]) == `"Provenance"` && call.valid {
						provenance = true
					}
					if string(r["resourceType"]) == `"Bundle"` && call.valid && bytes.Contains(call.payload, []byte(dtrQRCanonical+"|2.2.0")) {
						finalBundle = true
					}
				}
				if !qr || !provenance || !finalBundle {
					t.Fatalf("selected-lane QR=%v Provenance=%v final-profiled-bundle=%v", qr, provenance, finalBundle)
				}
			})
		}
	}
}

func TestDTRResumeIndependentPinsAndRefusals(t *testing.T) {
	for _, scenario := range []string{"uc06", "uc07"} {
		for _, mode := range []string{"approval", "original QR rejection", "target QR rejection", "Provenance rejection", "original outage", "target outage", "original missing", "target missing"} {
			t.Run(scenario+"/"+mode, func(t *testing.T) {
				opts := pendFixtureOpts{member: "MBR-UC06", birthDate: "1969-07-21", familyName: "Reyes", pendedItem: "functional-status", declared: []string{shnsdk.ContractPACRD20, shnsdk.ContractPADTR20, shnsdk.ContractPAPAS22}}
				if scenario == "uc07" {
					opts.member = "MBR-UC07"
					opts.birthDate = "1990-08-25"
					opts.familyName = "Haddad"
					opts.pendedItem = "patient-reported-functional-status"
					opts.extraRoles = map[string]string{"phg": "phg"}
				}
				gw, stub := newPendResumeFixture(t, opts)
				original := &dtrRecordingValidator{base: syntheticFakeValidator()}
				target := &dtrRecordingValidator{base: syntheticFakeValidator()}
				gw.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": original, "2.2": target}
				request := httptest.NewRequest(http.MethodPost, "/scenario/"+scenario, nil)
				before := httptest.NewRecorder()
				st, ok := gw.scenarioToPend(before, request, scenario, opts.member)
				if !ok {
					t.Fatalf("pend %d %s", before.Code, before.Body.String())
				}
				if st.dtrLine != "2.0" || st.pasDTRLine != "2.2" {
					t.Fatalf("wrong pins: DTR=%s PAS DTR=%s", st.dtrLine, st.pasDTRLine)
				}
				gw.cfg.ConformanceEnforcement = EnforcementStrict
				stub.overrideResponse = func(leg string, payload []byte) []byte {
					if leg == "pas-claim-update" {
						return dtrResumeApprovalBundle(t, payload, opts.member)
					}
					return payload
				}
				stub.responseDeclarations = map[string]string{"pas-claim-update": shnsdk.ContractPAPAS22}
				sourceBefore, err := st.qrSource.buildAtLine(st.dtrLine)
				if err != nil {
					t.Fatal(err)
				}
				driftRegistryTo(t, gw, st.recipient, []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21, shnsdk.ContractPAPAS22})
				original.calls = nil
				target.calls = nil
				switch mode {
				case "original QR rejection":
					original.mode = "QR rejection"
				case "target QR rejection":
					target.mode = "QR rejection"
				case "Provenance rejection":
					original.mode = mode
				case "original outage":
					original.mode = "validator outage"
				case "target outage":
					target.mode = "validator outage"
				case "original missing":
					delete(gw.cfg.ValidatorsByLine, "2.0")
				case "target missing":
					delete(gw.cfg.ValidatorsByLine, "2.2")
				}
				after := httptest.NewRecorder()
				if scenario == "uc06" {
					ok = gw.completeClinician(after, request, st, "", "")
				} else {
					ok = gw.completePatient(after, request, st, "")
				}
				want := 200
				if strings.Contains(mode, "rejection") {
					want = 422
				}
				if strings.Contains(mode, "outage") || strings.Contains(mode, "missing") {
					want = 503
				}
				// The unchanged pinned line remains natively routable without
				// the optional target validator; strict enforcement refuses it
				// at the content boundary before dispatch.
				if after.Code != want || ok != (mode == "approval") {
					t.Fatalf("status=%d want=%d ok=%v %s", after.Code, want, ok, after.Body.String())
				}
				sourceAfter, err := st.qrSource.buildAtLine(st.dtrLine)
				if err != nil || !bytes.Equal(sourceBefore, sourceAfter) {
					t.Fatal("completion mutated pending source")
				}
				if mode != "approval" {
					if legAttempted(stub.legTypes, "pas-claim-update") {
						t.Fatal("sent failed completion")
					}
					return
				}
				var oldQR, newQR []byte
				prov := false
				for _, c := range original.calls {
					var r struct{ ResourceType string }
					_ = json.Unmarshal(c.payload, &r)
					if r.ResourceType == "QuestionnaireResponse" && c.valid && c.profile == dtrQRCanonical+"|2.0.1" {
						oldQR = c.payload
					}
					if r.ResourceType == "Provenance" && c.valid {
						prov = true
					}
				}
				for _, c := range target.calls {
					var r struct{ ResourceType string }
					_ = json.Unmarshal(c.payload, &r)
					if r.ResourceType == "QuestionnaireResponse" && c.valid && c.profile == dtrQRCanonical+"|2.2.0" {
						newQR = c.payload
					}
				}
				if len(oldQR) == 0 || len(newQR) == 0 || bytes.Equal(oldQR, newQR) || !prov {
					t.Fatal("lost separate original QR, target QR or original Provenance validation")
				}
				patientCalls := 0
				for _, leg := range stub.legTypes {
					if leg == "patient-dtr" {
						patientCalls++
					}
				}
				if scenario == "uc07" && patientCalls != 1 {
					t.Fatalf("patient exchange repeated: %d", patientCalls)
				}
			})
		}
	}
}
