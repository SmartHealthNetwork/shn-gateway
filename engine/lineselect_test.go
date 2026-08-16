// lineselect_test.go — build-at-the-selected-line:
// the declared-set accessor, the paCatalog ok-guards, the line-aware
// validator seam (F7), and the request-frame rules on BOTH sides.
package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ---- (1) ok-guards: an unknown legType is a loud error, never a silent
// version-neutral fallthrough (the fail-open seed closes). ----

func TestSelectLegToken_UnknownLegTypeErrors(t *testing.T) {
	g := &Gateway{cfg: Config{Reg: shnsdk.NewRegistry()}}
	tok, err := g.selectLegToken("payer", "no-such-leg")
	if err == nil {
		t.Fatalf("unknown legType must error, got token %q", tok)
	}
	if !strings.Contains(err.Error(), "no-such-leg") {
		t.Fatalf("error must name the legType, got %v", err)
	}
	// A known version-neutral leg still returns ("", nil) — the guard rejects
	// UNKNOWN legTypes, not neutral ones.
	if tok, err := g.selectLegToken("payer", "federated-query"); err != nil || tok != "" {
		t.Fatalf("version-neutral leg = (%q,%v), want (\"\",nil)", tok, err)
	}
}

func TestContractTokenForLeg_UnknownLegTypeErrors(t *testing.T) {
	g := &Gateway{cfg: Config{Reg: shnsdk.NewRegistry()}}
	if _, err := g.contractTokenForLeg("no-such-leg", ""); err == nil {
		t.Fatal("unknown legType must error")
	}
	// Built-token wins: the stamp names the line the payload was BUILT at.
	got, err := g.contractTokenForLeg("pas-claim", "pa.pas@2.2")
	if err != nil || got != "pa.pas@2.2" {
		t.Fatalf("contractTokenForLeg(pas-claim, pa.pas@2.2) = (%q,%v)", got, err)
	}
	// No built token: this build's own declared highest line.
	got, err = g.contractTokenForLeg("pas-claim", "")
	if err != nil || got != "pa.pas@2.0" {
		t.Fatalf("contractTokenForLeg(pas-claim, \"\") = (%q,%v), want pa.pas@2.0", got, err)
	}
	// Version-neutral leg: never stamped, even with a built token in hand.
	if got, err := g.contractTokenForLeg("federated-query", "pa.pas@2.2"); err != nil || got != "" {
		t.Fatalf("version-neutral stamp = (%q,%v), want (\"\",nil)", got, err)
	}
}

func TestNativeResponder_UnknownLegTypeErrors(t *testing.T) {
	n := &nativeResponder{declaredContractVersions: []string{"pa.pas@2.0"}}
	if _, err := n.Handle(context.Background(), "no-such-leg", "corr", "pci", nil); err == nil {
		t.Fatal("nativeResponder must fail closed on an unknown legType")
	}
}

// ---- (2) D1a: the declared-set accessor drives selection. ----

