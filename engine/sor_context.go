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
