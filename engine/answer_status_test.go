//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer gateway frames its system's own 2xx answer with that status, for a
// requester that decodes frames; one that does not gets the answer bare, which
// it reads as 200.
func TestRespondLeg_FramesTheParticipantsStatus(t *testing.T) {
	answer := []byte(`{"resourceType":"ClaimResponse","status":"active"}`)
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		body := answer
		if status == http.StatusNoContent {
			body = nil
		}
		for _, framed := range []bool{true, false} {
			g, requester := newInboundTestGateway(t, framed)
			rec := httptest.NewRecorder()
			g.respondLeg(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select",
				"corr-1", relayedResponse(body), statedAnswer{status: status}, "pci-1", requester.ID, "", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("%d framed=%v: to-Hub status %d, want 200", status, framed, rec.Code)
			}
			payload := openResponseLeg(t, requester, rec.Body.Bytes())
			if !framed {
				if shnsdk.IsFramed(payload) || !bytes.Equal(payload, body) {
					t.Fatalf("%d: a requester without frames got %q, want the answer bare", status, payload)
				}
				continue
			}
			hdr, got, err := shnsdk.DecodeHTTPFrame(payload)
			if err != nil {
				t.Fatalf("%d: decode frame: %v", status, err)
			}
			if hdr.Status != status || !bytes.Equal(got, body) {
				t.Fatalf("framed %d %q, want %d and the answer exactly", hdr.Status, got, status)
			}
		}
	}
}

// A success a responder signals on the error path is answered on the success
// seal by the same rule as any other: the status its relayed answer was given
// (AnswerStatus), never the signal itself.
func TestRespondLegError_2xxKeepsTheAnswersStatus(t *testing.T) {
	answer := []byte(`{"resourceType":"ClaimResponse","status":"active"}`)
	for _, row := range []struct {
		name string
		r    LegResult
		want int
	}{
		{"relayed, given 201", LegResult{Status: http.StatusCreated, AnswerStatus: http.StatusCreated, Response: relayedResponse(answer)}, http.StatusCreated},
		{"relayed, given none", LegResult{Status: http.StatusCreated, Response: relayedResponse(answer)}, http.StatusOK},
		{"built by the gateway", LegResult{Status: http.StatusCreated, AnswerStatus: http.StatusCreated, Response: testResponse(answer)}, http.StatusOK},
	} {
		g, requester := newInboundTestGateway(t, true)
		rec := httptest.NewRecorder()
		g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select",
			"corr-1", row.r, "pci-1", requester.ID, "", "")
		hdr, got, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: decode frame: %v", row.name, err)
		}
		if hdr.Status != row.want || !bytes.Equal(got, answer) {
			t.Fatalf("%s: framed %d %q, want %d and the answer exactly", row.name, hdr.Status, got, row.want)
		}
	}
	// The misuse path frames what the answer states, its headers with it.
	g, requester := newInboundTestGateway(t, true)
	rec := httptest.NewRecorder()
	g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select", "corr-1",
		LegResult{Status: http.StatusCreated, AnswerStatus: http.StatusCreated, AnswerHeaders: map[string]string{shnsdk.FrameHeaderETag: `"v1"`}, Response: relayedResponse(answer)},
		"pci-1", requester.ID, "", "")
	hdr, _, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
	if err != nil || hdr.Headers[shnsdk.FrameHeaderETag] != `"v1"` {
		t.Fatalf("framed %+v, %v", hdr, err)
	}
}

// The status a payer gateway frames is its system's own only for an answer
// relayed from that system; an answer it built itself is 200, and a status
// that is not a success is never framed as one.
func TestLegResult_AnswerStatus(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle"}`)
	built, err := relay.Authored(relay.BuilderSDKPatientDTR, body, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name string
		r    LegResult
		want int
	}{
		{"relayed, 201", LegResult{Response: relayedResponse(body), AnswerStatus: http.StatusCreated}, http.StatusCreated},
		{"relayed, 204", LegResult{Response: relayedResponse(nil), AnswerStatus: http.StatusNoContent}, http.StatusNoContent},
		{"relayed, none stated", LegResult{Response: relayedResponse(body)}, http.StatusOK},
		{"relayed, not a success", LegResult{Response: relayedResponse(body), AnswerStatus: http.StatusNotFound}, http.StatusOK},
		{"built by the gateway", LegResult{Response: built, AnswerStatus: http.StatusCreated}, http.StatusOK},
	} {
		if got := row.r.answerStatus(); got != row.want {
			t.Errorf("%s: answerStatus %d, want %d", row.name, got, row.want)
		}
	}
}

