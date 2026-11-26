package relay

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"

// A CDex Task notification's addresses into the provider's server (E-17): the
// places the payer's gateway re-keys to its own base
// for that provider, decided by structure.

// notificationAddressValue is E-17's place, for its signed-content exemption:
// n is the string value of
//   - a root Bundle's entry's fullUrl;
//   - a root Bundle's entry's request.url;
//   - a root Bundle's entry's response.location;
//   - the url of a root Bundle's link, or of a root Bundle's entry's link;
//   - in the resource of the root Bundle's first entry, a Parameters (the
//     SubscriptionStatus), the valueReference.reference of a parameter named
//     subscription, or of a part named focus of a parameter named
//     notification-event.
//
// Member names are compared as the relay's strict decoder folds them
// (memberIs), as E-20's fullUrl is. Nothing else is such a place.
func (m *model) notificationAddressValue(n splice.NodeID) bool {
	d := m.doc
	parent := m.parent[n]
	if parent < 0 || d.Kind(n) != splice.KindString {
		return false
	}
	// entry[i].fullUrl
	if m.bundleEntry(parent) && memberIs(d, parent, n, "fullUrl", true) {
		return true
	}
	// entry[i].request.url
	if memberIs(d, parent, n, "url", true) {
		if entry := m.parent[parent]; entry >= 0 && m.bundleEntry(entry) && memberIs(d, entry, parent, "request", true) && d.Kind(parent) == splice.KindObject {
			return true
		}
	}
	// entry[i].response.location
	if memberIs(d, parent, n, "location", true) && d.Kind(parent) == splice.KindObject {
		if entry := m.parent[parent]; entry >= 0 && m.bundleEntry(entry) && memberIs(d, entry, parent, "response", true) {
			return true
		}
	}
	// link[i].url, of the root Bundle or of one of its entries
	if memberIs(d, parent, n, "url", true) && (m.bundleLink(parent) || m.entryLink(parent)) {
		return true
	}
	// ….valueReference.reference of the SubscriptionStatus's subscription
	// parameter, or of its notification-event's focus part.
	if !memberIs(d, parent, n, "reference", true) || d.Kind(parent) != splice.KindObject {
		return false
	}
	holder := m.parent[parent] // the parameter or part
	if holder < 0 || !memberIs(d, holder, parent, "valueReference", true) {
		return false
	}
	switch foldedString(d, holder, "name") {
	case "subscription":
		return m.statusParameter(holder)
	case "focus":
		parts := m.parent[holder]
		if parts < 0 || d.Kind(parts) != splice.KindArray {
			return false
		}
		return m.eventFocusParts(parts)
	}
	return false
}

// entryLink reports whether l is an element of the link array of a root
// Bundle's entry, decided once per array (entryLinks): every link url an E-17
// edit names in one entry shares it, so the entry's members are read once,
// not once per edit.
func (m *model) entryLink(l splice.NodeID) bool {
	d := m.doc
	links := m.parent[l]
	if links < 0 || d.Kind(l) != splice.KindObject || d.Kind(links) != splice.KindArray {
		return false
	}
	if ok, seen := m.entryLinks[links]; seen {
		return ok
	}
	entry := m.parent[links]
	ok := entry >= 0 && m.bundleEntry(entry) && memberIs(d, entry, links, "link", true)
	if m.entryLinks == nil {
		m.entryLinks = map[splice.NodeID]bool{}
	}
	m.entryLinks[links] = ok
	return ok
}

// eventFocusParts reports whether parts is the part array of the
// SubscriptionStatus's notification-event parameter, decided once per array
// (eventParts): every focus part an E-17 edit names shares it, so the event's
// members are read once, not once per edit.
func (m *model) eventFocusParts(parts splice.NodeID) bool {
	if ok, seen := m.eventParts[parts]; seen {
		return ok
	}
	d := m.doc
	event := m.parent[parts]
	ok := event >= 0 && memberIs(d, event, parts, "part", true) && foldedString(d, event, "name") == "notification-event" && m.statusParameter(event)
	if m.eventParts == nil {
		m.eventParts = map[splice.NodeID]bool{}
	}
	m.eventParts[parts] = ok
	return ok
}

// statusParameter reports whether p is an element of the parameter array of
// the SubscriptionStatus: the Parameters that is the resource of the root
// Bundle's first entry.
func (m *model) statusParameter(p splice.NodeID) bool {
	d := m.doc
	params := m.parent[p]
	if params < 0 || d.Kind(p) != splice.KindObject || d.Kind(params) != splice.KindArray {
		return false
	}
	return m.statusParams(params)
}

// statusParams reports whether params is the parameter array of the
// SubscriptionStatus, decided once per array (statusLists): the Parameters'
// members and the root Bundle's entries are read once, not once per edit.
func (m *model) statusParams(params splice.NodeID) bool {
	if ok, seen := m.statusLists[params]; seen {
		return ok
	}
	ok := m.isStatusParams(params)
	if m.statusLists == nil {
		m.statusLists = map[splice.NodeID]bool{}
	}
	m.statusLists[params] = ok
	return ok
}

func (m *model) isStatusParams(params splice.NodeID) bool {
	d := m.doc
	status := m.parent[params]
	if status < 0 || !memberIs(d, status, params, "parameter", true) || foldedString(d, status, "resourceType") != "Parameters" {
		return false
	}
	entry := m.parent[status]
	if entry < 0 || !m.bundleEntry(entry) || !memberIs(d, entry, status, "resource", true) {
		return false
	}
	entries := m.parent[entry]
	elems := d.Elems(entries)
	return len(elems) > 0 && elems[0] == entry
}
