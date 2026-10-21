package pasgraph

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// inquiry.go reads a PAS inquiry answer as the gateway reads it: the shape of
// the answer and the response Bundles it carries, whether it reads at an IG
// line, and whether every patient it names is the same one. The inquiry leg
// relays by these, and a reader of retained evidence judges an answer by them
// (pa/pasanswer).

// decodeMessage reads a participant's message body for a decision: through
// relay.Decode, so a repeated member name, or a member whose name matches a
// field only when case is ignored, is refused, as the gateway's own reads
// refuse it. It is the engine's decodeMessage (engine/messagedecode.go), which
// this package cannot import.
func decodeMessage(b []byte, v any) error {
	return relay.Decode(relay.NewBody(b, relay.OriginPeerFrame), v)
}

// The output parameter a `Claim/$inquire` answer carries its response Bundles
// under.
//
// InquiryDeclaredOutput is what every published operation definition declares:
// `return`, 1..1 at PAS 2.0.1 and 2.1.0 and 0..* at 2.2.1 (verified against the
// published packages, 2026-09-18).
//
// InquiryRecordedOutput is what the Da Vinci reference payer actually sends —
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
	InquiryDeclaredOutput = "return"
	InquiryRecordedOutput = "responseBundle"
)

// ValidateInquiryAnswer checks that a 2xx answer is one of the shapes the
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
func ValidateInquiryAnswer(body []byte) (ok bool, detail string) {
	// A repeated member name is named, with where, so the payer can fix it.
	if dup, ok := jsonmember.Repeated(body); ok {
		return false, "payer inquiry answer repeats " + dup + ": it can be read two ways, so it is not carried"
	}
	kind, returns, _, err := InquiryAnswerShape(body)
	if err != nil {
		return false, "invalid prior-authorization inquiry answer"
	}
	switch kind {
	case "Bundle", "OperationOutcome":
		return true, ""
	case "Parameters":
		for _, r := range returns {
			inner, _, _, err := InquiryAnswerShape(r)
			if err != nil || (inner != "Bundle" && inner != "OperationOutcome") {
				return false, "invalid prior-authorization inquiry answer"
			}
		}
		return true, ""
	}
	return false, "invalid prior-authorization inquiry answer"
}

