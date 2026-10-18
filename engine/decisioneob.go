// decisioneob.go — the payer gateway's decision ExplanationOfBenefit:
// the patient-facing record of the payer's decision, built only from what the
// payer's answer and the request supply.
//
// The EOB is this gateway's own side record, never sent to a peer. Every value
// it states is read from a source: the request Claim (type, provider,
// insurance, the item's sequence and net) or the payer's ClaimResponse
// (created, insurer, outcome, its own item adjudication — category, amount
// and reasons value for value (re-marshalled, so <, > and & may be escaped), its review action re-expressed value for value
// in the EOB's own extension — notes and authorization numbers). An element the EOB requires that its
// source does not supply is never filled in: the EOB is not written, the
// gateway records why (DecisionEOBNotWrittenEvent), and the payer's decision
// itself still relays and is recorded.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// DecisionEOBNotWrittenEvent is the observer event a payer gateway raises
// when it records a payer's decision without its decision EOB because an
// element the EOB requires is absent at its source. Detail names the element
// and the source ("decision EOB not written: insurer absent at the payer's
// ClaimResponse"). The payer's answer is relayed unchanged and the decision is
// recorded; nothing is filled in for the missing element.
const DecisionEOBNotWrittenEvent = "pa.decision-eob-not-written"

// The sources a decision EOB reads.
const (
	eobSourceRequestBundle = "the request Bundle"
	eobSourceRequestClaim  = "the request Claim"
	eobSourceRequestItem   = "the request Claim item"
	eobSourceClaimResponse = "the payer's ClaimResponse"
)

// eobElementSource names where each element the EOB requires is read.
var eobElementSource = map[string]string{
	"type":               eobSourceRequestClaim,
	"provider":           eobSourceRequestClaim,
	"insurance":          eobSourceRequestClaim,
	"insurance.focal":    eobSourceRequestClaim,
	"insurance.coverage": eobSourceRequestClaim,
	"item.sequence":      eobSourceRequestItem,
	"created":            eobSourceClaimResponse,
	"insurer":            eobSourceClaimResponse,
	"outcome":            eobSourceClaimResponse,
}

// eobNotWritten is a decision EOB not written: a required element is absent at
// its source, or the payer's detail cannot be carried on it as written (a
// review action sub-extension with no counterpart, a note typed outside
// NoteType). It is never a refusal: the decision relays and is recorded.
type eobNotWritten struct{ detail string }

func (e *eobNotWritten) Error() string { return "decision EOB not written: " + e.detail }

// The findings of a decision the payer states on no item the EOB could carry.
// An EOB is the record of the payer's decision on the request item: one that
// carries the item without the payer's adjudication of it would read as an
// approval whatever the payer decided, so none is written.
const (
	eobNoSubmitItemAnswered  = "no item of " + eobSourceClaimResponse + " answers the sequence of " + eobSourceRequestItem + " that states the decision's product coding"
	eobNoInquiryItemAnswered = "no item of " + eobSourceClaimResponse + " carries the item trace number of the inquiry line the decision answers"
)

// decisionEOBSource is what one decision's EOB is read from.
type decisionEOBSource struct {
	// request is the request Bundle the requester sent (the submission, or
	// the inquiry that learned the decision).
	request []byte
	// itemIndex is the request Claim item the decision answers, or -1 for the
	// item whose product coding is the order's (submit).
	itemIndex int
	// itemTrace is the item trace number ("system|value") that picked the
	// inquiry line (itemIndex): the ClaimResponse item answering it is the
	// one carrying the same trace number, never one picked by sequence. On
	// submit it is "" and the ClaimResponse answers the request's own item
	// sequences.
	itemTrace string
	// inquiry marks the inquiry leg: its ClaimResponse item is matched by
	// itemTrace only.
	inquiry bool
	// claimResponse is the payer's ClaimResponse that carried the decision,
	// and answer the Bundle it came in (nil when it came bare).
	claimResponse []byte
	answer        []byte
}

