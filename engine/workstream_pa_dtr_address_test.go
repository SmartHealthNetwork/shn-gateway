//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/dtraddress"
)

// addressed is what the requester received from a payer gateway through the
// PA answer dispatch (paAddressIssuer around the leg's handler): the status,
// the body, and the headers its frame states.
type addressed struct {
	status  int
	body    []byte
	headers map[string]string
}

// sendAddressed delivers body on the questionnaire leg through the PA answer
// dispatch, naming operation, and subpath when set, as a provider's gateway
// frames it.
func (d dtrPayer) sendAddressed(t *testing.T, operation, subpath string, body []byte) addressed {
	t.Helper()
	return d.sendAddressedIn(t, context.Background(), operation, subpath, body)
}

// sendAddressedIn is sendAddressed on a request context derived from base.
func (d dtrPayer) sendAddressedIn(t *testing.T, base context.Context, operation, subpath string, body []byte) addressed {
	t.Helper()
	d.partner.lastPath, d.partner.lastBody = "", nil
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: d.requester.ID, Recipient: "payer", TransactionType: "dtr-questionnaire-fetch",
		AuthorityFrame: "provider-tpo", Timestamp: d.g.cfg.Clock().Format(time.RFC3339),
		CorrelationID: "corr-dtr-addr",
	}, body, d.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withRequestFrameOperation(withAnswerLine(base, "pa.dtr@2.0"), operation)
	if subpath != "" {
		ctx = withRequestFrameSubpath(ctx, subpath)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	paWorkstream{}.answer("dtr-questionnaire-fetch")(d.g, rec, r, env,
		shnsdk.Token{Operation: "dtr-questionnaire-fetch", Subject: d.coveredPC, CorrelationID: "corr-dtr-addr"}, body, "pa.dtr@2.0")
	if rec.Code != http.StatusOK {
		return addressed{status: rec.Code, body: rec.Body.Bytes()}
	}
	hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, d.requester, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response frame: %v", err)
	}
	return addressed{status: hdr.Status, body: answer, headers: hdr.Headers}
}

// issuedTag is the tag the payer gateway d issues for an address at line
// 2.0 under its configured base.
func (d dtrPayer) issuedTag(family, remainder string) string {
	return d.g.paState().addressKey.Tag(dtraddress.Address{Family: family, Payer: d.g.cfg.HolderID, Provider: d.requester.ID,
		Line: "2.0", Base: d.partner.srv.URL, Remainder: remainder})
}

// adaptiveQR is a next-question input about the covered member whose
// contained Questionnaire carries the adaptive extension ext.
func adaptiveQR(ext string) []byte {
	return []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/` + dtrFrameMember + `"},` +
		`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active","extension":[{"url":"` + questionnaireAdaptiveExtension + `",` + ext + `}]}],` +
		`"questionnaire":"#q"}`)
}

