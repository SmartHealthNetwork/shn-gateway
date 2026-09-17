package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func safeSoRError(err error) *SoRReadError {
	status, _ := SoRFailureResponse(err)
	if status == http.StatusServiceUnavailable {
		return &SoRReadError{Kind: SoRUnavailable}
	}
	var typed *SoRReadError
	if errors.As(err, &typed) && typed != nil && typed.Kind == SoRAuthenticationFailed {
		return &SoRReadError{Kind: SoRAuthenticationFailed}
	}
	return &SoRReadError{Kind: SoRInvalidResponse}
}
func writeSoRFailure(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	status, msg := SoRFailureResponse(err)
	writeJSON(w, status, map[string]string{"error": msg})
	return true
}

// Reference callbacks are scoped to one SDK invocation. Once a read fails,
// further callback calls do no work; callers check the captured error first.
func sorReferenceCallback(ctx context.Context, sor SystemOfRecord) (func(string) ([]byte, bool), *error) {
	var readErr error
	return func(ref string) ([]byte, bool) {
		if readErr != nil || sor == nil {
			return nil, false
		}
		b, found, err := ReadSystemOfRecord(sor).ResolveByReferenceContext(ctx, ref)
		if err != nil {
			readErr = safeSoRError(err)
			return nil, false
		}
		return b, found
	}, &readErr
}

// memberCoverage reads the member's Coverage records from the system of record
// and returns the one to route on. found is false when there is none. A read
// failure is the system-of-record failure status. Several records are
// accepted only when every one of them names the same payer; otherwise the
// member's payer is ambiguous and the answer is 422, never a guess.
//
// With several same-payer records the first one the system returned is used;
// the routing identity is the same whichever is used. Gateway-originated CRD
// requests are to carry the whole Coverage search result instead (a planned
// change that keeps this routing rule).
func (g *Gateway) memberCoverage(ctx context.Context, member string) (coverage []byte, found bool, status int, msg string) {
	covs, err := ReadSystemOfRecord(g.cfg.SoR).OpenCoverageContext(ctx, member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, false, status, msg
	}
	switch len(covs) {
	case 0:
		return nil, false, 0, ""
	case 1:
		return covs[0], true, 0, ""
	}
	var first shnsdk.PayerIdentifier
	for i, c := range covs {
		resolve, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
		pid, parseErr := shnsdk.ParseCoveragePayer(c, resolve)
		if *readErr != nil {
			status, msg := SoRFailureResponse(*readErr)
			return nil, false, status, msg
		}
		if parseErr != nil || (i > 0 && pid != first) {
			return nil, false, http.StatusUnprocessableEntity, "ambiguous coverage for routing: the member's Coverage records do not name one payer"
		}
		first = pid
	}
	return covs[0], true, 0, ""
}

func (g *Gateway) recipientForSoR(ctx context.Context, coverage []byte) (string, shnsdk.PayerIdentifier, int, string) {
	if g.cfg.PayerRouter == nil {
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, "no payer router configured"
	}
	resolve, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
	parsed, ok := shnsdk.ParsePayerIdentifier(coverage, resolve)
	if *readErr != nil {
		status, msg := SoRFailureResponse(*readErr)
		return "", shnsdk.PayerIdentifier{}, status, msg
	}
	if !ok {
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, "no payer identifier on member coverage"
	}
	holder, ok := g.cfg.PayerRouter.Resolve(parsed)
	if !ok {
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, fmt.Sprintf("no registered payer for identifier %s|%s", parsed.System, parsed.Value)
	}
	return holder, parsed, 0, ""
}