type eobSourceRef struct {
	Reference  string          `json:"reference"`
	Identifier json.RawMessage `json:"identifier"`
	Display    string          `json:"display"`
}

type eobSourceEntry struct {
	FullURL  string          `json:"fullUrl"`
	Resource json.RawMessage `json:"resource"`
}

// eobSourceResource is the part of a referenced resource a reference may be
// stated by: its identity and its identifiers.
type eobSourceResource struct {
	ResourceType string            `json:"resourceType"`
	ID           string            `json:"id"`
	Identifier   []json.RawMessage `json:"identifier"`
}

const (
	systemNPI    = "http://hl7.org/fhir/sid/us-npi"
	systemV20203 = "http://terminology.hl7.org/CodeSystem/v2-0203"
)

// bundleEntries reads a Bundle's entries; nil for anything else.
func bundleEntries(b []byte) []eobSourceEntry {
	if len(b) == 0 {
		return nil
	}
	var bundle struct {
		ResourceType string           `json:"resourceType"`
		Entry        []eobSourceEntry `json:"entry"`
	}
	if decodeMessage(b, &bundle) != nil || bundle.ResourceType != "Bundle" {
		return nil
	}
	return bundle.Entry
}

// resolveEOBRef finds the entry a literal reference names: by fullUrl, or by
// the resource's own Type/id.
func resolveEOBRef(ref string, entries []eobSourceEntry) (eobSourceResource, bool) {
	for _, e := range entries {
		var r eobSourceResource
		if decodeMessage(e.Resource, &r) != nil {
			continue
		}
		local := r.ResourceType + "/" + r.ID
		if e.FullURL == ref || (r.ID != "" && local == ref) {
			return r, true
		}
	}
	return eobSourceResource{}, false
}

// pickEOBIdentifier chooses, from a resource's identifiers, the one a
// reference to it is stated by: an NPI, else a member identifier (v2-0203
// MB), else the first, each exactly as written.
func pickEOBIdentifier(ids []json.RawMessage) json.RawMessage {
	type ident struct {
		System string `json:"system"`
		Type   struct {
			Coding []struct {
				System string `json:"system"`
				Code   string `json:"code"`
			} `json:"coding"`
		} `json:"type"`
	}
	var member json.RawMessage
	for _, raw := range ids {
		var id ident
		if decodeMessage(raw, &id) != nil {
			continue
		}
		if id.System == systemNPI {
			return raw
		}
		for _, c := range id.Type.Coding {
			if member == nil && c.System == systemV20203 && c.Code == "MB" {
				member = raw
			}
		}
	}
	if member != nil {
		return member
	}
	if len(ids) > 0 {
		return ids[0]
	}
	return nil
}

// eobReferenceFrom states a source reference on the EOB: by the identifier the
// reference carries, or the one the resource it names in the source Bundle
// carries; otherwise by the literal reference as written. Nothing is minted:
// a reference with neither is returned empty (absent).
func eobReferenceFrom(r *eobSourceRef, entries []eobSourceEntry) shnsdk.PADecisionEOBReference {
	if r == nil {
		return shnsdk.PADecisionEOBReference{}
	}
	out := shnsdk.PADecisionEOBReference{Display: r.Display}
	trimmed := strings.TrimSpace(string(r.Identifier))
	if trimmed != "" && trimmed != "null" {
		out.Identifier = r.Identifier
		return out
	}
	if r.Reference == "" {
		return shnsdk.PADecisionEOBReference{}
	}
	if res, ok := resolveEOBRef(r.Reference, entries); ok {
		if id := pickEOBIdentifier(res.Identifier); id != nil {
			out.Identifier = id
			return out
		}
	}
	out.Reference = r.Reference
	return out
}

