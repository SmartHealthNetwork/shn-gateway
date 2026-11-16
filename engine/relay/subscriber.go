package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// The subscriber's side of a topic Subscription (the notifications design
// §6.2): E-18 seals the channel of the create or update the subscriber's
// gateway sends, E-20 restores it on the answers, and E-23 writes a create's
// or an update's Location at the subscriber's per-counterpart base.

// The inputs a Subscription's Location (E-23) is made from (HeaderInputs): the
// per-counterpart base it is written under, and the Subscription location the
// notifier's gateway framed (subscriptionLocation) that follows it.
const (
	SubscriptionLocationInputBase = "base"
	SubscriptionLocationInputTail = "tail"
)

// verifySubscriptionLocation is E-23's check: value is exactly the base input,
// "/", then the tail input, where the tail is a Subscription location,
// "Subscription/{id}" or "Subscription/{id}/_history/{vid}" with each id a
// FHIR id ([A-Za-z0-9\-\.]{1,64}) other than "." and "..", the form
// shnsdk.ParseSubscriptionLocation reads, read here by the core's own
// grammar; and the base is an absolute http or https URL with a host and no
// user information, query, fragment, trailing slash or dot segment, in ASCII
// (checkLocationBase, E-16's).
func verifySubscriptionLocation(value string, in HeaderInputs) error {
	base, tail := in[SubscriptionLocationInputBase], in[SubscriptionLocationInputTail]
	if err := checkLocationBase(base); err != nil {
		return err
	}
	if !subscriptionLocation(tail) {
		return fmt.Errorf("the tail %q is not a Subscription location", tail)
	}
	if value != base+"/"+tail {
		return errors.New("the value is not the base, then the Subscription location")
	}
	return nil
}

// subscriptionLocationForm is a Subscription location's form; dot segments
// are refused apart.
var subscriptionLocationForm = regexp.MustCompile(`^Subscription/[A-Za-z0-9\-.]{1,64}(/_history/[A-Za-z0-9\-.]{1,64})?$`)

