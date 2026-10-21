package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The remedy each refusal of an unresolved reference names, by the
// reference's form and position.
const (
	remedyRelativeInsurer  = "; send the Organization with the request (contained, or as an entry of its Bundle), the payer identifier on the insurer reference, or hold the Organization in this participant's system of record with the request's patient"
	remedyRelativeCoverage = "; send the Coverage with the request (contained, or as an entry of its Bundle), the payer identifier on the insurer reference, or hold the Coverage in this participant's system of record with the request's patient"
	remedyRelativePayor    = "; send the Organization with the request (contained, or as an entry of its Bundle), a payor identifier, or hold the Organization in this participant's system of record with the request's patient"
	remedyContainedInsurer = "; send the Organization as a contained resource with that id, or the payer identifier on the insurer reference"
	remedyEntryInsurer     = "; send the Organization as an entry of the request's Bundle whose fullUrl is that reference, or the payer identifier on the insurer reference"
	remedyEntryCoverage    = "; send the Coverage as an entry of the request's Bundle whose fullUrl is that reference, or the payer identifier on the insurer reference"
	remedyAbsoluteInsurer  = "; send the Organization as an entry of the request's Bundle whose fullUrl is that reference, or the payer identifier on the insurer reference; only a relative Organization/<id> reference is read from this participant's system of record"
	remedyVersionedInsurer = "; send the Organization with the request (contained, or as an entry of its Bundle), or the payer identifier on the insurer reference; only a relative Organization/<id> reference is read from this participant's system of record"
	// eligibilityHeldCoverageRemedy is the remedy for the payor of a
	// Coverage read from the system of record, whatever its form.
	eligibilityHeldCoverageRemedy = "a Coverage read from this participant's system of record names its payor by a payor identifier, an Organization it contains, or a relative Organization/<id> that system holds with the request's patient"
)

// eligibilitySoREnv is the in-process exchange at level with sor as the
// provider's system of record, and the payer answering for prefetchMember.
func eligibilitySoREnv(t *testing.T, level ConformanceEnforcement, sor *prefetchSoR) *inProcessExchange {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	env.originator.cfg.SoR = sor
	env.payerReturns(LegResult{Response: testResponse(payersEligibilityAnswer("Patient/" + prefetchMember))})
	return env
}

// eligibilitySoRHolding is a system of record naming prefetchMember's patient
// and holding resources by reference.
func eligibilitySoRHolding(t *testing.T, resources map[string]any) *prefetchSoR {
	t.Helper()
	sor := newPrefetchSoR()
	for ref, res := range resources {
		sor.reads[ref] = mustJSON(t, res)
	}
	return sor
}

// eligibilitySoRPayer is the payer Organization payer-org identified by
// value in the CMS payer system.
func eligibilitySoRPayer(value string) map[string]any {
	return map[string]any{"resourceType": "Organization", "id": "payer-org", "identifier": []any{eligibilityPayorIdentifier(value)}}
}

// eligibilitySoRCoverage is the Coverage id of prefetchMember with one payor.
func eligibilitySoRCoverage(id string, payor map[string]any) map[string]any {
	cov := eligibilityCoverage(id, payor)
	cov["beneficiary"] = map[string]any{"reference": "Patient/" + prefetchMember}
	return cov
}

// eligibilitySoRRequest is prefetchMember's request whose insurer is insurer,
// containing contained and naming refs in insurance.
func eligibilitySoRRequest(t *testing.T, insurer map[string]any, contained []any, refs ...string) []byte {
	t.Helper()
	var insurance []any
	for _, ref := range refs {
		insurance = append(insurance, map[string]any{"coverage": map[string]any{"reference": ref}})
	}
	return eligibilityWith(t, prefetchMember, map[string]any{"insurer": insurer, "contained": contained, "insurance": insurance})
}

// withRef is resource with key set to value.
func withRef(resource map[string]any, key string, value any) map[string]any {
	out := map[string]any{key: value}
	for k, v := range resource {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// readsOf is what the system of record was asked to read by reference.
func (s *prefetchSoR) readsOf() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.read)
}

