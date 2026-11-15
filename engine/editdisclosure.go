package engine

import (
	"net/http"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// EditedHeader names, on an answer the gateway writes to its own
// participant's system, each registered edit it made to that answer which the
// network discloses there: the edit's published name (relay.EditName), then,
// when the edit changed content an in-payload signature covers,
// `; signed="<carriers>"`, the signatures by carrier as a SignedContentError
// names them, comma-separated. One value per edit; absent when no disclosed
// edit was made.
const EditedHeader = "X-SHN-Edited"

// headerDisclosedEdits are the registered edits EditedHeader names: the PAS
// pend's and the attachment-request Task's return-path re-keys, and the
// adaptive address's re-key and restore, made even on signed content, so a
// failed signature check on the participant's side has an explanation; and
// the attachment-request Task's Location (E-16), a header the provider's
// system did not write; and a topic Subscription's channel re-key (E-19), made
// even on signed content, and its url search re-key (E-21), each a change of a
// payer's request the provider's system did not see sent; and on the
// subscriber's answer, the restore of its Subscriptions (E-20), made even on
// signed content, and a create's or update's Location (E-23), a header the
// notifier's system did not write; and on a CDex Task notification the payer's
// gateway delivers, the re-key of its addresses into the provider's server
// (E-17), made even on signed content.
var headerDisclosedEdits = []relay.EditID{relay.EditPASPendReturnRekey, relay.EditCDexTaskReturnRekey, relay.EditDTRAdaptiveAddressRekey, relay.EditDTRAdaptiveAddressRestore, relay.EditCDexTaskLocationRekey,
	relay.EditSubscriptionEndpointRekey, relay.EditSubscriptionURLSearchRekey, relay.EditSubscriptionEndpointRestore, relay.EditSubscriptionLocationRekey,
	relay.EditCDexTaskNotificationAddressRekey}

// setEditedHeader adds EditedHeader for each disclosed edit p carries, in the
// order headerDisclosedEdits lists them, whatever order they were made in. The
// signatures p names (SignedEdited) are its body edits': a header edit
// changes no byte of the body, so it names none.
func setEditedHeader(h http.Header, p relay.Payload) {
	made := p.Edits()
	for _, id := range headerDisclosedEdits {
		if !slices.Contains(made, id) {
			continue
		}
		name, ok := relay.EditName(relayEdits, id)
		if !ok {
			name = string(id)
		}
		if signed := p.SignedEdited(); len(signed) > 0 && !headerEdit(id) {
			name += `; signed="` + strings.Join(signed, ", ") + `"`
		}
		h.Add(EditedHeader, name)
	}
}

// headerEdit reports whether the registry's row for id is a header edit
// (relay.EditKindSetHeader).
func headerEdit(id relay.EditID) bool {
	return slices.ContainsFunc(relayEdits, func(e relay.Edit) bool { return e.ID == id && e.Kind == relay.EditKindSetHeader })
}
