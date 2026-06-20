package engine

import (
	"reflect"
	"testing"
)

func TestInMemoryExchangeStore_BeginAppendGet(t *testing.T) {
	s := NewInMemoryExchangeStore()
	ex := s.Begin(workstreamPA)
	if ex.ID == "" {
		t.Fatal("Begin: empty Exchange.ID")
	}
	if ex.Workstream != workstreamPA {
		t.Fatalf("Begin: workstream = %q, want %q", ex.Workstream, workstreamPA)
	}
	rec := LegRecord{Type: "crd-order-select", CorrelationID: "corr-child-1", Subjects: []string{"pci-1"}, Outcome: "approved"}
	if err := s.AppendLeg(ex.ID, rec); err != nil {
		t.Fatalf("AppendLeg: %v", err)
	}
	got, ok := s.Get(ex.ID)
	if !ok {
		t.Fatal("Get: exchange not found after AppendLeg")
	}
	if len(got.Legs) != 1 || got.Legs[0].CorrelationID != "corr-child-1" {
		t.Fatalf("Get: legs = %+v, want one leg with child corr", got.Legs)
	}
	if ex.ID == got.Legs[0].CorrelationID {
		t.Fatal("§11 violated: parent Exchange.ID equals child leg CorrelationID")
	}
}

func TestAppendLeg_UnknownExchangeFailsClosed(t *testing.T) {
	s := NewInMemoryExchangeStore()
	if err := s.AppendLeg("no-such-exchange", LegRecord{Type: "crd-order-select"}); err == nil {
		t.Fatal("AppendLeg to unknown exchange: want error, got nil")
	}
}

func TestLegProject_DropsContent(t *testing.T) {
	leg := Leg{
		Type:     "crd-order-select",
		Physics:  paCatalog["crd-order-select"].Physics,
		Content:  Content{WorkstreamType: workstreamPA, Bytes: []byte(`{"clinical":"secret"}`)},
		Subjects: []string{"pci-1"},
	}
	rec := leg.Project("corr-child-1", "approved")
	if rec.Type != "crd-order-select" || rec.CorrelationID != "corr-child-1" || rec.Outcome != "approved" {
		t.Fatalf("Project: metadata not carried: %+v", rec)
	}
	if len(rec.Subjects) != 1 || rec.Subjects[0] != "pci-1" {
		t.Fatalf("Project: subjects not carried: %+v", rec.Subjects)
	}
}

func TestLegRecord_HasNoBytesField(t *testing.T) {
	assertNoBytes(t, reflect.TypeOf(LegRecord{}), "LegRecord")
	assertNoBytes(t, reflect.TypeOf(Exchange{}), "Exchange")
}

func assertNoBytes(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	switch typ.Kind() {
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			t.Fatalf("%s is a []byte — clinical bytes must never reach the durable Exchange seam (AI-1)", path)
		}
		assertNoBytes(t, typ.Elem(), path+"[]")
	case reflect.Ptr:
		assertNoBytes(t, typ.Elem(), path)
	case reflect.Map:
		// A map value (or key) could smuggle a []byte/Content past a struct-only walk —
		// e.g. a future LegRecord.Metadata map[string][]byte. Recurse into both.
		assertNoBytes(t, typ.Key(), path+"[key]")
		assertNoBytes(t, typ.Elem(), path+"[val]")
	case reflect.Interface:
		// An interface field is inherently unguardable: its dynamic value could be a
		// []byte or Content. Fail closed — a stored interface must be a deliberate,
		// reviewed decision, never a silent hole in the non-aggregation guarantee.
		t.Fatalf("%s is an interface — unguardable; a stored interface could carry clinical bytes (AI-1)", path)
	case reflect.Struct:
		if typ == reflect.TypeOf(Content{}) {
			t.Fatalf("%s embeds engine.Content — content must never reach the durable Exchange seam (AI-1)", path)
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			assertNoBytes(t, f.Type, path+"."+f.Name)
		}
	}
}