// decisionEOB is the projection shared by every leg that records a payer
// decision: the submit leg, and the inquiry leg that learns a decision later
// (inquire.go). One projection means the two legs cannot state a payer's own
// decision differently.
//
// It returns *eobNotWritten whenever the EOB cannot be written: an element the
// EOB requires is absent at its source, no request item (or, on an inquiry,
// no ClaimResponse item) can be matched to the decision, no ClaimResponse
// item answers that request item, or the payer's answer
// cannot be carried as written (a note type outside NoteType, an unmapped
// review action sub-extension, an adjudication that is not an object). The
// decision still relays and is recorded. Its detail names the element and
// its position only, never a value the payer or requester stated.
func decisionEOB(corrID, patientRef, procSystem, cpt, cptDisplay string, parsed shnsdk.PriorAuthResult, src decisionEOBSource) ([]byte, error) {
	var claim struct {
		ResourceType string          `json:"resourceType"`
		Type         json.RawMessage `json:"type"`
		Provider     *eobSourceRef   `json:"provider"`
		Insurance    []struct {
			Focal    *bool         `json:"focal"`
			Coverage *eobSourceRef `json:"coverage"`
		} `json:"insurance"`
		Item []struct {
			Sequence         int             `json:"sequence"`
			Net              json.RawMessage `json:"net"`
			ProductOrService json.RawMessage `json:"productOrService"`
		} `json:"item"`
	}
	request := bundleEntries(src.request)
	claimSeen, claimRead := false, false
	for _, e := range request {
		var head struct {
			ResourceType string `json:"resourceType"`
		}
		if decodeMessage(e.Resource, &head) != nil || head.ResourceType != "Claim" {
			continue
		}
		claimSeen = true
		claimRead = decodeMessage(e.Resource, &claim) == nil
		break
	}
	switch {
	case !claimSeen:
		return nil, &eobNotWritten{detail: "Claim absent at " + eobSourceRequestBundle}
	case !claimRead:
		return nil, &eobNotWritten{detail: "Claim unreadable at " + eobSourceRequestBundle}
	}
	var cr struct {
		PreAuthRef   string            `json:"preAuthRef"`
		Created      string            `json:"created"`
		Outcome      string            `json:"outcome"`
		Insurer      *eobSourceRef     `json:"insurer"`
		Adjudication []json.RawMessage `json:"adjudication"`
		Item         []struct {
			ItemSequence int               `json:"itemSequence"`
			Extension    []json.RawMessage `json:"extension"`
			Adjudication []json.RawMessage `json:"adjudication"`
		} `json:"item"`
		AddItem []json.RawMessage `json:"addItem"`
	}
	if len(src.claimResponse) == 0 {
		return nil, &eobNotWritten{detail: "ClaimResponse absent at the payer's answer"}
	}
	if decodeMessage(src.claimResponse, &cr) != nil {
		return nil, &eobNotWritten{detail: "ClaimResponse unreadable at the payer's answer"}
	}

	// The request item the decision answers: the line the inquiry asked
	// about, else the item that codes the order. Never another item: the
	// order's code is never paired with an item that does not state it.
	item := -1
	switch {
	case src.itemIndex >= 0:
		if src.itemIndex < len(claim.Item) {
			item = src.itemIndex
		}
	default:
		for i, it := range claim.Item {
			if item < 0 && itemCodesProduct(it.ProductOrService, procSystem, cpt) {
				item = i
			}
		}
	}
	if item < 0 {
		return nil, &eobNotWritten{detail: "no item of " + eobSourceRequestClaim + " states the decision's product coding"}
	}
	// The payer's ClaimResponse items that answer that request item: on an
	// inquiry, the ones carrying the trace number that picked the line; on
	// submit, the ones answering its sequence. Another item's adjudication is
	// never paired with this one. When none answers it, the payer's decision
	// is on no adjudication the EOB could carry, and an EOB stating the item
	// without it would read as an approval whatever the payer decided: none
	// is written.
	var answered []int
	for i, it := range cr.Item {
		if src.inquiry {
			if src.itemTrace != "" && slices.Contains(itemTraceKeys(it.Extension), src.itemTrace) {
				answered = append(answered, i)
			}
			continue
		}
		if it.ItemSequence == claim.Item[item].Sequence {
			answered = append(answered, i)
		}
	}
	switch {
	case claim.Item[item].Sequence <= 0:
		// The request item states no sequence, so no payer item can answer
		// it: the request's absence is named, not the payer's.
		return nil, &eobNotWritten{detail: "item.sequence absent at " + eobElementSource["item.sequence"]}
	case len(answered) > 0:
	case src.inquiry:
		return nil, &eobNotWritten{detail: eobNoInquiryItemAnswered}
	default:
		return nil, &eobNotWritten{detail: eobNoSubmitItemAnswered}
	}
	p := shnsdk.PADecisionEOBExactParams{
		ID:         decisionEOBID(corrID),
		PatientRef: patientRef,
		ClaimType:  claim.Type,
		Created:    cr.Created,
		Insurer:    eobReferenceFrom(cr.Insurer, bundleEntries(src.answer)),
		Provider:   eobReferenceFrom(claim.Provider, request),
		Outcome:    cr.Outcome,
		// The request item's own productOrService, as written.
		ProductOrService: claim.Item[item].ProductOrService,
		ItemSequence:     claim.Item[item].Sequence,
		SubmittedAmount:  claim.Item[item].Net,
		// The EOB carries only the notes the payer's own decision carries
		// (never a fixed appeal text), each typed and numbered as the payer
		// stated it.
		ProcessNotes: parsed.ProcessNotes,
		// The payer's header adjudication, transcribed as the item's.
		HeaderAdjudications: cr.Adjudication,
	}
	for _, in := range claim.Insurance {
		p.Insurance = append(p.Insurance, shnsdk.PADecisionEOBInsurance{Focal: in.Focal, Coverage: eobReferenceFrom(in.Coverage, request)})
	}
	// The payer's own adjudication of the item it answered, carried value for
	// value: its category, its amount only where it stated one, its reasons
	// and its review action. The request's net is the submitted amount only
	// where the payer stated no submitted entry (the builder).
	sequences := map[int]bool{}
	for _, i := range answered {
		p.Adjudications = append(p.Adjudications, cr.Item[i].Adjudication...)
		sequences[cr.Item[i].ItemSequence] = true
	}
	// The payer's addItems for that item (or for the claim as a whole),
	// carried as written with their adjudication transcribed. An addItem is
	// tied to the item by the request item's own sequence, or by the sequence
	// of a payer item that answered it. The EOB carries that one item under
	// the request item's sequence, so an addItem tied to it that also names any
	// other sequence (another item's, or the payer item's own numbering on an
	// inquiry) cannot be carried as written: no EOB, never a silent drop.
	eobSeq := claim.Item[item].Sequence
	for i, ai := range cr.AddItem {
		var head struct {
			ItemSequence []int `json:"itemSequence"`
		}
		_ = decodeMessage(ai, &head) // an addItem that is not an object is refused by the builder
		tied, foreign := len(head.ItemSequence) == 0, false
		for _, seq := range head.ItemSequence {
			tied = tied || seq == eobSeq || sequences[seq]
			foreign = foreign || seq != eobSeq
		}
		if !tied {
			continue // another item's addItem
		}
		if foreign {
			return nil, &eobNotWritten{detail: fmt.Sprintf("addItem %d of %s names an item sequence the EOB does not carry", i+1, eobSourceClaimResponse)}
		}
		p.AddItems = append(p.AddItems, ai)
	}
	// The payer's own authorization numbers, exactly as stated: its
	// ClaimResponse.preAuthRef, then each review action number on what it
	// answered for that item (the item's, the header's and its addItems').
	// Never inferred from the decision.
	if cr.PreAuthRef != "" {
		p.PreAuthRefs = append(p.PreAuthRefs, cr.PreAuthRef)
	}
	p.PreAuthRefs = append(p.PreAuthRefs, reviewActionNumbers(p.Adjudications)...)
	p.PreAuthRefs = append(p.PreAuthRefs, reviewActionNumbers(p.HeaderAdjudications)...)
	for _, ai := range p.AddItems {
		var a struct {
			Adjudication []json.RawMessage `json:"adjudication"`
		}
		if decodeMessage(ai, &a) == nil {
			p.PreAuthRefs = append(p.PreAuthRefs, reviewActionNumbers(a.Adjudication)...)
		}
	}
	eob, err := shnsdk.BuildPADecisionEOBExact(p)
	var absent *shnsdk.PADecisionEOBAbsentError
	switch {
	case err == nil:
		return eob, nil
	case errors.As(err, &absent):
		return nil, &eobNotWritten{detail: absent.Element + " absent at " + eobElementSource[absent.Element]}
	}
	// Any other refusal of the builder is payer detail the resource cannot
	// carry as written (a note typed outside NoteType, a review action
	// sub-extension with no counterpart, an adjudication that is not an
	// object): named by element and position, never by a payer value, never
	// dropped, never a refusal of the decision.
	return nil, &eobNotWritten{detail: strings.TrimPrefix(err.Error(), "shnsdk: ")}
}

