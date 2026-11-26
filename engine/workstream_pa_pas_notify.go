//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
	"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"
	"github.com/SmartHealthNetwork/shn-gateway/pa/pastopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PAS topic's content notifications (pas-subscription-notify): a payer's own system posts a
// full-resource notification of a provider's Subscription (its decision, a
// PAS Response Bundle) to the notification address its gateway wrote, as it
// posts every notification of that Subscription; the payer's gateway sends it
// on the topic's notify leg, which names a patient, and the provider's
// gateway delivers it to the endpoint the provider's own system wrote, with
// E-22 made. Every resource the notification carries is read, wherever a FHIR
// reader could take one, at any depth, its member names and resourceType in
// any case (subscription.Notification.Resources), and held to these rules as
// the decision is; the notification is carried as sent. Every rule is a
// network rule, the same at every conformance level, unless it names a
// conformance rule:
//   - the notification route's rules on every notification of the topic
//     (handleNotifyIngress, workstream_pa_pas_signal.go): the credential, a
//     notification of the PAS topic, no additional-context;
//   - the receiver (N6, pastopic.Topic.CheckContent): every ClaimResponse,
//     wherever it sits, names an applicationReceiverCode attested for the
//     subscriber, or the notification is refused 403 on authority, an absent
//     code as a mismatched one, and so is one whose resources cannot be read;
//   - the patient: each decision's ClaimResponses' patients, read by
//     the identity rules every PAS answer is read by (pasgraph.ReadNotified),
//     the leg token's patient the first decision's first, bound by the member
//     it names (resolveSubjectPCI) from the Patient entry it resolves to when
//     that Patient carries demographics, and from the member alone otherwise
//     (bindPASNotified), and every other ClaimResponse's patient, wherever it
//     sits, read from its patient reference. A notification whose
//     patient cannot be identified is refused 422 on authority; one larger
//     than the gateway reads within its own limit is refused 422 on authority
//     too, as one it cannot bind within that limit;
//   - E-22, made by the provider's gateway on every notification the leg
//     delivers: one relay cannot read one way only is refused 400 on the
//     fidelity of SHN's own edit before it is sent (readNotified), as the
//     provider's gateway refuses it, 400, before it delivers;
//   - each decision that is a PAS Response Bundle is held to the response
//     graph rule a PAS answer is read by, at the PAS lines this payer
//     declares its system speaks, or at any line it could be on when it
//     declares none
//     (pasgraph.ValidateNotified); one that meets none of them is carried,
//     and refused 422 at the levels that refuse answer.shape (structural and
//     strict), the refusal naming each line and its failure;
//   - a notification naming more than one patient (pasNotifiedMixed) is
//     carried, the others on the involved list (an exchange is recorded under
//     every patient involved; pasNotifiedMembers), and
//     refused 422 at the levels that refuse patient.mixed (strict).

// legPASSubscriptionNotify is the PAS topic's notify (content) leg.
const legPASSubscriptionNotify = pastopic.ContentLeg

// The PAS notification route's refusals of a content notification.
const (
	// errPASNotifyNotSubscribers opens the refusal of a content
	// notification the receiver check refuses (N6); why follows.
	errPASNotifyNotSubscribers = "the notification's decision is not addressed to the subscriber this address is for (per-leg authority); nothing was sent: "
	// errPASNotifyNoPatient opens the refusal of a content notification
	// whose patient cannot be identified; why follows.
	errPASNotifyNoPatient = "cannot identify the patient the notified decision is about (per-leg authority); nothing was sent: "
	// errPASNotifyOverLimit: a notified decision larger than the gateway
	// reads within its own limit.
	errPASNotifyOverLimit = "cannot bind the notified decision's patient within the gateway's limit (per-leg authority); nothing was sent"
	// errPASNotifyMixed: a notified decision naming more than one patient,
	// at a level that refuses patient.mixed.
	errPASNotifyMixed = "the notified decision names more than one patient (patient.mixed); nothing was sent"
	// errPASNotifyUnreadable: a content notification the provider's
	// gateway could not make E-22 on, since it cannot be read one way only.
	errPASNotifyUnreadable = "the notification cannot be read one way only (JSON the network cannot read unambiguously, such as a lone surrogate in a string), so the provider's gateway could not make its edit of the documentation return address (fidelity of SHN's own edit); nothing was sent"
	// errPASNotifyAnswerDefect opens the refusal of a notified decision with
	// a defect that does not touch its patient's binding, at a level that
	// refuses answer.shape; the defect follows.
	errPASNotifyAnswerDefect = "the notified decision is outside the shape a PAS answer is read by (answer.shape); nothing was sent: "
)

// pasNotifierContent is the PAS topic's content check and patient binding,
// on a payer's gateway.
var pasNotifierContent = &notifierContent{
	subscriber: func(g *Gateway, provider string) subscription.Holder {
		return providerHolder(provider, g.attestedProviderIDs(provider))
	},
	notSubscribers: reasonPASNotifyNotSubscribers,
	bind:           (*Gateway).bindPASNotified,
	readable:       readNotified,
	unreadable:     errPASNotifyUnreadable,
}

// pasNotified is a PAS content notification as its binding and its involved
// list read it: every resource it carries, wherever it sits
// (subscription.Notification.Resources), its decisions, and every
// ClaimResponse's patient.
type pasNotified struct {
	resources []subscription.Resource
	// decisions are the resources of the entries after the
	// SubscriptionStatus, each read for its patients (pasgraph.ReadNotified).
	decisions []pasNotifiedDecision
	// first is the leg token's patient, the first decision's first
	// ClaimResponse's, and firstRaw the decision that names it.
	first      pasgraph.NotifiedPatient
	firstRaw   []byte
	firstGraph *pasgraph.Graph
	// body is the notification as posted.
	body []byte
}

type pasNotifiedDecision struct {
	raw   []byte
	d     *pasgraph.Notified
	outer int
}

