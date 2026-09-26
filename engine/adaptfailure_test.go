package engine

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdaptFailureStatus: a typed bridging refusal, however it is wrapped, is
// the legible 422 the compatibility matrix states; every other egressAdapt
// failure is this gateway's own fault and stays a 502.
func TestAdaptFailureStatus(t *testing.T) {
	refusal := &SemanticChangeError{Contract: "pa.dtr", From: "2.1", To: "2.2", Direction: "up", MissingElements: []string{"QuestionnaireResponse.extension:qr-coverage"}}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"typed refusal", refusal, http.StatusUnprocessableEntity},
		{"wrapped refusal", fmt.Errorf("apply chain: %w", refusal), http.StatusUnprocessableEntity},
		{"step parse failure", errors.New("engine: pa.pas step: unexpected end of JSON input"), http.StatusBadGateway},
		{"provenance fault", fmt.Errorf("engine: egressAdapt: %w", errors.New("loss round trip mismatch")), http.StatusBadGateway},
	} {
		if got := adaptFailureStatus(tc.err); got != tc.want {
			t.Errorf("%s: adaptFailureStatus = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestEveryEgressAdaptSiteAnswersThroughAdaptFailureStatus fences the call
// sites: an egressAdapt failure is answered with adaptFailureStatus (or, on the
// bridging demo lane only, reshaped by bridgeRefusalText), never a status
// written by hand. The provider ingress sites cannot raise a typed refusal
// today (the CDS Hooks request is line-inert, the questionnaire-package fetch
// is an envelope leg), so this fence is what keeps them on the 422 contract
// when a chain that can refuse reaches them.
func TestEveryEgressAdaptSiteAnswersThroughAdaptFailureStatus(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sites, bridged := 0, 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if !strings.Contains(line, "g.egressAdapt(") {
				continue
			}
			sites++
			window := strings.Join(lines[i+1:min(i+5, len(lines))], "\n")
			switch {
			case strings.Contains(window, "bridgeRefusalText("):
				bridged++
			case !strings.Contains(window, "adaptFailureStatus("):
				t.Errorf("%s:%d: the egressAdapt failure is not answered through adaptFailureStatus:\n%s", f, i+1, window)
			}
		}
	}
	// The scan must be able to match: the engine has 19 call sites today.
	if sites < 19 {
		t.Fatalf("found %d egressAdapt call sites, want the engine's full set (19 or more)", sites)
	}
	// Only the bridging demo lane reshapes a refusal into its structured 200.
	if bridged != 1 {
		t.Fatalf("%d egressAdapt sites reshape the refusal with bridgeRefusalText, want only the bridging demo lane's", bridged)
	}
}
