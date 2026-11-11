package engine

import (
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TagValidatorClient Wraps v's transport with tag (MON-R25): a validator
// built by NewOperationValidator, or the bounded inner transport of a
// certification client from NewCertificationOperationValidator, beneath its
// own wrapper. The transport itself, its connections and every limit stay as
// they were. A validator sidecar follows no redirect: a routing validator's
// client is put under the network-service redirect policy (serviceclient)
// whether or not certs is set; a certification client applies it itself. A
// nil v changes nothing; a nil certs tags nothing.
func TagValidatorClient(v *shnsdk.OperationValidator, certs *endpointcert.Recorder, tag endpointcert.Tag) {
	if v == nil {
		return
	}
	c := *shnsdk.NewClient() // as the SDK would default a validator with no client
	if v.Client != nil {
		c = *v.Client
	}
	if t, ok := c.Transport.(certificationTransport); ok {
		if certs != nil {
			t.inner = certs.Wrap(tag, t.inner)
		}
		c.Transport = t
	} else {
		if certs != nil {
			c.Transport = certs.Wrap(tag, c.Transport)
		}
		c.Transport = serviceclient.Transport(c.Transport)
	}
	v.Client = &c
}

// serviceValidator puts v under the network-service redirect policy when it
// is an *shnsdk.OperationValidator whose client is not under it yet
// (TagValidatorClient with no recorder): a validator an embedding program
// passes follows no redirect, as the ones the gateway builds. One already
// under it (a certification client, which applies the policy itself, or one
// the gateway built and tagged) is left as it is: only its Client is read, so
// the gateway's own validators, which its lanes may already be using, are
// never written here. An embedder's validator not under it yet has its
// Client set in place (Config.Validator says what that asks of a validator
// shared between gateways).
func serviceValidator(v shnsdk.Validator) {
	op, ok := v.(*shnsdk.OperationValidator)
	if !ok || op == nil || validatorFollowsNoRedirect(op) {
		return
	}
	TagValidatorClient(op, nil, endpointcert.Tag{})
}

// validatorFollowsNoRedirect reports whether op's client is under the
// network-service redirect policy.
func validatorFollowsNoRedirect(op *shnsdk.OperationValidator) bool {
	if op.Client == nil {
		return false
	}
	if _, ok := op.Client.Transport.(certificationTransport); ok {
		return true
	}
	return serviceclient.Refuses(op.Client.Transport)
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
