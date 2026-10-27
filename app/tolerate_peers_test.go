//go:build !shn_coreonly

package app

import (
	"reflect"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// CONFORMANCE_TOLERATE_PEERS reaches the engine as the participant wrote it, is
// logged at boot (none when unset), and a malformed value refuses to boot.
func TestBuild_ConformanceToleratePeers(t *testing.T) {
	sys := shnsdk.CMSPayerIdentity.System
	t.Run("listed", func(t *testing.T) {
		b, out, err := buildForTest(t, nosorBuildEnv(t, "payer", map[string]string{"CONFORMANCE_TOLERATE_PEERS": sys + "|00001," + sys + "|00300"}))
		if err != nil {
			t.Fatal(err)
		}
		got := reflectToleratePeers(t, b)
		want := []shnsdk.PayerIdentifier{{System: sys, Value: "00001"}, {System: sys, Value: "00300"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("engine tolerates %v, want %v", got, want)
		}
		if !strings.Contains(out, "gateway: conformance tolerated peers="+sys+"|00001,"+sys+"|00300\n") {
			t.Fatalf("boot log does not name the tolerated payers:\n%s", out)
		}
	})
	t.Run("unset", func(t *testing.T) {
		b, out, err := buildForTest(t, nosorBuildEnv(t, "payer", nil))
		if err != nil {
			t.Fatal(err)
		}
		if got := reflectToleratePeers(t, b); len(got) != 0 {
			t.Fatalf("unset tolerates %v", got)
		}
		if !strings.Contains(out, "gateway: conformance tolerated peers=none\n") {
			t.Fatalf("boot log does not say none:\n%s", out)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		_, _, err := buildForTest(t, nosorBuildEnv(t, "payer", map[string]string{"CONFORMANCE_TOLERATE_PEERS": "00001"}))
		if err == nil || !strings.Contains(err.Error(), "CONFORMANCE_TOLERATE_PEERS") {
			t.Fatalf("a malformed value booted: %v", err)
		}
	})
}

// reflectToleratePeers is the tolerance list build handed engine.New, read from
// the unexported config as builtSystemOfRecord reads the system of record.
func reflectToleratePeers(t *testing.T, b built) []shnsdk.PayerIdentifier {
	t.Helper()
	if b.gateway == nil {
		t.Fatal("build returned no gateway")
	}
	f := reflect.ValueOf(b.gateway).Elem().FieldByName("cfg").FieldByName("ConformanceToleratePeers")
	if !f.IsValid() {
		t.Fatal("engine.Gateway has no cfg.ConformanceToleratePeers")
	}
	var out []shnsdk.PayerIdentifier
	for i := 0; i < f.Len(); i++ {
		e := f.Index(i)
		out = append(out, shnsdk.PayerIdentifier{System: e.FieldByName("System").String(), Value: e.FieldByName("Value").String()})
	}
	return out
}
