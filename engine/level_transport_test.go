package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Per-level rows for three checks that sit beside routing and the payer
// identity mapping: a Claim insurer reference the mapping cannot resolve, an
// answer whose frame stamps a contract line other than the one its leg was
// routed on, and a payer-side PAS request with no Coverage. Where the check
// judges the participant's own content it is not checked at none, recorded at
// observe and refused at strict with the status and body strict has always
// given, and the message is carried or relayed exactly below strict. Where it
// addresses the message (routing, the mapping's Coverage) or is SHN's own
// frame metadata (the contract-line stamp) it refuses at every level.

var (
	levelOwnPayer     = shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00001"}
	levelBackendPayer = shnsdk.PayerIdentifier{System: "urn:example:payer-backend", Value: "BACKEND-7"}
)

// withIdentityMapping configures the payer identity mapping from the payer id
// the fixtures' Coverages name to the identity the payer's own system expects.
func withIdentityMapping() []NativeOption {
	return []NativeOption{WithPayorEdgeIdentity(levelOwnPayer, levelBackendPayer)}
}

// mappedPayor is body with its one payer identifier naming levelOwnPayer
// replaced by levelBackendPayer: the request the participant's system
// receives when the mapping maps the Coverage and nothing else.
func mappedPayor(t *testing.T, body []byte) []byte {
	t.Helper()
	own := []byte(`{"system":"` + levelOwnPayer.System + `","value":"` + levelOwnPayer.Value + `"}`)
	if n := bytes.Count(body, own); n != 1 {
		t.Fatalf("fixture: want one own payer identifier, found %d", n)
	}
	return bytes.Replace(body, own, []byte(`{"system":"`+levelBackendPayer.System+`","value":"`+levelBackendPayer.Value+`"}`), 1)
}

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

// The payer identity mapping maps a Claim's insurer only when it names this
// payer; the payer's system is addressed by the Coverages. An insurer
// reference that resolves to no entry, or to several, is the requester's own
// content: below strict the insurer is left as sent and the Coverage is
// mapped as usual; strict refuses it with the mapping's 422.
func TestLevelPayerPAS_UnresolvedInsurer(t *testing.T) {
	// The amendment fixture's own Claim.insurer names Organization/payer,
	// which no entry of the bundle is.
	update, related := updateBundle(t)
	if !bytes.Contains(update, []byte(`"insurer":{"reference":"Organization/payer"}`)) {
		t.Fatal("fixture: the amendment's insurer changed")
	}
	dup := `{"resource":{"resourceType":"Organization","id":"dup"}}`
	rows := map[string]struct {
		leg, path string
		body      []byte
		msg       string
	}{
		"submit, an insurer naming no entry": {"pas-claim", pasSubmitPath,
			[]byte(pasIngressBundle("00001", `"insurer":{"reference":"Organization/absent-insurer"},`)),
			`Organization/absent-insurer\" resolves to no resource in the request`},
		"submit, an insurer naming two entries": {"pas-claim", pasSubmitPath,
			withReplaced(t, pasIngressBundle("00001", `"insurer":{"reference":"Organization/dup"},`), `"entry":[`, `"entry":[`+dup+`,`+dup+`,`),
			`Organization/dup\" resolves to more than one resource in the request`},
		"amendment, an insurer naming no entry": {"pas-claim-update", pasSubmitPath, update,
			`Organization/payer\" resolves to no resource in the request`},
		"inquiry, an insurer naming no entry": {"pas-claim-inquire", pasInquirePath,
			withReplaced(t, levelInquiry(t, "MBR-COVERED", ""), `"use":"preauthorization",`, `"use":"preauthorization","insurer":{"reference":"Organization/absent-insurer"},`),
			`Organization/absent-insurer\" resolves to no resource in the request`},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level, withIdentityMapping()...)
				if row.leg == "pas-claim-update" {
					p.seedPend(t, related)
				}
				got := p.send(t, row.leg, "", row.body)
				if refusesAt(level, RuleInsurer) {
					p.wantRefused(t, got, http.StatusUnprocessableEntity, row.msg)
				} else {
					if got.status != http.StatusOK || !got.framed {
						t.Fatalf("at %s the request must be forwarded: answer %d %s", level, got.status, got.body)
					}
					if want := mappedPayor(t, row.body); p.partner.lastPath != row.path || !bytes.Equal(p.partner.lastBody, want) {
						t.Fatalf("at %s the participant's system must receive the request with only its Coverage mapped, at %s; got %s:\n%s\nwant\n%s", level, row.path, p.partner.lastPath, p.partner.lastBody, want)
					}
					if want := p.partner.respByPath[row.path]; !bytes.Equal(got.body, want) {
						t.Fatalf("at %s the answer must be relayed exactly:\n got %s\nwant %s", level, got.body, want)
					}
				}
				p.wantFindings(t, row.leg, RuleInsurer, "peer")
			})
		}
	}
}

