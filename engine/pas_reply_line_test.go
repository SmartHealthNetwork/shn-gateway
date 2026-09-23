package engine

import (
	"context"
	"net/http"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// PCV-07/08: an independently declared response never borrows the request's
// PAS line. The pinned payer accepts 2.2 requests and returns 2.0 answers.
func TestPASApplicationReplyUsesProducerLineForOptionalProfile(t *testing.T) {
	body := pasGolden(t, "claimresponse-pended.json")
	for _, tc := range []struct {
		name    string
		level   ConformanceEnforcement
		version string
		status  int
		calls20 int
	}{
		{"none producer 2.0", EnforcementNone, shnsdk.ContractPAPAS20, 0, 0},
		{"strict producer 2.0", EnforcementStrict, shnsdk.ContractPAPAS20, 0, 1},
		{"strict missing declaration", EnforcementStrict, "", http.StatusServiceUnavailable, 0},
		{"strict unknown declaration", EnforcementStrict, "pa.pas@9.9", http.StatusServiceUnavailable, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v20, v22 := syntheticLineValidator("2.0"), syntheticLineValidator("2.2")
			g := &Gateway{cfg: Config{ConformanceEnforcement: tc.level,
				// A legacy/default validator must not fill an unknown producer lane.
				Validator: syntheticFakeValidator(), ValidatorsByLine: map[string]shnsdk.Validator{"2.0": v20, "2.2": v22}}}
			status, msg := g.validatePASApplicationReply(context.Background(), body, ApplicationReply{DeclaredVersion: tc.version})
			if status != tc.status {
				t.Fatalf("status=%d message=%q, want %d", status, msg, tc.status)
			}
			if len(v20.Calls()) != tc.calls20 || len(v22.Calls()) != 0 {
				t.Fatalf("validator calls: 2.0=%+v 2.2=%+v", v20.Calls(), v22.Calls())
			}
		})
	}
}
