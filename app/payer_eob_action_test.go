package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestPayerEOBActionConfig_RequiresOwnedRegisteredConnector(t *testing.T) {
	e := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/etc/shn", "SHN_DISCOVERY_URL": "https://disc.test",
		"PAYER_EOB_ACTIONS": "1", "PAYER_EOB_ACTIONS_BASE_URL": "https://payer.example",
		"FHIR_VALIDATE_URL": "https://validator.example/fhir",
	}
	if _, err := loadConfig(env(e)); err == nil || !strings.Contains(err.Error(), "INGRESS_CLIENTS_FILE") {
		t.Fatalf("payer EOB action without registered connector: %v", err)
	}
	e["INGRESS_CLIENTS_FILE"] = writeClientsFile(t, testValidClientsJSON(t))
	if _, err := loadConfig(env(e)); err == nil || !strings.Contains(err.Error(), "payer_eob_record") {
		t.Fatalf("payer EOB action with no explicit grant: %v", err)
	}
	granted := strings.Replace(testValidClientsJSON(t), `"client_id":"br-provider"`, `"client_id":"payer-source","payer_eob_record":true`, 1)
	e["INGRESS_CLIENTS_FILE"] = writeClientsFile(t, granted)
	if _, err := loadConfig(env(e)); err == nil || !strings.Contains(err.Error(), "system/ExplanationOfBenefit.write") {
		t.Fatalf("payer EOB action without write scope: %v", err)
	}
	granted = strings.Replace(granted, "system/Davinci.write", "system/ExplanationOfBenefit.write", 1)
	e["INGRESS_CLIENTS_FILE"] = writeClientsFile(t, granted)
	delete(e, "FHIR_VALIDATE_URL")
	if _, err := loadConfig(env(e)); err == nil || !strings.Contains(err.Error(), "FHIR_VALIDATE_URL") {
		t.Fatalf("payer EOB action without certifier: %v", err)
	}
	e["FHIR_VALIDATE_URL"] = "https://validator.example/fhir"
	cfg, err := loadConfig(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PayerEOBActionsEnabled || !cfg.IngressClients["payer-source"].PayerEOBRecord {
		t.Fatal("payer EOB action/grant was not wired")
	}
}

func TestPayerEOBActionConfig_NoneStillBuildsDedicatedCertifier(t *testing.T) {
	var called bool
	validator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost || !strings.Contains(r.URL.String(), "$validate") {
			t.Errorf("action checker request %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`)
	}))
	defer validator.Close()
	cfg := config{PayerEOBActionsEnabled: true, FHIRValidateURL: validator.URL}
	if cfg.ConformanceEnforcement != 0 {
		t.Fatal("test requires native none")
	}
	action := payerEOBActionValidator(cfg)
	if action == nil {
		t.Fatal("explicit action lost its certifier at native none")
	}
	ev, err := action.(shnsdk.EvidenceValidator).ValidateEvidence(context.Background(), []byte(`{"resourceType":"ExplanationOfBenefit"}`), "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-priorauthorization")
	if err != nil || !called || !ev.ExecutionAttempted || ev.Profile.State != shnsdk.ValidationValid {
		t.Fatalf("dedicated action checker: called=%v evidence=%+v err=%v", called, ev, err)
	}
}
