package endpointcert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The closed lists are the catalogue's: a rename or a dropped value is a
// change to an exported attribute, so it fails here first.
func TestMONR25KindAndRoleListsArePinned(t *testing.T) {
	wantKinds := []string{
		"ingress", "metrics-pull", "otlp",
		"fhir", "fhir-token", "payer-davinci", "payer-dtr", "payer-pas", "payer-eligibility", "payer-token", "cds",
		"populate", "populate-token", "crd-fhir-server",
		"discovery", "hub", "hub-key", "authorization", "authorization-key", "registrar", "consent", "audit",
		"validator", "certify",
		"diagnostic", "store",
		"trust-root",
	}
	if got := KindNames(); !slices.Equal(got, wantKinds) {
		t.Fatalf("KindNames() = %q, want %q", got, wantKinds)
	}
	if got, want := RoleNames(), []string{"self", "server", "trust-root"}; !slices.Equal(got, want) {
		t.Fatalf("RoleNames() = %q, want %q", got, want)
	}
	for _, k := range Kinds() {
		if !k.Valid() {
			t.Errorf("declared kind %q is not Valid", k)
		}
	}
	if (Kind{}).Valid() || (Role{}).Valid() {
		t.Fatal("the zero Kind or Role is Valid")
	}
}

func leaf(notAfter time.Time, dns ...string) *x509.Certificate {
	return &x509.Certificate{NotAfter: notAfter, DNSNames: dns}
}

func TestMONR25RecordReplacesAnEndpointAndSoonestIsTheMinimum(t *testing.T) {
	r := New()
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, ok := r.Soonest(Of(Hub), Server); ok {
		t.Fatal("Soonest before any record: ok, want unknown")
	}
	r.Record(Of(Hub), Server, "a", day.Add(10*24*time.Hour))
	r.Record(Of(Hub), Server, "b", day.Add(30*24*time.Hour))
	if got, ok := r.Soonest(Of(Hub), Server); !ok || !got.Equal(day.Add(10*24*time.Hour)) {
		t.Fatalf("Soonest = %v, %v; want the earlier endpoint", got, ok)
	}
	// Endpoint a renews: its new certificate replaces the old expiry.
	r.Record(Of(Hub), Server, "a", day.Add(90*24*time.Hour))
	if got, _ := r.Soonest(Of(Hub), Server); !got.Equal(day.Add(30 * 24 * time.Hour)) {
		t.Fatalf("after a's renewal Soonest = %v, want b's", got)
	}
	// Kinds and roles are separate series.
	if _, ok := r.Soonest(Of(Registrar), Server); ok {
		t.Fatal("another kind reports hub's record")
	}
	if _, ok := r.Soonest(Of(Hub), Self); ok {
		t.Fatal("another role reports hub's server record")
	}
}

func TestMONR25RecordIgnoresUndeclaredValues(t *testing.T) {
	r := New()
	r.Record(Of(Kind{"made-up"}), Server, "a", time.Now())
	r.Record(Of(Hub), Role{"made-up"}, "a", time.Now())
	r.Record(Of(Hub), Server, "", time.Now())
	if len(r.entries) != 0 {
		t.Fatalf("undeclared values recorded: %v", r.entries)
	}
	var nilR *Recorder
	if nilR.Wrap(Of(Hub), nil) != nil {
		t.Fatal("nil Recorder's Wrap of nil is not nil")
	}
	if c := nilR.Client(Of(Hub), &http.Client{Timeout: time.Second}); c.Transport != nil || c.Timeout != time.Second {
		t.Fatalf("nil Recorder's Client changed the client: %+v", c)
	}
	base := &http.Transport{}
	if New().Wrap(Tag{}, base) != http.RoundTripper(base) {
		t.Fatal("an invalid tag wrapped a transport")
	}
	nilR.Record(Of(Hub), Server, "a", time.Now()) // must not panic
	if _, ok := nilR.Soonest(Of(Hub), Server); ok {
		t.Fatal("nil Recorder reports a value")
	}
}

