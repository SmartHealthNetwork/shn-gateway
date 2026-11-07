//go:build !shn_coreonly

package engine

import (
	"context"
	"slices"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/dtraddress"
)

// rekeyContext is what the re-key (E-12) of one questionnaire-fetch answer
// reads, all of it this gateway's own: the payer the answer came from, the
// answer's line, what the payer's gateway stated in the sealed answer frame,
// which operation it answers, the address the round was called at (a
// next-question round through an issued address), and whether the package
// request carried a Questionnaire.
type rekeyContext struct {
	payer, line                 string
	stated                      statedAnswer
	nextQuestion                bool
	round                       *dtraddress.Subpath
	requestCarriedQuestionnaire bool
}

// dtrAdaptiveRekey returns the re-key (E-12) of the questionnaire-fetch answer
// d as the provider's gateway relays it to its own DTR client: for each
// questionnaireAdaptive address on any Questionnaire in it that the payer's
// gateway stated and tagged, the payer's base is replaced by this gateway's
// per-payer address and the remainder kept byte for byte; a true is made
// explicit as the address of the payer's own DTR endpoint. Each address it
// cannot re-key is carried as sent, and the reason recorded on ctx's exchange.
// Nothing is re-keyed toward a payer whose gateway does not declare v1opsub,
// since only such a gateway restores the address (E-13).
func (g *Gateway) dtrAdaptiveRekey(ctx context.Context, d *relay.Document, rc rekeyContext) []relay.Change {
	exts := adaptiveExtensions(d)
	if len(exts) == 0 {
		return nil
	}
	notMade := func(r string) { exchangeOf(ctx).editNotMade(r) }
	// A false is never re-keyed, so an answer holding only false addresses
	// has nothing the re-key could make, and nothing is recorded.
	if !slices.ContainsFunc(exts, func(e adaptiveExtension) bool {
		v, ok := d.Member(e.node, "valueBoolean")
		var isTrue bool
		return !ok || d.Kind(v) != relay.KindBool || d.DecodeValue(v, &isTrue) != nil || isTrue
	}) {
		return nil
	}
	if entry, ok := g.cfg.Reg.Lookup(rc.payer); !ok || !shnsdk.SupportsRequestFrameV1OpSub(entry.RequestFrames) {
		notMade(EditNotMadeRekeyPayerGatewayOlder)
		return nil
	}
	base := rc.stated.address[shnsdk.FrameHeaderOperationBase]
	tags := map[string]string{}
	if v := rc.stated.address[shnsdk.FrameHeaderOperationAddressTags]; v != "" {
		pairs, err := dtraddress.ParseTags(v)
		if err != nil {
			// No address can be matched to a tag: the reason is the tags,
			// recorded once, and every address is carried as sent.
			notMade(EditNotMadeRekeyUnreadableTag)
			return nil
		}
		for _, p := range pairs {
			tags[p.Remainder] = p.Tag
		}
	}
	parent := documentParents(d)
	// address reads a valueUrl as an address under the stated base the re-key
	// can give back byte for byte, its remainder, or the reason it cannot.
	address := func(v relay.NodeID) (string, string) {
		if d.Kind(v) != relay.KindString {
			return "", EditNotMadeRekeyUnreadableAddress
		}
		value, err := d.StringValue(v)
		if err != nil || !strings.Contains(value, "://") {
			return "", EditNotMadeRekeyUnreadableAddress
		}
		if base == "" {
			return "", EditNotMadeRekeyNotUnderBase
		}
		if _, under := dtraddress.Under(value, base); !under {
			return "", EditNotMadeRekeyNotUnderBase
		}
		if s, e := d.Span(v); e-s != len(value)+2 {
			return "", EditNotMadeRekeyEscapedAddress
		}
		if !strings.HasPrefix(value, base) {
			return "", EditNotMadeRekeyBaseSpelledDifferently
		}
		remainder := value[len(base):]
		if !dtraddress.CleanRemainder(remainder) {
			return "", EditNotMadeRekeyUncleanRemainder
		}
		return remainder, ""
	}
	// The addresses tagged on a top-level Questionnaire entry: a contained copy
	// of one of them reuses its tag.
	taggedTopLevel := map[string]bool{}
	for _, ext := range exts {
		if v, ok := d.Member(ext.node, "valueUrl"); ok && topLevelEntry(d, parent, ext.questionnaire) {
			if r, why := address(v); why == "" && tags[r] != "" {
				taggedTopLevel[r] = true
			}
		}
	}
	var ops []relay.Op
	for _, ext := range exts {
		if v, ok := d.Member(ext.node, "valueBoolean"); ok {
			var isTrue bool
			if d.Kind(v) != relay.KindBool || d.DecodeValue(v, &isTrue) != nil {
				notMade(EditNotMadeRekeyUnreadableAddress)
				continue
			}
			if !isTrue {
				continue
			}
			tag, ok := tags[dtraddress.BoolRemainder]
			if !ok {
				notMade(EditNotMadeRekeyNotUnderBase)
				continue
			}
			u, err := dtraddress.IssuedURL(g.cfg.IngressBaseURL, dtraddress.FamilyFetchServer, rc.payer, rc.line, tag, "")
			if err != nil {
				notMade(EditNotMadeRekeyAddressNotWritable)
				continue
			}
			op, err := d.AdaptiveForm(ext.node, u)
			if err != nil {
				notMade(EditNotMadeRekeyUnreadableAddress)
				continue
			}
			ops = append(ops, op)
			continue
		}
		v, ok := d.Member(ext.node, "valueUrl")
		if !ok {
			notMade(EditNotMadeRekeyUnreadableAddress)
			continue
		}
		remainder, why := address(v)
		if why != "" {
			notMade(why)
			continue
		}
		if rc.nextQuestion {
			own := rc.round != nil && rc.round.Family == dtraddress.FamilyPayer && remainder == rc.round.Remainder
			if remainder != "" && !own {
				notMade(EditNotMadeRekeyNotTheRoundsAddress)
				continue
			}
		} else {
			if rc.requestCarriedQuestionnaire {
				notMade(EditNotMadeRekeyRequestCarriedAQuestionnaire)
				continue
			}
			if !topLevelEntry(d, parent, ext.questionnaire) && !taggedTopLevel[remainder] {
				notMade(EditNotMadeRekeyContainedInPackageResponse)
				continue
			}
		}
		tag, ok := tags[remainder]
		if !ok {
			notMade(EditNotMadeRekeyNotUnderBase)
			continue
		}
		u, err := dtraddress.IssuedURL(g.cfg.IngressBaseURL, dtraddress.FamilyPayer, rc.payer, rc.line, tag, remainder)
		if err != nil {
			notMade(EditNotMadeRekeyAddressNotWritable)
			continue
		}
		quoted, err := relay.JSONString(u)
		if err != nil {
			notMade(EditNotMadeRekeyAddressNotWritable)
			continue
		}
		ops = append(ops, d.Replace(v, quoted))
	}
	if len(ops) == 0 {
		return nil
	}
	return []relay.Change{{Edit: relay.EditDTRAdaptiveAddressRekey, Ops: ops}}
}

