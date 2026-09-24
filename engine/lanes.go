package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// DefaultLaneURL is the Compose service address for a native validator line.
func DefaultLaneURL(line string) string {
	return "http://shn-validator-" + strings.ReplaceAll(line, ".", "-") + ":8080/fhir"
}

// LaneQualifier proves a validator's finite synthetic corpus for one line.
type LaneQualifier func(context.Context, string, string) error

// DiscoveredLane admits a default endpoint only after one successful qualification.
// Its manager supplies the lifecycle context; ordinary validation never changes readiness.
type DiscoveredLane struct {
	line, base string
	validator  shnsdk.Validator
	ready      atomic.Bool
	once       sync.Once
	err        error
}

func NewDiscoveredLane(line, base string, validator shnsdk.Validator) *DiscoveredLane {
	return &DiscoveredLane{line: line, base: base, validator: validator}
}

// Ready is a local atomic snapshot with no network activity.
func (d *DiscoveredLane) Ready() bool { return d.ready.Load() }

// Base is the endpoint the lane qualifies.
func (d *DiscoveredLane) Base() string { return d.base }

func (d *DiscoveredLane) Qualify(ctx context.Context, q LaneQualifier) error {
	d.once.Do(func() {
		if d.err = ctx.Err(); d.err != nil {
			return
		}
		d.err = q(ctx, d.base, d.line)
		if d.err == nil {
			d.err = ctx.Err()
		}
		if d.err == nil {
			d.ready.Store(true)
		}
	})
	return d.err
}

func (d *DiscoveredLane) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	if !d.Ready() {
		return shnsdk.Result{}, fmt.Errorf("validator line %s has not qualified", d.line)
	}
	return d.validator.Validate(ctx, body, profile)
}

func nativeContractLineCount(contract string) int {
	lines := map[string]bool{}
	for _, tok := range shnsdk.NativeContractVersions() {
		c, l, ok := strings.Cut(tok, "@")
		if ok && c == contract {
			lines[l] = true
		}
	}
	return len(lines)
}

// validatorForContractLine preserves single-contract canonical compatibility
// without treating that alias as a configured multi-line validator.
func (g *Gateway) validatorForContractLine(contract, line string) shnsdk.Validator {
	if line == "" {
		return g.cfg.Validator
	}
	if nativeContractLineCount(contract) < 2 && g.cfg.CanonicalFallbackLines[line] {
		return g.cfg.ValidatorsByLine[line]
	}
	if g.cfg.CanonicalFallbackLines[line] {
		return g.qualifiedDefault(line)
	}
	return g.validatorForLine(line)
}

func (g *Gateway) qualifiedDefault(line string) shnsdk.Validator {
	d := g.cfg.DefaultValidatorsByLine[line]
	if d == nil || !d.Ready() {
		return nil
	}
	if g.cfg.Observer != nil {
		return observingValidator{inner: d, g: g}
	}
	return d
}

func (g *Gateway) adaptationValidator(contract, line string) shnsdk.Validator {
	if v := g.validatorForContractLine(contract, line); v != nil {
		return v
	}
	if g.cfg.AdaptationValidator != nil {
		return g.cfg.AdaptationValidator(contract, line)
	}
	return nil
}
