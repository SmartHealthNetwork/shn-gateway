//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
)

// errNoReturnPath: the gateway has no network return path to write, so a
// return-path re-key (E-10, E-11) cannot be made. A released gateway never reaches
// it: one serving the ingress refuses to start without a base a return path
// can be built under (the app's PROVIDER_DAVINCI_INGRESS_BASE_URL check, and
// New's checkIngressBase, which accepts exactly the bases cdexroute.ReturnBase
// builds under). Only a test build's ingress bypass, EnableIngressForTest with
// IngressBaseURL cleared, serves the ingress without one.
var errNoReturnPath = errors.New("no documentation return path")

// errNotifiedUnreadable is E-22 unable to read a PAS content notification
// one way only (relay.Doc), so it cannot be made.
var errNotifiedUnreadable = errors.New("engine: E-22 cannot read the notification one way only")

// readNotified is how both gateways read a PAS content notification for
// E-22 before it is sent or delivered: notificationUnreadable when relay
// cannot read it one way only (such as a lone surrogate in a string), which
// each refuses on the fidelity of SHN's own edit, as E-17's reader
// (readNotification) is held.
func readNotified(body []byte, origin relay.Origin, _ string) notificationRead {
	if _, err := relay.Doc(relay.NewBody(body, origin)); err != nil {
		return notificationUnreadable
	}
	return notificationPlacesRead
}

// noRekeyedAnswer is the payload a return-path re-key (pendReturnRekey,
// cdexTaskReturnRekey) or the created Task's Location (cdexTaskLocated)
// returns with an error.
var noRekeyedAnswer relay.Payload

// pasTempCodesSystem is the PAS code system that types a Task's payer-url input.
const pasTempCodesSystem = "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes"

// pendReturnRekey returns a payer's PAS answer as this gateway relays it to
// its own participant's system: each PAS Task's payer-url input re-keyed to
// this gateway's documentation return path for payer, the participant the leg
// was routed to and answered (E-10), so the documentation the provider sends
// there reaches that payer through the network. The value is rebuilt from the
// ingress base and payer alone, never read from the answer. It is made even
// where a signature covers the payer-url, and the forward discloses it
// (EditedHeader). It is made only for a payer that takes the leg
// (payerTakesDocumentation): an edit is justified by carrying the message, and
// the Hub refuses the leg to a payer that does not take it. An answer with no payer-url
// to re-key is relayed exactly; a payer-url that is not a string valueUrl, an
// answer that cannot be read, or a payer not known to take the leg, is carried
// as sent and recorded (ExchangeRecord.EditsNotMade). The error is
// a fidelity refusal of the edit: errNoReturnPath when there is no return
// path to write, otherwise the edit failing its own verification.
func (g *Gateway) pendReturnRekey(ctx context.Context, answer []byte, contentType, payer string) (relay.Payload, error) {
	body := relay.NewBody(answer, relay.OriginPeerFrame)
	doc, err := relay.Doc(body)
	if err != nil {
		exchangeOf(ctx).editNotMade(EditNotMadeUnreadableAnswer)
		return relay.Exact(body, contentType), nil
	}
	targets, nonstructural := payerURLInputs(doc)
	if nonstructural {
		exchangeOf(ctx).editNotMade(EditNotMadeNoStructuralPayerURL)
	}
	if len(targets) == 0 {
		return relay.Exact(body, contentType), nil
	}
	if !g.payerTakesDocumentation(payer) {
		exchangeOf(ctx).editNotMade(EditNotMadePayerDoesNotTake)
		return relay.Exact(body, contentType), nil
	}
	ret, err := cdexroute.ReturnBase(g.cfg.IngressBaseURL, payer)
	if err != nil {
		return noRekeyedAnswer, fmt.Errorf("%w: %v", errNoReturnPath, err)
	}
	var v bytes.Buffer
	enc := json.NewEncoder(&v)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ret); err != nil {
		return noRekeyedAnswer, err
	}
	value := bytes.TrimSpace(v.Bytes())
	ops := make([]relay.Op, 0, len(targets))
	for _, n := range targets {
		ops = append(ops, doc.Replace(n, value))
	}
	return relay.Apply(body, contentType, relay.EditPASPendReturnRekey, ops...)
}

// payerTakesDocumentation reports whether the registry's entry for payer lists
// the documentation submission. A payer with no list predates the leg and the
// Hub refuses it, and a payer this gateway cannot read is unknown, never taken
// to take it.
func (g *Gateway) payerTakesDocumentation(payer string) bool {
	entry, ok := g.cfg.Reg.Lookup(payer)
	return ok && slices.Contains(entry.Legs, legCDexSubmit)
}

