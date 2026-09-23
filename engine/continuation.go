// continuation.go — the server-held prior-authorization continuation: what a
// provider gateway keeps about an authorization a payer pended, so the same
// decision can be asked for again later.
//
// WHY IT EXISTS. A payer answers a submission with its own decision, and a pend
// is one of those answers. Nothing polls for a later one: a later decision comes
// only from an explicit inquiry. The requester therefore needs, minutes or days
// afterwards, the handful of facts an inquiry is built from. A participant's own
// system keeps them itself (it has its own records). The gateway's OWN originator
// flows — the operator console, the scenario routes and the headless
// provider-data runs — have no such system behind them, so the gateway keeps the
// record for them here.
//
// WHAT IS STORED IS METADATA ONLY (AI-1). Identity, routing, the member, the
// identifiers the payer answered with, and the item map: the sequence, product
// code and service date of each line that was submitted. No order bytes, no
// QuestionnaireResponse bytes, nothing clinical. At inquiry time the order is
// re-read from the participant's own system of record by its reference, and the
// item map is what says whether what is there now is still what was submitted.
// TestContinuation_NoClinicalColumns is the durable half of that guard.
//
// THE ID IS THE CAPABILITY. It carries 128 bits of randomness and is bound to
// {holder, continuationID}. An id this gateway's holder did not mint answers 404,
// never 403: an operator surface that distinguished "not yours" from "no such
// thing" would say which ids exist.
//
// DURABILITY IS A DISCLOSED PROPERTY, NOT AN ASSUMPTION. A deployment with
// Postgres shares continuations across replicas and across restarts. A deployment
// without one (the Kit, a single-process gateway) holds them in memory, where a
// restart loses them — and a continuation that is asked for after such a restart
// answers 410 with the sentence below, never a bare "not found". Saying "unknown"
// for state the gateway itself dropped would put the fault on the caller. See
// ContinuationLost.
package engine

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ContinuationRetention is how long a continuation survives its last update. It
// is the pend ledger's period, deliberately: a continuation exists to ask the
// ledger's own authorization about itself, so one outliving the other would leave
// either a capability naming nothing or an authorization nothing can name.
const ContinuationRetention = PendLedgerRetention

// The continuation id's shape. The id is opaque to every caller; only the store
// that minted it reads it, and it reads exactly one thing — whether the id came
// from a store that can still have it.
//
// The random half is 128 bits and is what makes the id a capability. The marker
// half is what lets a store tell "an id I never minted" from "an id an earlier
// instance of me minted and could not keep", which is the difference between the
// 404 and the 410 below. A volatile store stamps its own instance; a durable one
// stamps a constant, because a durable store's restart loses nothing.
const (
	// ContinuationVolatileMark prefixes an id minted by a store that cannot keep
	// it across a restart; ContinuationDurableMark prefixes one minted by a store
	// that can. Exported because the durable backend lives in another package and
	// must mint under the same rule the reader applies.
	ContinuationVolatileMark = "m"
	ContinuationDurableMark  = "d"
	// continuationRandomBytes is the capability's entropy: 128 bits.
	continuationRandomBytes = 16
	// continuationInstanceBytes is the volatile instance stamp. It is not a
	// secret and adds no authority — it only names which run of the process
	// minted the id.
	continuationInstanceBytes = 4
)

// ContinuationLookup is what resolving a continuation id found. It is three
// answers, not two, because "the gateway dropped it" and "there is no such id"
// are different facts and a caller acts on them differently: one says try again
// from your own records, the other says this id names nothing.
type ContinuationLookup int

const (
	// ContinuationFound: the record is returned.
	ContinuationFound ContinuationLookup = iota + 1
	// ContinuationUnknown: no such continuation for this holder. 404 — never
	// 403, which would confirm that the id exists somewhere.
	ContinuationUnknown
	// ContinuationLost: the id was minted by a NON-DURABLE store that is not
	// this run — normally an earlier run of this gateway, and in any case one
	// that cannot have kept the record. 410, with the sentence
	// ContinuationLostMessage states.
	ContinuationLost
)

// ContinuationLostMessage is what a gateway answers when it dropped a
// continuation it had minted. It names the cause and both ways forward: submit
// again, or inquire from the requester's own system, which never depended on this
// gateway's memory in the first place.
const ContinuationLostMessage = "continuation lost (gateway restarted; submit again or inquire from your own system)"

