//go:build !shn_coreonly

package relay

import (
	"errors"
	"slices"
	"testing"
)

// zoneOwnership is a literal copy of the ownership rows the engine's zones
// register (the PA and FQS adapters' legs). A core-only build compiles no
// zone (ownership_coreonly_test.go).
func zoneOwnership() map[Key]Rule {
	R, E, A := OwnershipRelayed, OwnershipEdited, OwnershipAuthored
	own := func(o ...Ownership) []Ownership { return o }
	b := func(ids ...BuilderID) []BuilderID { return ids }
	req, resp := DirectionRequest, DirectionResponse
	rq, rc := RoleRequester, RoleRecipient
	refusal := Rule{Allowed: own(A), Builders: b("gateway-refusal")}
	relayOnly := Rule{Allowed: own(R)}
	m := map[Key]Rule{
		{"crd-order-dispatch", rq, req, OutcomeCarried}:      {Allowed: own(R, E), Edits: []EditID{"E-01", "E-02", "E-07"}},
		{"crd-order-select", rq, req, OutcomeCarried}:        {Allowed: own(R, E), Edits: []EditID{"E-01", "E-02", "E-07"}},
		{"dtr-questionnaire-fetch", rq, req, OutcomeCarried}: {Allowed: own(R, E), Edits: []EditID{"E-04", "E-05"}},
		{"pas-claim", rq, req, OutcomeCarried}:               relayOnly,
		{"pas-claim-inquire", rq, req, OutcomeCarried}:       relayOnly,
		{"coverage-eligibility", rq, req, OutcomeCarried}:    relayOnly,
		{"federated-query", rq, req, OutcomeCarried}:         relayOnly,
		{"pas-claim-update", rq, req, OutcomeCarried}:        relayOnly,
		{"cdex-attachment-submit", rq, req, OutcomeCarried}:  relayOnly,

		{"coverage-eligibility", rq, req, OutcomeOriginated}:    {Allowed: own(A), Builders: b("sdk-eligibility")},
		{"crd-order-dispatch", rq, req, OutcomeOriginated}:      {Allowed: own(A), Builders: b("sdk-crd-request")},
		{"crd-order-select", rq, req, OutcomeOriginated}:        {Allowed: own(A), Builders: b("sdk-crd-request")},
		{"dtr-questionnaire-fetch", rq, req, OutcomeOriginated}: {Allowed: own(A), Builders: b("sdk-dtr-package", "dtr-next-question")},
		{"federated-query", rq, req, OutcomeOriginated}:         {Allowed: own(A), Builders: b("sdk-federated-query")},
		{"patient-dtr", rq, req, OutcomeOriginated}:             {Allowed: own(A), Builders: b("sdk-patient-dtr")},
		{"pas-claim", rq, req, OutcomeOriginated}:               {Allowed: own(A, E), Edits: []EditID{"E-09"}, Builders: b("sdk-pas-submit")},
		{"pas-claim-inquire", rq, req, OutcomeOriginated}:       {Allowed: own(A), Builders: b("sdk-pas-inquiry")},
		{"pas-claim-update", rq, req, OutcomeOriginated}:        {Allowed: own(A, E), Edits: []EditID{"E-09"}, Builders: b("sdk-pas-update")},

		{"crd-order-dispatch", rq, resp, OutcomeAnswered}:      relayOnly,
		{"crd-order-select", rq, resp, OutcomeAnswered}:        relayOnly,
		{"dtr-questionnaire-fetch", rq, resp, OutcomeAnswered}: {Allowed: own(R, E), Edits: []EditID{"E-08"}},
		{"pas-claim", rq, resp, OutcomeAnswered}:               {Allowed: own(R, E), Edits: []EditID{"E-10"}},
		{"pas-claim-inquire", rq, resp, OutcomeAnswered}:       {Allowed: own(R, E), Edits: []EditID{"E-10"}},
		{"coverage-eligibility", rq, resp, OutcomeAnswered}:    relayOnly,
		{"federated-query", rq, resp, OutcomeAnswered}:         relayOnly,
		{"pas-claim-update", rq, resp, OutcomeAnswered}:        {Allowed: own(R, E), Edits: []EditID{"E-10"}},
		{"cdex-attachment-submit", rq, resp, OutcomeAnswered}:  relayOnly,

		{"coverage-eligibility", rq, resp, OutcomeUpstreamError}:    relayOnly,
		{"crd-order-dispatch", rq, resp, OutcomeUpstreamError}:      relayOnly,
		{"crd-order-select", rq, resp, OutcomeUpstreamError}:        relayOnly,
		{"dtr-questionnaire-fetch", rq, resp, OutcomeUpstreamError}: relayOnly,
		{"federated-query", rq, resp, OutcomeUpstreamError}:         relayOnly,
		{"patient-dtr", rq, resp, OutcomeUpstreamError}:             relayOnly,
		{"pas-claim", rq, resp, OutcomeUpstreamError}:               relayOnly,
		{"pas-claim-inquire", rq, resp, OutcomeUpstreamError}:       relayOnly,
		{"pas-claim-update", rq, resp, OutcomeUpstreamError}:        relayOnly,
		{"cdex-attachment-submit", rq, resp, OutcomeUpstreamError}:  relayOnly,

		{"coverage-eligibility", rc, req, OutcomeCarried}:    relayOnly,
		{"crd-order-dispatch", rc, req, OutcomeCarried}:      {Allowed: own(R, E), Edits: []EditID{"E-03"}},
		{"crd-order-select", rc, req, OutcomeCarried}:        {Allowed: own(R, E), Edits: []EditID{"E-03"}},
		{"dtr-questionnaire-fetch", rc, req, OutcomeCarried}: {Allowed: own(R, E), Edits: []EditID{"E-03"}},
		{"pas-claim", rc, req, OutcomeCarried}:               {Allowed: own(R, E), Edits: []EditID{"E-03"}},
		{"pas-claim-inquire", rc, req, OutcomeCarried}:       {Allowed: own(R, E), Edits: []EditID{"E-03"}},
		{"pas-claim-update", rc, req, OutcomeCarried}:        {Allowed: own(R, E), Edits: []EditID{"E-03"}},
		{"cdex-attachment-submit", rc, req, OutcomeCarried}:  relayOnly,

		{"coverage-eligibility", rc, resp, OutcomeAnswered}:    {Allowed: own(A, R), Builders: b("sdk-eligibility")},
		{"crd-order-dispatch", rc, resp, OutcomeAnswered}:      relayOnly,
		{"crd-order-select", rc, resp, OutcomeAnswered}:        relayOnly,
		{"dtr-questionnaire-fetch", rc, resp, OutcomeAnswered}: relayOnly,
		{"federated-query", rc, resp, OutcomeAnswered}:         {Allowed: own(A), Builders: b("cdex-fulfillment")},
		{"patient-dtr", rc, resp, OutcomeAnswered}:             {Allowed: own(A), Builders: b("sdk-patient-dtr")},
		{"pas-claim", rc, resp, OutcomeAnswered}:               {Allowed: own(R)},
		{"pas-claim-inquire", rc, resp, OutcomeAnswered}:       relayOnly,
		{"pas-claim-update", rc, resp, OutcomeAnswered}:        {Allowed: own(R)},
		{"cdex-attachment-submit", rc, resp, OutcomeAnswered}:  relayOnly,

		{"coverage-eligibility", rc, resp, OutcomeUpstreamError}:    relayOnly,
		{"crd-order-dispatch", rc, resp, OutcomeUpstreamError}:      relayOnly,
		{"crd-order-select", rc, resp, OutcomeUpstreamError}:        relayOnly,
		{"dtr-questionnaire-fetch", rc, resp, OutcomeUpstreamError}: relayOnly,
		{"pas-claim", rc, resp, OutcomeUpstreamError}:               relayOnly,
		{"pas-claim-inquire", rc, resp, OutcomeUpstreamError}:       relayOnly,
		{"pas-claim-update", rc, resp, OutcomeUpstreamError}:        relayOnly,
		{"cdex-attachment-submit", rc, resp, OutcomeUpstreamError}:  relayOnly,
	}
	for _, leg := range []string{"cdex-attachment-submit", "coverage-eligibility", "crd-order-dispatch", "crd-order-select",
		"dtr-questionnaire-fetch", "federated-query", "patient-dtr", "pas-claim", "pas-claim-inquire", "pas-claim-update"} {
		m[Key{leg, rq, resp, OutcomeRefused}] = refusal
		m[Key{leg, rc, resp, OutcomeRefused}] = refusal
	}
	return m
}