// itemCodesProduct reports whether a request item's productOrService states
// the order's product coding: the code, in the order's system when the
// order states one.
func itemCodesProduct(raw json.RawMessage, system, code string) bool {
	var cc struct {
		Coding []struct {
			System string `json:"system"`
			Code   string `json:"code"`
		} `json:"coding"`
	}
	if code == "" || decodeMessage(raw, &cc) != nil {
		return false
	}
	for _, c := range cc.Coding {
		if c.Code == code && (system == "" || c.System == system) {
			return true
		}
	}
	return false
}

// itemTraceKeys reads every item trace number ("system|value") an item's
// extensions carry.
func itemTraceKeys(exts []json.RawMessage) []string {
	var out []string
	for _, raw := range exts {
		if k := identifierExtensionKey([]json.RawMessage{raw}, pasExtItemTraceNumberURL); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// firstClaimResponse is the ClaimResponse a PAS answer carries: the body
// itself when it is one, else the first ClaimResponse entry of its Bundle
// (the graph read requires exactly one). nil when there is none.
func firstClaimResponse(body []byte) []byte {
	var head struct {
		ResourceType string `json:"resourceType"`
	}
	if decodeMessage(body, &head) != nil {
		return nil
	}
	if head.ResourceType == "ClaimResponse" {
		return body
	}
	for _, e := range bundleEntries(body) {
		var h struct {
			ResourceType string `json:"resourceType"`
		}
		if decodeMessage(e.Resource, &h) == nil && h.ResourceType == "ClaimResponse" {
			return e.Resource
		}
	}
	return nil
}

// reviewActionNumbers reads the "number" sub-extension values of the PAS
// review actions on these adjudication entries, in order, as written.
func reviewActionNumbers(adjudications []json.RawMessage) []string {
	var out []string
	for _, raw := range adjudications {
		var a struct {
			Extension []struct {
				URL       string `json:"url"`
				Extension []struct {
					URL         string `json:"url"`
					ValueString string `json:"valueString"`
				} `json:"extension"`
			} `json:"extension"`
		}
		if decodeMessage(raw, &a) != nil {
			continue
		}
		for _, ext := range a.Extension {
			if ext.URL != pasReviewActionExtURL {
				continue
			}
			for _, sub := range ext.Extension {
				if sub.URL == "number" && sub.ValueString != "" {
					out = append(out, sub.ValueString)
				}
			}
		}
	}
	return out
}

// pasReviewActionExtURL is the PAS review action extension a payer's
// ClaimResponse states its decision (and any authorization number) on.
const pasReviewActionExtURL = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction"
