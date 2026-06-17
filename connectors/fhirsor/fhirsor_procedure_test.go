package fhirsor_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// procFakeFHIR has an anchoring Condition (so ClinicalContext returns found=true) and a single
// Procedure coded `procCode`. The Procedure search returns the procedure ONLY when the request's
// code filter contains that code — so a non-spine procedure (not in ProcedureValueSet) yields an
// empty Procedure searchset under the value-set filter, and PriorSurgery must be false.
func procFakeFHIR(t *testing.T, procCode string) *fhirclient.Client {
	t.Helper()
	patient := `{"resourceType":"Patient","id":"p","identifier":[{"system":"urn:shn:member","value":"MBR-PROC"}],"name":[{"family":"Proc"}],"birthDate":"1970-01-01"}`
	condition := `{"resourceType":"Condition","id":"c","code":{"coding":[{"system":"` + shnsdk.SystemICD10CM + `","code":"` + shnsdk.ConditionCodeLumbar + `"}]},"subject":{"reference":"Patient/p"}}`
	procedure := `{"resourceType":"Procedure","id":"pr","status":"completed","code":{"coding":[{"system":"` + shnsdk.SystemSNOMED + `","code":"` + procCode + `"}]},"subject":{"reference":"Patient/p"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		empty := `{"resourceType":"Bundle","type":"searchset"}`
		switch {
		case strings.HasPrefix(r.URL.Path, "/Patient"):
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
		case strings.HasPrefix(r.URL.Path, "/Condition"):
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + condition + `}]}`))
		case strings.HasPrefix(r.URL.Path, "/Procedure"):
			if code := r.URL.Query().Get("code"); code == "" || strings.Contains(code, procCode) {
				w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + procedure + `}]}`))
				return
			}
			w.Write([]byte(empty))
		default:
			w.Write([]byte(empty))
		}
	}))
	t.Cleanup(srv.Close)
	return fhirclient.New(srv.URL, nil)
}

func TestClinicalContext_PriorSurgeryIsCodeAware(t *testing.T) {
	// A spine procedure (in ProcedureValueSet) → PriorSurgery true.
	spine := fhirsor.New(procFakeFHIR(t, shnsdk.ProcLaminectomySNOMED))
	cc, ok := spine.ClinicalContext("MBR-PROC")
	if !ok {
		t.Fatal("want found=true")
	}
	if !cc.PriorSurgery || cc.PriorSurgeryRef == "" {
		t.Errorf("spine procedure: PriorSurgery=%v ref=%q, want true/non-empty", cc.PriorSurgery, cc.PriorSurgeryRef)
	}

	// A non-spine procedure (NOT in ProcedureValueSet) → PriorSurgery false (Flag 4).
	noise := fhirsor.New(procFakeFHIR(t, "80146002")) // appendectomy — not a spine procedure
	cc2, ok2 := noise.ClinicalContext("MBR-PROC")
	if !ok2 {
		t.Fatal("want found=true")
	}
	if cc2.PriorSurgery || cc2.PriorSurgeryRef != "" {
		t.Errorf("non-spine procedure: PriorSurgery=%v ref=%q, want false/empty", cc2.PriorSurgery, cc2.PriorSurgeryRef)
	}
}
