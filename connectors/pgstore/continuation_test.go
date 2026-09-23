package pgstore

// continuation_test.go — the continuation store's durable rows, plus the one
// hermetic row that fences what its tables may declare.
//
// TestContinuation_NoClinicalColumns runs with no database: it parses the very
// DDL EnsureSchema executes. The rest are pg-gated (testPool skips without
// SHN_TEST_DATABASE_URL) and run in the serial `pg` job.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// TestContinuation_NoClinicalColumns: the continuation is metadata. Its tables
// declare no opaque column — no BYTEA, no JSON — so there is nowhere for an order
// or a QuestionnaireResponse to be kept, and the ddl fence's exception list gains
// no entry (AI-1).
//
// The generic fence already walks every gw_* table. This row names the two
// continuation tables directly, because "the fence is green" would also be true
// of a schema in which these tables did not exist at all.
func TestContinuation_NoClinicalColumns(t *testing.T) {
	tables := parseDDLTables(t, ddl)
	for _, name := range []string{"gw_pa_continuation", "gw_pa_continuation_item"} {
		cols, ok := tables[name]
		if !ok {
			t.Fatalf("ddl declares no %s", name)
		}
		for _, c := range cols {
			if contentBearingTypes[normalizeSQLType(c.typ)] {
				t.Errorf("%s.%s is %s: a continuation stores metadata only — no order bytes and no "+
					"QuestionnaireResponse bytes. The order is re-read from the participant's own system "+
					"at inquiry time.", name, c.name, c.typ)
			}
		}
		if _, exempt := ddlContentExceptions[name]; exempt {
			t.Errorf("%s must need no ddlContentExceptions entry", name)
		}
	}
	// The item map's own columns are pinned: they ARE the "what was submitted"
	// record the order-changed check compares against, so a column quietly
	// dropped would make that check compare against less than it says.
	items := tables["gw_pa_continuation_item"]
	for _, want := range []string{"sequence", "product_code", "product_display", "service_date", "trace_number", "authorization_number", "administration_reference_number"} {
		if _, ok := columnType(items, want); !ok {
			t.Errorf("gw_pa_continuation_item declares no %s column", want)
		}
	}
	// And the parent's arrays stay arrays of scalars rather than becoming one
	// opaque column with the keys inside it.
	parent := tables["gw_pa_continuation"]
	for _, want := range []string{"item_trace_numbers", "payer_claimresponse_ids", "claim_references"} {
		typ, ok := columnType(parent, want)
		if !ok {
			t.Fatalf("gw_pa_continuation declares no %s column", want)
		}
		if !strings.HasSuffix(strings.ToUpper(typ), "[]") || normalizeSQLType(typ) != "TEXT" {
			t.Errorf("gw_pa_continuation.%s is %s, want a TEXT[] of identifier strings", want, typ)
		}
	}

	// THE PARENT'S COLUMN SET IS PINNED, EXACTLY, the way the item table's is
	// above. The type check alone is not the guard it looks like: it catches
	// BYTEA and JSONB, and would wave through `order_json TEXT NOT NULL` — a
	// perfectly scalar column holding the order bytes this table exists not to
	// hold. What keeps clinical content out is the column set, not the types, so
	// a new column has to be added HERE and read on its own terms.
	wantParent := map[string]bool{
		"holder_id": true, "continuation_id": true, // identity
		"payer_holder": true, "line": true, "correlation_id": true, // routing
		"subject_pci": true, "member_id": true, "sor_patient_id": true, // the patient, three ways
		"order_ref": true, "provider_npi": true, // what was asked, and who asked
		"claim_identifier": true, "claim_type": true, "claim_priority": true,
		"claim_references":   true, // exact submitted Claim identity metadata, never clinical bytes
		"item_trace_numbers": true, "payer_claimresponse_ids": true, "payer_preauth_ref": true,
		"last_outcome": true, "created_at": true, "updated_at": true,
	}
	got := map[string]bool{}
	for _, c := range parent {
		got[c.name] = true
		if !wantParent[c.name] {
			t.Errorf("gw_pa_continuation gained the column %s (%s). A continuation stores metadata only — "+
				"identity, routing, the member, what was asked and what the payer answered. If this column "+
				"belongs there, add it to this list and say what it holds; the type check will not stop a "+
				"TEXT column holding an order.", c.name, c.typ)
		}
	}
	for name := range wantParent {
		if !got[name] {
			t.Errorf("gw_pa_continuation no longer declares %s — if the column is gone, shrink this list too", name)
		}
	}
}

