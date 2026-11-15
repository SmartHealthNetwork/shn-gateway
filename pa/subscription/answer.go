package subscription

import (
	"bytes"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
	"github.com/SmartHealthNetwork/shn-gateway/pa/dtraddress"
	"github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// BackportSubscriptionStatusProfile is the R4 Backport IG's profile of the
// SubscriptionStatus Parameters, which an instance $status answers with, one
// entry of a searchset Bundle (STU 1.1; a history Bundle before it).
const BackportSubscriptionStatusProfile = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription-status-r4"

// statusParameters are the parameters the SubscriptionStatus Parameters
// profile defines, each with the value[x] members it may carry; a parameter
// with no value carries none. notification-event carries parts instead
// (statusEventParts).
var statusParameters = map[string][]string{
	"subscription":                    {"valueReference"},
	"topic":                           {"valueCanonical", "valueUri"},
	"status":                          {"valueCode"},
	"type":                            {"valueCode"},
	"events-since-subscription-start": {"valueString", "valueInteger", "valueUnsignedInt", "valueDecimal"},
	"events-in-notification":          {"valueString", "valueInteger", "valueUnsignedInt", "valueDecimal"},
	"error":                           {"valueCodeableConcept"},
	"notification-event":              nil,
}

// statusEventParts are the parts of a notification-event parameter, each
// with the value[x] members it may carry.
var statusEventParts = map[string][]string{
	"event-number":       {"valueString", "valueInteger", "valueUnsignedInt", "valueDecimal"},
	"timestamp":          {"valueInstant", "valueDateTime"},
	"focus":              {"valueReference"},
	"additional-context": {"valueReference"},
}

func badAnswer(reason string) *Refusal {
	return refuse(http.StatusBadGateway, recording.RefusalAuthority, badAnswerPrefix+reason)
}

// badAnswerPrefix opens every badAnswer reason.
const badAnswerPrefix = "the provider's answer on a Subscription interaction is not one this network delivers: "

// CheckSubscribeAnswer is the notifier gateway's check of a 2xx answer its
// system gave on a subscribe leg, before the answer is delivered: structural
// and exact, a network rule at every level, tied to the interaction it
// answers (in, as the frame's subscriptionRequest names it; id the
// Subscription's id, read only for an instance $status). It passes exactly:
//   - a read or a vread: a Subscription;
//   - a create or an update: a Subscription, no body, or an OperationOutcome
//     (a system that answers minimal by its own default);
//   - a delete: an OperationOutcome, or no body;
//   - a search or a page of one: a searchset Bundle whose every entry's
//     resource is a Subscription or an OperationOutcome;
//   - an instance $status: a Bundle of type searchset (the Backport IG STU
//     1.1's) or history (an earlier server's), whose entries are exactly one
//     SubscriptionStatus Parameters of the profile's shape (statusShape),
//     whose subscription names id.
//
// A shape valid for another interaction is refused. Nothing in any of them
// may hold a resource but each Bundle entry's own: no contained resource and
// no resource-valued member, an OperationOutcome included. Member names are
// read folded, as a lenient reader would take them, and a member that is not
// the array or object it should be fails closed. Anything else, a body that
// is not one unambiguous JSON object included, is refused 502, recorded
// authority: the network delivers no other resource on this leg.
func CheckSubscribeAnswer(body []byte, in shnsdk.SubscriptionInteraction, id string, shape NotificationShape) *Refusal {
	_, r := AnswerSubscriptions(body, in, id, shape)
	return r
}

// AnswerSubscriptions is CheckSubscribeAnswer, and the Subscriptions the answer
// carries when it passes, in order: the answer itself when it is one, and each
// entry's of a searchset Bundle. The caller holds each to the ownership test
// (Keys.Owns): read here as CheckSubscribeAnswer reads the answer, so a
// Subscription the check passes is never one the caller does not see.
func AnswerSubscriptions(body []byte, in shnsdk.SubscriptionInteraction, id string, shape NotificationShape) ([]*Subscription, *Refusal) {
	noBody := in == shnsdk.SubscriptionCreate || in == shnsdk.SubscriptionUpdate || in == shnsdk.SubscriptionDelete
	// A JSON object or array followed by anything is never one JSON
	// value, whatever ClassifyBody makes of its first.
	if jsonValueThenMore(body) {
		return nil, badAnswer("it is not one JSON object this network can read unambiguously")
	}
	switch kind, _, err := ClassifyBody(body); {
	case kind == BodyEmpty && noBody:
		return nil, nil
	case kind == BodyEmpty:
		return nil, badAnswer("it has no body, which a " + string(in) + " is not answered with")
	case kind == BodyFHIRXML:
		return nil, badAnswer("it is FHIR XML, which this network reads only as JSON and so cannot check")
	case kind == BodyNotFHIR:
		return nil, badAnswer("it is not FHIR")
	case err != nil:
		return nil, badAnswer("it is not one JSON object this network can read unambiguously")
	}
	// Read after a UTF-8 byte order mark, as ClassifyBody and
	// CheckErrorAnswer read it: the classifier took it for FHIR JSON.
	body = bytes.TrimPrefix(body, utf8BOM)
	root, err := scanJSON(body)
	if err != nil || root.kind != '{' {
		return nil, badAnswer("it is not one JSON object this network can read unambiguously")
	}
	rt, _ := root.memberFolded("resourceType").stringOf()
	switch in {
	case shnsdk.SubscriptionRead, shnsdk.SubscriptionVRead, shnsdk.SubscriptionCreate, shnsdk.SubscriptionUpdate:
		if rt == "Subscription" {
			if holdsResource(root, true) {
				return nil, badAnswer("the Subscription holds a resource")
			}
			return []*Subscription{{body: body, root: root}}, nil
		}
		if rt == "OperationOutcome" && noBody {
			return nil, outcomeShape(root)
		}
	case shnsdk.SubscriptionDelete:
		if rt == "OperationOutcome" {
			return nil, outcomeShape(root)
		}
	case shnsdk.SubscriptionSearch, shnsdk.SubscriptionPage:
		if rt == "Bundle" {
			return searchsetSubscriptions(body, root)
		}
	case shnsdk.SubscriptionStatus:
		if rt == "Bundle" {
			return nil, statusBundle(root, id, shape)
		}
	}
	return nil, badAnswer("a " + string(in) + " is not answered with this (" + rt + ")")
}

// outcomeShape refuses an OperationOutcome that holds a resource.
func outcomeShape(root *jnode) *Refusal {
	if holdsResource(root, true) {
		return badAnswer("the OperationOutcome holds a resource")
	}
	return nil
}

// bundleEntries is a Bundle of one of types, its entries checked to be
// objects each carrying one resource object and nothing else that holds a
// resource, with the Bundle holding no resource outside its entries. It
// returns each entry's resource.
func bundleEntries(root *jnode, types ...string) ([]*jnode, *Refusal) {
	typ, _ := root.memberFolded("type").stringOf()
	if !slices.Contains(types, typ) {
		return nil, badAnswer("the Bundle is not of type " + strings.Join(types, " or "))
	}
	entries := root.memberFolded("entry")
	if entries != nil && entries.kind != '[' {
		return nil, badAnswer("the Bundle's entry is not an array")
	}
	for _, m := range root.members {
		if jsonmember.FoldedName(m.name) == foldedEntry {
			continue
		}
		if holdsResource(m.val, false) {
			return nil, badAnswer("the Bundle holds a resource outside its entries")
		}
	}
	var out []*jnode
	if entries == nil {
		return out, nil
	}
	for _, e := range entries.elems {
		if e.kind != '{' {
			return nil, badAnswer("an entry is not an object")
		}
		res := e.memberFolded("resource")
		if res == nil || res.kind != '{' {
			return nil, badAnswer("an entry carries no resource object")
		}
		for _, m := range e.members {
			if jsonmember.FoldedName(m.name) != foldedResource && holdsResource(m.val, false) {
				return nil, badAnswer("an entry holds a resource outside its resource")
			}
		}
		if holdsResource(res, true) {
			return nil, badAnswer("an entry's resource holds a resource")
		}
		out = append(out, res)
	}
	return out, nil
}

// searchsetSubscriptions is AnswerSubscriptions for a search's or a page's
// answer: a searchset whose entries are Subscriptions and OperationOutcomes.
func searchsetSubscriptions(body []byte, root *jnode) ([]*Subscription, *Refusal) {
	resources, r := bundleEntries(root, "searchset")
	if r != nil {
		return nil, r
	}
	var subs []*Subscription
	for _, res := range resources {
		switch rt, _ := res.memberFolded("resourceType").stringOf(); rt {
		case "Subscription":
			subs = append(subs, &Subscription{body: body, root: res})
		case "OperationOutcome":
		default:
			return nil, badAnswer("the searchset carries an entry of another type")
		}
	}
	return subs, nil
}

// statusCheck is what a SubscriptionStatus Bundle is held to: what names the
// body in a refusal; entry, its SubscriptionStatus; types, the Bundle types it
// may be; addressTypes, the resource types an address in its Bundle may
// name; and notification, a notification's entry layout (the SubscriptionStatus
// first, then an entry per focus that carries no resource) for an instance
// $status answer's (exactly one entry, the SubscriptionStatus).
//
// A notification's addresses are judged by path segment (bySegment), below
// base, the provider's FHIR base its gateway states, when an address is under
// it: a segment that is a resource type's name, not a token inside one, so
// an id such as Task/Patient-1 and a base such as /Organization/9/fhir name
// nothing. An instance $status answer's are judged by token, as they always
// were.
type statusCheck struct {
	what, entry  string
	types        []string
	addressTypes []string
	notification bool
	bySegment    bool
	base         string
}

// statusAnswer is an instance $status answer's statusCheck.
var statusAnswer = statusCheck{what: "a $status answer", entry: "a $status entry", types: []string{"searchset", "history"}, addressTypes: statusAddressTypes}

// statusBundle is AnswerSubscriptions for an instance $status's answer: a
// searchset (Backport STU 1.1) or history Bundle whose entries are exactly one
// SubscriptionStatus Parameters, whose subscription names id.
func statusBundle(root *jnode, id string, shape NotificationShape) *Refusal {
	p, r := checkStatusBundle(root, shape, statusAnswer)
	if r != nil {
		return r
	}
	if !statusNames(p, id) {
		return badAnswer("the SubscriptionStatus names another Subscription than the one asked for")
	}
	return nil
}

// checkStatusBundle holds root, a SubscriptionStatus Bundle, to k and to the
// topic's notification shape, and returns its SubscriptionStatus: the whole
// Bundle against the Backport SubscriptionStatus schema, closed at every
// depth; its members and meta; its entries (k's layout); the
// SubscriptionStatus's parameters and parts (statusShape); each focus and
// additional-context to the shape (statusEventReferences); its addresses
// (statusBundleAddresses); its topic (statusTopic); and no extension and no
// reference but its own anywhere (statusOnlyItsReferences).
func checkStatusBundle(root *jnode, shape NotificationShape, k statusCheck) (*jnode, *Refusal) {
	// The whole Bundle first, against the Backport SubscriptionStatus
	// shape: every member at every depth one it defines, of its type.
	if why := statusAnswerSchema.check(root, "the Bundle"); why != "" {
		return nil, badAnswer(k.what + " is outside the Backport SubscriptionStatus shape: " + why)
	}
	if name, ok := onlyMembers(root, statusBundleMembers); !ok {
		return nil, badAnswer(k.what + "'s Bundle carries " + name + ", which this network does not deliver on it")
	}
	if meta := root.memberFolded("meta"); meta != nil {
		if name, ok := onlyMembers(meta, statusMetaMembers); !ok {
			return nil, badAnswer(k.what + "'s Bundle meta carries " + name)
		}
	}
	var p *jnode
	if k.notification {
		var r *Refusal
		if p, r = notificationEntries(root, k); r != nil {
			return nil, r
		}
	} else {
		resources, r := bundleEntries(root, k.types...)
		if r != nil {
			return nil, r
		}
		if len(resources) != 1 {
			return nil, badAnswer(k.what + " carries exactly one SubscriptionStatus")
		}
		p = resources[0]
	}
	if rt, _ := p.memberFolded("resourceType").stringOf(); rt != "Parameters" {
		return nil, badAnswer(k.what + "'s entry is not a SubscriptionStatus Parameters")
	}
	if r := statusShape(p, k); r != nil {
		return nil, r
	}
	if r := statusEventReferences(p, shape); r != nil {
		return nil, r
	}
	if r := statusBundleAddresses(root, k); r != nil {
		return nil, r
	}
	if r := statusTopic(p, shape, k); r != nil {
		return nil, r
	}
	if r := statusOnlyItsReferences(root, p, k); r != nil {
		return nil, r
	}
	return p, nil
}

// notificationEntries is a notification's entry layout (k.notification): a
// Bundle of one of k.types holding no resource outside its entries, whose
// first entry carries the SubscriptionStatus as its one resource object, and
// whose every later entry (an id-only event's, one per focus) carries no
// resource at all and holds none. It returns the SubscriptionStatus.
func notificationEntries(root *jnode, k statusCheck) (*jnode, *Refusal) {
	typ, _ := root.memberFolded("type").stringOf()
	if !slices.Contains(k.types, typ) {
		return nil, badAnswer(k.what + "'s Bundle is not of type " + strings.Join(k.types, " or "))
	}
	entries := root.memberFolded("entry")
	if entries == nil || entries.kind != '[' || len(entries.elems) == 0 {
		return nil, badAnswer(k.what + "'s Bundle has no entry array")
	}
	for _, m := range root.members {
		if jsonmember.FoldedName(m.name) != foldedEntry && holdsResource(m.val, false) {
			return nil, badAnswer(k.what + "'s Bundle holds a resource outside its entries")
		}
	}
	var status *jnode
	for i, e := range entries.elems {
		if e.kind != '{' {
			return nil, badAnswer(k.what + "'s entry is not an object")
		}
		res := e.memberFolded("resource")
		switch {
		case i == 0 && (res == nil || res.kind != '{'):
			return nil, badAnswer(k.what + "'s first entry carries no SubscriptionStatus")
		case i == 0 && holdsResource(res, true):
			return nil, badAnswer(k.what + "'s SubscriptionStatus holds a resource")
		case i > 0 && res != nil:
			return nil, badAnswer(k.what + " carries a resource beside its SubscriptionStatus")
		}
		for _, m := range e.members {
			if jsonmember.FoldedName(m.name) != foldedResource && holdsResource(m.val, false) {
				return nil, badAnswer(k.what + "'s entry holds a resource outside its resource")
			}
		}
		if i == 0 {
			status = res
		}
	}
	return status, nil
}

// statusNames reports whether the SubscriptionStatus p's subscription
// parameter references the Subscription id: "Subscription/<id>", or an
// absolute http or https URL whose path ends in it, with no query, fragment
// or version.
func statusNames(p *jnode, id string) bool {
	if id == "" {
		return false
	}
	for _, pm := range p.memberFolded("parameter").elems {
		if name, _ := pm.memberFolded("name").stringOf(); name != "subscription" {
			continue
		}
		vr := pm.memberFolded("valueReference")
		ref, ok := vr.memberFolded("reference").stringOf()
		if !ok {
			return false
		}
		// A type, when stated, names a Subscription, as the focus's must
		// name what its reference does.
		if tm := vr.memberFolded("type"); tm != nil {
			if t, ok := tm.stringOf(); !ok || (t != "Subscription" && t != "http://hl7.org/fhir/StructureDefinition/Subscription") {
				return false
			}
		}
		want := "Subscription/" + id
		if ref == want {
			return true
		}
		u, err := url.Parse(ref)
		return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.RawQuery == "" && !u.ForceQuery &&
			u.Fragment == "" && !strings.ContainsRune(ref, '#') && strings.HasSuffix(u.EscapedPath(), "/"+want)
	}
	return false
}

// CheckErrorAnswer is the notifier gateway's check of an answer its system gave
// on a subscribe leg that is not a 2xx, before it is carried: a network rule at
// every level, the body read by its structure alone (ClassifyBody), never by
// the media type it is labelled with. FHIR JSON passes only as an
// OperationOutcome that holds no resource (no contained resource, no
// resource-valued member); FHIR XML only as an OperationOutcome root holding
// no element named contained or as a FHIR resource type at any depth; any
// other FHIR (XML outside the FHIR namespace holding an element in it
// included), and FHIR that cannot be read one way only, is refused 502,
// recorded authority. An answer that is not FHIR (an HTML page, plain text,
// JSON that names no resource type, such a JSON value followed by text) or
// has no body is carried as on every leg; an OperationOutcome followed by
// text that is not FHIR JSON is judged as that OperationOutcome.
func CheckErrorAnswer(body []byte) *Refusal {
	kind, rt, err := ClassifyBody(body)
	switch kind {
	case BodyEmpty, BodyNotFHIR:
		return nil
	case BodyFHIRXML:
		if err != nil {
			return badAnswer("its error answer is FHIR XML this network cannot read one way only")
		}
		if rt != "OperationOutcome" {
			return badAnswer("its error answer is a resource other than an OperationOutcome")
		}
		x, _ := readXML(bytes.TrimLeft(bytes.TrimPrefix(body, utf8BOM), " \t\r\n"))
		if xmlHoldsResource(x) {
			return badAnswer("the OperationOutcome holds a resource")
		}
		return nil
	}
	if err != nil {
		return badAnswer("its error answer is FHIR JSON this network cannot read unambiguously")
	}
	if rt != "OperationOutcome" {
		return badAnswer("its error answer is a resource other than an OperationOutcome")
	}
	// The OperationOutcome is the body's first JSON value; what follows it
	// holds no FHIR JSON, or the body would be unreadable (ClassifyBody).
	root, err := scanJSON(firstJSONValue(bytes.TrimLeft(bytes.TrimPrefix(body, utf8BOM), " \t\r\n")))
	if err != nil {
		return badAnswer("its error answer is FHIR JSON this network cannot read unambiguously")
	}
	return outcomeShape(root)
}

// CheckNoResource is the network's rule on a Subscription a subscribe leg
// carries, at the notifier's gateway on a create or update, a rule at every
// level: the leg names no patient on either side, so the Subscription holds no
// resource. A contained member anywhere, or any object with a resourceType
// member (in any case) below the Subscription itself, decided by structure and
// never by the resource's type, is refused 403, recorded authority.
func CheckNoResource(s *Subscription) *Refusal {
	if holdsResource(s.root, true) {
		return forbidden("the Subscription holds a resource, which this leg, naming no patient, never carries")
	}
	return nil
}

var (
	foldedEntry        = jsonmember.FoldedName("entry")
	foldedResource     = jsonmember.FoldedName("resource")
	foldedResourceType = jsonmember.FoldedName("resourceType")
	foldedContained    = jsonmember.FoldedName("contained")
)

// holdsResource reports whether n holds a resource: an object with a
// resourceType member below n (or n itself, unless self is set, when n is the
// resource being judged), or a contained member anywhere in n.
func holdsResource(n *jnode, self bool) bool {
	if n == nil {
		return false
	}
	for _, m := range n.members {
		f := jsonmember.FoldedName(m.name)
		if f == foldedContained || (!self && f == foldedResourceType) {
			return true
		}
		if holdsResource(m.val, false) {
			return true
		}
	}
	for _, e := range n.elems {
		if holdsResource(e, false) {
			return true
		}
	}
	return false
}

// statusShape is the SubscriptionStatus Parameters' shape, exactly: a
// meta.profile, when present, naming the Backport status profile alone (with
// or without a |version); a parameter array of objects, each named one of the
// profile's parameters and carrying only its name, an id, and the one value
// it may carry, or, for notification-event, parts of the profile's own, each
// carrying only its name, an id and its one value; exactly one subscription and
// one type; and a reference carrying only reference and type. No other member
// is read past; anything else is refused 502, recorded authority.
func statusShape(p *jnode, k statusCheck) *Refusal {
	notStatus := func(why string) *Refusal { return badAnswer(k.entry + " is not a SubscriptionStatus: " + why) }
	if name, ok := onlyMembers(p, statusParametersMembers); !ok {
		return notStatus("it carries " + name + ", which a Parameters resource does not define here")
	}
	if meta := p.memberFolded("meta"); meta != nil {
		if name, ok := onlyMembers(meta, statusMetaMembers); !ok {
			return notStatus("its meta carries " + name)
		}
		prof := meta.memberFolded("profile")
		if prof != nil && prof.kind != '[' {
			return notStatus("meta.profile is not an array")
		}
		if prof != nil {
			for _, e := range prof.elems {
				if v, ok := e.stringOf(); !ok || !canonicalIs(v, BackportSubscriptionStatusProfile) {
					return notStatus("it names another profile")
				}
			}
		}
	}
	params := p.memberFolded("parameter")
	if params == nil || params.kind != '[' {
		return notStatus("it has no parameter array")
	}
	counts := map[string]int{}
	for _, pm := range params.elems {
		name, r := paramShape(pm, statusParameters)
		if r != nil {
			return notStatus(r.Reason)
		}
		counts[name]++
		if name != "notification-event" {
			continue
		}
		parts := pm.memberFolded("part")
		if parts == nil {
			continue
		}
		if parts.kind != '[' {
			return notStatus("a notification-event's part is not an array")
		}
		for _, pt := range parts.elems {
			if _, r := paramShape(pt, statusEventParts); r != nil {
				return notStatus(r.Reason)
			}
		}
	}
	if counts["subscription"] != 1 || counts["type"] != 1 {
		return notStatus("it names not exactly one subscription and one type")
	}
	return nil
}

// statusParametersMembers are the members a SubscriptionStatus Parameters
// may carry on a $status answer: Parameters' own, which leave out text,
// contained, extension and modifierExtension (a Parameters resource is not a
// DomainResource in R4), so no narrative sits beside the parameters; an
// extension or a reference anywhere else is statusOnlyItsReferences'.
var statusParametersMembers = []string{"resourceType", "id", "meta", "implicitRules", "language", "parameter"}

// statusMetaMembers are the members a $status answer's meta may carry:
// Meta's own but extension.
var statusMetaMembers = []string{"id", "versionId", "lastUpdated", "source", "profile", "security", "tag"}

// statusBundleMembers are the members a $status answer's Bundle may carry:
// a Bundle's own but identifier and signature, which can hold a reference.
var statusBundleMembers = []string{"resourceType", "id", "meta", "implicitRules", "language", "type", "timestamp", "total", "link", "entry"}

// onlyMembers reports whether n, an object, carries only the members allowed
// names (read folded), and the first it carries that is not one.
func onlyMembers(n *jnode, allowed []string) (string, bool) {
	if n.kind != '{' {
		return "something that is not an object", false
	}
	for _, m := range n.members {
		f := jsonmember.FoldedName(m.name)
		if !slices.ContainsFunc(allowed, func(a string) bool { return jsonmember.FoldedName(a) == f }) {
			return m.name, false
		}
	}
	return "", true
}

// jschema is a JSON shape: kind '"' a string, '0' a number, 'b' true or
// false, '{' an object whose members are only those named (compared folded),
// each of its shape, '[' an array whose every element is of elem's shape.
type jschema struct {
	kind    byte
	members map[string]*jschema
	elem    *jschema
}

func obj(members map[string]*jschema) *jschema {
	folded := make(map[string]*jschema, len(members))
	for name, m := range members {
		folded[jsonmember.FoldedName(name)] = m
	}
	return &jschema{kind: '{', members: folded}
}

func arr(elem *jschema) *jschema { return &jschema{kind: '[', elem: elem} }

// check reports why n is not of s's shape ("" when it is), at, the member
// it is, named in the reason.
func (s *jschema) check(n *jnode, at string) string {
	switch s.kind {
	case 'b':
		if n.kind != 't' && n.kind != 'f' {
			return at + " is not true or false"
		}
	case '{', '[', '"', '0':
		if n.kind != s.kind {
			return at + " is not the JSON type it is written as"
		}
	}
	switch s.kind {
	case '{':
		for _, m := range n.members {
			ms := s.members[jsonmember.FoldedName(m.name)]
			if ms == nil {
				return at + " carries " + m.name
			}
			if why := ms.check(m.val, at+"."+m.name); why != "" {
				return why
			}
		}
	case '[':
		for _, e := range n.elems {
			if why := s.elem.check(e, at+"[]"); why != "" {
				return why
			}
		}
	}
	return ""
}

// statusAnswerSchema is a $status answer as the Backport IG's
// SubscriptionStatus defines it, closed at every depth: a Bundle (searchset
// or history) of entries, each a Parameters of the status parameters and
// parts, with Meta, Coding, CodeableConcept and Reference their own members
// and every value of its FHIR type's JSON type. It admits no extension
// anywhere; which parameter carries which value, the Bundle's type, the
// references and the addresses are statusBundle's other checks.
var statusAnswerSchema = func() *jschema {
	str, num, boolean := &jschema{kind: '"'}, &jschema{kind: '0'}, &jschema{kind: 'b'}
	coding := obj(map[string]*jschema{"id": str, "system": str, "version": str, "code": str, "display": str, "userSelected": boolean})
	cc := obj(map[string]*jschema{"id": str, "coding": arr(coding), "text": str})
	ref := obj(map[string]*jschema{"reference": str, "type": str})
	meta := obj(map[string]*jschema{"id": str, "versionId": str, "lastUpdated": str, "source": str, "profile": arr(str), "security": arr(coding), "tag": arr(coding)})
	values := func(m map[string]*jschema) map[string]*jschema {
		for _, v := range []string{"valueCanonical", "valueUri", "valueCode", "valueString", "valueInstant", "valueDateTime"} {
			m[v] = str
		}
		for _, v := range []string{"valueInteger", "valueUnsignedInt", "valueDecimal"} {
			m[v] = num
		}
		m["valueReference"], m["valueCodeableConcept"] = ref, cc
		return m
	}
	part := obj(values(map[string]*jschema{"id": str, "name": str}))
	param := obj(values(map[string]*jschema{"id": str, "name": str, "part": arr(part)}))
	parameters := obj(map[string]*jschema{"resourceType": str, "id": str, "meta": meta, "implicitRules": str, "language": str, "parameter": arr(param)})
	entry := obj(map[string]*jschema{
		"fullUrl":  str,
		"resource": parameters,
		"search":   obj(map[string]*jschema{"mode": str, "score": num}),
		"request":  obj(map[string]*jschema{"method": str, "url": str}),
		"response": obj(map[string]*jschema{"status": str, "location": str, "etag": str, "lastModified": str}),
	})
	return obj(map[string]*jschema{
		"resourceType": str, "id": str, "meta": meta, "implicitRules": str, "language": str,
		"type": str, "timestamp": str, "total": num,
		"link":  arr(obj(map[string]*jschema{"relation": str, "url": str})),
		"entry": arr(entry),
	})
}()

// codeableConceptShape holds a CodeableConcept (the error parameter's) to its
// own members, and each of its codings to a Coding's, so it carries nothing
// that can name a resource (an identifier, a type).
func codeableConceptShape(cc *jnode) *Refusal {
	if name, ok := onlyMembers(cc, []string{"id", "coding", "text"}); !ok {
		return &Refusal{Reason: "a CodeableConcept carries " + name}
	}
	codings := cc.memberFolded("coding")
	if codings == nil {
		return nil
	}
	if codings.kind != '[' {
		return &Refusal{Reason: "a CodeableConcept's coding is not an array"}
	}
	for _, c := range codings.elems {
		if name, ok := onlyMembers(c, []string{"id", "system", "version", "code", "display", "userSelected"}); !ok {
			return &Refusal{Reason: "a Coding carries " + name}
		}
	}
	return nil
}

// statusTopic holds the SubscriptionStatus p's topic parameter, when it names
// one, to the topic's own canonical (shape.Topic, with or without a
// |version): a $status answer names no other topic, and so no other address.
func statusTopic(p *jnode, shape NotificationShape, k statusCheck) *Refusal {
	for _, pm := range p.memberFolded("parameter").elems {
		if name, _ := pm.memberFolded("name").stringOf(); name != "topic" {
			continue
		}
		for _, m := range []string{"valueCanonical", "valueUri"} {
			v := pm.memberFolded(m)
			if v == nil {
				continue
			}
			if s, ok := v.stringOf(); !ok || shape.Topic == "" || !canonicalIs(s, shape.Topic) {
				return badAnswer(k.what + "'s topic is not the topic's own canonical")
			}
		}
	}
	return nil
}

// valueKind is the JSON kind a SubscriptionStatus value[x] member is written
// as: an object for a Reference or a CodeableConcept, a number for an
// integer, unsignedInt or decimal, and a string for every other type the
// profile's parameters carry.
func valueKind(member string) byte {
	switch jsonmember.FoldedName(member) {
	case jsonmember.FoldedName("valueReference"), jsonmember.FoldedName("valueCodeableConcept"):
		return '{'
	case jsonmember.FoldedName("valueInteger"), jsonmember.FoldedName("valueUnsignedInt"), jsonmember.FoldedName("valueDecimal"):
		return '0'
	}
	return '"'
}

// statusEntryMembers, statusSearchMembers, statusRequestMembers,
// statusResponseMembers and statusLinkMembers close a $status answer's
// Bundle entries and links to their own members (a response's outcome, a
// resource, is left out: the entry's resource is the one it carries).
var (
	statusEntryMembers    = []string{"fullUrl", "resource", "search", "request", "response"}
	statusSearchMembers   = []string{"mode", "score"}
	statusRequestMembers  = []string{"method", "url"}
	statusResponseMembers = []string{"status", "location", "etag", "lastModified"}
	statusLinkMembers     = []string{"relation", "url"}
)

// statusAddressTypes are the resource types an address in a $status answer's
// Bundle (an entry's fullUrl, request.url and response.location, a link's
// url) may name: the Subscription asked about and its SubscriptionStatus.
var statusAddressTypes = []string{"Subscription", "Parameters"}

// statusBundleAddresses holds a $status answer's entries and links to their
// own members, each address among them a string that names no resource type
// but statusAddressTypes: every token of it, split at anything but a letter or
// digit, that is a FHIR R4 resource type's name (fhirResourceTypes) must be
// one of them, so no address names a Patient, in its path or its query.
func statusBundleAddresses(root *jnode, k statusCheck) *Refusal {
	address := func(n *jnode, what string) *Refusal {
		if n == nil {
			return nil
		}
		raw, ok := n.stringOf()
		if !ok {
			return badAnswer(k.what + "'s " + what + " is not a string")
		}
		if k.bySegment {
			return segmentTypes(raw, what, k)
		}
		// Read as a server reads a URL's path and query: percent-escapes
		// decoded, so an escaped letter hides no type's name.
		v, err := url.PathUnescape(raw)
		if err != nil {
			return badAnswer(k.what + "'s " + what + " cannot be read one way only")
		}
		for _, tok := range strings.FieldsFunc(v, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if fhirResourceTypes[tok] && !slices.Contains(k.addressTypes, tok) {
				return badAnswer(k.what + "'s " + what + " names a " + tok)
			}
		}
		return nil
	}
	closed := func(n *jnode, allowed []string, what string) *Refusal {
		if n == nil {
			return nil
		}
		if name, ok := onlyMembers(n, allowed); !ok {
			return badAnswer(k.what + "'s " + what + " carries " + name)
		}
		return nil
	}
	if links := root.memberFolded("link"); links != nil {
		if links.kind != '[' {
			return badAnswer(k.what + "'s link is not an array")
		}
		for _, l := range links.elems {
			if r := closed(l, statusLinkMembers, "link"); r != nil {
				return r
			}
			if r := address(l.memberFolded("url"), "link url"); r != nil {
				return r
			}
		}
	}
	entries := root.memberFolded("entry")
	if entries == nil {
		return nil
	}
	for _, e := range entries.elems {
		if r := closed(e, statusEntryMembers, "entry"); r != nil {
			return r
		}
		if r := address(e.memberFolded("fullUrl"), "entry's fullUrl"); r != nil {
			return r
		}
		for _, c := range []struct {
			member  string
			allowed []string
			addr    string
		}{{"search", statusSearchMembers, ""}, {"request", statusRequestMembers, "url"}, {"response", statusResponseMembers, "location"}} {
			n := e.memberFolded(c.member)
			if r := closed(n, c.allowed, "entry's "+c.member); r != nil {
				return r
			}
			if n != nil && c.addr != "" {
				if r := address(n.memberFolded(c.addr), "entry's "+c.member+"."+c.addr); r != nil {
					return r
				}
			}
		}
	}
	return nil
}

// segmentTypes is statusBundleAddresses' rule for a notification's address
// raw (k.bySegment): the address names no resource type but k.addressTypes
// in a path segment, read percent-decoded and split at every character a FHIR
// id never holds, below k.base when the address is under it
// (dtraddress.Under), so the provider's base and a Task's id (Patient-1) name
// nothing, while Patient%2F, a backslash after Patient, and Patient; do; and
// none in its query, read by token as a server reads it, since
// a search parameter's value may name one.
func segmentTypes(raw, what string, k statusCheck) *Refusal {
	unreadable := badAnswer(k.what + "'s " + what + " cannot be read one way only")
	path, query, _ := strings.Cut(raw, "?")
	path, _, _ = strings.Cut(path, "#")
	if rem, ok := dtraddress.Under(path, strings.TrimRight(k.base, "/")); k.base != "" && ok {
		path = rem
	} else if u, err := url.Parse(path); err == nil && u.Host != "" {
		path = u.EscapedPath()
	}
	for _, seg := range strings.Split(path, "/") {
		s, err := url.PathUnescape(seg)
		if err != nil {
			return unreadable
		}
		// Each piece of the decoded segment between characters a FHIR id
		// never holds (a slash, escaped or not, a backslash a WHATWG reader
		// takes for one, a path parameter's semicolon, and every other), so
		// no reader finds a type's name the segment hides.
		for _, piece := range strings.FieldsFunc(s, func(r rune) bool { return !fhirIDRune(r) }) {
			if fhirResourceTypes[piece] && !slices.Contains(k.addressTypes, piece) {
				return badAnswer(k.what + "'s " + what + " names a " + piece)
			}
		}
	}
	q, err := url.QueryUnescape(query)
	if err != nil {
		return unreadable
	}
	for _, tok := range strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if fhirResourceTypes[tok] && !slices.Contains(k.addressTypes, tok) {
			return badAnswer(k.what + "'s " + what + " names a " + tok)
		}
	}
	return nil
}

