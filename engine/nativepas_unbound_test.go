package engine

// nativepas_unbound_test.go — the pend ledger records an amendment and never
// decides whether the payer sees it. Whatever this gateway's ledger holds for
// the authorization — no pend, a decision, another amendment still with the
// payer, or a ledger it cannot read — the amendment reaches the payer's own
// system, the payer's answer is relayed exactly, and the ledger records that
// answer by its own rules. Only an amendment that bound the pend owns the row.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// beginFailsStore is a ledger whose amendment check cannot be read.
type beginFailsStore struct{ *censusSoR }

func (beginFailsStore) BeginClaimUpdateReason(string, string) (bool, PendRefusal, error) {
	return false, PendRefusalNone, errors.New("ledger unavailable")
}

func (beginFailsStore) BeginClaimUpdate(string, string) (bool, error) {
	return false, errors.New("ledger unavailable")
}

func TestNativeUpdate_ReachesThePayerWhateverTheLedgerHolds(t *testing.T) {
	const origCorr = "convergence-pas-submit-0001"
	const pci = "PCI-CONF-UPD"
	approved := fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`), true)
	raw, err := os.ReadFile(filepath.Join("testdata", "br-payer", "pas-update-response-amend-after-resolution.json"))
	if err != nil {
		t.Fatal(err)
	}
	rePend := fixturePASResponse(t, raw, true)

	type ledgerState struct {
		name   string
		reason PendRefusal
		store  func(t *testing.T) (Store, *censusSoR)
	}
	plain := func(seed func(*censusSoR)) func(t *testing.T) (Store, *censusSoR) {
		return func(t *testing.T) (Store, *censusSoR) {
			s := newCensusSoR()
			seed(s)
			return s, s
		}
	}
	states := []ledgerState{
		{"no pend here", PendRefusalNotPended, plain(func(*censusSoR) {})},
		{"already decided", PendRefusalDecided, plain(func(s *censusSoR) {
			if _, err := s.RecordDecision(pci, origCorr, PendOutcomeDenied, time.Unix(1000, 0).UTC(), nil); err != nil {
				t.Fatal(err)
			}
		})},
		{"another amendment in progress", PendRefusalInProgress, plain(func(s *censusSoR) {
			_ = s.RecordPendedClaim(pci, origCorr)
			if ok, _ := s.BeginClaimUpdate(pci, origCorr); !ok {
				t.Fatal("seed begin failed")
			}
		})},
		{"ledger unreadable", pendRefusalLedgerUnavailable, func(t *testing.T) (Store, *censusSoR) {
			s := newCensusSoR()
			_ = s.RecordPendedClaim(pci, origCorr)
			return beginFailsStore{s}, s
		}},
	}
	for _, st := range states {
		for _, ans := range []struct {
			name string
			body []byte
		}{{"re-pend", rePend}, {"approval", approved}} {
			t.Run(st.name+"/"+ans.name, func(t *testing.T) {
				var posts atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					posts.Add(1)
					w.Header().Set("Content-Type", "application/fhir+json")
					_, _ = w.Write(ans.body)
				}))
				defer srv.Close()
				store, mem := st.store(t)
				before, hadRow, _ := mem.PendRecordOf(pci, origCorr)
				n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", store, fixedClock)
				ctx, leg := withPASLeg(context.Background(), "provider")
				res, err := n.Handle(ctx, "pas-claim-update", "corr-1", pci, originatorBuiltConformantUpdateBundle(t))
				if err != nil || res.Status != 0 {
					t.Fatalf("the amendment must reach the payer and its answer be relayed: err=%v status=%d msg=%s", err, res.Status, res.Message)
				}
				if posts.Load() != 1 {
					t.Fatalf("payer received %d posts, want exactly 1", posts.Load())
				}
				if !bytes.Equal(responseBytes(res), ans.body) || !res.ResponseRelayed() {
					t.Fatal("the payer's answer must reach the requester unchanged")
				}
				if res.Rollback != nil {
					t.Fatal("an amendment that did not bind the pend owns no row to release")
				}
				if !hasNote(leg.notes, PendAmendmentUnboundEvent+":"+string(st.reason)) {
					t.Fatalf("notes %v do not say why the amendment did not bind (%s)", leg.notes, st.reason)
				}
				if !hadRow {
					// An unbound amendment never creates a row.
					if res.Commit != nil {
						t.Fatal("an amendment with no pend here must record nothing")
					}
					if _, found, _ := mem.PendRecordOf(pci, origCorr); found {
						t.Fatal("a row was created")
					}
					return
				}
				if res.Commit == nil {
					t.Fatal("the payer's answer must be recorded onto the authorization this gateway holds")
				}
				if err := res.Commit(); err != nil {
					t.Fatalf("commit: %v", err)
				}
				after, _, _ := mem.PendRecordOf(pci, origCorr)
				switch {
				case ans.name == "approval":
					if after.State != PendStateDecided {
						t.Fatalf("the payer's decision was not recorded: %+v", after)
					}
				case st.reason == PendRefusalInProgress:
					if after.State != PendStateInProgress {
						t.Fatalf("a re-pend reopened the amendment in progress: %+v", after)
					}
				case before.State == PendStateDecided:
					// The payer dated this re-pend after the recorded decision, so
					// its later word supersedes it (PendRePend).
					if after.State != PendStatePended {
						t.Fatalf("the payer's later re-pend did not supersede the decision: %+v", after)
					}
				}
			})
		}
	}
}

func hasNote(notes []string, kind string) bool {
	for _, n := range notes {
		if n == kind {
			return true
		}
	}
	return false
}

// An update that names no prior claim still reaches the payer, which decides what
// it means; this gateway has no key to record anything under, so it writes
// nothing.
func TestNativeUpdate_NoPriorClaimNamedRecordsNothing(t *testing.T) {
	approved := fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`), true)
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write(approved)
	}))
	defer srv.Close()
	bundle := mutateBundleEntries(t, originatorBuiltConformantUpdateBundle(t), func(res map[string]any) {
		if res["resourceType"] == "Claim" {
			delete(res, "related")
		}
	})
	s := newCensusSoR()
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
	ctx, leg := withPASLeg(context.Background(), "provider")
	res, err := n.Handle(ctx, "pas-claim-update", "corr-1", "PCI-CONF-UPD", bundle)
	if err != nil || res.Status != 0 || posts.Load() != 1 || !bytes.Equal(responseBytes(res), approved) {
		t.Fatalf("the update must reach the payer and its answer be relayed: err=%v status=%d posts=%d", err, res.Status, posts.Load())
	}
	if res.Commit != nil || res.Rollback != nil {
		t.Fatal("with no prior claim named there is nothing to record or release")
	}
	if !hasNote(leg.notes, PendAmendmentUnboundEvent+":"+string(pendRefusalNoPriorClaim)) {
		t.Fatalf("notes %v", leg.notes)
	}
	if _, found, _ := s.PendRecordOf("PCI-CONF-UPD", ""); found {
		t.Fatal("a record was written under an empty key")
	}
}