func TestMONR25EndpointsPerSeriesAreBounded(t *testing.T) {
	r := New()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	// Endpoint 0 carries the soonest expiry and is the one seen longest ago.
	for i := 0; i <= MaxEndpointsPerSeries; i++ {
		r.Record(Of(CRDFHIRServer), Server, fmt.Sprintf("e%d", i), base.Add(time.Duration(i+1)*time.Hour))
	}
	if n := len(r.entries[series{Of(CRDFHIRServer), Server}]); n != MaxEndpointsPerSeries {
		t.Fatalf("%d endpoints held, want %d", n, MaxEndpointsPerSeries)
	}
	if got, _ := r.Soonest(Of(CRDFHIRServer), Server); !got.Equal(base.Add(2 * time.Hour)) {
		t.Fatalf("Soonest = %v; the endpoint seen longest ago was not the one forgotten", got)
	}
	// A re-recorded endpoint counts as seen now, and a full table takes it without eviction.
	r.Record(Of(CRDFHIRServer), Server, "e1", base.Add(2*time.Hour))
	r.Record(Of(CRDFHIRServer), Server, "new", base.Add(100*time.Hour))
	if got, _ := r.Soonest(Of(CRDFHIRServer), Server); !got.Equal(base.Add(2 * time.Hour)) {
		t.Fatalf("Soonest = %v: re-recording e1 did not refresh its place", got)
	}
}

func TestMONR25HookRunsAfterThePriorHookAndNeverFails(t *testing.T) {
	r := New()
	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	refuse := errors.New("prior hook refuses")
	if err := r.VerifyConnection(Of(Hub), func(tls.ConnectionState) error { return refuse })(tls.ConnectionState{ServerName: "h", PeerCertificates: []*x509.Certificate{leaf(exp)}}); !errors.Is(err, refuse) {
		t.Fatalf("hook returned %v, want the prior hook's error unchanged", err)
	}
	if _, ok := r.Soonest(Of(Hub), Server); ok {
		t.Fatal("a connection the prior hook refused was recorded")
	}
	if err := r.VerifyConnection(Of(Hub), nil)(tls.ConnectionState{}); err != nil {
		t.Fatalf("hook with no peer certificate = %v, want nil", err)
	}
	if err := r.VerifyConnection(Of(Hub), nil)(tls.ConnectionState{ServerName: "h", PeerCertificates: []*x509.Certificate{leaf(exp)}}); err != nil {
		t.Fatalf("hook = %v, want nil", err)
	}
	if got, ok := r.Soonest(Of(Hub), Server); !ok || !got.Equal(exp) {
		t.Fatalf("Soonest = %v, %v; want the leaf's NotAfter", got, ok)
	}
}

func TestMONR25EndpointNameKeysAnIPLiteralBySANs(t *testing.T) {
	exp := time.Now()
	a := &x509.Certificate{NotAfter: exp, IPAddresses: []net.IP{net.ParseIP("10.0.0.1")}}
	b := &x509.Certificate{NotAfter: exp, IPAddresses: []net.IP{net.ParseIP("10.0.0.2")}}
	if endpointName("", a) == endpointName("", b) {
		t.Fatal("two IP endpoints share a key")
	}
	if endpointName("", a) != endpointName("", &x509.Certificate{IPAddresses: a.IPAddresses}) {
		t.Fatal("a renewal of the same IP endpoint gets a new key")
	}
	if endpointName("h.example", a) == endpointName("", a) {
		t.Fatal("a named endpoint is keyed by its SANs")
	}
	if endpointName("", &x509.Certificate{}) == "san:" {
		t.Fatal("a leaf with no SANs gets an empty key")
	}
}

