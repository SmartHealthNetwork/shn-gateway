package telemetry

import (
	"slices"
	"sync"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/auditappend"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/recording"
	metrics "github.com/SmartHealthNetwork/shn-sdk/metrics"
)

// EMF is the CloudWatch Embedded Metric Format sink: it writes each
// measurement as the EMF lines the gateway has always written, byte for byte
// (MON-R17, TestEMFGolden), with the Service and, on a leg, the role
// dimensions it has always carried. An instrument with no EMF line
// (TelemetryExports) writes nothing.
type EMF struct {
	em      *metrics.Emitter
	service string
	role    string

	// active is the exchanges in flight by direction, and the most seen since
	// the last sample (SampleActiveRequests): ActiveRequests writes no line
	// per change, only its peak each sample (MON-R23).
	activeMu   sync.Mutex
	active     map[string]float64
	activePeak map[string]float64
}

// NewEMF returns the EMF sink writing through em, a nil em writing nothing.
func NewEMF(em *metrics.Emitter, service, role string) *EMF {
	return &EMF{em: em, service: service, role: role, active: map[string]float64{}, activePeak: map[string]float64{}}
}

// SampleActiveRequests writes the ActiveRequests gauge {Service, direction}
// for each direction: the most exchanges in flight at once since the last
// sample, a known 0 included. A nil EMF writes nothing.
func (e *EMF) SampleActiveRequests() {
	if e == nil {
		return
	}
	e.activeMu.Lock()
	peaks := make(map[string]float64, len(recording.ExchangeDirections))
	for _, dir := range recording.ExchangeDirections {
		if dir == recording.ExchangeOther {
			continue
		}
		peaks[dir] = e.activePeak[dir]
		e.activePeak[dir] = e.active[dir]
	}
	e.activeMu.Unlock()
	for _, dir := range recording.ExchangeDirections {
		if peak, ok := peaks[dir]; ok {
			e.em.EmitGauge("ActiveRequests", peak, "Count", map[string]string{"Service": e.service,
				"direction": closedDim(recording.ExchangeDirections, dir)})
		}
	}
}

// Record writes m's EMF lines.
func (e *EMF) Record(m Measurement) {
	if write := emfLines[m.Instrument]; write != nil {
		write(e, m)
	}
}

var _ SnapshotSink = (*EMF)(nil)

// RecordSnapshot writes each series' EMF line and then, for an instrument
// with one, a rollup line over the whole snapshot: the single stream an
// alarm watches (CertificateExpiryMin, DependencyAgeMax). A rollup writes
// nothing for a snapshot with nothing it covers, so an unknown value stays
// absent (MON-R15).
func (e *EMF) RecordSnapshot(i *Instrument, series []Measurement) {
	for _, m := range series {
		e.Record(m)
	}
	if roll := emfRollups[i]; roll != nil {
		roll(e, series)
	}
}

// closedDim is v when list names it, else Other. The registry has already
// folded every value; each EMF line folds again against the same list, so
// every dimension it writes visibly comes from a fixed list.
func closedDim(list []string, v string) string {
	if slices.Contains(list, v) {
		return v
	}
	return Other
}

// ms is a duration's whole milliseconds, truncated as the EMF lines always
// were.
func ms(m Measurement) float64 { return float64(m.Duration.Milliseconds()) }

// backendNotAnError are the backend classes that do not count in the
// BackendError rollup: those in which the participant's system gave an
// answer of its own, and a call the request's end cut short, the requester
// leaving before the gateway's deadline for that system (a system slower
// than the deadline is its timeout, which counts). Every other class counts.
var backendNotAnError = map[string]bool{BackendOK: true, engine.BackendHTTP3xx: true, engine.BackendHTTP4xx: true, engine.BackendCancelled: true}

// poolStat is the EMF StorePool stat dimension of each pool state, and
// poolStats those dimensions' values.
var (
	poolStat  = map[string]string{"total": "TotalConns", "acquired": "AcquiredConns", "idle": "IdleConns"}
	poolStats = []string{"TotalConns", "AcquiredConns", "IdleConns"}
)

