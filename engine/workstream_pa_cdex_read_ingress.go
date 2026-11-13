//go:build !shn_coreonly

package engine

import (
	"net/http"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextask"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The payer gateway's side of the payer's read of its CDex Task
// (cdex-task-status, D129): the payer's own system reads the
// attachment-request Task it created at a provider's base on its gateway, as
// a plain FHIR read or vread at that base (GET <base>/Task/{id},
// GET <base>/Task/{id}/_history/{vid}), and the network carries the read to
// that provider, whose gateway reads the Task at the provider's own system
// (workstream_pa_cdex_read.go). The read is the payer system's own, carried
// exactly: an empty body, with its path and query, as sent, in the request
// frame's readPath. The network refuses one only on its own primitives:
//   - a bearer whose scopes permit no read of a Task (the leg's authority,
//     403, before anything is read);
//   - a read at the plain base, which names no provider: the Task's id is the
//     provider's, so it cannot be routed (routing, 404, the base's own
//     OperationOutcome naming where the Task is read);
//   - a path whose id or version is not a FHIR id, or a query that cannot be
//     carried as one (routing, 400, nothing sent: shnsdk.ParseCDexTaskReadPath);
//   - a base that names no registered provider (routing, 404);
//   - a read carrying a body (message integrity, 400).
//
// A provider whose gateway does not take the read is refused by the Hub,
// which records it, and its refusal is carried as on every leg. The
// provider's answer is delivered as it came: a 2xx with its status, media
// type, ETag, Last-Modified and Retry-After; any other with its status,
// media type and body. Once a 2xx answer naming a patient is
// delivered, this gateway binds the Task's patient through its own records
// and reports that binding to the Hub (requesterbinding.go), after the
// answer and without changing it.

// RouteCDexReadBase is the read route at a provider's base on this gateway,
// as exchange records name it.
const RouteCDexReadBase = "cdex-read-base"

// RouteCDexRead is the read route at this gateway's plain Task endpoint,
// which serves no read, as exchange records name it.
const RouteCDexRead = "cdex-read"

// The refusals of a payer's read that only its gateway makes.
const (
	// errCDexReadPlainBase: a read at the plain Task endpoint names no
	// provider, and the Task's id is the provider's, so it cannot be routed
	// (D129, Q5).
	errCDexReadPlainBase = "Task not found at this base: a Task is read at the base of the provider it was created for (GET [base]/Task/[id]), which " + CDexProvidersPath + " lists; the Location the Task's create answered names that address"
	// errCDexReadPath opens the refusal of a read whose path is not a Task's
	// read or vread.
	errCDexReadPath = "the request is not a Task read or vread (GET [base]/Task/[id] or [base]/Task/[id]/_history/[vid], each id a FHIR id, then an optional query); nothing was sent: "
	// errCDexReadBody: a read carries no body.
	errCDexReadBody = "a Task read carries no body; nothing was sent"
)

// handleCDexReadIngress is the payer system's read or vread of its Task at a
// provider's base: routed to the provider the base names, carried exactly,
// and the provider's answer delivered as it came.
func (g *Gateway) handleCDexReadIngress(w http.ResponseWriter, r *http.Request) {
	w, ok := g.admitCDexRead(w, r)
	if !ok {
		return
	}
	exchangeOf(r.Context()).leg(legCDexTaskStatus)
	w, scope := g.withScope(w, relay.RoleRequester)
	if !readNoBody(w, r) {
		return
	}
	path, ok := cdexReadPathOf(w, r, 3)
	if !ok {
		return
	}
	recipient, status, msg, reason := g.cdexRequestRecipient(r)
	if status != 0 {
		// The read's own answer for an address that names no one: FHIR's
		// read answers 404, never 422, which it defines only for a proposed
		// resource.
		exchangeOf(r.Context()).refusedFor(RefusalRouting, reason)
		writeJSON(w, http.StatusNotFound, notFoundOutcome(msg))
		return
	}
	// A module ingress client builds no read: no builder builds this leg's
	// request, so it is refused here.
	if _, ok := g.ingressBuilder(w, r, legCDexTaskStatus); !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: legCDexTaskStatus, Seam: "payer-ingress", Whose: "own"}))
	scope.leg = legCDexTaskStatus
	ex := g.exchanges.Begin(workstreamPA)
	child := g.ingressCorrelation(w, r)
	content := Content{WorkstreamType: workstreamPA, Payload: relay.Exact(relay.NewBody(nil, relay.OriginIngressRequest), ""), Carried: true, ReadPath: path.String()}
	var stated statedAnswer
	var answerType string
	ctx, report := withRequesterBindingReport(withStatedAnswerSink(withAnswerMediaTypeSink(r.Context(), &answerType), &stated))
	// The read names no patient (its row's subject: from-answer), so it is
	// authorized for none.
	answer, err := g.OriginateLeg(ctx, r, recipient, legCDexTaskStatus, "", child, "", content)
	spec, _ := legSpecFor(legCDexTaskStatus)
	legProj := Leg{Type: legCDexTaskStatus, Physics: spec.Physics, Content: content}
	if err != nil {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	answerCtx := withFindingContext(r.Context(), findingContext{LegType: legCDexTaskStatus, CorrelationID: child, Seam: "payer-ingress", Whose: "peer"})
	if repeatsAMember(answerCtx, answer) {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the provider's answer can be read two ways: it repeats a member name"})
		return
	}
	g.recordLeg(ex.ID, legProj.Project(child, "answered"))
	ct := writableAnswerMediaType(answerType)
	delivered := g.writeRelayedAnswer(w, stated, ct,
		relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), ct),
		relay.Key{Leg: legCDexTaskStatus, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
	// A delivered 2xx whose token names a patient disclosed that patient:
	// this gateway binds it through its own records, by the member the Task
	// names (cdexReadMember), and reports its binding to the Hub, after the
	// answer (requesterbinding.go).
	g.reportRequesterBindingAfter(r.Context(), legCDexTaskStatus, report, delivered, stated, answer, cdexReadMember)
}

