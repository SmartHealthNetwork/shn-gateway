//go:build !shn_coreonly

package engine

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/dtraddress"
)

// The provider's native ingress for its own DTR client's adaptive rounds
// (SDC $next-question) on the dtr-questionnaire-fetch leg, which already
// declares the next-question frame operation. The rounds ride the package
// fetch's leg and ingress: the same route record (RouteDTR), the same ingress
// credential, and the same leg authority, so a credential the package route
// refuses is refused here too.
//
// Three routes, decided by the pattern the mux matched (r.Pattern), never by
// the path's text:
//   - POST /dtr/payer/{payer}/{rest...}: an adaptive address the provider's
//     gateway issued for a payer, read from the escaped path as
//     <line>/<tag><remainder>/Questionnaire/$next-question;
//   - POST /dtr/fetch-server/{payer}/{line}/{tag}/Questionnaire/$next-question:
//     a questionnaireAdaptive of true, made explicit for a payer;
//   - POST /Questionnaire/$next-question: the plain route, which names no
//     payer (a $next-question round carries no member, order or coverage).
//
// A round that passes every check this gateway makes, in order, is forwarded
// to its payer naming the address it was called at (operationSubpath); the
// payer's gateway verifies the address's tag and delivers it there, or refuses
// it as routing (verifyRoundSubpath). The forward goes only to a payer whose
// gateway declares v1opsub, as a payer's gateway does from the release that
// mounts these routes (paRoutes).
//
// The provider ingress CapabilityStatement (GET /metadata, the SDK's
// BuildProviderIngressCapabilityStatement) lists $next-question beside
// $questionnaire-package: it states exactly the Questionnaire operations the
// ingress mounts routes for, in both directions
// (TestProviderIngressStatement_QuestionnaireOperationsAreTheMountedRoutes).

const (
	// patternDTRNextQuestionPayer, patternDTRNextQuestionFetchServer and
	// patternDTRNextQuestionPlain are the routes' mux patterns, which the
	// handler dispatches on.
	patternDTRNextQuestionPayer       = "POST /dtr/payer/{payer}/{rest...}"
	patternDTRNextQuestionFetchServer = "POST /dtr/fetch-server/{payer}/{line}/{tag}/Questionnaire/$next-question"
	patternDTRNextQuestionPlain       = "POST /Questionnaire/$next-question"

	errNextQuestionNoPayer = "a $next-question round names no payer: it carries no member, order or coverage to route by. " +
		"Fetch the questionnaire package through this gateway and call the questionnaireAdaptive address it returns"
	errNextQuestionUnknownPayer = "the adaptive address names no registered payer"
	errNextQuestionNoSubpath    = "the payer's gateway does not support adaptive addresses yet, so this round cannot be carried on the network"
	errNextQuestionTwoAddresses = "the round's Questionnaire carries more than one adaptive address this gateway issued, so it names no one payer: " +
		"call the adaptive address the questionnaire names"
	errNextQuestionAddress = "the adaptive address is not one this gateway issues: after the payer it names a line, an address tag, " +
		"then any further segments and Questionnaire/$next-question, each segment made only of unreserved characters (A-Z a-z 0-9 - . _ ~) and none of them . or .."
	errNextQuestionNoSubject  = "cannot bind the round to a patient: its QuestionnaireResponse names no subject"
	errNextQuestionNotPatient = "cannot bind the round to a patient: its QuestionnaireResponse subject is not a Patient reference"
	errNextQuestionUnheld     = "request patient does not resolve"
	errNextQuestionMixed      = "inconsistent patient reference in ingress payload"
	errNextQuestionNotInput   = "parse next-question input failed"
)

