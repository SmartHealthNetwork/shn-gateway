package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// The delivered answer's tests in the core zone (zonefence): the writer, the
// guard on relayOriginationError and the check on who uses the mark. The rows
// through the routes, and the check that every operator route is test
// tooling, are in the PA zone (workstream_pa_delivered_answer_test.go).

// The titles a delivered answer's problem type states, pinned here apart from
// deliveredTitles.
const (
	deliveredYesTitle     = "A recipient received this request and may have acted on it"
	deliveredUnknownTitle = "The recipient may have received this request and acted on it"
)

// quoteJSON is s as a JSON string, as the gateway's answers encode it.
func quoteJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wantDeliveredProblem requires rec to be a delivered answer on a route whose
// answers are not FHIR: status, HubDeliveredHeader delivered, contentType, and
// the {"error"} text the caller has always read, first and byte for byte, then
// the problem details members: type, title, status and detail.
func wantDeliveredProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, delivered deliveredMaybe, contentType, text string) {
	t.Helper()
	title := map[deliveredMaybe]string{deliveredYes: deliveredYesTitle, deliveredUnknown: deliveredUnknownTitle}[delivered]
	want := `{"error":` + quoteJSON(t, text) +
		`,"type":"tag:smarthealthnetwork.org,2026:gateway/delivered/` + string(delivered) + `"` +
		`,"title":"` + title + `","status":` + strconv.Itoa(status) +
		`,"detail":` + quoteJSON(t, text) + "}\n"
	if rec.Code != status || rec.Header().Get(HubDeliveredHeader) != string(delivered) ||
		rec.Header().Get("Content-Type") != contentType || rec.Body.String() != want {
		t.Fatalf("answer %d delivered=%q %q\n%s\nwant %d delivered=%q %s\n%s", rec.Code, rec.Header().Get(HubDeliveredHeader),
			rec.Header().Get("Content-Type"), rec.Body.String(), status, delivered, contentType, want)
	}
}

// deliveredOutcomeBody is the OperationOutcome a FHIR operation route answers,
// read strictly.
type deliveredOutcomeBody struct {
	ResourceType string `json:"resourceType"`
	Issue        []struct {
		Severity    string `json:"severity"`
		Code        string `json:"code"`
		Diagnostics string `json:"diagnostics"`
		Details     *struct {
			Coding []struct {
				System string `json:"system"`
				Code   string `json:"code"`
			} `json:"coding"`
		} `json:"details"`
	} `json:"issue"`
}

func readOutcome(t *testing.T, rec *httptest.ResponseRecorder) deliveredOutcomeBody {
	t.Helper()
	var o deliveredOutcomeBody
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil || o.ResourceType != "OperationOutcome" || len(o.Issue) != 1 {
		t.Fatalf("answer %d %s is not one OperationOutcome issue: %v", rec.Code, rec.Body.String(), err)
	}
	return o
}

// wantDeliveredOutcome requires rec to be a delivered answer on a FHIR
// operation route: status, HubDeliveredHeader delivered, and the refusal's
// OperationOutcome, its issue's code and diagnostics as before, its details
// coding the delivered value.
func wantDeliveredOutcome(t *testing.T, rec *httptest.ResponseRecorder, status int, delivered deliveredMaybe, text string) {
	t.Helper()
	if rec.Code != status || rec.Header().Get(HubDeliveredHeader) != string(delivered) || rec.Header().Get("Content-Type") != "application/fhir+json" {
		t.Fatalf("answer %d delivered=%q %q %s, want %d delivered=%q application/fhir+json", rec.Code, rec.Header().Get(HubDeliveredHeader),
			rec.Header().Get("Content-Type"), rec.Body.String(), status, delivered)
	}
	o := readOutcome(t, rec)
	code := map[int]string{http.StatusInternalServerError: "processing", http.StatusBadGateway: "processing", http.StatusGatewayTimeout: "timeout"}[status]
	is := o.Issue[0]
	if is.Severity != "error" || is.Code != code || is.Diagnostics != text || is.Details == nil || len(is.Details.Coding) != 1 ||
		is.Details.Coding[0].System != "tag:smarthealthnetwork.org,2026:gateway/delivered" || is.Details.Coding[0].Code != string(delivered) {
		t.Fatalf("issue %+v %s, want code %q, diagnostics %q and details coding delivered %q", is, rec.Body.String(), code, text, delivered)
	}
}

