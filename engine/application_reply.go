package engine

import (
	"fmt"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// ApplicationReply is a participant's answer, including non-2xx answers. A
// gateway refusal or transport failure is returned separately as an error.
// Payload is the sole immutable owner of the answer bytes and media type.
type ApplicationReply struct {
	Status          int
	Payload         relay.Payload
	DeclaredVersion string
	VersionSource   string // producer, endpoint, configured-endpoint, builder, transform, or empty
}

func (a ApplicationReply) key(leg string) relay.Key {
	outcome := relay.OutcomeAnswered
	if a.Status/100 != 2 {
		outcome = relay.OutcomeUpstreamError
	}
	return relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: outcome}
}

// bytes preserves the existing requester response ownership checks.
func (a ApplicationReply) bytes(leg string) ([]byte, error) {
	return relay.Transmit(a.Payload, relay.Check(a.key(leg)))
}

// legacy retains the engine's error identity for existing workflow callers.
func (a ApplicationReply) legacy(leg string, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	body, err := a.bytes(leg)
	if err != nil {
		return nil, err
	}
	if a.Status/100 != 2 {
		return nil, &RelayError{Status: a.Status, Body: body, ContentType: a.Payload.ContentType(), leg: leg}
	}
	return body, nil
}

func (g *Gateway) writeApplicationReply(w http.ResponseWriter, a ApplicationReply, leg string) {
	ct := a.Payload.ContentType()
	g.writePayload(w, a.Status, ct, a.Payload, a.key(leg))
}

// normalizeResult accepts legacy Status application answers while keeping a
// response with zero bytes distinct from an unset response (a local refusal).
func normalizeResult(r LegResult) (LegResult, error) {
	if r.ApplicationStatus != 0 && r.Status != 0 && r.ApplicationStatus != r.Status {
		return r, fmt.Errorf("conflicting responder status fields")
	}
	if r.ApplicationStatus != 0 && r.Response.Ownership() == 0 {
		return r, fmt.Errorf("application status without response ownership")
	}
	if r.Response.Ownership() != 0 {
		if r.ApplicationStatus == 0 {
			r.ApplicationStatus = r.Status
		}
		if r.ApplicationStatus == 0 {
			r.ApplicationStatus = http.StatusOK
		}
		if r.ApplicationStatus < 100 || r.ApplicationStatus > 599 {
			return r, fmt.Errorf("invalid application status")
		}
		if r.ApplicationStatus/100 == 2 {
			r.Status = 0
		} else {
			r.Status = r.ApplicationStatus
		}
	}
	return r, nil
}

// requestVersion carries only the producer's independent declaration. ProfileID
// selects builds/routes; it is never evidence of what carried bytes assert.
func requestVersion(c Content) string { return c.DeclaredVersion }
