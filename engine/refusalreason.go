package engine

// refusalreason.go — the reasons a gateway's exchange record keeps for the
// refusals it makes before the request it refuses reaches the Hub
// (ExchangeRecord.RefusalReason).
//
// Such a refusal reaches no Hub record, so the gateway's own record keeps the
// reason its participant was given beside the status, refusal.by and
// refusal.rule. The reason is the gateway's own fixed refusal sentence: never a
// value read from the request or its body, a credential or a patient. Every
// reason is a constant of type refusalReason declared in this file or in a
// zone's own (workstream_pa_refusalreason.go, workstream_fqs_refusalreason.go,
// module1_refusalreason.go), listed in coreRefusalReasons or in the list its
// workstream or module supplies (refusalReasons); emitExchange records any
// other value as ExchangeOther, and TestRefusalReasons_OnlyRegisteredConstants
// pins that no other file declares or converts one. Where an answer adds a
// detail read from the request or one the gateway chose (an identifier, a
// member name, a count, the contract lines), the answer is unchanged and the
// reason is the fixed sentence in it.

// refusalReason is one of the registered fixed sentences: the whole of the
// answer a refusal writes, or, where the answer adds a detail read from the
// request or names one the gateway chose, the fixed sentence it contains.
type refusalReason string

// Authentication and scope at an ingress.
const (
	reasonIngressAuthentication refusalReason = msgIngressAuthentication
	reasonScopeTaskWrite        refusalReason = "insufficient_scope: this route takes a bearer granted " + TaskWriteScope
	reasonScenarioCaller        refusalReason = errUnauthorizedCaller
)

// Message integrity, authority and size, on every route.
const (
	reasonUnsupportedCoding     refusalReason = errUnsupportedCoding
	reasonPatientUnidentifiable refusalReason = msgPatientUnidentifiable
	reasonRequestTooLarge       refusalReason = errRequestTooLarge
	reasonEnvelopeTooLarge      refusalReason = errRequestTooLarge + ": sealed for the network"
)

// Routing with no recipient.
const (
	reasonNoPayerRouter           refusalReason = msgNoPayerRouter
	reasonNoPayerIdentifier       refusalReason = noPayerIdentifier
	reasonCoverageNamesTwoPayers  refusalReason = msgCoverageNamesTwoPayers
	reasonUnregisteredPayer       refusalReason = msgUnregisteredPayer
	reasonNoCoverageOnFile        refusalReason = msgNoCoverageOnFile
	reasonMemberCoverageTwoPayers refusalReason = msgAmbiguousCoverage
	reasonNoSharedLine            refusalReason = "no shared contract line"
)

// coreRefusalReasons are the core's registered reasons.
func coreRefusalReasons() []refusalReason {
	return []refusalReason{
		reasonIngressAuthentication, reasonScopeTaskWrite, reasonScenarioCaller,
		reasonUnsupportedCoding, reasonPatientUnidentifiable, reasonRequestTooLarge, reasonEnvelopeTooLarge,
		reasonNoPayerRouter, reasonNoPayerIdentifier, reasonCoverageNamesTwoPayers, reasonUnregisteredPayer,
		reasonNoCoverageOnFile, reasonMemberCoverageTwoPayers, reasonNoSharedLine,
	}
}

// closedReason is reason when it is a registered sentence (coreRefusalReasons,
// or one a compiled-in workstream or module registers), else ExchangeOther.
func closedReason(reason refusalReason) string {
	for _, list := range [][]refusalReason{coreRefusalReasons(), compiledRefusalReasons()} {
		for _, r := range list {
			if r == reason {
				return string(r)
			}
		}
	}
	return ExchangeOther
}

// scopeReason is the reason of the 403 a route that takes scope refuses a
// bearer granted another scope with; "" for a scope no route takes.
func scopeReason(scope string) refusalReason {
	if scope == TaskWriteScope {
		return reasonScopeTaskWrite
	}
	return ""
}