// emfLines writes each instrument's EMF lines. The three rollups (LegError,
// BackendError, AuditAppendDropped) are single-stream {Env, Service} lines the
// per-service alarms read without metric math.
var emfLines = map[*Instrument]func(*EMF, Measurement){
	Exchanges: func(e *EMF, m Measurement) {
		e.em.EmitCount("Exchange", m.Value, map[string]string{"Service": e.service,
			"direction": closedDim(recording.ExchangeDirections, m.Attributes[0]),
			"exchange":  closedDim(engine.ExchangeKinds, m.Attributes[1]),
			"outcome":   closedDim(recording.ExchangeOutcomes, m.Attributes[2])})
	},
	ExchangeDuration: func(e *EMF, m Measurement) {
		e.em.EmitLatency("ExchangeLatency", ms(m), map[string]string{"Service": e.service,
			"direction": closedDim(recording.ExchangeDirections, m.Attributes[0]),
			"exchange":  closedDim(engine.ExchangeKinds, m.Attributes[1])})
	},
	BackendCalls: func(e *EMF, m Measurement) {
		class := BackendOK
		if m.Attributes[1] != BackendOK {
			class = closedDim(engine.BackendErrorClasses, m.Attributes[1])
		}
		e.em.EmitCount("BackendCall", m.Value, map[string]string{"Service": e.service,
			"exchange": closedDim(engine.ExchangeKinds, m.Attributes[0]), "class": class})
		if !backendNotAnError[class] {
			e.em.EmitCount("BackendError", m.Value, map[string]string{"Service": e.service})
		}
	},
	BackendDuration: func(e *EMF, m Measurement) {
		e.em.EmitLatency("BackendLatency", ms(m), map[string]string{"Service": e.service,
			"exchange": closedDim(engine.ExchangeKinds, m.Attributes[0])})
	},
	Legs: func(e *EMF, m Measurement) {
		outcome := closedDim(engine.LegOutcomes, m.Attributes[0])
		e.em.EmitCount("LegOutcome", m.Value, map[string]string{"Service": e.service, "role": e.role, "outcome": outcome})
		// denied is a policy decision, not an error.
		if outcome == engine.LegOutcomeFailed || outcome == engine.LegOutcomeUnreachable {
			e.em.EmitCount("LegError", m.Value, map[string]string{"Service": e.service})
		}
	},
	StoreErrors: func(e *EMF, m Measurement) {
		e.em.EmitCount("StoreError", m.Value, map[string]string{"Service": e.service,
			"store": closedDim(engine.StoreErrorStores, m.Attributes[0])})
	},
	InvolvedOmitted: func(e *EMF, m Measurement) {
		e.em.EmitCount("InvolvedOmitted", m.Value, map[string]string{"Service": e.service,
			"reason": closedDim(engine.InvolvedOmitReasons, m.Attributes[0])})
	},
	StorePoolConnections: func(e *EMF, m Measurement) {
		e.em.EmitGauge("StorePool", m.Value, "Count", map[string]string{"Service": e.service,
			"stat": closedDim(poolStats, poolStat[m.Attributes[0]])})
	},
	StorePoolEmptyAcquires: func(e *EMF, m Measurement) {
		e.em.EmitGauge("StorePool", m.Value, "Count", map[string]string{"Service": e.service, "stat": "EmptyAcquireCount"})
	},
	AuditAppends: func(e *EMF, m Measurement) {
		e.em.EmitCount("AuditAppendSent", m.Value, map[string]string{"Service": e.service})
	},
	AuditRetries: func(e *EMF, m Measurement) {
		e.em.EmitCount("AuditAppendRetried", m.Value, map[string]string{"Service": e.service,
			"reason": closedDim(auditappend.RetryReasons, m.Attributes[0])})
	},
	AuditDrops: func(e *EMF, m Measurement) {
		e.em.EmitCount("AuditAppendDroppedBy", m.Value, map[string]string{"Service": e.service,
			"reason": closedDim(auditappend.DropReasons, m.Attributes[0])})
		e.em.EmitCount("AuditAppendDropped", m.Value, map[string]string{"Service": e.service})
	},
	AuditQueueDepth: func(e *EMF, m Measurement) {
		e.em.EmitGauge("AuditQueueDepth", m.Value, "Count", map[string]string{"Service": e.service})
	},
	AuditEndpointUnknown: func(e *EMF, m Measurement) {
		e.em.EmitGauge("AuditEndpointUnknown", m.Value, "Count", map[string]string{"Service": e.service})
	},
	ValidatorLaneQualified: func(e *EMF, m Measurement) {
		e.em.EmitGauge("ValidatorLane", m.Value, "Count", map[string]string{"Service": e.service,
			"line": closedDim(IGLines, m.Attributes[0]), "lane": closedDim(LaneKinds, m.Attributes[1])})
	},
	Refusals: func(e *EMF, m Measurement) {
		// EMF leaves out the exchange: CloudWatch bills each dimension
		// combination, and with it the metric's bound is ten times larger.
		// OpenTelemetry keeps it.
		rule := closedDim(recording.RefusalRules, m.Attributes[2])
		e.em.EmitCount("ExchangeRefused", m.Value, map[string]string{"Service": e.service,
			"direction": closedDim(recording.ExchangeDirections, m.Attributes[0]),
			"rule":      rule,
			"by":        closedDim(recording.RefusalParties, m.Attributes[3])})
		// The alarm's own metric on {Env, Service, rule} (MON-R34): a single
		// stream per rule, read without metric math.
		e.em.EmitCount("ExchangeRefusedByRule", m.Value, map[string]string{"Service": e.service, "rule": rule})
	},
	ExchangeStageDuration: func(e *EMF, m Measurement) {
		e.em.EmitLatency("ExchangeStageLatency", ms(m), map[string]string{"Service": e.service,
			"direction": closedDim(recording.ExchangeDirections, m.Attributes[0]),
			"exchange":  closedDim(engine.ExchangeKinds, m.Attributes[1]),
			"stage":     closedDim(Stages, m.Attributes[2])})
	},
	ActiveRequests: func(e *EMF, m Measurement) {
		dir := closedDim(recording.ExchangeDirections, m.Attributes[0])
		e.activeMu.Lock()
		defer e.activeMu.Unlock()
		e.active[dir] += m.Value
		if e.active[dir] > e.activePeak[dir] {
			e.activePeak[dir] = e.active[dir]
		}
	},
	ExchangeRequestSize: func(e *EMF, m Measurement) {
		e.em.EmitGauge("RequestBytes", m.Value, "Bytes", map[string]string{"Service": e.service,
			"direction": closedDim(recording.ExchangeDirections, m.Attributes[0]),
			"exchange":  closedDim(engine.ExchangeKinds, m.Attributes[1])})
	},
	ExchangeResponseSize: func(e *EMF, m Measurement) {
		e.em.EmitGauge("ResponseBytes", m.Value, "Bytes", map[string]string{"Service": e.service,
			"direction": closedDim(recording.ExchangeDirections, m.Attributes[0]),
			"exchange":  closedDim(engine.ExchangeKinds, m.Attributes[1])})
	},
	ConformanceFindings: func(e *EMF, m Measurement) {
		// EMF leaves out the exchange, as ExchangeRefused does: CloudWatch
		// bills each dimension combination, and with it the metric's bound is
		// ten times larger. OpenTelemetry keeps it.
		e.em.EmitCount("ConformanceFinding", m.Value, map[string]string{"Service": e.service,
			"level":     closedDim(engine.ConformanceLevels, m.Attributes[1]),
			"kind":      closedDim(engine.CheckKinds, m.Attributes[2]),
			"action":    closedDim(engine.FindingActions, m.Attributes[3]),
			"tolerated": closedDim(Booleans, m.Attributes[4])})
	},
	ValidatorDuration: func(e *EMF, m Measurement) {
		e.em.EmitLatency("ValidatorLatency", ms(m), map[string]string{"Service": e.service,
			"line": closedDim(IGLines, m.Attributes[0]), "lane": closedDim(LaneKinds, m.Attributes[1])})
	},
	ConformanceChecksDropped: func(e *EMF, m Measurement) {
		e.em.EmitCount("ObserveCheckDropped", m.Value, map[string]string{"Service": e.service})
	},
	ValidatorLaneAttempts: func(e *EMF, m Measurement) {
		e.em.EmitCount("ValidatorLaneAttempt", m.Value, map[string]string{"Service": e.service,
			"line": closedDim(IGLines, m.Attributes[0]), "lane": closedDim(LaneKinds, m.Attributes[1]),
			"outcome": closedDim(LaneAttemptOutcomes, m.Attributes[2])})
	},
	GatewayInfo: func(e *EMF, m Measurement) {
		e.em.EmitGauge("GatewayInfo", m.Value, "Count", map[string]string{"Service": e.service,
			"wireProtocol": closedDim(WireProtocols, m.Attributes[0]), "level": closedDim(ConformanceLevels, m.Attributes[1]),
			"line": closedDim(IGLines, m.Attributes[2])})
	},
	CertificateExpiry: func(e *EMF, m Measurement) {
		e.em.EmitGauge("CertificateExpiry", m.Value, "Seconds", map[string]string{"Service": e.service,
			"kind": closedDim(EndpointKinds, m.Attributes[0]), "role": closedDim(CertRoles, m.Attributes[1]),
			"line": closedDim(CertIGLines, m.Attributes[2])})
	},
	IngressKeyRemaining: func(e *EMF, m Measurement) {
		e.em.EmitGauge("IngressKeyRemaining", m.Value, "Seconds", map[string]string{"Service": e.service})
	},
	IngressKeyAge: func(e *EMF, m Measurement) {
		e.em.EmitGauge("IngressKeyAge", m.Value, "Seconds", map[string]string{"Service": e.service})
	},
	DependencyAge: func(e *EMF, m Measurement) {
		e.em.EmitGauge("DependencyAge", m.Value, "Seconds", map[string]string{"Service": e.service,
			"dependency": closedDim(Dependencies, m.Attributes[0])})
	},
	PeersKnown: func(e *EMF, m Measurement) {
		e.em.EmitGauge("PeersKnown", m.Value, "Count", map[string]string{"Service": e.service})
	},
	TokenLatency: func(e *EMF, m Measurement) {
		e.em.EmitLatency("TokenLatency", ms(m), map[string]string{"Service": e.service})
	},
	TokenAcquisition: func(e *EMF, m Measurement) {
		e.em.EmitCount("TokenAcquisition", m.Value, map[string]string{"Service": e.service,
			"outcome": closedDim(engine.TokenOutcomes, m.Attributes[0])})
	},
}

