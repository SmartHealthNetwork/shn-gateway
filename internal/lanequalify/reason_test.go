package lanequalify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// A failed qualification names its kind, whatever wraps it, and never the
// error's own text: a missing lane reads as a name that does not resolve.
func TestFailureReasonNamesTheKindOnly(t *testing.T) {
	const private = "private outcome payload"
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	for want, err := range map[string]error{
		"name does not resolve":                     fmt.Errorf("metadata unavailable: %w: %w", &net.DNSError{Err: private, Name: "shn-validator-2-1", IsNotFound: true}, context.DeadlineExceeded),
		"name lookup failed":                        &net.DNSError{Err: private, Name: "validator", IsTemporary: true},
		"connection refused":                        fmt.Errorf("metadata unavailable: %w: %w", dial, context.DeadlineExceeded),
		"metadata answered HTTP 503":                fmt.Errorf("metadata unavailable: %w: %w", &MetadataStatusError{Status: 503}, context.DeadlineExceeded),
		"metadata is not an R4 CapabilityStatement": ErrNotR4Metadata,
		"qualification corpus did not pass":         fmt.Errorf("%w: %w", ErrCorpus, errors.New(private)),
		"no answer within the qualification budget": fmt.Errorf("%w: %w", ErrCorpus, context.DeadlineExceeded),
		"qualification stopped":                     context.Canceled,
		"qualification did not pass":                errors.New(private),
	} {
		got := FailureReason(err)
		if got != want {
			t.Errorf("FailureReason(%v) = %q, want %q", err, got, want)
		}
		if strings.Contains(got, private) {
			t.Errorf("reason %q carries the error's text", got)
		}
	}
	if FailureReason(nil) != "" {
		t.Error("a success has no failure reason")
	}
	if Host("http://shn-validator-2-2:8080/fhir") != "shn-validator-2-2" || Host(":bad") != "" {
		t.Error("Host")
	}
}