// handleCDexReadPlain is the payer system's read or vread at the plain Task
// endpoint: it names no provider, and the Task's id is the provider's, so the
// network cannot route it (D129, Q5). It is answered 404 with this gateway's
// own OperationOutcome naming where a Task is read, recorded as routing, once
// the bearer is admitted for a read. Sending it to every provider is not
// done: that would send the id to providers that never saw it.
func (g *Gateway) handleCDexReadPlain(w http.ResponseWriter, r *http.Request) {
	w, ok := g.admitCDexRead(w, r)
	if !ok {
		return
	}
	exchangeOf(r.Context()).leg(legCDexTaskStatus)
	if !readNoBody(w, r) {
		return
	}
	exchangeOf(r.Context()).refusedFor(RefusalRouting, reasonCDexReadPlainBase)
	writeJSON(w, http.StatusNotFound, notFoundOutcome(errCDexReadPlainBase))
}

// readNoBody reads a read's body as every ingress route reads one, to the
// network's limit, and refuses one that has any: a read carries no body
// (message integrity, 400). ok is false when it refused.
func readNoBody(w http.ResponseWriter, r *http.Request) bool {
	body, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes)
	if !ok {
		return false
	}
	if len(body) != 0 {
		exchangeOf(r.Context()).refusingFor(RefusalIntegrity, reasonCDexReadBody)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errCDexReadBody})
		return false
	}
	return true
}

// admitCDexRead checks the payer system's bearer for a read route, on the
// writer scoped to the requester it returns: a refusal (401, or 403 for a
// bearer whose scopes permit no read of a Task) is written before anything
// else is read. ok is false when it refused.
func (g *Gateway) admitCDexRead(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, bool) {
	w, _ = g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	return w, !g.ingressScopeRefused(w, r, taskRead)
}

// cdexReadPathOf is the read's path below the base, as the request sent it:
// the escaped path's segments after the Task segment, which is segment at of
// the base's path (the read routes' patterns put it there), then the query
// exactly as sent, read as one Task read or vread
// (shnsdk.ParseCDexTaskReadPath). The path is taken escaped, so an escaped
// byte in an id is read as it was sent, never as the byte it decodes to. A
// path that is not one is refused 400 on routing, nothing sent; ok is false.
func cdexReadPathOf(w http.ResponseWriter, r *http.Request, at int) (shnsdk.CDexTaskPath, bool) {
	segs := strings.Split(r.URL.EscapedPath(), "/")
	raw := ""
	if len(segs) > at+2 && segs[at+1] == "Task" {
		raw = strings.Join(segs[at+1:], "/")
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		raw += "?" + r.URL.RawQuery
	}
	path, err := shnsdk.ParseCDexTaskReadPath(raw)
	if err != nil {
		exchangeOf(r.Context()).refusedFor(RefusalRouting, reasonCDexReadPath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errCDexReadPath + err.Error()})
		return shnsdk.CDexTaskPath{}, false
	}
	return path, true
}