func TestDeclaredContractVersionsAccessor(t *testing.T) {
	g := &Gateway{cfg: Config{Reg: shnsdk.NewRegistry()}}
	if got := g.declaredContractVersions(); len(got) != len(shnsdk.SupportedContractVersions()) {
		t.Fatalf("default declared set = %v, want the build default", got)
	}
	reg := shnsdk.NewRegistry()
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", ContractVersions: []string{"pa.pas@2.2", "pa.pas@2.0"}})
	over := &Gateway{cfg: Config{Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0", "pa.pas@2.2"}}}
	tok, err := over.selectLegToken("payer", "pas-claim")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "pa.pas@2.2" {
		t.Fatalf("selection off the OVERRIDDEN declared set = %q, want pa.pas@2.2", tok)
	}
	// The un-overridden build declares 2.0 only — same peer, lower common line.
	base := &Gateway{cfg: Config{Reg: reg}}
	if tok, _ := base.selectLegToken("payer", "pas-claim"); tok != "pa.pas@2.0" {
		t.Fatalf("default declared set selection = %q, want pa.pas@2.0", tok)
	}
}

// ---- (F7) the line-aware validator seam. ----

func TestValidatorForLine(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()
	// Legacy wiring (Validator only, no per-line map): every line uses it.
	g := &Gateway{cfg: Config{Validator: canonical}}
	if g.validatorForLine("2.2") == nil {
		t.Fatal("a deployment that declares no per-line lanes must serve every line from Validator")
	}
	// Explicitly-laned deployment: a line with no lane is UNLANED (nil), never
	// silently validated against the canonical lane.
	laned := &Gateway{cfg: Config{
		Validator:        canonical,
		ValidatorsByLine: map[string]shnsdk.Validator{"2.0": canonical},
	}}
	if laned.validatorForLine("2.0") == nil {
		t.Fatal("configured lane must resolve")
	}
	if laned.validatorForLine("2.2") != nil {
		t.Fatal("unconfigured lane must be nil (fail-closed, FR-36/FR-G29)")
	}
}

func TestValidateFHIRUnlanedFailsClosed(t *testing.T) {
	g := &Gateway{cfg: Config{
		Validator:        shnsdk.NewFakeValidator(),
		ValidatorsByLine: map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()},
	}}
	status, msg := g.validateFHIR(context.Background(), []byte(`{"resourceType":"Patient"}`), "egress", "2.2")
	if status != http.StatusInternalServerError || !strings.Contains(msg, "2.2") {
		t.Fatalf("unlaned validate = (%d,%q), want 500 naming the line", status, msg)
	}
}

// ---- (8) request-frame receiver: honor a framed request iff native AND laned. ----

// d9Gateway builds a minimal payer-side Gateway with the given per-line lanes and
// a sender registered with the given declared contract-version tokens.
func d9Gateway(lanes map[string]shnsdk.Validator, senderDeclared []string) *Gateway {
	reg := shnsdk.NewRegistry()
	reg.Set("requester", shnsdk.RegistryEntry{ID: "requester", Role: "provider", ContractVersions: senderDeclared})
	return &Gateway{cfg: Config{
		Reg:              reg,
		Validator:        shnsdk.NewFakeValidator(),
		ValidatorsByLine: lanes,
	}}
}

func framedRequest(t *testing.T, token string, body []byte) []byte {
	t.Helper()
	out, err := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, map[string]string{
		"Content-Type":                    "application/fhir+json",
		shnsdk.FrameHeaderContractVersion: token,
	}, body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFramedRequestHonored(t *testing.T) {
	fake := shnsdk.NewFakeValidator()
	g := d9Gateway(map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake}, nil)
	body := []byte(`{"resourceType":"Bundle"}`)
	got, answer, status, msg := g.unframeRequest("pas-claim", framedRequest(t, "pa.pas@2.2", body))
	if status != 0 {
		t.Fatalf("honored request refused: %d %s", status, msg)
	}
	if answer != "pa.pas@2.2" {
		t.Fatalf("answer token = %q, want pa.pas@2.2", answer)
	}
	if string(got) != string(body) {
		t.Fatalf("unframed body = %q, want the inner payload verbatim", got)
	}
}

func TestFramedRequestUnknownLineRefused(t *testing.T) {
	g := d9Gateway(nil, nil)
	_, _, status, msg := g.unframeRequest("pas-claim", framedRequest(t, "pa.pas@9.9", []byte(`{}`)))
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", status)
	}
	if !strings.Contains(msg, "pa.pas@9.9") {
		t.Fatalf("refusal must name the claimed token: %q", msg)
	}
}

func TestFramedRequestWrongContractRefused(t *testing.T) {
	g := d9Gateway(nil, nil)
	// A pa.crd token on a pa.pas leg is a category error, not a line question.
	_, _, status, msg := g.unframeRequest("pas-claim", framedRequest(t, "pa.crd@2.0", []byte(`{}`)))
	if status != http.StatusUnprocessableEntity || !strings.Contains(msg, "pa.crd@2.0") {
		t.Fatalf("cross-contract claim = (%d,%q), want a 422 naming the token", status, msg)
	}
}

func TestFramedRequestNativeButUnlanedRefused(t *testing.T) {
	fake := shnsdk.NewFakeValidator()
	// 2.2 is natively buildable but this deployment configured no 2.2 lane.
	g := d9Gateway(map[string]shnsdk.Validator{"2.0": fake}, nil)
	_, _, status, msg := g.unframeRequest("pas-claim", framedRequest(t, "pa.pas@2.2", []byte(`{}`)))
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", status)
	}
	if !strings.Contains(msg, "2.2") || !strings.Contains(strings.ToLower(msg), "validator") {
		t.Fatalf("refusal must name the missing lane (FR-36/FR-G29): %q", msg)
	}
}

