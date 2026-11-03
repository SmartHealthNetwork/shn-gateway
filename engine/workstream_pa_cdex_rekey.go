//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextask"
)

// cdexTaskReturnRekey returns a payer's attachment-request Task as this
// provider's gateway carries it to its own system's Task endpoint: its one
// payer-url input re-keyed to this gateway's documentation return path for
// payer, the authenticated payer that sent the Task (E-11, D127), so the
// documentation the provider sends there reaches that payer through the
// network. The value is rebuilt from the ingress base and payer alone, never
// read from the Task. It is made even where a signature covers the payer-url,
// and the forward discloses it (EditedHeader). It is made only for a payer
// that takes the documentation submission (payerTakesDocumentation), since the
// Hub refuses that leg to a payer that does not.
//
// A Task with no payer-url is carried exactly. One whose payer-url cannot be
// re-keyed is carried as sent and the exchange records why
// (ExchangeRecord.EditsNotMade): a payer-url that is not a non-empty valueUrl,
// several payer-url inputs or an input that cannot be read (so which address
// to re-key is unknown, and none is re-keyed), a body that cannot be read as
// one Task, or a payer not known to take the submission. The error is a
// fidelity refusal of the edit: errNoReturnPath when there is no return path
// to write, otherwise the edit failing its own verification.
//
// The leg's carry to the provider's own Task endpoint is its only caller
// (handleCDexRequestInbound), after the Task is read and refused on the
// network's primitives: a body it cannot read as one Task never reaches it
// there, so EditNotMadeTaskUnreadable is recorded only for a caller that has
// not read it first.
func (g *Gateway) cdexTaskReturnRekey(ctx context.Context, task []byte, contentType, payer string) (relay.Payload, error) {
	body := relay.NewBody(task, relay.OriginPeerFrame)
	t, err := cdextask.Read(body)
	if err != nil {
		exchangeOf(ctx).editNotMade(EditNotMadeTaskUnreadable)
		return relay.Exact(body, contentType), nil
	}
	switch t.PayerURL.State {
	case cdextask.Absent:
		return relay.Exact(body, contentType), nil
	case cdextask.Unreadable:
		if t.PayerURL.Index >= 0 {
			exchangeOf(ctx).editNotMade(EditNotMadeTaskNoStructuralPayerURL)
		} else {
			exchangeOf(ctx).editNotMade(EditNotMadeTaskUnreadablePayerURL)
		}
		return relay.Exact(body, contentType), nil
	}
	if !g.payerTakesDocumentation(payer) {
		exchangeOf(ctx).editNotMade(EditNotMadeTaskPayerDoesNotTake)
		return relay.Exact(body, contentType), nil
	}
	ret, err := cdexroute.ReturnBase(g.cfg.IngressBaseURL, payer)
	if err != nil {
		return noRekeyedAnswer, fmt.Errorf("%w: %v", errNoReturnPath, err)
	}
	doc, err := relay.Doc(body)
	if err != nil {
		return noRekeyedAnswer, err
	}
	target, ok := taskInputValueURL(doc, t.PayerURL.Index)
	if !ok {
		return noRekeyedAnswer, fmt.Errorf("the Task's payer-url input %d has no valueUrl to re-key", t.PayerURL.Index)
	}
	var v bytes.Buffer
	enc := json.NewEncoder(&v)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ret); err != nil {
		return noRekeyedAnswer, err
	}
	return relay.Apply(body, contentType, relay.EditCDexTaskReturnRekey, doc.Replace(target, bytes.TrimSpace(v.Bytes())))
}

// taskInputValueURL is the valueUrl string of input i of the Task at doc's
// root, decided by structure.
func taskInputValueURL(doc *relay.Document, i int) (relay.NodeID, bool) {
	inputs, ok := doc.Member(doc.Root(), "input")
	if !ok || doc.Kind(inputs) != relay.KindArray {
		return 0, false
	}
	elems := doc.Elems(inputs)
	if i < 0 || i >= len(elems) || doc.Kind(elems[i]) != relay.KindObject {
		return 0, false
	}
	v, ok := doc.Member(elems[i], "valueUrl")
	if !ok || doc.Kind(v) != relay.KindString {
		return 0, false
	}
	return v, true
}
