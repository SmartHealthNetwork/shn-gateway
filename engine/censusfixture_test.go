// censusfixture_test.go — the ENGINE-SCALE hermetic persona fixture.
//
// §4.1 deletes the shipped in-process persona census: the published gateway module
// no longer carries a hardcoded demo roster, and every DEPLOYMENT reads its members from a
// real FHIR system of record (gateway/connectors/fhirsor). Hermetic tests still need a
// deterministic SoR, and §4.4's answer — the seeded-fixture SoR — lives substrate-side in
// the root module (test/harness, internal/fixturesor), which this module cannot import.
//
// So the roster the engine's own unit tests drive lives HERE, in a _test.go file: it is
// compiled only into `go test ./engine`, is unreachable from any shipped binary or partner
// import, and no production code path can resolve a member through it. It is the engine's
// test fixture, not a payer.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// censusSoR is the fixture SystemOfRecord + Store the engine's unit tests run against.
// The Store half is the real production MemStore (embedded, not re-implemented), so a
// test exercising the pended-claim ledger or the EOB store exercises the shipped code.
type censusSoR struct {
	*MemStore
}

var (
	_ SystemOfRecord = (*censusSoR)(nil)
	_ Store          = (*censusSoR)(nil)
)

// newCensusSoR returns a fixture SoR with its own (empty) Store state, so two holders in
// one test keep separate auth-number/EOB/pended-claim state.
func newCensusSoR() *censusSoR { return &censusSoR{MemStore: NewMemStore()} }

// unusedResponder satisfies the payer role's REQUIRED content occupant (§3.2's
// fail-closed boot) for tests whose subject is not payer content at all — error relay,
// inbound auth, coverage-derived routing, the engine-side eligibility handler. It never
// answers: a leg reaching it means the test drove payer content it did not intend to, so
// it panics rather than inventing a verdict.
type unusedResponder struct{}

func (unusedResponder) Handle(_ context.Context, leg, _, _ string, _ []byte) (LegResult, error) {
	panic("engine test fixture: unusedResponder was asked to answer leg " + leg + " — this test needs a real occupant")
}

var _ LegResponder = unusedResponder{}

// approvingPASResponder is a minimal payer content occupant for tests whose subject is
// the ENGINE's own framing/fencing behaviour, not payer policy: it answers pas-claim with
// a conformant ClaimResponse approval for whatever member the submitted bundle names. The
// verdict is FIXED on purpose — a framing test wants content held constant — and it is
// not a policy: nothing here reads the QuestionnaireResponse.
type approvingPASResponder struct{ clock func() time.Time }

func (r approvingPASResponder) Handle(_ context.Context, leg, corrID, _ string, requestFHIR []byte) (LegResult, error) {
	if leg != "pas-claim" {
		panic("engine test fixture: approvingPASResponder only answers pas-claim, got " + leg)
	}
	s, status, msg := parseConformantPASSubjects(requestFHIR)
	if status != 0 {
		return LegResult{Status: status, Message: msg}, nil
	}
	now := r.clock()
	crJSON, err := shnsdk.BuildClaimResponseAtLine("2.0", "AUTH-0001", now.Add(24*time.Hour).Format(time.RFC3339), "Patient/"+s.member, corrID, now)
	if err != nil {
		return LegResult{}, err
	}
	return LegResult{ResponseFHIR: crJSON}, nil
}

var _ LegResponder = approvingPASResponder{}

// testQuestionnairePackage wraps a bare Questionnaire into the $questionnaire-package
// collection Bundle a PAYER answers the DTR leg with — as TEST INPUT.
//
// The engine used to build this itself, for the in-process payer that no longer exists: a
// payer now relays its own Da Vinci endpoint's package bytes verbatim, so the production
// builder was deleted (§3.2). The suites below still need a package to FEED the gateway,
// which is a fixture concern, not a gateway one — hence this test-only builder. It keeps the
// deleted builder's signature so the call sites are a rename, nothing more.
func testQuestionnairePackage(questionnaire []byte) ([]byte, error) {
	var q struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(questionnaire, &q); err != nil {
		return nil, fmt.Errorf("testQuestionnairePackage: questionnaire is not valid json: %w", err)
	}
	if q.URL == "" {
		return nil, errors.New("testQuestionnairePackage: questionnaire has no url for the entry fullUrl")
	}
	return json.Marshal(map[string]any{
		"resourceType": "Bundle",
		"type":         "collection",
		"entry": []map[string]any{{
			"fullUrl":  q.URL,
			"resource": json.RawMessage(questionnaire),
		}},
	})
}

// mustNew constructs a Gateway or fails the test. New returns an error since §3.2's
// fail-closed payer boot (a payer with no content occupant), so every in-package
// construction site funnels through here rather than dropping the error.
func mustNew(t *testing.T, cfg Config) *Gateway {
	t.Helper()
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return g
}

