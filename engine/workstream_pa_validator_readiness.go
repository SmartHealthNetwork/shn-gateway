//go:build !shn_coreonly

// workstream_pa_validator_readiness.go — the PA workstream's test hook over
// its PAS routing lanes.

package engine

// ValidatorReadinessForTest snapshots the existing PAS routing lanes without
// qualification or validation work. It is independent of source evidence.
func (g *Gateway) ValidatorReadinessForTest() map[string]bool {
	out := map[string]bool{}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		v := g.validatorForContractLine("pa.pas", line)
		ready := v != nil
		if r, ok := v.(interface{ Ready() bool }); ok {
			ready = r.Ready()
		}
		out[line] = ready
	}
	return out
}
