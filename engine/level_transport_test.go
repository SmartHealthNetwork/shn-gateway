//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Per-level rows for checks that sit beside routing: a Claim insurer
// reference that resolves to nothing, an answer whose frame stamps a contract
// line other than the one its leg was routed on, a payer-side CRD request with
// no Coverage, and a payer-side PAS request with no Coverage. Where the check
// judges the participant's own content it is not checked at none, recorded at
// observe and refused at strict with the status and body strict has always
// given, and the message is carried or relayed exactly below strict. Where it
// addresses the message (routing) or is SHN's own frame metadata (the
// contract-line stamp) it refuses at every level. A request the network routed
// to a payer is already addressed: the payer's gateway carries its payer
// identifiers to the payer's system exactly as sent.

// bundleWithout is a Bundle with the entries of the named resource types
// removed.
func bundleWithout(t *testing.T, body []byte, types ...string) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	var kept []any
	for _, e := range b["entry"].([]any) {
		rt := e.(map[string]any)["resource"].(map[string]any)["resourceType"]
		drop := false
		for _, want := range types {
			drop = drop || rt == want
		}
		if !drop {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(b["entry"].([]any)) {
		t.Fatalf("fixture: nothing of %v removed", types)
	}
	b["entry"] = kept
	return mustJSON(t, b)
}

// withReplaced replaces old with new once in body, which must change.
func withReplaced(t *testing.T, body, old, new string) []byte {
	t.Helper()
	out := strings.Replace(body, old, new, 1)
	if out == body {
		t.Fatalf("fixture: %q not replaced", old)
	}
	return []byte(out)
}

// ---- a Claim insurer reference that does not resolve ----

// A Claim's insurer is the requester's own content, and a payer's gateway
// does not read it: an insurer reference that resolves to no entry, or to
// several, reaches the payer's system exactly as sent at every level, its
// answer is relayed exactly, and nothing is recorded.
func TestLevelPayerPAS_UnresolvedInsurerCarriedAsSent(t *testing.T) {
	t.Parallel()
	// The amendment fixture names its payer's own entry; one mutation points its
	// operative Claim's insurer at Organization/payer, which no entry of the
	// bundle is.
	update, related := updateBundle(t)
	update = withReplaced(t, string(update), `"insurer":{"reference":"Organization/org-cms-payer"}`, `"insurer":{"reference":"Organization/payer"}`)
	dup := `{"resource":{"resourceType":"Organization","id":"dup"}}`
	rows := map[string]struct {
		leg, path string
		body      []byte
	}{
		"submit, an insurer naming no entry": {"pas-claim", pasSubmitPath,
			[]byte(pasIngressBundle("00001", `"insurer":{"reference":"Organization/absent-insurer"},`))},
		"submit, an insurer naming two entries": {"pas-claim", pasSubmitPath,
			withReplaced(t, pasIngressBundle("00001", `"insurer":{"reference":"Organization/dup"},`), `"entry":[`, `"entry":[`+dup+`,`+dup+`,`)},
		"amendment, an insurer naming no entry": {"pas-claim-update", pasSubmitPath, update},
		"inquiry, an insurer naming no entry": {"pas-claim-inquire", pasInquirePath,
			withReplaced(t, levelInquiry(t, "MBR-COVERED", ""), `"use":"preauthorization",`, `"use":"preauthorization","insurer":{"reference":"Organization/absent-insurer"},`)},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				if row.leg == "pas-claim-update" {
					p.seedPend(t, related)
				}
				got := p.send(t, row.leg, "", row.body)
				if got.status != http.StatusOK || !got.framed {
					t.Fatalf("at %s the request must be forwarded: answer %d %s", level, got.status, got.body)
				}
				if p.partner.lastPath != row.path || !bytes.Equal(p.partner.lastBody, row.body) {
					t.Fatalf("at %s the participant's system must receive the request as sent at %s; got %s:\n%s", level, row.path, p.partner.lastPath, p.partner.lastBody)
				}
				if want := p.partner.respByPath[row.path]; !bytes.Equal(got.body, want) {
					t.Fatalf("at %s the answer must be relayed exactly:\n got %s\nwant %s", level, got.body, want)
				}
				// The insurer reference was read only by the payer identity
				// mapping (its rule, claim.insurer, is retired with it).
				for _, f := range p.content() {
					if f.Rule == "claim.insurer" {
						t.Fatalf("at %s an insurer finding was recorded: %v", level, findingsText(p.content()))
					}
				}
			})
		}
	}
}