// ContinuationUnknownMessage is the answer for an id this holder has no record of
// minting. It says nothing about whether such an id exists elsewhere.
const ContinuationUnknownMessage = "unknown continuation"

// The continuation store's guards.
var (
	// ErrContinuationHolderRequired: the holder is half the binding, so a record
	// without one would be bound to nothing.
	ErrContinuationHolderRequired = errors.New("engine: continuation: a holder is required")
	// ErrContinuationItemsInconsistent: the stored trace-number list and the item
	// map are two views of one submission. They are written together, so they
	// disagree only when a caller built them separately — which would leave an
	// inquiry naming lines the submission did not have.
	ErrContinuationItemsInconsistent = errors.New("engine: continuation: the item trace numbers do not match the item map")
)

// ContinuationItem is one submitted line, as the submission stated it and as the
// payer answered about it: its sequence, the product it asked for, the date of
// service, the trace number the payer echoes, and the authorization and
// administration reference numbers the payer gave the line. Metadata; no
// clinical content.
//
// The two reference numbers are here because an inquiry carries them when they
// are held — they are the authorization number and the administration reference
// number a payer matches an inquiry on. A continuation that did not keep them
// could only ever build a less specific inquiry than the one the payer expects.
type ContinuationItem struct {
	// Sequence is the Claim.item.sequence the line was submitted under.
	Sequence int
	// ProductCode is the line's product coding as "system|code".
	ProductCode string
	// ProductDisplay is that coding's human-readable description, as the
	// participant's own order stated it. It identifies nothing — ProductCode
	// does that — and it is kept because the INQUIRY carries it and the payer's
	// decision resource renders it (FR-28): a continuation that dropped it would
	// leave a patient reading a bare procedure code for a service their own
	// records describe in words.
	ProductDisplay string
	// ServiceDate is the line's date of service, as the submission stated it.
	ServiceDate string
	// TraceNumber is the line's item trace number as "system|value", or empty
	// when the line carried none.
	TraceNumber string
	// AuthorizationNumber and AdministrationReferenceNumber are what the payer
	// gave this line, when it gave them.
	AuthorizationNumber           string
	AdministrationReferenceNumber string
}

// Continuation is the record a provider gateway keeps for one pended
// authorization. Every field is metadata: identity, routing, the member, what was
// asked, and what the payer answered with.
type Continuation struct {
	// ID is the capability. Empty on the way in means "mint one".
	ID string
	// Holder is the gateway's own holder id — the other half of the binding.
	Holder string
	// PayerHolder and Line route the inquiry: the payer participant, and the
	// prior-authorization line the submission ran at, so a continuation finishes
	// on the line it started on.
	PayerHolder string
	Line        string
	// CorrID is the submission's correlation id, kept so a continuation and the
	// exchange it continues can be read together.
	CorrID string
	// SubjectPCI, MemberID and SoRPatientID identify the patient three ways: as
	// the substrate subject, as the member the payer knows, and as the record in
	// the participant's own system the inquiry re-reads from.
	SubjectPCI   string
	MemberID     string
	SoRPatientID string
	// OrderRef is the reference of the order in the participant's OWN system.
	// The order itself is never stored: it is re-read at inquiry time, and the
	// item map below is what says whether it still matches what was submitted.
	OrderRef string
	// ProviderNPI is the ordering provider's NPI, which the inquiry must name.
	ProviderNPI string
	// ClaimIdentifier is the submitted Claim.identifier as "system|value" — the
	// one the payer echoes in ClaimResponse.request.identifier.
	ClaimIdentifier string
	// ClaimReferences are exact request-side Claim references retained from the
	// submitted Bundle for selected inquiry-answer linkage. They are not
	// inferred from a payer reply or an identifier.
	ClaimReferences []string
	// ClaimType and ClaimPriority are the submitted Claim's type and priority
	// codings, as "system|code". An inquiry Claim must carry the same ones the
	// request carried, so a continuation that did not keep them could not build
	// a conformant inquiry at all — the inquiry builder refuses without them. The
	// codings' DISPLAY strings are not retained: nothing reads them, and the
	// builder does not need them.
	ClaimType     string
	ClaimPriority string
	// ItemTraceNumbers are the submitted item trace numbers, in submission
	// order. They are the same values Items carries; both are stored because the
	// durable schema keeps the lines in a child table and the submitted list on
	// the row, and PutContinuation refuses a pair that disagrees.
	ItemTraceNumbers []string
	// Items is the item map: what each submitted line asked for.
	Items []ContinuationItem
	// PayerClaimResponseIDs and PayerPreAuthRef are what the payer's pend
	// answered with — the keys a later inquiry names the authorization by.
	PayerClaimResponseIDs []string
	PayerPreAuthRef       string
	// LastOutcome is the last thing the payer said about this authorization:
	// "pended", or a decision once one arrived.
	LastOutcome string
	// CreatedAt and UpdatedAt are the store's own clock.
	CreatedAt, UpdatedAt time.Time
}