// readPASNotified reads n by the rules above, or why it is refused: a
// notification whose resources cannot be read (403 on authority, as N6
// refuses it first), a decision over the gateway's limit, and a decision or
// a ClaimResponse anywhere whose patient cannot be identified (each 422 on
// authority).
func readPASNotified(n *subscription.Notification) (*pasNotified, *notifierRefusal) {
	resources, rf := n.Resources()
	if rf != nil {
		return nil, &notifierRefusal{rule: RefusalAuthority, status: http.StatusForbidden, msg: errPASNotifyNotSubscribers + rf.Reason, reason: reasonPASNotifyNotSubscribers}
	}
	noPatient := func(why string) *notifierRefusal {
		return &notifierRefusal{rule: RefusalAuthority, status: http.StatusUnprocessableEntity, msg: errPASNotifyNoPatient + why, reason: reasonPASNotifyNoPatient}
	}
	p := &pasNotified{resources: resources, body: n.Body()}
	for i, r := range resources {
		// Every ClaimResponse, wherever it sits, names a patient the network
		// can identify: a Patient/<id>, or a urn naming a Patient in its
		// Bundle's scope (urnScopedPatient).
		if r.TypeIs("ClaimResponse") {
			ref, ok := notifiedPatientRef(r.Raw)
			identity := notifiedIdentity(r.EntryFullURL, ref, "")
			named := pasgraph.PatientTail(identity) != "" || (isURN(identity) && urnScopedPatient(resources, i, identity) >= 0)
			if !ok || !named {
				return nil, noPatient("a ClaimResponse it carries names no patient by a reference the network can identify")
			}
		}
		if !r.Entry {
			continue
		}
		d, err := pasgraph.ReadNotifiedAt(r.Raw, r.FullURL)
		switch {
		case errors.Is(err, pasgraph.ErrOverLimit):
			return nil, &notifierRefusal{rule: RefusalAuthority, status: http.StatusUnprocessableEntity, msg: errPASNotifyOverLimit, reason: reasonPASNotifyOverLimit}
		case err != nil:
			return nil, noPatient(err.Error())
		}
		if p.firstRaw == nil && len(d.Patients) > 0 {
			p.first, p.firstRaw, p.firstGraph = d.Patients[0], r.Raw, d.Graph
		}
		p.decisions = append(p.decisions, pasNotifiedDecision{raw: r.Raw, d: d, outer: i})
	}
	if p.firstRaw == nil {
		return nil, noPatient("it carries no ClaimResponse as its decision")
	}
	return p, nil
}

// notifiedPatientRef is a ClaimResponse's patient reference, its member
// names compared under case folding, as a lenient reader takes them; ok is
// false when it names none by a string.
func notifiedPatientRef(raw []byte) (string, bool) {
	var cr map[string]any
	if pasgraph.DecodeObject(raw, &cr) != nil {
		return "", false
	}
	patient, _ := pasgraph.FoldedMember(cr, "patient").(map[string]any)
	ref, ok := pasgraph.FoldedMember(patient, "reference").(string)
	return ref, ok && ref != ""
}

// notifiedIdentity is the identity the patient reference ref denotes, by
// FHIR R4's Bundle reference rule through the shared resolver
// (pasgraph.Graph.SubjectIdentity): written in an entry whose fullUrl (entryURL, the
// nearest entry it sits in) is RESTful, a relative reference resolves against
// that entry's base; in an entry whose fullUrl is a URN or that states none,
// or outside any entry, against the payer's stated base (base, the server
// the notification came from). An absolute reference is itself; a relative
// one with no base to resolve against, its own text. Without a version.
func notifiedIdentity(entryURL, ref, base string) string {
	ref = pasgraph.StripHistoryRef(ref)
	if u, err := url.Parse(ref); err != nil || u.IsAbs() || strings.HasPrefix(ref, "#") {
		return ref
	}
	if u, err := url.Parse(entryURL); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		// The entry's resource, as its fullUrl names it: the owner the
		// shared resolver reads the base from.
		segs := strings.Split(strings.TrimRight(u.Path, "/"), "/")
		if len(segs) >= 2 {
			g := &pasgraph.Graph{ByURL: map[string]*pasgraph.Entry{}}
			owner := &pasgraph.Entry{FullURL: entryURL, Resource: map[string]any{"resourceType": segs[len(segs)-2], "id": segs[len(segs)-1]}}
			if identity, why := g.SubjectIdentity(owner, ref); why == "" {
				return identity
			}
		}
	}
	if base != "" {
		return strings.TrimRight(base, "/") + "/" + ref
	}
	return ref
}

// graphIdentity is the identity the patient reference ref denotes, written
// in a resource of a decision whose graph is g (nil for a resource outside
// any decision) and that sits in the entry entryURL: inside the graph, by
// the graph's own rule, the PAS answer's (pasgraph.Graph.SubjectIdentity
// with the real owner entry: the graph entry at entryURL, or the bare
// ClaimResponse a decision is, at its notification entry's fullUrl (bareURL)
// when that is its RESTful identity (pasgraph.BareFullURL), as
// pasgraph.ReadNotifiedAt binds it; a relative reference under a URN entry
// resolving to the one entry whose RESTful identity ends in it), as the
// leg's own patient is bound (pasgraph.ReadNotified); where the graph
// resolves nothing, by the Patient/<id> it names, resolved against the
// stated base as the leg's is; and outside any graph (or an entry the graph
// does not hold, as in a nested Bundle), by notifiedIdentity's rule.
func graphIdentity(g *pasgraph.Graph, entryURL, bareURL, ref, base string) string {
	var owner *pasgraph.Entry
	switch {
	case g == nil:
	case g.Bundle["resourceType"] == "ClaimResponse":
		owner = &pasgraph.Entry{Resource: g.Bundle, FullURL: pasgraph.BareFullURL(g.Bundle, bareURL)}
	default:
		owner = g.ByURL[entryURL]
	}
	if owner == nil {
		return notifiedIdentity(entryURL, ref, base)
	}
	if identity, why := g.SubjectIdentity(owner, pasgraph.StripHistoryRef(ref)); why == "" {
		return identity
	}
	if tail := pasgraph.PatientTail(ref); tail != "" {
		return notifiedIdentity("", tail, base)
	}
	return pasgraph.StripHistoryRef(ref)
}

// notifiedPatientKey is the patient an identity (notifiedIdentity) names, for
// counting patients: the identity as the shared rule compares entries
// (entryKey: scheme and host in any case, a default port), or, for one that
// resolved against no base, its Patient/<id>.
func notifiedPatientKey(identity string) string {
	identity = pasgraph.StripHistoryRef(identity)
	if u, err := url.Parse(identity); err == nil && u.IsAbs() {
		return entryKey(identity)
	}
	if tail := pasgraph.PatientTail(identity); tail != "" {
		return tail
	}
	return identity
}

