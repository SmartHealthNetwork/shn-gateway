// gateway/engine/relayresp_test.go
package engine

import "testing"

func TestRelayError_Error(t *testing.T) {
	e := &RelayError{Status: 502, Body: []byte(`{"x":1}`)}
	if e.Error() == "" {
		t.Fatal("RelayError.Error() empty")
	}
}