type censusPersona struct {
	demo    Demo
	inforce bool
	reason  string
	// clinical is the provider-LOCAL clinical context; hasClinical is false
	// for personas with no clinical data to auto-fill from.
	clinical    shnsdk.ClinicalContext
	hasClinical bool
}

var censusPersonas = map[string]censusPersona{
	"MBR-COVERED": {
		demo:    Demo{BirthDate: "1975-04-02", FamilyName: "Johansson"},
		inforce: true,
		reason:  "",
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
		},
		hasClinical: true,
	},
	"MBR-NOTCOVERED": {
		demo:    Demo{BirthDate: "1980-09-15", FamilyName: "Reyes"},
		inforce: false,
		reason:  "coverage-terminated",
	},
	// MBR-UC04 (Maria Chen) — UC-04 pended-on-missing-DiagnosticReport (FR-35/39).
	// PriorSurgery=true triggers the pend; weeks=6 means it approves once the
	// operative report (SupplementalReport) is attached via ClaimUpdate (FR-32).
	"MBR-UC04": {
		demo:    Demo{BirthDate: "1982-11-03", FamilyName: "Chen"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			PriorSurgery:             true,
			PriorSurgeryRef:          "Procedure/proc-laminectomy",
		},
		hasClinical: true,
	},
	// MBR-UC06 (David Reyes) — UC-06 pended-on-missing-attested-functional-status
	// (FR-35/39). HighDisability=true triggers the pend; weeks=6 means it approves
	// once the clinician-attested functional-status item is supplied (manual entry
	// path — no SupplementalReport for this member).
	"MBR-UC06": {
		demo:    Demo{BirthDate: "1969-07-21", FamilyName: "Reyes"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			HighDisability:           true,
			HighDisabilityRef:        "Observation/obs-odi",
		},
		hasClinical: true,
	},
	// MBR-UC05 (Linda Johansson) — UC-05 federated EXTERNAL retrieval. PriorSurgery
	// pends the PA; the operative report is NOT local (SupplementalReport returns
	// false), forcing the consent-gated federated query to metro-spine.
	"MBR-UC05": {
		demo:    Demo{BirthDate: "1968-03-12", FamilyName: "Johansson"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			PriorSurgery:             true,
			PriorSurgeryRef:          "Procedure/proc-microdiscectomy",
		},
		hasClinical: true,
	},
	// MBR-UC08 — UC-08 denial-driver: only 4 weeks of conservative therapy (< 6),
	// no prior surgery, not high-disability → Adjudicate returns Denied (FR-22/35).
	// NeuroDeficitRef is included so the auto-filled QR carries a non-empty
	// information-origin sourceReference (a missing Ref produces an empty reference
	// that fails live FHIR validation — real UC-05 bug, condition M51.16).
	"MBR-UC08": {
		demo:    Demo{BirthDate: "1971-02-09", FamilyName: "Okafor"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 4,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
		},
		hasClinical: true,
	},
	// MBR-D-UC05 — the demo lane's own UC-05 persona (§4.3), distinct from the default
	// lane's MBR-UC05 above (sceneMember resolves to this one only when
	// OriginationProfile is "demo"). The demo lane does not read SoR orders for UC-05
	// (orderSource builds from the originationCodes tuple — sceneMember's
	// own doc comment), so this persona only needs its OWN Coverage/clinical isolation
	// from MBR-UC05, not a distinct order. Same clinical shape as MBR-UC05 (PriorSurgery
	// pends the PA; the operative report is not local, forcing the consent-gated
	// federated query to metro-spine) so it drives the identical UC-05 flow on the
	// demo lane.
	"MBR-D-UC05": {
		demo:    Demo{BirthDate: "1968-03-12", FamilyName: "Johansson-Demo"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			PriorSurgery:             true,
			PriorSurgeryRef:          "Procedure/proc-microdiscectomy",
		},
		hasClinical: true,
	},
	// MBR-D-UC08 — the demo lane's own UC-08 persona (§4.3), distinct from the default
	// lane's MBR-UC08 above (sceneMember resolves to this one only when
	// OriginationProfile is "demo"). No clinical data is needed: the demo/default
	// lane's orderSource never reads SoR clinical facts for UC-08's J3490 family — the
	// order comes from the fixed originationCodes tuple, and the not-covered verdict
	// TestHandleUC08_DemoLane_ProceedsPastNotCoveredToDeny drives comes from the
	// stubbed CRD card response, not from anything here. Only ResolvePatient/
	// OpenCoverage need to succeed.
	"MBR-D-UC08": {
		demo:    Demo{BirthDate: "1968-12-21", FamilyName: "Adeyemi"},
		inforce: true,
	},
	// The rest of the demo roster (§4.3). MBR-D-UC05/-UC08 above predate these
	// because an engine test drove those two directly; the remaining eight are
	// here so the WHOLE demo roster resolves in this fixture, which is what lets
	// the canary-twin generator below cover the demo arm — a canary twin is
	// GENERATED from its original's row, so an original the fixture does not
	// carry cannot have a twin, and a demo member without a twin fails every
	// canary request for its scenario closed (scenarioMember's no-twin 400).
	//
	// Demographics are the deployed roster's own (internal/fhirseed's table, the
	// source of truth a real deployment reads from FHIR), so a member resolves to
	// the same PCI here as it does there — the gateway/connectors/scaffold
	// precedent. Clinical facts are deliberately minimal: the demo lane's
	// orderSource builds from the originationCodes tuple rather than SoR clinical
	// facts, and the pend/deny verdicts come from the payer content occupant, so
	// only ResolvePatient / OpenCoverage / CoverageInforce have to answer.
	// MBR-D-UC01-NC is the one row that carries a verdict of its own: UC-01's
	// not-covered branch reads exactly this coverage status.
	"MBR-D-UC01": {
		demo:    Demo{BirthDate: "1972-03-14", FamilyName: "Larsen"},
		inforce: true,
	},
	"MBR-D-UC01-NC": {
		demo:    Demo{BirthDate: "1972-03-14", FamilyName: "Larsen-Terminated"},
		inforce: false,
		reason:  "coverage-terminated",
	},
	"MBR-D-UC02": {
		demo:    Demo{BirthDate: "1965-06-11", FamilyName: "Fontaine"},
		inforce: true,
	},
	"MBR-D-UC03": {
		demo:    Demo{BirthDate: "1979-09-02", FamilyName: "Whitfield"},
		inforce: true,
	},
	"MBR-D-UC04": {
		demo:    Demo{BirthDate: "1958-12-19", FamilyName: "Okereke"},
		inforce: true,
	},
	"MBR-D-UC05-NC": {
		demo:    Demo{BirthDate: "1963-02-27", FamilyName: "Marchetti-Noconsent"},
		inforce: true,
	},
	"MBR-D-UC06": {
		demo:    Demo{BirthDate: "1970-05-08", FamilyName: "Adeyemi"},
		inforce: true,
	},
	"MBR-D-UC07": {
		demo:    Demo{BirthDate: "1985-10-23", FamilyName: "Kowalczyk"},
		inforce: true,
	},
	// MBR-UC07HCPCS: the in-process mirror of the two-RI L8000 DV-approve.
	// HCPCS order (L8000); a DIRECT CRD→DTR→PAS approve (NOT patient-authorship — that
	// distinctive stays UC-07/MBR-UC07). DEF-4 stub (AI-9): it answers the REUSED lumbar
	// questionnaire (clinical fixture smell — a HCPCS order on an MSK questionnaire — accepted;
	// the fidelity that matters is the HCPCS order code → HCPCS EOB → render). weeks=6, no
	// pend trigger → the payer approves on the first submit.
	"MBR-UC07HCPCS": {
		demo:    Demo{BirthDate: "1977-01-30", FamilyName: "Nakamura"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
		},
		hasClinical: true,
	},
	// MBR-UC07 (Nadia Haddad) — UC-07 patient-entry: pends on a missing PATIENT-reported
	// (patient-attested) functional status; approves once the patient authors + attests
	// it via the Trust-operated PHG (FR-18/27). NeuroDeficitRef is included so the
	// auto-filled QR carries a non-empty information-origin sourceReference (a missing
	// Ref produces an empty reference that fails live FHIR validation — real UC-05 bug).
	"MBR-UC07": {
		demo:    Demo{BirthDate: "1990-08-25", FamilyName: "Haddad"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			PatientReported:          true,
		},
		hasClinical: true,
	},
	// MBR-UC05-NOCONSENT — Linda's no-consent twin: same shape, distinct demographics
	// (distinct PCI) so consentsvc has NO standing permit → the federated query is
	// denied and the PA stays pended (the no-consent branch, DoD).
	"MBR-UC05-NOCONSENT": {
		demo:    Demo{BirthDate: "1968-03-12", FamilyName: "Johansson-Noconsent"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			NeuroDeficit:             false,
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			PriorSurgery:             true,
			PriorSurgeryRef:          "Procedure/proc-microdiscectomy",
		},
		hasClinical: true,
	},
	// MBR-BRIDGE-DEMO / MBR-BRIDGE-REFUSE — the bridging-demo personas. Both
	// drive UC-03 (CRD order-select → DTR fetch/auto-fill → PAS submit, CPT 72148/M51.16,
	// handleUC03's g.scenarioMember seam) and carry the SAME approve-worthy clinical shape as
	// MBR-COVERED (weeks=6, no neuro deficit, prior imaging, no prior surgery — mirrors
	// shnsdk.DemoLumbarContext) so neither persona's OWN clinical facts ever deny the PA; the
	// "refuse" run's failure comes from the demo's gated egress-native-lines mechanism (D-6),
	// never from a clinical verdict. What DOES differ is the payer identity their OpenCoverage
	// answers with (see censusPayerOverrides below): MBR-BRIDGE-DEMO routes to the bridge-demo-
	// payer holder, MBR-BRIDGE-REFUSE to bridge-demo-refuse, instead of the CMS
	// conformance payer. Distinct demographics from MBR-COVERED so their PCI (and every
	// audit/observer surface keyed on it) is attributable. Demo values are copied VERBATIM into
	// internal/fhirseed/fhirseed.go's demographics table (cross-partition PCI — the MBR-OX
	// precedent's rule, applied here too).
	"MBR-BRIDGE-DEMO": {
		demo:    Demo{BirthDate: "1983-03-11", FamilyName: "Solberg-BridgeDemo"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
		},
		hasClinical: true,
	},
	"MBR-BRIDGE-REFUSE": {
		demo:    Demo{BirthDate: "1986-09-27", FamilyName: "Amara-BridgeRefuse"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
		},
		hasClinical: true,
	},
	// MBR-PAYERB / MBR-PAYERUNKNOWN are HERMETIC-TEST-ONLY personas (FR-G40): they exist
	// solely so gateway/engine/payerrouting_test.go (and test/adversarial) can prove/disprove
	// coverage-derived payer routing without standing up a real multi-payer FHIR SoR. They carry
	// no clinical context (routing tests never reach CRD/DTR content). Deliberately NOT in
	// PersonaRefs (personas.go): never live-seeded, never console-exposed, never driven by any UC
	// scenario handler's sceneMember (unlike MBR-BRIDGE-DEMO/MBR-BRIDGE-REFUSE above, which the
	// Kit's runner now drives) — see censusPayerOverrides below, which is what actually
	// gives them a non-default payer identity.
	"MBR-PAYERB": {
		demo:    Demo{BirthDate: "1972-01-01", FamilyName: "Routingtest-PayerB"},
		inforce: true,
	},
	"MBR-PAYERUNKNOWN": {
		demo:    Demo{BirthDate: "1972-01-02", FamilyName: "Routingtest-Unknown"},
		inforce: true,
	},
	// MBR-OX / MBR-PD-UC03 — the two provider-data ORDER-DISPATCH personas:
	// HomeOxygen (E0431) and its UC-03 HomeOxygenDispatch analog (E1390). Their
	// PROVIDER-side facts (Patient/DeviceRequest/Coverage) come from a real or bring-your-own
	// FHIR SoR (originate_homeoxygen.go's originateDispatch reads OpenOrder/OpenCoverage off
	// g.cfg.SoR, never a literal) — but the PAYER-side inbound bind for the crd-order-dispatch
	// leg (conformantCRDDispatchBind, crd_dispatch_native.go) resolves the member against
	// THIS stub when the payer gateway boots on the memstub default (no FHIR_DATA_URL), which
	// is what every internal/devstack / test/harness.go / in-process payer wiring does — the
	// in-process payer must cover the same demo personas the seeded fixtures define, or
	// payer- and provider-side census drifts apart. Before
	// this fix the stub had no entry for either member, so that bind 400'd "unknown member"
	// even though the hosted payer (internal/fhirseed's payer-tenant Coverage, seeded via
	// seedCoverage) already covers both. Demo values are copied VERBATIM from
	// internal/fhirseed/fhirseed.go's demographics table (the single seed source of truth) —
	// they MUST match exactly, or the payer-computed PCI would diverge from the
	// provider-computed one and the bind would 403 "token subject does not match" instead.
	// No clinical context: the order-dispatch leg never reads ClinicalContext (mirrors
	// MBR-PAYERB/MBR-PAYERUNKNOWN's shape above, not the CPT-lumbar personas').
	"MBR-OX": {
		demo:    Demo{BirthDate: "1958-07-14", FamilyName: "Okafor-Oxygen"},
		inforce: true,
	},
	"MBR-PD-UC03": {
		demo:    Demo{BirthDate: "1956-04-09", FamilyName: "Diallo-OxygenConcentrator"},
		inforce: true,
	},
	// MBR-PD-UC04 / MBR-PD-UC05 / MBR-PD-UC05-NC / MBR-PD-UC06 / MBR-PD-UC07 — the provider-data
	// ORDER-SELECT attestation personas (the HomeHealthAssessment G0151 lane: UC-04 single-shot,
	// UC-05 federated-query consent + noconsent twins, UC-06 clinician attestation, UC-07 patient
	// attestation). Same rule as MBR-OX/MBR-PD-UC03 above: their
	// provider-side facts (Patient/ServiceRequest/Coverage/ClinicalImpression/Goal) come from the
	// published provider-data persona bundle loaded into a real FHIR SoR, but the PAYER-side
	// inbound binds (the conformant PAS bind and the adaptive $next-question bind) resolve the
	// member against THIS stub when the payer gateway boots on the memstub default — the
	// hermetic provider-data harness. Demo values are copied VERBATIM from
	// internal/fhirseed/fhirseed.go's demographics table (the single seed source of truth) —
	// they MUST match exactly, or the payer-computed PCI diverges from the provider-computed
	// one and the bind 403s "token subject does not match". No clinical context: the HHA lane
	// attests from the seeded order, never from ClinicalContext.
	"MBR-PD-UC04": {
		demo:    Demo{BirthDate: "1949-03-22", FamilyName: "Castellano-HomeHealth"},
		inforce: true,
	},
	"MBR-PD-UC05": {
		demo:    Demo{BirthDate: "1955-02-09", FamilyName: "Velazquez-Federated"},
		inforce: true,
	},
	"MBR-PD-UC05-NC": {
		demo:    Demo{BirthDate: "1955-02-09", FamilyName: "Velazquez-NoConsent"},
		inforce: true,
	},
	"MBR-PD-UC06": {
		demo:    Demo{BirthDate: "1957-11-03", FamilyName: "Okonkwo-HomeHealth"},
		inforce: true,
	},
	"MBR-PD-UC07": {
		demo:    Demo{BirthDate: "1960-08-14", FamilyName: "Nwosu-PatientAttest"},
		inforce: true,
	},
}