// bindPASNotified is a PAS content notification's patients, by the rules
// above: the leg token's patient, or why the notification is refused (the
// involved list names every other, read from the request by
// requestMembers). body is the notification as posted, and base the payer's
// FHIR base its gateway states.
func (g *Gateway) bindPASNotified(ctx context.Context, n *subscription.Notification, body []byte, base string) (string, *notifierRefusal) {
	p, nr := readPASNotified(n)
	if nr != nil {
		return "", nr
	}
	// An entry the network could not read must not bear on any patient's
	// binding: each member binds the same from the decision as sent and from
	// the entries read cleanly, by the shared rule's own reader (its
	// demographics, and whether the member's number is a Coverage party's),
	// or the decision is refused, since what the binding would read there is
	// unknown, and unknown is not empty.
	for _, dec := range p.decisions {
		if len(dec.d.Defects) == 0 {
			continue
		}
		for _, pt := range dec.d.Patients {
			member := pt.Member()
			cleanPCI, _, cleanErr := g.resolveSubjectPCI(ctx, member, dec.d.Scope())
			rawPCI, _, rawErr := g.resolveSubjectPCI(ctx, member, dec.raw)
			for _, err := range []error{cleanErr, rawErr} {
				if err != nil && !errors.Is(err, errPatientUnidentifiable) {
					status, msg := subjectBindingFailure(ctx, err)
					return "", &notifierRefusal{status: status, msg: msg, recorded: true}
				}
			}
			if cleanPCI != rawPCI || errors.Is(cleanErr, errPatientUnidentifiable) != errors.Is(rawErr, errPatientUnidentifiable) {
				return "", &notifierRefusal{rule: RefusalAuthority, status: http.StatusUnprocessableEntity,
					msg: errPASNotifyNoPatient + "an entry the network could not read bears on the binding of member " + member, reason: reasonPASNotifyNoPatient}
			}
		}
	}
	// The leg's patient is the member its ClaimResponse names, bound by the
	// shared rule in its own order (resolveSubjectBinding): the payer's own
	// member when its system of record holds it (and, for a participant that
	// requires known members, refused as unknown when it does not); otherwise
	// the identifier derived from the member and the demographics of the
	// Patient entry the ClaimResponse's patient reference resolves to, or the
	// one urn-written Patient of its decision at the stated base with the id
	// it names (two such are neither), when it resolves to one that carries
	// them (pasNotifiedOwn); and otherwise,
	// when it resolves to a Patient that carries none or to no Patient
	// entry, from the member id alone, the only identity the decision states
	// for its patient. Only in that last case is the notification as a whole
	// read, through its canonicalCopy,
	// by the shared rule's reader (carriesMembersParty), and only for whether
	// the member's number is a Coverage party's, which refuses the decision
	// 422 (a patient the network cannot identify), as the shared rule refuses
	// a derived binding with no demographics. Another Patient under the
	// member's id, anywhere in the notification, never decides the binding:
	// it is named on the involved list as its own identity
	// (pasNotifiedIdentities).
	unreadable := &notifierRefusal{rule: RefusalAuthority, status: http.StatusUnprocessableEntity,
		msg: errPASNotifyNoPatient + "it cannot be read for its patients one way only", reason: reasonPASNotifyNoPatient}
	member := p.first.Member()
	scope, demographics := memberOnlyScope, false
	// A copy of the own Patient is always made for an entry the graph read;
	// pasNotifiedIdentities, below, fails closed if one ever is not.
	own, _, _ := pasNotifiedOwn(p, base)
	// The walk read the body as one JSON document, so a copy of it is
	// always made.
	whole := canonicalCopy(p.body)
	if own != nil {
		if _, ok := carriedDemographics(own, member, false); ok {
			scope, demographics = own, true
		}
	}
	pci, held, found, readErr := g.resolveSubjectBinding(ctx, member, scope)
	if errors.Is(readErr, errPatientUnidentifiable) {
		return "", &notifierRefusal{rule: RefusalAuthority, status: http.StatusUnprocessableEntity, msg: msgPatientUnidentifiable, reason: reasonPatientUnidentifiable}
	}
	if readErr != nil {
		status, msg := subjectBindingFailure(ctx, readErr)
		return "", &notifierRefusal{status: status, msg: msg, recorded: true}
	}
	if found && !held && !demographics && carriesMembersParty(whole, member, false) {
		return "", &notifierRefusal{rule: RefusalAuthority, status: http.StatusUnprocessableEntity, msg: msgPatientUnidentifiable, reason: reasonPatientUnidentifiable}
	}
	if !found {
		// Only a participant that requires known members gets here: a check
		// it opted into, recorded as conformance.
		return "", &notifierRefusal{rule: RefusalConformance, status: http.StatusBadRequest, msg: "unknown member"}
	}
	// Each decision that is a PAS Response Bundle is held to the response
	// graph rule a PAS answer is read by, at the PAS lines this payer
	// declares its system speaks, or at any line it could be on when it
	// declares none,
	// since a notification states none (pasgraph.ValidateNotified); one that
	// meets none of them is outside the shape a PAS answer is read by
	// (answer.shape).
	var defect error
	declared := g.notifiedPASLines()
	lines := make([]string, len(p.decisions))
	for i, dec := range p.decisions {
		if dec.d.Graph.Bundle["resourceType"] != "Bundle" {
			continue
		}
		line, err := pasgraph.ValidateNotified(dec.raw, declared)
		if err != nil && defect == nil {
			defect = err
		}
		lines[i] = line
	}
	if defect != nil && g.guard(ctx, KindContent, RuleAnswerShape, body) {
		return "", &notifierRefusal{status: http.StatusUnprocessableEntity, msg: errPASNotifyAnswerDefect + strings.TrimPrefix(defect.Error(), "engine: "), recorded: true}
	}
	identities, distinct, ok := pasNotifiedIdentities(p, base)
	if !ok {
		return "", unreadable
	}
	mixed, unavailable := g.pasNotifiedMixed(ctx, p, identities, distinct, lines, pci)
	if mixed && g.guard(ctx, KindContent, RulePatientMixed, body) {
		return "", &notifierRefusal{status: http.StatusUnprocessableEntity, msg: errPASNotifyMixed, recorded: true}
	}
	// The check could not finish: where it refuses, the refusal is the
	// system of record's own failure (subjectBindingFailure), never a claim
	// that the notification names a second patient.
	if unavailable != nil && g.guardUnavailable(ctx, KindContent, RulePatientMixed, body) {
		status, msg := subjectBindingFailure(ctx, unavailable)
		return "", &notifierRefusal{status: status, msg: msg, recorded: true}
	}
	return pci, nil
}

