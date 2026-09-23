package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// PCV-04/06: the strict federated-query request can be checked against its
// Task.for subject; this binding is separate from the facility's mandatory
// consent and source-disclosure controls, which apply at every level.
func TestFederatedQueryStrictTaskForSubject(t *testing.T) {
	gw := &Gateway{cfg: Config{SubjectReferenceResolver: censusSubjectResolver("provider")}}
	owner := shnsdk.ResolvePCI("MBR-D-UC05", "1968-03-12", "Johansson-Demo")
	for _, tc := range []struct {
		name, patient string
		want          CheckState
	}{
		{"matching", "Patient/MBR-D-UC05", CheckValid},
		{"other patient", "Patient/MBR-UC04", CheckInvalid},
		{"foreign namespace with same local id", "https://foreign.example/fhir/Patient/MBR-D-UC05", CheckUnavailable},
		{"unresolved reference", "Patient/not-in-census", CheckUnavailable},
		{"missing reference", "", CheckUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := shnsdk.BuildCDexTaskDataRequest("Patient/MBR-D-UC05", "DiagnosticReport", "2024-01-01", "2024-02-01", shnsdk.CDexTaskMeta{
				AuthoredOn: time.Unix(1700000000, 0).UTC(), Requester: "provider", Owner: "metro-spine",
			})
			if err != nil {
				t.Fatal(err)
			}
			var task map[string]any
			if err := json.Unmarshal(body, &task); err != nil {
				t.Fatal(err)
			}
			if tc.patient == "" {
				delete(task, "for")
			} else {
				task["for"] = map[string]any{"reference": tc.patient}
			}
			body, err = json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			got := gw.checkSubjectConsistency(context.Background(), CheckInput{Body: body, Direction: "request", Exchange: ExchangeContext{
				holder: "provider", recipient: "metro-spine", legType: "federated-query", subjectPCI: owner,
			}})
			if got.State != tc.want {
				t.Fatalf("Task.for patient check = %+v, want %s", got, tc.want)
			}
			if tc.name == "other patient" {
				for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
					in := CheckInput{Body: body, Direction: "request", Exchange: ExchangeContext{
						holder: "provider", recipient: "metro-spine", legType: "federated-query", subjectPCI: owner, policy: NewConformancePolicy(level),
					}}
					if err := gw.enforceContent(context.Background(), in); err != nil {
						t.Fatalf("%s changed delivery for foreign Task.for: %v", level, err)
					}
				}
			}
		})
	}
}
