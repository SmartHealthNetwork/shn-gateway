package engine

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Reading the Coverage through the request's own fhirServer (CDS Hooks): the
// one thing a provider gateway reads from a CDS client's server, and only to
// route. When a CRD request carries no coverage prefetch for a member the
// provider's system of record does not hold, and names its EHR's FHIR server
// (fhirServer, with fhirAuthorization), the gateway searches that member's
// Coverage there, as a CRD service the EHR called directly would, and
// routes by it. A Coverage whose payor is an Organization reference the answer
// does not resolve (the search asks for no _include, which CDS Hooks does not
// ask a client to support) takes one more read, of that Organization: at most
// two reads a request, within one time budget. Nothing read is added to the
// request, and fhirServer and fhirAuthorization are still removed before the
// network.
//
// The read goes to a URL a request names, so it is fenced in every mode: an
// https base with no query, fragment or credentials; an address the mode
// reads, checked on the literal, on every resolved address, and again on the
// address actually dialed; no redirects, no environment proxy, verified TLS, a
// few seconds and a size cap; an answer that is a Coverage searchset about the
// bound patient, and an Organization that is the one asked for. A read that
// cannot get what routing needs is a 412 (CDS Hooks: the service could not
// obtain the data the request left out); each refusal is its own reason.

// The CDS_FHIR_SERVER_READ modes (Config.FHIRServerRead).
const (
	// FHIRServerReadOff never reads fhirServer: the participant's opt-out.
	FHIRServerReadOff = "off"
	// FHIRServerReadPrivate (the default; "" too) reads a server inside the
	// participant's own network or on the internet: any address but those a
	// gateway never reads (loopback, link-local with the cloud metadata and
	// credential endpoints, unspecified, multicast, broadcast, reserved, and a
	// NAT64 or 6to4 address embedding one), on any port at a private address
	// and on 443 at a public one.
	FHIRServerReadPrivate = "private"
	// FHIRServerReadPublic reads only a server on port 443 at a public
	// address: for a gateway whose own network is not its participant's (a
	// gateway SHN hosts or runs).
	FHIRServerReadPublic = "public"
)

// ValidFHIRServerRead reports whether mode is a CDS_FHIR_SERVER_READ value
// ("" is private).
func ValidFHIRServerRead(mode string) bool {
	switch mode {
	case "", FHIRServerReadOff, FHIRServerReadPrivate, FHIRServerReadPublic:
		return true
	}
	return false
}

// The read's bounds.
const (
	fhirServerDialTimeout = 2 * time.Second
	fhirServerTLSTimeout  = 2 * time.Second
	fhirServerReadTimeout = 4 * time.Second
	fhirServerMaxBody     = 512 << 10
	fhirServerMaxToken    = 8 << 10
)

// fhirServerRefused is the status of a read that could not get what routing
// needs, whatever stopped it: CDS Hooks' 412 for a service that cannot obtain
// the data a request left out. Only an answer that contradicts the request
// (another patient's coverage, another Organization than asked for) is a 502,
// and routing ambiguity a 422 (ingress_crd.go).
const fhirServerRefused = http.StatusPreconditionFailed

// The refusals of the fhirServer read, each its own reason.
const (
	fhirServerNotBase      = "no coverage to route by: fhirServer is not an absolute base URL (no query or fragment)"
	fhirServerNotHTTPS     = "no coverage to route by: fhirServer must be https"
	fhirServerCredentials  = "no coverage to route by: fhirServer must not carry credentials"
	fhirServerNotPort443   = "no coverage to route by: fhirServer must use port 443"
	fhirServerPublicNot443 = "no coverage to route by: fhirServer at a public address must use port 443"
	fhirServerNotPublic    = "no coverage to route by: fhirServer's address is not public"
	fhirServerNeverRead    = "no coverage to route by: fhirServer's address is one a gateway never reads (loopback, link-local, metadata or reserved)"
	fhirServerNoAddress    = "no coverage to route by: fhirServer's host did not resolve"
	fhirServerRedirected   = "no coverage to route by: fhirServer redirected; redirects are not followed"
	fhirServerTLSFailed    = "no coverage to route by: fhirServer's TLS could not be verified"
	fhirServerUnreachable  = "no coverage to route by: fhirServer could not be reached"
	fhirServerTimedOut     = "no coverage to route by: fhirServer did not answer in time"
	fhirServerTooLarge     = "no coverage to route by: fhirServer's answer exceeds 512 KiB"
	fhirServerNotSearchset = "no coverage to route by: fhirServer's answer is not a Coverage searchset"
	fhirServerRefusedToken = "no coverage to route by: fhirServer refused the fhirAuthorization token"
	fhirServerAnswered     = "no coverage to route by: fhirServer answered with an error"
	fhirServerOtherPatient = "no coverage to route by: fhirServer returned another patient's coverage"
	fhirServerNoCoverage   = "no coverage to route by: fhirServer holds no Coverage for the patient"
	fhirServerBadToken     = "no coverage to route by: fhirAuthorization is not a bearer token"
	// The payor Organization read.
	fhirServerNoOrganization  = "no coverage to route by: fhirServer holds no Organization for the coverage's payor"
	fhirServerNotOrganization = "no coverage to route by: fhirServer's answer is not the payor Organization"
	fhirServerTwoPayors       = "no coverage to route by: fhirServer's coverages name more than one payor Organization by reference alone"
)

