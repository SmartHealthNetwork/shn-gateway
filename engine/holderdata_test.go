package engine

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestStubHolderData_Personas proves the two UC-01 personas in isolation.
// MBR-COVERED must resolve to a stable pci: value with CoverageInforce=true.
// MBR-NOTCOVERED must resolve to a stable pci: value with inforce=false and
// reason="coverage-terminated". UNKNOWN must yield found=false.
func TestStubHolderData_Personas(t *testing.T) {
	d := NewStubHolderData()

	t.Run("MBR-COVERED resolved and inforce", func(t *testing.T) {
		pci, demo, found := d.ResolvePatient("MBR-COVERED")
		if !found {
			t.Fatal("expected found=true for MBR-COVERED")
		}
		if !strings.HasPrefix(pci, "pci:") {
			t.Errorf("pci must start with 'pci:', got %q", pci)
		}
		if pci == "pci:" {
			t.Errorf("pci must have a non-empty hash after 'pci:', got %q", pci)
		}
		if demo.BirthDate != "1975-04-02" {
			t.Errorf("BirthDate = %q, want 1975-04-02", demo.BirthDate)
		}
		if demo.FamilyName != "Johansson" {
			t.Errorf("FamilyName = %q, want Johansson", demo.FamilyName)
		}

		inforce, reason := d.CoverageInforce("MBR-COVERED")
		if !inforce {
			t.Error("CoverageInforce: want true for MBR-COVERED")
		}
		if reason != "" {
			t.Errorf("CoverageInforce reason = %q, want empty string", reason)
		}
	})

	t.Run("MBR-COVERED pci is stable across calls", func(t *testing.T) {
		pci1, _, _ := d.ResolvePatient("MBR-COVERED")
		pci2, _, _ := d.ResolvePatient("MBR-COVERED")
		if pci1 != pci2 {
			t.Errorf("pci must be deterministic: first=%q second=%q", pci1, pci2)
		}
	})

	t.Run("MBR-NOTCOVERED resolved and not inforce", func(t *testing.T) {
		pci, demo, found := d.ResolvePatient("MBR-NOTCOVERED")
		if !found {
			t.Fatal("expected found=true for MBR-NOTCOVERED")
		}
		if !strings.HasPrefix(pci, "pci:") {
			t.Errorf("pci must start with 'pci:', got %q", pci)
		}
		if demo.BirthDate != "1980-09-15" {
			t.Errorf("BirthDate = %q, want 1980-09-15", demo.BirthDate)
		}
		if demo.FamilyName != "Reyes" {
			t.Errorf("FamilyName = %q, want Reyes", demo.FamilyName)
		}

		inforce, reason := d.CoverageInforce("MBR-NOTCOVERED")
		if inforce {
			t.Error("CoverageInforce: want false for MBR-NOTCOVERED")
		}
		if reason != "coverage-terminated" {
			t.Errorf("CoverageInforce reason = %q, want 'coverage-terminated'", reason)
		}
	})

	t.Run("MBR-COVERED and MBR-NOTCOVERED have different PCIs", func(t *testing.T) {
		pci1, _, _ := d.ResolvePatient("MBR-COVERED")
		pci2, _, _ := d.ResolvePatient("MBR-NOTCOVERED")
		if pci1 == pci2 {
			t.Errorf("different members must have different PCIs, both got %q", pci1)
		}
	})

	t.Run("UNKNOWN member not found", func(t *testing.T) {
		_, _, found := d.ResolvePatient("UNKNOWN")
		if found {
			t.Error("expected found=false for UNKNOWN member")
		}

		inforce, reason := d.CoverageInforce("UNKNOWN")
		if inforce {
			t.Error("CoverageInforce: want false for unknown member")
		}
		if reason != "" {
			t.Errorf("CoverageInforce reason for unknown = %q, want empty", reason)
		}
	})
}

