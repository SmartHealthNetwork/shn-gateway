package engine

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// MON-R25: TagValidatorClient tags a routing validator's transport and a
// certification client's inner transport, keeps the certification
// wrapper's own limits, and leaves a fixture transport as it is.
func TestMONR25TagValidatorClientTagsBothValidatorShapes(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		fmt.Fprint(w, `{"resourceType":"OperationOutcome","issue":[]}`)
	}))
	defer srv.Close()
	rec := endpointcert.New()
	trusted := srv.Client().Transport.(*http.Transport).TLSClientConfig

	routing := shnsdk.NewOperationValidator(srv.URL)
	base := srv.Client().Transport
	routing.Client.Transport = base
	TagValidatorClient(routing, rec, endpointcert.Lane(endpointcert.Validator, "2.1"))
	if routing.Client.Timeout != 30*time.Second {
		t.Fatalf("routing client timeout = %v", routing.Client.Timeout)
	}
	if got, tag, ok := endpointcert.Unwrap(routing.Client.Transport); !ok || got != base || tag != endpointcert.Lane(endpointcert.Validator, "2.1") {
		t.Fatalf("routing transport = %v, %v, %v; want its own transport, tagged", got, tag, ok)
	}
	if _, err := routing.Validate(t.Context(), []byte(`{"resourceType":"Bundle"}`), ""); err != nil {
		t.Fatalf("validate through the wrapped transport: %v", err)
	}
	if _, ok := rec.Soonest(endpointcert.Lane(endpointcert.Validator, "2.1"), endpointcert.Server); !ok {
		t.Fatal("the routing validator's answer was not recorded under its lane")
	}

	cert := NewCertificationOperationValidator(srv.URL)
	inner := cert.Client.Transport.(certificationTransport).inner.(*http.Transport)
	inner.TLSClientConfig = &tls.Config{RootCAs: trusted.RootCAs}
	TagValidatorClient(cert, rec, endpointcert.Lane(endpointcert.Certify, "2.2"))
	ct, ok := cert.Client.Transport.(certificationTransport)
	if !ok {
		t.Fatal("the certification wrapper was dropped")
	}
	got, tag, ok := endpointcert.Unwrap(ct.inner)
	if !ok || got != http.RoundTripper(inner) || tag != endpointcert.Lane(endpointcert.Certify, "2.2") {
		t.Fatalf("certification inner = %v, %v, %v; want its own bounded transport, tagged", got, tag, ok)
	}
	if inner.MaxConnsPerHost != 1 || cert.Client.Timeout != certificationClientTimeout {
		t.Fatal("the certification client's limits changed")
	}
	if _, err := cert.Validate(t.Context(), []byte(`{"resourceType":"Bundle"}`), ""); err != nil {
		t.Fatalf("certification through the tagged transport: %v", err)
	}
	if na, ok := rec.Soonest(endpointcert.Lane(endpointcert.Certify, "2.2"), endpointcert.Server); !ok || !na.Equal(srv.Certificate().NotAfter) {
		t.Fatalf("certify 2.2 = %v, %v; the handshake was not recorded under the lane", na, ok)
	}

	fixture := &shnsdk.OperationValidator{BaseURL: srv.URL, Client: &http.Client{Transport: certRoundTrip(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("fixture") })}}
	TagValidatorClient(fixture, rec, endpointcert.Lane(endpointcert.Validator, "2.0"))
	if got, _, ok := endpointcert.Unwrap(fixture.Client.Transport); !ok {
		t.Fatal("a fixture transport was not wrapped")
	} else if _, same := got.(certRoundTrip); !same {
		t.Fatal("a fixture transport was replaced")
	}
	TagValidatorClient(nil, rec, endpointcert.Lane(endpointcert.Validator, "2.0")) // must not panic
}