// ---- an answer stamped with another contract line ----

// stampedAnswer frames answer as a 2xx answer whose contract-version stamp
// names a line of routed's contract other than routed.
func stampedAnswer(t *testing.T, routed string, answer []byte) []byte {
	t.Helper()
	contract, line, ok := strings.Cut(routed, "@")
	if !ok {
		t.Fatalf("routed token %q names no line", routed)
	}
	other := ""
	for _, l := range []string{"2.0", "2.1", "2.2"} {
		if l != line {
			other = contract + "@" + l
			break
		}
	}
	frame, err := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, map[string]string{
		"Content-Type":                    "application/fhir+json",
		shnsdk.FrameHeaderContractVersion: other,
	}, answer)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// routedToken is the line the provider ingress routes leg on: the route
// selected before the build (selectLegRoute) for the CRD and DTR ingress,
// the intersection token OriginateLeg fills in for the PAS ingress.
func routedToken(t *testing.T, env *inProcessExchange, leg string) string {
	t.Helper()
	switch leg {
	case "pas-claim", "pas-claim-inquire":
		tok, err := env.originator.selectLegToken(env.payerID, leg)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	route, err := env.originator.selectLegRoute(env.payerID, leg, "")
	if err != nil {
		t.Fatal(err)
	}
	return route.Token
}

const stampMismatch = "response contract version mismatch"

// stampRow is one provider ingress driven with an answer stamped with another
// line than its leg routed.
type stampRow struct {
	leg    string
	answer func(*testing.T) []byte
	setup  func(*testing.T, *inProcessExchange)
	send   func(*testing.T, *inProcessExchange) *httptest.ResponseRecorder
}

// providerIngressStampRows are the provider ingresses an answer reaches.
func providerIngressStampRows() map[string]stampRow {
	dtrBody := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	return map[string]stampRow{
		"PAS submit": {"pas-claim", func(*testing.T) []byte { return levelPASPayerAnswer }, nil,
			func(t *testing.T, env *inProcessExchange) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				env.originator.handlePASIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", ""))))
				return rec
			}},
		"PAS inquiry": {"pas-claim-inquire", levelInquiryPayerAnswer, nil,
			func(t *testing.T, env *inProcessExchange) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				env.originator.handlePASInquireIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$inquire", strings.NewReader(levelInquiry(t, "MBR-COVERED", ""))))
				return rec
			}},
		"CRD": {"crd-order-select", realCRDAnswer, nil,
			func(t *testing.T, env *inProcessExchange) *httptest.ResponseRecorder {
				return ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			}},
		"DTR": {dtrLeg, func(*testing.T) []byte { return packageAnswer },
			func(t *testing.T, env *inProcessExchange) {
				env.originator.cfg.SoR = newPrefetchSoR().sor()
				declareFramedDTR(t, env, true)
			},
			func(t *testing.T, env *inProcessExchange) *httptest.ResponseRecorder {
				return postDTRIngress(env, dtrBody)
			}},
	}
}

