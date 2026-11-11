//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// dtrOutputParametersProfile is the output Parameters profile of the DTR
// $questionnaire-package operation, at every line: each line's validator lane
// holds its own package's version of it, which states that line's parameter
// names and cardinalities.
const dtrOutputParametersProfile = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-qpackage-output-parameters"

// dtrLines are the DTR lines among declared, the tokens a gateway declares, in
// their declared order.
func dtrLines(declared []string) []string {
	var lines []string
	for _, tok := range declared {
		contract, version, ok := strings.Cut(tok, "@")
		if ok && contract == "pa.dtr" && !slices.Contains(lines, version) {
			lines = append(lines, version)
		}
	}
	return lines
}

// dtrLineEvidence is what checking a questionnaire package answer at one line
// says about its own line.
type dtrLineEvidence int

const (
	// dtrNotAtLine: not valid at the line, or the gateway has no lane for it,
	// so the line is never evidence.
	dtrNotAtLine dtrLineEvidence = iota
	// dtrAtLine: valid at the line.
	dtrAtLine
	// dtrLineUnchecked: the check could not run, or its lane is still
	// qualifying, so whether the answer is at the line is unknown. It is not
	// "not at the line".
	dtrLineUnchecked
)

// dtrAnswerAt checks answer at line against the DTR output Parameters profile,
// read as SHN's own evidence for E-08, not as a judgment of the payer's answer:
// it runs at every conformance level and records no finding. A line whose
// lane answered and found the answer invalid gives its result to read
// (dtrAnswerSourceLine reads it beside the other lines').
func (g *Gateway) dtrAnswerAt(ctx context.Context, answer []byte, line string) (dtrLineEvidence, shnsdk.Result, bool) {
	v, qualifying := g.candidateLaneAt("pa.dtr", line)
	if qualifying {
		return dtrLineUnchecked, shnsdk.Result{}, false
	}
	if v == nil {
		return dtrNotAtLine, shnsdk.Result{}, false
	}
	askCtx, req, profile := validateRequestAsking(ctx, answer, dtrOutputParametersProfile)
	start := g.exchangeClock()
	res, err := v.Validate(askCtx, req, profile)
	g.routingValidatorTook(line, g.exchangeClock().Sub(start))
	switch {
	case err != nil:
		return dtrLineUnchecked, shnsdk.Result{}, false
	case res.Valid:
		return dtrAtLine, res, true
	}
	return dtrNotAtLine, res, true
}

// dtrAnswerSourceLine is the line of answer's own name family it validates at,
// trying the line the payer was routed at first, with dtrAtLine. A line whose
// check could not run ends the search with dtrLineUnchecked: the source line is
// then unknown, and a later line that validates would be a guess at it. A line
// whose lane only lacked a profile the answer declares does not end it: a line
// whose lane holds that profile (another line's, say) is tried, and the line
// that lacked it is not the answer's line (classifyAtLines); only when no line
// that judged the answer held it is the check one that could not run.
// dtrNotAtLine when it validates at none.
func (g *Gateway) dtrAnswerSourceLine(ctx context.Context, answer []byte, family dtrNameFamily, payerLine string) (string, dtrLineEvidence) {
	lines := dtrFamilyLines(family)
	if i := slices.Index(lines, payerLine); i > 0 {
		lines = append([]string{payerLine}, slices.Delete(slices.Clone(lines), i, i+1)...)
	}
	var readLines []string
	var results []shnsdk.Result
	for _, line := range lines {
		evidence, res, answered := g.dtrAnswerAt(ctx, answer, line)
		switch {
		case evidence == dtrAtLine:
			return line, dtrAtLine
		case evidence == dtrLineUnchecked:
			return line, dtrLineUnchecked
		case !answered:
			continue
		}
		readLines, results = append(readLines, line), append(results, res)
		if verdicts, lacking := classifyAtLines(readLines, results, answer, dtrOutputParametersProfile); verdicts[len(results)-1] == VerdictUnavailable && !lacking[len(results)-1] {
			return line, dtrLineUnchecked
		}
	}
	verdicts, _ := classifyAtLines(readLines, results, answer, dtrOutputParametersProfile)
	for i, v := range verdicts {
		if v == VerdictUnavailable {
			return readLines[i], dtrLineUnchecked
		}
	}
	return lines[0], dtrNotAtLine
}

