package engine

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The address classifier, one row per range: whether no mode reads it
// (neverRead) and whether the public mode does (publicAddress). The private
// mode, the default, reads every address that is not never-read.
func TestFHIRServerAddressClassifier(t *testing.T) {
	type want struct{ never, public bool }
	never, private, public := want{true, false}, want{false, false}, want{false, true}
	for addr, w := range map[string]want{
		// No mode reads: loopback, unspecified, "this network", link-local
		// (the instance metadata, the ECS credential endpoint, the VPC
		// resolver), the EC2 IPv6 metadata endpoint, multicast, broadcast,
		// reserved, a zone, and a mapped, compatible, NAT64 or 6to4 address
		// that embeds one of them.
		"127.0.0.1": never, "::1": never, "0.0.0.0": never, "::": never, "0.1.2.3": never,
		"169.254.169.254": never, "169.254.170.2": never, "169.254.169.253": never, "fe80::1": never,
		"fd00:ec2::254": never, "fd00:ec2::23": never, "fd20:ce::254": never, "100.100.100.200": never, "224.0.0.1": never, "ff02::1": never, "255.255.255.255": never, "240.0.0.1": never,
		"2606:4700:4700::1111%en0": never, "::ffff:127.0.0.1": never, "::ffff:169.254.169.254": never, "::7f00:1": never,
		"64:ff9b::7f00:1": never, "64:ff9b::a9fe:a9fe": never, "64:ff9b:1:7f00:0:100::": never,
		"2002:7f00:1::": never, "2002:a9fe:a9fe::1": never,
		// The private mode reads, the public mode does not: RFC 1918, ULA,
		// CGNAT, documentation and benchmark ranges, and NAT64 or 6to4 of a
		// readable address.
		"10.0.0.5": private, "172.16.0.1": private, "192.168.1.1": private, "fc00::1": private, "fd12:3456::1": private,
		"100.64.0.1": private, "::ffff:10.0.0.5": private, "192.0.0.8": private, "192.0.2.1": private,
		"198.18.0.1": private, "198.51.100.1": private, "203.0.113.1": private, "2001:db8::1": private, "100::1": private,
		"64:ff9b::a00:5": private, "64:ff9b:1:a00:0:500::": private, "2002:a00:1::1": private, "64:ff9b::808:808": private,
		// Both read: public addresses.
		"8.8.8.8": public, "93.184.216.34": public, "100.63.255.255": public, "100.128.0.0": public,
		"172.32.0.1": public, "192.0.1.1": public, "2606:4700:4700::1111": public, "::ffff:8.8.8.8": public,
	} {
		a := netip.MustParseAddr(addr)
		if got := (want{neverRead(a), publicAddress(a)}); got != w {
			t.Errorf("%s: never read %t, public %t; want %t, %t", addr, got.never, got.public, w.never, w.public)
		}
	}
}

// The port a mode reads at an address: 443 alone in the public mode; in the
// private mode any port at a private address and 443 at a public one.
func TestFHIRServerReadablePort(t *testing.T) {
	pub, priv := fhirServerReader{mode: FHIRServerReadPublic}, fhirServerReader{mode: FHIRServerReadPrivate}
	for _, row := range []struct {
		r          fhirServerReader
		addr, port string
		msg        string
	}{
		{pub, "93.184.216.34", "443", ""},
		{pub, "93.184.216.34", "", ""},
		{pub, "93.184.216.34", "8443", fhirServerNotPort443},
		{pub, "10.0.0.5", "443", fhirServerNotPublic},
		{pub, "127.0.0.1", "443", fhirServerNotPublic},
		{priv, "10.0.0.5", "8443", ""},
		{priv, "fd12:3456::1", "9443", ""},
		{priv, "93.184.216.34", "443", ""},
		{priv, "93.184.216.34", "8443", fhirServerPublicNot443},
		{priv, "64:ff9b::808:808", "8443", fhirServerPublicNot443},
		{priv, "2002:808:808::1", "8443", fhirServerPublicNot443},
		{priv, "64:ff9b::a00:5", "8443", ""},
		{priv, "127.0.0.1", "8443", fhirServerNeverRead},
		{priv, "169.254.169.254", "80", fhirServerNeverRead},
	} {
		if ok, msg := row.r.readable(netip.MustParseAddr(row.addr), row.port); ok != (row.msg == "") || msg != row.msg {
			t.Errorf("%s %s:%s: %t %q, want %q", row.r.mode, row.addr, row.port, ok, msg, row.msg)
		}
	}
}

// The request's own values, checked before anything is resolved or dialed.
func TestFHIRServerReadRefusesTheRequestsValues(t *testing.T) {
	public := fhirServerReader{mode: FHIRServerReadPublic}
	private := fhirServerReader{mode: FHIRServerReadPrivate}
	bearer := fhirServerAuth{present: true, token: "t", tokenType: "Bearer"}
	for name, row := range map[string]struct {
		r    fhirServerReader
		base string
		auth fhirServerAuth
		msg  string
	}{
		"not a URL":                             {public, "%zz", bearer, fhirServerNotBase},
		"relative":                              {public, "/fhir", bearer, fhirServerNotBase},
		"opaque":                                {public, "https:ehr.example", bearer, fhirServerNotBase},
		"a query":                               {public, "https://ehr.example/fhir?x=1", bearer, fhirServerNotBase},
		"an empty query":                        {public, "https://ehr.example/fhir?", bearer, fhirServerNotBase},
		"a fragment":                            {public, "https://ehr.example/fhir#x", bearer, fhirServerNotBase},
		"http":                                  {public, "http://ehr.example/fhir", bearer, fhirServerNotHTTPS},
		"http, private mode":                    {private, "http://ehr.example/fhir", bearer, fhirServerNotHTTPS},
		"another scheme":                        {public, "ftp://ehr.example/fhir", bearer, fhirServerNotHTTPS},
		"credentials":                           {public, "https://u:p@ehr.example/fhir", bearer, fhirServerCredentials},
		"credentials, private mode":             {private, "https://u@ehr.example/fhir", bearer, fhirServerCredentials},
		"another port":                          {public, "https://ehr.example:8443/fhir", bearer, fhirServerNotPort443},
		"a port out of range, private mode":     {private, "https://ehr.example:70000/fhir", bearer, fhirServerNotBase},
		"port zero, private mode":               {private, "https://10.0.0.5:0/fhir", bearer, fhirServerNotBase},
		"a private literal":                     {public, "https://10.0.0.5/fhir", bearer, fhirServerNotPublic},
		"a loopback literal":                    {public, "https://127.0.0.1/fhir", bearer, fhirServerNotPublic},
		"a metadata literal":                    {public, "https://169.254.169.254/latest", bearer, fhirServerNotPublic},
		"an IPv6 private":                       {public, "https://[fd00:ec2::254]/fhir", bearer, fhirServerNotPublic},
		"a mapped private":                      {public, "https://[::ffff:10.0.0.5]/fhir", bearer, fhirServerNotPublic},
		"a loopback literal, private mode":      {private, "https://127.0.0.1:8443/fhir", bearer, fhirServerNeverRead},
		"a metadata literal, private mode":      {private, "https://169.254.169.254/latest", bearer, fhirServerNeverRead},
		"an EC2 IPv6 metadata literal":          {private, "https://[fd00:ec2::254]/fhir", bearer, fhirServerNeverRead},
		"a NAT64 of loopback, private mode":     {private, "https://[64:ff9b::7f00:1]/fhir", bearer, fhirServerNeverRead},
		"a public literal on another port":      {private, "https://93.184.216.34:8443/fhir", bearer, fhirServerPublicNot443},
		"a token of another type, private mode": {private, "https://ehr.example/fhir", fhirServerAuth{present: true, token: "t", tokenType: "MAC"}, fhirServerBadToken},
		"a token of another type": {public, "https://ehr.example/fhir",
			fhirServerAuth{present: true, token: "t", tokenType: "MAC"}, fhirServerBadToken},
		"no token":                         {public, "https://ehr.example/fhir", fhirServerAuth{present: true, tokenType: "Bearer"}, fhirServerBadToken},
		"a token too long":                 {public, "https://ehr.example/fhir", fhirServerAuth{present: true, token: strings.Repeat("a", fhirServerMaxToken+1), tokenType: "Bearer"}, fhirServerBadToken},
		"a token with a newline":           {public, "https://ehr.example/fhir", fhirServerAuth{present: true, token: "a\r\nX-Evil: 1", tokenType: "bearer"}, fhirServerBadToken},
		"a token with a control character": {public, "https://ehr.example/fhir", fhirServerAuth{present: true, token: "a\x01b", tokenType: "Bearer"}, fhirServerBadToken},
	} {
		t.Run(name, func(t *testing.T) {
			dialed := false
			row.r.dialed = func(netip.AddrPort) { dialed = true }
			row.r.resolve = func(context.Context, string) ([]netip.Addr, error) {
				t.Fatal("resolved a value the request's own check refuses")
				return nil, nil
			}
			_, _, _, status, msg := row.r.read(context.Background(), fhirServerRead{base: row.base, auth: row.auth, patient: "p"})
			if status != http.StatusPreconditionFailed || msg != row.msg || dialed {
				t.Fatalf("%d %q (dialed %t), want 412 %q before any dial", status, msg, dialed, row.msg)
			}
		})
	}
	// Accepted: the default port, a path, a trailing slash, no token at all,
	// and in the private mode a name on another port (checked once resolved),
	// a private address on any port, and a public one on 443.
	for _, row := range []struct {
		r    fhirServerReader
		base string
		auth fhirServerAuth
	}{
		{public, "https://ehr.example/api/FHIR/R4/", bearer},
		{public, "https://ehr.example:443/fhir", fhirServerAuth{}},
		{private, "https://ehr.internal:8443/fhir", bearer},
		{private, "https://10.0.0.5:8443/fhir", bearer},
		{private, "https://[fd12:3456::1]:9443/fhir", bearer},
		{private, "https://93.184.216.34/fhir", bearer},
	} {
		u, status, msg := row.r.target(fhirServerRead{base: row.base, auth: row.auth, patient: "p 1"})
		if status != 0 || !strings.HasSuffix(u.Path, "/Coverage") || u.Query().Get("patient") != "p 1" ||
			u.Query().Has("status") || u.Query().Has("_include") || len(u.Query()) != 1 {
			t.Errorf("%s: %v %d %q", row.base, u, status, msg)
		}
	}
}

