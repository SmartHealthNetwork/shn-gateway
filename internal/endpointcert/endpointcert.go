// Package endpointcert records when the certificates this gateway depends on
// expire, by closed endpoint kind and certificate role (MON-R25): the server
// certificate each outbound endpoint presents, and the gateway's own
// certificates and loaded trust roots as they are read. The
// certificate-expiry gauge reads Soonest; nothing here exports anything.
//
// Every outbound HTTP client the gateway builds has its transport Wrapped
// with its kind. The wrapper sends each request through the client's own
// transport as before, so connections, pools and every setting are
// unchanged, and reads the certificate from each response that came over
// TLS, after verification. The store database, which has no HTTP response,
// takes VerifyConnection instead, which runs after verification too.
//
// The kind and role lists are closed and this package is their source: the
// telemetry registry references them, never the reverse. The package imports
// only the standard library.
package endpointcert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Kind is a closed endpoint kind. Its only values are the ones declared
// below; the zero Kind is no kind, and nothing is recorded under it.
type Kind struct{ name string }

// String is the kind's attribute value (shn.endpoint.kind).
func (k Kind) String() string { return k.name }

// Valid reports whether k is one of the declared kinds.
func (k Kind) Valid() bool { return slices.Contains(kinds, k) }

// The endpoint kinds. Own: the gateway's own listeners and its OTLP client
// certificate. Outbound: one kind per endpoint setting that can name a host
// of its own, so an expiry says which certificate to renew; a setting left
// to fall back on another's URL reports that certificate under its own kind.
// TrustRootKind: the CA bundles it loaded.
var (
	Ingress     = Kind{"ingress"}
	MetricsPull = Kind{"metrics-pull"}
	OTLP        = Kind{"otlp"}

	FHIR             = Kind{"fhir"}              // FHIR_DATA_URL
	FHIRToken        = Kind{"fhir-token"}        // FHIR_TOKEN_URL
	PayerDavinci     = Kind{"payer-davinci"}     // PAYER_DAVINCI_BASE_URL
	PayerDTR         = Kind{"payer-dtr"}         // PAYER_DAVINCI_DTR_BASE_URL
	PayerPAS         = Kind{"payer-pas"}         // PAYER_DAVINCI_PAS_BASE_URL
	PayerEligibility = Kind{"payer-eligibility"} // PAYER_ELIGIBILITY_URL
	PayerToken       = Kind{"payer-token"}       // PAYER_DAVINCI_TOKEN_URL
	CDS              = Kind{"cds"}               // PAYER_DAVINCI_CDS_BASE_URL
	Populate         = Kind{"populate"}          // PROVIDER_DTR_POPULATE_URL
	PopulateToken    = Kind{"populate-token"}    // PROVIDER_DTR_POPULATE_TOKEN_URL
	CDexTask         = Kind{"cdex-task"}         // PROVIDER_CDEX_TASK_URL
	CDexTaskToken    = Kind{"cdex-task-token"}   // PROVIDER_CDEX_TOKEN_URL
	CRDFHIRServer    = Kind{"crd-fhir-server"}   // the fhirServer a CRD request names
	Discovery        = Kind{"discovery"}         // DISCOVERY_URL
	Hub              = Kind{"hub"}               // HUB_URL
	HubKey           = Kind{"hub-key"}           // HUB_TRANSPORT_KEY_URL, read once at boot
	Authorization    = Kind{"authorization"}     // AUTHZ_URL
	AuthorizationKey = Kind{"authorization-key"} // AUTHZ_PUBKEY_URL, read once at boot
	Registrar        = Kind{"registrar"}         // REGISTRAR_URL
	Consent          = Kind{"consent"}           // CONSENT_URL
	Audit            = Kind{"audit"}             // the discovered audit endpoint
	Validator        = Kind{"validator"}         // FHIR_VALIDATE_URL[_<line>], or the line's default lane; by IG line
	Certify          = Kind{"certify"}           // FHIR_CERTIFY_URL_<line>; by IG line
	Diagnostic       = Kind{"diagnostic"}        // DIAGNOSTIC_SINK_URL
	Store            = Kind{"store"}             // SHN_STORE_DATABASE_URL

	TrustRootKind = Kind{"trust-root"}
)