// A round names the address it was called at (operationSubpath). It is
// delivered there only when its tag verifies for the requesting provider, at
// the base this payer's gateway delivers under now; otherwise it is refused
// 422 as routing and nothing reaches the payer's system.
func TestDTRAddress_ARoundIsDeliveredOnlyAtAnAddressItsPayerIssued(t *testing.T) {
	d := newDTRPayer(t)
	d.partner.respByPath["/forms/a"+nextPath] = d.partner.respByPath[nextPath]
	round := []byte(nextQuestionQR(dtrFrameMember))
	payerTag, fetchTag := d.issuedTag(dtraddress.FamilyPayer, "/forms/a"), d.issuedTag(dtraddress.FamilyFetchServer, "")
	other, _ := dtraddress.DeriveKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize)))
	rotated := other.Tag(dtraddress.Address{Family: dtraddress.FamilyPayer, Payer: d.g.cfg.HolderID, Provider: d.requester.ID,
		Line: "2.0", Base: d.partner.srv.URL, Remainder: "/forms/a"})
	anotherProvider := d.g.paState().addressKey.Tag(dtraddress.Address{Family: dtraddress.FamilyPayer, Payer: d.g.cfg.HolderID,
		Provider: "another-provider", Line: "2.0", Base: d.partner.srv.URL, Remainder: "/forms/a"})
	for name, row := range map[string]struct {
		op, subpath, wantPath string
		status                int
	}{
		"a payer address":           {shnsdk.FrameOperationNextQuestion, "/payer/2.0/" + payerTag + "/forms/a", "/forms/a" + nextPath, http.StatusOK},
		"a fetch-server address":    {shnsdk.FrameOperationNextQuestion, "/fetch-server/2.0/" + fetchTag, nextPath, http.StatusOK},
		"no address (module 1)":     {shnsdk.FrameOperationNextQuestion, "", nextPath, http.StatusOK},
		"a forged tag":              {shnsdk.FrameOperationNextQuestion, "/payer/2.0/" + strings.Repeat("A", 22) + "/forms/a", "", http.StatusUnprocessableEntity},
		"another remainder":         {shnsdk.FrameOperationNextQuestion, "/payer/2.0/" + payerTag + "/forms/b", "", http.StatusUnprocessableEntity},
		"another provider's tag":    {shnsdk.FrameOperationNextQuestion, "/payer/2.0/" + anotherProvider + "/forms/a", "", http.StatusUnprocessableEntity},
		"a tag under a rotated key": {shnsdk.FrameOperationNextQuestion, "/payer/2.0/" + rotated + "/forms/a", "", http.StatusUnprocessableEntity},
		// A tag over the unclean remainder itself: refused by the clean rule,
		// not by the tag.
		"an unclean remainder": {shnsdk.FrameOperationNextQuestion, "/payer/2.0/" + d.issuedTag(dtraddress.FamilyPayer, "/forms/a/") + "/forms/a/", "", http.StatusUnprocessableEntity},
		"the line changed":     {shnsdk.FrameOperationNextQuestion, "/payer/2.2/" + payerTag + "/forms/a", "", http.StatusUnprocessableEntity},
		"on a package request": {shnsdk.FrameOperationQuestionnairePackage, "/payer/2.0/" + payerTag + "/forms/a", "", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			body := round
			if row.op == shnsdk.FrameOperationQuestionnairePackage {
				body = dtrPackageFixture(t, "valid/dtr-package-params-2.0.json")
			}
			got := d.sendAddressed(t, row.op, row.subpath, body)
			if got.status != row.status {
				t.Fatalf("status %d %s, want %d", got.status, got.body, row.status)
			}
			if row.status == http.StatusUnprocessableEntity && !strings.Contains(string(got.body), errRoundAddressNotIssued) {
				t.Fatalf("refusal %s, want %q", got.body, errRoundAddressNotIssued)
			}
			if d.partner.lastPath != row.wantPath {
				t.Fatalf("the payer's system was called at %q, want %q", d.partner.lastPath, row.wantPath)
			}
		})
	}
}

// The forwards that predate the address keep their URLs byte for byte: a
// configured DTR base with a trailing slash is still joined as main joins it,
// for the package request and module 1's round, with the address issuer on.
func TestDTRAddress_ExistingForwardURLsAreUnchangedForATrailingSlashBase(t *testing.T) {
	d := newDTRPayer(t)
	d.g.cfg.Responder = NewNativeResponder(d.partner.srv.Client(), d.partner.srv.URL+"/fhir/", nil, WithCRDService("shn-order-select"))
	d.partner.respByPath["/fhir/"+packagePath] = d.partner.respByPath[packagePath]
	d.partner.respByPath["/fhir/"+nextPath] = d.partner.respByPath[nextPath]
	for op, body := range map[string][]byte{
		shnsdk.FrameOperationQuestionnairePackage: dtrPackageFixture(t, "valid/dtr-package-params-2.0.json"),
		shnsdk.FrameOperationNextQuestion:         []byte(nextQuestionQR(dtrFrameMember)),
	} {
		got := d.sendAddressed(t, op, "", body)
		want := "/fhir/" + map[string]string{shnsdk.FrameOperationQuestionnairePackage: packagePath, shnsdk.FrameOperationNextQuestion: nextPath}[op]
		if got.status != http.StatusOK || d.partner.lastPath != want {
			t.Fatalf("%s: status %d, called at %q, want %q (main's join)", op, got.status, d.partner.lastPath, want)
		}
	}
}

