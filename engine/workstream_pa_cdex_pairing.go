//go:build !shn_coreonly

// workstream_pa_cdex_pairing.go — how a CDex attachment submission pairs with
// the attachment request it answers.
//
// A payer integrated directly matches an attachment to the request it sent, by
// the tracking identifier the request carries and the submission repeats
// (CDex 2.1.0: Task.identifier typed cdex-temp#tracking-id, and the
// $submit-attachment TrackingId). Each side's gateway does the same for the
// exchange it carried: when it carries a request it records the tracking
// identifier, scoped to the other participant, with the member and the patient
// it bound the request to; a submission from that participant naming the same
// tracking identifier and member, for a member this side's system of record does
// not hold, binds to that patient, never re-derived from its own bytes. That is
// what keeps the two legs on one patient: the request binds from its own
// subject Patient, the submission (which carries none, and whose attachment
// content is the participant's document, never read for binding) by its record.
//
// An unpaired submission is never refused: it binds by the rule for a leg with
// no subject Patient, over its top-level resources alone, as the network bound
// every submission before, and the gateway raises CDexRequestUnpairedEvent.
// The record holds opaque values only (the tracking identifier, the other
// participant, the member and the patient identifier), never content.
package engine

import (
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextask"
)

// CDexRequestUnpairedEvent is the observer event a gateway raises when it
// binds an attachment submission for a member its system of record does not
// hold and finds no record of the request it answers that it can pair it with.
// Detail is the reason, one of CDexUnpairedReasons. The submission is carried
// as usual, bound as the network binds a leg with no subject Patient; the event
// carries no identifier. A submission that answers a pended prior
// authorization rather than an attachment request (its TrackingId is the
// authorization's identifier) raises it too: no request was carried for it.
const CDexRequestUnpairedEvent = "cdex.request-unpaired"

// The reasons CDexRequestUnpairedEvent carries.
const (
	// CDexUnpairedNoTrackingID: the submission carries no TrackingId the
	// gateway can read, or two that differ.
	CDexUnpairedNoTrackingID = "no-tracking-id"
	// CDexUnpairedNoRecord: this gateway carried no request with that
	// tracking identifier, or its record is past retention, or was lost to a
	// restart of an in-memory store.
	CDexUnpairedNoRecord = "no-record"
	// CDexUnpairedCounterpartyDiffers: the request with that tracking
	// identifier was carried with another participant.
	CDexUnpairedCounterpartyDiffers = "counterparty-differs"
	// CDexUnpairedMemberDiffers: the request was for another member.
	CDexUnpairedMemberDiffers = "member-differs"
	// CDexUnpairedMemberConflict: two requests with that tracking identifier
	// were carried with that participant for different patients, so it names
	// no one request.
	CDexUnpairedMemberConflict = "member-conflict"
	// CDexUnpairedStoreError: the record could not be read.
	CDexUnpairedStoreError = "store-error"
)

// CDexUnpairedReasons is the closed list of CDexRequestUnpairedEvent's reasons.
var CDexUnpairedReasons = []string{
	CDexUnpairedNoTrackingID, CDexUnpairedNoRecord, CDexUnpairedCounterpartyDiffers,
	CDexUnpairedMemberDiffers, CDexUnpairedMemberConflict, CDexUnpairedStoreError,
}

// CDexRequestRetention is how long a request's record survives its last
// change: the pend ledger's retention.
const CDexRequestRetention = PendLedgerRetention

// MaxCDexTrackingBytes bounds a tracking identifier's system and value
// together. A longer one is neither recorded nor looked up (its submission is
// unpaired, no-record): the key is part of the record's primary key, and a
// multi-kilobyte value would overflow its index on a healthy database.
const MaxCDexTrackingBytes = 512

// CDexRequestKey names one attachment request a gateway carried, as the
// submission that answers it names it: the other participant of the exchange
// and the request's tracking identifier.
type CDexRequestKey struct {
	Counterparty  string
	System, Value string
}

