package engine

// relaylocate.go — finds the payer identity a request's Coverages name, and
// the exact string values the payer-identity mapping (edit E-03) replaces.
//
// The locator reads the request through relay.Doc, the same strict,
// duplicate-refusing view every other decision reads, and returns
// operations that replace single string values only. Everything else in the
// request is left to the splice, which copies it byte for byte.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// payorEdgeCarrier says where a request carries its Coverages.
type payorEdgeCarrier uint8

const (
	// payorEdgePASBundle: a prior-authorization request Bundle. Every
	// Coverage entry is mapped, and every Claim's insurer that names this
	// payer.
	payorEdgePASBundle payorEdgeCarrier = iota + 1
	// payorEdgeDTRParameters: a $questionnaire-package Parameters. Every
	// "coverage" parameter is mapped; the other parameter resources are the
	// siblings a reference may resolve to. A payer gateway sends its
	// payer's system a framed $questionnaire-package operation's own
	// Parameters on this carrier.
	payorEdgeDTRParameters
	// payorEdgeCRDRequest: a CDS Hooks request. prefetch.coverage is a bare
	// Coverage (the other non-Bundle prefetch resources are its siblings) or a
	// Bundle (every Coverage entry is mapped; only the Bundle's own entries
	// are its siblings). A reference found in neither place is refused.
	payorEdgeCRDRequest
)

// requiresCoverage reports whether a request on this carrier must name a
// payer. A questionnaire request may legitimately carry no Coverage.
func (c payorEdgeCarrier) requiresCoverage() bool {
	return c == payorEdgePASBundle || c == payorEdgeCRDRequest
}

// payorEdgeRefused is a request the payer-identity mapping refuses: the
// status and message the requester is answered with.
type payorEdgeRefused struct {
	status  int
	message string
}

func (r *payorEdgeRefused) Error() string { return r.message }

func (r *payorEdgeRefused) legResult() LegResult {
	return LegResult{Status: r.status, Message: r.message}
}

// payorEdgeMismatch is the refusal for a request whose Coverages name no
// payer this gateway can read, or a payer none of this gateway's own
// identities covers.
func payorEdgeMismatch(own []shnsdk.PayerIdentifier, got shnsdk.PayerIdentifier, gotOK bool) *payorEdgeRefused {
	lr := payorEdgeRefusal(own, got, gotOK)
	return &payorEdgeRefused{status: lr.Status, message: lr.Message}
}

// resourceSlot is a resource a reference may resolve to: its node, and the
// fullUrl, type and id it is found by.
type resourceSlot struct {
	node    relay.NodeID
	fullURL string
	typ, id string
}

// payorTarget is a resolved payer identity and the two string values that
// carry it.
type payorTarget struct {
	system, value relay.NodeID
	identity      shnsdk.PayerIdentifier
}

type partyResolution uint8

const (
	partyFound partyResolution = iota + 1
	// partyNoIdentity: nothing readable names the party (no identifier, no
	// reference, or a target that is not an Organization with an identifier).
	partyNoIdentity
	// partyNoTarget: the reference resolves to nothing in the request.
	partyNoTarget
	// partySeveralTargets: the reference resolves to more than one resource.
	partySeveralTargets
)

type payorLocator struct {
	d *relay.Document
}

// stringMember returns obj's member key when it is a string.
func (l payorLocator) stringMember(obj relay.NodeID, key string) (relay.NodeID, string, bool) {
	v, ok := l.d.Member(obj, key)
	if !ok || l.d.Kind(v) != relay.KindString {
		return 0, "", false
	}
	s, err := l.d.StringValue(v)
	if err != nil {
		return 0, "", false
	}
	return v, s, true
}

func (l payorLocator) text(obj relay.NodeID, key string) string {
	_, s, _ := l.stringMember(obj, key)
	return s
}

// objectMember returns obj's member key when it is an object.
func (l payorLocator) objectMember(obj relay.NodeID, key string) (relay.NodeID, bool) {
	v, ok := l.d.Member(obj, key)
	if !ok || l.d.Kind(v) != relay.KindObject {
		return 0, false
	}
	return v, true
}

// arrayMember returns the elements of obj's member key when it is an array.
func (l payorLocator) arrayMember(obj relay.NodeID, key string) []relay.NodeID {
	v, ok := l.d.Member(obj, key)
	if !ok {
		return nil
	}
	return l.d.Elems(v)
}

func (l payorLocator) slot(n relay.NodeID, fullURL string) resourceSlot {
	return resourceSlot{node: n, fullURL: fullURL, typ: l.text(n, "resourceType"), id: l.text(n, "id")}
}

// isResource reports whether n is an object with a resourceType.
func (l payorLocator) isResource(n relay.NodeID) bool {
	return l.d.Kind(n) == relay.KindObject && l.text(n, "resourceType") != ""
}