// A name must resolve only to addresses the mode reads, and the dial goes to
// the address checked: a name with any address the mode does not read is
// refused before any connection.
func TestFHIRServerReadRefusesAnUnreadableName(t *testing.T) {
	for name, row := range map[string]struct {
		mode  string
		base  string
		addrs []string
		err   bool
		msg   string
	}{
		"public: a private address":            {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"10.0.0.5"}, false, fhirServerNotPublic},
		"public: loopback":                     {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"127.0.0.1"}, false, fhirServerNotPublic},
		"public: a public and a private":       {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"93.184.216.34", "10.0.0.5"}, false, fhirServerNotPublic},
		"public: the instance metadata":        {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"169.254.169.254"}, false, fhirServerNotPublic},
		"public: a NAT64 embedding of private": {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"64:ff9b::a00:5"}, false, fhirServerNotPublic},
		// SHN's own VPCs (platform, audit DR, hosted, partner sim), the VPC
		// resolver, the task credential endpoint and every other metadata
		// address: nothing inside a VPC is read in the public mode.
		"public: the platform VPC":             {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"10.20.0.10"}, false, fhirServerNotPublic},
		"public: the audit DR VPC":             {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"10.21.3.4"}, false, fhirServerNotPublic},
		"public: the hosted VPC":               {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"10.22.1.5"}, false, fhirServerNotPublic},
		"public: the partner-sim VPC":          {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"10.23.1.5"}, false, fhirServerNotPublic},
		"public: 172.16/12":                    {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"172.31.0.2"}, false, fhirServerNotPublic},
		"public: 192.168/16":                   {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"192.168.0.1"}, false, fhirServerNotPublic},
		"public: the VPC resolver":             {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"169.254.169.253"}, false, fhirServerNotPublic},
		"public: the task credential address":  {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"169.254.170.2"}, false, fhirServerNotPublic},
		"public: the IPv6 instance metadata":   {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"fd00:ec2::254"}, false, fhirServerNotPublic},
		"public: the Pod Identity agent":       {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"fd00:ec2::23"}, false, fhirServerNotPublic},
		"public: another cloud's metadata":     {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"100.100.100.200"}, false, fhirServerNotPublic},
		"public: a mapped metadata":            {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"::ffff:169.254.169.254"}, false, fhirServerNotPublic},
		"public: a 6to4 of the metadata":       {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"2002:a9fe:a9fe::1"}, false, fhirServerNotPublic},
		"public: IPv6 loopback":                {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"::1"}, false, fhirServerNotPublic},
		"public: IPv6 link-local":              {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"fe80::1"}, false, fhirServerNotPublic},
		"public: IPv6 unique local":            {FHIRServerReadPublic, "https://ehr.example/fhir", []string{"fd12:3456::1"}, false, fhirServerNotPublic},
		"private: loopback":                    {FHIRServerReadPrivate, "https://ehr.example/fhir", []string{"127.0.0.1"}, false, fhirServerNeverRead},
		"private: the instance metadata":       {FHIRServerReadPrivate, "https://ehr.example/fhir", []string{"169.254.169.254"}, false, fhirServerNeverRead},
		"private: the ECS credential endpoint": {FHIRServerReadPrivate, "https://ehr.example/fhir", []string{"169.254.170.2"}, false, fhirServerNeverRead},
		"private: a private and a loopback":    {FHIRServerReadPrivate, "https://ehr.example:8443/fhir", []string{"10.0.0.5", "::1"}, false, fhirServerNeverRead},
		"private: a NAT64 of loopback":         {FHIRServerReadPrivate, "https://ehr.example/fhir", []string{"64:ff9b::7f00:1"}, false, fhirServerNeverRead},
		"private: a 6to4 of the metadata":      {FHIRServerReadPrivate, "https://ehr.example/fhir", []string{"2002:a9fe:a9fe::1"}, false, fhirServerNeverRead},
		"private: public on another port":      {FHIRServerReadPrivate, "https://ehr.example:8443/fhir", []string{"93.184.216.34"}, false, fhirServerPublicNot443},
		"private: public and private, 8443":    {FHIRServerReadPrivate, "https://ehr.example:8443/fhir", []string{"10.0.0.5", "93.184.216.34"}, false, fhirServerPublicNot443},
		"public: no address":                   {FHIRServerReadPublic, "https://ehr.example/fhir", nil, false, fhirServerNoAddress},
		"private: a resolver error":            {FHIRServerReadPrivate, "https://ehr.example/fhir", nil, true, fhirServerNoAddress},
		"private: a resolver timeout":          {FHIRServerReadPrivate, "https://ehr.example/fhir", []string{"timeout"}, true, fhirServerTimedOut},
	} {
		t.Run(name, func(t *testing.T) {
			var dialed []netip.AddrPort
			r := fhirServerReader{mode: row.mode,
				resolve: func(context.Context, string) ([]netip.Addr, error) {
					if row.err {
						return nil, &net.DNSError{Err: "no such host", Name: "ehr.example", IsTimeout: len(row.addrs) == 1 && row.addrs[0] == "timeout"}
					}
					var out []netip.Addr
					for _, a := range row.addrs {
						out = append(out, netip.MustParseAddr(a))
					}
					return out, nil
				},
				dial: func(context.Context, string, string) (net.Conn, error) {
					t.Error("dialed an address the mode does not read")
					return nil, errors.New("refused")
				},
				dialed: func(ap netip.AddrPort) { dialed = append(dialed, ap) }}
			_, _, _, status, msg := r.read(context.Background(), fhirServerRead{base: row.base, patient: "p"})
			if status != http.StatusPreconditionFailed || msg != row.msg || len(dialed) != 0 {
				t.Fatalf("%d %q, dialed %v; want 412 %q and no dial", status, msg, dialed, row.msg)
			}
		})
	}
}