// The coverage carry (E-07) is admitted only where a provider's gateway
// carries its participant's CDS Hooks request to the network: a payload
// claiming it on any other transmit, another leg's request included, is
// refused, and nothing is sent.
func TestCoverageCarryOnlyOnTheCRDRequestTransmit(t *testing.T) {
	body := NewBody([]byte(`{"hook":"order-select","prefetch":{}}`), OriginIngressRequest)
	d := mustDoc(t, body)
	prefetch, ok := d.Member(d.Root(), "prefetch")
	if !ok {
		t.Fatal("no prefetch")
	}
	carry, err := Apply(body, "application/json", EditCDSCoverageCarry,
		d.InsertMember(prefetch, "coverage", []byte(`{"resourceType":"Bundle","type":"searchset","total":0,"entry":[]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := carry.Edits(); !slices.Equal(got, []EditID{EditCDSCoverageCarry}) {
		t.Fatalf("edits %v", got)
	}
	crdRequest := func(k Key) bool {
		return k.Role == RoleRequester && k.Direction == DirectionRequest && k.Outcome == OutcomeCarried &&
			(k.Leg == "crd-order-dispatch" || k.Leg == "crd-order-select")
	}
	admitted := 0
	for k := range LegOwnership() {
		err := Check(k)(carry)
		switch {
		case crdRequest(k) && err != nil:
			t.Errorf("%s refused the coverage carry: %v", k, err)
		case crdRequest(k):
			admitted++
		case err == nil:
			t.Errorf("%s admitted the coverage carry", k)
		}
	}
	if admitted != 2 {
		t.Fatalf("admitted on %d transmits, want the two CRD request transmits", admitted)
	}
	for _, k := range []Key{
		{"dtr-questionnaire-fetch", RoleRequester, DirectionRequest, OutcomeCarried},
		{"pas-claim", RoleRequester, DirectionRequest, OutcomeCarried},
		{"crd-order-select", RoleRecipient, DirectionRequest, OutcomeCarried},
	} {
		err := Check(k)(carry)
		var oe *OwnershipError
		if !errors.As(err, &oe) || oe.Key != k {
			t.Fatalf("%s: want an *OwnershipError, got %v", k, err)
		}
		if _, terr := Transmit(carry, Check(k)); terr == nil {
			t.Fatalf("%s: Transmit sent the refused payload", k)
		}
	}
}