var kinds = []Kind{
	Ingress, MetricsPull, OTLP,
	FHIR, FHIRToken, PayerDavinci, PayerDTR, PayerPAS, PayerEligibility, PayerToken, CDS,
	Populate, PopulateToken, CDexTask, CDexTaskToken, CRDFHIRServer,
	Discovery, Hub, HubKey, Authorization, AuthorizationKey, Registrar, Consent, Audit,
	Validator, Certify,
	Diagnostic, Store,
	TrustRootKind,
}

// Kinds is every endpoint kind, in catalogue order.
func Kinds() []Kind { return slices.Clone(kinds) }

// KindNames is Kinds as attribute values.
func KindNames() []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = k.name
	}
	return out
}

// Role is a closed certificate role (shn.cert.role).
type Role struct{ name string }

// String is the role's attribute value.
func (r Role) String() string { return r.name }

// Valid reports whether r is one of the declared roles.
func (r Role) Valid() bool { return slices.Contains(roles, r) }

// The certificate roles: the gateway's own certificate, a server's
// certificate seen at a handshake, and a loaded trust root.
var (
	Self      = Role{"self"}
	Server    = Role{"server"}
	TrustRoot = Role{"trust-root"}
)

var roles = []Role{Self, Server, TrustRoot}

// Roles is every certificate role.
func Roles() []Role { return slices.Clone(roles) }

// RoleNames is Roles as attribute values.
func RoleNames() []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = r.name
	}
	return out
}

// Tag is what a client is tagged with and a certificate is recorded under:
// an endpoint kind and, for the validator and certify kinds only, the IG
// line of the lane (shn.ig_line), so each lane is its own series and a new
// line needs no new kind. The zero Tag tags nothing.
type Tag struct {
	kind   Kind
	igLine string
}

// Of is the tag of a kind that has no lanes.
func Of(kind Kind) Tag { return Tag{kind: kind} }

// Lane is the tag of the validator or certify lane for igLine.
func Lane(kind Kind, igLine string) Tag { return Tag{kind: kind, igLine: igLine} }

// Kind is the tag's endpoint kind.
func (t Tag) Kind() Kind { return t.kind }

// IGLine is the tag's IG line: set for a validator or certify lane, empty
// for every other kind. Its values are the contract lines the gateway's
// lanes are built for; the registry folds any other to "other".
func (t Tag) IGLine() string { return t.igLine }

// Valid reports whether t is a declared kind, with an IG line exactly when
// the kind has lanes.
func (t Tag) Valid() bool {
	if !t.kind.Valid() {
		return false
	}
	return (t.igLine != "") == t.kind.HasLanes()
}

// HasLanes reports whether k is recorded per IG line.
func (k Kind) HasLanes() bool { return k == Validator || k == Certify }

// MaxEndpointsPerSeries bounds the endpoints remembered per series (tag and
// role). A configured kind has one or a handful; one whose endpoints a
// request names (crd-fhir-server) is open-ended, so the endpoint seen
// longest ago is forgotten first. Endpoint names never leave this package.
const MaxEndpointsPerSeries = 32

type series struct {
	tag  Tag
	role Role
}

type entry struct {
	notAfter time.Time
	at       time.Time // when it was last recorded, on the Recorder's clock
	seen     uint64    // the Recorder's sequence at the last record: the eviction order
}

// MaxAge is how long a server certificate reading stands without a new
// response from its endpoint: an endpoint the gateway has stopped calling
// (a crd-fhir-server host a request named once) ages out, and its kind then
// reads as unknown, not as that certificate. The boot-read kinds and the
// gateway's own and trust-root certificates do not age: each was read once,
// and stays what was read.
const MaxAge = 24 * time.Hour

