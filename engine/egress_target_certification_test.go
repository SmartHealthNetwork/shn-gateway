package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type targetEvidenceProbe struct {
	legacyCalls, evidenceCalls int
	profile                    string
	body                       []byte
	evidence                   shnsdk.ValidationEvidence
	onEvidence                 func()
}

func (v *targetEvidenceProbe) Validate(_ context.Context, body []byte, _ string) (shnsdk.Result, error) {
	v.legacyCalls++
	body[0] = 'x' // the target checker does not own the transmitted bytes
	return shnsdk.Result{Valid: true}, nil
}

func (v *targetEvidenceProbe) ValidateEvidence(_ context.Context, body []byte, profile string) (shnsdk.ValidationEvidence, error) {
	v.evidenceCalls++
	v.profile = profile
	v.body = bytes.Clone(body)
	body[0] = 'x'
	if v.onEvidence != nil {
		v.onEvidence()
	}
	return v.evidence, nil
}

func TestBridgedTargetCertifiesVersionedProfileOnOwnedBytesEveryLevel(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			body := pasGolden(t, "2.2/conformant/pas-submit-request.json")
			want := bytes.Clone(body)
			probe := &targetEvidenceProbe{evidence: shnsdk.ValidationEvidence{ExecutionAttempted: true,
				Profile:     shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid},
				Terminology: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationUnavailable}}}
			g := &Gateway{cfg: Config{ConformanceEnforcement: level, ValidatorsByLine: map[string]shnsdk.Validator{"2.2": probe}}}
			ctx := withFindingContext(context.Background(), findingContext{LegType: "pas-claim", CorrelationID: "target-proof", Seam: "originate", Whose: "own"})
			status, msg := g.validateFHIREgressOrBridged(ctx, body, "pa.pas", "2.2", true)
			profile, ok := profileFor("PASRequestBundle", "2.2", "pas-claim")
			if status != 0 || msg != "" || !ok || probe.profile != profile || probe.legacyCalls != 0 || probe.evidenceCalls != 1 || !bytes.Equal(probe.body, want) || !bytes.Equal(body, want) {
				t.Fatalf("target proof status=%d msg=%q profile=%q legacy=%d evidence=%d checkerBytesSame=%t wireBytesSame=%t", status, msg, probe.profile, probe.legacyCalls, probe.evidenceCalls, bytes.Equal(probe.body, want), bytes.Equal(body, want))
			}
		})
	}
}

func TestBridgedTargetActualOperationValidatorEvidence(t *testing.T) {
	body := pasGolden(t, "2.2/conformant/pas-submit-request.json")
	profile, ok := profileFor("PASRequestBundle", "2.2", "pas-claim-update")
	if !ok {
		t.Fatal("missing target profile")
	}
	for _, tc := range []struct {
		name, outcome string
		status        int
	}{
		{"valid profile, terminology unproven", `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`, 0},
		{"invalid profile", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid"}]}`, http.StatusBadGateway},
		{"unsupported profile", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","details":{"coding":[{"system":"http://hl7.org/fhir/java-core-messageId","code":"Validation_VAL_Profile_Unknown"}]}}]}`, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				got, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || r.URL.Path != "/Bundle/$validate" || r.URL.Query().Get("profile") != profile || !bytes.Equal(got, body) {
					t.Errorf("target request method=%s path=%s profile=%q bytes=%d err=%v", r.Method, r.URL.Path, r.URL.Query().Get("profile"), len(got), err)
				}
				w.Header().Set("Content-Type", "application/fhir+json")
				fmt.Fprint(w, tc.outcome)
			}))
			defer srv.Close()
			g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementNone,
				ValidatorsByLine: map[string]shnsdk.Validator{"2.2": shnsdk.NewOperationValidator(srv.URL)}}}
			ctx := withFindingContext(context.Background(), findingContext{LegType: "pas-claim-update", CorrelationID: "target-http", Seam: "originate", Whose: "own"})
			status, _ := g.validateFHIREgressOrBridged(ctx, body, "pa.pas", "2.2", true)
			if status != tc.status || calls != 1 {
				t.Fatalf("target profile result status=%d calls=%d, want %d/1", status, calls, tc.status)
			}
		})
	}
}

func TestNativeTargetNoneMakesNoOptionalValidationCall(t *testing.T) {
	probe := &targetEvidenceProbe{}
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementNone, ValidatorsByLine: map[string]shnsdk.Validator{"2.2": probe}}}
	body := []byte(`{"resourceType":"Bundle"}`)
	status, msg := g.validateFHIREgressOrBridged(context.Background(), body, "pa.pas", "2.2", false)
	if status != 0 || msg != "" || probe.legacyCalls != 0 || probe.evidenceCalls != 0 {
		t.Fatalf("native none checked: %d %q legacy=%d evidence=%d", status, msg, probe.legacyCalls, probe.evidenceCalls)
	}
}