// A package answer states its base and tags only the addresses its payer's
// system issued in its own right: on a top-level Questionnaire entry, never a
// contained one, and none when the request carried a Questionnaire; a true at
// any position. Every tag verifies for this provider.
func TestDTRAddress_APackageAnswerStatesItsBaseAndTags(t *testing.T) {
	d := newDTRPayer(t)
	base := d.partner.srv.URL
	q := func(id, ext string) string {
		return `{"resourceType":"Questionnaire","id":"` + id + `","status":"active","extension":[{"url":"` + questionnaireAdaptiveExtension + `",` + ext + `}]}`
	}
	answer := func(entries ...string) []byte {
		var es []string
		for _, e := range entries {
			es = append(es, `{"resource":`+e+`}`)
		}
		return []byte(`{"resourceType":"Bundle","type":"collection","entry":[` + strings.Join(es, ",") + `]}`)
	}
	contained := `{"resourceType":"QuestionnaireResponse","status":"in-progress","contained":[` + q("c", `"valueUrl":"`+base+`/forms/b"`) + `],"questionnaire":"#c"}`
	for name, row := range map[string]struct {
		answer    []byte
		request   []byte
		wantBase  bool
		wantPairs []string
	}{
		"a top-level address, a contained one and a true": {answer(q("a", `"valueUrl":"`+base+`/forms/a"`), contained, q("t", `"valueBoolean":true`)),
			nil, true, []string{"/forms/a", "*"}},
		"only a true": {answer(q("t", `"valueBoolean":true`)), nil, false, []string{"*"}},
		"a request carrying a Questionnaire": {answer(q("a", `"valueUrl":"`+base+`/forms/a"`)),
			[]byte(strings.Replace(string(dtrPackageFixture(t, "valid/dtr-package-params-2.0.json")), `"parameter": [`, `"parameter": [{"name":"order","resource":`+q("x", `"valueBoolean":false`)+`},`, 1)),
			true, nil},
		"an escaped address": {answer(q("a", `"valueUrl":"`+strings.ReplaceAll(base, "/", `\/`)+`\/forms\/a"`)), nil, true, nil},
		// Under the base (the scheme case-folded) but not spelled as it: the
		// base is stated, the address not tagged.
		"a base spelled differently": {answer(q("a", `"valueUrl":"`+strings.Replace(base, "http://", "HTTP://", 1)+`/forms/a"`)), nil, true, nil},
		"an address elsewhere":       {answer(q("a", `"valueUrl":"https://elsewhere.example/forms/a"`)), nil, false, nil},
		"an unclean remainder":       {answer(q("a", `"valueUrl":"`+base+`/forms/a/"`)), nil, true, nil},
	} {
		t.Run(name, func(t *testing.T) {
			d.partner.respByPath[packagePath] = row.answer
			req := row.request
			if req == nil {
				req = dtrPackageFixture(t, "valid/dtr-package-params-2.0.json")
			}
			got := d.sendAddressed(t, shnsdk.FrameOperationQuestionnairePackage, "", req)
			if got.status != http.StatusOK {
				t.Fatalf("status %d %s", got.status, got.body)
			}
			if b, ok := got.headers[shnsdk.FrameHeaderOperationBase]; ok != row.wantBase || (ok && b != base) {
				t.Fatalf("operationBase %q %v, want stated %v as %q", b, ok, row.wantBase, base)
			}
			pairs, err := dtraddress.ParseTags(got.headers[shnsdk.FrameHeaderOperationAddressTags])
			if err != nil {
				t.Fatal(err)
			}
			var remainders []string
			for _, p := range pairs {
				remainders = append(remainders, p.Remainder)
				family, r := dtraddress.FamilyPayer, p.Remainder
				if r == dtraddress.BoolRemainder {
					family, r = dtraddress.FamilyFetchServer, ""
				}
				if p.Tag != d.issuedTag(family, r) {
					t.Fatalf("pair %+v is not this provider's tag", p)
				}
			}
			if strings.Join(remainders, " ") != strings.Join(row.wantPairs, " ") {
				t.Fatalf("tagged %q, want %q", remainders, row.wantPairs)
			}
			if bytes.Contains(got.body, []byte("operationBase")) {
				t.Fatal("an address header reached the body")
			}
		})
	}
}