// wantNotDelivered requires rec to be an answer that states no delivery, as
// before: no HubDeliveredHeader, the {"error"} body (or, on a FHIR operation
// route, the OperationOutcome without details) with text.
func wantNotDelivered(t *testing.T, rec *httptest.ResponseRecorder, fhir bool, status int, text string) {
	t.Helper()
	if set := rec.Header().Values(HubDeliveredHeader); len(set) > 0 || rec.Code != status {
		t.Fatalf("answer %d delivered=%q %s, want %d stating no delivery", rec.Code, rec.Header().Get(HubDeliveredHeader), rec.Body.String(), status)
	}
	if fhir {
		o := readOutcome(t, rec)
		if rec.Header().Get("Content-Type") != "application/fhir+json" || o.Issue[0].Details != nil || o.Issue[0].Diagnostics != text {
			t.Fatalf("answer %q %s, want an OperationOutcome of %q with no details", rec.Header().Get("Content-Type"), rec.Body.String(), text)
		}
		return
	}
	want := `{"error":` + quoteJSON(t, text) + "}\n"
	if rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != want {
		t.Fatalf("answer %q %s, want application/json %s", rec.Header().Get("Content-Type"), rec.Body.String(), want)
	}
}

// The writer in each envelope, with its status and text as given: on a FHIR
// operation route the OperationOutcome coding the value; on a route outside
// the full promise (problemDetailsWriter, under the route's scope) problem
// details as application/problem+json; on any other route the same members as
// application/json, the media type its errors have always had.
func TestWriteDeliveredMaybe_EachEnvelope(t *testing.T) {
	t.Parallel()
	const text = "the recipient may have received this request (x): check its outcome before resending"
	for _, d := range []deliveredMaybe{deliveredYes, deliveredUnknown} {
		for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
			rec := httptest.NewRecorder()
			writeDeliveredMaybe(rec, status, d, text)
			wantDeliveredProblem(t, rec, status, d, "application/json", text)

			rec = httptest.NewRecorder()
			scoped, _ := (&Gateway{}).withScope(&problemDetailsWriter{rec}, relay.RoleRequester)
			writeDeliveredMaybe(scoped, status, d, text)
			wantDeliveredProblem(t, rec, status, d, "application/problem+json", text)

			rec = httptest.NewRecorder()
			writeDeliveredMaybe(&fhirOperationWriter{rec}, status, d, text)
			wantDeliveredOutcome(t, rec, status, d, text)
		}
	}
}

