package subscription

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/pa/dtraddress"
)

// The edits' names. The engine registers each in relay under its name
// and records it; nothing here registers them.
const (
	// EditHeaderSeal is E-18: the subscriber's gateway seals
	// the channel, removing channel.endpoint and channel.header from a create
	// or update it sends; both travel only sealed.
	EditHeaderSeal = "subscription-header-seal"
	// EditEndpointRekey is E-19: the notifier's gateway re-keys
	// channel.endpoint to its notification address and channel.header to its
	// credential and the sealed headers.
	EditEndpointRekey = "subscription-endpoint-rekey"
	// EditEndpointRestore is E-20: the subscriber's gateway restores
	// channel.endpoint and channel.header in every Subscription that opens,
	// and re-keys the answer's addresses under the notifier's stated base.
	EditEndpointRestore = "subscription-endpoint-restore"
	// EditURLSearchRekey is E-21: the notifier's gateway re-keys a search's
	// url to the notification address E-19 would write.
	EditURLSearchRekey = "subscription-url-search-rekey"
)

// Why E-20 was not made on a Subscription, or on the answer's addresses.
const (
	// NotMadeNoEndpoint: the Subscription states no channel.endpoint string.
	NotMadeNoEndpoint = "no-endpoint"
	// NotMadeForeignAddress: its endpoint is not a notification address of
	// the answering notifier's gateway; it is left as it is.
	NotMadeForeignAddress = "not-a-notification-address"
	// NotMadeAnotherBinding: its address names another subscriber or topic.
	NotMadeAnotherBinding = "another-subscriber-or-topic"
	// NotMadeNoSealedHeaders: its channel.header holds no one X-SHN-Channel
	// value.
	NotMadeNoSealedHeaders = "no-sealed-headers"
	// NotMadeKeyRetired: its sealed values are under a key that is neither
	// current nor the configured predecessor.
	NotMadeKeyRetired = "key-retired"
	// NotMadeSealInvalid: its sealed values do not open.
	NotMadeSealInvalid = "seal-invalid"
	// NotMadeNoStatedBase: the answer holds an http or https
	// fullUrl or link and the notifier stated no base, so none is re-keyed.
	NotMadeNoStatedBase = "no-stated-base"
	// NotMadeUnreadable: the Bundle's entry or link list is
	// present and not an array, or an entry is not an object, so nothing in
	// it is restored or re-keyed.
	NotMadeUnreadable = "unreadable-bundle"
	// NotMadeLinkURL: a url parameter of a link, any of them,
	// with or without a modifier, names a notification address, or cannot be
	// read, and neither the search this gateway framed (Search) nor a
	// Subscription restored in the same answer restores it to the
	// subscriber's endpoint; the link keeps that url as written.
	NotMadeLinkURL = "link-url-not-restored"
)

// Search is the search by url a subscriber's gateway framed for its system,
// as E-20 restores the answer's links from it: Endpoint, the
// endpoint the system searched by, and Sealed, the values this gateway sealed
// it to (SealEndpointForSearch: the current key's, and the predecessor's
// during an overlap), each "<kid>.<sealed>", which E-21 wrote into the
// notification addresses the notifier's server echoes. The zero Search is an
// interaction that framed none, such as a page.
type Search struct {
	Endpoint string
	Sealed   []string
}

// NotMade is an edit the library did not make, and why (each edit has
// its not-made reasons, which the engine records).
type NotMade struct{ Edit, Reason string }