func TestBridgedTargetRejectsMissingOrUnprovedProfile(t *testing.T) {
	for _, tc := range []struct {
		name, leg     string
		evidence      shnsdk.ValidationEvidence
		lane          bool
		status, calls int
	}{
		{"missing lane", "pas-claim", shnsdk.ValidationEvidence{}, false, http.StatusServiceUnavailable, 0},
		{"missing operation", "", *syntheticEvidence(), true, http.StatusServiceUnavailable, 0},
		{"invalid profile", "pas-claim", shnsdk.ValidationEvidence{ExecutionAttempted: true, Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationInvalid}}, true, http.StatusBadGateway, 1},
		{"unsupported profile", "pas-claim", shnsdk.ValidationEvidence{ExecutionAttempted: true, Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationUnavailable}}, true, http.StatusServiceUnavailable, 1},
		{"no execution", "pas-claim", shnsdk.ValidationEvidence{Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationValid}}, true, http.StatusServiceUnavailable, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &targetEvidenceProbe{evidence: tc.evidence}
			g := &Gateway{cfg: Config{HolderID: "target-proof", ConformanceEnforcement: EnforcementNone}}
			if tc.lane {
				g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.2": probe}
			}
			ctx := withFindingContext(context.Background(), findingContext{LegType: tc.leg, CorrelationID: "target-reject", Seam: "originate", Whose: "own"})
			body := []byte(`{"resourceType":"Bundle"}`)
			status, msg := g.validateFHIREgressOrBridged(ctx, body, "pa.pas", "2.2", true)
			if status != tc.status || msg == "" || probe.evidenceCalls != tc.calls || probe.legacyCalls != 0 {
				t.Fatalf("target refusal status=%d msg=%q evidence=%d legacy=%d, want %d/%d", status, msg, probe.evidenceCalls, probe.legacyCalls, tc.status, tc.calls)
			}
		})
	}
}

func TestBridgedTargetCancellationCannotAuthorizeOutput(t *testing.T) {
	for _, during := range []bool{false, true} {
		name := "before"
		if during {
			name = "during"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probe := &targetEvidenceProbe{evidence: *syntheticEvidence()}
			if during {
				probe.onEvidence = cancel
			} else {
				cancel()
			}
			g := &Gateway{cfg: Config{ValidatorsByLine: map[string]shnsdk.Validator{"2.2": probe}}}
			ctx = withFindingContext(ctx, findingContext{LegType: "pas-claim-update", CorrelationID: "target-cancel"})
			status, _ := g.validateFHIREgressOrBridged(ctx, []byte(`{"resourceType":"Bundle"}`), "pa.pas", "2.2", true)
			wantCalls := 0
			if during {
				wantCalls = 1
			}
			if status != http.StatusServiceUnavailable || probe.evidenceCalls != wantCalls || probe.legacyCalls != 0 {
				t.Fatalf("canceled target status=%d evidence=%d legacy=%d", status, probe.evidenceCalls, probe.legacyCalls)
			}
		})
	}
}

func TestBridgedTargetLegacyOnlyCheckerCannotCertify(t *testing.T) {
	legacy := &legacyEvidenceProbe{}
	g := &Gateway{cfg: Config{ValidatorsByLine: map[string]shnsdk.Validator{"2.2": legacy}}}
	ctx := withFindingContext(context.Background(), findingContext{LegType: "pas-claim", CorrelationID: "legacy-target"})
	status, _ := g.validateFHIREgressOrBridged(ctx, []byte(`{"resourceType":"Bundle"}`), "pa.pas", "2.2", true)
	if status != http.StatusServiceUnavailable || legacy.calls.Load() != 0 {
		t.Fatalf("legacy-only checker certified target: status=%d calls=%d", status, legacy.calls.Load())
	}
}

// PCV-10/15: a transformed update with a refused target profile never crosses
// the recipient boundary, while the earlier pended application reply survives.
func TestBridgedTargetRefusalStopsResumeDispatchAndCarriesPriorReply(t *testing.T) {
	declared21 := []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}
	gw, stub := newPendResumeFixture(t, pendFixtureOpts{
		member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad",
		pendedItem: "patient-reported-functional-status", extraRoles: map[string]string{"phg": "phg"},
		declared: declared21, deliveryOnly: true,
	})
	driftRegistryTo(t, gw, "payer", []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS22})
	gw.cfg.EgressNativeLines = []string{"2.1"}
	gw.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.1": &recordingValidator{valid: true}, "2.2": &recordingValidator{valid: true}}

	start := httptest.NewRecorder()
	st, ok := gw.scenarioToPend(start, httptest.NewRequest(http.MethodPost, "/scenario/uc07", nil), "uc07", "MBR-UC07")
	if !ok || st.pasReply.ApplicationReply == nil {
		t.Fatalf("fixture did not receive pended application reply: status=%d body=%s", start.Code, start.Body.String())
	}
	probe := &targetEvidenceProbe{evidence: shnsdk.ValidationEvidence{ExecutionAttempted: true,
		Profile: shnsdk.ValidationCheckEvidence{State: shnsdk.ValidationInvalid, Code: "synthetic-target-refusal"}}}
	gw.cfg.ValidatorsByLine["2.2"] = probe
	resume := httptest.NewRecorder()
	if gw.completePatient(resume, httptest.NewRequest(http.MethodPost, "/scenario/uc07/complete", nil), st, "") {
		t.Fatal("target-invalid transformed update completed")
	}
	if resume.Code != http.StatusBadGateway || probe.evidenceCalls != 1 || probe.legacyCalls != 0 || len(stub.claimedFor("pas-claim-update")) != 0 {
		t.Fatalf("target refusal status=%d evidence=%d legacy=%d dispatched=%v body=%s", resume.Code, probe.evidenceCalls, probe.legacyCalls, stub.claimedFor("pas-claim-update"), resume.Body.String())
	}
	var result struct {
		Error            string                `json:"error"`
		ApplicationReply *ApplicationReplyView `json:"applicationReply"`
	}
	if err := json.Unmarshal(resume.Body.Bytes(), &result); err != nil || result.Error != "adaptation_failed" || result.ApplicationReply == nil || result.ApplicationReply.Leg != "patient-dtr" || result.ApplicationReply.BodyBase64 == "" {
		leg, size := "", 0
		if result.ApplicationReply != nil {
			leg, size = result.ApplicationReply.Leg, len(result.ApplicationReply.BodyBase64)
		}
		t.Fatalf("prior reply lost on target refusal: error=%q leg=%q bodySize=%d parse=%v", result.Error, leg, size, err)
	}
}