var (
	// dtrLineSegment is a pa.dtr contract line as a path segment.
	dtrLineSegment = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	// addressTagSegment is an address tag: 22 base64url characters (an
	// HMAC-SHA-256 truncated to 128 bits, unpadded).
	addressTagSegment = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	// cleanSegment is one segment of an adaptive address's remainder:
	// unreserved characters only, with no percent-encoding.
	cleanSegment = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

// nextQuestionSuffix is the operation's path after the address: two segments,
// compared after unescaping each, as the mux compares a literal segment.
var nextQuestionSuffix = []string{"Questionnaire", "$next-question"}

// nextQuestionAddress is what a per-payer route's path names.
type nextQuestionAddress struct {
	family, payer, line, tag string
	remainder                []string
}

// subpath is the address as the round's request frame names it
// (shnsdk.FrameHeaderOperationSubpath).
func (a nextQuestionAddress) subpath() dtraddress.Subpath {
	s := dtraddress.Subpath{Family: a.family, Line: a.line, Tag: a.tag}
	if len(a.remainder) > 0 {
		s.Remainder = "/" + strings.Join(a.remainder, "/")
	}
	return s
}

// parseNextQuestionRest reads the {rest...} of a payer-family route from the
// request's escaped path: <line>/<tag>, then zero or more clean remainder
// segments, then Questionnaire/$next-question. ok is false for anything else.
func parseNextQuestionRest(escapedRest string) (line, tag string, remainder []string, ok bool) {
	segs := strings.Split(escapedRest, "/")
	if len(segs) < 2+len(nextQuestionSuffix) {
		return "", "", nil, false
	}
	n := len(segs)
	for i, want := range nextQuestionSuffix {
		got, err := url.PathUnescape(segs[n-len(nextQuestionSuffix)+i])
		if err != nil || got != want {
			return "", "", nil, false
		}
	}
	line, tag = segs[0], segs[1]
	if !dtrLineSegment.MatchString(line) || !addressTagSegment.MatchString(tag) {
		return "", "", nil, false
	}
	for _, s := range segs[2 : n-len(nextQuestionSuffix)] {
		if !cleanSegment.MatchString(s) || s == "." || s == ".." {
			return "", "", nil, false
		}
	}
	return line, tag, segs[2 : n-len(nextQuestionSuffix)], true
}

// escapedRest is the escaped path after prefix, the {rest...} a payer-family
// route's pattern matched, with its payer segment: the path the client sent,
// never the decoded PathValue.
func escapedRest(r *http.Request, prefix string) (payer, rest string, ok bool) {
	p := r.URL.EscapedPath()
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	payer, rest, ok = strings.Cut(p[len(prefix):], "/")
	if !ok || payer == "" {
		return "", "", false
	}
	decoded, err := url.PathUnescape(payer)
	if err != nil {
		return "", "", false
	}
	return decoded, rest, true
}

// handleDTRNextQuestionIngress answers a provider DTR client's $next-question
// round on one of the routes above. See the comment at the top of this file
// for the seam it holds.
func (g *Gateway) handleDTRNextQuestionIngress(w http.ResponseWriter, r *http.Request) {
	const legType = "dtr-questionnaire-fetch"
	w, scope := g.withScope(w, relay.RoleRequester)
	scope.leg = legType
	w = &fhirOperationWriter{w}
	// The package route's own credential check, first, so a credential it
	// refuses is refused here the same way.
	if g.ingressAuthRefused(w, r) {
		return
	}
	exchangeOf(r.Context()).leg(legType)
	if _, ok := g.ingressBuilder(w, r, legType); !ok {
		return
	}
	// Tag the leg for every check, as the package route does.
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: legType, Seam: "provider-ingress", Whose: "own"}))
	ctx := r.Context()
	refuse := func(rule string, status int, msg string, reason refusalReason) {
		exchangeOf(ctx).refusingFor(rule, reason)
		writeJSON(w, status, map[string]string{"error": msg})
	}

	var (
		addr     nextQuestionAddress
		body     []byte
		bodyRead bool
	)
	switch r.Pattern {
	case patternDTRNextQuestionPayer:
		payer, rest, ok := escapedRest(r, "/dtr/payer/")
		if !ok {
			refuse(RefusalRouting, http.StatusUnprocessableEntity, errNextQuestionAddress, reasonNextQuestionAddress)
			return
		}
		addr.family, addr.payer = dtraddress.FamilyPayer, payer
		if status, msg, reason := g.nextQuestionPayerRefusal(payer); status != 0 {
			refuse(RefusalRouting, status, msg, reason)
			return
		}
		line, tag, remainder, ok := parseNextQuestionRest(rest)
		if !ok {
			refuse(RefusalRouting, http.StatusUnprocessableEntity, errNextQuestionAddress, reasonNextQuestionAddress)
			return
		}
		addr.line, addr.tag, addr.remainder = line, tag, remainder
	case patternDTRNextQuestionFetchServer:
		// Read from the escaped path as the payer family is: a line or tag
		// spelled with percent-encoding is not one this gateway issues.
		payer, rest, ok := escapedRest(r, "/dtr/fetch-server/")
		if !ok {
			refuse(RefusalRouting, http.StatusUnprocessableEntity, errNextQuestionAddress, reasonNextQuestionAddress)
			return
		}
		addr.family, addr.payer = dtraddress.FamilyFetchServer, payer
		if status, msg, reason := g.nextQuestionPayerRefusal(payer); status != 0 {
			refuse(RefusalRouting, status, msg, reason)
			return
		}
		line, tag, remainder, ok := parseNextQuestionRest(rest)
		if !ok || len(remainder) != 0 {
			refuse(RefusalRouting, http.StatusUnprocessableEntity, errNextQuestionAddress, reasonNextQuestionAddress)
			return
		}
		addr.line, addr.tag = line, tag
	default:
		// The plain route names no payer: it is routed by the one adaptive
		// address this gateway issued that the round's own Questionnaire
		// carries, as if the client had called that address (C5). The
		// payer's gateway verifies its tag as it verifies any round's.
		b, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes)
		if !ok {
			return
		}
		body, bodyRead = b, true
		a, msg, reason := g.plainRoundAddress(b)
		if msg != "" {
			refuse(RefusalRouting, http.StatusUnprocessableEntity, msg, reason)
			return
		}
		addr = a
		if status, msg, reason := g.nextQuestionPayerRefusal(addr.payer); status != 0 {
			refuse(RefusalRouting, status, msg, reason)
			return
		}
	}

	// The line must be a pa.dtr contract line the network defines.
	if !dtrContractLines()[addr.line] {
		refuse(RefusalRouting, http.StatusUnprocessableEntity, errNextQuestionAddress, reasonNextQuestionAddress)
		return
	}
	// The line is not selected here. A round is the participant's own bytes,
	// so it is carried as the package fetch is (the carried selection, which
	// never rebuilds a participant's bytes at another line, and refuses a
	// pair only under the participant's own REQUIRE_SHARED_LINE), never by
	// the resume selection, which serves only follow-ups this gateway builds.
	// That selection comes with the forward.

	if !bodyRead {
		b, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes)
		if !ok {
			return
		}
		body = b
	}
	pci, status, msg := g.bindNextQuestionRound(r, body)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// Every check passed: the round goes to the payer's gateway, which
	// verifies the address it names and delivers it there (verifyRoundSubpath).
	g.forwardNextQuestionRound(w, r, addr, pci, body)
}