func TestBareRequestTolerated(t *testing.T) {
	// Symmetric recomputation: the sender declares {2.0,2.2}, this build declares
	// {2.0,2.2} → highest common 2.2, with no request frame in sight.
	g := d9Gateway(nil, []string{"pa.pas@2.0", "pa.pas@2.2"})
	g.cfg.DeclaredContractVersions = []string{"pa.pas@2.0", "pa.pas@2.2"}
	body := []byte(`{"resourceType":"Bundle"}`)
	got, answer, status, msg := g.unframeRequestFrom("requester", "pas-claim", body)
	if status != 0 {
		t.Fatalf("bare request refused: %d %s", status, msg)
	}
	if answer != "pa.pas@2.2" {
		t.Fatalf("recomputed answer = %q, want pa.pas@2.2", answer)
	}
	if string(got) != string(body) {
		t.Fatal("bare body must pass through verbatim")
	}
	// A SILENT sender (no declaration) falls back to this build's own canonical line.
	g2 := d9Gateway(nil, nil)
	if _, answer, status, _ := g2.unframeRequestFrom("requester", "pas-claim", body); status != 0 || answer != "pa.pas@2.0" {
		t.Fatalf("silent sender = (%q,%d), want pa.pas@2.0 / 0", answer, status)
	}
	// A version-neutral leg is never framed and never gets an answer token.
	if _, answer, status, _ := g2.unframeRequestFrom("requester", "federated-query", body); status != 0 || answer != "" {
		t.Fatalf("version-neutral leg = (%q,%d), want \"\" / 0", answer, status)
	}
}

func TestTamperedRequestFrameRefused(t *testing.T) {
	fake := shnsdk.NewFakeValidator()
	g := d9Gateway(map[string]shnsdk.Validator{"2.0": fake, "2.2": fake}, nil)
	frame := framedRequest(t, "pa.pas@2.2", []byte(`{"resourceType":"Bundle"}`))
	// One mutation on an otherwise-valid framed request: flip the claimed token
	// to a line no build speaks. The receiver must refuse, never fall back.
	tampered := framedRequest(t, "pa.pas@4.7", []byte(`{"resourceType":"Bundle"}`))
	if string(frame) == string(tampered) {
		t.Fatal("fixture bug: tampered frame is identical")
	}
	_, _, status, msg := g.unframeRequest("pas-claim", tampered)
	if status != http.StatusUnprocessableEntity || !strings.Contains(msg, "pa.pas@4.7") {
		t.Fatalf("tampered token = (%d,%q), want a 422 naming it", status, msg)
	}
}

// ---- (5) stamp honesty: a verbatim foreign relay is UNSTAMPED. ----

// TestRelayedForeignBodyUnstamped drives the conformant PAS submit handler with a
// responder that declares its answer a verbatim foreign relay (markForeignRelay,
// the native-forward path) and asserts the sealed frame carries NO contractVersion
// header — SHN must not vouch for the line of bytes it did not produce. The
// non-relayed sibling IS stamped, at the line it was built at.
func TestRelayedForeignBodyUnstamped(t *testing.T) {
	run := func(t *testing.T, relayed bool, answerTok string) (map[string]string, bool) {
		t.Helper()
		g, requester := newInboundTestGateway(t, true)
		pci, _, ok := g.cfg.SoR.ResolvePatient("MBR-COVERED")
		if !ok {
			t.Fatal("MBR-COVERED not resolvable")
		}
		bundle := conformantPASBundleWithQR(t, "MBR-COVERED")
		inner := g.cfg.Responder
		g.cfg.Responder = relayFlagResponder{inner: inner, relayed: relayed}

		env, err := shnsdk.Seal(shnsdk.Metadata{
			Sender: requester.ID, Recipient: "payer", TransactionType: "pas-claim",
			AuthorityFrame: "payer-coverage", Timestamp: g.cfg.Clock().Format(time.RFC3339),
			CorrelationID: "corr-relay-1",
		}, bundle, g.cfg.Identity.EncPub)
		if err != nil {
			t.Fatal(err)
		}
		tok := shnsdk.Token{Operation: "pas-submit", Subject: pci, CorrelationID: "corr-relay-1"}
		rec := httptest.NewRecorder()
		r := newSignedInboundRequest(t, g, requester.ID)
		g.handlePASNativeInbound(rec, r, env, tok, bundle, answerTok)
		if rec.Code != 200 {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		payload := openResponseLeg(t, requester, rec.Body.Bytes())
		if !shnsdk.IsFramed(payload) {
			t.Fatalf("capable requester must get a frame, got %q", payload)
		}
		hdr, _, derr := shnsdk.DecodeHTTPFrame(payload)
		if derr != nil {
			t.Fatal(derr)
		}
		stamped, has := hdr.Headers[shnsdk.FrameHeaderContractVersion]
		return map[string]string{"stamp": stamped}, has
	}

	t.Run("relayed foreign body carries no stamp", func(t *testing.T) {
		got, has := run(t, true, "pa.pas@2.0")
		if has {
			t.Fatalf("a verbatim foreign relay must be UNSTAMPED, got %q", got["stamp"])
		}
	})
	t.Run("SHN-produced body is stamped at its built line", func(t *testing.T) {
		got, has := run(t, false, "pa.pas@2.0")
		if !has || got["stamp"] != "pa.pas@2.0" {
			t.Fatalf("stamp = %q (present=%v), want pa.pas@2.0", got["stamp"], has)
		}
	})
}

// relayFlagResponder marks the inner responder's answer as a verbatim foreign
// relay (or not) — the one variable TestRelayedForeignBodyUnstamped changes.
type relayFlagResponder struct {
	inner   LegResponder
	relayed bool
}

func (r relayFlagResponder) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	res, err := r.inner.Handle(ctx, leg, corrID, subjectPCI, requestFHIR)
	if err != nil || !r.relayed {
		return res, err
	}
	return markForeignRelay(res), nil
}

