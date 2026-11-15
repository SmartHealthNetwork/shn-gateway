package subscription

import (
	"net/http"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// The interactions a subscribe leg never carries are refused by their own
// rule, from the method and path alone; every other request is left to the
// parser (nil here), the leg's own interactions and the values only the
// parser refuses alike.
func TestCheckInteraction(t *testing.T) {
	for _, c := range []struct {
		request   string
		status    int
		primitive string
	}{
		{"PATCH Subscription/sub-1", http.StatusMethodNotAllowed, recording.RefusalFidelity},
		{"PATCH Subscription", http.StatusMethodNotAllowed, recording.RefusalFidelity},
		{"PATCH Subscription/sub-1?x=1", http.StatusMethodNotAllowed, recording.RefusalFidelity},
		{"GET Subscription/sub-1/_history", http.StatusForbidden, recording.RefusalAuthority},
		{"GET Subscription/sub-1/_history?_count=1", http.StatusForbidden, recording.RefusalAuthority},
		{"GET Subscription/$status", http.StatusForbidden, recording.RefusalAuthority},
		{"GET Subscription/$status?x=1", http.StatusForbidden, recording.RefusalAuthority},
	} {
		wantRefusal(t, c.request, CheckInteraction(c.request), c.status, c.primitive)
	}
	for _, request := range []string{
		"POST Subscription",
		"GET Subscription/sub-1",
		"GET Subscription/sub-1/_history/2",
		"PUT Subscription/sub-1",
		"DELETE Subscription/sub-1",
		"GET Subscription/sub-1/$status",
		"GET Subscription?url=x",
		"GET ?_getpages=a",
		"GET Subscription/_history",
		"GET Task/t1/_history",
		"",
	} {
		if r := CheckInteraction(request); r != nil {
			t.Errorf("%q: refused %+v, want it left to the parser", request, r)
		}
	}
}
