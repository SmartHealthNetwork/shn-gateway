// bundleref.go — how a PAS request Bundle's own entries answer a reference
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
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	refAmbiguous               // several entries answer it, and they do not name one payer
)

// resolve returns the entry ref names (entryAnswers). The owner's fullUrl gives
// a relative reference no base of its own: a relative reference names every
// entry with that type and id. Several entries resolve as the first of them
// only when each is an Organization naming the same payer identifier, read as
// routing reads it (shnsdk.ParseOrganizationIdentifier); otherwise they are
// ambiguous.
func (b bundleRefs) resolve(ref string) (json.RawMessage, refResolution) {
	return resolveAmong(b.entries, ref)
}

// resolveAmong is resolve's rule over any set of entries: the ones ref names
// (entryAnswers) resolve as the first of them only when each is an
// Organization naming the same payer identifier.
func resolveAmong(entries []bundleRefEntry, ref string) (json.RawMessage, refResolution) {
	var matches []json.RawMessage
	for _, e := range entries {
		if entryAnswers(ref, e.fullURL, e.typ, e.id) {
			matches = append(matches, e.resource)
		}
	}
	if len(matches) == 0 {
		return nil, refNoEntry
	}
	// An entry naming no payer reads as the zero identifier, which no named
	// one equals.
	first, _ := shnsdk.ParseOrganizationIdentifier(matches[0])
	for _, m := range matches[1:] {
		if pid, named := shnsdk.ParseOrganizationIdentifier(m); !named || pid != first {
			return nil, refAmbiguous
		}
	}
	return matches[0], refResolved
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
// identifier routing can read, or "" when it cannot tell. It reads the same
// payor, in the same order, as routing does (shnsdk.ParseCoveragePayer) and
// echoes nothing from the body.
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
		return "Coverage.payor matches more than one entry of the Bundle, and they do not name one payer"
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
func (g *Gateway) pasRecipient(body []byte) (string, shnsdk.PayerIdentifier, int, string) {
	refs := readBundleRefs(body)
	coverage := refs.firstCoverage()
	recipient, pid, status, msg := g.recipientForWith(coverage, refs.resolver())
	if msg == noPayerIdentifier {
		if why := refs.payorMiss(coverage); why != "" {
			msg += ": " + why
		}
	}
	return recipient, pid, status, msg
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
	payorDisagreesInRequest = "Coverage.payor matches more than one resource of the request, and they do not name one payer"
	// payorDisagreesWithSearch is the reason for a Coverage the system of
	// record supplied: the request's resources and the records that system's
	// Coverage search returned answer the reference without naming one payer.
	payorDisagreesWithSearch = "Coverage.payor matches more than one resource of the request and the system of record's Coverage search, and they do not name one payer"
)

// payorRefs resolves a CDS Hooks or $questionnaire-package request's payor
// references, every one of them (recipientForCoverages): among local by the
// agreement rule (resolveAmong), then, for a reference local does not answer,
// through next (the system of record, or the request's fhirServer). One
// local answers without naming one payer resolves to nothing and is never
// looked up further, so no other source picks a payer the request leaves in
// doubt; disagreed records it, and disagree is the refusal's reason.
type payorRefs struct {
	local     []bundleRefEntry
	disagree  string
	next      func(ref string) ([]byte, bool)
	disagreed bool
}

func (p *payorRefs) resolve(ref string) ([]byte, bool) {
	res, how := resolveAmong(p.local, ref)
	switch {
	case how == refResolved:
		return res, true
	case how == refAmbiguous:
		p.disagreed = true
		return nil, false
	case p.next == nil:
		return nil, false
	}
	return p.next(ref)
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
// Coverages (they must name one payer), each Coverage read alone, so that
// every payor reference resolves through refs and none among a Bundle's
// entries without the agreement rule. A payor refs leaves in doubt is
// refused with refs' reason.
func (g *Gateway) recipientForCoverages(covs [][]byte, refs *payorRefs) (string, int, string) {
	if g.cfg.PayerRouter == nil {
		return "", http.StatusUnprocessableEntity, "no payer router configured"
	}
	var first shnsdk.PayerIdentifier
	for i, c := range covs {
		pid, err := shnsdk.ParseCoveragePayer(c, refs.resolve)
		switch {
		case err != nil && refs.disagreed:
			return "", http.StatusUnprocessableEntity, noPayerIdentifier + ": " + refs.disagree
		case err != nil:
			return "", http.StatusUnprocessableEntity, noPayerIdentifier
		case i > 0 && pid != first:
			return "", http.StatusUnprocessableEntity, "ambiguous coverage for routing: the coverage names more than one payer"
		}
		first = pid
	}
	if len(covs) == 0 {
		return "", http.StatusUnprocessableEntity, noPayerIdentifier
	}
	holder, ok := g.cfg.PayerRouter.Resolve(first)
	if !ok {
		return "", http.StatusUnprocessableEntity,
			fmt.Sprintf("no registered payer for identifier %s|%s", first.System, first.Value)
	}
	return holder, 0, ""
}
