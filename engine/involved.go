package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// An exchange that involves more than one patient is recorded under each of
// them: a leg whose workstream says it names involved patients
// (legInvolvement) carries, in its envelope's involved list, a token for
// every other patient it involves, and the Hub
// records the exchange under each of them as well as the leg token's patient.
// It is recording only. Nothing here refuses a leg, judges its content or
// changes its bytes: a patient that cannot be named is left out, on the call's
// exchange record, with an observer event and a count, and the leg is sent as
// usual.
//
// It runs only when the network's Hub reads the list (Config.HubAcceptsInvolved),
// at every conformance level: it is the network's audit, not an optional
// check of the payload, so it is independent of the patient-mixing checks and
// never feeds them.
//
//   - Requester side (request-named): every other patient the request carries,
//     identified through this holder's own system of record as the leg's subject
//     is (resolveSubjectPCI): held, or derived from a Patient the request
//     carries for it with demographics of its own. One named by an id alone is
//     unknown, never named under an identifier derived from that id.
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
	// involvedOmitUnknown: the member has no identifier this gateway can stand
	// behind: not held, and either this participant requires known members
	// (Config.RequireKnownMembers), or the request names it by an id alone
	// (carrying no Patient for it with demographics), or names it in a way
	// that cannot identify one patient.
	involvedOmitUnknown = "unknown-member"
	// involvedOmitRefused: the Authorization Framework refused the patient's
	// token (the refusal is recorded there).
	involvedOmitRefused = "authorization-refused"
	// involvedOmitFailed: no token that verifies could be obtained (the
	// Authorization Framework was unreachable or failed, or its token does not
	// verify for this leg and patient).
	involvedOmitFailed = "authorization-failed"
	// involvedOmitUnlabelled: the minted token does not carry the involvement
	// requested (an Authorization Framework that predates it).
	involvedOmitUnlabelled = "involvement-not-carried"
	// involvedOmitBudget: the leg's involved pass ran out of its budget
	// (Config.InvolvedBudget) before the patient was named.
	involvedOmitBudget = "budget"
)

// InvolvedOmitReasons is the closed list of reasons InvolvedMetric and
// ExchangeRecord.InvolvedOmitted report.
var InvolvedOmitReasons = []string{involvedOmitOverflow, involvedOmitUnreadable, involvedOmitUnknown, involvedOmitRefused, involvedOmitFailed, involvedOmitUnlabelled, involvedOmitBudget}

func (g *Gateway) involvedBudget() time.Duration {
	if g.cfg.InvolvedBudget > 0 {
		return g.cfg.InvolvedBudget
	}
	return defaultInvolvedBudget
}