// The address a connection is actually made to is checked again, just
// before it is made, under the mode's own rules.
func TestFHIRServerDialControl(t *testing.T) {
	for _, row := range []struct {
		mode, addr string
		ok         bool
	}{
		{FHIRServerReadPublic, "93.184.216.34:443", true}, {FHIRServerReadPublic, "[2606:4700:4700::1111]:443", true},
		{FHIRServerReadPublic, "93.184.216.34:8443", false}, {FHIRServerReadPublic, "10.0.0.5:443", false},
		{FHIRServerReadPublic, "127.0.0.1:443", false}, {FHIRServerReadPublic, "169.254.169.254:443", false},
		{FHIRServerReadPublic, "169.254.170.2:443", false}, {FHIRServerReadPublic, "[fd00:ec2::254]:443", false},
		{FHIRServerReadPublic, "10.20.0.10:443", false}, {FHIRServerReadPublic, "10.22.1.5:443", false},
		{FHIRServerReadPrivate, "10.0.0.5:8443", true}, {FHIRServerReadPrivate, "93.184.216.34:443", true},
		{FHIRServerReadPrivate, "93.184.216.34:8443", false}, {FHIRServerReadPrivate, "127.0.0.1:443", false},
		{FHIRServerReadPrivate, "169.254.169.254:80", false}, {FHIRServerReadPrivate, "[::1]:443", false},
		{FHIRServerReadPrivate, "nonsense", false},
	} {
		if err := (fhirServerReader{mode: row.mode}).dialControl("tcp", row.addr, nil); (err == nil) != row.ok {
			t.Errorf("%s %s: %v, want allowed %t", row.mode, row.addr, err, row.ok)
		}
	}
	for _, mode := range []string{FHIRServerReadPublic, FHIRServerReadPrivate} {
		if (fhirServerReader{mode: mode}).dialer().Control == nil {
			t.Errorf("%s: the dialer does not check the connected address", mode)
		}
	}
}

// ehrServer is an EHR's FHIR server over TLS, reached as example.com (the
// test certificate's name). The name resolves to a private address on 8443
// (base) in the private mode and to a public one on 443 (publicBase) in the
// public mode; the dial, past every address check, connects to the local
// server whatever address was checked.
type ehrServer struct {
	srv        *httptest.Server
	base       string
	publicBase string
	dialed     atomic.Value // the address last dialed
	auth       atomic.Value // the Authorization header last seen
	headers    atomic.Value // every header last seen
	clientCert atomic.Bool  // a client certificate was ever presented
	query      atomic.Value
	calls      atomic.Int32
	handler    func(w http.ResponseWriter, r *http.Request)
}

func newEHRServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *ehrServer {
	t.Helper()
	e := &ehrServer{handler: handler}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.calls.Add(1)
		e.auth.Store(r.Header.Get("Authorization"))
		e.headers.Store(r.Header.Clone())
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			e.clientCert.Store(true)
		}
		e.query.Store(r.URL.RequestURI())
		e.handler(w, r)
	}))
	// The server asks for a client certificate, so one the reader held would
	// be presented.
	e.srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	e.srv.StartTLS()
	t.Cleanup(e.srv.Close)
	e.base, e.publicBase = "https://example.com:8443/fhir", "https://example.com/fhir"
	return e
}

func (e *ehrServer) reader(mode string) fhirServerReader {
	roots := x509.NewCertPool()
	roots.AddCert(e.srv.Certificate())
	addr := netip.MustParseAddr("10.20.30.40")
	if mode == FHIRServerReadPublic {
		addr = netip.MustParseAddr("93.184.216.34")
	}
	listener := e.srv.Listener.Addr().String()
	return fhirServerReader{mode: mode, roots: roots,
		resolve: func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{addr}, nil },
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			e.dialed.Store(address)
			var d net.Dialer
			return d.DialContext(ctx, network, listener)
		}}
}

// coverageSearchset is the EHR's answer: the member's Coverage and, included,
// the payer Organization it names.
func coverageSearchset(beneficiary, payerValue string) string {
	return `{"resourceType":"Bundle","type":"searchset","total":1,"entry":[` +
		`{"fullUrl":"https://example.com/fhir/Coverage/c9","resource":{"resourceType":"Coverage","id":"c9","status":"active",` +
		`"beneficiary":{"reference":"Patient/` + beneficiary + `"},"payor":[{"reference":"Organization/o1"}]},"search":{"mode":"match"}},` +
		`{"fullUrl":"https://example.com/fhir/Organization/o1","resource":{"resourceType":"Organization","id":"o1",` +
		`"identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"` + payerValue + `"}]},"search":{"mode":"include"}}]}`
}

func ehrAnswer(status int, contentType, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// A read that succeeds: one GET of the Coverage search with the request's own
// bearer token, through no proxy (the environment's proxy settings are read
// once per process, so the transport itself is checked).
func TestFHIRServerReadReadsTheCoverage(t *testing.T) {
	for _, mode := range []string{FHIRServerReadPublic, FHIRServerReadPrivate} {
		tr := fhirServerReader{mode: mode}.client().Transport.(*http.Transport)
		if tr.Proxy != nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatalf("%s: proxy set %t, TLS %+v", mode, tr.Proxy != nil, tr.TLSClientConfig)
		}
	}
	// The private mode (a private address, port 8443) and the public mode
	// (a public address, 443) each read the same way.
	for mode, want := range map[string]struct{ dialed string }{
		FHIRServerReadPrivate: {"10.20.30.40:8443"},
		FHIRServerReadPublic:  {"93.184.216.34:443"},
	} {
		e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset("p1", "00001")))
		base := e.base
		if mode == FHIRServerReadPublic {
			base = e.publicBase
		}
		value, count, query, status, msg := e.reader(mode).read(context.Background(),
			fhirServerRead{base: base, auth: fhirServerAuth{present: true, token: "tok-1", tokenType: "Bearer"}, patient: "p1"})
		if status != 0 || count != 1 || !bytes.Contains(value, []byte(`"Organization"`)) {
			t.Fatalf("%s: %d %q, %d coverages", mode, status, msg, count)
		}
		if e.calls.Load() != 1 || e.auth.Load() != "Bearer tok-1" || e.dialed.Load() != want.dialed ||
			e.query.Load() != "/fhir/Coverage?patient=p1" || query != e.query.Load() {
			t.Fatalf("%s: calls %d, auth %q, dialed %v, query %q (recorded %q)", mode, e.calls.Load(), e.auth.Load(), e.dialed.Load(), e.query.Load(), query)
		}
		// No token in the request: none is sent.
		if _, _, _, status, _ := e.reader(mode).read(context.Background(), fhirServerRead{base: base, patient: "p1"}); status != 0 || e.auth.Load() != "" {
			t.Fatalf("%s: %d, auth %q, want none sent", mode, status, e.auth.Load())
		}
	}
}

