package engine

import (
	"errors"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

var errFramedCRDUnsupported = errors.New("recipient does not support declared CRD hook request framing")

// inboundFrameCRDHook checks request-only addressing independently of the body.
// A legacy bare request carries no declaration; callers must not guess a hook.
func inboundFrameCRDHook(legType string, payload []byte) (string, int, string) {
	if !shnsdk.IsFramed(payload) {
		return "", 0, ""
	}
	hdr, _, err := shnsdk.DecodeHTTPFrame(payload)
	if err != nil {
		return "", http.StatusBadRequest, "request frame decode failed"
	}
	hook := hdr.Headers[shnsdk.FrameHeaderCRDHook]
	if hook != "" && !validCRDHook(legType, hook) {
		return "", http.StatusBadRequest, "context_invalid"
	}
	return hook, 0, ""
}

// ExchangeContext is byte-bound addressing supplied by an authenticated source.
// Only ingress and network verification create its private fields. It neither
// certifies clinical content nor changes the catalog's per-leg authority.
type ExchangeContext struct {
	holder, clientID, recipient, legType, subjectPCI                string
	correlationID, custodian, consentRef                            string
	operation, contractVersion, versionSource, contentType, crdHook string
	bodySHA256                                                      string
	boundary                                                        []BoundaryCompletion
	policy                                                          ConformancePolicy
}
type BoundaryCompletion struct{ ID, Version string }
type IngressPrincipal struct{ ClientID string }