// notifiedPASLines are the PAS IG lines this payer's participant declares
// its own system speaks (PAYER_DAVINCI_CONTRACT_VERSIONS, the forward's
// declared set: WithDeclaredContractVersions), as the response graph rule
// names them ("2.0" is 2.0.1, as the validator reads the line: pasVersions in
// gateway/deploy/validator), in pasgraph.NotifiedLines' order; none when it
// declares no PAS line, or the gateway forwards to no system of its own (it
// then holds a decision at any line it could be on). The gateway's own
// declaration (Config.DeclaredContractVersions) is not read: it always holds
// a set, this build's default when the operator states none, so it cannot
// tell a payer that declares no line from one that declares the default.
func (g *Gateway) notifiedPASLines() []string {
	n, ok := g.cfg.Responder.(*nativeResponder)
	if !ok {
		return nil
	}
	set := contractLineSet(n.declaredContractVersions, "pa.pas")
	if set["2.0"] {
		set["2.0.1"] = true
	}
	var out []string
	for _, l := range pasgraph.NotifiedLines {
		if set[l] {
			out = append(out, l)
		}
	}
	return out
}

// pasNotifiedMixed reports whether p names more than one patient, leg the
// leg's own patient as bound:
//   - any reference names another identity than the leg's (distinct:
//     pasNotifiedIdentities, each resolved by FHIR R4's Bundle rule);
//   - any identity the involved list names (pasNotifiedIdentities, the one
//     reading both use) but the leg's own binds otherwise than the leg's
//     patient, or cannot be bound: another patient;
//   - or a decision read at an IG line (lines, "" for one not read at any)
//     names a subject that is not its ClaimResponse's patient, by the subject
//     rule a PAS answer is held to at that line (pasAnswerSubjectMismatch).
//
// An identity that cannot be bound because the payer's own system of record
// could not answer for it is no evidence of a second patient: when nothing
// above shows one, that failure is unavailable, the check could not finish
// (the caller refuses with it where the check refuses, as the CRD legs do).
// An identity the notification itself makes unidentifiable or unreadable is
// another patient.
func (g *Gateway) pasNotifiedMixed(ctx context.Context, p *pasNotified, identities []namedMember, distinct bool, lines []string, leg string) (mixed bool, unavailable error) {
	if distinct {
		return true, nil
	}
	for _, m := range identities {
		if m.subject {
			continue
		}
		pci, _, found, err := g.resolveBinding(ctx, m.member, m.scope, true)
		// Each scope here is one carried Patient's canonical copy, which
		// decodes and is no Coverage's party, so the binding gives neither
		// errPatientUnidentifiable nor errRequestPatientsUnreadable today; were
		// it to, the notification itself would have made the identity
		// unbindable, another patient, so those stay mixed (a safeguard).
		switch {
		case err != nil && !notProvablyThePatient(err) && !errors.Is(err, errRequestPatientsUnreadable):
			if unavailable == nil {
				unavailable = err
			}
		case err != nil || !found || pci != leg:
			return true, nil
		}
	}
	for i, dec := range p.decisions {
		if lines[i] != "" && pasAnswerSubjectMismatch(dec.raw, lines[i]) != nil {
			return true, nil
		}
	}
	return false, unavailable
}

// notificationOwn is the notification with each outer resource read as
// empty: its own members alone (the SubscriptionStatus's parameters and
// parts, an entry's own members, the Bundle's), each outer resource being
// read on its own.
func notificationOwn(p *pasNotified) []byte {
	own := make([]byte, 0, len(p.body))
	at := 0
	for i, r := range p.resources {
		if !r.IsOuter(i) {
			continue
		}
		own = append(own, p.body[at:r.At]...)
		own = append(own, "{}"...)
		at = r.At + len(r.Raw)
	}
	return append(own, p.body[at:]...)
}

// memberOnlyScope is the bytes a member is bound from by its id alone: they
// carry no Patient.
var memberOnlyScope = []byte(`{}`)

// copyNames are the member names this leg's own readers read, as FHIR
// spells them: every name the shared view renames (viewNames: the names the
// shared binding readers read, so a resource this file compares with one the
// view gives back reads the same), and the names of the references the
// notification's walk follows besides (a Coverage's payor, a resource's
// patient, a reference's type). Demographics (birthDate, name, family) are
// not among them: they are read as every leg's binding reads them. copyTypes
// are the resource types those readers test for.
var copyNames, copyTypes = func() (map[string]string, map[string]string) {
	names, types := map[string]string{}, map[string]string{}
	for _, n := range slices.Concat(viewNames, []string{"payor", "patient", "type"}) {
		names[jsonmember.FoldedName(n)] = n
	}
	for _, t := range []string{"Patient", "Coverage", "Bundle", "RelatedPerson", "Organization"} {
		types[jsonmember.FoldedName(t)] = t
	}
	return names, types
}()

// canonicalCopy is raw, one JSON value, with each of copyNames spelled as
// FHIR spells it, and each resourceType of copyTypes spelled as FHIR spells
// it, compared under case folding, at any depth: the copy this leg's own
// readers, which read names exactly, read as a folding reader reads raw; the
// shared readers it is passed to read it through their own view, which it
// already agrees with. raw names no member twice under folding (the
// notification route refuses one that does), so no two members fold to one.
// nil when raw cannot be read.
func canonicalCopy(raw []byte) []byte {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return nil
	}
	var canon func(any) any
	canon = func(node any) any {
		switch x := node.(type) {
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, child := range x {
				name := k
				if c, ok := copyNames[jsonmember.FoldedName(k)]; ok {
					name = c
				}
				out[name] = canon(child)
			}
			if t, ok := out["resourceType"].(string); ok {
				if c, ok := copyTypes[jsonmember.FoldedName(t)]; ok {
					out["resourceType"] = c
				}
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, child := range x {
				out[i] = canon(child)
			}
			return out
		case json.Number:
			// This file's own readers decode a copy with encoding/json into
			// float64, which a FHIR decimal beyond its range (1e400) would
			// make them read as nothing: none of them reads a number, so
			// such a one is written as its literal text, a string.
			if _, err := strconv.ParseFloat(string(x), 64); err != nil {
				return string(x)
			}
			return x
		}
		return node
	}
	b, err := json.Marshal(canon(v))
	if err != nil {
		return nil
	}
	return b
}