// fhirIDRune reports whether r may appear in a FHIR id: an ASCII letter or
// digit, a hyphen or a dot.
func fhirIDRune(r rune) bool {
	// r|0x20 folds an ASCII capital to its lower-case letter.
	lower := r | 0x20
	return 'a' <= lower && lower <= 'z' || '0' <= r && r <= '9' || r == '-' || r == '.'
}

// statusOnlyItsReferences holds a $status answer, its Bundle and the
// SubscriptionStatus p in it, to carrying no extension and no reference
// but its own: a member named extension or modifierExtension (read folded)
// anywhere is refused, and so is any object with a reference member but the
// subscription parameter's and each notification-event focus's and
// additional-context's valueReference, which statusNames and
// statusEventReferences judge. The leg names no patient, so nothing else in
// the answer may point at anything.
func statusOnlyItsReferences(root, p *jnode, k statusCheck) *Refusal {
	own := map[*jnode]bool{}
	for _, pm := range p.memberFolded("parameter").elems {
		switch name, _ := pm.memberFolded("name").stringOf(); name {
		case "subscription":
			own[pm.memberFolded("valueReference")] = true
		case "notification-event":
			if parts := pm.memberFolded("part"); parts != nil {
				for _, pt := range parts.elems {
					own[pt.memberFolded("valueReference")] = true
				}
			}
		}
	}
	var walk func(n *jnode) *Refusal
	walk = func(n *jnode) *Refusal {
		switch n.kind {
		case '{':
			if n.memberFolded("reference") != nil && !own[n] {
				return badAnswer(k.what + " carries a reference that is not its subscription's or its focus's")
			}
			for _, m := range n.members {
				if f := jsonmember.FoldedName(m.name); f == jsonmember.FoldedName("extension") || f == jsonmember.FoldedName("modifierExtension") {
					return badAnswer(k.what + " carries " + m.name + ", which this network does not deliver on it")
				}
				if r := walk(m.val); r != nil {
					return r
				}
			}
		case '[':
			for _, e := range n.elems {
				if r := walk(e); r != nil {
					return r
				}
			}
		}
		return nil
	}
	return walk(root)
}

