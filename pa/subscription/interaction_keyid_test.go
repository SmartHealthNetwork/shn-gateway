package subscription

import (
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// E-21's check in the relay core reads a notification address's key id at
// the length this library writes it.
func TestRelayReadsTheKeyIDAtItsLength(t *testing.T) {
	if relay.NotificationKeyIDLen != KeyIDLen {
		t.Fatalf("relay.NotificationKeyIDLen %d, KeyIDLen %d", relay.NotificationKeyIDLen, KeyIDLen)
	}
}
