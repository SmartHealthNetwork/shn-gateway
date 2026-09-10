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
	"time"
)

func TestNativePASRetainsApprovedBundle(t *testing.T) {
	body, err := assembleTerminalPASBundle([]byte(assemblyRealPending), []byte(assemblyRealTerminal), fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	srv := stubPartnerSrv(t, 200, body)
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
	result, err := n.Handle(context.Background(), "pas-claim", "corr-native", "PCI-1", originatorBuiltConformantBundle(t, "MBR-COVERED"))
	if err != nil || result.Status != 0 {
		t.Fatalf("status=%d err=%v", result.Status, err)
	}
	if !bytes.Equal(body, result.ResponseFHIR) || !result.ResponseRelayed || !result.ResponseSubjectForeign {
		t.Fatal("native Bundle or provenance lost")
	}
}

func TestNativePASPollAssemblesRetainedGraph(t *testing.T) {
	for _, mutation := range []string{"valid", "id", "patient", "request", "new reference"} {
		t.Run(mutation, func(t *testing.T) {
			terminal := []byte(assemblyRealTerminal)
			if mutation != "valid" {
				var obj map[string]any
				json.Unmarshal(terminal, &obj)
				switch mutation {
				case "id":
					obj["id"] = "changed"
				case "patient", "request":
					obj[mutation] = map[string]any{"reference": "Patient/foreign"}
				default:
					obj["insurer"] = map[string]any{"reference": "Organization/new"}
				}
				terminal, _ = json.Marshal(obj)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.Write(terminal)
				} else {
					w.Write([]byte(assemblyRealPending))
				}
			}))
			defer srv.Close()
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock, WithPendReQuery(time.Second, time.Millisecond))
			request := originatorBuiltConformantBundle(t, "MBR-COVERED")
			var req map[string]any
			json.Unmarshal(request, &req)
			for _, v := range req["entry"].([]any) {
				r := v.(map[string]any)["resource"].(map[string]any)
				if r["resourceType"] == "Claim" {
					r["item"] = []any{map[string]any{"extension": []any{map[string]any{"url": pasInfoChangedExtURL, "valueBoolean": true}}}}
				}
			}
			request, _ = json.Marshal(req)
			result, err := n.Handle(context.Background(), "pas-claim", "corr-poll", "PCI-1", request)
			if mutation != "valid" {
				if err == nil && result.Status == 0 {
					t.Fatal("accepted changed terminal graph")
				}
				if result.Commit != nil {
					t.Fatal("invalid assembly has commit")
				}
				return
			}
			if err != nil || result.Status != 0 {
				t.Fatalf("status=%d err=%v message=%s", result.Status, err, result.Message)
			}
			if result.ResponseRelayed || !result.ResponseSubjectForeign {
				t.Fatal("assembly misrepresented as relay")
			}
			if err := validatePASBundleGraph(result.ResponseFHIR); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			json.Unmarshal(result.ResponseFHIR, &got)
			if got["resourceType"] != "Bundle" {
				t.Fatal("lost native response")
			}
			if !bytes.Contains(result.ResponseFHIR, []byte(`"resourceType":"Task"`)) {
				t.Fatal("retained Task lost")
			}
		})
	}
}

func TestPASForeignGraphSubjectFence(t *testing.T) {
	for _, field := range []string{"Claim", "Coverage", "ServiceRequest", "Patient"} {
		t.Run(field, func(t *testing.T) {
			var b map[string]any
			json.Unmarshal([]byte(assemblyRealPending), &b)
			other := map[string]any{"fullUrl": "http://localhost:8081/fhir/Patient/other", "resource": map[string]any{"resourceType": "Patient", "id": "other"}}
			b["entry"] = append(b["entry"].([]any), other)
			for _, v := range b["entry"].([]any) {
				r := v.(map[string]any)["resource"].(map[string]any)
				if r["resourceType"] == field {
					key := "patient"
					if field == "Coverage" {
						key = "beneficiary"
					}
					if field == "ServiceRequest" {
						key = "subject"
					}
					if field != "Patient" {
						r[key] = map[string]any{"reference": "http://localhost:8081/fhir/Patient/other"}
					}
				}
			}
			raw, _ := json.Marshal(b)
			g := &Gateway{}
			status, _ := g.fenceResponseSubject("pas-claim", "Patient/MBR-COVERED", LegResult{ResponseFHIR: raw, ResponseSubjectForeign: true})
			if status != http.StatusForbidden {
				t.Fatalf("foreign sibling conflict status=%d", status)
			}
		})
	}
	g := &Gateway{}
	status, msg := g.fenceResponseSubject("pas-claim", "Patient/MBR-COVERED", LegResult{ResponseFHIR: []byte(assemblyRealPending), ResponseSubjectForeign: true})
	if status != 0 {
		t.Fatalf("foreign namespace incorrectly compared with bound member: %d %s", status, msg)
	}
}

