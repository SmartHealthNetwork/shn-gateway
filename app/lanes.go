package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const defaultLaneStartupBudget = 600 * time.Second

// defaultValidatorLanesNone is FHIR_DEFAULT_VALIDATOR_LANES's one value: this
// gateway's network has no Compose default validator services, so it never
// probes their names. Unset keeps the default lanes.
const defaultValidatorLanesNone = "none"

// laneManager owns every default qualification worker for one gateway lifecycle.
type laneManager struct {
	defaults  map[string]*engine.DiscoveredLane
	fallbacks map[string]bool
	cancel    context.CancelFunc
	workers   sync.WaitGroup
}

func (m *laneManager) Close() {
	if m != nil {
		m.cancel()
		m.workers.Wait()
	}
}

func checkValidatorLaneURL(name, base string) error {
	if err := checkOptionalURL(name, base); err != nil {
		return err
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("invalid %s %q: validator endpoint requires a host", name, base)
	}
	return nil
}

func discoverValidatorLanes(ctx context.Context, getenv func(string) string, declared []string, canonical shnsdk.Validator, cfg config, resolve func(string) string, qualify engine.LaneQualifier) (map[string]shnsdk.Validator, *laneManager, error) {
	startup, cancel := context.WithTimeout(ctx, defaultLaneStartupBudget)
	m := &laneManager{defaults: map[string]*engine.DiscoveredLane{}, fallbacks: map[string]bool{}, cancel: cancel}
	fail := func(err error) (map[string]shnsdk.Validator, *laneManager, error) { m.Close(); return nil, nil, err }
	linesPerContract := map[string]map[string]bool{}
	for _, tok := range shnsdk.NativeContractVersions() {
		contract, line, _ := strings.Cut(tok, "@")
		if linesPerContract[contract] == nil {
			linesPerContract[contract] = map[string]bool{}
		}
		linesPerContract[contract][line] = true
	}
	required := map[string]bool{}
	for _, tok := range declared {
		contract, line, _ := strings.Cut(tok, "@")
		if len(linesPerContract[contract]) > 1 {
			required[line] = true
		}
	}
	supplied := cfg
	for _, entry := range []struct {
		line  string
		value *string
	}{{"2.1", &supplied.FHIRValidateURL21}, {"2.2", &supplied.FHIRValidateURL22}} {
		if *entry.value != "" {
			if err := checkValidatorLaneURL("FHIR_VALIDATE_URL_"+strings.ReplaceAll(entry.line, ".", "_"), *entry.value); err != nil {
				return fail(err)
			}
			continue
		}
		if getenv("SHN_FAKE_VALIDATOR") == "1" {
			continue
		}
		if cfg.DefaultValidatorLanes == defaultValidatorLanesNone {
			// A network without the Compose validator services (a hosted tenant):
			// no default lane exists to probe, so none is created. A declared line
			// then needs its FHIR_VALIDATE_URL_<line> (validatorLanesForDeclared).
			continue
		}
		base := resolve(entry.line)
		if err := checkValidatorLaneURL("default validator "+entry.line, base); err != nil {
			return fail(err)
		}
		m.defaults[entry.line] = engine.NewDiscoveredLane(entry.line, base, shnsdk.NewOperationValidator(base))
		*entry.value = base
	}
	lanes, err := validatorLanesForDeclared(getenv, declared, canonical, supplied)
	if err != nil {
		return fail(err)
	}
	for line := range m.defaults {
		delete(lanes, line)
	}
	// A single-line contract's line (pa.pdex@2.1) rides the canonical validator
	// only as a fallback alias, which the engine never lends to a multi-line
	// contract at that line. With default lanes the line's default was removed
	// above; with none, validatorLanesForDeclared has already aliased it, and it
	// must be marked the same way, or PA traffic at that line would validate
	// against the canonical IG.
	canonicalLine := shnsdk.LineOf(shnsdk.ContractPAPAS20)
	explicit := map[string]string{"2.1": cfg.FHIRValidateURL21, "2.2": cfg.FHIRValidateURL22}
	aliasOnly := func(line string) bool {
		return cfg.DefaultValidatorLanes == defaultValidatorLanesNone && getenv("SHN_FAKE_VALIDATOR") != "1" && line != canonicalLine && explicit[line] == ""
	}
	for _, tok := range declared {
		contract, line, _ := strings.Cut(tok, "@")
		if len(linesPerContract[contract]) < 2 && (lanes[line] == nil || aliasOnly(line)) {
			lanes[line] = canonical
			m.fallbacks[line] = true
		}
	}
	qualifyLane := func(line string, d *engine.DiscoveredLane) error {
		started := time.Now()
		emit := func(state, reason string) {
			// Only the synthetic default's identity, the host it dials, lifecycle
			// timing and a failure's kind are emitted; validator outcomes and error
			// text never enter this serialized log path (lanequalify.FailureReason).
			event, _ := json.Marshal(struct {
				Version    int     `json:"version"`
				Line       string  `json:"line"`
				Base       string  `json:"base"`
				Host       string  `json:"host"`
				State      string  `json:"state"`
				Reason     string  `json:"reason,omitempty"`
				At         string  `json:"at"`
				DurationMS float64 `json:"duration_ms"`
			}{1, line, resolve(line), lanequalify.Host(resolve(line)), state, reason, time.Now().UTC().Format(time.RFC3339Nano), float64(time.Since(started)) / float64(time.Millisecond)})
			log.Printf("gateway: validator_qualification %s", event)
		}
		emit("started", "")
		err := d.Qualify(startup, qualify)
		state := "ready"
		if err != nil {
			state = "failed"
		}
		emit(state, lanequalify.FailureReason(err))
		return err
	}
	// Declared defaults finish before admission. Explicit overrides retain their
	// existing URL-only startup behavior and never enter this qualification path.
	for _, line := range []string{"2.1", "2.2"} {
		d := m.defaults[line]
		if d == nil || !required[line] {
			continue
		}
		if err := qualifyLane(line, d); err != nil {
			return fail(fmt.Errorf("gateway: declared validator line %s at %s failed qualification: %w", line, resolve(line), err))
		}
	}
	for line, d := range m.defaults {
		if required[line] {
			continue
		}
		m.workers.Add(1)
		go func() { defer m.workers.Done(); _ = qualifyLane(line, d) }()
	}
	return lanes, m, nil
}

// qualifyDefaultLane waits only for a bounded FHIR metadata response before the
// single finite corpus. Metadata availability never grants lane readiness.
func qualifyDefaultLane(ctx context.Context, base, line string) error {
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
