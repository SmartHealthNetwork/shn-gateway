package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The origination route names its member and the order its claim facts are
// stated for: a request naming no member, another member, or
// an order the member does not hold is refused before any leg. And every
// required fact the request does not state is refused through the route at
// every line, the amendment's certificationType included.

func uc06RouteFixture(t *testing.T, line string) (*Gateway, *pendResumeSubstrate) {
	t.Helper()
	opts := pendFixtureOpts{member: "MBR-UC06", birthDate: "1969-07-21", familyName: "Reyes", pendedItem: "functional-status"}
	switch line {
	case "2.1":
		opts.declared = []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}
	case "2.2":
		opts.declared = []string{shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22}
	}
	gw, stub := newPendResumeFixture(t, opts)
	if line != "2.0" {
		gw.cfg.Validator = NewLineFakeValidator("2.0")
		gw.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": NewLineFakeValidator("2.0"), line: NewLineFakeValidator(line)}
	}
	return gw, stub
}

// editBody decodes a route body, applies edit, and re-encodes it.
func editBody(t *testing.T, body string, edit func(map[string]json.RawMessage)) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, _ := json.Marshal(m)
	return string(out)
}

func dropFact(t *testing.T, fact string) func(map[string]json.RawMessage) {
	return func(m map[string]json.RawMessage) {
		var f map[string]json.RawMessage
		if err := json.Unmarshal(m["claimFacts"], &f); err != nil {
			t.Fatal(err)
		}
		delete(f, fact)
		m["claimFacts"], _ = json.Marshal(f)
	}
}