func prefixes(ps ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ps))
	for _, p := range ps {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

// neverReadPrefixes are the ranges netip's own predicates do not cover that
// no mode reads: "this network", reserved, broadcast, the cloud metadata
// endpoints outside link-local (EC2's IPv6 one and its Pod Identity agent,
// GCP's IPv6 one, Alibaba's), and the IPv4-compatible block around ::1.
var neverReadPrefixes = prefixes("0.0.0.0/8", "240.0.0.0/4", "255.255.255.255/32", "fd00:ec2::254/128", "fd00:ec2::23/128", "fd20:ce::254/128", "100.100.100.200/32", "::/96")

// nonPublicPrefixes are the further ranges netip's own predicates do not cover
// that no public server answers from, or that embed an IPv4 address (NAT64,
// 6to4): the public mode reads none of them.
var nonPublicPrefixes = prefixes(
	"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001:db8::/32", "100::/64",
)

func inPrefixes(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

var (
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local     = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour      = netip.MustParsePrefix("2002::/16")
)

// embeddedIPv4 returns the IPv4 address a NAT64 (RFC 6052: the /96 well-known
// prefix, and the /48 local-use one, which skips the u octet) or 6to4 address
// carries.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	b := a.As16()
	switch {
	case nat64WellKnown.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case nat64Local.Contains(a):
		return netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}), true
	case sixToFour.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

// neverRead reports whether a is an address no mode reads.
func neverRead(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.Zone() != "" || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || inPrefixes(neverReadPrefixes, a) {
		return true
	}
	if v4, ok := embeddedIPv4(a); ok {
		return neverRead(v4)
	}
	return false
}

// publicAddress reports whether a is an address a public server answers from:
// none a gateway never reads, not private (RFC 1918, ULA), and in none of the
// ranges above.
func publicAddress(a netip.Addr) bool {
	a = a.Unmap()
	return !neverRead(a) && !a.IsPrivate() && !inPrefixes(nonPublicPrefixes, a)
}

// readable reports whether the mode reads a server at a on port.
func (r fhirServerReader) readable(a netip.Addr, port string) (bool, string) {
	if r.public() {
		switch {
		case !publicAddress(a):
			return false, fhirServerNotPublic
		case port != "" && port != "443":
			return false, fhirServerNotPort443
		}
		return true, ""
	}
	switch {
	case neverRead(a):
		return false, fhirServerNeverRead
	case publicServer(a) && port != "" && port != "443":
		return false, fhirServerPublicNot443
	}
	return true, ""
}

// publicServer reports whether a reaches a public server: a public address,
// or a NAT64 or 6to4 one embedding a public IPv4 address.
func publicServer(a netip.Addr) bool {
	a = a.Unmap()
	if v4, ok := embeddedIPv4(a); ok {
		return publicAddress(v4)
	}
	return publicAddress(a)
}

// fhirServerAuth is the request's fhirAuthorization, as far as the read uses it.
type fhirServerAuth struct {
	present   bool
	token     string
	tokenType string
}

// fhirServerRead is one read: the request's fhirServer, its authorization,
// and the patient to search for.
type fhirServerRead struct {
	base    string
	auth    fhirServerAuth
	patient string
}

// fhirServerReader performs the read under a mode. resolve, dial and roots
// are the network: the defaults are the system resolver, a dialer that checks
// the connected address, and the system roots. Tests replace them; every
// address check runs before dial, whatever replaces it.
type fhirServerReader struct {
	mode    string
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	roots   *x509.CertPool
	// dial, when set, makes the connection to the checked address in place of
	// the checking dialer (tests: a local TLS server stands in for it).
	dial func(ctx context.Context, network, address string) (net.Conn, error)
	// dialed, when set, sees each address the reader dials.
	dialed func(netip.AddrPort)
}

func (r fhirServerReader) public() bool { return r.mode == FHIRServerReadPublic }

// target checks the request's values and returns the search URL.
func (r fhirServerReader) target(req fhirServerRead) (*url.URL, int, string) {
	u, err := url.Parse(req.base)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(req.base, "#") {
		return nil, fhirServerRefused, fhirServerNotBase
	}
	if u.Scheme != "https" {
		return nil, fhirServerRefused, fhirServerNotHTTPS
	}
	if u.User != nil {
		return nil, fhirServerRefused, fhirServerCredentials
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.ParseUint(p, 10, 16); err != nil || n == 0 {
			return nil, fhirServerRefused, fhirServerNotBase
		}
	}
	if r.public() && u.Port() != "" && u.Port() != "443" {
		return nil, fhirServerRefused, fhirServerNotPort443
	}
	if a, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil {
		if ok, msg := r.readable(a, u.Port()); !ok {
			return nil, fhirServerRefused, msg
		}
	}
	if req.auth.present {
		if !strings.EqualFold(req.auth.tokenType, "Bearer") || req.auth.token == "" || len(req.auth.token) > fhirServerMaxToken ||
			strings.ContainsFunc(req.auth.token, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
			return nil, fhirServerRefused, fhirServerBadToken
		}
	}
	search := *u
	search.Path = strings.TrimRight(u.Path, "/") + "/Coverage"
	search.RawPath = ""
	// No status filter: the routing choice (routingCoverages) prefers the
	// active Coverages, and falls back to a cancelled one's payer, which is
	// the party to answer that the member is not covered.
	search.RawQuery = url.Values{"patient": {req.patient}}.Encode()
	return &search, 0, ""
}

// fhirServerAddrError is the refusal of an address (or its port) the mode
// does not read, at resolution or at connect.
type fhirServerAddrError struct{ msg string }

func (e *fhirServerAddrError) Error() string { return e.msg }

// dialControl checks the address a connection is actually made to, just
// before it is made: the dial goes only to an address already checked, and
// this holds it there whatever the resolver later answers (rebinding).
func (r fhirServerReader) dialControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return &fhirServerAddrError{msg: fhirServerUnreachable}
	}
	if ok, msg := r.readable(ap.Addr(), strconv.Itoa(int(ap.Port()))); !ok {
		return &fhirServerAddrError{msg: msg}
	}
	return nil
}

