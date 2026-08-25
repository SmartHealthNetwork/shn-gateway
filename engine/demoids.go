// demoids.go — the two bridging-demo PAYER identities. They are identities, not
// persona content: coverage-derived routing (FeedPayerRouter) reads a member's
// Coverage.payor and sends the leg to whichever holder attests that identity, so the
// bridging demo needs its own obviously-synthetic payer ids that internal/fhirseed (the
// deployed-stack seed source of truth, a separate Go module) and the SHN Kit surfaces
// can NAME rather than duplicate as literals. They survive the retirement of the
// in-process persona census (§4.1) because their consumers are the seed source and
// the Kit, not the deleted census.
package engine

import shnsdk "github.com/SmartHealthNetwork/shn-sdk"

// bridgeDemoPayerSystem namespaces the bridging-demo payer identities distinctly
// from the shared NAIC-style payer-id OID (urn:oid:2.16.840.1.113883.6.300)
// every other holder identity claims a numeric code under (00001/00078/00099/…).
// The bridge-demo payers are a visualization fixture, not a real-world-registered payer, so
// they get their own obviously-synthetic namespace rather than squatting on the next unclaimed
// NAIC-style number.
const bridgeDemoPayerSystem = "urn:shn:demo-payer"

// BridgeDemoPayerID / BridgeRefusePayerID are the payer identities the bridging-demo
// members' Coverage.payor names: coverage-derived routing (FeedPayerRouter) sends their
// legs to the bridge-demo-payer / bridge-demo-refuse holders rather than the CMS
// conformance payer. Exported so internal/fhirseed and the SHN Kit surfaces (runner
// branches, the live bridging fixture) name the exact identity instead of duplicating the
// literal. Plain shnsdk.PayerIdentifier values, following the sdk/payer.go
// CMSPayerIdentity idiom; sdk promotion is optional at a later publish.
var (
	BridgeDemoPayerID   = shnsdk.PayerIdentifier{System: bridgeDemoPayerSystem, Value: "SHN-BRIDGE-DEMO"}
	BridgeRefusePayerID = shnsdk.PayerIdentifier{System: bridgeDemoPayerSystem, Value: "SHN-BRIDGE-REFUSE"}
)