// Canary twins are generated, never hand-copied: each clones its original's
// coverage + clinical facts (a fixture change flows to the twin) with a
// "-Canary" family so the derived PCI is distinct and canary traffic is
// attributable on every surface that shows a name.
func init() {
	for orig, twin := range CanaryTwins {
		p, ok := censusPersonas[orig]
		if !ok {
			panic("engine: canary twin of unknown persona: " + orig)
		}
		p.demo.FamilyName += "-Canary"
		censusPersonas[twin] = p
	}
}

// censusPayerOverrides names members whose OpenCoverage payor is DELIBERATELY DISTINCT from the
// default CMSPayerIdentity (FR-G40: the hermetic two/three-payer routing proof). Every
// member ABSENT from this map — i.e. every pre-existing persona — keeps resolving to
// CMSPayerIdentity (00001), so all pre-existing hermetic origination stays green/byte-identical.
// MBR-PAYERUNKNOWN's value (00099) is deliberately never registered in any PayerRouter used by
// tests/harness — it exists to drive the "no registered payer" fail-closed path (AI-G11 / OWD-G10).
var censusPayerOverrides = map[string]shnsdk.PayerIdentifier{
	"MBR-PAYERB":        {System: shnsdk.CMSPayerIdentity.System, Value: "00078"},
	"MBR-PAYERUNKNOWN":  {System: shnsdk.CMSPayerIdentity.System, Value: "00099"},
	"MBR-BRIDGE-DEMO":   BridgeDemoPayerID,
	"MBR-BRIDGE-REFUSE": BridgeRefusePayerID,
}