type pasAssemblyValidator struct {
	profile string
	valid   bool
	err     error
}

func (v *pasAssemblyValidator) Validate(ctx context.Context, b []byte, profile string) (shnsdk.Result, error) {
	v.profile = profile
	if ctx.Err() != nil {
		return shnsdk.Result{}, ctx.Err()
	}
	return shnsdk.Result{Valid: v.valid}, v.err
}
func TestPASAssemblyCertificationAndProvenance(t *testing.T) {
	raw, err := assembleTerminalPASBundle([]byte(assemblyRealPending), []byte(assemblyRealTerminal), fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "invalid", "unavailable", "missing lane", "cancelled", "unknown line", "invalid flags", "long correlation"} {
		t.Run(mode, func(t *testing.T) {
			validator := &pasAssemblyValidator{valid: true}
			g := &Gateway{cfg: Config{HolderID: "payer", Clock: fixedClock, ValidatorsByLine: map[string]shnsdk.Validator{"2.0": validator}}}
			result := LegResult{ResponseFHIR: raw, ResponseAssembled: true, ResponseSubjectForeign: true}
			ctx := context.Background()
			token := "pa.pas@2.0"
			correlation := "corr-assembly"
			switch mode {
			case "invalid":
				validator.valid = false
			case "unavailable":
				validator.err = fmt.Errorf("unavailable")
			case "missing lane":
				g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.1": validator}
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "unknown line":
				token = "pa.pas@9.9"
			case "invalid flags":
				result.ResponseRelayed = true
			case "long correlation":
				correlation = strings.Repeat("a", 257)
			}
			event, status, _ := g.validatePASResult(ctx, result, token, correlation, "pas-claim")
			if mode != "valid" {
				if status == 0 || event != nil {
					t.Fatal("accepted uncertified assembly")
				}
				return
			}
			if status != 0 || event == nil {
				t.Fatal("missing successful provenance")
			}
			if validator.profile != "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-response-bundle|2.0.1" {
				t.Fatalf("wrong profile %s", validator.profile)
			}
			if event.Kind != "leg.assembled" || event.CorrelationID != correlation || !bytes.Contains(event.Payload, []byte(`"resourceType":"Provenance"`)) || !bytes.Contains(event.Payload, []byte("Organization/payer")) {
				t.Fatal("incomplete assembly provenance")
			}
			if bytes.Contains(event.Payload, []byte("SubscriberExample")) || bytes.Contains(event.Payload, []byte("AUTH-")) || bytes.Contains(event.Payload, []byte("LossReport")) {
				t.Fatal("clinical content or invented transform in provenance")
			}
		})
	}
}

