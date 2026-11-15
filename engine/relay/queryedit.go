package relay

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// queryVerifiers are the core's checks of each query edit, by its id: a
// closed map, as headerVerifiers is. A query edit is made only with its check
// here (ValidateEdits, QueryEdited); no row and no caller supplies one.
var queryVerifiers = map[EditID]func(from, to string, in QueryInputs) error{
	EditSubscriptionURLSearchRekey: verifySearchURLRekey,
}

// QueryInputs are the facts a query edit's check reads, by name: what the
// rewritten value must be made from, as HeaderInputs are a header edit's.
type QueryInputs map[string]string

// The inputs E-21's check reads: the notifier gateway's ingress, the topic's
// token and the subscriber the Hub authenticated, which name the notification
// addresses of this gateway for that subscriber.
const (
	QueryInputIngress    = "ingress"
	QueryInputTopic      = "topic"
	QueryInputSubscriber = "subscriber"
)

// validateQueryEdit reports why e cannot be an EditKindRewriteQuery row: its
// id is a query edit the core checks (queryVerifiers), made on a request a
// recipient forwards to its own system, naming no header or header inputs,
// and dropping no answer header.
func validateQueryEdit(e Edit) error {
	switch {
	case e.Kind != EditKindRewriteQuery || !slices.Contains(queryEdits, e.ID):
		return fmt.Errorf("edit %s is not a query edit", e.ID)
	case queryVerifiers[e.ID] == nil:
		return fmt.Errorf("query edit %s has no check in the core (queryVerifiers)", e.ID)
	case e.Role != RoleRecipient || e.Direction != DirectionRequest:
		return fmt.Errorf("query edit %s is not made on a request a recipient forwards to its own system", e.ID)
	case e.Header != "" || len(e.HeaderInputs) != 0:
		return fmt.Errorf("query edit %s names a header or header inputs", e.ID)
	case len(e.DropsAnswerHeaders) != 0:
		return fmt.Errorf("query edit %s drops %v: it changes no answer", e.ID, e.DropsAnswerHeaders)
	}
	return nil
}

// QueryEdited is src, a request relayed exactly, sent with its query rewritten
// from from to to by the registered query edit id, verified by the core's
// check of that edit (queryVerifiers): OwnershipEdited, with id as its one
// edit and the body unchanged; EditedQuery is to. When to equals from no edit
// is made, and src is returned. An id that is not a registered query edit, or
// src not relayed exactly, is ErrInvalidEdit (ErrUnknownEdit for an id that is
// not registered at all); a rewrite the check refuses is ErrVerifyMismatch.
func QueryEdited(src Payload, id EditID, from, to string, in QueryInputs) (Payload, error) {
	switch {
	case !slices.Contains(editIDs, id):
		return Payload{}, fmt.Errorf("%w: %q", ErrUnknownEdit, id)
	case !slices.Contains(queryEdits, id):
		return Payload{}, fmt.Errorf("%w: %s is not a query edit", ErrInvalidEdit, id)
	case src.Ownership() != OwnershipRelayed:
		return Payload{}, fmt.Errorf("%w: %s is made only on a peer's request relayed exactly, not %s", ErrInvalidEdit, id, src.Ownership())
	}
	if to == from {
		return src, nil
	}
	if err := queryVerifiers[id](from, to, in); err != nil {
		return Payload{}, fmt.Errorf("%w: %s: the registered verifier refused the query: %v", ErrVerifyMismatch, id, err)
	}
	q := to
	return Payload{c: &sealed{b: src.c.b, own: OwnershipEdited, edits: []EditID{id}, contentType: src.c.contentType, query: &q}}, nil
}

