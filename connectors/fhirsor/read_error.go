package fhirsor

import (
	"context"
	"errors"
	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

func invalidResponse() error { return &engine.SoRReadError{Kind: engine.SoRInvalidResponse} }
func safeReadError(err error) error {
	if err == nil {
		return nil
	}
	var safe *engine.SoRReadError
	if errors.As(err, &safe) {
		return safe
	}
	// An answer that repeats a member name can be read two ways: an invalid
	// response that says so, which the gateway records as message integrity.
	if errors.Is(err, relay.ErrDuplicateKey) {
		return engine.SoRRepeatedMember()
	}
	// A redirect from https to http, not followed: the read failed, and the
	// error says why, so the record names it.
	if errors.Is(err, smartauth.ErrCleartextRedirect) && !smartauth.IsTokenAcquisitionError(err) {
		return engine.SoRCleartextRedirect()
	}
	kind := engine.SoRInvalidResponse
	var status *fhirclient.HTTPError
	var token *smartauth.TokenEndpointError
	var transport *fhirclient.TransportError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		kind = engine.SoRUnavailable
	case errors.As(err, &token):
		if token.StatusCode == 429 || token.StatusCode >= 500 {
			kind = engine.SoRUnavailable
		} else {
			kind = engine.SoRAuthenticationFailed
		}
	case errors.As(err, &status):
		if status.StatusCode == 401 || status.StatusCode == 403 {
			kind = engine.SoRAuthenticationFailed
		} else if status.StatusCode == 429 || status.StatusCode >= 500 {
			kind = engine.SoRUnavailable
		}
	case smartauth.IsTokenAcquisitionError(err):
		var network *smartauth.TokenTransportError
		if errors.As(err, &network) {
			kind = engine.SoRUnavailable
		}
	case errors.As(err, &transport):
		kind = engine.SoRUnavailable
	}
	return &engine.SoRReadError{Kind: kind}
}