// forwardNextQuestionRound carries a checked round to its payer as the
// package fetch carries its request: the participant's own bytes, at the line
// the carried selection picks and never rebuilt (refused for its lines only
// under the participant's own REQUIRE_SHARED_LINE), framed as a next-question
// operation naming the address it was called at. The payer's answer is
// checked at this gateway's conformance level as the package answer is, by
// its own declared profile (no DTR line before 2.2 defines an adaptive
// QuestionnaireResponse profile), and reaches the client with the adaptive
// addresses its gateway issued re-keyed (E-12).
func (g *Gateway) forwardNextQuestionRound(w http.ResponseWriter, r *http.Request, addr nextQuestionAddress, pci string, body []byte) {
	const legType = "dtr-questionnaire-fetch"
	child := g.ingressCorrelation(w, r)
	route, ok := g.selectLegLineOrFail(r.Context(), w, addr.payer, legType, child, carriedRequest)
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: legType, CorrelationID: child, Seam: "provider-ingress", Whose: "own",
	}))
	request := relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), mediaFHIRJSON)
	requestKey := contentRequestKey(legType, Content{Payload: request, Carried: true})
	sent, terr := relay.Transmit(request, relay.Check(requestKey))
	if terr != nil {
		g.ownershipRefused(requestKey, terr)
		writeOwnershipFault(w)
		return
	}
	adapted, _, recordWalk, aerr := g.egressWalk(route, sent, ExchangeIdentity{CorrelationID: child, LegType: legType, Counterpart: addr.payer})
	if aerr != nil {
		writeJSON(w, adaptFailureStatus(aerr), map[string]string{"error": aerr.Error()})
		return
	}
	if !bytes.Equal(adapted, sent) {
		if g.cfg.RequireSharedLine {
			exchangeOf(r.Context()).routedFor(http.StatusUnprocessableEntity, reasonDTRLineUnchanged)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msgDTRLineUnchanged})
			return
		}
		route = route.asSent()
	} else if err := recordWalk(); err != nil {
		writeJSON(w, adaptFailureStatus(err), map[string]string{"error": err.Error()})
		return
	}
	sub := addr.subpath()
	ex := g.exchanges.Begin(workstreamPA)
	content := Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route),
		Payload: request, Carried: true, Operation: shnsdk.FrameOperationNextQuestion, Subpath: sub.String(),
		MediaType: carriedFHIRMediaType(r.Header.Get("Content-Type"))}
	var stated statedAnswer
	var answerType string
	answerJSON, err := g.OriginateLeg(withStatedAnswerSink(withAnswerMediaTypeSink(r.Context(), &answerType), &stated), r, addr.payer, legType, pci, child, "", content)
	leg := Leg{Type: legType, Physics: legPhysics(legType), Content: content, Subjects: subjectsOf(pci)}
	if err != nil {
		g.recordLeg(ex.ID, leg.Project(child, "error"))
		// The payer's gateway stopped declaring that it reads a round's
		// address after the ingress checked it: nothing was sent, refused as
		// the ingress refuses it.
		if errors.Is(err, errRoundAddressUnsupported) {
			exchangeOf(r.Context()).refusedFor(RefusalRouting, reasonNextQuestionNoSubpath)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": errNextQuestionNoSubpath})
			return
		}
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	answerCtx := withFindingContext(r.Context(), findingContext{LegType: legType, CorrelationID: child, Seam: "provider-ingress", Whose: "peer"})
	if len(bytes.TrimSpace(answerJSON)) > 0 && g.carriedAnswerRefused(answerCtx, w, answerJSON, shnsdk.LineOf(route.Token), "pa.dtr", "", g.roundToleratedPayers(addr.payer)) {
		g.recordLeg(ex.ID, leg.Project(child, "error"))
		return
	}
	// The round's answer keeps the media type the payer's system stated.
	ct := writableAnswerMediaType(answerType)
	relayed := relay.Exact(relay.NewBody(answerJSON, relay.OriginPeerFrame), ct)
	rekeyed := g.rekeyAnswer(r.Context(), relayed, rekeyContext{payer: addr.payer, line: addr.line, stated: stated, nextQuestion: true, round: &sub})
	g.recordLeg(ex.ID, leg.Project(child, "ok"))
	g.writeRelayedAnswer(w, stated, ct, rekeyed, dtrAnswerKey)
	// The round's answer is in the line its route was sealed at: no answer is
	// bridged here, so evidence judges it at that line.
	exchangeOf(r.Context()).answerLine(route.Token)
}

