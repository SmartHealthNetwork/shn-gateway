//go:build shn_toyworkstream

package relay

// profileOwnership is the ownership rows the toy test profile adds: its one
// leg carried as each participant sent it, both ways, beside the refusal and
// relayed application error rows every leg has.
func profileOwnership() map[Key]Rule {
	relayed := Rule{Allowed: []Ownership{OwnershipRelayed}}
	refusal := Rule{Allowed: []Ownership{OwnershipAuthored}, Builders: []BuilderID{BuilderGatewayRefusal}}
	const leg = "toy-echo"
	return map[Key]Rule{
		{leg, RoleRequester, DirectionRequest, OutcomeCarried}:        relayed,
		{leg, RoleRecipient, DirectionRequest, OutcomeCarried}:        relayed,
		{leg, RoleRecipient, DirectionResponse, OutcomeAnswered}:      relayed,
		{leg, RoleRequester, DirectionResponse, OutcomeAnswered}:      relayed,
		{leg, RoleRequester, DirectionResponse, OutcomeUpstreamError}: relayed,
		{leg, RoleRecipient, DirectionResponse, OutcomeUpstreamError}: relayed,
		{leg, RoleRecipient, DirectionResponse, OutcomeRefused}:       refusal,
		{leg, RoleRequester, DirectionResponse, OutcomeRefused}:       refusal,
	}
}