// The guard on relayOriginationError: every error it answers is marked
// delivered (writeDeliveredMaybe) exactly when the recipient may have received
// the request, and with the value each row states: yes when the recipient
// answered (the Hub marked yes; an answer refused for size; an answer refused
// after its token verified), unknown when it may have received it (the Hub
// marked unknown; an unmarked Hub 5xx; a connection lost or a wait ended after
// the send; a Hub answer that could not be read, decoded or verified). Every
// other answer states none, in both envelopes. An unknown answer never says
// the recipient answered.
func TestRelayOriginationError_MarksDeliveredExactlyWhenTheRecipientMayHaveReceivedIt(t *testing.T) {
	t.Parallel()
	hub := func(status int, body, mark string) error { return hubRefusal(status, []byte(body), mark) }
	notActed := hubRefusal(http.StatusServiceUnavailable, []byte(`{"error":"not yet"}`), "no")
	notActed.notActed, notActed.retryAfter = true, 5
	type row struct {
		err  error
		want string // "", "yes" or "unknown"
	}
	rows := map[string]row{
		"ownership fault":                     {fmt.Errorf("x: %w", relay.ErrOwnershipRefused), ""},
		"no shared line":                      {&RouteRefusalError{Contract: "pa.pas", LegType: "pas-claim", Recipient: "payer"}, ""},
		"envelope too large":                  {&envelopeTooLargeError{limit: 8 << 20}, ""},
		"ambiguous coverage":                  {errAmbiguousCoverage, ""},
		"timeout, unsent":                     {&hubTimeoutError{budget: time.Second}, ""},
		"timeout, sent":                       {&hubTimeoutError{budget: time.Second, sent: true}, "unknown"},
		"timeout, sent, wrapped":              {fmt.Errorf("leg: %w", &hubTimeoutError{sent: true}), "unknown"},
		"authz limited":                       {&authzLimited{retryAfter: "3"}, ""},
		"authz unreachable":                   {errAuthzUnreachable, ""},
		"authorization denied":                {errAuthorizationDenied, ""},
		"hub routing failed":                  {errHubUnreachable, ""},
		"not acted":                           {notActed, ""},
		"connection lost":                     {&hubRefusalError{status: http.StatusBadGateway, reason: "the connection to the Hub failed after the request was sent", delivered: "unknown", leg: "pas-claim"}, "unknown"},
		"Hub answer unreadable":               {&answerLostError{cause: "the Hub's answer could not be read"}, "unknown"},
		"Hub answer not an envelope":          {&answerLostError{cause: "the Hub's answer is not an envelope"}, "unknown"},
		"answer token fails verification":     {&answerLostError{cause: "response leg authorization failed"}, "unknown"},
		"answer token fails, wrapped":         {fmt.Errorf("leg: %w", &answerLostError{cause: "response leg authorization failed"}), "unknown"},
		"verified answer fails decryption":    {&answerLostError{cause: "response decryption failed", verified: true}, "yes"},
		"verified answer, frame mismatch":     {&answerLostError{cause: "response contract version mismatch: x", rule: RefusalRouting, verified: true}, "yes"},
		"answer from another sender":          {&answerLostError{cause: "response sender mismatch"}, "unknown"},
		"answer on another correlation":       {&answerLostError{cause: "response correlation mismatch"}, "unknown"},
		"verified answer, wrapped":            {fmt.Errorf("leg: %w", &answerLostError{cause: "response frame decode failed", verified: true}), "yes"},
		"answer too large":                    {&answerLostError{cause: errResponseTooLarge, rule: RefusalRouting, tooLarge: true}, "yes"},
		"relayed answer":                      {&RelayError{Status: http.StatusConflict, Body: []byte(`{"resourceType":"OperationOutcome"}`), ContentType: "application/fhir+json", leg: "pas-claim"}, ""},
		"some other failure":                  {errors.New("something else"), ""},
		"recipient refused at its edge":       {hub(http.StatusBadGateway, `{"error":"`+recipientRefusedPrefix+`(403)"}`, "no"), ""},
		"leg not taken":                       {hub(http.StatusUnprocessableEntity, `{"error":"recipient does not take this leg"}`, "no"), ""},
		"hub 413":                             {hub(http.StatusRequestEntityTooLarge, `{"error":"`+errRequestTooLarge+`"}`, "no"), ""},
		"unmarked 4xx in front of the Hub":    {hub(http.StatusForbidden, `<html>Forbidden</html>`, ""), ""},
		"unmarked 5xx in front of the Hub":    {hub(http.StatusBadGateway, `<html>Bad Gateway</html>`, ""), "unknown"},
		"unmarked 504 in front of the Hub":    {hub(http.StatusGatewayTimeout, ``, ""), "unknown"},
		"Hub mark outside its three values":   {hub(http.StatusBadGateway, `{"error":"x"}`, "maybe"), "unknown"},
		"Hub mark outside its values, on 4xx": {hub(http.StatusConflict, `{"error":"x"}`, "maybe"), ""},
	}
	// The Hub's marked answers at every status: its mark is the value, and
	// "no" is none.
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		for _, mark := range []string{"yes", "unknown", "no"} {
			want := mark
			if mark == "no" {
				want = ""
			}
			for _, reason := range []string{"response audit append failed", hubReasonAnswerTooLarge, hubReasonRecipientRefusedAnswer} {
				rows[fmt.Sprintf("hub %d marked %s: %s", status, mark, reason)] = row{hub(status, `{"error":"`+reason+`","rule":"authority"}`, mark), want}
			}
		}
	}
	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	g := &Gateway{}
	for _, name := range names {
		r := rows[name]
		for _, fhir := range []bool{false, true} {
			rec := httptest.NewRecorder()
			var rw http.ResponseWriter = rec
			if fhir {
				rw = &fhirOperationWriter{rec}
			}
			if !g.relayOriginationError(rw, r.err) {
				if r.want != "" {
					t.Fatalf("%s: not answered, want delivered %q", name, r.want)
				}
				continue
			}
			got := rec.Header().Values(HubDeliveredHeader)
			body := rec.Body.String()
			typed := strings.Contains(body, "tag:smarthealthnetwork.org,2026:gateway/delivered")
			if r.want == "" {
				if len(got) > 0 || typed {
					t.Errorf("%s (fhir %v): answered %d delivered=%v %s, want no delivered mark", name, fhir, rec.Code, got, body)
				}
				continue
			}
			if len(got) != 1 || got[0] != r.want || !strings.Contains(body, `/`+r.want+`"`) && !strings.Contains(body, `"code":"`+r.want+`"`) {
				t.Errorf("%s (fhir %v): answered %d delivered=%v %s, want delivered %q", name, fhir, rec.Code, got, body, r.want)
			}
			if r.want == "unknown" {
				for _, phrase := range []string{"received this request and answered", "its gateway refused to send the answer"} {
					if strings.Contains(body, phrase) {
						t.Errorf("%s (fhir %v): an unknown answer says %q: %s", name, fhir, phrase, body)
					}
				}
			}
		}
	}
}