// The provider gateway writes a framed answer to its participant's system with
// the status the frame states; a bare answer, or a frame from a payer gateway
// that framed every success as 200, is written 200.
func TestCRDIngress_WritesTheAnswersStatus(t *testing.T) {
	answer := []byte(`{"cards":[]}`)
	for _, row := range []struct {
		name string
		lr   LegResult
		want int
		body []byte
	}{
		{"framed 201", LegResult{Response: testResponse(answer), AnswerStatus: http.StatusCreated}, http.StatusCreated, answer},
		{"framed 202", LegResult{Response: testResponse(answer), AnswerStatus: http.StatusAccepted}, http.StatusAccepted, answer},
		{"framed 200", LegResult{Response: testResponse(answer), AnswerStatus: http.StatusOK}, http.StatusOK, answer},
		{"bare", LegResult{Response: testResponse(answer)}, http.StatusOK, answer},
	} {
		t.Run(row.name, func(t *testing.T) {
			env := newInProcessExchange(t)
			env.payerReturns(row.lr)
			rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			if rec.Code != row.want || !bytes.Equal(rec.Body.Bytes(), row.body) {
				t.Fatalf("EHR got %d %q, want %d %q", rec.Code, rec.Body.Bytes(), row.want, row.body)
			}
		})
	}
}

// A frame stating 204 No Content that carries a body says two things: the
// provider's system could be written the status or the bytes, not both. The
// answer is not read, at every level, and the refusal is integrity.
func TestCRDIngress_NoContentWithABodyIsRefused(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			env.payerReturns(LegResult{Response: testResponse([]byte(`{"cards":[]}`)), AnswerStatus: http.StatusNoContent})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCRD)(rec, crdIngressPost(conformantCRDRequest("MBR-COVERED")))
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "response frame states 204 No Content and carries a body") {
				t.Fatalf("EHR got %d %s", rec.Code, rec.Body.String())
			}
			wantRefusal(t, got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalIntegrity)
		})
	}
	t.Run("an empty 204 is carried", func(t *testing.T) {
		env := newInProcessExchange(t)
		env.originator.cfg.ConformanceEnforcement = EnforcementNone
		env.payerReturns(LegResult{Response: testResponse(nil), AnswerStatus: http.StatusNoContent})
		rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("EHR got %d %q, want 204 and no body", rec.Code, rec.Body.Bytes())
		}
	})
}