// verifySearchURLRekey is E-21's check: from, the search as the subscriber's
// gateway framed it, names no url parameter (a name that decodes to url in any
// case, with or without a modifier), and to is exactly one url parameter,
// written first, then from byte for byte ("&" between them when from is not
// empty). The url's value names one or two notification addresses of this
// gateway for the subscriber the Hub authenticated, and nothing else
// (notificationAddresses, from the inputs): a search can only ever find that
// subscriber's own Subscriptions. Neither query may hold a space, a "#", a
// control character or bytes that are not UTF-8, which a request's query
// cannot carry.
func verifySearchURLRekey(from, to string, in QueryInputs) error {
	for _, q := range []string{from, to} {
		if !utf8.ValidString(q) || strings.ContainsFunc(q, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '#' }) {
			return errors.New("a query holds a space, a #, a control character or bytes that are not UTF-8")
		}
	}
	if from != "" {
		for _, part := range strings.Split(from, "&") {
			name, _, _ := strings.Cut(part, "=")
			n, err := url.QueryUnescape(name)
			if base, _, _ := strings.Cut(n, ":"); err != nil || strings.EqualFold(base, "url") {
				return errors.New("the search as framed names url, or a name that cannot be read")
			}
		}
	}
	first, rest, _ := strings.Cut(to, "&")
	value, ok := strings.CutPrefix(first, "url=")
	switch {
	case !ok || value == "":
		return errors.New("the rewrite does not open with one url parameter with a value")
	case from == "" && to != first:
		return errors.New("the rewrite adds more than the url parameter")
	case from != "" && rest != from:
		return errors.New("the rewrite changes the search's other parameters")
	}
	return notificationAddresses(value, in)
}

// notificationAddresses reports why value, a url parameter's value as a
// query carries it, is not one or two notification addresses of the gateway
// whose ingress, topic and subscriber in names: the value, unescaped, split
// at each comma a FHIR search does not escape, each part unescaped (`\,`,
// `\$`, `\|`, `\\`), and each exactly
// "<ingress>/notify/<topic>/<subscriber>/<kid>/<sealed>", the ingress
// without an ending slash, the topic and subscriber path-escaped, as the
// address is written, the key id NotificationKeyIDLen base64url characters
// and the sealed value non-empty base64url. An input left out is an error: the check is never skipped.
func notificationAddresses(value string, in QueryInputs) error {
	ingress, topic, subscriber := strings.TrimRight(in[QueryInputIngress], "/"), in[QueryInputTopic], in[QueryInputSubscriber]
	if ingress == "" || topic == "" || subscriber == "" {
		return errors.New("the check's inputs name no ingress, topic or subscriber")
	}
	raw, err := url.QueryUnescape(value)
	if err != nil {
		return errors.New("the url's value cannot be read")
	}
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c == '\\' && i+1 < len(raw) && strings.IndexByte(`,$|\\`, raw[i+1]) >= 0:
			cur.WriteByte(raw[i+1])
			i++
		case c == '\\':
			return errors.New("the url's value holds a backslash that escapes nothing")
		case c == ',':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, cur.String())
	if len(parts) > 2 {
		return errors.New("the url's value names more than two addresses")
	}
	prefix := ingress + "/notify/" + url.PathEscape(topic) + "/" + url.PathEscape(subscriber) + "/"
	for _, part := range parts {
		rem, ok := strings.CutPrefix(part, prefix)
		kid, sealed, two := strings.Cut(rem, "/")
		if !ok || !two || len(kid) != NotificationKeyIDLen || !base64URL(kid) || !base64URL(sealed) {
			return errors.New("the url's value names an address that is not this gateway's notification address for the subscriber")
		}
	}
	return nil
}

// NotificationKeyIDLen is a notification address's key id's length, in
// base64url characters, as the subscription library writes it
// (subscription.KeyIDLen, which a test there holds equal).
const NotificationKeyIDLen = 11

// base64URLAlphabet is base64url's alphabet (RFC 4648 §5).
const base64URLAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// base64URL reports whether s is non-empty and only base64url's alphabet.
func base64URL(s string) bool { return s != "" && strings.Trim(s, base64URLAlphabet) == "" }
