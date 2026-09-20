// inquire.go — the Da Vinci prior-authorization inquiry leg (pas-claim-inquire):
// how a requester asks a payer for the decision on an authorization the payer
// pended, and how a payer gateway answers.
//
// Nothing here polls and nothing here assembles. The requester asks explicitly,
// the payer's own system answers, and the payer's bytes reach the requester
// exactly as the payer wrote them — with the payer's own media type. The only
// change the leg may make is the registered payer-identity restamp on the request
// it sends its own system, the same edit the submit and update legs make.
//
// The one thing the payer gateway DERIVES from the answer is its own pend
// ledger: an inquiry that comes back decided is how a payer gateway learns the
// decision for an authorization it recorded as pended. That is a Store
// side-effect, orthogonal to the relay — the requester receives the payer's
// answer whether or not anything in the ledger matches it.
//
// TWO ANSWER SHAPES, one reader. The published operation definitions differ by
// IG line: at 2.0.1 and 2.1.0 `Claim/$inquire` returns the response Bundle
// itself; at 2.2.1 it returns a `Parameters` whose `return` parameters are 0..*
// response Bundles, one ClaimResponse each. Every ClaimResponse in either shape
// is read, because an answer that carries several authorizations decides several.
//
// WHAT PROFILE-VALIDATES AN INBOUND PEER'S INQUIRY: nothing on this path, by
// design, and the same is true of the payer's answer. Neither hop runs an
// enforcing `$validate` over either one — a peer's bytes are preserved and this
// gateway does not certify content it did not produce, the posture the submit and
// update legs already take. Concretely, on the payer hop the response check
// stands down for a relayed answer (and an inquiry's answer is always relayed),
// and the one `$validate` that does run covers only this gateway's OWN decision
// ExplanationOfBenefit; on the provider hop the inquiry is carried as sent.
//
// Two consequences worth stating rather than discovering. First, a line-specific
// defect in a peer's inquiry — an inquiry naming no item, which the earliest
// prior-authorization line requires and the later ones do not — is not refused
// here; the payer's own system is what certifies an inquiry it receives, which is
// where that judgement belongs. Second, the certification lane still OBSERVES
// both directions, at the inquiry's own profiles and at every candidate line
// (TestPASInquire_NotProfileValidatedButObserved pins the profile resolution), so
// such a defect is visible as evidence. That evidence is observational: no
// decision on this path reads it. The 2.2.1 `Parameters` wrapper is not observed
// either, because the IG governs it by its operation definition and declares no
// profile for it.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// pasExtItemTraceNumberURL is the Da Vinci PAS item trace-number extension. A
// requester's own trace numbers are what a payer echoes, so they are the keys an
// inquiry's answer is matched back by.
const pasExtItemTraceNumberURL = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber"

// ---- the request: subject bind ----

// pasInquiryFacts is what the inquiry bind extracted: the one member every
// patient reference in the inquiry names, and the item lines it asks about (their
// trace numbers and product coding — the coding a decision's own
// ExplanationOfBenefit is built from, sourced from the requester's own inquiry and
// never invented).
type pasInquiryFacts struct {
	member string
	items  []pasInquiryItemFact
}

type pasInquiryItemFact struct {
	traceNumber string // "system|value", or "" when the line carries none
	system      string
	code        string
	display     string
}

// parsePASInquiryFacts reads a Da Vinci PAS inquiry request Bundle and binds it to
// ONE member.
//
// The bind covers EVERY Patient the Bundle names, not just the first: the
// inquiry Claim's patient, each Patient entry's own identity, and every
// patient-bearing reference on any entry (a Coverage beneficiary, an order's
// subject, a Task's for). One inquiry asks about one member's authorizations, so a
// Bundle that names two members is refused before any authority check can be
// satisfied by the member it happens to read first.
//
// The inquiry request Bundle profile allows no `entry.request` or `entry.response`
// at any line; a Bundle carrying one is not an inquiry and is refused here rather
// than sent on.
func parsePASInquiryFacts(bundleJSON []byte) (pasInquiryFacts, int, string) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Entry        []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
			Request  json.RawMessage `json:"request"`
			Response json.RawMessage `json:"response"`
		} `json:"entry"`
	}
	if err := decodeMessage(bundleJSON, &probe); err != nil {
		return pasInquiryFacts{}, http.StatusBadRequest, "parse inquiry bundle failed"
	}
	if probe.ResourceType != "Bundle" {
		return pasInquiryFacts{}, http.StatusBadRequest, "PAS inquiry is not a Bundle"
	}
	if probe.Type != "collection" {
		return pasInquiryFacts{}, http.StatusBadRequest, "PAS inquiry Bundle is not a collection"
	}
	var (
		facts     pasInquiryFacts
		claimSeen bool
		members   = map[string]bool{}
	)
	note := func(ref string) {
		if m := patientMemberFromRef(ref); m != "" {
			members[m] = true
		}
	}
	for _, e := range probe.Entry {
		if len(e.Request) > 0 || len(e.Response) > 0 {
			return pasInquiryFacts{}, http.StatusBadRequest, "PAS inquiry Bundle entries carry no request or response"
		}
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		if err := decodeMessage(e.Resource, &head); err != nil {
			return pasInquiryFacts{}, http.StatusBadRequest, "parse inquiry bundle entry failed"
		}
		if head.ResourceType == "Patient" {
			// A Patient entry IS a patient reference target: its own identity counts.
			if head.ID != "" {
				members[head.ID] = true
			} else {
				note(e.FullURL)
			}
		}
		// Every patient-bearing reference on this resource, whatever its type.
		note(refMember(e.Resource, "patient"))
		note(refMember(e.Resource, "subject"))
		note(refMember(e.Resource, "beneficiary"))
		note(refMember(e.Resource, "for"))
		if head.ResourceType != "Claim" || claimSeen {
			continue
		}
		claimSeen = true
		var claim struct {
			Use     string `json:"use"`
			Patient struct {
				Reference string `json:"reference"`
			} `json:"patient"`
			Item []struct {
				Extension        []json.RawMessage `json:"extension"`
				ProductOrService struct {
					Coding []struct {
						System  string `json:"system"`
						Code    string `json:"code"`
						Display string `json:"display"`
					} `json:"coding"`
				} `json:"productOrService"`
			} `json:"item"`
		}
		if err := decodeMessage(e.Resource, &claim); err != nil {
			return pasInquiryFacts{}, http.StatusBadRequest, "parse inquiry Claim failed"
		}
		if claim.Use != "preauthorization" {
			return pasInquiryFacts{}, http.StatusBadRequest, "PAS inquiry Claim is not a preauthorization"
		}
		if claim.Patient.Reference == "" {
			return pasInquiryFacts{}, http.StatusBadRequest, "PAS inquiry Claim missing patient"
		}
		facts.member = patientMemberFromRef(claim.Patient.Reference)
		for _, it := range claim.Item {
			f := pasInquiryItemFact{traceNumber: identifierExtensionKey(it.Extension, pasExtItemTraceNumberURL)}
			if len(it.ProductOrService.Coding) > 0 {
				c := it.ProductOrService.Coding[0]
				f.system, f.code, f.display = c.System, c.Code, c.Display
			}
			facts.items = append(facts.items, f)
		}
	}
	if !claimSeen || facts.member == "" {
		return pasInquiryFacts{}, http.StatusBadRequest, "PAS inquiry Bundle missing Claim.patient"
	}
	// An inquiry that names NO item line is accepted. A requester may ask by the
	// authorization number alone — which the later prior-authorization lines carry
	// on the inquiry Claim itself, not on an item — so a Bundle with no items is a
	// conformant request at those lines, and the payer's own system is what answers
	// it. Item cardinality is a profile question, which the certifier decides; what
	// this bind decides is authority, which is the one member below. Refusing here
	// would refuse a conformant peer, and would make this gateway stricter than the
	// published Responder, whose own inquiry reader requires no item either.
	//
	// The only thing the items are read for is the product coding a recorded
	// decision's ExplanationOfBenefit states, and an inquiry that names none simply
	// records the decision without one — nothing is invented to fill the gap.
	//
	// The bind: one member across the whole Bundle.
	if len(members) != 1 || !members[facts.member] {
		return pasInquiryFacts{}, http.StatusForbidden, "inconsistent patient in PAS inquiry"
	}
	return facts, 0, ""
}