// ResolvePatient returns the member's PCI and demographics. Unknown members yield
// found=false.
func (d *censusSoR) ResolvePatient(memberID string) (pci string, demo Demo, found bool) {
	p, ok := censusPersonas[memberID]
	if !ok {
		return "", Demo{}, false
	}
	pci = shnsdk.ResolvePCI(memberID, p.demo.BirthDate, p.demo.FamilyName)
	return pci, p.demo, true
}

// PatientFHIRRef — the in-memory stub uses logical refs (no FHIR store / scoped ids).
func (d *censusSoR) PatientFHIRRef(memberID string) (string, bool) {
	if _, ok := censusPersonas[memberID]; !ok {
		return "", false
	}
	return "Patient/" + memberID, true
}

// CoverageInforce answers whether the member's coverage is in force. Unknown
// members are treated as not in force.
func (d *censusSoR) CoverageInforce(memberID string) (inforce bool, reason string) {
	p, ok := censusPersonas[memberID]
	if !ok {
		return false, ""
	}
	return p.inforce, p.reason
}

// ClinicalContext returns the provider-LOCAL clinical context for a member.
// Members with no clinical data (not-covered, unknown) yield found=false.
func (d *censusSoR) ClinicalContext(memberID string) (shnsdk.ClinicalContext, bool) {
	p, ok := censusPersonas[memberID]
	if !ok || !p.hasClinical {
		return shnsdk.ClinicalContext{}, false
	}
	return p.clinical, true
}