// fixturePASResponse constructs a closed SYNTHETIC test graph from decision
// content. The fixed Patient/Claim/Organization data is explicitly test-owned;
// it is never used to claim that historical payer captures were complete.
func fixturePASResponse(t *testing.T, decision []byte, bundle bool) []byte {
	t.Helper()
	var top map[string]any
	if json.Unmarshal(decision, &top) != nil {
		t.Fatal("invalid decision fixture")
	}
	cr := top
	hasTask := false
	if top["resourceType"] == "Bundle" {
		cr = nil
		for _, v := range top["entry"].([]any) {
			r := v.(map[string]any)["resource"].(map[string]any)
			if r["resourceType"] == "ClaimResponse" {
				cr = r
			}
			if r["resourceType"] == "Task" {
				hasTask = true
			}
		}
	}
	if cr == nil || cr["resourceType"] != "ClaimResponse" {
		return decision
	}
	if cr["id"] == nil {
		cr["id"] = "fixture-cr"
	}
	cr["patient"] = map[string]any{"reference": "Patient/p"}
	cr["request"] = map[string]any{"reference": "Claim/c"}
	if cr["insurer"] != nil {
		cr["insurer"] = map[string]any{"reference": "Organization/insurer"}
	}
	if cr["requestor"] != nil {
		cr["requestor"] = map[string]any{"reference": "Organization/requestor"}
	}
	if !bundle {
		raw, _ := json.Marshal(cr)
		return raw
	}
	b := assemblySmallGraph()
	entries := b["entry"].([]any)
	entries[0].(map[string]any)["resource"] = cr
	entries[0].(map[string]any)["fullUrl"] = "https://payer.test/fhir/ClaimResponse/" + cr["id"].(string)
	for _, id := range []string{"insurer", "requestor"} {
		if cr[id] != nil {
			entries = append(entries, map[string]any{"fullUrl": "https://payer.test/fhir/Organization/" + id, "resource": map[string]any{"resourceType": "Organization", "id": id}})
		}
	}
	if hasTask {
		entries = append(entries, map[string]any{"fullUrl": "urn:uuid:10000000-0000-4000-8000-000000000003", "resource": map[string]any{"resourceType": "Task", "id": "fixture-task", "status": "requested", "intent": "order", "for": map[string]any{"reference": "https://payer.test/fhir/Patient/p"}}})
	}
	b["entry"] = entries
	b["timestamp"] = fixedClock().Format(time.RFC3339Nano)
	raw, _ := json.Marshal(b)
	if err := validatePASBundleGraph(raw); err != nil {
		t.Fatal("invalid synthetic fixture graph")
	}
	return raw
}

type pasResultResponder struct {
	result       LegResult
	err          error
	beforeReturn func()
}

func (r pasResultResponder) Handle(context.Context, string, string, string, []byte) (LegResult, error) {
	if r.beforeReturn != nil {
		r.beforeReturn()
	}
	return r.result, r.err
}
func TestPASAssemblyInboundCommitOrdering(t *testing.T) {
	assembled, err := assembleTerminalPASBundle([]byte(assemblyRealPending), []byte(assemblyRealTerminal), fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		for _, mode := range []string{"valid", "validator reject", "validator unavailable", "cancelled", "seal failure", "store failure", "responder error", "bare operation"} {
			t.Run(leg+"/"+mode, func(t *testing.T) {
				g, requester := newInboundTestGateway(t, true)
				pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
				request := conformantPASBundleWithQR(t, "MBR-COVERED")
				if leg == "pas-claim-update" {
					request = originatorBuiltConformantUpdateBundle(t)
				}
				commits, rollbacks, observed := 0, 0, 0
				result := LegResult{ResponseFHIR: assembled, ResponseAssembled: true, ResponseSubjectForeign: true, Commit: func() error {
					commits++
					if mode == "store failure" {
						return fmt.Errorf("store unavailable")
					}
					return nil
				}, Rollback: func() { rollbacks++ }}
				if mode == "bare operation" {
					result.ResponseFHIR = claimResponseFor(t, "Patient/MBR-COVERED")
					result.ResponseAssembled = false
					result.ResponseSubjectForeign = false
				}
				responder := pasResultResponder{result: result}
				if mode == "responder error" {
					responder.err = fmt.Errorf("transport failed")
				}
				g.cfg.Responder = responder
				validator := &pasAssemblyValidator{valid: mode != "validator reject"}
				if mode == "validator unavailable" {
					validator.err = fmt.Errorf("validator unavailable")
				}
				g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": validator}
				g.cfg.Observer = func(e ObserverEvent) {
					if e.Kind == "leg.assembled" {
						observed++
						if commits != 1 {
							t.Error("assembly event before Commit")
						}
					}
				}
				env := shnsdk.Envelope{Metadata: shnsdk.Metadata{Sender: requester.ID, Recipient: "payer", TransactionType: leg, AuthorityFrame: "payer-coverage", CorrelationID: "corr-assembly-inbound"}}
				if mode == "seal failure" {
					env.Metadata.Sender = "missing-requester"
				}
				tok := shnsdk.Token{Subject: pci, CorrelationID: env.Metadata.CorrelationID}
				rec := httptest.NewRecorder()
				r := newSignedInboundRequest(t, g, requester.ID)
				if mode == "cancelled" {
					ctx, cancel := context.WithCancel(r.Context())
					responder.beforeReturn = cancel
					g.cfg.Responder = responder
					r = r.WithContext(ctx)
				}
				if leg == "pas-claim" {
					g.handlePASNativeInbound(rec, r, env, tok, request, "pa.pas@2.0")
				} else {
					g.handlePASUpdateNativeInbound(rec, r, env, tok, request, "pa.pas@2.0")
				}
				if mode == "valid" {
					if rec.Code != 200 || commits != 1 || rollbacks != 0 || observed != 1 {
						t.Fatalf("status=%d commits=%d releases=%d events=%d body=%s", rec.Code, commits, rollbacks, observed, rec.Body.String())
					}
					payload := openResponseLeg(t, requester, rec.Body.Bytes())
					hdr, body, err := shnsdk.DecodeHTTPFrame(payload)
					if err != nil || hdr.Headers[shnsdk.FrameHeaderContractVersion] != "pa.pas@2.0" || !bytes.Equal(body, assembled) {
						t.Fatal("assembled response or certified stamp changed")
					}
					return
				}
				wantCommits := 0
				if mode == "store failure" {
					wantCommits = 1
				}
				if rec.Code == 200 || commits != wantCommits || rollbacks != 1 || observed != 0 {
					t.Fatalf("status=%d commits=%d releases=%d events=%d", rec.Code, commits, rollbacks, observed)
				}
			})
		}
	}
}

