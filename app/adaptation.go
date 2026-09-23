package app

import (
	"context"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The factory owns no client or worker until an explicit adaptation asks for
// proof support. Native none traffic never invokes it. Default endpoints still
// require finite qualification, performed under the adapting operation's bound.
func adaptationValidatorFactory(cfg config, canonical string, fake bool, create func(string) shnsdk.Validator) func(string, string) shnsdk.Validator {
	var mu sync.Mutex
	clients := map[string]shnsdk.Validator{}
	return func(contract, line string) shnsdk.Validator {
		if contract == "" || (line != "2.0" && line != "2.1" && line != "2.2") {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if v := clients[line]; v != nil {
			return v
		}
		if fake {
			clients[line] = engine.NewLineFakeValidator(line)
			return clients[line]
		}
		base := map[string]string{"2.0": canonical, "2.1": cfg.FHIRValidateURL21, "2.2": cfg.FHIRValidateURL22}[line]
		if base == "" {
			if line == "2.0" {
				return nil
			}
			base = engine.DefaultLaneURL(line)
			clients[line] = &adaptationDefault{lane: engine.NewDiscoveredLane(line, base, create(base))}
		} else {
			clients[line] = create(base)
		}
		return clients[line]
	}
}

type adaptationDefault struct{ lane *engine.DiscoveredLane }

func (v *adaptationDefault) qualify(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	return v.lane.Qualify(ctx, qualifyDefaultLane)
}
func (v *adaptationDefault) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	if err := v.qualify(ctx); err != nil {
		return shnsdk.Result{}, err
	}
	return v.lane.Validate(ctx, body, profile)
}
func (v *adaptationDefault) ValidateEvidence(ctx context.Context, body []byte, profile string) (shnsdk.ValidationEvidence, error) {
	if err := v.qualify(ctx); err != nil {
		return shnsdk.ValidationEvidence{}, err
	}
	return v.lane.ValidateEvidence(ctx, body, profile)
}
