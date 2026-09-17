package relay

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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

// Is reports whether target is ErrOwnershipRefused.
func (e *OwnershipError) Is(target error) bool { return target == ErrOwnershipRefused }

// testBinary reports whether the process is a test binary. Payloads that
// tests inject are admitted only there.
var testBinary = testing.Testing

var (
	relayed  = []Ownership{OwnershipRelayed}
	authored = []Ownership{OwnershipAuthored}
	either   = []Ownership{OwnershipRelayed, OwnershipAuthored}
	// payerIdentityMapped is a request relayed exactly or with the payer
	// identity mapped.
	payerIdentityMapped = []EditID{EditPayorEdgeRestamp}
)

func refusal(leg string, role Role) (Key, Rule) {
	return Key{leg, role, DirectionResponse, OutcomeRefused},
		Rule{Allowed: authored, Builders: []BuilderID{BuilderGatewayRefusal}}
}

// Legs names every exchange leg the table covers, in sorted order.
func Legs() []string { return slices.Clone(legs) }

var legs = []string{
	"coverage-eligibility",
	"crd-order-dispatch",
	"crd-order-select",
	"dtr-questionnaire-fetch",
	"federated-query",
	"pas-claim",
	"pas-claim-update",
	"patient-dtr",
}

// legOwnership is the pinned table. It records what each transmit carries
// today, including the paths that still rebuild a participant's message
// under an interim builder; such a path is permitted only where it is
// listed here.
//
// Transmits that are not exchange legs are outside this table: the
// gateway's discovery and conformance documents (CDS Services discovery,
// CapabilityStatement, the Da Vinci configuration document), its token and
// authorization endpoints, the payer's Patient Access API, the call to the
// participant's own pre-population service, the reads that send no body
// (service discovery, a follow-up read of a pending decision, the demo
// patient-view lookup), and the summaries a scenario endpoint returns to its
// caller.
var legOwnership = func() map[Key]Rule {
	m := map[Key]Rule{
		// Requester to the network, carrying its participant's request.
		// A prior-authorization Bundle is relayed exactly; a CDS Hooks
		// request is relayed exactly or with the callback removed and absent
		// prefetch values added; a questionnaire package request is relayed
		// exactly or with the patient's Coverage added when it carries none.
		{"crd-order-dispatch", RoleRequester, DirectionRequest, OutcomeCarried}: {
			Allowed: []Ownership{OwnershipRelayed, OwnershipEdited}, Edits: []EditID{EditCDSCallbackStrip, EditCDSPrefetchObtain},
		},
		{"crd-order-select", RoleRequester, DirectionRequest, OutcomeCarried}: {
			Allowed: []Ownership{OwnershipRelayed, OwnershipEdited}, Edits: []EditID{EditCDSCallbackStrip, EditCDSPrefetchObtain},
		},
		{"dtr-questionnaire-fetch", RoleRequester, DirectionRequest, OutcomeCarried}: {
			Allowed: []Ownership{OwnershipRelayed, OwnershipEdited}, Edits: []EditID{EditDTRCoverageObtain},
		},
		{"pas-claim", RoleRequester, DirectionRequest, OutcomeCarried}:        {Allowed: relayed},
		{"pas-claim-update", RoleRequester, DirectionRequest, OutcomeCarried}: {Allowed: relayed},

		// Requester to the network, with a request its own workflow makes.
		{"coverage-eligibility", RoleRequester, DirectionRequest, OutcomeOriginated}:    {Allowed: authored, Builders: []BuilderID{BuilderSDKEligibility}},
		{"crd-order-dispatch", RoleRequester, DirectionRequest, OutcomeOriginated}:      {Allowed: authored, Builders: []BuilderID{BuilderSDKCRDRequest}},
		{"crd-order-select", RoleRequester, DirectionRequest, OutcomeOriginated}:        {Allowed: authored, Builders: []BuilderID{BuilderSDKCRDRequest}},
		{"dtr-questionnaire-fetch", RoleRequester, DirectionRequest, OutcomeOriginated}: {Allowed: authored, Builders: []BuilderID{BuilderSDKDTRPackage, BuilderDTRNextQuestion}},
		{"federated-query", RoleRequester, DirectionRequest, OutcomeOriginated}:         {Allowed: authored, Builders: []BuilderID{BuilderSDKFederatedQuery}},
		{"patient-dtr", RoleRequester, DirectionRequest, OutcomeOriginated}:             {Allowed: authored, Builders: []BuilderID{BuilderSDKPatientDTR}},
		{"pas-claim", RoleRequester, DirectionRequest, OutcomeOriginated}:               {Allowed: authored, Builders: []BuilderID{BuilderSDKPASSubmit}},
		{"pas-claim-update", RoleRequester, DirectionRequest, OutcomeOriginated}:        {Allowed: authored, Builders: []BuilderID{BuilderSDKPASUpdate}},

		// Requester to its participant's system: the recipient's answer,
		// exactly as it arrived.
		{"crd-order-dispatch", RoleRequester, DirectionResponse, OutcomeAnswered}:      {Allowed: relayed},
		{"crd-order-select", RoleRequester, DirectionResponse, OutcomeAnswered}:        {Allowed: relayed},
		{"dtr-questionnaire-fetch", RoleRequester, DirectionResponse, OutcomeAnswered}: {Allowed: relayed},
		{"pas-claim", RoleRequester, DirectionResponse, OutcomeAnswered}:               {Allowed: relayed},
		{"pas-claim-update", RoleRequester, DirectionResponse, OutcomeAnswered}:        {Allowed: relayed},

		// Recipient to its participant's system, carrying the network's
		// request. A prior-authorization Bundle and a CDS Hooks request are
		// relayed exactly, or with the optional payer-identity mapping. A
		// questionnaire operation's own input is relayed the same way; the
		// older questionnaire request envelope is still rebuilt.
		{"crd-order-dispatch", RoleRecipient, DirectionRequest, OutcomeCarried}: {
			Allowed: []Ownership{OwnershipRelayed, OwnershipEdited}, Edits: payerIdentityMapped,
		},
		{"crd-order-select", RoleRecipient, DirectionRequest, OutcomeCarried}: {
			Allowed: []Ownership{OwnershipRelayed, OwnershipEdited}, Edits: payerIdentityMapped,
		},
		{"dtr-questionnaire-fetch", RoleRecipient, DirectionRequest, OutcomeCarried}: {
			Allowed:  []Ownership{OwnershipRelayed, OwnershipEdited, OwnershipAuthored},
			Edits:    payerIdentityMapped,
			Builders: []BuilderID{BuilderInterimDTRProjection},
		},
		{"pas-claim", RoleRecipient, DirectionRequest, OutcomeCarried}: {
			Allowed: []Ownership{OwnershipRelayed, OwnershipEdited}, Edits: payerIdentityMapped,
		},
		{"pas-claim-update", RoleRecipient, DirectionRequest, OutcomeCarried}: {
			Allowed: []Ownership{OwnershipRelayed, OwnershipEdited}, Edits: payerIdentityMapped,
		},

		// Recipient to the network, answering. Eligibility, patient-authored
		// answers and data-request answers are the gateway's own messages;
		// the others are the participant's answer, relayed, except where an
		// interim builder still rebuilds it.
		{"coverage-eligibility", RoleRecipient, DirectionResponse, OutcomeAnswered}:    {Allowed: authored, Builders: []BuilderID{BuilderSDKEligibility}},
		{"crd-order-dispatch", RoleRecipient, DirectionResponse, OutcomeAnswered}:      {Allowed: relayed},
		{"crd-order-select", RoleRecipient, DirectionResponse, OutcomeAnswered}:        {Allowed: relayed},
		{"dtr-questionnaire-fetch", RoleRecipient, DirectionResponse, OutcomeAnswered}: {Allowed: relayed},
		{"federated-query", RoleRecipient, DirectionResponse, OutcomeAnswered}:         {Allowed: authored, Builders: []BuilderID{BuilderCDexFulfillment}},
		{"patient-dtr", RoleRecipient, DirectionResponse, OutcomeAnswered}:             {Allowed: authored, Builders: []BuilderID{BuilderSDKPatientDTR}},
		{"pas-claim", RoleRecipient, DirectionResponse, OutcomeAnswered}:               {Allowed: either, Builders: []BuilderID{BuilderInterimPASAssembly}},
		{"pas-claim-update", RoleRecipient, DirectionResponse, OutcomeAnswered}:        {Allowed: either, Builders: []BuilderID{BuilderInterimPASAssembly}},
	}
	// An application error from the participant's system is relayed. On
	// the recipient an interim builder still replaces an empty error body,
	// and the bare error a requester that negotiated no frame receives.
	for _, leg := range []string{"crd-order-dispatch", "crd-order-select", "dtr-questionnaire-fetch", "pas-claim", "pas-claim-update"} {
		m[Key{leg, RoleRecipient, DirectionResponse, OutcomeUpstreamError}] = Rule{
			Allowed: either, Builders: []BuilderID{BuilderInterimEmptyErrorSubstitution},
		}
	}
	// On the requester, an application error the recipient answered with is
	// relayed to the participant's system exactly as it arrived.
	for _, leg := range legs {
		m[Key{leg, RoleRequester, DirectionResponse, OutcomeUpstreamError}] = Rule{Allowed: relayed}
	}
	// Either side may refuse, with its own refusal, on any leg and before a
	// leg is established.
	for _, leg := range append([]string{LegUnestablished}, legs...) {
		for _, role := range []Role{RoleRequester, RoleRecipient} {
			k, r := refusal(leg, role)
			m[k] = r
		}
	}
	return m
}()

// LegOwnership returns a copy of the pinned table.
func LegOwnership() map[Key]Rule {
	out := make(map[Key]Rule, len(legOwnership))
	for k, r := range legOwnership {
		out[k] = r.clone()
	}
	return out
}

// Check returns the permission check for transmit k, for use with
// Transmit. The check refuses, with an *OwnershipError:
//
//   - a Payload no constructor made;
//   - a key the table does not list;
//   - an ownership value the key does not permit;
//   - an edited payload carrying an edit the key does not list;
//   - an authored payload from a builder the key does not list.
//
// A payload a test injected is admitted on any listed key, and only in a
// test binary.
func Check(k Key) func(Payload) error { return checkIn(legOwnership, k) }

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
			for _, e := range edits {
				if !slices.Contains(rule.Edits, e) {
					return refuse("edit " + string(e) + " not permitted")
				}
			}
		case OwnershipAuthored:
			if !slices.Contains(rule.Builders, p.Builder()) {
				return refuse("builder not permitted")
			}
		}
		return nil
	}
}