// SealOutbound is E-18, at the subscriber's gateway on a subscribe leg's
// create or update: it seals the channel, channel.endpoint
// (SealEndpoint) and the channel.header values (SealHeaders, an absent header
// an empty set), and returns the body with the channel.endpoint and
// channel.header members removed and every other byte as it was, with the two
// sealed values for the request frame (subscriptionEndpoint,
// subscriptionChannel). Both travel only sealed, so neither the subscriber's
// endpoint nor its secret crosses the network in clear; the notifier's
// gateway writes channel.endpoint and channel.header again from the sealed
// values (E-19). A Subscription with no channel.endpoint string, a
// channel.header that is not an array of strings, a channel._header
// (extensions on the header values, which could not be restored), a
// channel._endpoint (extensions on the endpoint, which would cross the network
// and attach to the notifier gateway's notification address, read back on the
// endpoint restored; in any case of the name, as a lenient reader takes it),
// or a channel member a lenient reader would take for header (another case of
// the name, which would cross the network in clear) is not sealed, and err
// says why.
func SealOutbound(k Keys, b Binding, s *Subscription) (body []byte, sealedEndpoint, sealedHeaders string, err error) {
	ep, ok := s.Endpoint()
	if !ok || ep == "" {
		return nil, "", "", errors.New("subscription: the Subscription states no channel.endpoint")
	}
	ch := s.channel()
	// An endpoint in another case is no channel.endpoint (above); a header
	// in another case would cross the network in clear.
	if ch.memberFolded("header") != nil && ch.member("header") == nil {
		return nil, "", "", errors.New("subscription: channel names header in another case, which cannot be sealed one way only")
	}
	if ch.member("_header") != nil {
		return nil, "", "", errors.New("subscription: channel._header cannot be sealed and restored")
	}
	if ch.memberFolded("_endpoint") != nil {
		return nil, "", "", errors.New("subscription: channel._endpoint cannot be sealed: its extensions would cross the network and attach to the notification address")
	}
	headers, ok := s.headers()
	if !ok {
		return nil, "", "", errors.New("subscription: channel.header is not an array of strings")
	}
	if k.KeyID() == "" {
		return nil, "", "", errors.New("subscription: no keys")
	}
	sealedEndpoint = k.SealEndpoint(b, ep)
	if sealedEndpoint == "" {
		return nil, "", "", fmt.Errorf("subscription: channel.endpoint is too long to seal: its sealed value would be over the %d bytes an open reads", maxSealedLen)
	}
	sealedHeaders, err = k.SealHeaders(b, sealedEndpoint, headers)
	if err != nil {
		return nil, "", "", err
	}
	body, err = removeMembers(s.body, ch, "endpoint", "header")
	if err != nil {
		return nil, "", "", err
	}
	return body, sealedEndpoint, sealedHeaders, nil
}

// removeMembers is body with obj's members named names (those it has)
// removed, each with exactly one adjacent comma, and every other byte as it
// was. Two adjacent members are removed as one run, so no comma is taken
// twice.
func removeMembers(body []byte, obj *jnode, names ...string) ([]byte, error) {
	var keep []jmember
	gone := false
	for _, m := range obj.members {
		if slices.Contains(names, m.name) {
			gone = true
			continue
		}
		keep = append(keep, m)
	}
	if !gone {
		return slices.Clone(body), nil
	}
	// The object is rewritten member by member only where members go: each
	// removed run's bytes, from its first member's name to the next kept
	// member's name (or, at the end, from the last kept member's value to the
	// last removed member's value), are dropped.
	var sp []splice
	for i := 0; i < len(obj.members); {
		if !slices.Contains(names, obj.members[i].name) {
			i++
			continue
		}
		j := i
		for j+1 < len(obj.members) && slices.Contains(names, obj.members[j+1].name) {
			j++
		}
		switch {
		case j+1 < len(obj.members):
			sp = append(sp, splice{start: obj.members[i].nameStart, end: obj.members[j+1].nameStart})
		case i > 0:
			sp = append(sp, splice{start: obj.members[i-1].val.end, end: obj.members[j].val.end})
		default:
			sp = append(sp, splice{start: obj.members[i].nameStart, end: obj.members[j].val.end})
		}
		i = j + 1
	}
	return applySplices(body, sp)
}

