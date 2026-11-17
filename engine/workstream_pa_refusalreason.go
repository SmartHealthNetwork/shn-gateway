//go:build !shn_coreonly

package engine

// workstream_pa_refusalreason.go — the PA adapter's registered refusal
// reasons (refusalreason.go): its ingress routes' refusals before the request
// reaches the Hub.
const (
	reasonCDexSubmissionTwoWays        refusalReason = errCDexSubmissionTwoWays
	reasonCDexRequestTwoWays           refusalReason = errCDexRequestTwoWays
	reasonEligibilityTwoWays           refusalReason = "it can be read two ways, so it is not read"
	reasonEligibilityAnotherCase       refusalReason = "member names are case-sensitive, so the request names no patient to bind"
	reasonEligibilityReplacedValue     refusalReason = errEligibilityReplacedCharacter
	reasonCDexNoMember                 refusalReason = errCDexNoMember
	reasonCDexRequestNotTask           refusalReason = errCDexRequestNotTask
	reasonCDexRequestCode              refusalReason = errCDexRequestCode
	reasonCDexRequestNoPatient         refusalReason = errCDexRequestNoPatient
	reasonNextQuestionNotInput         refusalReason = errNextQuestionNotInput
	reasonNextQuestionNoSubject        refusalReason = errNextQuestionNoSubject
	reasonNextQuestionNotPatient       refusalReason = errNextQuestionNotPatient
	reasonNoCoverage                   refusalReason = noCoverageInRequestOrSoR
	reasonNoCoverageToRouteBy          refusalReason = "no coverage to route by"
	reasonCoverageNotSearchable        refusalReason = msgCoverageNotSearchable
	reasonCoverageUnreadable           refusalReason = msgCoverageUnreadable
	reasonCoveragesNameTwoPayers       refusalReason = msgCoveragesNameTwoPayers
	reasonCRDLineUnchanged             refusalReason = msgCRDLineUnchanged
	reasonDTRLineUnchanged             refusalReason = msgDTRLineUnchanged
	reasonFramedDTRUnsupported         refusalReason = "payer gateway does not support framed DTR operations (upgrade required)"
	reasonCDexReturnPathNoPayer        refusalReason = errCDexReturnPathNoPayer
	reasonCDexSubmissionNoPayer        refusalReason = errCDexSubmissionNoPayer
	reasonCDexRequestNoProvider        refusalReason = errCDexRequestNoProvider
	reasonCDexRequestOwnerUnreadable   refusalReason = errCDexRequestOwnerUnreadable
	reasonCDexRequestOwnerNoProvider   refusalReason = errCDexRequestOwnerNoProvider
	reasonCDexRequestOwnerTwoProviders refusalReason = errCDexRequestOwnerTwoProviders
	reasonEligibilityTwoPayers         refusalReason = eligibilityBatchTwoPayers
	reasonEligibilityNoPayer           refusalReason = "no payer to route the eligibility request by"
	reasonEligibilityRequestTwoPayers  refusalReason = eligibilityTwoPayers
	reasonEligibilityAmbiguousRef      refusalReason = "ambiguous reference for routing"
	reasonNextQuestionAddress          refusalReason = errNextQuestionAddress
	reasonNextQuestionNoPayer          refusalReason = errNextQuestionNoPayer
	reasonNextQuestionUnknownPayer     refusalReason = errNextQuestionUnknownPayer
	reasonNextQuestionNoSubpath        refusalReason = errNextQuestionNoSubpath
	reasonNextQuestionTwoAddresses     refusalReason = errNextQuestionTwoAddresses
	reasonAttachmentBudget             refusalReason = errAttachmentBudget
	reasonAttachmentOverDeclared       refusalReason = errAttachmentTooLarge
	reasonEligibilityBatchLimit        refusalReason = msgEligibilityBatchLimit
	reasonCDexReadPlainBase            refusalReason = errCDexReadPlainBase
	reasonCDexReadPath                 refusalReason = errCDexReadPath
	reasonCDexReadBody                 refusalReason = errCDexReadBody
	reasonPASBaseNoPayer               refusalReason = errPASBaseNoPayer
	reasonPASSubscriptionPlainBase     refusalReason = errPASSubscriptionPlainBase
	reasonPASSubscriptionNotFound      refusalReason = errPASSubscriptionNotFound
)

