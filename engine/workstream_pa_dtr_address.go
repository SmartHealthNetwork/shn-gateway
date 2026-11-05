//go:build !shn_coreonly

package engine

import (
	"context"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/dtraddress"
)

// questionnaireAdaptiveExtension is the SDC extension a Questionnaire names
// its $next-question server in: its valueUrl, or, at DTR 2.0.1, the boolean
// true. The PA workstream's own name for it: module 1 keeps its own.
const questionnaireAdaptiveExtension = "http://hl7.org/fhir/uv/sdc/StructureDefinition/sdc-questionnaire-questionnaireAdaptive"

// errRoundAddressNotIssued refuses a round whose operationSubpath does not
// verify at this payer's gateway: an address the payer never issued to the
// requesting provider, one whose base or line changed, or one issued under a
// key since rotated (D125). The client recovers by fetching the package
// again.
const errRoundAddressNotIssued = "not an address this payer issued, or its base or key changed; fetch the package again"

// dtrAddressBase is a responder that delivers DTR operations to the payer's
// own system and so knows the base it delivers under at each line
// (nativeResponder.dtrBaseAt).
type dtrAddressBase interface {
	dtrBaseAt(line string) (string, bool)
}

// roundAddress is the verified address a round was called at: delivered at
// base, then remainder, then the operation's path.
type roundAddress struct {
	family, line, base, remainder string
}

// url is where the round is delivered: the base and remainder of a payer
// address, or the base of a fetch-server address, then the operation's path.
func (a roundAddress) url(path string) string { return a.base + a.remainder + path }

type roundAddressKey struct{}

// verifiedRoundAddress is the address verifyRoundSubpath verified for this
// request, if any: the forward delivers there and nowhere else.
func verifiedRoundAddress(ctx context.Context) (roundAddress, bool) {
	a, ok := ctx.Value(roundAddressKey{}).(roundAddress)
	return a, ok
}

// verifyRoundSubpath verifies the address a round names (its
// operationSubpath) as one this payer's gateway issued to the requesting
// provider, recomputing its tag over the family, this payer's id, the
// provider's id, the address's line, the base this gateway delivers under at
// that line now, and the remainder. A round that names none is delivered as
// before (ok). One that does not verify is refused 422 as routing, and nothing
// is forwarded.
func (g *Gateway) verifyRoundSubpath(ctx context.Context, requester string) (context.Context, int, string) {
	raw, named := requestFrameSubpath(ctx)
	if !named {
		return ctx, 0, ""
	}
	// The header is defined only for a next-question round; one that is not
	// an address's form is not an address this payer issued.
	if RequestFrameOperation(ctx) != shnsdk.FrameOperationNextQuestion {
		return ctx, http.StatusBadRequest, "operationSubpath is defined only for a next-question round"
	}
	s, err := dtraddress.ParseSubpath(raw)
	if err != nil {
		return ctx, http.StatusUnprocessableEntity, errRoundAddressNotIssued
	}
	st := g.paState()
	resolver, ok := g.cfg.Responder.(dtrAddressBase)
	if st == nil || st.addressKey.IsZero() || !ok {
		return ctx, http.StatusUnprocessableEntity, errRoundAddressNotIssued
	}
	base, ok := resolver.dtrBaseAt(s.Line)
	if !ok {
		return ctx, http.StatusUnprocessableEntity, errRoundAddressNotIssued
	}
	a := dtraddress.Address{Family: s.Family, Payer: g.cfg.HolderID, Provider: requester, Line: s.Line, Base: base, Remainder: s.Remainder}
	if !st.addressKey.Verify(a, s.Tag) {
		return ctx, http.StatusUnprocessableEntity, errRoundAddressNotIssued
	}
	return context.WithValue(ctx, roundAddressKey{}, roundAddress{family: s.Family, line: s.Line, base: base, remainder: s.Remainder}), 0, ""
}

// addressIssuer is what a payer gateway tags the addresses of a DTR answer
// with: its key, its own participant id, and the provider the answer goes to
// (the authenticated requester of the exchange).
type addressIssuer struct {
	key             dtraddress.Key
	payer, provider string
	// carried observes an address in the re-key's shape that names another
	// payer, carried as sent (dtr.adaptive-address-carried).
	carried func()
}

type addressIssuerKey struct{}

func withAddressIssuer(ctx context.Context, a addressIssuer) context.Context {
	return context.WithValue(ctx, addressIssuerKey{}, a)
}

func addressIssuerOf(ctx context.Context) (addressIssuer, bool) {
	a, ok := ctx.Value(addressIssuerKey{}).(addressIssuer)
	return a, ok && !a.key.IsZero()
}

