//go:build !shn_coreonly

package engine

// workstream_pa_cdex_request_test.go — the provider gateway's side of the
// payer's attachment-request Task: with no Task endpoint configured for
// the provider's own system and no module 1, the leg is refused as routing
// (D120), at every conformance level, and never declared.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdexRequestProvider is a provider's gateway receiving a payer's leg from the
// Hub, at one conformance level, recording its exchanges.
type cdexRequestProvider struct {
	g         *Gateway
	payer     inboundTestRequester
	hubPriv   []byte
	authzPriv []byte
	got       *exchangeRecords
}

func newCDexRequestProvider(t *testing.T, level ConformanceEnforcement, opts ...func(*Config)) *cdexRequestProvider {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	hubPub, hubPriv := genED25519(t)
	_, provSignPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	payEncPub, payEncPriv := genKeyPair(t)
	paySignPub, _ := genED25519(t)
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	reg := shnsdk.NewRegistry()
	reg.Set("payer-x", shnsdk.RegistryEntry{ID: "payer-x", Role: "payer", EncPub: payEncPub, SignPub: paySignPub, MessageFrames: shnsdk.SupportedMessageFrames()})
	sor := newCensusSoR()
	cfg := Config{
		Role:            "provider",
		HolderID:        "provider-x",
		Identity:        shnsdk.Identity{HolderID: "provider-x", SignPriv: provSignPriv, EncPub: provEncPub, EncPriv: provEncPriv},
		AuthzURL:        "http://stub.test",
		AuthzPub:        authzPub,
		HubTransportPub: hubPub,
		Reg:             reg,
		Validator:       shnsdk.NewFakeValidator(),
		SoR:             sor,
		Store:           sor,
		Clock:           clock,
		Client:          &http.Client{Transport: &inboundAuthzStub{authzPriv: authzPriv, clock: clock, holder: "provider-x"}},
	}
	for _, o := range opts {
		o(&cfg)
	}
	g := mustNew(t, cfg)
	g.cfg.ConformanceEnforcement = level
	var got exchangeRecords
	g.cfg.ExchangeObserved = got.observe
	return &cdexRequestProvider{
		g:       g,
		payer:   inboundTestRequester{ID: "payer-x", EncPub: payEncPub, EncPriv: payEncPriv},
		hubPriv: hubPriv, authzPriv: authzPriv, got: &got,
	}
}

// send delivers body on the attachment-request leg as the Hub would: sealed to
// this provider, under a token the Authorization Framework bound to the leg's
// payer-payment frame and operation.
func (p *cdexRequestProvider) send(t *testing.T, body []byte) payerAnswer {
	t.Helper()
	return p.sendWith(t, body, nil)
}

// sendWith is send with mutate applied to the envelope's metadata and the
// token before the token is signed.
func (p *cdexRequestProvider) sendWith(t *testing.T, body []byte, mutate func(*shnsdk.Metadata, *shnsdk.Token)) payerAnswer {
	t.Helper()
	spec, ok := legSpecFor(legCDexRequest)
	if !ok {
		t.Fatalf("no catalog entry for %s", legCDexRequest)
	}
	clock := p.g.cfg.Clock()
	corr := fmt.Sprintf("corr-%s", legCDexRequest)
	payload, err := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, map[string]string{}, body)
	if err != nil {
		t.Fatal(err)
	}
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: p.payer.ID, Recipient: p.g.cfg.HolderID, TransactionType: spec.WireName,
		AuthorityFrame: spec.ReqFrame, Timestamp: clock.Format(time.RFC3339), CorrelationID: corr,
	}, payload, p.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := shnsdk.Token{
		Operation: spec.Op, Subject: "pci:payer-task-subject", Frame: spec.ReqFrame, Holder: p.payer.ID,
		CorrelationID: corr, Expiry: clock.Add(time.Hour), PayloadHash: sha256hexT(env.Ciphertext),
	}
	if mutate != nil {
		mutate(&env.Metadata, &unsigned)
	}
	tok := signTestToken(unsigned, p.authzPriv)
	tokBytes, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	env.Metadata.AuthzToken = string(tokBytes)
	raw, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	as, err := json.Marshal(shnsdk.IssueAssertion("hub", p.g.cfg.HolderID, p.hubPriv, clock, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", bytes.NewReader(raw))
	r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(as))
	rec := httptest.NewRecorder()
	p.g.inboundRoute()(rec, r)
	p.g.drainObserveChecks()
	if rec.Code != http.StatusOK {
		return payerAnswer{status: rec.Code, body: rec.Body.Bytes(), corr: corr}
	}
	hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, p.payer, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response frame: %v", err)
	}
	return payerAnswer{status: hdr.Status, body: answer, framed: true, corr: corr}
}

