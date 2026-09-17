package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// testResponse seals b as an answer a test responder builds itself (its
// ownership is authored).
func testResponse(b []byte) relay.Payload { return relay.ForTest(b, "application/fhir+json") }

// relayedResponse seals b as a participant's answer, relayed exactly.
func relayedResponse(b []byte) relay.Payload {
	return relay.Exact(relay.NewBody(b, relay.OriginUpstreamResponse), "application/fhir+json")
}

// responseBytes reads a result's answer.
func responseBytes(r LegResult) []byte { return relay.BytesForTest(r.Response) }

// testRequest seals b as a request a test sends.
func testRequest(b []byte) relay.Payload { return relay.ForTest(b, "application/json") }

// peerBody seals b as a body that arrived from the network.
func peerBody(b []byte) relay.Body { return relay.NewBody(b, relay.OriginPeerFrame) }