// dtrRelayResponder answers the DTR leg the way the NATIVE forward does: a verbatim
// foreign $questionnaire-package Bundle, flagged ResponseRelayed but NOT
// ResponseSubjectForeign (the subject fence must stay live on this leg).
type dtrRelayResponder struct {
	body    []byte
	relayed bool
}

func (d dtrRelayResponder) Handle(_ context.Context, _, _, _ string, _ []byte) (LegResult, error) {
	return LegResult{ResponseFHIR: d.body, ResponseRelayed: d.relayed}, nil
}

// TestDTRRelayedPackageUnstamped is the DTR sibling of
// TestRelayedForeignBodyUnstamped (review finding 3): the native-forward DTR leg
// relays a partner's $questionnaire-package VERBATIM, so the stamp-honesty rule applies
// to it identically — SHN must not stamp a contract line onto bytes it did not
// build. An SHN-PRODUCED package is still stamped at its built line.
func TestDTRRelayedPackageUnstamped(t *testing.T) {
	pkg := []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	stampOf := func(t *testing.T, relayed bool) (string, bool) {
		t.Helper()
		g, requester := newInboundTestGateway(t, true)
		g.cfg.Responder = dtrRelayResponder{body: pkg, relayed: relayed}
		env, err := shnsdk.Seal(shnsdk.Metadata{
			Sender: requester.ID, Recipient: "payer", TransactionType: "dtr-questionnaire-fetch",
			AuthorityFrame: "payer-coverage", Timestamp: g.cfg.Clock().Format(time.RFC3339),
			CorrelationID: "corr-dtr-1",
		}, []byte(`{"canonical":"http://example/Questionnaire/q"}`), g.cfg.Identity.EncPub)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		r := newSignedInboundRequest(t, g, requester.ID)
		g.handleDTRInbound(rec, r, env, shnsdk.Token{Operation: "dtr-questionnaire-fetch", Subject: "pci-1", CorrelationID: "corr-dtr-1"},
			[]byte(`{"canonical":"http://example/Questionnaire/q"}`), "pa.dtr@2.0")
		if rec.Code != 200 {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		payload := openResponseLeg(t, requester, rec.Body.Bytes())
		hdr, _, derr := shnsdk.DecodeHTTPFrame(payload)
		if derr != nil {
			t.Fatalf("decode frame: %v", derr)
		}
		stamped, has := hdr.Headers[shnsdk.FrameHeaderContractVersion]
		return stamped, has
	}

	t.Run("relayed foreign package carries no stamp", func(t *testing.T) {
		if got, has := stampOf(t, true); has {
			t.Fatalf("a verbatim foreign $questionnaire-package must be UNSTAMPED, got %q", got)
		}
	})
	t.Run("SHN-produced package is stamped", func(t *testing.T) {
		got, has := stampOf(t, false)
		if !has || got != "pa.dtr@2.0" {
			t.Fatalf("stamp = %q (present=%v), want pa.dtr@2.0", got, has)
		}
	})
}

// TestDTREgressValidatesOnAnswerLine (review finding 1): the payer builds its
// $questionnaire-package at the ANSWER LINE, so the egress $validate must run on
// THAT line's lane. This gateway has only a 2.0 lane, so an answer at 2.2 must fail
// closed on the missing lane rather than being silently checked against the 2.0 IG.
// If payer.go regresses to passing "" the validate lands on the canonical lane and
// this test goes green-when-it-should-not — hence the paired 2.0 control.
func TestDTREgressValidatesOnAnswerLine(t *testing.T) {
	pkg := []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	run := func(t *testing.T, answerTok string) *httptest.ResponseRecorder {
		t.Helper()
		g, requester := newInboundTestGateway(t, true)
		g.cfg.Responder = dtrRelayResponder{body: pkg} // SHN-produced ⇒ egress-validated
		// Explicitly laned: 2.0 only. 2.2 is therefore UNLANED (fail-closed).
		g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()}
		env, err := shnsdk.Seal(shnsdk.Metadata{
			Sender: requester.ID, Recipient: "payer", TransactionType: "dtr-questionnaire-fetch",
			AuthorityFrame: "payer-coverage", Timestamp: g.cfg.Clock().Format(time.RFC3339),
			CorrelationID: "corr-dtr-2",
		}, []byte(`{"canonical":"http://example/Questionnaire/q"}`), g.cfg.Identity.EncPub)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		r := newSignedInboundRequest(t, g, requester.ID)
		g.handleDTRInbound(rec, r, env, shnsdk.Token{Operation: "dtr-questionnaire-fetch", Subject: "pci-1", CorrelationID: "corr-dtr-2"},
			[]byte(`{"canonical":"http://example/Questionnaire/q"}`), answerTok)
		return rec
	}

	t.Run("laned answer line validates and answers", func(t *testing.T) {
		if rec := run(t, "pa.dtr@2.0"); rec.Code != 200 {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unlaned answer line fails closed", func(t *testing.T) {
		rec := run(t, "pa.dtr@2.2")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 — a 2.2 package must NOT be validated on the 2.0 lane; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "2.2") {
			t.Fatalf("failure must name the missing lane: %s", rec.Body.String())
		}
	})
}

