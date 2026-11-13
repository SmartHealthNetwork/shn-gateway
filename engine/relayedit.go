package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// relayEdit declares one registered edit (relay.Edit). The registry is
// relayEdits, gathered from the compiled-in workstreams (workstreams.go).
type relayEdit = relay.Edit

// RelayEditName returns the registered name of the edit id names (E-01 is
// "cds-callback-strip"), and whether the registry holds it.
//
// Deprecated: use relay.EditName over the workstream's edit rows (package
// github.com/SmartHealthNetwork/shn-gateway/engine/relay; the PA rows are
// paedits.Rows, package github.com/SmartHealthNetwork/shn-gateway/pa/paedits).
func RelayEditName(id relay.EditID) (string, bool) {
	return relay.EditName(relayEdits, id)
}

// RelayEditReceivedBy names the party of a leg whose received bytes the edit
// id names changes, and whether the registry holds it.
//
// Deprecated: use relay.EditReceivedBy over the workstream's edit rows
// (package github.com/SmartHealthNetwork/shn-gateway/engine/relay; the PA rows
// are paedits.Rows, package github.com/SmartHealthNetwork/shn-gateway/pa/paedits).
func RelayEditReceivedBy(id relay.EditID) (relay.Role, bool) {
	return relay.EditReceivedBy(relayEdits, id)
}

// headerEdited is relay.HeaderEdited over the gateway's own registry
// (relayEdits): the header id's row sets, its value checked against in by the
// core's check of the edit, by its id, never one the row or the caller
// supplies. It is the one
// production call of relay.HeaderEdited (TestHeaderEditedIsCalledOnlyFromTheRegistryWrapper).
func headerEdited(src relay.Payload, id relay.EditID, header, value string, in relay.HeaderInputs) (relay.Payload, error) {
	return relay.HeaderEdited(relayEdits, src, id, header, value, in)
}