// An existing metadata row gains no invented request-linkage authority when
// EnsureSchema adds the column. This runs only against the owned PG test DB.
func TestContinuation_ExistingRowMigrationDefaultsEmpty(t *testing.T) {
	pool := openTestPool(t)
	dropGWTables(t, pool)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE TABLE gw_pa_continuation (
		holder_id TEXT NOT NULL, continuation_id TEXT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (holder_id, continuation_id))`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gw_pa_continuation (holder_id, continuation_id) VALUES ('provider-a', 'old')`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var refs []string
	if err := pool.QueryRow(ctx, `SELECT claim_references FROM gw_pa_continuation WHERE holder_id='provider-a' AND continuation_id='old'`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("migration invented old-row request authority: %q", refs)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
}

func continuationStore(t *testing.T, holder string, clock func() time.Time) *PgStore {
	t.Helper()
	pool := testPool(t)
	s, err := NewPgStore(context.Background(), pool, holder)
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	if clock != nil {
		s.now = clock
	}
	return s
}

func durableSample(holder string) engine.Continuation {
	return engine.Continuation{
		Holder:           holder,
		PayerHolder:      "payer-a",
		Line:             "2.0",
		CorrID:           "corr-1",
		SubjectPCI:       "pci:one",
		MemberID:         "MBR-D-UC04",
		SoRPatientID:     "Patient/p1",
		OrderRef:         "ServiceRequest/sr-1",
		ProviderNPI:      "1234567893",
		ClaimIdentifier:  "urn:shn:claim|claim-1",
		ClaimReferences:  []string{"Claim/claim-1", "https://shn.example/fhir/Claim/claim-1"},
		ClaimType:        "http://terminology.hl7.org/CodeSystem/claim-type|professional",
		ClaimPriority:    "http://terminology.hl7.org/CodeSystem/processpriority|normal",
		ItemTraceNumbers: []string{"urn:shn:trace|corr-1-1"},
		Items: []engine.ContinuationItem{{
			Sequence: 1, ProductCode: "hcpcs|E0424", ServiceDate: "2027-04-01",
			TraceNumber: "urn:shn:trace|corr-1-1", AuthorizationNumber: "REF-BB-1", AdministrationReferenceNumber: "REF-NT-1",
		}},
		PayerClaimResponseIDs: []string{"urn:payer:cr|cr-1"},
		PayerPreAuthRef:       "PA-0001",
		LastOutcome:           engine.ContinuationOutcomePended,
	}
}

// TestContinuation_SurvivesRestart: the durable store keeps what it minted. A
// gateway that restarts answers the same continuation, which is exactly the
// property the in-memory store discloses that it does not have.
func TestContinuation_SurvivesRestart(t *testing.T) {
	before := continuationStore(t, "provider-a", nil)
	stored, err := before.PutContinuation(durableSample("provider-a"))
	if err != nil {
		t.Fatalf("PutContinuation: %v", err)
	}
	if !strings.HasPrefix(stored.ID, engine.ContinuationDurableMark+"-") {
		t.Fatalf("a durable store must mint under the durable mark: %q", stored.ID)
	}
	// The restart: a new store over the same database, same holder.
	after, err := NewPgStore(context.Background(), before.pool, "provider-a")
	if err != nil {
		t.Fatal(err)
	}
	got, look, err := after.ReadContinuation("provider-a", stored.ID)
	if err != nil || look != engine.ContinuationFound {
		t.Fatalf("after a restart: lookup = %v, err = %v", look, err)
	}
	if got.OrderRef != "ServiceRequest/sr-1" || got.PayerPreAuthRef != "PA-0001" ||
		!slices.Equal(got.ClaimReferences, stored.ClaimReferences) ||
		got.ClaimType != "http://terminology.hl7.org/CodeSystem/claim-type|professional" ||
		got.Items[0].AuthorizationNumber != "REF-BB-1" || got.Items[0].AdministrationReferenceNumber != "REF-NT-1" ||
		len(got.Items) != 1 || got.Items[0].ProductCode != "hcpcs|E0424" ||
		len(got.ItemTraceNumbers) != 1 || got.ItemTraceNumbers[0] != "urn:shn:trace|corr-1-1" {
		t.Fatalf("a stored fact did not survive: %+v", got)
	}
	// And nothing here is ever reported as lost: a durable store did not drop it.
	_, look, err = after.ReadContinuation("provider-a", engine.NewContinuationID(engine.ContinuationDurableMark))
	if err != nil {
		t.Fatal(err)
	}
	if look != engine.ContinuationUnknown {
		t.Fatalf("an id this store never minted = %v, want unknown", look)
	}
}

// TestContinuation_ReplicaSwitch: two replicas of one holder's gateway share the
// store, so a continuation minted at one is answerable at the other. That is the
// shared-state property every durable seam in this package has, applied to the
// surface a browser or a partner may reach through a load balancer.
func TestContinuation_ReplicaSwitch(t *testing.T) {
	replicaA := continuationStore(t, "provider-a", nil)
	replicaB, err := NewPgStore(context.Background(), replicaA.pool, "provider-a")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := replicaA.PutContinuation(durableSample("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	got, look, err := replicaB.ReadContinuation("provider-a", stored.ID)
	if err != nil || look != engine.ContinuationFound {
		t.Fatalf("replica B: lookup = %v, err = %v", look, err)
	}
	// The other replica then records the decision, and the first sees it.
	got.LastOutcome = engine.ContinuationOutcomeApproved
	if _, err := replicaB.PutContinuation(got); err != nil {
		t.Fatal(err)
	}
	back, look, err := replicaA.ReadContinuation("provider-a", stored.ID)
	if err != nil || look != engine.ContinuationFound {
		t.Fatalf("replica A: lookup = %v, err = %v", look, err)
	}
	if back.LastOutcome != "approved" {
		t.Fatalf("LastOutcome = %q, want approved", back.LastOutcome)
	}
	if !slices.Equal(back.ClaimReferences, stored.ClaimReferences) {
		t.Fatalf("replica update lost submitted references: %v", back.ClaimReferences)
	}
	if !back.CreatedAt.Equal(stored.CreatedAt) {
		t.Fatalf("an update at another replica moved CreatedAt: %v → %v", stored.CreatedAt, back.CreatedAt)
	}
}

// TestContinuation_OtherHolderID404Durable: the binding is {holder, id} in the
// durable store too — another holder sharing the database is told nothing exists.
func TestContinuation_OtherHolderID404Durable(t *testing.T) {
	a := continuationStore(t, "provider-a", nil)
	stored, err := a.PutContinuation(durableSample("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPgStore(context.Background(), a.pool, "provider-b")
	if err != nil {
		t.Fatal(err)
	}
	_, look, err := b.ReadContinuation("provider-b", stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if look != engine.ContinuationUnknown {
		t.Fatalf("another holder's lookup = %v, want unknown", look)
	}
}

// TestContinuation_ItemMapIsReplacedNotMerged: the item map states what ONE
// submission asked for. A re-write with fewer lines leaves fewer lines, not a
// union with the previous submission's.
func TestContinuation_ItemMapIsReplacedNotMerged(t *testing.T) {
	s := continuationStore(t, "provider-a", nil)
	c := durableSample("provider-a")
	c.ItemTraceNumbers = []string{"urn:shn:trace|a", "urn:shn:trace|b"}
	c.Items = []engine.ContinuationItem{
		{Sequence: 1, ProductCode: "hcpcs|E0424", ServiceDate: "2027-04-01", TraceNumber: "urn:shn:trace|a"},
		{Sequence: 2, ProductCode: "hcpcs|L8000", ServiceDate: "2027-04-01", TraceNumber: "urn:shn:trace|b"},
	}
	stored, err := s.PutContinuation(c)
	if err != nil {
		t.Fatal(err)
	}
	stored.ItemTraceNumbers = []string{"urn:shn:trace|a"}
	stored.Items = stored.Items[:1]
	if _, err := s.PutContinuation(stored); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ReadContinuation("provider-a", stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Sequence != 1 {
		t.Fatalf("items = %+v, want exactly the current submission's one line", got.Items)
	}
}

// TestContinuation_RetentionPurgeIsBoundedPerCall: the durable sweep drops what
// is past its retention, at most engine.PendPurgeMax per call, and the item rows
// go with the row they belong to.
func TestContinuation_RetentionPurgeIsBoundedPerCall(t *testing.T) {
	now := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	s := continuationStore(t, "provider-a", func() time.Time { return now })
	fresh := now
	now = now.Add(-engine.ContinuationRetention - 24*time.Hour)
	var ids []string
	for i := 0; i < engine.PendPurgeMax+20; i++ {
		c, err := s.PutContinuation(durableSample("provider-a"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
		s.mu.Lock()
		s.lastContinuationPurge = now
		s.mu.Unlock()
	}
	now = fresh
	kept, err := s.PutContinuation(durableSample("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	remaining := 0
	for _, id := range ids {
		if _, look, _ := s.ReadContinuation("provider-a", id); look == engine.ContinuationFound {
			remaining++
		}
	}
	if want := len(ids) - engine.PendPurgeMax; remaining != want {
		t.Fatalf("after one sweep %d stale continuations remain, want %d", remaining, want)
	}
	if _, look, _ := s.ReadContinuation("provider-a", kept.ID); look != engine.ContinuationFound {
		t.Fatal("a continuation inside the retention window was purged")
	}
	var orphans int
	if err := s.pool.QueryRow(context.Background(), `
SELECT count(*) FROM gw_pa_continuation_item i
 WHERE NOT EXISTS (SELECT 1 FROM gw_pa_continuation c
                    WHERE c.holder_id=i.holder_id AND c.continuation_id=i.continuation_id)`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("%d item rows outlived the continuation they belong to", orphans)
	}
}

// TestContinuation_DurableStoreSaysItIsDurable: the Kit's notice and CONFIGURATION
// both read the store's own answer rather than re-deriving it from how the
// gateway happens to be wired.
func TestContinuation_DurableStoreSaysItIsDurable(t *testing.T) {
	s := continuationStore(t, "provider-a", nil)
	var cs engine.ContinuationStore = s
	if !engine.DurableContinuations(cs) {
		t.Fatal("the durable store must report itself as durable")
	}
}

// TestContinuation_PutReturnsWhatTheRowHolds is the rejection row for the
// precision gap that made TestContinuation_ReplicaSwitch fail in CI.
//
// `timestamptz` keeps microseconds. A Go instant can carry nanoseconds. If the
// store hands back the instant it was given rather than the one it wrote, the
// returned struct disagrees with its own row.
//
// The clock is INJECTED with a deliberate sub-microsecond remainder, because the
// host clock decides whether this defect is even reachable: macOS reports
// time.Now() only to microseconds, so the bug is invisible there and reproduces
// on Linux every time. A row that depends on the host's clock resolution is a row
// that passes on the developer's machine and fails in CI — which is exactly how
// this defect reached CI in the first place.
func TestContinuation_PutReturnsWhatTheRowHolds(t *testing.T) {
	ragged := time.Date(2026, 9, 19, 11, 23, 40, 791857913, time.UTC)
	s := continuationStore(t, "provider-a", func() time.Time { return ragged })
	put, err := s.PutContinuation(durableSample("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	read, look, err := s.ReadContinuation("provider-a", put.ID)
	if err != nil || look != engine.ContinuationFound {
		t.Fatalf("lookup = %v, err = %v", look, err)
	}
	if !put.CreatedAt.Equal(read.CreatedAt) {
		t.Fatalf("PutContinuation returned a CreatedAt the row does not hold: %v (returned) vs %v (stored)",
			put.CreatedAt, read.CreatedAt)
	}
	if !put.UpdatedAt.Equal(read.UpdatedAt) {
		t.Fatalf("PutContinuation returned an UpdatedAt the row does not hold: %v (returned) vs %v (stored)",
			put.UpdatedAt, read.UpdatedAt)
	}
}
