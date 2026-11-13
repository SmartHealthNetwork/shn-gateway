package engine

import (
	"context"
	"encoding/json"
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
// carried: a refusal at a later leg never reads as one before any leg. Its
// text is the error member, or on a FHIR operation route its OperationOutcome's
// first diagnostics. A refusal after a carried leg states delivered yes in
// its body (the problem type, or the issue's coding): that leg's recipient
// answered and may have acted (deliveryMark).
func wantCarried(t *testing.T, body string, legs ...string) {
	t.Helper()
	var doc struct {
		Error string `json:"error"`
		Type  string `json:"type"`
		Issue []struct {
			Diagnostics string `json:"diagnostics"`
			Details     *struct {
				Coding []struct{ System, Code string } `json:"coding"`
			} `json:"details"`
		} `json:"issue"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("answer %s is not JSON: %v", body, err)
	}
	text, yes := doc.Error, doc.Type == deliveredSystem+"/yes"
	if len(doc.Issue) > 0 {
		is := doc.Issue[0]
		text = is.Diagnostics
		yes = is.Details != nil && len(is.Details.Coding) == 1 && is.Details.Coding[0].System == deliveredSystem && is.Details.Coding[0].Code == "yes"
	}
	wantCarriedText(t, text, legs...)
	if len(legs) > 0 && !yes {
		t.Fatalf("answer %s after the %v legs were carried does not state delivered yes", body, legs)
	}
	if len(legs) == 0 && (doc.Type != "" || len(doc.Issue) > 0 && doc.Issue[0].Details != nil) {
		t.Fatalf("answer %s before any leg was sent states delivery", body)
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

// mounted is w and r as routes() and recordExchange give every requester
// call: the delivery mark, on the writer and the context, so a handler driven
// straight states delivery as the mounted route does (deliveryMark). A
// harness without it would be more permissive than the real route.
func mounted(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request) {
	return withDeliveryMark(w, r)
}

// wantCarriedText fails unless text, a refusal's own message, ends saying
// that exactly legs were carried, once.
func wantCarriedText(t *testing.T, text string, legs ...string) {
	t.Helper()
	if want := carriedStatement(legs...); !strings.HasSuffix(text, want) {
		t.Fatalf("message %q does not end %q", text, want)
	}
	if n := strings.Count(text, "carried and stand") + strings.Count(text, "No leg of this request was sent"); n != 1 {
		t.Fatalf("message %q says which legs were carried %d times, want once", text, n)
	}
}
