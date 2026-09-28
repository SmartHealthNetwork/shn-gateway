package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
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

// QualifyValidatorLane is the qualification a validator lane passes before it
// is used: it waits only for a bounded FHIR metadata response, then posts the
// line's single finite readiness corpus, which also absorbs a lane's first,
// still-warming answers. Metadata availability never grants lane readiness. It
// is a LaneQualifier.
func QualifyValidatorLane(ctx context.Context, base, line string) error {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	// The last metadata attempt's outcome, so a lane that never answers fails
	// naming why (its name does not resolve, it refuses), not only that time ran out.
	var last error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimRight(base, "/")+"/metadata", nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := client.Do(req)
		if err != nil && ctx.Err() == nil {
			// Kept only while the budget runs: the attempt the budget cuts off
			// says nothing about the lane.
			last = err
		}
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var metadata struct {
					ResourceType string `json:"resourceType"`
					FHIRVersion  string `json:"fhirVersion"`
				}
				if readErr != nil || len(body) > 4<<20 || json.Unmarshal(body, &metadata) != nil || metadata.ResourceType != "CapabilityStatement" || !strings.HasPrefix(metadata.FHIRVersion, "4.0.") {
					cancel()
					return lanequalify.ErrNotR4Metadata
				}
				cancel()
				if err := lanequalify.Warm(ctx, strings.TrimRight(base, "/"), line, nil); err != nil {
					return fmt.Errorf("%w: %w", lanequalify.ErrCorpus, err)
				}
				return nil
			}
			last = &lanequalify.MetadataStatusError{Status: resp.StatusCode}
		}
		cancel()
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			if last != nil {
				return fmt.Errorf("metadata unavailable: %w: %w", last, ctx.Err())
			}
			return fmt.Errorf("metadata unavailable: %w", ctx.Err())
		case <-timer.C:
		}
	}
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