// roundToleratedPayers is the payer identities a round's answer is judged as
// coming from, for the participant's own tolerance (D122): a round carries no
// Coverage to name them, but its payer is known from its address, so they are
// the identities the participant tolerates that route to that payer by the
// routing read the package route makes (PayerRouter). None tolerates nothing,
// and an identity that routes to another payer tolerates nothing here.
func (g *Gateway) roundToleratedPayers(payer string) []shnsdk.PayerIdentifier {
	if g.cfg.PayerRouter == nil {
		return nil
	}
	var out []shnsdk.PayerIdentifier
	for _, p := range g.cfg.ConformanceToleratePeers {
		if holder, ok := g.cfg.PayerRouter.Resolve(p); ok && holder == payer {
			out = append(out, p)
		}
	}
	return out
}

// nextQuestionPayerRefusal is the routing refusal of a round to payer, in the
// order the design states: a registered payer, then framed DTR operations
// (the package route's own refusal), then the adaptive-address token.
func (g *Gateway) nextQuestionPayerRefusal(payer string) (int, string, refusalReason) {
	entry, ok := g.cfg.Reg.Lookup(payer)
	if !ok || entry.Role != "payer" {
		return http.StatusUnprocessableEntity, errNextQuestionUnknownPayer, reasonNextQuestionUnknownPayer
	}
	// The legs a payer declares are not read here: the network checks a
	// recipient's declared legs only for a leg the taxonomy marks with the
	// release that brings it, and the questionnaire fetch is not marked, so
	// the Hub carries it to any payer, as it carries the package fetch.
	if status, msg := g.framedDTRRefusal(payer); status != 0 {
		return status, msg, reasonFramedDTRUnsupported
	}
	if shnsdk.SupportsRequestFrameV1OpSub(entry.RequestFrames) {
		return 0, "", ""
	}
	return http.StatusUnprocessableEntity, errNextQuestionNoSubpath, reasonNextQuestionNoSubpath
}

