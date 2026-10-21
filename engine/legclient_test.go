package engine

import (
	"bytes"
	"fmt"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// moduleOneSends are module 1's sends, one row per site (file, function,
// leg). A new one is added here deliberately, with its row in legSiteRows.
var moduleOneSends = map[string]int{
	`module1_dtr_adaptive.go:nextQuestionLeg:"dtr-questionnaire-fetch"`:        1,
	`module1_originate.go:handleScenario:"coverage-eligibility"`:               1,
	`module1_originate.go:handleUC03Bridge:"pas-claim"`:                        1,
	`module1_originate.go:handleUC04:"pas-claim"`:                              1,
	`module1_originate.go:handleUC04:"pas-claim-update"`:                       1,
	`module1_originate.go:handleUC05:"federated-query"`:                        1,
	`module1_originate.go:handleUC05:"pas-claim"`:                              1,
	`module1_originate.go:handleUC05:"pas-claim-update"`:                       1,
	`module1_originate.go:handleUC07HCPCS:"pas-claim"`:                         1,
	`module1_originate.go:handleUC08:"pas-claim"`:                              1,
	`module1_originate.go:originateNoPACRD:"crd-order-select"`:                 1,
	`module1_originate.go:runCRDThenDTROrder:"crd-order-select"`:               1,
	`module1_originate.go:runCRDThenDTROrder:"dtr-questionnaire-fetch"`:        1,
	`module1_originate_homeoxygen.go:runCRDDispatch:"crd-order-dispatch"`:      1,
	`module1_originate_homeoxygen.go:runCRDDispatch:"dtr-questionnaire-fetch"`: 1,
	`module1_originate_resume.go:completeClinician:"pas-claim-update"`:         1,
	`module1_originate_resume.go:completePatient:"pas-claim-update"`:           1,
	`module1_originate_resume.go:completePatient:"patient-dtr"`:                1,
	`module1_originate_resume.go:scenarioToPend:"pas-claim"`:                   1,
	`module1_originate_wait.go:inquireContinuation:"pas-claim-inquire"`:        1,
	`module1_pas_tail.go:submitPASClaim:"pas-claim"`:                           1,
}

// Module 1 reaches its legs only through the LegClient: OriginateLeg
// is called by sendLeg and by the core's own ingress handlers, which send a
// participant's own request on its behalf, and by nothing else. egressBuild
// and egressAdapt have one caller each (TestBridgedCheckIsModuleTwosCertify).
func TestModuleOneReachesItsLegsOnlyThroughTheLegClient(t *testing.T) {
	_, files := engineFiles(t)
	callers := map[string]int{}
	sends := map[string]int{}
	calls, bound := 0, 0
	for name, f := range files {
		base := filepath.Base(name)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					switch {
					case !ok:
					case sel.Sel.Name == "OriginateLeg":
						callers[base+":"+fn.Name.Name]++
					case sel.Sel.Name == "sendLeg" && len(n.Args) == 3:
						leg := "?"
						if lit, ok := n.Args[2].(*ast.CompositeLit); ok {
							if v, ok := keyed(lit, "Leg").(*ast.BasicLit); ok {
								leg = v.Value
							}
						}
						sends[base+":"+fn.Name.Name+":"+leg]++
						calls++
					}
				case *ast.AssignStmt:
					// A site acts on why its request got no answer; it never
					// discards it.
					if call, ok := n.Rhs[0].(*ast.CallExpr); ok && len(n.Lhs) == 2 {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "sendLeg" {
							if id, ok := n.Lhs[1].(*ast.Ident); ok && id.Name != "_" {
								bound++
							}
						}
					}
				}
				return true
			})
		}
	}
	if bound != calls {
		t.Errorf("%d of %d sendLeg calls bind their failure: a call that drops it acts on nothing the leg said", bound, calls)
	}
	if fmt.Sprint(sends) != fmt.Sprint(moduleOneSends) {
		t.Errorf("module 1 sends (file:function:leg) = %v, want %v", sends, moduleOneSends)
	}
	want := map[string]int{
		"legclient.go:sendLeg": 1,
		// The core's ingress: a participant's own request, sent for it.
		"workstream_pa_eligibility_ingress.go:handleEligibilityIngress": 1,
		"workstream_fqs_ingress.go:handleFederatedQueryIngress":         1,
		"workstream_pa_ingress.go:handleCRDIngress":                     1,
		"workstream_pa_ingress.go:handleDTRIngress":                     1,
		"workstream_pa_ingress.go:handlePASIngress":                     1,
		"workstream_pa_inquire.go:handlePASInquireIngress":              1,
	}
	if fmt.Sprint(callers) != fmt.Sprint(want) {
		t.Errorf("OriginateLeg is called by %v, want %v: a module 1 site sends through sendLeg", callers, want)
	}
}