// patientMemberFromRef returns the bare member id of a Patient reference,
// tolerating a relative ref and an absolute fullUrl the same way the submit legs
// do. "" for a reference that names no Patient.
func patientMemberFromRef(ref string) string {
	if ref == "" || !strings.Contains(ref, "Patient/") {
		return ""
	}
	return pasMemberFromRef(ref)
}

// refMember reads resource.<field>.reference, for the patient-bearing fields.
func refMember(resource json.RawMessage, field string) string {
	var probe map[string]json.RawMessage
	if decodeMessage(resource, &probe) != nil {
		return ""
	}
	raw, ok := probe[field]
	if !ok {
		return ""
	}
	var v struct {
		Reference string `json:"reference"`
	}
	if decodeMessage(raw, &v) != nil {
		return ""
	}
	return v.Reference
}

// identifierExtensionKey reads the "system|value" of an extension's
// valueIdentifier, for the named extension url. "" when absent or incomplete: a
// half-stated identifier is not a lookup key.
func identifierExtensionKey(exts []json.RawMessage, url string) string {
	for _, raw := range exts {
		var e struct {
			URL             string `json:"url"`
			ValueIdentifier struct {
				System string `json:"system"`
				Value  string `json:"value"`
			} `json:"valueIdentifier"`
		}
		if decodeMessage(raw, &e) != nil || e.URL != url {
			continue
		}
		return identifierKey(e.ValueIdentifier.System, e.ValueIdentifier.Value)
	}
	return ""
}

// identifierKey is the ledger's "system|value" key form. "" when either half is
// missing.
func identifierKey(system, value string) string {
	if strings.TrimSpace(system) == "" || strings.TrimSpace(value) == "" {
		return ""
	}
	return system + "|" + value
}

// ---- the answer: shape and reading ----

// inquiryAnswerResponse is one ClaimResponse the answer carried, with the bytes it
// carried it as (read by the shared decision parsers) and the lookup keys it
// states.
type inquiryAnswerResponse struct {
	raw     []byte
	keys    PendKeys
	created time.Time
}

// validatePASInquiryAnswer checks that a 2xx answer is one of the shapes the
// published operation definitions declare, WITHOUT changing a byte of it: the
// response Bundle itself (2.0.1, 2.1.0), a Parameters of response Bundles
// (2.2.1), or the OperationOutcome either line may answer with. Any other
// resource is an answer this leg cannot read, which is the payer's problem to
// fix and a loud 502 here rather than a silent pass.
//
// A Parameters carrying its Bundles under the recorded nonconformant output name
// is READ here, not refused: its Bundles are the payer's answer and this check is
// about whether the answer can be read at all. The deviation is reported by the
// caller, on its own seam.
func validatePASInquiryAnswer(body []byte) LegResult {
	kind, returns, _, err := inquiryAnswerShape(body)
	if err != nil {
		return fail502("invalid prior-authorization inquiry answer")
	}
	switch kind {
	case "Bundle", "OperationOutcome":
		return LegResult{}
	case "Parameters":
		for _, r := range returns {
			inner, _, _, err := inquiryAnswerShape(r)
			if err != nil || (inner != "Bundle" && inner != "OperationOutcome") {
				return fail502("invalid prior-authorization inquiry answer")
			}
		}
		return LegResult{}
	}
	return fail502("invalid prior-authorization inquiry answer")
}

// The output parameter a `Claim/$inquire` answer carries its response Bundles
// under.
//
// pasInquiryDeclaredOutput is what every published operation definition declares:
// `return`, 1..1 at PAS 2.0.1 and 2.1.0 and 0..* at 2.2.1 (verified against the
// published packages, 2026-09-18).
//
// pasInquiryRecordedOutput is what the Da Vinci reference payer actually sends —
// `responseBundle` — recorded from the pinned image to
// testdata/br-payer/pas-inquire-response.json and present on the unpatched
// upstream commit too, so it is that implementation's own choice. An answer
// arriving under it is READ and REPORTED as nonconformant (design §6.4 addendum,
// 2026-09-18), never normalized and never dropped: a reader that only knew the
// declared name would take the payer's bytes to the requester and record nothing
// from them, with no signal anywhere — silent loss, which this network forbids as
// squarely as fabrication.
//
// The set is CLOSED. A third name is not read, and would leave the answer
// carrying no response Bundle this gateway can see; adding one is a change here,
// with a recording behind it.
const (
	pasInquiryDeclaredOutput = "return"
	pasInquiryRecordedOutput = "responseBundle"
)

// pasInquiryOutputCarries reports whether an output parameter name carries a
// response Bundle, and whether that name deviates from the declared one.
func pasInquiryOutputCarries(name string) (carries, deviant bool) {
	switch name {
	case pasInquiryDeclaredOutput:
		return true, false
	case pasInquiryRecordedOutput:
		return true, true
	}
	return false, false
}