// The unknown title never says the recipient answered: a request marked
// unknown may not have reached it (test/invariants pins the text too).
func TestDeliveredTitles_UnknownNeverSaysTheRecipientAnswered(t *testing.T) {
	if deliveredTitles[deliveredYes] != deliveredYesTitle || deliveredTitles[deliveredUnknown] != deliveredUnknownTitle || len(deliveredTitles) != 2 {
		t.Fatalf("titles %v, want the two pinned", deliveredTitles)
	}
	for _, phrase := range []string{"received this request and answered", "its gateway refused to send the answer"} {
		if strings.Contains(deliveredTitles[deliveredUnknown], phrase) {
			t.Fatalf("the unknown title %q says %q", deliveredTitles[deliveredUnknown], phrase)
		}
	}
}

// deliveredHeaderUses names every use of the delivered mark in files (each
// keyed by its package's name): every reference to the HubDeliveredHeader
// identifier outside its own declaration, by the function or declaration it
// is in, and every string literal naming the header (in any case) outside
// that declaration's value, as "literal in <function>".
func deliveredHeaderUses(files []*ast.File) []string {
	var found []string
	scan := func(key string, n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if n.Name == "HubDeliveredHeader" {
					found = append(found, key)
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING && strings.Contains(strings.ToLower(n.Value), "x-shn-delivered") {
					found = append(found, "literal in "+key)
				}
			}
			return true
		})
	}
	for _, f := range files {
		pkg := f.Name.Name
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				key := pkg + "." + d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					t := d.Recv.List[0].Type
					if s, ok := t.(*ast.StarExpr); ok {
						t = s.X
					}
					if id, ok := t.(*ast.Ident); ok {
						key = pkg + "." + id.Name + "." + d.Name.Name
					}
				}
				scan(key, d)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						scan(pkg+" declaration", spec)
						continue
					}
					own := len(vs.Names) == 1 && vs.Names[0].Name == "HubDeliveredHeader" && pkg == "engine"
					for _, v := range vs.Values {
						if !own {
							scan(pkg+" declaration of "+vs.Names[0].Name, v)
						}
					}
					if vs.Type != nil {
						scan(pkg+" declaration of "+vs.Names[0].Name, vs.Type)
					}
				}
			}
		}
	}
	sort.Strings(found)
	return found
}

// moduleSources parses every non-test Go file of the gateway module, plus
// extra (path → source), whatever its build constraint.
func moduleSources(t *testing.T, extra map[string]string) []*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	err := filepath.WalkDir("..", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "testdata" || (strings.HasPrefix(n, ".") && n != "..") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, src := range extra {
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) < 100 {
		t.Fatalf("read %d files of the gateway module: the check must see them", len(files))
	}
	return files
}

// deliveredHeaderAllowed is every use of the delivered mark in the gateway
// module: the requester reading the Hub's mark (postEnvelope), its one writer
// of an error answer to its caller (writeRefusal), which states delivery with
// the body's marks, that writer's floor (deliveryWriter.WriteHeader, reading
// then setting it), a recipient's gateway attesting to the Hub that its
// system answered (attestAnswerRefused), and a requester's gateway reading
// the Hub's mark on its 503 to POST /requester-binding, where "no" is the
// Hub's statement that it took none of the record, so it is sent again once
// (resendRequesterBinding). Another would be a delivered answer
// that does not carry the problem members or the OperationOutcome coding, or
// a mark set past the writer.
var deliveredHeaderAllowed = []string{"engine.Gateway.postEnvelope", "engine.attestAnswerRefused",
	"engine.deliveryWriter.WriteHeader", "engine.deliveryWriter.WriteHeader", "engine.resendRequesterBinding", "engine.writeRefusal"}

func TestDeliveredHeader_UsedOnlyByItsReaderAndWriters(t *testing.T) {
	t.Parallel()
	if got := deliveredHeaderUses(moduleSources(t, nil)); strings.Join(got, ",") != strings.Join(deliveredHeaderAllowed, ",") {
		t.Fatalf("the delivered mark is used by %v, want only %v", got, deliveredHeaderAllowed)
	}
}

