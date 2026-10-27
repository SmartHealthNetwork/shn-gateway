//go:build !shn_coreonly

package engine

import "github.com/SmartHealthNetwork/shn-gateway/pa/personas"

// The PA scenario personas moved to package personas. The names below are
// that package's, unchanged, until a later minor release removes them.

// Lanes a /scenario/* route resolves its member on.
const (
	// Deprecated: use personas.LaneConformance (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	LaneConformance = personas.LaneConformance
	// Deprecated: use personas.LaneProviderData (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	LaneProviderData = personas.LaneProviderData
	// Deprecated: use personas.LaneDemo (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	LaneDemo = personas.LaneDemo
)

// ScenarioPersona is one member a /scenario/* route originates for.
//
// Deprecated: use personas.ScenarioPersona (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
type ScenarioPersona = personas.ScenarioPersona

// ScenarioRoster is every (scenario, lane, persona set) a /scenario/* route
// resolves, with its member and product, sorted.
//
// Deprecated: use personas.ScenarioRoster (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
func ScenarioRoster() []ScenarioPersona { return personas.ScenarioRoster() }

// CanaryTwins maps each scenario member to its dedicated canary twin. It is
// the personas map itself.
//
// Deprecated: use personas.CanaryTwins (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
var CanaryTwins = personas.CanaryTwins

// The bridging-demo payer identities.
var (
	// Deprecated: use personas.BridgeDemoPayerID (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	BridgeDemoPayerID = personas.BridgeDemoPayerID
	// Deprecated: use personas.BridgeRefusePayerID (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	BridgeRefusePayerID = personas.BridgeRefusePayerID
)

// DemoOrderTuple is one demo-lane scenario's originated order.
//
// Deprecated: use personas.DemoOrderTuple (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
type DemoOrderTuple = personas.DemoOrderTuple

// DemoOrderSet is one DemoOrderTuple per UC scenario the demo lane originates.
//
// Deprecated: use personas.DemoOrderSet (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
type DemoOrderSet = personas.DemoOrderSet

// DemoOrderCodes returns the demo lane's per-UC order tuples.
//
// Deprecated: use personas.DemoOrderCodes (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
func DemoOrderCodes() DemoOrderSet { return personas.DemoOrderCodes() }

// Demo-profile display/dx constants.
const (
	// Deprecated: use personas.DemoDisplayE0250 (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	DemoDisplayE0250 = personas.DemoDisplayE0250
	// Deprecated: use personas.DemoDxE0250 (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	DemoDxE0250 = personas.DemoDxE0250
	// Deprecated: use personas.DemoDisplayL8000 (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	DemoDisplayL8000 = personas.DemoDisplayL8000
	// Deprecated: use personas.DemoDxL8000 (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	DemoDxL8000 = personas.DemoDxL8000
	// Deprecated: use personas.DemoDisplayG0151 (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	DemoDisplayG0151 = personas.DemoDisplayG0151
	// Deprecated: use personas.DemoDxG0151 (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	DemoDxG0151 = personas.DemoDxG0151
	// Deprecated: use personas.DemoDisplayJ3490 (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	DemoDisplayJ3490 = personas.DemoDisplayJ3490
	// Deprecated: use personas.DemoDxJ3490 (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	DemoDxJ3490 = personas.DemoDxJ3490
	// Deprecated: use personas.DemoDisplayE1390UC03 (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	DemoDisplayE1390UC03 = personas.DemoDisplayE1390UC03
	// Deprecated: use personas.DemoDxE1390UC03 (package github.com/SmartHealthNetwork/shn-gateway/pa/personas).
	DemoDxE1390UC03 = personas.DemoDxE1390UC03
)
