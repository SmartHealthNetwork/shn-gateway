package subscription

import (
	"encoding/json"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// The Subscriptions R5 Backport IG's canonicals (§7.1 rules 1, 2 and 4).
// BackportSubscriptionProfile is the profile a topic subscription's
// meta.profile names (§7.1 rule 1).
const (
	BackportSubscriptionProfile = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription"
	extFilterCriteria           = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria"
	extPayloadContent           = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content"
	extHeartbeatPeriod          = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-heartbeat-period"
	extTimeout                  = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-timeout"
	extMaxCount                 = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-max-count"
)

// backportExtensionAt is where each allowlisted Backport extension may sit in
// a Subscription (§7.1 rule 4), as the member path of the extension array
// holding it: the filter criteria on criteria, the payload content on
// channel.payload, the others on channel.
var backportExtensionAt = map[string]string{
	extFilterCriteria:  "_criteria.extension",
	extPayloadContent:  "channel._payload.extension",
	extHeartbeatPeriod: "channel.extension",
	extTimeout:         "channel.extension",
	extMaxCount:        "channel.extension",
}

// backportValueType is the one value[x] each allowlisted Backport extension
// carries (§7.1 rule 4), and the JSON kind of that value: a reader that keeps
// the first value[x] it meets could read another than the one checked here.
var backportValueType = map[string]struct {
	name string
	kind byte
}{
	extFilterCriteria:  {"valueString", '"'},
	extPayloadContent:  {"valueCode", '"'},
	extHeartbeatPeriod: {"valueUnsignedInt", '0'},
	extTimeout:         {"valueUnsignedInt", '0'},
	extMaxCount:        {"valuePositiveInt", '0'},
}

// backportValue is an allowlisted Backport extension's value (§7.1 rule 4):
// the extension holds its url, optionally an id, and exactly its one value[x]
// of its type, and nothing else (no other value[x], no _value, no nested
// extension or modifierExtension). Anything else is refused 403, authority.
func backportValue(e *jnode, url string) (*jnode, *Refusal) {
	want := backportValueType[url]
	if e.kind != '{' {
		return nil, forbidden(refusalShapeFirst + "an extension is not an object")
	}
	for _, m := range e.members {
		if m.name != "url" && m.name != "id" && m.name != want.name {
			return nil, forbidden(refusalShapeFirst + "the extension " + url + " carries " + m.name + " beside its " + want.name)
		}
	}
	v := e.member(want.name)
	if v == nil || v.kind != want.kind {
		return nil, forbidden(refusalShapeFirst + "the extension " + url + " states no " + want.name)
	}
	return v, nil
}

// The channel this network carries (§7.1 rule 2; PAS spec-56).
const (
	channelRestHook   = "rest-hook"
	channelPayload    = "application/fhir+json"
	contentEmpty      = "empty"
	maxFHIRIDLen      = 64
	refusalShapeFirst = "the Subscription is outside the shape this network carries: "
)

// Refusal is a refusal the caller answers and records (§9): the HTTP status, the
// network primitive it is refused on (one of gateway/recording's refusal
// rules: routing, authority, authentication, integrity or fidelity), and the
// reason.
type Refusal struct {
	Status    int
	Primitive string
	Reason    string
}

// Unreadable reports whether r refuses a body this network cannot read to
// make its own edit on: the fidelity of SHN's own edit (ReadSubscription).
func (r *Refusal) Unreadable() bool { return r != nil && r.Primitive == recording.RefusalFidelity }

func refuse(status int, primitive, reason string) *Refusal {
	return &Refusal{Status: status, Primitive: primitive, Reason: reason}
}

func forbidden(reason string) *Refusal {
	return refuse(http.StatusForbidden, recording.RefusalAuthority, reason)
}

// Topic is one notification topic the network carries (§5, §7.1): PAS's and
// the HRex Task topic, each implemented in its own lane's file.
type Topic interface {
	// Token is the topic's notification-address segment, also Binding.Topic:
	// "pas" or "cdex-task".
	Token() string
	// Canonical is the topic's criteria canonical, with no |version.
	Canonical() string
	// Contents are the payload contents the network carries for the topic:
	// PAS {"full-resource", "id-only"}, CDex {"id-only"}.
	Contents() []string
	// Extensions are the extension URLs the topic's Subscription profile
	// defines, allowed beside the Backport IG's (§7.1 rule 4).
	Extensions() []string
	// CheckFilter is §7.1 rule 3, whole: the filter holds only the topic's
	// parameters, each naming values the subscriber may name.
	CheckFilter(subscriber Holder, params []FilterParam) *Refusal
	// SubscribeLeg is the subscribe leg's flat transaction type.
	SubscribeLeg() string
	// ContentLeg is the notify leg's flat transaction type, "" when the
	// topic has no content row (CDex).
	ContentLeg() string
	// SignalLeg is the signal leg's flat transaction type.
	SignalLeg() string
	// CheckContent is the topic's authority check on a content notification
	// (PAS: §7.3's N6). It is never called for a no-content one.
	CheckContent(subscriber Holder, n *Notification) *Refusal
	// NotificationShape is the reference types a notification-event's focus
	// and additional-context may name, as the topic's pinned definition
	// declares them (its notificationShape, or its resourceTrigger's
	// resource for focus when it declares none): what an instance $status
	// answer is held to (CheckSubscribeAnswer).
	NotificationShape() NotificationShape
}

// NotificationShape is a topic's notification shape as the network holds an
// answer to it: Topic the topic's canonical, which a SubscriptionStatus's
// topic parameter names (with or without a |version); Focus the resource
// types a notification-event's focus may reference; AdditionalContext those
// its additional-context may (none: any is refused).
type NotificationShape struct {
	Topic                    string
	Focus, AdditionalContext []string
}

// Identifier is a (system, value) pair as the registry attests it (§7.1,
// N4).
type Identifier struct{ System, Value string }

// Holder is the authenticated subscriber as the registry attests it (§7.1): its
// participant id, and a provider's organization identifiers or a payer's
// payer identifiers.
type Holder struct {
	ID          string
	ProviderIDs []Identifier
	PayerIDs    []Identifier
}

// Subscription is a parsed Subscription body (§6.2, §7.1), kept as its bytes and their
// structure so an edit rewrites only the members it edits.
type Subscription struct {
	body []byte
	root *jnode
}

// ParseSubscription reads a Subscription body (§6.2 "An unreadable
// Subscription"): a media type other than FHIR JSON (application/fhir+json or
// application/json, any parameters), or none, is refused 415, and a body that
// is not one unambiguous JSON object whose resourceType is "Subscription" is
// refused 400; each is recorded as the fidelity of SHN's own edit, which
// cannot be made on a body it cannot read.
func ParseSubscription(body []byte, contentType string) (*Subscription, *Refusal) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil || (mt != "application/fhir+json" && mt != "application/json") {
		return nil, refuse(http.StatusUnsupportedMediaType, recording.RefusalFidelity, "a Subscription this network re-keys must be FHIR JSON (application/fhir+json)")
	}
	root, err := scanJSON(body)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, recording.RefusalFidelity, "the Subscription is not one JSON object this network can read unambiguously")
	}
	if rt, _ := root.member("resourceType").stringOf(); root.kind != '{' || rt != "Subscription" {
		return nil, refuse(http.StatusBadRequest, recording.RefusalFidelity, "the body is not a Subscription")
	}
	return &Subscription{body: body, root: root}, nil
}