// payerURLInputs finds, by structure, the payer-url inputs of every Task in
// doc (an object whose resourceType is Task, an element of its input array
// typed PASTempCodes#payer-url): targets are their string valueUrl values;
// nonstructural is true when one carries no string valueUrl.
func payerURLInputs(doc *relay.Document) (targets []relay.NodeID, nonstructural bool) {
	str := func(obj relay.NodeID, key string) string {
		v, ok := doc.Member(obj, key)
		if !ok || doc.Kind(v) != relay.KindString {
			return ""
		}
		s, _ := doc.StringValue(v)
		return s
	}
	for i := 0; i < doc.Len(); i++ {
		task := relay.NodeID(i)
		if doc.Kind(task) != relay.KindObject || str(task, "resourceType") != "Task" {
			continue
		}
		inputs, ok := doc.Member(task, "input")
		if !ok || doc.Kind(inputs) != relay.KindArray {
			continue
		}
		for _, in := range doc.Elems(inputs) {
			if doc.Kind(in) != relay.KindObject || !typedPayerURL(doc, in, str) {
				continue
			}
			if v, ok := doc.Member(in, "valueUrl"); ok && doc.Kind(v) == relay.KindString {
				targets = append(targets, v)
				continue
			}
			nonstructural = true
		}
	}
	return targets, nonstructural
}

// typedPayerURL reports whether the input in is typed PASTempCodes#payer-url.
func typedPayerURL(doc *relay.Document, in relay.NodeID, str func(relay.NodeID, string) string) bool {
	typ, ok := doc.Member(in, "type")
	if !ok || doc.Kind(typ) != relay.KindObject {
		return false
	}
	codings, ok := doc.Member(typ, "coding")
	if !ok || doc.Kind(codings) != relay.KindArray {
		return false
	}
	for _, c := range doc.Elems(codings) {
		if doc.Kind(c) == relay.KindObject && str(c, "system") == pasTempCodesSystem && str(c, "code") == "payer-url" {
			return true
		}
	}
	return false
}

// refusePendRekey writes the fidelity refusal of a re-key pendReturnRekey
// could not make: the payer answered, and its answer is not relayed with its
// return address off the network.
func refusePendRekey(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNoReturnPath) {
		exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageBuild)
	} else {
		exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageVerify)
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": withPayerAnsweredNote(
		"the gateway cannot re-key the answer's documentation return address to the network (fidelity of SHN's own edit): " + err.Error())})
}

// pasNotifiedReturnRekey is E-22 (pas-notified-return-rekey; the
// notifications design §6.4, D127 decision 10): E-10's change, made on a
// payer's PAS content notification the provider's gateway delivers to its own
// system. Each PAS Task's payer-url input in the notified decision, found by
// structure (payerURLInputs), is re-keyed to this gateway's documentation
// return path for the payer the Hub authenticated as the notifier, rebuilt
// from the ingress base and that payer alone, never read from the
// notification. It is made only for a payer that takes the documentation
// submission, and even where a signature covers the payer-url (the delivery
// discloses it). A payer-url that is not a string valueUrl, or a payer not
// known to take the submission, is carried as sent and recorded. A
// notification relay cannot read one way only is an edit this gateway
// cannot make, as E-17's is: whether it carries a payer-url is unknown, and
// unknown is not empty, so it is never delivered with the payer's address in
// it. err is errNotifiedUnreadable then (the delivery refuses such a
// notification first, pasContentDelivery's rekey check, so err is its last
// guard), and errNoReturnPath when there is no return path to write.
func (g *Gateway) pasNotifiedReturnRekey(x *exchangeRecorder, doc *relay.Document, in deliveryInput) ([]relay.Op, error) {
	if doc == nil {
		return nil, errNotifiedUnreadable
	}
	targets, nonstructural := payerURLInputs(doc)
	if nonstructural {
		x.editNotMade(EditNotMadeNotifiedNoStructuralPayerURL)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	if !g.payerTakesDocumentation(in.notifier) {
		x.editNotMade(EditNotMadeNotifiedPayerDoesNotTake)
		return nil, nil
	}
	ret, err := cdexroute.ReturnBase(g.cfg.IngressBaseURL, in.notifier)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoReturnPath, err)
	}
	var v bytes.Buffer
	enc := json.NewEncoder(&v)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ret); err != nil {
		return nil, err
	}
	value := bytes.TrimSpace(v.Bytes())
	ops := make([]relay.Op, 0, len(targets))
	for _, n := range targets {
		ops = append(ops, doc.Replace(n, value))
	}
	return ops, nil
}
