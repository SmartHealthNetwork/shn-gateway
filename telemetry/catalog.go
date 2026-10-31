package telemetry

import (
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/auditappend"
	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// The closed lists this package owns. The others are their owners': the
// engine's exchange vocabulary, its leg and token outcomes, store names and
// involved reasons, and the audit outbox's reasons.
// BackendOK is the backend.class of a usable answer from the participant's
// own system; every other class is an engine error class.
const BackendOK = "ok"

var (
	// BackendClasses are BackendOK and the engine's error classes.
	BackendClasses = func() []string {
		out := []string{BackendOK}
		for _, c := range engine.BackendErrorClasses {
			out = append(out, c)
		}
		return out
	}()
	// PoolStates are the shared-state pool's connection states.
	PoolStates = []string{"total", "acquired", "idle"}
	// IGLines are the IG lines a validator lane serves.
	IGLines = []string{"2.0", "2.1", "2.2"}
	// LaneKinds are a validator lane's kinds: routing, the default lane
	// routing admits; certification, the client module 3 certifies through.
	LaneKinds = []string{"routing", "certification"}
	// LaneAttemptOutcomes are a default lane qualification attempt's outcomes.
	LaneAttemptOutcomes = []string{"ready", "failed"}
	// ExportOutcomes are an OTLP export's outcomes (MON-R13): ok, accepted;
	// failed, refused or not delivered; dropped, accepted with data points
	// rejected.
	ExportOutcomes = []string{"ok", "failed", "dropped"}
	// WireProtocols are the wire protocol versions a build can speak: every
	// one the SDK speaks (TestClosedListsMatchTheirSources), and the version
	// the gateway sends (TestBuild_GatewayInfoNamesTheWireSent_MON_R27).
	WireProtocols = []string{"1.1.0", "2.0.0"}
	// ConformanceLevels are the conformance enforcement levels, the engine's
	// (TestClosedListsMatchTheirSources).
	ConformanceLevels = []string{"none", "observe", "structural", "strict"}
	// EndpointKinds are the certificate endpoint kinds, endpointcert's, in
	// its order (TestClosedListsMatchTheirSources).
	EndpointKinds = []string{
		"ingress", "metrics-pull", "otlp",
		"fhir", "fhir-token", "payer-davinci", "payer-dtr", "payer-pas", "payer-eligibility", "payer-token", "cds",
		"populate", "populate-token", "crd-fhir-server",
		"discovery", "hub", "hub-key", "authorization", "authorization-key", "registrar", "consent", "audit",
		"validator", "certify",
		"diagnostic", "store",
		"trust-root",
	}
	// CertRoles are the certificate roles, endpointcert's
	// (TestClosedListsMatchTheirSources).
	CertRoles = []string{"self", "server", "trust-root"}
	// CertIGLines are a certificate's IG line values: an IG line for the
	// kinds served per line (validator, certify), else NoIGLine.
	CertIGLines = []string{"2.0", "2.1", "2.2", NoIGLine}
	// NoIGLine is a certificate's IG line value for an endpoint kind that is
	// not served per line (every kind but validator and certify).
	NoIGLine = "none"
	// Dependencies are the dependencies whose freshness is reported: the
	// registrar's holder feed, the one the gateway polls.
	Dependencies = []string{"registrar-feed"}
	// Stages are an exchange's latency stages (MON-R22): an inbound leg's
	// seven measured stages (engine.LegStages) and rest, the remainder of its
	// latency; an ingress call's network wait (engine.ExchangeRecord.Network)
	// and self, the remainder of its latency, this gateway's own time.
	Stages = []string{StageUnwrap, StageReads, StageForward, StageValidate, StageSeal, StageLedger, StageWrite, StageRest, StageNetwork, StageSelf}
	// Booleans are a flag attribute's values.
	Booleans = []string{"true", "false"}
	// RefusalStages are a refusal's shn.refusal.stage values: none for a
	// refusal on any rule but fidelity, else the stage the fidelity refusal
	// was made at (recording.RefusalStages: build, verify, other).
	RefusalStages = func() []string {
		out := []string{RefusalStageNone}
		for _, v := range recording.RefusalStages {
			out = append(out, v)
		}
		return out
	}()
)

// RefusalStageNone is the shn.refusal.stage of a refusal that is not a
// fidelity refusal.
const RefusalStageNone = "none"

// The latency stages (Stages).
const (
	StageUnwrap   = "unwrap"
	StageReads    = "reads"
	StageForward  = "forward"
	StageValidate = "validate"
	StageSeal     = "seal"
	StageLedger   = "ledger"
	StageWrite    = "write"
	StageRest     = "rest"
	StageNetwork  = "network"
	StageSelf     = "self"
)

// The attribute keys. Every key is shn.* and every value list is closed
// (MON-R18, MON-R19): no peer, id, URL or free text is ever an attribute.
const (
	KeyDirection    = "shn.direction"
	KeyExchange     = "shn.exchange"
	KeyOutcome      = "shn.outcome"
	KeyBackendClass = "shn.backend.class"
	KeyStore        = "shn.store"
	KeyReason       = "shn.reason"
	KeyPoolState    = "shn.pool.state"
	KeyIGLine       = "shn.ig_line"
	KeyLane         = "shn.lane"
	KeyWireProtocol = "shn.wire_protocol"
	KeyLevel        = "shn.conformance.level"
	KeyEndpointKind = "shn.endpoint.kind"
	KeyCertRole     = "shn.cert.role"
	KeyDependency   = "shn.dependency"
	KeyRefusalRule  = "shn.refusal.rule"
	KeyRefusalBy    = "shn.refusal.by"
	KeyRefusalStage = "shn.refusal.stage"
	KeyStage        = "shn.stage"
	KeyFindingKind  = "shn.finding.kind"
	KeyAction       = "shn.finding.action"
	KeyTolerated    = "shn.finding.tolerated"
)

func attr(key string, values []string) Attribute { return Attribute{Key: key, Values: values} }

// Each attribute holds its own copy of its closed list, taken by ranging over
// the owner's list: nothing done with an Attribute's values reaches the list
// an EMF line picks its dimension from.
var (
	directionValues = func() (out []string) {
		for _, v := range recording.ExchangeDirections {
			out = append(out, v)
		}
		return out
	}()
	exchangeValues = func() (out []string) {
		for _, v := range engine.ExchangeKinds {
			out = append(out, v)
		}
		return out
	}()
	outcomeValues = func() (out []string) {
		for _, v := range recording.ExchangeOutcomes {
			out = append(out, v)
		}
		return out
	}()
	legOutcomeValues = func() (out []string) {
		for _, v := range engine.LegOutcomes {
			out = append(out, v)
		}
		return out
	}()
	storeValues = func() (out []string) {
		for _, v := range engine.StoreErrorStores {
			out = append(out, v)
		}
		return out
	}()
	involvedValues = func() (out []string) {
		for _, v := range engine.InvolvedOmitReasons {
			out = append(out, v)
		}
		return out
	}()
	retryValues = func() (out []string) {
		for _, v := range auditappend.RetryReasons {
			out = append(out, v)
		}
		return out
	}()
	dropValues = func() (out []string) {
		for _, v := range auditappend.DropReasons {
			out = append(out, v)
		}
		return out
	}()
	poolStateValues = func() (out []string) {
		for _, v := range PoolStates {
			out = append(out, v)
		}
		return out
	}()
	igLineValues = func() (out []string) {
		for _, v := range IGLines {
			out = append(out, v)
		}
		return out
	}()
	laneValues = func() (out []string) {
		for _, v := range LaneKinds {
			out = append(out, v)
		}
		return out
	}()
	laneAttemptValues = func() (out []string) {
		for _, v := range LaneAttemptOutcomes {
			out = append(out, v)
		}
		return out
	}()
	exportOutcomeValues = func() (out []string) {
		for _, v := range ExportOutcomes {
			out = append(out, v)
		}
		return out
	}()
	refusalRuleValues = func() (out []string) {
		for _, v := range recording.RefusalRules {
			out = append(out, v)
		}
		return out
	}()
	refusalByValues = func() (out []string) {
		for _, v := range recording.RefusalParties {
			out = append(out, v)
		}
		return out
	}()
	refusalStageValues = func() (out []string) {
		for _, v := range RefusalStages {
			out = append(out, v)
		}
		return out
	}()
	stageValues = func() (out []string) {
		for _, v := range Stages {
			out = append(out, v)
		}
		return out
	}()
	wireProtocolValues = func() (out []string) {
		for _, v := range WireProtocols {
			out = append(out, v)
		}
		return out
	}()
	levelValues = func() (out []string) {
		for _, v := range ConformanceLevels {
			out = append(out, v)
		}
		return out
	}()
	endpointKindValues = func() (out []string) {
		for _, v := range EndpointKinds {
			out = append(out, v)
		}
		return out
	}()
	findingKindValues = func() (out []string) {
		for _, v := range engine.CheckKinds {
			out = append(out, v)
		}
		return out
	}()
	certRoleValues = func() (out []string) {
		for _, v := range CertRoles {
			out = append(out, v)
		}
		return out
	}()
	actionValues = func() (out []string) {
		for _, v := range engine.FindingActions {
			out = append(out, v)
		}
		return out
	}()
	certIGLineValues = func() (out []string) {
		for _, v := range CertIGLines {
			out = append(out, v)
		}
		return out
	}()
	dependencyValues = func() (out []string) {
		for _, v := range Dependencies {
			out = append(out, v)
		}
		return out
	}()
	tokenOutcomeValues = func() (out []string) {
		for _, v := range engine.TokenOutcomes {
			out = append(out, v)
		}
		return out
	}()
	toleratedValues = func() (out []string) {
		for _, v := range Booleans {
			out = append(out, v)
		}
		return out
	}()
)

// The 18 EMF metrics' instruments (MON-R20): 16 instruments, and three EMF
// rollups (BackendError, LegError, AuditAppendDropped) that are queries over
// them in OpenTelemetry. The EMF sink writes each EMF line exactly as before
// (MON-R17).
var (
	Exchanges = &Instrument{Name: "shn.gateway.exchanges", Kind: Counter, Unit: "{exchange}",
		Description: "Exchanges this gateway answered, both directions.",
		Attributes:  []Attribute{attr(KeyDirection, directionValues), attr(KeyExchange, exchangeValues), attr(KeyOutcome, outcomeValues)}}
	ExchangeDuration = &Instrument{Name: "shn.gateway.exchange.duration", Kind: Histogram, Unit: "s",
		Description: "Time from an exchange's arrival to its answer.",
		Attributes:  []Attribute{attr(KeyDirection, directionValues), attr(KeyExchange, exchangeValues)}, Buckets: DurationBuckets}
	BackendCalls = &Instrument{Name: "shn.gateway.backend.calls", Kind: Counter, Unit: "{call}",
		Description: "Calls to the participant's own system, by class.",
		Attributes:  []Attribute{attr(KeyExchange, exchangeValues), attr(KeyBackendClass, BackendClasses)}}
	BackendDuration = &Instrument{Name: "shn.gateway.backend.duration", Kind: Histogram, Unit: "s",
		Description: "Duration of a call to the participant's own system.",
		Attributes:  []Attribute{attr(KeyExchange, exchangeValues)}, Buckets: DurationBuckets}
	Legs = &Instrument{Name: "shn.gateway.legs", Kind: Counter, Unit: "{leg}",
		Description: "Origination leg events, by outcome.",
		Attributes:  []Attribute{attr(KeyOutcome, legOutcomeValues)}}
	StoreErrors = &Instrument{Name: "shn.gateway.store.errors", Kind: Counter, Unit: "{error}",
		Description: "Shared-state store failures, by store.",
		Attributes:  []Attribute{attr(KeyStore, storeValues)}}
	InvolvedOmitted = &Instrument{Name: "shn.gateway.involved.omitted", Kind: Counter, Unit: "{patient}",
		Description: "Involved patients a leg left out of its list, by reason.",
		Attributes:  []Attribute{attr(KeyReason, involvedValues)}}
	StorePoolConnections = &Instrument{Name: "shn.gateway.store.pool.connections", Kind: Gauge, Unit: "{connection}",
		Description: "Shared-state pool connections, by state.",
		Attributes:  []Attribute{attr(KeyPoolState, poolStateValues)}}
	StorePoolEmptyAcquires = &Instrument{Name: "shn.gateway.store.pool.empty_acquires", Kind: CumulativeCounter, Unit: "{acquire}",
		Description: "Shared-state pool acquires that waited for a connection."}
	AuditAppends = &Instrument{Name: "shn.gateway.audit.appends", Kind: Counter, Unit: "{record}",
		Description: "Audit records the Audit Plane accepted."}
	AuditRetries = &Instrument{Name: "shn.gateway.audit.retries", Kind: Counter, Unit: "{record}",
		Description: "Audit record sends retried, by reason.",
		Attributes:  []Attribute{attr(KeyReason, retryValues)}}
	AuditDrops = &Instrument{Name: "shn.gateway.audit.drops", Kind: Counter, Unit: "{record}",
		Description: "Audit records that left the outbox without the Audit Plane's acceptance, by reason.",
		Attributes:  []Attribute{attr(KeyReason, dropValues)}}
	AuditQueueDepth = &Instrument{Name: "shn.gateway.audit.queue.depth", Kind: Gauge, Unit: "{record}",
		Description: "Audit records queued in the outbox."}
	AuditEndpointUnknown = &Instrument{Name: "shn.gateway.audit.endpoint.unknown", Kind: Gauge, Unit: "{record}",
		Description: "Audit records queued while discovery names no audit endpoint."}
	ValidatorLaneQualified = &Instrument{Name: "shn.gateway.validator.lane.qualified", Kind: Gauge, Unit: "",
		Description: "1 when a validator lane is qualified, else 0.",
		Attributes:  []Attribute{attr(KeyIGLine, igLineValues), attr(KeyLane, laneValues)}}
	ValidatorLaneAttempts = &Instrument{Name: "shn.gateway.validator.lane.attempts", Kind: Counter, Unit: "{attempt}",
		Description: "Default validator lane qualification attempts, by outcome.",
		Attributes:  []Attribute{attr(KeyIGLine, igLineValues), attr(KeyLane, laneValues), attr(KeyOutcome, laneAttemptValues)}}
)

// The operational additions (MON-R25 to R27, R45). The gauges among them are
// Snapshots: each sample replaces the whole set, so a value that stops being
// known is absent rather than its last reading (MON-R15).
var (
	GatewayInfo = &Instrument{Name: "shn.gateway.info", Kind: Snapshot, Unit: "",
		Description: "1 for each IG line this gateway declares, with its wire protocol and conformance level.",
		Attributes:  []Attribute{attr(KeyWireProtocol, wireProtocolValues), attr(KeyLevel, levelValues), attr(KeyIGLine, igLineValues)}}
	// CertificateExpiry is the soonest certificate per endpoint kind and role
	// (MON-R25): server, what an endpoint presented; self, the gateway's own;
	// trust-root, one series for a custom bundle SSL_CERT_FILE or SSL_CERT_DIR
	// names, never the image's own. Its EMF rollup, CertificateExpiryMin,
	// covers the server and self roles only: a bundle can carry roots nothing
	// uses.
	CertificateExpiry = &Instrument{Name: "shn.gateway.certificate.expiry", Kind: Snapshot, Unit: "s",
		Description: "Seconds until the soonest certificate of each endpoint kind and role expires.",
		Attributes:  []Attribute{attr(KeyEndpointKind, endpointKindValues), attr(KeyCertRole, certRoleValues), attr(KeyIGLine, certIGLineValues)}}
	IngressKeyRemaining = &Instrument{Name: "shn.gateway.ingress_key.remaining", Kind: Snapshot, Unit: "s",
		Description: "Seconds until the current ingress signing key's not-after."}
	IngressKeyAge = &Instrument{Name: "shn.gateway.ingress_key.age", Kind: Snapshot, Unit: "s",
		Description: "Age of the newest ingress signing key, in seconds."}
	DependencyAge = &Instrument{Name: "shn.gateway.dependency.age", Kind: Snapshot, Unit: "s",
		Description: "Seconds since a dependency was last read successfully.",
		Attributes:  []Attribute{attr(KeyDependency, dependencyValues)}}
	PeersKnown = &Instrument{Name: "shn.gateway.peers.known", Kind: Snapshot, Unit: "{peer}",
		Description: "Holders the last successful registrar read converged."}
	TokenLatency = &Instrument{Name: "shn.gateway.authorization.token.duration", Kind: Histogram, Unit: "s",
		Description: "Time to acquire an Authorization Framework token, its retry included.", Buckets: DurationBuckets}
	TokenAcquisition = &Instrument{Name: "shn.gateway.authorization.tokens", Kind: Counter, Unit: "{token}",
		Description: "Authorization Framework token acquisitions, by outcome.",
		Attributes:  []Attribute{attr(KeyOutcome, tokenOutcomeValues)}}
)

// TelemetryExports counts OTLP exports by outcome (MON-R13). OpenTelemetry
// only: it has no EMF line.
var TelemetryExports = &Instrument{Name: "shn.gateway.telemetry.exports", Kind: Counter, Unit: "{export}",
	Description: "OTLP metric exports, by outcome.",
	Attributes:  []Attribute{attr(KeyOutcome, exportOutcomeValues)}}

// The exchange additions (MON-R20 to MON-R24): each is also an EMF line,
// written beside the 18 existing ones (MON-R17).
var (
	// Refusals' EMF line, ExchangeRefused, leaves out the exchange
	// dimension, since CloudWatch bills each combination: {Env, Service,
	// direction, rule, by}. Its OpenTelemetry attributes keep it, and the
	// refusal stage (shn.refusal.stage, RefusalStages), which no EMF line
	// carries: ExchangeRefusedByRule{rule=fidelity} counts fidelity refusals
	// at stage verify, and a build-stage refusal counts in ExchangeRefused and
	// here with shn.refusal.stage=build.
	Refusals = &Instrument{Name: "shn.gateway.refusals", Kind: Counter, Unit: "{refusal}",
		Description: "Exchanges refused, by the network rule, the party that refused and, for a fidelity refusal, its stage.",
		Attributes: []Attribute{attr(KeyDirection, directionValues), attr(KeyExchange, exchangeValues), attr(KeyRefusalRule, refusalRuleValues), attr(KeyRefusalBy, refusalByValues),
			attr(KeyRefusalStage, refusalStageValues)}}
	ExchangeStageDuration = &Instrument{Name: "shn.gateway.exchange.stage.duration", Kind: Histogram, Unit: "s",
		Description: "Time an exchange spent in each stage; an exchange's stages sum to its duration.",
		Attributes:  []Attribute{attr(KeyDirection, directionValues), attr(KeyExchange, exchangeValues), attr(KeyStage, stageValues)}, Buckets: DurationBuckets}
	ActiveRequests = &Instrument{Name: "shn.gateway.active_requests", Kind: UpDownCounter, Unit: "{request}",
		Description: "Exchanges in flight. In EMF, the peak each minute.",
		Attributes:  []Attribute{attr(KeyDirection, directionValues)}}
	ExchangeRequestSize = &Instrument{Name: "shn.gateway.exchange.request.size", Kind: Histogram, Unit: "By",
		Description: "Size of an exchange's request body, when it was read to its end.",
		Attributes:  []Attribute{attr(KeyDirection, directionValues), attr(KeyExchange, exchangeValues)}, Buckets: SizeBuckets}
	ExchangeResponseSize = &Instrument{Name: "shn.gateway.exchange.response.size", Kind: Histogram, Unit: "By",
		Description: "Size of the answer body an exchange wrote.",
		Attributes:  []Attribute{attr(KeyDirection, directionValues), attr(KeyExchange, exchangeValues)}, Buckets: SizeBuckets}
	ConformanceFindings = &Instrument{Name: "shn.gateway.conformance.findings", Kind: Counter, Unit: "{finding}",
		Description: "Conformance findings recorded, by level, kind, the decision recorded and whether the peer was tolerated.",
		Attributes: []Attribute{attr(KeyExchange, exchangeValues), attr(KeyLevel, levelValues), attr(KeyFindingKind, findingKindValues),
			attr(KeyAction, actionValues), attr(KeyTolerated, toleratedValues)}}
	ValidatorDuration = &Instrument{Name: "shn.gateway.validator.duration", Kind: Histogram, Unit: "s",
		Description: "Duration of a $validate call, by IG line and lane.",
		Attributes:  []Attribute{attr(KeyIGLine, igLineValues), attr(KeyLane, laneValues)}, Buckets: DurationBuckets}
	ConformanceChecksDropped = &Instrument{Name: "shn.gateway.conformance.checks.dropped", Kind: Counter, Unit: "{check}",
		Description: "Observe checks dropped because the observe queue was full or closed."}
)

// catalog is every instrument, in catalogue order. A new instrument is added
// here and, unless it is OpenTelemetry only, to the EMF sink's lines.
var catalog = []*Instrument{
	Exchanges, ExchangeDuration, BackendCalls, BackendDuration, Legs, StoreErrors, InvolvedOmitted,
	StorePoolConnections, StorePoolEmptyAcquires, AuditAppends, AuditRetries, AuditDrops,
	AuditQueueDepth, AuditEndpointUnknown, ValidatorLaneQualified, ValidatorLaneAttempts,
	GatewayInfo, CertificateExpiry, IngressKeyRemaining, IngressKeyAge, DependencyAge, PeersKnown, TokenLatency, TokenAcquisition,
	TelemetryExports,
	Refusals, ExchangeStageDuration, ActiveRequests, ExchangeRequestSize, ExchangeResponseSize,
	ConformanceFindings, ValidatorDuration, ConformanceChecksDropped,
	HTTPServerRequestDuration,
}

// Instruments returns the catalogue, in order. The Instruments are shared:
// read them, never write them.
func Instruments() []*Instrument {
	return append([]*Instrument(nil), catalog...)
}