// ReadSubscription reads a Subscription a subscribe leg's create or update
// carries, at the notifier's gateway, by the body's structure alone
// (ClassifyBody), never the media type it is labelled with; a network rule at
// every level:
//   - FHIR JSON or FHIR XML whose resourceType is not Subscription is refused
//     403, recorded authority: the leg carries no other resource (a JSON body
//     that is no resource itself but holds one included);
//   - a FHIR XML Subscription, and FHIR XML that cannot be read one way
//     only, are refused 415, and any other body that is not one JSON value
//     (plain text, HTML, XML in no or another namespace) 415, each recorded as
//     the fidelity of SHN's own edit, which cannot be made on a body it cannot
//     read (§6.2 "An unreadable Subscription");
//   - no body, a JSON value holding no resource, a JSON object or array
//     followed by anything but whitespace, and FHIR JSON that cannot
//     be read one way only (a repeated member name, a byte order mark, a
//     resourceType spelled in another case, a body in UTF-16 or UTF-32) are
//     refused 400, recorded the same way.
func ReadSubscription(body []byte) (*Subscription, *Refusal) {
	unreadable := func(status int, reason string) (*Subscription, *Refusal) {
		return nil, refuse(status, recording.RefusalFidelity, reason)
	}
	// A JSON object or array followed by anything is never one JSON
	// Subscription, whatever ClassifyBody makes of its first value.
	if jsonValueThenMore(body) {
		return unreadable(http.StatusBadRequest, "the Subscription is not one JSON object this network can read unambiguously")
	}
	kind, rt, err := ClassifyBody(body)
	switch {
	case (kind == BodyFHIRJSON || kind == BodyFHIRXML) && err == nil && rt != "Subscription":
		return nil, forbidden("the body is a resource other than a Subscription, which this leg never carries")
	case kind == BodyFHIRXML:
		return unreadable(http.StatusUnsupportedMediaType, "a Subscription this network re-keys must be FHIR JSON (application/fhir+json)")
	case kind == BodyEmpty:
		return unreadable(http.StatusBadRequest, "the request carries no Subscription")
	case kind == BodyNotFHIR && !json.Valid(body):
		return unreadable(http.StatusUnsupportedMediaType, "a Subscription this network re-keys must be FHIR JSON (application/fhir+json)")
	case kind == BodyNotFHIR:
		return unreadable(http.StatusBadRequest, "the body is not a Subscription")
	}
	root, err := scanJSON(body)
	if err != nil || root.kind != '{' {
		return unreadable(http.StatusBadRequest, "the Subscription is not one JSON object this network can read unambiguously")
	}
	if rt, _ := root.member("resourceType").stringOf(); rt != "Subscription" {
		return unreadable(http.StatusBadRequest, "the Subscription is not one JSON object this network can read unambiguously")
	}
	return &Subscription{body: body, root: root}, nil
}