// RekeyForNotifier is E-19, at the notifier's gateway on a subscribe leg's
// create or update forwarded to its system: it sets
// channel.endpoint to a.String() and channel.header to exactly two values,
// "Authorization: <k.Credential(a, sealedHeaders)>" and
// "X-SHN-Channel: <sealedHeaders>", and leaves every other byte as it was. The
// subscriber's gateway seals the channel (E-18), so the body it sends may
// state no channel.endpoint: E-19 then adds it, written from the sealed value
// alone. One it does state, a string, is overwritten. A Subscription with no
// channel object, a channel.endpoint that is not a string, a member of channel
// that a lenient reader would take for endpoint or header (another case of
// the name), or a channel._header is not re-keyed, and err says why.
func RekeyForNotifier(k Keys, a Address, sealedHeaders string, s *Subscription) (body []byte, err error) {
	ch := s.channel()
	if ch == nil || ch.kind != '{' {
		return nil, errors.New("subscription: the Subscription states no channel")
	}
	ep := ch.member("endpoint")
	if ep != nil && ep.kind != '"' {
		return nil, errors.New("subscription: channel.endpoint is not a string")
	}
	for _, name := range []string{"endpoint", "header"} {
		if ch.memberFolded(name) != nil && ch.member(name) == nil {
			return nil, errors.New("subscription: channel names " + name + " in another case, which cannot be re-keyed one way only")
		}
	}
	if ch.member("_header") != nil {
		return nil, errors.New("subscription: channel._header cannot be re-keyed")
	}
	if k.KeyID() == "" {
		return nil, errors.New("subscription: no keys")
	}
	cred := k.Credential(a, sealedHeaders)
	if cred == "" {
		return nil, errors.New("subscription: the sealed endpoint or sealed headers are not a sealed value, or the address is not one of this gateway")
	}
	headers := jsonStrings([]string{AuthorizationHeader + ": " + cred, ChannelHeader + ": " + sealedHeaders})
	var sp []splice
	var added []string // members added after channel's last, in this order
	if ep != nil {
		sp = append(sp, splice{start: ep.start, end: ep.end, repl: jsonString(a.String())})
	} else {
		added = append(added, string(jsonString("endpoint"))+":"+string(jsonString(a.String())))
	}
	if h := ch.member("header"); h != nil {
		sp = append(sp, splice{start: h.start, end: h.end, repl: headers})
	} else {
		added = append(added, string(jsonString("header"))+":"+string(headers))
	}
	if len(added) > 0 {
		sp = append(sp, insertMembers(ch, added))
	}
	return applySplices(s.body, sp)
}

// Restore is E-20, at the subscriber's gateway on a subscribe leg's answer
// relayed to its system: body is a Subscription, or a Bundle
// (a searchset and its pages) whose entries' Subscriptions are each
// restored, outcome entries carried as they are.
//
// b is the binding the answer is restored under: the subscriber (this
// gateway's participant), the notifier that answered (the leg's sender, as
// the Hub authenticated it) and the topic. A Subscription is restored when
// its channel.endpoint is an address whose tail is a notification address for
// b's subscriber and topic (ParseAddressTail; whatever comes before the tail
// is not read), and both sealed values open under k for b: the seals' bound
// data names the subscriber, the answering notifier and the topic, so a
// sealed value made for another notifier, another subscriber or another
// topic does not open. channel.endpoint then becomes the endpoint the
// subscriber wrote and channel.header the values it wrote (removed when it
// wrote none). Any other is left as it is, with a NotMade naming why (no such
// tail, another subscriber or topic, a retired key, a seal that does not
// open). In a Bundle, every entry's fullUrl and every link's url under
// statedBase (the notifier's, from the answer frame) is re-keyed to
// counterpartBase, the subscriber's per-counterpart base, keeping the path
// below the base and the query (a paging link at the stated base itself goes
// to the counterpart base's Subscription, where its page is fetched); with no
// statedBase, an http or https address is left and recorded once. Only http and https addresses are read as
// addresses into the notifier's base: a urn:uuid fullUrl names no server.
//
// An empty header set is restored as no channel.header member: FHIR JSON
// never writes an empty array, so a Subscription that wrote "header": [] and
// one that wrote no header name the same element, and E-18 seals both as the
// same empty set.
//
// made counts the Subscriptions restored and the addresses re-keyed; every
// other byte is as it was. A body that is not one unambiguous JSON value is
// err, as is a statedBase with no counterpartBase. RestoreEdits names the
// same changes by place, for a caller that makes them through its own
// verified edit.
func Restore(k Keys, b Binding, statedBase, counterpartBase string, body []byte) (out []byte, made int, notMade []NotMade, err error) {
	r, err := restore(k, b, statedBase, counterpartBase, Search{}, body)
	if err != nil {
		return nil, 0, nil, err
	}
	out, err = applySplices(body, r.splices)
	if err != nil {
		return nil, 0, nil, err
	}
	return out, r.made, r.notMade, nil
}

// RestoreEdit is one change E-20 makes to an answer's body (RestoreEdits),
// named by the byte span of the JSON value it changes in the body as given:
// that value replaced by Value, or, when Remove is set, the object member
// whose value it is removed (an empty header set, restored as no member).
type RestoreEdit struct {
	Start, End int
	Value      []byte
	Remove     bool
}