// SupplementalReport returns the provider-LOCAL supplemental DiagnosticReport
// for MBR-UC04 (UC-04, FR-32). All other members yield found=false.
// NOTE: MBR-UC05 deliberately yields false — the operative report is EXTERNAL
// (held by metro-spine), which is what forces the consent-gated federated query.
func (d *censusSoR) SupplementalReport(memberID string) ([]byte, bool) {
	if memberID != "MBR-UC04" {
		return nil, false
	}
	raw, err := shnsdk.BuildDiagnosticReport("dr-uc04-operative", "Patient/MBR-UC04", "72148", "MRI lumbar spine w/o contrast")
	if err != nil {
		return nil, false
	}
	return raw, true
}

// OpenOrder: the in-memory stub does not hold open orders; the provider-data lane
// requires a real FHIR SoR (FHIR_DATA_URL). Returns found=false.
func (d *censusSoR) OpenOrder(_ string) ([]byte, bool) { return nil, false }

// OpenCoverage returns the stub's modeled Coverage for the member (FR-G40): a US Core
// Coverage whose payor names the CMS payer Organization (shnsdk.CMSPayerIdentity) by DEFAULT,
// or the member's censusPayerOverrides entry when present (the hermetic multi-payer
// routing proof — MBR-PAYERB/MBR-PAYERUNKNOWN are the only members with an override; every
// other member is untouched, so all pre-existing hermetic origination stays byte-identical).
// Unknown members yield found=false.
func (d *censusSoR) OpenCoverage(memberID string) ([]byte, bool) {
	if _, _, found := d.ResolvePatient(memberID); !found {
		return nil, false
	}
	payer := shnsdk.CMSPayerIdentity
	if p, ok := censusPayerOverrides[memberID]; ok {
		payer = p
	}
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/"+memberID, memberID, payer)
	if err != nil {
		return nil, false
	}
	return cov, true
}

