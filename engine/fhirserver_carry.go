package engine

import (
	"errors"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Carrying the coverage read through the request's own fhirServer (E-07,
// relay.EditCDSCoverageCarry). The callback strip (E-01) removes fhirServer
// and fhirAuthorization, so a payer has no way to read the Coverage a CDS
// Hooks request left out. When the provider's gateway read that Coverage
// through fhirServer to route by (fhirserver_read.go), it carries what it
// read as prefetch.coverage, by default: a searchset the gateway writes
// around exactly the records routing used, each byte for byte as the EHR's
// server returned it, and nothing else of that server's answer (no link, no
// entry address of the server's own, no Bundle id or meta, no
// OperationOutcome). Every entry's fullUrl is a urn:uuid, except an included
// Organization that a carried Coverage names by an absolute reference on the
// request's own fhirServer base: its fullUrl is that reference, already in
// the Coverage's own bytes, so the payer resolves it.
// A request that carries a coverage key, even null, is never given another;
// with the read off, or no fhirServer named, nothing is read and nothing is
// carried.

// coverageCarry is what a CDS Hooks request whose coverage was read through
// its own fhirServer keeps until it is routed: the EHR's body and its
// structure, the edits already decided (E-01's removals, and any other), the
// read's answer and the fence it passed. The payor Organization routing may
// read is read during routing, so the request is built only then
// (carryFHIRServerCoverage).
type coverageCarry struct {
	body relay.Body
	doc  *relay.Document
	root relay.NodeID
	// prefetch is the request's prefetch object, or the handle of the one
	// an earlier change creates; hasPrefetch is false when there is neither,
	// and the carry creates it.
	prefetch    relay.NodeID
	hasPrefetch bool
	changes     []relay.Change
	// answer is the Coverage searchset the read returned, exactly; fence is
	// the patient fence it passed.
	answer []byte
	fence  patientFence
}

// carryFHIRServerCoverage builds the request a routed CDS Hooks request
// leaves the provider as, when its coverage was read through its own
// fhirServer: the edits already decided (E-01) and prefetch.coverage
// inserted (E-07), the searchset fhirServerCoverageValue writes from the
// read and the payor Organization routing read, if any. A request whose
// coverage came from anywhere else was built before routing and is left as
// it is. It is called only once the request is routed: a refused request
// builds nothing and sends nothing.
func (g *Gateway) carryFHIRServerCoverage(prepared *crdIngressRequest) (int, string) {
	c := prepared.carry
	if c == nil {
		return 0, ""
	}
	if prepared.fhirServer == nil {
		return http.StatusInternalServerError, "prepare cds request failed"
	}
	payor := prepared.fhirServer.payor
	value, status, msg := fhirServerCoverageValue(c.answer, payor, prepared.fhirServer.read.base, c.fence)
	if status != 0 {
		return status, msg
	}
	var ops []relay.Op
	target := c.prefetch
	if !c.hasPrefetch {
		op, h := c.doc.EnsureObjectMember(c.root, "prefetch")
		ops = append(ops, op)
		target = relay.NodeID(h)
	}
	ops = append(ops, c.doc.InsertMember(target, "coverage", value))
	changes := append(append([]relay.Change(nil), c.changes...), relay.Change{Edit: relay.EditCDSCoverageCarry, Ops: ops})
	request, err := relay.ApplyChanges(c.body, "application/json", changes...)
	var signed *relay.SignedContentError
	switch {
	case errors.As(err, &signed):
		return http.StatusUnprocessableEntity, signed.Error()
	case err != nil:
		return http.StatusInternalServerError, "prepare cds request failed"
	}
	prepared.request = request
	return 0, ""
}

// fhirServerCoverageValue writes the searchset E-07 carries
// (assembleSoRSearchset, each record an embed of the read's own bytes):
//
//   - a match entry for each Coverage routing chose (routingCoverageChoice:
//     the active ones, else all of them), in the answer's order;
//   - an include entry for the payor Organization routing resolved for each
//     chosen Coverage (fhirServerCarriedEntries: routing's own rule), an
//     Organization entry of the answer or the one routing read through
//     fhirServer (payor: the whole answer to that read), once each.
//
// Every entry's fullUrl is a urn:uuid the gateway assigns, with one
// exception: an included Organization that a chosen Coverage names by an
// absolute reference on the request's own fhirServer base (base) has that
// reference, as the Coverage writes it, as its fullUrl, so the reference the
// Coverage already carries resolves in the searchset (absolutePayorOnBase). No
// other entry (an OperationOutcome, any other resource) and nothing else of
// the answer (its links, entry addresses, id or meta) is carried; total is
// the number of matches. What is written passes the fill fence before it is
// carried: every record in it is the bound patient's own or names no patient
// (a refusal at every level, as for any record SHN adds to a message).
func fhirServerCoverageValue(answer, payor []byte, base string, fence patientFence) ([]byte, int, string) {
	entries, total, ok := fhirServerCarriedEntries(answer, payor, base)
	if !ok {
		return nil, http.StatusInternalServerError, "prepare cds request failed"
	}
	pages := [][]byte{answer}
	if payor != nil {
		pages = append(pages, payor)
	}
	value, _, err := assembleSoRSearchset(pages, entries, total)
	if err != nil {
		return nil, http.StatusInternalServerError, "prepare cds request failed"
	}
	// The fill fence: the routing read was fenced as a whole; what is carried
	// is fenced again as written, so nothing reaches the payer that did not
	// pass it.
	if err := fence.check(value); err != nil {
		status, msg := fillFenceRefusal(err)
		return nil, status, msg
	}
	return value, 0, ""
}

// fhirServerCarriedEntries locates, in the read's own bytes, the records
// fhirServerCoverageValue carries, and the number of matches. ok is false for
// an answer that holds no Coverage or cannot be read as the read read it.
//
// The Organizations included are exactly the ones routing resolved: each
// chosen Coverage is parsed for its payer by routing's own rule
// (shnsdk.ParseCoveragePayer: payor[0], its inline identifier first, then a
// contained or referenced Organization), with a resolver that answers a
// reference as routing's does from these records (an Organization entry of
// the answer, by fullUrl or Organization/<id>, entryAnswers; else the payor
// Organization routing read, by its id) and records what it answered. So a
// Coverage routed by its own identifier includes nothing, a second payor is
// never included, and an Organization only a Coverage not chosen names never
// is.
func fhirServerCarriedEntries(answer, payor []byte, base string) (entries []assemblyEntry, total int, ok bool) {
	doc, err := relay.Doc(relay.NewBody(answer, relay.OriginUpstreamResponse))
	if err != nil || doc.Kind(doc.Root()) != relay.KindObject {
		return nil, 0, false
	}
	list, _ := doc.Member(doc.Root(), "entry")
	type located struct {
		typ, id, fullURL string
		span             EntrySpan
	}
	var all []located
	var covs [][]byte
	var covAt []int
	for _, e := range doc.Elems(list) {
		res, ok := doc.Member(e, "resource")
		if !ok || doc.Kind(res) != relay.KindObject {
			return nil, 0, false
		}
		typ, _ := stringMember(doc, res, "resourceType")
		id, _ := stringMember(doc, res, "id")
		fullURL, _ := stringMember(doc, e, "fullUrl")
		s, end := doc.Span(res)
		if typ == "Coverage" {
			covs, covAt = append(covs, answer[s:end]), append(covAt, len(all))
		}
		all = append(all, located{typ, id, fullURL, EntrySpan{Page: 0, Start: s, End: end}})
	}
	if len(covs) == 0 {
		return nil, 0, false
	}
	_, payorID := entryHead(payor)
	// readAt is the position the payor Organization routing read takes
	// among the answer's entries: past them.
	readAt := len(all)
	// Routing resolves a payor reference against what it routes by
	// (routingCoverages): the chosen Coverages and every entry that is not a
	// Coverage. A Coverage not chosen is never what a reference resolves to.
	chosen := make(map[int]bool, len(covs))
	choice := routingCoverageChoice(covs)
	for _, i := range choice {
		chosen[covAt[i]] = true
	}
	// resolved holds the positions of the Organizations routing resolved;
	// absolute, the absolute reference on the base that resolved one
	// (absolutePayorOnBase). One position has at most one such reference: an
	// answer entry answers an absolute reference only when its fullUrl is that
	// reference, and the read only the one reference payorOrganizationID
	// spells on the base for its id.
	resolved := map[int]bool{}
	absolute := map[int]string{}
	record := func(at int, ref, id string) {
		resolved[at] = true
		if absolutePayorOnBase(ref, base, id) {
			absolute[at] = ref
		}
	}
	for _, i := range choice {
		_, _ = shnsdk.ParseCoveragePayer(covs[i], func(ref string) ([]byte, bool) {
			for at, l := range all {
				if l.typ == "Coverage" && !chosen[at] {
					continue
				}
				if entryAnswers(ref, l.fullURL, l.typ, l.id) {
					if l.typ != "Organization" {
						return nil, false
					}
					record(at, ref, l.id)
					return answer[l.span.Start:l.span.End], true
				}
			}
			if payor != nil {
				if id, ok := payorOrganizationID(ref, base); ok && id == payorID {
					record(readAt, ref, payorID)
					return payor, true
				}
			}
			return nil, false
		})
	}
	include := func(at int, span EntrySpan) assemblyEntry {
		return assemblyEntry{res: span, mode: "include", fullURL: absolute[at]}
	}
	for at, l := range all {
		switch {
		case chosen[at]:
			entries = append(entries, assemblyEntry{res: l.span, mode: "match"})
			total++
		case resolved[at]:
			entries = append(entries, include(at, l.span))
		}
	}
	if resolved[readAt] {
		entries = append(entries, include(readAt, EntrySpan{Page: 1, Start: 0, End: len(payor)}))
	}
	return entries, total, true
}

// absolutePayorOnBase reports whether ref is an absolute reference to the
// Organization id on the request's fhirServer base: "<b>/Organization/<id>",
// where b and base are the same FHIR base once each is normalised as
// sameFHIRBase does (scheme and host lowercased, the default port dropped, a
// trailing slash trimmed). A version, query or fragment, or any other base,
// is never one.
func absolutePayorOnBase(ref, base, id string) bool {
	if !fhirIDRE.MatchString(id) || id == "." || id == ".." {
		return false
	}
	refBase, ok := strings.CutSuffix(ref, "/Organization/"+id)
	if !ok || !strings.Contains(refBase, "://") {
		return false
	}
	return sameFHIRBase(refBase, base)
}
