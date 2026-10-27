//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A default lane whose first qualification failed answers a request framed at
// its line once a later attempt passes, with no restart. Before, the
// failure was final: the gateway carried every such request at its own line
// for the life of the process.
func TestFramedRequestAnsweredAtItsLineOnceTheLaneRequalifies(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()
	d := NewDiscoveredLane("2.2", DefaultLaneURL("2.2"), shnsdk.NewFakeValidator())
	g := d9Gateway(nil, nil)
	g.cfg.ValidatorsByContractLine = ContractLineLanes(map[string]shnsdk.Validator{"2.0": canonical},
		map[string]*DiscoveredLane{"2.2": d}, nil, nil)
	if err := d.Qualify(context.Background(), func(context.Context, string, string) error { return errors.New("down") }); err == nil {
		t.Fatal("a failing qualifier passed")
	}
	_, tok, carried, status, _ := g.unframeRequestFrom("requester", "pas-claim", framedRequest(t, "pa.pas@2.2", []byte(`{}`)))
	if status != 0 || tok == "pa.pas@2.2" || carried != "pa.pas@2.2" {
		t.Fatalf("after a failed qualification: (%d, %q, carried %q), want carried, not answered at 2.2", status, tok, carried)
	}
	if err := d.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatalf("a retry did not run: %v", err)
	}
	_, tok, carried, status, _ = g.unframeRequestFrom("requester", "pas-claim", framedRequest(t, "pa.pas@2.2", []byte(`{}`)))
	if status != 0 || tok != "pa.pas@2.2" || carried != "" {
		t.Fatalf("once the lane qualified: (%d, %q, carried %q), want answered at pa.pas@2.2", status, tok, carried)
	}
}
