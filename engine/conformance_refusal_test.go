package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

func TestConformanceRefusalClosedBoundedMetadata(t *testing.T) {
	for _, gateway := range []string{"payer", strings.Repeat("gateway", 100)} {
		g := &Gateway{cfg: Config{HolderID: gateway}}
		err := g.enforceRules(context.Background(), CheckInput{Exchange: ExchangeContext{policy: NewConformancePolicy(EnforcementStrict)}, Direction: "request", Body: []byte("private body")}, []ConformanceRule{{ID: "fhir.profile", Class: CheckDeep, Applies: func(CheckInput) bool { return true }, Check: func(context.Context, CheckInput) CheckResult {
			return CheckResult{State: CheckUnavailable, Code: "private validator diagnostic"}
		}}})
		var ce *conformanceError
		if !errors.As(err, &ce) {
			t.Fatalf("not a conformance refusal: %v", err)
		}
		for _, leg := range []string{"pas-claim", "crd-order-select"} {
			p, err := ce.refusalPayload(leg)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := relay.Transmit(p, relay.Check(answerKey(leg, relay.OutcomeRefused)))
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid(raw) || len(raw) > 2048 || strings.Contains(string(raw), "private") {
				t.Fatalf("unsafe metadata: %s", raw)
			}
			want := gateway
			if len(gateway) > 256 {
				want = "sha256:" + sha256hex([]byte(gateway))
			}
			if !strings.Contains(string(raw), want) {
				t.Fatal("configured gateway label or stable fingerprint lost")
			}
		}
	}
}