// ages reports whether a series' readings age out.
func (s series) ages() bool {
	if s.role != Server {
		return false
	}
	switch s.tag.kind {
	case Discovery, HubKey, AuthorizationKey:
		return false
	}
	return true
}

// Recorder holds, per tag and role, each endpoint's certificate NotAfter
// from its last record. Use New; a nil *Recorder records nothing and tags
// nothing.
type Recorder struct {
	mu      sync.Mutex
	now     func() time.Time
	seq     uint64
	entries map[series]map[string]entry
	// made is every tag a transport was wrapped for: bounded by the closed
	// kinds and the gateway's lines, so a test can tell what a build tagged.
	made map[Tag]bool
}

// New returns an empty Recorder on the wall clock.
func New() *Recorder { return NewWithClock(nil) }

// NewWithClock returns an empty Recorder whose readings age by now (MaxAge);
// nil means time.Now.
func NewWithClock(now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	return &Recorder{now: now, entries: map[series]map[string]entry{}, made: map[Tag]bool{}}
}

// Record sets endpoint's certificate NotAfter under tag and role, replacing
// what that endpoint had: a renewed certificate replaces the old one instead
// of pinning the earlier expiry. An invalid tag, an undeclared role or an
// empty endpoint records nothing.
func (r *Recorder) Record(tag Tag, role Role, endpoint string, notAfter time.Time) {
	if r == nil || !tag.Valid() || !role.Valid() || endpoint == "" {
		return
	}
	s := series{tag, role}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	m := r.entries[s]
	if m == nil {
		m = map[string]entry{}
		r.entries[s] = m
	}
	if _, ok := m[endpoint]; !ok && len(m) >= MaxEndpointsPerSeries {
		oldest, first := "", true
		for name, e := range m {
			if first || e.seen < m[oldest].seen {
				oldest, first = name, false
			}
		}
		delete(m, oldest)
	}
	m[endpoint] = entry{notAfter: notAfter, at: r.now(), seen: r.seq}
}

// Soonest is the earliest NotAfter recorded under tag and role. ok is false
// when nothing has been recorded there (before its first handshake, or for
// a kind this gateway does not use): unknown, never zero (MON-R15).
func (r *Recorder) Soonest(tag Tag, role Role) (notAfter time.Time, ok bool) {
	if r == nil {
		return time.Time{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := series{tag, role}
	return r.soonestLocked(s, r.entries[s])
}

// soonestLocked is the earliest NotAfter of s's readings that have not aged
// out, dropping those that have. Requires mu.
func (r *Recorder) soonestLocked(s series, m map[string]entry) (notAfter time.Time, ok bool) {
	now := r.now()
	for name, e := range m {
		if s.ages() && now.Sub(e.at) >= MaxAge {
			delete(m, name)
			continue
		}
		if !ok || e.notAfter.Before(notAfter) {
			notAfter, ok = e.notAfter, true
		}
	}
	return notAfter, ok
}

// Reading is one series' soonest NotAfter.
type Reading struct {
	Tag      Tag
	Role     Role
	NotAfter time.Time
}

// Readings is every series with a record, each at its soonest NotAfter, in
// catalogue order (kind, then IG line, then role). A series with no record
// is absent, never a zero.
func (r *Recorder) Readings() []Reading {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Reading, 0, len(r.entries))
	for s, m := range r.entries {
		if na, ok := r.soonestLocked(s, m); ok {
			out = append(out, Reading{Tag: s.tag, Role: s.role, NotAfter: na})
		}
	}
	slices.SortFunc(out, func(a, b Reading) int {
		if c := slices.Index(kinds, a.Tag.kind) - slices.Index(kinds, b.Tag.kind); c != 0 {
			return c
		}
		if c := strings.Compare(a.Tag.igLine, b.Tag.igLine); c != 0 {
			return c
		}
		return slices.Index(roles, a.Role) - slices.Index(roles, b.Role)
	})
	return out
}

// VerifyConnection returns a tls.Config.VerifyConnection hook for tag that
// first runs next (the hook the config already had; nil for none) and
// returns its error unchanged. Only a connection both have accepted records
// its leaf's NotAfter as a server certificate. The hook never fails a
// connection itself.
func (r *Recorder) VerifyConnection(tag Tag, next func(tls.ConnectionState) error) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if next != nil {
			if err := next(cs); err != nil {
				return err
			}
		}
		r.RecordConnection(tag, cs)
		return nil
	}
}