// A package request answered at another line than the one it claims states
// no base and no tag: the requester's gateway would write the claimed line
// into the re-keyed address, which the tag (bound to the answer's line) does
// not, and every round would be refused. The address is then carried as sent,
// and the client calls the payer directly. A request carried but answered at
// the line it claims (a declared line with no ready lane) states both, as a
// request answered as claimed does.
func TestDTRAddress_APackageAnsweredAtAnotherLineStatesNoTags(t *testing.T) {
	d := newDTRPayer(t)
	base := d.partner.srv.URL
	d.partner.respByPath[packagePath] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"a","status":"active",` +
		`"extension":[{"url":"` + questionnaireAdaptiveExtension + `","valueUrl":"` + base + `/forms/a"}]}}]}`)
	req := dtrPackageFixture(t, "valid/dtr-package-params-2.0.json")
	for name, row := range map[string]struct {
		ctx    context.Context
		stated bool
	}{
		"a request at a line it answers": {context.Background(), true},
		// sendAddressedIn answers at 2.0.
		"a request carried and answered at another line":       {withRequestLineCarried(context.Background(), "pa.dtr@2.2"), false},
		"a request carried but answered at the line it claims": {withRequestLineCarried(context.Background(), "pa.dtr@2.0"), true},
	} {
		t.Run(name, func(t *testing.T) {
			got := d.sendAddressedIn(t, row.ctx, shnsdk.FrameOperationQuestionnairePackage, "", req)
			if got.status != http.StatusOK {
				t.Fatalf("status %d %s", got.status, got.body)
			}
			_, baseStated := got.headers[shnsdk.FrameHeaderOperationBase]
			_, tagsStated := got.headers[shnsdk.FrameHeaderOperationAddressTags]
			if baseStated != row.stated || tagsStated != row.stated {
				t.Fatalf("operationBase stated %v, operationAddressTags stated %v, want both %v", baseStated, tagsStated, row.stated)
			}
			if !bytes.Contains(got.body, []byte(base+"/forms/a")) {
				t.Fatalf("the answer lost the payer's own address: %s", got.body)
			}
		})
	}
}

// A round's answer is tagged at the line of the address the round was
// delivered to, which the provider's gateway writes too, even when the
// round's request claims another line it was carried from: the guard that
// leaves a package answered at another line untagged does not reach rounds.
func TestDTRAddress_ARoundCarriedFromAnotherLineIsStillTagged(t *testing.T) {
	d := newDTRPayer(t)
	base := d.partner.srv.URL
	d.partner.respByPath["/forms/a"+nextPath] = []byte(`{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":` +
		`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/` + dtrFrameMember + `"},` +
		`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active","extension":[{"url":"` + questionnaireAdaptiveExtension + `","valueUrl":"` + base + `/forms/a"}]}],"questionnaire":"#q"}}]}`)
	got := d.sendAddressedIn(t, withRequestLineCarried(context.Background(), "pa.dtr@2.2"), shnsdk.FrameOperationNextQuestion,
		"/payer/2.0/"+d.issuedTag(dtraddress.FamilyPayer, "/forms/a")+"/forms/a", []byte(nextQuestionQR(dtrFrameMember)))
	if got.status != http.StatusOK {
		t.Fatalf("status %d %s", got.status, got.body)
	}
	if b, ok := got.headers[shnsdk.FrameHeaderOperationBase]; !ok || b != base {
		t.Fatalf("operationBase %q %v, want %q stated", b, ok, base)
	}
	pairs, err := dtraddress.ParseTags(got.headers[shnsdk.FrameHeaderOperationAddressTags])
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].Remainder != "/forms/a" || pairs[0].Tag != d.issuedTag(dtraddress.FamilyPayer, "/forms/a") {
		t.Fatalf("tagged %+v, want the round's own address at its own line", pairs)
	}
}

// A next-question answer tags only the payer's base and the address the
// round was delivered to.
func TestDTRAddress_ARoundsAnswerTagsOnlyTheBaseAndItsOwnAddress(t *testing.T) {
	d := newDTRPayer(t)
	base := d.partner.srv.URL
	ext := func(v string) string {
		return `{"url":"` + questionnaireAdaptiveExtension + `","valueUrl":"` + v + `"}`
	}
	d.partner.respByPath["/forms/a"+nextPath] = []byte(`{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":` +
		`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/` + dtrFrameMember + `"},` +
		`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active","extension":[` + ext(base) + `,` + ext(base+"/forms/a") + `,` + ext(base+"/forms/c") + `]}],"questionnaire":"#q"}}]}`)
	got := d.sendAddressed(t, shnsdk.FrameOperationNextQuestion, "/payer/2.0/"+d.issuedTag(dtraddress.FamilyPayer, "/forms/a")+"/forms/a", []byte(nextQuestionQR(dtrFrameMember)))
	if got.status != http.StatusOK {
		t.Fatalf("status %d %s", got.status, got.body)
	}
	pairs, err := dtraddress.ParseTags(got.headers[shnsdk.FrameHeaderOperationAddressTags])
	if err != nil {
		t.Fatal(err)
	}
	var remainders []string
	for _, p := range pairs {
		remainders = append(remainders, p.Remainder)
	}
	if strings.Join(remainders, "|") != "|/forms/a" {
		t.Fatalf("tagged %q, want the base and the round's own address", remainders)
	}
}