// What the server does, refused: a redirect (not followed), an unverifiable
// certificate, an error, a refused token, a slow or oversized answer, and
// anything that is not a Coverage searchset.
func TestFHIRServerReadRefusesWhatTheServerDoes(t *testing.T) {
	followed := atomic.Bool{}
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	t.Cleanup(other.Close)
	for name, row := range map[string]struct {
		handler func(http.ResponseWriter, *http.Request)
		roots   bool
		ctx     time.Duration
		status  int
		msg     string
	}{
		"a redirect": {handler: func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) },
			roots: true, status: 412, msg: fhirServerRedirected},
		"an unverifiable certificate": {handler: ehrAnswer(200, "application/fhir+json", coverageSearchset("p1", "00001")),
			status: 412, msg: fhirServerTLSFailed},
		"401":  {handler: ehrAnswer(401, "", ""), roots: true, status: 412, msg: fhirServerRefusedToken},
		"403":  {handler: ehrAnswer(403, "", ""), roots: true, status: 412, msg: fhirServerRefusedToken},
		"500":  {handler: ehrAnswer(500, "", ""), roots: true, status: 412, msg: fhirServerAnswered},
		"404":  {handler: ehrAnswer(404, "", ""), roots: true, status: 412, msg: fhirServerAnswered},
		"slow": {handler: func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, roots: true, ctx: 200 * time.Millisecond, status: 412, msg: fhirServerTimedOut},
		"too large": {handler: ehrAnswer(200, "application/fhir+json", `{"resourceType":"Bundle","type":"searchset","x":"`+strings.Repeat("a", fhirServerMaxBody)+`"}`),
			roots: true, status: 412, msg: fhirServerTooLarge},
		// Headers over the 64 KiB cap: the transport gives up on the answer, so
		// it reads as not reached.
		"headers over 64 KiB": {handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Padding", strings.Repeat("a", 70<<10))
			ehrAnswer(200, "application/fhir+json", coverageSearchset("p1", "00001"))(w, r)
		}, roots: true, status: 412, msg: fhirServerUnreachable},
		"not JSON":            {handler: ehrAnswer(200, "text/html", "<html/>"), roots: true, status: 412, msg: fhirServerNotSearchset},
		"a searchset as text": {handler: ehrAnswer(200, "text/plain", coverageSearchset("p1", "00001")), roots: true, status: 412, msg: fhirServerNotSearchset},
		"no content type": {handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = nil
			_, _ = w.Write([]byte(coverageSearchset("p1", "00001")))
		}, roots: true, status: 412, msg: fhirServerNotSearchset},
		"not a Bundle":       {handler: ehrAnswer(200, "application/json", `{"resourceType":"Coverage"}`), roots: true, status: 412, msg: fhirServerNotSearchset},
		"not a searchset":    {handler: ehrAnswer(200, "application/json", `{"resourceType":"Bundle","type":"collection"}`), roots: true, status: 412, msg: fhirServerNotSearchset},
		"a Patient entry":    {handler: ehrAnswer(200, "application/json", `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p1"}}]}`), roots: true, status: 412, msg: fhirServerNotSearchset},
		"unreadable entries": {handler: ehrAnswer(200, "application/json", `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":7}]}`), roots: true, status: 412, msg: fhirServerNotSearchset},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEHRServer(t, row.handler)
			r := e.reader(FHIRServerReadPrivate)
			if !row.roots {
				r.roots = nil
			}
			ctx := context.Background()
			if row.ctx > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, row.ctx)
				defer cancel()
			}
			_, _, _, status, msg := r.read(ctx, fhirServerRead{base: e.base, patient: "p1"})
			if status != row.status || msg != row.msg {
				t.Fatalf("%d %q, want %d %q", status, msg, row.status, row.msg)
			}
		})
	}
	if followed.Load() {
		t.Fatal("a redirect was followed")
	}
}

// fhirServerRequest is the EHR's order-select request for strangerMember, a
// member the system of record does not hold, naming base as its fhirServer
// and carrying no coverage prefetch.
func fhirServerRequest(base string) []byte {
	b := ehrRequest(patientOnly)
	b = bytes.ReplaceAll(b, []byte(`"example"`), []byte(`"`+strangerMember+`"`))
	b = bytes.ReplaceAll(b, []byte(`Patient/example"`), []byte(`Patient/`+strangerMember+`"`))
	return bytes.Replace(b, []byte("https://ehr.example/fhir"), []byte(base), 1)
}

// fhirServerEnv is a provider gateway at its default but for the read mode
// given, reaching e as example.com.
func fhirServerEnv(t *testing.T, e *ehrServer, mode string) (*inProcessExchange, *observed) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = newPrefetchSoR().sor()
	env.originator.cfg.FHIRServerRead = mode
	rmode := mode
	if rmode != FHIRServerReadPublic {
		rmode = FHIRServerReadPrivate
	}
	r := e.reader(rmode)
	env.originator.cfg.fhirServerRoots, env.originator.cfg.fhirServerResolve, env.originator.cfg.fhirServerDial = r.roots, r.resolve, r.dial
	obs := &observed{}
	env.originator.cfg.Observer = obs.observe
	return env, obs
}

// captureLog collects the holder log for the rest of the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	b := &syncBuffer{}
	previous := log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(previous) })
	return b
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A CRD request with no coverage for a member the system of record does not
// hold, naming its EHR's fhirServer, is routed by the Coverage read there.
// Nothing is added to what is carried: no coverage prefetch, and fhirServer
// and fhirAuthorization still removed. The read is recorded with its source,
// and its log names the host, never the token.
func TestCRDIngressRoutesByTheCoverageReadThroughFHIRServer(t *testing.T) {
	e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")))
	env, obs := fhirServerEnv(t, e, "")
	logs := captureLog(t)
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
	if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	if e.calls.Load() != 1 || e.auth.Load() != "Bearer ehr-secret-token" {
		t.Fatalf("fhirServer read %d times with %q", e.calls.Load(), e.auth.Load())
	}
	sent := sentRequest(t, env)
	if got := names(membersOf(t, sent, "prefetch")); strings.Join(got, ",") != "patient" {
		t.Fatalf("prefetch carried %v, want only the EHR's patient: nothing read is added", got)
	}
	for _, key := range []string{"fhirServer", "fhirAuthorization", "ehr-secret-token", "example.com"} {
		if bytes.Contains(sent, []byte(key)) {
			t.Fatalf("the carried request holds %q", key)
		}
	}
	ev, ok := obs.prefetch(t)["coverage"]
	if !ok || ev.Source != sourceFHIRServer || ev.Outcome != SearchOK || ev.Count != 1 || strings.Contains(ev.Query, "example.com") {
		t.Fatalf("recorded %+v", ev)
	}
	if out := logs.String(); !strings.Contains(out, "fhirServer example.com") || strings.Contains(out, "ehr-secret-token") {
		t.Fatalf("log %q: want the host and never the token", out)
	}
}

// With no coverage, no member the system of record holds and no fhirServer,
// the request says what to send: refused 412 before any read, at every level.
func TestCRDIngressNoCoverageAndNoFHIRServer(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")))
			env, _ := fhirServerEnv(t, e, "")
			env.originator.cfg.ConformanceEnforcement = level
			body := bytes.Replace(fhirServerRequest(e.base), []byte(`"fhirServer" : "`+e.base+`",`), nil, 1)
			if bytes.Contains(body, []byte("fhirServer")) {
				t.Fatalf("the request still names a fhirServer: %s", body)
			}
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, crdNoCoverageToRouteBy)
			if e.calls.Load() != 0 {
				t.Fatalf("fhirServer read %d times", e.calls.Load())
			}
		})
	}
}

// The private mode, the default, never reads an address no mode reads: a
// fhirServer at a loopback or metadata literal, or a name resolving to one,
// is refused before any connection.
func TestCRDIngressDefaultNeverReadsLoopbackOrMetadata(t *testing.T) {
	for name, row := range map[string]struct {
		base string
		addr string
	}{
		"a loopback literal":           {"https://127.0.0.1:8443/fhir", ""},
		"a metadata literal":           {"https://169.254.169.254/latest", ""},
		"a name resolving to loopback": {"https://ehr.example/fhir", "127.0.0.1"},
		"a name resolving to metadata": {"https://ehr.example/fhir", "169.254.170.2"},
	} {
		t.Run(name, func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.SoR = newPrefetchSoR().sor()
			env.originator.cfg.fhirServerResolve = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr(row.addr)}, nil
			}
			env.originator.cfg.fhirServerDial = func(context.Context, string, string) (net.Conn, error) {
				t.Error("dialed an address no mode reads")
				return nil, errors.New("refused")
			}
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(row.base)))
			refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, fhirServerNeverRead)
		})
	}
}

// A Coverage that names its payor by identifier: routed by the one read.
func TestCRDIngressRoutesByAPayorIdentifierReadThroughFHIRServer(t *testing.T) {
	e := newEHRServer(t, ehrAnswer(200, "application/json", `{"resourceType":"Bundle","type":"searchset","entry":[`+
		`{"resource":{"resourceType":"Coverage","id":"c9","status":"active","beneficiary":{"reference":"Patient/`+strangerMember+`"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}}]}`))
	env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
	if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
}