// inquiryAnswerShape reports the answer's resourceType and, for a Parameters, the
// resources of the output parameters this gateway READS, together with every
// parameter name that carried a resource and departs from the declared one —
// whether it is read (the recorded `responseBundle`) or not read at all
// (first-seen order, no repeats).
//
// An unread name is in that list on purpose. The read set is closed, so a payer
// using a third name has its Bundles skipped by every reader here — and skipped
// silently is exactly the failure this leg was opened to fix, one name over. The
// answer still relays; what changes is that the gateway SAYS it could not read
// part of it, instead of behaving as though there was nothing there.
func inquiryAnswerShape(body []byte) (string, []json.RawMessage, []string, error) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name     string          `json:"name"`
			Resource json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if err := decodeMessage(body, &probe); err != nil {
		return "", nil, nil, err
	}
	if probe.ResourceType == "" {
		return "", nil, nil, fmt.Errorf("engine: inquiry answer names no resourceType")
	}
	if probe.ResourceType != "Parameters" {
		return probe.ResourceType, nil, nil, nil
	}
	var (
		returns []json.RawMessage
		deviant []string
		seen    = map[string]bool{}
	)
	for _, p := range probe.Parameter {
		if len(p.Resource) == 0 {
			continue // a parameter with no resource carries no response bundle
		}
		carries, isDeviant := pasInquiryOutputCarries(p.Name)
		if carries {
			returns = append(returns, p.Resource)
		}
		if (isDeviant || !carries) && !seen[p.Name] {
			seen[p.Name] = true
			deviant = append(deviant, p.Name)
		}
	}
	return "Parameters", returns, deviant, nil
}

// pasInquiryOutputDeviations names each nonconformant output parameter an answer
// carried a response Bundle under. Empty for every conformant answer, and for one
// this gateway cannot read at all — an unreadable answer is the 502 the shape
// check already raises, not a nonconformance to report.
func pasInquiryOutputDeviations(body []byte) []string {
	_, _, deviant, err := inquiryAnswerShape(body)
	if err != nil {
		return nil
	}
	return deviant
}

// reportInquiryAnswerNonconformance states each departure a payer's inquiry answer
// makes from the operation it answered: on the operator's observer as it is found,
// and as the sentences the caller puts on that answer's certification evidence. One
// departure, said once, in two places an operator already looks.
//
// Each sentence names BOTH terms — what the payer sent and what the definition
// declares — because either alone leaves the reader to guess which side is wrong.
//
// It reports; it decides nothing. The answer was read, the answer relays unchanged,
// and no branch of this leg is taken because of what this returns.
func (g *Gateway) reportInquiryAnswerNonconformance(corrID, counterpart string, answer []byte) []string {
	var stated []string
	for _, used := range pasInquiryOutputDeviations(answer) {
		sentence := "prior-authorization inquiry answer carries its response bundle under output parameter " +
			used + "; the operation declares " + pasInquiryDeclaredOutput
		if read, _ := pasInquiryOutputCarries(used); !read {
			// Said differently because the consequence is different: this one was not
			// read, so nothing in it reached the ledger or the subject check. The
			// requester still has the payer's bytes; the operator now knows part of
			// them went unread rather than finding an exchange that looks ordinary.
			sentence = "prior-authorization inquiry answer carries a resource under output parameter " +
				used + ", which this gateway does not read; the operation declares " + pasInquiryDeclaredOutput
		}
		stated = append(stated, sentence)
		g.observe(ObserverEvent{Kind: "peer.nonconformant", Direction: "ingress", LegType: "pas-claim-inquire",
			CorrelationID: corrID, Counterpart: counterpart, Op: "pas-inquire-response", Detail: sentence})
	}
	return stated
}

// pasInquiryAnswerSubjects returns every patient identity an inquiry's answer
// names, anywhere in it, and whether the answer is one readable document.
//
// It is the ANSWER-side twin of parsePASInquiryFacts' bind, and it is deliberately
// as deep as the submit legs' response check: every object at any depth — each
// `return` Bundle, each entry, and each contained resource inside an entry — has
// its subject-bearing members read (`patient`, `subject`, `beneficiary`, `for`,
// and the Reference choice of a `subject[x]`), and every Patient resource
// contributes its own identity. Reading only the ClaimResponses would let an
// answer whose Coverage beneficiary or Patient entry names a different member
// through: one inquiry asks about one member's authorizations, so an answer that
// names two is not an answer to it.
//
// The identities are compared with EACH OTHER, not with this network's member id:
// a payer answers in its own patient namespace. The caller applies the
// bound-member comparison only where the answer is this gateway's own.
//
// REFERENCE FORMS. A reference is resolved the way FHIR says one is, not by
// reading the text after "Patient/": every spelling that names the SAME resource
// has to yield ONE identity, and every spelling that names a DIFFERENT one has to
// yield another. Each response Bundle is its own resolution scope
// (inquiryPatientScope): a reference is looked up against that Bundle's own
// `fullUrl` and `Type/id` index, a `#…` reference against its owner, an
// identifier-only or typed-Patient reference by the identifier it states, and a
// `/_history/` suffix is dropped before any of that — a version-specific
// reference names the same patient as the version-less one, and refusing that
// pair would turn away a conformant answer.
//
// A reference nothing in the Bundle resolves is its OWN identity, as written. So
// two answers naming `Patient/A` on two different servers are two identities,
// while an unresolvable `urn:uuid:` is never quietly equal to a `Patient/…`.
//
// WITHIN one Bundle a patient's identity is its ADDRESS, and only its address.
// Two entries are never folded together, however their identifiers read: a Bundle
// carrying two distinct Patient records is carrying two patients, and a shared
// identifier between them is a fact about the payer's data, not a licence to
// treat one answer as being about the other.
//
// ACROSS Bundles — the 2.2.1 shape returns one per authorization, and a payer may
// address one patient differently in each — the comparison is the referenced
// Patient's FULL identifier set, sorted. Equal sets are the same record. It is
// the whole set rather than one chosen identifier because any single-key rule
// makes the answer depend on which identifier sorts first, and whether two
// patients collapse then turns on the spelling of an identifier system, which is
// no kind of property to rest a patient boundary on. Sets that differ are refused
// even when they overlap: a payer returning one member under two different
// identifier sets is itself inconsistent, and refusing is the safe reading.
//
// Written here rather than reusing the submit legs' response-graph reader, which
// validates a $submit-shaped graph of exactly one ClaimResponse and would refuse
// both an inquiry answer carrying several and a legitimate answer carrying none.
// The field set and the depth are the submission walk's; what differs is that
// this one has no single expected identity to compare against, because a payer
// answers in its own patient namespace.
//
// The second return is false for a document this cannot read at all, for a
// subject member that names nobody readable — no resolvable reference and no
// complete identifier — and for a Bundle whose addresses are not unique. The
// submission walk refuses the first two, and a subject naming nobody is not an
// answer about one patient.
//
// ONE KNOWN ASYMMETRY, deliberately left as it is. An identifier-only
// `ClaimResponse.patient` in a Bundle carrying no Patient entry keys as the
// single identifier it states, while a Patient ENTRY carrying that same lone
// identifier keys as its identifier SET; the two spellings therefore do not
// compare equal, so one patient answered about in both those shapes across two
// Bundles is refused. It fails closed, no published example uses either shape,
// and the alternative — treating a bare identifier as a whole set — would make a
// one-identifier reference equal to any record whose set happens to be that one
// identifier. Recorded rather than fixed.
func pasInquiryAnswerSubjects(answer []byte) (map[string]bool, bool) {
	var doc any
	if err := decodeMessage(answer, &doc); err != nil {
		return nil, false
	}
	members := map[string]bool{}
	for _, bundle := range inquiryAnswerScopes(doc) {
		scope := newInquiryPatientScope(bundle)
		local := map[string]bool{}
		if !scope.collect(bundle, local) {
			return nil, false
		}
		if len(local) > 1 {
			// Two patients inside ONE Bundle. Their addresses are distinct by
			// construction, so carrying them through as addresses keeps the answer
			// inconsistent whatever any other Bundle says — and no identifier can
			// collapse them on the way out.
			for id := range local {
				members[id] = true
			}
			continue
		}
		for id := range local {
			members[scope.crossBundleKey(id)] = true
		}
	}
	return members, true
}