// TestStubHolderData_ClinicalContext proves the covered persona carries the
// full provider-LOCAL clinical context, and non-covered/unknown members do not.
func TestStubHolderData_ClinicalContext(t *testing.T) {
	d := NewStubHolderData()

	t.Run("covered member has clinical context", func(t *testing.T) {
		cc, found := d.ClinicalContext("MBR-COVERED")
		if !found {
			t.Fatal("expected found=true for MBR-COVERED")
		}
		if cc.ConditionCode != "M51.16" {
			t.Errorf("ConditionCode = %q, want M51.16", cc.ConditionCode)
		}
		if cc.ConditionRef != "Condition/cond-m5116" {
			t.Errorf("ConditionRef = %q, want Condition/cond-m5116", cc.ConditionRef)
		}
		if cc.ConservativeTherapyWeeks != 6 {
			t.Errorf("ConservativeTherapyWeeks = %d, want 6", cc.ConservativeTherapyWeeks)
		}
		if cc.ConservativeTherapyRef != "Observation/obs-pt-weeks" {
			t.Errorf("ConservativeTherapyRef = %q, want Observation/obs-pt-weeks", cc.ConservativeTherapyRef)
		}
		if cc.ConservativeDate != "2026-05-20" {
			t.Errorf("ConservativeDate = %q, want 2026-05-20", cc.ConservativeDate)
		}
		if cc.NeuroDeficit {
			t.Error("NeuroDeficit = true, want false")
		}
		if cc.NeuroDeficitRef != "Observation/obs-neuro" {
			t.Errorf("NeuroDeficitRef = %q, want Observation/obs-neuro", cc.NeuroDeficitRef)
		}
		if !cc.PriorImaging {
			t.Error("PriorImaging = false, want true")
		}
		if cc.PriorImagingRef != "DiagnosticReport/dr-xray" {
			t.Errorf("PriorImagingRef = %q, want DiagnosticReport/dr-xray", cc.PriorImagingRef)
		}
	})

	t.Run("not-covered member has no clinical context", func(t *testing.T) {
		if _, found := d.ClinicalContext("MBR-NOTCOVERED"); found {
			t.Error("expected found=false for MBR-NOTCOVERED")
		}
	})

	t.Run("unknown member has no clinical context", func(t *testing.T) {
		if _, found := d.ClinicalContext("UNKNOWN"); found {
			t.Error("expected found=false for UNKNOWN")
		}
	})
}

// TestStubHolderData_AuthNumber proves the auth-number store round-trips and
// that an absent serviceRequestRef yields found=false.
func TestStubHolderData_AuthNumber(t *testing.T) {
	d := NewStubHolderData()

	t.Run("store then read round-trip", func(t *testing.T) {
		if err := d.StoreAuthNumber("ServiceRequest/sr-1", "PA-12345"); err != nil {
			t.Fatalf("StoreAuthNumber: %v", err)
		}
		got, found := d.AuthNumber("ServiceRequest/sr-1")
		if !found {
			t.Fatal("expected found=true after StoreAuthNumber")
		}
		if got != "PA-12345" {
			t.Errorf("AuthNumber = %q, want PA-12345", got)
		}
	})

	t.Run("absent ref not found", func(t *testing.T) {
		if _, found := d.AuthNumber("ServiceRequest/absent"); found {
			t.Error("expected found=false for absent ref")
		}
	})
}

// TestStubHolderData_AuthNumberConcurrent is a -race smoke test for the
// auth-number store under concurrent Store/AuthNumber access.
func TestStubHolderData_AuthNumberConcurrent(t *testing.T) {
	d := NewStubHolderData()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		ref := "ServiceRequest/sr-" + strconvI(i)
		go func() {
			defer wg.Done()
			_ = d.StoreAuthNumber(ref, "PA-"+strconvI(i))
		}()
		go func() {
			defer wg.Done()
			d.AuthNumber(ref)
		}()
	}
	wg.Wait()
}

func strconvI(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return string(b)
}