// The restore (E-13): a round carrying back an address this payer issued to
// this provider reaches the payer's system with the address the payer wrote,
// or its true; any other address is carried as sent.
func TestDTRAddress_ARoundCarryingAnIssuedAddressIsRestored(t *testing.T) {
	d := newDTRPayer(t)
	base := d.partner.srv.URL
	issued := func(family, remainder, tag string) string {
		return `"valueUrl":"https://provider-gw.example/dtr/` + family + `/payer/2.0/` + tag + remainder + `"`
	}
	for name, row := range map[string]struct{ ext, want string }{
		"a payer address":        {issued("payer", "/forms/a", d.issuedTag(dtraddress.FamilyPayer, "/forms/a")), `"valueUrl":"` + base + `/forms/a"`},
		"a fetch-server address": {issued("fetch-server", "", d.issuedTag(dtraddress.FamilyFetchServer, "")), `"valueBoolean":true`},
		"a forged tag":           {issued("payer", "/forms/a", strings.Repeat("A", 22)), issued("payer", "/forms/a", strings.Repeat("A", 22))},
		"another payer's address": {`"valueUrl":"https://provider-gw.example/dtr/payer/another/2.0/` + d.issuedTag(dtraddress.FamilyPayer, "/forms/a") + `/forms/a"`,
			`"valueUrl":"https://provider-gw.example/dtr/payer/another/2.0/` + d.issuedTag(dtraddress.FamilyPayer, "/forms/a") + `/forms/a"`},
	} {
		t.Run(name, func(t *testing.T) {
			got := d.sendAddressed(t, shnsdk.FrameOperationNextQuestion, "", adaptiveQR(row.ext))
			if got.status != http.StatusOK {
				t.Fatalf("status %d %s", got.status, got.body)
			}
			if !bytes.Contains(d.partner.lastBody, []byte(row.want)) {
				t.Fatalf("the payer's system received\n%s\nwant it to carry %s", d.partner.lastBody, row.want)
			}
			if want := bytes.Replace(adaptiveQR(row.ext), []byte(row.ext), []byte(row.want), 1); !bytes.Equal(d.partner.lastBody, want) {
				t.Fatalf("other bytes changed:\n%s\nwant\n%s", d.partner.lastBody, want)
			}
		})
	}
}

