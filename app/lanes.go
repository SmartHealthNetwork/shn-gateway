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
	if cfg.ConformanceEnforcement == engine.EnforcementNone {
		return map[string]shnsdk.Validator{}, m, nil
	}
	fail := func(err error) (map[string]shnsdk.Validator, *laneManager, error) { m.Close(); return nil, nil, err }
	linesPerContract := map[string]map[string]bool{}
	for _, tok := range shnsdk.NativeContractVersions() {
		contract, line, _ := strings.Cut(tok, "@")
		if linesPerContract[contract] == nil {
			linesPerContract[contract] = map[string]bool{}
		}
		linesPerContract[contract][line] = true
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
	for _, tok := range declared {
		contract, line, _ := strings.Cut(tok, "@")
		if len(linesPerContract[contract]) < 2 && lanes[line] == nil {
			lanes[line] = canonical
			m.fallbacks[line] = true
		}
	}
	qualifyLane := func(line string, d *engine.DiscoveredLane) error {
		started := time.Now()
		emit := func(state string) {
			// Only the synthetic default's identity and lifecycle timing are emitted;
			// validator outcomes and error text never enter this serialized log path.
			event, _ := json.Marshal(struct {
				Version    int     `json:"version"`
				Line       string  `json:"line"`
				Base       string  `json:"base"`
				State      string  `json:"state"`
				At         string  `json:"at"`
				DurationMS float64 `json:"duration_ms"`
			}{1, line, resolve(line), state, time.Now().UTC().Format(time.RFC3339Nano), float64(time.Since(started)) / float64(time.Millisecond)})
			log.Printf("gateway: validator_qualification %s", event)
		}
		emit("started")
		err := d.Qualify(startup, qualify)
		state := "ready"
		if err != nil {
			state = "failed"
		}
		emit(state)
		return err
	}
	// Qualification is optional background work at every level. A strict
	// operation refuses only when its required checker is unavailable.
	for line, d := range m.defaults {
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
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimRight(base, "/")+"/metadata", nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := client.Do(req)
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
					return fmt.Errorf("metadata is not an R4 CapabilityStatement")
				}
				cancel()
				return lanequalify.Warm(ctx, strings.TrimRight(base, "/"), line, nil)
			}
		}
		cancel()
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("metadata unavailable: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
