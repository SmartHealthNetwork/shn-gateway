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
	for _, tok := range NativeContractVersions() {
		c, l, ok := strings.Cut(tok, "@")
		if ok && c == contract {
			lines[l] = true
		}
	}
	return len(lines)
}

// validatorForContractLine resolves the $validate lane for contract at line
// (line "" = no line in play: the canonical Validator). The lane is the one
// configured for that contract line exactly (Config.ValidatorsByContractLine):
// a discovered lane serves once it has qualified. A deployment with no lanes
// configured serves every contract line from Config.Validator; one with lanes
// is authoritative, so a contract line it did not configure (another line, or
// an unknown contract) is unlaned and resolves to nil, and its callers fail
// closed rather than validating one IG version's bytes against another's.
func (g *Gateway) validatorForContractLine(contract, line string) shnsdk.Validator {
	if line == "" {
		return g.cfg.Validator
	}
	v := g.cfg.ValidatorsByContractLine[contract+"@"+line]
	if d, ok := v.(*DiscoveredLane); ok {
		return g.readyLane(d)
	}
	if v != nil {
		return v
	}
	if len(g.cfg.ValidatorsByContractLine) > 0 {
		return nil
	}
	return g.cfg.Validator
}

// deriveContractLanes gives a caller that still sets the deprecated per-line
// lanes, and not ValidatorsByContractLine, the contract-line lanes they name.
func deriveContractLanes(cfg *Config) {
	if len(cfg.ValidatorsByContractLine) == 0 && (len(cfg.ValidatorsByLine) > 0 || len(cfg.DefaultValidatorsByLine) > 0 || len(cfg.CanonicalFallbackLines) > 0) {
		cfg.ValidatorsByContractLine = ContractLineLanes(cfg.ValidatorsByLine, cfg.DefaultValidatorsByLine, cfg.CanonicalFallbackLines, cfg.DeclaredContractVersions)
	}
}

// ContractLineLanes derives contract-line lanes (Config.ValidatorsByContractLine)
// from per-line ones: byLine's explicit lanes, defaults' discovered lanes, and
// the lines on which byLine's lane is the canonical validator lent to a
// single-line contract (fallbacks). For every contract this gateway builds
// natively (NativeContractVersions, never the SDK's set) or declared names, at
// every line any of them names:
//   - at a fallback line, a single-line contract borrows byLine's lane, and a
//     multi-line contract has only the line's discovered lane, never the
//     canonical one;
//   - at any other line, an explicit lane wins over a discovered one.
func ContractLineLanes(byLine map[string]shnsdk.Validator, defaults map[string]*DiscoveredLane, fallbacks map[string]bool, declared []string) map[string]shnsdk.Validator {
	contracts := map[string]bool{}
	for _, tok := range append(NativeContractVersions(), declared...) {
		if c, _, ok := strings.Cut(tok, "@"); ok {
			contracts[c] = true
		}
	}
	lines := map[string]bool{}
	for l := range byLine {
		lines[l] = true
	}
	for l := range defaults {
		lines[l] = true
	}
	for l := range fallbacks {
		lines[l] = true
	}
	out := map[string]shnsdk.Validator{}
	for c := range contracts {
		for l := range lines {
			key := c + "@" + l
			switch {
			case fallbacks[l] && nativeContractLineCount(c) < 2:
				// The canonical validator lent to a single-line contract: nil
				// when byLine names none, which still makes the deployment
				// laned (it serves nothing there).
				out[key] = byLine[l]
			case fallbacks[l]:
				// A multi-line contract at an alias line has only the line's
				// discovered lane: nil when there is none.
				if d := defaults[l]; d != nil {
					out[key] = d
				} else {
					out[key] = nil
				}
			case byLine[l] != nil:
				out[key] = byLine[l]
			case defaults[l] != nil:
				out[key] = defaults[l]
			default:
				// A line byLine names with no validator still makes the
				// deployment laned: it serves nothing there.
				if _, named := byLine[l]; named {
					out[key] = nil
				}
			}
		}
	}
	return out
}

// servesLine reports whether this gateway builds and answers at contract's
// line: a validator lane serves it, or no payload check consumes lanes here and
// the gateway declares it. A gateway whose payload checks consume no lane needs
// no validator to answer or continue at a line it declares (FR-G29). Where they
// do, a gateway refuses to boot with a declared line it has no lane for, so this
// is the lane alone there. Native reach and transform chains do not read it:
// a chain needs a lane, so a gateway with no validator never builds a bridged
// transform, and native reach needs one unless no payload check consumes lanes
// and the message's builder is certified at the line (buildsAt).
func (g *Gateway) servesLine(contract, line string) bool {
	if g.validatorForContractLine(contract, line) != nil {
		return true
	}
	return !payloadChecksConsumeLanes(g.cfg.ConformanceEnforcement) && contractLineSet(g.declaredContractVersions(), contract)[line]
}

// payloadChecksConsumeLanes reports whether this gateway's payload conformance
// checks use its validator lanes at all: every level but none, which checks no
// payload. It is the one place a lane decision reads the conformance level.
// The other consumers of a lane: native reach builds at a peer's line where a
// lane serves it, or, where this predicate is false, for a builder certified at
// that line (buildsAt); module 2's Certify needs one for any chain it plans, and
// E-08 reads one for the answer's source line, at every level.
func payloadChecksConsumeLanes(level ConformanceEnforcement) bool {
	return level != EnforcementNone
}

// readyLane is a discovered lane once it has qualified, observed when the
// gateway has an observer; nil while it has not.
func (g *Gateway) readyLane(d *DiscoveredLane) shnsdk.Validator {
	if d == nil || !d.Ready() {
		return nil
	}
	if g.cfg.Observer != nil {
		return observingValidator{inner: d, g: g}
	}
	return d
}
