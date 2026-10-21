package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
)

func TestPACatalog_Ops(t *testing.T) {
	want := map[string]string{
		"coverage-eligibility":    "eligibility-inquiry",
		"crd-order-select":        "crd-order-select",
		"crd-order-dispatch":      "crd-order-dispatch",
		"dtr-questionnaire-fetch": "dtr-questionnaire-fetch",
		"patient-dtr":             "patient-dtr-request",
		"pas-claim":               "pas-submit",
		"pas-claim-update":        "pas-update-submit",
		"pas-claim-inquire":       "pas-inquire",
	}
	if len(paCatalog) != len(want) {
		t.Fatalf("paCatalog has %d entries, want %d", len(paCatalog), len(want))
	}
	for legType, op := range want {
		spec, ok := paCatalog[legType]
		if !ok {
			t.Errorf("paCatalog missing %q", legType)
			continue
		}
		if spec.Op != op {
			t.Errorf("paCatalog[%q].Op = %q, want %q", legType, spec.Op, op)
		}
	}
}

// TestPACatalog_AllReqFramesProviderTPO documents the PA invariant that every
// originated leg is framed provider-tpo (handleInbound pins this frame). This is what
// lets handleInbound read spec.ReqFrame from the catalog in place of its former inline
// "provider-tpo" literal (the inbound FulfillLeg dispatch now does exactly that).
func TestPACatalog_AllReqFramesProviderTPO(t *testing.T) {
	for legType, spec := range paCatalog {
		if spec.ReqFrame != "provider-tpo" {
			t.Errorf("paCatalog[%q].ReqFrame = %q, want provider-tpo", legType, spec.ReqFrame)
		}
	}
}

func TestOriginateLeg_UnknownLegTypeFailsClosed(t *testing.T) {
	g := &Gateway{} // never reaches roundTrip: the catalog miss returns first
	// workstreamPA passes the selection-seam guard, so this exercises the legType-miss
	// path specifically (an unknown legType WITHIN the served workstream).
	_, err := g.OriginateLeg(context.Background(), nil, "payer", "no-such-leg", "pci", "corr", "", Content{WorkstreamType: workstreamPA, Payload: testRequest([]byte("{}"))})
	if err == nil {
		t.Fatal("OriginateLeg with unknown legType: want error, got nil")
	}
}

// TestOriginateLeg_WrongWorkstreamFailsClosed proves the WorkstreamType selection seam
// (catalogFor) fires BEFORE any leg is looked up: a VALID legType ("crd-order-select")
// carried by a WorkstreamType this build does not compile in, or by none, fails closed
// with the seam's own error, without ever reaching a catalog or roundTrip. The error
// returns cleanly on a zero-value Gateway — if the seam did NOT fire first, the valid
// legType would reach roundTrip and panic on the nil registry, failing the test
// differently.
func TestOriginateLeg_WrongWorkstreamFailsClosed(t *testing.T) {
	g := &Gateway{}
	for _, ws := range []string{"x12-278", ""} {
		_, err := g.OriginateLeg(context.Background(), nil, "payer", "crd-order-select", "pci", "corr", "", Content{WorkstreamType: ws, Payload: testRequest([]byte("{}"))})
		want := fmt.Sprintf("OriginateLeg: content workstream %q not served by this gateway", ws)
		if err == nil || err.Error() != want {
			t.Fatalf("OriginateLeg with WorkstreamType %q: err = %v, want %q", ws, err, want)
		}
	}
}