// ehrRoutes answers the Coverage search with search and every other read (the
// payor Organization's) with org.
func ehrRoutes(search string, org func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fhir/Coverage" {
			ehrAnswer(200, "application/fhir+json", search)(w, r)
			return
		}
		org(w, r)
	}
}

// bareRefSearchset is a searchset of the stranger member's active Coverages,
// one a payor reference, with no Organization included: a bare id is written
// "Organization/<id>".
func bareRefSearchset(refs ...string) string {
	var entries []string
	for i, ref := range refs {
		if !strings.Contains(ref, "/") {
			ref = "Organization/" + ref
		}
		entries = append(entries, fmt.Sprintf(`{"resource":{"resourceType":"Coverage","id":"c%d","status":"active","beneficiary":{"reference":"Patient/%s"},"payor":[{"reference":%q,"display":"Payer"}]}}`, i, strangerMember, ref))
	}
	return `{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`
}

func payorOrganization(id, payerValue string) string {
	return `{"resourceType":"Organization","id":"` + id + `","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"` + payerValue + `"}]}`
}

// A Coverage naming its payor by reference alone (the search asks for no
// _include) is routed by one more read, of that Organization, on the same
// server with the same token. Two Coverages naming the same Organization take
// that one read. Nothing either read returns is carried.
func TestCRDIngressRoutesByThePayorOrganizationReadThroughFHIRServer(t *testing.T) {
	for name, search := range map[string]string{"one coverage": bareRefSearchset("o1"), "two naming the same": bareRefSearchset("o1", "o1")} {
		t.Run(name, func(t *testing.T) {
			var paths []string
			var mu sync.Mutex
			e := newEHRServer(t, nil)
			e.handler = ehrRoutes(search, func(w http.ResponseWriter, r *http.Request) {
				ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001"))(w, r)
			})
			inner := e.handler
			e.handler = func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path+" "+r.Header.Get("Authorization"))
				mu.Unlock()
				inner(w, r)
			}
			env, obs := fhirServerEnv(t, e, FHIRServerReadPrivate)
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if want := []string{"/fhir/Coverage Bearer ehr-secret-token", "/fhir/Organization/o1 Bearer ehr-secret-token"}; !slices.Equal(paths, want) {
				t.Fatalf("read %v, want %v", paths, want)
			}
			sent := sentRequest(t, env)
			for _, key := range []string{"Organization", "00001", "fhirServer", "ehr-secret-token"} {
				if bytes.Contains(sent, []byte(key)) {
					t.Fatalf("the carried request holds %q", key)
				}
			}
			if ev := obs.prefetch(t)["coverage"]; ev.Source != sourceFHIRServer || ev.Query != "/fhir/Organization/o1" || ev.Outcome != SearchOK {
				t.Fatalf("recorded %+v", ev)
			}
		})
	}
}

// Both reads share one time budget: the Organization read, slower than what
// the search leaves of the budget but faster than the budget itself, runs out
// of time.
func TestCRDIngressFHIRServerReadsShareOneBudget(t *testing.T) {
	slow := func(d time.Duration, next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
			next(w, r)
		}
	}
	e := newEHRServer(t, nil)
	search := slow(200*time.Millisecond, ehrAnswer(200, "application/fhir+json", bareRefSearchset("o1")))
	org := slow(500*time.Millisecond, ehrAnswer(200, "application/json", payorOrganization("o1", "00001")))
	e.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fhir/Coverage" {
			search(w, r)
			return
		}
		org(w, r)
	}
	env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
	env.originator.cfg.fhirServerBudget = 600 * time.Millisecond
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
	refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, fhirServerTimedOut)
	if e.calls.Load() != 2 {
		t.Fatalf("fhirServer read %d times, want 2", e.calls.Load())
	}
	// The same server with a budget both reads fit: routed.
	env, _ = fhirServerEnv(t, e, FHIRServerReadPrivate)
	env.originator.cfg.fhirServerBudget = 2 * time.Second
	rec = httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
}

// Refused before the network, at every level: the read off (the
// participant's opt-out), a server in the public mode on another port, a
// Coverage about another
// patient (never routed on), no Coverage at all, and the payor Organization
// read's refusals.
func TestCRDIngressFHIRServerRefusals(t *testing.T) {
	for name, row := range map[string]struct {
		mode    string
		handler func(http.ResponseWriter, *http.Request)
		calls   int32
		status  int
		msg     string
	}{
		"off": {FHIRServerReadOff, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")), 0, 412, crdNoCoverageReadOff},
		"public, on another port": {FHIRServerReadPublic, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")),
			0, 412, fhirServerNotPort443},
		"another patient's coverage": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", coverageSearchset("someone-else", "00001")),
			1, 502, fhirServerOtherPatient},
		"no coverage": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", `{"resourceType":"Bundle","type":"searchset","total":0}`),
			1, 412, fhirServerNoCoverage},
		// The patient check refuses these about the request's own patient:
		// an answer the read cannot use, never another patient's coverage.
		"a repeated fullUrl": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", repeatedFullURLSearchset()),
			1, 412, fhirServerNotSearchset},
		"a contained payor without an id": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", containedNoIDSearchset()),
			1, 412, fhirServerNotSearchset},
		// A patient the check cannot identify is not another patient: a
		// Coverage naming none, or a contained beneficiary with only an MRN.
		"a coverage naming no patient": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", strings.Replace(coverageSearchset(strangerMember, "00001"),
			`"beneficiary":{"reference":"Patient/`+strangerMember+`"},`, "", 1)), 1, 412, fhirServerNotSearchset},
		"a contained beneficiary with only an MRN": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", strings.Replace(coverageSearchset(strangerMember, "00001"),
			`"beneficiary":{"reference":"Patient/`+strangerMember+`"},`,
			`"contained":[{"resourceType":"Patient","id":"p","identifier":[{"system":"urn:oid:1.2.3.4","value":"MRN-1"}]}],"beneficiary":{"reference":"#p"},`, 1)),
			1, 412, fhirServerNotSearchset},
		// Another patient named inside the Coverage's own contained resource
		// (a subscriber's RelatedPerson) leaves the beneficiary this patient:
		// not another patient's coverage, and not one the read can use.
		"a contained subscriber naming another patient": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", strings.Replace(coverageSearchset(strangerMember, "00001"),
			`"beneficiary":{"reference":"Patient/`+strangerMember+`"},`,
			`"contained":[{"resourceType":"RelatedPerson","id":"rp","patient":{"reference":"Patient/someone-else"}}],"subscriber":{"reference":"#rp"},"beneficiary":{"reference":"Patient/`+strangerMember+`"},`, 1)),
			1, 412, fhirServerNotSearchset},
		// A Coverage whose beneficiary names another member is another
		// patient's (as is one naming another Patient id, above).
		"a beneficiary by another member's identifier": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", strings.Replace(coverageSearchset(strangerMember, "00001"),
			`"beneficiary":{"reference":"Patient/`+strangerMember+`"},`,
			`"beneficiary":{"type":"Patient","identifier":{"system":"`+shnsdk.MemberSystem+`","value":"someone-else"}},`, 1)),
			1, 502, fhirServerOtherPatient},
		// The payor Organization read: at most one, never a third read.
		"no such Organization":  {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(404, "", "")), 2, 412, fhirServerNoOrganization},
		"the Organization gone": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(410, "", "")), 2, 412, fhirServerNoOrganization},
		"another Organization answered": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "application/json", payorOrganization("o2", "00001"))),
			2, 502, fhirServerNotOrganization},
		"not an Organization": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "application/json", `{"resourceType":"Patient","id":"o1"}`)),
			2, 502, fhirServerNotOrganization},
		"not JSON for the Organization": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "text/html", "<html/>")),
			2, 502, fhirServerNotOrganization},
		"the Organization as text": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "text/plain", payorOrganization("o1", "00001"))),
			2, 502, fhirServerNotOrganization},
		"the token refused for the Organization": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(403, "", "")), 2, 412, fhirServerRefusedToken},
		"the Organization read fails":            {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(500, "", "")), 2, 412, fhirServerAnswered},
		"the Organization redirected": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/fhir/Organization/o2", http.StatusFound)
		}), 2, 412, fhirServerRedirected},
		"two Organizations by reference alone": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1", "o2"), ehrAnswer(200, "application/json", payorOrganization("o1", "00001"))),
			2, 422, fhirServerTwoPayors},
		"an Organization on another server": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("https://other.example/fhir/Organization/o1"), ehrAnswer(200, "application/json", payorOrganization("o1", "00001"))),
			1, 422, "no payer identifier on member coverage"},
		"a dot-segment Organization reference": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("Organization/.."), ehrAnswer(200, "application/json", payorOrganization("o1", "00001"))),
			1, 422, "no payer identifier on member coverage"},
		"a versioned Organization reference": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("Organization/o1/_history/2"), ehrAnswer(200, "application/json", payorOrganization("o1", "00001"))),
			1, 422, "no payer identifier on member coverage"},
		"an Organization with no payer identifier": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "application/json", `{"resourceType":"Organization","id":"o1","name":"Payer One"}`)),
			2, 422, "no payer identifier on member coverage"},
		"coverages naming two payers": {FHIRServerReadPrivate, ehrAnswer(200, "application/json", `{"resourceType":"Bundle","type":"searchset","entry":[`+
			`{"resource":{"resourceType":"Coverage","id":"c9","status":"active","beneficiary":{"reference":"Patient/`+strangerMember+`"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}},`+
			`{"resource":{"resourceType":"Coverage","id":"c8","status":"active","beneficiary":{"reference":"Patient/`+strangerMember+`"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00002"}}]}}]}`),
			1, 422, "ambiguous coverage for routing"},
		"a payer no one registered": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "99999")),
			1, 422, "no registered payer for identifier urn:oid:2.16.840.1.113883.6.300|99999"},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				e := newEHRServer(t, row.handler)
				env, _ := fhirServerEnv(t, e, row.mode)
				env.originator.cfg.ConformanceEnforcement = level
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
				refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
				if e.calls.Load() != row.calls {
					t.Fatalf("fhirServer read %d times, want %d", e.calls.Load(), row.calls)
				}
			})
		}
	}
}