func TestMONR25LaneTagsCarryAnIGLineOnlyForValidatorAndCertify(t *testing.T) {
	for _, tc := range []struct {
		tag  Tag
		want bool
	}{
		{Lane(Validator, "2.1"), true},
		{Lane(Certify, "2.2"), true},
		{Of(Validator), false},
		{Of(Certify), false},
		{Lane(Hub, "2.1"), false},
		{Of(Hub), true},
		{Tag{}, false},
	} {
		if got := tc.tag.Valid(); got != tc.want {
			t.Errorf("%v Valid = %v, want %v", tc.tag, got, tc.want)
		}
	}
	r := New()
	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	r.Record(Lane(Validator, "2.1"), Server, "v21", exp)
	r.Record(Lane(Validator, "2.2"), Server, "v22", exp.Add(time.Hour))
	r.Record(Of(Validator), Server, "bare", exp) // no line: not recorded
	if got, ok := r.Soonest(Lane(Validator, "2.2"), Server); !ok || !got.Equal(exp.Add(time.Hour)) {
		t.Fatalf("2.2 lane = %v, %v; the lanes are not separate series", got, ok)
	}
	r.Record(Of(Hub), Server, "h", exp.Add(2*time.Hour))
	r.Record(Of(Hub), Self, "h", exp.Add(3*time.Hour))
	want := []Reading{
		{Of(Hub), Self, exp.Add(3 * time.Hour)},
		{Of(Hub), Server, exp.Add(2 * time.Hour)},
		{Lane(Validator, "2.1"), Server, exp},
		{Lane(Validator, "2.2"), Server, exp.Add(time.Hour)},
	}
	if got := r.Readings(); !slices.Equal(got, want) {
		t.Fatalf("Readings = %v, want %v (catalogue order: kind, IG line, role)", got, want)
	}
	var nilR *Recorder
	if nilR.Readings() != nil {
		t.Fatal("nil Recorder has readings")
	}
}

// Wrap changes nothing about verification or the connection: an untrusted
// server fails exactly as through base and records nothing; a trusted one
// records its certificate from the response, including on a reused
// connection, and the request goes through base itself.
func TestMONR25WrapRecordsFromTheResponseAndLeavesVerificationUnchanged(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()
	trusted := srv.Client().Transport.(*http.Transport)

	untrusted := trusted.Clone()
	untrusted.TLSClientConfig = &tls.Config{RootCAs: x509.NewCertPool()}
	r := New()
	_, plainErr := (&http.Client{Transport: untrusted}).Get(srv.URL)
	_, taggedErr := (&http.Client{Transport: r.Wrap(Of(Hub), untrusted)}).Get(srv.URL)
	var pv, tv *tls.CertificateVerificationError
	if !errors.As(plainErr, &pv) || !errors.As(taggedErr, &tv) || pv.Err.Error() != tv.Err.Error() {
		t.Fatalf("untrusted server: plain %v, tagged %v; want the same verification error from both", plainErr, taggedErr)
	}
	if _, ok := r.Soonest(Of(Hub), Server); ok {
		t.Fatal("a refused handshake was recorded")
	}

	conns.Store(0)
	c := &http.Client{Transport: r.Wrap(Of(Hub), trusted)}
	for i := 0; i < 2; i++ {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("trusted server through the wrapper: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if i == 0 {
			r = New() // the second answer, on the reused connection, must record on its own
			c.Transport = r.Wrap(Of(Hub), trusted)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("%d connections, want the one base keeps alive", n)
	}
	if got, ok := r.Soonest(Of(Hub), Server); !ok || !got.Equal(srv.Certificate().NotAfter) {
		t.Fatalf("Soonest = %v, %v; want the server certificate's NotAfter from the reused connection's answer", got, ok)
	}
}

// Wrap keeps base itself: Unwrap returns the very transport (nil for
// http.DefaultTransport, read at each request), Client keeps the timeout and
// redirect policy, and a plain-HTTP answer records nothing.
func TestMONR25WrapKeepsTheTransportItWraps(t *testing.T) {
	r := New()
	base := &http.Transport{MaxIdleConns: 7}
	if got, tag, ok := Unwrap(r.Wrap(Of(Hub), base)); !ok || got != http.RoundTripper(base) || tag != Of(Hub) {
		t.Fatalf("Unwrap = %v, %v, %v; want base itself and hub", got, tag, ok)
	}
	if got, _, ok := Unwrap(r.Wrap(Of(Hub), nil)); !ok || got != nil {
		t.Fatal("a wrapper of the default transport does not keep nil")
	}
	if _, _, ok := Unwrap(base); ok {
		t.Fatal("an unwrapped transport reads as wrapped")
	}

	// nil means http.DefaultTransport at each request, as it did for the client.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	old := http.DefaultTransport
	used := false
	http.DefaultTransport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		used = true
		return old.RoundTrip(req)
	})
	defer func() { http.DefaultTransport = old }()
	resp, err := (&http.Client{Transport: r.Wrap(Of(Hub), nil)}).Get(srv.URL)
	if err != nil || !used {
		t.Fatalf("a nil base did not send through http.DefaultTransport as it is now: %v", err)
	}
	resp.Body.Close()
	if _, ok := r.Soonest(Of(Hub), Server); ok {
		t.Fatal("a plain-HTTP answer recorded a certificate")
	}

	redirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	bc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: redirect}
	cc := r.Client(Of(Consent), bc)
	if cc == bc || cc.Timeout != 30*time.Second || cc.CheckRedirect == nil || bc.Transport != nil {
		t.Fatalf("Client = %+v (base %+v)", cc, bc)
	}
	if got, tag, ok := Unwrap(cc.Transport); !ok || got != nil || tag != Of(Consent) {
		t.Fatalf("client transport = %v, %v, %v", got, tag, ok)
	}

	// Rewrap: a client built from another's settings records under its tag.
	other := &http.Transport{}
	if got, tag, ok := Unwrap(Rewrap(cc.Transport, other)); !ok || got != http.RoundTripper(other) || tag != Of(Consent) {
		t.Fatal("Rewrap did not keep the tag over the new base")
	}
	if Rewrap(base, other) != http.RoundTripper(other) {
		t.Fatal("Rewrap of an unwrapped transport is not the new base")
	}

	// CloseIdleConnections reaches base.
	closed := &closeCounter{}
	r.Wrap(Of(Hub), closed).(interface{ CloseIdleConnections() }).CloseIdleConnections()
	if closed.n != 1 {
		t.Fatal("CloseIdleConnections did not reach base")
	}
}

