package cdslisting

import (
	"slices"
	"strings"
	"testing"
)

// A payer's listing is read as JSON specifies, member names
// case-sensitively: "ID" and "HOOK" are other members, so they never replace
// or stand in for an entry's id and hook, and a listing naming both reads its
// "id" and "hook".
func TestCDSServiceListingIsReadCaseSensitively(t *testing.T) {
	got, err := Read([]byte(`{"Services":[{"id":"x","hook":"order-sign"}],"services":[` +
		`{"id":"a","hook":"order-sign","ID":"z","HOOK":"order-dispatch"},` +
		`{"id":"b","hook":"order-select","ID":"a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []Service{{ID: "a", Hook: "order-sign"}, {ID: "b", Hook: "order-select"}}; !slices.Equal(got, want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	for body, says := range map[string]string{
		`{"Services":[{"id":"a","hook":"order-sign"}]}`: "lists no services array",
		`{"services":null}`:                             "lists no services array",
		`{"services":[{"ID":"a","hook":"order-sign"}]}`: "has no id or no hook",
		`{"services":[{"id":"a","HOOK":"order-sign"}]}`: "has no id or no hook",
		`{"services":[{"id":7,"hook":"order-sign"}]}`:   "service 0's id",
		`{"services":["order-sign"]}`:                   "parse services",
		`[]`:                                            "parse",
	} {
		if _, err := Read([]byte(body)); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want an error saying %q", body, err, says)
		}
	}
}