// errTransport fails every request, so a forward that gets PAST the version filter
// surfaces as a transport fault rather than reaching any network.
type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("partner unreachable (test transport)")
}

// TestNativeForwardFilterUsesDeclaredSet (review finding 2, D1a): the foreign-peer
// version filter's OWN half must be this deployment's DECLARED set, not the library
// build constant. A deployment declaring 2.2 must forward to a 2.2-only partner;
// the same responder without the override refuses (proving the override is what
// moved the outcome).
func TestNativeForwardFilterUsesDeclaredSet(t *testing.T) {
	peer := []string{"pa.crd@2.2"} // partner speaks 2.2 only
	// A client whose transport always errors: once the filter PASSES, the forward
	// itself fails as a transport fault — a clearly different outcome from the 422
	// refuse-before-forward, and it sends zero bytes anywhere.
	newResponder := func(own []string) LegResponder {
		opts := []NativeOption{WithDeclaredContractVersions(peer)}
		if own != nil {
			opts = append(opts, WithOwnContractVersions(own))
		}
		return NewNativeResponder(&http.Client{Transport: errTransport{}}, "http://partner.invalid",
			"svc", NewStubHolderData(), func() time.Time { return time.Unix(1700000000, 0).UTC() }, opts...)
	}
	t.Run("build constant alone refuses", func(t *testing.T) {
		res, err := newResponder(nil).Handle(context.Background(), "crd-order-select", "corr", "pci", nil)
		if err != nil || res.Status != http.StatusUnprocessableEntity {
			t.Fatalf("want a legible 422 refusal, got status=%d err=%v", res.Status, err)
		}
		if !strings.Contains(res.Message, "pa.crd@2.0") || !strings.Contains(res.Message, "pa.crd@2.2") {
			t.Fatalf("refusal must name both declarations: %s", res.Message)
		}
	})
	t.Run("declared override forwards", func(t *testing.T) {
		res, err := newResponder([]string{"pa.crd@2.0", "pa.crd@2.2"}).Handle(context.Background(), "crd-order-select", "corr", "pci", nil)
		if res.Status == http.StatusUnprocessableEntity {
			t.Fatalf("declared override must clear the version filter, still refused: %s", res.Message)
		}
		if err == nil {
			t.Fatal("fixture bug: the forward should have failed as a transport fault once the filter passed")
		}
	})
	t.Run("accessor mirrors the gateway's empty-means-default rule", func(t *testing.T) {
		n := &nativeResponder{}
		if strings.Join(n.ownDeclared(), ",") != strings.Join(shnsdk.SupportedContractVersions(), ",") {
			t.Fatalf("unset ownContractVersions must fall back to the build default, got %v", n.ownDeclared())
		}
	})
}