// ResolveByReference: the in-memory stub does not resolve FHIR references; the
// provider-data lane requires a real FHIR SoR (FHIR_DATA_URL). Returns found=false.
func (d *censusSoR) ResolveByReference(_ string) ([]byte, bool) { return nil, false }

// FacilityRecords returns metro-spine's held records for MBR-UC05 (UC-05): the
// operative DiagnosticReport and its DocumentReference. All other members yield
// found=false. The provider does NOT hold these — they are retrieved by the
// consent-gated federated query (the non-aggregation showcase).
func (d *censusSoR) FacilityRecords(memberID string) (map[string][]byte, bool) {
	if memberID != "MBR-UC05" {
		return nil, false
	}
	patientRef := "Patient/MBR-UC05"
	dr, err := shnsdk.BuildDiagnosticReport("dr-uc05-operative", patientRef, "72148", "Operative report — lumbar microdiscectomy")
	if err != nil {
		return nil, false
	}
	docref, err := shnsdk.BuildDocumentReference("docref-uc05-operative", patientRef, "DiagnosticReport/dr-uc05-operative")
	if err != nil {
		return nil, false
	}
	return map[string][]byte{"DiagnosticReport": dr, "DocumentReference": docref}, true
}

// TestCensusFixture_Personas proves the two UC-01 personas in isolation.
// MBR-COVERED must resolve to a stable pci: value with CoverageInforce=true.
// MBR-NOTCOVERED must resolve to a stable pci: value with inforce=false and
// reason="coverage-terminated". UNKNOWN must yield found=false.
func TestCensusFixture_Personas(t *testing.T) {
	d := newCensusSoR()

	t.Run("MBR-COVERED resolved and inforce", func(t *testing.T) {
		pci, demo, found := d.ResolvePatient("MBR-COVERED")
		if !found {
			t.Fatal("expected found=true for MBR-COVERED")
		}
		if !strings.HasPrefix(pci, "pci:") {
			t.Errorf("pci must start with 'pci:', got %q", pci)
		}
		if pci == "pci:" {
			t.Errorf("pci must have a non-empty hash after 'pci:', got %q", pci)
		}
		if demo.BirthDate != "1975-04-02" {
			t.Errorf("BirthDate = %q, want 1975-04-02", demo.BirthDate)
		}
		if demo.FamilyName != "Johansson" {
			t.Errorf("FamilyName = %q, want Johansson", demo.FamilyName)
		}

		inforce, reason := d.CoverageInforce("MBR-COVERED")
		if !inforce {
			t.Error("CoverageInforce: want true for MBR-COVERED")
		}
		if reason != "" {
			t.Errorf("CoverageInforce reason = %q, want empty string", reason)
		}
	})

	t.Run("MBR-COVERED pci is stable across calls", func(t *testing.T) {
		pci1, _, _ := d.ResolvePatient("MBR-COVERED")
		pci2, _, _ := d.ResolvePatient("MBR-COVERED")
		if pci1 != pci2 {
			t.Errorf("pci must be deterministic: first=%q second=%q", pci1, pci2)
		}
	})

	t.Run("MBR-NOTCOVERED resolved and not inforce", func(t *testing.T) {
		pci, demo, found := d.ResolvePatient("MBR-NOTCOVERED")
		if !found {
			t.Fatal("expected found=true for MBR-NOTCOVERED")
		}
		if !strings.HasPrefix(pci, "pci:") {
			t.Errorf("pci must start with 'pci:', got %q", pci)
		}
		if demo.BirthDate != "1980-09-15" {
			t.Errorf("BirthDate = %q, want 1980-09-15", demo.BirthDate)
		}
		if demo.FamilyName != "Reyes" {
			t.Errorf("FamilyName = %q, want Reyes", demo.FamilyName)
		}

		inforce, reason := d.CoverageInforce("MBR-NOTCOVERED")
		if inforce {
			t.Error("CoverageInforce: want false for MBR-NOTCOVERED")
		}
		if reason != "coverage-terminated" {
			t.Errorf("CoverageInforce reason = %q, want 'coverage-terminated'", reason)
		}
	})

	t.Run("MBR-COVERED and MBR-NOTCOVERED have different PCIs", func(t *testing.T) {
		pci1, _, _ := d.ResolvePatient("MBR-COVERED")
		pci2, _, _ := d.ResolvePatient("MBR-NOTCOVERED")
		if pci1 == pci2 {
			t.Errorf("different members must have different PCIs, both got %q", pci1)
		}
	})

	t.Run("UNKNOWN member not found", func(t *testing.T) {
		_, _, found := d.ResolvePatient("UNKNOWN")
		if found {
			t.Error("expected found=false for UNKNOWN member")
		}

		inforce, reason := d.CoverageInforce("UNKNOWN")
		if inforce {
			t.Error("CoverageInforce: want false for unknown member")
		}
		if reason != "" {
			t.Errorf("CoverageInforce reason for unknown = %q, want empty", reason)
		}
	})
}