// The check names each way of setting the mark past the writer.
func TestDeliveredHeader_CheckFires(t *testing.T) {
	t.Parallel()
	const head = "package engine\nimport \"net/http\"\n"
	for name, row := range map[string]struct{ src, want string }{
		"through the header":          {head + `func markItself(w http.ResponseWriter) { w.Header().Set(HubDeliveredHeader, "yes") }`, "engine.markItself"},
		"through a variable":          {head + `func markItself(w http.ResponseWriter) { h := w.Header(); h.Set(HubDeliveredHeader, "yes") }`, "engine.markItself"},
		"by index":                    {head + `func markItself(w http.ResponseWriter) { w.Header()[HubDeliveredHeader] = []string{"yes"} }`, "engine.markItself"},
		"the literal":                 {head + `func markItself(w http.ResponseWriter) { w.Header().Set("X-SHN-Delivered", "yes") }`, "literal in engine.markItself"},
		"the literal, another case":   {head + `func markItself(w http.ResponseWriter) { w.Header().Set("x-shn-delivered", "unknown") }`, "literal in engine.markItself"},
		"a map handed to writeAnswer": {head + `func (g *Gateway) markItself(w http.ResponseWriter) { g.writeAnswer(w, 502, "application/json", map[string]string{HubDeliveredHeader: "yes"}, relay.Payload{}, relay.Key{}) }`, "engine.Gateway.markItself"},
		"a name of its own":           {head + `const myMark = HubDeliveredHeader`, "engine declaration of myMark"},
		"another package":             {"package app\nimport \"github.com/SmartHealthNetwork/shn-gateway/engine\"\nvar mark = engine.HubDeliveredHeader", "app declaration of mark"},
	} {
		t.Run(name, func(t *testing.T) {
			got := deliveredHeaderUses(moduleSources(t, map[string]string{"mark_itself.go": row.src}))
			found := false
			for _, k := range got {
				found = found || k == row.want
			}
			if !found {
				t.Fatalf("the check missed %q: %v", row.want, got)
			}
		})
	}
}

// engineScope is one top-level declaration of the engine package's own
// source the fences read: a function by its name ("engine.f", or
// "engine.T.m" for a method), or a package-level declaration by its first
// name ("engine declaration of v"), so a function literal assigned at package
// level is read as well.
type engineScope struct {
	key  string
	node ast.Node
}

func engineScopes(files []*ast.File) []engineScope {
	var out []engineScope
	for _, f := range files {
		if f.Name.Name != "engine" {
			continue
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				key := "engine." + d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					t := d.Recv.List[0].Type
					if s, ok := t.(*ast.StarExpr); ok {
						t = s.X
					}
					if id, ok := t.(*ast.Ident); ok {
						key = "engine." + id.Name + "." + d.Name.Name
					}
				}
				out = append(out, engineScope{key, d.Body})
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
						out = append(out, engineScope{"engine declaration of " + vs.Names[0].Name, vs})
					}
				}
			}
		}
	}
	return out
}

// refusalWriterAllowed is every engine function that may write an HTTP status
// itself, outside writeRefusal: the writer wrappers' own WriteHeader (the
// delivery mark's floor among them), the two bodies' last step
// (writeRefusalBody, from writeRefusal alone; writeLocalJSON, from writeJSON
// alone, below 400), an answer written as it was carried or built
// (writeAnswer: deliveryWriter's floor states delivery on it), and the token
// endpoint's OAuth error (oauthErr), which no leg precedes. http.Error is
// allowed only on the two discovery documents, which send no leg.
var refusalWriterAllowed = map[string][]string{
	"WriteHeader": {"engine.deliveryWriter.WriteHeader", "engine.exchangeWriter.WriteHeader", "engine.notActedFence.WriteHeader",
		"engine.writeRefusalBody", "engine.writeLocalJSON", "engine.Gateway.writeAnswer", "engine.oauthErr"},
	"http.Error":       {"engine.Gateway.handleDavinciConfiguration", "engine.Gateway.handleIngressMetadata"},
	"writeRefusalBody": {"engine.writeRefusal"},
	"writeLocalJSON":   {"engine.writeJSON"},
	// The body's delivery marks go only into an answer this gateway builds:
	// one carried from a recipient keeps its bytes (relay fidelity).
	"markDelivered": {"engine.writeRefusal"},
}