// rekeyAnswer is answer with the re-key (E-12) made, or answer as it was:
// the answer is empty (a 204, or a 2xx with no body, which carries nothing to
// re-key), the answer cannot be read as JSON (recorded), nothing is re-keyed,
// or the relay refuses the change (recorded as an unreadable answer: the
// address is carried as sent).
func (g *Gateway) rekeyAnswer(ctx context.Context, answer relay.Payload, rc rekeyContext) relay.Payload {
	if answer.Len() == 0 {
		return answer
	}
	out, err := relay.Then(answer, relay.OriginPeerFrame, func(d *relay.Document) ([]relay.Change, error) {
		return g.dtrAdaptiveRekey(ctx, d, rc), nil
	})
	if err != nil {
		exchangeOf(ctx).editNotMade(EditNotMadeRekeyUnreadableAnswer)
		return answer
	}
	return out
}

// rekeyPackageAnswer is the package answer with the re-key (E-12) made, at
// the line of the token the request was sent with, the address tagged only
// when request (the client's own package request) carried no Questionnaire.
func (g *Gateway) rekeyPackageAnswer(ctx context.Context, answer relay.Payload, payer, token string, stated statedAnswer, request []byte) relay.Payload {
	carried := true
	if d, err := relay.Doc(relay.NewBody(request, relay.OriginIngressRequest)); err == nil {
		carried = carriesQuestionnaire(d)
	}
	return g.rekeyAnswer(ctx, answer, rekeyContext{payer: payer, line: shnsdk.LineOf(token), stated: stated, requestCarriedQuestionnaire: carried})
}