// An unbound amendment records nothing onto an authorization another requester
// holds, and never creates one for another patient under a correlation that
// already names an authorization: the correlation keeps naming exactly one.
func TestNativeUpdate_UnboundAmendmentRecordsOnlyOntoItsOwnAuthorization(t *testing.T) {
	const origCorr = "convergence-pas-submit-0001"
	approved := fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`), true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write(approved)
	}))
	defer srv.Close()
	t.Run("another requester's authorization", func(t *testing.T) {
		s := newCensusSoR()
		if _, err := s.RecordDecision("PCI-CONF-UPD", origCorr, PendOutcomeDenied, time.Unix(1000, 0).UTC(), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordPendedKeyed("PCI-CONF-UPD", origCorr, time.Unix(2000, 0).UTC(), PendKeys{RequesterHolder: "provider-a", PreAuthRef: "PA-1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordDecision("PCI-CONF-UPD", origCorr, PendOutcomeDenied, time.Unix(3000, 0).UTC(), nil); err != nil {
			t.Fatal(err)
		}
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		ctx, _ := withPASLeg(context.Background(), "provider-b")
		res, err := n.Handle(ctx, "pas-claim-update", "corr-1", "PCI-CONF-UPD", originatorBuiltConformantUpdateBundle(t))
		if err != nil || res.Status != 0 || !bytes.Equal(responseBytes(res), approved) {
			t.Fatalf("the amendment must still reach the payer: %v %d", err, res.Status)
		}
		if res.Commit != nil {
			t.Fatal("another requester's authorization must not be written")
		}
	})
	t.Run("another requester's pended authorization is not bound", func(t *testing.T) {
		s := newCensusSoR()
		if _, err := s.RecordPendedKeyed("PCI-CONF-UPD", origCorr, time.Unix(2000, 0).UTC(), PendKeys{RequesterHolder: "provider-a", PreAuthRef: "PA-1"}); err != nil {
			t.Fatal(err)
		}
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		ctx, leg := withPASLeg(context.Background(), "provider-b")
		res, err := n.Handle(ctx, "pas-claim-update", "corr-1", "PCI-CONF-UPD", originatorBuiltConformantUpdateBundle(t))
		if err != nil || res.Status != 0 || !bytes.Equal(responseBytes(res), approved) {
			t.Fatalf("the amendment must still reach the payer: %v %d", err, res.Status)
		}
		if res.Commit != nil || res.Rollback != nil {
			t.Fatal("another requester's authorization must be neither bound nor written")
		}
		if !hasNote(leg.notes, PendAmendmentUnboundEvent+":"+string(pendRefusalOtherRequester)) {
			t.Fatalf("notes %v", leg.notes)
		}
		if rec, _, _ := s.PendRecordOf("PCI-CONF-UPD", origCorr); rec.State != PendStatePended {
			t.Fatalf("provider-a's authorization moved: %+v", rec)
		}
	})
	t.Run("another patient's authorization under the correlation", func(t *testing.T) {
		s := newCensusSoR()
		if _, err := s.RecordPendedKeyed("PCI-OTHER", origCorr, time.Unix(2000, 0).UTC(), PendKeys{RequesterHolder: "provider", PreAuthRef: "PA-1"}); err != nil {
			t.Fatal(err)
		}
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		ctx, _ := withPASLeg(context.Background(), "provider")
		res, err := n.Handle(ctx, "pas-claim-update", "corr-1", "PCI-CONF-UPD", originatorBuiltConformantUpdateBundle(t))
		if err != nil || res.Status != 0 {
			t.Fatalf("the amendment must still reach the payer: %v %d", err, res.Status)
		}
		if res.Commit != nil {
			t.Fatal("an unbound amendment must not create a second authorization under the correlation")
		}
		if _, found, _ := s.PendRecordOf("PCI-CONF-UPD", origCorr); found {
			t.Fatal("a row was created for the second patient")
		}
	})
}

// releaseCountingStore counts releases and fails every keyed pend write.
type releaseCountingStore struct {
	*censusSoR
	releases int
}

func (s *releaseCountingStore) ReleaseClaimUpdate(pci, corr string) error {
	s.releases++
	return s.censusSoR.ReleaseClaimUpdate(pci, corr)
}

func (s *releaseCountingStore) RecordPendedKeyed(string, string, time.Time, PendKeys) (PendTransition, error) {
	return PendTransition{}, ErrPendKeyTooLong
}

// A bound amendment whose re-pend releases the claim and then fails to record
// is not released a second time by its rollback: by then another amendment may
// hold the claim.
func TestNativeUpdate_TheClaimIsReleasedOnce(t *testing.T) {
	const origCorr = "convergence-pas-submit-0001"
	raw, err := os.ReadFile(filepath.Join("testdata", "br-payer", "pas-update-response-amend-after-resolution.json"))
	if err != nil {
		t.Fatal(err)
	}
	rePend := fixturePASResponse(t, raw, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write(rePend)
	}))
	defer srv.Close()
	s := &releaseCountingStore{censusSoR: newCensusSoR()}
	_ = s.RecordPendedClaim("PCI-CONF-UPD", origCorr)
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
	ctx, _ := withPASLeg(context.Background(), "provider")
	res, err := n.Handle(ctx, "pas-claim-update", "corr-1", "PCI-CONF-UPD", originatorBuiltConformantUpdateBundle(t))
	if err != nil || res.Commit == nil || res.Rollback == nil {
		t.Fatalf("bound re-pend: err=%v commit=%v rollback=%v", err, res.Commit != nil, res.Rollback != nil)
	}
	if err := res.Commit(); err == nil {
		t.Fatal("the record write was meant to fail")
	}
	res.Rollback()
	if s.releases != 1 {
		t.Fatalf("releases = %d, want 1", s.releases)
	}
}