// The outcomes a continuation records. A pend is the state a continuation exists
// for; a decision is what closes it.
const (
	ContinuationOutcomePended   = "pended"
	ContinuationOutcomeApproved = PendOutcomeApproved
	ContinuationOutcomeDenied   = PendOutcomeDenied
)

// PayerKeys projects the payer-stated lookup keys this continuation holds, in the
// requester's own namespace. It is what an answer is matched back by, and it
// deliberately does NOT include a fresh inquiry's own Claim.identifier.
func (c Continuation) PayerKeys(requesterHolder string) PendKeys {
	k := PendKeys{RequesterHolder: requesterHolder, PreAuthRef: c.PayerPreAuthRef}
	if c.ClaimIdentifier != "" {
		k.RequestIDs = append(k.RequestIDs, c.ClaimIdentifier)
	}
	k.ClaimResponseIDs = append(k.ClaimResponseIDs, c.PayerClaimResponseIDs...)
	k.ItemTraceNumbers = append(k.ItemTraceNumbers, c.ItemTraceNumbers...)
	return k
}

// ContinuationStore holds the continuations one gateway minted. It is OPTIONAL on
// the Store seam: a Store that does not implement it leaves the gateway with the
// in-memory default, and the deployment's durability is what that choice
// discloses — never something the exchange path silently depends on.
type ContinuationStore interface {
	// PutContinuation writes c and returns it as stored. An empty ID mints one;
	// a non-empty one updates that holder's record of that id. The binding is the
	// KEY, so one holder writing an id cannot reach another holder's record of the
	// same id — there is nothing to refuse. CreatedAt is set on the first write
	// and preserved after it; UpdatedAt is the store's clock on every write.
	PutContinuation(c Continuation) (Continuation, error)
	// ReadContinuation resolves {holder, id}. See ContinuationLookup: found,
	// unknown (404) or lost (410). The error channel is a store failure, so
	// "unavailable" never reads as "unknown".
	ReadContinuation(holder, id string) (Continuation, ContinuationLookup, error)
}

// ContinuationsOf reports the Store's continuation store, if it has one. This is
// the assertion the originator flows branch on.
func ContinuationsOf(s Store) (ContinuationStore, bool) {
	c, ok := s.(ContinuationStore)
	if !ok {
		return nil, false
	}
	return c, true
}

// ContinuationRefusal maps a lookup that did not find a record to the status and
// message the route answers with. It exists so every surface — the scenario
// route, the console and the Kit — refuses with the SAME two sentences, and a
// third one cannot be invented at a call site.
func ContinuationRefusal(l ContinuationLookup) (int, string) {
	switch l {
	case ContinuationLost:
		return http.StatusGone, ContinuationLostMessage
	default:
		return http.StatusNotFound, ContinuationUnknownMessage
	}
}

// ValidateContinuation runs the guards every backend applies before it writes.
func ValidateContinuation(c Continuation) error {
	if strings.TrimSpace(c.Holder) == "" {
		return ErrContinuationHolderRequired
	}
	traces := map[string]int{}
	for _, it := range c.Items {
		if it.TraceNumber != "" {
			traces[it.TraceNumber]++
		}
	}
	listed := map[string]int{}
	for _, tn := range c.ItemTraceNumbers {
		if tn != "" {
			listed[tn]++
		}
	}
	if len(traces) != len(listed) {
		return fmt.Errorf("%w: %d in the item map, %d listed", ErrContinuationItemsInconsistent, len(traces), len(listed))
	}
	for tn, n := range traces {
		if listed[tn] != n {
			return fmt.Errorf("%w: %q appears %d times in the item map and %d times in the list", ErrContinuationItemsInconsistent, tn, n, listed[tn])
		}
	}
	return nil
}