// notFoundOutcome is this gateway's own 404 as an OperationOutcome: one error
// issue of code not-found whose diagnostics is msg.
func notFoundOutcome(msg string) map[string]any {
	return map[string]any{"resourceType": "OperationOutcome", "issue": []map[string]string{{"severity": "error", "code": "not-found", "diagnostics": msg}}}
}

// cdexReadMember is the member a delivered Task names its patient by, for
// the payer's own binding: the member number Task.for names
// (cdexTaskMember), as the create binds it. readable is false for an answer
// that is not a Task; member is "" for a Task.for naming no one member
// number, such as one that is only a reference into the provider's own
// system, which this gateway cannot read: its binding is then unavailable,
// never guessed (D129, Q1).
func cdexReadMember(answer []byte) (member string, readable bool) {
	task, err := cdextask.Read(relay.NewBody(answer, relay.OriginPeerFrame))
	if err != nil {
		return "", false
	}
	member, _ = cdexTaskMember(task)
	return member, true
}

// cdexBase is one base of this gateway's CDex ingress: the plain base, or a
// listed provider's, and whether that provider serves reads.
type cdexBase struct {
	provider bool
	reads    bool
}

// cdexBaseOf is the base the request's provider segment names: a registered
// provider that declares it takes the attachment request, as
// /cdex/providers lists it. ok is false for any other.
func (g *Gateway) cdexBaseOf(r *http.Request) (cdexBase, bool) {
	e, ok := g.cfg.Reg.Lookup(r.PathValue("provider"))
	if !ok || e.Role != "provider" || !slices.Contains(e.Legs, legCDexRequest) {
		return cdexBase{}, false
	}
	return cdexBase{provider: true, reads: slices.Contains(e.Legs, legCDexTaskStatus)}, true
}

// errCDexBaseUnknown is the discovery routes' answer at a base that names no
// listed provider.
const errCDexBaseUnknown = "this base names no provider this gateway lists: " + CDexProvidersPath + " lists the providers' bases"

// handleCDexMetadata is GET /metadata at the plain base: FHIR's capabilities
// interaction, its CapabilityStatement declaring Task create only.
func (g *Gateway) handleCDexMetadata(w http.ResponseWriter, _ *http.Request) {
	g.writeCDexMetadata(w, cdexBase{})
}

// handleCDexBaseMetadata is GET <provider's base>/metadata: Task create, and
// read and vread when that provider serves them (its registry entry declares
// the read's leg), so no base advertises a read the Hub would refuse.
func (g *Gateway) handleCDexBaseMetadata(w http.ResponseWriter, r *http.Request) {
	base, ok := g.cdexBaseOf(r)
	if !ok {
		writeJSON(&fhirOperationWriter{w}, http.StatusNotFound, notFoundOutcome(errCDexBaseUnknown))
		return
	}
	g.writeCDexMetadata(w, base)
}

func (g *Gateway) writeCDexMetadata(w http.ResponseWriter, base cdexBase) {
	b, err := shnsdk.BuildPayerCDexIngressCapabilityStatement(g.cfg.Clock(), shnsdk.PayerCDexBase{Provider: base.provider, Reads: base.reads})
	if err != nil {
		// The gateway's own failure, before anything was sent: an ordinary
		// refusal, through the one writer of error answers.
		writeJSON(&fhirOperationWriter{w}, http.StatusInternalServerError, map[string]string{"error": "capability statement build failed"})
		return
	}
	w.Header().Set("Content-Type", "application/fhir+json")
	_, _ = w.Write(b)
}

// handleCDexBaseSmartConfig is <provider's base>/.well-known/smart-configuration:
// SMART App Launch has every FHIR base serve its configuration, so a client
// that uses a provider's base discovers this gateway's token endpoint there.
// It is the plain base's document.
func (g *Gateway) handleCDexBaseSmartConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.cdexBaseOf(r); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": errCDexBaseUnknown})
		return
	}
	g.ingressAuth.handleSmartConfig(w, r)
}
