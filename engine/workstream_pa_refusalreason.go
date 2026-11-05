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
	reasonNextQuestionUnverifiable     refusalReason = errNextQuestionUnverifiable
	reasonAttachmentBudget             refusalReason = errAttachmentBudget
	reasonAttachmentOverDeclared       refusalReason = errAttachmentTooLarge
	reasonEligibilityBatchLimit        refusalReason = msgEligibilityBatchLimit
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
		reasonNextQuestionNoPayer, reasonNextQuestionUnknownPayer, reasonNextQuestionNoSubpath,
		reasonNextQuestionUnverifiable, reasonAttachmentBudget, reasonAttachmentOverDeclared,
		reasonEligibilityBatchLimit,
	}
}
