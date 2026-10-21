package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// paLegOwnership is the PA workstream's ownership rows: what each transmit of
// each PA leg carries today (relay.RegisterOwnership adds the rows every leg
// has: the requester's relayed application error and the refusals). A path
// that still rebuilt a participant's message would do so under an interim
// builder, permitted only where it is listed here; none remains.
//
// Transmits that are not exchange legs are outside the table: the gateway's
// discovery and conformance documents (CDS Services discovery,
// CapabilityStatement, the Da Vinci configuration document), its token and
// authorization endpoints, the call to the participant's own pre-population
// service, the reads that send no body (service discovery, a follow-up read
// of a pending decision), and the summaries a scenario endpoint returns to its
// caller.
var paLegOwnership = func() map[relay.Key]relay.Rule {
	k := func(leg string, role relay.Role, dir relay.Direction, outcome relay.Outcome) relay.Key {
		return relay.Key{Leg: leg, Role: role, Direction: dir, Outcome: outcome}
	}
	m := map[relay.Key]relay.Rule{
		// Requester to the network, carrying its participant's request.
		// A prior-authorization Bundle is relayed exactly; a CDS Hooks
		// request is relayed exactly or with the callback removed, absent
		// prefetch values added under the enrichment opt-in, and the coverage
		// read through the removed fhirServer to route by carried; a questionnaire package request is relayed
		// exactly or with the patient's Coverage added when it carries none (and,
		// under the unknown-member seam, the provider's own Patient record).
		k("crd-order-dispatch", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeCarried): {
			Allowed: []relay.Ownership{relay.OwnershipRelayed, relay.OwnershipEdited}, Edits: []relay.EditID{relay.EditCDSCallbackStrip, relay.EditCDSPrefetchObtain, relay.EditCDSCoverageCarry},
		},
		k("crd-order-select", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeCarried): {
			Allowed: []relay.Ownership{relay.OwnershipRelayed, relay.OwnershipEdited}, Edits: []relay.EditID{relay.EditCDSCallbackStrip, relay.EditCDSPrefetchObtain, relay.EditCDSCoverageCarry},
		},
		k("dtr-questionnaire-fetch", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeCarried): {
			Allowed: []relay.Ownership{relay.OwnershipRelayed, relay.OwnershipEdited}, Edits: []relay.EditID{relay.EditDTRCoverageObtain, relay.EditDTRPatientObtain},
		},
		k("pas-claim", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeCarried):         {Allowed: paRelayed},
		k("pas-claim-inquire", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeCarried): {Allowed: paRelayed},
		// A coverage eligibility request the participant's own system submits
		// is relayed exactly, in the form it was submitted.
		k("coverage-eligibility", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeCarried): {Allowed: paRelayed},
		k("pas-claim-update", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeCarried):     {Allowed: paRelayed},

		// Requester to the network, with a request its own workflow makes.
		k("coverage-eligibility", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeOriginated):    {Allowed: paAuthored, Builders: []relay.BuilderID{relay.BuilderSDKEligibility}},
		k("crd-order-dispatch", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeOriginated):      {Allowed: paAuthored, Builders: []relay.BuilderID{relay.BuilderSDKCRDRequest}},
		k("crd-order-select", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeOriginated):        {Allowed: paAuthored, Builders: []relay.BuilderID{relay.BuilderSDKCRDRequest}},
		k("dtr-questionnaire-fetch", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeOriginated): {Allowed: paAuthored, Builders: []relay.BuilderID{relay.BuilderSDKDTRPackage, relay.BuilderDTRNextQuestion}},
		k("patient-dtr", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeOriginated):             {Allowed: paAuthored, Builders: []relay.BuilderID{relay.BuilderSDKPatientDTR}},
		// A submission or update the gateway built is sent as built, or, built
		// at this gateway's line and carried to the payer's through the
		// registered transform (E-09), as the builder's message then that edit.
		k("pas-claim", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeOriginated): {Allowed: paAuthoredOrTransformed, Edits: paTransform, Builders: []relay.BuilderID{relay.BuilderSDKPASSubmit}},
		// An inquiry the requester's own workflow makes: the gateway asks a payer
		// about an authorization it pended, on behalf of a participant whose
		// system is not the one asking. The participant's own inquiry is carried,
		// not authored, and has its own row above.
		k("pas-claim-inquire", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeOriginated): {Allowed: paAuthored, Builders: []relay.BuilderID{relay.BuilderSDKPASInquiry}},
		k("pas-claim-update", relay.RoleRequester, relay.DirectionRequest, relay.OutcomeOriginated):  {Allowed: paAuthoredOrTransformed, Edits: paTransform, Builders: []relay.BuilderID{relay.BuilderSDKPASUpdate}},

		// Requester to its participant's system: the recipient's answer,
		// exactly as it arrived, apart from one IG-line bridge.
		k("crd-order-dispatch", relay.RoleRequester, relay.DirectionResponse, relay.OutcomeAnswered): {Allowed: paRelayed},
		k("crd-order-select", relay.RoleRequester, relay.DirectionResponse, relay.OutcomeAnswered):   {Allowed: paRelayed},
		// A questionnaire package answer from another DTR line reaches the
		// requester's system under its own line's parameter names.
		k("dtr-questionnaire-fetch", relay.RoleRequester, relay.DirectionResponse, relay.OutcomeAnswered): {
			Allowed: []relay.Ownership{relay.OwnershipRelayed, relay.OwnershipEdited}, Edits: []relay.EditID{relay.EditDTRAnswerLineNames},
		},
		k("pas-claim", relay.RoleRequester, relay.DirectionResponse, relay.OutcomeAnswered):            {Allowed: paRelayed},
		k("pas-claim-inquire", relay.RoleRequester, relay.DirectionResponse, relay.OutcomeAnswered):    {Allowed: paRelayed},
		k("coverage-eligibility", relay.RoleRequester, relay.DirectionResponse, relay.OutcomeAnswered): {Allowed: paRelayed},
		k("pas-claim-update", relay.RoleRequester, relay.DirectionResponse, relay.OutcomeAnswered):     {Allowed: paRelayed},

		// Recipient to its participant's system, carrying the network's
		// request. A prior-authorization Bundle and a CDS Hooks request are
		// relayed exactly, or with the optional payer-identity mapping. A
		// questionnaire operation's own input is relayed the same way.
		k("crd-order-dispatch", relay.RoleRecipient, relay.DirectionRequest, relay.OutcomeCarried): {
			Allowed: []relay.Ownership{relay.OwnershipRelayed, relay.OwnershipEdited}, Edits: paPayerIdentityMapped,
		},
		k("crd-order-select", relay.RoleRecipient, relay.DirectionRequest, relay.OutcomeCarried): {
			Allowed: []relay.Ownership{relay.OwnershipRelayed, relay.OwnershipEdited}, Edits: paPayerIdentityMapped,
		},
		k("dtr-questionnaire-fetch", relay.RoleRecipient, relay.DirectionRequest, relay.OutcomeCarried): {
			Allowed: []relay.Ownership{relay.OwnershipRelayed, relay.OwnershipEdited}, Edits: paPayerIdentityMapped,
		},
		k("pas-claim", relay.RoleRecipient, relay.DirectionRequest, relay.OutcomeCarried): {
			Allowed: []relay.Ownership{relay.OwnershipRelayed, relay.OwnershipEdited}, Edits: paPayerIdentityMapped,
		},
		k("pas-claim-update", relay.RoleRecipient, relay.DirectionRequest, relay.OutcomeCarried): {
			Allowed: []relay.Ownership{relay.OwnershipRelayed, relay.OwnershipEdited}, Edits: paPayerIdentityMapped,
		},
		// An inquiry names an authorization; it is sent to the payer's own
		// system exactly, or with only the payer identity mapped.
		k("pas-claim-inquire", relay.RoleRecipient, relay.DirectionRequest, relay.OutcomeCarried): {
			Allowed: []relay.Ownership{relay.OwnershipRelayed, relay.OwnershipEdited}, Edits: paPayerIdentityMapped,
		},
		// A coverage-eligibility request goes to the payer's own system only when
		// the payer declares its own eligibility endpoint, and then exactly.
		k("coverage-eligibility", relay.RoleRecipient, relay.DirectionRequest, relay.OutcomeCarried): {Allowed: paRelayed},

		// Recipient to the network, answering. Patient-authored answers are
		// the gateway's own messages; eligibility is the
		// gateway's own answer from the payer's records, or the payer's answer
		// relayed when the payer declares its own endpoint; the others are the
		// participant's answer, relayed.
		k("coverage-eligibility", relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeAnswered): {
			Allowed: []relay.Ownership{relay.OwnershipAuthored, relay.OwnershipRelayed}, Builders: []relay.BuilderID{relay.BuilderSDKEligibility},
		},
		k("crd-order-dispatch", relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeAnswered):      {Allowed: paRelayed},
		k("crd-order-select", relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeAnswered):        {Allowed: paRelayed},
		k("dtr-questionnaire-fetch", relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeAnswered): {Allowed: paRelayed},
		k("patient-dtr", relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeAnswered):             {Allowed: paAuthored, Builders: []relay.BuilderID{relay.BuilderSDKPatientDTR}},
		k("pas-claim", relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeAnswered):               {Allowed: paRelayed},
		k("pas-claim-update", relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeAnswered):        {Allowed: paRelayed},
		// An inquiry's answer is the payer's own message, relayed. No builder is
		// listed: this leg has never rebuilt an answer, so nothing may author one.
		k("pas-claim-inquire", relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeAnswered): {Allowed: paRelayed},
	}
	// An application error from the participant's system is relayed exactly
	// as it answered (for coverage-eligibility, from a payer that declares its
	// own endpoint), an empty body included, to a requester that negotiated a
	// frame. A requester that did not cannot receive it: the gateway checks
	// the error under this key, then answers with its own refusal under the
	// refused key, so the participant's bytes never leave it unsealed.
	for _, leg := range []string{"coverage-eligibility", "crd-order-dispatch", "crd-order-select", "dtr-questionnaire-fetch", "pas-claim", "pas-claim-inquire", "pas-claim-update"} {
		m[k(leg, relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeUpstreamError)] = relay.Rule{Allowed: paRelayed}
	}
	return m
}()

var (
	paRelayed  = []relay.Ownership{relay.OwnershipRelayed}
	paAuthored = []relay.Ownership{relay.OwnershipAuthored}
	// paAuthoredOrTransformed and paTransform are the gateway's own message,
	// as built or carried to another IG line by the registered transform.
	paAuthoredOrTransformed = []relay.Ownership{relay.OwnershipAuthored, relay.OwnershipEdited}
	paTransform             = []relay.EditID{relay.EditRegisteredTransform}
	// paPayerIdentityMapped is a request relayed exactly or with the payer
	// identity mapped.
	paPayerIdentityMapped = []relay.EditID{relay.EditPayorEdgeRestamp}
)