// paramShape checks one parameter or part against allowed (its name's
// value[x] members), and returns its name.
func paramShape(pm *jnode, allowed map[string][]string) (string, *Refusal) {
	if pm.kind != '{' {
		return "", &Refusal{Reason: "a parameter is not an object"}
	}
	name, ok := pm.memberFolded("name").stringOf()
	values, known := allowed[name]
	if !ok || !known {
		return "", &Refusal{Reason: "a parameter is not one the profile defines"}
	}
	for _, m := range pm.members {
		f := jsonmember.FoldedName(m.name)
		switch {
		case f == jsonmember.FoldedName("name"):
		case f == jsonmember.FoldedName("id"):
			if m.val.kind != '"' {
				return "", &Refusal{Reason: "the parameter " + name + "'s id is not a string"}
			}
		case f == jsonmember.FoldedName("part") && name == "notification-event":
		case slices.ContainsFunc(values, func(v string) bool { return jsonmember.FoldedName(v) == f }):
			if f == jsonmember.FoldedName("valueReference") && !referenceShape(m.val) {
				return "", &Refusal{Reason: "a reference carries more than reference and type"}
			}
			if want := valueKind(m.name); m.val.kind != want {
				return "", &Refusal{Reason: "the parameter " + name + "'s " + m.name + " is not the JSON type its FHIR type is written as"}
			}
			if f == jsonmember.FoldedName("valueCodeableConcept") {
				if r := codeableConceptShape(m.val); r != nil {
					return "", r
				}
			}
		default:
			return "", &Refusal{Reason: "the parameter " + name + " carries " + m.name}
		}
	}
	return name, nil
}