// RestoreEdits is Restore's changes, each named by place (RestoreEdit) rather
// than made: the same decisions, the same made count and the same not-made
// reasons, for a caller that makes and verifies the changes itself.
func RestoreEdits(k Keys, b Binding, statedBase, counterpartBase string, body []byte) (edits []RestoreEdit, made int, notMade []NotMade, err error) {
	r, err := restore(k, b, statedBase, counterpartBase, Search{}, body)
	if err != nil {
		return nil, 0, nil, err
	}
	return r.edits, r.made, r.notMade, nil
}

// RestoreSearch is Restore on the answer to a search this gateway framed (s):
// a link's url naming only notification addresses for b's subscriber and
// topic whose sealed values are s.Sealed is restored to s.Endpoint, whatever
// Subscriptions the answer holds, so the link repeats the search; a url
// Restore would restore from the answer's own Subscriptions still is.
func RestoreSearch(k Keys, b Binding, statedBase, counterpartBase string, s Search, body []byte) (out []byte, made int, notMade []NotMade, err error) {
	r, err := restore(k, b, statedBase, counterpartBase, s, body)
	if err != nil {
		return nil, 0, nil, err
	}
	out, err = applySplices(body, r.splices)
	if err != nil {
		return nil, 0, nil, err
	}
	return out, r.made, r.notMade, nil
}

// RestoreSearchEdits is RestoreSearch's changes, each named by place
// (RestoreEdit), as RestoreEdits is Restore's.
func RestoreSearchEdits(k Keys, b Binding, statedBase, counterpartBase string, s Search, body []byte) (edits []RestoreEdit, made int, notMade []NotMade, err error) {
	r, err := restore(k, b, statedBase, counterpartBase, s, body)
	if err != nil {
		return nil, 0, nil, err
	}
	return r.edits, r.made, r.notMade, nil
}

func restore(k Keys, b Binding, statedBase, counterpartBase string, s Search, body []byte) (*restorer, error) {
	if statedBase != "" && counterpartBase == "" {
		return nil, errors.New("subscription: a stated base needs the counterpart base to re-key to")
	}
	root, err := scanJSON(body)
	if err != nil {
		return nil, err
	}
	r := &restorer{k: k, b: b, stated: statedBase, counterpart: counterpartBase, search: s}
	switch rt, _ := root.memberFolded("resourceType").stringOf(); rt {
	case "Subscription":
		r.subscription(root)
	case "Bundle":
		// Member names are read folded, as a lenient reader would take
		// them: a Bundle whose entry is spelled "Entry" is restored, never
		// passed over with nothing recorded.
		entries, links := root.memberFolded("entry"), root.memberFolded("link")
		if (entries != nil && entries.kind != '[') || (links != nil && links.kind != '[') {
			r.not(NotMadeUnreadable)
		}
		if entries != nil {
			for _, e := range entries.elems {
				if e.kind != '{' {
					r.not(NotMadeUnreadable)
					continue
				}
				if res := e.memberFolded("resource"); res != nil {
					if t, _ := res.memberFolded("resourceType").stringOf(); t == "Subscription" {
						r.subscription(res)
					}
				}
				r.address(e.memberFolded("fullUrl"), false)
			}
		}
		if links != nil {
			for _, l := range links.elems {
				r.address(l.memberFolded("url"), true)
			}
		}
	}
	if r.unstated {
		r.notMade = append(r.notMade, NotMade{Edit: EditEndpointRestore, Reason: NotMadeNoStatedBase})
	}
	return r, nil
}

type restorer struct {
	k                   Keys
	b                   Binding
	stated, counterpart string
	splices             []splice
	edits               []RestoreEdit
	made                int
	notMade             []NotMade
	unstated            bool
	// opened maps each channel.endpoint a Subscription in this answer
	// held, a notification address, to the endpoint E-20 restored there.
	opened map[string]string
	// search is the search this gateway framed, if any (RestoreSearch).
	search Search
}

func (r *restorer) not(reason string) {
	r.notMade = append(r.notMade, NotMade{Edit: EditEndpointRestore, Reason: reason})
}

