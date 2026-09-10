package smartauth

import (
	"errors"
	"strconv"
)

// tokenAcquisitionError replaces the transport's prior formatted wrapper while
// preserving its exact message and immediate unwrap target.
type tokenAcquisitionError struct{ cause error }

func (e *tokenAcquisitionError) Error() string { return "smartauth: acquire token: " + e.cause.Error() }
func (e *tokenAcquisitionError) Unwrap() error { return e.cause }

// IsTokenAcquisitionError reports whether err wraps a bearer transport's token
// acquisition failure, including through net/http's URL error. Matching is typed:
// resource-request errors with similar text do not qualify. It does not expose
// token endpoint details or change the underlying error's unwrap chain.
func IsTokenAcquisitionError(err error) bool {
	var acquisition *tokenAcquisitionError
	return errors.As(err, &acquisition)
}

// TokenEndpointError carries an HTTP refusal without credentials or response body.
type TokenEndpointError struct{ StatusCode int }

func (e *TokenEndpointError) Error() string {
	return "smartauth: token endpoint status " + strconv.Itoa(e.StatusCode)
}

// TokenTransportError distinguishes unavailable token endpoints from invalid responses.
type TokenTransportError struct{ Cause error }

func (e *TokenTransportError) Error() string { return "smartauth: token endpoint: " + e.Cause.Error() }
func (e *TokenTransportError) Unwrap() error { return e.Cause }