// A provider's own request names its insurer, or a coverage and that
// coverage's payor, by a relative reference to a resource it does not carry:
// the resource is read from the participant's system of record, which names
// the request's patient, only to route. The request is carried byte-identical
// and the payer's answer relayed, at every level.
func TestEligibilityIngress_RoutesByReferencesTheSystemOfRecordHolds(t *testing.T) {
	cms := eligibilityPayorIdentifier(shnsdk.CMSPayerIdentity.Value)
	payer := eligibilitySoRPayer(shnsdk.CMSPayerIdentity.Value)
	byOrg := map[string]any{"reference": "Organization/payer-org"}
	display := map[string]any{"display": "the plan"}
	request, patient, coverage, _ := eligFAISParts(prefetchMember, shnsdk.CMSPayerIdentity.Value)
	request["insurer"] = display
	byOrgCoverage := map[string]any{}
	for k, v := range coverage {
		byOrgCoverage[k] = v
	}
	byOrgCoverage["payor"] = []any{byOrg}
	entryCoverage := map[string]any{}
	for k, v := range coverage {
		entryCoverage[k] = v
	}
	entryCoverage["payor"] = []any{map[string]any{"identifier": cms}}
	for name, row := range map[string]struct {
		body  []byte
		held  map[string]any
		reads []string
	}{
		"insurer Organization": {eligibilitySoRRequest(t, byOrg, nil),
			map[string]any{"Organization/payer-org": payer}, []string{"Organization/payer-org"}},
		"coverage naming its payor by identifier": {eligibilitySoRRequest(t, display, nil, "Coverage/cov-1"),
			map[string]any{"Coverage/cov-1": eligibilitySoRCoverage("cov-1", map[string]any{"identifier": cms})}, []string{"Coverage/cov-1"}},
		"coverage and its payor Organization": {eligibilitySoRRequest(t, display, nil, "Coverage/cov-1"),
			map[string]any{"Coverage/cov-1": eligibilitySoRCoverage("cov-1", byOrg), "Organization/payer-org": payer},
			[]string{"Coverage/cov-1", "Organization/payer-org"}},
		"coverage with its payor contained": {eligibilitySoRRequest(t, display, nil, "Coverage/cov-1"),
			map[string]any{"Coverage/cov-1": func() map[string]any {
				c := eligibilitySoRCoverage("cov-1", map[string]any{"reference": "#org"})
				c["contained"] = []any{map[string]any{"resourceType": "Organization", "id": "org", "identifier": []any{cms}}}
				return c
			}()}, []string{"Coverage/cov-1"}},
		"payor of a contained coverage": {eligibilitySoRRequest(t, display, []any{eligibilitySoRCoverage("cov", byOrg)}, "#cov"),
			map[string]any{"Organization/payer-org": payer}, []string{"Organization/payer-org"}},
		// In a Bundle, the entries answer first: a Coverage entry's payor no
		// entry answers is read from the system of record.
		"payor of a FAIS Bundle's Coverage entry": {eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", patient}, [2]any{"urn:uuid:coverage", byOrgCoverage}),
			map[string]any{"Organization/payer-org": payer}, []string{"Organization/payer-org"}},
		// A reference an entry answers is the entry's: the system of record,
		// holding a Coverage of that id naming another payer, is not read.
		"a Bundle entry over the system of record": {eligFAISBundle(t, [2]any{"", withRef(request, "insurance", []any{map[string]any{"coverage": map[string]any{"reference": "Coverage/cov-1"}}})},
			[2]any{"urn:uuid:patient", patient}, [2]any{"http://ehr.example/fhir/Coverage/cov-1", entryCoverage}),
			map[string]any{"Coverage/cov-1": eligibilitySoRCoverage("cov-1", map[string]any{"identifier": eligibilityPayorIdentifier("99999")})}, nil},
		// Each request of a batch is read for its own patient.
		"each request of a batch": {eligBatch(t, eligibilitySoRRequest(t, byOrg, nil), eligibilitySoRRequest(t, display, nil, "Coverage/cov-1")),
			map[string]any{"Organization/payer-org": payer, "Coverage/cov-1": eligibilitySoRCoverage("cov-1", map[string]any{"identifier": cms})},
			[]string{"Organization/payer-org", "Coverage/cov-1"}},
		// A reference the request names twice is read once.
		"a coverage named twice": {eligibilitySoRRequest(t, display, nil, "Coverage/cov-1", "Coverage/cov-1"),
			map[string]any{"Coverage/cov-1": eligibilitySoRCoverage("cov-1", map[string]any{"identifier": cms})}, []string{"Coverage/cov-1"}},
		// The insurer is the recipient, read first: the coverages are not read.
		"insurer over coverages": {eligibilitySoRRequest(t, byOrg, nil, "Coverage/cov-1"),
			map[string]any{"Organization/payer-org": payer}, []string{"Organization/payer-org"}},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sor := eligibilitySoRHolding(t, row.held)
				env := eligibilitySoREnv(t, level, sor)
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(row.body))
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s, want routed and answered", rec.Code, rec.Body)
				}
				if sent := env.lastRequestPayload(); !bytes.Equal(sent, row.body) {
					t.Fatalf("the payer's side received\n%s\nwant the request as sent\n%s", sent, row.body)
				}
				if x := got.only(t); x.Recipient != env.payerID || len(x.Edits) != 0 {
					t.Fatalf("record %+v, want the payer, carried unedited", x)
				}
				if reads := sor.readsOf(); !slices.Equal(reads, row.reads) {
					t.Fatalf("system of record read %q, want %q", reads, row.reads)
				}
			})
		}
	}
}