// The contract-line stamp on an answer's frame is SHN's own frame metadata,
// set only on an answer an SHN gateway built. An answer stamped with a line
// other than the one its leg was routed on is relayed and recorded by default
// (TestLevelProviderIngress_AnswerStampedWithAnotherLineIsRelayedAtEveryLevel).
// Under the participant's own setting (RequireSharedLine) it refuses at every
// level with the 502 strict has always given, on every provider ingress
// (including those that relay the answer exactly), and records no content
// finding.
func TestLevelProviderIngress_AnswerStampedWithAnotherLineRefusesAtEveryLevel(t *testing.T) {
	t.Parallel()
	rows := providerIngressStampRows()
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				env.originator.cfg.RequireSharedLine = true
				if row.setup != nil {
					row.setup(t, env)
				}
				answer := row.answer(t)
				env.payerReturns(LegResult{Response: testResponse(stampedAnswer(t, routedToken(t, env, row.leg), answer))})
				ev := observeLevelEvents(env.originator)
				rec := row.send(t, env)
				if env.routeHitCount() != 1 {
					t.Fatalf("the request must be carried once, network hits %d", env.routeHitCount())
				}
				// The payer answered, so it may have acted: the refusal says
				// so, and to check before resending, as every refusal of an
				// answer does.
				if !strings.Contains(rec.Body.String(), "the recipient received this request and answered") ||
					!strings.Contains(rec.Body.String(), "it may have acted on the request: check its outcome before resending") {
					t.Errorf("the refusal does not say the payer may have acted: %s", rec.Body)
				}
				if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), stampMismatch) {
					t.Fatalf("answer %d %s, want 502 %q", rec.Code, rec.Body.String(), stampMismatch)
				}
				if o := lastLegOutcome(t, env.originator); o != "error" {
					t.Fatalf("the leg is recorded error, got %q", o)
				}
				if len(ev.findings) != 0 || len(ev.skipped) != 0 {
					t.Fatalf("a network rule records no finding and skips nothing: %+v %+v", ev.findings, ev.skipped)
				}
			})
		}
	}
}

// A caller that reads the answer at the routed line (this gateway's own
// workflow, which parses the answer and builds its next leg from it) cannot
// read an answer declaring another line faithfully: the mismatch refuses at
// every level and records no content finding.
func TestLevelOriginate_AnswerStampedWithAnotherLineRefusesAtEveryLevel(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.RequireSharedLine = true
			ev := observeLevelEvents(env.originator)
			sealBare(env, stampedAnswer(t, "pa.crd@2.0", []byte(`{"cards":[]}`)))
			_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "",
				Content{WorkstreamType: workstreamPA, ProfileID: "pa.crd@2.0", Payload: testRequest(env.crdReq)})
			if err == nil || !strings.Contains(err.Error(), stampMismatch) {
				t.Fatalf("want %q at %s, got %v", stampMismatch, level, err)
			}
			if len(ev.findings) != 0 {
				t.Fatalf("a mismatch read at the routed line records no content finding, got %+v", ev.findings)
			}
		})
	}
}

// ---- a CRD request with no Coverage ----

// levelCRDSelectCoverage is conformantCRD's prefetch coverage member, and
// levelCRDDispatchCoverage crdDispatchRequest's.
const (
	levelCRDSelectCoverage   = `"coverage":{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"Patient/MBR-COVERED"}}`
	levelCRDDispatchCoverage = `,"coverage":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[{"reference":"Organization/o2"}]}}]}`
	levelEmptySearchset      = `{"resourceType":"Bundle","type":"searchset","total":0,"entry":[]}`
)

// crdSelectWithoutPrefetch is conformantCRD with no prefetch member at all.
func crdSelectWithoutPrefetch(t *testing.T) []byte {
	t.Helper()
	body := conformantCRD("MBR-COVERED", "72148")
	i := bytes.Index(body, []byte(",\n      \"prefetch\":{"))
	j := bytes.LastIndex(body, []byte("\n    }"))
	if i < 0 || j < i {
		t.Fatal("fixture: prefetch not found")
	}
	out := slices.Concat(body[:i], body[j:])
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if _, ok := probe["prefetch"]; ok {
		t.Fatal("fixture: prefetch still present")
	}
	return out
}

