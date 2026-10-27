package auditappend

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ErrInvalid is a record the guard refuses: it is counted and never stored.
var ErrInvalid = errors.New("auditappend: record refused by the content guard")

// The shapes the guard bounds. The member shapes are the Audit Plane's own
// member rules for the two classes a gateway writes;
// the root module's test holds the guard to them over a shared corpus
// (testdata/guard_corpus.json). The guard is stricter than the Plane where
// the Plane leaves a member unbounded (authorityFrame and scope):
// nothing the gateway stores is unbounded.
var (
	holderShape      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	frameShape       = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
	payloadHashShape = regexp.MustCompile(`^[0-9a-f]{64}$`)
	moduleBuilder    = regexp.MustCompile(`^[a-z0-9-]{1,48}$`)
	moduleEdit       = regexp.MustCompile(`^E-([0-9]{2}|[a-z]{1,16})$`)
	moduleLine       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	moduleLossPath   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.\[\]:_-]{0,127}$`)
	gatewayVersion   = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]{1,64})?$`)
	gatewayWire      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	recordIDShape    = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)
)

const (
	maxModuleEdits = 16
	maxModuleLoss  = 32
	maxWireVersion = 8
	maxModules     = 3
)

// HolderOK reports whether a holder id has the shape the guard allows a
// sender: a holder outside it would have every record refused.
func HolderOK(id string) bool { return holderShape.MatchString(id) }