// The restore runs on every leg that can carry the questionnaire back, here
// the documentation submission's forward: an issued address is restored, one
// that does not verify is carried and recorded as not made, and one naming
// another payer is carried and observed.
func TestDTRAddress_TheSubmissionForwardRestoresAndRecords(t *testing.T) {
	p := newStubPartner(t)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, fixedClock)
	key, err := dtraddress.DeriveKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := n.dtrBaseAt("2.0")
	tag := key.Tag(dtraddress.Address{Family: dtraddress.FamilyPayer, Payer: "payer", Provider: "provider-1", Line: "2.0", Base: base, Remainder: "/forms/a"})
	submission := func(value string) []byte {
		return []byte(`{"resourceType":"Parameters","parameter":[{"name":"attachment","resource":{"resourceType":"QuestionnaireResponse","status":"completed",` +
			`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active","extension":[{"url":"` + questionnaireAdaptiveExtension + `","valueUrl":"` + value + `"}]}],` +
			`"questionnaire":"#q"}}]}`)
	}
	issued := "https://provider-gw.example/dtr/payer/payer/2.0/" + tag + "/forms/a"
	for name, row := range map[string]struct {
		value, want, notMade string
		carried              int
	}{
		"an issued address":          {issued, base + "/forms/a", "", 0},
		"a forged tag":               {strings.Replace(issued, tag, strings.Repeat("A", 22), 1), strings.Replace(issued, tag, strings.Repeat("A", 22), 1), EditNotMadeAddressNotVerified, 0},
		"another payer's address":    {strings.Replace(issued, "/payer/payer/", "/payer/another/", 1), strings.Replace(issued, "/payer/payer/", "/payer/another/", 1), "", 1},
		"an address SHN never wrote": {"https://payer.example/fhir/forms/a", "https://payer.example/fhir/forms/a", "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			x := &exchangeRecorder{}
			var carried int
			ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
			ctx = withAddressIssuer(ctx, addressIssuer{key: key, payer: "payer", provider: "provider-1", carried: func() { carried++ }})
			p.respByPath["/$submit-attachment"] = []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`)
			if _, err := n.forwardCDexSubmit(ctx, peerBody(submission(row.value))); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(p.lastBody, submission(row.want)) {
				t.Fatalf("the payer's system received\n%s\nwant\n%s", p.lastBody, submission(row.want))
			}
			if got := strings.Join(x.rec.EditsNotMade, ","); got != row.notMade {
				t.Fatalf("not made %q, want %q", got, row.notMade)
			}
			if carried != row.carried {
				t.Fatalf("observed %d carried addresses, want %d", carried, row.carried)
			}
			// The restore is disclosed to the payer's system; a forward it
			// is not made on discloses nothing.
			want := []string(nil)
			if row.want != row.value {
				want = []string{"dtr-adaptive-address-restore"}
			}
			if got := p.lastHeader.Values(EditedHeader); !slices.Equal(got, want) {
				t.Fatalf("%s = %q, want %q", EditedHeader, got, want)
			}
		})
	}
}

// An address naming another payer, carried on a round, is observed as the
// fact alone: the event names its kind, direction, leg and correlation, and
// nothing else (no address, tag or participant identifier), as its stable
// promise states.
func TestDTRAddress_TheCarriedEventIsTheFactAlone(t *testing.T) {
	d := newDTRPayer(t)
	var events []ObserverEvent
	d.g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == "dtr.adaptive-address-carried" {
			events = append(events, e)
		}
	}
	other := `"valueUrl":"https://provider-gw.example/dtr/payer/another/2.0/` + d.issuedTag(dtraddress.FamilyPayer, "/forms/a") + `/forms/a"`
	if got := d.sendAddressed(t, shnsdk.FrameOperationNextQuestion, "", adaptiveQR(other)); got.status != http.StatusOK {
		t.Fatalf("status %d %s", got.status, got.body)
	}
	if len(events) != 1 {
		t.Fatalf("observed %d carried events, want 1", len(events))
	}
	raw, err := json.Marshal(events[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "time")
	want := map[string]any{"kind": "dtr.adaptive-address-carried", "direction": "ingress", "legType": "dtr-questionnaire-fetch", "correlationId": "corr-dtr-addr"}
	if len(fields) != len(want) {
		t.Fatalf("event fields %v, want exactly %v", fields, want)
	}
	for k, v := range want {
		if fields[k] != v {
			t.Fatalf("event %s = %v, want %v (all: %v)", k, fields[k], v, fields)
		}
	}
	if strings.Contains(string(raw), "another") || strings.Contains(string(raw), "provider") {
		t.Fatalf("the event names an address or a participant: %s", raw)
	}
}

// Module 1's own round names no address: its answer tags only the payer's
// base, never another address in it.
func TestDTRAddress_ARoundWithNoAddressTagsOnlyTheBase(t *testing.T) {
	d := newDTRPayer(t)
	base := d.partner.srv.URL
	ext := func(v string) string {
		return `{"url":"` + questionnaireAdaptiveExtension + `","valueUrl":"` + v + `"}`
	}
	d.partner.respByPath[nextPath] = []byte(`{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":` +
		`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/` + dtrFrameMember + `"},` +
		`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active","extension":[` + ext(base) + `,` + ext(base+"/forms/a") + `]}],"questionnaire":"#q"}}]}`)
	got := d.sendAddressed(t, shnsdk.FrameOperationNextQuestion, "", []byte(nextQuestionQR(dtrFrameMember)))
	if got.status != http.StatusOK {
		t.Fatalf("status %d %s", got.status, got.body)
	}
	pairs, err := dtraddress.ParseTags(got.headers[shnsdk.FrameHeaderOperationAddressTags])
	if err != nil || len(pairs) != 1 || pairs[0].Remainder != "" || pairs[0].Tag != d.issuedTag(dtraddress.FamilyPayer, "") {
		t.Fatalf("tagged %+v (%v), want the base alone", pairs, err)
	}
}

// The restore is the one change the PAS forward makes to a claim (E-13): the
// payer's system receives its own address back, and the payer identifier the
// claim names exactly as sent.
func TestDTRAddress_ThePASForwardRestoresAlone(t *testing.T) {
	const base = "https://payer.example/fhir/dtr"
	n := NewNativeResponder(nil, base, nil, WithCRDService("order-sign"))
	key, err := dtraddress.DeriveKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	tag := key.Tag(dtraddress.Address{Family: dtraddress.FamilyPayer, Payer: "payer", Provider: "provider-1", Line: "2.0", Base: base, Remainder: "/forms/a"})
	qr := `{"resource":{"resourceType":"QuestionnaireResponse","status":"completed","contained":[{"resourceType":"Questionnaire","id":"q","status":"active",` +
		`"extension":[{"url":"` + questionnaireAdaptiveExtension + `","valueUrl":"https://provider-gw.example/dtr/payer/payer/2.0/` + tag + `/forms/a"}]}],"questionnaire":"#q"}}`
	bundle := conformantSubmitBundle(t, ownIdentity, false)
	i := bytes.LastIndex(bundle, []byte("]"))
	bundle = append(append(append([]byte{}, bundle[:i]...), []byte(","+qr)...), bundle[i:]...)
	ctx := withAddressIssuer(context.Background(), addressIssuer{key: key, payer: "payer", provider: "provider-1"})
	in := peerBody(bundle)
	p, err := restoredRequest(in, "application/fhir+json", n.adaptiveRestore(ctx, in))
	if err != nil {
		t.Fatalf("restoredRequest: %v", err)
	}
	if !slices.Equal(p.Edits(), []relay.EditID{relay.EditDTRAdaptiveAddressRestore}) {
		t.Fatalf("edits %v, want E-13 alone", p.Edits())
	}
	out := string(relay.BytesForTest(p))
	if !strings.Contains(out, `"valueUrl":"`+base+`/forms/a"`) || strings.Contains(out, "provider-gw.example") || !strings.Contains(out, ownIdentity.Value) {
		t.Fatalf("forwarded %s", out)
	}
}

// Each PAS leg's forward passes the restore: an issued address in a
// submission, an update or an inquiry reaches the payer's system restored.
func TestDTRAddress_EveryPASLegRestores(t *testing.T) {
	for leg, bundle := range map[string][]byte{
		"pas-claim":         originatorBuiltConformantBundle(t, "MBR-COVERED"),
		"pas-claim-update":  originatorBuiltConformantUpdateBundle(t),
		"pas-claim-inquire": originatorBuiltInquiryBundle(t, "MBR-COVERED"),
	} {
		t.Run(leg, func(t *testing.T) {
			p := newStubPartner(t)
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, fixedClock, WithCRDService("shn-order-select"), WithPASStore(newCensusSoR()))
			key, err := dtraddress.DeriveKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)))
			if err != nil {
				t.Fatal(err)
			}
			base, _ := n.dtrBaseAt("2.0")
			tag := key.Tag(dtraddress.Address{Family: dtraddress.FamilyPayer, Payer: "payer", Provider: "provider-1", Line: "2.0", Base: base, Remainder: "/forms/a"})
			var doc map[string]any
			if err := json.Unmarshal(bundle, &doc); err != nil {
				t.Fatal(err)
			}
			var patient any
			for _, e := range doc["entry"].([]any) {
				if r, _ := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "Claim" {
					patient = r["patient"]
				}
			}
			if patient == nil {
				t.Fatal("the bundle's Claim names no patient")
			}
			doc["entry"] = append(doc["entry"].([]any), map[string]any{"resource": map[string]any{
				"resourceType": "QuestionnaireResponse", "status": "completed", "questionnaire": "#q", "subject": patient,
				"contained": []any{map[string]any{"resourceType": "Questionnaire", "id": "q", "status": "active", "extension": []any{map[string]any{
					"url": questionnaireAdaptiveExtension, "valueUrl": "https://provider-gw.example/dtr/payer/payer/2.0/" + tag + "/forms/a",
				}}}},
			}})
			addressed, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			ctx := withAddressIssuer(context.Background(), addressIssuer{key: key, payer: "payer", provider: "provider-1"})
			_, _ = n.Handle(ctx, leg, "corr-restore", "PCI-1", addressed) // the forward is the point, not the answer
			sent := string(p.lastBody)
			if !strings.Contains(sent, `"valueUrl":"`+base+`/forms/a"`) || strings.Contains(sent, "provider-gw.example") {
				t.Fatalf("the payer's system received %s", sent)
			}
		})
	}
}