// emfRollups are the snapshot rollup lines (MON-R34): one line per sample
// over the whole snapshot, on the single stream an alarm watches with the
// Minimum or Maximum statistic.
var emfRollups = map[*Instrument]func(*EMF, []Measurement){
	// CertificateExpiryMin is the soonest expiry of the server certificates
	// the gateway's endpoints presented and of its own certificates. A
	// trust-root is left out: a bundle can carry roots nothing uses, so it is
	// on the dashboard rather than the alarm. No line when neither role has a
	// reading.
	CertificateExpiry: func(e *EMF, series []Measurement) {
		var soonest float64
		found := false
		for _, m := range series {
			if r := m.Attributes[1]; r != endpointcert.Server.String() && r != endpointcert.Self.String() {
				continue
			}
			if !found || m.Value < soonest {
				soonest, found = m.Value, true
			}
		}
		if found {
			e.em.EmitGauge("CertificateExpiryMin", soonest, "Seconds", map[string]string{"Service": e.service})
		}
	},
	// DependencyAgeMax is each dependency's age on its own stream.
	DependencyAge: func(e *EMF, series []Measurement) {
		for _, m := range series {
			e.em.EmitGauge("DependencyAgeMax", m.Value, "Seconds", map[string]string{"Service": e.service,
				"dependency": closedDim(Dependencies, m.Attributes[0])})
		}
	},
}