// inquiryAnswerScopes returns each resolution scope in an answer: the response
// Bundle itself, every response Bundle of a 2.2.1 `Parameters` — under the
// declared output name AND under the recorded nonconformant one, because a
// Bundle this gateway reads for the ledger is a Bundle whose subject it must
// also check — or the bare document when it is neither (a lone ClaimResponse
// resolves nothing, and its references stand as written).
func inquiryAnswerScopes(doc any) []map[string]any {
	root, ok := doc.(map[string]any)
	if !ok {
		return nil
	}
	if rt, _ := root["resourceType"].(string); rt != "Parameters" {
		return []map[string]any{root}
	}
	var out []map[string]any
	params, _ := root["parameter"].([]any)
	for _, p := range params {
		param, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _ := param["name"].(string)
		if carries, _ := pasInquiryOutputCarries(name); !carries {
			continue
		}
		if res, ok := param["resource"].(map[string]any); ok {
			out = append(out, res)
		}
	}
	return out
}

// inquiryPatientScope is one response Bundle's own reference-resolution table.
//
// address maps every spelling that names one of the Bundle's entries — its
// `fullUrl`, its `Type/id`, and for a Patient each business identifier it states
// — to that entry's ADDRESS. The address is the identity inside this Bundle; an
// identifier is a way of NAMING an entry here, never a way of deciding that two
// entries are one.
//
// identifiers holds each Patient entry's full sorted identifier set, by address.
// That set, and nothing smaller, is what says whether the patient one Bundle
// answered about is the patient another answered about.
//
// A spelling that names MORE THAN ONE entry — a repeated `fullUrl`, a repeated
// `Type/id`, or one identifier stated by two Patient entries — makes the whole
// Bundle unreadable rather than resolving to whichever entry was indexed last.
// Which patient an answer is about must not depend on iteration order, and a
// Bundle whose addresses are not unique is malformed at the FHIR level anyway.
type inquiryPatientScope struct {
	address     map[string]string
	identifiers map[string]string
	// malformed: a spelling names more than one entry (see above).
	malformed bool
}

// inquirySubjectFields are the members that name a resource's primary subject —
// the submission walk's set, so the two agree on WHICH references bind.
var inquirySubjectFields = map[string]bool{
	"patient": true, "subject": true, "beneficiary": true, "for": true,
	"subjectReference": true, "patientReference": true,
}

func newInquiryPatientScope(bundle map[string]any) *inquiryPatientScope {
	s := &inquiryPatientScope{
		address:     map[string]string{},
		identifiers: map[string]string{},
	}
	seen := map[string]bool{}
	entries, _ := bundle["entry"].([]any)
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		res, ok := entry["resource"].(map[string]any)
		if !ok {
			continue
		}
		fullURL, _ := entry["fullUrl"].(string)
		typ, _ := res["resourceType"].(string)
		id, _ := res["id"].(string)
		// Where this entry lives. Only where it lives: a Patient's own identifier
		// names it, but never redefines which entry it is.
		address := fullURL
		if address == "" && typ != "" && id != "" {
			address = typ + "/" + id
		}
		if address == "" {
			continue // nothing in the Bundle can name this entry
		}
		// Two entries may not share an address: a repeated fullUrl or Type/id
		// leaves every reference to it naming two resources at once.
		//
		// The two spellings are deduplicated per entry first. An entry whose
		// relative fullUrl IS its own `Type/id` states one address twice, which is
		// a nonconformant fullUrl and not an ambiguity — nothing about it leaves a
		// reference naming two resources.
		spellings := map[string]bool{}
		for _, spelling := range []string{stripHistoryRef(fullURL), typ + "/" + id} {
			if spelling == "" || spelling == "/" {
				continue
			}
			spellings[spelling] = true
		}
		for spelling := range spellings {
			if seen[spelling] {
				s.malformed = true
			}
			seen[spelling] = true
		}
		s.register(stripHistoryRef(fullURL), address)
		if typ != "" && id != "" {
			s.register(typ+"/"+id, address)
		}
		if typ == "Patient" {
			keys := patientIdentifierKeys(res)
			for _, key := range keys {
				s.register(key, address)
			}
			if len(keys) > 0 {
				s.identifiers[address] = "patient-identifiers|" + strings.Join(keys, ",")
			}
		}
	}
	return s
}

// register maps one spelling to the entry it names. A second entry claiming it —
// two Patient records stating one identifier — makes the Bundle unreadable rather
// than letting the spelling resolve to one of them.
func (s *inquiryPatientScope) register(spelling, address string) {
	if spelling == "" {
		return
	}
	if existing, seen := s.address[spelling]; seen && existing != address {
		s.malformed = true
		return
	}
	s.address[spelling] = address
}

// resolve returns the address a spelling names, and whether this Bundle names it
// at all.
func (s *inquiryPatientScope) resolve(spelling string) (string, bool) {
	address, ok := s.address[spelling]
	return address, ok
}

// crossBundleKey is how one Bundle's patient is compared with another's: the
// referenced Patient's full identifier set when it states one, else the address
// itself — two Bundles that address one patient differently and carry no
// identifier for it have nothing left to say they are the same.
func (s *inquiryPatientScope) crossBundleKey(identity string) string {
	if set, ok := s.identifiers[identity]; ok {
		return set
	}
	return identity
}