// replace replaces n with v, as a splice and as a RestoreEdit.
func (r *restorer) replace(n *jnode, v []byte) {
	r.splices = append(r.splices, splice{start: n.start, end: n.end, repl: v})
	r.edits = append(r.edits, RestoreEdit{Start: n.start, End: n.end, Value: v})
}

func (r *restorer) subscription(sub *jnode) {
	ch := sub.member("channel")
	epNode := ch.member("endpoint")
	ep, ok := epNode.stringOf()
	if !ok {
		r.not(NotMadeNoEndpoint)
		return
	}
	a, ok := ParseAddressTail(ep)
	if !ok {
		r.not(NotMadeForeignAddress)
		return
	}
	if a.Subscriber != r.b.Subscriber || a.Topic != r.b.Topic {
		r.not(NotMadeAnotherBinding)
		return
	}
	headers, _ := (&Subscription{root: sub}).headers()
	sealed, ok := headerValue(headers, ChannelHeader)
	if !ok {
		r.not(NotMadeNoSealedHeaders)
		return
	}
	endpoint, err := r.k.OpenEndpoint(r.b, a.SealedEndpoint)
	if err == nil {
		var written []string
		written, err = r.k.OpenHeaders(r.b, a.SealedEndpoint, sealed)
		if err == nil {
			r.replace(epNode, jsonString(endpoint))
			if r.opened == nil {
				r.opened = map[string]string{}
			}
			r.opened[ep] = endpoint
			h := ch.member("header")
			switch {
			case len(written) > 0:
				r.replace(h, jsonStrings(written))
			default:
				r.splices = append(r.splices, removeMember(ch, ch.memberIndex("header")))
				r.edits = append(r.edits, RestoreEdit{Start: h.start, End: h.end, Remove: true})
			}
			r.made++
			return
		}
	}
	if errors.Is(err, ErrKeyRetired) {
		r.not(NotMadeKeyRetired)
		return
	}
	r.not(NotMadeSealInvalid)
}

// address re-keys one absolute address under the stated base. A paging link
// (link) at the stated base itself, with a query, is a page of a search's
// answer, which the subscriber fetches at its per-counterpart base's
// Subscription (shnsdk's page form, "GET ?_getpages=…", is mapped back there by
// its gateway): it is re-keyed to "<counterpartBase>/Subscription?<query>".
func (r *restorer) address(n *jnode, link bool) {
	v, ok := n.stringOf()
	if !ok {
		return
	}
	if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		// Not an absolute http or https address, so neither re-keyed nor
		// restored (a relative link included): a url naming a notification
		// address is recorded, never passed over.
		r.notLinkURL(v, link)
		return
	}
	if r.stated == "" {
		r.unstated = true
		r.notLinkURL(v, link)
		return
	}
	if rekeyed, ok := underBase(v, r.stated, r.counterpart); ok {
		if link && strings.HasPrefix(rekeyed, strings.TrimRight(r.counterpart, "/")+"?") {
			rekeyed = strings.TrimRight(r.counterpart, "/") + "/Subscription" + strings.TrimPrefix(rekeyed, strings.TrimRight(r.counterpart, "/"))
		}
		if link {
			rekeyed = r.linkURLRestored(rekeyed)
		}
		r.replace(n, jsonString(rekeyed))
		r.made++
		return
	}
	// Not under the stated base, so neither re-keyed nor restored.
	r.notLinkURL(v, link)
}

// notLinkURL records NotMadeLinkURL for a paging link (link) left as written
// whose url names a notification address or cannot be read
// (linkNamesAddress): it is recorded, never passed over in silence.
func (r *restorer) notLinkURL(v string, link bool) {
	if link && linkNamesAddress(v) {
		r.not(NotMadeLinkURL)
	}
}

// linkNamesAddress reports whether any url parameter of link's query, with
// or without a modifier (url:below and the like), has a value that cannot be
// read or names a notification address (namesAddress). Every parameter is
// read, never only the first: a parameter's name is read unescaped, and one
// whose name cannot be read may be a url parameter, so its value is read too.
func linkNamesAddress(link string) bool {
	_, query, ok := strings.Cut(link, "?")
	if !ok {
		return false
	}
	for _, part := range strings.Split(query, "&") {
		rawName, value, _ := strings.Cut(part, "=")
		if name, err := url.QueryUnescape(rawName); err == nil && name != "url" && !strings.HasPrefix(name, "url:") {
			continue
		}
		raw, err := url.QueryUnescape(value)
		if err != nil || namesAddress(raw) {
			return true
		}
	}
	return false
}

