//go:build !shn_coreonly

package engine

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	"github.com/SmartHealthNetwork/shn-gateway/pa/dtraddress"
)

// The subscriber's gateway's edits of a notification it delivers to its own
// system (the notifications design §6.2 step 5, §6.4, §8): every registered
// edit of a topic's signal or notify leg that the subscriber's gateway makes,
// as recipient, on the request (relayEdits: E-17 on cdex-task-signal, and
// E-22 on pas-subscription-notify with the PAS lane), is decided by its maker
// (deliveryEdits) and made through relay, which verifies it, keeps the
// signatures it changed, and lets the delivery disclose it (X-SHN-Edited,
// `; signed=` where a signature covered what changed). Nothing else changes a
// notification on its way to the subscriber's endpoint.

// deliveryInput is what a delivery's edits are made from: the notifier the
// Hub authenticated, and the base its gateway stated in the request frame.
type deliveryInput struct {
	notifier      string
	statedBase    string
	hasStatedBase bool
}

// deliveryEdit decides one registered edit of a notification: the operations
// over doc that make it, none when it is not made (each reason recorded on x).
// doc is nil when the notification cannot be read as one JSON document, when
// the edit records why it was not made and makes none.
type deliveryEdit func(g *Gateway, x *exchangeRecorder, doc *relay.Document, in deliveryInput) ([]relay.Op, error)

// deliveryEdits are the makers of the registered edits a subscriber's gateway
// makes on a notification it delivers, by the edit's id. Every registered
// edit of a signal or notify leg on the recipient's request has one
// (TestDeliveryEdits_EveryRegisteredEditHasAMaker).
var deliveryEdits = map[relay.EditID]deliveryEdit{
	relay.EditCDexTaskNotificationAddressRekey: (*Gateway).notificationAddressRekey,
}