// Endpoint is the Subscription's channel.endpoint, and whether it states
// one as a string: what CheckEndpoint judges (§6.5).
func (s *Subscription) Endpoint() (string, bool) {
	return s.channel().member("endpoint").stringOf()
}

func (s *Subscription) channel() *jnode { return s.root.member("channel") }

// headers is channel.header's values, and whether every one is a string.
// Absent is an empty set.
func (s *Subscription) headers() ([]string, bool) {
	h := s.channel().member("header")
	if h == nil {
		return []string{}, true
	}
	if h.kind != '[' {
		return nil, false
	}
	out := make([]string, 0, len(h.elems))
	for _, e := range h.elems {
		v, ok := e.stringOf()
		if !ok {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// headerValue is the value of the one channel.header entry named name (in
// any case), and whether exactly one names it.
func headerValue(headers []string, name string) (string, bool) {
	var found []string
	for _, h := range headers {
		n, v, ok := strings.Cut(h, ":")
		if ok && strings.EqualFold(n, name) {
			found = append(found, strings.Trim(v, " \t"))
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// canonicalIs reports whether v is canonical, with or without a |version
// suffix whose version is non-empty and holds no "|", "?" or "#" (§7.1 rule
// 1; a FHIR canonical's version).
func canonicalIs(v, canonical string) bool {
	if v == canonical {
		return true
	}
	ver, ok := strings.CutPrefix(v, canonical+"|")
	return ok && ver != "" && !strings.ContainsAny(ver, "|?# \t\r\n")
}

// CheckShape is §7.1's closed shape, a network rule at every level, at the
// notifier's gateway on a Subscription create or update. It refuses 403,
// recorded authority:
//   - rule 1: a Subscription whose meta.profile does not name the Backport
//     Subscription profile, or whose criteria is not t's canonical, with or
//     without a |version (a query after it included);
//   - rule 2: a channel other than rest-hook, a payload other than
//     application/fhir+json, or a payload content (the Backport
//     payload-content extension on channel.payload, required) outside t's
//     contents, empty always among them;
//   - rule 4: any modifierExtension, anywhere, and any extension other than
//     the Backport IG's filter-criteria, payload-content, heartbeat-period,
//     timeout and max-count, each where the IG puts it and holding exactly its
//     one value[x] (backportValue), and those the topic's profile defines
//     (t.Extensions), anywhere. Member names are compared under case folding,
//     as a lenient reader would take them;
//
// then rule 3, the filter (ParseFilters, then t.CheckFilter).
func CheckShape(s *Subscription, t Topic, subscriber Holder) *Refusal {
	if r := checkTopicSubscription(s, t); r != nil {
		return r
	}
	if r := checkChannel(s, t); r != nil {
		return r
	}
	if r := checkExtensions(s.root, nil, t.Extensions()); r != nil {
		return r
	}
	params, r := ParseFilters(s)
	if r != nil {
		return r
	}
	return t.CheckFilter(subscriber, params)
}

// checkTopicSubscription is §7.1 rule 1.
func checkTopicSubscription(s *Subscription, t Topic) *Refusal {
	profiled := false
	if p := s.root.member("meta").member("profile"); p != nil && p.kind == '[' {
		for _, e := range p.elems {
			if v, ok := e.stringOf(); ok && canonicalIs(v, BackportSubscriptionProfile) {
				profiled = true
			}
		}
	}
	if !profiled {
		return forbidden(refusalShapeFirst + "meta.profile does not name the Backport Subscription profile, so it is not a topic subscription")
	}
	criteria, ok := s.root.member("criteria").stringOf()
	if !ok || !canonicalIs(criteria, t.Canonical()) {
		return forbidden(refusalShapeFirst + "criteria is not the topic's canonical " + t.Canonical())
	}
	return nil
}

// checkChannel is §7.1 rule 2.
func checkChannel(s *Subscription, t Topic) *Refusal {
	ch := s.channel()
	if typ, _ := ch.member("type").stringOf(); typ != channelRestHook {
		return forbidden(refusalShapeFirst + "channel.type is not rest-hook")
	}
	if payload, _ := ch.member("payload").stringOf(); payload != channelPayload {
		return forbidden(refusalShapeFirst + "channel.payload is not application/fhir+json")
	}
	var contents []string
	p := ch.memberFolded("_payload")
	if p != nil && p.kind != '{' {
		return forbidden(refusalShapeFirst + "channel._payload is not an object")
	}
	ext := p.memberFolded("extension")
	if ext != nil && ext.kind != '[' {
		return forbidden(refusalShapeFirst + "channel._payload.extension is not an array")
	}
	if ext != nil {
		for _, e := range ext.elems {
			if u, _ := e.member("url").stringOf(); u == extPayloadContent {
				v, r := backportValue(e, u)
				if r != nil {
					return r
				}
				contents = append(contents, v.str)
			}
		}
	}
	switch {
	case len(contents) != 1:
		return forbidden(refusalShapeFirst + "channel.payload states no one payload content")
	case contents[0] == contentEmpty:
		return forbidden(refusalShapeFirst + "the payload content empty is not carried")
	case !slices.Contains(t.Contents(), contents[0]):
		return forbidden(refusalShapeFirst + "the payload content " + contents[0] + " is not one this topic carries (" + strings.Join(t.Contents(), ", ") + ")")
	}
	return nil
}

// Folded spellings of the member names rule 4 reads, and of the paths where
// each Backport extension may sit: a lenient reader takes another case, or a
// Unicode fold, of a name for the name itself.
var (
	foldedExtension         = jsonmember.FoldedName("extension")
	foldedModifierExtension = jsonmember.FoldedName("modifierExtension")
)

func foldedPath(p string) string {
	parts := strings.Split(p, ".")
	for i, s := range parts {
		parts[i] = jsonmember.FoldedName(s)
	}
	return strings.Join(parts, ".")
}

// checkExtensions is §7.1 rule 4 over n and everything below it. path is the
// folded member names from the Subscription's root to n.
func checkExtensions(n *jnode, path []string, topic []string) *Refusal {
	switch n.kind {
	case '[':
		for _, e := range n.elems {
			if r := checkExtensions(e, path, topic); r != nil {
				return r
			}
		}
	case '{':
		for _, m := range n.members {
			folded := jsonmember.FoldedName(m.name)
			at := append(slices.Clone(path), folded)
			switch folded {
			case foldedModifierExtension:
				return forbidden(refusalShapeFirst + "it carries a modifierExtension")
			case foldedExtension:
				if r := checkExtensionArray(m.val, strings.Join(at, "."), topic); r != nil {
					return r
				}
				continue
			}
			if r := checkExtensions(m.val, at, topic); r != nil {
				return r
			}
		}
	}
	return nil
}

// checkExtensionArray checks each extension of an extension array at path (a
// folded path). An allowlisted Backport extension holds exactly its one
// value (backportValue). A topic extension's own content (its value, and any
// sub-extension its definition gives it) is its definition's; only a
// modifierExtension inside it is refused.
func checkExtensionArray(arr *jnode, path string, topic []string) *Refusal {
	if arr.kind != '[' {
		return forbidden(refusalShapeFirst + "an extension member is not an array")
	}
	for _, e := range arr.elems {
		u, _ := e.member("url").stringOf()
		where, backport := backportExtensionAt[u]
		switch {
		case backport && foldedPath(where) == path:
			if _, r := backportValue(e, u); r != nil {
				return r
			}
		case slices.Contains(topic, u):
		default:
			return forbidden(refusalShapeFirst + "it carries an extension this network does not carry there: " + u)
		}
		if containsMember(e, foldedModifierExtension) {
			return forbidden(refusalShapeFirst + "it carries a modifierExtension")
		}
	}
	return nil
}

// containsMember reports whether n, or anything below it, has a member whose
// folded name is folded.
func containsMember(n *jnode, folded string) bool {
	for _, m := range n.members {
		if jsonmember.FoldedName(m.name) == folded || containsMember(m.val, folded) {
			return true
		}
	}
	for _, e := range n.elems {
		if containsMember(e, folded) {
			return true
		}
	}
	return false
}

// FilterValue is one comma-separated value of a filter parameter (§7.1 rule
// 3), read as a
// token: "system|value" has HasSystem, and "|value" is HasSystem with an
// empty System.
type FilterValue struct {
	System    string
	HasSystem bool
	Value     string
}

// FilterParam is one filter parameter (§7.1 rule 3): its name, its modifier ("" when none)
// and its values.
type FilterParam struct {
	Name, Modifier string
	Values         []FilterValue
}

var (
	filterResourceType = regexp.MustCompile(`^[A-Z][A-Za-z]*$`)
	filterName         = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-]*$`)
	filterModifier     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9\-]*$`)
)

// ParseFilters reads a Subscription's filter (§7.1 rule 3) exactly as the
// notifier matches it: each Backport filter-criteria extension on criteria
// (_criteria), one or more, whose valueString is an optional
// "<ResourceType>?" then parameters joined by "&", each
// "<name>[:<modifier>]=<value>[,<value>…]", the values comma-separated, a
// token's "system|value" read for both. No _criteria, or a _criteria with no
// extension list, is no filter, an empty list for the topic to judge. A
// filter that cannot be read one way only is refused 403, recorded
// authority: a _criteria that is not an object, an extension list that is
// not an array or holds something other than an object, a filter-criteria
// extension without exactly its one valueString (a second value[x] among
// them: a reader that keeps the first it meets could read it instead), an
// empty parameter, name, modifier or value, a missing "=", more than one
// "?", a value naming a system and no value, and a percent sign, plus sign,
// backslash, space or control character anywhere (an escape the notifier
// might read differently: a URL decoder reads "+" as a space).
func ParseFilters(s *Subscription) ([]FilterParam, *Refusal) {
	out := []FilterParam{}
	crit := s.root.memberFolded("_criteria")
	if crit == nil {
		return out, nil
	}
	if crit.kind != '{' {
		return nil, forbidden(refusalShapeFirst + "_criteria is not an object, so its filter cannot be read")
	}
	ext := crit.memberFolded("extension")
	if ext == nil {
		return out, nil
	}
	if ext.kind != '[' {
		return nil, forbidden(refusalShapeFirst + "_criteria.extension is not an array, so its filter cannot be read")
	}
	for _, e := range ext.elems {
		if e.kind != '{' {
			return nil, forbidden(refusalShapeFirst + "a criteria extension is not an object, so the filter cannot be read")
		}
		if u, _ := e.member("url").stringOf(); u != extFilterCriteria {
			continue
		}
		v, r := backportValue(e, extFilterCriteria)
		if r != nil {
			return nil, r
		}
		params, r := parseFilterCriteria(v.str)
		if r != nil {
			return nil, r
		}
		out = append(out, params...)
	}
	return out, nil
}

func malformedFilter(why string) *Refusal {
	return forbidden(refusalShapeFirst + "its filter cannot be read one way only: " + why)
}

func parseFilterCriteria(v string) ([]FilterParam, *Refusal) {
	if strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '%' || r == '\\' || r == '+' }) {
		return nil, malformedFilter("it holds a percent sign, plus sign, backslash, space or control character")
	}
	query := v
	if rt, q, ok := strings.Cut(v, "?"); ok {
		if !filterResourceType.MatchString(rt) || strings.Contains(q, "?") {
			return nil, malformedFilter("only one resource type and \"?\" may open it")
		}
		query = q
	}
	if query == "" {
		return nil, malformedFilter("it names no parameter")
	}
	var out []FilterParam
	for _, part := range strings.Split(query, "&") {
		key, values, ok := strings.Cut(part, "=")
		if !ok {
			return nil, malformedFilter("a parameter has no \"=\"")
		}
		name, modifier, hasModifier := strings.Cut(key, ":")
		if !filterName.MatchString(name) || (hasModifier && !filterModifier.MatchString(modifier)) {
			return nil, malformedFilter("a parameter's name or modifier is not one")
		}
		p := FilterParam{Name: name, Modifier: modifier}
		for _, raw := range strings.Split(values, ",") {
			fv := FilterValue{Value: raw}
			if sys, val, has := strings.Cut(raw, "|"); has {
				fv = FilterValue{System: sys, HasSystem: true, Value: val}
			}
			if fv.Value == "" || strings.Contains(fv.Value, "|") {
				return nil, malformedFilter("a value is empty, or names a system and no value")
			}
			p.Values = append(p.Values, fv)
		}
		out = append(out, p)
	}
	return out, nil
}

// MatchIdentifiers is §7.1 rule 3's tie between a subscriber and the
// identifiers its filter may name (N4): a bare value matches on value against
// any attested identifier; an explicit system, the empty one included, must
// equal an attested (system, value) pair. A value that matches none, or no
// value at all, is refused 403, recorded authority.
func MatchIdentifiers(values []FilterValue, attested []Identifier) *Refusal {
	if len(values) == 0 {
		return forbidden(refusalShapeFirst + "its filter names no identifier")
	}
	for _, v := range values {
		match := slices.ContainsFunc(attested, func(a Identifier) bool {
			return a.Value == v.Value && (!v.HasSystem || a.System == v.System)
		})
		if !match {
			named := v.Value
			if v.HasSystem {
				named = v.System + "|" + v.Value
			}
			return forbidden("the Subscription's filter names an identifier not attested for the subscriber: " + named)
		}
	}
	return nil
}

// CheckEndpoint is §6.5's rule on a Subscription's channel.endpoint, at the
// subscriber's gateway on a create or update, a network rule at every level.
// It refuses 422: an endpoint that is not an absolute https URL with a host,
// that holds a space or a backslash (which url.Parse would take, but a url
// search for the endpoint refuses, SearchURLValues), or that names userinfo
// or a fragment, recorded integrity (the last hop
// carries PHI); and one that is a notification address (loop, by
// LooksLikeNotificationAddress), recorded routing. A nil loop refuses every
// endpoint: the rule is never skipped.
func CheckEndpoint(endpoint string, loop func(string) bool) *Refusal {
	u, err := url.Parse(endpoint)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.Scheme != "https" || !searchEndpoint(endpoint) {
		return refuse(http.StatusUnprocessableEntity, recording.RefusalIntegrity, "channel.endpoint must be an absolute https URL with no space or backslash")
	}
	if u.User != nil {
		return refuse(http.StatusUnprocessableEntity, recording.RefusalIntegrity, "channel.endpoint must not name userinfo")
	}
	if u.Fragment != "" || strings.Contains(endpoint, "#") {
		return refuse(http.StatusUnprocessableEntity, recording.RefusalIntegrity, "channel.endpoint must not name a fragment")
	}
	if loop == nil || loop(endpoint) {
		return refuse(http.StatusUnprocessableEntity, recording.RefusalRouting, "channel.endpoint is a notification address of this network")
	}
	return nil
}

// Owns is §7.2's ownership test, decided from the Subscription alone, at the
// notifier's gateway whose ingress is ingress: the Subscription belongs to
// subscriber when its channel.endpoint is a notification address of this
// gateway for that subscriber and t, and its channel.header holds exactly one
// Authorization and one X-SHN-Channel value whose credential verifies for
// that address and those sealed headers (§6.3).
func (k Keys) Owns(s *Subscription, ingress, subscriber string, t Topic) bool {
	ep, ok := s.Endpoint()
	if !ok {
		return false
	}
	a, ok := ParseAddress(ep, ingress)
	if !ok || a.Topic != t.Token() || a.Subscriber != subscriber {
		return false
	}
	headers, ok := s.headers()
	if !ok {
		return false
	}
	auth, okA := headerValue(headers, AuthorizationHeader)
	sealed, okC := headerValue(headers, ChannelHeader)
	return okA && okC && k.VerifyCredential(a, sealed, auth) == nil
}

// The search and result parameters a Subscription search may carry (§7.2,
// N7), and a page's (§7.2 "Pages").
var (
	searchParams = []string{"url", "status", "_id", "_count", "_elements"}
	pageParams   = []string{"_getpages", "_getpagesoffset", "_count", "_bundletype", "_elements"}
	// elementName is one element an _elements names.
	elementName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]*$`)
)

// CheckSearch is §7.2's search rule, at the notifier's gateway before
// anything is sent. A search is forwarded only when it carries exactly one
// non-empty url, and otherwise only the search parameters status and _id and
// the result parameters _count and _elements, each _elements keeping channel.
// Anything else is refused 403, recorded authority: a search with no url, any
// parameter with a modifier (url:below, status:not and the rest), any other
// parameter (_summary and _total among them), and an _elements that leaves
// out channel, whose answer could not be tested.
func CheckSearch(q url.Values) *Refusal {
	if r := checkParams(q, searchParams, "a search"); r != nil {
		return r
	}
	if urls := q["url"]; len(urls) != 1 || urls[0] == "" {
		return forbidden("a Subscription search must carry exactly one url, which names the subscriber's own notification endpoint")
	}
	for _, v := range q["_elements"] {
		if !slices.Contains(strings.Split(v, ","), "channel") {
			return forbidden("a Subscription search's _elements must keep channel, or its answer cannot be checked")
		}
	}
	return nil
}

// CheckPageQuery is §7.2's rule for a page of a search's answer: exactly one
// non-empty _getpages, and otherwise only _getpagesoffset, _count,
// _bundletype and, as a HAPI FHIR server's page link copies it from the
// search, _elements, none with a modifier. An _elements is given at most once,
// names only elements, and keeps channel, as CheckSearch requires of a search:
// a page that drops channel could not be tested. Anything else is refused
// 403, recorded authority. q carries no order: the order a page's link writes
// its parameters in (_elements last) is the frame's grammar
// (shnsdk.ParseSubscriptionRequest).
func CheckPageQuery(q url.Values) *Refusal {
	if r := checkParams(q, pageParams, "a page"); r != nil {
		return r
	}
	if p := q["_getpages"]; len(p) != 1 || p[0] == "" {
		return forbidden("a page of a Subscription search must carry exactly one _getpages")
	}
	if e, ok := q["_elements"]; ok {
		if len(e) != 1 {
			return forbidden("a page of a Subscription search carries _elements at most once")
		}
		names := strings.Split(e[0], ",")
		for _, n := range names {
			if !elementName.MatchString(n) {
				return forbidden("a page of a Subscription search's _elements names only elements")
			}
		}
		if !slices.Contains(names, "channel") {
			return forbidden("a page of a Subscription search's _elements must keep channel, or its answer cannot be checked")
		}
	}
	return nil
}

func checkParams(q url.Values, allowed []string, what string) *Refusal {
	for _, k := range slices.Sorted(maps.Keys(q)) {
		if strings.Contains(k, ":") {
			return forbidden(what + " of Subscriptions carries a parameter with a modifier, which this network does not carry: " + k)
		}
		if !slices.Contains(allowed, k) {
			return forbidden(what + " of Subscriptions carries a parameter this network does not carry: " + k + " (only " + strings.Join(allowed, ", ") + ")")
		}
	}
	return nil
}

var fhirID = regexp.MustCompile(`^[A-Za-z0-9\-.]{1,64}$`)

// CheckID is §6.1's id rule, at both gateways: a Subscription id or version
// outside [A-Za-z0-9\-\.]{1,64}, or the dot segment "." or "..", which a
// server reading the path below its base would take as a step out of it, is
// refused 400, recorded routing.
func CheckID(id string) *Refusal {
	if len(id) > maxFHIRIDLen || !fhirID.MatchString(id) || id == "." || id == ".." {
		return refuse(http.StatusBadRequest, recording.RefusalRouting, "a Subscription id or version must be a FHIR id ([A-Za-z0-9-.]{1,64}) and not \".\" or \"..\"")
	}
	return nil
}
