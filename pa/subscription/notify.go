package subscription

import (
	"net/http"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"

	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// Notification is a parsed notification Bundle (§7.3), kept as the bytes the
// notifier's system posted.
type Notification struct {
	body    []byte
	root    *jnode
	entries []*jnode
}

// Body is the notification's bytes as received (§7.3), which the route
// carries as sent. The caller must not change
// them.
func (n *Notification) Body() []byte { return n.body }

func notANotification(reason string) *Refusal {
	return refuse(http.StatusBadRequest, recording.RefusalRouting, reason)
}

// ParseNotification is the notification route's structural minimum (§7.3), a
// network rule at every level: one unambiguous JSON Bundle whose first entry's
// resource is a SubscriptionStatus Parameters naming t's canonical (with or
// without a |version) in exactly one topic parameter (valueCanonical).
// Anything else is refused 400, recorded routing: it decides which leg and
// subscriber a POST is for. It is never profile validity: the notification
// br-payer sends is not a valid R4 Bundle, and is read.
func ParseNotification(body []byte, t Topic) (*Notification, *Refusal) {
	root, err := scanJSON(body)
	if err != nil {
		return nil, notANotification("the notification is not one JSON value this network can read unambiguously")
	}
	if rt, _ := root.member("resourceType").stringOf(); root.kind != '{' || rt != "Bundle" {
		return nil, notANotification("the notification is not a Bundle")
	}
	entries := root.member("entry")
	if entries == nil || entries.kind != '[' || len(entries.elems) == 0 {
		return nil, notANotification("the notification Bundle has no entry")
	}
	status := entries.elems[0].member("resource")
	if rt, _ := status.member("resourceType").stringOf(); rt != "Parameters" {
		return nil, notANotification("the notification's first entry is not a SubscriptionStatus Parameters")
	}
	topics := 0
	named := false
	if params := status.member("parameter"); params != nil {
		for _, p := range params.elems {
			if name, _ := p.member("name").stringOf(); name == "topic" {
				topics++
				v, _ := p.member("valueCanonical").stringOf()
				named = canonicalIs(v, t.Canonical())
			}
		}
	}
	if topics != 1 || !named {
		return nil, notANotification("the notification's SubscriptionStatus does not name this address's topic " + t.Canonical())
	}
	return &Notification{body: body, root: root, entries: entries.elems}, nil
}

// NoContent is §5's no-content test, by structure: the Bundle carries no
// resource but the SubscriptionStatus (no other entry has a resource member),
// the SubscriptionStatus carries no contained resource and no
// resource-valued parameter or part at any depth, and no entry has a
// response.outcome. A handshake, a heartbeat and an id-only event pass it; a
// full-resource event does not.
//
// Above all, no object anywhere in the Bundle has a resourceType member (its
// name folded) but the root Bundle itself and the first entry's
// SubscriptionStatus Parameters, and no object anywhere has a contained
// member: a resource at any other place (a member of the root, of an entry,
// of an entry's request, of a part's valueReference, of the status's meta)
// is content, whatever the place's name.
//
// It fails closed: member names are compared under case folding, as a
// lenient reader would take them, and an entry or parameter that is not an
// object, or a parameter or part list that is present and not an array,
// counts as content, since a lenient reader may still find a resource in it.
func (n *Notification) NoContent() bool {
	status := n.entries[0].member("resource")
	if resourceBelow(n.root, n.root, status) {
		return false
	}
	for i, e := range n.entries {
		if e.kind != '{' {
			return false
		}
		if i > 0 && e.memberFolded("resource") != nil {
			return false
		}
		if r := e.memberFolded("response"); r != nil && (r.kind != '{' || r.memberFolded("outcome") != nil) {
			return false
		}
	}
	if status.memberFolded("contained") != nil {
		return false
	}
	return !partsHoldResource(status.memberFolded("parameter"))
}

// CheckShape holds the notification to the closed SubscriptionStatus Bundle
// shape an instance $status answer is held to (checkStatusBundle), as a
// notification: a history Bundle whose first entry is the SubscriptionStatus
// and whose every later entry carries no resource; every member at every
// depth one the Backport SubscriptionStatus defines, of its type, with no
// extension and no signature anywhere; each focus a resource type the
// topic's shape lists and no additional-context it does not; no reference but
// the subscription's and each focus's; and no address naming a resource type
// but the Subscription, its status, and the shape's focus types, read by path
// segment below base, the provider's FHIR base its gateway states ("" for
// none). The leg names
// no patient, so a notification outside that shape is refused, 403 on
// authority. Each place in it that can hold an address into a server is one
// E-17 re-keys (TestNotificationShapeAddressesAreE17Places).
func (n *Notification) CheckShape(shape NotificationShape, base string) *Refusal {
	k := statusCheck{what: "a notification", entry: "the notification's SubscriptionStatus", types: []string{"history"},
		addressTypes: append(slices.Clone(statusAddressTypes), shape.Focus...), notification: true, bySegment: true, base: base}
	p, r := checkStatusBundle(n.root, shape, k)
	if r != nil {
		return forbidden(strings.TrimPrefix(r.Reason, badAnswerPrefix))
	}
	// The subscription reference's type, an identity member, is held to
	// Subscription (as statusNames holds a $status answer's), as each
	// focus's is to the type its reference names (statusEventReferences).
	for _, pm := range p.memberFolded("parameter").elems {
		if name, _ := pm.memberFolded("name").stringOf(); name != "subscription" {
			continue
		}
		if tm := pm.memberFolded("valueReference").memberFolded("type"); tm != nil {
			if t, ok := tm.stringOf(); !ok || (t != "Subscription" && t != "http://hl7.org/fhir/StructureDefinition/Subscription") {
				return forbidden("a notification's subscription reference names a type other than Subscription")
			}
		}
	}
	return nil
}

// notificationIdentityMembers are the uri- and canonical-typed members the
// closed notification shape admits that name rather than locate, by path
// (lower case, "[]" for every array index), each with why. E-17 never reads
// them: each is carried as sent, never re-keyed (that would change what it
// asserts) and never refused, and recorded nowhere. Every other uri-like
// member the shape admits is a locator, one of E-17's places
// (TestNotificationShapeAddressesAreE17Places binds the two lists to the
// shape, and refuses a url or a reference here).
var notificationIdentityMembers = func() map[string]string {
	m := map[string]string{}
	const (
		rules   = "implicitRules names the rules the resource was built under; it identifies them and is never followed"
		source  = "meta.source identifies the source system (a request id may follow #); it is never followed"
		profile = "meta.profile is a profile's canonical, an identity"
		system  = "a coding's system is a code system's identity"
		topic   = "the topic parameter's canonical is held to the topic's own (statusTopic); a part carries none (paramShape)"
		refType = "a reference's type is a resource type's name or canonical, held to the type its reference names"
	)
	for _, res := range []string{"bundle", "bundle.entry[].resource"} {
		m[res+".implicitrules"], m[res+".meta.source"], m[res+".meta.profile[]"] = rules, source, profile
		m[res+".meta.security[].system"], m[res+".meta.tag[].system"] = system, system
	}
	for _, at := range []string{"bundle.entry[].resource.parameter[]", "bundle.entry[].resource.parameter[].part[]"} {
		m[at+".valuecanonical"], m[at+".valueuri"] = topic, topic
		m[at+".valuereference.type"] = refType
		m[at+".valuecodeableconcept.coding[].system"] = system
	}
	return m
}()

// ContextDeclared reports whether each part named additional-context of
// each notification-event parameter of the SubscriptionStatus references a
// resource type shape.AdditionalContext lists, read from the reference itself
// (referenceType), as an instance $status answer is held to it
// (statusEventReferences). A topic whose shape lists none (the HRex Task
// topic) admits no additional-context part, whatever it holds: the part
// names a resource beside the notification's focus, which the topic does
// not declare its notifications disclose.
//
// It reads only parameter and part lists that are arrays, and their object
// elements: it fails closed only after NoContent, which counts any other
// list or element as content. Each caller holds NoContent first.
func (n *Notification) ContextDeclared(shape NotificationShape) bool {
	status := n.entries[0].member("resource")
	params := status.memberFolded("parameter")
	if params == nil {
		return true
	}
	for _, pm := range params.elems {
		if name, _ := pm.memberFolded("name").stringOf(); name != "notification-event" {
			continue
		}
		parts := pm.memberFolded("part")
		if parts == nil {
			continue
		}
		for _, pt := range parts.elems {
			if name, _ := pt.memberFolded("name").stringOf(); name != "additional-context" {
				continue
			}
			ref := pt.memberFolded("valueReference")
			if ref == nil || len(shape.AdditionalContext) == 0 {
				return false
			}
			if t, ok := referenceType(ref); !ok || !slices.Contains(shape.AdditionalContext, t) {
				return false
			}
		}
	}
	return true
}

// resourceBelow reports whether node, or anything below it, is a resource
// other than root and status (an object with a resourceType member, its name
// folded), or holds a contained member: the any-depth reading of NoContent.
func resourceBelow(node, root, status *jnode) bool {
	if node == nil {
		return false
	}
	for _, m := range node.members {
		f := jsonmember.FoldedName(m.name)
		if f == foldedContained || (f == foldedResourceType && node != root && node != status) {
			return true
		}
		if resourceBelow(m.val, root, status) {
			return true
		}
	}
	for _, e := range node.elems {
		if resourceBelow(e, root, status) {
			return true
		}
	}
	return false
}

// partsHoldResource reports whether list, a parameter or part list, may hold
// a resource: it is present and not an array, or one of its elements is not
// an object, has a resource member, or has a part list that may.
func partsHoldResource(list *jnode) bool {
	if list == nil {
		return false
	}
	if list.kind != '[' {
		return true
	}
	for _, p := range list.elems {
		if p.kind != '{' || p.memberFolded("resource") != nil || partsHoldResource(p.memberFolded("part")) {
			return true
		}
	}
	return false
}

// Leg is the leg a notification rides (§5, §7.4): the topic's signal leg
// when n passes NoContent, and its content leg otherwise. It is "" when the
// topic has no such leg (CDex has no content leg), which the caller refuses
// 403, recorded authority.
func Leg(t Topic, n *Notification) string {
	if n.NoContent() {
		return t.SignalLeg()
	}
	return t.ContentLeg()
}

// Delivery is where the subscriber's gateway delivers a notification (§6.2
// step 5): the
// endpoint the subscriber's system wrote, and the header values it wrote.
type Delivery struct {
	Endpoint string
	Headers  []string
}

// OpenDelivery opens a notification's sealed endpoint and headers at the
// subscriber's gateway (§6.2 step 5), under b: the subscriber, the notifier
// that authenticated the leg, and the topic. Either value failing to open,
// whether altered, under a retired key, or sealed for another notifier,
// topic or endpoint, is refused 403, recorded authority, and nothing is
// delivered. The opened endpoint is then held to §6.5's rule
// (CheckEndpoint, with loop), and refused as that rule refuses it: a value
// sealed for a search (SealEndpointForSearch seals any endpoint a system
// searches for) is never delivered to unless it is an endpoint the rule
// accepts.
func (k Keys) OpenDelivery(b Binding, sealedEndpoint, sealedHeaders string, loop func(string) bool) (Delivery, *Refusal) {
	endpoint, err := k.OpenEndpoint(b, sealedEndpoint)
	if err != nil {
		return Delivery{}, forbidden("the notification's sealed endpoint does not open for this subscriber, notifier and topic")
	}
	headers, err := k.OpenHeaders(b, sealedEndpoint, sealedHeaders)
	if err != nil {
		return Delivery{}, forbidden("the notification's sealed headers do not open for this subscriber, notifier, topic and endpoint")
	}
	if r := CheckEndpoint(endpoint, loop); r != nil {
		return Delivery{}, r
	}
	return Delivery{Endpoint: endpoint, Headers: headers}, nil
}
