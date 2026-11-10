// bundleref.go — how a Bundle's own entries answer a reference (a PAS request
// Bundle's, or the Coverage search result a request the gateway builds carries)
// written inside it.
//
// Routing reads one reference this way: the Coverage's payor. An absolute
// reference names the entry whose fullUrl equals it, as FHIR R4 resolves one
// in a Bundle; a relative "[type]/[id]" names the entry with that type and id,
// whatever its fullUrl (FHIR would resolve it against the referencing entry's
// base). Resolution never leaves the Bundle. A reference no entry answers
// resolves to nothing. A reference several entries answer resolves only when
// every one is an Organization naming the same payer identifier (a Bundle that
// repeats its payor); when they disagree, or one names none, the rule never
// picks one of them.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type bundleRefEntry struct {
	fullURL  string
	typ, id  string
	resource json.RawMessage
}

// bundleRefs is a Bundle's entries, as references resolve against them.
// readable is false when the body is not a Bundle whose entries can be read.
type bundleRefs struct {
	entries  []bundleRefEntry
	readable bool
}

// readBundleRefs indexes a Bundle's entries. A body that does not decode has
// none, so every reference resolves to nothing. fullUrl, resourceType and id
// are read by their exact member names, and one that is not a string is read
// as absent, so that a malformed or differently cased member leaves the rest of
// the Bundle readable.
func readBundleRefs(bundleJSON []byte) bundleRefs {
	var b struct {
		Entry []map[string]json.RawMessage `json:"entry"`
	}
	if decodeMessage(bundleJSON, &b) != nil {
		return bundleRefs{}
	}
	out := bundleRefs{readable: true}
	for _, e := range b.Entry {
		res := e["resource"]
		if len(res) == 0 {
			continue
		}
		typ, id := entryHead(res)
		out.entries = append(out.entries, bundleRefEntry{jsonText(e["fullUrl"]), typ, id, res})
	}
	return out
}

// entryHead reads a resource's resourceType and id by their exact member
// names ("" for one that is absent or not a string).
func entryHead(res json.RawMessage) (typ, id string) {
	var head map[string]json.RawMessage
	if decodeMessage(res, &head) != nil {
		return "", ""
	}
	return jsonText(head["resourceType"]), jsonText(head["id"])
}

// jsonText is raw's value when it is a JSON string, and "" otherwise.
func jsonText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// firstCoverage returns the first Coverage entry's resource, or nil when the
// Bundle carries none. It is the Coverage the PAS routes read.
func (b bundleRefs) firstCoverage() json.RawMessage {
	for _, e := range b.entries {
		if e.typ == "Coverage" {
			return e.resource
		}
	}
	return nil
}

type refResolution int

const (
	refResolved  refResolution = iota
	refNoEntry                 // no entry answers the reference
	refAmbiguous               // several entries answer it, and not every one is an Organization
)

// resolve returns the entry ref names (entryAnswers). The owner's fullUrl gives
// a relative reference no base of its own: a relative reference names every
// entry with that type and id. Several Organization entries that answer it
// resolve, for routing, as one Organization naming every identifier they name
// (resolveAmong); several entries not all Organizations are ambiguous.
func (b bundleRefs) resolve(ref string) (json.RawMessage, refResolution) {
	return resolveAmong(b.entries, ref)
}