func startUC06(gw *Gateway, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	gw.handleUC06Start(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc06/start", strings.NewReader(body))))
	return rec
}

func TestOriginationRoute_NamesItsMemberAndOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(map[string]json.RawMessage)
		status int
		want   string
	}{
		{"no member", func(m map[string]json.RawMessage) { delete(m, "member") }, 400, `names no \"member\"`},
		{"another member", func(m map[string]json.RawMessage) { m["member"] = json.RawMessage(`"MBR-UC07"`) }, 422,
			"this route originates for MBR-UC06; the request names MBR-UC07"},
		{"facts stated for another order", func(m map[string]json.RawMessage) {
			m["order"] = json.RawMessage(`{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"J3490"}`)
		}, 422, "fidelity of SHN's own build"},
		{"no order", func(m map[string]json.RawMessage) { delete(m, "order") }, 400, `names no \"order\"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, stub := uc06RouteFixture(t, "2.0")
			rec := startUC06(gw, editBody(t, testRouteBody(gw, "/scenario/uc06/start", ""), tc.edit))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("%d %s, want %d naming %q", rec.Code, rec.Body.String(), tc.status, tc.want)
			}
			if len(stub.legTypes) != 0 {
				t.Fatalf("legs sent before the refusal: %v", stub.legTypes)
			}
		})
	}
	// The control: the route's member and the order it holds start the flow.
	gw, stub := uc06RouteFixture(t, "2.0")
	if rec := startUC06(gw, testRouteBody(gw, "/scenario/uc06/start", "")); rec.Code != 200 || !legAttempted(stub.legTypes, "pas-claim") {
		t.Fatalf("control %d %s legs=%v", rec.Code, rec.Body.String(), stub.legTypes)
	}
}

func TestOriginationRoute_RefusesUnstatedFactsAtEveryLine(t *testing.T) {
	for _, line := range []string{"2.1", "2.2"} {
		for fact, element := range map[string]string{
			"claimType": "Claim.type", "priority": "Claim.priority", "serviceCategory": "Claim.item.category",
			"placeOfService": "Claim.item.location[x]", "requestType": "Claim.item requestType",
		} {
			t.Run(line+"/"+fact, func(t *testing.T) {
				gw, stub := uc06RouteFixture(t, line)
				rec := startUC06(gw, editBody(t, testRouteBody(gw, "/scenario/uc06/start", ""), dropFact(t, fact)))
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), element) {
					t.Fatalf("%d %s, want 422 naming %s", rec.Code, rec.Body.String(), element)
				}
				// Refused before any leg: not the coverage check, not the
				// questionnaire fetch, not the claim.
				if len(stub.legTypes) != 0 {
					t.Fatalf("legs sent before the refusal: %v", stub.legTypes)
				}
			})
		}
	}
}

// pendThenComplete starts UC-06 to its pend with startBody and completes it
// with completeEdit applied to the route body; it returns the completion.
func pendThenComplete(t *testing.T, line, startBody string, completeEdit func(map[string]json.RawMessage)) (*httptest.ResponseRecorder, *pendResumeSubstrate) {
	t.Helper()
	gw, stub := uc06RouteFixture(t, line)
	if startBody == "" {
		startBody = testRouteBody(gw, "/scenario/uc06/start", "")
	}
	start := startUC06(gw, startBody)
	var started startResp
	if start.Code != 200 || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
		t.Fatalf("start %d %s", start.Code, start.Body.String())
	}
	body := editBody(t, testRouteBody(gw, "/scenario/uc06/complete", `"resumeToken":"`+started.ResumeToken+`"`), completeEdit)
	rec := httptest.NewRecorder()
	gw.handleUC06Complete(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc06/complete", strings.NewReader(body))))
	return rec, stub
}

// sentUpdateClaim is the operative Claim of the one amendment sent.
func sentUpdateClaim(t *testing.T, stub *pendResumeSubstrate) map[string]json.RawMessage {
	t.Helper()
	sent := stub.requests["pas-claim-update"]
	if len(sent) != 1 {
		t.Fatalf("%d amendments sent", len(sent))
	}
	var b struct {
		Entry []struct {
			Resource map[string]json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(sent[0], &b); err != nil || len(b.Entry) == 0 {
		t.Fatalf("amendment: %v", err)
	}
	return b.Entry[0].Resource
}

func TestAmendmentRoute_CertificationTypeStatedOrRefused(t *testing.T) {
	for _, line := range []string{"2.1", "2.2"} {
		t.Run(line+"/absent", func(t *testing.T) {
			rec, stub := pendThenComplete(t, line, "", func(m map[string]json.RawMessage) { delete(m, "certificationType") })
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "certificationType") {
				t.Fatalf("%d %s, want 422 naming certificationType", rec.Code, rec.Body.String())
			}
			if legAttempted(stub.legTypes, "pas-claim-update") {
				t.Fatal("an amendment without its certificationType was sent")
			}
		})
		t.Run(line+"/stated", func(t *testing.T) {
			const stated = `{"system":"https://codesystem.x12.org/005010/1322","code":"I","display":"as stated"}`
			rec, stub := pendThenComplete(t, line, "", func(m map[string]json.RawMessage) { m["certificationType"] = json.RawMessage(stated) })
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if claim := sentUpdateClaim(t, stub); !strings.Contains(string(claim["item"]), `"display":"as stated"`) {
				t.Fatalf("the stated certificationType is not carried: %s", claim["item"])
			}
		})
	}
}

func TestAmendmentRoute_ResumeWithoutFactsCarriesThePendedSubmissionsFacts(t *testing.T) {
	const statPriority = `{"system":"http://terminology.hl7.org/CodeSystem/processpriority","code":"stat"}`
	gwProbe, _ := uc06RouteFixture(t, "2.0")
	start := editBody(t, testRouteBody(gwProbe, "/scenario/uc06/start", ""), func(m map[string]json.RawMessage) {
		var f map[string]json.RawMessage
		_ = json.Unmarshal(m["claimFacts"], &f)
		f["priority"] = json.RawMessage(statPriority)
		m["claimFacts"], _ = json.Marshal(f)
	})
	rec, stub := pendThenComplete(t, "2.0", start, func(m map[string]json.RawMessage) { delete(m, "claimFacts") })
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if p := string(sentUpdateClaim(t, stub)["priority"]); !strings.Contains(p, `"code":"stat"`) {
		t.Fatalf("amendment priority %s, want the pended submission's stated stat", p)
	}
}

func TestUC07Pending_CarriesThePendsMemberAndOrder(t *testing.T) {
	opts := pendFixtureOpts{member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad", pendedItem: "patient-reported-functional-status",
		extraRoles: map[string]string{"phg": "phg"}}
	gw, _ := newPendResumeFixture(t, opts)
	rec := httptest.NewRecorder()
	gw.handleUC07Start(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc07/start",
		strings.NewReader(testRouteBody(gw, "/scenario/uc07/start", "")))))
	var started startResp
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &started) != nil {
		t.Fatalf("start %d %s", rec.Code, rec.Body.String())
	}
	st, _ := gw.loadPending(started.ResumeToken)
	pending := httptest.NewRecorder()
	gw.handleUC07Pending(pending, httptest.NewRequest(http.MethodGet, "/scenario/uc07/pending?patient="+st.pci, nil))
	var got uc07PendingResp
	if err := json.Unmarshal(pending.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	uc07 := DemoOrderCodes().UC07
	if got.Member != "MBR-UC07" || got.Order == nil || got.Order.System != uc07.System || got.Order.Code != uc07.Code {
		t.Fatalf("pending = %+v, want member MBR-UC07 and order %s|%s", got, uc07.System, uc07.Code)
	}
}

// TestSingleCallAmendment_CertificationTypeRefusedBeforeAnyLeg: the single
// call (/scenario/uc06, /scenario/uc07) submits, pends and amends in one
// request, so a request stating no amendment certificationType at 2.1/2.2 is
// refused before its first leg, never after its submission went out. The
// start route (/start), which sends no amendment, does not need one.
func TestSingleCallAmendment_CertificationTypeRefusedBeforeAnyLeg(t *testing.T) {
	noCert := func(m map[string]json.RawMessage) { delete(m, "certificationType") }
	nullCert := func(m map[string]json.RawMessage) { m["certificationType"] = json.RawMessage("null") }
	for _, line := range []string{"2.1", "2.2"} {
		for name, edit := range map[string]func(map[string]json.RawMessage){"absent": noCert, "null": nullCert} {
			t.Run(line+"/uc06/"+name, func(t *testing.T) {
				gw, stub := uc06RouteFixture(t, line)
				rec := httptest.NewRecorder()
				gw.handleUC06(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc06",
					strings.NewReader(editBody(t, testRouteBody(gw, "/scenario/uc06", ""), edit)))))
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "certificationType") {
					t.Fatalf("%d %s, want 422 naming certificationType", rec.Code, rec.Body.String())
				}
				if len(stub.legTypes) != 0 {
					t.Fatalf("legs sent before the refusal: %v", stub.legTypes)
				}
			})
		}
		t.Run(line+"/uc07/absent", func(t *testing.T) {
			gw, stub := uc07RouteFixture(t, line)
			rec := httptest.NewRecorder()
			gw.handleUC07(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc07",
				strings.NewReader(editBody(t, testRouteBody(gw, "/scenario/uc07", ""), noCert)))))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "certificationType") {
				t.Fatalf("%d %s, want 422 naming certificationType", rec.Code, rec.Body.String())
			}
			if len(stub.legTypes) != 0 {
				t.Fatalf("legs sent before the refusal: %v", stub.legTypes)
			}
		})
		t.Run(line+"/start needs none", func(t *testing.T) {
			gw, stub := uc06RouteFixture(t, line)
			if rec := startUC06(gw, editBody(t, testRouteBody(gw, "/scenario/uc06/start", ""), noCert)); rec.Code != 200 || !legAttempted(stub.legTypes, "pas-claim") {
				t.Fatalf("start without a certificationType: %d %s legs=%v", rec.Code, rec.Body.String(), stub.legTypes)
			}
		})
	}
}

func uc07RouteFixture(t *testing.T, line string) (*Gateway, *pendResumeSubstrate) {
	t.Helper()
	opts := pendFixtureOpts{member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad", pendedItem: "patient-reported-functional-status",
		extraRoles: map[string]string{"phg": "phg"}}
	switch line {
	case "2.1":
		opts.declared = []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}
	case "2.2":
		opts.declared = []string{shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22}
	}
	gw, stub := newPendResumeFixture(t, opts)
	if line != "2.0" {
		gw.cfg.Validator = NewLineFakeValidator("2.0")
		gw.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": NewLineFakeValidator("2.0"), line: NewLineFakeValidator(line)}
	}
	return gw, stub
}

// TestUC07Complete_RefusesBeforeThePatientDTRLeg: /scenario/uc07/complete
// checks the member, the order and the amendment's facts before it sends any
// leg — the PHG patient-DTR exchange included. A completion the amendment
// cannot be built for sends nothing; the control completes through both legs.
func TestUC07Complete_RefusesBeforeThePatientDTRLeg(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		edit       func(map[string]json.RawMessage)
		status     int
		want       string
	}{
		{"another member", "2.0", func(m map[string]json.RawMessage) { m["member"] = json.RawMessage(`"MBR-UC06"`) }, 422, "the request names MBR-UC06"},
		{"another order", "2.0", func(m map[string]json.RawMessage) {
			m["order"] = json.RawMessage(`{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"J3490"}`)
		}, 422, "fidelity of SHN's own build"},
		{"a fact stated invalid", "2.0", func(m map[string]json.RawMessage) {
			var f map[string]json.RawMessage
			_ = json.Unmarshal(m["claimFacts"], &f)
			f["priority"] = json.RawMessage(`{"code":"stat"}`)
			m["claimFacts"], _ = json.Marshal(f)
		}, 422, "Claim.priority"},
		{"no certificationType at 2.1", "2.1", func(m map[string]json.RawMessage) { delete(m, "certificationType") }, 422, "certificationType"},
		{"no certificationType at 2.2", "2.2", func(m map[string]json.RawMessage) { delete(m, "certificationType") }, 422, "certificationType"},
		{"control", "2.2", func(map[string]json.RawMessage) {}, 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, stub := uc07RouteFixture(t, tc.line)
			start := httptest.NewRecorder()
			gw.handleUC07Start(start, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc07/start",
				strings.NewReader(testRouteBody(gw, "/scenario/uc07/start", "")))))
			var started startResp
			if start.Code != 200 || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
				t.Fatalf("start %d %s", start.Code, start.Body.String())
			}
			before := len(stub.legTypes)
			body := editBody(t, testRouteBody(gw, "/scenario/uc07/complete", `"resumeToken":"`+started.ResumeToken+`"`), tc.edit)
			rec := httptest.NewRecorder()
			gw.handleUC07Complete(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc07/complete", strings.NewReader(body))))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("%d %s, want %d naming %q", rec.Code, rec.Body.String(), tc.status, tc.want)
			}
			sent := stub.legTypes[before:]
			if tc.status == 200 {
				if !legAttempted(sent, "patient-dtr") || !legAttempted(sent, "pas-claim-update") {
					t.Fatalf("control legs %v, want the patient-DTR exchange and the amendment", sent)
				}
				return
			}
			if len(sent) != 0 {
				t.Fatalf("legs sent before the refusal: %v", sent)
			}
		})
	}
}

// TestUC05_AmendmentCertificationTypeRefusedBeforeAnyLeg: the consented UC-05
// amends with the federated evidence in the same request, so at 2.1/2.2 a
// request stating no amendment certificationType is refused before its first
// leg (no coverage check, no federated query, no submission).
func TestUC05_AmendmentCertificationTypeRefusedBeforeAnyLeg(t *testing.T) {
	for _, line := range []string{"2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			opts := pendFixtureOpts{member: "MBR-D-UC05", birthDate: "1968-03-12", familyName: "Johansson-Demo",
				pendedItem: "operative-diagnostic-report", extraRoles: map[string]string{"facility": "metro-spine"}}
			if line == "2.1" {
				opts.declared = []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}
			} else {
				opts.declared = []string{shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22}
			}
			gw, stub := newPendResumeFixture(t, opts)
			gw.cfg.OriginationProfile = "demo"
			gw.cfg.Validator = NewLineFakeValidator("2.0")
			gw.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": NewLineFakeValidator("2.0"), line: NewLineFakeValidator(line)}
			body := editBody(t, testRouteBody(gw, "/scenario/uc05", ""), func(m map[string]json.RawMessage) { delete(m, "certificationType") })
			rec := httptest.NewRecorder()
			gw.handleUC05(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc05", strings.NewReader(body))))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "certificationType") {
				t.Fatalf("%d %s, want 422 naming certificationType", rec.Code, rec.Body.String())
			}
			if len(stub.legTypes) != 0 {
				t.Fatalf("legs sent before the refusal: %v", stub.legTypes)
			}
		})
	}
}

// TestOriginationRoute_OversizeBodyIs413: an origination request body larger
// than the gateway reads is answered 413, as the routes answered before the
// claim input was read at their entry — never a 400 about its claim facts —
// and nothing is sent.
func TestOriginationRoute_OversizeBodyIs413(t *testing.T) {
	huge := `{"pad":"` + strings.Repeat("x", shnsdk.MaxRequestBytes) + `"}`
	for _, tc := range []struct {
		path string
		run  func(*Gateway, http.ResponseWriter, *http.Request)
	}{
		{"/scenario/uc04", func(g *Gateway, w http.ResponseWriter, r *http.Request) { g.handleUC04(w, r) }},
		{"/scenario/uc05", func(g *Gateway, w http.ResponseWriter, r *http.Request) { g.handleUC05(w, r) }},
		{"/scenario/uc07", func(g *Gateway, w http.ResponseWriter, r *http.Request) { g.handleUC07(w, r) }},
	} {
		t.Run(tc.path, func(t *testing.T) {
			gw, stub := uc07RouteFixture(t, "2.0")
			rec := httptest.NewRecorder()
			tc.run(gw, rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(huge))))
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%d %s, want 413", rec.Code, rec.Body.String())
			}
			if len(stub.legTypes) != 0 {
				t.Fatalf("legs sent: %v", stub.legTypes)
			}
		})
	}
}