// deliveryEditIDs are the registered edits of leg that the recipient makes on
// the request (relayEdits), in the registry's order.
func deliveryEditIDs(leg string) []relay.EditID {
	var ids []relay.EditID
	for _, e := range relayEdits {
		if e.Role == relay.RoleRecipient && e.Direction == relay.DirectionRequest && slices.Contains(e.Legs, leg) {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// noDelivery is the payload a notification that is not delivered carries:
// none.
var noDelivery relay.Payload

// deliveredNotification is the notification body, as the subscriber's
// gateway delivers it on leg to its own system with media type ct: relayed
// exactly when no edit is made, and otherwise with each registered edit the
// recipient makes on the leg's request made through relay (deliveryEditIDs,
// deliveryEdits). err is an edit failing relay's verification, or a
// registered edit with no maker: this gateway's own fault, nothing delivered.
func (g *Gateway) deliveredNotification(x *exchangeRecorder, leg string, body []byte, ct string, in deliveryInput) (relay.Payload, error) {
	rb := relay.NewBody(body, relay.OriginPeerFrame)
	ids := deliveryEditIDs(leg)
	if len(ids) == 0 {
		return relay.Exact(rb, ct), nil
	}
	doc, err := relay.Doc(rb)
	if err != nil {
		doc = nil
	}
	var changes []relay.Change
	for _, id := range ids {
		make, ok := deliveryEdits[id]
		if !ok {
			return noDelivery, fmt.Errorf("engine: the registered edit %s of %s has no maker", id, leg)
		}
		ops, err := make(g, x, doc, in)
		if err != nil {
			return noDelivery, err
		}
		if len(ops) > 0 {
			changes = append(changes, relay.Change{Edit: id, Ops: ops})
		}
	}
	if len(changes) == 0 {
		return relay.Exact(rb, ct), nil
	}
	return relay.ApplyChanges(rb, ct, changes...)
}

// notificationAddressRekey is E-17 (cdex-task-notification-address-rekey;
// the CDex design §15.7, D129): each absolute http or https address into the
// provider's server in a CDex Task notification (notificationAddresses) that
// lies under the base the provider's gateway stated (statedBase), by
// dtraddress.Under's segment-boundary match, is re-keyed to this gateway's
// own base for the provider that sent it (cdexroute.ProviderBase), the rest
// of the address kept byte for byte. A relative reference, and a fullUrl that
// is a urn:uuid, is no address into the server and is carried as sent. Each
// address it does not re-key is carried as sent, never guessed, and why is
// recorded: no statedBase in the request frame (recorded for every such
// notification), an address not under it, or no base of this gateway's for
// the provider. A notification whose places E-17 cannot read one way only
// (relay cannot read it, or a member name it reads names more than one
// member) is an edit this gateway cannot make: err, nothing delivered, the
// fidelity of SHN's own edit. The signal handler refuses such a notification
// before delivery (notificationReadable), so err is its last guard.
func (g *Gateway) notificationAddressRekey(x *exchangeRecorder, doc *relay.Document, in deliveryInput) ([]relay.Op, error) {
	if doc == nil {
		return nil, errNotificationUnreadable
	}
	places, readable := notificationAddresses(doc)
	if !readable {
		return nil, errNotificationUnreadable
	}
	if !in.hasStatedBase || in.statedBase == "" {
		x.editNotMade(EditNotMadeNotificationNoStatedBase)
	}
	counterpart, baseErr := cdexroute.ProviderBase(g.cfg.IngressBaseURL, in.notifier)
	var ops []relay.Op
	for _, n := range places {
		v, err := doc.StringValue(n)
		if err != nil || !absoluteHTTP(v) {
			continue
		}
		switch {
		case !in.hasStatedBase || in.statedBase == "":
			continue
		case baseErr != nil:
			x.editNotMade(EditNotMadeNotificationNoBase)
			continue
		}
		rekeyed, ok := rekeyUnderBase(v, in.statedBase, counterpart)
		if !ok {
			x.editNotMade(EditNotMadeNotificationNotUnderBase)
			continue
		}
		value, err := jsonValue(rekeyed)
		if err != nil {
			return nil, fmt.Errorf("engine: E-17 cannot write a re-keyed address: %w", err)
		}
		ops = append(ops, doc.Replace(n, value))
	}
	return ops, nil
}

// errNotificationUnreadable is E-17 unable to read a notification's places
// one way only.
var errNotificationUnreadable = errors.New("engine: E-17 cannot read the notification's addresses one way only")

// absoluteHTTP reports whether v is an absolute http or https URL with a
// host: an address into a server. A relative reference and a urn:uuid are
// not.
func absoluteHTTP(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// unreadableAddress reports whether v reads, to a lenient URL reader, as an
// address into a server that E-17 cannot read by structure: once the reader
// strips C0 controls and spaces from either end, drops each tab, line feed
// and carriage return, and takes a backslash for a slash (as the WHATWG URL
// parser does for http and https), v opens with the http: or https: scheme,
// in any case, or with two slashes (a network-path reference, which carries
// a host), yet v as written is not an absolute http or https URL with a host
// (absoluteHTTP) holding none of those bytes and naming its server plainly
// (plainAuthority). Such an address is neither re-keyed nor plainly someone
// else's: a client may follow it into the provider's server from the payer's
// system, where E-17's match (dtraddress.Under) reads another server.
func unreadableAddress(v string) bool {
	read := strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r':
			return -1
		case '\\':
			return '/'
		}
		return r
	}, strings.TrimFunc(v, func(r rune) bool { return r <= ' ' }))
	lower := strings.ToLower(read)
	if !strings.HasPrefix(lower, "http:") && !strings.HasPrefix(lower, "https:") && !strings.HasPrefix(read, "//") {
		return false
	}
	return read != v || !absoluteHTTP(v) || !plainAuthority(v)
}

// plainAuthority reports whether v, an absolute http or https URL, names its
// server so that every URL reader reads the same one: no userinfo; a host of
// non-empty dot-separated labels of ASCII letters, digits, hyphens and
// underscores (no percent-encoding, which the WHATWG parser decodes, no
// other character, which it maps by IDNA, and no trailing dot), and, when
// its last label opens with a digit (which the WHATWG parser reads as an IPv4
// number, in decimal, octal or hexadecimal), a dotted IPv4 address in its
// canonical form; or a bracketed IPv6 address in its canonical form, not an
// IPv4-mapped one (which readers spell two ways); and a port, when a colon
// opens one, of digits with no leading zero, at most 65535. Each
// form it refuses names, to some reader, a server that E-17's match does not
// read as the stated base's.
func plainAuthority(v string) bool {
	_, rest, ok := strings.Cut(v, "://")
	if !ok {
		return false
	}
	auth := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		auth = rest[:i]
	}
	host, port := auth, ""
	if strings.HasPrefix(auth, "[") {
		end := strings.Index(auth, "]")
		if end < 0 {
			return false
		}
		host, port = auth[1:end], auth[end+1:]
		a, err := netip.ParseAddr(host)
		if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" || a.String() != strings.ToLower(host) {
			return false
		}
	} else {
		if i := strings.LastIndex(auth, ":"); i >= 0 {
			host, port = auth[:i], auth[i:]
		}
		if !plainHostName(host) {
			return false
		}
	}
	if port == "" {
		return true
	}
	p, ok := strings.CutPrefix(port, ":")
	if !ok || p == "" || len(p) > 5 || p[0] == '0' || strings.Trim(p, "0123456789") != "" {
		return false
	}
	n, err := strconv.Atoi(p)
	return err == nil && n <= 65535
}

