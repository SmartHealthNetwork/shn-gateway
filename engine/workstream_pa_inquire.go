// workstream_pa_inquire.go — the Da Vinci prior-authorization inquiry leg (pas-claim-inquire):
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
// stands down for a relayed answer (and an inquiry's answer is always relayed);
// on the provider hop the inquiry is carried as sent.
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
	"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// pasExtItemTraceNumberURL is the Da Vinci PAS item trace-number extension. A
// requester's own trace numbers are what a payer echoes, so they are the keys an
// inquiry's answer is matched back by.
const pasExtItemTraceNumberURL = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber"

// ---- the request: subject bind ----

// pasInquiryFacts is what the inquiry bind extracted: the one member every
// patient reference in the inquiry names.
type pasInquiryFacts struct {
	member string
}

// parsePASInquiryFacts is readPASInquiryFacts refusing every defect it finds.
// No request path calls it: the provider ingress and the payer side both judge
// content by their level through readPASInquiryFacts. It stays as the
// whole-strictness read the inquiry tests pin.
func parsePASInquiryFacts(bundleJSON []byte) (pasInquiryFacts, int, string) {
	return readPASInquiryFacts(bundleJSON, func(string) bool { return true })
}

// readPASInquiryFacts reads a Da Vinci PAS inquiry request Bundle and binds it to
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
//
// The content defects are handed to refuses, which reports whether the defect
// refuses the inquiry. The checks run in the same order whatever it answers, so
// a caller whose refuses always answers true refuses every defect it finds.
//
// What refuses whatever refuses answers: an inquiry that cannot be read, is not
// a Bundle, or has an entry or Claim that cannot be read (the subject cannot be
// read, and a repeated member name is read one way only), a Patient entry
// whose id cannot be read, and one with no Claim.patient (the subject,
// RuleSubjectPCI). The content defects, handed to
// refuses: a Bundle that is not a collection, an entry carrying a request or
// response, a value of the wrong type off the subject path, and a Claim that
// is not a preauthorization (RuleRequestShape), and
// another patient named inside the inquiry (RulePatientMixed). A defect that
// does not refuse leaves the inquiry bound to its Claim's patient.
func readPASInquiryFacts(bundleJSON []byte, refuses func(rule string) bool) (pasInquiryFacts, int, string) {
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
	// The entries (their resources and fullUrls, the subject path) and the
	// resourceType must read; a value of the wrong type elsewhere is the
	// inquiry's own shape (RuleRequestShape).
	var probeCore struct {
		ResourceType string `json:"resourceType"`
		Entry        []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := decodeContent(bundleJSON, &probe, &probeCore, func() bool { return refuses(RuleRequestShape) }); err != nil {
		return pasInquiryFacts{}, http.StatusBadRequest, "parse inquiry bundle failed"
	}
	if probe.ResourceType != "Bundle" {
		return pasInquiryFacts{}, http.StatusBadRequest, "PAS inquiry is not a Bundle"
	}
	if probe.Type != "collection" && refuses(RuleRequestShape) {
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
		if (len(e.Request) > 0 || len(e.Response) > 0) && refuses(RuleRequestShape) {
			return pasInquiryFacts{}, http.StatusBadRequest, "PAS inquiry Bundle entries carry no request or response"
		}
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		// The entry's resourceType must read. A Patient entry's own id is a
		// patient identity this inquiry names, so it must read too; the id of
		// any other entry is the inquiry's own shape (RuleRequestShape).
		// decodeContent reads headCore before it asks, so the callback sees
		// the entry's type.
		var headCore struct {
			ResourceType string `json:"resourceType"`
		}
		if err := decodeContent(e.Resource, &head, &headCore, func() bool {
			return headCore.ResourceType == "Patient" || refuses(RuleRequestShape)
		}); err != nil {
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
			// Item is read for its shape alone: nothing uses its values, but a
			// line of the wrong type is the inquiry's own shape, which a level
			// that refuses RuleRequestShape refuses.
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
		// Claim.patient (the subject) must read; a value of the wrong type
		// elsewhere on the Claim is the inquiry's own shape.
		var claimCore struct {
			Patient struct {
				Reference string `json:"reference"`
			} `json:"patient"`
		}
		if err := decodeContent(e.Resource, &claim, &claimCore, func() bool { return refuses(RuleRequestShape) }); err != nil {
			return pasInquiryFacts{}, http.StatusBadRequest, "parse inquiry Claim failed"
		}
		if claim.Use != "preauthorization" && refuses(RuleRequestShape) {
			return pasInquiryFacts{}, http.StatusBadRequest, "PAS inquiry Claim is not a preauthorization"
		}
		if claim.Patient.Reference == "" {
			return pasInquiryFacts{}, http.StatusBadRequest, "PAS inquiry Claim missing patient"
		}
		facts.member = patientMemberFromRef(claim.Patient.Reference)
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
	// The bind: one member across the whole Bundle.
	if (len(members) != 1 || !members[facts.member]) && refuses(RulePatientMixed) {
		return pasInquiryFacts{}, http.StatusForbidden, "inconsistent patient in PAS inquiry"
	}
	// And no resource it carries, contained or nested, is or names another
	// patient, as on the submit legs (pasCarriesAnotherPatient).
	if pasCarriesAnotherPatient(bundleJSON, facts.member) && refuses(RulePatientMixed) {
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

// inquiryAnswerRead is how an inquiry answer reads at its candidate lines
// (FR-G29's order): at the first whose rule (validatePASInquiryAnswerAt) it
// meets.
func inquiryAnswerRead(lines []string, body []byte) answerLineRead {
	return readAtCandidateLines(lines, func(line string) bool {
		return validatePASInquiryAnswerAt(body, line).Status == 0
	})
}

// pasInquiryAnswerRead is inquiryAnswerRead over this gateway's candidate
// lines for an answer on the leg token names; the zero read when it names no
// line.
func (g *Gateway) pasInquiryAnswerRead(token string, body []byte) answerLineRead {
	contract, line, ok := strings.Cut(token, "@")
	if !ok || line == "" {
		return answerLineRead{}
	}
	return inquiryAnswerRead(g.answerCandidateLines(contract, line, body), body)
}

// pasInquiryOutputDeviations names each nonconformant output parameter an answer
// carried a response Bundle under. Empty for every conformant answer, and for one
// this gateway cannot read at all — an unreadable answer is the shape check's
// (answer.shape: a 502 at strict), not a nonconformance to report.
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
// and no branch of this leg is taken because of what this returns. It is a
// conformance check of the payer's answer, so at none it does not run.
func (g *Gateway) reportInquiryAnswerNonconformance(corrID, counterpart string, answer []byte) []string {
	if !g.conformance().RunsKind(KindContent) {
		return nil
	}
	var stated []string
	for _, used := range pasInquiryOutputDeviations(answer) {
		sentence := "prior-authorization inquiry answer carries its response bundle under output parameter " +
			used + "; the operation declares " + pasgraph.InquiryDeclaredOutput
		if read, _ := pasInquiryOutputCarries(used); !read {
			// Said differently because the consequence is different: this one was not
			// read, so nothing in it reached the ledger or the subject check. The
			// requester still has the payer's bytes; the operator now knows part of
			// them went unread rather than finding an exchange that looks ordinary.
			sentence = "prior-authorization inquiry answer carries a resource under output parameter " +
				used + ", which this gateway does not read; the operation declares " + pasgraph.InquiryDeclaredOutput
		}
		stated = append(stated, sentence)
		g.observe(ObserverEvent{Kind: "peer.nonconformant", Direction: "ingress", LegType: "pas-claim-inquire",
			CorrelationID: corrID, Counterpart: counterpart, Op: "pas-inquire-response", Detail: sentence})
	}
	return stated
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
func (n *nativeResponder) handlePASInquireNative(ctx context.Context, contract string, in relay.Body, requestFHIR []byte) (LegResult, error) {
	const fhirJSON = "application/fhir+json"
	forward, refused, err := n.payorEdgeRequest(in, payorEdgePASBundle, fhirJSON, n.refusesRequest(ctx, RuleInsurer, requestFHIR))
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
	return LegResult{Response: relay.Exact(up.body, up.contentType), AnswerStatus: up.status, ResponseSubjectForeign: true}, nil
}

// ---- the payer gateway's inbound handler ----

// handlePASInquireInbound serves the inquiry leg payer-side.
//
// Authority is evaluated independently here, as on every leg: the inquiry must
// bind to one member (readPASInquiryFacts, over EVERY Patient it names), bound by
// this payer's own system; the ledger projection is keyed by that binding, never
// by the inbound token's subject. Nothing about the
// authority is inherited from the submit that created the authorization — a
// requester holding a submit token cannot ask about the authorization with it,
// because the catalog pins this leg's own operation into the token binding.
//
// The ledger effect is derived AFTER the answer is in hand and BEFORE the response
// leg is sealed, so the answer the requester receives and the decision the ledger
// records are the same answer. It follows the same order the submit leg uses:
// fence, build the response leg, then write holder state — a response-leg failure can never leave a recorded
// decision the requester never received.
//
// The subject (Claim.patient, bound by this payer's own system — bindInboundSubject — whose
// binding keys the ledger projection, never the token's subject) and an inquiry that cannot be
// read refuse at every level; the inquiry's own shape and consistency (readPASInquiryFacts) are not checked at none,
// recorded at observe and refused at strict. The payer's answer is its own content (payerAnswerRead): below strict an
// answer this gateway cannot read, or whose subjects do not bind, is relayed exactly as the payer
// sent it, and nothing is written from it.
func (g *Gateway) handlePASInquireInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, bundleJSON []byte, answerTok string) {
	facts, status, msg := readPASInquiryFacts(bundleJSON, func(rule string) bool {
		return g.guard(r.Context(), KindContent, rule, bundleJSON)
	})
	if status != 0 {
		g.refuseInbound(w, r, legPASClaimInquire, env, tok, answerTok, status, msg, nil)
		return
	}
	// Everything the payer records about the inquiry is keyed by its own binding
	// of the member the inquiry names, never by the token.
	subjectPCI, status, msg := g.bindInboundSubject(r.Context(), facts.member, bundleJSON)
	if status != 0 {
		// A system-of-record failure is this gateway's own 5xx, framed by
		// refuseInbound like every other leg's.
		g.refuseInbound(w, r, legPASClaimInquire, env, tok, answerTok, status, msg, nil)
		return
	}
	boundPatientRef := "Patient/" + facts.member
	g.noteSubjectBinding(r.Context(), "pas-claim-inquire", env.Metadata.CorrelationID, tok.Subject, subjectPCI)

	capture := &nativeCertificationCapture{}
	// The payer's own departures from the operation it answered, reported with the
	// answer they describe. Filled once the answer is in hand, below.
	var answerNonconformance []string
	defer func() {
		if capture.attempted {
			g.conformance().certify("pas-claim-inquire", "payer-native", env.Metadata.CorrelationID, shnsdk.LineOf(answerTok), capture.request, capture.response, answerNonconformance...)
		}
	}()
	observationContext := context.WithValue(r.Context(), nativeCertificationKey{}, capture)
	result, err := g.cfg.Responder.Handle(observationContext, "pas-claim-inquire", env.Metadata.CorrelationID, subjectPCI, bundleJSON)
	committed := false
	defer func() {
		if !committed && result.Rollback != nil {
			// Releasing the claim is the pend ledger's write.
			stopLedger := exchangeOf(r.Context()).stage(addLedger)
			result.Rollback()
			stopLedger()
		}
	}()
	if err != nil {
		g.responderFailed(w, r, legPASClaimInquire, env, tok, answerTok, err)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, "payer-coverage", "pas-inquire-response", "pas-claim-inquire",
			env.Metadata.CorrelationID, result, tok.Subject, env.Metadata.Sender, "", answerTok)
		return
	}
	responseFHIR, err := g.admit(result.Response, answerKey("pas-claim-inquire", relay.OutcomeAnswered))
	if err != nil {
		g.refuseInbound(w, r, legPASClaimInquire, env, tok, answerTok, http.StatusInternalServerError, errOwnershipFault, nil)
		return
	}
	// The answer's content checks name it as the participant's own answer.
	ownFC := findingContextFrom(r.Context())
	ownFC.Whose = "own"
	read := g.newPayerAnswerRead(withFindingContext(r.Context(), ownFC), result, responseFHIR, "")
	inquiryRead := g.pasInquiryAnswerRead(answerTok, responseFHIR)
	if f, ok := laterLineFinding(ownFC, g.conformance().Level().String(), RuleAnswerShape, inquiryRead, responseFHIR); ok && g.conformance().Runs(KindContent, RuleAnswerShape) {
		g.emitFindingIn(r.Context(), f)
	}
	if bad := validatePASInquiryAnswerAt(responseFHIR, inquiryRead.Line); bad.Status != 0 && read.refusesUnreadable(RuleAnswerShape) {
		g.refuseInbound(w, r, legPASClaimInquire, env, tok, answerTok, bad.Status, bad.Message, nil)
		return
	}
	var (
		ledgerCommit func() error
		events       []ObserverEvent
	)
	if read.read() {
		// Whatever the payer's answer departs from, stated before anything is derived
		// from it. Reported IMMEDIATELY rather than with the ledger's events: the
		// payer deviated whatever this exchange does next, and an operator asking why
		// a decision went unrecorded is asking about exactly the runs that fail later.
		answerNonconformance = g.reportInquiryAnswerNonconformance(env.Metadata.CorrelationID, env.Metadata.Sender, responseFHIR)
		// The ledger effect, derived from the answer.
		stop := exchangeOf(r.Context()).stage(addLedger)
		ledgerCommit, events = g.inquiryLedgerEffect(env.Metadata.Sender, subjectPCI, env.Metadata.CorrelationID, facts, responseFHIR)
		stop()
	}
	if status, msg := g.fenceResponseSubjectWith(r.Context(), "pas-claim-inquire", boundPatientRef, env.Metadata.CorrelationID, result, read.refuses); status != 0 {
		g.refuseInbound(w, r, legPASClaimInquire, env, tok, answerTok, status, msg, nil)
		return
	}
	// The direction flips here, as it does on every inbound leg that answers with
	// bytes it built itself. handleInbound tagged this context with the peer's
	// inquiry — right for everything above — but everything validated from here
	// on is THIS participant's own build: the answer when this gateway produced
	// it (validatePASResult certifies only that case; a relayed answer returns
	// early, being the payer's own message). Retag
	// rather than re-tag from scratch, so the leg and the correlation id
	// handleInbound resolved are carried, not re-derived.
	fc := findingContextFrom(r.Context())
	fc.Whose = "own"
	ctx := withFindingContext(r.Context(), fc)
	if status, msg := g.validatePASResult(ctx, result, answerTok, "pas-claim-inquire"); status != 0 {
		g.refuseInbound(w, r, legPASClaimInquire, env, tok, answerTok, status, msg, nil)
		return
	}
	// Stamp honesty, as on every relaying leg: a verbatim foreign relay is left
	// unstamped, because this build did not produce the payer's bytes and cannot
	// vouch for the line they were built at. The answer's media type is the payer's
	// own.
	stampTok := stampForBuiltAnswer(result, answerTok)
	respBytes, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-inquire-response", "pas-claim-inquire", env.Metadata.CorrelationID,
		result.Response, answerKey("pas-claim-inquire", relay.OutcomeAnswered),
		g.successFrame(r.Context(), env.Metadata.Sender, result.answerStatus(), result.ResponseContentType(), stampTok),
		tok.Subject, env.Metadata.Sender, "")
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if !read.read() {
		// An answer relayed unread writes nothing: no decision. The
		// ledger's lookup events describe a write that is not made.
		g.payerLocalWriteSkipped("pas-claim-inquire", env.Metadata.CorrelationID, read.unread)
		writeLeg(w, respBytes)
		return
	}
	// The ledger records and never gates: a write that fails is reported to the
	// operator, never swallowed, and the payer's answer still reaches the
	// requester. The ledger's transitions are reported by the write itself, so
	// one that failed reports none; the notes below are about the lookup, and
	// stand either way.
	writeFailed := false
	stopLedger := exchangeOf(r.Context()).stage(addLedger)
	if ledgerCommit != nil {
		if err := ledgerCommit(); err != nil {
			writeFailed = true
			g.payerLocalWriteFailed("pas-claim-inquire", env.Metadata.CorrelationID, err)
		}
	}
	if !writeFailed && result.Commit != nil {
		if err := result.Commit(); err != nil {
			writeFailed = true
			g.payerLocalWriteFailed("pas-claim-inquire", env.Metadata.CorrelationID, err)
		}
	}
	stopLedger()
	committed = !writeFailed
	for _, e := range events {
		g.observe(e)
	}
	writeLeg(w, respBytes)
}

// PendOtherSubjectEvent is the observer event an inquiry raises when the
// payer's answer resolves to an authorization this requester holds under a
// different member binding than the inquiry's. No decision is recorded from it.
const PendOtherSubjectEvent = "pend.other-subject"

// PendInquiryUnmatchedEvent is the observer event an inquiry raises when any
// ClaimResponse in the payer's answer could not be recorded against one
// authorization. One event per inquiry, whose Detail counts them by
// verdict ("no-strong-key=2 ambiguous=390"), and names the key kinds that
// disagreed. The payer's answer is relayed either way.
const PendInquiryUnmatchedEvent = "pend.inquiry-unmatched"

// pendUnmatchedCount tallies an inquiry's unrecorded ClaimResponses by verdict.
type pendUnmatchedCount struct {
	n         map[PendMatchVerdict]int
	disagreed map[string]bool
}

func (c *pendUnmatchedCount) add(m PendMatch) {
	if c.n == nil {
		c.n, c.disagreed = map[PendMatchVerdict]int{}, map[string]bool{}
	}
	c.n[m.Verdict]++
	if m.Verdict == PendMatchDisagrees && m.Kind != "" {
		c.disagreed[m.Kind] = true
	}
}

// String is the event's Detail, in a fixed order; "" when nothing was unmatched.
func (c *pendUnmatchedCount) String() string {
	var parts []string
	for _, v := range []PendMatchVerdict{PendMatchNoStrongKey, PendMatchNone, PendMatchAmbiguous, PendMatchDisagrees, PendMatchRequesterKeyOnly} {
		if c.n[v] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", v, c.n[v]))
		}
	}
	if len(c.disagreed) > 0 {
		kinds := make([]string, 0, len(c.disagreed))
		for k := range c.disagreed {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		parts = append(parts, "disagreed on "+strings.Join(kinds, ","))
	}
	return strings.Join(parts, " ")
}

// inquiryLedgerEffect derives this payer gateway's own pend-ledger effect from the
// answer, and returns the write to run once the response leg is sealed plus the
// observer events the ledger's transitions raised.
//
// For every ClaimResponse the answer carries:
//   - exactly one match whose state the payer has now decided → RecordDecision;
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
func (g *Gateway) inquiryLedgerEffect(requester, subject, legCorrID string, facts pasInquiryFacts, answer []byte) (func() error, []ObserverEvent) {
	ledger, ok := LedgerOf(g.cfg.Store)
	if !ok || strings.TrimSpace(requester) == "" {
		return nil, nil
	}
	type decision struct {
		subjectPCI string
		corrID     string
		outcome    string
		decidedAt  time.Time
		authNumber string
	}
	var (
		decisions []decision
		events    []ObserverEvent
		unmatched pendUnmatchedCount
	)
	for _, ans := range readPASInquiryAnswers(requester, answer) {
		if len(StrongPendProbe(ans.keys)) == 0 {
			// Nothing the payer issued for one authorization alone, so nothing to
			// search by. Counted, not consulted.
			unmatched.add(PendMatch{Verdict: PendMatchNoStrongKey})
			continue
		}
		m, err := ledger.LookupPended(requester, ans.keys)
		subjectPCI, corrID := m.SubjectPCI, m.CorrelationID
		switch {
		case err != nil:
			// The ledger could not be consulted. The payer's answer still reaches
			// the requester — the relay is not the ledger's to withhold — and the
			// operator sees why nothing was recorded.
			g.noteStoreError(storeErrPended)
			events = append(events, ObserverEvent{Kind: "pend.lookup-unavailable", Direction: "ingress",
				LegType: "pas-claim-inquire", CorrelationID: legCorrID, Op: "pas-inquire"})
			continue
		case m.Verdict != PendMatchFound:
			// No authorization this answer can be recorded against: none, more
			// than one, or one that holds a key the answer contradicts. Counted
			// and reported below; never a bare skip.
			unmatched.add(m)
			continue
		case subjectPCI != subject:
			// The payer's answer names an authorization this requester holds for
			// another member than the one this inquiry is bound to — or for the
			// same member bound differently, when a leg carried a different Patient.
			// Nothing is recorded; the operator is told.
			events = append(events, ObserverEvent{Kind: PendOtherSubjectEvent, Direction: "ingress",
				LegType: "pas-claim-inquire", CorrelationID: legCorrID, Op: "pas-inquire", Detail: "authorization " + corrID})
			continue
		}
		outcome, parsed, decided := inquiryDecisionOf(ans.raw)
		if !decided {
			continue // still pended, or an answer that states no decision
		}
		if outcome == PendOutcomeDenied && ans.keys.PreAuthRef != "" {
			// As on the submit leg: a decision read as a denial that states an
			// authorization number in the ClaimResponse's own preAuthRef may
			// be a partial certification. This gateway's reading cannot hold
			// it, so no decision is recorded and the operator is told why. The
			// payer's answer is relayed regardless.
			events = append(events, ObserverEvent{Kind: pendDecisionNotRecordedEvent, Direction: "ingress",
				LegType: "pas-claim-inquire", CorrelationID: legCorrID, Op: "pas-inquire",
				Detail: "authorization " + corrID + ": " + decisionReadAsDenialWithAuthNumber})
			continue
		}
		decisions = append(decisions, decision{subjectPCI: subjectPCI, corrID: corrID, outcome: outcome, decidedAt: ans.created, authNumber: parsed.PreAuthRef})
	}
	if detail := unmatched.String(); detail != "" {
		events = append(events, ObserverEvent{Kind: PendInquiryUnmatchedEvent, Direction: "ingress",
			LegType: "pas-claim-inquire", CorrelationID: legCorrID, Op: "pas-inquire", Detail: detail})
	}
	if len(decisions) == 0 {
		return nil, events
	}
	commit := func() error {
		for _, d := range decisions {
			tr, err := ledger.RecordDecision(d.subjectPCI, d.corrID, d.outcome, d.decidedAt, PendKeys{RequesterHolder: requester, PreAuthRef: d.authNumber})
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
// ledger outcome it records, the decision detail it states, and whether it is a
// decision at all. A response the payer still reports as pended,
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
	const leg = "pas-claim-inquire"
	builder, ok := g.ingressBuilder(w, r, leg)
	if !ok {
		exchangeOf(r.Context()).leg(leg)
		return
	}
	// Tag the leg for every check before routing; the correlation id is added
	// once the leg is routed.
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: leg, Seam: "provider-ingress", Whose: "own"}))
	// The subject (Claim.patient resolving to a pci, the unknown-member setting inside
	// resolveSubjectPCI), an inquiry that cannot be read and routing refuse at
	// every level; the inquiry's own shape and consistency (readPASInquiryFacts)
	// are not checked at none, recorded at observe and refused at strict.
	facts, status, msg := readPASInquiryFacts(body, func(rule string) bool {
		return g.guard(r.Context(), KindContent, rule, body)
	})
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pci, found, readErr := g.resolveSubjectPCI(r.Context(), facts.member, body)
	if readErr != nil {
		status, msg := subjectBindingFailure(r.Context(), readErr)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	// Routed by the inquiry's OWN Coverage, the same rule the submit ingress
	// follows: a Coverage naming no resolvable payer fails closed rather than
	// defaulting to one.
	recipient, _, status, msg := g.pasRecipient(body)
	if status != 0 {
		exchangeOf(r.Context()).routed(status)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	scope.leg = leg
	exchangeOf(r.Context()).leg(leg)
	ex := g.exchanges.Begin(workstreamPA)
	child := g.ingressCorrelation(w, r)
	requestObservation := append([]byte(nil), body...)
	var responseObservation []byte
	var observedTarget string
	defer func() {
		g.conformance().certify(leg, "provider-ingress", child, shnsdk.LineOf(observedTarget), requestObservation, responseObservation)
	}()
	observationContext := context.WithValue(r.Context(), certificationTargetKey{}, &observedTarget)
	// A module client's inquiry is the gateway's own, sent as its module built it.
	request, ok := sealModuleRequest(w, builder, relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), "application/fhir+json"), body)
	if !ok {
		return
	}
	content := Content{WorkstreamType: workstreamPA, Payload: request, Carried: true, OnBehalf: builder != ""}
	var answerStatus int
	answer, err := g.OriginateLeg(withAnswerStatusSink(observationContext, &answerStatus), r, recipient, leg, pci, child, "", content)
	responseObservation = append([]byte(nil), answer...)
	var relayed *RelayError
	if errors.As(err, &relayed) {
		responseObservation = append([]byte(nil), relayed.Body...)
	}
	spec, _ := legSpecFor(leg)
	legProj := Leg{Type: leg, Physics: spec.Physics, Content: content, Subjects: []string{pci}}
	if err != nil {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// The answer is the payer's content: an answer this gateway cannot read
	// (RuleAnswerShape) and one naming more than one patient
	// (RulePatientAnswer) refuse at strict; below strict the payer's bytes are
	// relayed exactly as received, recorded at observe. The answer is read at
	// every level for the local record below, which never acts on an answer it
	// could not read.
	answerCtx := withFindingContext(r.Context(), findingContext{LegType: leg, CorrelationID: child, Seam: "provider-ingress", Whose: "peer"})
	unread := ""
	inquiryRead := g.pasInquiryAnswerRead(observedTarget, answer)
	if f, ok := laterLineFinding(findingContextFrom(answerCtx), g.conformance().Level().String(), RuleAnswerShape, inquiryRead, answer); ok && g.conformance().Runs(KindContent, RuleAnswerShape) {
		g.emitFindingIn(answerCtx, f)
	}
	if bad := validatePASInquiryAnswerAt(answer, inquiryRead.Line); bad.Status != 0 {
		// A repeated member name is read one way only (RuleDuplicateKey): it
		// refuses at every level, with strict's refusal.
		if repeatsAMember(answerCtx, answer) || g.guard(answerCtx, KindContent, RuleAnswerShape, answer) {
			g.recordLeg(ex.ID, legProj.Project(child, "error"))
			writeJSON(w, bad.Status, map[string]string{"error": bad.Message})
			return
		}
		unread = RuleAnswerShape
	}
	// The same subject-linkage rule the submit ingress applies to a payer's
	// response: every patient the answer names, at every depth, must be the same
	// one. Shape alone would pass an answer whose Coverage beneficiary named
	// another member. An answer that cannot be read has no subjects to bind;
	// the rule above already covers it.
	if unread == "" && !consistentPASInquiryAnswerSubjects(answer) {
		if g.guard(answerCtx, KindContent, RulePatientAnswer, answer) {
			g.recordLeg(ex.ID, legProj.Project(child, "error"))
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS inquiry answer has inconsistent patient linkage"})
			return
		}
		unread = RulePatientAnswer
	}
	if g.carriedAnswerRefused(answerCtx, w, answer, shnsdk.LineOf(observedTarget)) {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		return
	}
	// "ok" says the answer was read and bound; one relayed unread is recorded
	// "answered" (localWriteSkipped).
	outcome := "ok"
	if unread != "" {
		outcome = "answered"
		g.localWriteSkipped(leg, child, unread)
	}
	g.recordLeg(ex.ID, legProj.Project(child, outcome))
	g.writePayload(w, relayedAnswerStatus(answerStatus), "application/fhir+json",
		relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), "application/fhir+json"),
		relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}

// The PAS inquiry answer's readers are internal/pasgraph's; these are the
// engine's names for them, for the inquiry leg.

// validatePASInquiryAnswer refuses an inquiry answer that is none of the
// shapes the published operation definitions declare (pasgraph.ValidateInquiryAnswer).
func validatePASInquiryAnswer(body []byte) LegResult {
	if ok, detail := pasgraph.ValidateInquiryAnswer(body); !ok {
		return fail502(detail)
	}
	return LegResult{}
}

// validatePASInquiryAnswerAt is validatePASInquiryAnswer for an answer
// exchanged at IG line (pasgraph.ValidateInquiryAnswerAt).
func validatePASInquiryAnswerAt(body []byte, line string) LegResult {
	if ok, detail := pasgraph.ValidateInquiryAnswerAt(body, line); !ok {
		return fail502(detail)
	}
	return LegResult{}
}

// inquiryAnswerShape is pasgraph.InquiryAnswerShape.
func inquiryAnswerShape(body []byte) (string, []json.RawMessage, []string, error) {
	return pasgraph.InquiryAnswerShape(body)
}

// pasInquiryOutputCarries is pasgraph.InquiryOutputCarries.
func pasInquiryOutputCarries(name string) (carries, deviant bool) {
	return pasgraph.InquiryOutputCarries(name)
}

// consistentPASInquiryAnswerSubjects is pasgraph.ConsistentInquiryAnswerSubjects.
func consistentPASInquiryAnswerSubjects(answer []byte) bool {
	return pasgraph.ConsistentInquiryAnswerSubjects(answer)
}