// A reference the system of record does not supply keeps the request
// unroutable, refused 422 naming the reference and what resolves it: the
// system does not hold it, does not name the request's patient (it is then
// not read), or the reference is not a relative one (never read). Coverages
// that, read from the system of record or carried, name different payers are
// refused, never picked. At every level, before anything is sent.
func TestEligibilityIngress_ReferencesTheSystemOfRecordDoesNotResolve(t *testing.T) {
	cms := eligibilityPayorIdentifier(shnsdk.CMSPayerIdentity.Value)
	byOrg := map[string]any{"reference": "Organization/payer-org"}
	display := map[string]any{"display": "the plan"}
	otherPatient := func(s *prefetchSoR) { s.sorID = "someone-else" }
	request, patient, _, faisPayer := eligFAISParts(prefetchMember, shnsdk.CMSPayerIdentity.Value)
	request["insurer"] = display
	insurerOnly := withRef(request, "insurer", byOrg)
	delete(insurerOnly, "insurance")
	for name, row := range map[string]struct {
		body  []byte
		held  map[string]any
		sor   func(*prefetchSoR)
		msg   string
		reads []string
	}{
		"insurer not held": {body: eligibilitySoRRequest(t, byOrg, nil),
			msg: eligibilityInsurerUnresolved + remedyRelativeInsurer + `: insurer "Organization/payer-org"`, reads: []string{"Organization/payer-org"}},
		"coverage not held": {body: eligibilitySoRRequest(t, display, nil, "Coverage/cov-1"),
			msg: eligibilityCoverageUnresolved + remedyRelativeCoverage + `: insurance[0].coverage "Coverage/cov-1"`, reads: []string{"Coverage/cov-1"}},
		"payor not held": {body: eligibilitySoRRequest(t, display, nil, "Coverage/cov-1"),
			held:  map[string]any{"Coverage/cov-1": eligibilitySoRCoverage("cov-1", byOrg)},
			msg:   eligibilityPayorUnresolved + "; " + eligibilityHeldCoverageRemedy + `: insurance[0].coverage "Coverage/cov-1", payor "Organization/payer-org"`,
			reads: []string{"Coverage/cov-1", "Organization/payer-org"}},
		"system naming another patient": {body: eligibilitySoRRequest(t, byOrg, nil),
			held: map[string]any{"Organization/payer-org": eligibilitySoRPayer(shnsdk.CMSPayerIdentity.Value)}, sor: otherPatient,
			msg: eligibilityInsurerUnresolved + remedyRelativeInsurer + `: insurer "Organization/payer-org"`},
		"absolute reference": {body: eligibilitySoRRequest(t, map[string]any{"reference": "http://ehr.example/fhir/Organization/payer-org"}, nil),
			held: map[string]any{"Organization/payer-org": eligibilitySoRPayer(shnsdk.CMSPayerIdentity.Value)},
			msg:  eligibilityInsurerUnresolved + remedyAbsoluteInsurer + `: insurer "http://ehr.example/fhir/Organization/payer-org"`},
		"versioned reference": {body: eligibilitySoRRequest(t, map[string]any{"reference": "Organization/payer-org/_history/1"}, nil),
			held: map[string]any{"Organization/payer-org": eligibilitySoRPayer(shnsdk.CMSPayerIdentity.Value)},
			msg:  eligibilityInsurerUnresolved + remedyVersionedInsurer + `: insurer "Organization/payer-org/_history/1"`},
		// The system is read for a request whose patient it names: another
		// request of the batch, for a patient it does not name, is not read
		// from it.
		"a batch's request for a patient the system does not name": {body: eligBatch(t, eligibilityRequest(t, prefetchMember),
			eligibilityWith(t, "other", map[string]any{"insurer": byOrg})),
			held: map[string]any{"Organization/payer-org": eligibilitySoRPayer(shnsdk.CMSPayerIdentity.Value)},
			msg:  "request 1 of the batch: " + eligibilityInsurerUnresolved + remedyRelativeInsurer + `: insurer "Organization/payer-org"`},
		// A reference a Bundle entry answers is that entry's, of whatever
		// type: it is not read from the system of record too.
		"a reference an entry of another type answers": {body: eligFAISBundle(t, [2]any{"", insurerOnly}, [2]any{"urn:uuid:patient", patient},
			[2]any{"Organization/payer-org", map[string]any{"resourceType": "Patient", "id": "payer-org"}}),
			held: map[string]any{"Organization/payer-org": eligibilitySoRPayer(shnsdk.CMSPayerIdentity.Value)}, msg: eligibilityNoPayer},
		// A Coverage read from the system of record is not among the
		// request's entries: its payor does not resolve against them.
		"a held coverage's payor against the request's entries": {body: eligFAISBundle(t,
			[2]any{"", withRef(request, "insurance", []any{map[string]any{"coverage": map[string]any{"reference": "Coverage/cov-9"}}})},
			[2]any{"urn:uuid:patient", patient}, [2]any{"urn:uuid:payer", faisPayer}),
			held:  map[string]any{"Coverage/cov-9": eligibilitySoRCoverage("cov-9", map[string]any{"reference": "urn:uuid:payer"})},
			msg:   eligibilityPayorUnresolved + "; " + eligibilityHeldCoverageRemedy + `: insurance[0].coverage "Coverage/cov-9", payor "urn:uuid:payer"`,
			reads: []string{"Coverage/cov-9"}},
		// A reference to another type than the one expected names no payer,
		// and is not read: its refusal is that of a request naming none.
		"insurer naming another type": {body: eligibilitySoRRequest(t, map[string]any{"reference": "Patient/" + prefetchMember}, nil), msg: eligibilityNoPayer},
		"coverage naming another type": {body: eligibilitySoRRequest(t, display, nil, "Organization/payer-org"),
			held: map[string]any{"Organization/payer-org": eligibilitySoRPayer(shnsdk.CMSPayerIdentity.Value)},
			msg:  eligibilityCoverageNoPayer + `: insurance[0].coverage "Organization/payer-org"`},
		"payor naming another type": {body: eligibilitySoRRequest(t, display, []any{eligibilitySoRCoverage("cov", map[string]any{"reference": "Patient/" + prefetchMember})}, "#cov"),
			msg: eligibilityCoverageNoPayer + `: insurance[0].coverage "#cov"`},
		// A urn names an entry of the request's Bundle, never a record.
		"insurer urn no entry answers": {body: eligibilitySoRRequest(t, map[string]any{"reference": "urn:uuid:payer"}, nil),
			msg: eligibilityInsurerUnresolved + remedyEntryInsurer + `: insurer "urn:uuid:payer"`},
		// A system that cannot name the patient counts as one that does not
		// hold it: nothing is read from it.
		"system failing to name the patient": {body: eligibilitySoRRequest(t, byOrg, nil),
			held: map[string]any{"Organization/payer-org": eligibilitySoRPayer(shnsdk.CMSPayerIdentity.Value)},
			sor:  func(s *prefetchSoR) { s.refErr = &SoRReadError{Kind: SoRUnavailable} },
			msg:  eligibilityInsurerUnresolved + remedyRelativeInsurer + `: insurer "Organization/payer-org"`},
		// A held Organization naming no payer identifier names no payer.
		"held insurer naming no payer": {body: eligibilitySoRRequest(t, byOrg, nil),
			held: map[string]any{"Organization/payer-org": map[string]any{"resourceType": "Organization", "id": "payer-org", "name": "the plan"}},
			msg:  eligibilityNoPayer, reads: []string{"Organization/payer-org"}},
		"coverages naming two payers": {body: eligibilitySoRRequest(t, display, []any{eligibilitySoRCoverage("cov", map[string]any{"identifier": cms})}, "#cov", "Coverage/cov-2"),
			held: map[string]any{"Coverage/cov-2": eligibilitySoRCoverage("cov-2", map[string]any{"identifier": eligibilityPayorIdentifier("99999")})},
			msg:  eligibilityTwoPayers, reads: []string{"Coverage/cov-2"}},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sor := eligibilitySoRHolding(t, row.held)
				if row.sor != nil {
					row.sor(sor)
				}
				env := eligibilitySoREnv(t, level, sor)
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(row.body))
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, jsonEscaped(row.msg))
				wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
				if reads := sor.readsOf(); !slices.Equal(reads, row.reads) {
					t.Fatalf("system of record read %q, want %q", reads, row.reads)
				}
			})
		}
	}
}

