package lanequalify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"
)

// ErrNotR4Metadata is a validator whose metadata answer is not an R4
// CapabilityStatement.
var ErrNotR4Metadata = errors.New("metadata is not an R4 CapabilityStatement")

// ErrCorpus is a validator that answered its metadata but not the readiness
// corpus as expected.
var ErrCorpus = errors.New("qualification corpus did not pass")

// MetadataStatusError is a metadata answer with a status other than 200.
type MetadataStatusError struct{ Status int }

func (e *MetadataStatusError) Error() string {
	return fmt.Sprintf("metadata answered HTTP %d", e.Status)
}

// Host is the host a lane's base URL dials, or "" when it has none.
func Host(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// FailureReason names why a qualification failed in the qualifier's own
// words, from the error's kind alone: a lane that is missing reads as a name
// that does not resolve, never as a bare failure. It never carries a
// validator's answer or an error's text, so no outcome payload can reach the
// log it is written to.
func FailureReason(err error) string {
	var dns *net.DNSError
	var status *MetadataStatusError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &dns) && dns.IsNotFound:
		return "name does not resolve"
	case errors.As(err, &dns):
		return "name lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &status):
		return status.Error()
	case errors.Is(err, ErrNotR4Metadata):
		return ErrNotR4Metadata.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer within the qualification budget"
	case errors.Is(err, context.Canceled):
		return "qualification stopped"
	case errors.Is(err, ErrCorpus):
		return ErrCorpus.Error()
	}
	return "qualification did not pass"
}