// refusalWriterBypasses is every call in the engine package's own source that
// would write an error answer past writeRefusal, and so past the delivery
// mark: a WriteHeader with a status not a 2xx constant, http.Error, or a
// call of writeRefusalBody or writeLocalJSON, outside the functions
// refusalWriterAllowed names. Each is "<what> in <function>".
func refusalWriterBypasses(files []*ast.File) []string {
	allowed := func(what, fn string) bool { return slices.Contains(refusalWriterAllowed[what], fn) }
	success := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "http" && slices.Contains([]string{"StatusOK", "StatusCreated", "StatusAccepted", "StatusNoContent"}, sel.Sel.Name)
	}
	var found []string
	for _, sc := range engineScopes(files) {
		key := sc.key
		ast.Inspect(sc.node, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			what := ""
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if id, ok := fun.X.(*ast.Ident); ok && id.Name == "http" && fun.Sel.Name == "Error" {
					what = "http.Error"
				} else if fun.Sel.Name == "WriteHeader" && !(len(call.Args) == 1 && success(call.Args[0])) {
					what = "WriteHeader"
				}
			case *ast.Ident:
				if fun.Name == "writeRefusalBody" || fun.Name == "writeLocalJSON" || fun.Name == "markDelivered" {
					what = fun.Name
				}
			}
			if what != "" && !allowed(what, key) {
				found = append(found, what+" in "+key)
			}
			return true
		})
	}
	sort.Strings(found)
	return found
}

// Every error answer this gateway builds goes through writeRefusal, the one
// writer that states delivery once a leg of the call was answered: no engine
// function writes an error status, or a refusal's or a local answer's body,
// past it.
func TestRefusals_AllGoThroughWriteRefusal(t *testing.T) {
	t.Parallel()
	if got := refusalWriterBypasses(moduleSources(t, nil)); len(got) != 0 {
		t.Fatalf("error answers written past writeRefusal (and so past the delivery mark): %v", got)
	}
}

// The check names each way past writeRefusal, and passes a 2xx written
// directly.
func TestRefusals_CheckFires(t *testing.T) {
	t.Parallel()
	const head = "package engine\nimport \"net/http\"\n"
	for name, row := range map[string]struct{ src, want string }{
		"a status written directly": {head + `func refuseItself(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }`, "WriteHeader in engine.refuseItself"},
		"a status from a variable":  {head + `func refuseItself(w http.ResponseWriter, s int) { w.WriteHeader(s) }`, "WriteHeader in engine.refuseItself"},
		"on a method":               {head + `func (g *Gateway) refuseItself(w http.ResponseWriter) { w.WriteHeader(502) }`, "WriteHeader in engine.Gateway.refuseItself"},
		"http.Error":                {head + `func refuseItself(w http.ResponseWriter) { http.Error(w, "no", http.StatusBadGateway) }`, "http.Error in engine.refuseItself"},
		"the refusal body":          {head + `func refuseItself(w http.ResponseWriter) { writeRefusalBody(w, 502, "application/json", nil) }`, "writeRefusalBody in engine.refuseItself"},
		"the local body":            {head + `func refuseItself(w http.ResponseWriter) { writeLocalJSON(w, 502, "application/json", nil) }`, "writeLocalJSON in engine.refuseItself"},
		"a carried answer marked":   {head + `func (g *Gateway) markCarried(b []byte) []byte { m, _ := markDelivered(b, 502, deliveredYes); return m }`, "markDelivered in engine.Gateway.markCarried"},
		"at package level":          {head + "var refuseItself = func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }", "WriteHeader in engine declaration of refuseItself"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := refusalWriterBypasses(moduleSources(t, map[string]string{"refuse_itself.go": row.src})); !slices.Contains(got, row.want) {
				t.Fatalf("the check missed %q: %v", row.want, got)
			}
		})
	}
	t.Run("a 2xx written directly", func(t *testing.T) {
		src := head + `func answerItself(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) }`
		if got := refusalWriterBypasses(moduleSources(t, map[string]string{"answer_itself.go": src})); len(got) != 0 {
			t.Fatalf("a 2xx flagged: %v", got)
		}
	})
}

// deliveryMarkSetters is every function in the engine package's own source
// that sets a call's delivery mark: a call of legAnswered, or a Store on a
// mark's answered. Each is "<what> in <function>".
func deliveryMarkSetters(files []*ast.File) []string {
	var found []string
	for _, sc := range engineScopes(files) {
		key := sc.key
		ast.Inspect(sc.node, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				// A call of legAnswered is counted below, as a use of the name.
				if fun, ok := n.Fun.(*ast.SelectorExpr); ok && slices.Contains([]string{"Store", "Swap", "CompareAndSwap"}, fun.Sel.Name) {
					x := fun.X
					for {
						if p, ok := x.(*ast.ParenExpr); ok {
							x = p.X
						} else if u, ok := x.(*ast.UnaryExpr); ok {
							x = u.X
						} else {
							break
						}
					}
					if inner, ok := x.(*ast.SelectorExpr); ok && inner.Sel.Name == "answered" {
						found = append(found, "answered."+fun.Sel.Name+" in "+key)
					}
				}
			case *ast.Ident:
				// Any use of the name, a call or the function as a value.
				if n.Name == "legAnswered" {
					found = append(found, "legAnswered in "+key)
				}
			}
			return true
		})
	}
	sort.Strings(found)
	return found
}