// withoutContained is a canonicalCopy of a resource with its contained
// resources taken off: the resource itself, for reading its own members (the
// shared readers walk contained resources).
func withoutContained(canonical []byte) []byte {
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(canonical))
	d.UseNumber()
	if d.Decode(&m) != nil {
		return nil
	}
	delete(m, "contained")
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// pasNotifiedOwn is the Patient entry the leg's ClaimResponse's patient
// reference resolves to in its decision's graph (pasgraph.Graph.ByURL at the
// identity pasgraph.ReadNotified bound), or, where it resolves to none and
// the reference resolves at the stated base (base) to Patient/<id>, the one
// urn-written Patient of the decision with that id (urnOwn), as
// canonicalCopy spells it and without its contained resources
// (withoutContained: a Patient contained in it is another, never its
// demographics), and the key of its identity (ownKey, its fullUrl's:
// notifiedPatientKey); nil when it resolves to no Patient entry. The binding
// and the involved list read the leg's own from here, one rule for both. ok
// is false when no copy of it could be made: the caller fails closed.
func pasNotifiedOwn(p *pasNotified, base string) (scope []byte, ownKey string, ok bool) {
	if p.firstGraph == nil {
		return nil, "", true
	}
	e := p.firstGraph.ByURL[p.first.Identity]
	if e == nil || e.ResourceType() != "Patient" {
		e = urnOwn(p, base)
	}
	if e == nil {
		return nil, "", true
	}
	b, err := json.Marshal(e.Resource)
	if err != nil {
		return nil, "", false
	}
	scope = withoutContained(canonicalCopy(b))
	return scope, notifiedPatientKey(e.FullURL), scope != nil
}

// urnOwn is, where the leg's ClaimResponse's patient reference resolves at
// the stated base (base) to Patient/<id>, the one Patient entry of its
// decision written under a urn with that id (its resourceType and id read
// under case folding, as the walk reads them): a urn-written Patient is at the
// stated base (nameBaseOf), so it is the leg's own, as the involved list
// reads it. Two such Patients name neither (the shared-name rule); nil then,
// and when the reference resolves elsewhere.
func urnOwn(p *pasNotified, base string) *pasgraph.Entry {
	identity := p.first.Identity
	if u, err := url.Parse(identity); err != nil || !u.IsAbs() {
		identity = notifiedIdentity("", identity, base)
	}
	stated, _ := nameBaseOf("urn:", base)
	tail := pasgraph.PatientTail(identity)
	if tail == "" || keyBase(notifiedPatientKey(identity)) != stated {
		return nil
	}
	id := strings.TrimPrefix(tail, "Patient/")
	var found *pasgraph.Entry
	for _, e := range p.firstGraph.Ordered() {
		// The type and id are read as the walk reads them, under case
		// folding, so a Patient spelled "patient" or carrying "Id" is the
		// one the involved list reads, and counts toward the two-such rule.
		typ, _ := pasgraph.FoldedMember(e.Resource, "resourceType").(string)
		if jsonmember.FoldedName(typ) != jsonmember.FoldedName("Patient") || !isURN(e.FullURL) || pasgraph.FoldedMember(e.Resource, "id") != id {
			continue
		}
		if found != nil {
			return nil
		}
		found = e
	}
	return found
}

var legPASSubscriptionNotifyInbound = inboundLeg{"provider-tpo", "pas-notify-response", legPASSubscriptionNotify}

// The provider gateway's refusals of a PAS content notification it is
// delivered that name the topic.
const (
	// refusalPASNotifyContent: a content-leg message that is not a
	// notification of the PAS topic with content.
	refusalPASNotifyContent = "the message on this notification leg is not a notification of the PAS topic that carries its decision (per-leg authority): a handshake, a heartbeat or an id-only event rides the signal leg"
	// refusalPASNotifyContext: a notification-event's additional-context,
	// which the PAS topic's notification shape does not declare.
	refusalPASNotifyContext = "a notification-event carries an additional-context, which the PAS topic does not declare (per-leg authority)"
	// refusalPASNotifyUnreadable: a content notification E-22 cannot read
	// one way only.
	refusalPASNotifyUnreadable = "the notification cannot be read one way only (JSON the network cannot read unambiguously, such as a lone surrogate in a string), so the gateway cannot make its own registered edit of the payer's documentation return address (fidelity of SHN's own edit); nothing was delivered"
	// refusalPASNotifyNotTaken: a provider whose participant has not asked
	// its gateway to deliver the PAS topic's notifications.
	refusalPASNotifyNotTaken = "this provider takes no payer's PAS notification: its participant has not asked its gateway to deliver them"
)

// pasContentDelivery is the PAS topic's subscriber half on its content leg,
// on a provider's gateway whose participant asks for the topic's
// notifications, which makes E-22.
var pasContentDelivery = deliveryTopic{
	topic:   pasTopic,
	inbound: legPASSubscriptionNotifyInbound,
	content: true,
	refusals: deliveryRefusals{
		content:  refusalPASNotifyContent,
		context:  refusalPASNotifyContext,
		notTaken: refusalPASNotifyNotTaken,
	},
	// E-22 is made on every notification the leg delivers, so one it cannot
	// read one way only is refused before delivery, on the fidelity of SHN's
	// own edit, 400: the message came from a peer through the Hub, as the
	// payer's gateway refuses its own system's.
	rekey: &deliveryRekey{read: readNotified, unreadable: refusalPASNotifyUnreadable},
	taken: func(g *Gateway) bool { return g.cfg.Role == "provider" && g.cfg.ProviderPASSubscription },
}