// carryDTRAnswer is the payload the provider's gateway gives its participant's
// system for a payer's questionnaire package answer (E-08, the DTR answer's
// line-specific parameter names):
//
//   - When this gateway declares exactly one DTR line and the answer's
//     line-specific names (planDTRAnswerNames) all belong to the other name
//     family, and the answer is valid at its own line, those names are renamed
//     to this line's, and nothing else changes. The renamed answer must be
//     valid at this line: a bridged check, refused at every level, the
//     fidelity of SHN's IG-line edit. The bridge never fills.
//   - When the answer is not valid at its own line it is not renamed: it is
//     carried as sent. Whether the payer's answer conforms is the ingress's
//     own check, run before this one at the gateway's level at the lines its
//     names claim (carriedAnswerRefused), so nothing is judged twice.
//   - When the check at its own line could not run, whether to rename is
//     unknown. Nothing is renamed on a guess: the answer is carried as sent and
//     the record says so (EditNotMadeDTRCheckCouldNotRun).
//   - At none, with no lane for this gateway's DTR line (only none boots so),
//     nothing is renamed: the answer is carried exactly as it arrived.
//   - Every other answer is carried exactly as it arrived.
//
// contentType is the media type the payer's system stated, which the answer
// keeps whether it is carried as sent or renamed. routed is the contract token
// the leg ran at, and declared the tokens this gateway declares; wrote is the
// token of the line the answer is written in
// (routed when carried as sent, this gateway's line when carried across, empty
// on a refusal). status is non-zero for a refusal, with its message.
func (b chainBridge) carryDTRAnswer(ctx context.Context, answer []byte, contentType, routed string, declared []string, x ExchangeIdentity) (carried relay.Payload, wrote string, status int, msg string) {
	g := b.g
	payerLine := shnsdk.LineOf(routed)
	exact := relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), contentType)
	lines := dtrLines(declared)
	if len(lines) != 1 {
		return exact, routed, 0, ""
	}
	target := lines[0]
	// A gateway whose payload checks consume no lane boots without a lane for a
	// line it declares. With none for this line it could not certify a renamed
	// answer, so it never renames one: the answer is carried as sent. Where
	// payload checks consume lanes, a declared line always has its lane, and one
	// that has not yet qualified is renamed and then refused by Certify, which
	// cannot run without it. A lane still qualifying here is a check that could
	// not run on an answer that would have been renamed, recorded as any is.
	if lane, qualifying := g.candidateLaneAt("pa.dtr", target); !payloadChecksConsumeLanes(g.conformance().Level()) && lane == nil {
		if qualifying && len(planDTRAnswerNames(answer, target).Renames) > 0 {
			exchangeOf(ctx).editNotMade(EditNotMadeDTRCheckCouldNotRun)
		}
		return exact, routed, 0, ""
	}
	plan := planDTRAnswerNames(answer, target)
	if len(plan.Renames) == 0 {
		return exact, routed, 0, ""
	}
	fc := findingContextFrom(ctx)
	fc.Whose = "peer"
	source, evidence := g.dtrAnswerSourceLine(ctx, answer, plan.Source, payerLine)
	if evidence == dtrLineUnchecked {
		exchangeOf(ctx).editNotMade(EditNotMadeDTRCheckCouldNotRun)
	}
	if evidence != dtrAtLine {
		return exact, routed, 0, ""
	}
	// Each package's QuestionnaireResponse entries cross the lines through the
	// registered pa.dtr steps, along the chain the compat manifest defines,
	// each step reporting what it moved; a step's refusal is SHN's own edit
	// refusing, at every level.
	steps := chainFor("pa.dtr", source, target)
	if steps == nil {
		return noDTRAnswer, "", http.StatusBadGateway, dtrAnswerNotCarried(target, "no registered DTR chain from "+source)
	}
	var reports []LossReport
	stepQR := func(qr []byte) ([]byte, error) {
		out, r, err := applyChain(steps, source, qr, x)
		if err != nil {
			return nil, err
		}
		reports = append(reports, r...)
		return out, nil
	}
	renamed, err := renameDTRAnswer(answer, contentType, plan, stepQR)
	if err != nil {
		var refused *dtrQRStepError
		if errors.As(err, &refused) {
			g.observe(ObserverEvent{Kind: "leg.failed", Direction: "answer", LegType: x.LegType,
				CorrelationID: x.CorrelationID, Counterpart: x.Counterpart, Detail: refused.err.Error()})
			return noDTRAnswer, "", adaptFailureStatus(refused.err), dtrAnswerNotCarried(target, refused.err.Error())
		}
		return noDTRAnswer, "", http.StatusBadGateway, dtrAnswerNotCarried(target, "its parameter names could not be read")
	}
	sent, err := relay.Transmit(renamed, relay.Check(dtrAnswerKey))
	if err != nil {
		return noDTRAnswer, "", http.StatusBadGateway, dtrAnswerNotCarried(target, "its parameter names could not be read")
	}
	fc.Edit = relay.EditDTRAnswerLineNames
	fc.SourceSHA256 = sha256hex(answer)
	r := b.Certify(withFindingContext(ctx, fc), "pa.dtr", target, dtrOutputParametersProfile, sent)
	if r.Status != 0 {
		// A check that judged the answer invalid gives its issues; one that could
		// not run (no lane for the line, a lane not ready, a validator that did not
		// answer) refuses too, but says so: the answer was never judged.
		reason := "the check at that line could not run: " + r.Msg
		if r.Status == http.StatusUnprocessableEntity {
			reason = "it is not valid at that line once carried across"
			if len(r.Issues) > 0 {
				reason += ": " + strings.Join(r.Issues, "; ")
			}
		}
		return noDTRAnswer, "", r.Status, dtrAnswerNotCarried(target, reason)
	}
	if len(reports) > 0 {
		ev := transformedObserverEvent(reports)
		ev.Direction, ev.CorrelationID, ev.LegType, ev.Counterpart = "answer", x.CorrelationID, x.LegType, x.Counterpart
		if err := g.recordTransformed(ev, x, "pa.dtr", source, target, steps, reports, answer, sent); err != nil {
			return noDTRAnswer, "", http.StatusBadGateway, dtrAnswerNotCarried(target, err.Error())
		}
	}
	return renamed, "pa.dtr@" + target, 0, ""
}