// bindNextQuestionRound binds the round's patient by D121, as the package
// route binds its subject, from the forms the payer's gateway binds
// (framedNextQuestionSubject, patientMember): a Patient reference, relative or
// absolute. A round naming no subject, or a subject that is not a Patient
// reference, is refused at every level (authority). A subject the system of
// record does not hold is refused only when the participant requires known
// members (Config.RequireKnownMembers), a check it opted into (conformance);
// another patient the round names is the payload's own consistency
// (RulePatientMixed), refused at strict only.
func (g *Gateway) bindNextQuestionRound(r *http.Request, body []byte) (string, int, string) {
	ctx := r.Context()
	subject, named, ok := framedNextQuestionSubjectNamed(body)
	if !ok {
		exchangeOf(ctx).refusingFor(RefusalAuthority, reasonNextQuestionNotInput)
		return "", http.StatusBadRequest, errNextQuestionNotInput
	}
	if !named {
		exchangeOf(ctx).refusingFor(RefusalAuthority, reasonNextQuestionNoSubject)
		return "", http.StatusUnprocessableEntity, errNextQuestionNoSubject
	}
	member, ok := patientMember(subject)
	if !ok {
		exchangeOf(ctx).refusingFor(RefusalAuthority, reasonNextQuestionNotPatient)
		return "", http.StatusForbidden, errNextQuestionNotPatient
	}
	pci, found, err := g.resolveSubjectPCI(ctx, member, body)
	if err != nil {
		status, msg := subjectBindingFailure(ctx, err)
		return "", status, msg
	}
	if !found {
		// Only a participant that requires known members gets here: a check
		// it opted into, recorded as conformance.
		exchangeOf(ctx).refusing(RefusalConformance)
		return "", http.StatusForbidden, errNextQuestionUnheld
	}
	// The fence reads each resource the round carries: the
	// QuestionnaireResponse itself, bare, or every resource a Parameters
	// carries in a parameter or a part, never the Parameters wrapper. As on
	// the package route, whose fence has no FHIR bases to resolve against, an
	// absolute Patient reference reads as another patient (a known limit of
	// both routes).
	fence := newPatientFence(shnsdk.MemberSystem, member, nil, member)
	for _, res := range nextQuestionResources(body) {
		if err := fence.check(res); err != nil && g.guard(ctx, KindContent, RulePatientMixed, body) {
			return "", http.StatusForbidden, errNextQuestionMixed
		}
	}
	return pci, 0, ""
}

