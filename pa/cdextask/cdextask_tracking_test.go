package cdextask

import (
	"reflect"
	"testing"
)

var example19Tracking = IdentifierField{State: Present, Identifier: Identifier{
	System: "http://example.org/payer", Value: "trackingid123",
	Type: []Coding{{System: CDexTempCodes, Code: IdentifierTrackingID}},
}}

// The tracking identifier is the one Task.identifier typed
// cdex-temp#tracking-id, read by its type's system and code, never by text.
// Task.identifier is read only for pairing, so nothing in it refuses the
// Task: a member it names in another case is an unreadable tracking
// identifier, and the rest of the Task reads as it did.
func TestRead_Tracking(t *testing.T) {
	tracking := func(system, value string) map[string]any {
		id := map[string]any{"type": coding(CDexTempCodes, IdentifierTrackingID), "value": value}
		if system != "" {
			id["system"] = system
		}
		return id
	}
	other := map[string]any{"system": "http://example.org/other", "value": "x-1"}
	present := func(system, value string) IdentifierField {
		return IdentifierField{State: Present, Identifier: Identifier{System: system, Value: value, Type: []Coding{{System: CDexTempCodes, Code: IdentifierTrackingID}}}}
	}
	for name, row := range map[string]struct {
		edit func(map[string]any)
		want IdentifierField
	}{
		"the IG example":                    {func(map[string]any) {}, example19Tracking},
		"no identifier":                     {func(m map[string]any) { delete(m, "identifier") }, IdentifierField{State: Absent}},
		"identifier null":                   {func(m map[string]any) { m["identifier"] = nil }, unreadable},
		"identifier empty":                  {func(m map[string]any) { m["identifier"] = []any{} }, unreadable},
		"identifier not a list":             {func(m map[string]any) { m["identifier"] = tracking("s", "v") }, unreadable},
		"none typed tracking-id":            {func(m map[string]any) { m["identifier"] = []any{other} }, IdentifierField{State: Absent}},
		"beside another":                    {func(m map[string]any) { m["identifier"] = []any{other, tracking("s", "v")} }, present("s", "v")},
		"no system":                         {func(m map[string]any) { m["identifier"] = []any{tracking("", "v")} }, present("", "v")},
		"two the same":                      {func(m map[string]any) { m["identifier"] = []any{tracking("s", "v"), tracking("s", "v")} }, present("s", "v")},
		"two values that differ":            {func(m map[string]any) { m["identifier"] = []any{tracking("s", "v"), tracking("s", "w")} }, unreadable},
		"two systems that differ":           {func(m map[string]any) { m["identifier"] = []any{tracking("s", "v"), tracking("t", "v")} }, unreadable},
		"one without a system and one with": {func(m map[string]any) { m["identifier"] = []any{tracking("", "v"), tracking("s", "v")} }, unreadable},
		"the code under another system": {func(m map[string]any) {
			m["identifier"] = []any{map[string]any{"type": coding(PASTempCodes, IdentifierTrackingID), "system": "s", "value": "v"}}
		}, IdentifierField{State: Absent}},
		"its display, not its code": {func(m map[string]any) {
			m["identifier"] = []any{map[string]any{"type": map[string]any{"coding": []any{map[string]any{"system": CDexTempCodes, "code": "other", "display": IdentifierTrackingID}}, "text": IdentifierTrackingID}, "system": "s", "value": "v"}}
		}, IdentifierField{State: Absent}},
		"an empty value": {func(m map[string]any) { m["identifier"] = []any{tracking("s", "")} }, unreadable},
		"another that is unreadable": {func(m map[string]any) {
			m["identifier"] = []any{tracking("s", "v"), map[string]any{"system": "s"}}
		}, unreadable},
		"an identifier that is null": {func(m map[string]any) { m["identifier"] = []any{tracking("s", "v"), nil} }, unreadable},
		"Task.identifier named in another case": {func(m map[string]any) {
			m["Identifier"] = m["identifier"]
			delete(m, "identifier")
		}, unreadable},
		"a member in another case": {func(m map[string]any) {
			m["identifier"] = []any{map[string]any{"type": coding(CDexTempCodes, IdentifierTrackingID), "System": "s", "value": "v"}}
		}, unreadable},
	} {
		t.Run(name, func(t *testing.T) {
			task := read(t, mutate(t, row.edit))
			if !reflect.DeepEqual(task.Tracking, row.want) {
				t.Fatalf("tracking %+v\nwant %+v", task.Tracking, row.want)
			}
			// The rest of the Task reads as the IG example does.
			base := read(t, mutate(t, func(map[string]any) {}))
			base.Tracking, task.Tracking = IdentifierField{}, IdentifierField{}
			if !reflect.DeepEqual(task, base) {
				t.Fatal("the rest of the Task reads differently")
			}
		})
	}
}