// linkURLRestored is a paging link, already at the per-counterpart base, with
// its url search parameter restored as E-20 restores channel.endpoint: the
// notifier's server echoes the search it was given, a url naming the
// notification addresses E-21 wrote, which the subscriber's system never
// searched by. Each address in the value is restored from the search this
// gateway framed (searchEndpoint) or else from the Subscription this answer
// carries that E-20 restored there (opened); when every one is, all to one
// endpoint, the value becomes that endpoint, escaped as a FHIR search value
// and a query, and every other parameter keeps its bytes and place. The
// relay's check of E-20 holds the link to exactly that
// (subscriptionLinkURLRestored). Only a url parameter without a modifier is
// restored. When one does not restore so, the whole link is left as written.
// Whenever a url parameter the link keeps as written, any of them, with or
// without a modifier, names a notification address or cannot be read
// (linkNamesAddress), why is recorded (NotMadeLinkURL): a search by it is the
// subscriber's gateway's loop rule's to refuse, never an empty answer. A
// value restored is the subscriber's own endpoint, never read so.
func (r *restorer) linkURLRestored(link string) string {
	path, query, ok := strings.Cut(link, "?")
	if !ok {
		return link
	}
	parts := strings.Split(query, "&")
	kept := make([]string, 0, len(parts))
	for i, part := range parts {
		name, value, _ := strings.Cut(part, "=")
		if name != "url" {
			kept = append(kept, part)
			continue
		}
		raw, err := url.QueryUnescape(value)
		if err != nil {
			r.notLinkURL(link, true)
			return link
		}
		addrs, err := SearchURLValues(raw)
		if err != nil || len(addrs) == 0 {
			r.notLinkURL(link, true)
			return link
		}
		endpoint, restorable := "", true
		for _, a := range addrs {
			ep, ok := r.searchEndpoint(a)
			if !ok {
				ep, ok = r.opened[a]
			}
			if !ok || (endpoint != "" && ep != endpoint) {
				restorable = false
				continue
			}
			endpoint = ep
		}
		if !restorable {
			r.notLinkURL(link, true)
			return link
		}
		parts[i] = "url=" + url.QueryEscape(fhirSearchEscaper.Replace(endpoint))
	}
	r.notLinkURL("?"+strings.Join(kept, "&"), true)
	return path + "?" + strings.Join(parts, "&")
}

// namesAddress reports whether v, a link's url search value unescaped from
// the query, names a notification address in any part split at a comma, by
// the loop rule's reading (LooksLikeNotificationAddress, with any topic and
// participant): an address a search by it would be refused on, so a link left
// with it is recorded. It reads more than an exact tail (a trailing slash, an
// escaped key id), never less.
func namesAddress(v string) bool {
	anyName := func(s string) bool { return s != "" }
	for _, part := range strings.Split(v, ",") {
		if LooksLikeNotificationAddress(part, anyName, anyName) {
			return true
		}
	}
	return false
}

// searchEndpoint is the endpoint the framed search (r.search) restores a, a
// notification address in a link's url: a's tail names this binding's
// subscriber and topic, and its key id and sealed value are one of the values
// this gateway sealed the search to. What precedes the tail is not read, as
// for a Subscription's channel.endpoint.
func (r *restorer) searchEndpoint(a string) (string, bool) {
	if r.search.Endpoint == "" {
		return "", false
	}
	pa, ok := ParseAddressTail(a)
	if !ok || pa.Subscriber != r.b.Subscriber || pa.Topic != r.b.Topic || !slices.Contains(r.search.Sealed, pa.SealedEndpoint) {
		return "", false
	}
	return r.search.Endpoint, true
}

// underBase is v re-keyed from base to counterpart when v is under base by
// dtraddress.Under's structural match (scheme, host and port, then path
// segment by segment), with a query allowed after the path and kept as
// sent; ok is false for any other value, a fragment included, and one whose
// remainder holds a dot segment, raw or percent-encoded
// (dtraddress.DotFree), which a client would resolve above the
// per-counterpart base, into another counterpart's.
func underBase(v, base, counterpart string) (string, bool) {
	path, query, hasQuery := strings.Cut(v, "?")
	if strings.Contains(v, "#") {
		return "", false
	}
	rem, ok := dtraddress.Under(path, base)
	if !ok || !dtraddress.DotFree(rem) {
		return "", false
	}
	out := strings.TrimRight(counterpart, "/") + rem
	if hasQuery {
		out += "?" + query
	}
	return out, true
}

