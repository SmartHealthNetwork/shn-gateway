package app

import (
	"strings"
	"testing"
)

// TestBuild_PASNativeNoticeOnlyWhenSet: PAYER_DAVINCI_PAS_NATIVE is no longer
// a switch, so a native-forward payer says so when the key is set, to any
// value, and says nothing about a key it never set.
func TestBuild_PASNativeNoticeOnlyWhenSet(t *testing.T) {
	const notice = "PAYER_DAVINCI_PAS_NATIVE is set but is no longer a switch"
	for _, tc := range []struct {
		name   string
		extra  map[string]string
		notice bool
	}{
		{"unset", nil, false},
		{"true", map[string]string{"PAYER_DAVINCI_PAS_NATIVE": "true"}, true},
		{"false", map[string]string{"PAYER_DAVINCI_PAS_NATIVE": "false"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out, err := buildForTest(t, nosorBuildEnv(t, "payer", tc.extra))
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if got := strings.Contains(out, notice); got != tc.notice {
				t.Fatalf("notice printed = %v, want %v:\n%s", got, tc.notice, out)
			}
			if !tc.notice && strings.Contains(out, "PAYER_DAVINCI_PAS_NATIVE") {
				t.Fatalf("a payer that never set PAYER_DAVINCI_PAS_NATIVE was told about it:\n%s", out)
			}
		})
	}
}
