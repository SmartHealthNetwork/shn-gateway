package relay

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Role is the part a gateway plays on an exchange leg.
type Role uint8

const (
	// RoleRequester: the gateway that sends the leg's request into the
	// network and receives its answer (a provider gateway, for example).
	RoleRequester Role = iota + 1
	// RoleRecipient: the gateway that receives the leg's request from the
	// network and answers it (a payer, facility or patient-surface gateway).
	RoleRecipient
)

func (r Role) String() string {
	switch r {
	case RoleRequester:
		return "requester"
	case RoleRecipient:
		return "recipient"
	}
	return "Role(" + strconv.Itoa(int(r)) + ")"
}

// Direction says whether a transmit carries a leg's request or its answer.
//
// The role and direction together name the transmit boundary:
//
//   - requester, request: the gateway to the network;
//   - requester, response: the gateway to its own participant's system (the
//     EHR, or whatever called the gateway);
//   - recipient, request: the gateway to its own participant's system;
//   - recipient, response: the gateway to the network.
type Direction uint8

const (
	// DirectionRequest: the leg's request.
	DirectionRequest Direction = iota + 1
	// DirectionResponse: the leg's answer.
	DirectionResponse
)

func (d Direction) String() string {
	switch d {
	case DirectionRequest:
		return "request"
	case DirectionResponse:
		return "response"
	}
	return "Direction(" + strconv.Itoa(int(d)) + ")"
}

// Outcome classifies what a transmit carries.
type Outcome uint8

const (
	// OutcomeCarried (requests): a request another system made, carried on
	// to the next hop. On the requester, the participant's own system sent
	// it to the gateway; on the recipient, it arrived from the network.
	OutcomeCarried Outcome = iota + 1
	// OutcomeOriginated (requests): a request the gateway's own workflow
	// makes.
	OutcomeOriginated
	// OutcomeAnswered (responses): a successful answer.
	OutcomeAnswered
	// OutcomeUpstreamError (responses): an application error the answering
	// system returned, carried back.
	OutcomeUpstreamError
	// OutcomeRefused (responses): a refusal the gateway itself makes.
	OutcomeRefused
)

func (o Outcome) String() string {
	switch o {
	case OutcomeCarried:
		return "carried"
	case OutcomeOriginated:
		return "originated"
	case OutcomeAnswered:
		return "answered"
	case OutcomeUpstreamError:
		return "upstream-error"
	case OutcomeRefused:
		return "refused"
	}
	return "Outcome(" + strconv.Itoa(int(o)) + ")"
}

// validFor reports whether o can describe a transmit in direction d.
func (o Outcome) validFor(d Direction) bool {
	switch d {
	case DirectionRequest:
		return o == OutcomeCarried || o == OutcomeOriginated
	case DirectionResponse:
		return o == OutcomeAnswered || o == OutcomeUpstreamError || o == OutcomeRefused
	}
	return false
}

// LegUnestablished is the Leg of a refusal made before the leg is known
// (for example, a request that failed authentication).
const LegUnestablished = ""

// Key names one transmit: which leg, which side, which direction and what
// kind of message.
type Key struct {
	Leg       string
	Role      Role
	Direction Direction
	Outcome   Outcome
}

func (k Key) String() string {
	leg := k.Leg
	if leg == LegUnestablished {
		leg = "(no leg)"
	}
	return leg + "/" + k.Role.String() + "/" + k.Direction.String() + "/" + k.Outcome.String()
}

// Rule is what a transmit may carry: the permitted ownership values, the
// edits an OwnershipEdited payload may carry, and the builders an
// OwnershipAuthored payload may come from.
type Rule struct {
	Allowed  []Ownership
	Edits    []EditID
	Builders []BuilderID
}

func (r Rule) clone() Rule {
	return Rule{Allowed: slices.Clone(r.Allowed), Edits: slices.Clone(r.Edits), Builders: slices.Clone(r.Builders)}
}

// RefusedEvent is the name under which transmit boundaries count a payload
// the ownership table refused.
const RefusedEvent = "relay.ownership.refused"

// ErrOwnershipRefused: a payload is not permitted at the transmit it was
// handed to. This is a fault in the gateway; nothing is sent. Returned as an
// *OwnershipError.
var ErrOwnershipRefused = errors.New("relay: payload not permitted at this transmit")

// OwnershipError says which transmit refused which payload, and why.
type OwnershipError struct {
	Key       Key
	Ownership Ownership
	Edits     []EditID
	Builder   BuilderID
	Reason    string
}

func (e *OwnershipError) Error() string {
	desc := e.Ownership.String()
	if len(e.Edits) > 0 {
		ids := make([]string, len(e.Edits))
		for i, id := range e.Edits {
			ids[i] = string(id)
		}
		desc += "[" + strings.Join(ids, ",") + "]"
	}
	if e.Builder != "" {
		desc += " " + string(e.Builder)
	}
	return fmt.Sprintf("relay: %s payload not permitted at %s: %s", desc, e.Key, e.Reason)
}

