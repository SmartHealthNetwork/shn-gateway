//go:build !shn_coreonly

package engine

import (
	"net/http"
	"strings"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
)

// qrContextRefusalRow is a provider QR naming a context the provider-data
// build cannot carry: the context it names, the records the system holds,
// a read that fails, and the answer and record the gateway owes.
type qrContextRefusalRow struct {
	name     string
	optional string
	records  map[string][]byte
	failRef  string
	failErr  error
	status   int
	body     string
	// upstream marks the system's failure, recorded as the upstream error it
	// is; every other row is the fidelity of SHN's own build.
	upstream bool
	// unread is a reference the refusal is decided without reading.
	unread string
}

func qrContextRefusalRows() []qrContextRefusalRow {
	sr := func(extra string) []byte {
		return []byte(`{"resourceType":"ServiceRequest","id":"optional-order","status":"active","intent":"order","subject":{"reference":"Patient/MBR-OX"}` + extra + `}`)
	}
	opt := optionalContextReference("ServiceRequest", "optional-order")
	return []qrContextRefusalRow{
		{name: "not held", optional: opt, status: http.StatusBadGateway,
			body: "a record the request references cannot be filled: ServiceRequest/optional-order is not held by the system of record"},
		{name: "another patient's", optional: opt, records: map[string][]byte{"ServiceRequest/optional-order": []byte(`{"resourceType":"ServiceRequest","id":"optional-order","status":"active","intent":"order","subject":{"reference":"Patient/someone-else"}}`)},
			status: http.StatusBadGateway, body: "ServiceRequest/optional-order's subject is not exactly the request's patient"},
		{name: "names no patient where its type names it", optional: optionalContextReference("NutritionOrder", "optional-order"),
			records: map[string][]byte{"NutritionOrder/optional-order": []byte(`{"resourceType":"NutritionOrder","id":"optional-order","status":"active","intent":"order","subject":{"reference":"Patient/MBR-OX"}}`)},
			status:  http.StatusBadGateway, body: "NutritionOrder/optional-order names no patient.reference"},
		{name: "bytes it cannot carry as read", optional: opt, records: map[string][]byte{"ServiceRequest/optional-order": sr(`,"status":"active"`)},
			status: http.StatusBadGateway, body: "ServiceRequest/optional-order names a member twice, or in another case"},
		{name: "references a record no context carries", optional: opt, records: map[string][]byte{"ServiceRequest/optional-order": sr(`,"performer":[{"reference":"Practitioner/opt-perf"}]`)},
			status: http.StatusBadGateway, body: "ServiceRequest/optional-order references Practitioner/opt-perf, which a prior-authorization request this gateway builds cannot carry"},
		{name: "references another Coverage", optional: opt, records: map[string][]byte{"ServiceRequest/optional-order": sr(`,"insurance":[{"reference":"Coverage/other-cov"}]`)},
			status: http.StatusBadGateway, body: "ServiceRequest/optional-order references Coverage/other-cov, which a prior-authorization request this gateway builds cannot carry"},
		{name: "its requester is not held", optional: opt, records: map[string][]byte{"ServiceRequest/optional-order": sr(`,"requester":{"reference":"Practitioner/opt-req"}`)},
			status: http.StatusBadGateway, body: "ServiceRequest/optional-order: the order's requester Practitioner/opt-req is not in your system of record"},
		{name: "an Appointment", optional: optionalContextReference("Appointment", "appt-1"), status: http.StatusBadGateway, unread: "Appointment/appt-1",
			body: "Appointment/appt-1 is an Appointment, which a prior-authorization request this gateway builds cannot carry yet"},
		{name: "a type no request carries", optional: optionalContextReference("CarePlan", "cp-1"), status: http.StatusBadGateway, unread: "CarePlan/cp-1",
			body: "references CarePlan/cp-1, which a prior-authorization request this gateway builds cannot carry"},
		{name: "an absolute context", optional: `{"reference":"https://ehr.example/fhir/ServiceRequest/optional-order"}`, status: http.StatusBadGateway,
			unread: "https://ehr.example/fhir/ServiceRequest/optional-order",
			body:   "references https://ehr.example/fhir/ServiceRequest/optional-order, which a prior-authorization request this gateway builds cannot carry"},
		{name: "a urn context", optional: `{"reference":"urn:uuid:6f0c9a52-1a1b-4c43-9d55-0f4a1d2b9c11"}`, status: http.StatusBadGateway,
			body: "references urn:uuid:6f0c9a52-1a1b-4c43-9d55-0f4a1d2b9c11, which a prior-authorization request this gateway builds cannot carry"},
		{name: "a NutritionOrder whose orderer is not held", optional: optionalContextReference("NutritionOrder", "optional-order"),
			records: map[string][]byte{"NutritionOrder/optional-order": []byte(`{"resourceType":"NutritionOrder","id":"optional-order","status":"active","intent":"order","patient":{"reference":"Patient/MBR-OX"},"orderer":{"reference":"Practitioner/opt-ord"}}`)},
			status:  http.StatusBadGateway, body: "NutritionOrder/optional-order: the order's requester Practitioner/opt-ord is not in your system of record"},
		{name: "an Encounter naming a participant", optional: optionalContextReference("Encounter", "optional-order"),
			records: map[string][]byte{"Encounter/optional-order": []byte(`{"resourceType":"Encounter","id":"optional-order","status":"finished","subject":{"reference":"Patient/MBR-OX"},"participant":[{"individual":{"reference":"Practitioner/opt-enc"}}]}`)},
			status:  http.StatusBadGateway, body: "Encounter/optional-order references Practitioner/opt-enc, which a prior-authorization request this gateway builds cannot carry"},
		{name: "the system's read fails", optional: opt, failRef: "ServiceRequest/optional-order", failErr: &SoRReadError{Kind: SoRUnavailable},
			status: http.StatusServiceUnavailable, body: "system of record unavailable", upstream: true},
	}
}

