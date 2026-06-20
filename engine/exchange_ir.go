// exchange_ir.go — the Layer-2 Canonical Exchange IR: workstream-NEUTRAL
// vocabulary the gateway moves and guards but never clinically interprets. Zero Da
// Vinci, zero provider/payer. A workstream module (Layer 3, e.g. workstream_pa.go)
// supplies the concrete legTypes and content shapes; this file knows only their
// neutral envelope. The wire stays sealed Da Vinci bytes (AI-2 / OWD-G3): the IR is
// gateway-INTERNAL, never a neutral wire format.
package engine

// Content is a typed-opaque handle on a workstream payload. The IR validates it at
// the edge (FR-36/FR-G29) and binds its subject for authority, but never reads its
// clinical semantics. WorkstreamType identifies the owning module (read live by
// OriginateLeg's selection-seam guard). ProfileID is a RESERVED routing hint —
// validation is meta.profile-driven (every call site leaves ProfileID unset), so
// ProfileID is never the validation key; it is kept as the committed Content shape
// for future per-call profile pinning (PH-G3) and has no current reader (a reserved
// seam, not dead code). Bytes is the FHIR/payload bytes that reach the wire.
type Content struct {
	WorkstreamType string
	ProfileID      string
	Bytes          []byte
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