// TestAnswerLineFallbackUsesDeclaredSet (review finding 2, second half): when no
// answer line is carried, a builder falls back to THIS DEPLOYMENT'S declared line,
// not the library build constant.
func TestAnswerLineFallbackUsesDeclaredSet(t *testing.T) {
	if got := answerLineOr(context.Background(), "pa.pas"); got != "2.0" {
		t.Fatalf("no ctx ⇒ build default line, got %q", got)
	}
	ctx := withDeclaredContractVersions(context.Background(), []string{"pa.pas@2.0", "pa.pas@2.2"})
	if got := answerLineOr(ctx, "pa.pas"); got != "2.2" {
		t.Fatalf("declared-set fallback = %q, want 2.2 (the deployment's highest declared pa.pas line)", got)
	}
	// A carried answer line still wins over the fallback.
	both := withAnswerLine(ctx, "pa.pas@2.0")
	if got := answerLineOr(both, "pa.pas"); got != "2.0" {
		t.Fatalf("the carried answer line must win, got %q", got)
	}
}

// ---- (8) request-frame ORIGINATOR: the byte-identity fence + the framing rule. ----

// declareRecipientRequestFrames flips the harness recipient's registry entry to
// advertise requestFrames v1 (the framing capability gate), preserving everything else.
func declareRecipientRequestFrames(t *testing.T, e *inProcessExchange) {
	t.Helper()
	entry, ok := e.originator.cfg.Reg.Lookup(e.payerID)
	if !ok {
		t.Fatalf("recipient %q not in registry", e.payerID)
	}
	entry.RequestFrames = shnsdk.SupportedRequestFrames()
	e.originator.cfg.Reg.Set(e.payerID, entry)
}

// TestRequestNotFramedToNonDeclaringPeer is THE byte-identity fence:
// requestFrames defaults ON for SHN builds, so SHN↔SHN requests are
// framed immediately — but a peer whose registry entry does NOT declare the
// capability must receive the pre-framing BARE payload, byte for byte.
func TestRequestNotFramedToNonDeclaringPeer(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env) // messageFrames v1 (responses) — deliberately NOT requestFrames
	if _, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "",
		Content{WorkstreamType: workstreamPA, Bytes: env.crdReq}); err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	got := env.lastRequestPayload()
	if shnsdk.IsFramed(got) {
		t.Fatal("a peer that does not declare requestFrames must receive a BARE request")
	}
	if string(got) != string(env.crdReq) {
		t.Fatalf("request payload = %q, want the pre-framing bytes verbatim %q", got, env.crdReq)
	}
}

// TestRequestFramedToDeclaringPeer: the same exchange toward a requestFrames-declaring
// peer carries the routed token in the request frame, with the body verbatim inside.
func TestRequestFramedToDeclaringPeer(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env)
	declareRecipientRequestFrames(t, env)
	if _, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "",
		Content{WorkstreamType: workstreamPA, Bytes: env.crdReq}); err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	got := env.lastRequestPayload()
	if !shnsdk.IsFramed(got) {
		t.Fatalf("declaring peer must receive a FRAMED request, got %q", got)
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(got)
	if err != nil {
		t.Fatalf("decode request frame: %v", err)
	}
	if hdr.Headers[shnsdk.FrameHeaderContractVersion] != "pa.crd@2.0" {
		t.Fatalf("request stamp = %q, want the routed token pa.crd@2.0", hdr.Headers[shnsdk.FrameHeaderContractVersion])
	}
	if string(body) != string(env.crdReq) {
		t.Fatalf("framed request body = %q, want the payload verbatim", body)
	}
}

// TestVersionNeutralRequestNeverFramed: a version-neutral leg has no token to
// claim, so it stays bare even toward a declaring peer (there is nothing to say).
func TestVersionNeutralRequestNeverFramed(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env)
	declareRecipientRequestFrames(t, env)
	// coverage-eligibility is version-neutral (paCatalog Contract "").
	_, _ = env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "coverage-eligibility", "pci-1", "corr-vn", "",
		Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	if got := env.lastRequestPayload(); shnsdk.IsFramed(got) {
		t.Fatalf("version-neutral leg must never be framed, got %q", got)
	}
}
