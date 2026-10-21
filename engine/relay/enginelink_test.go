package relay_test

// Linking the gateway engine registers its workstreams' ownership rows when
// this test binary loads (relay.RegisterOwnership), so the package's own
// tests check the table the engine registers: TestLegOwnershipTablePinned
// pins the real table, not a copy kept here.
import _ "github.com/SmartHealthNetwork/shn-gateway/engine"