// ValidateInquiryAnswerAt is ValidateInquiryAnswer for an answer
// exchanged at IG line. An inquiry answer's graph is not closed at any line:
// it may name a Claim it does not carry. At 2.2.1 (§spec-44) every
// ClaimResponse it carries names the Claim it answers, by a reference to a
// Claim or by an identifier with a value, or says why it cannot by a Data
// Absent Reason on its request;
// 2.0.1 and 2.1.0 state no such rule.
func ValidateInquiryAnswerAt(body []byte, line string) (ok bool, detail string) {
	if ok, detail := ValidateInquiryAnswer(body); !ok || line != "2.2" {
		return ok, detail
	}
	kind, returns, _, _ := InquiryAnswerShape(body)
	bundles := returns
	if kind == "Bundle" {
		bundles = []json.RawMessage{body}
	}
	for _, raw := range bundles {
		var b struct {
			Entry []struct {
				FullURL  string         `json:"fullUrl"`
				Resource map[string]any `json:"resource"`
			} `json:"entry"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		types := map[string]string{}
		for _, e := range b.Entry {
			if rt, _ := e.Resource["resourceType"].(string); e.FullURL != "" {
				types[StripHistoryRef(e.FullURL)] = rt
			}
		}
		entryType := func(ref string) string { return types[StripHistoryRef(ref)] }
		for i, e := range b.Entry {
			if e.Resource["resourceType"] == "ClaimResponse" && !claimResponseNamesItsClaim(e.Resource, entryType) {
				return false, fmt.Sprintf("invalid prior-authorization inquiry answer: entry %d (ClaimResponse) names no Claim and gives no data-absent-reason for it (PAS 2.2.1)", i)
			}
		}
	}
	return true, ""
}

// claimResponseNamesItsClaim reports whether a ClaimResponse's request names
// its Claim (claimResponseRequestNamesClaim), or carries a Data Absent Reason
// saying why it does not.
func claimResponseNamesItsClaim(cr map[string]any, entryType func(ref string) string) bool {
	if ClaimResponseRequestNamesClaim(cr, entryType) {
		return true
	}
	request, _ := cr["request"].(map[string]any)
	exts, _ := request["extension"].([]any)
	for _, x := range exts {
		if m, _ := x.(map[string]any); m["url"] == "http://hl7.org/fhir/StructureDefinition/data-absent-reason" {
			return true
		}
	}
	return false
}

// InquiryOutputCarries reports whether an output parameter name carries a
// response Bundle, and whether that name deviates from the declared one.
func InquiryOutputCarries(name string) (carries, deviant bool) {
	switch name {
	case InquiryDeclaredOutput:
		return true, false
	case InquiryRecordedOutput:
		return true, true
	}
	return false, false
}

// InquiryAnswerShape reports the answer's resourceType and, for a Parameters, the
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
func InquiryAnswerShape(body []byte) (string, []json.RawMessage, []string, error) {
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
		return "", nil, nil, fmt.Errorf("pasgraph: inquiry answer names no resourceType")
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
		carries, isDeviant := InquiryOutputCarries(p.Name)
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

// InquiryAnswerSubjects returns every patient identity an inquiry's answer
// names, anywhere in it, and whether the answer is one readable document.
//
// It is the ANSWER-side twin of the engine's readPASInquiryFacts' bind, and it is deliberately
// as deep as the submit legs' response check: every object at any depth — each
// `return` Bundle, each entry, and each contained resource inside an entry — has
// its subject-bearing members read (`patient`, `subject`, `beneficiary`, `for`,
// and the Reference choice of a `subject[x]`), and every Patient resource
// contributes its own identity. Reading only the ClaimResponses would let an
// answer whose Coverage beneficiary or Patient entry names a different member
// through: one inquiry asks about one member's authorizations, so an answer that
// names two is not an answer to it.
//
// A dependent's Coverage names the parent as its subscriber or policyHolder, a
// Patient contained in the Coverage: that Coverage's party (EntryCoverageParties)
// is another person, so it is not one of the answer's patients. A contained
// Patient that does not hold as a contained resource is no party, so it is one
// of them.
// Only a Coverage that is itself an entry has a party, and only its slot naming
// that party is exempt from binding as a typed Patient reference.
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
// Written here rather than reusing the engine's submit legs' response-graph reader, which
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
func InquiryAnswerSubjects(answer []byte) (map[string]bool, bool) {
	var doc any
	if err := decodeMessage(answer, &doc); err != nil {
		return nil, false
	}
	members := map[string]bool{}
	for _, bundle := range InquiryAnswerScopes(doc) {
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

// InquiryScopeSubjects is one resolution scope's walk (InquiryAnswerScopes):
// every patient identity the scope names, by address, and false when a
// subject member names nobody readable. It, InquiryAnswerScopes and
// InquiryAnswerSubjects are exported for the engine's agreement rows
// (coverageparty_answer_agreement_test.go), which hold the inquiry answer's
// walk and the request graph to one Coverage-party reading.
func InquiryScopeSubjects(bundle map[string]any) (map[string]bool, bool) {
	members := map[string]bool{}
	ok := newInquiryPatientScope(bundle).collect(bundle, members)
	return members, ok
}

// InquiryAnswerScopes returns each resolution scope in an answer: the response
// Bundle itself, every response Bundle of a 2.2.1 `Parameters` — under the
// declared output name AND under the recorded nonconformant one, because a
// Bundle this gateway reads for the ledger is a Bundle whose subject it must
// also check — or the bare document when it is neither (a lone ClaimResponse
// resolves nothing, and its references stand as written).
func InquiryAnswerScopes(doc any) []map[string]any {
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
		if carries, _ := InquiryOutputCarries(name); !carries {
			continue
		}
		if res, ok := param["resource"].(map[string]any); ok {
			out = append(out, res)
		}
	}
	return out
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
		for _, spelling := range []string{StripHistoryRef(fullURL), typ + "/" + id} {
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
		s.register(StripHistoryRef(fullURL), address)
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

// identifierRefKey is the scope key of a Reference's own `identifier`.
func identifierRefKey(ref map[string]any) string {
	id, _ := ref["identifier"].(map[string]any)
	return identifierKeyOf(id)
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

// ConsistentInquiryAnswerSubjects reports whether every patient an inquiry's
// answer names is the same one. An answer that names NONE is consistent: "no such
// authorization" is an answer, and the inquiry response Bundle profile allows a
// Bundle with no ClaimResponse at all.
func ConsistentInquiryAnswerSubjects(answer []byte) bool {
	members, ok := InquiryAnswerSubjects(answer)
	return ok && len(members) <= 1
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
	// lone: the document walked is not a Bundle (a lone ClaimResponse, or a
	// Parameters return that is not a Bundle), so nothing it holds is an
	// entry of the answer and a Coverage it is has no party.
	lone := false
	// role is what v is to an entry-level Coverage's party rule
	// (EntryCoverageParties), as in the submission walk: a party is another
	// person, so it names no patient here (the rule itself requires that it
	// holds as a contained resource); the slot naming it is the one typed
	// Patient reference that names no patient.
	var walk func(v any, owner string, depth int, role CoveragePartyRole)
	walk = func(v any, owner string, depth int, role CoveragePartyRole) {
		switch v := v.(type) {
		case map[string]any:
			rt, isResource := v["resourceType"].(string)
			if role != RoleCoverageParty && rt == "Patient" {
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
			// valueReference) names a patient just as plainly. The one
			// exception is an entry-level Coverage's slot naming its own party.
			if typ, _ := v["type"].(string); (typ == "Patient" || typ == "http://hl7.org/fhir/StructureDefinition/Patient") && role != RoleCoveragePartySlot {
				if id := s.referenceIdentity(v, owner); id != "" {
					members[id] = true
				} else {
					// It says it points at a patient and then names none: the same
					// refusal a subject member gets, for the same reason. The
					// submission walk refuses this too.
					readable = false
				}
			}
			var parties EntryParties
			if depth == 0 && !lone && isResource && rt == "Coverage" {
				parties = EntryCoverageParties(v)
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
				if list, ok := val.([]any); ok && key == "contained" && parties.Any() {
					for i, c := range list {
						walk(c, owner, depth+2, parties.RoleOf(i))
					}
					continue
				}
				walk(val, owner, depth+1, parties.SlotRole(key, val))
			}
		case []any:
			for _, e := range v {
				walk(e, owner, depth+1, RoleNone)
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
		walk(res, owner, 0, RoleNone)
	}
	// A document that is not a Bundle (a lone ClaimResponse) has no entries; walk
	// it as its own owner so its references are still read.
	if len(entries) == 0 {
		if rt, _ := bundle["resourceType"].(string); rt != "" && rt != "Bundle" {
			id, _ := bundle["id"].(string)
			lone = true
			walk(bundle, rt+"/"+id, 0, RoleNone)
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
		r := StripHistoryRef(literal)
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

// A dependent's Coverage names the parent as its subscriber or policyHolder,
// often as a Patient contained in the Coverage, carrying only an MRN. A PAS
// answer (a $submit response, an inquiry's answer) carries the Coverage the
// payer answered for, so the subject rules over it (the engine's
// pasGraphSubjectMismatch, inquiryPatientScope.collect) read the parent as the Coverage's party,
// another person, rather than as a second patient. They decide it as the
// sdk's PAS response check does: by shnsdk.CoverageParty, for the contained
// list of a Coverage that is itself an entry of the answer. The rule also
// requires that the party holds as a contained resource (it contains
// nothing, its identifier is a list, and it spells the members the rule
// reads exactly), so a contained Patient that does not is no party and is
// read as the patient.

// CoveragePartyRole is what an object is to an entry-level Coverage's party
// rule.
type CoveragePartyRole uint8

const (
	RoleNone CoveragePartyRole = iota
	// RoleCoverageParty is a Coverage's party: its identity is not read as
	// the patient's. Its references are read as any other resource's.
	RoleCoverageParty
	// RoleCoveragePartySlot is the Coverage's subscriber or policyHolder
	// whose own reference names its party: the one typed Patient reference
	// that names another person.
	RoleCoveragePartySlot
)

// EntryParties are an entry-level Coverage's parties: by index into its
// contained list, and the local references naming them.
type EntryParties struct {
	byIndex []bool
	refs    map[string]bool
}

// EntryCoverageParties returns the parties of cov, an entry-level Coverage:
// each resource in its own contained list that shnsdk.CoverageParty names
// the Coverage's party. The rule requires the party's id to be unique among
// the Coverage's contained resources ("#id" then names one resource), so a
// contained Patient sharing its id with another is read as the patient.
func EntryCoverageParties(cov map[string]any) EntryParties {
	list, _ := cov["contained"].([]any)
	var p EntryParties
	for i, c := range list {
		cr, ok := c.(map[string]any)
		if !ok || !shnsdk.CoverageParty(cov, cr) {
			continue
		}
		if p.refs == nil {
			p.byIndex = make([]bool, len(list))
			p.refs = map[string]bool{}
		}
		p.byIndex[i] = true
		p.refs["#"+cr["id"].(string)] = true
	}
	return p
}

// Any reports whether the Coverage has a party.
func (p EntryParties) Any() bool { return p.refs != nil }

// RoleOf is the role of the contained resource at index i.
func (p EntryParties) RoleOf(i int) CoveragePartyRole {
	if i < len(p.byIndex) && p.byIndex[i] {
		return RoleCoverageParty
	}
	return RoleNone
}

// SlotRole is the role of the Coverage's member field, value v: a
// subscriber or policyHolder whose own reference names a party is that
// party's slot.
//
// The field check is redundant, kept as defence in depth: shnsdk.CoverageParty
// names a party only when nothing in the Coverage but its subscriber or
// policyHolder references it, so no other member's own reference can name a
// party here. No row can tell the check apart, so none pins it; were the rule
// to loosen, the check still keeps the exemption to the two slots.
func (p EntryParties) SlotRole(field string, v any) CoveragePartyRole {
	if p.refs == nil || (field != "subscriber" && field != "policyHolder") {
		return RoleNone
	}
	slot, _ := v.(map[string]any)
	if ref, _ := slot["reference"].(string); p.refs[ref] {
		return RoleCoveragePartySlot
	}
	return RoleNone
}