// bundleEntries returns the resources of Bundle b's entries.
func (l payorLocator) bundleEntries(b relay.NodeID) []resourceSlot {
	var out []resourceSlot
	for _, e := range l.arrayMember(b, "entry") {
		if l.d.Kind(e) != relay.KindObject {
			continue
		}
		r, ok := l.objectMember(e, "resource")
		if !ok {
			continue
		}
		out = append(out, l.slot(r, l.text(e, "fullUrl")))
	}
	return out
}

func ofType(slots []resourceSlot, typ string) []relay.NodeID {
	var out []relay.NodeID
	for _, s := range slots {
		if s.typ == typ {
			out = append(out, s.node)
		}
	}
	return out
}

// sites returns the Coverages to map, the Claims whose insurer to map, and
// the resources a reference in them may resolve to.
func (l payorLocator) sites(carrier payorEdgeCarrier) (coverages, claims []relay.NodeID, scope []resourceSlot) {
	root := l.d.Root()
	if l.d.Kind(root) != relay.KindObject {
		return nil, nil, nil
	}
	switch carrier {
	case payorEdgePASBundle:
		if l.text(root, "resourceType") != "Bundle" {
			return nil, nil, nil
		}
		scope = l.bundleEntries(root)
		return ofType(scope, "Coverage"), ofType(scope, "Claim"), scope
	case payorEdgeDTRParameters:
		for _, p := range l.arrayMember(root, "parameter") {
			if l.d.Kind(p) != relay.KindObject {
				continue
			}
			r, ok := l.objectMember(p, "resource")
			if !ok {
				continue
			}
			scope = append(scope, l.slot(r, ""))
			if l.text(p, "name") == "coverage" {
				coverages = append(coverages, r)
			}
		}
		return coverages, nil, scope
	case payorEdgeCRDRequest:
		pf, ok := l.objectMember(root, "prefetch")
		if !ok {
			return nil, nil, nil
		}
		c, ok := l.objectMember(pf, "coverage")
		if !ok {
			return nil, nil, nil
		}
		if l.text(c, "resourceType") == "Bundle" {
			scope = l.bundleEntries(c)
			return ofType(scope, "Coverage"), nil, scope
		}
		for _, m := range l.d.Members(pf) {
			if m.Value != c && l.isResource(m.Value) && l.text(m.Value, "resourceType") != "Bundle" {
				scope = append(scope, l.slot(m.Value, ""))
			}
		}
		return []relay.NodeID{c}, nil, scope
	}
	return nil, nil, nil
}

// identifierTarget returns the identity an Identifier object carries, when
// both its system and value are non-empty strings.
func (l payorLocator) identifierTarget(ident relay.NodeID) (payorTarget, bool) {
	if l.d.Kind(ident) != relay.KindObject {
		return payorTarget{}, false
	}
	sn, sys, sok := l.stringMember(ident, "system")
	vn, val, vok := l.stringMember(ident, "value")
	if !sok || !vok || sys == "" || val == "" {
		return payorTarget{}, false
	}
	return payorTarget{system: sn, value: vn, identity: shnsdk.PayerIdentifier{System: sys, Value: val}}, true
}

// party resolves a Reference element (a Coverage's payor, a Claim's
// insurer) owned by resource owner: its inline identifier first; otherwise
// the Organization its reference names, found among owner's contained
// resources ("#id") or among scope (by an exactly equal fullUrl, or a
// reference exactly "Type/id"). A version-specific (_history) or otherwise
// differently spelled reference is not resolved, so it is refused rather
// than guessed. The Organization's identity is its first identifier with a
// non-empty system and value. ref is the reference text, for a refusal to
// name.
func (l payorLocator) party(el, owner relay.NodeID, scope []resourceSlot) (t payorTarget, res partyResolution, ref string) {
	if l.d.Kind(el) != relay.KindObject {
		return payorTarget{}, partyNoIdentity, ""
	}
	if ident, ok := l.d.Member(el, "identifier"); ok {
		if inline, ok := l.identifierTarget(ident); ok {
			return inline, partyFound, ""
		}
	}
	ref = l.text(el, "reference")
	if ref == "" {
		return payorTarget{}, partyNoIdentity, ""
	}
	var matches []relay.NodeID
	if id, ok := strings.CutPrefix(ref, "#"); ok {
		for _, c := range l.arrayMember(owner, "contained") {
			if l.isResource(c) && id != "" && l.text(c, "id") == id {
				matches = append(matches, c)
			}
		}
	} else {
		for _, s := range scope {
			if s.node == owner {
				continue
			}
			if (s.fullURL != "" && s.fullURL == ref) || (s.typ != "" && s.id != "" && ref == s.typ+"/"+s.id) {
				matches = append(matches, s.node)
			}
		}
	}
	switch len(matches) {
	case 0:
		return payorTarget{}, partyNoTarget, ref
	case 1:
	default:
		return payorTarget{}, partySeveralTargets, ref
	}
	org := matches[0]
	if l.text(org, "resourceType") != "Organization" {
		return payorTarget{}, partyNoIdentity, ref
	}
	for _, ident := range l.arrayMember(org, "identifier") {
		if first, ok := l.identifierTarget(ident); ok {
			return first, partyFound, ref
		}
	}
	return payorTarget{}, partyNoIdentity, ref
}

