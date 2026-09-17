package relay

import "slices"

// BuilderID names code that writes the gateway's own messages. Authored
// accepts only registered ids.
type BuilderID string

// Registered builders.
const (
	// BuilderGatewayRefusal: a refusal the gateway itself makes.
	BuilderGatewayRefusal BuilderID = "gateway-refusal"
	// BuilderCDexFulfillment: a data-request answer that extends the payer's
	// Task and embeds the facility's records unchanged.
	BuilderCDexFulfillment BuilderID = "cdex-fulfillment"
	// BuilderCDexRecords: the facility's answer to a data request: the
	// records its own server holds, each embedded unchanged, with the
	// patient they are about and the gateway's provenance for each.
	BuilderCDexRecords BuilderID = "cdex-records"
	// BuilderSoRSearchset: a searchset that joins the pages the
	// participant's own server returned, embedding each entry unchanged.
	BuilderSoRSearchset BuilderID = "sor-searchset"
	// BuilderSDKCRDRequest: a CDS Hooks request the gateway originates.
	BuilderSDKCRDRequest BuilderID = "sdk-crd-request"
	// BuilderSDKDTRPackage: a questionnaire package request or answer the
	// gateway originates.
	BuilderSDKDTRPackage BuilderID = "sdk-dtr-package"
	// BuilderSDKPASSubmit: a prior-authorization submission the gateway
	// originates.
	BuilderSDKPASSubmit BuilderID = "sdk-pas-submit"
	// BuilderSDKPASUpdate: a prior-authorization update the gateway
	// originates.
	BuilderSDKPASUpdate BuilderID = "sdk-pas-update"
	// BuilderSDKPASInquiry: a prior-authorization inquiry the gateway
	// originates.
	BuilderSDKPASInquiry BuilderID = "sdk-pas-inquiry"
	// BuilderSDKEligibility: an eligibility request the gateway originates.
	BuilderSDKEligibility BuilderID = "sdk-eligibility"
	// BuilderSDKFederatedQuery: a federated query request the gateway
	// originates.
	BuilderSDKFederatedQuery BuilderID = "sdk-federated-query"
	// BuilderSDKPatientDTR: a patient-facing questionnaire request the
	// gateway originates.
	BuilderSDKPatientDTR BuilderID = "sdk-patient-dtr"
	// BuilderDTRNextQuestion: an adaptive questionnaire's next-question
	// input the gateway originates.
	BuilderDTRNextQuestion BuilderID = "dtr-next-question"
	// BuilderLegacyDTREnvelope: the older questionnaire request envelope,
	// kept while peers that send it remain supported.
	BuilderLegacyDTREnvelope BuilderID = "legacy-dtr-envelope"
)

// Interim builders name existing code paths that still rebuild a
// participant's message instead of relaying it. Each is registered so that
// such a path is visible and checked; each is removed once its path relays
// the participant's bytes. BuilderInterimEmptyErrorSubstitution is the
// recipient's own error body in place of the participant's application
// error: for an empty error body, and for the bare error a requester that
// negotiated no frame receives.
const (
	BuilderInterimDTRProjection          BuilderID = "defect-dtr-projection"
	BuilderInterimPASAssembly            BuilderID = "defect-pas-assembly"
	BuilderInterimEmptyErrorSubstitution BuilderID = "defect-empty-error-substitution"
)

// builderTestInjected is reserved for payloads that tests inject. Authored
// refuses it.
const builderTestInjected BuilderID = "test-injected"

var registeredBuilders = []BuilderID{
	BuilderGatewayRefusal,
	BuilderCDexFulfillment,
	BuilderCDexRecords,
	BuilderSoRSearchset,
	BuilderSDKCRDRequest,
	BuilderSDKDTRPackage,
	BuilderSDKPASSubmit,
	BuilderSDKPASUpdate,
	BuilderSDKPASInquiry,
	BuilderSDKEligibility,
	BuilderSDKFederatedQuery,
	BuilderSDKPatientDTR,
	BuilderDTRNextQuestion,
	BuilderLegacyDTREnvelope,
}

var interimBuilders = []BuilderID{
	BuilderInterimDTRProjection,
	BuilderInterimPASAssembly,
	BuilderInterimEmptyErrorSubstitution,
}

// authoredBuilders is the closed set Authored accepts.
var authoredBuilders = func() map[BuilderID]struct{} {
	m := make(map[BuilderID]struct{}, len(registeredBuilders)+len(interimBuilders))
	for _, id := range slices.Concat(registeredBuilders, interimBuilders) {
		m[id] = struct{}{}
	}
	return m
}()

// Builders returns every registered builder id: the builders above, then
// the interim ones.
func Builders() []BuilderID { return slices.Concat(registeredBuilders, interimBuilders) }