// handlePASNotifyInbound delivers a payer's PAS content notification to the
// provider's own endpoint (the rules above, and handleSignalDelivery's).
func (g *Gateway) handlePASNotifyInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
	g.handleSignalDelivery(w, r, env, tok, body, answerTok, pasContentDelivery)
}

// pasNotifiedMembers are the members a PAS decision notification names, for
// the involved list (an exchange is recorded under every patient involved),
// base the payer's FHIR base its gateway states
// (withNotifiedBase): pasNotifiedIdentities, the reading the binding's
// patient.mixed check uses too. A notification whose patients cannot be read
// is errRequestPatientsUnreadable, never read as naming no one, as on every
// leg (requestMembers); the binding refuses such a one first, so it is never
// sent.
func pasNotifiedMembers(payload []byte, base string) ([]namedMember, error) {
	n, rf := subscription.ParseNotification(payload, pasTopic)
	if rf != nil {
		return nil, errRequestPatientsUnreadable
	}
	p, nr := readPASNotified(n)
	if nr != nil {
		return nil, errRequestPatientsUnreadable
	}
	out, _, ok := pasNotifiedIdentities(p, base)
	if !ok {
		return nil, errRequestPatientsUnreadable
	}
	return out, nil
}

// notifiedBaseKey carries, in a PAS content notification's context, the
// payer's FHIR base its gateway states, for the involved list's reading of
// the notification (pasNotifiedMembers) as its binding reads it.
type notifiedBaseKey struct{}

// withNotifiedBase is ctx carrying base for the notification's involved list.
func withNotifiedBase(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, notifiedBaseKey{}, base)
}

// notifiedBaseFrom is the base withNotifiedBase put in ctx, "" when none.
func notifiedBaseFrom(ctx context.Context) string {
	base, _ := ctx.Value(notifiedBaseKey{}).(string)
	return base
}

// notifiedRef is a Patient reference a notification makes, a Coverage's
// party slots aside: the member it names, the key of the identity it
// resolves to (notifiedPatientKey, or, for a urn, the key of the Patient it
// names in its Bundle's scope, urnKey), and the outer resource it sits in
// (-1 for the notification's own members).
type notifiedRef struct {
	member, key string
	outer       int
}

// notifiedReferencesIn are the references node holds, its member names as
// canonicalCopy spells them, but those in a resource nested below it (stop)
// and those of a Coverage's party slots (subscriber, policyHolder, payor:
// partySlots, the shared rule's), which name a party, never a member.
func notifiedReferencesIn(node any) []string {
	var out []string
	slots := map[uintptr]bool{}
	var walk func(v any, top bool)
	walk = func(v any, top bool) {
		switch x := v.(type) {
		case map[string]any:
			if _, ok := x["resourceType"]; ok && !top {
				return
			}
			if x["resourceType"] == "Coverage" {
				for _, k := range partySlots {
					switch slot := x[k].(type) {
					case map[string]any:
						slots[reflect.ValueOf(slot).Pointer()] = true
					case []any:
						for _, s := range slot {
							if sm, ok := s.(map[string]any); ok {
								slots[reflect.ValueOf(sm).Pointer()] = true
							}
						}
					}
				}
			}
			if ref, ok := x["reference"].(string); ok && !slots[reflect.ValueOf(x).Pointer()] {
				out = append(out, ref)
			}
			for _, child := range x {
				walk(child, false)
			}
		case []any:
			for _, child := range x {
				walk(child, false)
			}
		}
	}
	walk(node, true)
	return out
}

// ownReference is a reference in the notification's own members, with the
// fullUrl of the nearest entry it sits in ("" for none).
type ownReference struct{ ref, entryURL string }

// notifiedOwnReferences are the references in v, the notification's own
// members (notificationOwn, canonicalCopy's spelling), each with the fullUrl
// of the Bundle entry it sits in (an element of an entry list: the
// SubscriptionStatus's, for its parameters), "" for none or one that states
// none. A
// Coverage is a resource, read on its own, so no party slot is among them.
func notifiedOwnReferences(v any) []ownReference {
	var out []ownReference
	var walk func(node any, entryURL string)
	walk = func(node any, entryURL string) {
		switch x := node.(type) {
		case map[string]any:
			if ref, ok := x["reference"].(string); ok {
				out = append(out, ownReference{ref, entryURL})
			}
			for k, child := range x {
				if list, ok := child.([]any); ok && k == "entry" {
					// A Bundle's entries: each sits in its own fullUrl, or in
					// none when it states none, as the walk reads them
					// (subscription.Resource.EntryFullURL).
					for _, e := range list {
						full := ""
						if em, ok := e.(map[string]any); ok {
							full, _ = em["fullUrl"].(string)
						}
						walk(e, full)
					}
					continue
				}
				walk(child, entryURL)
			}
		case []any:
			for _, child := range x {
				walk(child, entryURL)
			}
		}
	}
	walk(v, "")
	return out
}