// The public mode's own address checks, through the ingress: a server at 443
// whose name resolves to a private address is refused, and nothing connects.
func TestCRDIngressPublicModeRefusesAPrivateServer(t *testing.T) {
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = newPrefetchSoR().sor()
	env.originator.cfg.FHIRServerRead = FHIRServerReadPublic
	env.originator.cfg.fhirServerResolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
	}
	dialed := false
	env.originator.cfg.fhirServerDialed = func(netip.AddrPort) { dialed = true }
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest("https://ehr.internal.example/fhir")))
	refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, fhirServerNotPublic)
	if dialed {
		t.Fatal("dialed a private address")
	}
}

// A CRD request refused for no coverage to route by, however it came to have
// none, is recorded as the provider gateway's routing refusal: the read off,
// a fhirServer read that could not get it, the EHR's own null coverage, and
// no fhirServer at all.
func TestExchangeRecord_CRDNoCoverageToRouteByIsARoutingRefusal(t *testing.T) {
	e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", `{"resourceType":"Bundle","type":"searchset","total":0}`))
	for name, row := range map[string]struct {
		mode string
		body []byte
		msg  string
	}{
		"the read off":       {FHIRServerReadOff, fhirServerRequest(e.base), crdNoCoverageReadOff},
		"no active Coverage": {FHIRServerReadPrivate, fhirServerRequest(e.base), fhirServerNoCoverage},
		"the EHR's own null": {FHIRServerReadPrivate, ehrRequest(patientOnly + `,"coverage":null`), "no coverage in request or system of record"},
		"no fhirServer": {FHIRServerReadPrivate, bytes.Replace(fhirServerRequest(e.base), []byte(`"fhirServer" : "`+e.base+`",`), nil, 1),
			crdNoCoverageToRouteBy},
		// The read off answers as off, never suggesting a fhirServer it
		// would not read.
		"the read off, no fhirServer": {FHIRServerReadOff, bytes.Replace(fhirServerRequest(e.base), []byte(`"fhirServer" : "`+e.base+`",`), nil, 1),
			crdNoCoverageReadOff},
	} {
		t.Run(name, func(t *testing.T) {
			env, _ := fhirServerEnv(t, e, row.mode)
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCRD)(rec, crdIngressPost(row.body))
			if rec.Code != http.StatusPreconditionFailed || !strings.Contains(rec.Body.String(), row.msg) {
				t.Fatalf("answer %d %s, want 412 %q", rec.Code, rec.Body.String(), row.msg)
			}
			wantRefusal(t, got.only(t), http.StatusPreconditionFailed, RefusedByProviderGateway, RefusalRouting)
		})
	}
	// An answer that contradicts the request is the EHR's server answering
	// wrongly, not a refusal: recorded other, with no refusing party or rule.
	for name, handler := range map[string]func(http.ResponseWriter, *http.Request){
		"another patient's coverage": ehrAnswer(200, "application/fhir+json", coverageSearchset("someone-else", "00001")),
		"another Organization":       ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "application/json", payorOrganization("o2", "00001"))),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEHRServer(t, handler)
			env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCRD)(rec, crdIngressPost(fhirServerRequest(e.base)))
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("answer %d %s, want 502", rec.Code, rec.Body.String())
			}
			if r := got.only(t); r.Outcome != ExchangeOther || r.RefusedBy != "" || r.Rule != "" || r.Status != http.StatusBadGateway {
				t.Fatalf("record = %s %s/%s status %d, want other with no refusal, status 502", r.Outcome, r.RefusedBy, r.Rule, r.Status)
			}
		})
	}
}

// statusSearchset is a searchset of the stranger member's Coverages, each
// "status:payerValue", naming its payor by identifier.
func statusSearchset(coverages ...string) string {
	var entries []string
	for i, c := range coverages {
		status, payer, _ := strings.Cut(c, ":")
		entries = append(entries, fmt.Sprintf(`{"resource":{"resourceType":"Coverage","id":"c%d","status":%q,"beneficiary":{"reference":"Patient/%s"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":%q}}]}}`, i, status, strangerMember, payer))
	}
	return `{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`
}

// The read searches every Coverage, and routes on the active ones when any is
// active (a cancelled one naming another payer, here one no route registers,
// never makes routing ambiguous), otherwise on the cancelled ones when they
// name one payer: that payer answers that the member is not covered.
func TestCRDIngressFHIRServerRoutesOnTheActiveCoverage(t *testing.T) {
	for name, row := range map[string]struct {
		search string
		status int
		msg    string
	}{
		"active and cancelled naming another payer": {statusSearchset("cancelled:99999", "active:00001"), http.StatusOK, ""},
		"only cancelled, one payer":                 {statusSearchset("cancelled:00001", "cancelled:00001"), http.StatusOK, ""},
		"only cancelled, two payers":                {statusSearchset("cancelled:00001", "cancelled:99999"), http.StatusUnprocessableEntity, "ambiguous coverage for routing"},
		"two active payers":                         {statusSearchset("active:00001", "active:99999", "cancelled:00001"), http.StatusUnprocessableEntity, "ambiguous coverage for routing"},
		"none at all":                               {`{"resourceType":"Bundle","type":"searchset","total":0}`, http.StatusPreconditionFailed, fhirServerNoCoverage},
		// A Coverage whose status cannot be read is not active, as on the
		// system-of-record path (routingCoverageChoice).
		"active and one whose status is not a string naming another payer": {unreadableStatusSearchset(), http.StatusOK, ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", row.search))
			env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
			if row.status == http.StatusOK {
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				return
			}
			refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
		})
	}
}

