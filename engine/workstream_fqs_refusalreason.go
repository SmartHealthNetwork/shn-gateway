//go:build !shn_coreonly

package engine

// workstream_fqs_refusalreason.go — the FQS adapter's registered refusal
// reasons (refusalreason.go).
const (
	reasonFederatedQueryTwoWays    refusalReason = errFederatedQueryTwoWays
	reasonFederatedQueryNoPatient  refusalReason = "federated query rejected"
	reasonFederatedQueryNoOwner    refusalReason = federatedQueryNoOwner
	reasonFederatedQueryNoFacility refusalReason = "no facility to route the federated query to: the Task's owner"
)

// fqsRefusalReasons are the FQS adapter's registered reasons.
func fqsRefusalReasons() []refusalReason {
	return []refusalReason{
		reasonFederatedQueryTwoWays, reasonFederatedQueryNoPatient, reasonFederatedQueryNoOwner,
		reasonFederatedQueryNoFacility,
	}
}