// TestCensusFixture_ClinicalContext proves the covered persona carries the
// full provider-LOCAL clinical context, and non-covered/unknown members do not.
func TestCensusFixture_ClinicalContext(t *testing.T) {
	d := newCensusSoR()

	t.Run("covered member has clinical context", func(t *testing.T) {
		cc, found := d.ClinicalContext("MBR-COVERED")
		if !found {
			t.Fatal("expected found=true for MBR-COVERED")
		}
		if cc.ConditionCode != "M51.16" {
			t.Errorf("ConditionCode = %q, want M51.16", cc.ConditionCode)
		}
		if cc.ConditionRef != "Condition/cond-m5116" {
			t.Errorf("ConditionRef = %q, want Condition/cond-m5116", cc.ConditionRef)
		}
		if cc.ConservativeTherapyWeeks != 6 {
			t.Errorf("ConservativeTherapyWeeks = %d, want 6", cc.ConservativeTherapyWeeks)
		}
		if cc.ConservativeTherapyRef != "Observation/obs-pt-weeks" {
			t.Errorf("ConservativeTherapyRef = %q, want Observation/obs-pt-weeks", cc.ConservativeTherapyRef)
		}
		if cc.ConservativeDate != "2026-05-20" {
			t.Errorf("ConservativeDate = %q, want 2026-05-20", cc.ConservativeDate)
		}
		if cc.NeuroDeficit {
			t.Error("NeuroDeficit = true, want false")
		}
		if cc.NeuroDeficitRef != "Observation/obs-neuro" {
			t.Errorf("NeuroDeficitRef = %q, want Observation/obs-neuro", cc.NeuroDeficitRef)
		}
		if !cc.PriorImaging {
			t.Error("PriorImaging = false, want true")
		}
		if cc.PriorImagingRef != "DiagnosticReport/dr-xray" {
			t.Errorf("PriorImagingRef = %q, want DiagnosticReport/dr-xray", cc.PriorImagingRef)
		}
	})

	t.Run("not-covered member has no clinical context", func(t *testing.T) {
		if _, found := d.ClinicalContext("MBR-NOTCOVERED"); found {
			t.Error("expected found=false for MBR-NOTCOVERED")
		}
	})

	t.Run("unknown member has no clinical context", func(t *testing.T) {
		if _, found := d.ClinicalContext("UNKNOWN"); found {
			t.Error("expected found=false for UNKNOWN")
		}
	})
}

