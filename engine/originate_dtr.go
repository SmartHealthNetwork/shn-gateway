// originate_dtr.go — the $questionnaire-package request this gateway
// originates for its own participant after a CDS Hooks answer, built from the
// participant's own records and the payer's own answer.
package engine

import (
	"bytes"
	"fmt"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// searchsetMatches returns the resources of the match entries of a searchset
// the gateway assembled (sor-searchset), each exactly as the system of record
// returned it.
func searchsetMatches(bundle []byte, resourceType string) ([][]byte, error) {
	doc, err := relay.Doc(relay.NewBody(bundle, relay.OriginUpstreamResponse))
	if err != nil {
		return nil, err
	}
	var out [][]byte
	entries, ok := doc.Member(doc.Root(), "entry")
	if !ok {
		return nil, nil
	}
	for _, e := range doc.Elems(entries) {
		res, ok := doc.Member(e, "resource")
		if !ok || docText(doc, res, "resourceType") != resourceType {
			continue
		}
		if search, ok := doc.Member(e, "search"); !ok || docText(doc, search, "mode") != "match" {
			continue
		}
		s, end := doc.Span(res)
		out = append(out, bundle[s:end])
	}
	return out, nil
}

// originatedPackageRequest builds the $questionnaire-package input the
// gateway sends at DTR line for its own workflow:
//   - every Coverage the system of record's search matched (recs, the patient
//     named by the member id), each embedded byte for byte;
//   - order, the order as the payer returned it with its coverage information
//     (or as sent when the payer returned none), embedded byte for byte;
//   - the questionnaire canonical exactly as the payer stated it (a |version
//     kept);
//   - the payer's coverage-assertion-id as context, when it gave one.
//
// It returns the request's bytes and the request sealed as the gateway's own
// message with every embed verified.
func originatedPackageRequest(line string, recs crdOriginRecords, order []byte, canonical, assertionID string) (body []byte, sealed relay.Payload, err error) {
	coverages, err := searchsetMatches(recs.coverage, "Coverage")
	if err != nil {
		return nil, sealed, fmt.Errorf("read the coverage search result: %w", err)
	}
	in := shnsdk.QuestionnairePackageInputs{
		Coverages:      coverages,
		Orders:         [][]byte{order},
		Questionnaires: []string{canonical},
		Context:        assertionID,
	}
	pkg, err := shnsdk.BuildQuestionnairePackageParameters(line, in)
	if err != nil {
		return nil, sealed, err
	}
	embeds := make([]relay.Embed, 0, len(pkg.Copied))
	for _, c := range pkg.Copied {
		var src relay.Body
		switch {
		case c.Parameter == "coverage" && c.Index < len(in.Coverages):
			src = relay.NewBody(in.Coverages[c.Index], relay.OriginUpstreamResponse)
		case c.Parameter == "order" && c.Index < len(in.Orders):
			src = relay.NewBody(in.Orders[c.Index], relay.OriginPeerFrame)
		default:
			return nil, sealed, fmt.Errorf("questionnaire-package request copies an unknown input %s[%d]", c.Parameter, c.Index)
		}
		embeds = append(embeds, relay.Embed{Source: src, Start: c.Start, End: c.End, At: c.At})
	}
	sealed, err = relay.Authored(relay.BuilderSDKDTRPackage, pkg.Body, dtrPackageContentType, embeds...)
	return pkg.Body, sealed, err
}

// carryUnchanged walks a questionnaire request the gateway built to the
// payer's line (route): the walk changes no byte of it, and a walk that would
// change one is refused (502) rather than sent.
func (g *Gateway) carryUnchanged(route legRoute, body []byte, correlationID, recipient string) (int, string) {
	adapted, _, err := g.egressAdapt(route, body, ExchangeIdentity{CorrelationID: correlationID, LegType: "dtr-questionnaire-fetch", Counterpart: recipient})
	if err != nil {
		return http.StatusBadGateway, err.Error()
	}
	if !bytes.Equal(adapted, body) {
		return http.StatusBadGateway, "the questionnaire-package request cannot be carried to the payer's line unchanged"
	}
	return 0, ""
}