// A CRD request that carries no Coverage at all is already routed to this
// payer: its payer's own conformance level decides it. order-select reads its
// subject's coverage beneficiary as the request's own shape
// (RuleRequestShape), so a payer that opts in to structural or strict refuses
// it there; at none and observe it reaches the payer's system byte for byte.
// order-dispatch reads no coverage it requires, so it is carried as sent at
// every level.
func TestLevelPayerCRD_NoCoverageCarriedAsSent(t *testing.T) {
	t.Parallel()
	sel := map[string][]byte{
		"no prefetch.coverage":   crdSelectWith(t, levelCRDSelectCoverage, `"other":{}`),
		"prefetch.coverage null": crdSelectWith(t, levelCRDSelectCoverage, `"coverage":null`),
		"an empty searchset":     crdSelectWith(t, levelCRDSelectCoverage, `"coverage":`+levelEmptySearchset),
		"no prefetch":            crdSelectWithoutPrefetch(t),
	}
	for name, body := range sel {
		for _, level := range allLevels {
			t.Run(fmt.Sprintf("order-select/%s/%s", name, level), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "crd-order-select", "", body)
				p.wantRequestRow(t, "crd-order-select", got, body, crdSelectPath, RuleRequestShape, http.StatusBadRequest, "parse coverage beneficiary failed")
			})
		}
	}
	dispatch := map[string][]byte{
		"no prefetch.coverage":   crdDispatchWith(t, levelCRDDispatchCoverage, ``),
		"prefetch.coverage null": crdDispatchWith(t, levelCRDDispatchCoverage, `,"coverage":null`),
		"an empty searchset":     crdDispatchWith(t, levelCRDDispatchCoverage, `,"coverage":`+levelEmptySearchset),
	}
	for name, body := range dispatch {
		for _, level := range allLevels {
			t.Run("order-dispatch/"+name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "crd-order-dispatch", "", body)
				if got.status != http.StatusOK || !got.framed {
					t.Fatalf("at %s the request must be carried: answer %d %s", level, got.status, got.body)
				}
				if p.partner.lastPath != crdDispatchPath || !bytes.Equal(p.partner.lastBody, body) {
					t.Fatalf("at %s the payer's system must receive the request as sent at %s, got %s:\n%s", level, crdDispatchPath, p.partner.lastPath, p.partner.lastBody)
				}
				if want := p.partner.respByPath[crdDispatchPath]; !bytes.Equal(got.body, want) {
					t.Fatalf("at %s the answer must be relayed exactly:\n got %s\nwant %s", level, got.body, want)
				}
				if fs := p.content(); len(fs) != 0 {
					t.Fatalf("at %s nothing is recorded, got %v", level, findingsText(fs))
				}
			})
		}
	}
}

// ---- a PAS request with no Coverage ----

// levelPASCoverageEntry is the Coverage entry of pasIngressBundle.
const levelPASCoverageEntry = `,{"resource":{"resourceType":"Coverage","id":"cov1","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}}`

// A request routed to this payer is already addressed: a bundle with no
// Coverage is the request's own shape.
func TestLevelPayerPAS_NoCoverage(t *testing.T) {
	t.Parallel()
	update, related := updateBundle(t, "Coverage")
	rows := map[string]struct {
		leg  string
		body []byte
	}{
		"submit":    {"pas-claim", []byte(levelPASBundleWithout(t, levelPASCoverageEntry))},
		"amendment": {"pas-claim-update", update},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				if row.leg == "pas-claim-update" {
					p.seedPend(t, related)
				}
				got := p.send(t, row.leg, "", row.body)
				p.wantRequestRow(t, row.leg, got, row.body, pasSubmitPath, RuleRequestShape, http.StatusBadRequest, "PAS bundle carries no Coverage")
			})
		}
	}
}

// The provider ingress routes a PAS request by its Coverage's payor: a bundle
// with no Coverage refuses at every level, before the network.
func TestLevelProviderPAS_NoCoverageRefusesAtEveryLevel(t *testing.T) {
	t.Parallel()
	update, _ := updateBundle(t, "Coverage")
	rows := map[string]struct {
		path   string
		body   []byte
		status int
		msg    string
	}{
		"submit":    {"/Claim/$submit", []byte(levelPASBundleWithout(t, levelPASCoverageEntry)), http.StatusBadRequest, "PAS bundle carries no Coverage"},
		"amendment": {"/Claim/$submit", update, http.StatusBadRequest, "PAS bundle carries no Coverage"},
		"inquiry":   {"/Claim/$inquire", bundleWithout(t, []byte(levelInquiry(t, "MBR-COVERED", "")), "Coverage"), http.StatusUnprocessableEntity, "no payer identifier on member coverage"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				ev := observeLevelEvents(env.originator)
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, row.path, bytes.NewReader(row.body))
				if row.path == "/Claim/$inquire" {
					env.originator.handlePASInquireIngress(rec, req)
				} else {
					env.originator.handlePASIngress(rec, req)
				}
				refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
				if len(ev.findings) != 0 {
					t.Fatalf("routing records no content finding, got %+v", ev.findings)
				}
			})
		}
	}
}