// subscriptionLocation reports whether tail is a Subscription location.
func subscriptionLocation(tail string) bool {
	if !subscriptionLocationForm.MatchString(tail) {
		return false
	}
	for _, seg := range strings.Split(tail, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// rootChannel is the "channel" object of the document's root object when that
// root is a Subscription.
func (m *model) rootChannel() (splice.NodeID, bool) {
	d := m.doc
	ch, ok := d.Member(d.Root(), "channel")
	if !ok || d.Kind(ch) != splice.KindObject || stringMember(d, d.Root(), "resourceType") != "Subscription" {
		return 0, false
	}
	return ch, true
}

// subscriptionChannelSeal is E-18's exemption (D132, N2): the removal of the
// root Subscription's channel.endpoint or channel.header member, and nothing
// else, decided by structure.
func (m *model) subscriptionChannelSeal(eff effect) bool {
	ch, ok := m.rootChannel()
	return ok && eff.removed && eff.removedIn == ch && (eff.removedKey == "endpoint" || eff.removedKey == "header")
}

// subscriptionChannelRestore is E-20's exemption (D132, N2), decided by
// structure. In a Subscription that is the document's root, or the resource
// of an entry of a root Bundle: a replace of its channel.endpoint string with
// a JSON string, a replace of its channel.header array with an array of
// strings, or the removal of its channel.header member. In a root Bundle: a
// replace of an entry's fullUrl string, or of a link's url string, with a
// JSON string. A Bundle's own member names (entry, resource, fullUrl, link,
// url) are read folded, as the subscriber's gateway reads them; a
// Subscription's (channel, endpoint, header) exactly.
func (m *model) subscriptionChannelRestore(eff effect) bool {
	d := m.doc
	if eff.removed {
		ch := eff.removedIn
		return eff.removedKey == "header" && m.restorableChannel(ch)
	}
	if eff.replaced < 0 || eff.inserted {
		return false
	}
	n := eff.replaced
	v := m.replaced[n]
	parent := m.parent[n]
	if parent < 0 {
		return false
	}
	switch {
	case m.restorableChannel(parent) && memberIs(d, parent, n, "endpoint", false):
		return d.Kind(n) == splice.KindString && jsonString(v)
	case m.restorableChannel(parent) && memberIs(d, parent, n, "header", false):
		return d.Kind(n) == splice.KindArray && stringArray(v)
	case m.bundleEntry(parent) && memberIs(d, parent, n, "fullUrl", true):
		return d.Kind(n) == splice.KindString && jsonString(v)
	case m.bundleLink(parent) && memberIs(d, parent, n, "url", true):
		return d.Kind(n) == splice.KindString && jsonString(v) && m.subscriptionLinkURLRestored(n, v)
	}
	return false
}

// SubscriptionRestoreInputs are the facts E-20's check of a link's url reads
// (Change.Restore): the search by url the subscriber's gateway framed for its
// system, so a link restored from it is held to that search, whatever the
// answer holds. Subscriber and Topic name the binding the notification
// addresses must be for; Sealed are the values the gateway sealed the search's
// endpoint to, each "<kid>.<sealed>", one per key during an overlap; and
// Endpoint is the endpoint the subscriber's system searched by. An input left
// empty restores nothing: the check is never skipped.
type SubscriptionRestoreInputs struct {
	Subscriber, Topic string
	Sealed            []string
	Endpoint          string
}

// subscriptionLinkURLRestored is E-20's check of a link's query, v the
// replacement of the root Bundle link url n: the query keeps every parameter
// byte for byte, in its place, but a url parameter, which may change only as
// E-20 restores it, to one endpoint written as one FHIR search value, from
// notification addresses each of which is either the channel.endpoint a
// Subscription in this answer held and E-20 replaced in the same change set
// with that endpoint, or an address of the search the subscriber's gateway
// framed (framedSearchAddress), restored to that search's endpoint. So a
// link's url is restored to nothing but the endpoint the same edit opened
// from the payer's own seals, or the payer's own search, and never to
// another. The part before the query is the base re-key's, judged as before.
func (m *model) subscriptionLinkURLRestored(n splice.NodeID, v []byte) bool {
	old, err := m.doc.StringValue(n)
	var now string
	if err != nil || json.Unmarshal(bytes.TrimSpace(v), &now) != nil {
		return false
	}
	_, oq, oHas := strings.Cut(old, "?")
	_, nq, nHas := strings.Cut(now, "?")
	if oHas != nHas {
		return false
	}
	if !oHas {
		return true
	}
	op, np := strings.Split(oq, "&"), strings.Split(nq, "&")
	if len(op) != len(np) {
		return false
	}
	for i := range op {
		if op[i] == np[i] {
			continue
		}
		on, ov, _ := strings.Cut(op[i], "=")
		nn, nv, _ := strings.Cut(np[i], "=")
		if on != "url" || nn != "url" {
			return false
		}
		addrs, ok := fhirSearchValues(ov)
		ends, ok2 := fhirSearchValues(nv)
		if !ok || !ok2 || len(addrs) == 0 || len(ends) != 1 {
			return false
		}
		restored := m.restoredEndpoints()
		for _, a := range addrs {
			if ep, ok := restored[a]; ok && ep != "" && ep == ends[0] {
				continue
			}
			if m.framedSearchAddress(a) && ends[0] == m.restoreIn.Endpoint {
				continue
			}
			return false
		}
	}
	return true
}

// framedSearchAddress reports whether a, an address a link's url named, is a
// notification address of the search the subscriber's gateway framed
// (m.restoreIn), read by the core's own grammar: an absolute http or https
// URL with a host, no query or fragment (either could carry a tail of its own
// after a slash) and no space or backslash (which no search value the
// subscriber's gateway reads holds), whose last five path segments are "notify", the topic
// input and the subscriber input (each path-unescaped), then a key id and a
// sealed value that, joined by ".", are exactly one of the sealed inputs (a
// key id and a sealed value hold no "."). What precedes those segments is
// not read: the subscriber's gateway does not know the notifier gateway's
// ingress. With any input empty, no address is.
func (m *model) framedSearchAddress(a string) bool {
	in := m.restoreIn
	if in == nil || in.Subscriber == "" || in.Topic == "" || in.Endpoint == "" || len(in.Sealed) == 0 {
		return false
	}
	u, err := url.Parse(a)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || strings.ContainsAny(a, "# \\") {
		return false
	}
	segs := strings.Split(a, "/")
	n := len(segs)
	if n < 6 || segs[n-5] != "notify" {
		return false
	}
	topic, err1 := url.PathUnescape(segs[n-4])
	subscriber, err2 := url.PathUnescape(segs[n-3])
	return err1 == nil && err2 == nil && topic == in.Topic && subscriber == in.Subscriber &&
		slices.Contains(in.Sealed, segs[n-2]+"."+segs[n-1])
}

// restoredEndpoints maps each restorable Subscription channel.endpoint the
// change set replaces, by its original string, to the string it is replaced
// with; an original replaced with two different strings maps to none (""),
// which no restore matches. It is built once per model and kept, so the link
// check costs one pass over the replacements however many links there are.
func (m *model) restoredEndpoints() map[string]string {
	if m.restored != nil {
		return m.restored
	}
	d := m.doc
	out := map[string]string{}
	conflict := map[string]bool{}
	// In document order, so what the map holds never rests on the order a
	// map is walked in.
	for _, n := range slices.Sorted(maps.Keys(m.replaced)) {
		v := m.replaced[n]
		parent := m.parent[n]
		if parent < 0 || d.Kind(n) != splice.KindString || !m.restorableChannel(parent) || !memberIs(d, parent, n, "endpoint", false) {
			continue
		}
		orig, err := d.StringValue(n)
		var now string
		if err != nil || json.Unmarshal(bytes.TrimSpace(v), &now) != nil {
			continue
		}
		if prev, seen := out[orig]; (seen && prev != now) || conflict[orig] {
			conflict[orig], out[orig] = true, ""
			continue
		}
		out[orig] = now
	}
	m.restored = out
	return out
}

// fhirSearchValues reads a query parameter's value as a FHIR search reads
// it: unescaped from the query, split at each comma not escaped, each part's
// escapes (\, \$ \| \\) read. A backslash before any other character, or a
// value that does not unescape, is not read (ok false).
func fhirSearchValues(q string) ([]string, bool) {
	v, err := url.QueryUnescape(q)
	if err != nil {
		return nil, false
	}
	var out []string
	var cur strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '\\' && i+1 < len(v) && strings.IndexByte(`,$|\\`, v[i+1]) >= 0:
			cur.WriteByte(v[i+1])
			i++
		case c == '\\':
			return nil, false
		case c == ',':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String()), true
}

// restorableChannel reports whether ch is the "channel" object of a
// Subscription that is the root, or the resource of a root Bundle's entry.
func (m *model) restorableChannel(ch splice.NodeID) bool {
	d := m.doc
	sub := m.parent[ch]
	// The resourceType is read folded, as the subscriber's gateway reads it
	// (subscription.RestoreEdits), so the two never disagree on what is a
	// Subscription; channel, endpoint and header are read exactly by both.
	if sub < 0 || d.Kind(ch) != splice.KindObject || !memberIs(d, sub, ch, "channel", false) || foldedString(d, sub, "resourceType") != "Subscription" {
		return false
	}
	if sub == d.Root() {
		return true
	}
	entry := m.parent[sub]
	return entry >= 0 && m.bundleEntry(entry) && memberIs(d, entry, sub, "resource", true)
}

// bundleEntry reports whether e is an element of a root Bundle's entry array.
func (m *model) bundleEntry(e splice.NodeID) bool { return m.rootBundleElement(e, "entry") }

// bundleLink reports whether l is an element of a root Bundle's link array.
func (m *model) bundleLink(l splice.NodeID) bool { return m.rootBundleElement(l, "link") }

func (m *model) rootBundleElement(n splice.NodeID, list string) bool {
	d := m.doc
	arr := m.parent[n]
	if arr < 0 || d.Kind(n) != splice.KindObject || d.Kind(arr) != splice.KindArray {
		return false
	}
	return slices.Contains(m.rootBundleLists()[foldKey(list)], arr)
}

// rootBundleLists is, when the document's root is a Bundle (its resourceType
// read folded), the root's array members by their folded name; otherwise
// none. It reads the root once per model and keeps the result, so a check
// made once per edit costs nothing in the root's width.
func (m *model) rootBundleLists() map[string][]splice.NodeID {
	if m.rootLists != nil {
		return m.rootLists
	}
	d := m.doc
	m.rootLists = map[string][]splice.NodeID{}
	if d.Kind(d.Root()) != splice.KindObject || foldedString(d, d.Root(), "resourceType") != "Bundle" {
		return m.rootLists
	}
	for _, mem := range d.Members(d.Root()) {
		if d.Kind(mem.Value) == splice.KindArray {
			k := foldKey(mem.Name)
			m.rootLists[k] = append(m.rootLists[k], mem.Value)
		}
	}
	return m.rootLists
}

// memberIs reports whether n is obj's member named name: exactly, or, when
// folded, equal under the relay's case folding (foldKey, the strict decoder's).
func memberIs(d *splice.Doc, obj, n splice.NodeID, name string, folded bool) bool {
	for _, mem := range d.Members(obj) {
		if mem.Value != n {
			continue
		}
		if folded {
			return foldKey(mem.Name) == foldKey(name)
		}
		return mem.Name == name
	}
	return false
}

// foldedString is the string value of obj's member whose name folds to key's.
func foldedString(d *splice.Doc, obj splice.NodeID, key string) string {
	for _, mem := range d.Members(obj) {
		if foldKey(mem.Name) == foldKey(key) && d.Kind(mem.Value) == splice.KindString {
			s, _ := d.StringValue(mem.Value)
			return s
		}
	}
	return ""
}

// jsonString reports whether v is one JSON string.
func jsonString(v []byte) bool {
	t := bytes.TrimSpace(v)
	var s string
	return len(t) >= 2 && t[0] == '"' && json.Unmarshal(t, &s) == nil
}
