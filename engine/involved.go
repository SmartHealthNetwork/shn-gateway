package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// An exchange that involves more than one patient is recorded under each of
// them: a prior-authorization leg carries, in its envelope's
// involved list, a token for every other patient it involves, and the Hub
// records the exchange under each of them as well as the leg token's patient.
// It is recording only. Nothing here refuses a leg, judges its content or
// changes its bytes: a patient that cannot be named is left out, with an
// observer event and a count, and the leg is sent as usual.
//
// It runs only when the network's Hub reads the list (Config.HubAcceptsInvolved),
// at every conformance level: it is the network's audit, not an optional
// check of the payload, so it is independent of the patient-mixing checks and
// never feeds them.
//
//   - Requester side (request-named): every other patient the request carries,
//     identified through this holder's own system of record as the leg's subject
//     is (resolveSubjectPCI).
//   - Payer side (payer-held or payer-derived): the payer's own binding of the
//     member the request names, when it differs from the leg token's patient
//     (noteSubjectBinding).

// maxInvolved is the most other patients one leg names: the Hub refuses a
// longer request list and drops a longer answer list.
const maxInvolved = 16

// maxInvolvedMembers bounds the system-of-record reads one request's pass
// makes. A request naming more distinct members than this has the rest left
// out, as an overflow.
const maxInvolvedMembers = 2 * maxInvolved

// defaultInvolvedBudget is Config.InvolvedBudget's zero value: the most one
// leg's involved pass may add to it.
const defaultInvolvedBudget = 2 * time.Second

// involvedConcurrency bounds the token requests one involved list makes at a
// time.
const involvedConcurrency = 4

// InvolvedOmittedEvent is the observer event raised for each patient a leg
// leaves out of its involved list; Detail is the reason. It carries no
// identifier.
const InvolvedOmittedEvent = "involved.omitted"

// Why an involved patient was left out (the event's Detail and the metric's
// reason).
const (
	// involvedOmitOverflow: past maxInvolved patients, or past
	// maxInvolvedMembers members read.
	involvedOmitOverflow = "overflow"
	// involvedOmitUnreadable: the system of record could not be read for the
	// member.
	involvedOmitUnreadable = "system-of-record-unavailable"
	// involvedOmitUnknown: the member is not held and this participant requires
	// known members (Config.RequireKnownMembers), so it has no identifier.
	involvedOmitUnknown = "unknown-member"
	// involvedOmitRefused: the Authorization Framework refused the patient's
	// token (the refusal is recorded there).
	involvedOmitRefused = "authorization-refused"
	// involvedOmitFailed: the token could not be obtained (the Authorization
	// Framework was unreachable or failed).
	involvedOmitFailed = "authorization-failed"
	// involvedOmitUnlabelled: the minted token does not carry the involvement
	// requested (an Authorization Framework that predates it).
	involvedOmitUnlabelled = "involvement-not-carried"
	// involvedOmitBudget: the leg's involved pass ran out of its budget
	// (Config.InvolvedBudget) before the patient was named.
	involvedOmitBudget = "budget"
)

func (g *Gateway) involvedBudget() time.Duration {
	if g.cfg.InvolvedBudget > 0 {
		return g.cfg.InvolvedBudget
	}
	return defaultInvolvedBudget
}

// involvedForRequest is the requester side's involved list for one leg:
// every other patient the request carries (requestNamedPatients), each with
// its token (involvedList), all within the leg's budget. It reads and requests
// nothing unless the Hub reads the list and the leg is a prior-authorization
// leg.
func (g *Gateway) involvedForRequest(ctx context.Context, r *http.Request, leg, frame, op, corrID, payloadHash string, payload []byte, legSubject string) string {
	if !g.cfg.HubAcceptsInvolved || !involvedLegs[leg] {
		return ""
	}
	bctx, cancel := context.WithTimeout(ctx, g.involvedBudget())
	defer cancel()
	named := g.requestNamedPatients(bctx, leg, corrID, payload, legSubject)
	if len(named) == 0 {
		return ""
	}
	return g.involvedList(r.WithContext(bctx), "originate", leg, frame, op, corrID, payloadHash, legSubject, named)
}