// The routing choice itself: the rebuilt searchset keeps every entry but the
// Coverages that are not active, and only when one is.
func TestRoutingCoverages(t *testing.T) {
	both := statusSearchset("cancelled:99999", "active:00001")
	got := string(routingCoverages([]byte(both)))
	if strings.Contains(got, "99999") || !strings.Contains(got, `"active"`) || !strings.Contains(got, `"searchset"`) {
		t.Fatalf("kept %s", got)
	}
	for _, unchanged := range []string{statusSearchset("cancelled:00001"), `{"resourceType":"Bundle","type":"searchset"}`, `not json`} {
		if got := string(routingCoverages([]byte(unchanged))); got != unchanged {
			t.Fatalf("%s became %s", unchanged, got)
		}
	}
	withOrg := `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Coverage","status":"active","payor":[{"reference":"Organization/o1"}]}},{"resource":{"resourceType":"Organization","id":"o1"}},{"resource":{"resourceType":"Coverage","status":"draft"}}]}`
	if got := string(routingCoverages([]byte(withOrg))); !strings.Contains(got, `"Organization","id":"o1"`) || strings.Contains(got, "draft") {
		t.Fatalf("kept %s", got)
	}
	// A Coverage whose status is not a string is not active: beside an active
	// one it is left out, and every entry that is not a Coverage is kept.
	var rebuilt struct {
		Entry []struct {
			Resource struct {
				ResourceType string          `json:"resourceType"`
				ID           string          `json:"id"`
				Status       json.RawMessage `json:"status"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(routingCoverages([]byte(unreadableStatusSearchset())), &rebuilt); err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, e := range rebuilt.Entry {
		kept = append(kept, e.Resource.ResourceType+"/"+e.Resource.ID+" "+string(e.Resource.Status))
	}
	if want := []string{"Organization/pay-b ", `Coverage/c1 "active"`, "OperationOutcome/oo "}; !slices.Equal(kept, want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
}

// unreadableStatusSearchset is a fhirServer answer holding a Coverage whose
// status is not a string, naming a payer no route registers, its payor
// Organization, an active Coverage naming the routed payer (00001) and an
// OperationOutcome.
func unreadableStatusSearchset() string {
	both := statusSearchset("cancelled:99999", "active:00001")
	both = strings.Replace(both, `"status":"cancelled"`, `"status":7`, 1)
	both = strings.Replace(both, `},{"resource":{"resourceType":"Coverage","id":"c1"`,
		`},{"resource":{"resourceType":"Organization","id":"pay-b","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"99999"}]}},{"resource":{"resourceType":"Coverage","id":"c1"`, 1)
	return strings.TrimSuffix(both, `]}`) + `,{"resource":{"resourceType":"OperationOutcome","id":"oo","issue":[{"severity":"information","code":"informational"}]}}]}`
}

// A null fhirAuthorization is no authorization: the read sends no token, and
// the request is routed (a non-object is refused; see the bad-token rows).
func TestCRDIngressNullFHIRAuthorizationSendsNoToken(t *testing.T) {
	e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")))
	env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
	auth := []byte(`"fhirAuthorization" : { "access_token" : "ehr-secret-token", "token_type" : "Bearer", "expires_in" : 300 }`)
	body := fhirServerRequest(e.base)
	if !bytes.Contains(body, auth) {
		t.Fatalf("the fixture's fhirAuthorization moved: %s", body)
	}
	body = bytes.Replace(body, auth, []byte(`"fhirAuthorization" : null`), 1)
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(body))
	if rec.Code != http.StatusOK || e.calls.Load() != 1 || e.auth.Load() != "" {
		t.Fatalf("answer %d %s; read %d times with %q", rec.Code, rec.Body.String(), e.calls.Load(), e.auth.Load())
	}
}

// A name that answers a second resolution with another address (DNS
// rebinding) cannot move a read: each read resolves its name once, checks
// every address, and dials the address it checked. A later read resolves
// again and is checked again.
func TestFHIRServerReadDialsTheAddressItChecked(t *testing.T) {
	e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset("p1", "00001")))
	r := e.reader(FHIRServerReadPublic)
	var resolves atomic.Int32
	r.resolve = func(context.Context, string) ([]netip.Addr, error) {
		if resolves.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	req := fhirServerRead{base: e.publicBase, auth: fhirServerAuth{present: true, token: "tok-1", tokenType: "Bearer"}, patient: "p1"}
	if _, _, _, status, msg := r.read(context.Background(), req); status != 0 || resolves.Load() != 1 || e.dialed.Load() != "93.184.216.34:443" {
		t.Fatalf("%d %q: resolved %d times, dialed %v; want one resolution and the checked address dialed", status, msg, resolves.Load(), e.dialed.Load())
	}
	if _, _, _, status, msg := r.read(context.Background(), req); status != http.StatusPreconditionFailed || msg != fhirServerNotPublic || e.calls.Load() != 1 {
		t.Fatalf("the rebound read: %d %q after %d reads; want 412 %q and no second read", status, msg, e.calls.Load(), fhirServerNotPublic)
	}
}

// The read presents nothing of the gateway's own: only the request's own
// fhirAuthorization token, or no Authorization at all when the request sends
// none, no client certificate (the server asks for one), no cookie, and a
// transport of its own, so no credential or signer another client of the
// gateway's process carries. Both reads, in every mode that reads.
func TestFHIRServerReadPresentsNoCredentialOfItsOwn(t *testing.T) {
	for _, mode := range []string{FHIRServerReadPublic, FHIRServerReadPrivate} {
		c := fhirServerReader{mode: mode}.client()
		tr, ok := c.Transport.(*http.Transport)
		if !ok || c.Jar != nil || len(tr.TLSClientConfig.Certificates) != 0 || tr.TLSClientConfig.GetClientCertificate != nil ||
			tr == http.DefaultTransport || tr.ProxyConnectHeader != nil || tr.GetProxyConnectHeader != nil {
			t.Fatalf("%s: the read's client carries something of its own: %T %+v", mode, c.Transport, c)
		}
	}
	// Only these headers are sent: Go's own, Accept, the close of a
	// connection never reused, and the request's token.
	allowed := map[string]bool{"Accept": true, "Accept-Encoding": true, "User-Agent": true, "Connection": true}
	for name, auth := range map[string]fhirServerAuth{
		"the request's token":  {present: true, token: "ehr-secret-token", tokenType: "Bearer"},
		"no fhirAuthorization": {},
	} {
		for _, mode := range []string{FHIRServerReadPublic, FHIRServerReadPrivate} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				e := newEHRServer(t, nil)
				e.handler = ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001")))
				base := e.base
				if mode == FHIRServerReadPublic {
					base = e.publicBase
				}
				req := fhirServerRead{base: base, auth: auth, patient: strangerMember}
				r := e.reader(mode)
				check := func(read string) {
					t.Helper()
					h := e.headers.Load().(http.Header)
					for k := range h {
						if !allowed[k] && !(k == "Authorization" && auth.present) {
							t.Errorf("%s: sent header %s: %q", read, k, h.Values(k))
						}
					}
					want := ""
					if auth.present {
						want = "Bearer " + auth.token
					}
					if got := h.Values("Authorization"); (want == "" && len(got) != 0) || (want != "" && !slices.Equal(got, []string{want})) {
						t.Errorf("%s: Authorization %q, want %q", read, got, want)
					}
					if e.clientCert.Load() {
						t.Errorf("%s: presented a client certificate", read)
					}
				}
				if _, _, _, status, msg := r.read(context.Background(), req); status != 0 {
					t.Fatalf("search: %d %q", status, msg)
				}
				check("search")
				if _, _, status, msg := r.readOrganization(context.Background(), req, "o1"); status != 0 {
					t.Fatalf("Organization read: %d %q", status, msg)
				}
				check("Organization read")
			})
		}
	}
	// Through a provider gateway holding its own keys and tokens: the reads
	// carry the request's token, or none when the request sends none.
	for name, strip := range map[string]bool{"the request's token": false, "no fhirAuthorization": true} {
		t.Run("ingress/"+name, func(t *testing.T) {
			e := newEHRServer(t, nil)
			var seen []http.Header
			var mu sync.Mutex
			routes := ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001")))
			e.handler = func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Header.Clone())
				mu.Unlock()
				routes(w, r)
			}
			env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
			body := fhirServerRequest(e.base)
			want := []string{"Bearer ehr-secret-token"}
			if strip {
				auth := []byte(`"fhirAuthorization" : { "access_token" : "ehr-secret-token", "token_type" : "Bearer", "expires_in" : 300 },`)
				if !bytes.Contains(body, auth) {
					t.Fatalf("the fixture's fhirAuthorization moved: %s", body)
				}
				body, want = bytes.Replace(body, auth, nil, 1), nil
			}
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			if rec.Code != http.StatusOK || len(seen) != 2 || e.clientCert.Load() {
				t.Fatalf("answer %d %s; %d reads, client certificate %t", rec.Code, rec.Body.String(), len(seen), e.clientCert.Load())
			}
			for _, h := range seen {
				for k := range h {
					if !allowed[k] && k != "Authorization" {
						t.Errorf("sent header %s: %q", k, h.Values(k))
					}
				}
				if got := h.Values("Authorization"); !slices.Equal(got, want) {
					t.Errorf("Authorization %q, want %q", got, want)
				}
			}
		})
	}
}

