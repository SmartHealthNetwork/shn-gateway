//go:build !shn_coreonly

package engine

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
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
// Temporary seam (tracked in the SHN platform repository): NOT MOUNTED. The
// production mux (paRoutes) does not mount these routes until the forward and
// the address tag check land; a row asserts that, so mounting them is a
// deliberate change. The adaptive address the payer's gateway issues, its tag
// and the frame headers that carry it are not built yet. Until they are, no
// round is sent even where the handler runs: every check this gateway makes is
// made, in order, and a round that passes them all is refused 422 routing,
// because the payer's gateway cannot verify the address.
// Invariant: nothing is sent before the payer's gateway can verify the
// address, so this handler is never more permissive than the forward that
// replaces the final refusal. Mounting, the pinned ingress surface, the HTTP
// metric's route patterns, the stability route rows, the hosted edge's
// forward rule and the advertising (CapabilityStatement, discovery) all land
// with the forward, as one change.

const (
	// patternDTRNextQuestionPayer, patternDTRNextQuestionFetchServer and
	// patternDTRNextQuestionPlain are the routes' mux patterns, which the
	// handler dispatches on.
	patternDTRNextQuestionPayer       = "POST /dtr/payer/{payer}/{rest...}"
	patternDTRNextQuestionFetchServer = "POST /dtr/fetch-server/{payer}/{line}/{tag}/Questionnaire/$next-question"
	patternDTRNextQuestionPlain       = "POST /Questionnaire/$next-question"

	// requestFrameV1OpSub is the request-frame token a payer's gateway
	// declares when it carries an adaptive address's path remainder and
	// verifies the address it issued. It moves to the SDK with the frame
	// headers.
	requestFrameV1OpSub = "v1opsub"

	errNextQuestionNoPayer = "a $next-question round names no payer: it carries no member, order or coverage to route by. " +
		"Fetch the questionnaire package through this gateway and call the questionnaireAdaptive address it returns"
	errNextQuestionUnknownPayer = "the adaptive address names no registered payer that takes the questionnaire fetch"
	errNextQuestionNoSubpath    = "the payer's gateway does not support adaptive addresses yet, so this round cannot be carried on the network"
	errNextQuestionAddress      = "the adaptive address is not one this gateway issues: after the payer it names a line, an address tag, " +
		"then any further segments and Questionnaire/$next-question, each segment made only of unreserved characters (A-Z a-z 0-9 - . _ ~) and none of them . or .."
	errNextQuestionUnverifiable = "the payer's gateway cannot verify this adaptive address yet, so the round is not sent. " +
		"Call the payer's own $next-question endpoint until adaptive support reaches this network"
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
	payer, line, tag string
	remainder        []string
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

	var addr nextQuestionAddress
	switch r.Pattern {
	case patternDTRNextQuestionPayer:
		payer, rest, ok := escapedRest(r, "/dtr/payer/")
		if !ok {
			refuse(RefusalRouting, http.StatusUnprocessableEntity, errNextQuestionAddress, reasonNextQuestionAddress)
			return
		}
		addr.payer = payer
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
		addr.payer = payer
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
		// The plain route: no payer is named. Routing it by the one tagged
		// address the round's own contained Questionnaire carries (the
		// design's plain-route rule) is deferred until the payer's gateway
		// issues tags, so there is a tag to read; until then the plain route
		// refuses every round.
		refuse(RefusalRouting, http.StatusUnprocessableEntity, errNextQuestionNoPayer, reasonNextQuestionNoPayer)
		return
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

	body, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes)
	if !ok {
		return
	}
	if status, msg := g.bindNextQuestionRound(r, body); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// Every check passed. Until the payer's gateway can verify the address,
	// nothing is sent (the seam above).
	refuse(RefusalRouting, http.StatusUnprocessableEntity, errNextQuestionUnverifiable, reasonNextQuestionUnverifiable)
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
	for _, f := range entry.RequestFrames {
		if f == requestFrameV1OpSub {
			return 0, "", ""
		}
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
func (g *Gateway) bindNextQuestionRound(r *http.Request, body []byte) (int, string) {
	ctx := r.Context()
	subject, named, ok := framedNextQuestionSubjectNamed(body)
	if !ok {
		exchangeOf(ctx).refusingFor(RefusalAuthority, reasonNextQuestionNotInput)
		return http.StatusBadRequest, errNextQuestionNotInput
	}
	if !named {
		exchangeOf(ctx).refusingFor(RefusalAuthority, reasonNextQuestionNoSubject)
		return http.StatusUnprocessableEntity, errNextQuestionNoSubject
	}
	member, ok := patientMember(subject)
	if !ok {
		exchangeOf(ctx).refusingFor(RefusalAuthority, reasonNextQuestionNotPatient)
		return http.StatusForbidden, errNextQuestionNotPatient
	}
	_, found, err := g.resolveSubjectPCI(ctx, member, body)
	if err != nil {
		return subjectBindingFailure(ctx, err)
	}
	if !found {
		// Only a participant that requires known members gets here: a check
		// it opted into, recorded as conformance.
		exchangeOf(ctx).refusing(RefusalConformance)
		return http.StatusForbidden, errNextQuestionUnheld
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
			return http.StatusForbidden, errNextQuestionMixed
		}
	}
	return 0, ""
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
