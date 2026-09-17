package relay

import "bytes"

// ForTest seals b as a payload a test injects: OwnershipAuthored under the
// builder id reserved for tests, which Check admits on any listed transmit.
// It panics outside a test binary, so no gateway can use it to send
// anything.
func ForTest(b []byte, contentType string) Payload {
	if !testBinary() {
		panic("relay: ForTest is available only in tests")
	}
	return Payload{c: &sealed{b: seal(bytes.Clone(b)), own: OwnershipAuthored, builder: builderTestInjected, contentType: contentType}}
}

// BytesForTest returns a copy of p's bytes without a permission check, for
// tests that inspect what a gateway would send. It panics outside a test
// binary.
func BytesForTest(p Payload) []byte {
	if !testBinary() {
		panic("relay: BytesForTest is available only in tests")
	}
	if p.c == nil {
		return nil
	}
	return bytes.Clone(p.c.b.get())
}