// Each read's outcome is recorded: one the server did not complete is
// unavailable, one refused before or after it is not run, and an answer is
// zero or ok by its Coverages.
func TestFHIRServerOutcome(t *testing.T) {
	for _, row := range []struct {
		status int
		msg    string
		count  int
		want   SearchOutcome
	}{
		{0, "", 1, SearchOK}, {0, "", 0, SearchZero},
		{412, fhirServerNoCoverage, 0, SearchZero}, {412, fhirServerNoOrganization, 0, SearchZero},
		{412, fhirServerNoAddress, 0, SearchUnavailable}, {412, fhirServerTLSFailed, 0, SearchUnavailable},
		{412, fhirServerUnreachable, 0, SearchUnavailable}, {412, fhirServerTimedOut, 0, SearchUnavailable},
		{412, fhirServerAnswered, 0, SearchUnavailable}, {412, fhirServerRefusedToken, 0, SearchUnavailable},
		{412, fhirServerTooLarge, 0, SearchBound},
		{412, fhirServerNotSearchset, 0, SearchMalformed}, {502, fhirServerNotOrganization, 0, SearchMalformed},
		{412, fhirServerRedirected, 0, SearchMalformed}, {502, fhirServerOtherPatient, 1, SearchMalformed},
		{412, fhirServerNotPublic, 0, SearchNotRun}, {412, fhirServerNeverRead, 0, SearchNotRun},
		{412, fhirServerNotBase, 0, SearchNotRun}, {412, fhirServerBadToken, 0, SearchNotRun},
		{412, fhirServerNotPort443, 0, SearchNotRun},
	} {
		if got := fhirServerOutcome(row.status, row.msg, row.count); got != row.want {
			t.Errorf("%d %q (%d): %s, want %s", row.status, row.msg, row.count, got, row.want)
		}
	}
}

// Through the ingress, each read's recorded outcome follows the read.
func TestCRDIngressRecordsTheFHIRServerReadsOutcome(t *testing.T) {
	for name, row := range map[string]struct {
		mode    string
		handler func(http.ResponseWriter, *http.Request)
		want    SearchOutcome
	}{
		"routed":            {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")), SearchOK},
		"an error answered": {FHIRServerReadPrivate, ehrAnswer(500, "", ""), SearchUnavailable},
		"no Coverage":       {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", `{"resourceType":"Bundle","type":"searchset","total":0}`), SearchZero},
		"too large": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", `{"resourceType":"Bundle","type":"searchset","x":"`+strings.Repeat("a", fhirServerMaxBody)+`"}`),
			SearchBound},
		"another patient's coverage": {FHIRServerReadPrivate, ehrAnswer(200, "application/fhir+json", coverageSearchset("someone-else", "00001")), SearchMalformed},
		"no such Organization":       {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(404, "", "")), SearchZero},
		"another Organization": {FHIRServerReadPrivate, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "application/json", payorOrganization("o2", "00001"))),
			SearchMalformed},
		"refused before the read": {FHIRServerReadPublic, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")),
			SearchNotRun},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEHRServer(t, row.handler)
			env, obs := fhirServerEnv(t, e, row.mode)
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
			ev := obs.prefetch(t)["coverage"]
			if ev.Source != sourceFHIRServer || ev.Outcome != row.want {
				t.Fatalf("answer %d; recorded %+v, want outcome %s", rec.Code, ev, row.want)
			}
			// As a system-of-record search: matches only for an answer used,
			// and a page only for an answer read: none when unavailable (an
			// error status among them) or not run, one otherwise (a redirect, a
			// 404 on the Organization read, an answer over the bound).
			wantCount, wantPages := 0, 1
			if row.want == SearchOK {
				wantCount = 1
			}
			if row.want == SearchNotRun || row.want == SearchUnavailable {
				wantPages = 0
			}
			if ev.Count != wantCount || ev.Pages != wantPages {
				t.Fatalf("recorded %d matches in %d pages, want %d in %d", ev.Count, ev.Pages, wantCount, wantPages)
			}
		})
	}
}

// A null or empty fhirServer names no server, as a null fhirAuthorization
// sends no token: the request is refused as one naming none, and nothing is
// read.
func TestCRDIngressNullOrEmptyFHIRServerNamesNone(t *testing.T) {
	for name, value := range map[string]string{"null": `null`, "empty": `""`} {
		t.Run(name, func(t *testing.T) {
			e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")))
			env, _ := fhirServerEnv(t, e, "")
			named := []byte(`"fhirServer" : "` + e.base + `"`)
			body := fhirServerRequest(e.base)
			if !bytes.Contains(body, named) {
				t.Fatalf("the fixture's fhirServer moved: %s", body)
			}
			body = bytes.Replace(body, named, []byte(`"fhirServer" : `+value), 1)
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, crdNoCoverageToRouteBy)
			if e.calls.Load() != 0 {
				t.Fatalf("fhirServer read %d times", e.calls.Load())
			}
		})
	}
}

// repeatedFullURLSearchset is the stranger member's searchset with its payor
// Organization entry repeated under the same fullUrl.
func repeatedFullURLSearchset() string {
	b := coverageSearchset(strangerMember, "00001")
	i := strings.Index(b, `{"fullUrl":"https://example.com/fhir/Organization/o1"`)
	org := b[i : len(b)-2]
	return b[:len(b)-2] + "," + org + "]}"
}

// containedNoIDSearchset is the stranger member's Coverage with its payor
// Organization contained, but with no id to reference it by.
func containedNoIDSearchset() string {
	return `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Coverage","id":"c1","status":"active",` +
		`"contained":[{"resourceType":"Organization","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]}],` +
		`"beneficiary":{"reference":"Patient/` + strangerMember + `"},"payor":[{"reference":"#o1"}]}}]}`
}

// An answer's Content-Type is read for its media type alone: a parameter such
// as charset (a FHIR server's default) is accepted, on the Coverage search
// and on the Organization read, in either JSON media type.
func TestFHIRServerReadAcceptsAMediaTypeWithParameters(t *testing.T) {
	for _, ct := range []string{"application/fhir+json; charset=utf-8", "application/json;charset=UTF-8", "Application/FHIR+JSON"} {
		t.Run(ct, func(t *testing.T) {
			e := newEHRServer(t, ehrAnswer(200, ct, coverageSearchset("p1", "00001")))
			if _, count, _, status, msg := e.reader(FHIRServerReadPrivate).read(context.Background(),
				fhirServerRead{base: e.base, patient: "p1"}); status != 0 || count != 1 {
				t.Fatalf("search: %d %q, %d coverages", status, msg, count)
			}
			o := newEHRServer(t, ehrAnswer(200, ct, `{"resourceType":"Organization","id":"o1"}`))
			if _, _, status, msg := o.reader(FHIRServerReadPrivate).readOrganization(context.Background(),
				fhirServerRead{base: o.base, patient: "p1"}, "o1"); status != 0 {
				t.Fatalf("Organization read: %d %q", status, msg)
			}
		})
	}
}
