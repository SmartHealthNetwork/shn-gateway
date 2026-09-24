package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// fixedClock is shared by tests that need deterministic participant-owned
// timestamps. It used to live in the native-PAS projection test family.
var fixedClock = func() time.Time { return time.Unix(1700000000, 0).UTC() }

const relayRequester = "holder:requesting-ehr"

const pasInfoChangedExtURL = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-infoChanged"

var payerCreated = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func ledgerState(t *testing.T, store Store, pci, corr string) PendRecord {
	t.Helper()
	ledger, ok := LedgerOf(store)
	if !ok {
		t.Fatal("the fixture store has no pend ledger")
	}
	rec, found, err := ledger.PendRecordOf(pci, corr)
	if err != nil || !found {
		t.Fatalf("no ledger row for %s/%s (found=%v err=%v)", pci, corr, found, err)
	}
	return rec
}

func loadDeniedClaimResponseBytes(t *testing.T) []byte {
	t.Helper()
	b, err := shnsdk.BuildDeniedResponse("Patient/MBR-COVERED", "partner", "denied for test", fixedClock())
	if err != nil {
		t.Fatalf("build denied ClaimResponse: %v", err)
	}
	return b
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

func definitionPackage(t *testing.T, line string) string {
	t.Helper()
	b, err := shnsdk.BuildQuestionnairePackageAtLine(line, []byte(`{"resourceType":"Questionnaire","id":"q","url":"https://fixture.test/Questionnaire/q","status":"active"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// These helpers opt controlled fixtures into the baseline Validator contract.
// They do not claim real endpoint, IG, or terminology qualification.
func syntheticFakeValidator() *shnsdk.FakeValidator { return &shnsdk.FakeValidator{} }

func syntheticLineValidator(line string) *LineFakeValidator {
	return NewLineFakeValidator(line)
}

type syntheticValidatorFunc func([]byte) (shnsdk.Result, error)
type syntheticEvidenceValidatorFunc = syntheticValidatorFunc

func (f syntheticValidatorFunc) Validate(_ context.Context, b []byte, _ string) (shnsdk.Result, error) {
	return f(b)
}

type certificationValidatorFunc func(context.Context, []byte, string) (shnsdk.Result, error)

func (f certificationValidatorFunc) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	return f(ctx, body, profile)
}

func certificationGateway(t *testing.T, validator shnsdk.Validator, observer func(ObserverEvent)) *Gateway {
	t.Helper()
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Clock: time.Now, Validator: validator, Observer: observer}}
	g.startCertification()
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func certificationSubmit(g *Gateway, id string) {
	in := observationInput(EnforcementObserve)
	in.Exchange.correlationID = id
	g.observeContent(in)
}

func certificationFlush(t *testing.T, g *Gateway) { observationFlush(t, g) }
func censusSubjectResolver(holders ...string) SubjectReferenceResolver {
	links := map[PatientReference]string{}
	for member, person := range censusPersonas {
		pci := shnsdk.ResolvePCI(member, person.demo.BirthDate, person.demo.FamilyName)
		for _, holder := range holders {
			links[PatientReference{holder, "fhir-relative", "Patient/" + member}] = pci
			links[PatientReference{holder, shnsdk.MemberSystem, member}] = pci
			links[PatientReference{holder, "https://" + holder + ".example/fhir", "Patient/" + member}] = pci
		}
	}
	return subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
		pci, ok := links[ref]
		return pci, ok, nil
	})
}