// A call's delivery mark is set at one point only: roundTrip's success
// return, after roundTripInner verified the answer (legAnswered), and
// legAnswered is the only writer of the mark.
func TestDeliveryMark_SetOnlyByRoundTrip(t *testing.T) {
	t.Parallel()
	want := []string{"answered.Store in engine.legAnswered", "legAnswered in engine.Gateway.roundTrip"}
	if got := deliveryMarkSetters(moduleSources(t, nil)); !slices.Equal(got, want) {
		t.Fatalf("the delivery mark is set by %v, want only %v", got, want)
	}
	for name, row := range map[string]struct{ src, want string }{
		"another caller":    {"package engine\nimport \"context\"\nfunc markEarly(ctx context.Context) { legAnswered(ctx) }", "legAnswered in engine.markEarly"},
		"the mark directly": {"package engine\nfunc markEarly(m *deliveryMark) { m.answered.Store(true) }", "answered.Store in engine.markEarly"},
		"by swap":           {"package engine\nfunc markEarly(m *deliveryMark) { m.answered.Swap(true) }", "answered.Swap in engine.markEarly"},
		"by compare":        {"package engine\nfunc markEarly(m *deliveryMark) { (&m.answered).CompareAndSwap(false, true) }", "answered.CompareAndSwap in engine.markEarly"},
		"as a value":        {"package engine\nimport \"context\"\nvar markLater = []func(context.Context){legAnswered}", "legAnswered in engine declaration of markLater"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := deliveryMarkSetters(moduleSources(t, map[string]string{"mark_early.go": row.src})); !slices.Contains(got, row.want) {
				t.Fatalf("the check missed %q: %v", row.want, got)
			}
		})
	}
}