// involvedForAnswer is the payer side's involved list for one answer: its own
// binding of the member when that differs from the leg token's patient, with
// its token, within the leg's budget. None when the leg has no collector.
func (g *Gateway) involvedForAnswer(r *http.Request, leg, frame, op, corrID, payloadHash, legSubject string) string {
	entries := involvedCollectorFrom(r.Context()).list()
	if len(entries) == 0 {
		return ""
	}
	bctx, cancel := context.WithTimeout(r.Context(), g.involvedBudget())
	defer cancel()
	return g.involvedList(r.WithContext(bctx), "ingress", leg, frame, op, corrID, payloadHash, legSubject, entries)
}

// involvedLegs are the prior-authorization legs whose request names the
// other patients it carries and whose answer names the payer's own binding.
var involvedLegs = map[string]bool{
	"crd-order-select":        true,
	"crd-order-dispatch":      true,
	"dtr-questionnaire-fetch": true,
	"pas-claim":               true,
	"pas-claim-update":        true,
	"pas-claim-inquire":       true,
}

// involvedPatient is one other patient a leg involves.
type involvedPatient struct {
	pci, involvement string
}

// omitInvolved reports one patient left out of a leg's involved list.
func (g *Gateway) omitInvolved(direction, leg, corrID, reason string) {
	g.observe(ObserverEvent{Kind: InvolvedOmittedEvent, Direction: direction, LegType: leg, CorrelationID: corrID, Detail: reason})
	if g.cfg.InvolvedMetric != nil {
		g.cfg.InvolvedMetric(reason)
	}
}

// involvedList mints a token for each patient in entries, bound to the leg
// exactly as its own token is (frame, operation, correlation, payload hash)
// with that patient as subject and its involvement, and returns the encoded
// list for Metadata.Involved. The leg's own patient and repeats are dropped,
// the rest ordered by identifier then involvement, and any past maxInvolved
// left out. The tokens are requested involvedConcurrency at a time, within
// r's context (the leg's budget). A patient whose token is refused, cannot be
// obtained, does not carry the involvement requested, or is not minted before
// the budget runs out is left out. None left encodes as "".
func (g *Gateway) involvedList(r *http.Request, direction, leg, frame, op, corrID, payloadHash, legSubject string, entries []involvedPatient) string {
	seen := map[string]bool{legSubject: true}
	var list []involvedPatient
	for _, e := range entries {
		if e.pci == "" || seen[e.pci] {
			continue
		}
		seen[e.pci] = true
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].pci != list[j].pci {
			return list[i].pci < list[j].pci
		}
		return list[i].involvement < list[j].involvement
	})
	if len(list) > maxInvolved {
		for range list[maxInvolved:] {
			g.omitInvolved(direction, leg, corrID, involvedOmitOverflow)
		}
		list = list[:maxInvolved]
	}
	// The tokens are requested involvedConcurrency at a time, each result kept
	// in its patient's place, so the list and the events keep that order.
	type minted struct {
		entry  shnsdk.InvolvedToken
		reason string
	}
	results := make([]minted, len(list))
	sem := make(chan struct{}, involvedConcurrency)
	var wg sync.WaitGroup
	for i, e := range list {
		sem <- struct{}{}
		if r.Context().Err() != nil {
			<-sem
			results[i].reason = involvedOmitBudget
			continue
		}
		wg.Add(1)
		go func(i int, e involvedPatient) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i].entry, results[i].reason = g.mintInvolved(r, frame, op, corrID, payloadHash, e)
		}(i, e)
	}
	wg.Wait()
	var out []shnsdk.InvolvedToken
	for _, m := range results {
		if m.reason != "" {
			g.omitInvolved(direction, leg, corrID, m.reason)
			continue
		}
		out = append(out, m.entry)
	}
	encoded, err := shnsdk.EncodeInvolved(out)
	if err != nil {
		for range out {
			g.omitInvolved(direction, leg, corrID, involvedOmitFailed)
		}
		return ""
	}
	return encoded
}

