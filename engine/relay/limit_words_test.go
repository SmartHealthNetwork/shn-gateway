package relay

import (
	"errors"
	"strings"
	"testing"
)

// Each relay limit a body passes is named in the words a refusal carries: one
// row for each limit, past it by one, and the body at it read.
func TestLimitError_LimitNamesEachLimit(t *testing.T) {
	array := func(n int) string { return "[" + strings.TrimSuffix(strings.Repeat("0,", n), ",") + "]" }
	for _, row := range []struct {
		name     string
		at, past string
		want     string
	}{
		// The root object is one level, so the array nests 63 more.
		{"depth", `{"a":` + strings.Repeat("[", 63) + strings.Repeat("]", 63) + `}`, `{"a":` + strings.Repeat("[", 64) + strings.Repeat("]", 64) + `}`, "nests deeper than 64 levels"},
		// The object, its member name and the array are three tokens.
		{"tokens", `{"a":` + array(1_000_000-3) + `}`, `{"a":` + array(1_000_000-2) + `}`, "holds more than 1,000,000 JSON tokens"},
		// The size limit passed by one byte.
		{"size", `{"a":"` + strings.Repeat("x", 16<<20-8) + `"}`, `{"a":"` + strings.Repeat("x", 16<<20-7) + `"}`, "is larger than 16 MiB"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := Doc(NewBody([]byte(row.at), OriginUpstreamResponse)); err != nil {
				t.Fatalf("at the limit: %v, want read", err)
			}
			_, err := Doc(NewBody([]byte(row.past), OriginUpstreamResponse))
			var limit *LimitError
			if !errors.As(err, &limit) {
				t.Fatalf("past the limit: %v, want a *LimitError", err)
			}
			if got := limit.Limit(); got != row.want {
				t.Fatalf("Limit() = %q, want %q", got, row.want)
			}
		})
	}
}
