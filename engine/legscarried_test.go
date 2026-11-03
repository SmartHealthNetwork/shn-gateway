package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withLegsTracked gives a request driven straight at a handler the tracker
// routes() gives every exchange route, so the refusal it writes can say which
// legs it carried (carriedLegsStatement).
func withLegsTracked(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), legsCarriedKey{}, &legsCarried{}))
}

// carriedStatement is what a refusal written after legs says about them.
func carriedStatement(legs ...string) string {
	c := &legsCarried{legs: legs}
	return carriedLegsStatement(context.WithValue(context.Background(), legsCarriedKey{}, c))
}

// wantCarried fails unless body ends its refusal saying that exactly legs were
// carried: a refusal at a later leg never reads as one before any leg.
func wantCarried(t *testing.T, body string, legs ...string) {
	t.Helper()
	if want := carriedStatement(legs...); !strings.Contains(body, want+`"`) {
		t.Fatalf("answer %s does not say %q", body, want)
	}
	if n := strings.Count(body, "carried and stand") + strings.Count(body, "No leg of this request was sent"); n != 1 {
		t.Fatalf("answer %s says which legs were carried %d times, want once", body, n)
	}
}

func TestCarriedLegsStatement(t *testing.T) {
	for _, row := range []struct {
		legs []string
		want string
	}{
		{nil, "No leg of this request was sent."},
		{[]string{"crd-order-select"}, "The crd-order-select leg this request sent was carried and stands."},
		{[]string{"crd-order-select", "dtr-questionnaire-fetch"}, "The crd-order-select and dtr-questionnaire-fetch legs this request sent were carried and stand."},
		{[]string{"crd-order-select", "dtr-questionnaire-fetch", "pas-claim"}, "The crd-order-select, dtr-questionnaire-fetch and pas-claim legs this request sent were carried and stand."},
	} {
		if got := carriedStatement(row.legs...); got != row.want {
			t.Errorf("%v: %q, want %q", row.legs, got, row.want)
		}
		// Said once: a message that already says it is not said again.
		ctx := context.WithValue(context.Background(), legsCarriedKey{}, &legsCarried{legs: row.legs})
		once := withCarriedLegs(ctx, "refused")
		if twice := withCarriedLegs(ctx, once); twice != once || once != "refused. "+row.want {
			t.Errorf("%v: %q then %q, want %q once", row.legs, once, twice, "refused. "+row.want)
		}
	}
	// Unknown is not empty: a call with no tracker cannot say which legs it
	// sent, and never says that none were.
	if got := carriedLegsStatement(context.Background()); got != "Any leg this request sent before this point was carried and stands." {
		t.Errorf("no tracker: %q", got)
	}
}

// Every exchange route carries a tracker of its own, which a leg answered 2xx
// adds to and nothing else does.
func TestTracksLegsCarried_OnePerCall(t *testing.T) {
	var seen []*legsCarried
	h := tracksLegsCarried(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Context().Value(legsCarriedKey{}).(*legsCarried)
		seen = append(seen, c)
		carriedLeg(r.Context(), "crd-order-select")
	})
	for i := 0; i < 2; i++ {
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	}
	if len(seen) != 2 || seen[0] == nil || seen[0] == seen[1] || len(seen[0].legs) != 1 || len(seen[1].legs) != 1 {
		t.Fatalf("trackers %+v, want one per call holding its own leg", seen)
	}
	carriedLeg(context.Background(), "pas-claim") // no tracker: records nothing, never panics
}

// An answer the gateway writes itself says which legs were carried once a leg
// was, once, and is left as it is before any leg and on a call with no
// tracker (a route that builds no legs).
func TestAfterLegs(t *testing.T) {
	tracked := func(legs ...string) context.Context {
		return context.WithValue(context.Background(), legsCarriedKey{}, &legsCarried{legs: legs})
	}
	if got := afterLegs(tracked(), "bad request"); got != "bad request" {
		t.Errorf("before any leg: %q, want the answer as it is", got)
	}
	ctx := tracked("crd-order-select", "dtr-questionnaire-fetch")
	once := afterLegs(ctx, "expected pended response")
	if want := "expected pended response. " + carriedStatement("crd-order-select", "dtr-questionnaire-fetch"); once != want {
		t.Errorf("after legs: %q, want %q", once, want)
	}
	if twice := afterLegs(ctx, once); twice != once {
		t.Errorf("said twice: %q", twice)
	}
	if got := afterLegs(context.Background(), "refused"); got != "refused" {
		t.Errorf("no tracker: %q, want the answer as it is", got)
	}
}
