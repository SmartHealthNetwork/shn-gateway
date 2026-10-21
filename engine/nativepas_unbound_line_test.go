package engine

// nativepas_unbound_line_test.go — the log line an amendment that binds no pend
// leaves on the payer gateway. A live check reads the pend ledger's hold on a
// claim from this line alone (an amendment of a pended claim binds and logs
// nothing; one of a decided claim logs already-decided), so its wording and its
// order are pinned here: written before the amendment is forwarded, which is
// before the exchange's access line.

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPASUpdate_UnboundLineNamesTheLedgerReason(t *testing.T) {
	const origCorr = "convergence-pas-submit-0001"
	const pci = "PCI-UNBOUND-LINE"
	answer := fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"queued"}`), true)
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, s *censusSoR)
		want string // the line's reason; "" means the amendment binds and logs no line
	}{
		{"a pended claim binds", func(t *testing.T, s *censusSoR) {
			if err := s.RecordPendedClaim(pci, origCorr); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"a decided claim", func(t *testing.T, s *censusSoR) {
			if _, err := testLedger(s).RecordDecision(pci, origCorr, PendOutcomeApproved, time.Unix(1000, 0).UTC(), PendKeys{}); err != nil {
				t.Fatal(err)
			}
		}, "already-decided"},
		{"a claim not pended here", func(*testing.T, *censusSoR) {}, "not-pended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			corr := "corr-unbound-line-" + strings.ReplaceAll(tc.name, " ", "-")
			line := "gateway: pas-claim-update: correlation " + corr + ": amendment forwarded without binding a local pend ("
			var logs lockedBuffer
			prev, flags := log.Writer(), log.Flags()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })

			var atForward string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atForward = logs.String()
				w.Header().Set("Content-Type", "application/fhir+json")
				_, _ = w.Write(answer)
			}))
			defer srv.Close()
			s := newCensusSoR()
			tc.seed(t, s)
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
			ctx, _ := withPASLeg(context.Background(), "provider")
			res, err := n.Handle(ctx, "pas-claim-update", corr, pci, originatorBuiltConformantUpdateBundle(t))
			if err != nil || res.Status != 0 {
				t.Fatalf("the amendment must reach the payer: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			if res.Rollback != nil {
				defer res.Rollback()
			}
			lines := strings.Count(logs.String(), line)
			if tc.want == "" {
				if lines != 0 {
					t.Fatalf("an amendment that bound the pend logged an unbound line:\n%s", logs.String())
				}
				if res.Rollback == nil {
					t.Fatal("the amendment did not bind the pended claim")
				}
				return
			}
			if lines != 1 || !strings.Contains(logs.String(), line+tc.want+")\n") {
				t.Fatalf("want exactly one line %q, got:\n%s", line+tc.want+")", logs.String())
			}
			if !strings.Contains(atForward, line+tc.want+")\n") {
				t.Fatalf("the unbound line must be written before the amendment is forwarded; at the forward the log held:\n%s", atForward)
			}
		})
	}
}
