// exchange_ir.go — the Layer-2 Canonical Exchange IR: workstream-NEUTRAL
// vocabulary the gateway moves and guards but never clinically interprets. Zero Da
// Vinci, zero provider/payer. A workstream module (Layer 3, e.g. workstream_pa.go)
// supplies the concrete legTypes and content shapes; this file knows only their
// neutral envelope. The wire stays sealed workstream-native bytes (AI-2 / OWD-G3): the IR is
// gateway-INTERNAL, never a neutral wire format.
package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// Content is a typed-opaque handle on a workstream payload. The IR validates it at
// the edge (FR-36/FR-G29) and binds its subject for authority, but never reads its
// clinical semantics. WorkstreamType identifies the owning module (read live by
// OriginateLeg's selection-seam guard). ProfileID carries the routed/pinned
// contract-version token used for routing, building and validation. DeclaredVersion
// independently records what a producer explicitly declares about carried bytes.
// Request framing temporarily retains its legacy ProfileID fallback;
// that routing fallback does not populate DeclaredVersion or VersionSource.
// Payload holds the FHIR/payload bytes that reach the wire. Route is the observer-
// facing routing story: the select-before-build sites set it from
// routeInfoFor(route) alongside ProfileID; it rides through roundTrip's
// leg.originated emission (Content.Route -> ObserverEvent.Route) and is nil
// wherever ProfileID was set by the legacy OriginateLeg fallback (nothing was
// selected to report) or by a contract-unmapped leg.
type Content struct {
	WorkstreamType string
	ProfileID      string
	// DeclaredVersion is supplied by the producer, independently of routing.
	DeclaredVersion string
	VersionSource   string
	// CRDHook is declared CRD addressing, carried inside a sealed request frame
	// only to a recipient advertising v1crd. It is never an HTTP header.
	CRDHook string
	// Payload is the request. It is checked against the leg's ownership row
	// before it is sent: a request this gateway's own workflow builds is
	// relay.Authored by a registered builder; a request carried from the
	// participant's system (Carried) is that system's message.
	Payload relay.Payload
	// Carried marks a request the participant's own system sent to this
	// gateway, carried on to the network (the Da Vinci ingress).
	Carried bool
	Route   *RouteInfo
	// Operation names the DTR operation whose own input Payload is
	// (shnsdk.FrameOperationQuestionnairePackage or
	// shnsdk.FrameOperationNextQuestion); "" for every other request. A
	// request that names one is always sent in a request frame carrying the
	// operation header, and only to a recipient that declares
	// shnsdk.RequestFrameV1Op; any other recipient is refused before
	// anything is sent (framedDTRRefusal).
	Operation string
}

// workstreamPA is the WorkstreamType tag for the Prior-Authorization module.
const workstreamPA = "da-vinci-pa"

// LegPhysics classifies a leg on four workstream-independent axes. These
// four drive transport / authority / audit / commit / lifecycle uniformly, regardless
// of workstream. notification + async are headroom (no PA scenario exercises them) that
// keeps async-pend / payer-to-payer / Provider Access additive.
type LegPhysics struct {
	Kind     string // KindRequestResponse | KindNotification
	Effect   string // EffectReadOnly | EffectMutating
	Timing   string // TimingSync | TimingAsync
	Locality string // LocalitySubstrate | LocalityHolderLocal
}

const (
	KindRequestResponse = "request-response"
	KindNotification    = "notification"
	EffectReadOnly      = "readonly"
	EffectMutating      = "mutating"
	TimingSync          = "sync"
	TimingAsync         = "async"
	LocalitySubstrate   = "substrate-leg"
	LocalityHolderLocal = "holder-local"
)

// Leg is one step of an Exchange: a sealed substrate round-trip or a holder-local
// operation. It carries the workstream-defined legType, the Content moved on it, and
// its leg-physics classification. Subjects are the PCI(s) authority binds to. This is
// SET-SHAPED from day one (PA puts exactly one element) so the IR does not bake in a
// singular assumption it would have to migrate out of; the bulk MANY-subjects
// authority basis (a roster / Group-attribution primitive) is a different, deferred
// Layer-1 primitive, NOT N× the single-PCI binding modeled here.
type Leg struct {
	Type     string
	Physics  LegPhysics
	Content  Content
	Subjects []string
}

// Project is the SINGLE chokepoint where clinical bytes are dropped: the in-flight Leg
// (carrying Content) projects to the metadata-only LegRecord the store keeps. There is no
// other path from Leg to the store, so "bytes never reach the durable seam" is enforced at
// one reviewable line — l.Content is intentionally NOT carried.
func (l Leg) Project(correlationID, outcome string) LegRecord {
	return LegRecord{
		Type:          l.Type,
		CorrelationID: correlationID,
		Subjects:      l.Subjects,
		Physics:       l.Physics,
		Outcome:       outcome,
	}
}

// Exchange is one correlated business interaction: a PA case today; a
// member-transfer / query / remittance later. It holds the correlation root and its
// legs.
//
// DECISION PIN (migration-class): Exchange.ID is the PARENT correlation root that
// GROUPS legs; each Leg keeps its OWN per-leg substrate CorrelationID (minted by
// Config.CorrelationGen) as the child. The parent↔child relationship is fixed HERE,
// before any durable store or audit record references it — changing it later is a
// migration, not an edit. The durable, expiring store that replaces the in-memory
// exchange map is a planned future drop-in. No per-leg binding fence (gateway.go
// VerifyBound, inbound.go subject fence) is weakened by this grouping — Exchange.ID
// never enters a sealed envelope.
type Exchange struct {
	ID         string
	Workstream string
	Legs       []LegRecord
}