// markedCall is a requester call's writer with its delivery mark (as
// recordExchange gives it), the mark set when answered: a leg of the call
// handed back a verified answer (legAnswered).
func markedCall(t *testing.T, answered bool) (*httptest.ResponseRecorder, http.ResponseWriter) {
	t.Helper()
	rec := httptest.NewRecorder()
	w, r := withDeliveryMark(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if answered {
		legAnswered(r.Context())
	}
	return rec, w
}

// writeRefusal decides delivery by the call: before a leg was answered a
// refusal states none, as before; after it every refusal is delivered yes,
// whatever it is and whatever the leg's own failure said (an unknown included,
// a recipient of an earlier leg may have acted), in each envelope, with every
// member and issue it had kept and its status unchanged.
func TestWriteRefusal_AfterAnAnsweredLegIsDeliveredYes(t *testing.T) {
	t.Parallel()
	const text = "PAS response has inconsistent patient linkage"
	t.Run("before any leg was answered", func(t *testing.T) {
		rec, w := markedCall(t, false)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": text})
		wantNotDelivered(t, rec, false, http.StatusBadGateway, text)
		rec, w = markedCall(t, false)
		writeJSON(&fhirOperationWriter{w}, http.StatusBadGateway, map[string]string{"error": text})
		wantNotDelivered(t, rec, true, http.StatusBadGateway, text)
	})
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(fmt.Sprintf("after/%d", status), func(t *testing.T) {
			rec, w := markedCall(t, true)
			writeJSON(w, status, map[string]string{"error": text})
			wantDeliveredProblem(t, rec, status, deliveredYes, "application/json", text)

			rec, w = markedCall(t, true)
			scoped, _ := (&Gateway{}).withScope(&problemDetailsWriter{w}, relay.RoleRequester)
			writeJSON(scoped, status, map[string]string{"error": text})
			wantDeliveredProblem(t, rec, status, deliveredYes, "application/problem+json", text)
		})
	}
	t.Run("after/the OperationOutcome", func(t *testing.T) {
		rec, w := markedCall(t, true)
		writeJSON(&fhirOperationWriter{w}, http.StatusBadGateway, map[string]string{"error": text})
		wantDeliveredOutcome(t, rec, http.StatusBadGateway, deliveredYes, text)
	})
	t.Run("after/an outcome's echoed issues are kept", func(t *testing.T) {
		rec, w := markedCall(t, true)
		writeJSON(&fhirOperationWriter{w}, http.StatusUnprocessableEntity, refusalOutcome(http.StatusUnprocessableEntity, "ingress validation failed", []string{"Coverage.payor: minimum required = 1"}))
		var o struct {
			Issue []struct {
				Code, Diagnostics string
				Details           *struct {
					Coding []struct{ System, Code string }
				}
			}
		}
		if rec.Header().Get(HubDeliveredHeader) != "yes" || json.Unmarshal(rec.Body.Bytes(), &o) != nil || len(o.Issue) != 2 ||
			o.Issue[0].Details == nil || len(o.Issue[0].Details.Coding) != 1 || o.Issue[0].Details.Coding[0].Code != "yes" ||
			o.Issue[1].Details != nil || o.Issue[1].Diagnostics != "Coverage.payor: minimum required = 1" || o.Issue[1].Code != "invalid" {
			t.Fatalf("delivered=%q %s, want yes coded on the first issue, the echoed issue kept", rec.Header().Get(HubDeliveredHeader), rec.Body)
		}
	})
	t.Run("after/a body's other members are kept", func(t *testing.T) {
		rec, w := markedCall(t, true)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": text, "rule": "integrity"})
		want := `{"error":` + quoteJSON(t, text) + `,"rule":"integrity","type":"tag:smarthealthnetwork.org,2026:gateway/delivered/yes","title":"` +
			deliveredYesTitle + `","status":502,"detail":` + quoteJSON(t, text) + "}\n"
		if rec.Header().Get(HubDeliveredHeader) != "yes" || rec.Body.String() != want {
			t.Fatalf("delivered=%q\n%s\nwant\n%s", rec.Header().Get(HubDeliveredHeader), rec.Body, want)
		}
	})
	t.Run("after/an unknown from a later leg is yes", func(t *testing.T) {
		rec, w := markedCall(t, true)
		writeDeliveredMaybe(w, http.StatusGatewayTimeout, deliveredUnknown, "no answer on the hub leg within 30s (hub leg timeout)")
		wantDeliveredProblem(t, rec, http.StatusGatewayTimeout, deliveredYes, "application/json", "no answer on the hub leg within 30s (hub leg timeout)")
	})
	t.Run("after/an error written past writeRefusal states it in the header", func(t *testing.T) {
		rec, w := markedCall(t, true)
		w.WriteHeader(http.StatusBadGateway)
		if rec.Header().Get(HubDeliveredHeader) != "yes" {
			t.Fatalf("delivered=%q, want the floor's yes", rec.Header().Get(HubDeliveredHeader))
		}
		rec, w = markedCall(t, true)
		w.WriteHeader(http.StatusOK)
		if len(rec.Header().Values(HubDeliveredHeader)) != 0 {
			t.Fatalf("a 2xx marked %q", rec.Header().Get(HubDeliveredHeader))
		}
	})
}

// wantAnsweredRefusalYes requires rec, a refusal written after a leg of its
// call was answered, to state delivered yes: the header, and in its body the
// coding on an OperationOutcome's first issue, or the problem type after the
// error member of any other body.
func wantAnsweredRefusalYes(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var doc struct {
		Error        string `json:"error"`
		Type         string `json:"type"`
		ResourceType string `json:"resourceType"`
		Issue        []struct {
			Details *struct {
				Coding []struct{ System, Code string } `json:"coding"`
			} `json:"details"`
		} `json:"issue"`
	}
	ok := rec.Code >= http.StatusBadRequest && rec.Header().Get(HubDeliveredHeader) == "yes" && json.Unmarshal(rec.Body.Bytes(), &doc) == nil
	if ok && doc.ResourceType == "OperationOutcome" {
		ok = len(doc.Issue) > 0 && doc.Issue[0].Details != nil && len(doc.Issue[0].Details.Coding) == 1 &&
			doc.Issue[0].Details.Coding[0].System == deliveredSystem && doc.Issue[0].Details.Coding[0].Code == "yes"
	} else if ok {
		ok = doc.Error != "" && doc.Type == deliveredSystem+"/yes"
	}
	if !ok {
		t.Fatalf("answer %d delivered=%q %s, want delivered yes after the answered leg", rec.Code, rec.Header().Get(HubDeliveredHeader), rec.Body.String())
	}
}

// wantNoDeliveryStated requires rec to carry no HubDeliveredHeader: an answer
// relayed as the recipient gave it, or a refusal before any leg was answered.
func wantNoDeliveryStated(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if v := rec.Header().Values(HubDeliveredHeader); len(v) != 0 {
		t.Fatalf("answer %d delivered=%q %s, want no delivery stated", rec.Code, v, rec.Body.String())
	}
}