// TestPersonas_UC04_UC06 verifies the UC-04/UC-06 personas and the
// SupplementalReport accessor (UC-04 FR-32, FR-35/39).
func TestPersonas_UC04_UC06(t *testing.T) {
	d := NewStubHolderData()

	if _, _, found := d.ResolvePatient("MBR-UC04"); !found {
		t.Fatal("MBR-UC04 must resolve")
	}
	cc, ok := d.ClinicalContext("MBR-UC04")
	if !ok || !cc.PriorSurgery {
		t.Fatalf("MBR-UC04 ClinicalContext PriorSurgery: ok=%v cc=%+v", ok, cc)
	}
	dr, ok := d.SupplementalReport("MBR-UC04")
	if !ok || len(dr) == 0 {
		t.Fatal("MBR-UC04 must have a supplemental DiagnosticReport")
	}

	cc6, ok := d.ClinicalContext("MBR-UC06")
	if !ok || !cc6.HighDisability {
		t.Fatalf("MBR-UC06 ClinicalContext HighDisability: ok=%v cc=%+v", ok, cc6)
	}
	if _, ok := d.SupplementalReport("MBR-UC06"); ok {
		t.Fatal("MBR-UC06 has no separate DiagnosticReport (manual entry path)")
	}
}

// TestPendedClaimLedger verifies the state machine: record(pended) → begin(claim) →
// finalize(gone) and the release path (FR-21/FR-6).
func TestPendedClaimLedger(t *testing.T) {
	d := NewStubHolderData()
	// No record yet → cannot begin.
	ok, err := d.BeginClaimUpdate("pci:1", "corr-1")
	if err != nil {
		t.Fatalf("BeginClaimUpdate error: %v", err)
	}
	if ok {
		t.Fatal("BeginClaimUpdate must be false with no prior pend")
	}
	if err := d.RecordPendedClaim("pci:1", "corr-1"); err != nil {
		t.Fatalf("RecordPendedClaim error: %v", err)
	}
	// Mismatched key must not be claimable.
	ok1, _ := d.BeginClaimUpdate("pci:2", "corr-1")
	ok2, _ := d.BeginClaimUpdate("pci:1", "corr-2")
	if ok1 || ok2 {
		t.Fatal("ledger is keyed on (pci, correlation) — mismatched key must not begin")
	}
	// Begin claims it; a SECOND begin (concurrent/replay) fails while in-progress.
	ok, err = d.BeginClaimUpdate("pci:1", "corr-1")
	if err != nil {
		t.Fatalf("BeginClaimUpdate error: %v", err)
	}
	if !ok {
		t.Fatal("first BeginClaimUpdate on a pended claim must succeed")
	}
	ok, _ = d.BeginClaimUpdate("pci:1", "corr-1")
	if ok {
		t.Fatal("second BeginClaimUpdate while in-progress must fail")
	}
	// Release returns it to pended → claimable again (retry after insufficient).
	if err := d.ReleaseClaimUpdate("pci:1", "corr-1"); err != nil {
		t.Fatalf("ReleaseClaimUpdate error: %v", err)
	}
	ok, err = d.BeginClaimUpdate("pci:1", "corr-1")
	if err != nil {
		t.Fatalf("BeginClaimUpdate error: %v", err)
	}
	if !ok {
		t.Fatal("after release, the claim must be claimable again")
	}
	// Finalize removes it → not claimable (replay protection).
	if err := d.FinalizeClaimUpdate("pci:1", "corr-1"); err != nil {
		t.Fatalf("FinalizeClaimUpdate error: %v", err)
	}
	ok, _ = d.BeginClaimUpdate("pci:1", "corr-1")
	if ok {
		t.Fatal("finalized claim must not be claimable (replay protection)")
	}
}

// TestPendedClaim_AtomicUnderConcurrency (race regression, #3): with N goroutines
// racing to claim the SAME pended claim, EXACTLY ONE wins. Run under -race.
func TestPendedClaim_AtomicUnderConcurrency(t *testing.T) {
	d := NewStubHolderData()
	if err := d.RecordPendedClaim("pci:1", "corr-1"); err != nil {
		t.Fatalf("RecordPendedClaim: %v", err)
	}

	const n = 64
	var wins int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ok, _ := d.BeginClaimUpdate("pci:1", "corr-1")
			if ok {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent BeginClaimUpdate winners = %d, want exactly 1", wins)
	}
}