// The mapping's own addressing refuses at every level: a Coverage payor
// reference that resolves to no entry.
func TestLevelPayerPAS_CoveragePayorReferenceRefusesAtEveryLevel(t *testing.T) {
	runPayerNetworkRows(t, map[string]payerNetworkRow{
		"a Coverage payor naming no entry": {leg: "pas-claim", opts: withIdentityMapping(),
			body: withReplaced(t, pasIngressBundle("00001", ""),
				`"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]`,
				`"payor":[{"reference":"Organization/absent"}]`),
			status: http.StatusUnprocessableEntity, msg: `Organization/absent\" resolves to no resource in the request`},
	})
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
	route, err := env.originator.selectLegRoute(env.payerID, leg)
	if err != nil {
		t.Fatal(err)
	}
	return route.Token
}

const stampMismatch = "response contract version mismatch"

// The contract-line stamp on an answer's frame is SHN's own frame metadata,
// set only on an answer an SHN gateway built: a network rule. An answer
// stamped with a line other than the one its leg was routed on refuses at
// every level with the 502 strict has always given, on every provider ingress
// (including those that relay the answer exactly), and records no content
// finding.
func TestLevelProviderIngress_AnswerStampedWithAnotherLineRefusesAtEveryLevel(t *testing.T) {
	dtrBody := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	rows := map[string]struct {
		leg    string
		answer func(*testing.T) []byte
		setup  func(*testing.T, *inProcessExchange)
		send   func(*testing.T, *inProcessExchange) *httptest.ResponseRecorder
	}{
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
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
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

// ---- a PAS request with no Coverage ----

// levelPASCoverageEntry is the Coverage entry of pasIngressBundle.
const levelPASCoverageEntry = `,{"resource":{"resourceType":"Coverage","id":"cov1","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}}`

// A request routed to this payer is already addressed; without the payer
// identity mapping a bundle with no Coverage is the request's own shape.
func TestLevelPayerPAS_NoCoverage(t *testing.T) {
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
				p.wantRequestRow(t, row.leg, got, row.body, pasSubmitPath, RuleRequestShape, http.StatusBadRequest, "PAS bundle missing Coverage.beneficiary")
			})
		}
	}
}

// With the payer identity mapping configured, the payer's own system is
// addressed by the Coverage's payor: a bundle with no Coverage refuses at
// every level.
func TestLevelPayerPAS_NoCoverageWithIdentityMappingRefusesAtEveryLevel(t *testing.T) {
	update, _ := updateBundle(t, "Coverage")
	runPayerNetworkRows(t, map[string]payerNetworkRow{
		"submit": {leg: "pas-claim", body: []byte(levelPASBundleWithout(t, levelPASCoverageEntry)), opts: withIdentityMapping(),
			status: http.StatusBadRequest, msg: "PAS bundle missing Coverage.beneficiary"},
		"amendment": {leg: "pas-claim-update", body: update, opts: withIdentityMapping(),
			status: http.StatusBadRequest, msg: "PAS bundle missing Coverage.beneficiary"},
		"inquiry": {leg: "pas-claim-inquire", body: bundleWithout(t, []byte(levelInquiry(t, "MBR-COVERED", "")), "Coverage"), opts: withIdentityMapping(),
			status: http.StatusBadRequest, msg: "inbound Coverage carries no resolvable payor identifier"},
	})
}

// The native forward reads the bundle again for the member and the order; with
// the payer identity mapping configured its read refuses a bundle with no
// Coverage at every level too, with the same status and body, and nothing is
// sent to the payer's system.
func TestLevelPayerPASResponder_NoCoverageWithIdentityMappingRefusesAtEveryLevel(t *testing.T) {
	body := []byte(levelPASBundleWithout(t, levelPASCoverageEntry))
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newStubPartner(t)
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, fixedClock,
				append([]NativeOption{WithConformancePolicy(NewConformancePolicy(level))}, withIdentityMapping()...)...)
			res, err := n.Handle(t.Context(), "pas-claim", "corr-1", "pci-1", body)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != http.StatusBadRequest || res.Message != "PAS bundle missing Coverage.beneficiary" {
				t.Fatalf("got %d %q", res.Status, res.Message)
			}
			if p.lastPath != "" {
				t.Fatalf("a refused request reached the participant's system at %s", p.lastPath)
			}
		})
	}
}

// The provider ingress routes a PAS request by its Coverage's payor: a bundle
// with no Coverage refuses at every level, before the network.
func TestLevelProviderPAS_NoCoverageRefusesAtEveryLevel(t *testing.T) {
	update, _ := updateBundle(t, "Coverage")
	rows := map[string]struct {
		path   string
		body   []byte
		status int
		msg    string
	}{
		"submit":    {"/Claim/$submit", []byte(levelPASBundleWithout(t, levelPASCoverageEntry)), http.StatusBadRequest, "PAS bundle missing Coverage.beneficiary"},
		"amendment": {"/Claim/$submit", update, http.StatusBadRequest, "PAS bundle missing Coverage.beneficiary"},
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