// MON-R25: the consent check goes through ConsentClient when it is set,
// and through the Hub's Client when it is not (as before).
func TestMONR25ConsentCheckUsesItsOwnClient(t *testing.T) {
	consent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"permit":true,"consentRef":"ref-1"}`)
	}))
	defer consent.Close()
	id, err := shnsdk.GenerateIdentity("h-consent")
	if err != nil {
		t.Fatal(err)
	}
	refuse := &http.Client{Transport: certRoundTrip(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("the Hub client was used") })}
	g := &Gateway{cfg: Config{HolderID: "h-consent", Identity: id, Clock: time.Now, ConsentURL: consent.URL, Client: refuse, ConsentClient: consent.Client()}}
	if ref, status, msg := g.consentBackstop(t.Context(), "pci-1", "requester"); ref != "ref-1" || status != 0 {
		t.Fatalf("consentBackstop = %q, %d, %q; want the consent service's permit through ConsentClient", ref, status, msg)
	}
	g.cfg.ConsentClient = nil
	if _, status, _ := g.consentBackstop(t.Context(), "pci-1", "requester"); status != http.StatusBadGateway {
		t.Fatalf("with no ConsentClient status = %d, want the Hub client's failure (502)", status)
	}

}

// MON-R25: the token request goes through AuthzClient when it is set, and
// through the Hub's Client when it is not (as before).
func TestMONR25TokenRequestUsesItsOwnClient(t *testing.T) {
	authz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // reached: the denial sentinel
	}))
	defer authz.Close()
	id, err := shnsdk.GenerateIdentity("h-authz")
	if err != nil {
		t.Fatal(err)
	}
	refuse := &http.Client{Transport: certRoundTrip(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("the Hub client was used") })}
	g := &Gateway{cfg: Config{HolderID: "h-authz", Identity: id, Clock: time.Now, AuthzURL: authz.URL, Client: refuse, AuthzClient: authz.Client()}}
	if _, _, _, err := g.authorizeOnce(t.Context(), []byte(`{}`)); !errors.Is(err, errAuthorizationDenied) {
		t.Fatalf("authorizeOnce = %v; want the Framework's denial through AuthzClient", err)
	}
	g.cfg.AuthzClient = nil
	if _, _, _, err := g.authorizeOnce(t.Context(), []byte(`{}`)); !errors.Is(err, errAuthzUnreachable) {
		t.Fatalf("with no AuthzClient = %v; want the Hub client's failure", err)
	}
}

// MON-R25: a CRD request's fhirServer read records the server certificate
// under crd-fhir-server through CertRecorder, after the read's own
// verification against its roots; with no recorder it records nothing and
// reads as before.
func TestMONR25FHIRServerReadRecordsItsServersCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		fmt.Fprint(w, `{"resourceType":"Bundle","type":"searchset","entry":[]}`)
	}))
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	rec := endpointcert.New()
	r := fhirServerReader{mode: FHIRServerReadPrivate, roots: roots, certs: rec,
		resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		}}
	// The read keeps its own guarded transport, unchanged, inside the
	// wrapper: no proxy, no reuse, its header bound, its roots, its checking
	// dialer, and no hook. It is never compared with http.DefaultTransport.
	wrapped, tag, ok := endpointcert.Unwrap(r.client().Transport)
	tr, isTransport := wrapped.(*http.Transport)
	if !ok || tag != endpointcert.Of(endpointcert.CRDFHIRServer) || !isTransport || tr.Proxy != nil || !tr.DisableKeepAlives || tr.MaxResponseHeaderBytes != 64<<10 || tr.DialContext == nil ||
		tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs != roots || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || tr.TLSClientConfig.VerifyConnection != nil {
		t.Fatalf("the fhirServer read's transport is not its own guarded one, wrapped: %v %+v", tag, wrapped)
	}
	r.read(context.Background(), fhirServerRead{base: "https://example.com/fhir", patient: "p"})
	if na, ok := rec.Soonest(endpointcert.Of(endpointcert.CRDFHIRServer), endpointcert.Server); !ok || !na.Equal(srv.Certificate().NotAfter) {
		t.Fatalf("crd-fhir-server = %v, %v; want the server certificate's NotAfter", na, ok)
	}

	// An untrusted server is refused exactly as before and records nothing.
	rec2 := endpointcert.New()
	r.roots, r.certs = x509.NewCertPool(), rec2
	if _, _, _, status, _ := r.read(context.Background(), fhirServerRead{base: "https://example.com/fhir", patient: "p"}); status == http.StatusOK {
		t.Fatal("an untrusted server was read")
	}
	if _, ok := rec2.Soonest(endpointcert.Of(endpointcert.CRDFHIRServer), endpointcert.Server); ok {
		t.Fatal("a refused handshake was recorded")
	}
}

// certRoundTrip is a RoundTripper fixture.
type certRoundTrip func(*http.Request) (*http.Response, error)

func (f certRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// MON-R25: a lane's own metadata check records under the lane client's
// kind (a routing validator's, or a certification client's inner
// transport's), over a transport that is as it was: the default transport's
// Clone without keep-alives.
func TestMONR25LaneChecksRecordUnderTheirLanesKind(t *testing.T) {
	rec := endpointcert.New()
	routing := shnsdk.NewOperationValidator("https://lane.test/fhir")
	TagValidatorClient(routing, rec, endpointcert.Lane(endpointcert.Validator, "2.1"))
	gated := NewGatedCertificationValidator(NewDiscoveredLane("2.2", "https://lane.test/fhir", nil), nil, "unconfigured")
	TagValidatorClient(gated.CertificationClient(), rec, endpointcert.Lane(endpointcert.Certify, "2.2"))

	for _, c := range []struct {
		v    *shnsdk.OperationValidator
		want endpointcert.Tag
	}{
		{routing, endpointcert.Lane(endpointcert.Validator, "2.1")},
		{gated.CertificationClient(), endpointcert.Lane(endpointcert.Certify, "2.2")},
	} {
		base, tag, ok := endpointcert.Unwrap(ownConnectionClient(c.v).Transport)
		tr, isTransport := base.(*http.Transport)
		if !ok || tag != c.want {
			t.Fatalf("the check records under %v (%v), want %v", tag, ok, c.want)
		}
		if !isTransport || !tr.DisableKeepAlives || tr.ForceAttemptHTTP2 || tr.MaxConnsPerHost != 0 || (tr.TLSClientConfig != nil && tr.TLSClientConfig.VerifyConnection != nil) {
			t.Fatalf("%v: the check's transport is not the default's Clone without keep-alives", c.want)
		}
	}
	if _, _, ok := endpointcert.Unwrap(ownConnectionClient(nil).Transport); ok {
		t.Fatal("a check with no lane client records under a kind")
	}
}

// MON-R25: routing's attempts on a default lane and a certification
// client's own attempts each mark their context with their side's kind for
// the line, so the one qualifier the gateway gives both records each
// attempt's certificate where it belongs.
func TestMONR25QualificationAttemptsMarkTheirSide(t *testing.T) {
	seen := make(chan endpointcert.Tag, 16)
	q := func(ctx context.Context, _, _ string) error {
		tag, _ := endpointcert.TagOf(ctx)
		seen <- tag
		return fmt.Errorf("not ready")
	}
	lane := NewDiscoveredLane("2.1", "https://lane.test/fhir", nil)
	_ = lane.Qualify(t.Context(), q)
	if got := <-seen; got != endpointcert.Lane(endpointcert.Validator, "2.1") {
		t.Fatalf("routing's attempt carried %v, want validator 2.1", got)
	}

	gated := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator("https://cert.test/fhir"), q)
	gated.Start()
	defer gated.Close()
	if got := <-seen; got != endpointcert.Lane(endpointcert.Certify, "2.2") {
		t.Fatalf("certification's attempt carried %v, want certify 2.2", got)
	}
}
