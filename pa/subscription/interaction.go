package subscription

import (
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// CheckInteraction is the rule on the interactions a subscribe leg never carries, read from
// the request's method
// and path alone, before the request is parsed as one it carries
// (shnsdk.ParseSubscriptionRequest), at either gateway, a network rule at
// every level. request is "<METHOD> <path>[?<query>]", the path below the
// base:
//   - a PATCH, whatever its path, is refused 405, recorded the fidelity of
//     SHN's own edit: the network re-keys a Subscription's channel only in a
//     whole one, a create's or an update's, and cannot make E-19 on a patch;
//   - an instance's history ("Subscription/<id>/_history") and the type-level
//     $status ("Subscription/$status") are refused 403, recorded authority:
//     the leg's authority covers a Subscription's own interactions, and
//     neither is one (a history would carry every version's channel, a
//     type-level $status every Subscription's status).
//
// Anything else is nil here: the caller parses it, and refuses what the
// parser does not admit as routing.
func CheckInteraction(request string) *Refusal {
	method, target, _ := strings.Cut(request, " ")
	path, _, _ := strings.Cut(target, "?")
	segs := strings.Split(path, "/")
	switch {
	case method == http.MethodPatch:
		return refuse(http.StatusMethodNotAllowed, recording.RefusalFidelity, "a PATCH is not carried: the network re-keys a Subscription's channel only in a whole Subscription, a create's or an update's")
	case len(segs) == 3 && segs[0] == "Subscription" && segs[2] == "_history":
		return forbidden("an instance's history is not carried: it is outside the leg's authority, which covers a Subscription's own interactions")
	case len(segs) == 2 && segs[0] == "Subscription" && segs[1] == "$status":
		return forbidden("a type-level $status is not carried: it is outside the leg's authority, which covers a Subscription's own interactions")
	}
	return nil
}