// collect walks one scope and records every patient identity it names, by
// address. It reports false when a subject member names nobody readable.
func (s *inquiryPatientScope) collect(bundle map[string]any, members map[string]bool) bool {
	readable := true
	var walk func(v any, owner string, depth int)
	walk = func(v any, owner string, depth int) {
		switch v := v.(type) {
		case map[string]any:
			if rt, _ := v["resourceType"].(string); rt == "Patient" {
				// A Patient entry IS its address; a contained Patient belongs to
				// the resource containing it. FHIR requires a contained resource to
				// carry an id, so one without an id is malformed and gets an
				// identity of its own rather than being folded into another's.
				if depth == 0 {
					members[owner] = true
				} else {
					id, _ := v["id"].(string)
					members[owner+"#"+id] = true
				}
			}
			// A typed Patient reference binds wherever it sits, not only in a
			// subject member: a polymorphic path (an actor, an extension's
			// valueReference) names a patient just as plainly.
			if typ, _ := v["type"].(string); typ == "Patient" || typ == "http://hl7.org/fhir/StructureDefinition/Patient" {
				if id := s.referenceIdentity(v, owner); id != "" {
					members[id] = true
				} else {
					// It says it points at a patient and then names none: the same
					// refusal a subject member gets, for the same reason. The
					// submission walk refuses this too.
					readable = false
				}
			}
			for key, val := range v {
				if inquirySubjectFields[key] {
					refs := referenceObjects(val)
					if len(refs) == 0 {
						readable = false // a subject member that is not a Reference at all
					}
					for _, ref := range refs {
						id := s.referenceIdentity(ref, owner)
						if id == "" {
							// No resolvable reference and no complete identifier:
							// a display string, or an identifier missing its
							// system, names nobody. The submission walk refuses
							// both, and so does this.
							readable = false
							continue
						}
						members[id] = true
					}
				}
				walk(val, owner, depth+1)
			}
		case []any:
			for _, e := range v {
				walk(e, owner, depth+1)
			}
		}
	}
	entries, _ := bundle["entry"].([]any)
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		res, ok := entry["resource"].(map[string]any)
		if !ok {
			continue
		}
		fullURL, _ := entry["fullUrl"].(string)
		owner := fullURL
		if owner == "" {
			typ, _ := res["resourceType"].(string)
			id, _ := res["id"].(string)
			owner = typ + "/" + id
		}
		walk(res, owner, 0)
	}
	// A document that is not a Bundle (a lone ClaimResponse) has no entries; walk
	// it as its own owner so its references are still read.
	if len(entries) == 0 {
		if rt, _ := bundle["resourceType"].(string); rt != "" && rt != "Bundle" {
			id, _ := bundle["id"].(string)
			walk(bundle, rt+"/"+id, 0)
		}
	}
	// A Bundle whose addresses are not unique is unreadable: see the type.
	return readable && !s.malformed
}

// referenceIdentity resolves one Reference to the address it names, within this
// scope. "" when it names nobody readable — neither a resolvable reference nor a
// complete identifier.
func (s *inquiryPatientScope) referenceIdentity(ref map[string]any, owner string) string {
	if literal, _ := ref["reference"].(string); literal != "" {
		r := stripHistoryRef(literal)
		if strings.HasPrefix(r, "#") {
			return owner + r
		}
		if address, ok := s.resolve(r); ok {
			return address
		}
		// Unresolvable: its own identity, as written. Never silently equal to a
		// reference of another form.
		return r
	}
	// An identifier-only logical reference names a patient just as a literal one
	// does; reading it as nothing is how a second member slips past. It names the
	// entry carrying that identifier when the Bundle has one, and otherwise stands
	// as an identity of its own.
	if key := identifierRefKey(ref); key != "" {
		if address, ok := s.resolve(key); ok {
			return address
		}
		return key
	}
	return ""
}

// stripHistoryRef drops a `/_history/…` suffix: a version-specific reference
// names the same resource as the version-less one.
func stripHistoryRef(ref string) string {
	if i := strings.Index(ref, "/_history/"); i >= 0 {
		return ref[:i]
	}
	return ref
}