// The restore records an address in the re-key's shape whose remainder is
// not clean, and a request it cannot read, and carries each as sent.
func TestDTRAddress_TheRestoreRecordsWhatItCannotRead(t *testing.T) {
	p := newStubPartner(t)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, fixedClock)
	key, _ := dtraddress.DeriveKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{6}, ed25519.SeedSize)))
	p.respByPath["/$submit-attachment"] = []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`)
	unclean := []byte(`{"resourceType":"Parameters","parameter":[{"name":"attachment","resource":{"resourceType":"QuestionnaireResponse","status":"completed",` +
		`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active","extension":[{"url":"` + questionnaireAdaptiveExtension +
		`","valueUrl":"https://provider-gw.example/dtr/payer/payer/2.0/` + strings.Repeat("A", 22) + `/forms/a/"}]}],"questionnaire":"#q"}}]}`)
	for name, row := range map[string]struct {
		body []byte
		want string
	}{
		"an unclean remainder":     {unclean, EditNotMadeAddressUncleanRemainder},
		"a request it cannot read": {[]byte(`not json`), EditNotMadeAddressUnreadableRequest},
	} {
		t.Run(name, func(t *testing.T) {
			x := &exchangeRecorder{}
			ctx := withAddressIssuer(context.WithValue(context.Background(), exchangeRecorderKey{}, x), addressIssuer{key: key, payer: "payer", provider: "provider-1"})
			if _, err := n.forwardCDexSubmit(ctx, peerBody(row.body)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(p.lastBody, row.body) {
				t.Fatalf("forwarded %s, want it as sent", p.lastBody)
			}
			if !slices.Equal(x.rec.EditsNotMade, []string{row.want}) {
				t.Fatalf("not made %v, want %s", x.rec.EditsNotMade, row.want)
			}
		})
	}
}

// A round whose frame names another line than its address is delivered at
// the address's line, which the tag binds, and the difference is recorded as
// a line carry (LineCarriedRequest) with its event, never refused. A round at
// its own address's line records none.
func TestDTRAddress_ARoundAtAnotherLineThanItsAddressIsRecorded(t *testing.T) {
	for _, row := range []struct {
		line    string
		carried bool
	}{{"2.2", true}, {"2.0", false}} {
		t.Run(row.line, func(t *testing.T) {
			d := newDTRPayer(t)
			var events []ObserverEvent
			d.g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			d.partner.respByPath["/forms/a"+nextPath] = d.partner.respByPath[nextPath]
			tag := d.g.paState().addressKey.Tag(dtraddress.Address{Family: dtraddress.FamilyPayer, Payer: d.g.cfg.HolderID,
				Provider: d.requester.ID, Line: row.line, Base: d.partner.srv.URL, Remainder: "/forms/a"})
			// sendAddressed frames the round at pa.dtr@2.0.
			got := d.sendAddressed(t, shnsdk.FrameOperationNextQuestion, "/payer/"+row.line+"/"+tag+"/forms/a", []byte(nextQuestionQR(dtrFrameMember)))
			if got.status != http.StatusOK || d.partner.lastPath != "/forms/a"+nextPath {
				t.Fatalf("status %d %s, delivered at %q: want the round delivered at its address", got.status, got.body, d.partner.lastPath)
			}
			var carried []ObserverEvent
			for _, e := range events {
				if e.Kind == LineCarriedEvent {
					carried = append(carried, e)
				}
			}
			if row.carried != (len(carried) == 1) || len(carried) > 1 {
				t.Fatalf("line-carried events %+v, want carried %v", carried, row.carried)
			}
			if row.carried && (!strings.Contains(carried[0].Detail, "2.2") || !strings.Contains(carried[0].Detail, "2.0")) {
				t.Fatalf("the event's detail %q does not name both lines", carried[0].Detail)
			}
		})
	}
}