// referenceShape reports whether n is a Reference carrying only reference and
// type, each a string.
func referenceShape(n *jnode) bool {
	if n.kind != '{' {
		return false
	}
	for _, m := range n.members {
		f := jsonmember.FoldedName(m.name)
		if (f != jsonmember.FoldedName("reference") && f != jsonmember.FoldedName("type")) || m.val.kind != '"' {
			return false
		}
	}
	return true
}

// statusEventReferences holds each notification-event's focus and
// additional-context to the topic's notification shape: a focus names a
// resource type shape.Focus lists, an additional-context one
// shape.AdditionalContext lists (none: any is refused), each read from the
// reference itself (referenceType). A reference whose type cannot be read
// one way only (a contained "#x", a urn:uuid, no reference string, a type
// member that disagrees) is unknown, never taken for an allowed one: the leg
// names no patient, so a reference to anything the topic does not declare is
// refused 502, recorded authority.
func statusEventReferences(p *jnode, shape NotificationShape) *Refusal {
	for _, pm := range p.memberFolded("parameter").elems {
		if name, _ := pm.memberFolded("name").stringOf(); name != "notification-event" {
			continue
		}
		parts := pm.memberFolded("part")
		if parts == nil {
			continue
		}
		for _, pt := range parts.elems {
			name, _ := pt.memberFolded("name").stringOf()
			var allowed []string
			switch name {
			case "focus":
				allowed = shape.Focus
			case "additional-context":
				allowed = shape.AdditionalContext
			default:
				continue
			}
			ref := pt.memberFolded("valueReference")
			if ref == nil {
				continue
			}
			t, ok := referenceType(ref)
			switch {
			case !ok:
				return badAnswer("a notification-event's " + name + " names no resource type this network can read one way only")
			case !slices.Contains(allowed, t):
				return badAnswer("a notification-event's " + name + " references a " + t + ", which the topic's notification shape does not declare there")
			}
		}
	}
	return nil
}

