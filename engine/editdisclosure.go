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
// failed signature check on the participant's side has an explanation.
var headerDisclosedEdits = []relay.EditID{relay.EditPASPendReturnRekey, relay.EditCDexTaskReturnRekey, relay.EditDTRAdaptiveAddressRekey, relay.EditDTRAdaptiveAddressRestore}

// setEditedHeader adds EditedHeader for each disclosed edit p carries.
func setEditedHeader(h http.Header, p relay.Payload) {
	for _, id := range p.Edits() {
		if !slices.Contains(headerDisclosedEdits, id) {
			continue
		}
		name, ok := relay.EditName(relayEdits, id)
		if !ok {
			name = string(id)
		}
		if signed := p.SignedEdited(); len(signed) > 0 {
			name += `; signed="` + strings.Join(signed, ", ") + `"`
		}
		h.Add(EditedHeader, name)
	}
}
