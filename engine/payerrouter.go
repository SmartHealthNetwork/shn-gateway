package engine

import (
	"encoding/json"
	"fmt"
	"os"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// PayerRouter resolves a Coverage.payor identity to a substrate holder id. ok=false ⇒ no mapping
// ⇒ the caller FAILS CLOSED. There is no default holder (AI-G11 / OWD-G10).
type PayerRouter interface {
	Resolve(p shnsdk.PayerIdentifier) (holderID string, ok bool)
}

// PayerDirectoryEntry is one row of the PAYER_DIRECTORY file.
type PayerDirectoryEntry struct {
	System   string `json:"system"`
	Value    string `json:"value"`
	HolderID string `json:"holderId"`
}

type payerKey struct{ system, value string }

// ConfigPayerRouter is the static resolver: a provider-maintained map. (FeedPayerRouter,
// network discovery, is the feed-derived resolver behind this same interface.)
type ConfigPayerRouter struct{ m map[payerKey]string }

// NewConfigPayerRouter builds a router; a duplicate (system,value) is a hard error (ambiguous routing).
func NewConfigPayerRouter(entries []PayerDirectoryEntry) (*ConfigPayerRouter, error) {
	m := make(map[payerKey]string, len(entries))
	for _, e := range entries {
		if e.System == "" || e.Value == "" || e.HolderID == "" {
			return nil, fmt.Errorf("payer directory: entry missing system/value/holderId: %+v", e)
		}
		k := payerKey{e.System, e.Value}
		if _, dup := m[k]; dup {
			return nil, fmt.Errorf("payer directory: duplicate mapping for %s|%s (ambiguous routing)", e.System, e.Value)
		}
		m[k] = e.HolderID
	}
	return &ConfigPayerRouter{m: m}, nil
}

func (r *ConfigPayerRouter) Resolve(p shnsdk.PayerIdentifier) (string, bool) {
	h, ok := r.m[payerKey{p.System, p.Value}]
	return h, ok
}

// LoadPayerDirectory reads a JSON array of entries from path (the PAYER_DIRECTORY file).
func LoadPayerDirectory(path string) ([]PayerDirectoryEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("payer directory: read %s: %w", path, err)
	}
	var entries []PayerDirectoryEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("payer directory: parse %s: %w", path, err)
	}
	return entries, nil
}

// FeedPayerRouter is the feed-derived resolver: it indexes the live converged /holders registry
// (operator-attested payer-id claims, FR-G41) to map a Coverage.payor identity → payer holder id.
// It resolves fresh on every call, so a post-boot feed registration/rotation is visible without a
// restart (the FR-G10 no-restart property). Ambiguity — a (system,value) claimed by more than one
// holder — FAILS CLOSED (ok=false), the same defense the DB UNIQUE(system,value) enforces (AI-G12).
// It considers ONLY role=payer entries: payer-ids on a non-payer entry (a manifest-seeded holder
// that bypasses the registrar role-gate) are never routed to — the router self-fails-closed on role
// rather than trusting the upstream 400-gate (authority gates on holder_role==payer).
type FeedPayerRouter struct{ reg shnsdk.Registry }

// NewFeedPayerRouter builds a router backed by reg (a value type whose internals are shared; the
// router sees live poller updates).
func NewFeedPayerRouter(reg shnsdk.Registry) *FeedPayerRouter { return &FeedPayerRouter{reg: reg} }

func (r *FeedPayerRouter) Resolve(p shnsdk.PayerIdentifier) (string, bool) {
	hit := ""
	for _, id := range r.reg.IDs() {
		e, ok := r.reg.Lookup(id)
		if !ok || e.Role != "payer" { // role filter: only payer holders route (AI-G12 defense-in-depth)
			continue
		}
		for _, pid := range e.PayerIDs {
			if pid == p {
				if hit != "" && hit != id {
					return "", false // ambiguous across holders → fail closed (AI-G12)
				}
				hit = id
			}
		}
	}
	if hit == "" {
		return "", false
	}
	return hit, true
}