// REJECTION, at every level: a context the provider's QR names that the
// provider-data build cannot carry is refused before the payer, nothing sent
// on the PAS leg. Each is the fidelity of SHN's own build at the build stage,
// 502 (what cannot be carried is what the gateway read from its own
// participant's system, its $populate answer and the records it names), but
// the system's failed read, which is the system's own failure. Fidelity is a
// network rule, so the refusal is the same at every level.
//
// Each level runs as a top-level test of its own, as the evidence rows do.
func TestQRContext_RefusedAtNone(t *testing.T)       { qrContextRefused(t, EnforcementNone) }
func TestQRContext_RefusedAtObserve(t *testing.T)    { qrContextRefused(t, EnforcementObserve) }
func TestQRContext_RefusedAtStructural(t *testing.T) { qrContextRefused(t, EnforcementStructural) }
func TestQRContext_RefusedAtStrict(t *testing.T)     { qrContextRefused(t, EnforcementStrict) }

func qrContextRefused(t *testing.T, level ConformanceEnforcement) {
	t.Parallel()
	for _, row := range qrContextRefusalRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := runAuthoredHomeOxygen(t, authoredHomeOxygenRun{level: level, optional: row.optional, records: row.records, failRef: row.failRef, failErr: row.failErr})
			wantQRContextRefused(t, got, row.status, row.body, row.upstream)
			if row.unread != "" && countReads(got.reads, row.unread) != 0 {
				t.Fatalf("%s was read: it is refused by its type, before any read", row.unread)
			}
		})
	}
}

