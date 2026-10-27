//go:build !shn_coreonly

package app

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// TestBuild_PASNativeOnlyWarns: PAYER_DAVINCI_PAS_NATIVE is no longer read, so a
// native-forward payer that sets it, to any value, boots and logs the ignored
// setting's warning once, and one that never set it hears nothing about it.
func TestBuild_PASNativeOnlyWarns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]string
		warns bool
	}{
		{"unset", nil, false},
		{"true", map[string]string{"PAYER_DAVINCI_PAS_NATIVE": "true"}, true},
		{"false", map[string]string{"PAYER_DAVINCI_PAS_NATIVE": "false"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var boot bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&boot)
			defer log.SetOutput(previous)
			_, out, err := buildForTest(t, nosorBuildEnv(t, "payer", tc.extra))
			log.SetOutput(previous)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			want := 0
			if tc.warns {
				want = 1
			}
			if n := strings.Count(boot.String(), pasNativeLeftoverLine); n != want {
				t.Fatalf("the warning is logged %d times, want %d:\n%s", n, want, boot.String())
			}
			if !tc.warns && strings.Contains(boot.String()+out, "PAYER_DAVINCI_PAS_NATIVE") {
				t.Fatalf("a payer that never set PAYER_DAVINCI_PAS_NATIVE was told about it:\n%s%s", boot.String(), out)
			}
		})
	}
}
