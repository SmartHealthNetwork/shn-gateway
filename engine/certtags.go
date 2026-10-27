package engine

import (
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TagValidatorClient Wraps v's transport with tag (MON-R25): a validator
// built by NewOperationValidator, or the bounded inner transport of a
// certification client from NewCertificationOperationValidator, beneath its
// own wrapper. The transport itself, its connections and every limit stay as
// they were. A nil v or certs changes nothing.
func TagValidatorClient(v *shnsdk.OperationValidator, certs *endpointcert.Recorder, tag endpointcert.Tag) {
	if v == nil || certs == nil {
		return
	}
	c := *shnsdk.NewClient() // as the SDK would default a validator with no client
	if v.Client != nil {
		c = *v.Client
	}
	if t, ok := c.Transport.(certificationTransport); ok {
		t.inner = certs.Wrap(tag, t.inner)
		c.Transport = t
	} else {
		c.Transport = certs.Wrap(tag, c.Transport)
	}
	v.Client = &c
}

// CertificationInnerForTest is a certification client's inner transport;
// ok is false for a client NewCertificationOperationValidator did not build.
func CertificationInnerForTest(v *shnsdk.OperationValidator) (http.RoundTripper, bool) {
	if v == nil || v.Client == nil {
		return nil, false
	}
	t, ok := v.Client.Transport.(certificationTransport)
	return t.inner, ok
}
