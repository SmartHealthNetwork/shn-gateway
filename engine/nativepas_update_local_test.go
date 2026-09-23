package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// PCV-08: these are explicitly invoked local ledger operations. A native
// PAS update does not call them; its peer answer is tested independently.
func TestExplicitPASUpdateLedger_RePendKeysAndCleanup(t *testing.T) {
	store := NewMemStore()
	store.now = func() time.Time { return payerCreated.Add(time.Hour) }
	const pci, corr = "PCI-LOCAL", "submit-local"
	old := PendKeys{RequesterHolder: relayRequester, RequestIDs: []string{"urn:shn:corr|submit-local"}}
	if _, err := store.RecordPendedKeyed(pci, corr, payerCreated.Add(-time.Hour), old); err != nil {
		t.Fatal(err)
	}
	assertLookup := func(holder string, keys PendKeys, foundWant bool) {
		t.Helper()
		subject, gotCorr, found, ambiguous, err := store.LookupPended(holder, keys)
		if err != nil || found != foundWant || ambiguous || (found && (subject != pci || gotCorr != corr)) {
			t.Fatalf("lookup holder=%s: subject=%s corr=%s found=%v ambiguous=%v err=%v", holder, subject, gotCorr, found, ambiguous, err)
		}
	}
	assertLookup(relayRequester, old, true)
	assertLookup("holder:foreign", old, false)
	if ok, why, err := store.BeginClaimUpdateReason("PCI-OTHER", corr); err != nil || ok || why != PendRefusalNotPended {
		t.Fatalf("foreign subject begin: ok=%v why=%q err=%v", ok, why, err)
	}
	if ok, why, err := store.BeginClaimUpdateReason(pci, corr); err != nil || !ok || why != PendRefusalNone {
		t.Fatalf("begin: ok=%v why=%q err=%v", ok, why, err)
	}
	if rec := ledgerState(t, store, pci, corr); rec.State != PendStateInProgress || rec.RequesterHolder != relayRequester {
		t.Fatalf("not bound to requester/in progress: %+v", rec)
	}
	if ok, why, err := store.BeginClaimUpdateReason(pci, corr); err != nil || ok || why != PendRefusalInProgress {
		t.Fatalf("duplicate begin: ok=%v why=%q err=%v", ok, why, err)
	}
	// Acquired work that fails locally must release; the next amendment can bind.
	if err := store.ReleaseClaimUpdate(pci, corr); err != nil {
		t.Fatal(err)
	}
	if rec := ledgerState(t, store, pci, corr); rec.State != PendStatePended {
		t.Fatalf("release did not clean up: %+v", rec)
	}
	if ok, _, err := store.BeginClaimUpdateReason(pci, corr); err != nil || !ok {
		t.Fatalf("later begin after cleanup: ok=%v err=%v", ok, err)
	}
	newer := PendKeys{RequesterHolder: relayRequester, ItemTraceNumbers: []string{"urn:payer:trace|trace-rp"}}
	transition, err := store.RecordPendedKeyed(pci, corr, payerCreated, newer)
	if err != nil || transition.Keys != 2 {
		t.Fatalf("re-pend union: transition=%+v err=%v", transition, err)
	}
	if rec := ledgerState(t, store, pci, corr); rec.State != PendStatePended || rec.RequesterHolder != relayRequester {
		t.Fatalf("re-pend did not return to bound pended state: %+v", rec)
	}
	assertLookup(relayRequester, old, true)
	assertLookup(relayRequester, newer, true)
	assertLookup("holder:foreign", newer, false)
	if _, err := store.RecordPendedKeyed(pci, corr, payerCreated.Add(time.Minute), PendKeys{RequesterHolder: "holder:foreign", ItemTraceNumbers: []string{"urn:payer:trace|stolen"}}); !errors.Is(err, ErrPendRequesterMismatch) {
		t.Fatalf("foreign requester re-pend: %v", err)
	}
	if ok, why, err := store.BeginClaimUpdateReason(pci, corr); err != nil || !ok || why != PendRefusalNone {
		t.Fatalf("later amendment cannot bind: ok=%v why=%q err=%v", ok, why, err)
	}
	if err := store.ReleaseClaimUpdate(pci, corr); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitPASUpdateLedger_DecisionsAndRefusals(t *testing.T) {
	a1, err := shnsdk.BuildClaimResponse("AUTH-A1-1", "2030-01-01", "Patient/MBR-OX", "corr-a1", fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	approved, err := shnsdk.ParseClaimResponse(a1)
	if err != nil || approved.Outcome != "approved" || approved.ReviewAction == nil || approved.ReviewAction.Code.Code != "A1" {
		t.Fatalf("supplied A1 is not an explicit approval: parsed=%+v err=%v", approved, err)
	}
	deniedBody := loadDeniedClaimResponseBytes(t)
	denied, err := shnsdk.ParseClaimResponse(deniedBody)
	if err != nil || denied.Outcome != "denied" {
		t.Fatalf("supplied denial not parsed: parsed=%+v err=%v", denied, err)
	}
	for _, row := range []struct {
		name, outcome string
		body          []byte
	}{{"approved A1", approved.Outcome, a1}, {"denied", denied.Outcome, deniedBody}} {
		t.Run(row.name, func(t *testing.T) {
			var payerReply struct {
				Created string `json:"created"`
			}
			if err := json.Unmarshal(row.body, &payerReply); err != nil {
				t.Fatal(err)
			}
			decisionAt, err := time.Parse(time.RFC3339, payerReply.Created)
			if err != nil || !decisionAt.Equal(fixedClock()) {
				t.Fatalf("producer created=%q, parsed=%s err=%v", payerReply.Created, decisionAt, err)
			}
			store := NewMemStore()
			store.now = func() time.Time { return decisionAt.Add(time.Hour) }
			const pci, corr = "PCI-DECISION", "submit-decision"
			if _, err := store.RecordPendedKeyed(pci, corr, decisionAt.Add(-time.Hour), PendKeys{RequesterHolder: relayRequester, RequestIDs: []string{"urn:shn:corr|submit-decision"}}); err != nil {
				t.Fatal(err)
			}
			if ok, _, err := store.BeginClaimUpdateReason(pci, corr); err != nil || !ok {
				t.Fatalf("begin: %v %v", ok, err)
			}
			if _, err := store.RecordDecision(pci, corr, row.outcome, decisionAt, nil); err != nil {
				t.Fatal(err)
			}
			rec := ledgerState(t, store, pci, corr)
			if rec.State != PendStateDecided || rec.Outcome != row.outcome || !rec.DecidedAt.Equal(decisionAt) || rec.RequesterHolder != relayRequester {
				t.Fatalf("decision not recorded truthfully: %+v", rec)
			}
			if ok, why, err := store.BeginClaimUpdateReason(pci, corr); err != nil || ok || why != PendRefusalDecided {
				t.Fatalf("decided begin: ok=%v why=%q err=%v", ok, why, err)
			}
			if err := store.ReleaseClaimUpdate(pci, corr); err != nil {
				t.Fatal(err)
			}
			if after := ledgerState(t, store, pci, corr); after != rec {
				t.Fatalf("cleanup reverted decided row: before=%+v after=%+v", rec, after)
			}
		})
	}
}

func TestExplicitPASUpdateLedger_FinalizeAndMissing(t *testing.T) {
	store := NewMemStore()
	const pci, corr = "PCI-FINAL", "submit-final"
	if ok, why, err := store.BeginClaimUpdateReason(pci, corr); err != nil || ok || why != PendRefusalNotPended {
		t.Fatalf("missing: ok=%v why=%q err=%v", ok, why, err)
	}
	if err := store.RecordPendedClaim(pci, corr); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := store.BeginClaimUpdateReason(pci, corr); err != nil || !ok {
		t.Fatalf("begin: ok=%v err=%v", ok, err)
	}
	if err := store.FinalizeClaimUpdate(pci, corr); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.PendRecordOf(pci, corr); err != nil || found {
		t.Fatalf("finalize left a row: found=%v err=%v", found, err)
	}
	if ok, why, err := store.BeginClaimUpdateReason(pci, corr); err != nil || ok || why != PendRefusalNotPended {
		t.Fatalf("finalized begin: ok=%v why=%q err=%v", ok, why, err)
	}
}
