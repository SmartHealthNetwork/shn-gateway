//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	"github.com/SmartHealthNetwork/shn-gateway/pa/dtraddress"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The created Task's Location (E-16, cdex-task-location-rekey): a payer's
// system that creates an attachment-request Task is answered with a Location
// naming the Task at its own gateway's base for the provider the Task went to,
// never at the provider's own server. The payer's system reads the Task at that
// address through the same gateway (cdex-task-status) when that provider's
// gateway takes the read.
//
// It is decided at the provider's gateway, which frames the tail of its
// system's Location (taskLocation: "Task/{id}" or "Task/{id}/_history/{vid}")
// when that Location lies under its own Task endpoint, and declares that it
// does (the cdexloc request frame); and made at the payer's gateway, which
// writes its base for that provider, then the tail. Either side that cannot
// carry its half records why, and the 2xx is carried without a Location.

// cdexTaskLocationHeader is the header E-16 sets on the answer to the payer's
// system.
const cdexTaskLocationHeader = "Location"

// cdexTaskLocationFrame is the answer-frame header the provider's gateway
// states on its system's 2xx answer to a payer's Task (LegResult.AddressHeaders):
// the tail of the system's Location after taskURL, the gateway's own Task
// endpoint (PROVIDER_CDEX_TASK_URL), as "Task" then that remainder. A
// relative Location is resolved against the URL the answer came from
// (upstreamReply.answeredAt), as HTTP defines; the result must lie under
// taskURL by the segment-boundary match E-12 uses (dtraddress.Under), never a
// string prefix, and its remainder must read as a Task id with an optional
// version (shnsdk.ParseCDexTaskLocation). Otherwise nothing is framed, and the
// reason is recorded on ctx's exchange: no Location at all, or one that is
// not such an address (several Locations among them, as none can be told to be
// the Task's).
func cdexTaskLocationFrame(ctx context.Context, up upstreamReply, taskURL string) map[string]string {
	notMade := func(why string) map[string]string {
		exchangeOf(ctx).editNotMade(why)
		return nil
	}
	switch {
	case len(up.locations) == 0 || (len(up.locations) == 1 && strings.TrimSpace(up.locations[0]) == ""):
		return notMade(EditNotMadeTaskLocationNoLocation)
	case len(up.locations) > 1 || up.answeredAt == nil:
		return notMade(EditNotMadeTaskLocationNotUnderBase)
	}
	ref, err := url.Parse(up.locations[0])
	if err != nil {
		return notMade(EditNotMadeTaskLocationNotUnderBase)
	}
	remainder, ok := dtraddress.Under(up.answeredAt.ResolveReference(ref).String(), taskURL)
	if !ok {
		return notMade(EditNotMadeTaskLocationNotUnderBase)
	}
	tail := "Task" + remainder
	if _, err := shnsdk.ParseCDexTaskLocation(tail); err != nil {
		return notMade(EditNotMadeTaskLocationNotUnderBase)
	}
	return map[string]string{shnsdk.FrameHeaderTaskLocation: tail}
}

// errNoProviderBase: the payer's gateway has no base for the provider to write
// a Location under, so E-16 cannot be made: its ingress base is not one a base
// can be built under, or the provider's id cannot be placed in a path (a dot
// segment, a space or a control character), a provider /cdex/providers does
// not list either (cdexroute.ProviderBase).
var errNoProviderBase = errors.New("no base for the provider")

// cdexTaskLocated returns the provider's 2xx answer p, relayed exactly, as
// the payer's gateway writes it to its own system: with a Location naming the
// Task at this gateway's base for provider, the provider the Task was routed
// to, then the tail the provider's gateway framed (E-16), when that gateway
// declares cdexloc and framed a tail that reads as a Task location. Otherwise
// p is returned as it is and the reason recorded on ctx's exchange. The
// error is a fidelity refusal of the edit: errNoProviderBase when there is no
// base to write, otherwise the edit failing its own verification.
func (g *Gateway) cdexTaskLocated(ctx context.Context, p relay.Payload, stated statedAnswer, provider string) (relay.Payload, error) {
	notMade := func(why string) (relay.Payload, error) {
		exchangeOf(ctx).editNotMade(why)
		return p, nil
	}
	if entry, ok := g.cfg.Reg.Lookup(provider); !ok || !shnsdk.SupportsRequestFrameCDexLoc(entry.RequestFrames) {
		return notMade(EditNotMadeTaskLocationProviderGatewayOlder)
	}
	tail := stated.address[shnsdk.FrameHeaderTaskLocation]
	if tail == "" {
		return notMade(EditNotMadeTaskLocationNoTail)
	}
	loc, err := shnsdk.ParseCDexTaskLocation(tail)
	if err != nil {
		return notMade(EditNotMadeTaskLocationUnreadableTail)
	}
	base, err := cdexroute.ProviderBase(g.cfg.IngressBaseURL, provider)
	if err != nil {
		return noRekeyedAnswer, fmt.Errorf("%w: %v", errNoProviderBase, err)
	}
	// The core checks the value against the base and the Task location it is
	// made from (relay.HeaderEdited); which base, the provider's, is decided
	// here.
	return headerEdited(p, relay.EditCDexTaskLocationRekey, cdexTaskLocationHeader, cdexTaskLocationValue(base, loc),
		relay.HeaderInputs{relay.TaskLocationInputBase: base, relay.TaskLocationInputTail: loc.String()})
}

// cdexTaskLocationValue is the Location E-16 writes: the provider's base, then
// the Task location. The engine's tests replace it to make a value the core's
// check of the edit refuses (the refusal at stage verify).
var cdexTaskLocationValue = func(base string, loc shnsdk.CDexTaskPath) string {
	return base + "/" + loc.String()
}

// refuseCDexTaskLocation writes the fidelity refusal of a Location
// cdexTaskLocated could not make: no base to write it under is the build of
// the edit failing; any other failure is its verification. The provider's
// system has created the Task, so the refusal says to check before resending.
func refuseCDexTaskLocation(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNoProviderBase) {
		exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageBuild)
	} else {
		exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageVerify)
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the gateway cannot write the Task's Location at its network address " +
		"(fidelity of SHN's own edit): " + err.Error() + "; " + noteProviderAnswered})
}

// noteProviderAnswered is the note a refusal of the provider's answer to a
// payer's Task carries: the provider's system received and answered the Task,
// so the payer's system checks for it before creating it again.
const noteProviderAnswered = "the provider's system received and answered this Task: check for it before creating it again"