// nextQuestionResources are the resources a $next-question input carries, as
// their own bytes: the body when it is a bare QuestionnaireResponse, else the
// resource of every parameter and part of the Parameters
// (framedNextQuestionSubject has already read the body as one of the two).
func nextQuestionResources(body []byte) [][]byte {
	d, err := relay.Doc(relay.NewBody(body, relay.OriginIngressRequest))
	if err != nil || d.Kind(d.Root()) != relay.KindObject || docText(d, d.Root(), "resourceType") != "Parameters" {
		return [][]byte{body}
	}
	var out [][]byte
	var walk func(arr relay.NodeID)
	walk = func(arr relay.NodeID) {
		if d.Kind(arr) != relay.KindArray {
			return
		}
		for _, p := range d.Elems(arr) {
			if d.Kind(p) != relay.KindObject {
				continue
			}
			if res, ok := d.Member(p, "resource"); ok {
				s, e := d.Span(res)
				out = append(out, body[s:e])
			}
			if parts, ok := d.Member(p, "part"); ok {
				walk(parts)
			}
		}
	}
	if params, ok := d.Member(d.Root(), "parameter"); ok {
		walk(params)
	}
	return out
}

// dtrContractLines are the pa.dtr lines the network defines: the PA
// workstream's own native contract set, read as the zone declares it.
func dtrContractLines() map[string]bool {
	native, _ := paWorkstream{}.contractVersions()
	out := map[string]bool{}
	for _, tok := range native {
		if line, ok := strings.CutPrefix(tok, "pa.dtr@"); ok {
			out[line] = true
		}
	}
	return out
}

// plainRoundAddress is the address a round on the plain route is routed by:
// the one distinct questionnaireAdaptive valueUrl, on any Questionnaire the
// round carries, that is an address this gateway issued (under its own
// ingress base, in the re-key's form). None names no payer; two distinct
// ones name no one payer; either is refused as routing. The same address
// twice is one. The one address is checked as a per-payer path is
// (parseNextQuestionRest): a remainder that is not clean is refused as the
// path would be.
func (g *Gateway) plainRoundAddress(body []byte) (nextQuestionAddress, string, refusalReason) {
	none := func() (nextQuestionAddress, string, refusalReason) {
		return nextQuestionAddress{}, errNextQuestionNoPayer, reasonNextQuestionNoPayer
	}
	d, err := relay.Doc(relay.NewBody(body, relay.OriginIngressRequest))
	if err != nil {
		return none()
	}
	own := strings.TrimRight(g.cfg.IngressBaseURL, "/") + "/dtr/"
	found := map[string]dtraddress.Issued{}
	for _, ext := range adaptiveExtensions(d) {
		v, ok := d.Member(ext.node, "valueUrl")
		if !ok || d.Kind(v) != relay.KindString {
			continue
		}
		value, err := d.StringValue(v)
		if err != nil || !strings.HasPrefix(value, own) {
			continue
		}
		if is, ok := dtraddress.FindIssued(value); ok && strings.HasPrefix(value, own+is.Family+"/") {
			found[value] = is
		}
	}
	switch len(found) {
	case 0:
		return none()
	case 1:
	default:
		return nextQuestionAddress{}, errNextQuestionTwoAddresses, reasonNextQuestionTwoAddresses
	}
	for _, is := range found {
		if !dtraddress.CleanRemainder(is.Remainder) {
			return nextQuestionAddress{}, errNextQuestionAddress, reasonNextQuestionAddress
		}
		a := nextQuestionAddress{family: is.Family, payer: is.Payer, line: is.Line, tag: is.Tag}
		if is.Remainder != "" {
			a.remainder = strings.Split(strings.TrimPrefix(is.Remainder, "/"), "/")
		}
		return a, "", ""
	}
	return none()
}