// Every registered edit of an answer's body drops its ETag, by the row that
// discloses the edit, and leaves what still describes the answer; a header
// edit (E-16) keeps every header, since the body is the participant's byte for
// byte. An unedited answer keeps all of it.
func TestStatedAnswer_EveryAnswerEditDropsTheETag(t *testing.T) {
	stated := statedAnswer{status: http.StatusOK, headers: map[string]string{
		shnsdk.FrameHeaderETag: `"v1"`, shnsdk.FrameHeaderLastModified: "yesterday", shnsdk.FrameHeaderRetryAfter: "1",
	}}
	if got := stated.about(relayedResponse([]byte(`{}`))); len(got) != 3 {
		t.Fatalf("unedited: %v", got)
	}
	// A value each edit of an answer may change: the adaptive address of a
	// Questionnaire for the re-key (E-12), which changes nothing else, and a
	// plain member for every other edit, which may not change that address.
	target := func(id relay.EditID) (relay.Body, *relay.Document, relay.NodeID) {
		raw := `{"a":"b"}`
		if id == relay.EditDTRAdaptiveAddressRekey {
			raw = `{"resourceType":"Questionnaire","extension":[{"url":"` + sdcQuestionnaireAdaptiveExt + `","valueUrl":"b"}]}`
		}
		body := relay.NewBody([]byte(raw), relay.OriginUpstreamResponse)
		doc, err := relay.Doc(body)
		if err != nil {
			t.Fatal(err)
		}
		if id != relay.EditDTRAdaptiveAddressRekey {
			member, _ := doc.Member(doc.Root(), "a")
			return body, doc, member
		}
		exts, _ := doc.Member(doc.Root(), "extension")
		member, _ := doc.Member(doc.Elems(exts)[0], "valueUrl")
		return body, doc, member
	}
	var edits, headerEdits int
	for _, e := range relayEdits {
		if e.Direction != relay.DirectionResponse {
			continue
		}
		if e.Kind == relay.EditKindSetHeader {
			// A header edit changes no byte of the body: the answer's
			// headers, its ETag among them, still describe it, and are kept.
			headerEdits++
			if dropped := relay.EditDropsAnswerHeaders(relayEdits, e.ID); len(dropped) != 0 {
				t.Errorf("%s drops %v, but changes no byte of the body", e.ID, dropped)
			}
			edited, err := headerEdited(relayedResponse([]byte(`{}`)), e.ID, e.Header, "https://payer.example/cdex/provider/p/Task/t-1",
				relay.HeaderInputs{relay.TaskLocationInputBase: "https://payer.example/cdex/provider/p", relay.TaskLocationInputTail: "Task/t-1"})
			if err != nil {
				t.Fatalf("%s: %v", e.ID, err)
			}
			if got := stated.about(edited); len(got) != 3 || got[shnsdk.FrameHeaderETag] != `"v1"` {
				t.Errorf("%s: %v, want every header kept", e.ID, got)
			}
			continue
		}
		edits++
		if !slices.Contains(relay.EditDropsAnswerHeaders(relayEdits, e.ID), relay.ETagHeader) {
			t.Errorf("%s leaves the ETag standing", e.ID)
		}
		if !strings.Contains(e.Disclosure, "ETag") {
			t.Errorf("%s does not disclose the drop", e.ID)
		}
		body, doc, member := target(e.ID)
		edited, err := relay.Apply(body, "application/json", e.ID, doc.Replace(member, []byte(`"c"`)))
		if err != nil {
			t.Fatalf("%s: %v", e.ID, err)
		}
		got := stated.about(edited)
		if _, kept := got[shnsdk.FrameHeaderETag]; kept || len(got) != 2 {
			t.Errorf("%s: %v", e.ID, got)
		}
	}
	if edits == 0 || headerEdits == 0 {
		t.Fatal("no edit of an answer's body, or none of its header: this row proves nothing")
	}
}