// Is reports whether target is ErrOwnershipRefused, or
// ErrNoOwnershipRegistered for a transmit checked before any workstream
// registered.
func (e *OwnershipError) Is(target error) bool {
	return target == ErrOwnershipRefused || (target == ErrNoOwnershipRegistered && e.Reason == noOwnership)
}

// testBinary reports whether the process is a test binary. Payloads that
// tests inject are admitted only there.
var testBinary = testing.Testing

// ErrNoOwnershipRegistered: no workstream has registered its ownership rows,
// so no transmit is permitted. The workstream adapters register when the
// gateway engine loads; a program that links this package without the
// engine permits nothing. Returned as an *OwnershipError, which also matches
// ErrOwnershipRefused: nothing is sent.
var ErrNoOwnershipRegistered = errors.New("relay: no workstream ownership registered")

// noOwnership is the OwnershipError reason for a transmit checked before any
// workstream registered.
const noOwnership = "no workstream ownership registered"

// ownershipTable is the table the compiled-in workstreams register once, when
// the gateway engine loads, and every transmit reads after. Registration is
// write-once: a second registration, or one after the table was first read,
// panics, so no transmit is ever checked against a table that later changes.
type ownershipTable struct {
	mu   sync.Mutex
	read bool // the table was read before any registration
	reg  atomic.Pointer[registeredOwnership]
}

type registeredOwnership struct {
	legs  []string
	rules map[Key]Rule
}

var table ownershipTable

// RegisterOwnership registers the exchange legs of the compiled-in
// workstreams and their ownership rows: what each transmit of each leg may
// carry. The table adds the rows every leg has: on the requester, an
// application error relayed to its participant's system exactly as it
// arrived; and on either side, the gateway's own refusal, before a leg is
// established (LegUnestablished) and on any leg. rows must not list those.
//
// The gateway engine registers its workstreams when it loads; nothing else
// calls this. RegisterOwnership panics when the table was already
// registered or already read, or when legs or rows are malformed.
func RegisterOwnership(legs []string, rows map[Key]Rule) { table.register(legs, rows) }

func (t *ownershipTable) register(legs []string, rows map[Key]Rule) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.reg.Load() != nil:
		panic("relay: workstream ownership registered twice")
	case t.read:
		panic("relay: workstream ownership registered after the table was read")
	}
	reg, err := buildOwnership(legs, rows)
	if err != nil {
		panic(err)
	}
	t.reg.Store(reg)
}

