//go:build !shn_coreonly

package app

import (
	"encoding/json"
	"fmt"
	"slices"
)

// wireVersionSent is the wire protocol version this gateway sends: it names
// each leg by its workstream-prefixed name, 2.0.0's. It is the gateway's own
// value, not shnsdk.WireProtocolVersion, which is the version of whichever
// shn-sdk the module compiles against.
const wireVersionSent = "2.0.0"

// checkWireAccepted is the start check: the gateway starts only on a network
// whose discovery accepts wireVersionSent, by list membership, never a
// version comparison or a text match. A network that does not would refuse
// what it sends, and the gateway does not fall back to flat names.
//
// The list is the descriptor's wireProtocolVersionsAccepted, read under
// PARTICIPANT_PROTOCOL.md §1a: "Additive: absent means `wireProtocolVersion`
// alone." So an absent key is [wireProtocolVersion], while a stated list, an
// empty or null one included, is that list. Both are read from raw, the
// descriptor as served, by their exact keys. A descriptor that is not an
// object, or states neither, says nothing the gateway can confirm, and it
// does not start.
func checkWireAccepted(raw json.RawMessage, discoveryURL string) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil || keys == nil {
		return wireUnknown(fmt.Errorf("the descriptor is not a JSON object"))
	}
	var accepted []string
	if list, stated := keys["wireProtocolVersionsAccepted"]; stated {
		if err := json.Unmarshal(list, &accepted); err != nil {
			return wireUnknown(fmt.Errorf("wireProtocolVersionsAccepted: %w", err))
		}
	} else {
		var v string
		if spoken, ok := keys["wireProtocolVersion"]; ok {
			if err := json.Unmarshal(spoken, &v); err != nil {
				return wireUnknown(fmt.Errorf("wireProtocolVersion: %w", err))
			}
		}
		if v == "" {
			return wireUnknown(fmt.Errorf("the descriptor states neither wireProtocolVersionsAccepted nor wireProtocolVersion"))
		}
		accepted = []string{v}
	}
	if slices.Contains(accepted, wireVersionSent) {
		return nil
	}
	return fmt.Errorf("gateway: the network at %s does not accept wire protocol %s (it accepts %q); this gateway sends %s leg names and does not start. Point it at a network whose discovery accepts %s, or run a gateway before v1.0.0",
		discoveryURL, wireVersionSent, accepted, wireVersionSent, wireVersionSent)
}

// wireUnknown is the refusal of a descriptor the gateway cannot read: it
// cannot confirm the network accepts what it sends, so it does not start.
func wireUnknown(err error) error {
	return fmt.Errorf("fetch discovery: %w (so this gateway cannot confirm the network accepts wire protocol %s, and does not start)", err, wireVersionSent)
}