// mintInvolved requests patient e's token for the leg and returns its list
// entry, or why it is left out.
func (g *Gateway) mintInvolved(r *http.Request, frame, op, corrID, payloadHash string, e involvedPatient) (shnsdk.InvolvedToken, string) {
	tok, err := g.authorizeRequest(r, authorizeReq{Frame: frame, Operation: op, SubjectPCI: e.pci, CorrelationID: corrID, PayloadHash: payloadHash, Involvement: e.involvement})
	switch {
	case errors.Is(err, errAuthorizationDenied):
		return shnsdk.InvolvedToken{}, involvedOmitRefused
	case err != nil && r.Context().Err() != nil:
		return shnsdk.InvolvedToken{}, involvedOmitBudget
	case err != nil:
		return shnsdk.InvolvedToken{}, involvedOmitFailed
	case tok.Involvement != e.involvement:
		return shnsdk.InvolvedToken{}, involvedOmitUnlabelled
	}
	s, err := tokenJSON(tok)
	if err != nil {
		return shnsdk.InvolvedToken{}, involvedOmitFailed
	}
	return shnsdk.InvolvedToken{Token: s, Involvement: e.involvement}, ""
}

// requestNamedPatients identifies every other patient a request carries, for
// the requester side of the involved list. Each distinct member the request
// names (carriedMembers) is identified through this holder's own system of
// record exactly as the leg's subject is (resolveSubjectPCI, over the same
// member string the subject binding reads from a reference), so every
// reference to the leg's own patient resolves to legSubject and is not named.
// Each other patient is named request-named; one that cannot be identified is
// left out. It reads nothing unless the Hub reads the list and the leg is a
// prior-authorization request leg.
func (g *Gateway) requestNamedPatients(ctx context.Context, leg, corrID string, payload []byte, legSubject string) []involvedPatient {
	if !g.cfg.HubAcceptsInvolved || !involvedLegs[leg] {
		return nil
	}
	members := carriedMembers(payload, leg)
	if len(members) > maxInvolvedMembers {
		for range members[maxInvolvedMembers:] {
			g.omitInvolved("originate", leg, corrID, involvedOmitOverflow)
		}
		members = members[:maxInvolvedMembers]
	}
	var out []involvedPatient
	for _, m := range members {
		if ctx.Err() != nil {
			g.omitInvolved("originate", leg, corrID, involvedOmitBudget)
			continue
		}
		pci, found, err := g.resolveSubjectPCI(ctx, m, payload)
		switch {
		case err != nil && ctx.Err() != nil:
			g.omitInvolved("originate", leg, corrID, involvedOmitBudget)
			continue
		case err != nil:
			g.omitInvolved("originate", leg, corrID, involvedOmitUnreadable)
			continue
		case !found || pci == "":
			g.omitInvolved("originate", leg, corrID, involvedOmitUnknown)
			continue
		case pci == legSubject:
			continue
		}
		out = append(out, involvedPatient{pci: pci, involvement: shnsdk.InvolvementRequestNamed})
	}
	return out
}