// TestStubHolderData_Reset (#4): Reset clears mutable state — auth numbers and the
// pended-claim ledger — so the demo returns to clean synthetic state.
func TestStubHolderData_Reset(t *testing.T) {
	d := NewStubHolderData()
	if err := d.StoreAuthNumber("ServiceRequest/sr-1", "PA-abc"); err != nil {
		t.Fatalf("StoreAuthNumber: %v", err)
	}
	if err := d.RecordPendedClaim("pci:1", "corr-1"); err != nil {
		t.Fatalf("RecordPendedClaim: %v", err)
	}

	d.Reset()

	if _, found := d.AuthNumber("ServiceRequest/sr-1"); found {
		t.Error("Reset must clear the auth-number store")
	}
	ok, _ := d.BeginClaimUpdate("pci:1", "corr-1")
	if ok {
		t.Error("Reset must clear the pended-claim ledger")
	}
	// Read-only personas must survive Reset.
	if _, _, found := d.ResolvePatient("MBR-UC04"); !found {
		t.Error("Reset must not affect read-only persona fixtures")
	}
}

// TestEOBReaders_ReturnDefensiveCopies verifies that EOBByID and EOBsForPatient
// return defensive copies so a caller cannot mutate stored state (review #5).
func TestEOBReaders_ReturnDefensiveCopies(t *testing.T) {
	d := NewStubHolderData()
	pci := "pci:test-eob-copy"
	orig := []byte(`{"resourceType":"ExplanationOfBenefit","id":"eob-x"}`)
	if err := d.RecordEOB(pci, "eob-x", orig); err != nil {
		t.Fatalf("RecordEOB: %v", err)
	}

	// EOBByID: mutating the returned bytes must not change stored state.
	got, ok := d.EOBByID("eob-x")
	if !ok {
		t.Fatal("EOBByID not found")
	}
	got[0] = 'X'
	again, _ := d.EOBByID("eob-x")
	if again[0] == 'X' {
		t.Fatal("EOBByID returned a mutable reference to stored bytes")
	}

	// EOBsForPatient: mutating the returned slice/elements must not change state.
	list, ok := d.EOBsForPatient(pci)
	if !ok || len(list) != 1 {
		t.Fatalf("EOBsForPatient = %d (ok=%v), want 1", len(list), ok)
	}
	list[0][0] = 'Y'
	list = append(list, []byte("junk"))
	_ = list
	after, _ := d.EOBsForPatient(pci)
	if len(after) != 1 {
		t.Fatalf("stored slice grew to %d after caller append — not a copy", len(after))
	}
	if after[0][0] == 'Y' {
		t.Fatal("EOBsForPatient returned mutable references to stored bytes")
	}
}

// TestEOBStore verifies the payer-side PA-decision EOB store (UC-08, FR-28):
// RecordEOB → EOBsForPatient + EOBByID; Reset clears the store.
func TestEOBStore(t *testing.T) {
	d := NewStubHolderData()
	if _, ok := d.EOBByID("eob-uc08"); ok {
		t.Fatal("EOBByID on empty store returned ok")
	}
	if err := d.RecordEOB("pci:abc", "eob-uc08", []byte(`{"resourceType":"ExplanationOfBenefit","id":"eob-uc08"}`)); err != nil {
		t.Fatalf("RecordEOB: %v", err)
	}
	got, ok := d.EOBsForPatient("pci:abc")
	if !ok || len(got) != 1 {
		t.Fatalf("EOBsForPatient = %v, %v; want one EOB", len(got), ok)
	}
	if _, ok := d.EOBByID("eob-uc08"); !ok {
		t.Fatal("EOBByID after record returned not-ok")
	}
	d.Reset()
	if _, ok := d.EOBsForPatient("pci:abc"); ok {
		t.Fatal("EOBsForPatient after Reset returned ok")
	}
}