func referenceRefusal(ref string, res partyResolution) *payorEdgeRefused {
	what := "no resource in the request"
	if res == partySeveralTargets {
		what = "more than one resource in the request"
	}
	return &payorEdgeRefused{
		status: http.StatusUnprocessableEntity,
		message: fmt.Sprintf("payer backend identity mapping: reference %q resolves to %s; "+
			"refusing rather than guessing which payer it names", ref, what),
	}
}

// locatePayorEdge returns the operations that map the payer identity in the
// request d to backend, or a *payorEdgeRefused. own is the set of identities
// this gateway owns (payoredge.go, ownPayerIdentities) — a payer publishes
// several on the network feed, and a request routed here on any one of them
// is a request this payer owns.
//
// Every Coverage's routing payor (payor[0], the one payer routing reads)
// must name exactly one payer identity, and it must be one of own: a
// Coverage naming no readable payer, or a payer none of own covers, is
// refused, and so is a request whose Coverages name different payers. A
// reference that resolves to no resource, or to several, is refused naming
// the reference.
//
// A Claim's insurer is resolved the same way, and mapped when it names own.
// An insurer that names another identity (a payer may name itself by NPI on
// the insurer and by its payer id on the Coverage), or no readable identity,
// is left as sent: which payer answers is decided on the Coverages, so the
// insurer's mapping is not what addresses the payer's system. An insurer
// reference that resolves to no resource or to several is the request's own
// content (RuleInsurer), decided by refusesInsurer: when it refuses, the
// request is refused naming the reference; when it does not, that insurer is
// left as sent like one naming no readable identity, and the Coverages are
// mapped as usual. refusesInsurer nil refuses. Only string values that differ
// from backend are replaced, so a mapping to the identity the request already
// carries makes no edit.
func locatePayorEdge(d *relay.Document, carrier payorEdgeCarrier, own []shnsdk.PayerIdentifier, backend shnsdk.PayerIdentifier, refusesInsurer func() bool) ([]relay.Op, error) {
	l := payorLocator{d: d}
	coverages, claims, scope := l.sites(carrier)
	if len(coverages) == 0 {
		if carrier.requiresCoverage() {
			return nil, payorEdgeMismatch(own, shnsdk.PayerIdentifier{}, false)
		}
		return nil, nil
	}
	var targets []payorTarget
	for _, c := range coverages {
		payors := l.arrayMember(c, "payor")
		if len(payors) == 0 {
			return nil, payorEdgeMismatch(own, shnsdk.PayerIdentifier{}, false)
		}
		t, res, ref := l.party(payors[0], c, scope)
		switch res {
		case partyFound:
			targets = append(targets, t)
		case partyNoTarget, partySeveralTargets:
			return nil, referenceRefusal(ref, res)
		default:
			return nil, payorEdgeMismatch(own, shnsdk.PayerIdentifier{}, false)
		}
	}
	for _, t := range targets[1:] {
		if t.identity != targets[0].identity {
			a, b := targets[0].identity, t.identity
			return nil, &payorEdgeRefused{
				status: http.StatusUnprocessableEntity,
				message: fmt.Sprintf("payer backend identity mapping: the request's Coverages name more than one payer "+
					"(%s|%s and %s|%s); refusing rather than adjudicating for several payers", a.System, a.Value, b.System, b.Value),
			}
		}
	}
	if got := targets[0].identity; !slices.Contains(own, got) {
		return nil, payorEdgeMismatch(own, got, true)
	}
	for _, c := range claims {
		ins, ok := l.d.Member(c, "insurer")
		if !ok {
			continue
		}
		t, res, ref := l.party(ins, c, scope)
		switch {
		case res == partyNoTarget || res == partySeveralTargets:
			if refusesInsurer == nil || refusesInsurer() {
				return nil, referenceRefusal(ref, res)
			}
		case res == partyFound && slices.Contains(own, t.identity):
			targets = append(targets, t)
		}
	}
	sys, err := json.Marshal(backend.System)
	if err != nil {
		return nil, err
	}
	val, err := json.Marshal(backend.Value)
	if err != nil {
		return nil, err
	}
	var ops []relay.Op
	seen := map[relay.NodeID]bool{}
	replace := func(n relay.NodeID, current, next string, value []byte) {
		if current == next || seen[n] {
			return
		}
		seen[n] = true
		ops = append(ops, d.Replace(n, value))
	}
	for _, t := range targets {
		replace(t.system, t.identity.System, backend.System, sys)
		replace(t.value, t.identity.Value, backend.Value, val)
	}
	return ops, nil
}