// TestExchangeIR_Composes is a COMPOSE/shape check: it confirms the IR vocabulary
// nests (Exchange holds LegRecords projected from in-flight Legs) and compiles. The
// len()==2 + field readbacks are tautological by design — this slice does not yet
// thread a live Exchange, so there is no behavior to assert. The LOAD-BEARING
// assertions are the physics cross-checks below: each leg's Physics, pulled from
// paCatalog, must carry the right Effect (CRD readonly, PAS mutating). Do not read the
// rest as behavioral coverage.
func TestExchangeIR_Composes(t *testing.T) {
	legs := []Leg{
		{Type: "crd-order-select", Physics: paCatalog["crd-order-select"].Physics,
			Content: Content{WorkstreamType: workstreamPA, Payload: testRequest([]byte("{}"))}, Subjects: []string{"pci-1"}},
		{Type: "pas-claim", Physics: paCatalog["pas-claim"].Physics,
			Content: Content{WorkstreamType: workstreamPA, Payload: testRequest([]byte("{}"))}, Subjects: []string{"pci-1"}},
	}
	ex := Exchange{ID: "exch-1", Workstream: workstreamPA}
	for _, l := range legs {
		ex.Legs = append(ex.Legs, l.Project("corr-"+l.Type, "ok"))
	}
	if len(ex.Legs) != 2 {
		t.Fatalf("want 2 legs, got %d", len(ex.Legs))
	}
	if legs[0].Physics.Effect != EffectReadOnly {
		t.Errorf("crd-order-select Effect = %q, want readonly", legs[0].Physics.Effect)
	}
	if legs[1].Physics.Effect != EffectMutating {
		t.Errorf("pas-claim Effect = %q, want mutating", legs[1].Physics.Effect)
	}
	if ex.Legs[0].Physics.Effect != EffectReadOnly || ex.Legs[1].Physics.Effect != EffectMutating {
		t.Errorf("projected LegRecord physics not carried: %+v", ex.Legs)
	}
}

// TestPACatalog_Pinned pins every field of every PA catalog row: the frames,
// operations and scope the catalog takes from the leg taxonomy, and the physics
// and contract family the adapter adds. Deriving the catalog changes none of
// them.
func TestPACatalog_Pinned(t *testing.T) {
	readOnly := LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate}
	mutating := LegPhysics{Kind: KindRequestResponse, Effect: EffectMutating, Timing: TimingSync, Locality: LocalitySubstrate}
	want := map[string]legSpec{
		"coverage-eligibility":    {ReqFrame: "provider-tpo", RespFrame: "payer-coverage", Op: "eligibility-inquiry", RespOp: "eligibility-response", Scope: "eligibility-only", Physics: readOnly, Seam: "payer-native"},
		"dtr-questionnaire-fetch": {ReqFrame: "provider-tpo", RespFrame: "payer-coverage", Op: "dtr-questionnaire-fetch", RespOp: "dtr-questionnaire", Scope: "questionnaire-only", Physics: readOnly, Contract: "pa.dtr", OperationHeader: true, Seam: "payer-native"},
		"patient-dtr":             {ReqFrame: "provider-tpo", RespFrame: "patient-authorship", Op: "patient-dtr-request", RespOp: "patient-dtr-response", Scope: "patient-authorship-only", Physics: readOnly, Seam: "phg-inbound"},
		"crd-order-select":        {ReqFrame: "provider-tpo", RespFrame: "payer-coverage", Op: "crd-order-select", RespOp: "crd-cards", Scope: "crd-context", Physics: readOnly, Contract: "pa.crd", Seam: "payer-native"},
		"crd-order-dispatch":      {ReqFrame: "provider-tpo", RespFrame: "payer-coverage", Op: "crd-order-dispatch", RespOp: "crd-dispatch-cards", Scope: "crd-context", Physics: readOnly, Contract: "pa.crd", Seam: "payer-native"},
		"pas-claim":               {ReqFrame: "provider-tpo", RespFrame: "payer-coverage", Op: "pas-submit", RespOp: "pas-response", Scope: "pas-bundle", Physics: mutating, Contract: "pa.pas", Seam: "payer-native"},
		"pas-claim-update":        {ReqFrame: "provider-tpo", RespFrame: "payer-coverage", Op: "pas-update-submit", RespOp: "pas-update-response", Scope: "pas-update-bundle", Physics: mutating, Contract: "pa.pas", Seam: "payer-native"},
		"pas-claim-inquire":       {ReqFrame: "provider-tpo", RespFrame: "payer-coverage", Op: "pas-inquire", RespOp: "pas-inquire-response", Scope: "pas-inquire-bundle", Physics: readOnly, Contract: "pa.pas", Seam: "payer-native"},
	}
	if !maps.Equal(paCatalog, want) {
		for tx := range want {
			if paCatalog[tx] != want[tx] {
				t.Errorf("paCatalog[%q] = %+v, want %+v", tx, paCatalog[tx], want[tx])
			}
		}
		t.Fatalf("paCatalog has %d rows, want %d", len(paCatalog), len(want))
	}
}