// wantQRContextRefused fails unless the run was answered status with body,
// sent no PAS request, and recorded the refusal as the fidelity of SHN's own
// build at the build stage (or, upstream, as the system's failure).
func wantQRContextRefused(t *testing.T, got authoredHomeOxygenResult, status int, body string, upstream bool) {
	t.Helper()
	if got.code != status || !strings.Contains(got.body, body) {
		t.Fatalf("answer %d %s, want %d %s", got.code, got.body, status, body)
	}
	if len(got.outgoing) != 0 {
		t.Fatal("the PAS request was sent")
	}
	r := onlyRecord(t, got.recs)
	if upstream {
		if r.Outcome != ExchangeUpstreamError || r.Status != status {
			t.Fatalf("record %s status %d, want %s %d", r.Outcome, r.Status, ExchangeUpstreamError, status)
		}
		return
	}
	wantRefusal(t, r, status, RefusedByProviderGateway, RefusalFidelity)
	if r.RefusalStage != vocab.RefusalStageBuild {
		t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
	}
}

// REJECTION, at every level: on the demo profile, which reads no orders from
// a system of record, a QR naming a context beside the exchange's own order
// and Coverage is refused before the payer, on the fidelity of SHN's own
// build, 502: nothing can supply the record.
func TestQRContext_DemoProfileRefuses(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			t.Parallel()
			got := runAuthoredHomeOxygen(t, authoredHomeOxygenRun{level: level, profile: "demo", optional: optionalContextReference("ServiceRequest", "optional-order")})
			wantQRContextRefused(t, got, http.StatusBadGateway, "ServiceRequest/optional-order cannot be filled: this gateway's demo profile reads no orders from a system of record", false)
		})
	}
}

// REJECTION, at every level: on the demo profile a context in any literal
// shape the request does not carry is refused, an absolute one included; and
// a context the request already carries is no record to fill, on either
// profile, so the request is sent.
func TestQRContext_DemoProfileRefusesEveryUncarriedShape(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			t.Parallel()
			const abs = "https://ehr.example/fhir/ServiceRequest/optional-order"
			got := runAuthoredHomeOxygen(t, authoredHomeOxygenRun{level: level, profile: "demo", optional: `{"reference":"` + abs + `"}`})
			wantQRContextRefused(t, got, http.StatusBadGateway, abs+" cannot be filled: this gateway's demo profile reads no orders from a system of record", false)
			for _, profile := range []string{"demo", "provider-data"} {
				got := runAuthoredHomeOxygen(t, authoredHomeOxygenRun{level: level, profile: profile, optional: `{"reference":"Patient/MBR-OX"}`})
				if got.code != http.StatusOK || len(got.outgoing) == 0 {
					t.Fatalf("%s: a context the request carries: %d %s, want the request sent", profile, got.code, got.body)
				}
				// A context naming a resource the QR contains is the QR's own.
				got = runAuthoredHomeOxygen(t, authoredHomeOxygenRun{level: level, profile: profile, optional: `{"reference":"#ctx-order"}`,
					contained: `{"resourceType":"ServiceRequest","id":"ctx-order","status":"active","intent":"order","subject":{"reference":"Patient/MBR-OX"}}`})
				if got.code != http.StatusOK || len(got.outgoing) == 0 {
					t.Fatalf("%s: a context the QR contains: %d %s, want the request sent", profile, got.code, got.body)
				}
			}
		})
	}
}

// The not-held split, by structure: a record the caller's request names that
// the system does not hold is 422, and a QR context it does not hold 502,
// though both say the same words. The status follows where the reference
// came from, never the text.
func TestQRContext_NotHeldStatusFollowsWhereTheReferenceCameFrom(t *testing.T) {
	named, context := recordNotHeld("Condition/c"), contextNotHeld("Condition/c")
	if named.Error() != context.Error() {
		t.Fatalf("the two say different things: %q, %q", named, context)
	}
	if got := buildFidelityStatus(named); got != http.StatusUnprocessableEntity {
		t.Fatalf("request-named evidence not held = %d, want 422", got)
	}
	if got := buildFidelityStatus(context); got != http.StatusBadGateway {
		t.Fatalf("a QR context not held = %d, want 502", got)
	}
	for _, err := range []error{named, context} {
		if _, _, ok := evidenceFidelityRefusal(err); !ok {
			t.Fatalf("%v is not a fidelity refusal of the build", err)
		}
	}
}