// sendLeg takes a built request through egressBuild and its leg, and reports
// the route it was sent on; a check's refusal stops it with nothing sent, and
// a failure of the leg is reported as the leg's, with what was sent.
func TestSendLeg(t *testing.T) {
	_, _, order := originRecords(prefetchMember)
	sor, _ := originSystem(t, prefetchMember)
	body := []byte(`{"hook":"order-select"}`)
	for _, tc := range []struct {
		name        string
		legStatus   int
		check       int
		wantStatus  int
		wantFromLeg bool
		wantSent    bool
	}{
		{name: "answered", wantSent: true},
		{name: "the site's check refuses", check: http.StatusUnprocessableEntity, wantStatus: http.StatusUnprocessableEntity},
		{name: "the leg fails", legStatus: http.StatusInternalServerError, wantStatus: http.StatusBadGateway, wantFromLeg: true, wantSent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := payerAnswerFor(t, order, shnsdk.CoveredCovered, shnsdk.PANeededNoAuth, "")
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				if tc.legStatus != 0 {
					return tc.legStatus, nil
				}
				return 0, answer
			})
			route, err := g.routeFor("crd-order-select", "payer", "")
			if err != nil {
				t.Fatal(err)
			}
			var checkedAt string
			sent, failed := g.sendLeg(t.Context(), httptest.NewRequest(http.MethodPost, "/", nil), legSend{
				Workstream: workstreamPA, Leg: "crd-order-select", Recipient: "payer", PCI: "pci-1", Corr: strings.Repeat("0", 31) + "9", Route: &route,
				Build: &moduleBuild{Builder: relay.BuilderSDKCRDRequest, ContentType: "application/json", Body: body,
					Check: func(line string, _ []byte) (int, string) {
						checkedAt = line
						return tc.check, "refused by the site"
					}},
			})
			if checkedAt != shnsdk.LineOf(route.Token) {
				t.Errorf("the site's check ran at line %q, want the route's %q", checkedAt, shnsdk.LineOf(route.Token))
			}
			payer.mu.Lock()
			reached := len(payer.requests["crd-order-select"])
			payer.mu.Unlock()
			if want := map[bool]int{true: 1}[tc.wantSent]; reached != want {
				t.Errorf("the payer got %d requests, want %d", reached, want)
			}
			if tc.wantStatus == 0 {
				if failed != nil {
					t.Fatalf("want an answer, got %+v", failed)
				}
				if !bytes.Equal(sent.Answer, answer) || !bytes.Equal(sent.Bytes, body) || sent.Route.Token != route.Token {
					t.Fatalf("sent %+v: want the payer's answer, the bytes built and the route given", sent)
				}
				return
			}
			if failed == nil || failed.Status != tc.wantStatus || failed.FromLeg != tc.wantFromLeg {
				t.Fatalf("failure %+v: want status %d, from the leg %v", failed, tc.wantStatus, tc.wantFromLeg)
			}
			if tc.wantFromLeg {
				if failed.Err == nil || failed.Msg != failed.Err.Error() {
					t.Fatalf("a leg's failure carries its error as its message: %+v", failed)
				}
				if !bytes.Equal(sent.Bytes, body) || sent.Route.Token != route.Token {
					t.Fatalf("a leg's failure keeps what was sent: %+v", sent)
				}
				return
			}
			if failed.Err != nil || failed.Msg != "refused by the site" || sent.Answer != nil {
				t.Fatalf("a check's refusal is the check's, with nothing sent: %+v %+v", failed, sent)
			}
		})
	}
}

// writeLegFailure answers a framed relay error from the leg as
// relayOriginationError writes it, and anything else, a refusal before the leg
// included, with the failure's own status and message.
func TestWriteLegFailure(t *testing.T) {
	g := &Gateway{}
	refusal := &RouteRefusalError{LegType: "pas-claim", Recipient: "payer"}
	for _, tc := range []struct {
		name       string
		f          legFailure
		wantStatus int
		wantBody   string
	}{
		{"a check's refusal", legFailure{Status: http.StatusUnprocessableEntity, Msg: "unsupported DTR attachment line"}, http.StatusUnprocessableEntity, "unsupported DTR attachment line"},
		{"a leg's plain failure", legFailure{Status: http.StatusBadGateway, Msg: "hub said no", Err: fmt.Errorf("hub said no"), FromLeg: true}, http.StatusBadGateway, "hub said no"},
		{"a leg's route refusal", legFailure{Status: http.StatusBadGateway, Msg: refusal.Error(), Err: refusal, FromLeg: true}, http.StatusUnprocessableEntity, ""},
		{"a walk's route refusal is not the leg's", legFailure{Status: http.StatusBadGateway, Msg: refusal.Error(), Err: refusal}, http.StatusBadGateway, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			g.writeLegFailure(rec, &tc.f)
			if rec.Code != tc.wantStatus || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("got %d %s, want %d containing %q", rec.Code, rec.Body, tc.wantStatus, tc.wantBody)
			}
		})
	}
}
