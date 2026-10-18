package engine

import (
	"reflect"
	"testing"
)

// namedDTRAnswer is a Parameters answer naming its parameters names, in order.
func namedDTRAnswer(names ...string) []byte {
	b := []byte(`{"resourceType":"Parameters","parameter":[`)
	for i, n := range names {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":"`+n+`","resource":{"resourceType":"Bundle","type":"collection"}}`...)
	}
	return append(b, "]}"...)
}

func TestPlanDTRAnswerNames(t *testing.T) {
	for _, row := range []struct {
		name   string
		answer []byte
		target string
		want   []dtrAnswerRename
	}{
		// Toward 2.2: the 2.0.1/2.1 package and outcome names.
		{"2.0 package and outcome to 2.2", namedDTRAnswer("PackageBundle", "operationOutcome"), "2.2",
			[]dtrAnswerRename{{0, "PackageBundle", "packagebundle"}, {1, "operationOutcome", "outcome"}}},
		{"2.0 package only to 2.2", namedDTRAnswer("PackageBundle"), "2.2",
			[]dtrAnswerRename{{0, "PackageBundle", "packagebundle"}}},
		{"2.0 outcome only to 2.2", namedDTRAnswer("operationOutcome"), "2.2",
			[]dtrAnswerRename{{0, "operationOutcome", "outcome"}}},
		{"several 2.0 packages and outcomes to 2.2", namedDTRAnswer("PackageBundle", "operationOutcome", "PackageBundle", "operationOutcome"), "2.2",
			[]dtrAnswerRename{{0, "PackageBundle", "packagebundle"}, {1, "operationOutcome", "outcome"}, {2, "PackageBundle", "packagebundle"}, {3, "operationOutcome", "outcome"}}},
		{"a 2.0 package beside the common outcome name, to 2.2", namedDTRAnswer("PackageBundle", "outcome"), "2.2",
			[]dtrAnswerRename{{0, "PackageBundle", "packagebundle"}}},
		// Toward 2.0 and 2.1: the 2.2 package name; outcome is their OperationDefinition's own.
		{"2.2 package and outcome to 2.0", namedDTRAnswer("packagebundle", "outcome"), "2.0",
			[]dtrAnswerRename{{0, "packagebundle", "PackageBundle"}}},
		{"2.2 package only to 2.1", namedDTRAnswer("packagebundle"), "2.1",
			[]dtrAnswerRename{{0, "packagebundle", "PackageBundle"}}},
		// No edit: carried as sent.
		{"2.2 outcome only to 2.0 (no line-specific name)", namedDTRAnswer("outcome"), "2.0", nil},
		{"same line 2.2", namedDTRAnswer("packagebundle", "outcome"), "2.2", nil},
		{"same line 2.0", namedDTRAnswer("PackageBundle", "operationOutcome"), "2.0", nil},
		{"2.0.1 names to 2.1 (one family)", namedDTRAnswer("PackageBundle", "operationOutcome"), "2.1", nil},
		{"2.1 names to 2.0 (one family)", namedDTRAnswer("PackageBundle"), "2.0", nil},
		{"names of both families", namedDTRAnswer("PackageBundle", "packagebundle"), "2.2", nil},
		{"names of both families, to 2.0", namedDTRAnswer("operationOutcome", "packagebundle"), "2.0", nil},
		{"return is carried", namedDTRAnswer("return"), "2.2", nil},
		{"return beside a 2.0 outcome: only the outcome", namedDTRAnswer("return", "operationOutcome"), "2.2",
			[]dtrAnswerRename{{1, "operationOutcome", "outcome"}}},
		{"no target line", namedDTRAnswer("PackageBundle"), "", nil},
		{"an unknown target line", namedDTRAnswer("PackageBundle"), "3.0", nil},
		{"not a Parameters", []byte(`{"resourceType":"Bundle","type":"collection"}`), "2.2", nil},
		{"not JSON", []byte(`not json`), "2.2", nil},
		{"an OperationOutcome", []byte(`{"resourceType":"OperationOutcome","issue":[]}`), "2.2", nil},
		{"an empty Parameters", []byte(`{"resourceType":"Parameters"}`), "2.2", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := planDTRAnswerNames(row.answer, row.target)
			if !reflect.DeepEqual(got.Renames, row.want) {
				t.Fatalf("renames %+v, want %+v", got.Renames, row.want)
			}
			if got.Target != row.target {
				t.Fatalf("target %q", got.Target)
			}
		})
	}
}
