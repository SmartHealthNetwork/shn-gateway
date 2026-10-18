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

// declaredDTRLines are the DTR lines this gateway declares, in its declared
// order.
func (g *Gateway) declaredDTRLines() []string {
	var lines []string
	for _, tok := range g.declaredContractVersions() {
		contract, version, ok := strings.Cut(tok, "@")
		if ok && contract == "pa.dtr" && !slices.Contains(lines, version) {
			lines = append(lines, version)
		}
	}
	return lines
}

// dtrAnswerValidAt reports whether answer is valid at line against the DTR
// output Parameters profile, read as SHN's own evidence for E-08, not as a
// judgment of the payer's answer: it runs at every conformance level, records
// no finding, and anything short of a valid result (an invalid one, a check
// that could not run, no lane for the line) is no evidence.
func (g *Gateway) dtrAnswerValidAt(ctx context.Context, answer []byte, line string) bool {
	v := g.validatorForContractLine("pa.dtr", line)
	if v == nil {
		return false
	}
	req, profile := validateRequest(answer, dtrOutputParametersProfile)
	res, err := v.Validate(ctx, req, profile)
	return err == nil && res.Valid
}

// dtrAnswerSourceLine is the line of answer's own name family it validates at,
// trying the line the payer was routed at first. "" when it validates at none.
func (g *Gateway) dtrAnswerSourceLine(ctx context.Context, answer []byte, family dtrNameFamily, payerLine string) (string, bool) {
	lines := dtrFamilyLines(family)
	if i := slices.Index(lines, payerLine); i > 0 {
		lines = append([]string{payerLine}, slices.Delete(slices.Clone(lines), i, i+1)...)
	}
	for _, line := range lines {
		if g.dtrAnswerValidAt(ctx, answer, line) {
			return line, true
		}
	}
	return lines[0], false
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
//     carried as sent, and the governed check the gateway's own DTR fetch
//     applies to a payer's package answer reports it (no finding at none,
//     recorded at observe and structural, refused at strict).
//   - Every other answer is carried exactly as it arrived.
//
// routed is the contract token the leg ran at; wrote is the token of the line
// the answer is written in (routed when carried as sent, this gateway's line
// when carried across, empty on a refusal). status is non-zero for a refusal,
// with its message.
func (g *Gateway) carryDTRAnswer(ctx context.Context, answer []byte, routed string, x ExchangeIdentity) (carried relay.Payload, wrote string, status int, msg string) {
	const contentType = "application/fhir+json"
	payerLine := shnsdk.LineOf(routed)
	exact := relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), contentType)
	lines := g.declaredDTRLines()
	if len(lines) != 1 {
		return exact, routed, 0, ""
	}
	target := lines[0]
	plan := planDTRAnswerNames(answer, target)
	if len(plan.Renames) == 0 {
		return exact, routed, 0, ""
	}
	fc := findingContextFrom(ctx)
	fc.Whose = "peer"
	source, valid := g.dtrAnswerSourceLine(ctx, answer, plan.Source, payerLine)
	if !valid {
		if status, msg := g.validateGoverned(withFindingContext(ctx, fc), fc, g.validatorForContractLine("pa.dtr", source),
			answer, "ingress", source, dtrOutputParametersProfile, false).refusal(); status != 0 {
			return noDTRAnswer, "", status, msg
		}
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
	b, err := relay.Transmit(renamed, relay.Check(dtrAnswerKey))
	if err != nil {
		return noDTRAnswer, "", http.StatusBadGateway, dtrAnswerNotCarried(target, "its parameter names could not be read")
	}
	fc.Edit = relay.EditDTRAnswerLineNames
	fc.SourceSHA256 = sha256hex(answer)
	r := g.validateGoverned(withFindingContext(ctx, fc), fc, g.validatorForContractLine("pa.dtr", target),
		b, "egress", target, dtrOutputParametersProfile, true)
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
		if err := g.recordTransformed(ev, x, "pa.dtr", source, target, steps, reports, answer, b); err != nil {
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