// CDexRequestRecord is what a gateway keeps of a request it carried: the member
// and the patient identifier it bound the request to.
type CDexRequestRecord struct {
	Member, PCI string
}

// CDexRequestLookup is what a lookup found.
type CDexRequestLookup uint8

const (
	// CDexRequestNone: no record of the tracking identifier, with any
	// participant, inside retention.
	CDexRequestNone CDexRequestLookup = iota
	// CDexRequestFound: the record for the key.
	CDexRequestFound
	// CDexRequestConflicted: the key was recorded for two different
	// patients; it names no one request.
	CDexRequestConflicted
	// CDexRequestOtherCounterparty: no record for the key, but one of the
	// tracking identifier with another participant.
	CDexRequestOtherCounterparty
)

// CDexRequestStore is the record of the attachment requests a gateway carried.
//
// RecordCDexRequest records k → rec. A key recorded again with the same record
// changes nothing; with a different one it is marked conflicted and never
// overwritten, so a request sent again for another patient can never re-bind a
// later submission to that patient. A record past CDexRequestRetention is gone:
// never found, and recorded afresh. LookupCDexRequest resolves k. The error
// channel is a store failure, so "unavailable" never reads as "none".
type CDexRequestStore interface {
	RecordCDexRequest(k CDexRequestKey, rec CDexRequestRecord) error
	LookupCDexRequest(k CDexRequestKey) (CDexRequestRecord, CDexRequestLookup, error)
}

// ErrCDexRequestKey is a key with no participant or tracking value, or one
// over MaxCDexTrackingBytes. Callers never pass one; the stores refuse it.
var ErrCDexRequestKey = errors.New("cdex request record: the key has no participant or tracking value, or is too long")

// ValidCDexRequestKey reports whether k can be recorded.
func ValidCDexRequestKey(k CDexRequestKey) bool {
	return k.Counterparty != "" && k.Value != "" && len(k.System)+len(k.Value) <= MaxCDexTrackingBytes
}

// cdexRequestRow is one record in the in-memory store.
type cdexRequestRow struct {
	rec        CDexRequestRecord
	conflicted bool
	// changed is when the row was recorded or marked conflicted: retention
	// counts from it.
	changed time.Time
}

// MemCDexRequests is the in-memory CDexRequestStore: the default for a
// deployment with no durable store. What it recorded before a restart is gone,
// so a submission answering such a request is unpaired (no-record).
type MemCDexRequests struct {
	mu   sync.Mutex
	rows map[CDexRequestKey]cdexRequestRow
	// now is the store's own clock (retention). Injected so the retention
	// rows are deterministic; never reassigned outside tests.
	now       func() time.Time
	lastPurge time.Time
}

var _ CDexRequestStore = (*MemCDexRequests)(nil)

// NewMemCDexRequests returns an empty in-memory record.
func NewMemCDexRequests() *MemCDexRequests {
	return &MemCDexRequests{rows: map[CDexRequestKey]cdexRequestRow{}, now: time.Now}
}