// TestPersonas_UC04_UC06 verifies the UC-04/UC-06 personas and the
// SupplementalReport accessor (UC-04 FR-32, FR-35/39).
func TestPersonas_UC04_UC06(t *testing.T) {
	d := newCensusSoR()

	if _, _, found := d.ResolvePatient("MBR-UC04"); !found {
		t.Fatal("MBR-UC04 must resolve")
	}
	cc, ok := d.ClinicalContext("MBR-UC04")
	if !ok || !cc.PriorSurgery {
		t.Fatalf("MBR-UC04 ClinicalContext PriorSurgery: ok=%v cc=%+v", ok, cc)
	}
	dr, ok := d.SupplementalReport("MBR-UC04")
	if !ok || len(dr) == 0 {
		t.Fatal("MBR-UC04 must have a supplemental DiagnosticReport")
	}

	cc6, ok := d.ClinicalContext("MBR-UC06")
	if !ok || !cc6.HighDisability {
		t.Fatalf("MBR-UC06 ClinicalContext HighDisability: ok=%v cc=%+v", ok, cc6)
	}
	if _, ok := d.SupplementalReport("MBR-UC06"); ok {
		t.Fatal("MBR-UC06 has no separate DiagnosticReport (manual entry path)")
	}
}

// TestPendedClaimLedger verifies the state machine: record(pended) → begin(claim) →
// TestCensusFixture_BridgeDemoPersonas proves the bridging-demo personas: both resolve, are in force,
// carry the SAME approve-worthy clinical shape as MBR-COVERED, and — the load-bearing part —
// each member's stub Coverage read (OpenCoverage) parses to its OWN exported demo payer
// identity rather than the default CMSPayerIdentity every un-overridden persona shares.
func TestCensusFixture_BridgeDemoPersonas(t *testing.T) {
	d := newCensusSoR()

	for _, tc := range []struct {
		member    string
		birthDate string
		family    string
		wantPayer shnsdk.PayerIdentifier
	}{
		{"MBR-BRIDGE-DEMO", "1983-03-11", "Solberg-BridgeDemo", BridgeDemoPayerID},
		{"MBR-BRIDGE-REFUSE", "1986-09-27", "Amara-BridgeRefuse", BridgeRefusePayerID},
	} {
		t.Run(tc.member, func(t *testing.T) {
			pci, demo, found := d.ResolvePatient(tc.member)
			if !found {
				t.Fatalf("expected found=true for %s", tc.member)
			}
			if !strings.HasPrefix(pci, "pci:") || pci == "pci:" {
				t.Errorf("pci must be a non-empty 'pci:'-prefixed hash, got %q", pci)
			}
			if demo.BirthDate != tc.birthDate || demo.FamilyName != tc.family {
				t.Errorf("demo = %+v, want {%q %q}", demo, tc.birthDate, tc.family)
			}

			inforce, reason := d.CoverageInforce(tc.member)
			if !inforce || reason != "" {
				t.Errorf("CoverageInforce = (%v,%q), want (true,\"\")", inforce, reason)
			}

			cc, ccFound := d.ClinicalContext(tc.member)
			if !ccFound {
				t.Fatal("expected ClinicalContext found=true")
			}
			if cc.ConditionCode != "M51.16" || cc.ConservativeTherapyWeeks != 6 || cc.NeuroDeficit || !cc.PriorImaging {
				t.Errorf("ClinicalContext = %+v, want the MBR-COVERED-shaped approve-worthy facts", cc)
			}

			covJSON, covFound := d.OpenCoverage(tc.member)
			if !covFound {
				t.Fatal("expected OpenCoverage found=true")
			}
			gotPayer, ok := shnsdk.ParsePayerIdentifier(covJSON, nil)
			if !ok {
				t.Fatalf("ParsePayerIdentifier: ok=false for %s", covJSON)
			}
			if gotPayer != tc.wantPayer {
				t.Errorf("OpenCoverage(%s) payer = %+v, want %+v", tc.member, gotPayer, tc.wantPayer)
			}
		})
	}

	// The two demo personas must never collide on payer identity or PCI (attribution).
	demoCov, _ := d.OpenCoverage("MBR-BRIDGE-DEMO")
	refuseCov, _ := d.OpenCoverage("MBR-BRIDGE-REFUSE")
	demoPayer, _ := shnsdk.ParsePayerIdentifier(demoCov, nil)
	refusePayer, _ := shnsdk.ParsePayerIdentifier(refuseCov, nil)
	if demoPayer == refusePayer {
		t.Fatalf("MBR-BRIDGE-DEMO and MBR-BRIDGE-REFUSE must resolve to DIFFERENT payer identities, both got %+v", demoPayer)
	}
	demoPCI, _, _ := d.ResolvePatient("MBR-BRIDGE-DEMO")
	refusePCI, _, _ := d.ResolvePatient("MBR-BRIDGE-REFUSE")
	if demoPCI == refusePCI {
		t.Fatalf("MBR-BRIDGE-DEMO and MBR-BRIDGE-REFUSE must have different PCIs, both got %q", demoPCI)
	}
}
