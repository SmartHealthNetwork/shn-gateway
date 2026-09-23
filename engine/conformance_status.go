package engine

import (
	"context"
	"encoding/json"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"io"
	"net/http"
	"sync"
	"time"
)

// ConformanceStatus is PHI-free operational capability metadata.
// It never certifies a message or makes a readiness/routing decision.
type ConformanceStatus struct {
	Level        string            `json:"level"`
	RuleSet      string            `json:"ruleSet"`
	Availability map[string]string `json:"availability"`
	// Dropped counts conformance observation jobs only, not finding delivery or inspection loss.
	Dropped uint64 `json:"dropped"`
}

var conformanceCategories = [...]string{"structural", "builtInDeep", "patientConsistency", "profile", "terminology"}

// UnavailableConformanceStatus represents an unknown/old/down gateway, not none.
func UnavailableConformanceStatus() ConformanceStatus {
	s := ConformanceStatus{Availability: make(map[string]string, len(conformanceCategories))}
	for _, category := range conformanceCategories {
		s.Availability[category] = "unavailable"
	}
	return s
}

// ConformanceStatus reads bounded local snapshots only: no callbacks, I/O,
// validation, payloads or finding copies. Recent execution demonstrates only partial coverage; a successful message
// never proves full profile/terminology coverage. Qualification is not liveness. Per-message applicability can still be unknown.
func (g *Gateway) ConformanceStatus() ConformanceStatus {
	s := UnavailableConformanceStatus()
	s.Level, s.RuleSet = g.policy().Level().String(), ConformanceRuleSet
	if g.policy().Level() == EnforcementNone {
		for _, category := range conformanceCategories {
			s.Availability[category] = "disabled"
		}
		return s
	}
	s.Availability["structural"], s.Availability["builtInDeep"] = "available", "available"
	if g.cfg.SubjectReferenceResolver != nil {
		s.Availability["patientConsistency"] = "available"
	}
	now := g.checkerNow()
	g.checkerAvailability.mu.Lock()
	for _, row := range g.checkerAvailability.rows {
		age := now.Sub(row.at)
		if row.at.IsZero() || age < 0 || age >= checkerAvailabilityFreshness {
			continue
		}
		if executedCheck(row.profile) {
			s.Availability["profile"] = "partial"
		}
		if executedCheck(row.terminology) {
			s.Availability["terminology"] = "partial"
		}
	}
	g.checkerAvailability.mu.Unlock()
	if w := g.certification; w != nil {
		w.mu.Lock()
		s.Dropped = w.dropped
		w.mu.Unlock()
	}
	return s
}

// FetchConformanceStatus reads only the closed informational health object.
// Callers authenticate/authorize their participant before supplying its trusted
// gateway URL. All failure modes return unavailable; no error body is forwarded.
func FetchConformanceStatus(ctx context.Context, client *http.Client, base string) ConformanceStatus {
	unavailable := UnavailableConformanceStatus()
	if base == "" {
		return unavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return unavailable
	}
	if client == nil {
		client = http.DefaultClient
	}
	hc := *client
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := hc.Do(req)
	if err != nil {
		return unavailable
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return unavailable
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (64<<10)+1))
	if err != nil || len(b) > 64<<10 {
		return unavailable
	}
	var envelope struct {
		Conformance *ConformanceStatus `json:"conformance"`
	}
	if json.Unmarshal(b, &envelope) != nil || envelope.Conformance == nil {
		return unavailable
	}
	s := envelope.Conformance
	level, err := ParseConformanceEnforcement(s.Level)
	if err != nil || s.Level == "" || (s.RuleSet != ConformanceRuleSet && s.RuleSet != "participant-conformance/4" && s.RuleSet != "participant-conformance/3" && s.RuleSet != "participant-conformance/2" && s.RuleSet != "participant-conformance/1") {
		return unavailable
	}
	out := UnavailableConformanceStatus()
	out.Level, out.RuleSet, out.Dropped = s.Level, s.RuleSet, s.Dropped
	for _, category := range conformanceCategories {
		v := s.Availability[category]
		if level == EnforcementNone {
			if v != "disabled" {
				return unavailable
			}
		} else {
			switch v {
			case "available", "partial", "unavailable":
			default:
				return unavailable
			}
		}
		out.Availability[category] = v
	}
	return out
}

// A finite observation window bounds stale execution evidence. This is recent
// demonstrated coverage, never liveness or qualification of an entire IG line.
const checkerAvailabilityFreshness = 5 * time.Minute

var checkerScopes = [...]string{"pa.crd@2.0", "pa.crd@2.1", "pa.crd@2.2", "pa.dtr@2.0", "pa.dtr@2.1", "pa.dtr@2.2", "pa.pas@2.0", "pa.pas@2.1", "pa.pas@2.2"}

type checkerAvailabilityRow struct {
	at                   time.Time
	profile, terminology shnsdk.ValidationState
}
type checkerAvailability struct {
	mu   sync.Mutex
	rows [len(checkerScopes)]checkerAvailabilityRow
}

func executedCheck(s shnsdk.ValidationState) bool {
	return s == shnsdk.ValidationValid || s == shnsdk.ValidationInvalid
}
func (g *Gateway) checkerNow() time.Time {
	if g.cfg.Clock != nil {
		return g.cfg.Clock()
	}
	return time.Now()
}
func (g *Gateway) recordCheckerAvailability(contract, line string, ev shnsdk.ValidationEvidence) {
	if !ev.ExecutionAttempted || g.policy().Level() == EnforcementNone {
		return
	}
	for i, scope := range checkerScopes {
		if scope == contract+"@"+line {
			row := checkerAvailabilityRow{at: g.checkerNow(), profile: ev.Profile.State, terminology: ev.Terminology.State}
			g.checkerAvailability.mu.Lock()
			// A delayed recorder cannot replace a more recent execution result.
			if !row.at.Before(g.checkerAvailability.rows[i].at) {
				g.checkerAvailability.rows[i] = row
			}
			g.checkerAvailability.mu.Unlock()
			return
		}
	}
}