// An answer states its headers only as a 2xx relayed from the system: never one
// this gateway built, and never a refusal.
func TestLegResult_StatedHeaders(t *testing.T) {
	headers := map[string]string{shnsdk.FrameHeaderETag: `"v1"`}
	relayed := relayedResponse([]byte(`{}`))
	built, err := relay.Authored(relay.BuilderGatewayRefusal, []byte(`{}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		result LegResult
		want   int
	}{
		"relayed 2xx":   {LegResult{Response: relayed, AnswerStatus: 201, AnswerHeaders: headers}, 1},
		"built":         {LegResult{Response: built, AnswerStatus: 201, AnswerHeaders: headers}, 0},
		"not a 2xx":     {LegResult{Response: relayed, AnswerStatus: 503, AnswerHeaders: headers}, 0},
		"no status set": {LegResult{Response: relayed, AnswerHeaders: headers}, 0},
	} {
		if got := len(c.result.stated().headers); got != c.want {
			t.Errorf("%s: %d headers, want %d", name, got, c.want)
		}
	}
}

// Only a framed 2xx is read for what it states, and a frame's named headers
// alone: a peer's other keys are not, and its value is read as it came, so the
// write decides what can be carried.
func TestNoteStatedAnswer(t *testing.T) {
	var got statedAnswer
	ctx := withStatedAnswerSink(context.Background(), &got)
	noteStatedAnswer(ctx, shnsdk.HTTPFrameHeader{Status: 202, Headers: map[string]string{
		shnsdk.FrameHeaderETag: "a\r\nSet-Cookie: x", shnsdk.FrameHeaderRetryAfter: "1", "Content-Type": "x", "Location": "y",
	}})
	if got.status != 202 || len(got.headers) != 2 {
		t.Fatalf("%+v", got)
	}
	carried, notCarried := writableAnswerHeaders(got.headers)
	if _, ok := carried[shnsdk.FrameHeaderETag]; ok || carried[shnsdk.FrameHeaderRetryAfter] != "1" {
		t.Fatalf("carried %v", carried)
	}
	if !slices.Equal(notCarried, []string{"ETag:" + AnswerHeaderValueNotCarriable}) {
		t.Fatalf("not carried %v", notCarried)
	}
}

func TestAnswerHeaderNotCarriedValues_Closed(t *testing.T) {
	// The answer's own headers, and the address headers a recipient's gateway
	// states for the requester's gateway alone: a payer's on a DTR answer, a
	// provider's on its answer to a payer's Task (E-16).
	names := append(slices.Clone(answerHeaderNames), shnsdk.FrameHeaderOperationBase, shnsdk.FrameHeaderOperationAddressTags, shnsdk.FrameHeaderTaskLocation)
	for _, reason := range []string{AnswerHeaderValueNotCarriable, AnswerHeaderOverFrameLimit, AnswerHeaderOverAnswerLimit} {
		for _, name := range names {
			if !slices.Contains(AnswerHeaderNotCarriedValues, name+":"+reason) {
				t.Errorf("%s:%s is not on the closed list", name, reason)
			}
		}
	}
	if !slices.Contains(AnswerHeaderNotCarriedValues, "ETag:"+AnswerHeaderDecodedFromGzip) || len(AnswerHeaderNotCarriedValues) != 3*len(names)+1 {
		t.Fatalf("%v", AnswerHeaderNotCarriedValues)
	}
}

// What a peer's frame states is written to the participant only if it can be:
// a decoder keeps a value as it came, and a value with a line break in it would
// otherwise reach the participant's system. The answer goes through, recorded.
func TestWriteRelayedAnswer_LeavesOutWhatCannotBeCarried(t *testing.T) {
	x := &exchangeRecorder{}
	rec := httptest.NewRecorder()
	k := relay.Key{Leg: "crd-order-select", Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered}
	(&Gateway{}).writeRelayedAnswer(&exchangeWriter{ResponseWriter: rec, x: x}, statedAnswer{status: http.StatusAccepted, headers: map[string]string{
		shnsdk.FrameHeaderETag: "a\r\nSet-Cookie: x", shnsdk.FrameHeaderRetryAfter: "1",
	}}, "application/json", relayedResponse([]byte(`{}`)), k)
	if rec.Code != http.StatusAccepted || rec.Header().Get("ETag") != "" || rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if !slices.Equal(x.rec.AnswerHeadersNotCarried, []string{"ETag:" + AnswerHeaderValueNotCarriable}) {
		t.Fatalf("recorded %v", x.rec.AnswerHeadersNotCarried)
	}
}

// An answer relayed unread for this gateway's records is still the payer's, and
// keeps what it states.
func TestRelayUnread_KeepsWhatTheAnswerStates(t *testing.T) {
	up := upstreamReply{body: relay.NewBody([]byte(`{}`), relay.OriginUpstreamResponse), contentType: "application/fhir+json",
		status: http.StatusCreated, headers: map[string]string{shnsdk.FrameHeaderETag: `"v1"`}}
	got := relayUnread(context.Background(), up, RuleEOBDecision, nil).stated()
	if got.status != http.StatusCreated || got.headers[shnsdk.FrameHeaderETag] != `"v1"` {
		t.Fatalf("%+v", got)
	}
}

// A local fault is the gateway's own answer, and states nothing of the payer's.
func TestWriteRelayedAnswer_AFaultStatesNoHeader(t *testing.T) {
	edited := newBoundaryPayloads(t, relay.BuilderGatewayRefusal, `{}`).edited
	rec := httptest.NewRecorder()
	x := &exchangeRecorder{}
	(&Gateway{}).writeRelayedAnswer(&exchangeWriter{ResponseWriter: rec, x: x},
		statedAnswer{status: http.StatusAccepted, headers: map[string]string{shnsdk.FrameHeaderRetryAfter: "1", shnsdk.FrameHeaderETag: "a\x00b"}},
		"application/json", edited, answerKey("crd-order-select", relay.OutcomeAnswered))
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	// No answer went through, so none went through without a header.
	if len(x.rec.AnswerHeadersNotCarried) != 0 {
		t.Fatalf("recorded %v", x.rec.AnswerHeadersNotCarried)
	}
}
