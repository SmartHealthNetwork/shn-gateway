package app

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// certificationWarmAttempts bounds how many requests the boot warm-up sends one
// certification endpoint.
const certificationWarmAttempts = 3

// warmCertification sends each certification endpoint it may dial without a
// qualification of its own (the 2.0 validator) one PAS request bundle at its
// line, against the versioned profile a
// certification of that bundle names, so a new gateway process's first
// contact with a validator is not paid inside a real certification's
// candidate budget. It runs off the request path (startWorkers), never gates
// readiness, and records no evidence: it holds only the validator map, uses its
// own client built with the certification client's limits (never the engine's,
// whose one connection a real certification may need), stops at an endpoint's
// first answer, and logs one line per endpoint naming its host, whether it
// answered, and after how many requests. A gated lane (a default, or a 2.1 or
// 2.2 address) is not warmed here: its qualification already posts the
// readiness corpus to it.
func warmCertification(ctx context.Context, validators map[string]shnsdk.Validator) {
	lines := make([]string, 0, len(validators))
	for line := range validators {
		lines = append(lines, line)
	}
	sort.Strings(lines)
	var wg sync.WaitGroup
	for _, line := range lines {
		configured, ok := validators[line].(*shnsdk.OperationValidator)
		if !ok {
			continue
		}
		body, profile, ok := lanequalify.CertificationRow(line)
		if !ok {
			continue
		}
		wg.Go(func() { warmCertificationEndpoint(ctx, line, configured.BaseURL, body, profile) })
	}
	wg.Wait()
}

func warmCertificationEndpoint(ctx context.Context, line, base string, body []byte, profile string) {
	client := certificationClient(base)
	defer client.Client.CloseIdleConnections()
	started := time.Now()
	state, attempts := "unanswered", 0
	for attempts < certificationWarmAttempts {
		if ctx.Err() != nil {
			state = "stopped"
			break
		}
		attempts++
		// Any answer is the warm-up's result, whatever its verdict: the verdict
		// and any error text stay out of the log (validator bytes).
		if _, err := client.Validate(ctx, body, profile); err == nil {
			state = "answered"
			break
		}
	}
	event, _ := json.Marshal(struct {
		Version    int     `json:"version"`
		Line       string  `json:"line"`
		Host       string  `json:"host"`
		State      string  `json:"state"`
		Attempts   int     `json:"attempts"`
		DurationMS float64 `json:"duration_ms"`
	}{1, line, lanequalify.Host(base), state, attempts, float64(time.Since(started)) / float64(time.Millisecond)})
	log.Printf("gateway: certification_warm %s", event)
}