// addressHeaders are the address headers a payer gateway states on a 2xx
// answer to a framed DTR operation (§14.5 of the CDex design): the base the
// operation was delivered under (operationBase), when any questionnaireAdaptive
// valueUrl in the answer is under it however spelled, and the tag of each
// address it issued (operationAddressTags):
//
//   - on a next-question answer, a valueUrl that is the base itself, or the
//     address the round was delivered to;
//   - on a package answer, a valueUrl on a Questionnaire that is a top-level
//     entry of a Bundle, never one contained in another resource, and none at
//     all when the request carried a Questionnaire anywhere;
//   - in either, only a valueUrl spelled exactly as the base followed by a
//     clean remainder and written without JSON escapes;
//   - a valueBoolean true anywhere, as "*" over the fetch-server family.
//
// It returns nil when nothing is stated: no issuer on ctx, no base at the
// answer's line, or an answer it cannot read.
func (n *nativeResponder) addressHeaders(ctx context.Context, request, answer relay.Body) map[string]string {
	issuer, ok := addressIssuerOf(ctx)
	if !ok {
		return nil
	}
	round, delivered := verifiedRoundAddress(ctx)
	line, base := round.line, round.base
	if !delivered {
		line = answerLineOr(ctx, "pa.dtr")
		if base, ok = n.dtrBaseAt(line); !ok {
			return nil
		}
	}
	doc, err := relay.Doc(answer)
	if err != nil {
		return nil
	}
	nextQuestion := RequestFrameOperation(ctx) == shnsdk.FrameOperationNextQuestion
	requestQuestionnaire := false
	if !nextQuestion {
		rd, err := relay.Doc(request)
		if err != nil {
			return nil
		}
		requestQuestionnaire = carriesQuestionnaire(rd)
	}
	tag := func(family, remainder string) string {
		return issuer.key.Tag(dtraddress.Address{Family: family, Payer: issuer.payer, Provider: issuer.provider, Line: line, Base: base, Remainder: remainder})
	}
	var pairs []dtraddress.TagPair
	seen := map[string]bool{}
	add := func(remainder, t string) {
		if !seen[remainder] {
			seen[remainder] = true
			pairs = append(pairs, dtraddress.TagPair{Remainder: remainder, Tag: t})
		}
	}
	stateBase := false
	parent := documentParents(doc)
	for _, ext := range adaptiveExtensions(doc) {
		if v, ok := doc.Member(ext.node, "valueBoolean"); ok {
			var t bool
			if doc.Kind(v) == relay.KindBool && doc.DecodeValue(v, &t) == nil && t {
				add(dtraddress.BoolRemainder, tag(dtraddress.FamilyFetchServer, ""))
			}
			continue
		}
		v, ok := doc.Member(ext.node, "valueUrl")
		if !ok || doc.Kind(v) != relay.KindString {
			continue
		}
		value, err := doc.StringValue(v)
		if err != nil {
			continue
		}
		if _, under := dtraddress.Under(value, base); !under {
			continue
		}
		stateBase = true
		if s, e := doc.Span(v); e-s != len(value)+2 {
			continue // written with a JSON escape: the restore could not give it back byte for byte
		}
		remainder, spelled := dtraddress.Spelled(value, base)
		if !spelled {
			continue
		}
		switch {
		case nextQuestion:
			if remainder != "" && !(delivered && round.family == dtraddress.FamilyPayer && remainder == round.remainder) {
				continue
			}
		case requestQuestionnaire || !topLevelEntry(doc, parent, ext.questionnaire):
			continue
		}
		add(remainder, tag(dtraddress.FamilyPayer, remainder))
	}
	out := map[string]string{}
	if stateBase {
		out[shnsdk.FrameHeaderOperationBase] = base
	}
	if len(pairs) > 0 {
		out[shnsdk.FrameHeaderOperationAddressTags] = dtraddress.FormatTags(pairs)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// adaptiveExtension is a questionnaireAdaptive extension found in a document
// and the Questionnaire it is on.
type adaptiveExtension struct{ node, questionnaire relay.NodeID }

// adaptiveExtensions finds, by structure at any depth, every
// questionnaireAdaptive extension of a Questionnaire in doc.
func adaptiveExtensions(doc *relay.Document) []adaptiveExtension {
	var out []adaptiveExtension
	for n := relay.NodeID(0); int(n) < doc.Len(); n++ {
		if doc.Kind(n) != relay.KindObject || docText(doc, n, "resourceType") != "Questionnaire" {
			continue
		}
		exts, ok := doc.Member(n, "extension")
		if !ok || doc.Kind(exts) != relay.KindArray {
			continue
		}
		for _, e := range doc.Elems(exts) {
			if doc.Kind(e) == relay.KindObject && docText(doc, e, "url") == questionnaireAdaptiveExtension {
				out = append(out, adaptiveExtension{node: e, questionnaire: n})
			}
		}
	}
	return out
}

// carriesQuestionnaire reports whether doc carries a Questionnaire anywhere,
// found by structure, contained ones included.
func carriesQuestionnaire(doc *relay.Document) bool {
	for n := relay.NodeID(0); int(n) < doc.Len(); n++ {
		if doc.Kind(n) == relay.KindObject && docText(doc, n, "resourceType") == "Questionnaire" {
			return true
		}
	}
	return false
}

// documentParents maps each node of doc to its parent (-1 for the root).
func documentParents(doc *relay.Document) []relay.NodeID {
	parent := make([]relay.NodeID, doc.Len())
	for i := range parent {
		parent[i] = -1
	}
	for n := relay.NodeID(0); int(n) < doc.Len(); n++ {
		switch doc.Kind(n) {
		case relay.KindObject:
			for _, m := range doc.Members(n) {
				parent[m.Value] = n
			}
		case relay.KindArray:
			for _, e := range doc.Elems(n) {
				parent[e] = n
			}
		}
	}
	return parent
}

// topLevelEntry reports whether q is the resource of an entry of a Bundle's
// entry array, and that Bundle is not itself inside a resource's contained
// array: a top-level entry of a package bundle.
func topLevelEntry(doc *relay.Document, parent []relay.NodeID, q relay.NodeID) bool {
	entry := parent[q]
	if entry < 0 || doc.Kind(entry) != relay.KindObject {
		return false
	}
	if r, ok := doc.Member(entry, "resource"); !ok || r != q {
		return false
	}
	arr := parent[entry]
	if arr < 0 || doc.Kind(arr) != relay.KindArray {
		return false
	}
	bundle := parent[arr]
	if bundle < 0 || doc.Kind(bundle) != relay.KindObject || docText(doc, bundle, "resourceType") != "Bundle" {
		return false
	}
	if e, ok := doc.Member(bundle, "entry"); !ok || e != arr {
		return false
	}
	for a := parent[bundle]; a >= 0; a = parent[a] {
		if doc.Kind(a) == relay.KindArray {
			if holder := parent[a]; holder >= 0 {
				if c, ok := doc.Member(holder, "contained"); ok && c == a {
					return false
				}
			}
		}
	}
	return true
}

// adaptiveRestore returns the restore (E-13) operations for the request in,
// as the payer's gateway forwards it to its own system: for each
// questionnaireAdaptive address on any Questionnaire in it that this payer's
// gateway issued to the provider that sent it, and whose tag verifies, the
// address the payer wrote (its base at the address's line, then the
// remainder), or its true for a fetch-server address. An address in that
// shape naming another payer is carried and observed; one naming this payer
// that does not verify, or whose remainder is not clean, is carried and
// recorded. Nothing is restored without an issuer on ctx.
func (n *nativeResponder) adaptiveRestore(ctx context.Context, in relay.Body) []relay.Op {
	issuer, ok := addressIssuerOf(ctx)
	if !ok {
		return nil
	}
	doc, err := relay.Doc(in)
	if err != nil {
		exchangeOf(ctx).editNotMade(EditNotMadeAddressUnreadableRequest)
		return nil
	}
	var ops []relay.Op
	for _, ext := range adaptiveExtensions(doc) {
		v, ok := doc.Member(ext.node, "valueUrl")
		if !ok || doc.Kind(v) != relay.KindString {
			continue
		}
		value, err := doc.StringValue(v)
		if err != nil {
			continue
		}
		is, ok := dtraddress.FindIssued(value)
		if !ok {
			continue
		}
		if is.Payer != issuer.payer {
			if issuer.carried != nil {
				issuer.carried()
			}
			continue
		}
		if !dtraddress.CleanRemainder(is.Remainder) {
			exchangeOf(ctx).editNotMade(EditNotMadeAddressUncleanRemainder)
			continue
		}
		base, ok := n.dtrBaseAt(is.Line)
		a := dtraddress.Address{Family: is.Family, Payer: issuer.payer, Provider: issuer.provider, Line: is.Line, Base: base, Remainder: is.Remainder}
		if !ok || !issuer.key.Verify(a, is.Tag) {
			exchangeOf(ctx).editNotMade(EditNotMadeAddressNotVerified)
			continue
		}
		if is.Family == dtraddress.FamilyFetchServer {
			op, err := doc.AdaptiveForm(ext.node, "")
			if err != nil {
				exchangeOf(ctx).editNotMade(EditNotMadeAddressUnreadableRequest)
				continue
			}
			ops = append(ops, op)
			continue
		}
		restored, err := relay.JSONString(base + is.Remainder)
		if err != nil {
			continue
		}
		ops = append(ops, doc.Replace(v, restored))
	}
	return ops
}