// fhirServerSystemResolve and fhirServerSystemDial are the network a read
// uses when nothing replaces it. The engine's tests replace both with a
// refusal that fails the run, so no test reads a real fhirServer.
var (
	fhirServerSystemResolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	fhirServerSystemDial = func(ctx context.Context, d *net.Dialer, network, address string) (net.Conn, error) {
		return d.DialContext(ctx, network, address)
	}
)

// dialer is the connection dialer of the default network: it checks the
// connected address (dialControl).
func (r fhirServerReader) dialer() *net.Dialer {
	return &net.Dialer{Timeout: fhirServerDialTimeout, Control: r.dialControl}
}

// client builds the one-read client: it dials only addresses it checked,
// checks the connected address again, follows no redirect and uses no proxy.
func (r fhirServerReader) client() *http.Client {
	resolve := r.resolve
	if resolve == nil {
		resolve = fhirServerSystemResolve
	}
	dial := r.dial
	if dial == nil {
		d := r.dialer()
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return fhirServerSystemDial(ctx, d, network, address)
		}
	}
	transport := &http.Transport{
		Proxy:                  nil,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: r.roots},
		TLSHandshakeTimeout:    fhirServerTLSTimeout,
		ResponseHeaderTimeout:  fhirServerReadTimeout,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 64 << 10,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			var addrs []netip.Addr
			if a, err := netip.ParseAddr(host); err == nil {
				addrs = []netip.Addr{a}
			} else if addrs, err = resolve(ctx, host); err != nil || len(addrs) == 0 {
				return nil, &fhirServerDNSError{err: err}
			}
			// Every address the name has must be one the mode reads: a set
			// that mixes in one it does not is refused, whichever would be
			// dialed.
			// Temporary seam: the public mode reads SHN's own public names too
			// (a refusal of them is tracked in the SHN platform repository). It
			// stays additive: at most two GETs a request, their answers never
			// carried, the request's own token only, and nothing inside a VPC
			// reachable.
			for _, a := range addrs {
				if ok, msg := r.readable(a, port); !ok {
					return nil, &fhirServerAddrError{msg: msg}
				}
			}
			ap := netip.AddrPortFrom(addrs[0].Unmap(), mustPort(port))
			if r.dialed != nil {
				r.dialed(ap)
			}
			return dial(ctx, network, ap.String())
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   fhirServerReadTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type fhirServerDNSError struct{ err error }