// cdexAttachmentRequestTask is a CDex 2.1.0 Task Attachment Request a payer's
// system writes: attachment-request-code, a contained Patient, the codes it
// needs and its payer-url.
func cdexAttachmentRequestTask() []byte {
	return []byte(`{"resourceType":"Task","meta":{"profile":["http://hl7.org/fhir/us/davinci-cdex/StructureDefinition/cdex-task-attachment-request"]},` +
		`"contained":[{"resourceType":"Patient","id":"patient","identifier":[{"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v2-0203","code":"MB"}]},"system":"http://example.org/payer/member","value":"MBR-COVERED"}],"name":[{"family":"Member","given":["Covered"]}],"birthDate":"1970-01-01"},` +
		`{"resourceType":"PractitionerRole","id":"owner","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}]}],` +
		`"identifier":[{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-cdex/CodeSystem/cdex-temp","code":"tracking-id"}]},"system":"http://example.org/payer/tracking","value":"TRK-1"}],` +
		`"status":"requested","intent":"order",` +
		`"code":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"attachment-request-code"}]},` +
		`"for":{"reference":"#patient"},"authoredOn":"2026-11-02T00:00:00Z",` +
		`"requester":{"identifier":{"system":"http://example.org/payer","value":"PAYER-X"}},"owner":{"reference":"#owner"},` +
		`"input":[{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"attachments-needed"}]},"valueCodeableConcept":{"coding":[{"system":"http://loinc.org","code":"11506-3"}]}},` +
		`{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"payer-url"}]},"valueUrl":"https://payer.example/fhir"}]}`)
}

// A provider's gateway with no Task endpoint for its own system and no module
// 1 has nowhere to carry a payer's attachment-request Task: at every level it
// refuses the leg 422 as routing, framed back to the payer as its own refusal,
// and records the refusal as the provider gateway's.
func TestCDexRequest_UnconfiguredProviderRefusesAsRouting(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newCDexRequestProvider(t, level)
			ans := p.send(t, cdexAttachmentRequestTask())
			if !ans.framed || ans.status != http.StatusUnprocessableEntity || !strings.Contains(string(ans.body), refusalCDexRequestNotTaken) {
				t.Fatalf("answer %d (framed %v) %s, want a framed 422 %q", ans.status, ans.framed, ans.body, refusalCDexRequestNotTaken)
			}
			rec := p.got.only(t)
			wantRefusal(t, rec, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
			if rec.Exchange != legCDexRequest || rec.Direction != DirectionInbound || rec.Sender != p.payer.ID || rec.Recipient != p.g.cfg.HolderID {
				t.Fatalf("record %+v, want the payer's attachment request delivered to this provider", rec)
			}
		})
	}
}

// The provider gateway's party is its own on a leg delivered to it, as a
// payer gateway's is on its legs; the ingress and the other roles are as
// before.
func TestExchangeParty_ProviderRecipient(t *testing.T) {
	for _, c := range []struct {
		role, direction, want string
	}{
		{"provider", DirectionInbound, RefusedByProviderGateway},
		{"provider", DirectionIngress, RefusedByProviderGateway},
		{"payer", DirectionInbound, RefusedByPayerGateway},
		// A payer's own ingress (the attachment-request Task): its refusals
		// are the payer gateway's.
		{"payer", DirectionIngress, RefusedByPayerGateway},
		{"facility", DirectionInbound, ExchangeOther},
		{"phg", DirectionInbound, ExchangeOther},
	} {
		g := &Gateway{cfg: Config{Role: c.role}}
		if got := g.exchangeParty(c.direction); got != c.want {
			t.Errorf("exchangeParty(%s, %s) = %q, want %q", c.role, c.direction, got, c.want)
		}
	}
}

// The payer's attachment request is bound to its own request frame and
// operation (the leg's row): a token for another frame or operation is
// refused 403 on authority before the Task is read, at every conformance
// level, recorded as the provider gateway's refusal. A network rule.
func TestCDexRequest_ATokenForAnotherFrameOrOperationIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*shnsdk.Metadata, *shnsdk.Token){
		"the provider's frame": func(m *shnsdk.Metadata, tok *shnsdk.Token) {
			m.AuthorityFrame, tok.Frame = "provider-tpo", "provider-tpo"
		},
		"the payer's coverage frame": func(m *shnsdk.Metadata, tok *shnsdk.Token) {
			m.AuthorityFrame, tok.Frame = "payer-coverage", "payer-coverage"
		},
		"the token alone on another frame": func(_ *shnsdk.Metadata, tok *shnsdk.Token) { tok.Frame = "payer-coverage" },
		"another operation":                func(_ *shnsdk.Metadata, tok *shnsdk.Token) { tok.Operation = "cdex-attachment-submit" },
		"another holder's token":           func(_ *shnsdk.Metadata, tok *shnsdk.Token) { tok.Holder = "provider-x" },
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newCDexRequestProvider(t, level)
				ans := p.sendWith(t, cdexAttachmentRequestTask(), mutate)
				if ans.framed || ans.status != http.StatusForbidden || strings.TrimSpace(string(ans.body)) != `{"error":"authz verification failed"}` {
					t.Fatalf("answer %d (framed %v) %s, want the bare 403 authz verification failed", ans.status, ans.framed, ans.body)
				}
				wantRefusal(t, p.got.only(t), http.StatusForbidden, RefusedByProviderGateway, RefusalAuthority)
			})
		}
	}
}