// involvedForRequest is the requester side's involved list for one leg:
// every other patient the request carries (requestNamedPatients), each with
// its token (involvedList), all within the leg's budget. It reads and requests
// nothing unless the Hub reads the list and the leg's request names the other
// patients it carries (legInvolvement).
func (g *Gateway) involvedForRequest(ctx context.Context, r *http.Request, leg, frame, op, corrID, payloadHash string, payload []byte, legSubject string) string {
	if request, _ := legInvolvement(leg); !g.cfg.HubAcceptsInvolved || !request {
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

// involvedPatient is one other patient a leg involves.
type involvedPatient struct {
	pci, involvement string
}

// omitInvolved reports one patient left out of a leg's involved list.
func (g *Gateway) omitInvolved(ctx context.Context, direction, leg, corrID, reason string) {
	exchangeOf(ctx).involvedOmitted(reason)
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
// obtained, does not verify for this leg and patient, does not carry the
// involvement requested, or is not minted before the budget runs out is left
// out. None left encodes as "".
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
			g.omitInvolved(r.Context(), direction, leg, corrID, involvedOmitOverflow)
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
			g.omitInvolved(r.Context(), direction, leg, corrID, m.reason)
			continue
		}
		out = append(out, m.entry)
	}
	encoded, err := shnsdk.EncodeInvolved(out)
	if err != nil {
		for range out {
			g.omitInvolved(r.Context(), direction, leg, corrID, involvedOmitFailed)
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
	case errors.Is(err, errTokenUnverified):
		return shnsdk.InvolvedToken{}, involvedOmitFailed
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
// names (namedMembers) is identified through this holder's own system of
// record exactly as the leg's subject is (identifyNamed, over the same member
// string the subject binding reads from a reference), so every reference to
// the leg's own patient resolves to legSubject and is not named. Each other
// patient is named request-named; one that cannot be identified is left out.
// It reads nothing unless the Hub reads the list and the leg's request names
// the other patients it carries (legInvolvement).
func (g *Gateway) requestNamedPatients(ctx context.Context, leg, corrID string, payload []byte, legSubject string) []involvedPatient {
	if request, _ := legInvolvement(leg); !g.cfg.HubAcceptsInvolved || !request {
		return nil
	}
	members := namedMembers(payload, leg)
	var cut []namedMember
	if len(members) > maxInvolvedMembers {
		cut = members[maxInvolvedMembers:]
		for range cut {
			g.omitInvolved(ctx, "originate", leg, corrID, involvedOmitOverflow)
		}
		members = members[:maxInvolvedMembers]
	}
	results := make([]namedResult, len(members))
	for i, m := range members {
		if m.subject {
			// The leg's own patient by the request's structure: no read, and
			// never left out.
			results[i].outcome = namedSubject
			continue
		}
		if ctx.Err() != nil {
			results[i].budget = true
			continue
		}
		pci, outcome, err := g.identifyNamed(ctx, m, legSubject)
		results[i] = namedResult{pci: pci, outcome: outcome, err: err, budget: err != nil && ctx.Err() != nil}
	}
	decided, extras, complete := subjectCarriedNames(ctx, payload, members, results, cut)
	var out []involvedPatient
	named := map[string]bool{}
	for i, m := range members {
		r := results[i]
		switch {
		case r.outcome == namedSubject && r.err == nil && !r.budget:
			continue
		case r.budget:
			g.omitInvolved(ctx, "originate", leg, corrID, involvedOmitBudget)
			continue
		case r.err != nil:
			g.omitInvolved(ctx, "originate", leg, corrID, involvedOmitUnreadable)
			continue
		case decided[m.member] == decidedUnknown:
			g.omitInvolved(ctx, "originate", leg, corrID, involvedOmitUnknown)
			continue
		case decided[m.member] == decidedUnreadable:
			g.omitInvolved(ctx, "originate", leg, corrID, involvedOmitUnreadable)
			continue
		case decided[m.member] == decidedOverflow:
			g.omitInvolved(ctx, "originate", leg, corrID, involvedOmitOverflow)
			continue
		case m.legName:
			// The name the leg's binding reads its own patient by, read here
			// as another's (this request may differ from the one the leg
			// bound): never named.
			g.omitInvolved(ctx, "originate", leg, corrID, involvedOmitUnknown)
			continue
		case named[r.pci]:
			continue
		case r.outcome == namedUnknown:
			// A member the request cannot identify is named under no
			// identifier, never another person's.
			g.omitInvolved(ctx, "originate", leg, corrID, involvedOmitUnknown)
			continue
		}
		named[r.pci] = true
		if !complete {
			// The budget ran out before subjectCarriedNames reached every
			// name it would decide: this one may be another name of the
			// leg's own patient, so it is left out, never named.
			g.omitInvolved(ctx, "originate", leg, corrID, involvedOmitBudget)
			continue
		}
		out = append(out, involvedPatient{pci: r.pci, involvement: shnsdk.InvolvementRequestNamed})
	}
	for range extras {
		// A different person the request carries under the leg's own
		// patient's name: counted, never folded into the subject.
		g.omitInvolved(ctx, "originate", leg, corrID, involvedOmitUnknown)
	}
	return out
}

// namedResult is what identifyNamed made of one member, or that the pass's
// budget ran out before it.
type namedResult struct {
	pci     string
	outcome int
	err     error
	budget  bool
}

// nameDecision is what subjectCarriedNames decided of another name of the leg's
// own patient's carried Patient: it is left out, for that reason, never named.
type nameDecision int

const (
	// decidedUnknown: the name may be the subject's or another person's.
	decidedUnknown nameDecision = iota + 1
	// decidedUnreadable: the read of the member that may be the subject failed.
	decidedUnreadable
	// decidedOverflow: the member that may be the subject was past
	// maxInvolvedMembers, so it was never read.
	decidedOverflow
)

// subjectCarriedNames decides, off the PAS submit and update legs (which
// decide the subject's names by Claim.patient, pasSubjectNames) and coverage
// eligibility (whose members bind each from its own request), which names a
// request gives a patient may be another of the leg's own patient's names or
// another person's, by the request's structure alone: it reads nothing and
// compares no demographics. Every name the walk decides is counted, never
// named; there only a member the system of record holds is named at all
// (identifyNamed), and only when the walk does not reach it.
//
// The walk starts from every name that may be the one the leg's binding reads
// its own patient by: the leg's own patient's names (the members that are the
// leg's subject, by the request's structure or a binding equal to the leg's
// subject, and on CRD context.patientId whatever its own read makes of it,
// namedMember.legName), every member whose own read identifies no one or
// fails, and each member past maxInvolvedMembers (cut, never read). A member
// read as another patient the system holds is none: its held identity is not
// the leg's subject's. (A read that runs out of time ends the walk with it.)
// From each it follows every Patient the request carries under the name,
// wherever and however it is carried (a Coverage's party included), to every
// other name that Patient goes by, and on (carriedIndex), and decides each
// name it reaches but the subject's own:
//   - from the subject's names, unknown, and each distinct Patient it reaches
//     beyond the first is an extra, counted unknown: two Patients may be two
//     people;
//   - from a member whose own read failed, unreadable; from a cut member,
//     overflow; from one read as no one, unknown.
//
// A name reached from several is decided by the first that reaches it, the
// subject's names walked first, then failed reads, cut members and the rest. (A
// request readers may read two ways, repeating a member name, names no member
// at all: carriedMembers reads it as the strict reader does.)
//
// The request is parsed once, and each name and each carried Patient is
// followed at most once for each decision, so the walk is linear in the
// request. complete is false when ctx ended before the walk did, checked at
// each name and each Patient: a name it did not reach may be tied to another,
// so requestNamedPatients names no member then.
func subjectCarriedNames(ctx context.Context, payload []byte, members []namedMember, results []namedResult, cut []namedMember) (decided map[string]nameDecision, extras int, complete bool) {
	decided = map[string]nameDecision{}
	decide := func(n string, d nameDecision) {
		if _, ok := decided[n]; !ok {
			decided[n] = d
		}
	}
	walked := func(m namedMember) bool { return !m.pas && !m.ownScope }
	if !slices.ContainsFunc(members, walked) && !slices.ContainsFunc(cut, walked) {
		return decided, 0, true
	}
	subject := map[string]bool{}
	for i, m := range members {
		r := results[i]
		if walked(m) && (m.legName || r.outcome == namedSubject && r.err == nil && !r.budget) {
			subject[m.member] = true
		}
	}
	idx := indexCarriedPatients(payload)
	// What is walked, for which decision, in this order. fromSubject walks the
	// subject's names, counting the distinct Patients they reach.
	const (
		fromSubject = iota
		fromUnreadable
		fromOverflow
		fromMember
		walks
	)
	decisionOf := [walks]nameDecision{decidedUnknown, decidedUnreadable, decidedOverflow, decidedUnknown}
	var seen [walks]map[string]bool
	var seenSite [walks]map[int]bool
	for w := range walks {
		seen[w], seenSite[w] = map[string]bool{}, map[int]bool{}
	}
	distinct := map[string]bool{}
	ended := false
	// walk decides, as w's decision, every name reachable from root through the
	// Patients the request carries under each name reached, until ctx ends
	// (ended), checked at each name and each Patient.
	walk := func(root string, w int) {
		if ended || seen[w][root] {
			return
		}
		seen[w][root] = true
		for queue := []string{root}; len(queue) > 0; queue = queue[1:] {
			if ctx.Err() != nil {
				ended = true
				return
			}
			for _, i := range idx.byName[queue[0]] {
				if seenSite[w][i] {
					continue
				}
				if ctx.Err() != nil {
					ended = true
					return
				}
				seenSite[w][i] = true
				if w == fromSubject {
					distinct[idx.key(i)] = true
				}
				for _, n := range idx.sites[i].names {
					if seen[w][n] || subject[n] {
						continue
					}
					seen[w][n] = true
					decide(n, decisionOf[w])
					queue = append(queue, n)
				}
			}
		}
	}
	// Each member's walk, the walks taken in that order. A member read as
	// another patient the system holds is no seed: its held identity is not the
	// leg's subject's, so it is not the name the leg's binding reads.
	var roots [walks][]string
	for i, m := range members {
		r := results[i]
		switch {
		case !walked(m):
		case subject[m.member]:
			roots[fromSubject] = append(roots[fromSubject], m.member)
		case r.budget:
			// The budget ran out at its read, so complete is false and no
			// member is named.
		case r.err != nil:
			roots[fromUnreadable] = append(roots[fromUnreadable], m.member)
		case r.outcome != namedOther:
			roots[fromMember] = append(roots[fromMember], m.member)
		}
	}
	for _, m := range cut {
		if walked(m) && !subject[m.member] {
			roots[fromOverflow] = append(roots[fromOverflow], m.member)
		}
	}
	for w := range walks {
		for _, root := range roots[w] {
			walk(root, w)
		}
	}
	if len(distinct) > 1 {
		extras = len(distinct) - 1
	}
	// A read that ran out of time ended the budget, so the walk is not
	// complete even when no walk ran.
	return decided, extras, !ended && ctx.Err() == nil
}

// carriedIndex is every Patient a request carries, by every name a reader may
// read it under, from one parse and one walk of the request.
type carriedIndex struct {
	sites []carriedSite
	// byName is the sites of the Patients that go by each name.
	byName map[string][]int
}

// carriedSite is one Patient the request carries, its names and, once read,
// its key (patientKey).
type carriedSite struct {
	patient map[string]any
	names   []string
	key     string
	keyed   bool
}

// indexCarriedPatients indexes every Patient payload carries, at any depth and
// under any member (contained in any resource, a Coverage's party, a Bundle
// entry, an operation parameter, a prefetch value), read leniently so that no
// name some reader reads is missed: a resource whose resourceType is
// "Patient" in any case, by its id and every identifier's value, in any
// system, each member name taken in any case, and, as the resource of an
// object with a fullUrl, by that fullUrl and the id it names (fullURLNames).
// Reading more names than a binding does only counts more.
func indexCarriedPatients(payload []byte) *carriedIndex {
	idx := &carriedIndex{byName: map[string][]int{}}
	var doc any
	if len(payload) == 0 || json.Unmarshal(payload, &doc) != nil {
		return idx
	}
	add := func(p map[string]any, fullURLs []string) {
		names := lenientPatientNames(p)
		for _, u := range fullURLs {
			names = append(names, fullURLNames(u)...)
		}
		names = slices.DeleteFunc(names, func(n string) bool { return n == "" })
		if len(names) == 0 {
			return
		}
		i := len(idx.sites)
		idx.sites = append(idx.sites, carriedSite{patient: p, names: names})
		for _, n := range names {
			idx.byName[n] = append(idx.byName[n], i)
		}
	}
	// strings is every string member of v named name in any case.
	strs := func(v map[string]any, name string) []string {
		var out []string
		for k, x := range v {
			if s, ok := x.(string); ok && strings.EqualFold(k, name) {
				out = append(out, s)
			}
		}
		return out
	}
	var walk func(node any, fullURLs []string)
	walk = func(node any, fullURLs []string) {
		switch v := node.(type) {
		case map[string]any:
			if slices.ContainsFunc(strs(v, "resourceType"), func(t string) bool { return strings.EqualFold(t, "Patient") }) {
				add(v, fullURLs)
			}
			urls := strs(v, "fullUrl")
			for k, child := range v {
				if strings.EqualFold(k, "resource") {
					walk(child, urls)
					continue
				}
				walk(child, nil)
			}
		case []any:
			for _, child := range v {
				walk(child, nil)
			}
		}
	}
	walk(doc, nil)
	return idx
}

// fullURLNames are the names a Patient carried as the resource of an entry
// with fullURL goes by through it: the fullUrl itself and the id it names, as
// every leg's reader reads a Patient reference (the id after its last
// "Patient/", a "/_history/<version>" taken off or not, and the id up to the
// next "/").
func fullURLNames(fullURL string) []string {
	names := []string{fullURL}
	for _, u := range []string{fullURL, stripHistoryRef(fullURL)} {
		i := strings.LastIndex(u, "Patient/")
		if i < 0 {
			continue
		}
		id := u[i+len("Patient/"):]
		names = append(names, id)
		if j := strings.IndexByte(id, '/'); j >= 0 {
			names = append(names, id[:j])
		}
	}
	return names
}

// lenientPatientNames are a Patient's id and every identifier's value, in any
// system, each member name taken in any case and an identifier carried as one
// object or a list.
func lenientPatientNames(p map[string]any) []string {
	var out []string
	for k, v := range p {
		switch {
		case strings.EqualFold(k, "id"):
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		case strings.EqualFold(k, "identifier"):
			list, ok := v.([]any)
			if !ok {
				list = []any{v}
			}
			for _, x := range list {
				xm, _ := x.(map[string]any)
				for xk, xv := range xm {
					if s, ok := xv.(string); ok && strings.EqualFold(xk, "value") {
						out = append(out, s)
					}
				}
			}
		}
	}
	return out
}

// key is site i's patientKey, computed once.
func (x *carriedIndex) key(i int) string {
	if s := &x.sites[i]; !s.keyed {
		s.key, s.keyed = patientKey(s.patient), true
	}
	return x.sites[i].key
}

// patientKey is a Patient's identity among those a request carries: its JSON
// (encoding/json sorts map keys, so equal Patients have equal keys).
func patientKey(p map[string]any) string {
	raw, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(raw)
}

// What identifyNamed makes of a member a request names.
const (
	// namedOther is another patient, identified.
	namedOther = iota
	// namedSubject is the leg's own patient.
	namedSubject
	// namedUnknown is a patient this gateway cannot identify.
	namedUnknown
)

// identifyNamed identifies a member a request names exactly as the leg's
// subject is bound (resolveBinding, reading a Patient whose id is the member's
// as theirs though a Coverage names it as its party). A member that binds to
// the leg's own patient is the subject. Otherwise:
//   - on the PAS submit and update legs (namedMember.pas), a member that is
//     one of the subject's names by the request's structure is the subject, one
//     the subject's entry shares with another carried Patient is unknown, and
//     every other is named only under the identity the system of record holds
//     for it: one it does not hold is unknown, a Patient the request carries
//     with demographics of its own included (pasSubjectNames);
//   - on CRD, DTR and the PAS inquiry likewise, a member the system holds by
//     its held identity, and one it does not hold unknown (subjectCarriedNames
//     then counts a held one some carried Patient ties to another name);
//   - on coverage eligibility, whose members bind each from its own request, a
//     member the system holds by its held identity, one it does not hold by
//     the identifier derived from a Patient its request carries for it (by that
//     id or member identifier) with demographics of its own (FR-G61), and
//     anything else unknown.
//
// Never under an identifier derived from an id alone: that identifier names no
// one the network knows, and the same person named by another id would be
// recorded under another. A read the system fails to answer is returned as is.
func (g *Gateway) identifyNamed(ctx context.Context, m namedMember, legSubject string) (string, int, error) {
	switch {
	case m.subject:
		return "", namedSubject, nil
	case m.shared:
		return "", namedUnknown, nil
	}
	pci, held, found, err := g.resolveBinding(ctx, m.member, m.scope, true)
	switch {
	case errors.Is(err, errPatientUnidentifiable):
		return "", namedUnknown, nil
	case err != nil:
		return "", 0, err
	case !found || pci == "":
		return "", namedUnknown, nil
	case pci == legSubject:
		return pci, namedSubject, nil
	case held:
		return pci, namedOther, nil
	case !m.ownScope:
		// Off coverage eligibility, a member the system of record does not
		// hold is never named under an identifier derived from the request:
		// whether another Patient the request carries is that member, the
		// leg's own patient or someone else is not decided by the request's
		// structure.
		return "", namedUnknown, nil
	}
	if _, carried := carriedDemographics(m.scope, m.member, true); carried {
		return pci, namedOther, nil
	}
	return "", namedUnknown, nil
}

// namedMember is a member a request names, with the bytes its patient is
// bound from.
type namedMember struct {
	member string
	scope  []byte
	// pas is whether the leg is PAS submit or update, whose request names its
	// subject by Claim.patient: subject and shared are decided by the
	// request's structure there (pasSubjectNames), and a member the system of
	// record does not hold is unknown.
	pas, subject, shared bool
	// ownScope is whether the member is bound from resources of its own, apart
	// from the rest of the request (a coverage-eligibility batch's request):
	// subjectCarriedNames reads no name of it across the request.
	ownScope bool
	// legName is whether the member is the name the leg's subject binding
	// reads the leg's own patient by (CRD's context.patientId), a member read
	// as any other: subjectCarriedNames takes it as one of the subject's names
	// whatever its own read makes of it, since that read and the binding's
	// may differ (identifyNamed reads a Patient under the member's id as its
	// own record though a Coverage names it as its party; the binding does not).
	legName bool
}

// namedMembers is every member a request on leg names, with the bytes its
// patient is bound from, as the workstream that declares the leg reads them
// (its adapter's requestMembers); none for a leg no compiled-in workstream
// declares.
func namedMembers(payload []byte, leg string) []namedMember {
	w, ok := workstreamFor(leg)
	if !ok {
		return nil
	}
	return w.requestMembers(leg, payload)
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

// withInvolvedCollector gives a leg whose answer names the responder's own
// binding (legInvolvement) a collector when the Hub reads the involved list;
// otherwise ctx is returned as is.
func (g *Gateway) withInvolvedCollector(ctx context.Context, leg string) context.Context {
	if _, answer := legInvolvement(leg); !g.cfg.HubAcceptsInvolved || !answer {
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