// pasNotifiedIdentities are the identities a PAS decision notification
// names, each bound from the bytes that name it (ownScope), every member name
// and resourceType read under case folding (canonicalCopy), base the payer's
// FHIR base its gateway states. One Patient is one person:
//   - the leg's own patient (subject): the Patient entry its ClaimResponse's
//     reference resolves to, or the one urn-written Patient of its decision
//     with the id that reference names at the stated base, without its
//     contained resources (pasNotifiedOwn, the binding's own reading), or
//     its member alone when it resolves to none;
//   - every other Patient the notification carries, at any depth (the walk:
//     a decision's entries, a nested Bundle, a contained resource, a
//     SubscriptionStatus parameter), but a Coverage's party (the shared rule's
//     party reading, visitCarriedPatients), is ONE identity, bound once from
//     itself without its contained resources, under its id, or its fullUrl,
//     or its place in the notification when it has neither;
//   - every Patient reference, in a resource or in the notification's own
//     members (notificationOwn), is resolved inside a decision by that
//     decision's graph, as the leg's patient is (graphIdentity), and outside
//     any by FHIR R4's Bundle rule (notifiedIdentity), and names the one
//     Patient that answers to it (notifiedPatient.namedBy: that Patient's
//     identity, or any name it is read under, pasPatientNames, at the base
//     its identity is at; the rule the leg's own Patient's names follow). A
//     reference that answers to the leg's own is the leg's; one that answers
//     to another Patient names that Patient's identity, listed once; one
//     that answers to none, or only to a Coverage's party, is an identity of
//     its own, bound from the resource it sits in (the whole notification for
//     its own members). Any reference that is not the leg's own is distinct:
//     it names another identity than the leg's.
//
// ok is false when a copy of a resource could not be made, which never
// happens for a body the walk read; the caller fails closed if it does.
func pasNotifiedIdentities(p *pasNotified, base string) (out []namedMember, distinct, ok bool) {
	seen := map[string]bool{}
	add := func(member string, scope []byte, subject bool) {
		key := member + "\x00" + string(scope)
		if member == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, namedMember{member: member, scope: scope, subject: subject, ownScope: true})
	}
	leg := p.first.Member()
	own, ownKey, ok := pasNotifiedOwn(p, base)
	if !ok {
		return nil, false, false
	}
	if own != nil {
		add(leg, own, true)
	} else {
		add(leg, memberOnlyScope, true)
	}
	// The decision's patient, as resolved: the identity the graph bound, or,
	// where it bound only the Patient/<id> its reference names (a
	// ClaimResponse in no RESTful entry), that resolved against the stated
	// base.
	legIdentity := p.first.Identity
	if u, err := url.Parse(legIdentity); err != nil || !u.IsAbs() {
		legIdentity = notifiedIdentity("", legIdentity, base)
	}
	// The leg's patient by its member and identity; the Patient entry it
	// resolves to, when there is one, answers under every name it is read
	// under (carriedNotifiedPatient, marked own).
	legPatient := notifiedPatient{names: map[string]bool{leg: true}, key: notifiedPatientKey(legIdentity), own: true}
	legPatient.nameBase, legPatient.nameBased = nameBaseOf(legIdentity, base)
	graphs := map[int]*pasgraph.Graph{}
	for _, dec := range p.decisions {
		graphs[dec.outer] = dec.d.Graph
	}
	copies := map[int][]byte{}
	canon := make([][]byte, len(p.resources))
	var refs []notifiedRef
	patients := []*notifiedPatient{&legPatient}
	for j, r := range p.resources {
		c := canonicalCopy(r.Raw)
		if c == nil {
			return nil, false, false
		}
		canon[j] = c
		if r.IsOuter(j) {
			copies[j] = c
		}
		if r.TypeIs("Patient") {
			if np := carriedNotifiedPatient(p, j, c, own, ownKey, base); np != nil {
				patients = append(patients, np)
			}
		}
	}
	// record is a reference's identity as a key and the member it names: a
	// urn resolves, as FHIR R4's Bundle rule has it, against the entries of
	// the Bundle the referencing resource sits in (bundle), then outward
	// through each Bundle enclosing it, never into another nested Bundle; a
	// urn that names nothing in its scope is unresolved, and names no member.
	record := func(identity string, bundle, outer int) {
		if isURN(identity) {
			for b := bundle; ; b = p.resources[b].Bundle {
				var found []*notifiedPatient
				for _, np := range patients {
					if np.fullURL != "" && np.bundle == b && entryKey(np.fullURL) == entryKey(identity) {
						found = append(found, np)
					}
				}
				switch {
				case len(found) == 1:
					refs = append(refs, notifiedRef{member: found[0].member, key: found[0].key, outer: outer})
					return
				case len(found) > 1:
					// Two Patients in one Bundle under one urn: it names
					// neither (the shared-name rule), an identity of its
					// own under the urn itself.
					refs = append(refs, notifiedRef{member: entryKey(identity), key: "urn@" + strconv.Itoa(b) + " ambiguous " + entryKey(identity), outer: outer})
					return
				}
				if b < 0 {
					return
				}
			}
		}
		if member := strings.TrimPrefix(pasgraph.PatientTail(identity), "Patient/"); member != "" {
			refs = append(refs, notifiedRef{member: member, key: notifiedPatientKey(identity), outer: outer})
		}
	}
	for j, r := range p.resources {
		var v any
		if json.Unmarshal(canon[j], &v) != nil {
			return nil, false, false
		}
		for _, ref := range notifiedReferencesIn(v) {
			record(graphIdentity(graphs[r.Outer], r.EntryFullURL, p.resources[r.Outer].FullURL, ref, base), r.Bundle, r.Outer)
		}
	}
	ownMembers := canonicalCopy(notificationOwn(p))
	whole := canonicalCopy(p.body)
	if ownMembers == nil || whole == nil {
		return nil, false, false
	}
	var v any
	if json.Unmarshal(ownMembers, &v) != nil {
		return nil, false, false
	}
	for _, ref := range notifiedOwnReferences(v) {
		record(notifiedIdentity(ref.entryURL, ref.ref, base), -1, -1)
	}
	// Each Patient the notification carries is one identity, bound once,
	// from itself (the leg's own, so bound, is the leg's entry above); a
	// Coverage's party is none.
	for _, np := range patients {
		if !np.party {
			add(np.member, np.scope, false)
		}
	}
	// Each reference names the one Patient that answers to it (namedBy, the
	// rule the leg's own Patient's names follow), or, with none, an identity
	// of its own, bound from the resource it sits in.
	for _, ref := range refs {
		to := matchedPatient(patients, ref)
		switch {
		case to != nil && to.own:
			continue
		case to != nil && !to.party:
			// That Patient is listed above, as itself.
			distinct = true
			continue
		}
		distinct = true
		scope := whole
		if ref.outer >= 0 {
			scope = copies[ref.outer]
		}
		add(ref.member, scope, false)
	}
	return out, distinct, true
}

// notifiedPatient is one Patient a notification carries, read as one
// person: the names it is read under (pasPatientNames), the key of its
// identity (its fullUrl, notifiedPatientKey; "" when it states none), the
// member and bytes it is bound from, whether it is the leg's own and whether
// it is a Coverage's party (the shared rule's party reading).
type notifiedPatient struct {
	names      map[string]bool
	key        string
	member     string
	scope      []byte
	own, party bool
	// fullURL is its entry's fullUrl, and bundle the Bundle it sits in
	// (subscription.Resource.Bundle), the scope a urn names it in.
	fullURL string
	bundle  int
	// nameBase is the base its names are read at (nameBased): its fullUrl's
	// base when that is RESTful; the payer's stated base when it is a urn
	// (the notification came from that server, and a urn says only that the
	// entry has no RESTful address); none when it states no fullUrl.
	nameBase  string
	nameBased bool
}