// A read of the system of record that fails is that system's failure, and so
// is an answer that is not the resource asked for: refused with the
// system-of-record failure status before anything is sent, at every level.
func TestEligibilityIngress_SystemOfRecordReadFailureIsItsOwn(t *testing.T) {
	byOrg := map[string]any{"reference": "Organization/payer-org"}
	unavailable := &SoRReadError{Kind: SoRUnavailable}
	unavailableStatus, unavailableMsg := SoRFailureResponse(unavailable)
	invalidStatus, invalidMsg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
	for name, row := range map[string]struct {
		body   []byte
		held   map[string]any
		failed string // the reference whose read fails
		status int
		msg    string
	}{
		"insurer read fails": {body: eligibilitySoRRequest(t, byOrg, nil), failed: "Organization/payer-org", status: unavailableStatus, msg: unavailableMsg},
		"coverage read fails": {body: eligibilitySoRRequest(t, map[string]any{"display": "the plan"}, nil, "Coverage/cov-1"),
			failed: "Coverage/cov-1", status: unavailableStatus, msg: unavailableMsg},
		"payor read fails": {body: eligibilitySoRRequest(t, map[string]any{"display": "the plan"}, nil, "Coverage/cov-1"),
			held:   map[string]any{"Coverage/cov-1": eligibilitySoRCoverage("cov-1", byOrg)},
			failed: "Organization/payer-org", status: unavailableStatus, msg: unavailableMsg},
		"another resource answered": {body: eligibilitySoRRequest(t, byOrg, nil),
			held:   map[string]any{"Organization/payer-org": map[string]any{"resourceType": "Patient", "id": "payer-org"}},
			status: invalidStatus, msg: invalidMsg},
		"another id answered": {body: eligibilitySoRRequest(t, byOrg, nil),
			held: map[string]any{"Organization/payer-org": map[string]any{"resourceType": "Organization", "id": "other",
				"identifier": []any{eligibilityPayorIdentifier(shnsdk.CMSPayerIdentity.Value)}}},
			status: invalidStatus, msg: invalidMsg},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sor := eligibilitySoRHolding(t, row.held)
				if row.failed != "" {
					sor.readErrs = map[string]error{row.failed: unavailable}
				}
				env := eligibilitySoREnv(t, level, sor)
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(row.body))
				refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
			})
		}
	}
}