// current is the registered table, or nil before registration (which then
// can no longer happen).
func (t *ownershipTable) current() *registeredOwnership {
	if r := t.reg.Load(); r != nil {
		return r
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.read = true
	return t.reg.Load()
}

// buildOwnership checks legs and rows and adds the rows every leg has.
func buildOwnership(legs []string, rows map[Key]Rule) (*registeredOwnership, error) {
	if len(legs) == 0 {
		return nil, errors.New("relay: ownership registered with no legs")
	}
	sorted := slices.Sorted(slices.Values(legs))
	for i, leg := range sorted {
		switch {
		case leg == LegUnestablished:
			return nil, errors.New("relay: a registered leg has no name")
		case i > 0 && sorted[i-1] == leg:
			return nil, fmt.Errorf("relay: leg %q registered twice", leg)
		}
	}
	m := make(map[Key]Rule, len(rows)+3*len(sorted)+2)
	for k, r := range rows {
		switch {
		case !slices.Contains(sorted, k.Leg):
			return nil, fmt.Errorf("relay: %s: the leg is not registered", k)
		case k.Role != RoleRequester && k.Role != RoleRecipient:
			return nil, fmt.Errorf("relay: %s: invalid role", k)
		case !k.Outcome.validFor(k.Direction):
			return nil, fmt.Errorf("relay: %s: the outcome does not fit the direction", k)
		case k.Outcome == OutcomeRefused:
			return nil, fmt.Errorf("relay: %s: refusals are the table's own rows", k)
		case k.Role == RoleRequester && k.Outcome == OutcomeUpstreamError:
			return nil, fmt.Errorf("relay: %s: the requester's relayed application error is the table's own row", k)
		}
		if err := checkRule(r); err != nil {
			return nil, fmt.Errorf("relay: %s: %w", k, err)
		}
		m[k] = r.clone()
	}
	// On the requester, an application error the recipient answered with is
	// relayed to the participant's system exactly as it arrived.
	for _, leg := range sorted {
		m[Key{leg, RoleRequester, DirectionResponse, OutcomeUpstreamError}] = Rule{Allowed: relayed}
	}
	// Either side may refuse, with its own refusal, on any leg and before a
	// leg is established.
	for _, leg := range append([]string{LegUnestablished}, sorted...) {
		for _, role := range []Role{RoleRequester, RoleRecipient} {
			k, r := refusal(leg, role)
			m[k] = r
		}
	}
	return &registeredOwnership{legs: sorted, rules: m}, nil
}

// checkRule refuses a workstream rule that is not closed over the registered
// sets: one that permits nothing, names an ownership value that does not
// exist, lists edits without permitting Edited (or the reverse), lists
// builders without permitting Authored (or the reverse), or names an edit or
// a builder that is not registered. The gateway's refusal and the test
// builder are never a workstream's to list.
func checkRule(r Rule) error {
	if len(r.Allowed) == 0 {
		return errors.New("permits nothing")
	}
	for _, o := range r.Allowed {
		if o < OwnershipRelayed || o > OwnershipAuthored {
			return fmt.Errorf("invalid ownership %v", o)
		}
	}
	if slices.Contains(r.Allowed, OwnershipEdited) != (len(r.Edits) > 0) {
		return errors.New("edits are listed without Edited, or Edited without edits")
	}
	if slices.Contains(r.Allowed, OwnershipAuthored) != (len(r.Builders) > 0) {
		return errors.New("builders are listed without Authored, or Authored without builders")
	}
	for _, e := range r.Edits {
		if !slices.Contains(editIDs, e) {
			return fmt.Errorf("edit %s is not registered", e)
		}
		if slices.Contains(transformEdits, e) && len(r.Builders) == 0 {
			return fmt.Errorf("transform %s is listed without the builders whose messages it carries", e)
		}
	}
	for _, b := range r.Builders {
		switch {
		case b == BuilderGatewayRefusal:
			return errors.New("the gateway's refusal is the table's own row")
		case !slices.Contains(Builders(), b):
			return fmt.Errorf("builder %s is not registered", b)
		}
	}
	return nil
}

var (
	relayed  = []Ownership{OwnershipRelayed}
	authored = []Ownership{OwnershipAuthored}
)

func refusal(leg string, role Role) (Key, Rule) {
	return Key{leg, role, DirectionResponse, OutcomeRefused},
		Rule{Allowed: authored, Builders: []BuilderID{BuilderGatewayRefusal}}
}

// Legs names every registered exchange leg, in sorted order; none before
// registration.
func Legs() []string { return table.legs() }

func (t *ownershipTable) legs() []string {
	r := t.current()
	if r == nil {
		return nil
	}
	return slices.Clone(r.legs)
}

// LegOwnership returns a copy of the registered table; an empty table before
// registration.
func LegOwnership() map[Key]Rule { return table.ownership() }

func (t *ownershipTable) ownership() map[Key]Rule {
	r := t.current()
	if r == nil {
		return map[Key]Rule{}
	}
	out := make(map[Key]Rule, len(r.rules))
	for k, rule := range r.rules {
		out[k] = rule.clone()
	}
	return out
}

// Check returns the permission check for transmit k, for use with
// Transmit. The check refuses, with an *OwnershipError:
//
//   - every payload, while no workstream has registered its ownership
//     (ErrNoOwnershipRegistered);
//   - a Payload no constructor made;
//   - a key the table does not list;
//   - an ownership value the key does not permit;
//   - an edited payload carrying an edit the key does not list;
//   - an authored payload from a builder the key does not list.
//
// A payload a test injected is admitted on any listed key, and only in a
// test binary.
func Check(k Key) func(Payload) error { return table.check(k) }

func (t *ownershipTable) check(k Key) func(Payload) error {
	return func(p Payload) error {
		r := t.current()
		if r == nil {
			return &OwnershipError{Key: k, Ownership: p.Ownership(), Edits: p.Edits(), Builder: p.Builder(), Reason: noOwnership}
		}
		return checkIn(r.rules, k)(p)
	}
}

func checkIn(table map[Key]Rule, k Key) func(Payload) error {
	return func(p Payload) error {
		refuse := func(reason string) error {
			return &OwnershipError{Key: k, Ownership: p.Ownership(), Edits: p.Edits(), Builder: p.Builder(), Reason: reason}
		}
		if p.Ownership() == 0 {
			return refuse("the payload has no ownership")
		}
		rule, ok := table[k]
		if !ok {
			return refuse("the transmit is not in the ownership table")
		}
		if p.Ownership() == OwnershipAuthored && p.Builder() == builderTestInjected {
			if testBinary() {
				return nil
			}
			return refuse("test-injected payloads are admitted only in tests")
		}
		if !slices.Contains(rule.Allowed, p.Ownership()) {
			return refuse("ownership not permitted")
		}
		switch p.Ownership() {
		case OwnershipEdited:
			edits := p.Edits()
			if len(edits) == 0 {
				return refuse("an edited payload names no edit")
			}
			transformed := false
			for _, e := range edits {
				if !slices.Contains(rule.Edits, e) {
					return refuse("edit " + string(e) + " not permitted")
				}
				transformed = transformed || slices.Contains(transformEdits, e)
			}
			// A transform keeps its builder, and only a transform has one: the
			// builder's message, then the edit, and neither half alone.
			switch {
			case transformed && p.Builder() == "":
				return refuse("a transformed payload names no builder")
			case !transformed && p.Builder() != "":
				return refuse("an edited payload names a builder without a transform")
			case transformed && !slices.Contains(rule.Builders, p.Builder()):
				return refuse("builder not permitted")
			}
		case OwnershipAuthored:
			if !slices.Contains(rule.Builders, p.Builder()) {
				return refuse("builder not permitted")
			}
		}
		return nil
	}
}
