//go:build !shn_coreonly

package engine

// module1_refusalreason.go — module 1's registered refusal reasons
// (refusalreason.go): its origination routes' refusals before the request
// reaches the Hub.
const (
	reasonNoPHG         refusalReason = msgNoPHG
	reasonUnknownMember refusalReason = "unknown member"
)

// moduleOneRefusalReasons are module 1's registered reasons.
func moduleOneRefusalReasons() []refusalReason {
	return []refusalReason{reasonNoPHG, reasonUnknownMember}
}