// patientIdentifierKeys are every business identifier a Patient states, as scope
// keys, sorted and deduplicated — so the SET is the same whatever order a payer
// writes them in, and comparing two sets is comparing two patients.
func patientIdentifierKeys(patient map[string]any) []string {
	ids, _ := patient["identifier"].([]any)
	seen := map[string]bool{}
	var out []string
	for _, i := range ids {
		if id, ok := i.(map[string]any); ok {
			if key := identifierKeyOf(id); key != "" && !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	sort.Strings(out)
	return out
}

// identifierRefKey is the scope key of a Reference's own `identifier`.
func identifierRefKey(ref map[string]any) string {
	id, _ := ref["identifier"].(map[string]any)
	return identifierKeyOf(id)
}

// identifierKeyOf is "patient-identifier|<system>|<value>" — prefixed so it can
// never collide with a reference spelling, and empty unless both halves are
// stated (a half-stated identifier names nobody).
func identifierKeyOf(id map[string]any) string {
	if id == nil {
		return ""
	}
	system, _ := id["system"].(string)
	value, _ := id["value"].(string)
	if strings.TrimSpace(system) == "" || strings.TrimSpace(value) == "" {
		return ""
	}
	return "patient-identifier|" + system + "|" + value
}

// referenceObjects reads a Reference, or a list of them, as objects (the
// identifier form matters, so the string alone is not enough).
func referenceObjects(v any) []map[string]any {
	switch v := v.(type) {
	case map[string]any:
		return []map[string]any{v}
	case []any:
		var out []map[string]any
		for _, e := range v {
			out = append(out, referenceObjects(e)...)
		}
		return out
	}
	return nil
}

// consistentPASInquiryAnswerSubjects reports whether every patient an inquiry's
// answer names is the same one. An answer that names NONE is consistent: "no such
// authorization" is an answer, and the inquiry response Bundle profile allows a
// Bundle with no ClaimResponse at all.
func consistentPASInquiryAnswerSubjects(answer []byte) bool {
	members, ok := pasInquiryAnswerSubjects(answer)
	return ok && len(members) <= 1
}

// readPASInquiryAnswers returns every ClaimResponse the answer carries, in both
// answer shapes, each with the lookup keys it states in the REQUESTER's
// namespace.
//
// The keys are only what the payer's own response carried: the identifiers it
// echoed for the claim that was submitted (`request.identifier`), its own
// `ClaimResponse.identifier`s, the pre-authorization reference, and the item
// trace numbers. The INQUIRY's own `Claim.identifier` is deliberately not among
// them — at 2.2.1 that field is the inquiry's own trace number, and the IG says
// it is not used to search for previous authorizations, so matching on it would
// resolve a follow-up by a fact the inquiry minted for itself.
func readPASInquiryAnswers(requesterHolder string, body []byte) []inquiryAnswerResponse {
	kind, returns, _, err := inquiryAnswerShape(body)
	if err != nil {
		return nil
	}
	switch kind {
	case "Parameters":
		var out []inquiryAnswerResponse
		for _, r := range returns {
			out = append(out, readPASInquiryAnswers(requesterHolder, r)...)
		}
		return out
	case "Bundle":
		var probe struct {
			Entry []struct {
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		}
		if decodeMessage(body, &probe) != nil {
			return nil
		}
		var out []inquiryAnswerResponse
		for _, e := range probe.Entry {
			var head struct {
				ResourceType string `json:"resourceType"`
			}
			if decodeMessage(e.Resource, &head) != nil || head.ResourceType != "ClaimResponse" {
				continue
			}
			out = append(out, claimResponseLookup(requesterHolder, e.Resource))
		}
		return out
	case "ClaimResponse":
		return []inquiryAnswerResponse{claimResponseLookup(requesterHolder, body)}
	}
	return nil
}

// claimResponseLookup reads one ClaimResponse's lookup keys and its payer-stated
// date.
func claimResponseLookup(requesterHolder string, raw []byte) inquiryAnswerResponse {
	var cr struct {
		Identifier []struct {
			System string `json:"system"`
			Value  string `json:"value"`
		} `json:"identifier"`
		Request struct {
			Identifier struct {
				System string `json:"system"`
				Value  string `json:"value"`
			} `json:"identifier"`
		} `json:"request"`
		PreAuthRef string `json:"preAuthRef"`
		Created    string `json:"created"`
		Item       []struct {
			Extension []json.RawMessage `json:"extension"`
		} `json:"item"`
	}
	out := inquiryAnswerResponse{raw: raw, keys: PendKeys{RequesterHolder: requesterHolder}}
	if decodeMessage(raw, &cr) != nil {
		return out
	}
	for _, id := range cr.Identifier {
		if k := identifierKey(id.System, id.Value); k != "" {
			out.keys.ClaimResponseIDs = append(out.keys.ClaimResponseIDs, k)
		}
	}
	if k := identifierKey(cr.Request.Identifier.System, cr.Request.Identifier.Value); k != "" {
		out.keys.RequestIDs = append(out.keys.RequestIDs, k)
	}
	out.keys.PreAuthRef = strings.TrimSpace(cr.PreAuthRef)
	for _, it := range cr.Item {
		if k := identifierExtensionKey(it.Extension, pasExtItemTraceNumberURL); k != "" {
			out.keys.ItemTraceNumbers = append(out.keys.ItemTraceNumbers, k)
		}
	}
	if t, err := time.Parse(time.RFC3339, cr.Created); err == nil {
		out.created = t.UTC()
	}
	return out
}

// ---- the payer gateway's native forward ----

// handlePASInquireNative forwards an inquiry to the payer's own
// `Claim/$inquire`, using the per-line endpoint evidence exactly as the submit
// leg does, and relays whatever comes back with the payer's own media type.
//
// The only change it may make to the request is the registered payer-identity
// restamp; the answer is never edited, never re-shaped and never certified by
// this gateway.
func (n *nativeResponder) handlePASInquireNative(ctx context.Context, contract string, in relay.Body) (LegResult, error) {
	const fhirJSON = "application/fhir+json"
	forward, refused, err := n.payorEdgeRequest(in, payorEdgePASBundle, fhirJSON)
	if err != nil {
		return LegResult{}, err
	}
	if refused.Status != 0 {
		return refused, nil
	}
	// The PAS base, not the shared one: $inquire is a PAS operation, and a payer
	// that serves its PAS operations from their own base serves this one there
	// too. A requester reaches such a payer's $submit and not its $inquire — the
	// same failure one operation over, on the only leg by which a decision made
	// later ever arrives. Read through the accessor, because several tests build
	// this responder as a struct literal and skip the constructor's defaults;
	// behaviour is unchanged unless WithPASBaseURL was applied.
	inquireURL := n.resolvedURL(ctx, contract, n.pasBase(), "/Claim/$inquire")
	up, bad, err := n.post(ctx, inquireURL, "", forward, "pas-claim-inquire", "PAS inquiry")
	if err != nil {
		return LegResult{}, err // no-response fault → engine 500
	}
	if bad.Status != 0 {
		return bad, nil // the payer's own application error, relayed
	}
	// The payer answers in its OWN patient namespace, as it does on every other
	// relayed leg, so the response member-fence stands down and only the answer's
	// internal consistency is checked.
	return LegResult{Response: relay.Exact(up.body, up.contentType), ResponseSubjectForeign: true}, nil
}

// ---- the payer gateway's inbound handler ----

// handlePASInquireInbound serves the inquiry leg payer-side.
//
// Authority is evaluated independently here, as on every leg: the inquiry must
// bind to one member (parsePASInquiryFacts, over EVERY Patient it names) and that
// member must resolve to the inbound token's own subject. Nothing about the
// authority is inherited from the submit that created the authorization — a
// requester holding a submit token cannot ask about the authorization with it,
// because the catalog pins this leg's own operation into the token binding.
//
// The ledger effect is derived AFTER the answer is in hand and BEFORE the response
// leg is sealed, so the answer the requester receives and the decision the ledger
// records are the same answer. It follows the same order the submit leg uses:
// fence, egress-$validate the gateway's own side-effects, build the response leg,
// then write holder state — a response-leg failure can never leave a recorded
// decision the requester never received.
func (g *Gateway) handlePASInquireInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, bundleJSON []byte, answerTok string) {
	facts, status, msg := parsePASInquiryFacts(bundleJSON)
	if status != 0 {
		g.refuseInbound(w, r, legPASClaimInquire, env, tok, answerTok, status, msg, nil)
		return
	}
	pci, found, readErr := g.resolveSubjectPCI(r.Context(), facts.member, bundleJSON)
	if writeSoRFailure(w, readErr) {
		return
	}
	if !found {
		g.refuseInbound(w, r, legPASClaimInquire, env, tok, answerTok, http.StatusBadRequest, refusalUnknownMember, nil)
		return
	}
	if pci != tok.Subject {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "token subject does not match request patient"})
		return
	}
	boundPatientRef := "Patient/" + facts.member

	capture := &nativeCertificationCapture{}
	// The payer's own departures from the operation it answered, reported with the
	// answer they describe. Filled once the answer is in hand, below.
	var answerNonconformance []string
	defer func() {
		if capture.attempted {
			g.certificationPair("pas-claim-inquire", "payer-native", env.Metadata.CorrelationID, shnsdk.LineOf(answerTok), capture.request, capture.response, answerNonconformance...)
		}
	}()
	observationContext := context.WithValue(r.Context(), nativeCertificationKey{}, capture)
	result, err := g.cfg.Responder.Handle(observationContext, "pas-claim-inquire", env.Metadata.CorrelationID, tok.Subject, bundleJSON)
	committed := false
	defer func() {
		if !committed && result.Rollback != nil {
			result.Rollback()
		}
	}()
	if err != nil {
		g.responderFailed(w, "pas-claim-inquire", err)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, "payer-coverage", "pas-inquire-response", "pas-claim-inquire",
			env.Metadata.CorrelationID, result, tok.Subject, env.Metadata.Sender, "", answerTok)
		return
	}
	responseFHIR, err := g.admit(result.Response, answerKey("pas-claim-inquire", relay.OutcomeAnswered))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errOwnershipFault})
		return
	}
	if bad := validatePASInquiryAnswer(responseFHIR); bad.Status != 0 {
		writeJSON(w, bad.Status, map[string]string{"error": bad.Message})
		return
	}
	// Whatever the payer's answer departs from, stated before anything is derived
	// from it. Reported IMMEDIATELY rather than with the ledger's events: the
	// payer deviated whatever this exchange does next, and an operator asking why
	// a decision went unrecorded is asking about exactly the runs that fail later.
	answerNonconformance = g.reportInquiryAnswerNonconformance(env.Metadata.CorrelationID, env.Metadata.Sender, responseFHIR)
	// The ledger effect, derived from the answer. The EOBs it produces join the
	// gateway's own side-effects, so they are member-fenced and egress-$validated
	// below exactly as the submit leg's decision EOB is.
	ledgerCommit, events := g.inquiryLedgerEffect(env.Metadata.Sender, tok.Subject, boundPatientRef, env.Metadata.CorrelationID, facts, responseFHIR, &result)
	if status, msg := g.fenceResponseSubject("pas-claim-inquire", boundPatientRef, result); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// The direction flips here, as it does on every inbound leg that answers with
	// bytes it built itself. handleInbound tagged this context with the peer's
	// inquiry — right for everything above — but everything validated from here
	// on is THIS participant's own build: the answer when this gateway produced
	// it (validatePASResult certifies only that case; a relayed answer returns
	// early, being the payer's own message) and the ledger's EOBs below. Retag
	// rather than re-tag from scratch, so the leg and the correlation id
	// handleInbound resolved are carried, not re-derived.
	fc := findingContextFrom(r.Context())
	fc.Whose = "own"
	ctx := withFindingContext(r.Context(), fc)
	if status, msg := g.validatePASResult(ctx, result, answerTok, "pas-claim-inquire"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	for _, b := range result.SideEffectFHIR {
		if status, msg := g.validateFHIR(ctx, b, "egress", ""); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}
	// Stamp honesty, as on every relaying leg: a verbatim foreign relay is left
	// unstamped, because this build did not produce the payer's bytes and cannot
	// vouch for the line they were built at. The answer's media type is the payer's
	// own.
	stampTok := stampForBuiltAnswer(result, answerTok)
	respBytes, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-inquire-response", "pas-claim-inquire", env.Metadata.CorrelationID,
		result.Response, answerKey("pas-claim-inquire", relay.OutcomeAnswered),
		g.successFrame(env.Metadata.Sender, result.ResponseContentType(), stampTok),
		tok.Subject, env.Metadata.Sender, "")
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if ledgerCommit != nil {
		if err := ledgerCommit(); err != nil {
			// The established holder-write contract for the PAS legs: a failed write
			// is reported, never swallowed. The payer's bytes are not sent in that
			// case, and the requester can ask again — an inquiry is a read, so
			// repeating it is safe.
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed"})
			return
		}
	}
	if result.Commit != nil {
		if err := result.Commit(); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed"})
			return
		}
	}
	committed = true
	for _, e := range events {
		g.observe(e)
	}
	writeLeg(w, respBytes)
}