// plainHostName reports whether host is a name every URL reader reads alike
// (plainAuthority).
func plainHostName(host string) bool {
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if l == "" {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			// c|0x20 folds an ASCII capital to its lower-case letter.
			if lower := c | 0x20; !('a' <= lower && lower <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	if last := labels[len(labels)-1]; '0' <= last[0] && last[0] <= '9' {
		a, err := netip.ParseAddr(host)
		return err == nil && a.Is4() && a.String() == host
	}
	return true
}

// notificationRead is how a notification's E-17 places read
// (readNotification).
type notificationRead int

const (
	// notificationPlacesRead: every value at E-17's places reads one way
	// only, and each into the provider's own server can be re-keyed.
	notificationPlacesRead notificationRead = iota
	// notificationUnreadable: relay cannot read the notification, or a member
	// name E-17 reads, folded, names more than one member
	// (notificationAddresses), so E-17 could not be made on it one way only.
	notificationUnreadable
	// notificationAddressUnreadable: a value at one of E-17's places is an
	// unreadableAddress.
	notificationAddressUnreadable
	// notificationAddressNotRekeyable: a value at one of E-17's places names
	// the provider's own server (providerAuthority) and is not under base by
	// E-17's match (rekeyUnderBase: a fragment, a dot segment, a path
	// outside the base, escaped or not): E-17 cannot write it at the payer
	// gateway's base, and it would carry the provider's own address to the
	// payer.
	notificationAddressNotRekeyable
)

// readNotification reads body as E-17 will, against base, the provider's FHIR
// base its gateway states ("" when none is stated, in which case no value is
// judged against it). Both gateways refuse a notification that does not read
// as notificationPlacesRead, the provider's before it sends and the payer's
// before it delivers.
func readNotification(body []byte, origin relay.Origin, base string) notificationRead {
	doc, err := relay.Doc(relay.NewBody(body, origin))
	if err != nil {
		return notificationUnreadable
	}
	places, ok := notificationAddresses(doc)
	if !ok {
		return notificationUnreadable
	}
	read := notificationPlacesRead
	for _, n := range places {
		v, err := doc.StringValue(n)
		switch {
		case err != nil:
			continue
		case unreadableAddress(v):
			return notificationAddressUnreadable
		case base != "" && absoluteHTTP(v) && providerAuthority(v, base):
			if _, ok := rekeyUnderBase(v, base, base); !ok {
				read = notificationAddressNotRekeyable
			}
		}
	}
	return read
}

// providerAuthority reports whether v, an absolute http or https URL naming
// its server plainly (plainAuthority), names the server base does: the same
// scheme, the same host (ASCII, compared without case) and the same port,
// a scheme's default port read as written. It is read from the parsed URLs,
// never by a string prefix.
func providerAuthority(v, base string) bool {
	u, err := url.Parse(v)
	b, berr := url.Parse(base)
	if err != nil || berr != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, b.Scheme) && strings.EqualFold(u.Hostname(), b.Hostname()) && effectivePort(u) == effectivePort(b)
}

// effectivePort is u's port, or its scheme's default when it names none.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// rekeyUnderBase is v re-keyed from base to counterpart when v is under base
// by dtraddress.Under's structural match (scheme, host and port, then path
// segment by segment, never a string prefix): counterpart, then the escaped
// path below base, as written, then a query after the path as sent. ok is
// false for any other value: one with a fragment, and one whose remainder
// holds a dot segment, raw or percent-encoded (dtraddress.DotFree), which a
// client would resolve above counterpart, into another provider's base.
func rekeyUnderBase(v, base, counterpart string) (string, bool) {
	if strings.Contains(v, "#") {
		return "", false
	}
	path, query, hasQuery := strings.Cut(v, "?")
	rem, ok := dtraddress.Under(path, strings.TrimRight(base, "/"))
	if !ok || !dtraddress.DotFree(rem) {
		return "", false
	}
	out := strings.TrimRight(counterpart, "/") + rem
	if hasQuery {
		out += "?" + query
	}
	return out, true
}

// notificationAddresses are the string values of a notification's places
// E-17 reads, by structure: the url of each of the root Bundle's links, each
// of its entries' fullUrl, request.url, response.location and the url of each
// of the entry's links, and in the resource of its first entry, a Parameters (the
// SubscriptionStatus), the valueReference.reference of each parameter named
// subscription and of each part named focus of each parameter named
// notification-event. Member names are read folded, as relay's verifier reads
// them; readable is false when a name read so names more than one member, in
// which case that member is not read.
func notificationAddresses(doc *relay.Document) (places []relay.NodeID, readable bool) {
	readable = true
	member := func(obj relay.NodeID, name string) (relay.NodeID, bool) {
		if doc.Kind(obj) != relay.KindObject {
			return 0, false
		}
		var found []relay.NodeID
		for _, m := range doc.Members(obj) {
			if jsonmember.FoldedName(m.Name) == jsonmember.FoldedName(name) {
				found = append(found, m.Value)
			}
		}
		if len(found) > 1 {
			readable = false
			return 0, false
		}
		if len(found) == 0 {
			return 0, false
		}
		return found[0], true
	}
	str := func(obj relay.NodeID, name string) (relay.NodeID, bool) {
		n, ok := member(obj, name)
		if !ok || doc.Kind(n) != relay.KindString {
			return 0, false
		}
		return n, true
	}
	elems := func(obj relay.NodeID, name string) []relay.NodeID {
		n, ok := member(obj, name)
		if !ok || doc.Kind(n) != relay.KindArray {
			return nil
		}
		return doc.Elems(n)
	}
	named := func(obj relay.NodeID) string {
		n, ok := str(obj, "name")
		if !ok {
			return ""
		}
		s, _ := doc.StringValue(n)
		return s
	}
	reference := func(holder relay.NodeID) {
		if ref, ok := member(holder, "valueReference"); ok {
			if n, ok := str(ref, "reference"); ok {
				places = append(places, n)
			}
		}
	}
	links := func(obj relay.NodeID) {
		for _, l := range elems(obj, "link") {
			if n, ok := str(l, "url"); ok {
				places = append(places, n)
			}
		}
	}
	root := doc.Root()
	links(root)
	entries := elems(root, "entry")
	for _, e := range entries {
		if n, ok := str(e, "fullUrl"); ok {
			places = append(places, n)
		}
		if req, ok := member(e, "request"); ok {
			if n, ok := str(req, "url"); ok {
				places = append(places, n)
			}
		}
		if resp, ok := member(e, "response"); ok {
			if n, ok := str(resp, "location"); ok {
				places = append(places, n)
			}
		}
		links(e)
	}
	if len(entries) == 0 {
		return places, readable
	}
	status, ok := member(entries[0], "resource")
	if !ok {
		return places, readable
	}
	for _, p := range elems(status, "parameter") {
		switch named(p) {
		case "subscription":
			reference(p)
		case "notification-event":
			for _, part := range elems(p, "part") {
				if named(part) == "focus" {
					reference(part)
				}
			}
		}
	}
	return places, readable
}