type closeCounter struct{ n int }

func (c *closeCounter) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unused")
}
func (c *closeCounter) CloseIdleConnections() { c.n++ }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A routed wrapper records a request marked with one of its extra tags
// under that tag and every other request under its default, all through
// the one base and its one pool.
func TestMONR25WrapRoutedRecordsEachRequestUnderItsMark(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()
	r := New()
	base := srv.Client().Transport
	c := &http.Client{Transport: r.WrapRouted(Of(PayerDavinci), base, Of(CDS))}
	get := func(ctx context.Context) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	get(context.Background())
	if _, ok := r.Soonest(Of(CDS), Server); ok {
		t.Fatal("an unmarked request was recorded as cds")
	}
	if _, ok := r.Soonest(Of(PayerDavinci), Server); !ok {
		t.Fatal("an unmarked request was not recorded under the default")
	}
	get(WithTag(context.Background(), Of(CDS)))
	if _, ok := r.Soonest(Of(CDS), Server); !ok {
		t.Fatal("a cds-marked request was not recorded as cds")
	}
	// A mark outside the routed set is recorded under none: neither that
	// kind nor folded into the default.
	r2 := New()
	c.Transport = r2.WrapRouted(Of(PayerDavinci), base, Of(CDS))
	get(WithTag(context.Background(), Of(Hub)))
	if _, ok := r2.Soonest(Of(Hub), Server); ok {
		t.Fatal("a kind outside the routed set was recorded")
	}
	if _, ok := r2.Soonest(Of(PayerDavinci), Server); ok {
		t.Fatal("a request marked with another kind was folded into the default")
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("%d connections, want every kind on base's one pool", n)
	}
	if got := r.Made(); !slices.Equal(got, []Tag{Of(PayerDavinci), Of(CDS)}) {
		t.Fatalf("Made = %v", got)
	}
}

