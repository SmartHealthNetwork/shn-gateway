package app

import (
	"strings"
	"testing"
)

func TestNativeResponseDeclarationsConfig(t *testing.T) {
	for _, raw := range []string{"", " ", "[]", `[{"operation":"pas-submit","endpoint":"https://payer.example/Claim/$submit","contractVersion":"pa.pas@2.2"}]`, "null"} {
		for _, base := range []string{"", "https://payer.example"} {
			e := baseEnv(map[string]string{"ROLE": "payer", "PAYER_DAVINCI_BASE_URL": base, "PAYER_DAVINCI_RESPONSE_DECLARATIONS": raw})
			_, err := loadConfig(func(k string) string { return e[k] })
			wantBad := raw == "null" || (raw != "" && base == "")
			if (err != nil) != wantBad {
				t.Errorf("raw=%q base=%q error=%v", raw, base, err)
			}
			if err != nil && strings.Contains(err.Error(), "https://payer.example/") {
				t.Errorf("unsafe error %v", err)
			}
		}
	}
}