func TestPASSHNProducedGraphSubjectFence(t *testing.T) {
	b := assemblySmallGraph()
	raw, _ := json.Marshal(b)
	g := &Gateway{}
	if status, msg := g.fenceResponseSubject("pas-claim", "Patient/p", LegResult{ResponseFHIR: raw}); status != 0 {
		t.Fatalf("bound graph rejected: %d %s", status, msg)
	}
	b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "https://payer.test/fhir/Patient/other", "resource": map[string]any{"resourceType": "Patient", "id": "other"}})
	b["entry"].([]any)[2].(map[string]any)["resource"].(map[string]any)["patient"] = map[string]any{"reference": "Patient/other"}
	raw, _ = json.Marshal(b)
	if status, _ := g.fenceResponseSubject("pas-claim", "Patient/p", LegResult{ResponseFHIR: raw}); status != http.StatusForbidden {
		t.Fatal("accepted SHN sibling subject conflict")
	}
}

func TestPASContainedSubjectIdentity(t *testing.T) {
	b := assemblySmallGraph()
	patient := b["entry"].([]any)[1].(map[string]any)["resource"].(map[string]any)
	patient["contained"] = []any{map[string]any{"resourceType": "Procedure", "id": "procedure", "subject": map[string]any{"reference": "#"}}}
	raw, _ := json.Marshal(b)
	if !consistentPASResponseSubjects(raw) {
		t.Fatal("contained subject backreference lost its exact owner")
	}
	patient["contained"] = []any{map[string]any{"resourceType": "Patient", "id": "p"}}
	raw, _ = json.Marshal(b)
	if consistentPASResponseSubjects(raw) {
		t.Fatal("contained patient inherited outer patient identity")
	}
}

func TestPASSubjectFenceAllPrimarySubjectFields(t *testing.T) {
	// R4 patient-bearing examples span both formerly omitted resource types and
	// differently named/repeating subject fields. The guard must not depend on a
	// resource-type allowlist that silently misses new clinical resources.
	rows := []struct {
		resource, field string
		repeated        bool
	}{
		{"AllergyIntolerance", "patient", false}, {"Immunization", "patient", false}, {"ImmunizationEvaluation", "patient", false}, {"ImmunizationRecommendation", "patient", false}, {"MedicationStatement", "subject", false}, {"MedicationAdministration", "subject", false}, {"MedicationDispense", "subject", false}, {"Consent", "patient", false}, {"FamilyMemberHistory", "patient", false}, {"EpisodeOfCare", "patient", false}, {"NutritionOrder", "patient", false}, {"VisionPrescription", "patient", false}, {"MolecularSequence", "patient", false}, {"DetectedIssue", "patient", false}, {"BodyStructure", "patient", false}, {"RelatedPerson", "patient", false}, {"Device", "patient", false}, {"SupplyDelivery", "patient", false}, {"ResearchSubject", "individual", false}, {"EnrollmentRequest", "candidate", false}, {"Account", "subject", true}, {"ActivityDefinition", "subjectReference", false}, {"NewClinicalResource", "subject", false},
	}
	for _, row := range rows {
		for _, form := range []string{"bound", "identifier only", "wrong reference", "empty reference"} {
			t.Run(row.resource+"/"+form, func(t *testing.T) {
				b := assemblySmallGraph()
				ref := map[string]any{"reference": "https://payer.test/fhir/Patient/p"}
				switch form {
				case "identifier only":
					ref = map[string]any{"identifier": map[string]any{"system": "urn:member", "value": "OTHER-MEMBER"}}
				case "wrong reference":
					ref = map[string]any{"reference": "https://foreign.test/fhir/Patient/p"}
				case "empty reference":
					ref = map[string]any{"reference": ""}
				}
				var value any = ref
				if row.repeated {
					value = []any{ref}
				}
				resource := map[string]any{"resourceType": row.resource, "id": "subject-test", row.field: value}
				b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "https://payer.test/fhir/" + row.resource + "/subject-test", "resource": resource})
				raw, _ := json.Marshal(b)
				if consistentPASResponseSubjects(raw) != (form == "bound") {
					t.Fatalf("subject form %s incorrectly accepted/refused", form)
				}
			})
		}
	}
}