// NewContinuationID mints an id for a store with the given mark. The random half
// is 128 bits from crypto/rand; a failure there is unrecoverable, exactly as it
// is for a correlation id, because a weak capability is worse than no capability.
func NewContinuationID(mark string) string {
	var b [continuationRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("gateway: crypto/rand failed generating a continuation id: %v", err))
	}
	return mark + "-" + hex.EncodeToString(b[:])
}

// continuationMarkOf returns the mark an id was minted under, and whether the id
// has the shape a store mints at all.
func continuationMarkOf(id string) (string, bool) {
	mark, rest, ok := strings.Cut(id, "-")
	if !ok || mark == "" || len(rest) != 2*continuationRandomBytes {
		return "", false
	}
	if _, err := hex.DecodeString(rest); err != nil {
		return "", false
	}
	return mark, true
}

// MemContinuations is the in-memory ContinuationStore: the default for a
// deployment with no durable store behind it.
//
// It is NOT a stand-in for the durable one. It answers the same three lookups,
// and the one place the two differ is the one this design discloses: what this
// store minted before a restart is gone, and it says so with ContinuationLost
// rather than pretending the caller made the id up. That is the whole reason the
// id carries an instance mark.
type MemContinuations struct {
	mu sync.Mutex
	// mark is this instance's mint mark: the volatile marker plus a stamp for
	// THIS run of the process.
	mark string
	// byID is keyed by holder + "|" + id — the binding, not the id alone.
	byID map[string]Continuation
	// now is the store's own clock (retention, timestamps). Injected so the
	// retention rows are deterministic; never reassigned outside tests.
	now       func() time.Time
	lastPurge time.Time
}

var _ ContinuationStore = (*MemContinuations)(nil)

// NewMemContinuations returns an in-memory continuation store with a fresh
// instance mark, so ids it minted in an earlier run are recognizable as lost.
func NewMemContinuations() *MemContinuations {
	return &MemContinuations{
		mark: newContinuationMark(),
		byID: map[string]Continuation{},
		now:  time.Now,
	}
}

// newContinuationMark stamps one generation of in-memory continuations: the
// volatile marker plus randomness for THIS run. Two generations never share a
// mark, which is what makes an id from an earlier one recognizable as lost.
func newContinuationMark() string {
	var b [continuationInstanceBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("gateway: crypto/rand failed generating a continuation instance mark: %v", err))
	}
	return ContinuationVolatileMark + hex.EncodeToString(b[:])
}

func continuationKey(holder, id string) string { return holder + "|" + id }