// RecordConnection records the server certificate of a connection whose
// verification has already passed, under tag: for a client whose calls
// carry no *http.Response to read it from (a gRPC call's peer). A state with
// no peer certificate records nothing.
func (r *Recorder) RecordConnection(tag Tag, cs tls.ConnectionState) {
	if len(cs.PeerCertificates) > 0 {
		leaf := cs.PeerCertificates[0]
		r.Record(tag, Server, endpointName(cs.ServerName, leaf), leaf.NotAfter)
	}
}

// RecordOwn records cert's leaf as this gateway's own (self) certificate of
// kind, named by the setting it was read from: read once, when it is
// loaded. A nil or unreadable certificate records nothing.
func (r *Recorder) RecordOwn(kind Kind, setting string, cert *tls.Certificate) {
	if r == nil || cert == nil || len(cert.Certificate) == 0 {
		return
	}
	leaf := cert.Leaf
	if leaf == nil {
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return
		}
		leaf = parsed
	}
	r.Record(Of(kind), Self, setting, leaf.NotAfter)
}

// endpointName keys an endpoint by the name the client asked for. An IP
// literal sends no server name, so it is keyed by the leaf's subject
// alternative names instead, which a renewal of the same endpoint keeps.
func endpointName(serverName string, leaf *x509.Certificate) string {
	if serverName != "" {
		return "name:" + serverName
	}
	var b strings.Builder
	b.WriteString("san:")
	for _, n := range leaf.DNSNames {
		b.WriteString(n + ",")
	}
	for _, ip := range leaf.IPAddresses {
		b.WriteString(ip.String() + ",")
	}
	for _, u := range leaf.URIs {
		b.WriteString(u.String() + ",")
	}
	if b.Len() == len("san:") {
		b.WriteString("subject:" + leaf.Subject.String())
	}
	return b.String()
}

// Wrap returns base tagged with tag: a RoundTripper that sends each request
// through base exactly as before (http.DefaultTransport, read at each request,
// when base is nil, as an http.Client with no transport does), and then
// records the server certificate of each response that came over TLS
// (resp.TLS.PeerCertificates[0]) under tag. Verification has already run
// when there is a response, so nothing about it changes; a response on a
// reused connection records too. The connections, their pool and every
// transport setting stay base's. A nil Recorder or an invalid tag returns
// base as it is.
func (r *Recorder) Wrap(tag Tag, base http.RoundTripper) http.RoundTripper {
	return r.WrapRouted(tag, base)
}

// WrapRouted is Wrap, but a request whose context names one of extra
// (WithTag) is recorded under that tag instead of def: one client, one
// bearer layer and one connection pool serve several settings' kinds. A
// request whose context names any other kind is recorded under none.
func (r *Recorder) WrapRouted(def Tag, base http.RoundTripper, extra ...Tag) http.RoundTripper {
	if r == nil || !def.Valid() {
		return base
	}
	r.mu.Lock()
	r.made[def] = true
	for _, t := range extra {
		if t.Valid() {
			r.made[t] = true
		}
	}
	r.mu.Unlock()
	return &tagged{rec: r, def: def, extra: slices.Clone(extra), base: base}
}