// carriedMembers is every member a request on leg names, sorted and each
// once, each read exactly as that leg's subject binding reads the member a
// reference names, so every reference to the leg's own patient reads as the
// subject does:
//   - a reference with "Patient/" names what the leg's reader returns: on the
//     DTR package the id after it, without a version suffix (patientMember);
//     on every other leg everything after the last "Patient/"
//     (pasMemberFromRef, the CRD and inquiry readers agreeing);
//   - a carried Patient names what the leg binds it by: on PAS submit and
//     update, which bind a Patient only through the reference to it, its
//     entry's fullUrl read as a reference, "#<id>" when contained, else its
//     id; on every other leg (CRD, the DTR package, the inquiry), its id;
//   - a urn:uuid: or #contained reference names a Patient only when the
//     request carries it, and then names what that Patient names.
//
// A Patient's member identifier is never read: the subject is bound by the id
// its request uses. An unreadable request names none.
func carriedMembers(payload []byte, leg string) []string {
	var doc any
	if len(payload) == 0 || json.Unmarshal(payload, &doc) != nil {
		return nil
	}
	read := func(ref string) (string, bool) {
		if !strings.Contains(ref, "Patient/") {
			return "", false
		}
		return pasMemberFromRef(ref), true
	}
	if leg == "dtr-questionnaire-fetch" {
		read = patientMember
	}
	// Only the PAS binding names a carried Patient by the reference to it; the
	// others (CRD, the DTR package, the inquiry) name it by its id.
	byReference := leg == "pas-claim" || leg == "pas-claim-update"
	set := map[string]bool{}
	pointers := map[string]string{} // a carried Patient's fullUrl or #id → what it names
	var refs []string
	var walk func(node any, fullURL string, contained bool)
	walk = func(node any, fullURL string, contained bool) {
		switch v := node.(type) {
		case map[string]any:
			if v["resourceType"] == "Patient" {
				id, _ := v["id"].(string)
				names := id
				switch {
				case byReference && fullURL != "":
					names = fullURL
					if m, ok := read(fullURL); ok {
						names = m
					}
				case byReference && contained && id != "":
					names = "#" + id
				}
				if fullURL != "" {
					pointers[fullURL] = names
				}
				if contained && id != "" {
					pointers["#"+id] = names
				}
				if names != "" {
					set[names] = true
				}
			}
			if ref, ok := v["reference"].(string); ok {
				refs = append(refs, ref)
			}
			entryURL, _ := v["fullUrl"].(string)
			for k, child := range v {
				switch {
				case k == "resource" && entryURL != "":
					walk(child, entryURL, false)
				case k == "contained":
					walk(child, "", true)
				default:
					walk(child, "", false)
				}
			}
		case []any:
			for _, child := range v {
				walk(child, "", contained)
			}
		}
	}
	walk(doc, "", false)
	for _, ref := range refs {
		if m, ok := read(ref); ok {
			if m != "" {
				set[m] = true
			}
			continue
		}
		if m, ok := pointers[ref]; ok {
			set[m] = true
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// involvedCollector gathers, while a payer leg is handled, its own binding of
// the member the request names when that differs from the leg token's
// patient, for the answer's involved list. It is in the request's context
// only when the Hub reads the list.
type involvedCollector struct {
	mu      sync.Mutex
	held    map[string]bool // each binding made, and whether it was held
	entries []involvedPatient
}

type involvedCollectorKey struct{}

// withInvolvedCollector gives a payer's prior-authorization leg a collector
// when the Hub reads the involved list; otherwise ctx is returned as is.
func (g *Gateway) withInvolvedCollector(ctx context.Context, leg string) context.Context {
	if !g.cfg.HubAcceptsInvolved || !involvedLegs[leg] {
		return ctx
	}
	return context.WithValue(ctx, involvedCollectorKey{}, &involvedCollector{held: map[string]bool{}})
}

func involvedCollectorFrom(ctx context.Context) *involvedCollector {
	c, _ := ctx.Value(involvedCollectorKey{}).(*involvedCollector)
	return c
}

// bound records a binding the payer made, and whether its system of record
// holds the member.
func (c *involvedCollector) bound(pci string, held bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held[pci] = held
}

// differs names the payer's binding pci as an involved patient: payer-held
// when its system of record holds the member, else payer-derived.
func (c *involvedCollector) differs(pci string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	involvement := shnsdk.InvolvementPayerDerived
	if c.held[pci] {
		involvement = shnsdk.InvolvementPayerHeld
	}
	c.entries = append(c.entries, involvedPatient{pci: pci, involvement: involvement})
}

func (c *involvedCollector) list() []involvedPatient {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]involvedPatient(nil), c.entries...)
}