// RecordCDexRequest records k → rec. See CDexRequestStore.
func (m *MemCDexRequests) RecordCDexRequest(k CDexRequestKey, rec CDexRequestRecord) error {
	if !ValidCDexRequestKey(k) {
		return ErrCDexRequestKey
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeLocked()
	now := m.now()
	row, ok := m.rows[k]
	switch {
	case !ok || !row.changed.After(now.Add(-CDexRequestRetention)):
		m.rows[k] = cdexRequestRow{rec: rec, changed: now}
	case row.conflicted || row.rec == rec:
	default:
		row.conflicted = true
		row.changed = now
		m.rows[k] = row
	}
	return nil
}

// LookupCDexRequest resolves k. See CDexRequestStore.
func (m *MemCDexRequests) LookupCDexRequest(k CDexRequestKey) (CDexRequestRecord, CDexRequestLookup, error) {
	if !ValidCDexRequestKey(k) {
		return CDexRequestRecord{}, CDexRequestNone, ErrCDexRequestKey
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeLocked()
	cutoff := m.now().Add(-CDexRequestRetention)
	if row, ok := m.rows[k]; ok && row.changed.After(cutoff) {
		if row.conflicted {
			return CDexRequestRecord{}, CDexRequestConflicted, nil
		}
		return row.rec, CDexRequestFound, nil
	}
	for other, row := range m.rows {
		if other.System == k.System && other.Value == k.Value && row.changed.After(cutoff) {
			return CDexRequestRecord{}, CDexRequestOtherCounterparty, nil
		}
	}
	return CDexRequestRecord{}, CDexRequestNone, nil
}

// resetCDexRequests clears the record (the demo reset contract).
func (m *MemCDexRequests) resetCDexRequests() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = map[CDexRequestKey]cdexRequestRow{}
	m.lastPurge = time.Time{}
}

// purgeLocked runs the lazy retention sweep on the pend ledger's throttle and
// bound (DuePendPurge, PendPurgeMax), so the two backends' observable
// retention is one rule.
func (m *MemCDexRequests) purgeLocked() {
	now := m.now()
	due, _ := DuePendPurge(now, m.lastPurge)
	if !due {
		return
	}
	m.lastPurge = now
	cutoff := now.Add(-CDexRequestRetention)
	purged := 0
	for k, row := range m.rows {
		if purged >= PendPurgeMax {
			return
		}
		if row.changed.After(cutoff) {
			continue
		}
		delete(m.rows, k)
		purged++
	}
}

// CDexRequestsOf reports the Store's record of carried attachment requests, if
// it has one: the Store itself when it implements CDexRequestStore (the durable
// store), or the PA workstream's in-memory record when it is, or embeds, an
// in-memory Store. A store that embeds one and declares only part of
// CDexRequestStore is answered with none; New refuses such a store
// (checkStore).
func CDexRequestsOf(s Store) (CDexRequestStore, bool) {
	if c, ok := s.(CDexRequestStore); ok {
		return c, true
	}
	if st, ok := memPAStateOf(s); ok && partialCDexRequestStore(s) == "" {
		return st.cdexRequests, true
	}
	return nil, false
}

// cdexRequests returns this gateway's record: the configured Store's own, and
// otherwise the in-memory default the continuation store also falls back to.
func (g *Gateway) cdexRequests() (CDexRequestStore, bool) {
	if cs, ok := CDexRequestsOf(g.cfg.Store); ok {
		return cs, true
	}
	fallback := g.fallbackStore.Load()
	if fallback == nil {
		fresh := NewMemStore()
		if g.fallbackStore.CompareAndSwap(nil, fresh) {
			fallback = fresh
		} else {
			fallback = g.fallbackStore.Load()
		}
	}
	return CDexRequestsOf(fallback)
}

// cdexTrackingKey is the key of the request task carries, with counterparty,
// and whether it has one: a tracking identifier that is absent, unreadable or
// too long records nothing.
func cdexTrackingKey(task cdextask.Task, counterparty string) (CDexRequestKey, bool) {
	if task.Tracking.State != cdextask.Present {
		return CDexRequestKey{}, false
	}
	id := task.Tracking.Identifier
	k := CDexRequestKey{Counterparty: counterparty, System: id.System, Value: id.Value}
	return k, ValidCDexRequestKey(k)
}

// recordCDexRequest records the request this gateway carried with
// counterparty, bound to member and pci. The request has already been carried:
// a record that cannot be written is logged and counted, never a failure of
// the leg.
func (g *Gateway) recordCDexRequest(task cdextask.Task, counterparty, member, pci string) {
	k, ok := cdexTrackingKey(task, counterparty)
	if !ok {
		return
	}
	store, ok := g.cdexRequests()
	if !ok {
		return
	}
	if err := store.RecordCDexRequest(k, CDexRequestRecord{Member: member, PCI: pci}); err != nil {
		log.Printf("gateway: cdex request record: %v", err)
		g.noteStoreError(storeErrCDexRequest)
	}
}

// cdexSubmissionTopLevel is the submission as its binding reads it: its
// top-level resources (each Parameters.parameter's resource), each as it was
// sent, and nothing of a parameter's parts, which hold the attachment content:
// the participant's document, carried as sent and never read for binding. A
// parameter's resource named in another case is read too: unknown is not
// empty, and the network bound from it in earlier releases.
func cdexSubmissionTopLevel(body []byte) []byte {
	var p map[string]json.RawMessage
	_ = json.Unmarshal(body, &p)
	type param struct {
		Name     string          `json:"name"`
		Resource json.RawMessage `json:"resource"`
	}
	top := struct {
		ResourceType string  `json:"resourceType"`
		Parameter    []param `json:"parameter"`
	}{ResourceType: "Parameters", Parameter: []param{}}
	var params []map[string]json.RawMessage
	for k, v := range p {
		if strings.EqualFold(k, "parameter") {
			var these []map[string]json.RawMessage
			if json.Unmarshal(v, &these) == nil {
				params = append(params, these...)
			}
		}
	}
	for _, x := range params {
		for k, v := range x {
			if strings.EqualFold(k, "resource") && len(v) > 0 && string(v) != "null" {
				top.Parameter = append(top.Parameter, param{Name: "resource", Resource: v})
			}
		}
	}
	b, err := json.Marshal(top)
	if err != nil {
		return nil
	}
	return b
}

// cdexPairing is what this gateway's record says of a submission from
// counterparty: the patient of the attachment request it answers, when this
// gateway carried that request with counterparty, naming the submission's
// tracking identifier, for the same member; otherwise "" and the reason it
// pairs with none (one of CDexUnpairedReasons). It is read before the
// submission is bound: a submission that pairs is bound from the system of
// record, for a member it holds, and otherwise under the request's patient,
// never from its own bytes, so nothing it carries can refuse it. A record that
// cannot be read is counted, and pairs with none.
func (g *Gateway) cdexPairing(sub cdexSubmission, counterparty string) (pci, reason string) {
	if !sub.tracking.ok {
		return "", CDexUnpairedNoTrackingID
	}
	k := CDexRequestKey{Counterparty: counterparty, System: sub.tracking.system, Value: sub.tracking.value}
	store, ok := g.cdexRequests()
	if !ok || !ValidCDexRequestKey(k) {
		return "", CDexUnpairedNoRecord
	}
	rec, lookup, err := store.LookupCDexRequest(k)
	switch {
	case err != nil:
		log.Printf("gateway: cdex request lookup: %v", err)
		g.noteStoreError(storeErrCDexRequest)
		return "", CDexUnpairedStoreError
	case lookup == CDexRequestConflicted:
		return "", CDexUnpairedMemberConflict
	case lookup == CDexRequestOtherCounterparty:
		return "", CDexUnpairedCounterpartyDiffers
	case lookup != CDexRequestFound:
		return "", CDexUnpairedNoRecord
	case rec.Member != sub.member:
		return "", CDexUnpairedMemberDiffers
	}
	return rec.PCI, ""
}

// cdexBindingPayload is what a submission is bound from: nothing of its own
// bytes when it pairs (paired is the request's patient), and otherwise its
// top-level resources (cdexSubmissionTopLevel).
func cdexBindingPayload(body []byte, paired string) []byte {
	if paired != "" {
		return nil
	}
	return cdexSubmissionTopLevel(body)
}

// noteCDexUnpaired raises CDexRequestUnpairedEvent for a submission carried
// unpaired, for a member the system of record does not hold.
func (g *Gateway) noteCDexUnpaired(corrID, reason string) {
	g.observe(ObserverEvent{Kind: CDexRequestUnpairedEvent, Direction: "ingress", LegType: legCDexSubmit, CorrelationID: corrID, Detail: reason})
}