func (e *fhirServerDNSError) Error() string {
	if e.err == nil {
		return "no address"
	}
	return e.err.Error()
}

// Unwrap and Timeout let a resolver's own timeout read as one.
func (e *fhirServerDNSError) Unwrap() error { return e.err }
func (e *fhirServerDNSError) Timeout() bool { return isTimeout(e.err) }

func mustPort(p string) uint16 {
	var n uint16
	for _, c := range p {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + uint16(c-'0')
	}
	return n
}

// read searches the patient's Coverage and returns the searchset, the
// number of Coverages it holds, the query it ran (path and query, no host)
// and, on a refusal, the status and reason.
func (r fhirServerReader) read(ctx context.Context, req fhirServerRead) (value []byte, coverages int, query string, status int, msg string) {
	target, status, msg := r.target(req)
	if status != 0 {
		return nil, 0, "", status, msg
	}
	query = target.EscapedPath() + "?" + target.RawQuery
	body, status, msg := r.get(ctx, target, req.auth, "")
	if status != 0 {
		return nil, 0, query, status, msg
	}
	var bundle struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Entry        []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if decodeMessage(body, &bundle) != nil || bundle.ResourceType != "Bundle" || bundle.Type != "searchset" {
		return nil, 0, query, fhirServerRefused, fhirServerNotSearchset
	}
	for _, e := range bundle.Entry {
		var head struct {
			ResourceType string `json:"resourceType"`
		}
		if decodeMessage(e.Resource, &head) != nil {
			return nil, 0, query, fhirServerRefused, fhirServerNotSearchset
		}
		switch head.ResourceType {
		case "Coverage":
			coverages++
		case "Organization":
		case "OperationOutcome":
			// A searchset may carry the server's warnings: not routed by.
		default:
			return nil, 0, query, fhirServerRefused, fhirServerNotSearchset
		}
	}
	return body, coverages, query, 0, ""
}

// payorOrganizationID returns the id of the Organization a Coverage.payor
// reference names on base: "Organization/<id>", or the same written absolute
// on base (when there is one). Anything else (another type, another server, a
// version) is not read.
func payorOrganizationID(ref, base string) (string, bool) {
	rest, ok := strings.CutPrefix(ref, "Organization/")
	if !ok && base != "" {
		rest, ok = strings.CutPrefix(ref, strings.TrimRight(base, "/")+"/Organization/")
	}
	if !ok || !fhirIDRE.MatchString(rest) || rest == "." || rest == ".." {
		return "", false
	}
	return rest, true
}

// readOrganization reads the Organization id on the request's fhirServer:
// the payor a Coverage names by reference alone. It returns the resource, the
// path it read (no host) and, on a refusal, the status and reason.
func (r fhirServerReader) readOrganization(ctx context.Context, req fhirServerRead, id string) (value []byte, query string, status int, msg string) {
	target, status, msg := r.target(req)
	if status != 0 {
		return nil, "", status, msg
	}
	target.Path = strings.TrimSuffix(target.Path, "/Coverage") + "/Organization/" + id
	target.RawQuery = ""
	query = target.EscapedPath()
	body, status, msg := r.get(ctx, target, req.auth, fhirServerNoOrganization)
	if msg == fhirServerNotSearchset {
		// Answered, but not with the Organization asked for.
		status, msg = http.StatusBadGateway, fhirServerNotOrganization
	}
	if status != 0 {
		return nil, query, status, msg
	}
	var org struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if decodeMessage(body, &org) != nil || org.ResourceType != "Organization" || org.ID != id {
		return nil, query, http.StatusBadGateway, fhirServerNotOrganization
	}
	return body, query, 0, ""
}