// fhirSearchEscaper escapes the characters FHIR search reads specially in a
// parameter value (backslash, comma, dollar sign, vertical bar), so one
// address is read as one value.
var fhirSearchEscaper = strings.NewReplacer(`\`, `\\`, `,`, `\,`, `$`, `\$`, `|`, `\|`)

// RekeySearchURL is E-21, at the notifier's gateway on a subscribe leg's
// search forwarded to its system: it returns q with url set to
// the notification address E-19 would write for each sealed endpoint the
// subscriber's gateway framed (Address{notifierIngress, b.Topic,
// b.Subscriber, sealed}), as one comma-separated value during a key overlap,
// which FHIR search reads as either, each escaped as a FHIR search value. So
// the search can match only the subscriber's own Subscriptions.
//
// No sealed endpoint, or one that is not a sealed value's form, is refused
// 403, recorded authority: the result never carries a query without url. q is
// not changed.
func RekeySearchURL(notifierIngress string, b Binding, sealedEndpoints []string, q url.Values) (url.Values, *Refusal) {
	if len(sealedEndpoints) == 0 {
		return nil, forbidden("a Subscription search names no sealed endpoint, so it cannot be scoped to the subscriber's own Subscriptions")
	}
	var addrs []string
	seen := map[string]bool{}
	for _, se := range sealedEndpoints {
		if _, raw, ok := splitSealed(se); !ok || len(raw) <= sivLen {
			return nil, forbidden("a Subscription search names a sealed endpoint that is not one")
		}
		if seen[se] {
			continue
		}
		seen[se] = true
		a := Address{Ingress: notifierIngress, Topic: b.Topic, Subscriber: b.Subscriber, SealedEndpoint: se}
		addrs = append(addrs, fhirSearchEscaper.Replace(a.String()))
	}
	out := make(url.Values, len(q)+1)
	for key, vs := range q {
		out[key] = append([]string(nil), vs...)
	}
	out["url"] = []string{strings.Join(addrs, ",")}
	return out, nil
}

// SearchURLValues reads a FHIR search's url parameter value, as a subscriber's
// system sends it, into the endpoints it names, each to be
// sealed by SealEndpointForSearch: the value is split at each comma that is
// not escaped, and each part unescaped (`\,`, `\$`, `\|` and `\\` are a comma,
// a dollar sign, a vertical bar and a backslash). A backslash before any
// other character or at the end, or an empty part, cannot be read one way
// only, and is an error: the caller refuses the search rather than guess. So
// is an endpoint that, unescaped, is not an absolute http or https URL with a
// host, or holds a backslash, a space or a control character. The SDK's
// ParseSubscriptionSearch reads a value the same way (test/sdkparity holds
// the two equal), and also refuses one that names more than one endpoint.
func SearchURLValues(v string) ([]string, error) {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\\':
			if i+1 == len(v) || !strings.ContainsRune(`,$|\`, rune(v[i+1])) {
				return nil, errors.New("subscription: a url search value holds a backslash that escapes nothing")
			}
			i++
			cur.WriteByte(v[i])
		case ',':
			if cur.Len() == 0 {
				return nil, errors.New("subscription: a url search value names an empty endpoint")
			}
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() == 0 {
		return nil, errors.New("subscription: a url search value names an empty endpoint")
	}
	out = append(out, cur.String())
	for _, e := range out {
		if !searchEndpoint(e) {
			return nil, errors.New("subscription: a url search value names an endpoint that is not an absolute http or https URL with no backslash, space or control character")
		}
	}
	return out, nil
}

// searchEndpoint reports whether e, one endpoint of a url search value
// unescaped, is an absolute http or https URL with a host, and holds no
// backslash, space or control character.
func searchEndpoint(e string) bool {
	// url.Parse refuses a control character.
	if strings.ContainsAny(e, " \\") {
		return false
	}
	u, err := url.Parse(e)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