func TestPASSubjectFenceNestedAndTypedPatientReferences(t *testing.T) {
	for _, field := range []string{"patient", "subject", "typed reference"} {
		t.Run(field, func(t *testing.T) {
			b := assemblySmallGraph()
			r := b["entry"].([]any)[2].(map[string]any)["resource"].(map[string]any)
			ref := map[string]any{"identifier": map[string]any{"value": "OTHER-MEMBER"}}
			if field == "typed reference" {
				ref["type"] = "Patient"
				r["extension"] = []any{map[string]any{"url": "urn:test:patient-reference", "valueReference": ref}}
			} else {
				r["extension"] = []any{map[string]any{"url": "urn:test:nested", "value": map[string]any{field: ref}}}
			}
			raw, _ := json.Marshal(b)
			if consistentPASResponseSubjects(raw) {
				t.Fatal("unbound nested patient identity bypassed fence")
			}
		})
	}
	// Ordinary subjectless resources remain useful members of the response graph.
	for _, typ := range []string{"Organization", "Practitioner", "PractitionerRole", "Location", "HealthcareService", "Medication", "Library", "ValueSet", "CodeSystem", "Questionnaire"} {
		t.Run(typ, func(t *testing.T) {
			b := assemblySmallGraph()
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "https://payer.test/fhir/" + typ + "/subjectless", "resource": map[string]any{"resourceType": typ, "id": "subjectless"}})
			raw, _ := json.Marshal(b)
			if !consistentPASResponseSubjects(raw) {
				t.Fatal("subjectless resource refused")
			}
		})
	}
}

// Non-subject business references do not establish a patient's identity.
func TestPASLogicalCoverageDoesNotBecomeSubject(t *testing.T) {
	b := assemblySmallGraph()
	entries := b["entry"].([]any)
	for _, entry := range entries {
		resource := entry.(map[string]any)["resource"].(map[string]any)
		if resource["resourceType"] == "Claim" {
			resource["insurance"] = []any{map[string]any{"coverage": map[string]any{"identifier": map[string]any{"system": "urn:coverage", "value": "MEMBERSHIP"}}}}
		}
	}
	raw, _ := json.Marshal(b)
	if !consistentPASResponseSubjects(raw) {
		t.Fatal("logical Coverage incorrectly treated as a patient subject")
	}
}

func TestPASSubjectFenceEncounterClinician(t *testing.T) {
	b := assemblySmallGraph()
	encounter := map[string]any{"resourceType": "Encounter", "id": "visit", "subject": map[string]any{"reference": "Patient/p"}, "participant": []any{map[string]any{"individual": map[string]any{"reference": "Practitioner/clinician"}}}}
	b["entry"] = append(b["entry"].([]any),
		map[string]any{"fullUrl": "https://payer.test/fhir/Encounter/visit", "resource": encounter},
		map[string]any{"fullUrl": "https://payer.test/fhir/Practitioner/clinician", "resource": map[string]any{"resourceType": "Practitioner", "id": "clinician"}})
	raw, _ := json.Marshal(b)
	if !consistentPASResponseSubjects(raw) {
		t.Fatal("same-patient Encounter clinician incorrectly treated as a patient")
	}
	encounter["subject"] = map[string]any{"identifier": map[string]any{"value": "OTHER-MEMBER"}}
	raw, _ = json.Marshal(b)
	if consistentPASResponseSubjects(raw) {
		t.Fatal("Encounter patient bypassed subject binding")
	}
}
