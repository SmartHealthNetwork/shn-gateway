package subscription

import (
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/pa/dtraddress"
)

// notifySegment is the notification address's first segment below the
// notifier gateway's ingress (§6.2 step 2).
const notifySegment = "notify"

// ChannelHeader is the channel.header name E-19 writes the sealed headers
// under, which the notifier's system echoes with every notification (§6.2
// step 2, step 5).
const ChannelHeader = "X-SHN-Channel"

// AuthorizationHeader is the channel.header name E-19 writes the notifier
// gateway's credential under (§6.3).
const AuthorizationHeader = "Authorization"

// Address is a notification address (§6.2 step 2): the notifier gateway's
// ingress, the topic's token, the subscriber's participant id, and the sealed
// endpoint SealEndpoint made, whose key id is the part before its ".".
type Address struct{ Ingress, Topic, Subscriber, SealedEndpoint string }

// String writes the address as E-19 writes it into channel.endpoint (§6.2
// step 2):
// "<ingress>/notify/<topic>/<subscriber>/<kid>/<sealed>", the ingress without
// an ending slash, the topic and subscriber path-escaped, and the sealed
// endpoint split at its "." into the key id and the sealed bytes.
func (a Address) String() string {
	kid, sealed, _ := strings.Cut(a.SealedEndpoint, ".")
	return strings.TrimRight(a.Ingress, "/") + "/" + notifySegment + "/" + url.PathEscape(a.Topic) + "/" +
		url.PathEscape(a.Subscriber) + "/" + kid + "/" + sealed
}

// ParseAddress reads raw as a notification address of the gateway whose
// ingress is ingress, by structure (§6.2 step 3, §7.2): raw is an absolute URL
// with no userinfo, query or fragment, under ingress by dtraddress.Under's
// match (scheme, host and port, then path segment by segment), and its path
// below ingress is exactly "/notify/<topic>/<subscriber>/<kid>/<sealed>",
// with a topic and subscriber that unescape to non-empty values, a key id's
// form and a non-empty base64url segment. Any other value is not one (ok is
// false); whether its seal opens is the caller's to ask.
func ParseAddress(raw, ingress string) (Address, bool) {
	rem, ok := dtraddress.Under(raw, strings.TrimRight(ingress, "/"))
	if !ok {
		return Address{}, false
	}
	segs := strings.Split(rem, "/")
	if len(segs) != 6 || segs[0] != "" || segs[1] != notifySegment {
		return Address{}, false
	}
	topic, err := url.PathUnescape(segs[2])
	if err != nil || topic == "" {
		return Address{}, false
	}
	subscriber, err := url.PathUnescape(segs[3])
	if err != nil || subscriber == "" {
		return Address{}, false
	}
	kid, sealed := segs[4], segs[5]
	if !validKeyID(kid) || !validB64URL(sealed) {
		return Address{}, false
	}
	return Address{Ingress: ingress, Topic: topic, Subscriber: subscriber, SealedEndpoint: kid + "." + sealed}, true
}

// ParseAddressTail reads raw as an address whose tail is a notification
// address, by structure alone (§6.2 step 3, at the subscriber's gateway): raw
// is a URL with no query or fragment whose path ends in exactly the five
// segments "notify", a topic and a subscriber that each unescape to a
// non-empty value, a key id's form and a non-empty base64url segment.
// Whatever comes before those segments, the address's Ingress, is not judged:
// the subscriber's gateway does not know the notifier gateway's ingress, and
// what a restore rests on is that the sealed values open under the binding
// that names the notifier that answered (Restore). Any other value is not one
// (ok is false).
func ParseAddressTail(raw string) (Address, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return Address{}, false
	}
	segs := strings.Split(raw, "/")
	n := len(segs)
	if n < 6 || segs[n-5] != notifySegment {
		return Address{}, false
	}
	topic, err := url.PathUnescape(segs[n-4])
	if err != nil || topic == "" {
		return Address{}, false
	}
	subscriber, err := url.PathUnescape(segs[n-3])
	if err != nil || subscriber == "" {
		return Address{}, false
	}
	kid, sealed := segs[n-2], segs[n-1]
	if !validKeyID(kid) || !validB64URL(sealed) {
		return Address{}, false
	}
	return Address{Ingress: strings.Join(segs[:n-5], "/"), Topic: topic, Subscriber: subscriber, SealedEndpoint: kid + "." + sealed}, true
}

// LooksLikeNotificationAddress is §6.5's loop rule: raw is a notification
// address of some gateway, whatever its host, when its path holds, one after
// another, the segments "notify", a topic token (isTopic), a registered
// participant id (isParticipant) and a key id's form. The path is read both
// segment by segment, each unescaped, and decoded whole, so an escaped slash
// ("notify%2Fpas%2F…") cannot hide the segments from a server that decodes
// it first. A value that does not parse as a URL is not one; CheckEndpoint
// refuses it on its own rule.
func LooksLikeNotificationAddress(raw string, isParticipant func(string) bool, isTopic func(string) bool) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	escaped := strings.Split(u.EscapedPath(), "/")
	segs := make([]string, len(escaped))
	for i, s := range escaped {
		v, err := url.PathUnescape(s)
		if err != nil {
			v = s
		}
		segs[i] = v
	}
	return holdsAddress(segs, isParticipant, isTopic) || holdsAddress(strings.Split(u.Path, "/"), isParticipant, isTopic)
}

func holdsAddress(segs []string, isParticipant func(string) bool, isTopic func(string) bool) bool {
	for i := 0; i+3 < len(segs); i++ {
		if segs[i] == notifySegment && isTopic(segs[i+1]) && isParticipant(segs[i+2]) && validKeyID(segs[i+3]) {
			return true
		}
	}
	return false
}