// WrapOwn is Wrap for a client whose requests are made under another
// client's mark: a token client a bearer layer calls with its operation's
// context. Each request is marked with tag before it is sent and recorded
// under it, whatever mark it arrived with.
func (r *Recorder) WrapOwn(tag Tag, base http.RoundTripper) http.RoundTripper {
	rt := r.Wrap(tag, base)
	if t, ok := rt.(*tagged); ok {
		t.own = true
	}
	return rt
}

// tagged is a RoundTripper Wrap made.
type tagged struct {
	rec   *Recorder
	def   Tag
	extra []Tag
	own   bool              // WrapOwn: every request is this tag's, whatever it is marked
	base  http.RoundTripper // nil: http.DefaultTransport, read at each use
}

func (t *tagged) transport() http.RoundTripper {
	if t.base == nil {
		return http.DefaultTransport
	}
	return t.base
}

// RoundTrip implements http.RoundTripper.
func (t *tagged) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.own {
		req = req.WithContext(WithTag(req.Context(), t.def))
	}
	resp, err := t.transport().RoundTrip(req)
	if resp != nil && resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		tag := t.def
		if c, ok := TagOf(req.Context()); ok && c != t.def {
			// A request marked with a kind this client does not serve is
			// recorded under none, never folded into its default.
			if !slices.Contains(t.extra, c) {
				return resp, err
			}
			tag = c
		}
		leaf := resp.TLS.PeerCertificates[0]
		t.rec.Record(tag, Server, endpointName(resp.TLS.ServerName, leaf), leaf.NotAfter)
	}
	return resp, err
}

// CloseIdleConnections closes base's idle connections, as http.Client's
// CloseIdleConnections did on base itself.
func (t *tagged) CloseIdleConnections() {
	if c, ok := t.transport().(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// Client returns a shallow copy of base (a fresh client when nil) whose
// transport is base's own, Wrapped with tag: the same timeout, redirect
// policy, jar and connections. A nil Recorder returns a copy of base as it is.
func (r *Recorder) Client(tag Tag, base *http.Client) *http.Client {
	var out http.Client
	if base != nil {
		out = *base
	}
	out.Transport = r.Wrap(tag, out.Transport)
	return &out
}

// Unwrap reports the transport rt sends through and the tag it records
// under (its default, for a routed one) when rt is one Wrap made; ok is
// false otherwise. base is nil for a wrapper of http.DefaultTransport.
func Unwrap(rt http.RoundTripper) (base http.RoundTripper, tag Tag, ok bool) {
	t, ok := rt.(*tagged)
	if !ok {
		return rt, Tag{}, false
	}
	return t.base, t.def, true
}

// Rewrap returns base Wrapped as from is (its recorder and tags), for a
// client built from another one's settings: base itself when from is not a
// wrapper.
func Rewrap(from, base http.RoundTripper) http.RoundTripper {
	t, ok := from.(*tagged)
	if !ok {
		return base
	}
	return &tagged{rec: t.rec, def: t.def, extra: t.extra, own: t.own, base: base}
}

// Made is every tag this Recorder has wrapped a transport for, in catalogue
// order.
func (r *Recorder) Made() []Tag {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Tag, 0, len(r.made))
	for t := range r.made {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b Tag) int {
		if c := slices.Index(kinds, a.kind) - slices.Index(kinds, b.kind); c != 0 {
			return c
		}
		return strings.Compare(a.igLine, b.igLine)
	})
	return out
}

type tagKey struct{}

// WithTag marks ctx's requests as going to an endpoint of tag, for a
// WrapRouted client: a call site that knows which setting its URL came from
// says so.
func WithTag(ctx context.Context, tag Tag) context.Context {
	return context.WithValue(ctx, tagKey{}, tag)
}

// TagOf is the tag WithTag put on ctx; ok is false when none.
func TagOf(ctx context.Context) (Tag, bool) {
	t, ok := ctx.Value(tagKey{}).(Tag)
	return t, ok
}