// inquiryLedgerEffect derives this payer gateway's own pend-ledger effect from the
// answer, and returns the write to run once the response leg is sealed plus the
// observer events the ledger's transitions raised.
//
// For every ClaimResponse the answer carries:
//   - exactly one match whose state the payer has now decided → RecordDecision,
//     with the decision's ExplanationOfBenefit in the same write;
//   - no match, an ambiguous match, or a match the payer still reports as pended →
//     NO ledger change.
//
// A match that resolves to another patient's authorization changes nothing either:
// this inquiry is bound to one subject, and an answer about a different one is not
// this requester's to decide.
//
// A Store with no pend ledger has no inquiry entry point at all: the answer relays
// and no state moves.
//
// legCorrID is THIS exchange's correlation, and it is what every event below is
// tied to — an event an operator cannot trace back to the exchange that raised it
// answers no question. Where the authorization the event is about has a
// correlation of its own, that one rides in Detail; a failed lookup has none to
// report, which is exactly the case that would otherwise carry an empty id.
func (g *Gateway) inquiryLedgerEffect(requester, subject, boundPatientRef, legCorrID string, facts pasInquiryFacts, answer []byte, result *LegResult) (func() error, []ObserverEvent) {
	ledger, ok := LedgerOf(g.cfg.Store)
	if !ok || strings.TrimSpace(requester) == "" {
		return nil, nil
	}
	type decision struct {
		subjectPCI string
		corrID     string
		outcome    string
		decidedAt  time.Time
		eob        *EOBRecord
	}
	var (
		decisions []decision
		events    []ObserverEvent
	)
	for _, ans := range readPASInquiryAnswers(requester, answer) {
		if len(ProbePendKeys(ans.keys)) == 0 {
			continue // the answer states nothing a follow-up could have named
		}
		subjectPCI, corrID, found, ambiguous, err := ledger.LookupPended(requester, ans.keys)
		switch {
		case err != nil:
			// The ledger could not be consulted. The payer's answer still reaches
			// the requester — the relay is not the ledger's to withhold — and the
			// operator sees why nothing was recorded.
			g.noteStoreError(storeErrPended)
			events = append(events, ObserverEvent{Kind: "pend.lookup-unavailable", Direction: "ingress",
				LegType: "pas-claim-inquire", CorrelationID: legCorrID, Op: "pas-inquire"})
			continue
		case ambiguous, !found, subjectPCI != subject:
			continue
		}
		outcome, parsed, decided := inquiryDecisionOf(ans.raw)
		if !decided {
			continue // still pended, or an answer that states no decision
		}
		eob, eobErr := g.inquiryDecisionEOB(subjectPCI, corrID, boundPatientRef, facts, ans, parsed)
		if eobErr != nil {
			// The payer's own decision detail cannot be stated on a decision EOB.
			// Refusing the RECORD, not the relay: the answer still reaches the
			// requester, and nothing half-stated is written.
			events = append(events, ObserverEvent{Kind: "pend.decision-not-recorded", Direction: "ingress",
				LegType: "pas-claim-inquire", CorrelationID: legCorrID, Op: "pas-inquire",
				Detail: "authorization " + corrID + ": " + eobErr.Error()})
			continue
		}
		if eob != nil {
			result.SideEffectFHIR = append(result.SideEffectFHIR, eob.JSON)
		}
		decisions = append(decisions, decision{subjectPCI: subjectPCI, corrID: corrID, outcome: outcome, decidedAt: ans.created, eob: eob})
	}
	if len(decisions) == 0 {
		return nil, events
	}
	commit := func() error {
		for _, d := range decisions {
			tr, err := ledger.RecordDecision(d.subjectPCI, d.corrID, d.outcome, d.decidedAt, d.eob)
			if err != nil {
				return err
			}
			if tr.Event != "" {
				g.observe(ObserverEvent{Kind: tr.Event, Direction: "ingress", LegType: "pas-claim-inquire",
					CorrelationID: legCorrID, Op: "pas-inquire", Detail: "authorization " + d.corrID})
			}
		}
		return nil
	}
	return commit, events
}