// Check is the content guard: a record the gateway may store and send. It
// is a version 2 record of a class a gateway writes (module-details or
// gateway-start), sent by holder, whose every member has its class's closed
// shape and bound, and whose members outside its class are empty. A record
// it refuses is never stored (the outbox holds metadata only, never an
// unchecked value), so its error says which rule, never the value.
func Check(r shnsdk.AuditRecord, holder string) error {
	if err := check(r, holder); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func check(r shnsdk.AuditRecord, holder string) error {
	switch {
	case r.Version != shnsdk.AuditRecordVersion2:
		return errors.New("not a version 2 record")
	case holder == "" || r.Sender != holder:
		return errors.New("sender is not this gateway's holder")
	case !holderShape.MatchString(r.Sender) || !holderShape.MatchString(r.Recipient):
		return errors.New("sender or recipient is outside its shape")
	case !recordIDShape.MatchString(r.RecordID):
		return errors.New("recordId is not a ULID")
	case !timestampOK(r.Timestamp):
		return errors.New("timestamp is not RFC 3339 UTC")
	case r.ConsentRef != "" || r.SubjectPCI != "" || r.Involvement != "":
		return errors.New("carries a subject, an involvement or a consent reference")
	case r.Resends != "" || r.Finding != nil:
		return errors.New("carries resends or a finding")
	}
	switch r.TransactionType {
	case shnsdk.AuditTypeModuleDetails:
		return checkModuleRecord(r)
	case shnsdk.AuditTypeGatewayStart:
		return checkStartRecord(r)
	}
	return errors.New("transactionType is not a class a gateway writes")
}

// timestampOK is an RFC 3339 instant in UTC, at most nanosecond precision.
func timestampOK(ts string) bool {
	if len(ts) > len("2006-01-02T15:04:05.999999999Z") || !strings.HasSuffix(ts, "Z") {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, ts)
	return err == nil
}

// framesOK bounds authorityFrame and scope, which the Plane leaves
// unbounded: empty, or printable ASCII without spaces, at most 128 bytes.
func framesOK(r shnsdk.AuditRecord) bool {
	for _, v := range []string{r.AuthorityFrame, r.Scope} {
		if v != "" && !frameShape.MatchString(v) {
			return false
		}
	}
	return true
}

// checkModuleRecord is a module-details record.
func checkModuleRecord(r shnsdk.AuditRecord) error {
	m := r.Module
	switch {
	case m == nil:
		return errors.New("a module-details record needs its module")
	case r.Gateway != nil:
		return errors.New("gateway is only on a gateway-start record")
	case r.Outcome != "edited" && r.Outcome != "authored":
		return errors.New("a module-details outcome is edited or authored")
	case m.Ownership != r.Outcome:
		return errors.New("module.ownership differs from the record's outcome")
	case r.Recipient == r.Sender:
		return errors.New("a module-details recipient is the leg's counterpart holder")
	case !payloadHashShape.MatchString(r.PayloadBundleHash):
		return errors.New("a module-details record needs its leg's payloadBundleHash")
	case r.CorrelationID == "" || shnsdk.BoundedCorrelationID(r.CorrelationID) != r.CorrelationID:
		return errors.New("correlationId is missing or not in its bounded form")
	case !framesOK(r):
		return errors.New("authorityFrame or scope is outside its bound")
	}
	switch m.Body {
	case "request", "forward", "answer":
	default:
		return errors.New("module.body is outside its set")
	}
	if !prefixedLeg(m.Leg) {
		return errors.New("module.leg is no version 2 exchange name")
	}
	if len(m.Edits) > maxModuleEdits {
		return fmt.Errorf("module.edits holds more than %d edits", maxModuleEdits)
	}
	for _, e := range m.Edits {
		if !moduleEdit.MatchString(e) {
			return errors.New("a module.edits entry is outside its shape")
		}
	}
	if m.Builder != "" && !moduleBuilder.MatchString(m.Builder) {
		return errors.New("module.builder is outside its shape")
	}
	keeping := shnsdk.BuilderKeepingEdits()
	switch m.Ownership {
	case "authored":
		if m.Builder == "" {
			return errors.New("an authored body names its builder")
		}
		if len(m.Edits) > 0 {
			return errors.New("an authored body names no edit")
		}
	case "edited":
		if len(m.Edits) == 0 {
			return errors.New("an edited body names at least one edit")
		}
		if m.Builder != "" {
			for _, e := range m.Edits {
				if !slices.Contains(keeping, e) {
					return errors.New("an edited body names a builder only when every edit keeps it")
				}
			}
		}
	}
	return checkModuleLines(m, keeping)
}

// prefixedLeg reports whether leg is a leg's version 2 (prefixed) name, from
// the SDK's name table.
func prefixedLeg(leg string) bool {
	for flat, prefixed := range shnsdk.PrefixedTransactionTypes() {
		if leg == prefixed && prefixed != flat {
			return true
		}
	}
	return false
}

// checkModuleLines is a module's lines and loss.
func checkModuleLines(m *shnsdk.Module, keeping []string) error {
	for _, l := range []string{m.LineFrom, m.LineTo} {
		if l != "" && !moduleLine.MatchString(l) {
			return errors.New("module.lineFrom or lineTo is outside its shape")
		}
	}
	if (m.LineFrom == "") != (m.LineTo == "") {
		return errors.New("module.lineFrom and lineTo are both set or both empty")
	}
	if m.LineFrom != "" && !slices.ContainsFunc(m.Edits, func(e string) bool { return slices.Contains(keeping, e) }) {
		return errors.New("module.lineFrom and lineTo need a line-bridge edit")
	}
	if len(m.Loss) > 0 && m.LineFrom == "" {
		return errors.New("module.loss is only on a body a line bridge translated")
	}
	if len(m.Loss) > maxModuleLoss {
		return fmt.Errorf("module.loss holds more than %d entries", maxModuleLoss)
	}
	for i, l := range m.Loss {
		switch l.Kind {
		case "carried", "synthesized":
			if !moduleLossPath.MatchString(l.Path) {
				return errors.New("a module.loss path is outside its shape")
			}
		case "truncated":
			if l.Path != "" || i != maxModuleLoss-1 {
				return fmt.Errorf("a truncated module.loss entry is the %dth and last, with no path", maxModuleLoss)
			}
		default:
			return errors.New("module.loss kind is outside its set")
		}
	}
	return nil
}

// checkStartRecord is a gateway-start record.
func checkStartRecord(r shnsdk.AuditRecord) error {
	g := r.Gateway
	switch {
	case g == nil:
		return errors.New("a gateway-start record needs its gateway")
	case r.Module != nil:
		return errors.New("module is only on a module-details record")
	case r.Outcome != "started":
		return errors.New("a gateway-start outcome is started")
	case r.Recipient != r.Sender:
		return errors.New("a gateway-start recipient is its sender")
	case r.AuthorityFrame != "" || r.Scope != "" || r.PayloadBundleHash != "" || r.CorrelationID != "":
		return errors.New("a gateway-start record carries no leg member")
	case !gatewayVersion.MatchString(g.Version):
		return errors.New("gateway.version is outside its shape")
	case len(g.WireVersions) > maxWireVersion:
		return fmt.Errorf("gateway.wireVersions holds more than %d versions", maxWireVersion)
	case len(g.Modules) > maxModules:
		return fmt.Errorf("gateway.modules holds more than %d modules", maxModules)
	}
	for _, v := range g.WireVersions {
		if !gatewayWire.MatchString(v) {
			return errors.New("a gateway.wireVersions entry is outside its shape")
		}
	}
	for i, m := range g.Modules {
		switch m {
		case shnsdk.ModuleOnBehalf, shnsdk.ModuleLineBridge, shnsdk.ModuleConformance:
		default:
			return errors.New("a gateway.modules entry is outside its set")
		}
		if i > 0 && g.Modules[i-1] >= m {
			return errors.New("gateway.modules is sorted, each module once")
		}
	}
	return nil
}