// resolveAmong is resolve's rule over any set of entries: the one ref names
// (entryAnswers), or, when several Organizations answer it, the payor's
// identifiers are every identifier they name, in document order, each once
// (shnsdk.OrganizationIdentifiers), and the payer directory decides among
// them as it does across payors (routeCandidates): identifiers no participant
// registers are passed over, one registered holder routes, two are ambiguous.
// No entry is picked over another, and nothing is judged by an identifier's
// position. What it returns then is a routing-only Organization holding those
// identifiers: it is read only for them, never carried or filled into a
// message. A message the gateway builds names the payor by the payor's own
// reference, which is the same however many entries answer it. Several
// entries that are not all Organizations are ambiguous.
func resolveAmong(entries []bundleRefEntry, ref string) (json.RawMessage, refResolution) {
	var matches []json.RawMessage
	for _, e := range entries {
		if entryAnswers(ref, e.fullURL, e.typ, e.id) {
			matches = append(matches, e.resource)
		}
	}
	switch {
	case len(matches) == 0:
		return nil, refNoEntry
	case len(matches) == 1:
		return matches[0], refResolved
	}
	var ids []shnsdk.PayerIdentifier
	for _, m := range matches {
		if typ, _ := entryHead(m); typ != "Organization" {
			return nil, refAmbiguous
		}
		for _, id := range shnsdk.OrganizationIdentifiers(m) {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return routingOrganization(ids), refResolved
}

// routingOrganization is the routing-only Organization resolveAmong answers for
// several Organization entries: their identifiers, and nothing else.
func routingOrganization(ids []shnsdk.PayerIdentifier) json.RawMessage {
	b, _ := json.Marshal(struct {
		ResourceType string                   `json:"resourceType"`
		Identifier   []shnsdk.PayerIdentifier `json:"identifier"`
	}{"Organization", append([]shnsdk.PayerIdentifier{}, ids...)})
	return b
}

// entryAnswers is whether a Bundle entry with this fullUrl, resource type and
// id is what ref names: an absolute reference (a URL, or a URN such as
// urn:uuid) names the entry whose fullUrl equals it, and a relative "[type]/[id]"
// names the entry whose resource has that type and id, whatever its fullUrl.
// A version-specific (_history) or otherwise differently spelled reference
// names no entry. The payer-identity mapping (relaylocate.go) matches payor
// and insurer references by the same rule; it refuses several matches, because
// it rewrites the identity of the one it resolves.
func entryAnswers(ref, fullURL, typ, id string) bool {
	return (fullURL != "" && fullURL == ref) || (typ != "" && id != "" && ref == typ+"/"+id)
}

// resolver is resolve as the payor resolver routing takes: it answers only a
// reference resolve resolves.
func (b bundleRefs) resolver() func(ref string) ([]byte, bool) {
	return func(ref string) ([]byte, bool) {
		res, how := b.resolve(ref)
		return res, how == refResolved
	}
}

// referenceIsAbsolute is whether ref is written as an absolute URL or URN,
// even one that does not parse.
func referenceIsAbsolute(ref string) bool {
	if strings.Contains(ref, "://") || strings.HasPrefix(ref, "urn:") {
		return true
	}
	u, err := url.Parse(ref)
	return err == nil && u.IsAbs()
}

// payorMiss says, in the participant's terms, why coverage names no payer
// identifier routing can read, or "" when it cannot tell. It reads payor[0],
// whose refusals refine routing's bare one (a later payor routing cannot read
// is named by noPayerIdentifierFor), and echoes nothing from the body.
func (b bundleRefs) payorMiss(coverage json.RawMessage) string {
	if coverage == nil {
		if !b.readable {
			return ""
		}
		return "the Bundle carries no Coverage"
	}
	var cov struct {
		Payor []struct {
			Reference string `json:"reference"`
		} `json:"payor"`
		Contained []json.RawMessage `json:"contained"`
	}
	if decodeMessage(coverage, &cov) != nil {
		return ""
	}
	if len(cov.Payor) == 0 {
		return "the Coverage names no payor"
	}
	ref := cov.Payor[0].Reference
	switch {
	case ref == "":
		return "Coverage.payor carries neither a reference nor an identifier with both a system and a value"
	case strings.HasPrefix(ref, "#"):
		// Routing takes a contained Organization with that id; anything else
		// with it is named only when no Organization has it.
		var other json.RawMessage
		for _, c := range cov.Contained {
			typ, id := entryHead(c)
			if id != strings.TrimPrefix(ref, "#") {
				continue
			}
			if typ == "Organization" {
				return organizationMiss(c, "the contained payor Organization")
			}
			if other == nil {
				other = c
			}
		}
		if other != nil {
			return organizationMiss(other, "the contained payor Organization")
		}
		return "Coverage.payor names a contained resource the Coverage does not contain"
	}
	org, how := b.resolve(ref)
	switch {
	case how == refAmbiguous:
		return "Coverage.payor matches more than one entry of the Bundle, and not every one is an Organization"
	case how == refNoEntry && referenceIsAbsolute(ref):
		return "Coverage.payor is an absolute reference that is no entry's fullUrl: the payor Organization must be an entry of the Bundle"
	case how == refNoEntry:
		return "Coverage.payor is a relative reference that matches no entry of the Bundle: the payor Organization must be an entry of the Bundle"
	}
	return organizationMiss(org, "the payor Organization in the Bundle")
}

// organizationMiss says why a resolved payor reads as no payer identifier. An
// identifier list that cannot be read carries no identifier routing can read.
func organizationMiss(res json.RawMessage, what string) string {
	if typ, _ := entryHead(res); typ != "Organization" {
		return "Coverage.payor references a resource that is not an Organization"
	}
	var org struct {
		Identifier []struct {
			System string `json:"system"`
			Value  string `json:"value"`
		} `json:"identifier"`
	}
	if decodeMessage(res, &org) == nil {
		for _, id := range org.Identifier {
			if id.System != "" && id.Value != "" {
				return ""
			}
		}
	}
	return what + " carries no identifier with both a system and a value, such as a NAIC code or payer id"
}

// pasRecipient routes a PAS request Bundle by its first Coverage's payor,
// resolved among the Bundle's own entries (recipientForWith). When that payor
// reads as no payer identifier, the refusal says why.
func (g *Gateway) pasRecipient(body []byte) (string, shnsdk.PayerIdentifier, int, string, refusalReason) {
	refs := readBundleRefs(body)
	coverage := refs.firstCoverage()
	recipient, pid, status, msg, reason := g.recipientForWith(coverage, refs.resolver())
	if msg == noPayerIdentifier {
		if why := refs.payorMiss(coverage); why != "" {
			msg += ": " + why
		}
	}
	return recipient, pid, status, msg, reason
}

// carriedRefs indexes the resources a request carries (CDS Hooks prefetch
// values, $questionnaire-package parameter resources, or records a system of
// record's search included) as a payor reference resolves against them: each
// by its resourceType and id, and each entry of one that is a Bundle by its
// fullUrl as well. Heads are read as readBundleRefs reads them.
func carriedRefs(resources [][]byte) []bundleRefEntry {
	var out []bundleRefEntry
	for _, res := range resources {
		typ, id := entryHead(res)
		out = append(out, bundleRefEntry{"", typ, id, res})
		if typ != "Bundle" {
			continue
		}
		out = append(out, readBundleRefs(res).entries...)
	}
	return out
}

const (
	// payorDisagreesInRequest is the reason a request's own resources answer
	// its Coverage's payor reference without naming one payer.
	payorDisagreesInRequest = "Coverage.payor matches more than one resource of the request, and not every one is an Organization"
	// payorDisagreesWithSearch is the reason for a Coverage the system of
	// record supplied: the request's resources and the records that system's
	// Coverage search returned answer the reference without naming one payer.
	payorDisagreesWithSearch = "Coverage.payor matches more than one resource of the request and the system of record's Coverage search, and not every one is an Organization"
)

// payorRefs resolves a CDS Hooks or $questionnaire-package request's payor
// references, every one of them (recipientForCoverages): among local by the
// agreement rule (resolveAmong), then, for a reference local does not answer,
// through next (the system of record, or the request's fhirServer). One
// local answers without naming one payer resolves to nothing and is never
// looked up further, so no other source picks a payer the request leaves in
// doubt; disagreed records it, and disagree is the refusal's reason.
// resolved holds the resource the last reference resolved to, from any
// source, so a refusal can say why it names no payer.
type payorRefs struct {
	local     []bundleRefEntry
	disagree  string
	next      func(ref string) ([]byte, bool)
	disagreed bool
	resolved  []byte
	// named holds the references of the Coverage being read (candidates)
	// that routing does not need read (unreadPayorRefs): they are resolved
	// among local alone and never read through next.
	named map[string]bool
}

func (p *payorRefs) resolve(ref string) ([]byte, bool) {
	res, how := resolveAmong(p.local, ref)
	switch {
	case how == refResolved:
		p.resolved = res
		return res, true
	case how == refAmbiguous:
		p.disagreed = true
		return nil, false
	case p.next == nil || p.named[ref]:
		return nil, false
	}
	res, ok := p.next(ref)
	if ok {
		p.resolved = res
	}
	return res, ok
}

// candidates is shnsdk.CoveragePayerCandidates of coverage, its payor
// references resolved by resolve: a payor that carries an identifier of its
// own has its reference resolved among what the request carries alone, never
// read from the system of record or the request's fhirServer.
func (p *payorRefs) candidates(coverage []byte) ([][]shnsdk.PayerIdentifier, error) {
	p.named, p.disagreed = unreadPayorRefs(coverage), false
	defer func() { p.named = nil }()
	return shnsdk.CoveragePayerCandidates(coverage, p.resolve)
}

// unreadPayorRefs is the set of payor references of coverage that routing never
// needs read: every payor naming them either carries an identifier (with a
// system and a value) of its own, which already names its payer, or states a
// type (Reference.type) other than Organization, which already settles that it
// names none (self-pay) or an unknown one.
func unreadPayorRefs(coverage []byte) map[string]bool {
	var probe struct {
		Payor []struct {
			Reference  string          `json:"reference"`
			Type       json.RawMessage `json:"type"`
			Identifier *struct {
				System string `json:"system"`
				Value  string `json:"value"`
			} `json:"identifier"`
		} `json:"payor"`
	}
	if json.Unmarshal(coverage, &probe) != nil {
		return nil
	}
	named := map[string]bool{}
	for _, p := range probe.Payor {
		if p.Reference == "" {
			continue
		}
		var stated string
		typed := json.Unmarshal(p.Type, &stated) == nil && stated != "" && stated != "Organization"
		unread := typed || (p.Identifier != nil && p.Identifier.System != "" && p.Identifier.Value != "")
		if v, seen := named[p.Reference]; !seen || v {
			named[p.Reference] = unread
		}
	}
	return named
}

// coverageResources returns the Coverages a coverage value holds: the value
// itself when it is a Coverage, or each Coverage entry of a Bundle. Their
// type and the Bundle's entries are read by their exact member names, and a
// member that names one only in another case (ResourceType, Entry,
// Resource) is not read past: the value then holds no Coverage to route by,
// rather than routing on the Coverages a case-folding reader would leave out
// or take differently. Any other value holds none.
func coverageResources(coverage []byte) [][]byte {
	var v struct {
		ResourceType string `json:"resourceType"`
		Entry        []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if decodeMessage(coverage, &v) != nil {
		return nil
	}
	switch v.ResourceType {
	case "Coverage":
		return [][]byte{coverage}
	case "Bundle":
	default:
		return nil
	}
	var covs [][]byte
	for _, e := range v.Entry {
		if len(e.Resource) == 0 || string(e.Resource) == "null" {
			continue
		}
		var head struct {
			ResourceType string `json:"resourceType"`
		}
		if decodeMessage(e.Resource, &head) != nil {
			return nil
		}
		if head.ResourceType == "Coverage" {
			covs = append(covs, e.Resource)
		}
	}
	return covs
}

// recipientForCoverages routes covs as recipientForWith routes a Bundle of
// Coverages (they must name one payer, by the holder the payer directory
// resolves for their identifiers, never by position), each Coverage read alone, so that
// every payor reference resolves through refs and none among a Bundle's
// entries without the agreement rule. A payor refs leaves in doubt is
// refused with refs' reason, and one that resolved to a resource naming no
// payer identifier with the reason organizationMiss gives (as a PAS
// Bundle's is); any other payor without one keeps the bare refusal.
func (g *Gateway) recipientForCoverages(covs [][]byte, refs *payorRefs) (string, shnsdk.PayerIdentifier, int, string, refusalReason) {
	none := shnsdk.PayerIdentifier{}
	if g.cfg.PayerRouter == nil {
		return "", none, http.StatusUnprocessableEntity, msgNoPayerRouter, reasonNoPayerRouter
	}
	var lists [][]shnsdk.PayerIdentifier
	for _, c := range covs {
		refs.resolved = nil
		cands, err := refs.candidates(c)
		var unresolved *shnsdk.UnresolvedPayorError
		switch {
		case errors.As(err, &unresolved) && unresolved.Index > 0:
			// A payor after payor[0] the request leaves unknown is named, as
			// on every other path (noPayerIdentifierFor), before payor[0]'s
			// own refinements: what refs last resolved is another payor's.
			return "", none, http.StatusUnprocessableEntity, noPayerIdentifierFor(err), reasonNoPayerIdentifier
		case err != nil && refs.disagreed:
			return "", none, http.StatusUnprocessableEntity, noPayerIdentifier + ": " + refs.disagree, reasonNoPayerIdentifier
		case err != nil && refs.resolved != nil:
			if why := organizationMiss(refs.resolved, "the payor Organization"); why != "" {
				return "", none, http.StatusUnprocessableEntity, noPayerIdentifier + ": " + why, reasonNoPayerIdentifier
			}
			return "", none, http.StatusUnprocessableEntity, noPayerIdentifier, reasonNoPayerIdentifier
		case err != nil:
			return "", none, http.StatusUnprocessableEntity, noPayerIdentifierFor(err), reasonNoPayerIdentifier
		}
		lists = append(lists, cands...)
	}
	if len(covs) == 0 {
		return "", none, http.StatusUnprocessableEntity, noPayerIdentifier, reasonNoPayerIdentifier
	}
	// The Coverages must name one payer: the holder the directory resolves for each.
	return g.routeCandidates(lists)
}

// The reasons coveragePayorRecords gives for a payor reference it leaves out.
const (
	payorRefNoneAnswers   = "none-answers"
	payorRefSeveralAnswer = "several-answer"
)

// coveragePayorRecords resolves every payor reference each of coverages names
// outside itself (coveragePayorRefs: not contained, not an identifier; every
// payor counts, as routing reads them) against bundle, the Bundle the
// coverages came in, by entryAnswers' rule, each distinct reference once, and
// returns the Organizations they name in order, as bundle holds them, each
// record once. known is a record the caller already holds for knownRef (the
// payor routing chose, memberPayerOrganization), answered when no entry of
// bundle does. A reference that no entry and no known record answers, or that
// several different Organizations answer (no record is picked over another),
// adds nothing: leftOut, when not nil, is told the reason
// (payorRefNoneAnswers, payorRefSeveralAnswer) and the reference's resource
// type (referenceResourceType). A reference that answers with a record that
// is not an Organization adds nothing and is not told. Nothing is minted.
func coveragePayorRecords(bundle []byte, coverages [][]byte, knownRef string, known []byte, leftOut func(reason, resourceType string)) [][]byte {
	refs := readBundleRefs(bundle)
	tell := func(reason, ref string) {
		if leftOut != nil {
			leftOut(reason, referenceResourceType(ref))
		}
	}
	var out [][]byte
	seen := map[string]bool{}
	add := func(record []byte) {
		if !slices.ContainsFunc(out, func(o []byte) bool { return bytes.Equal(o, record) }) {
			out = append(out, record)
		}
	}
	for _, c := range coverages {
		for _, ref := range coveragePayorRefs(c) {
			if strings.HasPrefix(ref, "#") || seen[ref] {
				continue
			}
			seen[ref] = true
			var orgs [][]byte
			answered := false
			for _, e := range refs.entries {
				if !entryAnswers(ref, e.fullURL, e.typ, e.id) {
					continue
				}
				answered = true
				if resourceTypeOf(e.resource) == "Organization" && !slices.ContainsFunc(orgs, func(o []byte) bool { return bytes.Equal(o, e.resource) }) {
					orgs = append(orgs, e.resource)
				}
			}
			switch {
			case !answered && ref == knownRef && len(known) > 0:
				if resourceTypeOf(known) == "Organization" {
					add(known)
				}
			case !answered:
				tell(payorRefNoneAnswers, ref)
			case len(orgs) == 1:
				add(orgs[0])
			case len(orgs) > 1:
				tell(payorRefSeveralAnswer, ref)
			}
		}
	}
	return out
}

// coveragePayorRefs is the reference of every payor of coverage that has one,
// in document order, but a self-pay payor's (selfPayPayor): it names no payer.
func coveragePayorRefs(coverage []byte) []string {
	var probe struct {
		Payor []struct {
			Reference string          `json:"reference"`
			Type      json.RawMessage `json:"type"`
		} `json:"payor"`
	}
	if json.Unmarshal(coverage, &probe) != nil {
		return nil
	}
	var refs []string
	for _, p := range probe.Payor {
		if p.Reference != "" && !selfPayPayor(coverage, p.Type, p.Reference) {
			refs = append(refs, p.Reference)
		}
	}
	return refs
}

// resourceTypeOf is resource's resourceType, or "" when it is not a JSON
// object naming one.
func resourceTypeOf(resource []byte) string {
	var head struct {
		ResourceType string `json:"resourceType"`
	}
	if len(resource) == 0 || json.Unmarshal(resource, &head) != nil {
		return ""
	}
	return head.ResourceType
}

// referenceResourceType is the resource type a reference names, read from
// its "[type]/[id]" tail, relative or absolute, when patientBindingPaths
// (the R4 resource types of the Patient CompartmentDefinition's listing)
// names it, or "" otherwise (a URN, or a tail that is no such type). Nothing
// else of the reference is returned.
func referenceResourceType(ref string) string {
	parts := strings.Split(strings.TrimSuffix(ref, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	typ := parts[len(parts)-2]
	if typ == "_history" && len(parts) >= 4 {
		typ = parts[len(parts)-4]
	}
	if _, ok := patientBindingPaths[typ]; !ok {
		return ""
	}
	return typ
}
