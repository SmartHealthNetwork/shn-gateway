package app

import (
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"testing"
)

func TestCertificationClientsAreIndependent(t *testing.T) {
	cfg := config{FHIRValidateURL21: "http://configured21/fhir"}
	clients := certificationValidators(env(nil), cfg, "http://canonical/fhir")
	want := map[string]string{"2.0": "http://canonical/fhir", "2.1": "http://configured21/fhir", "2.2": engine.DefaultLaneURL("2.2")}
	for line, endpoint := range want {
		v, ok := clients[line].(*shnsdk.OperationValidator)
		if !ok || v.BaseURL != endpoint {
			t.Fatalf("%s: %#v", line, clients[line])
		}
		defer v.Client.CloseIdleConnections()
		for other, w := range clients {
			if other != line && w.(*shnsdk.OperationValidator).Client == v.Client {
				t.Fatal("shared client")
			}
		}
	}
	fake := certificationValidators(env(map[string]string{"SHN_FAKE_VALIDATOR": "1"}), cfg, "")
	for _, v := range fake {
		if _, ok := v.(*engine.LineFakeValidator); !ok {
			t.Fatal("fake mode used network")
		}
	}
}