// PutContinuation writes c. See ContinuationStore.
func (m *MemContinuations) PutContinuation(c Continuation) (Continuation, error) {
	if err := ValidateContinuation(c); err != nil {
		return Continuation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeLocked()
	now := m.now()
	if c.ID == "" {
		c.ID = NewContinuationID(m.mark)
		c.CreatedAt = now
	} else if prev, ok := m.byID[continuationKey(c.Holder, c.ID)]; ok {
		c.CreatedAt = prev.CreatedAt
	} else if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	stored := c.clone()
	m.byID[continuationKey(c.Holder, c.ID)] = stored
	return stored.clone(), nil
}

// ReadContinuation resolves {holder, id}. See ContinuationStore.
func (m *MemContinuations) ReadContinuation(holder, id string) (Continuation, ContinuationLookup, error) {
	if strings.TrimSpace(holder) == "" {
		return Continuation{}, ContinuationUnknown, ErrContinuationHolderRequired
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeLocked()
	if c, ok := m.byID[continuationKey(holder, id)]; ok {
		return c.clone(), ContinuationFound, nil
	}
	// The one thing the id is read for, and it is worth being exact about what it
	// establishes. A volatile mark that is not THIS run's says the id was minted
	// by a non-durable store other than this one — in practice an earlier run of
	// this gateway, which is the case the disclosure exists for, though the store
	// keeps no register of its own past marks and so cannot prove that is which.
	// Either way the record is gone from a store that could not have kept it, and
	// telling the caller that is more use than "unknown".
	//
	// Anything else — this run's own mark, a durable mark, or no recognizable
	// shape at all — is simply not a record here, and says only that.
	if mark, ok := continuationMarkOf(id); ok &&
		strings.HasPrefix(mark, ContinuationVolatileMark) && mark != m.mark {
		return Continuation{}, ContinuationLost, nil
	}
	return Continuation{}, ContinuationUnknown, nil
}

// Durable reports that this store does not survive a restart. The Kit UI and
// CONFIGURATION state the same fact; this is where a surface reads it rather than
// re-deriving it from how the gateway happens to be wired.
func (m *MemContinuations) Durable() bool { return false }

// ResetContinuations clears the store (the demo reset contract). It starts a NEW
// generation — a fresh instance mark — so an id minted before the reset reads as
// what it is: a continuation this gateway had and discarded, answered with
// ContinuationLost rather than "unknown". A reset is the operator's own version
// of the restart this store already discloses, and the caller's way forward is
// the same either way.
func (m *MemContinuations) ResetContinuations() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mark = newContinuationMark()
	m.byID = map[string]Continuation{}
	m.lastPurge = time.Time{}
}

// purgeLocked runs the lazy retention sweep on the same throttle and the same
// bound the pend ledger's does, so the two backends' observable retention is one
// rule rather than two that must be kept in step.
func (m *MemContinuations) purgeLocked() {
	now := m.now()
	if now.Sub(m.lastPurge) < PendPurgeInterval {
		return
	}
	m.lastPurge = now
	cutoff := now.Add(-ContinuationRetention)
	purged := 0
	for k, c := range m.byID {
		if purged >= PendPurgeMax {
			return
		}
		if c.UpdatedAt.After(cutoff) {
			continue
		}
		delete(m.byID, k)
		purged++
	}
}

// clone returns a deep copy, so a caller cannot reach into stored state through
// the slices it handed in or got back.
func (c Continuation) clone() Continuation {
	out := c
	out.ItemTraceNumbers = append([]string(nil), c.ItemTraceNumbers...)
	out.ClaimReferences = append([]string(nil), c.ClaimReferences...)
	out.PayerClaimResponseIDs = append([]string(nil), c.PayerClaimResponseIDs...)
	out.Items = append([]ContinuationItem(nil), c.Items...)
	return out
}

// SortedContinuationItems returns c's items ordered by sequence. The durable
// backend reads its child rows back in whatever order the database gives, so both
// backends order them here and an inquiry's lines cannot depend on the store.
func SortedContinuationItems(items []ContinuationItem) []ContinuationItem {
	out := append([]ContinuationItem(nil), items...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

// DurableContinuations reports whether cs keeps a continuation across a restart.
// A store that does not say is treated as durable: the disclosure belongs to the
// store that knows it is not, and defaulting the other way would put a
// "continuations are lost here" notice on a deployment that keeps them.
func DurableContinuations(cs ContinuationStore) bool {
	d, ok := cs.(interface{ Durable() bool })
	if !ok {
		return true
	}
	return d.Durable()
}

// --- the bridge to the requester-retained continuation ---
//
// There is ONE inquiry builder and ONE answer matcher in this system: the SDK's,
// which a participant's own system already uses for its own records. The
// server-held record is the same facts, flattened into scalar columns so a
// durable store can hold them without an opaque column — not a second model of
// what an inquiry is. A gateway that built its own inquiries would be a parallel
// path, and the two would drift the first time a line moved.

// ContinuationFacts flattens the requester-retained continuation into the stored
// form. It is the ONLY place the SDK's shape becomes columns, so a fact that
// arrives there and is not stored here is visible as a compile error or as the
// round-trip row going red, not as a quietly narrower inquiry.
func ContinuationFacts(c Continuation, sdk shnsdk.PriorAuthContinuation) Continuation {
	c.Line, c.PayerHolder, c.MemberID, c.ProviderNPI = sdk.Line, sdk.PayerHolder, sdk.MemberID, sdk.ProviderNPI
	c.ClaimType = codingKey(sdk.ClaimType.System, sdk.ClaimType.Code)
	c.ClaimPriority = codingKey(sdk.Priority.System, sdk.Priority.Code)
	c.ClaimIdentifier = ""
	c.ClaimReferences = append([]string(nil), sdk.ClaimReferences...)
	if len(sdk.ClaimIdentifiers) > 0 {
		c.ClaimIdentifier = identifierKey(sdk.ClaimIdentifiers[0].System, sdk.ClaimIdentifiers[0].Value)
	}
	c.PayerPreAuthRef = sdk.PreAuthRef
	c.PayerClaimResponseIDs = nil
	for _, id := range sdk.ClaimResponseIdentifiers {
		if k := identifierKey(id.System, id.Value); k != "" {
			c.PayerClaimResponseIDs = append(c.PayerClaimResponseIDs, k)
		}
	}
	c.Items, c.ItemTraceNumbers = nil, nil
	for _, it := range sdk.Items {
		tn := identifierKey(it.TraceNumber.System, it.TraceNumber.Value)
		c.Items = append(c.Items, ContinuationItem{
			Sequence:                      it.Sequence,
			ProductCode:                   codingKey(it.ProductOrService.System, it.ProductOrService.Code),
			ProductDisplay:                it.ProductOrService.Display,
			ServiceDate:                   it.ServiceDate,
			TraceNumber:                   tn,
			AuthorizationNumber:           it.AuthorizationNumber,
			AdministrationReferenceNumber: it.AdministrationReferenceNumber,
		})
		if tn != "" {
			c.ItemTraceNumbers = append(c.ItemTraceNumbers, tn)
		}
	}
	return c
}

// SDKContinuation inflates the stored record back into the requester-retained
// shape the inquiry builder and the answer matcher take.
func (c Continuation) SDKContinuation() shnsdk.PriorAuthContinuation {
	out := shnsdk.PriorAuthContinuation{
		Line:        c.Line,
		PayerHolder: c.PayerHolder,
		MemberID:    c.MemberID,
		ProviderNPI: c.ProviderNPI,
		PreAuthRef:  c.PayerPreAuthRef,
		ClaimType:   codingOfKey(c.ClaimType),
		Priority:    codingOfKey(c.ClaimPriority),
	}
	out.ClaimReferences = append([]string(nil), c.ClaimReferences...)
	if id, ok := identifierOfKey(c.ClaimIdentifier); ok {
		out.ClaimIdentifiers = append(out.ClaimIdentifiers, id)
	}
	for _, k := range c.PayerClaimResponseIDs {
		if id, ok := identifierOfKey(k); ok {
			out.ClaimResponseIdentifiers = append(out.ClaimResponseIdentifiers, id)
		}
	}
	for _, it := range SortedContinuationItems(c.Items) {
		product := codingOfKey(it.ProductCode)
		product.Display = it.ProductDisplay
		item := shnsdk.PASInquiryItem{
			Sequence:                      it.Sequence,
			ProductOrService:              product,
			ServiceDate:                   it.ServiceDate,
			AuthorizationNumber:           it.AuthorizationNumber,
			AdministrationReferenceNumber: it.AdministrationReferenceNumber,
		}
		if id, ok := identifierOfKey(it.TraceNumber); ok {
			item.TraceNumber = id
		}
		out.Items = append(out.Items, item)
	}
	return out
}

// identifierOfKey splits a stored "system|value" back into its halves. A key
// with no separator is not an identifier this store wrote, and yields nothing
// rather than an identifier with an empty system.
func identifierOfKey(key string) (shnsdk.PASIdentifier, bool) {
	system, value, ok := strings.Cut(key, "|")
	if !ok || system == "" || value == "" {
		return shnsdk.PASIdentifier{}, false
	}
	return shnsdk.PASIdentifier{System: system, Value: value}, true
}

// codingKey is the stored form of a Coding: "system|code".
//
// It is NOT identifierKey. A Coding may legitimately carry a code with no system
// — `Claim.priority` is exactly that, a bare "normal" from a value set the
// profile fixes — and identifierKey drops such a pair, which would leave the
// inquiry builder refusing to build for want of a priority the request plainly
// had. A missing CODE is the real absence, and that is what yields nothing here.
func codingKey(system, code string) string {
	if strings.TrimSpace(code) == "" {
		return ""
	}
	return system + "|" + code
}

// codingOfKey splits a stored "system|code" into a Coding. The display is not
// stored, so it comes back empty — it is decoration, and nothing on this path
// reads it.
func codingOfKey(key string) shnsdk.PASCoding {
	system, code, ok := strings.Cut(key, "|")
	if !ok {
		return shnsdk.PASCoding{}
	}
	return shnsdk.PASCoding{System: system, Code: code}
}
