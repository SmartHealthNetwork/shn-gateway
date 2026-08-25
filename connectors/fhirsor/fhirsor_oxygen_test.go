package fhirsor_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// oxygenFakeFHIR stands up an httptest FHIR stub with an anchoring Condition (so
// ClinicalContext returns found=true) and, when withOxygen is true, two Observations coded
// with the real LOINC O2-sat (59408-5) and arterial PaO2 (2703-7) codes — the SAME codes
// internal/fixturesor's hermetic equivalent reads (task-B parity). The Observation search
// only returns a match when the request's code filter names the matching LOINC, mirroring
// fhirsor_procedure_test.go's procFakeFHIR code-aware pattern.
func oxygenFakeFHIR(t *testing.T, withOxygen bool) *fhirclient.Client {
	t.Helper()
	patient := `{"resourceType":"Patient","id":"p","identifier":[{"system":"urn:shn:member","value":"MBR-OXFAKE"}],"name":[{"family":"Oxfake"}],"birthDate":"1970-01-01"}`
	condition := `{"resourceType":"Condition","id":"c","code":{"coding":[{"system":"` + shnsdk.SystemICD10CM + `","code":"J44.9"}]},"subject":{"reference":"Patient/p"}}`
	o2sat := `{"resourceType":"Observation","id":"obs-o2sat","status":"final","code":{"coding":[{"system":"` + shnsdk.SystemLOINC + `","code":"` + shnsdk.OxygenSaturationLOINC + `"}]},"subject":{"reference":"Patient/p"},"valueQuantity":{"value":89,"unit":"%"}}`
	pao2 := `{"resourceType":"Observation","id":"obs-pao2","status":"final","code":{"coding":[{"system":"` + shnsdk.SystemLOINC + `","code":"` + shnsdk.ArterialPaO2LOINC + `"}]},"subject":{"reference":"Patient/p"},"valueQuantity":{"value":56,"unit":"mm[Hg]"}}`
	empty := `{"resourceType":"Bundle","type":"searchset"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/Patient"):
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
		case strings.HasPrefix(r.URL.Path, "/Condition"):
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + condition + `}]}`))
		case strings.HasPrefix(r.URL.Path, "/Observation"):
			if !withOxygen {
				w.Write([]byte(empty))
				return
			}
			code := r.URL.Query().Get("code")
			switch code {
			case shnsdk.SystemLOINC + "|" + shnsdk.OxygenSaturationLOINC:
				w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + o2sat + `}]}`))
			case shnsdk.SystemLOINC + "|" + shnsdk.ArterialPaO2LOINC:
				w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + pao2 + `}]}`))
			default:
				w.Write([]byte(empty))
			}
		default:
			w.Write([]byte(empty))
		}
	}))
	t.Cleanup(srv.Close)
	return fhirclient.New(srv.URL, nil)
}

// TestClinicalContext_OxygenFieldsReadFromRealFHIR proves the live connector reads the
// HomeOxygen-family facts (task-B / register §4 / FR-17) via real FHIR search — the
// verified-finding-1 gap: before this, ClinicalContext never populated
// OxygenSaturationPct/ArterialPaO2mmHg here at all, so gateway/engine's
// homeOxygenAutoFillEvidence cross-check was structurally dead against any real FHIR server.
func TestClinicalContext_OxygenFieldsReadFromRealFHIR(t *testing.T) {
	s := fhirsor.New(oxygenFakeFHIR(t, true))
	cc, ok := s.ClinicalContext("MBR-OXFAKE")
	if !ok {
		t.Fatal("want found=true (anchoring Condition present)")
	}
	if cc.OxygenSaturationPct != "89" || cc.OxygenSaturationRef != "Observation/obs-o2sat" {
		t.Errorf("OxygenSaturationPct=%q OxygenSaturationRef=%q, want (\"89\", \"Observation/obs-o2sat\")",
			cc.OxygenSaturationPct, cc.OxygenSaturationRef)
	}
	if cc.ArterialPaO2mmHg != "56" || cc.ArterialPaO2Ref != "Observation/obs-pao2" {
		t.Errorf("ArterialPaO2mmHg=%q ArterialPaO2Ref=%q, want (\"56\", \"Observation/obs-pao2\")",
			cc.ArterialPaO2mmHg, cc.ArterialPaO2Ref)
	}
	// Sanity: the fake's encoded values round-trip through the same strconv.Itoa the
	// production code path uses (int truncation of the FHIR decimal), not a coincidence.
	if got, _ := strconv.Atoi(cc.OxygenSaturationPct); got != 89 {
		t.Fatalf("OxygenSaturationPct did not round-trip as an int: %q", cc.OxygenSaturationPct)
	}
}

// TestClinicalContext_OxygenFieldsAbsentIsHonestZero is the rejection/negative row task-B
// requires: with the oxygen Observations ABSENT (search returns empty), ClinicalContext must
// return the honest zero value for both fields — never fabricate a value, and never silently
// fall back to some other family's number. This is what homeOxygenAutoFillEvidence's caller
// relies on to fall back to unattributed (neither item cross-checks, so neither gets
// Origin="auto") rather than a false "auto" claim.
func TestClinicalContext_OxygenFieldsAbsentIsHonestZero(t *testing.T) {
	s := fhirsor.New(oxygenFakeFHIR(t, false))
	cc, ok := s.ClinicalContext("MBR-OXFAKE")
	if !ok {
		t.Fatal("want found=true (anchoring Condition present)")
	}
	if cc.OxygenSaturationPct != "" || cc.OxygenSaturationRef != "" {
		t.Errorf("absent O2-sat Observation: got (%q,%q), want (\"\",\"\")", cc.OxygenSaturationPct, cc.OxygenSaturationRef)
	}
	if cc.ArterialPaO2mmHg != "" || cc.ArterialPaO2Ref != "" {
		t.Errorf("absent PaO2 Observation: got (%q,%q), want (\"\",\"\")", cc.ArterialPaO2mmHg, cc.ArterialPaO2Ref)
	}
}