// dtrQRStepError is a registered pa.dtr step refusing a package's
// QuestionnaireResponse.
type dtrQRStepError struct{ err error }

func (e *dtrQRStepError) Error() string { return e.err.Error() }
func (e *dtrQRStepError) Unwrap() error { return e.err }

// dtrAnswerNotCarried is the refusal of an answer E-08 could not carry: the
// payer answered, and its answer could not be carried into the requester's
// DTR line, for reason. The network did not fail and nothing was lost in
// transport; the refusal is the fidelity of SHN's IG-line edit.
func dtrAnswerNotCarried(target, reason string) string {
	return "the payer answered, but its questionnaire package answer cannot be carried into this gateway's DTR " +
		target + " line (its IG-line edit E-08 never fills content): " + reason
}

// renameDTRAnswer applies plan's renames to answer as E-08: one replaced string
// value per renamed parameter's name, and, inside each renamed package
// parameter's Bundle, each QuestionnaireResponse entry's resource replaced by
// stepQR's output for its own bytes. Nothing else changes: every other byte,
// every other entry included, stays the payer's.
func renameDTRAnswer(answer []byte, contentType string, plan dtrAnswerPlan, stepQR func([]byte) ([]byte, error)) (relay.Payload, error) {
	body := relay.NewBody(answer, relay.OriginPeerFrame)
	doc, err := relay.Doc(body)
	if err != nil {
		return noDTRAnswer, err
	}
	arr, ok := doc.Member(doc.Root(), "parameter")
	if !ok {
		return noDTRAnswer, errDTRAnswerShape
	}
	elems := doc.Elems(arr)
	ops := make([]relay.Op, 0, len(plan.Renames))
	for _, rn := range plan.Renames {
		if rn.Index >= len(elems) {
			return noDTRAnswer, errDTRAnswerShape
		}
		name, ok := doc.Member(elems[rn.Index], "name")
		if !ok {
			return noDTRAnswer, errDTRAnswerShape
		}
		if got, err := doc.StringValue(name); err != nil || got != rn.From {
			return noDTRAnswer, errDTRAnswerShape
		}
		value, _ := json.Marshal(rn.To)
		ops = append(ops, doc.Replace(name, value))
		if _, pkg := dtrPackageNames[rn.From]; !pkg {
			continue
		}
		bundle, ok := doc.Member(elems[rn.Index], "resource")
		if !ok {
			continue
		}
		entries, ok := doc.Member(bundle, "entry")
		if !ok {
			continue
		}
		for _, entry := range doc.Elems(entries) {
			res, ok := doc.Member(entry, "resource")
			if !ok {
				continue
			}
			rt, ok := doc.Member(res, "resourceType")
			if !ok {
				continue
			}
			if t, err := doc.StringValue(rt); err != nil || t != "QuestionnaireResponse" {
				continue
			}
			start, end := doc.Span(res)
			out, err := stepQR(answer[start:end])
			if err != nil {
				return noDTRAnswer, &dtrQRStepError{err: err}
			}
			ops = append(ops, doc.Replace(res, out))
		}
	}
	return relay.Apply(body, contentType, relay.EditDTRAnswerLineNames, ops...)
}

// dtrPackageNames are the package parameter's line-specific names.
var dtrPackageNames = map[string]struct{}{"PackageBundle": {}, "packagebundle": {}}

// errDTRAnswerShape is an answer whose parameter names do not read as the
// plan read them.
var errDTRAnswerShape = errors.New("the answer's parameter names do not read as planned")

// noDTRAnswer is the payload of an answer refused or not built: there is none.
var noDTRAnswer relay.Payload

// dtrAnswerKey is the transmit E-08 applies at: the requester's gateway
// answering its participant's system.
var dtrAnswerKey = relay.Key{Leg: "dtr-questionnaire-fetch", Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered}