// get performs one fenced GET and returns the JSON body. notFound, when set,
// is the reason a 404 or 410 is refused with (412); otherwise those are the
// server's error like any other.
func (r fhirServerReader) get(ctx context.Context, target *url.URL, auth fhirServerAuth, notFound string) ([]byte, int, string) {
	ctx, cancel := context.WithTimeout(ctx, fhirServerReadTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fhirServerRefused, fhirServerNotBase
	}
	hreq.Header.Set("Accept", "application/fhir+json")
	if auth.present {
		hreq.Header.Set("Authorization", "Bearer "+auth.token)
	}
	resp, err := r.client().Do(hreq)
	if err != nil {
		var dnsErr *fhirServerDNSError
		var addrErr *fhirServerAddrError
		var certErr *tls.CertificateVerificationError
		var unknownAuth x509.UnknownAuthorityError
		var hostErr x509.HostnameError
		switch {
		case errors.As(err, &addrErr):
			return nil, fhirServerRefused, addrErr.msg
		case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
			// Before the address: a lookup that ran out the budget did not
			// answer in time; it did not fail to resolve.
			return nil, fhirServerRefused, fhirServerTimedOut
		case errors.As(err, &dnsErr):
			return nil, fhirServerRefused, fhirServerNoAddress
		case errors.As(err, &certErr), errors.As(err, &unknownAuth), errors.As(err, &hostErr):
			return nil, fhirServerRefused, fhirServerTLSFailed
		}
		return nil, fhirServerRefused, fhirServerUnreachable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 3:
		return nil, fhirServerRefused, fhirServerRedirected
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fhirServerRefused, fhirServerRefusedToken
	case notFound != "" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone):
		return nil, fhirServerRefused, notFound
	case resp.StatusCode != http.StatusOK:
		return nil, fhirServerRefused, fhirServerAnswered
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fhirServerMaxBody+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, fhirServerRefused, fhirServerTimedOut
		}
		return nil, fhirServerRefused, fhirServerUnreachable
	}
	if len(body) > fhirServerMaxBody {
		return nil, fhirServerRefused, fhirServerTooLarge
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt != "application/fhir+json" && mt != "application/json" {
		return nil, fhirServerRefused, fhirServerNotSearchset
	}
	return bytes.TrimSpace(body), 0, ""
}

// routingCoverages is the searchset a request is routed by: its Coverages
// as routingCoverageChoice picks them (the active ones when any is active,
// else all of them), with every other entry (the payor Organizations) kept.
// The answer is never carried, so the rebuilt Bundle is only what routing
// reads.
func routingCoverages(searchset []byte) []byte {
	var bundle struct {
		Entry []json.RawMessage `json:"entry"`
	}
	if decodeMessage(searchset, &bundle) != nil {
		return searchset
	}
	var covs [][]byte
	var at []int
	for i, e := range bundle.Entry {
		var entry struct {
			Resource json.RawMessage `json:"resource"`
		}
		var head struct {
			ResourceType string `json:"resourceType"`
		}
		if decodeMessage(e, &entry) != nil || decodeMessage(entry.Resource, &head) != nil {
			return searchset
		}
		if head.ResourceType == "Coverage" {
			covs, at = append(covs, entry.Resource), append(at, i)
		}
	}
	chosen := routingCoverageChoice(covs)
	if len(chosen) == len(covs) {
		return searchset
	}
	drop := make(map[int]bool, len(at))
	for _, i := range at {
		drop[i] = true
	}
	for _, c := range chosen {
		drop[at[c]] = false
	}
	kept := make([]json.RawMessage, 0, len(bundle.Entry))
	for i, e := range bundle.Entry {
		if !drop[i] {
			kept = append(kept, e)
		}
	}
	out, err := json.Marshal(struct {
		ResourceType string            `json:"resourceType"`
		Type         string            `json:"type"`
		Entry        []json.RawMessage `json:"entry"`
	}{"Bundle", "searchset", kept})
	if err != nil {
		return searchset
	}
	return out
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// fhirServerHost is the host a read went to, for the log line: never its path,
// query or credentials.
func fhirServerHost(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return "?"
	}
	return u.Hostname()
}