// fhirResourceTypeName is a FHIR resource type's name as a reference spells
// it: a capital letter, then letters.
var fhirResourceTypeName = regexp.MustCompile(`^[A-Z][A-Za-z]*$`)

// referenceType is the resource type a Reference ref names, read from its
// reference string: a relative "<Type>/<id>" or "<Type>/<id>/_history/<vid>",
// or an absolute http or https URL whose path ends in one, with no query or
// fragment; a type member, when present, must name the same type (as the
// type's name, or its canonical under http://hl7.org/fhir/StructureDefinition/).
// Anything else (no reference string, a contained "#x", a urn, an id that is
// not a FHIR id) has no type it can be read as one way only: ok is false.
func referenceType(ref *jnode) (string, bool) {
	raw, ok := ref.memberFolded("reference").stringOf()
	if !ok || raw == "" {
		return "", false
	}
	path := raw
	if strings.Contains(raw, ":") {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
			return "", false
		}
		path = strings.TrimPrefix(u.EscapedPath(), "/")
	} else if strings.ContainsAny(raw, "?#") {
		return "", false
	}
	segs := strings.Split(path, "/")
	if n := len(segs); n >= 4 && segs[n-2] == "_history" {
		if !isFHIRID(segs[n-1]) {
			return "", false
		}
		segs = segs[:n-2]
	}
	n := len(segs)
	if n < 2 || (!strings.Contains(raw, ":") && n != 2) || !fhirResourceTypeName.MatchString(segs[n-2]) || !isFHIRID(segs[n-1]) {
		return "", false
	}
	t := segs[n-2]
	if tm := ref.memberFolded("type"); tm != nil {
		v, ok := tm.stringOf()
		if !ok || (v != t && v != "http://hl7.org/fhir/StructureDefinition/"+t) {
			return "", false
		}
	}
	return t, true
}

// isFHIRID reports whether s is a FHIR id and not a dot segment (CheckID).
func isFHIRID(s string) bool { return CheckID(s) == nil }