// matchedPatient is the one Patient a reference names: the Patient at its
// exact identity, matched against every Patient first; failing that, the
// Patient read under the name it states at the base its identity is at
// (namedBy). The leg's own Patient, carried more than once, is one person,
// and at its exact identity it is the one named (the graph's entry, over any
// other object stating that fullUrl); an identity two other Patients state,
// or a name two different Patients answer to, names neither (as
// pasSubjectNames reads a name two Patients share), so the reference is an
// identity of its own. nil when it names none.
func matchedPatient(patients []*notifiedPatient, ref notifiedRef) *notifiedPatient {
	// one is the one person match names, and whether any Patient matched;
	// ownWins, at an exact identity, takes the leg's own Patient (the
	// graph's resolved entry) over any other object stating that fullUrl.
	one := func(match func(np *notifiedPatient) bool, ownWins bool) (*notifiedPatient, bool) {
		var own *notifiedPatient
		var others []*notifiedPatient
		for _, np := range patients {
			switch {
			case !match(np):
			case np.own:
				own = np
			default:
				others = append(others, np)
			}
		}
		switch {
		case own != nil && (len(others) == 0 || ownWins):
			return own, true
		case own == nil && len(others) == 1:
			return others[0], true
		}
		return nil, own != nil || len(others) > 0
	}
	if to, matched := one(func(np *notifiedPatient) bool { return np.key != "" && np.key == ref.key }, true); matched {
		return to
	}
	to, _ := one(func(np *notifiedPatient) bool { return np.namedBy(ref.key, ref.member) }, false)
	return to
}

// urnScopedPatient is the index of the Patient a urn written in resources[i]
// names, as FHIR R4's Bundle rule resolves it: among the entries of the
// Bundle resources[i] sits in, then outward through each Bundle enclosing
// it, never into another nested Bundle; -1 when it names none in that
// scope.
func urnScopedPatient(resources []subscription.Resource, i int, urn string) int {
	for b := resources[i].Bundle; ; b = resources[b].Bundle {
		for k, r := range resources {
			if r.TypeIs("Patient") && r.Bundle == b && r.FullURL != "" && entryKey(r.FullURL) == entryKey(urn) {
				return k
			}
		}
		if b < 0 {
			return -1
		}
	}
}

// nameBaseOf is the base the names of a Patient at identity (a fullUrl, or
// the leg's resolved identity) are read at, and whether they are read at
// any: a RESTful identity's own base; for a urn, the payer's stated base
// (base), the server the notification came from.
func nameBaseOf(identity, base string) (string, bool) {
	if isURN(identity) {
		return keyBase(notifiedPatientKey(notifiedIdentity("", "Patient/_", base))), true
	}
	if tail := pasgraph.PatientTail(identity); tail != "" {
		return keyBase(notifiedPatientKey(identity)), true
	}
	return "", false
}

// isURN reports whether identity is a URN (a urn:uuid or urn:oid fullUrl).
func isURN(identity string) bool { return strings.HasPrefix(strings.ToLower(identity), "urn:") }

// urnKey is the key of a urn fullUrl within the Bundle (bundle) it sits in.
func urnKey(bundle int, fullURL string) string {
	return "urn@" + strconv.Itoa(bundle) + " " + entryKey(fullURL)
}

// namedBy reports whether a reference whose identity has key k and names
// member names the Patient np: its identity itself, or any name it is read
// under at the base its identity is at (as pasSubjectNames reads a
// request's subject by any of its names).
func (np *notifiedPatient) namedBy(k, member string) bool {
	if np.key == "" {
		return false
	}
	return k == np.key || (np.names[member] && np.nameBased && keyBase(k) == np.nameBase)
}

// keyBase is the base a key (notifiedPatientKey) is at: the key without its
// Patient/<id>, "" for one that resolved against no base.
func keyBase(k string) string { return strings.TrimSuffix(k, pasgraph.PatientTail(k)) }

// carriedNotifiedPatient is the Patient resources[j] (c its canonicalCopy),
// as one person, marked the leg's own when it is the leg's Patient entry: at
// its identity (ownKey) and its bytes (own, read without its contained
// resources).
func carriedNotifiedPatient(p *pasNotified, j int, c, own []byte, ownKey, base string) *notifiedPatient {
	r := p.resources[j]
	self := withoutContained(c)
	var m map[string]any
	if self == nil || json.Unmarshal(self, &m) != nil {
		return nil
	}
	np := &notifiedPatient{names: map[string]bool{}, scope: self, fullURL: r.FullURL, bundle: r.Bundle}
	if r.FullURL != "" {
		np.key = notifiedPatientKey(r.FullURL)
		if isURN(r.FullURL) {
			// A urn names it within its Bundle's scope only.
			np.key = urnKey(r.Bundle, r.FullURL)
		}
		np.nameBase, np.nameBased = nameBaseOf(r.FullURL, base)
	}
	for _, n := range pasPatientNames(m, r.FullURL) {
		np.names[n] = true
	}
	if id, _ := m["id"].(string); id != "" {
		np.member = id
	} else if r.FullURL != "" {
		// With no id, named by its fullUrl, as a PAS request's Patient is.
		np.member = r.FullURL
	} else {
		np.member = "#" + strconv.Itoa(r.At)
	}
	if own != nil && r.FullURL != "" && notifiedPatientKey(r.FullURL) == ownKey && sameJSON(self, own) {
		np.own = true
		return np
	}
	// A Coverage's party, by the shared rule's reading of the outer resource
	// it sits in (visitCarriedPatients, over its binding view: viewOf), under
	// any name it is read under. An outer resource no copy or view can be made
	// of is the zero view, which carries no patient.
	outer := viewOf(canonicalCopy(p.resources[r.Outer].Raw))
	for n := range np.names {
		visitCarriedPatients(outer, n, false, func(patient map[string]any, party bool) {
			if b, err := json.Marshal(patient); err == nil && party && sameJSON(withoutContained(b), self) {
				np.party = true
			}
		})
	}
	return np
}

// sameJSON reports whether a and b, each one JSON value, read as the same
// value.
func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