// A token client a bearer layer calls under its operation's mark records
// under its own kind (WrapOwn); a plain Wrap of the same request records
// under none, since the mark is not its kind.
func TestMONR25WrapOwnRecordsUnderItsOwnKindWhateverTheMark(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	for _, c := range []struct {
		own  bool
		want bool
	}{{true, true}, {false, false}} {
		r := New()
		rt := r.Wrap(Of(PayerToken), srv.Client().Transport)
		if c.own {
			rt = r.WrapOwn(Of(PayerToken), srv.Client().Transport)
		}
		req, _ := http.NewRequestWithContext(WithTag(context.Background(), Of(PayerPAS)), http.MethodGet, srv.URL, nil)
		resp, err := (&http.Client{Transport: rt}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if _, ok := r.Soonest(Of(PayerToken), Server); ok != c.want {
			t.Fatalf("own=%v: payer-token recorded %v, want %v", c.own, ok, c.want)
		}
		if _, ok := r.Soonest(Of(PayerPAS), Server); ok {
			t.Fatalf("own=%v: the token request was recorded under the operation's kind", c.own)
		}
	}
	if New().WrapOwn(Tag{}, nil) != nil {
		t.Fatal("WrapOwn of an invalid tag wrapped")
	}
}

// A reading stands for MaxAge without a new response from its endpoint, then
// ages out on the Recorder's clock and its kind reads as unknown. The
// boot-read kinds, and the gateway's own and trust-root certificates, keep
// what was read.
func TestMONR25ReadingsAgeOutAfterADayWithoutAResponse(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	r := NewWithClock(func() time.Time { return now })
	exp := now.Add(30 * 24 * time.Hour)
	r.Record(Of(CRDFHIRServer), Server, "ehr-a", exp)
	now = now.Add(12 * time.Hour)
	r.Record(Of(CRDFHIRServer), Server, "ehr-b", exp.Add(time.Hour))
	for _, boot := range []Tag{Of(Discovery), Of(HubKey), Of(AuthorizationKey)} {
		r.Record(boot, Server, "boot", exp)
	}
	r.Record(Of(Ingress), Self, "TLS_CERT_FILE", exp)

	now = now.Add(12*time.Hour - time.Nanosecond) // ehr-a: one nanosecond inside its day
	if got, ok := r.Soonest(Of(CRDFHIRServer), Server); !ok || !got.Equal(exp) {
		t.Fatalf("inside the day Soonest = %v, %v; want ehr-a's", got, ok)
	}
	now = now.Add(time.Nanosecond) // ehr-a's day is up; ehr-b's is not
	if got, ok := r.Soonest(Of(CRDFHIRServer), Server); !ok || !got.Equal(exp.Add(time.Hour)) {
		t.Fatalf("after ehr-a aged out Soonest = %v, %v; want ehr-b's", got, ok)
	}
	now = now.Add(12 * time.Hour)
	if _, ok := r.Soonest(Of(CRDFHIRServer), Server); ok {
		t.Fatal("an endpoint with no response for a day still reports")
	}
	for _, rd := range r.Readings() {
		if rd.Tag == Of(CRDFHIRServer) {
			t.Fatal("Readings lists an aged-out series")
		}
	}
	for _, boot := range []Tag{Of(Discovery), Of(HubKey), Of(AuthorizationKey)} {
		if _, ok := r.Soonest(boot, Server); !ok {
			t.Fatalf("boot-read %v aged out", boot)
		}
	}
	if _, ok := r.Soonest(Of(Ingress), Self); !ok {
		t.Fatal("the gateway's own certificate aged out")
	}
	// A new response renews the reading's day.
	r.Record(Of(CRDFHIRServer), Server, "ehr-a", exp)
	now = now.Add(MaxAge - time.Second)
	if _, ok := r.Soonest(Of(CRDFHIRServer), Server); !ok {
		t.Fatal("a fresh response did not renew the reading")
	}
}

func TestMONR25RecorderIsSafeForConcurrentUse(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.Record(Of(Hub), Server, fmt.Sprintf("e%d", (i*j)%40), time.Unix(int64(i*j), 0))
				r.Soonest(Of(Hub), Server)
				r.Wrap(Of(Audit), nil)
			}
		}(i)
	}
	wg.Wait()
}