// paRefusalReasons are the PA adapter's registered reasons.
func paRefusalReasons() []refusalReason {
	return []refusalReason{
		reasonCDexSubmissionTwoWays, reasonCDexRequestTwoWays, reasonEligibilityTwoWays,
		reasonEligibilityAnotherCase, reasonEligibilityReplacedValue, reasonCDexNoMember,
		reasonCDexRequestNotTask, reasonCDexRequestCode, reasonCDexRequestNoPatient,
		reasonNextQuestionNotInput, reasonNextQuestionNoSubject, reasonNextQuestionNotPatient,
		reasonNoCoverage, reasonNoCoverageToRouteBy,
		reasonCoverageNotSearchable, reasonCoverageUnreadable, reasonCoveragesNameTwoPayers,
		reasonCRDLineUnchanged, reasonDTRLineUnchanged, reasonFramedDTRUnsupported,
		reasonCDexReturnPathNoPayer, reasonCDexSubmissionNoPayer, reasonCDexRequestNoProvider,
		reasonCDexRequestOwnerUnreadable, reasonCDexRequestOwnerNoProvider,
		reasonCDexRequestOwnerTwoProviders, reasonEligibilityTwoPayers, reasonEligibilityNoPayer,
		reasonEligibilityRequestTwoPayers, reasonEligibilityAmbiguousRef, reasonNextQuestionAddress,
		reasonNextQuestionNoPayer, reasonNextQuestionUnknownPayer, reasonNextQuestionNoSubpath, reasonNextQuestionTwoAddresses,
		reasonAttachmentBudget, reasonAttachmentOverDeclared,
		reasonEligibilityBatchLimit,
		reasonCDexReadPlainBase, reasonCDexReadPath, reasonCDexReadBody,
		reasonSubscriberHistory, reasonSubscriberTypeStatus, reasonSubscriberInteraction,
		reasonSubscriberSearch, reasonSubscriberOneEndpoint, reasonSubscriberBody, reasonSubscriberIfMatch,
		reasonSubscriberEndpoint, reasonSubscriberEndpointLoop, reasonSubscriberSearchLoop, reasonSubscriberNotASubscription, reasonSubscriberHoldsResource,
		reasonCDexSubscriptionPlainCreate, reasonCDexSubscriptionPlainRead,
		reasonScopeSubscriptionCreate, reasonScopeSubscriptionRead, reasonScopeSubscriptionUpdate,
		reasonScopeSubscriptionDelete, reasonScopeSubscriptionSearch,
		reasonNotifyNotANotification, reasonNotifyContent, reasonNotifyShape, reasonNotifyContext, reasonNotifyAddress, reasonNotifyNotRekeyable,
		reasonPASBaseNoPayer, reasonPASSubscriptionPlainBase, reasonPASSubscriptionNotFound,
	}
}

// The subscriber's refusals of its system's Subscription interaction
// (workstream_pa_subscriber.go).
const (
	reasonSubscriberHistory      refusalReason = errSubscriberHistory
	reasonSubscriberTypeStatus   refusalReason = errSubscriberTypeStatus
	reasonSubscriberInteraction  refusalReason = errSubscriberInteraction
	reasonSubscriberSearch       refusalReason = errSubscriberSearch
	reasonSubscriberOneEndpoint  refusalReason = errSubscriberOneEndpoint
	reasonSubscriberBody         refusalReason = errSubscriberBody
	reasonSubscriberIfMatch      refusalReason = errSubscriberIfMatch
	reasonSubscriberEndpoint     refusalReason = "channel.endpoint must be an absolute https URL with no userinfo, fragment, space or backslash, where notifications are delivered with the network's integrity; nothing was sent"
	reasonSubscriberEndpointLoop refusalReason = "channel.endpoint is a notification address of this network, which would deliver each notification back into the network; nothing was sent"
	reasonSubscriberSearchLoop   refusalReason = "the search's url is a notification address of this network, under which no Subscription of this system is registered (the channel.endpoint its Subscriptions name is its own endpoint); nothing was sent"
	// The body judged by its structure (subscription.ReadSubscription,
	// subscription.CheckNoResource): the leg carries a Subscription alone, and
	// one that holds no resource.
	reasonSubscriberNotASubscription refusalReason = "the body is a resource other than a Subscription, which this leg never carries (per-leg authority); nothing was sent"
	reasonSubscriberHoldsResource    refusalReason = "the Subscription holds a resource, which this leg, naming no patient on either side, never carries (per-leg authority); nothing was sent"
)

// The refusal reasons of the Subscription routes (refusalReasons).
const (
	reasonCDexSubscriptionPlainCreate refusalReason = errCDexSubscriptionPlainCreate
	reasonCDexSubscriptionPlainRead   refusalReason = errCDexSubscriptionPlainRead
	reasonScopeSubscriptionCreate     refusalReason = "insufficient_scope: this route takes a bearer granted a scope that permits creating a Subscription, such as " + SubscriptionWriteScope + ", system/Subscription.c or system/Subscription.*, with no query"
	reasonScopeSubscriptionRead       refusalReason = "insufficient_scope: this route takes a bearer granted a scope that permits reading a Subscription, such as " + SubscriptionReadScope + ", system/Subscription.r or system/Subscription.*, with no query"
	reasonScopeSubscriptionUpdate     refusalReason = "insufficient_scope: this route takes a bearer granted a scope that permits updating a Subscription, such as " + SubscriptionWriteScope + ", system/Subscription.u or system/Subscription.*, with no query"
	reasonScopeSubscriptionDelete     refusalReason = "insufficient_scope: this route takes a bearer granted a scope that permits deleting a Subscription, such as " + SubscriptionWriteScope + ", system/Subscription.d or system/Subscription.*, with no query"
	reasonScopeSubscriptionSearch     refusalReason = "insufficient_scope: this route takes a bearer granted a scope that permits searching Subscriptions, such as " + SubscriptionReadScope + ", system/Subscription.s or system/Subscription.*, with no query"
)

// The notification route's refusals (workstream_pa_cdex_notify.go).
const (
	reasonNotifyNotANotification refusalReason = errNotifyNotANotification
	reasonNotifyContent          refusalReason = errNotifyContent
	reasonNotifyContext          refusalReason = errNotifyContext
	reasonNotifyAddress          refusalReason = errNotifyAddress
	reasonNotifyNotRekeyable     refusalReason = errNotifyNotRekeyable
	reasonNotifyShape            refusalReason = errNotifyShape
)
