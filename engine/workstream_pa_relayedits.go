//go:build !shn_coreonly

// workstream_pa_relayedits.go — the PA workstream's registered relay edits
// (E-01 … E-09), the rows of package paedits. The registry, its kinds and its
// closed-set check are core (engine/relay), and the core gathers the rows
// (workstreams.go).
package engine

import "github.com/SmartHealthNetwork/shn-gateway/pa/paedits"

// paRelayEdits are the PA workstream's registered edits, in id order: every
// edit the network registers is on a PA leg today. The core gathers them into
// the closed registry (relayEdits).
var paRelayEdits = paedits.Rows()