// inquiryDecisionOf classifies one ClaimResponse from an inquiry's answer: the
// ledger outcome it records, the decision detail its EOB is built from, and
// whether it is a decision at all. A response the payer still reports as pended,
// or one this gateway cannot read as a decision, decides nothing.
func inquiryDecisionOf(raw []byte) (string, shnsdk.PriorAuthResult, bool) {
	if pended, _, err := shnsdk.ParsePendedResponse(raw); err != nil || pended {
		return "", shnsdk.PriorAuthResult{}, false
	}
	parsed, err := shnsdk.ParseClaimResponse(raw)
	if err != nil {
		return "", shnsdk.PriorAuthResult{}, false
	}
	switch parsed.Outcome {
	case "approved":
		return PendOutcomeApproved, parsed, true
	case "denied":
		return PendOutcomeDenied, parsed, true
	}
	return "", shnsdk.PriorAuthResult{}, false
}

// inquiryDecisionEOB projects the decision EOB that is written in the SAME write
// as the decision. Its product coding comes from the REQUESTER's own inquiry —
// the item line whose trace number the payer echoed, else the first line it asked
// about — never from a code this gateway chose. A line with no recognizable
// product coding yields no EOB, exactly as a submit with none does: the decision
// is still recorded, and nothing is invented to carry it.
func (g *Gateway) inquiryDecisionEOB(subjectPCI, corrID, boundPatientRef string, facts pasInquiryFacts, ans inquiryAnswerResponse, parsed shnsdk.PriorAuthResult) (*EOBRecord, error) {
	item := inquiryItemFor(facts, ans.keys.ItemTraceNumbers)
	if item.code == "" {
		return nil, nil
	}
	eobJSON, err := decisionEOB(g.cfg.Clock, corrID, boundPatientRef, item.system, item.code, item.display, parsed)
	if err != nil {
		return nil, err
	}
	return &EOBRecord{SubjectPCI: subjectPCI, EOBID: "eob-" + corrID, JSON: eobJSON}, nil
}

// inquiryItemFor picks the inquiry line an answer is about: the line whose trace
// number the answer echoed, else the first line the inquiry asked about.
func inquiryItemFor(facts pasInquiryFacts, answerTraceNumbers []string) pasInquiryItemFact {
	for _, tn := range answerTraceNumbers {
		for _, it := range facts.items {
			if it.traceNumber != "" && it.traceNumber == tn {
				return it
			}
		}
	}
	if len(facts.items) > 0 {
		return facts.items[0]
	}
	return pasInquiryItemFact{}
}

// ---- the provider gateway's ingress ----

// handlePASInquireIngress is the provider-facing `POST /Claim/$inquire`: a
// participant's own system asks its own gateway, and the gateway carries the
// inquiry onto the network exactly as sent.
//
// It binds the inquiry over EVERY Patient the Bundle names and routes by the
// Coverage(s) the inquiry carries — no default payer. The payer's answer comes
// back to the participant's system exactly as the payer wrote it.
func (g *Gateway) handlePASInquireIngress(w http.ResponseWriter, r *http.Request) {
	w, scope := g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	if g.ingressAuthRefused(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	facts, status, msg := parsePASInquiryFacts(body)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pci, found, readErr := g.resolveSubjectPCI(r.Context(), facts.member, body)
	if writeSoRFailure(w, readErr) {
		return
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	// Routed by the inquiry's OWN Coverage, the same rule the submit ingress
	// follows: a Coverage naming no resolvable payer fails closed rather than
	// defaulting to one.
	recipient, _, status, msg := g.recipientForWith(pasBundleCoverage(body), bundleRefResolver(body))
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	const leg = "pas-claim-inquire"
	scope.leg = leg
	ex := g.exchanges.Begin(workstreamPA)
	child := g.cfg.CorrelationGen()
	requestObservation := append([]byte(nil), body...)
	var responseObservation []byte
	var observedTarget string
	defer func() {
		g.certificationPair(leg, "provider-ingress", child, shnsdk.LineOf(observedTarget), requestObservation, responseObservation)
	}()
	observationContext := context.WithValue(r.Context(), certificationTargetKey{}, &observedTarget)
	request := relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), "application/fhir+json")
	content := Content{WorkstreamType: workstreamPA, Payload: request, Carried: true}
	answer, err := g.OriginateLeg(observationContext, r, recipient, leg, pci, child, "", content)
	responseObservation = append([]byte(nil), answer...)
	var relayed *RelayError
	if errors.As(err, &relayed) {
		responseObservation = append([]byte(nil), relayed.Body...)
	}
	legProj := Leg{Type: leg, Physics: paCatalog[leg].Physics, Content: content, Subjects: []string{pci}}
	if err != nil {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if bad := validatePASInquiryAnswer(answer); bad.Status != 0 {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		writeJSON(w, bad.Status, map[string]string{"error": bad.Message})
		return
	}
	// The same subject-linkage rule the submit ingress applies to a payer's
	// response: every patient the answer names, at every depth, must be the same
	// one. Shape alone would pass an answer whose Coverage beneficiary named
	// another member.
	if !consistentPASInquiryAnswerSubjects(answer) {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS inquiry answer has inconsistent patient linkage"})
		return
	}
	g.recordLeg(ex.ID, legProj.Project(child, "ok"))
	g.writePayload(w, http.StatusOK, "application/fhir+json",
		relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), "application/fhir+json"),
		relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}