// TestFQSCatalog_Pinned pins the FQS adapter's one row as PA's catalog held
// it before the federated query became its own workstream's leg: the same
// frames, operations, scope, physics, contract family and seam.
func TestFQSCatalog_Pinned(t *testing.T) {
	readOnly := LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate}
	want := map[string]legSpec{
		"federated-query": {ReqFrame: "provider-tpo", RespFrame: "facility-disclosure", Op: "federated-query-submit", RespOp: "federated-query-response", Scope: "named-docs-only", Physics: readOnly, Seam: "facility-inbound"},
	}
	if got := fqsAdapter.catalog(); !maps.Equal(got, want) {
		t.Fatalf("the FQS catalog = %+v, want %+v", got, want)
	}
	if _, ok := paCatalog["federated-query"]; ok {
		t.Fatal("PA's catalog still declares the federated query")
	}
	if c, ok := catalogFor(workstreamFQS); !ok || !maps.Equal(c, want) {
		t.Fatalf("catalogFor(%q) = %v, %v; want the FQS catalog", workstreamFQS, c, ok)
	}
	if w, ok := workstreamFor("federated-query"); !ok || w.id() != workstreamFQS {
		t.Fatalf("the federated query's workstream is %v, want FQS", w)
	}
}

// TestPAInvolvement_Pinned: which PA legs' requests name the other patients
// they carry, and whose answers name the payer's own binding, for the
// network's involved list; a leg no workstream declares has neither, and its
// request names no member.
func TestPAInvolvement_Pinned(t *testing.T) {
	both := []string{"coverage-eligibility", "crd-order-select", "crd-order-dispatch", "dtr-questionnaire-fetch", "pas-claim", "pas-claim-update", "pas-claim-inquire"}
	for _, leg := range networkLegs() {
		request, answer := legInvolvement(leg)
		want := slices.Contains(both, leg)
		if request != want || answer != want {
			t.Errorf("%s: involvement (request %v, answer %v), want (%v, %v)", leg, request, answer, want, want)
		}
	}
	payload := []byte(`{"resourceType":"Patient","id":"m1"}`)
	for _, leg := range []string{"", "unknown", "PAS-CLAIM"} {
		if request, answer := legInvolvement(leg); request || answer {
			t.Errorf("%q: involvement (%v, %v), want neither", leg, request, answer)
		}
		if got := namedMembers(payload, leg); got != nil {
			t.Errorf("%q: members %v, want none", leg, got)
		}
	}
	if got := namedMembers(payload, "crd-order-select"); len(got) != 1 || got[0].member != "m1" || string(got[0].scope) != string(payload) {
		t.Errorf("crd-order-select: members %+v, want [m1] bound from the whole request, through the PA adapter", got)
	}
}

// TestPARequestMembers_EachLegReadsAsItsBinding: the questionnaire package
// reads a reference as its own binding does (patientMember), which takes
// "Patient/" only at the start of a path segment; the other legs read it as
// the PAS and inquiry readers do (pasMemberFromRef). A reference that only
// contains "Patient/" names a member on CRD but none on the DTR package.
func TestPARequestMembers_EachLegReadsAsItsBinding(t *testing.T) {
	payload := []byte(`{"resourceType":"Parameters","parameter":[{"name":"x","valueReference":{"reference":"urn:Patient/m9"}}]}`)
	if got := carriedMembers(payload, "dtr-questionnaire-fetch"); len(got) != 0 {
		t.Errorf("dtr-questionnaire-fetch: members %v, want none (the package's binding reads no Patient there)", got)
	}
	if got := carriedMembers(payload, "crd-order-select"); !slices.Equal(got, []string{"m9"}) {
		t.Errorf("crd-order-select: members %v, want [m9]", got)
	}
}

// TestPARequestMembers_PASNamesACarriedPatientByItsReference: on PAS submit
// and update, a carried Patient is named by its entry's fullUrl, as those legs
// bind it; the inquiry names it by its id.
func TestPARequestMembers_PASNamesACarriedPatientByItsReference(t *testing.T) {
	payload := []byte(`{"resourceType":"Bundle","entry":[{"fullUrl":"http://x/fhir/Patient/m1","resource":{"resourceType":"Patient","id":"local"}}]}`)
	for leg, want := range map[string][]string{
		"pas-claim":         {"m1"},
		"pas-claim-update":  {"m1"},
		"pas-claim-inquire": {"local"},
	} {
		if got := carriedMembers(payload, leg); !slices.Equal(got, want) {
			t.Errorf("%s: members %v, want %v", leg, got, want)
		}
	}
}
